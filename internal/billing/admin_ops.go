package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ispbill/internal/store"
)

// ChangePackage moves an account to another package without a payment.
//
// keepTime=true keeps the current expiry and only switches the speed (for a
// mistake, or a goodwill upgrade). keepTime=false starts the new package
// fresh from now (expiry = now + the package's duration). Either way the
// change is logged as an event, the router user's profile is updated and the
// session is restarted so the new speed applies at once.
func (s *Service) ChangePackage(ctx context.Context, id, pkgID string, keepTime bool) error {
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		pkg, ok := d.Packages[pkgID]
		if !ok || pkg.Deleted {
			return errors.New("choose a package")
		}
		now := s.now()
		old := d.Packages[a.PackageID]
		if keepTime {
			if a.PackageID == pkgID {
				return store.ErrNoChange
			}
			if !a.ExpiresAt.After(now) {
				return errors.New("this flat has no time left to keep; choose \"start fresh from now\" instead")
			}
		} else {
			a.ExpiresAt = now.Add(time.Duration(pkg.DurationMin) * time.Minute)
		}
		a.PackageID = pkgID
		d.Accounts[id] = a
		how := "kept expiry"
		if !keepTime {
			how = "started fresh, until " + a.ExpiresAt.In(EAT).Format("02 Jan 15:04")
		}
		d.AddEvent(store.Event{Time: now, SiteID: a.SiteID, Kind: "info", AccountID: id,
			Msg: fmt.Sprintf("%s: package changed by admin %s -> %s (%s)", a.Flat, nameOr(old.Name, "none"), pkg.Name, how)})
		s.enqueueLocked(d, a, true, "package changed")
		return nil
	})
	if err == nil {
		s.runAccountSite(ctx, id)
	}
	return err
}

// EndPlan expires the account's current package now (no refund is recorded;
// do that in the payments ledger if money is returned). The flat is taken
// offline and can buy again from the portal.
func (s *Service) EndPlan(ctx context.Context, id string) error {
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		now := s.now()
		if !a.ExpiresAt.After(now) {
			return store.ErrNoChange
		}
		a.ExpiresAt = now
		d.Accounts[id] = a
		d.AddEvent(store.Event{Time: now, SiteID: a.SiteID, Kind: "info", AccountID: id, Msg: a.Flat + ": plan ended by admin"})
		s.enqueueLocked(d, a, true, "plan ended")
		return nil
	})
	if err == nil {
		s.runAccountSite(ctx, id)
	}
	return err
}

// DeleteAccount closes a flat's account (it moves to Data.ClosedAccounts). Its router user and session are
// removed (retried in the background if the router is unreachable). Payment
// history is kept in the ledger; the account code is never reused.
// confirm must equal the account code, to avoid deleting the wrong flat.
func (s *Service) DeleteAccount(ctx context.Context, id, confirm string) error {
	var site string
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		if confirm != a.Code {
			return fmt.Errorf("type the account code %s to confirm", a.Code)
		}
		// Queue the router cleanup before the account disappears.
		s.enqueueLocked(d, a, true, "account deleted")
		// The flat is closed, not erased: it keeps its code, history and
		// payments, and its ONT goes back to stock as "returned".
		now := s.now()
		delete(d.Accounts, id)
		a.ClosedAt = now
		d.ClosedAccounts[id] = a
		closeTenancyLocked(d, id, now)
		for k, e := range d.Equipment {
			if e.AccountID == id {
				returnLocked(d, k, e, store.EqReturned, "account closed", now)
			}
		}
		for k, o := range d.Orders {
			if o.AccountID == id && o.Status == store.OrderPending {
				o.Status, o.Reason = store.OrderFailed, "account deleted"
				d.Orders[k] = o
			}
		}
		for k, p := range d.Payments {
			if p.AccountID == id && p.BillRef == "" {
				p.BillRef = a.Code + " (deleted: " + a.Flat + ")"
				d.Payments[k] = p
			}
		}
		d.AddEvent(store.Event{Time: s.now(), SiteID: a.SiteID, Kind: "info", Msg: fmt.Sprintf("%s (%s, MAC %s): account deleted by admin", a.Flat, a.Code, a.MAC)})
		site = a.SiteID
		return nil
	})
	if err == nil {
		s.log.Info("account deleted", "id", id)
		s.RunSiteTasks(ctx, site)
	}
	return err
}

// DeletePackage hides a package everywhere and removes its router profile
// from every building's router (in the background).
// It refuses while a flat still has time on it or unused vouchers exist;
// disable it instead in that case. Past payments keep showing its name.
func (s *Service) DeletePackage(_ context.Context, id string) error {
	var prof string
	err := s.st.Update(func(d *store.Data) error {
		p, ok := d.Packages[id]
		if !ok || p.Deleted {
			return ErrNotFound
		}
		now := s.now()
		n := 0
		for _, a := range d.Accounts {
			if a.PackageID == id && a.ExpiresAt.After(now) {
				n++
			}
		}
		if n > 0 {
			return fmt.Errorf("%d flat(s) still have time on %s. Change their package or wait for it to expire, or untick it in the portal with Disabled", n, p.Name)
		}
		for _, v := range d.Vouchers {
			if v.PackageID == id && !v.Used {
				return fmt.Errorf("there are unused %s vouchers; disable the package instead so they still work", p.Name)
			}
		}
		p.Deleted, p.Disabled = true, true
		d.Packages[id] = p
		prof = p.Profile()
		d.AddEvent(store.Event{Time: now, Kind: "info", Msg: "package deleted: " + p.Name})
		return nil
	})
	if err != nil {
		return err
	}
	s.syncAllSites(func(ctx context.Context, siteID string) {
		if rerr := s.router(siteID).RemoveProfile(ctx, prof); rerr != nil {
			s.log.Warn("could not remove router profile (harmless, remove it by hand)", "site", siteID, "profile", prof, "err", rerr)
		}
		if err := s.SyncProfiles(ctx, siteID); err != nil {
			s.log.Warn("profile push failed; will retry", "site", siteID, "err", err)
		}
	})
	return nil
}

func nameOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// MoveAccount moves a flat to another building: its router user is removed
// from the old building's router and created on the new one's. The account
// keeps its code, plan and expiry. It is refused when the new building already
// has that MAC or flat label. Open portal orders are failed. Past payments stay
// recorded under the building where they arrived.
func (s *Service) MoveAccount(ctx context.Context, id, toSiteID string) error {
	var from string
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		if a.SiteID == toSiteID {
			return store.ErrNoChange
		}
		to, ok := d.Sites[toSiteID]
		if !ok || to.Removed {
			return ErrNoSite
		}
		if flatTaken(d, toSiteID, a.Flat, id) {
			return fmt.Errorf("%s already has an account for %q. Rename the flat first", to.Name, a.Flat)
		}
		from = a.SiteID
		// Clean the old router, then the new one gets the user via its own task.
		s.enqueueLocked(d, a, true, "account moved away")
		a.SiteID = toSiteID
		a.IP = "" // an address belongs to one building; the new one allocates its own
		a.RouterActive = false
		d.Accounts[id] = a
		delete(d.Devices, store.DeviceKey(toSiteID, a.MAC))
		for k, o := range d.Orders {
			if o.AccountID == id && o.Status == store.OrderPending {
				o.Status, o.Reason = store.OrderFailed, "account moved to another building"
				d.Orders[k] = o
			}
		}
		s.enqueueLocked(d, a, true, "account moved in")
		d.AddEvent(store.Event{Time: s.now(), SiteID: toSiteID, Kind: "info", AccountID: id,
			Msg: fmt.Sprintf("%s (%s): moved from %s to %s", a.Flat, a.Code, d.Sites[from].Name, to.Name)})
		return nil
	})
	if err == nil {
		s.RunSiteTasks(ctx, from)
		s.RunSiteTasks(ctx, toSiteID)
	}
	return err
}
