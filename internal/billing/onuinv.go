package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/store"
)

// ONU inventory flows. The equipment register already holds ONTs as serial + WAN MAC pairs; these functions make
// the ONU serial the identity of a house: assigning an ONU sets the house MAC from the pair, and a swap moves the
// house to another ONU in one step.

// ONTOfAccount is the ONT installed at a house.
func ONTOfAccount(d *store.Data, accountID string) (store.Equipment, bool) {
	var best store.Equipment
	found := false
	for _, e := range d.Equipment {
		if e.Kind == store.KindONT && e.AccountID == accountID && e.Status == store.EqInstalled {
			if !found || e.InstalledAt.After(best.InstalledAt) {
				best, found = e, true
			}
		}
	}
	return best, found
}

// installONTLocked puts an in-stock ONT at a house and, when it has a MAC, makes that the house MAC.
func (s *Service) installONTLocked(d *store.Data, equipID string, a store.Account, now time.Time) error {
	e, ok := d.Equipment[equipID]
	if !ok || e.Kind != store.KindONT {
		return ErrNotFound
	}
	if e.Status != store.EqInStock {
		return fmt.Errorf("this ONU is %s; only an ONU in stock can be given to a house", strings.ReplaceAll(e.Status, "_", " "))
	}
	if e.MAC == "" {
		return errors.New("this ONU has no WAN MAC in the register yet: add it first (the MAC the router sees, not the label's)")
	}
	if err := s.changeMACLocked(d, a.ID, e.MAC); err != nil {
		return err
	}
	from := e.Status
	e.AccountID, e.SiteID, e.Status, e.InstalledAt, e.UpdatedAt = a.ID, a.SiteID, store.EqInstalled, now, now
	d.Equipment[e.ID] = e
	addEqEvent(d, e, store.EqEvAssigned, from, e.Status, "", now)
	return nil
}

// SwapONU gives a house another ONU from the register. The old ONU leaves the house with oldStatus (in stock,
// faulty or returned); the house MAC becomes the new ONU's WAN MAC and the router is updated.
func (s *Service) SwapONU(ctx context.Context, accountID, newEquipID, oldStatus, note string) error {
	switch oldStatus {
	case store.EqInStock, store.EqFaulty, store.EqReturned, store.EqRetired:
	case "":
		oldStatus = store.EqInStock
	default:
		return errors.New("the old ONU can go back in stock, or be faulty, returned or retired")
	}
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		now := s.now()
		for _, e := range d.Equipment {
			if e.Kind == store.KindONT && e.AccountID == accountID && e.Status == store.EqInstalled && e.ID != newEquipID {
				returnLocked(d, e.ID, e, oldStatus, note, now)
			}
		}
		if cur, ok := d.Equipment[newEquipID]; ok && cur.AccountID == accountID && cur.Status == store.EqInstalled {
			return store.ErrNoChange
		}
		return s.installONTLocked(d, newEquipID, a, now)
	})
	if err == nil || errors.Is(err, store.ErrNoChange) {
		s.runAccountSite(ctx, accountID)
	}
	return err
}

// UseReportedMAC trusts the WAN MAC an ONU reported over TR-069 over the register: the house's ONT item and the
// house get that MAC. It is the one-click answer to a MAC mismatch warning.
func (s *Service) UseReportedMAC(ctx context.Context, accountID, rawMAC string) error {
	mac, err := NormalizeMAC(rawMAC)
	if err != nil {
		return err
	}
	if s.isRouterMAC(mac) {
		return ErrRouterOwnMAC
	}
	err = s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		for _, o := range d.Equipment {
			if o.Status != store.EqRetired && strings.EqualFold(o.MAC, mac) && !(o.Kind == store.KindONT && o.AccountID == accountID) {
				return fmt.Errorf("%w (%s %s)", ErrDuplicateEquipmentMAC, o.Kind, o.Serial)
			}
		}
		if e, ok := ONTOfAccount(d, accountID); ok {
			e.MAC, e.UpdatedAt = mac, s.now()
			d.Equipment[e.ID] = e
			addEqEvent(d, e, store.EqEvEdited, e.Status, e.Status, "WAN MAC set to the one the ONU reported: "+mac, s.now())
		}
		if err := s.changeMACLocked(d, a.ID, mac); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		s.runAccountSite(ctx, accountID)
	}
	return err
}
