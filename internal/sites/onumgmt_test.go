package sites

import (
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func onuSite() store.Site {
	s := benchSite()
	s.ONUMgmtN = 3
	return s
}

func TestPlanONUMgmt(t *testing.T) {
	p, err := PlanONUMgmt(onuSite(), "10.99.0.1", 7547, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subnet != "10.6.3.0/24" || p.GatewayCIDR != "10.6.3.1/24" || p.Iface != "vlan99-onu" || p.VLAN != 99 || p.ACSURL != "http://10.99.0.1:7547/" || len(p.CRPorts) != 3 {
		t.Fatalf("plan: %+v", p)
	}
	// option 43: sub-option 1, length, the URL.
	if !strings.HasPrefix(p.OptionValue, "0x0116687474703a2f2f") {
		t.Fatalf("option 43 %q", p.OptionValue)
	}
	if _, err := PlanONUMgmt(store.Site{}, "10.99.0.1", 7547, "", nil); err != ErrNoONUMgmt {
		t.Fatalf("no subnet: %v", err)
	}
	s := onuSite()
	s.ONUMgmtVLAN = s.VLANID
	if _, err := PlanONUMgmt(s, "10.99.0.1", 7547, "", nil); err == nil || !strings.Contains(err.Error(), "customer VLAN") {
		t.Fatalf("management VLAN equal to the customer VLAN must be refused: %v", err)
	}
	s.ONUMgmtVLAN = 1
	if _, err := PlanONUMgmt(s, "10.99.0.1", 7547, "", nil); err == nil {
		t.Fatal("VLAN 1 must be refused")
	}
}

// The management firewall: the subnet may reach the ACS and the router's NTP and DHCP (no DNS: the ACS URL is an address), and nothing else;
// the server may reach the subnet on the connection-request ports only. Nothing opens the internet.
func TestONUMgmtRulesAreTight(t *testing.T) {
	p, _ := PlanONUMgmt(onuSite(), "10.99.0.1", 7547, "", []int{7547, 30005})
	fwd, in := p.Rules()
	text := ""
	for _, r := range append(fwd, in...) {
		text += r.Args() + " # " + r.Comment + "\n"
	}
	for _, want := range []string{
		"in-interface=vlan99-onu out-interface=wg-ftn src-address=10.6.3.0/24 dst-address=10.99.0.1/32 dst-port=7547",
		"in-interface=wg-ftn out-interface=vlan99-onu src-address=10.99.0.1/32 dst-address=10.6.3.0/24 dst-port=7547,30005",
		"chain=forward action=drop in-interface=vlan99-onu", "chain=forward action=drop out-interface=vlan99-onu",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	for _, r := range fwd[:2] {
		if r.Table != "filter" || !strings.Contains(r.Args(), "protocol=tcp") || !strings.Contains(r.Args(), "dst-port=") || !strings.Contains(r.Args(), "-address=") {
			t.Errorf("an accept must name protocol, ports and both addresses: %s", r.Args())
		}
	}
	for _, r := range in {
		if !strings.Contains(r.Args(), "in-interface=vlan99-onu") || !strings.Contains(r.Args(), "dst-port=") {
			t.Errorf("an input accept must be bound to the management VLAN and a port: %s", r.Args())
		}
	}
	// The generated firewall places the forward accepts above the tunnel drops and the input accepts above the final drop.
	site := onuSite()
	in2 := benchInput(site)
	in2.ACSURL = "http://10.99.0.1:7547/"
	out, err := RenderBootstrap(in2)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	idx := func(sub string) int { return strings.Index(s, sub) }
	if !(idx(`comment="ftn: onu to acs"`) < idx(`comment="ftn: wg no forward in"`) && idx(`comment="ftn: acs to onu"`) < idx(`comment="ftn: wg no forward out"`)) {
		t.Fatal("the ONU accepts must come before the tunnel drops")
	}
	if !(idx(`comment="ftn: onu ntp"`) < idx(`comment="ftn: input drop rest"`)) {
		t.Fatal("the ONU input accepts must come before the final input drop")
	}
	// The interface exists (section 4b) before the firewall (section 6) names it.
	if !(idx("/interface/vlan add name=vlan99-onu") < idx(`in-interface=vlan99-onu`) && idx(":put \"ftn 4b") < idx(":put \"ftn 6/12")) {
		t.Fatal("the VLAN interface must be created before a rule names it")
	}
}

func TestBootstrapWithoutONUMgmtIsUnchanged(t *testing.T) {
	out, err := RenderBootstrap(benchInput(benchSite()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "ftn: onu") || strings.Contains(string(out), "-onu") {
		t.Fatal("a router with no management subnet gets no ONU objects")
	}
}

func TestONUMgmtOverlapRefused(t *testing.T) {
	for name, mut := range map[string]func(*store.Site){
		"customer net": func(s *store.Site) { s.CustomerSubnet = "10.6.3.0/24" },
		"admin LAN":    func(s *store.Site) { s.LANSubnet = "10.6.3.0/24" },
		"vlan clash":   func(s *store.Site) { s.ONUMgmtVLAN = s.VLANID },
	} {
		s := onuSite()
		mut(&s)
		if _, err := RenderBootstrap(benchInput(s)); err == nil {
			t.Errorf("%s: a layout that overlaps the management network was rendered", name)
		}
	}
}

func TestONUMgmtSnippetPlacesRules(t *testing.T) {
	p, _ := PlanONUMgmt(onuSite(), "10.99.0.1", 7547, "", nil)
	sn := p.Snippet("sfp-sfpplus1")
	for _, want := range []string{"/interface/vlan add name=vlan99-onu vlan-id=99 interface=sfp-sfpplus1", `place-before=[find where comment="ftn: wg no forward in"]`,
		`place-before=[find where comment="ftn: input drop rest"]`, "dhcp-option=ftn-onu-acs"} {
		if !strings.Contains(sn, want) {
			t.Errorf("snippet lacks %q", want)
		}
	}
	if strings.Contains(sn, "/user") || strings.Contains(sn, "wireguard") || strings.Contains(sn, "/ip/route") || strings.Contains(sn, "/ip/service") {
		t.Error("the snippet must not touch users, wireguard, routes or services")
	}
}

// The management subnet is added to the router's AllowedIPs on the server (and removed routers give theirs up).
func TestONUMgmtPeersAndAllocation(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.ACSListen = "10.99.0.1:7547"; c.ACSCRPorts = []int{7547, 30005} })
	a, b := f.joined("Block A"), f.joined("Block B")
	a, b = f.site(a.ID), f.site(b.ID)
	if a.ONUMgmtN == 0 || b.ONUMgmtN == 0 || a.ONUMgmtN == b.ONUMgmtN {
		t.Fatalf("each router gets its own management subnet: %d %d", a.ONUMgmtN, b.ONUMgmtN)
	}
	peers, _ := os.ReadFile(filepath.Join(f.cfg.DataDir, "wg", "peers.conf"))
	for _, s := range []store.Site{a, b} {
		if !strings.Contains(string(peers), "AllowedIPs = "+s.TunnelIP+"/32, "+s.MgmtSubnet()) {
			t.Errorf("peers.conf lacks the management subnet of %s:\n%s", s.Name, peers)
		}
	}
	// Enabling on a router built before: allocates once, keeps the number, sets the VLAN.
	c := f.create("Old router")
	c1, err := f.m.EnsureONUMgmt(c.ID, 0)
	if err != nil || c1.ONUMgmtN == 0 {
		t.Fatalf("%v %+v", err, c1)
	}
	c2, err := f.m.EnsureONUMgmt(c.ID, 120)
	if err != nil || c2.ONUMgmtN != c1.ONUMgmtN || c2.ONUMgmtVLAN != 120 {
		t.Fatalf("%v %+v", err, c2)
	}
	if _, err := f.m.EnsureONUMgmt(c.ID, 1); err == nil {
		t.Fatal("VLAN 1 must be refused")
	}
}

func TestONUMgmtCheck(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.ACSListen = "10.99.0.1:7547"; c.ACSCRPorts = []int{7547, 30005} })
	site := onuSite()
	site.OLTInterface = "sfp-sfpplus1"
	p, _ := f.m.ONUPlan(site)
	fwd, in := p.Rules()
	sn := newSnapshot()
	sn.rows["/interface/vlan"] = []routeros.Row{{"name": p.Iface, "vlan-id": "99", "interface": "sfp-sfpplus1", "disabled": "false", "arp": "reply-only"}}
	sn.rows["/ip/address"] = []routeros.Row{{"address": p.GatewayCIDR, "interface": p.Iface, "disabled": "false"}}
	sn.rows["/ip/dhcp-server"] = []routeros.Row{{"name": ONUMgmtDHCPServer, "interface": p.Iface, "disabled": "false", "add-arp": "true"}}
	var rules []routeros.Row
	for _, r := range append(fwd, in...) {
		rules = append(rules, routeros.Row(r.Body("")))
		rules[len(rules)-1]["disabled"] = "false"
	}
	sn.rows["/ip/firewall/filter"] = rules
	e := &evalCtx{m: f.m, site: site, sn: sn}
	var it CheckItem
	evalONUMgmt(e, &it)
	if it.Status != StatusOK {
		t.Fatalf("complete network: %s %s", it.Status, it.Detail)
	}
	// A router built before: the VLAN answers every ARP and the DHCP server adds none: warned, with the fix said.
	sn.rows["/interface/vlan"][0]["arp"] = "enabled"
	sn.rows["/ip/dhcp-server"][0]["add-arp"] = "false"
	it = CheckItem{}
	evalONUMgmt(e, &it)
	if it.Status != StatusWarn || !strings.Contains(it.Detail, "ARP reply-only on "+p.Iface) || !strings.Contains(it.Detail, "add-arp on the DHCP server") {
		t.Fatalf("old ARP settings: %s %s", it.Status, it.Detail)
	}
	sn.rows["/interface/vlan"][0]["arp"] = "reply-only"
	sn.rows["/ip/dhcp-server"][0]["add-arp"] = "true"
	sn.rows["/ip/firewall/filter"] = rules[:3]
	it = CheckItem{}
	evalONUMgmt(e, &it)
	if it.Status != StatusWarn || !strings.Contains(it.Detail, "ftn: onu mgmt drop out") || it.Fix.Kind != FixManual {
		t.Fatalf("missing rules: %s %s", it.Status, it.Detail)
	}
	// A router without a management subnet is just informed.
	e.site = benchSite()
	it = CheckItem{}
	evalONUMgmt(e, &it)
	if it.Status != StatusInfo {
		t.Fatalf("no subnet: %s", it.Status)
	}
	_ = netip.Prefix{}
}

// Without a customer VLAN the OLT port is a bridge port, and a VLAN interface on a bridge port never receives a
// frame: the management VLAN then sits on the customer bridge.
func TestONUMgmtParent(t *testing.T) {
	s := store.Site{OLTInterface: "sfp-sfpplus1", VLANID: 100}
	if got := ONUMgmtParent(s); got != "sfp-sfpplus1" {
		t.Errorf("with a customer VLAN: %q", got)
	}
	s.VLANID = 0
	if got := ONUMgmtParent(s); got != CustomerBridge {
		t.Errorf("without a customer VLAN: %q", got)
	}
}

// M1 and L8: the management VLAN answers ARP only for its own address, the DHCP server adds an ARP entry per lease,
// and the ONUs are not told a DNS server and may not ask the router for one.
func TestONUMgmtSetupHardening(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.ACSListen = "10.99.0.1:7547"; c.ACSCRPorts = []int{7547, 30005} })
	p, _ := f.m.ONUPlan(onuSite())
	lines := strings.Join(p.SetupLines("sfp-sfpplus1"), "\n")
	if !strings.Contains(lines, "/interface/vlan add name="+p.Iface+" vlan-id=99 interface=sfp-sfpplus1 arp=reply-only") {
		t.Errorf("the VLAN is not reply-only:\n%s", lines)
	}
	if !strings.Contains(lines, "add-arp=yes") || strings.Contains(lines, "add-arp=no") {
		t.Errorf("the DHCP server does not add ARP entries:\n%s", lines)
	}
	if strings.Contains(lines, "dns-server") {
		t.Errorf("the ONUs are told a DNS server:\n%s", lines)
	}
	if !strings.Contains(lines, "ntp-server=") {
		t.Errorf("NTP was dropped:\n%s", lines)
	}
	_, in := p.Rules()
	for _, r := range in {
		if strings.Contains(r.Args(), "dst-port=53") {
			t.Errorf("the router answers DNS to the ONUs: %s", r.Args())
		}
	}
}

// A router built before 58000 was allowed has the rule "ftn: acs to onu" with dst-port=7547,30005: the sweep sets
// the ports in place (and only that), and a second pass writes nothing.
func TestONUMgmtRuleDstPortIsHealed(t *testing.T) {
	f, s, m := adminRig(t)
	s, err := f.m.EnsureONUMgmt(s.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := f.m.ONUPlan(s)
	if got := portList(p.CRPorts); got != "7547,30005,58000" {
		t.Fatalf("default ports %s", got)
	}
	m.SeedRow("/interface/vlan", R{"name": p.Iface, "vlan-id": strconv.Itoa(p.VLAN), "interface": ONUMgmtParent(s), "arp": "reply-only"})
	m.SeedRow("/ip/address", R{"address": p.GatewayCIDR, "interface": p.Iface})
	m.SeedRow("/ip/dhcp-server", R{"name": ONUMgmtDHCPServer, "interface": p.Iface, "add-arp": "yes"})
	fwd, in := p.Rules()
	for _, r := range append(fwd, in...) {
		body := r.Body("")
		if r.Comment == ACSToONUComment {
			body["dst-port"] = "7547,30005"
		}
		m.SeedRow("/ip/firewall/filter", R(body))
	}
	it := checkItem(t, f, s.ID, CheckONUMgmt)
	if it.Status != StatusWarn || it.Fix.Kind != FixAuto || !strings.Contains(strings.Join(it.Evidence, " "), "58000") {
		t.Fatalf("drift not reported as an automatic fix: %+v", it)
	}
	m.ResetWrites()
	out := heal(t, f, s)
	done := false
	for _, o := range out {
		if o.ID == CheckONUMgmt {
			done = o.Err == "" && len(o.Applied) > 0
		}
	}
	if !done {
		t.Fatalf("the ONU management rule was not fixed: %+v", out)
	}
	for _, r := range m.Table("/ip/firewall/filter") {
		if r["comment"] == ACSToONUComment && r["dst-port"] != "7547,30005,58000" {
			t.Fatalf("dst-port %q", r["dst-port"])
		}
	}
	m.ResetWrites()
	heal(t, f, s)
	if m.Writes() != 0 {
		t.Fatalf("a second pass wrote %d", m.Writes())
	}
}
