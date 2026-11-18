package web_test

import (
	"context"
	"io"
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
	srv  *web.Server
	st   *store.Store
	m    *settings.Manager
	mp   *payments.Mpesa
	rt   *routeros.Mock
	site store.Site
	csrf string
	cl   *http.Client
	br   *browser
	who  *who

	alertMu sync.Mutex
	alerts  []string // security alerts the owners would have got
}

func (r *setRig) gotAlerts() []string {
	r.alertMu.Lock()
	defer r.alertMu.Unlock()
	return append([]string(nil), r.alerts...)
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
	r := &setRig{ts: ts, srv: srv, st: st, m: m, mp: mp, rt: mgr.MockRouter(),
		cl: &http.Client{CheckRedirect: noRedirect}}
	st.View(func(d *store.Data) { r.site = d.Sites[envID] })
	srv.SetSecurityAlertSink(func(msg string) { r.alertMu.Lock(); r.alerts = append(r.alerts, msg); r.alertMu.Unlock() })
	r.who = seedUser(t, srv, st, "admin", store.RoleOwner, pw)
	r.br = signIn(t, ts, srv, st, r.who)
	r.csrf = r.br.CSRF
	return r
}

// do sends a signed-in request. The pass argument is kept for old call sites
// and ignored: the session is the credential now.
func (r *setRig) do(method, path string, form url.Values, _ string) *http.Response {
	return r.br.Do(r.cl, r.ts.URL, method, path, form)
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

func TestSecurityPasswordFlow(t *testing.T) {
	r := newSetRig(t)
	other := signIn(t, r.ts, r.srv, r.st, r.who) // a second browser
	// Wrong current password: nothing changes.
	code, msg, _ := r.post("/admin/security/password", url.Values{"current_password": {"nope"}, "new_password": {longPW}, "confirm_password": {longPW}})
	if code != 303 || !strings.HasPrefix(msg, "Error") {
		t.Fatalf("wrong current password: %d %q", code, msg)
	}
	if c, _, _ := r.attempt("", "admin", pw); c != 303 {
		t.Fatalf("the password changed anyway: %d", c)
	}
	// Too short, and not matching.
	_, msg, _ = r.post("/admin/security/password", url.Values{"current_password": {pw}, "new_password": {"short"}, "confirm_password": {"short"}, "totp": {r.who.Code(t, r.st)}})
	if !strings.HasPrefix(msg, "Error") {
		t.Fatalf("short password accepted: %q", msg)
	}
	_, msg, _ = r.post("/admin/security/password", url.Values{"current_password": {pw}, "new_password": {longPW}, "confirm_password": {longPW + "x"}, "totp": {r.who.Code(t, r.st)}})
	if !strings.HasPrefix(msg, "Error") {
		t.Fatalf("mismatch accepted: %q", msg)
	}
	code, msg, _ = r.post("/admin/security/password", url.Values{"current_password": {pw}, "new_password": {longPW}, "confirm_password": {longPW}, "totp": {r.who.Code(t, r.st)}})
	if code != 303 || strings.HasPrefix(msg, "Error") {
		t.Fatalf("password change: %d %q", code, msg)
	}
	// The old password is refused, the new one accepted.
	if c, _, _ := r.attempt("203.0.113.9", "admin", pw); c != 401 {
		t.Fatalf("old password still works: %d", c)
	}
	if c, _, _ := r.attempt("203.0.113.8", "admin", longPW); c != 303 {
		t.Fatalf("new password refused: %d", c)
	}
	// This browser stays signed in; every other one is signed out.
	if c := r.do("GET", "/admin", nil, "").StatusCode; c != 200 {
		t.Fatalf("the current session was ended: %d", c)
	}
	resp := other.Do(r.cl, r.ts.URL, "GET", "/admin", nil)
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("the other session survived the password change: %d", resp.StatusCode)
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

// Nothing a user types reaches the router as a script: ping goes through the
// JSON ping call, and every console bypass is refused before Execute.
func TestToolsNeverBuildScriptsFromInput(t *testing.T) {
	r := newSetRig(t)
	n := len(r.rt.Executed)
	for _, target := range []string{"fe80::1%x;/user/add name=x", "fe80::1%eth0", "8.8.8.8 count=1; /system reboot", "-c", "$(id)", "a b"} {
		for _, tool := range []string{"ping", "traceroute"} {
			resp := r.do("POST", "/admin/tools/"+tool, url.Values{"site": {r.site.ID}, "target": {target}}, pw)
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(b), "enter an IP address or a host name") {
				t.Errorf("%s %q accepted", tool, target)
			}
		}
	}
	resp := r.do("POST", "/admin/tools/ping", url.Values{"site": {r.site.ID}, "target": {"8.8.8.8"}}, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "received=") {
		t.Fatalf("ping result missing: %s", b)
	}
	if len(r.rt.Executed) != n {
		t.Fatalf("ping ran a script on the router: %v", r.rt.Executed[n:])
	}
	for _, cmd := range []string{`:put "$[/system reset-configuration]"`, `:put [/interface wireguard get wg0 private-key]`, `:global f [:parse ":put 1"]; $f`,
		"$f", "/system reset-configuration", "/ping 8.8.8.8 count=1; /user add name=x", "/interface wireguard get wg0 private-key", "/ip hotspot user print where password=x"} {
		resp := r.do("POST", "/admin/tools/console", url.Values{"site": {r.site.ID}, "cmd": {cmd}, "confirm_site": {r.site.Name}, "current_password": {pw}}, pw)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "not allowed") {
			t.Errorf("%q was not refused", cmd)
		}
	}
	if len(r.rt.Executed) != n {
		t.Fatalf("a refused command reached the router: %v", r.rt.Executed[n:])
	}
	// A console ping and print take the safe paths; print is forced to terse.
	resp = r.do("POST", "/admin/tools/console", url.Values{"site": {r.site.ID}, "cmd": {"/ping 8.8.8.8 count=2"}}, pw)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "received=") || len(r.rt.Executed) != n {
		t.Fatalf("console ping: %s %v", b, r.rt.Executed)
	}
	resp = r.do("POST", "/admin/tools/console", url.Values{"site": {r.site.ID}, "cmd": {"/ip address print"}}, pw)
	resp.Body.Close()
	if len(r.rt.Executed) != n+1 || r.rt.Executed[n] != "/ip address print terse without-paging" {
		t.Fatalf("executed %v", r.rt.Executed[n:])
	}
	// Every submission is audited: building, outcome and byte count, no output.
	var msgs []string
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if strings.HasPrefix(e.Msg, "console on ") {
				msgs = append(msgs, e.Msg)
			}
		}
	})
	if len(msgs) < 10 {
		t.Fatalf("audited %d: %v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if !strings.Contains(m, r.site.Name) || !strings.Contains(m, "bytes") {
			t.Errorf("audit line lacks building or size: %s", m)
		}
		if strings.Contains(m, "bridge-hotspot") {
			t.Errorf("audit line holds output: %s", m)
		}
	}
}

func TestConsoleOutputRedacted(t *testing.T) {
	r := newSetRig(t)
	r.rt.ExecOut = func(string) (string, error) {
		return "0 name=wg0 private-key=\"SECRETKEY\" listen-port=13231\n1 name=u password=hunter2 profile=default", nil
	}
	resp := r.do("POST", "/admin/tools/console", url.Values{"site": {r.site.ID}, "cmd": {"/interface wireguard print"}}, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "SECRETKEY") || strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "listen-port=13231") {
		t.Fatalf("output not redacted: %s", b)
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

// In production, replacing or reverting the callback token needs an explicit
// tick; the old token keeps working for the grace period, in both URL forms.
func TestProductionTokenChangeNeedsConfirmation(t *testing.T) {
	r := newSetRig(t)
	vals := url.Values{"mpesa.key": {"consumerkey1234"}, "mpesa.secret": {"consumersecret1234"}, "mpesa.shortcode": {"174379"},
		"mpesa.passkey": {"bfb279f9aa9bdbcf158e97dd71a467cd2e0c893059b10f78e6b72ada1ed2c919"}, "mpesa.env": {"production"}, "current_password": {pw}}
	if _, msg, _ := r.post("/admin/settings/mpesa", vals); !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("save: %q", msg)
	}
	tok := r.m.Effective().MpesaCallbackToken
	if tok == "" || !strings.Contains(r.get(t, "/admin/settings/mpesa"), "Production warning") {
		t.Fatal("no token, or no production warning in the page")
	}
	// Rotate without the tick: refused, token unchanged.
	_, msg, _ := r.post("/admin/settings/mpesa/rotate-token", url.Values{"current_password": {pw}})
	if !strings.HasPrefix(msg, "Error") || !strings.Contains(msg, "register") || r.m.Effective().MpesaCallbackToken != tok {
		t.Fatalf("rotate without confirmation: %q", msg)
	}
	// Saving a replacement token or reverting it is refused the same way.
	_, msg, _ = r.post("/admin/settings/mpesa", url.Values{"mpesa.callback_token": {"a-brand-new-token-0123456789"}, "current_password": {pw}})
	if !strings.HasPrefix(msg, "Error") || r.m.Effective().MpesaCallbackToken != tok {
		t.Fatalf("token replaced without confirmation: %q", msg)
	}
	_, msg, _ = r.post("/admin/settings/revert", url.Values{"key": {"mpesa.callback_token"}, "current_password": {pw}})
	if !strings.HasPrefix(msg, "Error") || r.m.Effective().MpesaCallbackToken != tok {
		t.Fatalf("revert without confirmation: %q", msg)
	}
	// With the tick the revert is still refused: it would leave M-Pesa without a token.
	_, msg, _ = r.post("/admin/settings/revert", url.Values{"key": {"mpesa.callback_token"}, "current_password": {pw}, "confirm_reregister": {"1"}})
	if !strings.HasPrefix(msg, "Error") || r.m.Effective().MpesaCallbackToken != tok {
		t.Fatalf("revert left M-Pesa without a token: %q", msg)
	}
	// With the tick, rotate works and tells until when the old token works.
	_, msg, _ = r.post("/admin/settings/mpesa/rotate-token", url.Values{"current_password": {pw}, "confirm_reregister": {"1"}})
	if strings.HasPrefix(msg, "Error") || !strings.Contains(msg, "still works until") {
		t.Fatalf("confirmed rotate: %q", msg)
	}
	c := r.m.Effective()
	if c.MpesaCallbackToken == tok || c.MpesaCallbackTokenPrev != tok {
		t.Fatal("previous token not kept")
	}
	// The callbacks accept the previous token in the ?t= and the path form.
	r.mp.SetConfig(payments.MpesaConfig{Enabled: true, CallbackToken: c.MpesaCallbackToken, PrevToken: tok, PrevUntil: time.Now().Add(time.Hour), PublicBaseURL: "https://x.example"})
	post := func(path string) int {
		resp, err := http.Post(r.ts.URL+path, "application/json", strings.NewReader(`{"TransID":"X1","BillRefNumber":"NOPE","TransAmount":"5"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, p := range []string{"/pay/cb/t/" + tok + "/c2b/validate", "/pay/cb/c2b/validate?t=" + tok, "/pay/cb/t/" + c.MpesaCallbackToken + "/c2b/validate"} {
		if code := post(p); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
	r.mp.SetConfig(payments.MpesaConfig{Enabled: true, CallbackToken: c.MpesaCallbackToken, PrevToken: tok, PrevUntil: time.Now().Add(-time.Second), PublicBaseURL: "https://x.example"})
	if code := post("/pay/cb/t/" + tok + "/c2b/validate"); code != 404 {
		t.Errorf("expired previous token still works: %d", code)
	}
}

// With a mail password stored, pointing the mail settings at another server
// (host, port or user) needs the current admin password, so the stored
// password is not handed to a server chosen without it.
func TestEmailServerChangeNeedsPasswordWhenOneIsStored(t *testing.T) {
	r := newSetRig(t)
	_, msg, _ := r.post("/admin/settings/email", url.Values{"smtp.host": {"smtp.example.com"}, "smtp.port": {"587"}, "smtp.user": {"me"},
		"smtp.pass": {"mail-secret-1234"}, "smtp.from": {"billing@example.com"}, "current_password": {pw}})
	if !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("first save: %q", msg)
	}
	for name, form := range map[string]url.Values{
		"host":       {"smtp.host": {"evil.example.net"}},
		"port":       {"smtp.port": {"2525"}},
		"user":       {"smtp.user": {"someone-else"}},
		"clear user": {"smtp.user": {""}},
	} {
		_, msg, _ = r.post("/admin/settings/email", form)
		if !strings.HasPrefix(msg, "Error") {
			t.Errorf("%s changed without the password: %q", name, msg)
		}
	}
	c := r.m.Effective()
	if c.SMTPHost != "smtp.example.com" || c.SMTPPort != 587 || c.SMTPUser != "me" {
		t.Fatalf("settings changed anyway: %+v", c.SMTPHost)
	}
	// Unrelated changes, and the same values again, need nothing.
	_, msg, _ = r.post("/admin/settings/email", url.Values{"smtp.host": {"smtp.example.com"}, "smtp.port": {"587"}, "smtp.user": {"me"}, "smtp.from": {"bills@example.com"}})
	if !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("a harmless change was refused: %q", msg)
	}
	// With the password the host can change.
	_, msg, _ = r.post("/admin/settings/email", url.Values{"smtp.host": {"smtp.other.com"}, "current_password": {pw}})
	if !strings.HasPrefix(msg, "Saved") || r.m.Effective().SMTPHost != "smtp.other.com" {
		t.Fatalf("host change with the password: %q", msg)
	}
}

// Without a stored mail password the first setup needs no re-authentication.
func TestEmailFirstSetupNeedsNoPassword(t *testing.T) {
	r := newSetRig(t)
	_, msg, _ := r.post("/admin/settings/email", url.Values{"smtp.host": {"smtp.example.com"}, "smtp.port": {"587"}})
	if !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("%q", msg)
	}
}

// Ping and traceroute from this server run the program with an argument
// vector (no shell), after the target passed the strict check.
func TestServerSideTools(t *testing.T) {
	r := newSetRig(t)
	type call struct {
		name string
		args []string
	}
	var mu sync.Mutex
	var calls []call
	r.srv.SetToolRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, call{name, args})
		mu.Unlock()
		return []byte("PING ok\n4 packets transmitted, 4 received\n"), nil
	})
	run := func(tool, target string) string {
		resp := r.do("POST", "/admin/tools/"+tool, url.Values{"from": {"server"}, "target": {target}}, pw)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	if out := run("ping", "8.8.8.8"); !strings.Contains(out, "4 packets transmitted") || !strings.Contains(out, "this server") {
		t.Fatalf("server ping output missing: %s", out)
	}
	if len(calls) != 1 || calls[0].name != "/usr/bin/ping" ||
		strings.Join(calls[0].args, " ") != "-n -c 4 -W 2 -w 10 -- 8.8.8.8" {
		t.Fatalf("ping call: %+v", calls)
	}
	run("traceroute", "example.co.ke")
	if len(calls) != 2 || calls[1].name != "/usr/bin/traceroute" ||
		strings.Join(calls[1].args, " ") != "-n -m 20 -q 1 -w 1 -- example.co.ke" {
		t.Fatalf("traceroute call: %+v", calls)
	}
	run("ping", "2001:db8::1")
	// Nothing odd reaches the program.
	r.srv.ResetToolLimiter()
	n := len(calls)
	for _, bad := range []string{"-c", "-f", "-4", "--help", "fe80::1%eth0", "8.8.8.8; id", "$(id)", "`id`", "a b", "8.8.8.8 -c1", "-example.com", "", "../x", "http://x"} {
		for _, tool := range []string{"ping", "traceroute"} {
			if out := run(tool, bad); !strings.Contains(out, "enter an IP address or a host name") {
				t.Errorf("%s %q was not refused", tool, bad)
			}
		}
	}
	if len(calls) != n {
		t.Fatalf("a refused target reached the program: %+v", calls[n:])
	}
	// A hung program ends at the timeout.
	r.srv.ResetToolLimiter()
	defer web.SetServerToolTimeout(150 * time.Millisecond)()
	r.srv.SetToolRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if out := run("ping", "8.8.8.8"); !strings.Contains(out, "timed out") {
		t.Fatalf("no timeout message: %s", out)
	}
}

func TestToolsPageOffersServerAndRouter(t *testing.T) {
	r := newSetRig(t)
	b := r.get(t, "/admin/tools")
	if !strings.Contains(b, `name="from"`) || !strings.Contains(b, `value="server"`) || !strings.Contains(b, "this server") {
		t.Fatal("the Tools page has no from=server choice")
	}
}

// Test buttons allow 5 a minute; the equipment finder 5 a minute.
func TestActionRateLimits(t *testing.T) {
	r := newSetRig(t)
	var msg string
	for i := 0; i < 6; i++ {
		_, msg, _ = r.post("/admin/settings/email/test", url.Values{})
	}
	if !strings.Contains(msg, "too many") {
		t.Fatalf("6th test email: %q", msg)
	}
	for i := 0; i < 5; i++ {
		resp := r.do("GET", "/admin/settings/equipment/find?mac=aa-bb-cc-dd-ee-0"+string(rune('1'+i)), nil, pw)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("search %d: %d", i, resp.StatusCode)
		}
	}
	resp := r.do("GET", "/admin/settings/equipment/find?mac=aa-bb-cc-dd-ee-09", nil, pw)
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 303 || !strings.Contains(loc.Query().Get("msg"), "too many") {
		t.Fatalf("6th search: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// The revealed-URLs page is the result of a POST. Its address answers a GET
// with a redirect, and the forms on it return to the M-Pesa tab, not to it.
func TestMpesaURLsPageIsNotADeadEnd(t *testing.T) {
	r := newSetRig(t)
	resp := r.do("GET", "/admin/settings/mpesa/urls", nil, pw)
	resp.Body.Close()
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/settings/mpesa" {
		t.Fatalf("GET /urls: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = r.do("POST", "/admin/settings/mpesa/urls", url.Values{"current_password": {pw}}, pw)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `name="next" value="/admin/settings/mpesa"`) || strings.Contains(string(b), `value="/admin/settings/mpesa/urls"`) {
		t.Fatalf("revealed page: %d, next field wrong", resp.StatusCode)
	}
}
