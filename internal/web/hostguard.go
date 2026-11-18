package web

import (
	"context"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
)

// hostGuard answers "is this mac/ip a device on the building hotspot right
// now?" from the router's /ip/hotspot/host table. It is only used when the
// portal runs on a public server (VERIFY_HOTSPOT_HOST=true): without it,
// anyone on the internet could open /portal?mac=<a flat's ONT MAC>.
type hostGuard struct {
	mu      sync.Mutex
	at      time.Time // last successful refresh
	tried   time.Time // last attempt (successful or not)
	hosts   map[string]string
	fetch   func(ctx context.Context) (map[string]string, error)
	maxAge  time.Duration
	minWait time.Duration
}

func newHostGuard(svc *billing.Service) *hostGuard {
	return &hostGuard{
		maxAge: 20 * time.Second, minWait: time.Second,
		fetch: func(ctx context.Context) (map[string]string, error) {
			hs, err := svc.HotspotHosts(ctx)
			if err != nil {
				return nil, err
			}
			m := make(map[string]string, len(hs))
			for _, h := range hs {
				if mac, err := billing.NormalizeMAC(h.MAC); err == nil {
					m[mac] = strings.TrimSpace(h.Address)
				}
			}
			return m, nil
		},
	}
}

// ok reports whether mac (normalised) is on the hotspot, and when ip is set,
// whether it has that address. A miss refreshes the table (at most every
// minWait), so a phone that just joined is found on its first page load.
func (g *hostGuard) ok(ctx context.Context, mac, ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	match := func() bool {
		addr, found := g.hosts[mac]
		return found && (ip == "" || addr == ip)
	}
	now := time.Now()
	if g.hosts != nil && now.Sub(g.at) < g.maxAge && match() {
		return true
	}
	if now.Sub(g.tried) < g.minWait {
		return g.hosts != nil && match()
	}
	g.tried = now
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m, err := g.fetch(ctx)
	if err != nil {
		return false // router unreachable: fail closed
	}
	g.hosts, g.at = m, now
	return match()
}
