package web_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/payments"
	"ispbill/internal/routeros"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
	"ispbill/internal/web"
	"ispbill/internal/wg"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// multi is an app with no legacy building: sites are created through the
// manager, and each has its own fake router.
type multi struct {
	*app
	mocks map[string]*routeros.Mock
	logs  *syncBuf
}

func (m *multi) addSite(t *testing.T, name string) store.Site {
	t.Helper()
	st, err := m.mgr.CreateSite(sites.SiteInput{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	_, pub, _ := wg.GenerateKeypair()
	m.st.Update(func(d *store.Data) error {
		s := d.Sites[st.ID]
		s.WGPublicKey = pub
		d.Sites[st.ID] = s
		return nil
	})
	m.mgr.Reload(st.ID)
	m.svc.SyncProfiles(context.Background(), st.ID)
	return st
}

func newMulti(t *testing.T) *multi { return newMultiOps(t, nil, nil) }

// newMultiOps is newMulti with the Phase 2 services installed by mkOps.
func newMultiOps(t *testing.T, mut func(*config.Config), mkOps func(cfg config.Config, st *store.Store, mgr *sites.Manager, svc *billing.Service) web.Ops) *multi {
	t.Helper()
	_, pub, _ := wg.GenerateKeypair()
	cfg := config.Config{BrandName: "Building WiFi", TestMode: true, AdminUser: "admin", AdminPass: pw,
		SupportPhone: "0700 111 222", NetflowAddr: "off", PingTargets: []string{"8.8.8.8"},
		WGPublicKey: pub, WGEndpoint: "203.0.113.10:51820", WGSubnet: "10.99.0.0/22", AdminAllowCIDR: "10.98.0.0/24", JoinTokenTTL: 30 * time.Minute,
		JoinRequireMikrotikUA: true, PublicBaseURL: "https://billing.example.co.ke", DataDir: t.TempDir(),
		FlowEvery: time.Hour, PortEvery: time.Hour, PingEvery: time.Hour, DNSEvery: time.Hour, SaveEvery: time.Hour}
	if mut != nil {
		mut(&cfg)
	}
	st, _ := storetest.Open(t, cfg.DataDir)
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	m := &multi{mocks: map[string]*routeros.Mock{}, logs: logs}
	var mu sync.Mutex
	var svc *billing.Service
	mgr := sites.New(sites.Options{Cfg: cfg, Store: st, Log: log,
		Dir: func(id string) metrics.Directory { return svc.Directory(id) },
		NewClient: func(s store.Site) routeros.Client {
			mu.Lock()
			defer mu.Unlock()
			if m.mocks[s.ID] == nil {
				m.mocks[s.ID] = routeros.NewMock()
			}
			return m.mocks[s.ID]
		}})
	svc = billing.NewService(st, mgr, log)
	svc.SeedPackages()
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Wait(); svc.Wait() })
	mp := payments.NewMpesa(payments.MpesaConfig{}, svc, st, log)
	prov := map[string]payments.Provider{"test": &payments.Test{Enabled_: true, Delay: 30 * time.Millisecond, C: svc}, "voucher": payments.Voucher{C: svc}, "mpesa": mp}
	srv := web.New(cfg, st, svc, prov, mp, mgr, log)
	if mkOps != nil {
		srv.SetOps(mkOps(cfg, st, mgr, svc))
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	m.app = &app{ts: ts, st: st, svc: svc, mgr: mgr,
		cl: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	m.admin("GET", "/admin", nil).Body.Close()
	return m
}

func (m *multi) mock(id string) *routeros.Mock {
	for i := 0; i < 50 && m.mocks[id] == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	return m.mocks[id]
}

func (m *multi) selectSite(t *testing.T, id string) {
	t.Helper()
	r := m.admin("GET", "/admin/site/select?site="+url.QueryEscape(id)+"&next=/admin/accounts", nil)
	r.Body.Close()
	if r.StatusCode != 303 {
		t.Fatalf("select: %d", r.StatusCode)
	}
	m.cookie = &http.Cookie{Name: "ftn_site", Value: id}
}

func TestPortalGuardIsPerSite(t *testing.T) {
	m := newMulti(t)
	A, B := m.addSite(t, "Alpha"), m.addSite(t, "Beta")
	macB := "AA:BB:CC:DD:EE:0B"
	if _, err := m.svc.CreateAccount(billing.AccountInput{SiteID: B.ID, Flat: "Flat B1", MAC: macB}); err != nil {
		t.Fatal(err)
	}
	m.mock(A.ID)
	m.mock(B.ID).AddHost(macB, "10.5.50.20")
	time.Sleep(1100 * time.Millisecond)
	// The device is on Beta's hotspot: Beta shows the flat, Alpha does not.
	_, b := m.portal(t, "/portal?site="+B.ID+"&mac="+macB+"&ip=10.5.50.20")
	if !strings.Contains(b, "Flat B1") || !strings.Contains(b, `name="site" value="`+B.ID+`"`) {
		t.Fatalf("site B portal: %s", b)
	}
	_, b = m.portal(t, "/portal?site="+A.ID+"&mac="+macB+"&ip=10.5.50.20")
	if strings.Contains(b, "Flat B1") {
		t.Fatal("site A's portal accepted site B's MAC")
	}
	// No site and two buildings: the neutral page.
	_, b = m.portal(t, "/portal?mac="+macB+"&ip=10.5.50.20")
	if strings.Contains(b, "Flat B1") || !strings.Contains(b, "Open this page from the WiFi login") {
		t.Fatalf("no site should be neutral: %s", b)
	}
}

func TestAdminSwitcherFiltersLists(t *testing.T) {
	m := newMulti(t)
	A, B := m.addSite(t, "Alpha"), m.addSite(t, "Beta")
	m.svc.CreateAccount(billing.AccountInput{SiteID: A.ID, Flat: "Flat A1", MAC: "AA:BB:CC:00:00:01"})
	m.svc.CreateAccount(billing.AccountInput{SiteID: B.ID, Flat: "Flat B1", MAC: "AA:BB:CC:00:00:0B"})
	m.svc.CreateAccount(billing.AccountInput{SiteID: B.ID, Flat: "Flat B2", MAC: "AA:BB:CC:00:00:02"})

	html := m.adminGet(t, "/admin/accounts") // no cookie, two buildings: all
	for _, f := range []string{"Flat A1", "Flat B1", "Flat B2", "<th>Building</th>", "All buildings"} {
		if !strings.Contains(html, f) {
			t.Errorf("all view lacks %q", f)
		}
	}
	m.selectSite(t, A.ID)
	html = m.adminGet(t, "/admin/accounts")
	if !strings.Contains(html, "Flat A1") || strings.Contains(html, "Flat B1") || strings.Contains(html, "<th>Building</th>") {
		t.Fatal("building A view is not filtered")
	}
	m.selectSite(t, B.ID)
	html = m.adminGet(t, "/admin/accounts")
	if strings.Contains(html, "Flat A1") || !strings.Contains(html, "Flat B2") {
		t.Fatal("building B view is not filtered")
	}
	// A new flat in this view lands in the selected building.
	m.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat B3"}, "mac": {"AA:BB:CC:00:00:03"}})
	if m.account(t, "Flat B3").SiteID != B.ID {
		t.Fatal("account created in the wrong building")
	}
	// The switcher refuses to bounce to another host.
	r := m.admin("GET", "/admin/site/select?site=all&next=//evil.example/x", nil)
	r.Body.Close()
	if loc := r.Header.Get("Location"); loc != "/admin" {
		t.Fatalf("open redirect: %q", loc)
	}
}

func TestMoveAccountThroughTheAdmin(t *testing.T) {
	m := newMulti(t)
	A, B := m.addSite(t, "Alpha"), m.addSite(t, "Beta")
	a, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: A.ID, Flat: "Flat 1", MAC: "AA:BB:CC:00:00:09"})
	loc := m.adminPost(t, "/admin/accounts/move", url.Values{"id": {a.ID}, "to": {B.ID}, "confirm": {"WRONG"}})
	if !strings.Contains(loc, "Error") {
		t.Fatalf("move without the code was allowed: %s", loc)
	}
	loc = m.adminPost(t, "/admin/accounts/move", url.Values{"id": {a.ID}, "to": {B.ID}, "confirm": {strings.ToLower(a.Code)}})
	if strings.Contains(loc, "Error") {
		t.Fatalf("move: %s", loc)
	}
	if got, _ := m.svc.GetAccount(a.ID); got.SiteID != B.ID {
		t.Fatal("account not moved")
	}
}

func TestAdminServedOnlyToLoopbackAndAdminNet(t *testing.T) {
	m := newMulti(t)
	for ip, want := range map[string]int{
		"10.99.0.5":    404, // a router tunnel
		"198.51.100.7": 404, // the internet
		"10.98.0.5":    200, // an admin WireGuard peer
		"10.98.1.5":    404, // outside the admin /24
		"127.0.0.1":    200,
	} {
		req, _ := http.NewRequest("GET", m.ts.URL+"/admin", nil)
		req.SetBasicAuth("admin", pw)
		req.Header.Set("X-Forwarded-For", ip)
		r, err := m.cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("admin from %s: %d, want %d", ip, r.StatusCode, want)
		}
	}
	// no forwarded header: the test client is on loopback
	req, _ := http.NewRequest("GET", m.ts.URL+"/admin", nil)
	req.SetBasicAuth("admin", pw)
	r, _ := m.cl.Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Errorf("admin from loopback: %d", r.StatusCode)
	}
}

func (m *multi) joinGet(token, ua string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", m.ts.URL+"/j/"+token, nil)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	r, err := m.cl.Do(req)
	if err != nil {
		panic(err)
	}
	b := new(bytes.Buffer)
	b.ReadFrom(r.Body)
	r.Body.Close()
	return r, b.String()
}

func TestJoinEndpoint(t *testing.T) {
	m := newMulti(t)
	site, err := m.mgr.CreateSite(sites.SiteInput{Name: "Gamma"})
	if err != nil {
		t.Fatal(err)
	}
	// The admin gets the command once, in the POST response.
	r := m.admin("POST", "/admin/sites/join", url.Values{"id": {site.ID}})
	page := body(t, r)
	mt := strings.Index(page, "/j/")
	if r.StatusCode != 200 || mt < 0 || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("join page %d: %.300s\n%s", r.StatusCode, page, m.logs.String())
	}
	rest := page[mt+3:]
	token := rest[:strings.IndexAny(rest, "\"' <&")]
	if len(token) < 40 {
		t.Fatalf("token %q", token)
	}
	if strings.Contains(m.adminGet(t, "/admin/sites/view?id="+site.ID), token) {
		t.Fatal("the command is shown again on a later page load")
	}

	// A link preview (wrong User-Agent) neither works nor uses the token up.
	r1, b1 := m.joinGet(token, "Slackbot-LinkExpanding 1.0")
	// An unknown token looks exactly the same.
	r2, b2 := m.joinGet("nope-nope-nope-nope-nope-nope-nope-nope-nope", "Mikrotik/7.15 Fetch")
	if r1.StatusCode != 404 || r2.StatusCode != 404 || b1 != b2 {
		t.Fatalf("bad requests must look identical: %d %q vs %d %q", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	r3, script := m.joinGet(token, "Mikrotik/7.15 Fetch")
	if r3.StatusCode != 200 || !strings.HasPrefix(r3.Header.Get("Content-Type"), "text/plain") || r3.Header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(script, "wireguard") {
		t.Fatalf("script %d %s %.200s", r3.StatusCode, r3.Header.Get("Content-Type"), script)
	}
	// Single use.
	r4, b4 := m.joinGet(token, "Mikrotik/7.15 Fetch")
	if r4.StatusCode != 404 || b4 != b1 {
		t.Fatalf("second use: %d %q", r4.StatusCode, b4)
	}
	// The token never reaches the log; only a redacted path does.
	if strings.Contains(m.logs.String(), token) || !strings.Contains(m.logs.String(), "/j/[redacted]") {
		t.Fatalf("log leaks or lacks the redaction:\n%s", m.logs.String())
	}
}

// The "no certificate check" command asks for ?nocheck=1, and then the script's
// own page downloads skip the check too; the normal command keeps it.
func TestJoinEndpointNoCheckVariant(t *testing.T) {
	m := newMulti(t)
	token := func(name string) (string, string) {
		site, err := m.mgr.CreateSite(sites.SiteInput{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		page := body(t, m.admin("POST", "/admin/sites/join", url.Values{"id": {site.ID}}))
		i := strings.Index(page, "/j/")
		if i < 0 {
			t.Fatalf("no command on the page: %.300s", page)
		}
		rest := page[i+3:]
		return rest[:strings.IndexAny(rest, "\"' <&?")], page
	}
	tok, page := token("Delta")
	if !strings.Contains(page, "/j/"+tok+"?nocheck=1") {
		t.Fatalf("the fallback command lacks ?nocheck=1: %.600s", page)
	}
	r, script := m.joinGet(tok+"?nocheck=1", "Mikrotik/7.15 Fetch")
	if r.StatusCode != 200 || strings.Contains(script, "check-certificate=yes") || !strings.Contains(script, "check-certificate=no") {
		t.Fatalf("nocheck script %d: wants check-certificate=no on the page downloads", r.StatusCode)
	}
	tok2, _ := token("Epsilon")
	r, script = m.joinGet(tok2, "Mikrotik/7.15 Fetch")
	if r.StatusCode != 200 || strings.Contains(script, "check-certificate=no") || !strings.Contains(script, "check-certificate=yes") {
		t.Fatalf("default script %d: wants check-certificate=yes", r.StatusCode)
	}
}

func TestJoinEndpointIsRateLimited(t *testing.T) {
	m := newMulti(t)
	got429 := false
	for i := 0; i < 30; i++ {
		r, _ := m.joinGet("guess", "Mikrotik/7")
		if r.StatusCode == 429 {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("no rate limit on /j/")
	}
}

func TestRouterFilesArePerSite(t *testing.T) {
	m := newMulti(t)
	A := m.addSite(t, "Alpha")
	code, b := m.portal(t, "/routeros/hotspot/"+A.ID+"/login.html")
	if code != 200 || !strings.Contains(b, "/portal?site="+A.ID+"&mac=$(mac-esc)") || strings.Contains(b, "{{") {
		t.Fatalf("per-site login.html %d %s", code, b)
	}
	if code, _ = m.portal(t, "/routeros/hotspot/sdeadbe/login.html"); code != 404 {
		t.Fatalf("unknown site: %d", code)
	}
	// No legacy building here, so the un-prefixed paths are gone.
	if code, _ = m.portal(t, "/routeros/hotspot/login.html"); code != 404 {
		t.Fatalf("legacy path served without a legacy building: %d", code)
	}
}

func TestVoucherFromAnotherBuildingIsRefused(t *testing.T) {
	m := newMulti(t)
	A, B := m.addSite(t, "Alpha"), m.addSite(t, "Beta")
	macA := "AA:BB:CC:00:0A:01"
	m.svc.CreateAccount(billing.AccountInput{SiteID: A.ID, Flat: "Flat A1", MAC: macA})
	m.mock(A.ID).AddHost(macA, "10.5.50.9")
	time.Sleep(1100 * time.Millisecond)
	var pkg string
	m.st.View(func(d *store.Data) {
		for id := range d.Packages {
			pkg = id
		}
	})
	_, vs, err := m.svc.GenerateVouchers(pkg, B.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"site": {A.ID}, "mac": {macA}, "ip": {"10.5.50.9"}, "voucher": {vs[0].Code}}
	r, _ := m.cl.PostForm(m.ts.URL+"/portal/voucher", form)
	b := body(t, r)
	if !strings.Contains(b, "invalid or already used") {
		t.Fatalf("voucher from building B worked in A: %.300s", b)
	}
}

func TestBuildingPages(t *testing.T) {
	m := newMulti(t)
	if h := m.adminGet(t, "/admin/sites"); !strings.Contains(h, "No buildings yet") {
		t.Fatal("empty list")
	}
	if h := m.adminGet(t, "/admin/sites/new"); !strings.Contains(h, "Add building") {
		t.Fatal("new page")
	}
	loc := m.adminPost(t, "/admin/sites/create", url.Values{"name": {"Delta Court"}, "support": {"0711000000"}})
	if !strings.Contains(loc, "/admin/sites/view?id=") {
		t.Fatalf("create: %s", loc)
	}
	id := strings.TrimPrefix(strings.SplitN(loc, "&msg", 2)[0], "/admin/sites/view?id=")
	if h := m.adminGet(t, "/admin/sites/view?id="+id); !strings.Contains(h, "Delta Court") || !strings.Contains(h, "Make the join command") {
		t.Fatal("view page")
	}
	if h := m.adminGet(t, "/admin/sites/status.json?id="+id); !strings.Contains(h, `"token":"none"`) {
		t.Fatalf("status json: %s", h)
	}
	if l := m.adminPost(t, "/admin/sites/create", url.Values{"name": {"delta court"}}); !strings.Contains(l, "Error") {
		t.Fatalf("duplicate name accepted: %s", l)
	}
	m.adminPost(t, "/admin/sites/disable", url.Values{"id": {id}, "disable": {"1"}})
	if l := m.adminPost(t, "/admin/sites/remove", url.Values{"id": {id}, "confirm": {"nope"}}); !strings.Contains(l, "Error") {
		t.Fatalf("remove without the name: %s", l)
	}
	if l := m.adminPost(t, "/admin/sites/remove", url.Values{"id": {id}, "confirm": {"Delta Court"}}); strings.Contains(l, "Error") {
		t.Fatalf("remove: %s", l)
	}
	if h := m.adminGet(t, "/admin/sites"); strings.Contains(h, "Delta Court") {
		t.Fatal("removed building still listed")
	}
}

func TestHealthzCountsBuildings(t *testing.T) {
	m := newMulti(t)
	m.addSite(t, "Alpha")
	r, _ := m.cl.Get(m.ts.URL + "/healthz")
	b := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, `"sites_total":1`) || !strings.Contains(b, `"router_ok"`) {
		t.Fatalf("healthz %d %s", r.StatusCode, b)
	}
}
