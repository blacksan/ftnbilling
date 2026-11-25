package sites

import (
	"context"
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
	has, arpOK := false, false
	for _, r := range e.sn.tbl("/interface/vlan") {
		if r["name"] == p.Iface && r["interface"] == ONUMgmtParent(e.site) && r["vlan-id"] == strconv.Itoa(p.VLAN) && !rowDisabled(r) {
			has, arpOK = true, r["arp"] == "reply-only"
		}
	}
	if !has {
		missing = append(missing, "the VLAN "+p.Iface)
	} else if !arpOK {
		missing = append(missing, "ARP reply-only on "+p.Iface)
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
	has, addARP := false, false
	for _, r := range e.sn.tbl("/ip/dhcp-server") {
		if r["name"] == ONUMgmtDHCPServer && r["interface"] == p.Iface && !rowDisabled(r) {
			has, addARP = true, yes(r["add-arp"])
		}
	}
	if !has {
		missing = append(missing, "the DHCP server "+ONUMgmtDHCPServer)
	} else if !addARP {
		missing = append(missing, "add-arp on the DHCP server "+ONUMgmtDHCPServer)
	}
	fwd, in := p.Rules()
	have := map[string]bool{}
	for _, r := range e.sn.tbl("/ip/firewall/filter") {
		if !rowDisabled(r) {
			have[r["comment"]] = true
		}
	}
	// A rule that exists with the wrong ports (built before 58000 was allowed) is not "missing": it is healed in place.
	var drift []string
	for _, r := range append(fwd, in...) {
		if !have[r.Comment] {
			missing = append(missing, "the firewall rule \""+r.Comment+"\"")
			continue
		}
		if r.Comment == ACSToONUComment {
			if want := onuRuleDstPort(r); want != "" {
				for _, row := range e.sn.tbl("/ip/firewall/filter") {
					if row["comment"] == r.Comment && !rowDisabled(row) && !sameAdminValue("dst-port", row["dst-port"], want) {
						drift = append(drift, "the ports of the rule \""+r.Comment+"\" (now "+row["dst-port"]+", should be "+want+")")
					}
				}
			}
		}
	}
	if len(drift) > 0 && len(missing) == 0 {
		it.Evidence = append(it.Evidence, drift...)
		it.Status = StatusWarn
		it.Detail = "The firewall rule that lets the server reach the ONUs does not allow all connection-request ports (" + portList(p.CRPorts) + "). The server updates it."
		it.Fix = CheckFix{Kind: FixAuto, Label: "Update the ONU management firewall rule", Role: store.RoleOperator}
		return
	}
	missing = append(missing, drift...)
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

// onuRuleDstPort is the dst-port of a rule as written.
func onuRuleDstPort(r ipoeRule) string {
	for _, p := range r.Props {
		if p[0] == "dst-port" {
			return p[1]
		}
	}
	return ""
}

// fixONUMgmt sets the dst-port of the "ftn: acs to onu" rule right where the router has the rule with other
// ports (idempotent: set only when different). It never adds, removes or moves a rule.
func fixONUMgmt(ctx context.Context, fe *fixEnv) error {
	p, err := fe.m.ONUPlan(fe.site)
	if err != nil {
		return changed("%v", err)
	}
	fwd, _ := p.Rules()
	var want ipoeRule
	for _, r := range fwd {
		if r.Comment == ACSToONUComment {
			want = r
		}
	}
	port := onuRuleDstPort(want)
	if port == "" {
		return nil
	}
	sn, err := fe.freshRead(ctx, nil, []string{"/ip/firewall/filter"})
	if err != nil {
		return err
	}
	fe.w.onuRules = map[string]bool{ACSToONUComment: true}
	for _, row := range sn.tbl("/ip/firewall/filter") {
		if row["comment"] != ACSToONUComment || sameAdminValue("dst-port", row["dst-port"], port) {
			continue
		}
		if err := fe.w.setRow(ctx, "/ip/firewall/filter", row, map[string]string{"dst-port": port}); err != nil {
			return err
		}
		fe.w.note("set the ports of the rule %q to %s", ACSToONUComment, port)
	}
	return nil
}
