package billing

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	"ispbill/internal/routeros"
)

// The ONU management VLAN leases (10.6.n.0/24) are read by the same sweep that reads the customer leases; the ACS
// asks here which MAC holds a management address, so a session does not cost a router read of its own.
const (
	onuLeaseFresh    = 10 * time.Minute // a snapshot this young answers without a read
	onuLeaseMinRead  = 60 * time.Second // at most one extra full read per building per minute
	onuLeaseMaxCache = 4096             // rows kept per building
)

type leaseSnap struct {
	at   time.Time
	byIP map[string]string // management address -> MAC (upper case, colon separated); "" never stored
}

type onuLeases struct {
	mu    sync.Mutex
	snap  map[string]leaseSnap // siteID
	tried map[string]time.Time // siteID -> last extra read
}

// noteMgmtLeases keeps the management-subnet rows of a lease read the caller already made.
func (s *Service) noteMgmtLeases(siteID, subnet string, rows []routeros.Row) {
	if subnet == "" {
		return
	}
	pfx, err := netip.ParsePrefix(subnet)
	if err != nil {
		return
	}
	m := map[string]string{}
	for _, r := range rows {
		ip, err := netip.ParseAddr(firstNonEmpty(r["active-address"], r["address"]))
		if err != nil || !pfx.Contains(ip) || len(m) >= onuLeaseMaxCache {
			continue
		}
		if st := r["status"]; st != "" && st != "bound" {
			continue // a waiting or offered lease is not a box that holds the address
		}
		mac, err := NormalizeMAC(firstNonEmpty(r["active-mac-address"], r["mac-address"]))
		if err != nil {
			continue
		}
		m[ip.String()] = mac
	}
	s.onuLeases.mu.Lock()
	if s.onuLeases.snap == nil {
		s.onuLeases.snap = map[string]leaseSnap{}
	}
	s.onuLeases.snap[siteID] = leaseSnap{at: s.now(), byIP: m}
	s.onuLeases.mu.Unlock()
}

// ONUMgmtLease says which MAC holds a management address on a building's router. ok is false when the answer could
// not be found out (no such building, router unreachable); ("", true) means the address is not leased to anyone.
func (s *Service) ONUMgmtLease(ctx context.Context, siteID, ip string) (mac string, ok bool) {
	site, found := s.site(siteID)
	if !found || site.MgmtSubnet() == "" {
		return "", false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "", false
	}
	ip = addr.String()
	look := func(fresh time.Duration) (string, bool) {
		s.onuLeases.mu.Lock()
		defer s.onuLeases.mu.Unlock()
		sn, have := s.onuLeases.snap[siteID]
		if !have || s.now().Sub(sn.at) > fresh {
			return "", false
		}
		return sn.byIP[ip], true
	}
	if mac, ok := look(onuLeaseFresh); ok {
		return mac, true
	}
	s.onuLeases.mu.Lock()
	if s.onuLeases.tried == nil {
		s.onuLeases.tried = map[string]time.Time{}
	}
	last, again := s.onuLeases.tried[siteID]
	if again && s.now().Sub(last) < onuLeaseMinRead {
		s.onuLeases.mu.Unlock()
		// too soon for another read: the old snapshot is still the best there is, if it is not ancient
		return look(time.Hour)
	}
	s.onuLeases.tried[siteID] = s.now()
	s.onuLeases.mu.Unlock()
	rows, err := s.router(siteID).Rows(ctx, pathLease, nil)
	if err != nil {
		return "", false
	}
	s.noteMgmtLeases(siteID, site.MgmtSubnet(), rows)
	return look(onuLeaseFresh)
}
