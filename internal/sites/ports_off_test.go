package sites

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// ---- the bootstrap script ----------------------------------------------------------------------------

func configFor() config.Config {
	return config.Config{PublicIP: "203.0.113.10", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24"}
}

func withREST(s store.Site) store.Site { s.RESTUser = "ispbill"; return s }

func renderOff(t *testing.T, mut func(*store.Site, *BootstrapInput)) string {
	t.Helper()
	s := benchSite()
	in := benchInput(s)
	if mut != nil {
		mut(&s, &in)
		in.Site = ApplyDefaults(configFor(), withREST(s))
	}
	out, err := RenderBootstrap(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func idx(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("script has no %q", sub)
	}
	return i
}

func TestBootstrapSwitchesEveryPortOffExceptUplinkAndAdmin(t *testing.T) {
	out := renderOff(t, nil)
	if !strings.Contains(out, `:local keepUp {"ether1";"ether2"}`) {
		t.Fatalf("the keep-up list is not the uplink and the admin port")
	}
	keep := out[idx(t, out, ":local keepUp") : idx(t, out, ":local keepUp")+40]
	for _, p := range []string{"ether4", "ether5", "sfp-sfpplus1"} {
		if strings.Contains(keep, `"`+p+`"`) {
			t.Errorf("%s must not be kept up", p)
		}
	}
	off := idx(t, out, `/interface/set $i disabled=yes`)
	// off before the WAN exists, so it is also in force if the script stops early
	if off > idx(t, out, `/ip/dhcp-client enable`) || off > idx(t, out, "ftn 3/12") {
		t.Fatal("the ports are switched off too late")
	}
	if off < idx(t, out, `comment="ftn: hold"`) {
		t.Fatal("the ports go off before the firewall hold")
	}
	// nothing later switches a port back on
	if strings.Count(out, "/interface/set") != 1 {
		t.Fatal("another script line writes an interface")
	}
	if strings.Contains(out, "/interface/enable") || strings.Contains(out, "/interface enable") {
		t.Fatal("the script enables an interface")
	}
	// the report goes out over the WAN, which is on after the rules: it does not name a customer or OLT port
	if !strings.Contains(out, "/tool/fetch") {
		t.Fatal("no report post")
	}
}

func TestBootstrapKeepsTheConfiguredAdminPortsAndUplinkUp(t *testing.T) {
	out := renderOff(t, func(s *store.Site, _ *BootstrapInput) {
		s.WANInterface, s.LANPorts, s.LANSubnet = "ether8", "ether2,ether7", "10.7.0.0/24"
	})
	if !strings.Contains(out, `:local keepUp {"ether8";"ether2";"ether7"}`) {
		t.Fatalf("keep-up list: %s", out[idx(t, out, ":local keepUp"):idx(t, out, ":local keepUp")+80])
	}
	if !strings.Contains(out, `src-address=10.7.0.0/24 out-interface-list=WAN comment="ftn: admin net no internet"`) {
		t.Fatal("the admin subnet rule does not use the configured subnet")
	}
}

func TestBootstrapAdminRulesOrderAndNothingElseChanges(t *testing.T) {
	out := renderOff(t, nil)
	accept := idx(t, out, `comment="ftn: admin allow billing site"`)
	drop := idx(t, out, `comment="ftn: admin no internet"`)
	net := idx(t, out, `comment="ftn: admin net no internet"`)
	est := idx(t, out, `comment="ftn: forward est/rel"`)
	walled := idx(t, out, `comment="ftn: ipoe walled payment page`)
	hold := idx(t, out, `comment="ftn: hold"`)
	if !(hold < accept && accept < drop && drop < net && net < walled && net < est) {
		t.Fatalf("order: hold %d accept %d drop %d net %d walled %d est %d", hold, accept, drop, net, walled, est)
	}
	line := func(c string) string {
		i := idx(t, out, `comment="`+c+`"`)
		return out[strings.LastIndex(out[:i], "\n")+1 : i]
	}
	if l := line("ftn: admin allow billing site"); !strings.Contains(l, "chain=forward action=accept in-interface=br-lan out-interface-list=WAN protocol=tcp dst-port=80,443 dst-address-list=ftn-admin-allow connection-state=new,established,related") {
		t.Fatalf("accept: %s", l)
	}
	if l := line("ftn: admin no internet"); !strings.Contains(l, "chain=forward action=drop in-interface=br-lan out-interface-list=WAN") {
		t.Fatalf("drop: %s", l)
	}
	// no earlier forward accept can let the admin LAN out: every forward rule above the drop is the hold or ours
	for _, l := range strings.Split(out[:drop], "\n") {
		if strings.Contains(l, "chain=forward action=accept") && !strings.Contains(l, "ftn: admin allow") {
			t.Fatalf("an accept above the admin drop: %s", l)
		}
	}
	// the old "lan to wan" accept is gone; masquerade stays for what the accept lets through; input is untouched
	if strings.Contains(out, "ftn: lan to wan") {
		t.Fatal("the old lan-to-wan accept is still there")
	}
	for _, keep := range []string{`comment="ftn: lan masquerade"`, `comment="ftn: lan input"`, `comment="ftn: lan to customers"`, `comment="ftn: customers to wan"`,
		`comment="ftn: mgmt winbox"`, `comment="ftn: wg rest"`, `comment="ftn: customer masquerade"`} {
		idx(t, out, keep)
	}
	// RouterOS 7.25 refuses a rule that names an interface that does not exist: bridge, list and tunnel come first
	first := idx(t, out, "in-interface=br-lan out-interface-list=WAN")
	if idx(t, out, "/interface/bridge add name=br-lan") > first || idx(t, out, `/interface/list add name=WAN`) > first {
		t.Fatal("a rule names br-lan or the WAN list before they exist")
	}
	if idx(t, out, `/interface/wireguard add name=wg-ftn`) > idx(t, out, "in-interface=wg-ftn") {
		t.Fatal("a rule names the tunnel before it exists")
	}
}

func TestBootstrapAdminAllowListHoldsTheServersNames(t *testing.T) {
	out := renderOff(t, nil)
	for _, want := range []string{
		`/ip/firewall/address-list add list=ftn-admin-allow address=billing.example.co.ke comment="ftn: admin allow billing.example.co.ke"`,
		`/ip/firewall/address-list add list=ftn-admin-allow address=203.0.113.10 comment="ftn: admin allow 203.0.113.10"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "tech.ftw.co.ke") {
		t.Fatal("no WebFig domain is configured")
	}
	out = renderOff(t, func(_ *store.Site, in *BootstrapInput) {
		in.PublicBase, in.WebFigDomain = "https://isp.ftw.co.ke", "Tech.FTW.co.ke"
	})
	for _, h := range []string{"isp.ftw.co.ke", "tech.ftw.co.ke", "203.0.113.10"} {
		if !strings.Contains(out, "list=ftn-admin-allow address="+h+" ") {
			t.Errorf("no entry for %s", h)
		}
	}
	// the list is made before the accept that uses it
	if idx(t, out, "address-list add list=ftn-admin-allow") > idx(t, out, `comment="ftn: admin allow billing site"`) {
		t.Fatal("the allow list comes after its rule")
	}
}

// ---- the sweep ---------------------------------------------------------------------------------------

func sweepNow(f *fixture, id string) {
	f.advance(10 * time.Minute)
	f.m.SweepSite(context.Background(), id)
}

func TestSweepSwitchesOffAPortTheOwnerDidNotEnable(t *testing.T) {
	f, s, m := cpRig(t, "ether3", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts = "ether3,sfp-sfpplus1" // ether4 and ether5 are not enabled by the owner
		d.Sites[s.ID] = x
		return nil
	})
	sweepNow(f, s.ID)
	for p, want := range map[string]string{"ether1": "", "ether2": "", "ether3": "", "sfp-sfpplus1": "", "ether4": "yes", "ether5": "yes"} {
		if got := ifaceRow(m, p)["disabled"]; got != want {
			t.Errorf("%s disabled=%q, want %q", p, got, want)
		}
	}
	if !hasEvent(f, s.ID, "switched off ether4") {
		t.Fatal("no activity line")
	}
}

func TestSweepNeverReEnablesAndNeverTouchesUplinkOrAdmin(t *testing.T) {
	f, s, m := cpRig(t, "ether3", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts = "" // the owner enabled nothing: customer port and OLT too are off
		d.Sites[s.ID] = x
		return nil
	})
	sweepNow(f, s.ID)
	for _, p := range []string{"ether3", "ether4", "ether5", "sfp-sfpplus1"} {
		if ifaceRow(m, p)["disabled"] != "yes" {
			t.Errorf("%s was not switched off", p)
		}
	}
	for _, p := range []string{"ether1", "ether2"} {
		if r := ifaceRow(m, p); r["disabled"] == "yes" {
			t.Errorf("the server switched off %s", p)
		}
	}
	// more sweeps, with the router's ports off: nothing is written, nothing is enabled
	m.ResetWrites()
	for i := 0; i < 3; i++ {
		sweepNow(f, s.ID)
	}
	for _, p := range []string{"ether3", "ether4", "ether5", "sfp-sfpplus1"} {
		if ifaceRow(m, p)["disabled"] != "yes" {
			t.Errorf("%s was switched back on", p)
		}
	}
	for _, r := range m.Table("/interface") {
		if r["name"] == "ether1" || r["name"] == "ether2" {
			if r["disabled"] == "yes" {
				t.Errorf("%s was switched off", r["name"])
			}
		}
	}
}

func TestEnableFlowRecordsTheOwnersChoiceAndTheSweepKeepsIt(t *testing.T) {
	f, s, m := cpRig(t, "ether3", 0)
	ctx := context.Background()
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts = ""
		d.Sites[s.ID] = x
		return nil
	})
	sweepNow(f, s.ID)
	if ifaceRow(m, "sfp-sfpplus1")["disabled"] != "yes" || ifaceRow(m, "ether3")["disabled"] != "yes" {
		t.Fatal("setup: the ports should be off")
	}
	// the Ports tab shows them off with an Enable action
	v, _ := f.m.Ports(ctx, s.ID, store.RoleOwner)
	if p := portByName(t, v, "ether3"); !p.Disabled || p.OwnerEnabled || !p.CanChange {
		t.Fatalf("%+v", p)
	}
	if p := portByName(t, v, "ether2"); p.Note != AdminPortNote || !p.OwnerEnabled || p.Disabled {
		t.Fatalf("admin port: %+v", p)
	}
	// the OLT cannot be switched off from here but can be enabled
	if _, err := f.m.ChangePort(ctx, s.ID, "sfp-sfpplus1", PortChange{Disabled: bp(false)}, ownerWho); err != nil {
		t.Fatalf("enable the OLT: %v", err)
	}
	if _, err := f.m.ChangePort(ctx, s.ID, "ether3", PortChange{Disabled: bp(false)}, operWho); err != nil {
		t.Fatalf("enable a customer port: %v", err)
	}
	if got := f.site(s.ID).EnabledPorts; !containsStr(SplitPorts(got), "ether3") || !containsStr(SplitPorts(got), "sfp-sfpplus1") {
		t.Fatalf("not recorded: %q", got)
	}
	if ifaceRow(m, "sfp-sfpplus1")["disabled"] == "yes" || ifaceRow(m, "ether3")["disabled"] == "yes" {
		t.Fatal("not enabled at the router")
	}
	sweepNow(f, s.ID)
	if ifaceRow(m, "sfp-sfpplus1")["disabled"] == "yes" || ifaceRow(m, "ether3")["disabled"] == "yes" {
		t.Fatal("the sweep switched an owner-enabled port off")
	}
	if ifaceRow(m, "ether4")["disabled"] != "yes" {
		t.Fatal("a port the owner did not enable is on")
	}
	// disabling forgets it again; the OLT is still refused with the new text
	if _, err := f.m.ChangePort(ctx, s.ID, "ether3", PortChange{Disabled: bp(true)}, operWho); err != nil {
		t.Fatal(err)
	}
	if containsStr(SplitPorts(f.site(s.ID).EnabledPorts), "ether3") {
		t.Fatal("still recorded as enabled")
	}
	pe := portErrOf(t, f, s.ID, "sfp-sfpplus1", PortChange{Disabled: bp(true)}, ownerWho)
	if pe.Kind != PortErrOLTOff || pe.Msg != OLTNoOffMsg {
		t.Fatalf("%+v", pe)
	}
	// somebody switches a port on at the router: the owner has not enabled it, so the sweep switches it off
	setField(t, m, "/interface", "ether5", "disabled", "false")
	sweepNow(f, s.ID)
	if ifaceRow(m, "ether5")["disabled"] != "yes" {
		t.Fatal("a hand-enabled port stays on")
	}
}

func TestEnablingAPortThatIsOnButNotEnabledByTheOwnerRecordsIt(t *testing.T) {
	f, s, _ := cpRig(t, "ether3", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts = ""
		d.Sites[s.ID] = x
		return nil
	})
	// ether4 is still on at the router (the sweep has not run): Enable must record it
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether4", PortChange{Disabled: bp(false)}, operWho); err != nil {
		t.Fatal(err)
	}
	if !containsStr(SplitPorts(f.site(s.ID).EnabledPorts), "ether4") {
		t.Fatalf("%q", f.site(s.ID).EnabledPorts)
	}
}

// ---- the upgrade of a router in service ----------------------------------------------------------------

func TestUpgradeKeepsEverythingInServiceUp(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	// the router before the upgrade: no policy recorded. ether1 WAN (link), ether2 admin (link), sfp-sfpplus1 = OLT,
	// ether5 = customer port (no link yet), ether3 on with a link and no role, ether4 on with no link and no role,
	// ether6 already switched off by the owner.
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.PortsPolicy, x.EnabledPorts, x.DisabledPorts = "", "", "ether6"
		d.Sites[s.ID] = x
		return nil
	})
	m.SeedRow("/interface", R{"name": "ether6", "type": "ether", "running": "false", "disabled": "true"})
	setField(t, m, "/interface", "ether5", "running", "false")
	setField(t, m, "/interface", "ether4", "running", "false")
	setField(t, m, "/interface", "sfp-sfpplus1", "running", "false") // the OLT is unplugged right now: still in service
	m.ResetWrites()

	sweepNow(f, s.ID)
	got := f.site(s.ID)
	if got.PortsPolicy != PortsPolicyOff {
		t.Fatalf("policy not started: %q", got.PortsPolicy)
	}
	en := SplitPorts(got.EnabledPorts)
	for _, p := range []string{"sfp-sfpplus1", "ether5", "ether3"} {
		if !containsStr(en, p) {
			t.Errorf("%s (in service) was not recorded as enabled: %q", p, got.EnabledPorts)
		}
	}
	for _, p := range []string{"ether4", "ether6", "ether1", "ether2"} {
		if containsStr(en, p) {
			t.Errorf("%s must not be recorded: %q", p, got.EnabledPorts)
		}
	}
	for p, want := range map[string]string{"sfp-sfpplus1": "", "ether5": "", "ether3": "", "ether1": "", "ether2": "", "ether4": "yes", "ether6": "true"} {
		if d := ifaceRow(m, p)["disabled"]; d != want && !(want == "true" && d == "yes") {
			t.Errorf("%s disabled=%q, want %q", p, d, want)
		}
	}
	// the running port with no role stays up and carries the warning; the ports with a role do not
	v, _ := f.m.Ports(ctx, s.ID, store.RoleOwner)
	if p := portByName(t, v, "ether3"); p.Warning != NoRoleUpWarn || p.Disabled || !p.OwnerEnabled {
		t.Fatalf("ether3: %+v", p)
	}
	for _, p := range []string{"ether5", "sfp-sfpplus1", "ether2"} {
		if w := portByName(t, v, p).Warning; w != "" {
			t.Errorf("%s: unexpected warning %q", p, w)
		}
	}
	if !hasActorEvent(f, s.ID, ActorSystem, "kept in service") {
		t.Fatal("the migration is not in the router's activity")
	}
	// a second sweep changes nothing
	m.ResetWrites()
	sweepNow(f, s.ID)
	if f.site(s.ID).EnabledPorts != got.EnabledPorts {
		t.Fatal("the record changed")
	}
	for _, r := range m.Table("/interface") {
		if r["name"] == "ether3" && r["disabled"] == "yes" {
			t.Fatal("ether3 went down")
		}
	}
}

func TestUpgradeWaitsForACompleteReadOfTheRouter(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.PortsPolicy, x.EnabledPorts = "", ""
		d.Sites[s.ID] = x
		return nil
	})
	m.SetReadFail("/interface", errors.New("boom"))
	sweepNow(f, s.ID)
	if f.site(s.ID).PortsPolicy != "" {
		t.Fatal("the policy started without reading the ports")
	}
	for _, r := range m.Table("/interface") {
		if r["disabled"] == "yes" {
			t.Fatalf("%s was switched off on an unread router", r["name"])
		}
	}
	m.SetReadFail("/interface", nil)
	sweepNow(f, s.ID)
	if f.site(s.ID).PortsPolicy != PortsPolicyOff {
		t.Fatal("the policy did not start once the router answered")
	}
}

func TestANewScriptStartsThePolicyAndForgetsOldChoices(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts, x.DisabledPorts = "ether3,ether4", "ether6"
		d.Sites[s.ID] = x
		return nil
	})
	tok, _, err := f.m.MintJoin(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt+"/7.25.1 (fetch)", "1.2.3.4", false); err != nil {
		t.Fatal(err)
	}
	if g := f.site(s.ID); g.PortsPolicy != PortsPolicyOff || g.EnabledPorts != "" || g.DisabledPorts != "" {
		t.Fatalf("%+v", g)
	}
}

// ---- the admin port rules are healed ---------------------------------------------------------------------

func adminRig(t *testing.T) (*fixture, store.Site, *routeros.Mock) {
	f, s, m := cpRig(t, "ether3", 0)
	f.m.cfg.WebFigDomain = "tech.ftw.co.ke"
	f.m.cfg.PublicBaseURL = "https://isp.ftw.co.ke"
	return f, s, m
}

func heal(t *testing.T, f *fixture, s store.Site) []AutoFixOutcome {
	t.Helper()
	f.advance(20 * time.Minute)
	f.m.chk.drop(s.ID)
	return f.m.AutoMaintain(context.Background(), s.ID)
}

func forwardOrder(m *routeros.Mock) []string {
	var out []string
	for _, r := range m.Table("/ip/firewall/filter") {
		if r["chain"] == "forward" {
			out = append(out, r["comment"])
		}
	}
	return out
}

func listEntries(m *routeros.Mock) map[string]string {
	out := map[string]string{}
	for _, r := range m.Table("/ip/firewall/address-list") {
		if r["list"] == AdminAllowList {
			out[r["address"]] = r["comment"]
		}
	}
	return out
}

func TestAdminRulesAreAddedAtTheTopAndHealedOnTheSweep(t *testing.T) {
	f, s, m := adminRig(t)
	// the router as an older script left it: the old lan-to-wan accept and est/rel, no admin rules
	m.SeedRow("/ip/firewall/filter", R{"chain": "forward", "action": "accept", "connection-state": "established,related", "comment": "ftn: forward est/rel"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "forward", "action": "accept", "in-interface-list": "LAN", "out-interface-list": "WAN", "comment": "ftn: lan to wan"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "in-interface-list": "LAN", "comment": "ftn: lan input"})
	countInput := func() (n int) {
		for _, r := range m.Table("/ip/firewall/filter") {
			if r["chain"] == "input" {
				n++
			}
		}
		return
	}
	inputsBefore := countInput()
	m.ResetWrites()
	out := heal(t, f, s)
	found := false
	for _, o := range out {
		if o.ID == CheckAdminNet {
			found = o.Err == "" && len(o.Applied) > 0
		}
	}
	if !found {
		t.Fatalf("the admin check was not fixed: %+v", out)
	}
	fw := forwardOrder(m)
	if len(fw) < 3 || fw[0] != AdminAllowComment || fw[1] != AdminDropComment || fw[2] != AdminNetDropComment {
		t.Fatalf("forward order: %v", fw)
	}
	want := map[string]bool{"isp.ftw.co.ke": true, "tech.ftw.co.ke": true, "203.0.113.10": true}
	got := listEntries(m)
	if len(got) != 3 {
		t.Fatalf("allow list: %v", got)
	}
	for a := range want {
		if got[a] != "ftn: admin allow "+a {
			t.Errorf("entry %s: %v", a, got)
		}
	}
	// the input chain and everything else was left alone
	if n := countInput(); n != inputsBefore {
		t.Fatalf("input rules: %d, was %d", n, inputsBefore)
	}
	// a clean router: nothing more is written
	m.ResetWrites()
	if out := heal(t, f, s); m.Writes() != 0 {
		t.Fatalf("a second pass wrote %d: %+v", m.Writes(), out)
	}
}

func TestAdminRuleBypassAboveItIsMovedBack(t *testing.T) {
	f, s, m := adminRig(t)
	heal(t, f, s)
	first := m.Table("/ip/firewall/filter")[0]
	// somebody puts an established/related accept above the admin rules
	if _, err := m.AddRow(context.Background(), "/ip/firewall/filter", map[string]string{"chain": "forward", "action": "accept", "connection-state": "established,related", "comment": "x: bypass", "place-before": first[".id"]}); err != nil {
		t.Fatal(err)
	}
	if fw := forwardOrder(m); fw[0] != "x: bypass" {
		t.Fatalf("setup: %v", fw)
	}
	heal(t, f, s)
	fw := forwardOrder(m)
	if fw[0] != AdminAllowComment || fw[1] != AdminDropComment || fw[2] != AdminNetDropComment {
		t.Fatalf("not at the top again: %v", fw)
	}
	n := 0
	for _, c := range fw {
		if strings.HasPrefix(c, "ftn: admin") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("doubled or lost rules: %v", fw)
	}
	if fw[3] != "x: bypass" {
		t.Fatalf("the foreign rule was touched: %v", fw)
	}
}

func TestAdminRulesRepairChangesAndDisabledRules(t *testing.T) {
	f, s, m := adminRig(t)
	heal(t, f, s)
	for _, r := range m.Table("/ip/firewall/filter") {
		switch r["comment"] {
		case AdminDropComment:
			m.SetRow(context.Background(), "/ip/firewall/filter", r[".id"], map[string]string{"disabled": "true"})
		case AdminAllowComment:
			m.SetRow(context.Background(), "/ip/firewall/filter", r[".id"], map[string]string{"dst-port": "1-65535"})
		}
	}
	heal(t, f, s)
	for _, r := range m.Table("/ip/firewall/filter") {
		if r["comment"] == AdminDropComment && yes(r["disabled"]) {
			t.Fatal("the drop rule is still disabled")
		}
		if r["comment"] == AdminAllowComment && r["dst-port"] != "80,443" {
			t.Fatalf("the allow rule is not repaired: %v", r)
		}
	}
	if c := checkItem(t, f, s.ID, CheckAdminNet); c.Status != StatusOK {
		t.Fatalf("%+v", c)
	}
}

func TestAdminAllowListFollowsTheServersNames(t *testing.T) {
	f, s, m := adminRig(t)
	heal(t, f, s)
	// an entry is deleted by hand and another name is added by hand
	for _, r := range m.Table("/ip/firewall/address-list") {
		if r["address"] == "tech.ftw.co.ke" {
			m.RemoveRow(context.Background(), "/ip/firewall/address-list", r[".id"])
		}
	}
	m.SeedRow("/ip/firewall/address-list", R{"list": AdminAllowList, "address": "evil.example.com", "comment": "ftn: admin allow evil.example.com"})
	heal(t, f, s)
	got := listEntries(m)
	if _, ok := got["tech.ftw.co.ke"]; !ok {
		t.Fatalf("the missing name was not added back: %v", got)
	}
	if _, ok := got["evil.example.com"]; ok {
		t.Fatalf("a stale name stays: %v", got)
	}
	// the server's domain changes: the new name comes in, the old one goes
	f.m.cfg.PublicBaseURL = "https://billing.new.example"
	f.m.cfg.WebFigDomain = ""
	heal(t, f, s)
	got = listEntries(m)
	if len(got) != 2 || got["billing.new.example"] == "" || got["203.0.113.10"] == "" {
		t.Fatalf("after the change: %v", got)
	}
	// an unrelated address list is not read or touched
	m.SeedRow("/ip/firewall/address-list", R{"list": "ftn-unreg", "address": "10.5.50.10-10.5.50.59", "comment": "ftn: unregistered range"})
	heal(t, f, s)
	if len(m.Table("/ip/firewall/address-list")) != 3 {
		t.Fatalf("%v", m.Table("/ip/firewall/address-list"))
	}
}

func TestAdminRulesNeedTheAdminBridge(t *testing.T) {
	f, s, m := adminRig(t)
	for _, r := range m.Table("/interface/bridge") {
		if r["name"] == "br-lan" {
			m.RemoveRow(context.Background(), "/interface/bridge", r[".id"])
		}
	}
	heal(t, f, s)
	for _, c := range forwardOrder(m) {
		if strings.HasPrefix(c, "ftn: admin") {
			t.Fatalf("a rule naming a missing bridge was written: %v", forwardOrder(m))
		}
	}
}

func TestFixWriterAdminAllowlist(t *testing.T) {
	m := routeros.NewMock()
	w := &fixWriter{rt: m, adminRules: adminComments(adminRuleSet("192.168.88.0/24")), adminList: true, wan: "ether1"}
	ctx := context.Background()
	ok := adminRuleSet("192.168.88.0/24")[1].Body("")
	if err := w.addRow(ctx, "/ip/firewall/filter", ok); err != nil {
		t.Fatalf("a managed rule: %v", err)
	}
	for name, body := range map[string]map[string]string{
		"input chain":   {"chain": "input", "action": "accept", "comment": AdminDropComment},
		"other comment": {"chain": "forward", "action": "drop", "comment": "ftn: something"},
		"bad property":  {"chain": "forward", "action": "drop", "comment": AdminDropComment, "log": "yes"},
	} {
		if err := w.addRow(ctx, "/ip/firewall/filter", body); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := w.addRow(ctx, "/ip/firewall/nat", ok); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("nat: %v", err)
	}
	if err := w.addRow(ctx, "/ip/firewall/address-list", map[string]string{"list": "ftn-unreg", "address": "1.2.3.4", "comment": "ftn: admin allow x"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("another list: %v", err)
	}
	if err := w.addRow(ctx, "/ip/firewall/address-list", map[string]string{"list": AdminAllowList, "address": "1.2.3.4", "comment": "ftn: admin allow 1.2.3.4"}); err != nil {
		t.Errorf("an allow entry: %v", err)
	}
	// the forbidden classes of writes
	for _, p := range []string{"/user", "/ip/service", "/system/identity", "/interface/wireguard", "/ip/route"} {
		if err := w.addRow(ctx, p, map[string]string{"name": "x"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("%s: %v", p, err)
		}
	}
	// no switching the uplink or an unlisted port
	if err := w.setRow(ctx, "/interface", routeros.Row{"name": "ether1", "type": "ether"}, map[string]string{"disabled": "yes"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("the uplink: %v", err)
	}
}
