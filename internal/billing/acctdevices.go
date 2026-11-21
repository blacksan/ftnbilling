package billing

import (
	"time"
)

// AccountDevice is the MAC an account is bound to, as the "Devices on this account" card shows it: the
// maker (from the MAC prefix), whether the router saw it just now, when it last did, and the port it sits on.
type AccountDevice struct {
	MAC      string
	Vendor   string
	Online   bool
	LastSeen time.Time
	Port     string // from the router's bridge host table; "" when the router has not said
	Lease    string // bound | waiting | none
}

// AccountDevice reports the account's bound MAC. ok is false when the account has none (or does not exist).
// It reads what the last router pass learned; it never asks the router.
func (s *Service) AccountDevice(accountID string) (AccountDevice, bool) {
	a, ok := s.GetAccount(accountID)
	if !ok || a.MAC == "" {
		return AccountDevice{}, false
	}
	rs := s.AccountRouterState(accountID)
	last := rs.LastSeen
	ps := s.prov(a.SiteID)
	ps.mu.Lock()
	if t := ps.seen[a.Code]; t.After(last) {
		last = t
	}
	ps.mu.Unlock()
	return AccountDevice{
		MAC: a.MAC, Vendor: Vendor(a.MAC), LastSeen: last, Port: rs.Port, Lease: rs.LeaseStatus,
		Online: !last.IsZero() && s.now().Sub(last) <= OnlineWindow,
	}, true
}
