package sites

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// ONUPlan is the management network plan of a site with the server's ACS settings applied.
func (m *Manager) ONUPlan(s store.Site) (ONUMgmtPlan, error) {
	port, url := m.ACSAddress()
	return PlanONUMgmt(s, ServerTunnelIP(m.pfx), port, url, m.cfg.ACSCRPorts)
}

// ACSAddress is the ACS port and the URL the ONUs are given: the server's tunnel address on the ACS port.
func (m *Manager) ACSAddress() (port int, url string) {
	port = DefaultACSPort
	if _, ps, err := net.SplitHostPort(m.cfg.ACSListen); err == nil {
		if n, err := strconv.Atoi(ps); err == nil && n > 0 {
			port = n
		}
	}
	scheme := "http"
	if m.cfg.ACSTLSCert != "" {
		scheme = "https"
	}
	return port, fmt.Sprintf("%s://%s:%d/", scheme, ServerTunnelIP(m.pfx), port)
}

// ONUMgmtEnabled says whether the server runs the ACS (the management network is then part of every new script).
func (m *Manager) ONUMgmtEnabled() bool { return m.cfg.ACSEnabled() }

// EnsureONUMgmt gives the router its management subnet number (once) and rewrites the WireGuard peers so the
// server routes the subnet to it. It returns the router. vlan > 0 sets the management VLAN (0 keeps it).
func (m *Manager) EnsureONUMgmt(siteID string, vlan int) (store.Site, error) {
	if vlan < 0 || vlan > 4094 || vlan == 1 {
		return store.Site{}, errors.New("the ONU management VLAN must be 2 to 4094")
	}
	var out store.Site
	err := m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		if vlan > 0 {
			if vlan == s.VLANID {
				return fmt.Errorf("VLAN %d is the customer VLAN of this router; use another VLAN for TR-069 management", vlan)
			}
			s.ONUMgmtVLAN = vlan
			if vlan == store.DefaultONUMgmtVLAN {
				s.ONUMgmtVLAN = 0
			}
		}
		if s.ONUMgmtN == 0 {
			n, ok := d.NextONUMgmtN()
			if !ok {
				return errors.New("no ONU management subnet is left (10.6.1.0/24 to 10.6.255.0/24 are all in use)")
			}
			s.ONUMgmtN = n
		}
		if s.MgmtVLANOrDefault() == s.VLANID {
			return fmt.Errorf("the ONU management VLAN %d is the customer VLAN of this router", s.MgmtVLANOrDefault())
		}
		if equalSite(d.Sites[siteID], s) {
			out = s
			return store.ErrNoChange
		}
		d.Sites[siteID] = s
		out = s
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNoChange) {
		return store.Site{}, err
	}
	if err := m.ApplyPeers(); err != nil {
		m.log.Warn("could not write the WireGuard peers file", "err", err)
	}
	return out, nil
}

func equalSite(a, b store.Site) bool {
	return a.ONUMgmtN == b.ONUMgmtN && a.ONUMgmtVLAN == b.ONUMgmtVLAN
}

// ONUMgmtNote is what CheckONUMgmt looks for on the router.
func evalONUMgmt(e *evalCtx, it *CheckItem) {
	if e.site.ONUMgmtN == 0 {
		it.Status = StatusInfo
		it.Detail = "ONU management (TR-069) is not set up on this router. Open the ONUs tab to enable it."
		return
	}
	if e.unknown(it, CheckONUMgmt) {
		return
	}
	p, err := e.m.ONUPlan(e.site)
	if err != nil {
		it.Status, it.Detail = StatusWarn, err.Error()
		return
	}
	var missing []string
	has := false
	for _, r := range e.sn.tbl("/interface/vlan") {
		if r["name"] == p.Iface && r["interface"] == ONUMgmtParent(e.site) && r["vlan-id"] == strconv.Itoa(p.VLAN) && !rowDisabled(r) {
			has = true
		}
	}
	if !has {
		missing = append(missing, "the VLAN "+p.Iface)
	}
	has = false
	for _, r := range e.sn.tbl("/ip/address") {
		if r["interface"] == p.Iface && r["address"] == p.GatewayCIDR && !rowDisabled(r) {
			has = true
		}
	}
	if !has {
		missing = append(missing, "the address "+p.GatewayCIDR)
	}
	has = false
	for _, r := range e.sn.tbl("/ip/dhcp-server") {
		if r["name"] == ONUMgmtDHCPServer && r["interface"] == p.Iface && !rowDisabled(r) {
			has = true
		}
	}
	if !has {
		missing = append(missing, "the DHCP server "+ONUMgmtDHCPServer)
	}
	fwd, in := p.Rules()
	have := map[string]bool{}
	for _, r := range e.sn.tbl("/ip/firewall/filter") {
		if !rowDisabled(r) {
			have[r["comment"]] = true
		}
	}
	for _, r := range append(fwd, in...) {
		if !have[r.Comment] {
			missing = append(missing, "the firewall rule \""+r.Comment+"\"")
		}
	}
	it.Evidence = append(it.Evidence, fmt.Sprintf("%s %s VLAN %d on %s", p.Iface, p.Subnet, p.VLAN, ONUMgmtParent(e.site)))
	if len(missing) == 0 {
		okItem(it, "The ONU management network "+p.Subnet+" (VLAN "+strconv.Itoa(p.VLAN)+") is in place on the router.")
		return
	}
	it.Status = StatusWarn
	it.Detail = "The router lacks part of the ONU management network: " + joinAnd(missing) + ". A new set-up line builds it, or paste the commands shown on the ONUs tab (no rebuild needed)."
	it.Fix = CheckFix{Kind: FixManual}
}

func joinAnd(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	out := ""
	for i, v := range s {
		switch {
		case i == 0:
			out = v
		case i == len(s)-1:
			out += " and " + v
		default:
			out += ", " + v
		}
	}
	return out
}
