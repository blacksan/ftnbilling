// Package sites runs one building ("site") at a time: its router client,
// metrics collector and host guard, the WireGuard peers, the one-paste join
// script and the shared NetFlow socket.
package sites

import (
	"context"
	"sync"
	"time"
)

// HostGuard answers "is this mac/ip a device on this building's hotspot right
// now?" from the router's /ip/hotspot/host table. The portal is public, so
// without it anyone on the internet could open /portal?mac=<a flat's ONT MAC>.
// There is one guard per building: every building uses the same customer
// subnet, so a lookup is only meaningful against that building's own router.
type HostGuard struct {
	mu      sync.Mutex
	at      time.Time // last successful refresh
	tried   time.Time // last attempt (successful or not)
	hosts   map[string]string
	fetch   func(ctx context.Context) (map[string]string, error)
	maxAge  time.Duration
	minWait time.Duration
}

// NewHostGuard builds a guard around a function that lists the router's
// hotspot hosts as normalised MAC -> IP.
func NewHostGuard(fetch func(ctx context.Context) (map[string]string, error)) *HostGuard {
	return &HostGuard{maxAge: 20 * time.Second, minWait: time.Second, fetch: fetch}
}

// OK reports whether mac (normalised) is on the hotspot, and when ip is set,
// whether it has that address. A miss refreshes the table (at most every
// minWait), so a phone that just joined is found on its first page load. It
// fails closed when the router cannot be reached.
func (g *HostGuard) OK(ctx context.Context, mac, ip string) bool {
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
