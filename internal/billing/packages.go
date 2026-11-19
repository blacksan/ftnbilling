package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ispbill/internal/store"
)

// SeedPackages creates the default packages if there are none.
func (s *Service) SeedPackages() error {
	return s.st.Update(func(d *store.Data) error {
		if len(d.Packages) > 0 {
			return store.ErrNoChange
		}
		defaults := []store.Package{
			{Name: "1 Hour", DurationMin: 60, SpeedMbps: 5, PriceKES: 10},
			{Name: "1 Day", DurationMin: 1440, SpeedMbps: 10, PriceKES: 50},
			{Name: "1 Week", DurationMin: 10080, SpeedMbps: 10, PriceKES: 250},
			{Name: "Monthly 20 Mbps", DurationMin: 43200, SpeedMbps: 20, PriceKES: 2000},
			{Name: "Monthly 40 Mbps", DurationMin: 43200, SpeedMbps: 40, PriceKES: 3000},
			{Name: "Monthly 80 Mbps", DurationMin: 43200, SpeedMbps: 80, PriceKES: 4000},
		}
		for i, p := range defaults {
			p.ID, p.Slug, p.Sort = RandomID(), Slugify(p.Name), (i+1)*10
			d.Packages[p.ID] = p
		}
		return nil
	})
}

// ErrPriceCollision is returned when two enabled packages would cost the same
// at one building: Paybill payments are matched to a package by amount.
var ErrPriceCollision = errors.New("two enabled packages would have the same price")

// priceCollision checks that, in every building, no two enabled effective
// packages share a price. It looks at pkgs (the candidate list) with each
// building's overrides applied.
func priceCollision(d *store.Data, pkgs map[string]store.Package, sites map[string]store.Site) error {
	check := func(siteID, label string) error {
		seen := map[int]string{}
		for _, p := range pkgs {
			if p.Deleted {
				continue
			}
			if o, ok := sites[siteID].PkgOverrides[p.ID]; ok {
				if o.PriceKES > 0 {
					p.PriceKES = o.PriceKES
				}
				p.Disabled = p.Disabled || o.Hidden
			}
			if p.Disabled || p.PriceKES <= 0 {
				continue
			}
			if other, dup := seen[p.PriceKES]; dup {
				return fmt.Errorf("%w: %s and %s both cost KES %d%s. Change a price, hide one here, or disable one", ErrPriceCollision, other, p.Name, p.PriceKES, label)
			}
			seen[p.PriceKES] = p.Name
		}
		return nil
	}
	if err := check("", ""); err != nil {
		return err
	}
	for id, st := range sites {
		if st.Removed {
			continue
		}
		if err := check(id, " at "+st.Name); err != nil {
			return err
		}
	}
	return nil
}

// SavePackage adds (ID empty) or edits a package and pushes its profile to
// every building's router in the background. The slug, and so the router
// profile name, never changes after creation. Saving is refused when it would
// give two enabled packages the same price at any building.
func (s *Service) SavePackage(ctx context.Context, p store.Package) (store.Package, error) {
	if p.Name == "" || p.DurationMin <= 0 || p.SpeedMbps <= 0 || p.PriceKES < 0 {
		return p, errors.New("name, duration, speed must be set and price cannot be negative")
	}
	err := s.st.Update(func(d *store.Data) error {
		cand := make(map[string]store.Package, len(d.Packages)+1)
		for k, v := range d.Packages {
			cand[k] = v
		}
		if p.ID == "" {
			p.ID = RandomID()
			base, slug := Slugify(p.Name), ""
			for i := 1; ; i++ {
				slug = base
				if i > 1 {
					slug = fmt.Sprintf("%s-%d", base, i)
				}
				clash := false
				for _, o := range d.Packages {
					clash = clash || o.Slug == slug
				}
				if !clash {
					break
				}
			}
			p.Slug = slug
			p.Sort = (len(d.Packages) + 1) * 10
		} else {
			old, ok := d.Packages[p.ID]
			if !ok || old.Deleted {
				return ErrNotFound
			}
			p.Slug, p.Sort = old.Slug, old.Sort
		}
		cand[p.ID] = p
		if err := priceCollision(d, cand, d.Sites); err != nil {
			return err
		}
		d.Packages[p.ID] = p
		return nil
	})
	if err != nil {
		return p, err
	}
	s.ResyncAll()
	return p, nil
}

// SaveSiteOverrides replaces a building's package overrides (hidden flag and
// price per package ID). Zero prices and no-op rows are dropped. It is refused
// when it would give two enabled packages the same price at this building.
func (s *Service) SaveSiteOverrides(siteID string, ov map[string]store.PkgOverride) error {
	return s.st.Update(func(d *store.Data) error {
		st, ok := d.Sites[siteID]
		if !ok || st.Removed {
			return ErrNoSite
		}
		clean := map[string]store.PkgOverride{}
		for id, o := range ov {
			if pk, ok := d.Packages[id]; !ok || pk.Deleted {
				continue
			}
			if o.PriceKES < 0 {
				return errors.New("a price cannot be negative")
			}
			if o.Hidden || o.PriceKES > 0 {
				clean[id] = o
			}
		}
		st.PkgOverrides = clean
		sites := map[string]store.Site{siteID: st}
		if err := priceCollision(d, d.Packages, sites); err != nil {
			return err
		}
		d.Sites[siteID] = st
		return nil
	})
}

// ---- vouchers -----------------------------------------------------------

// GenerateVouchers makes n unique 8-char single-use vouchers; returns the batch id.
// siteID "" makes vouchers that work at any building.
func (s *Service) GenerateVouchers(pkgID, siteID string, n int) (string, []store.Voucher, error) {
	if n < 1 || n > 500 {
		return "", nil, errors.New("count must be between 1 and 500")
	}
	var out []store.Voucher
	batch := s.now().In(EAT).Format("20060102-1504") + "-" + RandomCode(3)
	err := s.st.Update(func(d *store.Data) error {
		if p, ok := d.Packages[pkgID]; !ok || p.Deleted {
			return ErrNotFound
		}
		if siteID != "" {
			if st, ok := d.Sites[siteID]; !ok || st.Removed {
				return ErrNoSite
			}
		}
		now := s.now()
		for len(out) < n {
			c := RandomCode(8)
			if _, dup := d.Vouchers[c]; dup {
				continue
			}
			v := store.Voucher{Code: c, SiteID: siteID, PackageID: pkgID, Batch: batch, CreatedAt: now}
			d.Vouchers[c] = v
			out = append(out, v)
		}
		return nil
	})
	return batch, out, err
}

// LookupVoucher returns an unused voucher.
func (s *Service) LookupVoucher(code string) (v store.Voucher, err error) {
	s.st.View(func(d *store.Data) {
		var ok bool
		v, ok = d.Vouchers[NormalizeCode(code)]
		if !ok || v.Used {
			err = ErrVoucherUsed
		}
	})
	return
}

// Remaining formats the time left until t as "2 h 5 min".
func Remaining(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "expired"
	}
	m := int(d.Minutes())
	switch {
	case m >= 1440:
		return fmt.Sprintf("%d d %d h", m/1440, (m%1440)/60)
	case m >= 60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d min", m+1)
}

// ResyncAll starts a router reconcile per building in the background
// (15 s limit each), so a changed package speed reaches the flats' queues. It
// never blocks the caller; the sweeper retries a failed pass.
func (s *Service) ResyncAll() {
	for _, id := range s.runnableSites() {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := s.ReconcileSite(ctx, id); err != nil {
				s.log.Warn("router reconcile after a package change failed; will retry", "site", id, "err", err)
			}
		}()
	}
}
