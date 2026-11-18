package sites

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func setRows(m *routeros.Mock, path, key, val string, diff R) {
	for _, r := range m.Table(path) {
		if r[key] == val {
			m.SetRow(context.Background(), path, r[".id"], diff)
		}
	}
}

func TestKitDriftIsDetected(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	for name, mut := range map[string]func(){
		"tick source": func() {
			setRows(m, "/system/script", "name", "ftn-kit-tick", R{"source": kitTickSource() + "\n/user add name=x group=full"})
		},
		"rollback source": func() {
			setRows(m, "/system/script", "name", "ftn-kit-rollback", R{"source": "/system reset-configuration"})
		},
		"script policy": func() {
			setRows(m, "/system/script", "name", "ftn-kit-tick", R{"policy": kitTickPolicy + ",password,ssh"})
		},
		"script owner": func() { setRows(m, "/system/script", "name", "ftn-kit-rollback", R{"owner": "ftn-api"}) },
		"timer target": func() {
			setRows(m, "/system/scheduler", "name", "ftn-kit-timer", R{"on-event": "/system reset-configuration"})
		},
		"timer policy": func() { setRows(m, "/system/scheduler", "name", "ftn-kit-timer", R{"policy": "read"}) },
	} {
		// reset, then drift one thing
		setRows(m, "/system/script", "name", "ftn-kit-tick", R{"source": kitTickSource(), "policy": kitTickPolicy, "owner": "admin"})
		setRows(m, "/system/script", "name", "ftn-kit-rollback", R{"source": kitRollbackSource("hotspot1"), "policy": kitScriptPolic, "owner": "admin"})
		setRows(m, "/system/scheduler", "name", "ftn-kit-timer", R{"on-event": "ftn-kit-tick", "policy": kitTickPolicy})
		mut()
		it := item(t, get(t, f, s.ID, true), CheckKit)
		if it.Status != StatusFail || !strings.Contains(it.Detail, "not what the server installed") || it.Fix.Kind != FixManual {
			t.Errorf("%s: %+v", name, it)
		}
	}
	// Line ends and trailing spaces are not drift.
	setRows(m, "/system/script", "name", "ftn-kit-tick", R{"source": strings.ReplaceAll(kitTickSource(), "\n", " \r\n"), "policy": "test,write,read,policy,sensitive,ftp", "owner": "admin"})
	setRows(m, "/system/script", "name", "ftn-kit-rollback", R{"source": kitRollbackSource("hotspot1"), "policy": kitScriptPolic, "owner": "admin"})
	setRows(m, "/system/scheduler", "name", "ftn-kit-timer", R{"on-event": "ftn-kit-tick", "policy": kitTickPolicy})
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
}

func TestScriptHooksAreShownNeverFixed(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, true), CheckHooks); it.Status != StatusOK || it.BlocksCutover {
		t.Fatalf("%+v", it)
	}
	setRows(m, "/ip/dhcp-server", "name", "dhcp-hotspot", R{"lease-script": ":log info lease"})
	m.SeedRow("/ip/dhcp-client", R{"interface": "ether2", "script": "/system reset-configuration"})
	m.SeedRow("/ppp/profile", R{"name": "p", "on-up": ":log info up"})
	m.SeedRow("/ip/hotspot/user/profile", R{"name": "ispbill-x", "on-login": ":log info in", "on-logout": ":log info out"})
	m.SeedRow("/tool/netwatch", R{"host": "1.1.1.1", "up-script": ":log info up", "down-script": ""})
	it := item(t, get(t, f, s.ID, true), CheckHooks)
	if it.Status != StatusWarn || it.Fix.Kind != FixNone || len(it.Evidence) != 6 || it.BlocksCutover {
		t.Fatalf("%+v", it)
	}
	if _, err := f.m.ApplyFix(context.Background(), s.ID, CheckHooks, true, store.RoleOwner); !errors.Is(err, ErrNoFix) {
		t.Fatalf("%v", err)
	}
	m.SetPathFail("/tool/netwatch", errors.New("403"))
	if it = item(t, get(t, f, s.ID, true), CheckHooks); it.Status != StatusUnknown {
		t.Fatalf("%+v", it)
	}
}

func TestAPIPolicyNeedsRebootAndWarnsAboutExtraRights(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, true), CheckAPIPolicy); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	setRows(m, "/user/group", "name", "ftn-api", R{"policy": "read,write,test,sensitive,api,rest-api"})
	it := item(t, get(t, f, s.ID, true), CheckAPIPolicy)
	if it.Status != StatusFail || !strings.Contains(it.Detail, "reboot") || !strings.HasSuffix(it.Fix.Command, ",reboot") {
		t.Fatalf("%+v", it)
	}
	setRows(m, "/user/group", "name", "ftn-api", R{"policy": "read,write,test,sensitive,api,rest-api,reboot,policy,ssh,!ftp"})
	it = item(t, get(t, f, s.ID, true), CheckAPIPolicy)
	if it.Status != StatusWarn || !strings.Contains(it.Detail, "policy, ssh") || strings.Contains(it.Detail, "ftp") || it.Fix.Kind != FixManual {
		t.Fatalf("%+v", it)
	}
}

func dnsOff(m *routeros.Mock) { m.SeedOne("/ip/dns", R{"allow-remote-requests": "no"}) }

func TestDNSFixRulesAreStrict(t *testing.T) {
	f, s, m := checkSite(t, nil)
	dnsOff(m)
	if it := item(t, get(t, f, s.ID, true), CheckDNS); it.Fix.Kind != FixAuto {
		t.Fatalf("baseline %+v", it)
	}
	manual := func(why string) {
		t.Helper()
		it := item(t, get(t, f, s.ID, true), CheckDNS)
		if it.Fix.Kind != FixManual || !strings.Contains(it.Detail, why) {
			t.Fatalf("want manual (%s): %+v", why, it)
		}
	}
	// The real uplink is not in the WAN list.
	m.SeedRow("/ip/dhcp-client", R{"interface": "ether2", "disabled": "false"})
	manual("ether2 (DHCP client) is not in the WAN list")
	setRows(m, "/ip/dhcp-client", "interface", "ether2", R{"interface": "ether1"})
	setRows(m, "/ip/route", "dst-address", "0.0.0.0/0", R{"immediate-gw": "10.0.0.1%pppoe-out1"})
	manual("pppoe-out1 (default route) is not in the WAN list")
	setRows(m, "/ip/route", "dst-address", "0.0.0.0/0", R{"immediate-gw": "192.168.1.1%ether1"})
	m.SeedRow("/interface/pppoe-client", R{"name": "pppoe-out1", "interface": "ether3"})
	manual("is not in the WAN list")
	for _, r := range m.Table("/interface/pppoe-client") {
		m.RemoveRow(context.Background(), "/interface/pppoe-client", r[".id"])
	}
	// IPv6 on and no IPv6 drop: manual; with the drop: auto.
	m.SeedOne("/ipv6/settings", R{"disable-ipv6": "false"})
	manual("IPv6")
	m.SeedRow("/ipv6/firewall/filter", R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"})
	if it := item(t, get(t, f, s.ID, true), CheckDNS); it.Fix.Kind != FixAuto {
		t.Fatalf("v6 drop %+v", it)
	}
	m.SeedOne("/ipv6/settings", R{"disable-ipv6": "true"})
	// A negated-protocol accept ("!tcp" matches udp) is a hole.
	filter := func(rows ...R) {
		for _, r := range m.Table("/ip/firewall/filter") {
			m.RemoveRow(context.Background(), "/ip/firewall/filter", r[".id"])
		}
		for _, r := range rows {
			m.SeedRow("/ip/firewall/filter", r)
		}
	}
	drop := R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"}
	filter(R{"chain": "input", "action": "accept", "protocol": "!tcp"}, drop)
	manual("accepts udp")
	// An untracked accept is a hole when something creates untracked packets.
	untracked := R{"chain": "input", "action": "accept", "connection-state": "untracked"}
	filter(untracked, drop)
	if it := item(t, get(t, f, s.ID, true), CheckDNS); it.Fix.Kind != FixAuto {
		t.Fatalf("no notrack anywhere: harmless %+v", it)
	}
	m.SeedRow("/ip/firewall/raw", R{"chain": "prerouting", "action": "notrack"})
	manual("accepts")
	// ...and when the raw table cannot be read.
	for _, r := range m.Table("/ip/firewall/raw") {
		m.RemoveRow(context.Background(), "/ip/firewall/raw", r[".id"])
	}
	m.SetPathFail("/ip/firewall/raw", errors.New("403"))
	manual("accepts")
	m.SetPathFail("/ip/firewall/raw", nil)
	// An accept bound to something other than a LAN member / the customer interface is a hole.
	filter(R{"chain": "input", "action": "accept", "in-interface": "ether7", "protocol": "udp", "dst-port": "53"}, drop)
	manual("accepts udp")
	filter(R{"chain": "input", "action": "accept", "in-interface": "ether7", "protocol": "udp", "dst-port": "53"}, drop)
	m.SeedRow("/interface/list/member", R{"list": "LAN", "interface": "ether7"})
	filter(R{"chain": "input", "action": "accept", "in-interface": "ether7", "protocol": "udp", "dst-port": "53"},
		R{"chain": "input", "action": "accept", "in-interface": "ether7", "protocol": "tcp", "dst-port": "53"}, drop)
	if it := item(t, get(t, f, s.ID, true), CheckDNS); it.Fix.Kind != FixAuto {
		t.Fatalf("a LAN member's accept is fine: %+v", it)
	}
	// A drop that is not provably the WAN's (an unknown extra matcher) does not count.
	filter(R{"chain": "input", "action": "drop", "in-interface-list": "!LAN", "src-address-list": "bad"})
	manual("no input rule drops")
}

func TestCustomerDNSInput(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	c := get(t, f, s.ID, true)
	it := item(t, c, CheckCustDNS)
	if it.Status != StatusOK || !it.BlocksCutover {
		t.Fatalf("%+v", it)
	}
	// The defconf chain alone drops the customer network.
	clear := func() {
		for _, r := range m.Table("/ip/firewall/filter") {
			m.RemoveRow(ctx, "/ip/firewall/filter", r[".id"])
		}
	}
	clear()
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "connection-state": "established,related", "dynamic": "true"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "connection-state": "established,related"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"})
	it = item(t, get(t, f, s.ID, true), CheckCustDNS)
	if it.Status != StatusFail || it.Fix.Kind != FixAuto || !strings.Contains(it.Detail, "ping the internet but not open any website") {
		t.Fatalf("%+v", it)
	}
	first := m.Table("/ip/firewall/filter")[1][".id"] // the first non-dynamic input rule
	m.ResetWrites()
	res, err := f.m.ApplyFix(ctx, s.ID, CheckCustDNS, false, store.RoleOperator)
	if err != nil || res.Item.Status != StatusOK || len(res.Applied) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	rows := m.Table("/ip/firewall/filter")
	var ours []R
	for _, r := range rows {
		if r["comment"] == "ftn: customer dns" {
			ours = append(ours, r)
		}
	}
	if len(ours) != 2 || ours[0]["protocol"] != "udp" || ours[1]["protocol"] != "tcp" {
		t.Fatalf("%v", rows)
	}
	for _, r := range ours {
		if r["in-interface"] != "br-hotspot" || r["dst-port"] != "53" || r["action"] != "accept" || r["chain"] != "input" {
			t.Fatalf("%v", r)
		}
	}
	// Placed before the first static input rule, after the dynamic one.
	if rows[0]["dynamic"] != "true" || rows[1]["comment"] != "ftn: customer dns" || rows[2]["comment"] != "ftn: customer dns" || rows[3][".id"] != first {
		t.Fatalf("order: %v (first=%s)", rows, first)
	}
	// A second run adds nothing.
	n := len(m.Table("/ip/firewall/filter"))
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckCustDNS, false, store.RoleOperator); err != nil || len(m.Table("/ip/firewall/filter")) != n {
		t.Fatalf("%v", err)
	}
	// An existing rule of ours that sits below a drop does not take effect: manual, never a duplicate.
	clear()
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53", "in-interface": "br-hotspot", "comment": "ftn: customer dns"})
	it = item(t, get(t, f, s.ID, true), CheckCustDNS)
	if it.Fix.Kind != FixManual || !strings.Contains(it.Detail, "does not take effect") {
		t.Fatalf("%+v", it)
	}
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckCustDNS, false, store.RoleOwner); !errors.Is(err, ErrNoFix) {
		t.Fatalf("%v", err)
	}
	// The customer interface in the LAN list is already accepted by the defconf rule shape.
	clear()
	m.SeedRow("/interface/list/member", R{"list": "LAN", "interface": "br-hotspot"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "in-interface-list": "LAN"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"})
	if it = item(t, get(t, f, s.ID, true), CheckCustDNS); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
}

func TestFixWriterFirewallAndHotspotRules(t *testing.T) {
	m := routeros.NewMock()
	ctx := context.Background()
	w := &fixWriter{rt: m, insp: m, ifaces: map[string]bool{}, custIface: "br-hotspot"}
	good := R{"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53", "in-interface": "br-hotspot", "comment": "ftn: customer dns"}
	if err := w.addRow(ctx, "/ip/firewall/filter", good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(R){
		"comment":   func(r R) { r["comment"] = "x" },
		"action":    func(r R) { r["action"] = "drop" },
		"chain":     func(r R) { r["chain"] = "forward" },
		"port":      func(r R) { r["dst-port"] = "22" },
		"protocol":  func(r R) { r["protocol"] = "icmp" },
		"interface": func(r R) { r["in-interface"] = "ether1" },
		"extra":     func(r R) { r["src-address"] = "0.0.0.0/0" },
	} {
		b := R{}
		for k, v := range good {
			b[k] = v
		}
		mut(b)
		if err := w.addRow(ctx, "/ip/firewall/filter", b); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := w.setRow(ctx, "/ip/firewall/filter", R{".id": "*1", "comment": "ftn: customer dns"}, R{"disabled": "yes"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("set: %v", err)
	}
	if err := w.removeRow(ctx, "/ip/firewall/filter", R{".id": "*1", "comment": "ftn: customer dns"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("remove: %v (the DNS rules are permanent)", err)
	}
	w.custIface = ""
	if err := w.addRow(ctx, "/ip/firewall/filter", good); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("no customer interface: %v", err)
	}
	// Hotspot rows are re-judged on the row itself at removal.
	row := R{".id": "*9", "name": "K7Q2MX", "comment": "ispbill:Flat 3B"}
	if err := w.removeRow(ctx, "/ip/hotspot/user", row); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("no judge: %v", err)
	}
	w.orphans = func(path string, r routeros.Row) bool { return false }
	if err := w.removeRow(ctx, "/ip/hotspot/user", row); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("an account holds it: %v", err)
	}
	w.orphans = func(path string, r routeros.Row) bool { return true }
	if err := w.removeRow(ctx, "/ip/hotspot/user", row); err != nil {
		t.Errorf("orphan: %v", err)
	}
}

func TestOLTInOtherBridgeIsConfirmOwnerAndNeverAutomatic(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	liveOLT(m) // defconf bridge
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckOLT, false, store.RoleOwner); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("%v", err)
	}
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckOLT, true, store.RoleOperator); !errors.Is(err, ErrNeedsOwner) {
		t.Fatalf("%v", err)
	}
	// Called straight (a stale auto item): refused without confirm.
	m.ResetWrites()
	if err := fixOLT(ctx, f.fixEnv(s, m, false, store.RoleOperator)); !errors.Is(err, ErrConfirmRequired) || m.Writes() != 0 {
		t.Fatalf("%v", err)
	}
	// The automatic runs (after a join) leave it alone.
	out, err := f.m.ApplyAutoFixes(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out {
		if o.ID == CheckOLT {
			t.Fatalf("%+v", o)
		}
	}
	if !hasTable(m, "/interface/bridge/port", "interface", "sfp-sfpplus1") {
		t.Fatal("removed by an automatic run")
	}
	// Even called as the system actor.
	if _, err := f.m.applyFix(ctx, s.ID, CheckOLT, false, ActorSystem, false); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("%v", err)
	}
	res, err := f.m.ApplyFix(ctx, s.ID, CheckOLT, true, store.RoleOwner)
	if err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
}
