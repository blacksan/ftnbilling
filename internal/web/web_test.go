package web_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ispbill/internal/acs"
	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/payments"
	"ispbill/internal/routeros"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
	"ispbill/internal/web"
)

const (
	pw   = "pw-123456"
	mac1 = "AA:BB:CC:DD:EE:01"
)

type app struct {
	ts     *httptest.Server
	st     *store.Store
	svc    *billing.Service
	rt     *routeros.Mock
	mgr    *sites.Manager
	site   string
	cookie *http.Cookie // extra admin cookie (building selection)
	csrf   string
	cl     *http.Client
	tp     *payments.Test // the test-payment provider (Wait for its delayed completions)
	srv    *web.Server
	owner  *who
	br     *browser
	mp     *payments.Mpesa
	acs    *acs.Service // nil unless the test sets ACSListen
}

func newApp(t *testing.T, testMode bool) *app { return newAppCfg(t, testMode, nil) }

func newAppCfg(t *testing.T, testMode bool, mut func(*config.Config)) *app {
	t.Helper()
	cfg := config.Config{BrandName: "Building WiFi", TestMode: testMode, AdminUser: "admin", AdminPass: pw, RouterMock: true, DataKey: testKey,
		SupportPhone: "0700 111 222", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", WANCapacityMbps: 500, OLTCapacityMbps: 10000, FUPTB: 15,
		NetflowAddr: "off", CustomerSubnet: "10.5.50.0/24", PingTargets: []string{"8.8.8.8", "1.1.1.1"}}
	if mut != nil {
		mut(&cfg)
	}
	dir := t.TempDir()
	cfg.DataDir = dir
	cfg.FlowEvery, cfg.PortEvery, cfg.PingEvery, cfg.DNSEvery, cfg.SaveEvery = time.Hour, time.Hour, time.Hour, time.Hour, time.Hour
	st, _ := storetest.Open(t, dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	envID := seedSite(t, st, "Block A")
	var svc *billing.Service
	mgr := sites.New(sites.Options{Cfg: cfg, Store: st, Log: log, DataKey: cfg.DataKey, Dir: func(id string) metrics.Directory { return svc.Directory(id) }})
	svc = billing.NewService(st, mgr, log)
	svc.SeedPackages()
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Wait(); svc.Wait() })
	rt := mgr.MockRouter()
	mp := payments.NewMpesa(payments.MpesaConfig{}, svc, st, log)
	tp := &payments.Test{Enabled_: testMode, Delay: 30 * time.Millisecond, C: svc}
	t.Cleanup(tp.Wait) // runs before the store is closed
	prov := map[string]payments.Provider{
		"test":    tp,
		"voucher": payments.Voucher{C: svc}, "mpesa": mp,
	}
	srv := web.New(cfg, st, svc, prov, mp, mgr, log)
	var acsSvc *acs.Service
	if cfg.ACSEnabled() {
		var err error
		if acsSvc, err = acs.New(acs.Options{Store: st, Cfg: cfg, Log: log, DataKey: cfg.DataKey}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(acsSvc.Wait)
		srv.SetACS(acsSvc)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	a := &app{ts: ts, st: st, svc: svc, rt: rt, mgr: mgr, site: envID, srv: srv, mp: mp,
		cl: &http.Client{CheckRedirect: noRedirect}, tp: tp, acs: acsSvc}
	a.owner = seedUser(t, srv, st, "admin", store.RoleOwner, pw)
	a.br = signIn(t, ts, srv, st, a.owner)
	a.csrf = a.br.CSRF
	return a
}

// seedSite stores a joined IPoE building (the mock router serves it) and gives
// the shared mock the ftn-dhcp server the bootstrap script builds.
func seedSite(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	id := store.NewSiteID()
	err := st.Update(func(d *store.Data) error {
		d.Sites[id] = store.Site{ID: id, Name: name, Mode: store.SiteModeWG, WGPublicKey: "PUB" + id, TunnelIP: fmt.Sprintf("10.99.0.%d", len(d.Sites)+2),
			AccessMode: store.AccessIPoE, CreatedAt: time.Now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	apartmentFor(t, st, id, name)
	return id
}

// apartmentFor gives a router an apartment named after it and links the two,
// the shape the migration leaves existing data in.
func apartmentFor(t *testing.T, st *store.Store, siteID, name string) string {
	t.Helper()
	id := store.NewApartmentID()
	err := st.Update(func(d *store.Data) error {
		d.Apartments[id] = store.Apartment{ID: id, Name: name, CreatedAt: time.Now()}
		d.ApartmentSites[store.ApartmentSiteKey(id, siteID)] = store.ApartmentSite{ApartmentID: id, SiteID: siteID, CreatedAt: time.Now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// aptOf is the apartment linked to a router (the one apartmentFor made).
func aptOf(t *testing.T, st *store.Store, siteID string) string {
	t.Helper()
	var id string
	st.View(func(d *store.Data) {
		if ids := d.SiteApartmentIDs(siteID); len(ids) > 0 {
			id = ids[0]
		}
	})
	if id == "" {
		t.Fatalf("router %s has no apartment", siteID)
	}
	return id
}

// fillApartment adds the apartment of the form's router (or of the only
// apartment there is) to an add-customer post, as the form's select would.
func fillApartment(st *store.Store, path string, f url.Values) {
	if f == nil || (path != "/admin/accounts/create" && path != "/admin/devices/create") || f.Get("apartment_id") != "" {
		return
	}
	if f.Get("flat") == "x" {
		return
	}
	st.View(func(d *store.Data) {
		if site := f.Get("site"); site != "" {
			if ids := d.SiteApartmentIDs(site); len(ids) > 0 {
				f.Set("apartment_id", ids[0])
			}
			return
		}
		if len(d.Apartments) == 1 {
			for id := range d.Apartments {
				f.Set("apartment_id", id)
			}
		}
	})
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// admin performs a signed-in request; POST forms get the CSRF token added.
func (a *app) admin(method, path string, form url.Values) *http.Response {
	var rd io.Reader
	if method == "POST" {
		if form == nil {
			form = url.Values{}
		}
		form.Set("_csrf", a.br.CSRF)
		fillApartment(a.st, path, form)
		rd = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, a.ts.URL+path, rd)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(a.br.Cookie)
	if a.cookie != nil {
		req.AddCookie(a.cookie)
	}
	resp, err := a.cl.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

func (a *app) adminGet(t *testing.T, path string) string {
	t.Helper()
	r := a.admin("GET", path, nil)
	if r.StatusCode != 200 {
		t.Fatalf("GET %s: %d", path, r.StatusCode)
	}
	return body(t, r)
}

func (a *app) adminPost(t *testing.T, path string, f url.Values) string {
	t.Helper()
	r := a.admin("POST", path, f)
	if r.StatusCode != 303 {
		t.Fatalf("POST %s: %d %s", path, r.StatusCode, body(t, r))
	}
	loc, _ := url.QueryUnescape(r.Header.Get("Location"))
	r.Body.Close()
	return loc
}

func (a *app) portal(t *testing.T, path string) (int, string) {
	t.Helper()
	r, err := a.cl.Get(a.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, body(t, r)
}

func (a *app) pkgID(name string) (id string) {
	a.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if p.Name == name {
				id = p.ID
			}
		}
	})
	return
}

func (a *app) account(t *testing.T, flat string) store.Account {
	t.Helper()
	var out store.Account
	a.st.View(func(d *store.Data) {
		for _, x := range d.Accounts {
			if x.Flat == flat {
				out = x
			}
		}
	})
	if out.ID == "" {
		t.Fatalf("no account %s", flat)
	}
	return out
}

func TestAdminAuthAndCSRF(t *testing.T) {
	a := newApp(t, true)
	c := a.cl
	// Signed out, every admin page sends the browser to the login page.
	for _, p := range []string{"/admin", "/admin/accounts", "/admin/devices", "/admin/payments", "/admin/payments.csv", "/admin/payments/record",
		"/admin/unmatched", "/admin/metrics", "/admin/metrics.csv", "/admin/metrics/flat?code=X", "/admin/settings", "/admin/security"} {
		resp, _ := c.Get(a.ts.URL + p)
		resp.Body.Close()
		if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login") || resp.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("%s without a session: %d %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Basic auth no longer opens anything.
	req, _ := http.NewRequest("GET", a.ts.URL+"/admin", nil)
	req.SetBasicAuth("admin", pw)
	if resp, _ := c.Do(req); resp.StatusCode != 303 {
		t.Fatalf("basic auth: %d", resp.StatusCode)
	}
	html := a.adminGet(t, "/admin/packages")
	if m := regexp.MustCompile(`name="_csrf" value="([0-9a-f]+)"`).FindStringSubmatch(html); m == nil || m[1] != a.csrf {
		t.Fatalf("token not embedded: %v", m)
	}
	post := func(token string, withSession bool) int {
		form := url.Values{"name": {"Promo"}, "duration": {"30"}, "speed": {"3"}, "price": {"5"}, "_csrf": {token}}
		req, _ := http.NewRequest("POST", a.ts.URL+"/admin/packages/save", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withSession {
			req.AddCookie(a.br.Cookie)
		}
		r, _ := c.Do(req)
		r.Body.Close()
		return r.StatusCode
	}
	if post("", true) != 403 || post("deadbeef", true) != 403 {
		t.Error("CSRF must be enforced")
	}
	if code := post(a.csrf, false); code != 303 {
		t.Errorf("a valid token without a session must go to the login page, got %d", code)
	}
	// Every state-changing admin POST is covered, including the new ones.
	for _, p := range []string{"/admin/accounts/create", "/admin/payments/record", "/admin/devices/create", "/admin/unmatched/assign", "/admin/accounts/mac", "/admin/metrics/demo"} {
		req, _ := http.NewRequest("POST", a.ts.URL+p, strings.NewReader("flat=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(a.br.Cookie)
		if r, _ := c.Do(req); r.StatusCode != 403 {
			t.Errorf("POST %s without token: %d", p, r.StatusCode)
		}
	}
	if post(a.csrf, true) != 303 {
		t.Error("valid post")
	}
	resp, _ := c.PostForm(a.ts.URL+"/admin/packages/save", url.Values{"_csrf": {a.csrf}})
	if resp.StatusCode != 303 {
		t.Errorf("unauth post: %d", resp.StatusCode)
	}
}

func TestUnregisteredDeviceCanBecomeOrAttachToAnAccount(t *testing.T) {
	a := newApp(t, true)
	a.svc.NoteDevice(a.site, "AA:BB:CC:DD:EE:77", "10.5.50.9", "portal")
	list := a.adminGet(t, "/admin/devices")
	if !strings.Contains(list, "AA:BB:CC:DD:EE:77") || !strings.Contains(list, "10.5.50.9") || !strings.Contains(list, `data-open="addcustomer"`) || !strings.Contains(list, "Attach") {
		t.Fatalf("unregistered list: %s", list)
	}
	// One click: create an account for the MAC.
	loc := a.adminPost(t, "/admin/devices/create", url.Values{"mac": {"AA:BB:CC:DD:EE:77"}, "flat": {"Flat 3B"}})
	if !strings.Contains(loc, "/admin/accounts/view?code=") || !strings.Contains(loc, "Account created") {
		t.Fatal(loc)
	}
	if l := a.adminGet(t, "/admin/devices"); strings.Contains(l, "AA:BB:CC:DD:EE:77") {
		t.Error("device still listed after account creation")
	}
	acct := a.account(t, "Flat 3B")
	if acct.MAC != "AA:BB:CC:DD:EE:77" {
		t.Fatalf("mac %s", acct.MAC)
	}
	// Attach to an existing account.
	a.svc.NoteDevice(a.site, "AA:BB:CC:DD:EE:88", "10.5.50.31", "portal")
	a.adminPost(t, "/admin/devices/attach", url.Values{"mac": {"AA:BB:CC:DD:EE:88"}, "account": {acct.ID}})
	if got := a.account(t, "Flat 3B"); got.MAC != "AA:BB:CC:DD:EE:88" {
		t.Errorf("attach: %s", got.MAC)
	}
}

func TestMetricsPageRendersEmptyWithRouterDown(t *testing.T) {
	a := newApp(t, false)
	a.rt.SetFail(io.ErrUnexpectedEOF)
	for _, p := range []string{"today", "7d", "30d", "month", "bogus"} {
		h := a.adminGet(t, "/admin/metrics?period="+p)
		for _, want := range []string{"no data yet", "Things to look at", "<svg", "Waiting for data", "Busy hours", "WAN this month vs fair-use limit"} {
			if !strings.Contains(h, want) {
				t.Errorf("%s: empty page lacks %q", p, want)
			}
		}
	}
	if r := a.admin("GET", "/admin/metrics.csv", nil); r.StatusCode != 200 || !strings.HasPrefix(body(t, r), "house_number,account_code") {
		t.Error("csv header")
	}
	if r := a.admin("GET", "/admin/metrics/flat?code=NOPE", nil); r.StatusCode != 404 {
		t.Errorf("unknown flat: %d", r.StatusCode)
	}
}

func TestMetricsPageWithDemoDataDrillDownSortAndCSV(t *testing.T) {
	a := newApp(t, false)
	for i, f := range []string{"Flat 3B", "Flat 4A", "Flat 10C"} {
		a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {f}, "mac": {"AA:BB:CC:DD:EE:0" + string(rune('1'+i))}})
		acct := a.account(t, f)
		a.svc.ExtendAccount(context.Background(), acct.ID, a.pkgID("Monthly 20 Mbps"), "", false)
	}
	a.adminPost(t, "/admin/metrics/demo", url.Values{"period": {"7d"}})
	h := a.adminGet(t, "/admin/metrics?period=7d")
	for _, want := range []string{"Top 10 houses by download", "Services", "YouTube", "Flat 3B", "Mbps", "Download GB", "Time at cap", "Busy hours", "capacity 500", "Things to look at", "fair-use", "Packet loss", "WAN busy hours"} {
		if !strings.Contains(h, want) {
			t.Errorf("demo page lacks %q", want)
		}
	}
	if strings.Contains(h, "NaN") {
		t.Error("NaN in page")
	}
	if n := strings.Count(h, "<svg"); n < 7 {
		t.Errorf("only %d charts", n)
	}
	if !strings.Contains(a.adminGet(t, "/admin/metrics?period=today&sort=peak"), "sort=down") {
		t.Error("sort links")
	}
	acct := a.account(t, "Flat 3B")
	d := a.adminGet(t, "/admin/metrics/flat?code="+acct.Code+"&period=30d")
	for _, want := range []string{"Flat 3B", "Usage over time", "Services", "package 20", "Peak speed"} {
		if !strings.Contains(d, want) {
			t.Errorf("drill-down lacks %q", want)
		}
	}
	r := a.admin("GET", "/admin/metrics.csv?period=month&sort=flat", nil)
	csv := body(t, r)
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "Flat 3B,") || !strings.Contains(csv, "YouTube") {
		t.Fatalf("csv: %s", csv)
	}
}

func TestNoTaxWordingAnywhere(t *testing.T) {
	a := newApp(t, true)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	a.adminPost(t, "/admin/payments/record", url.Values{"account": {acct.Code}, "trans": {"WORDCHK123"}, "amount": {"250"}})
	a.svc.RecordUnmatched(billing.UnmatchedInput{Key: "UNMATCH999", Method: "mpesa-c2b", AmountKES: 77, Note: "odd"})
	a.svc.GenerateVouchers(a.pkgID("1 Day"), "", 2)
	a.adminPost(t, "/admin/metrics/demo", url.Values{})
	banned := regexp.MustCompile(`(?i)\bvat\b|etims|excise|tax|tariff levy|inclusive of|exclusive of`)
	pages := []string{"/admin", "/admin/accounts", "/admin/accounts/view?code=" + acct.Code, "/admin/devices", "/admin/payments", "/admin/payments.csv",
		"/admin/payments/record", "/admin/unmatched", "/admin/orders", "/admin/packages", "/admin/vouchers", "/admin/settings",
		"/admin/metrics", "/admin/metrics/flat?code=" + acct.Code, "/admin/metrics.csv"}
	for _, p := range pages {
		if m := banned.FindString(a.adminGet(t, p)); m != "" {
			t.Errorf("%s contains %q", p, m)
		}
	}
	for _, p := range []string{"/pay", "/portal/wait?order=x"} {
		if _, h := a.portal(t, p); banned.MatchString(h) {
			t.Errorf("%s: %q", p, banned.FindString(h))
		}
	}
	var vc store.Voucher
	a.st.View(func(d *store.Data) {
		for _, v := range d.Vouchers {
			vc = v
		}
	})
	if h := a.adminGet(t, "/admin/vouchers/print?batch="+vc.Batch); banned.MatchString(h) {
		t.Error("voucher print page")
	}
	// Package prices are plain KES amounts.
	if h := a.adminGet(t, "/admin/packages"); !strings.Contains(h, `value="3000"`) {
		t.Error("plain prices")
	}
}

func TestHealthz(t *testing.T) {
	a := newApp(t, true)
	_, b := a.portal(t, "/healthz")
	if !strings.Contains(b, `"router_ok":true`) || !strings.Contains(b, `"test_mode":true`) || !strings.Contains(b, `"mpesa_enabled":false`) {
		t.Errorf("healthz %s", b)
	}
}
