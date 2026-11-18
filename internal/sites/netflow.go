package sites

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"time"

	"ispbill/internal/store"
)

// ListenNetflow receives NetFlow v5 on one UDP socket for every building and
// hands each datagram to the right collector. addr "" or "off" disables it.
// The bound address is sent on ready (useful with ":0" in tests).
func (m *Manager) ListenNetflow(ctx context.Context, addr string, ready chan<- net.Addr) error {
	if addr == "" || strings.EqualFold(addr, "off") {
		return nil
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	defer pc.Close()
	if ready != nil {
		ready <- pc.LocalAddr()
	}
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		m.HandleNetflowPacket(time.Now(), buf[:n], from.String())
	}
}

// HandleNetflowPacket routes one datagram by its source address. Every router
// peer is allowed only its own /32 tunnel address, so the source address
// identifies (and authenticates) the building. A source that is no building's
// tunnel address goes to the env-mode building if there is one (a router that
// exports over the public internet, as before); otherwise it is counted as
// stray and dropped. It returns the building that took it ("" if dropped).
func (m *Manager) HandleNetflowPacket(now time.Time, pkt []byte, from string) string {
	var ip string
	if ap, err := netip.ParseAddrPort(from); err == nil {
		ip = ap.Addr().Unmap().String()
	} else if a, err := netip.ParseAddr(from); err == nil {
		ip = a.Unmap().String()
	}
	var siteID string
	m.st.View(func(d *store.Data) {
		if s, ok := d.SiteByTunnelIP(ip); ok {
			siteID = s.ID
		} else if s, ok := d.EnvSite(); ok {
			siteID = s.ID
		}
	})
	col := m.Collector(siteID)
	if siteID == "" || col == nil {
		m.stray.Add(1)
		return ""
	}
	col.HandleNetflow(now, pkt, from)
	return siteID
}
