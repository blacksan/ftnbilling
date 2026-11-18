package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/payments"
	"ispbill/internal/routeros"
	"ispbill/internal/settings"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
	"ispbill/internal/web"
)

// setRig is an app with the settings manager installed and a legacy building
// with a fake router (so the tools have something to talk to).
type setRig struct {
	ts   *httptest.Server
	st   *store.Store
	m    *settings.Manager
	mp   *payments.Mpesa
	rt   *routeros.Mock
	site store.Site
	csrf string
	cl   *http.Client
}

func newSetRig(t *testing.T) *setRig {
	t.Helper()
	cfg := config.Config{BrandName: "Building WiFi", AdminUser: "admin", AdminPass: pw, RouterMock: true,
		SupportPhone: "0700 111 222", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", WANCapacityMbps: 500, OLTCapacityMbps: 10000, FUPTB: 15,
		NetflowAddr: "off", CustomerSubnet: "10.5.50.0/24", PingTargets: []string{"8.8.8.8"}, PublicBaseURL: "https://billing.example.co.ke",
		DataKey: []byte("0123456789abcdef0123456789abcdef"), MpesaEnv: "sandbox", MpesaUnknownRef: "reject", WALang: "en", WAAPIVersion: "v21.0"}
	cfg.DataDir = t.TempDir()
	cfg.FlowEvery, cfg.PortEvery, cfg.PingEvery, cfg.DNSEvery, cfg.SaveEvery = time.Hour, time.Hour, time.Hour, time.Hour, time.Hour
	st, _ := storetest.Open(t, cfg.DataDir)
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
	m, err := settings.New(st, cfg.DataKey, cfg, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	mp := payments.NewMpesa(payments.MpesaConfig{}, svc, st, log)
	srv := web.New(cfg, st, svc, map[string]payments.Provider{"voucher": payments.Voucher{C: svc}, "mpesa": mp}, mp, mgr, log)
	srv.SetSettings(m)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	r := &setRig{ts: ts, st: st, m: m, mp: mp, rt: mgr.MockRouter(),
		cl: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	st.View(func(d *store.Data) { r.site = d.Sites[envID] })
	r.do("GET", "/admin", nil, pw).Body.Close()
	return r
}

func (r *setRig) do(method, path string, form url.Values, pass string) *http.Response {
	var rd io.Reader
	if method == "POST" {
		if form == nil {
			form = url.Values{}
		}
		form.Set("_csrf", r.csrf)
		rd = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, r.ts.URL+path, rd)
	req.SetBasicAuth("admin", pass)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if r.csrf != "" {
		req.AddCookie(&http.Cookie{Name: "ispbill_csrf", Value: r.csrf})
	}
	resp, err := r.cl.Do(req)
	if err != nil {
		panic(err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "ispbill_csrf" {
			r.csrf = c.Value
		}
	}
	return resp
}

func (r *setRig) get(t *testing.T, path string) string {
	t.Helper()
	resp := r.do("GET", path, nil, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

// post returns the status and, for a redirect, the decoded flash message.
func (r *setRig) post(path string, f url.Values) (int, string, string) {
	resp := r.do("POST", path, f, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	msg := ""
	if u, err := url.Parse(loc); err == nil {
		msg = u.Query().Get("msg")
	}
	return resp.StatusCode, msg, string(b)
}

const longPW = "a-long-new-password-42"

func TestSettingsPasswordFlow(t *testing.T) {
	r := newSetRig(t)
	// Wrong current password: nothing changes.
	code, msg, _ := r.post("/admin/settings/password", url.Values{"current_password": {"nope"}, "new_password": {longPW}, "confirm_password": {longPW}})
	if code != 303 || !strings.HasPrefix(msg, "Error") || r.m.HasAdminPassword() {
		t.Fatalf("wrong current password: %d %q hash=%v", code, msg, r.m.HasAdminPassword())
	}
	// Too short.
	_, msg, _ = r.post("/admin/settings/password", url.Values{"current_password": {pw}, "new_password": {"short"}, "confirm_password": {"short"}})
	if !strings.HasPrefix(msg, "Error") || r.m.HasAdminPassword() {
		t.Fatalf("short password accepted: %q", msg)
	}
	code, _, _ = r.post("/admin/settings/password", url.Values{"current_password": {pw}, "new_password": {longPW}, "confirm_password": {longPW}})
	if code != 303 || !r.m.HasAdminPassword() {
		t.Fatalf("password change: %d", code)
	}
	// The environment password no longer works; the new one does.
	if c := r.do("GET", "/admin", nil, pw).StatusCode; c != 401 {
		t.Fatalf("old env password still works: %d", c)
	}
	if c := r.do("GET", "/admin", nil, longPW).StatusCode; c != 200 {
		t.Fatalf("new password refused: %d", c)
	}
	// The password never reaches the audit log.
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if strings.Contains(e.Msg, longPW) || strings.Contains(e.Msg, "pbkdf2") {
				t.Fatalf("secret in event: %s", e.Msg)
			}
		}
	})
}

func TestSettingsLoginRateLimit(t *testing.T) {
	r := newSetRig(t)
	var last int
	for i := 0; i < 8; i++ {
		last = r.do("GET", "/admin", nil, "wrong-password").StatusCode
	}
	if last != 429 {
		t.Fatalf("after repeated failures want 429, got %d", last)
	}
	// Even the right password waits while blocked.
	resp := r.do("GET", "/admin", nil, pw)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("blocked client: %d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestSettingsSecretsNeverRendered(t *testing.T) {
	r := newSetRig(t)
	const tok = "EAAGsecrettoken1234567890WXYZ"
	form := url.Values{"wa.token": {tok}, "wa.phone_number_id": {"1234567890"}}
	// A secret needs the current password.
	_, msg, _ := r.post("/admin/settings/whatsapp", form)
	if !strings.HasPrefix(msg, "Error") {
		t.Fatalf("secret saved without the password: %q", msg)
	}
	form.Set("current_password", pw)
	_, msg, _ = r.post("/admin/settings/whatsapp", form)
	if !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("save: %q", msg)
	}
	if !r.m.Effective().WhatsAppConfigured() || r.m.Effective().WAToken != tok {
		t.Fatalf("token not in force: %+v", r.m.Effective().WAPhoneNumberID)
	}
	page := r.get(t, "/admin/settings/whatsapp")
	if strings.Contains(page, tok) || strings.Contains(page, "secrettoken") {
		t.Fatal("the token is in the page")
	}
	if !strings.Contains(page, "WXYZ") {
		t.Fatal("the masked token (last four) is missing")
	}
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if strings.Contains(e.Msg, tok) {
				t.Fatalf("token in the audit log: %s", e.Msg)
			}
		}
	})
	// Reverting a secret needs the password too, and works with it.
	_, msg, _ = r.post("/admin/settings/revert", url.Values{"key": {"wa.token"}})
	if !strings.HasPrefix(msg, "Error") {
		t.Fatalf("revert without password: %q", msg)
	}
	r.post("/admin/settings/revert", url.Values{"key": {"wa.token"}, "current_password": {pw}})
	if r.m.Effective().WAToken != "" {
		t.Fatal("token still set after revert")
	}
}

func TestSettingsPagesRender(t *testing.T) {
	r := newSetRig(t)
	for _, p := range []string{"/admin/settings", "/admin/settings/whatsapp", "/admin/settings/mpesa", "/admin/settings/email", "/admin/settings/equipment", "/admin/tools"} {
		b := r.get(t, p)
		if !strings.Contains(b, "Router tools") || !strings.Contains(b, "General and password") {
			t.Errorf("%s: sidebar or tabs missing", p)
		}
	}
	// The real callback token appears only after the password is given.
	r.post("/admin/settings/mpesa/rotate-token", url.Values{"current_password": {pw}})
	tok := r.m.Effective().MpesaCallbackToken
	if tok == "" {
		t.Fatal("no token after rotate")
	}
	if strings.Contains(r.get(t, "/admin/settings/mpesa"), tok) {
		t.Fatal("callback token shown without the password")
	}
	resp := r.do("POST", "/admin/settings/mpesa/urls", url.Values{"current_password": {pw}}, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "/pay/cb/t/"+tok+"/stk") {
		t.Fatalf("revealed URLs missing the token (%d)", resp.StatusCode)
	}
}

func TestToolsConsolePolicy(t *testing.T) {
	r := newSetRig(t)
	run := func(form url.Values) string {
		form.Set("site", r.site.ID)
		resp := r.do("POST", "/admin/tools/console", form, pw)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("console: %d %s", resp.StatusCode, b)
		}
		return string(b)
	}
	if out := run(url.Values{"cmd": {"/ip/address/print"}}); !strings.Contains(out, "bridge-hotspot") {
		t.Fatalf("read-only command did not run: %s", out)
	}
	if out := run(url.Values{"cmd": {"/user/print"}}); !strings.Contains(out, "not allowed") {
		t.Fatalf("denied command not refused: %s", out)
	}
	n := len(r.rt.Executed)
	change := url.Values{"cmd": {"/ip/address/add address=10.9.9.9/32 interface=ether1"}}
	if out := run(change); !strings.Contains(out, "type the building name") {
		t.Fatalf("change ran without confirmation: %s", out)
	}
	bad := url.Values{"cmd": change["cmd"], "confirm_site": {r.site.Name}, "current_password": {"wrong"}}
	run(bad)
	if len(r.rt.Executed) != n {
		t.Fatal("a change ran with a wrong password")
	}
	good := url.Values{"cmd": change["cmd"], "confirm_site": {r.site.Name}, "current_password": {pw}}
	run(good)
	if len(r.rt.Executed) != n+1 {
		t.Fatalf("confirmed change did not run: %v", r.rt.Executed)
	}
	audited := 0
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if strings.Contains(e.Msg, "console") {
				audited++
			}
		}
	})
	if audited < 4 {
		t.Fatalf("console attempts audited: %d", audited)
	}
	// Ping and traceroute.
	resp := r.do("POST", "/admin/tools/traceroute", url.Values{"site": {r.site.ID}, "target": {"8.8.8.8"}}, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "196.201.0.1") {
		t.Fatalf("traceroute rows missing: %d", resp.StatusCode)
	}
	resp = r.do("POST", "/admin/tools/ping", url.Values{"site": {r.site.ID}, "target": {"8.8.8.8; /system reset-configuration"}}, pw)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "enter an IP address or a host name") {
		t.Fatalf("a target with a command in it was accepted: %s", b)
	}
}

func TestEquipmentFinder(t *testing.T) {
	r := newSetRig(t)
	r.rt.AddLease(routeros.Lease{MAC: "AA:BB:CC:DD:EE:01", Address: "10.5.50.77", HostName: "phone", Status: "bound"})
	b := r.get(t, "/admin/settings/equipment/find?mac=aa-bb-cc-dd-ee-01")
	if !strings.Contains(b, "10.5.50.77") {
		t.Fatalf("lease not found: %s", b)
	}
	resp := r.do("GET", "/admin/settings/equipment/find?mac=zz", nil, pw)
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("bad MAC: %d", resp.StatusCode)
	}
}

func TestCallbackTokenRoutesFollowLiveConfig(t *testing.T) {
	r := newSetRig(t)
	post := func(path string) int {
		resp, err := http.Post(r.ts.URL+path, "application/json", strings.NewReader(`{"TransID":"X1","BillRefNumber":"NOPE","TransAmount":"5"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Disabled: every form is 404.
	if c := post("/pay/cb/t/tok/c2b/validate"); c != 404 {
		t.Fatalf("disabled: %d", c)
	}
	r.mp.SetConfig(payments.MpesaConfig{Enabled: true, CallbackToken: "tok", PublicBaseURL: "https://x.example"})
	for path, want := range map[string]int{
		"/pay/cb/t/tok/c2b/validate": 200, "/pay/cb/c2b/validate?t=tok": 200,
		"/pay/cb/t/wrong/c2b/validate": 404, "/pay/cb/c2b/validate": 404, "/pay/cb/t/tok/stk": 200,
	} {
		if c := post(path); c != want {
			t.Errorf("%s: %d want %d", path, c, want)
		}
	}
	// Rotating the token (live) kills the old URL at once.
	r.mp.SetConfig(payments.MpesaConfig{Enabled: true, CallbackToken: "tok2", PublicBaseURL: "https://x.example"})
	if c := post("/pay/cb/t/tok/c2b/validate"); c != 404 {
		t.Fatalf("old token still works: %d", c)
	}
}

func TestWebhookTurnsOnWithoutRestart(t *testing.T) {
	var in *notify.Inbound
	m := newMultiOps(t, nil, func(config.Config, *store.Store, *sites.Manager, *billing.Service) web.Ops {
		in = notify.NewInbound(notify.InboundOptions{})
		return web.Ops{Inbound: in}
	})
	get := func() int {
		resp, err := http.Get(m.ts.URL + "/wa/webhook?hub.mode=subscribe&hub.verify_token=vt&hub.challenge=77")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(); c != 404 {
		t.Fatalf("webhook live without secrets: %d", c)
	}
	in.SetSecrets("app-secret", "vt")
	if c := get(); c != 200 {
		t.Fatalf("webhook not live after the secrets were saved: %d", c)
	}
	in.SetSecrets("app-secret", "")
	if c := get(); c != 404 {
		t.Fatalf("webhook still live after the verify token was cleared: %d", c)
	}
}
