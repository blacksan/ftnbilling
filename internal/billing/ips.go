package billing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"ispbill/internal/netplan"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// ErrNoFreeIP means a building's static range (.60 to the last host of its
// customer subnet) is full.
var ErrNoFreeIP = errors.New("no free address left in this building's static range")

// defaultCustomerSubnet is the subnet a building without its own setting uses
// (the same default as sites.ApplyDefaults).
const defaultCustomerSubnet = "10.5.50.0/24"

// layoutSource is implemented by *sites.Manager: it fills in the router layout
// defaults of a building (customer subnet and so on).
type layoutSource interface {
	Effective(store.Site) store.Site
}

// PlanFor is the address plan of a building's customer subnet.
func (s *Service) PlanFor(site store.Site) (netplan.Plan, error) {
	if ls, ok := s.routers.(layoutSource); ok {
		site = ls.Effective(site)
	}
	sub := site.CustomerSubnet
	if strings.TrimSpace(sub) == "" {
		sub = defaultCustomerSubnet
	}
	return netplan.PlanSubnet(sub)
}

// FreeIPLocked returns the lowest unused address of the building's static
// range. Every open account counts, flagged ones too (their address is still
// theirs). Call it inside Update or View with d.
func FreeIPLocked(d *store.Data, siteID string, pl netplan.Plan, avoid ...map[string]bool) (string, error) {
	used := map[string]bool{pl.Gateway: true}
	for _, m := range avoid { // addresses of the router's own static leases
		for ip := range m {
			used[ip] = true
		}
	}
	for _, a := range d.Accounts {
		if a.SiteID == siteID && a.IP != "" {
			used[a.IP] = true
		}
	}
	ip, ok := pl.FirstFree(func(ip string) bool { return used[ip] })
	if !ok {
		return "", ErrNoFreeIP
	}
	return ip, nil
}

// EnsureIPsLocked gives every open account of the building that has no address
// one from the static range (accounts are taken in flat order, so the first
// flat gets the lowest address). It returns how many were allocated.
//
// An address outside the static range (the unregistered pool .10 to .59, or
// anything else) does not count as an address: such a flat would be walled
// the moment the router walls the unregistered pool, so it gets a fresh one.
func EnsureIPsLocked(d *store.Data, siteID string, pl netplan.Plan, avoid ...map[string]bool) (int, error) {
	n := 0
	for _, a := range d.AccountList() {
		if a.SiteID != siteID || (a.IP != "" && pl.InStatic(a.IP)) {
			continue
		}
		if a.IP != "" { // free the unusable address before searching
			a.IP = ""
			d.Accounts[a.ID] = a
		}
		ip, err := FreeIPLocked(d, siteID, pl, avoid...)
		if err != nil {
			return n, err
		}
		a.IP = ip
		d.Accounts[a.ID] = a
		n++
	}
	return n, nil
}

// EnsureIPs allocates addresses for the building's flats that have none.
func (s *Service) EnsureIPs(siteID string) (int, error) {
	site, ok := s.site(siteID)
	if !ok {
		return 0, ErrNoSite
	}
	pl, err := s.PlanFor(site)
	if err != nil {
		return 0, err
	}
	n := 0
	err = s.st.Update(func(d *store.Data) error {
		var e error
		if n, e = EnsureIPsLocked(d, siteID, pl, s.foreignIPs(siteID)); n == 0 && e == nil {
			return store.ErrNoChange
		}
		return e
	})
	return n, err
}

// AdoptResult says what AdoptLeases did.
type AdoptResult struct {
	Adopted   []string // "Flat 3B: 10.5.50.23"
	Unchanged int      // already had that address
	Skipped   []string // plain-words reasons
}

// AdoptLeases imports the router's current DHCP leases into the accounts: for each flat whose MAC holds a lease inside the
// building's customer subnet, the flat's IP becomes the leased address, so the
// ONT keeps its address when the static lease replaces the dynamic one. A flat
// whose address would clash with another flat, or that has no lease, keeps
// what it has (it gets a fresh address from the static range later).
func (s *Service) AdoptLeases(ctx context.Context, siteID string) (AdoptResult, error) {
	var res AdoptResult
	site, ok := s.site(siteID)
	if !ok {
		return res, ErrNoSite
	}
	pl, err := s.PlanFor(site)
	if err != nil {
		return res, err
	}
	rows, err := s.router(siteID).Rows(ctx, pathLease, nil)
	if err != nil {
		return res, err
	}
	// MAC -> address of the freshest bound lease inside the subnet.
	leased := map[string]string{}
	for _, r := range rows {
		mac, err := NormalizeMAC(r["mac-address"])
		if err != nil {
			continue
		}
		ip := firstNonEmpty(r["active-address"], r["address"])
		if !pl.InNetwork(ip) || ip == pl.Gateway {
			continue
		}
		if cur, had := leased[mac]; had && r["status"] != "bound" && cur != "" {
			continue
		}
		leased[mac] = ip
	}
	err = s.st.Update(func(d *store.Data) error {
		changed := false
		owner := map[string]string{} // ip -> account id, for this building
		for _, a := range d.Accounts {
			if a.SiteID == siteID && a.IP != "" {
				owner[a.IP] = a.ID
			}
		}
		for _, a := range d.AccountList() {
			if a.SiteID != siteID {
				continue
			}
			if a.MACConflict || a.MAC == "" {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s: MAC is flagged or missing, not adopted", a.Flat))
				continue
			}
			ip, has := leased[a.MAC]
			if !has {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s: no lease seen for %s", a.Flat, a.MAC))
				continue
			}
			if !pl.InStatic(ip) {
				// The unregistered pool (.10 to .59) is walled:
				// a flat must not keep an address from it. It gets a static one.
				if a.IP == "" || !pl.InStatic(a.IP) {
					fresh, ferr := FreeIPLocked(d, siteID, pl, s.foreignIPs(siteID))
					if ferr != nil {
						res.Skipped = append(res.Skipped, fmt.Sprintf("%s: leased %s is outside the static range and none is free", a.Flat, ip))
						continue
					}
					delete(owner, a.IP)
					a.IP = fresh
					owner[fresh] = a.ID
					d.Accounts[a.ID] = a
					changed = true
					res.Adopted = append(res.Adopted, fmt.Sprintf("%s: %s (leased %s is in the unregistered range, so a static address was given; the ONT picks it up when it renews or restarts)", a.Flat, fresh, ip))
					continue
				}
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s: leased %s is in the unregistered range, keeps %s", a.Flat, ip, a.IP))
				continue
			}
			if a.IP == ip {
				res.Unchanged++
				continue
			}
			if o, taken := owner[ip]; taken && o != a.ID {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %s is already the address of another flat", a.Flat, ip))
				continue
			}
			if a.IP != "" {
				delete(owner, a.IP)
			}
			a.IP = ip
			owner[ip] = a.ID
			d.Accounts[a.ID] = a
			res.Adopted = append(res.Adopted, fmt.Sprintf("%s: %s", a.Flat, ip))
			changed = true
		}
		if changed {
			d.AddEvent(store.Event{Time: s.now(), SiteID: siteID, Kind: "audit",
				Msg: fmt.Sprintf("Leases adopted from the router: %d flat(s) keep their current address", len(res.Adopted))})
			return nil
		}
		return store.ErrNoChange
	})
	sort.Strings(res.Adopted)
	return res, err
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ipLess orders dotted-quad strings numerically.
func ipLess(a, b string) bool {
	x, e1 := netip.ParseAddr(a)
	y, e2 := netip.ParseAddr(b)
	if e1 != nil || e2 != nil {
		return a < b
	}
	return x.Less(y)
}

var _ = routeros.RowID

// readdressClashes gives every open flat of the building whose address is held by one of the router's own
// static leases a fresh address from the static range, and queues its router work.
func (s *Service) readdressClashes(siteID string, pl netplan.Plan, foreignList []string) {
	if len(foreignList) == 0 {
		return
	}
	foreign := make(map[string]bool, len(foreignList))
	for _, ip := range foreignList {
		foreign[ip] = true
	}
	_ = s.st.Update(func(d *store.Data) error {
		changed := false
		for _, a := range d.AccountList() {
			if a.SiteID != siteID || a.IP == "" || !foreign[a.IP] {
				continue
			}
			fresh, err := FreeIPLocked(d, siteID, pl, foreign)
			if err != nil {
				continue
			}
			old := a.IP
			a.IP = fresh
			d.Accounts[a.ID] = a
			s.enqueueLocked(d, a, "address changed")
			d.AddEvent(store.Event{Time: s.now(), SiteID: siteID, Kind: "info", AccountID: a.ID,
				Msg: fmt.Sprintf("%s (%s): address %s is held by a static lease of the router, moved to %s", a.Flat, a.Code, old, fresh)})
			changed = true
		}
		if !changed {
			return store.ErrNoChange
		}
		return nil
	})
}

// foreignIPs are the addresses of the router's own static leases seen by the
// last pass: they are not handed to a flat.
func (s *Service) foreignIPs(siteID string) map[string]bool {
	ps := s.prov(siteID)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make(map[string]bool, len(ps.foreign))
	for ip := range ps.foreign {
		out[ip] = true
	}
	return out
}

// EnqueueSite queues a router task for every open flat of the building (after a
// router was set up or reset: it must be told about all of them again).
func (s *Service) EnqueueSite(siteID, reason string) {
	_ = s.st.Update(func(d *store.Data) error {
		n := 0
		for _, a := range d.AccountList() {
			if a.SiteID == siteID {
				s.enqueueLocked(d, a, reason)
				n++
			}
		}
		if n == 0 {
			return store.ErrNoChange
		}
		return nil
	})
}

// ForgetSite drops what the reconciler remembers of a building's router (its
// last pass, the addresses of the router's own leases, devices seen): the
// router was reset and the next pass starts from nothing.
func (s *Service) ForgetSite(siteID string) {
	ps := s.prov(siteID)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.status = ProvisionStatus{}
	ps.accts = map[string]acctSnap{}
	ps.seen = map[string]time.Time{}
	ps.started = time.Time{}
	ps.foreign = nil
	ps.sawOn = false
}
