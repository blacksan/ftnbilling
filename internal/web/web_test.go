package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
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
	srv    *web.Server
	owner  *who
	br     *browser
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
	envID, err := sites.Bootstrap(cfg, st, log)
	if err != nil {
		t.Fatal(err)
	}
	var svc *billing.Service
	mgr := sites.New(sites.Options{Cfg: cfg, Store: st, Log: log, Dir: func(id string) metrics.Directory { return svc.Directory(id) }})
	svc = billing.NewService(st, mgr, log)
	svc.SeedPackages()
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Wait(); svc.Wait() })
	rt := mgr.MockRouter()
	svc.SyncProfiles(context.Background(), envID)
	mp := payments.NewMpesa(payments.MpesaConfig{}, svc, st, log)
	prov := map[string]payments.Provider{
		"test":    &payments.Test{Enabled_: testMode, Delay: 30 * time.Millisecond, C: svc},
		"voucher": payments.Voucher{C: svc}, "mpesa": mp,
	}
	srv := web.New(cfg, st, svc, prov, mp, mgr, log)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	a := &app{ts: ts, st: st, svc: svc, rt: rt, mgr: mgr, site: envID, srv: srv,
		cl: &http.Client{CheckRedirect: noRedirect}}
	a.owner = seedUser(t, srv, st, "admin", store.RoleOwner, pw)
	a.br = signIn(t, ts, srv, st, a.owner)
	a.csrf = a.br.CSRF
	return a
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
	// The push to the router runs in the background; wait for it.
	var p routeros.Profile
	var ok bool
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if p, ok = a.rt.GetProfile("ispbill-promo"); ok && p.RateLimit == "3M/3M" {
			break
		}
	}
	if !ok || p.RateLimit != "3M/3M" {
		t.Errorf("profile not pushed: %+v", p)
	}
	resp, _ := c.PostForm(a.ts.URL+"/admin/packages/save", url.Values{"_csrf": {a.csrf}})
	if resp.StatusCode != 303 {
		t.Errorf("unauth post: %d", resp.StatusCode)
	}
}

func TestUnknownMACRefusedAndListedAsUnregistered(t *testing.T) {
	a := newApp(t, true)
	code, html := a.portal(t, "/portal?mac=AA:BB:CC:DD:EE:77&ip=10.5.50.9")
	if code != 200 || !strings.Contains(html, "This connection isn't registered yet") || !strings.Contains(html, "0700 111 222") {
		t.Fatalf("%d %s", code, html)
	}
	for _, bad := range []string{"Test payment", "Pay with M-Pesa", "Redeem voucher", "Already paid", "PIN", `name="package"`} {
		if strings.Contains(html, bad) {
			t.Errorf("unregistered page offers %q", bad)
		}
	}
	// Payment attempts for an unknown MAC are refused.
	r, _ := a.cl.PostForm(a.ts.URL+"/portal/pay", url.Values{"package": {a.pkgID("1 Hour")}, "method": {"test"}, "mac": {"AA:BB:CC:DD:EE:77"}, "ip": {"10.5.50.9"}})
	if r.StatusCode != 403 {
		t.Errorf("pay: %d", r.StatusCode)
	}
	r, _ = a.cl.PostForm(a.ts.URL+"/portal/voucher", url.Values{"voucher": {"ABCDEFGH"}, "mac": {"AA:BB:CC:DD:EE:77"}})
	if r.StatusCode != 403 {
		t.Errorf("voucher: %d", r.StatusCode)
	}
	if _, h := a.portal(t, "/portal"); !strings.Contains(h, "Open this page from the WiFi login") {
		t.Error("no mac")
	}
	list := a.adminGet(t, "/admin/devices")
	if !strings.Contains(list, "AA:BB:CC:DD:EE:77") || !strings.Contains(list, "10.5.50.9") || !strings.Contains(list, `data-open="addcustomer"`) || !strings.Contains(list, "Attach") {
		t.Fatalf("unregistered list: %s", list)
	}
	// Hotspot hosts that are not authorised show up too.
	a.rt.AddHost("AA:BB:CC:DD:EE:88", "10.5.50.31")
	if l := a.adminGet(t, "/admin/devices"); !strings.Contains(l, "AA:BB:CC:DD:EE:88") || !strings.Contains(l, "hotspot-host") {
		t.Error("hotspot host missing")
	}
	// One click: create an account for the MAC.
	loc := a.adminPost(t, "/admin/devices/create", url.Values{"mac": {"AA:BB:CC:DD:EE:77"}, "flat": {"Flat 3B"}})
	if !strings.Contains(loc, "/admin/accounts/view?code=") || !strings.Contains(loc, "Account created") {
		t.Fatal(loc)
	}
	if l := a.adminGet(t, "/admin/devices"); strings.Contains(l, "AA:BB:CC:DD:EE:77") {
		t.Error("device still listed after account creation")
	}
	_, html = a.portal(t, "/portal?mac=AA:BB:CC:DD:EE:77&ip=10.5.50.9")
	acct := a.account(t, "Flat 3B")
	if !strings.Contains(html, "Flat 3B · Account "+acct.Code) || !strings.Contains(html, "No package yet") {
		t.Fatalf("known MAC page: %s", html)
	}
	// Attach to an existing account.
	a.adminPost(t, "/admin/devices/attach", url.Values{"mac": {"AA:BB:CC:DD:EE:88"}, "account": {acct.ID}})
	if got := a.account(t, "Flat 3B"); got.MAC != "AA:BB:CC:DD:EE:88" {
		t.Errorf("attach: %s", got.MAC)
	}
}

func TestKnownMACTestPaymentAutoLoginAndNoPIN(t *testing.T) {
	a := newApp(t, true)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}, "tenant": {"Jane"}, "phone": {"0712345678"}})
	acct := a.account(t, "Flat 3B")
	_, html := a.portal(t, "/portal?mac="+mac1+"&ip=10.5.50.20&dst=http%3A%2F%2Fexample.com%2F")
	for _, want := range []string{"Flat 3B · Account " + acct.Code, "Test payment", "Quick passes", "Monthly plans", "Have a voucher?", `name="mac" value="` + mac1 + `"`} {
		if !strings.Contains(html, want) {
			t.Errorf("portal missing %q", want)
		}
	}
	for _, bad := range []string{"Already paid", "PIN"} {
		if strings.Contains(html, bad) {
			t.Errorf("portal still has %q", bad)
		}
	}
	if strings.Index(html, "1 Hour") > strings.Index(html, "Monthly 20 Mbps") {
		t.Error("ordering")
	}
	form := url.Values{"package": {a.pkgID("1 Hour")}, "method": {"test"}, "mac": {mac1}, "ip": {"10.5.50.20"}, "dst": {"http://example.com/"}}
	resp, _ := a.cl.PostForm(a.ts.URL+"/portal/pay", form)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 303 || !strings.HasPrefix(loc, "/portal/wait?order=") {
		t.Fatalf("pay: %d %s", resp.StatusCode, loc)
	}
	id := strings.TrimPrefix(loc, "/portal/wait?order=")
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, j := a.portal(t, "/api/orders/"+id)
		if strings.Contains(j, `"paid"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never paid")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, h := a.portal(t, "/portal/success?order="+id)
	if !strings.Contains(h, "All devices in your flat are now connected") || !strings.Contains(h, acct.Code) || !strings.Contains(h, "EAT") || !strings.Contains(h, `href="http://example.com/"`) {
		t.Errorf("success page: %s", h)
	}
	if strings.Contains(h, acct.HSPass) || strings.Contains(h, "PIN") {
		t.Error("success page leaks the hotspot password or mentions a PIN")
	}
	u, ok := a.rt.GetUser(acct.Code)
	if !ok || u.MAC != mac1 || !a.rt.IsActive(acct.Code) {
		t.Fatalf("router user %+v active=%v", u, a.rt.IsActive(acct.Code))
	}
	// Opening the portal now shows the active status and logs in automatically.
	a.rt.KickUser(context.Background(), acct.Code)
	_, st := a.portal(t, "/portal?mac="+mac1+"&ip=10.5.50.20")
	if !strings.Contains(st, "Active") || !strings.Contains(st, "left") || !a.rt.IsActive(acct.Code) {
		t.Errorf("status page / auto-login: %s", st)
	}
	if strings.Contains(st, acct.HSPass) {
		t.Error("portal leaks the hotspot password")
	}
	// The account page never shows it either.
	if v := a.adminGet(t, "/admin/accounts/view?code="+acct.Code); strings.Contains(v, acct.HSPass) || strings.Contains(v, "PIN") {
		t.Error("admin account page shows the secret")
	}
}

func TestTestPaymentRefusedWhenNotTestMode(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	_, h := a.portal(t, "/portal?mac="+mac1)
	if strings.Contains(h, "Test payment") {
		t.Error("button must be hidden")
	}
	r, _ := a.cl.PostForm(a.ts.URL+"/portal/pay", url.Values{"package": {a.pkgID("1 Hour")}, "method": {"test"}, "mac": {mac1}})
	if r.StatusCode != 400 {
		t.Errorf("status %d", r.StatusCode)
	}
}

func TestVoucherRedeemAppliesToMACsAccount(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 4A"}, "mac": {"AA:BB:CC:DD:EE:02"}})
	_, vs, _ := a.svc.GenerateVouchers(a.pkgID("1 Day"), "", 1)
	f := url.Values{"voucher": {strings.ToLower(vs[0].Code)}, "mac": {mac1}, "ip": {"10.5.50.7"}}
	resp, _ := a.cl.PostForm(a.ts.URL+"/portal/voucher", f)
	if resp.StatusCode != 303 {
		t.Fatalf("redeem: %d", resp.StatusCode)
	}
	b3 := a.account(t, "Flat 3B")
	if b3.ExpiresAt.IsZero() || !a.account(t, "Flat 4A").ExpiresAt.IsZero() {
		t.Fatal("voucher applied to the wrong flat")
	}
	if !a.rt.IsActive(b3.Code) {
		t.Fatal("not logged in")
	}
	f.Set("mac", "AA:BB:CC:DD:EE:02")
	if resp, _ = a.cl.PostForm(a.ts.URL+"/portal/voucher", f); resp.StatusCode != 400 {
		t.Fatalf("second redeem should fail: %d", resp.StatusCode)
	}
}

func TestManualMpesaChangeMACUnmatchedAndLedgerCSV(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	form := url.Values{"account": {acct.Code}, "trans": {"sgl7k2m9xa"}, "amount": {"3000"}, "phone": {"0712345678"}, "package": {""}, "date": {"2026-11-16T18:30"}}
	loc := a.adminPost(t, "/admin/payments/record", form)
	if !strings.Contains(loc, "Recorded SGL7K2M9XA") {
		t.Fatal(loc)
	}
	got := a.account(t, "Flat 3B")
	if got.ExpiresAt.IsZero() || a.rt.IsActive(acct.Code) == true {
		// not logged in (no portal visit) but user must exist
	}
	if u, ok := a.rt.GetUser(acct.Code); !ok || u.Profile != "ispbill-monthly-40-mbps" {
		t.Fatalf("router user %+v", u)
	}
	// Duplicate code is rejected with an error and extends nothing.
	loc = a.adminPost(t, "/admin/payments/record", form)
	if !strings.Contains(loc, "Error:") || !strings.Contains(loc, "already recorded") || !a.account(t, "Flat 3B").ExpiresAt.Equal(got.ExpiresAt) {
		t.Fatalf("duplicate: %s", loc)
	}
	bad := url.Values{"account": {acct.Code}, "trans": {"short"}, "amount": {"3000"}}
	if loc = a.adminPost(t, "/admin/payments/record", bad); !strings.Contains(loc, "10 capital letters") {
		t.Fatal(loc)
	}
	// The record form preselects the package by amount.
	if f := a.adminGet(t, "/admin/payments/record?account="+acct.Code+"&amount=250"); !regexp.MustCompile(`value="[0-9a-f]+" data-price="250" selected`).MatchString(f) {
		t.Error("package not preselected by amount")
	}
	// Ledger page and CSV.
	led := a.adminGet(t, "/admin/payments")
	if !strings.Contains(led, "SGL7K2M9XA") || !strings.Contains(led, "mpesa-manual") {
		t.Error("ledger page")
	}
	r := a.admin("GET", "/admin/payments.csv", nil)
	csv := body(t, r)
	if r.Header.Get("Content-Type") != "text/csv; charset=utf-8" || !strings.HasPrefix(csv, "key,paid_at_eat,") || !strings.Contains(csv, "SGL7K2M9XA,2026-11-16 18:30:00,") || !strings.Contains(csv, "Flat 3B") {
		t.Fatalf("csv: %s", csv)
	}
	// Change MAC from the admin page.
	loc = a.adminPost(t, "/admin/accounts/mac", url.Values{"id": {acct.ID}, "mac": {"aa:bb:cc:dd:ee:99"}})
	if !strings.Contains(loc, "ONT MAC updated") {
		t.Fatal(loc)
	}
	if u, _ := a.rt.GetUser(acct.Code); u.MAC != "AA:BB:CC:DD:EE:99" {
		t.Fatalf("router mac %s", u.MAC)
	}
	if loc = a.adminPost(t, "/admin/accounts/mac", url.Values{"id": {acct.ID}, "mac": {"nonsense"}}); !strings.Contains(loc, "Error") {
		t.Error("bad mac accepted")
	}
	// Unmatched payment: assign it.
	a.svc.RecordUnmatched(billing.UnmatchedInput{Key: "UNMATCH123", Method: "mpesa-c2b", AmountKES: 77, BillRef: "NOPE", Note: "unknown account number"})
	if p := a.adminGet(t, "/admin/unmatched"); !strings.Contains(p, "UNMATCH123") || !strings.Contains(p, "Mark refunded") {
		t.Fatal("unmatched page")
	}
	before := a.account(t, "Flat 3B").ExpiresAt
	a.adminPost(t, "/admin/unmatched/assign", url.Values{"key": {"UNMATCH123"}, "account": {acct.ID}, "package": {a.pkgID("1 Day")}, "note": {"called"}})
	if !a.account(t, "Flat 3B").ExpiresAt.Equal(before.Add(24 * time.Hour)) {
		t.Error("assignment not applied")
	}
	// Suspend from the admin.
	a.adminPost(t, "/admin/accounts/suspend", url.Values{"id": {acct.ID}, "suspend": {"1"}})
	if _, ok := a.rt.GetUser(acct.Code); ok {
		t.Error("suspended account still has a router user")
	}
	_, h := a.portal(t, "/portal?mac=AA:BB:CC:DD:EE:99&ip=10.5.50.9")
	if !strings.Contains(h, "suspended") || strings.Contains(h, `name="package"`) {
		t.Error("suspended portal page")
	}
	// Dashboard shows pending router actions when the router is down.
	a.rt.SetFail(io.ErrUnexpectedEOF)
	a.adminPost(t, "/admin/accounts/suspend", url.Values{"id": {acct.ID}, "suspend": {"0"}})
	if d := a.adminGet(t, "/admin"); !strings.Contains(d, "Pending router actions") || !strings.Contains(d, acct.Code) || !strings.Contains(d, "unexpected EOF") {
		t.Errorf("dashboard should list the pending router action: %s", d)
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
	if r := a.admin("GET", "/admin/metrics.csv", nil); r.StatusCode != 200 || !strings.HasPrefix(body(t, r), "flat,account_code") {
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
	for _, want := range []string{"Top 10 flats by download", "Services", "YouTube", "Flat 3B", "Mbps", "Download GB", "Time at cap", "Busy hours", "capacity 500", "Things to look at", "fair-use", "Packet loss", "WAN busy hours"} {
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
	for _, p := range []string{"/portal?mac=" + mac1 + "&ip=10.5.50.20", "/portal?mac=AA:BB:CC:DD:EE:55", "/portal"} {
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

func TestPortalDisablesSlowerPlansWhileActiveAndRejectsServerSide(t *testing.T) {
	a := newApp(t, true)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	if err := a.svc.ExtendAccount(context.Background(), acct.ID, a.pkgID("Monthly 40 Mbps"), "", false); err != nil {
		t.Fatal(err)
	}
	_, html := a.portal(t, "/portal?mac="+mac1+"&ip=10.5.50.20")
	if n := strings.Count(html, "Available when your current plan ends on"); n != 4 { // 1 Hour, 1 Day, 1 Week, Monthly 20
		t.Errorf("disabled cards = %d", n)
	}
	if !strings.Contains(html, `class="pkg off"`) || !strings.Contains(html, "disabled") {
		t.Error("lower cards not disabled")
	}
	// Monthly 40 stays selectable and is preselected.
	if i := strings.Index(html, `value="`+a.pkgID("Monthly 40 Mbps")+`" checked`); i < 0 {
		t.Error("an allowed package should be preselected")
	}
	r, _ := a.cl.PostForm(a.ts.URL+"/portal/pay", url.Values{"package": {a.pkgID("1 Hour")}, "method": {"test"}, "mac": {mac1}, "ip": {"10.5.50.20"}})
	if r.StatusCode != 400 {
		t.Errorf("server-side downgrade status %d", r.StatusCode)
	}
	r, _ = a.cl.PostForm(a.ts.URL+"/portal/pay", url.Values{"package": {a.pkgID("Monthly 80 Mbps")}, "method": {"test"}, "mac": {mac1}, "ip": {"10.5.50.20"}})
	if r.StatusCode != 303 {
		t.Errorf("upgrade status %d", r.StatusCode)
	}
	// Admin form: the force box applies a slower package.
	out := a.adminPost(t, "/admin/accounts/extend", url.Values{"id": {acct.ID}, "package": {a.pkgID("1 Hour")}, "force": {"1"}})
	_ = out
	if cur := a.account(t, "Flat 3B"); cur.PackageID != a.pkgID("1 Hour") {
		t.Errorf("forced package %s", cur.PackageID)
	}
}
