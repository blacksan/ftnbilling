package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// uiRig is a two-router install with a customer on each, so every admin page
// has something to draw.
func uiRig(t *testing.T) (*multi, string, string) {
	t.Helper()
	m := newMulti(t)
	a, b := m.addSite(t, "Alpha Court"), m.addSite(t, "Beta Heights")
	acct, err := m.svc.CreateAccount(billing.AccountInput{SiteID: a.ID, Flat: "Flat A1", Tenant: "Wanjiru", Phone: "0712345678", MAC: "AA:BB:CC:00:00:01"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.CreateAccount(billing.AccountInput{SiteID: b.ID, Flat: "Flat B1", MAC: "AA:BB:CC:00:00:0B"}); err != nil {
		t.Fatal(err)
	}
	return m, a.ID, acct.Code
}

func TestEveryAdminPageRenders(t *testing.T) {
	m, site, code := uiRig(t)
	paths := []string{"/admin", "/admin/accounts", "/admin/accounts?tab=active", "/admin/accounts?tab=expired", "/admin/accounts?tab=new", "/admin/accounts?q=Flat",
		"/admin/accounts/view?code=" + code, "/admin/accounts/view?code=" + code + "&tab=payments", "/admin/accounts/view?code=" + code + "&tab=usage", "/admin/accounts/view?code=" + code + "&tab=events",
		"/admin/devices", "/admin/devices?online=1", "/admin/payments", "/admin/payments?method=mpesa", "/admin/payments/record", "/admin/unmatched", "/admin/orders",
		"/admin/packages", "/admin/packages?tab=quick", "/admin/packages?tab=monthly", "/admin/vouchers",
		"/admin/routers", "/admin/routers?tab=online", "/admin/routers?add=1", "/admin/live", "/admin/map", "/admin/audit",
		"/admin/customers", "/admin/equipment", "/admin/security", "/admin/tools", "/admin/uptime", "/admin/alerts", "/admin/messages",
		"/admin/settings", "/admin/settings/whatsapp", "/admin/settings/mpesa", "/admin/settings/email", "/admin/settings/equipment", "/admin/search?q=Flat"}
	for _, tab := range []string{"overview", "setup", "clients", "interfaces", "plans", "profile", "danger"} {
		paths = append(paths, "/admin/routers/view?id="+site+"&tab="+tab)
	}
	for _, p := range paths {
		h := m.adminGet(t, p)
		for _, want := range []string{
			`<meta name="color-scheme" content="light dark">`, // both colour schemes
			"theme.js", "app.css", "app.js", // the blocking theme script, the tokens, the behaviour
			"data-theme-btn", // the theme switch
			`<main`,          // landmark for the skip link and screen readers
		} {
			if !strings.Contains(h, want) {
				t.Errorf("%s lacks %q", p, want)
			}
		}
		if strings.Contains(h, "<script>") || strings.Contains(h, "onclick=") {
			t.Errorf("%s has inline script", p)
		}
	}
	// Usage and quality is per router: with All selected it sends you to the list.
	m.selectSite(t, site)
	if h := m.adminGet(t, "/admin/metrics"); !strings.Contains(h, "theme.js") || strings.Contains(h, "onclick=") {
		t.Error("metrics page")
	}
	// The print sheet is its own page.
	if _, vs, err := m.svc.GenerateVouchers(m.pkgID("1 Day"), "", 2); err != nil {
		t.Fatal(err)
	} else if h := m.adminGet(t, "/admin/vouchers/print?batch="+vs[0].Batch); !strings.Contains(h, vs[0].Code) || !strings.Contains(h, "data-print") {
		t.Error("print sheet")
	}
}

func TestAdminPagesSayRouterAndLinkToRouters(t *testing.T) {
	m, site, _ := uiRig(t)
	list := m.adminGet(t, "/admin/routers")
	if !strings.Contains(list, "/admin/routers/view?id="+site) || strings.Contains(list, "/admin/sites") {
		t.Error("the list should link to /admin/routers/view only")
	}
	for _, p := range []string{"/admin/routers", "/admin/accounts", "/admin/devices", "/admin/payments", "/admin/vouchers", "/admin/settings"} {
		h := m.adminGet(t, p)
		// The brand name of the test install is "Building WiFi"; take it out first.
		h = strings.ReplaceAll(h, "Building WiFi", "")
		if strings.Contains(h, "uilding") {
			i := strings.Index(h, "uilding")
			t.Errorf("%s shows the old word near %q", p, h[max(0, i-40):min(len(h), i+40)])
		}
	}
}

func TestAdminCSPHeader(t *testing.T) {
	m, _, _ := uiRig(t)
	r := m.admin("GET", "/admin/routers", nil)
	r.Body.Close()
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self';", "connect-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "http") {
		t.Errorf("csp too loose: %s", csp)
	}
}

func TestOldSitesURLsRedirect(t *testing.T) {
	m, site, _ := uiRig(t)
	for from, to := range map[string]string{
		"/admin/sites":                 "/admin/routers",
		"/admin/sites/view?id=" + site: "/admin/routers/view?id=" + site,
		"/admin/fleet":                 "/admin/routers",
	} {
		r := m.admin("GET", from, nil)
		r.Body.Close()
		if r.StatusCode != 302 || r.Header.Get("Location") != to {
			t.Errorf("%s: %d -> %q, want %q", from, r.StatusCode, r.Header.Get("Location"), to)
		}
	}
}

func (m *multi) getJSON(t *testing.T, path string, into any) {
	t.Helper()
	b := m.adminGet(t, path)
	if err := json.Unmarshal([]byte(b), into); err != nil {
		t.Fatalf("%s: %v\n%.300s", path, err, b)
	}
}

func TestSearchLiveAndDevicesJSON(t *testing.T) {
	m, site, _ := uiRig(t)
	var sr struct {
		Groups []struct {
			Label string `json:"label"`
			Items []struct {
				Title string `json:"title"`
				Href  string `json:"href"`
			} `json:"items"`
		} `json:"groups"`
	}
	m.getJSON(t, "/admin/search.json?q=Flat", &sr)
	found := false
	for _, g := range sr.Groups {
		for _, it := range g.Items {
			found = found || strings.Contains(it.Title, "Flat A1") && strings.HasPrefix(it.Href, "/admin/")
		}
	}
	if !found {
		t.Errorf("search.json did not find Flat A1: %+v", sr)
	}
	var short map[string]any
	m.getJSON(t, "/admin/search.json?q=F", &short)
	if g, _ := short["groups"].([]any); len(g) != 0 {
		t.Errorf("one letter should search nothing: %v", short)
	}
	var lv map[string]any
	m.getJSON(t, "/admin/live.json", &lv)
	for _, k := range []string{"generated_at", "rows", "online", "down_bps", "up_bps"} {
		if _, ok := lv[k]; !ok {
			t.Errorf("live.json lacks %q: %v", k, lv)
		}
	}
	var dv map[string]any
	m.getJSON(t, "/admin/routers/devices.json?router="+site, &dv)
	if _, ok := dv["items"]; !ok {
		t.Errorf("devices.json: %v", dv)
	}
}

func (m *multi) postJSON(t *testing.T, path string, f url.Values) (int, map[string]any) {
	t.Helper()
	f.Set("_csrf", m.br.CSRF)
	fillApartment(m.st, path, f)
	req, _ := newJSONReq(m.ts.URL+path, f)
	req.AddCookie(m.br.Cookie)
	if m.cookie != nil {
		req.AddCookie(m.cookie)
	}
	resp, err := m.cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(body(t, resp)), &out)
	return resp.StatusCode, out
}

func TestAddCustomerOneFormFlow(t *testing.T) {
	m, site, _ := uiRig(t)
	pkg := m.pkgID("Monthly 20 Mbps")
	code, j := m.postJSON(t, "/admin/accounts/create", url.Values{"site": {site}, "flat": {"Flat C3"}, "tenant": {"Otieno"}, "phone": {"0722000111"}, "package": {pkg}, "mac": {"aa-bb-cc-00-00-33"}})
	if code != 201 || j["ok"] != true || j["code"] == "" {
		t.Fatalf("create: %d %v", code, j)
	}
	var st map[string]any
	m.getJSON(t, "/admin/accounts/status.json?id="+j["id"].(string), &st)
	if st["state"] == nil || st["text"] == "" {
		t.Errorf("status: %v", st)
	}
	// The same MAC again is refused with a message, not a stack of redirects.
	code, j = m.postJSON(t, "/admin/accounts/create", url.Values{"site": {site}, "flat": {"Flat C4"}, "mac": {"AA:BB:CC:00:00:33"}})
	if code != 422 || j["ok"] != false || j["error"] == "" {
		t.Errorf("duplicate MAC: %d %v", code, j)
	}
	// Without JavaScript the same form lands on the customer's page.
	loc := m.adminPost(t, "/admin/accounts/create", url.Values{"site": {site}, "flat": {"Flat C5"}, "mac": {"AA:BB:CC:00:00:35"}})
	if !strings.Contains(loc, "/admin/accounts/view?code=") {
		t.Errorf("no-JS create: %s", loc)
	}
	// The dialog is on the page for people who may write.
	if h := m.adminGet(t, "/admin/accounts"); !strings.Contains(h, `id="addcustomer"`) || !strings.Contains(h, `name="mac"`) || !strings.Contains(h, "Activate now") {
		t.Error("Add customer dialog missing")
	}
}

func TestAddRouterModalFlow(t *testing.T) {
	m, _, _ := uiRig(t)
	code, j := m.postJSON(t, "/admin/routers/create", url.Values{"name": {"Gamma Plaza"}})
	if code != 200 || j["ok"] != true || j["id"] == "" {
		t.Fatalf("create: %d %v", code, j)
	}
	id := j["id"].(string)
	if cmd, _ := j["command"].(string); !strings.Contains(cmd, "/tool/fetch") {
		t.Errorf("no join line: %v", j)
	}
	if !strings.HasSuffix(j["url"].(string), "&tab=setup") {
		t.Errorf("url: %v", j["url"])
	}
	var js map[string]any
	m.getJSON(t, "/admin/routers/status.json?id="+id, &js)
	if js["token"] == nil {
		t.Errorf("status.json: %v", js)
	}
	// A router that already serves customers is created as it is: no forced mode.
	code, j2 := m.postJSON(t, "/admin/routers/create", url.Values{"name": {"Delta Court"}, "serving": {"1"}})
	if code != 200 || j2["ok"] != true {
		t.Fatalf("serving create: %d %v", code, j2)
	}
	if code, j := m.postJSON(t, "/admin/routers/create", url.Values{"name": {"gamma plaza"}}); code != 422 || j["ok"] != false {
		t.Errorf("duplicate name: %d %v", code, j)
	}
}

func TestRouterSetupTabChecklistEndpoints(t *testing.T) {
	m, site, _ := uiRig(t)
	h := m.adminGet(t, "/admin/routers/view?id="+site+"&tab=setup")
	for _, want := range []string{"data-setup=", "data-checks", "data-recheck", "data-setup-view", "data-reset-router"} {
		if !strings.Contains(h, want) {
			t.Errorf("setup tab lacks %q", want)
		}
	}
	// The removed endpoints have no button anywhere.
	for _, gone := range []string{"/routers/confirm", "/sites/confirm", "setup/run", "setup/migrate", "setup/rollback", "/cutover", "/adopt", "/routers/mode", "Roll back", "Start migration", "Run set-up"} {
		if strings.Contains(h, gone) {
			t.Errorf("the setup tab offers %q", gone)
		}
	}
	// The bootstrap line is the only router command, and the reset is typed.
	if strings.Contains(h, "data-copy=") {
		t.Error("a manual command can be copied from the setup tab")
	}
	var ck map[string]any
	m.getJSON(t, "/admin/routers/checks.json?id="+site, &ck)
	if _, ok := ck["items"]; !ok {
		if _, ok := ck["error"]; !ok {
			t.Errorf("checks.json: %v", ck)
		}
	}
	// An unknown router is a 404, and a fix for an unknown check is refused.
	if r := m.admin("GET", "/admin/routers/checks.json?id=nope", nil); r.StatusCode != 404 {
		t.Errorf("unknown router: %d", r.StatusCode)
	}
	if code, j := m.postJSON(t, "/admin/routers/fix", url.Values{"id": {site}, "check": {"no-such-check"}}); code < 400 || j["ok"] == true {
		t.Errorf("unknown fix: %d %v", code, j)
	}
}

func newJSONReq(u string, f url.Values) (*http.Request, error) {
	req, err := http.NewRequest("POST", u, strings.NewReader(f.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// The router page names the tunnel address and how to reach WinBox through the
// management WireGuard profile; it never shows key material.
func TestRouterPageShowsRemoteWinbox(t *testing.T) {
	m, site, _ := uiRig(t)
	m.st.Update(func(d *store.Data) error {
		x := d.Sites[site]
		x.TunnelIP = "10.99.0.7"
		d.Sites[site] = x
		return nil
	})
	h := m.adminGet(t, "/admin/routers/view?id="+site+"&tab=overview")
	for _, want := range []string{"Tunnel address", "Remote WinBox", "Built without remote WinBox: reset the router and paste a new line to add it.", "10.99.0.7"} {
		if !strings.Contains(h, want) {
			t.Errorf("router page lacks %q", want)
		}
	}
	for _, bad := range []string{"PrivateKey", "private-key", "private_key"} {
		if strings.Contains(h, bad) {
			t.Errorf("router page shows %q", bad)
		}
	}
}
