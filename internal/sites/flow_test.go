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

type R = routeros.Row

func (f *fixture) startedRouter(siteID string) *routeros.Mock {
	f.t.Helper()
	f.start()
	f.m.Reload(siteID)
	f.m.Router(siteID)
	return f.mocks[siteID]
}

// ---- delivery ----------------------------------------------------------------------------

func TestSetupScriptAndResetStubDelivery(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	tok, _, err := f.m.MintJoin(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	// the stub is not served before the setup script was fetched
	if _, err := f.m.ResetStub(tok, mt); !errors.Is(err, ErrJoinInvalid) {
		t.Fatalf("stub before setup: %v", err)
	}
	script, id, err := f.m.ConsumeJoin(tok, mt+"/7.25.1 (fetch)", "1.2.3.4", false)
	if err != nil || id != s.ID || !strings.Contains(string(script), "ftn: ") {
		t.Fatalf("%v %s", err, id)
	}
	// the token is single use for the setup script
	if _, _, err := f.m.ConsumeJoin(tok, mt, "1.2.3.4", false); !errors.Is(err, ErrJoinInvalid) {
		t.Fatalf("second fetch: %v", err)
	}
	// the stub follows within five minutes, more than once, and only to a router
	f.advance(4 * time.Minute)
	for i := 0; i < 2; i++ {
		stub, err := f.m.ResetStub(tok, mt)
		if err != nil || string(stub) != ResetStubScript {
			t.Fatalf("stub: %v", err)
		}
	}
	if _, err := f.m.ResetStub(tok, "curl/8"); !errors.Is(err, ErrJoinInvalid) {
		t.Fatalf("stub to a browser: %v", err)
	}
	if _, err := f.m.ResetStub("nope", mt); !errors.Is(err, ErrJoinInvalid) {
		t.Fatalf("wrong token: %v", err)
	}
	f.advance(2 * time.Minute)
	if _, err := f.m.ResetStub(tok, mt); !errors.Is(err, ErrJoinInvalid) {
		t.Fatalf("stub after the window: %v", err)
	}
	got := f.site(s.ID)
	if got.WGPublicKey == "" || got.RESTPass == "" || got.AccessMode != store.AccessIPoE {
		t.Fatalf("keys not made: %+v", got)
	}
	if strings.Contains(string(script), got.RESTPass) == false {
		t.Fatal("the script does not hold the router's REST password")
	}
}

func TestDownloadScriptMakesNewKeysEachTime(t *testing.T) {
	f := newFixture(t, nil)
	s := f.create("Block A")
	a, err := f.m.DownloadScript(s.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	k1 := f.site(s.ID).WGPublicKey
	b, err := f.m.DownloadScript(s.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if k1 == f.site(s.ID).WGPublicKey || string(a) == string(b) {
		t.Fatal("a second download must make new keys")
	}
}

func TestPPPoEPasswordIsSealedAndOnlyInTheScript(t *testing.T) {
	f := newFixture(t, func(c *config.Config) { c.DataKey = []byte(strings.Repeat("k", 32)) })
	s, err := f.m.CreateRouter(SiteInput{Name: "P", WANMode: WANPPPoE, PPPoEUser: "u1", PPPoEPass: "s3cretpw"})
	if err != nil {
		t.Fatal(err)
	}
	got := f.site(s.ID)
	if got.PPPoEPassEnc == "" || strings.Contains(got.PPPoEPassEnc, "s3cretpw") {
		t.Fatalf("not sealed: %q", got.PPPoEPassEnc)
	}
	script, err := f.m.DownloadScript(s.ID, "owner")
	if err != nil || !strings.Contains(string(script), "s3cretpw") {
		t.Fatalf("script lacks the password: %v", err)
	}
	// clearing it
	if err := f.m.UpdateSite(s.ID, SiteInput{Name: "P", WANMode: WANDHCP, PPPoEPassClear: true}); err != nil {
		t.Fatal(err)
	}
	if f.site(s.ID).PPPoEPassEnc != "" {
		t.Fatal("not cleared")
	}
}

func TestPPPoEWithoutDataKeyIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.m.CreateRouter(SiteInput{Name: "P", WANMode: WANPPPoE, PPPoEUser: "u1", PPPoEPass: "pw"})
	if !errors.Is(err, ErrNoDataKey) {
		t.Fatalf("%v", err)
	}
}

// ---- reset -------------------------------------------------------------------------------

func TestResetRouterForgetsTheRouterButKeepsFlats(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.st.Update(func(d *store.Data) error {
		d.Settings["site."+s.ID+".join_report"] = store.Setting{Key: "site." + s.ID + ".join_report", Value: "x"}
		d.Settings["unrelated"] = store.Setting{Key: "unrelated", Value: "keep"}
		return nil
	})
	if err := f.m.ResetRouter(s.ID, "wrong", "owner"); !errors.Is(err, ErrNameMismatch) {
		t.Fatalf("%v", err)
	}
	if err := f.m.ResetRouter(s.ID, "Block A", "owner"); err != nil {
		t.Fatal(err)
	}
	got := f.site(s.ID)
	if got.WGPublicKey != "" || got.RESTPass != "" || !got.JoinedAt.IsZero() || got.Join.Hash != "" || got.TunnelIP == "" {
		t.Fatalf("%+v", got)
	}
	f.st.View(func(d *store.Data) {
		if _, ok := d.Settings["site."+s.ID+".join_report"]; ok {
			t.Error("router settings kept")
		}
		if _, ok := d.Settings["unrelated"]; !ok {
			t.Error("other settings removed")
		}
		if len(d.Accounts) != 2 {
			t.Errorf("accounts: %d", len(d.Accounts))
		}
	})
	// a new line can be made
	if _, _, err := f.m.MintJoin(s.ID); err != nil {
		t.Fatal(err)
	}
}

// ---- checks ------------------------------------------------------------------------------

func checkRig(t *testing.T) (*fixture, store.Site, *routeros.Mock) {
	t.Helper()
	f := newFixture(t, nil)
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	m := f.startedRouter(s.ID)
	seedHealthy(t, f, s, m)
	return f, f.site(s.ID), m
}

func seedHealthy(t *testing.T, f *fixture, s store.Site, m *routeros.Mock) {
	m.SeedOne("/system/resource", R{"version": "7.25.1 (stable)", "board-name": "RB5009UG+S+"})
	m.SeedRow("/ip/dhcp-server", R{"name": "ftn-dhcp", "interface": "br-customers", "disabled": "false"})
	m.SeedRow("/ip/route", R{"dst-address": "0.0.0.0/0", "gateway": "192.168.1.1", "active": "true", "disabled": "false"})
	w, err := f.m.walledSet(f.m.Effective(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groupsOf(w) {
		for _, r := range g.want {
			m.SeedRow(g.path, R(r.Body(CustomerBridge)))
		}
	}
}

func checkItem(t *testing.T, f *fixture, id, check string) CheckItem {
	t.Helper()
	c, err := f.m.Checks(context.Background(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	it, ok := c.Item(check)
	if !ok {
		t.Fatalf("no check %s", check)
	}
	return it
}

func TestWalledRulesCheckAndRepair(t *testing.T) {
	f, s, m := checkRig(t)
	if it := checkItem(t, f, s.ID, CheckWalled); it.Status != StatusOK {
		t.Fatalf("healthy: %+v", it)
	}
	// take one forward rule away and change another
	rows := m.Table("/ip/firewall/filter")
	var del, chg R
	for _, r := range rows {
		if strings.HasPrefix(r["comment"], "ftn: ipoe") {
			if del == nil {
				del = r
			} else if chg == nil {
				chg = r
			}
		}
	}
	if del == nil || chg == nil {
		t.Fatalf("no walled rows: %v", rows)
	}
	m.RemoveRow(context.Background(), "/ip/firewall/filter", del[".id"])
	m.SetRow(context.Background(), "/ip/firewall/filter", chg[".id"], map[string]string{"action": "accept"})
	m.SeedRow("/ip/firewall/nat", R{"chain": "dstnat", "comment": "ftn: ipoe old rule", "action": "accept"})
	it := checkItem(t, f, s.ID, CheckWalled)
	if it.Status != StatusFail || it.Fix.Kind != FixAuto {
		t.Fatalf("broken: %+v", it)
	}
	res, err := f.m.ApplyFix(context.Background(), s.ID, CheckWalled, false, store.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if res.Item.Status != StatusOK || len(res.Applied) == 0 {
		t.Fatalf("%+v", res)
	}
	if hasTable(m, "/ip/firewall/nat", "comment", "ftn: ipoe old rule") {
		t.Fatal("stale rule left")
	}
}

func hasTable(m *routeros.Mock, path, key, val string) bool {
	for _, r := range m.Table(path) {
		if r[key] == val {
			return true
		}
	}
	return false
}

func TestDHCPCheckAndFix(t *testing.T) {
	f, s, m := checkRig(t)
	if it := checkItem(t, f, s.ID, CheckDHCP); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	for _, r := range m.Table("/ip/dhcp-server") {
		m.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	it := checkItem(t, f, s.ID, CheckDHCP)
	if it.Status != StatusFail || it.Fix.Kind != FixAuto {
		t.Fatalf("%+v", it)
	}
	if _, err := f.m.ApplyFix(context.Background(), s.ID, CheckDHCP, false, store.RoleOperator); err != nil {
		t.Fatal(err)
	}
	if it := checkItem(t, f, s.ID, CheckDHCP); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	// no server at all: failed, and no fix (a new line builds it)
	for _, r := range m.Table("/ip/dhcp-server") {
		m.RemoveRow(context.Background(), "/ip/dhcp-server", r[".id"])
	}
	if it := checkItem(t, f, s.ID, CheckDHCP); it.Status != StatusFail || it.Fix.Kind != FixNone {
		t.Fatalf("%+v", it)
	}
}

func TestWANCheck(t *testing.T) {
	f, s, m := checkRig(t)
	if it := checkItem(t, f, s.ID, CheckWAN); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	for _, r := range m.Table("/ip/route") {
		m.RemoveRow(context.Background(), "/ip/route", r[".id"])
	}
	if it := checkItem(t, f, s.ID, CheckWAN); it.Status != StatusFail {
		t.Fatalf("%+v", it)
	}
}

func TestBootFileCheckAndFix(t *testing.T) {
	f, s, m := checkRig(t)
	if it := checkItem(t, f, s.ID, CheckBootFile); it.Status != StatusOK {
		t.Fatalf("no file: %+v", it)
	}
	m.SeedRow("/file", R{"name": SetupFile, "size": "9000"})
	m.SeedRow("/file", R{"name": "backup.backup"})
	// just fetched, no report: the script may still be running
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.Join.UsedAt = f.now
		d.Sites[s.ID] = x
		return nil
	})
	if it := checkItem(t, f, s.ID, CheckBootFile); it.Status != StatusInfo {
		t.Fatalf("running: %+v", it)
	}
	f.m.ApplyFix(context.Background(), s.ID, CheckBootFile, false, store.RoleOperator)
	if !hasTable(m, "/file", "name", SetupFile) {
		t.Fatal("removed a file of a running script")
	}
	f.advance(ReportWindow + time.Minute)
	if it := checkItem(t, f, s.ID, CheckBootFile); it.Status != StatusWarn || it.Fix.Kind != FixAuto {
		t.Fatalf("finished: %+v", it)
	}
	if _, err := f.m.ApplyFix(context.Background(), s.ID, CheckBootFile, false, store.RoleOperator); err != nil {
		t.Fatal(err)
	}
	if hasTable(m, "/file", "name", SetupFile) || !hasTable(m, "/file", "name", "backup.backup") {
		t.Fatalf("%v", m.Table("/file"))
	}
}

func TestFixWriterIsAnAllowlist(t *testing.T) {
	m := routeros.NewMock()
	w := &fixWriter{rt: m, insp: m, ifaces: map[string]bool{"sfp-sfpplus1": true, "ether5": true}}
	ctx := context.Background()
	for _, p := range []string{"/user", "/user/group", "/ip/service", "/system/scheduler", "/system/script", "/system/identity",
		"/interface/wireguard", "/interface/wireguard/peers", "/ip/route", "/ip/address", "/ip/firewall/address-list",
		"/interface/list/member", "/ip/hotspot", "/system/reset-configuration"} {
		if err := w.addRow(ctx, p, R{"name": "x", "comment": "ftn: netflow"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("addRow %s: %v", p, err)
		}
		if err := w.setRow(ctx, p, R{".id": "*1", "comment": "ftn: netflow"}, R{"disabled": "yes"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("setRow %s: %v", p, err)
		}
		if err := w.call(ctx, p+"/set", R{"x": "y"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("call %s: %v", p, err)
		}
	}
	// a firewall rule that is not a walled-garden rule is refused
	if err := w.addRow(ctx, "/ip/firewall/filter", R{"chain": "input", "action": "accept", "comment": "mine"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("foreign rule: %v", err)
	}
}

// ---- setup view --------------------------------------------------------------------------

func TestSetupViewStates(t *testing.T) {
	f := newFixture(t, nil)
	s, err := f.m.CreateRouter(SiteInput{Name: "Block A"})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := f.m.SetupView(context.Background(), s.ID)
	if !ok || v.State != "not_started" || len(v.Steps) != 4 {
		t.Fatalf("%+v", v)
	}
	if v.Steps[0].Key != StepFetched || v.Steps[3].Key != StepChecks {
		t.Fatalf("%+v", v.Steps)
	}
	tok, _, _ := f.m.MintJoin(s.ID)
	f.m.ConsumeJoin(tok, mt, "1.2.3.4", false)
	v, _ = f.m.SetupView(context.Background(), s.ID)
	if v.State != "waiting_router" || v.Steps[0].Status != "ok" || v.Steps[1].Status == "ok" {
		t.Fatalf("%+v", v)
	}
	if _, ok := f.m.SetupView(context.Background(), "nope"); ok {
		t.Fatal("unknown router")
	}
}
