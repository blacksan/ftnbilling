package sites

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func (f *fixture) fixEnv(s store.Site, m routeros.Client, confirm bool, role string) *fixEnv {
	eff := f.m.Effective(s)
	insp, _ := m.(routeros.Inspector)
	w := &fixWriter{rt: m, insp: insp, ifaces: map[string]bool{eff.OLTInterface: true, eff.BenchPort: true}}
	return &fixEnv{m: f.m, site: eff, w: w, confirm: confirm, role: role}
}

func hasTable(m *routeros.Mock, path, key, val string) bool {
	for _, r := range m.Table(path) {
		if r[key] == val {
			return true
		}
	}
	return false
}

func auditMsgs(f *fixture, siteID string) (out []string) {
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == siteID && e.Kind == "audit" && strings.HasPrefix(e.Msg, "setup fix") {
				out = append(out, e.Msg)
			}
		}
	})
	return
}

func TestFixWriterIsAnAllowlist(t *testing.T) {
	m := routeros.NewMock()
	w := &fixWriter{rt: m, insp: m, ifaces: map[string]bool{"sfp-sfpplus1": true, "ether5": true}}
	ctx := context.Background()
	for _, p := range []string{"/user", "/user/group", "/ip/service", "/system/scheduler", "/system/script", "/system/identity",
		"/interface/wireguard", "/interface/wireguard/peers", "/ip/route", "/ip/address", "/ip/firewall/filter", "/ip/firewall/nat",
		"/ip/firewall/address-list", "/interface/list/member", "/ip/dhcp-server", "/ip/hotspot"} {
		if err := w.addRow(ctx, p, R{"name": "x", "comment": "ftn: netflow"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("addRow %s: %v", p, err)
		}
		if err := w.setRow(ctx, p, R{".id": "*1", "interface": "ether5", "comment": "ftn: netflow"}, R{"disabled": "yes"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("setRow %s: %v", p, err)
		}
		if err := w.removeRow(ctx, p, R{".id": "*1", "interface": "ether5", "comment": "ftn: netflow"}); !errors.Is(err, ErrFixNotAllowed) && p != "/ip/hotspot" {
			t.Errorf("removeRow %s: %v", p, err)
		}
		if err := w.call(ctx, p+"/set", R{"x": "y"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("call %s: %v", p, err)
		}
	}
	// The two allowed commands are narrow.
	if err := w.call(ctx, "/ip/dns/set", R{"servers": "1.1.1.1"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("dns servers: %v", err)
	}
	if err := w.call(ctx, "/ip/dns/set", R{"allow-remote-requests": "yes", "servers": "1.1.1.1"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("dns two props: %v", err)
	}
	if err := w.call(ctx, "/ip/traffic-flow/set", R{"cache-entries": "1k"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("traffic-flow other: %v", err)
	}
	// Bridge ports: only this building's OLT and bench rows, and set only moves the bridge.
	if err := w.removeRow(ctx, "/interface/bridge/port", R{".id": "*1", "interface": "ether1"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("WAN port row: %v", err)
	}
	if err := w.addRow(ctx, "/interface/bridge/port", R{"bridge": "b", "interface": "ether2"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("other port: %v", err)
	}
	if err := w.setRow(ctx, "/interface/bridge/port", R{".id": "*1", "interface": "ether5"}, R{"disabled": "yes"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("set other property: %v", err)
	}
	if err := w.removeRow(ctx, "/ip/traffic-flow/target", R{".id": "*1", "comment": "someone else's"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("foreign target: %v", err)
	}
	if err := w.removeRow(ctx, "/ip/hotspot/ip-binding", R{".id": "*1", "comment": "ftn: keep"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("our binding: %v", err)
	}
	if m.Writes() != 0 || len(m.Calls()) != 0 {
		t.Fatalf("something was written: %d %v", m.Writes(), m.Calls())
	}
}

func TestApplyFixRolesAndKinds(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckDNS, false, store.RoleViewer); !errors.Is(err, ErrFixForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	if _, err := f.m.ApplyFix(ctx, s.ID, "nope", false, store.RoleOwner); !errors.Is(err, ErrUnknownCheck) {
		t.Fatalf("%v", err)
	}
	if _, err := f.m.ApplyFix(ctx, "nope", CheckDNS, false, store.RoleOwner); !errors.Is(err, billing.ErrNoSite) {
		t.Fatalf("%v", err)
	}
	// A healthy item: nothing to do, nothing written.
	m.ResetWrites()
	res, err := f.m.ApplyFix(ctx, s.ID, CheckDNS, false, store.RoleOperator)
	if err != nil || len(res.Applied) != 0 || res.Item.Status != StatusOK || m.Writes() != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// A manual item gives its command in the error.
	m.SeedOne("/system/device-mode", R{"scheduler": "false", "fetch": "true", "hotspot": "true"})
	_, err = f.m.ApplyFix(ctx, s.ID, CheckDMScheduler, true, store.RoleOwner)
	if !errors.Is(err, ErrNoFix) || !strings.Contains(err.Error(), "/system/device-mode/update scheduler=yes") {
		t.Fatalf("%v", err)
	}
	if m.Writes() != 0 {
		t.Fatal("manual fixes write nothing")
	}
}

func TestFixDNS(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "no"})
	res, err := f.m.ApplyFix(ctx, s.ID, CheckDNS, false, store.RoleOperator)
	if err != nil || res.Item.Status != StatusOK || len(res.Applied) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Path != "/ip/dns/set" || len(calls[0].Body) != 1 || calls[0].Body["allow-remote-requests"] != "yes" {
		t.Fatalf("%v", calls)
	}
	if msgs := auditMsgs(f, s.ID); len(msgs) != 1 || !strings.Contains(msgs[0], "by operator") {
		t.Fatalf("audit: %v", msgs)
	}
	// The firewall changes between the checklist and the click: refuse, write nothing.
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "no"})
	for _, r := range m.Table("/ip/firewall/filter") {
		m.RemoveRow(ctx, "/ip/firewall/filter", r[".id"])
	}
	m.ResetWrites()
	err = fixDNS(ctx, f.fixEnv(s, m, false, store.RoleOperator))
	if !errors.Is(err, ErrPreconditionChanged) || m.Writes() != 0 {
		t.Fatalf("%v writes=%d", err, m.Writes())
	}
}

func TestFixOLTPortAutomatic(t *testing.T) {
	f, s, m := checkSite(t, nil)
	liveOLT(m)
	res, err := f.m.ApplyFix(context.Background(), s.ID, CheckOLT, false, store.RoleOperator)
	if err != nil || res.Item.Status != StatusOK || len(res.Applied) != 1 || !strings.Contains(res.Applied[0], "sfp-sfpplus1") {
		t.Fatalf("%+v %v", res, err)
	}
	if hasTable(m, "/interface/bridge/port", "interface", "sfp-sfpplus1") {
		t.Fatal("the OLT port is still a bridge port")
	}
	if !hasTable(m, "/interface/bridge/port", "interface", "vlan100-customers") || !hasTable(m, "/interface/bridge/port", "interface", "ether5") {
		t.Fatal("other bridge ports must stay")
	}
}

func TestFixOLTRefusesWhenLearnedHostsAppear(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	liveOLT(m)
	// The checklist said auto; a device shows up before the fix runs.
	m.SeedRow("/interface/bridge/host", R{"mac-address": "00:19:3E:00:00:09", "on-interface": "sfp-sfpplus1", "local": "false"})
	m.ResetWrites()
	err := fixOLT(ctx, f.fixEnv(s, m, false, store.RoleOperator))
	if !errors.Is(err, ErrConfirmRequired) || !strings.Contains(err.Error(), "00:19:3E:00:00:09") || m.Writes() != 0 {
		t.Fatalf("%v writes=%d", err, m.Writes())
	}
	// Through ApplyFix: confirm kind, owner only.
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckOLT, false, store.RoleOwner); !errors.Is(err, ErrConfirmRequired) || !strings.Contains(err.Error(), "00:19:3E:00:00:09") {
		t.Fatalf("%v", err)
	}
	if _, err := f.m.ApplyFix(ctx, s.ID, CheckOLT, true, store.RoleOperator); !errors.Is(err, ErrNeedsOwner) {
		t.Fatalf("%v", err)
	}
	if m.Writes() != 0 {
		t.Fatal("nothing may be written without confirmation by an owner")
	}
	res, err := f.m.ApplyFix(ctx, s.ID, CheckOLT, true, store.RoleOwner)
	if err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if hasTable(m, "/interface/bridge/port", "interface", "sfp-sfpplus1") {
		t.Fatal("not removed")
	}
}

func TestFixOLTRefusesWANAndLayer3(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	liveOLT(m)
	m.SeedRow("/interface/list/member", R{"list": "WAN", "interface": "sfp-sfpplus1"})
	m.ResetWrites()
	if err := fixOLT(ctx, f.fixEnv(s, m, true, store.RoleOwner)); !errors.Is(err, ErrPreconditionChanged) || m.Writes() != 0 {
		t.Fatalf("WAN: %v", err)
	}
	for _, r := range m.Table("/interface/list/member") {
		if r["interface"] == "sfp-sfpplus1" {
			m.RemoveRow(ctx, "/interface/list/member", r[".id"])
		}
	}
	for _, c := range []struct{ path, key string }{{"/ip/dhcp-server", "interface"}, {"/ip/dhcp-client", "interface"}, {"/ip/hotspot", "interface"}, {"/ip/address", "interface"}} {
		id := m.SeedRow(c.path, R{"name": "x", "address": "1.2.3.4/24", c.key: "sfp-sfpplus1"})
		m.ResetWrites()
		if err := fixOLT(ctx, f.fixEnv(s, m, true, store.RoleOwner)); !errors.Is(err, ErrPreconditionChanged) || m.Writes() != 0 {
			t.Fatalf("%s: %v", c.path, err)
		}
		m.RemoveRow(ctx, c.path, id)
	}
	// The VLAN interface is not in a bridge: removing the port would cut customers off.
	for _, r := range m.Table("/interface/bridge/port") {
		if r["interface"] == "vlan100-customers" {
			m.RemoveRow(ctx, "/interface/bridge/port", r[".id"])
		}
	}
	m.ResetWrites()
	if err := fixOLT(ctx, f.fixEnv(s, m, true, store.RoleOwner)); !errors.Is(err, ErrPreconditionChanged) || m.Writes() != 0 {
		t.Fatalf("stranded VLAN: %v", err)
	}
	// An unreadable router is "unreachable", not a silent success.
	m.SetPathFail("/interface/bridge/host", errors.New("403"))
	if err := fixOLT(ctx, f.fixEnv(s, m, true, store.RoleOwner)); !errors.Is(err, ErrRouterUnreachable) {
		t.Fatalf("%v", err)
	}
}

func TestFixBenchPort(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	for _, r := range m.Table("/interface/bridge/port") {
		m.RemoveRow(ctx, "/interface/bridge/port", r[".id"])
	}
	res, err := f.m.ApplyFix(ctx, s.ID, CheckBench, false, store.RoleOperator)
	if err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	rows := m.Table("/interface/bridge/port")
	if len(rows) != 1 || rows[0]["interface"] != "ether5" || rows[0]["bridge"] != "br-hotspot" || rows[0]["comment"] != "ftn: bench port" {
		t.Fatalf("%v", rows)
	}
	// In another bridge with nobody on it: moved, not duplicated.
	for _, r := range m.Table("/interface/bridge/port") {
		m.SetRow(ctx, "/interface/bridge/port", r[".id"], R{"bridge": "bridge"})
	}
	if res, err = f.m.ApplyFix(ctx, s.ID, CheckBench, false, store.RoleOperator); err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if rows = m.Table("/interface/bridge/port"); len(rows) != 1 || rows[0]["bridge"] != "br-hotspot" {
		t.Fatalf("%v", rows)
	}
	// With a laptop on it: confirm + owner.
	for _, r := range m.Table("/interface/bridge/port") {
		m.SetRow(ctx, "/interface/bridge/port", r[".id"], R{"bridge": "bridge"})
	}
	m.SeedRow("/interface/bridge/host", R{"mac-address": "3C:00:00:00:00:01", "on-interface": "ether5", "local": "false"})
	if _, err = f.m.ApplyFix(ctx, s.ID, CheckBench, false, store.RoleOwner); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("%v", err)
	}
	if _, err = f.m.ApplyFix(ctx, s.ID, CheckBench, true, store.RoleOperator); !errors.Is(err, ErrNeedsOwner) {
		t.Fatalf("%v", err)
	}
	if res, err = f.m.ApplyFix(ctx, s.ID, CheckBench, true, store.RoleOwner); err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestFixTrafficFlow(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	m.SeedOne("/ip/traffic-flow", R{"enabled": "false", "interfaces": "all"})
	for _, r := range m.Table("/ip/traffic-flow/target") {
		m.RemoveRow(ctx, "/ip/traffic-flow/target", r[".id"])
	}
	m.SeedRow("/ip/traffic-flow/target", R{"dst-address": "9.9.9.9", "port": "9999", "comment": "other collector"})
	res, err := f.m.ApplyFix(ctx, s.ID, CheckTrafficFlow, false, store.RoleOperator)
	if err != nil || res.Item.Status != StatusOK || len(res.Applied) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	calls := m.Calls()
	if len(calls) != 1 || calls[0].Path != "/ip/traffic-flow/set" || calls[0].Body["interfaces"] != "br-hotspot" || calls[0].Body["enabled"] != "yes" {
		t.Fatalf("%v", calls)
	}
	var ours, other int
	for _, r := range m.Table("/ip/traffic-flow/target") {
		switch r["comment"] {
		case "ftn: netflow":
			ours++
			if r["dst-address"] != "10.99.0.1" || r["port"] != "2055" || r["version"] != "5" {
				t.Fatalf("%v", r)
			}
		case "other collector":
			other++
		}
	}
	if ours != 1 || other != 1 {
		t.Fatalf("ours=%d other=%d", ours, other)
	}
	// A wrong target of ours is corrected in place.
	for _, r := range m.Table("/ip/traffic-flow/target") {
		if r["comment"] == "ftn: netflow" {
			m.SetRow(ctx, "/ip/traffic-flow/target", r[".id"], R{"dst-address": "1.1.1.1", "disabled": "yes"})
		}
	}
	if res, err = f.m.ApplyFix(ctx, s.ID, CheckTrafficFlow, false, store.RoleOperator); err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if n := len(m.Table("/ip/traffic-flow/target")); n != 2 {
		t.Fatalf("%d targets", n)
	}
}

func TestFixOrphans(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	m.SeedRow("/ip/hotspot/user", R{"name": "K7Q2MX", "mac-address": "AA:BB:CC:DD:EE:01", "comment": "ispbill:Flat 3B"}) // a flat: stays
	m.SeedRow("/ip/hotspot/user", R{"name": "GONE01", "comment": "ispbill:Flat 99"})
	res, err := f.m.ApplyFix(ctx, s.ID, CheckOrphans, false, store.RoleOperator)
	if err != nil || res.Item.Status != StatusOK || len(res.Applied) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !hasTable(m, "/ip/hotspot/user", "name", "K7Q2MX") || hasTable(m, "/ip/hotspot/user", "name", "GONE01") {
		t.Fatalf("%v", m.Table("/ip/hotspot/user"))
	}
	// A test login nobody made through the server: confirm.
	m.SeedRow("/ip/hotspot/user", R{"name": "GONE02", "comment": "ispbill:Flat 98"})
	m.SeedRow("/ip/hotspot/user", R{"name": "test1", "mac-address": "3C:00:00:00:00:01"})
	m.SeedRow("/ip/hotspot/ip-binding", R{"mac-address": "3C:00:00:00:00:02", "type": "bypassed"})
	if _, err = f.m.ApplyFix(ctx, s.ID, CheckOrphans, false, store.RoleOperator); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("%v", err)
	}
	// The automatic run removes only what the server made.
	out, err := f.m.ApplyAutoFixes(ctx, s.ID)
	if err != nil || len(out) != 1 || out[0].ID != CheckOrphans || out[0].Err != "" {
		t.Fatalf("%+v %v", out, err)
	}
	if hasTable(m, "/ip/hotspot/user", "name", "GONE02") || !hasTable(m, "/ip/hotspot/user", "name", "test1") || len(m.Table("/ip/hotspot/ip-binding")) != 1 {
		t.Fatalf("%v %v", m.Table("/ip/hotspot/user"), m.Table("/ip/hotspot/ip-binding"))
	}
	if res, err = f.m.ApplyFix(ctx, s.ID, CheckOrphans, true, store.RoleOperator); err != nil || res.Item.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if hasTable(m, "/ip/hotspot/user", "name", "test1") || len(m.Table("/ip/hotspot/ip-binding")) != 0 || !hasTable(m, "/ip/hotspot/user", "name", "K7Q2MX") {
		t.Fatalf("%v", m.Table("/ip/hotspot/user"))
	}
	// An account that appears between the check and the click is never removed.
	m.SeedRow("/ip/hotspot/user", R{"name": "NEW001", "comment": "ispbill:Flat 7"})
	f.st.Update(func(d *store.Data) error {
		d.Accounts["a9"] = store.Account{ID: "a9", SiteID: s.ID, Code: "NEW001", Flat: "Flat 7", CreatedAt: f.now}
		return nil
	})
	m.ResetWrites()
	if err := fixOrphans(ctx, f.fixEnv(s, m, true, store.RoleOwner)); err != nil || m.Writes() != 0 {
		t.Fatalf("%v writes=%d", err, m.Writes())
	}
}

func TestApplyAutoFixesRunsOnlyAutoFixesAndAuditsEach(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	liveOLT(m)
	m.SeedRow("/interface/bridge/host", R{"mac-address": "00:19:3E:00:00:09", "on-interface": "sfp-sfpplus1", "local": "false"}) // OLT stays: confirm
	m.SeedOne("/ip/traffic-flow", R{"enabled": "no", "interfaces": "all"})                                                       // auto
	m.SeedOne("/system/device-mode", R{"scheduler": "no", "fetch": "yes", "hotspot": "yes"})                                     // manual
	out, err := f.m.ApplyAutoFixes(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != CheckTrafficFlow || out[0].Err != "" || len(out[0].Applied) == 0 {
		t.Fatalf("%+v", out)
	}
	if !hasTable(m, "/interface/bridge/port", "interface", "sfp-sfpplus1") {
		t.Fatal("a confirm fix must not run automatically")
	}
	if msgs := auditMsgs(f, s.ID); len(msgs) != 1 || !strings.Contains(msgs[0], "by system") {
		t.Fatalf("%v", msgs)
	}
	// A second run finds nothing to do.
	if out, _ = f.m.ApplyAutoFixes(ctx, s.ID); len(out) != 0 {
		t.Fatalf("%+v", out)
	}
	m.SetFail(errors.New("down"))
	if _, err := f.m.ApplyAutoFixes(ctx, s.ID); !errors.Is(err, ErrRouterUnreachable) {
		t.Fatalf("%v", err)
	}
}

// refusing wraps a Mock whose bridge-port removal the router refuses.
type refusing struct{ *routeros.Mock }

func (r refusing) RemoveRow(ctx context.Context, path, id string) error {
	return &routeros.APIError{Status: 400, Message: "Bad Request", Detail: "cannot remove: in use"}
}

func TestFixRouterRefusedAndUnreachableErrors(t *testing.T) {
	f, s, m := checkSite(t, nil)
	f.m.opt.NewClient = func(store.Site) routeros.Client { return refusing{m} }
	f.m.Reload(s.ID)
	liveOLT(m)
	_, err := f.m.ApplyFix(context.Background(), s.ID, CheckOLT, false, store.RoleOperator)
	var rr *RouterRefusedError
	if !errors.As(err, &rr) || !strings.Contains(rr.Msg, "cannot remove: in use") {
		t.Fatalf("%v", err)
	}
	if msgs := auditMsgs(f, s.ID); len(msgs) != 1 || !strings.Contains(msgs[0], "failed") {
		t.Fatalf("a failed fix is audited too: %v", msgs)
	}
	if e := classifyFixErr(errors.New("dial tcp: i/o timeout")); !errors.Is(e, ErrRouterUnreachable) {
		t.Fatalf("%v", e)
	}
}

func TestAutoFixRunsOnceAfterTheFirstJoinAnswer(t *testing.T) {
	f := newFixture(t, nil)
	f.m.autoFixDelay = 10 * time.Millisecond
	s := f.joined("Block A")
	if err := f.m.UpdateSite(s.ID, SiteInput{Name: "Block A", Provision: true, BenchPort: "ether5"}); err != nil {
		t.Fatal(err)
	}
	m := routeros.NewMock()
	f.mocks[s.ID] = m
	seedHealthy(m)
	liveOLT(m)
	f.start() // the router answers its first ping: the building counts as joined
	deadline := time.Now().Add(5 * time.Second)
	for hasTable(m, "/interface/bridge/port", "interface", "sfp-sfpplus1") {
		if time.Now().After(deadline) {
			t.Fatal("the post-join fixes did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
