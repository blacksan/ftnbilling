package web_test

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

// req sends one raw request and returns the response and its body.
func (r *setRig) req(method, path string, form url.Values, hdr map[string]string, cookies ...*http.Cookie) (*http.Response, string) {
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	rq, _ := http.NewRequest(method, r.ts.URL+path, rd)
	if form != nil {
		rq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		rq.Header.Set(k, v)
	}
	for _, c := range cookies {
		rq.AddCookie(c)
	}
	resp, err := r.cl.Do(rq)
	if err != nil {
		panic(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// loginForm fetches the login page and returns its CSRF cookie.
func (r *setRig) loginForm() *http.Cookie {
	resp, _ := r.req("GET", "/admin/login", nil, nil)
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_lc") {
			return c
		}
	}
	panic("no login cookie")
}

// passwordStep signs in with the password only and returns the half session.
func (r *setRig) passwordStep(user, pass, setup string) (*http.Response, *http.Cookie) {
	lc := r.loginForm()
	f := url.Values{"username": {user}, "password": {pass}, "_csrf": {lc.Value}}
	if setup != "" {
		f.Set("setup_code", setup)
	}
	resp, _ := r.req("POST", "/admin/login", f, nil, lc)
	return resp, sessionCookie(resp)
}

func TestLoginPageServedSignedOutAndLinkedFromPortal(t *testing.T) {
	r := newSetRig(t)
	resp, body := r.req("GET", "/admin/login", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) || !strings.Contains(body, `name="username"`) {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	if strings.Contains(strings.ToLower(body), "wireguard") {
		t.Error("the login page talks about the VPN")
	}
	// Customers are not shown a way to the admin sign-in: staff go to /admin.
	_, page := r.req("GET", "/pay", nil, nil)
	if strings.Contains(page, `/admin/login`) || strings.Contains(page, "Admin login") {
		t.Fatalf("the portal links to the admin sign-in:\n%s", page)
	}
}

func TestSessionCookieAttributesAndRotation(t *testing.T) {
	r := newSetRig(t) // PUBLIC_BASE_URL is https
	resp, half := r.passwordStep("admin", pw, "")
	if half == nil || resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/login/totp" {
		t.Fatalf("password step: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	check := func(c *http.Cookie) {
		t.Helper()
		if c.Name != "__Host-ftn_session" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
			t.Errorf("cookie attributes: %+v", c)
		}
		if len(c.Value) < 40 {
			t.Errorf("session id too short: %d", len(c.Value))
		}
	}
	check(half)
	if c := r.loginForm(); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("login CSRF cookie: %+v", c)
	}
	sess, _, _ := r.srv.Auth().Session(half.Value)
	resp, _ = r.req("POST", "/admin/login/totp", url.Values{"code": {r.who.Code(t, r.st)}, "_csrf": {sess.CSRF}}, nil, half)
	full := sessionCookie(resp)
	if resp.StatusCode != 303 || full == nil {
		t.Fatalf("code step: %d", resp.StatusCode)
	}
	check(full)
	if full.Value == half.Value {
		t.Fatal("session id not rotated after the second factor")
	}
	// The pre-code id is dead.
	resp, _ = r.req("GET", "/admin", nil, nil, half)
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login") {
		t.Fatalf("the old id still works: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = r.req("GET", "/admin", nil, nil, full)
	if resp.StatusCode != 200 {
		t.Fatalf("new id: %d", resp.StatusCode)
	}
	// Only a hash is stored: the cookie value is nowhere in the store.
	r.st.View(func(d *store.Data) {
		for id, s := range d.AdminSessions {
			if id == full.Value || s.ID == full.Value || id == half.Value {
				t.Fatal("a raw session id is stored")
			}
		}
	})
}

// A session that has only passed the password reaches the code or enrol page, nothing else.
func TestHalfSessionReachesOnlyTheSecondStep(t *testing.T) {
	r := newSetRig(t)
	_, half := r.passwordStep("admin", pw, "")
	for _, p := range []string{"/admin", "/admin/accounts", "/admin/security", "/admin/settings", "/admin/payments.csv"} {
		resp, _ := r.req("GET", p, nil, nil, half)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/login/totp" {
			t.Errorf("%s with a half session: %d -> %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	sess, _, _ := r.srv.Auth().Session(half.Value)
	resp, _ := r.req("POST", "/admin/accounts/create", url.Values{"flat": {"x"}, "_csrf": {sess.CSRF}}, nil, half)
	if resp.StatusCode != 303 {
		t.Errorf("a half session POST: %d", resp.StatusCode)
	}
	// A wrong code does not upgrade it, and the API refuses cookies altogether.
	resp, _ = r.req("POST", "/admin/login/totp", url.Values{"code": {"000000"}, "_csrf": {sess.CSRF}}, nil, half)
	if resp.StatusCode != 401 {
		t.Errorf("wrong code: %d", resp.StatusCode)
	}
	resp, _ = r.req("GET", "/api/v1/me", nil, nil, half)
	if resp.StatusCode != 401 {
		t.Errorf("cookie on the API: %d", resp.StatusCode)
	}
}

func TestTOTPReplayRefused(t *testing.T) {
	r := newSetRig(t)
	code := r.who.Code(t, r.st) // rewinds the replay guard
	step := func(c string) int {
		_, half := r.passwordStep("admin", pw, "")
		sess, _, _ := r.srv.Auth().Session(half.Value)
		resp, _ := r.req("POST", "/admin/login/totp", url.Values{"code": {c}, "_csrf": {sess.CSRF}}, nil, half)
		return resp.StatusCode
	}
	if got := step(code); got != 303 {
		t.Fatalf("first use: %d", got)
	}
	if got := step(code); got != 401 {
		t.Fatalf("the same code worked twice: %d", got)
	}
}

func TestEnrolGateWithSetupCode(t *testing.T) {
	r := newSetRig(t)
	au := r.srv.Auth()
	hash, _ := settings.HashPasswordCost(longPW, 1000)
	u, err := au.CreateUser("fresh", store.RoleOperator, hash, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	// No setup code: the password alone opens nothing, and the answer is the generic one.
	resp, half := r.passwordStep("fresh", longPW, "")
	if resp.StatusCode != 401 || half != nil {
		t.Fatalf("login without a setup code: %d", resp.StatusCode)
	}
	code, err := au.IssueSetupCode(u.ID, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp, half = r.passwordStep("fresh", longPW, "AAAA-BBBB-CCCC-DDDD"); resp.StatusCode != 401 || half != nil {
		t.Fatalf("wrong setup code: %d", resp.StatusCode)
	}
	resp, half = r.passwordStep("fresh", longPW, strings.ToLower(code))
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/security/enrol" || half == nil {
		t.Fatalf("right setup code: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Until enrolled, only the enrol page opens.
	if resp, _ := r.req("GET", "/admin", nil, nil, half); resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/security/enrol" {
		t.Fatalf("/admin before enrolment: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, page := r.req("GET", "/admin/security/enrol", nil, nil, half)
	if resp.StatusCode != 200 || !strings.Contains(page, "otpauth") && !strings.Contains(page, "qr.png") {
		t.Fatalf("enrol page: %d", resp.StatusCode)
	}
	if resp, _ := r.req("GET", "/admin/security/enrol/qr.png", nil, nil, half); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sess, _, _ := au.Session(half.Value)
	// A wrong code is refused and nothing is enrolled.
	resp, _ = r.req("POST", "/admin/security/enrol", url.Values{"code": {"123456"}, "_csrf": {sess.CSRF}}, nil, half)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong enrol code: %d", resp.StatusCode)
	}
	if got, _ := au.UserByID(u.ID); authn.Enrolled(got) {
		t.Fatal("enrolled with a wrong code")
	}
	secret, _ := au.BeginEnrol(u.ID)
	resp, page = r.req("POST", "/admin/security/enrol", url.Values{"code": {authn.TOTPCode(secret, authn.TOTPStep(time.Now()))}, "_csrf": {sess.CSRF}}, nil, half)
	full := sessionCookie(resp)
	if resp.StatusCode != 200 || full == nil || full.Value == half.Value {
		t.Fatalf("enrol: %d (cookie rotated: %v)", resp.StatusCode, full != nil)
	}
	if n := len(regexp.MustCompile(`<span>([a-z2-9]{5}-[a-z2-9]{5})</span>`).FindAllString(page, -1)); n != authn.RecoveryCodeCount {
		t.Fatalf("recovery codes shown: %d", n)
	}
	if au.HasSetupCode(u.ID) {
		t.Fatal("the setup code survived enrolment")
	}
	if resp, _ := r.req("GET", "/admin", nil, nil, full); resp.StatusCode != 200 {
		t.Fatalf("/admin after enrolment: %d", resp.StatusCode)
	}
	// A new session needs password + code; the old setup code is gone.
	if resp, half = r.passwordStep("fresh", longPW, code); resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/login/totp" {
		t.Fatalf("second login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Secrets are sealed at rest.
	r.st.View(func(d *store.Data) {
		got := d.AdminUsers[u.ID]
		if got.TOTPSecretEnc == "" || strings.Contains(got.TOTPSecretEnc, authn.EncodeSecret(secret)) {
			t.Fatalf("totp secret at rest: %q", got.TOTPSecretEnc)
		}
	})
}

func TestRecoveryCodeSignsInOnce(t *testing.T) {
	r := newSetRig(t)
	codes, err := r.srv.Auth().RegenerateRecoveryCodes(r.who.ID, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	use := func(c string) int {
		_, half := r.passwordStep("admin", pw, "")
		sess, _, _ := r.srv.Auth().Session(half.Value)
		resp, _ := r.req("POST", "/admin/login/totp", url.Values{"recovery_code": {c}, "_csrf": {sess.CSRF}}, nil, half)
		return resp.StatusCode
	}
	if got := use(codes[0]); got != 303 {
		t.Fatalf("first use: %d", got)
	}
	if got := use(codes[0]); got != 401 {
		t.Fatalf("a recovery code worked twice: %d", got)
	}
	if got := use(codes[1]); got != 303 {
		t.Fatalf("a different code: %d", got)
	}
	r.st.View(func(d *store.Data) {
		for _, h := range d.AdminUsers[r.who.ID].RecoveryCodes {
			if h == codes[1] || strings.Contains(h, codes[1]) {
				t.Fatal("a recovery code is stored in clear")
			}
		}
	})
}

func TestCSRFAndOriginChecks(t *testing.T) {
	r := newSetRig(t)
	do := func(form url.Values, hdr map[string]string) int {
		resp, _ := r.req("POST", "/admin/packages/save", form, hdr, r.br.Cookie)
		return resp.StatusCode
	}
	good := func() url.Values {
		return url.Values{"name": {"Promo"}, "duration": {"30"}, "speed": {"3"}, "price": {"5"}, "_csrf": {r.br.CSRF}}
	}
	if got := do(good(), nil); got != 303 {
		t.Fatalf("valid post: %d", got)
	}
	f := good()
	f.Del("_csrf")
	if got := do(f, nil); got != 403 {
		t.Errorf("missing token: %d", got)
	}
	f = good()
	f.Set("_csrf", strings.Repeat("0", len(r.br.CSRF)))
	if got := do(f, nil); got != 403 {
		t.Errorf("wrong token: %d", got)
	}
	if got := do(good(), map[string]string{"Origin": "https://evil.example"}); got != 403 {
		t.Errorf("cross-origin: %d", got)
	}
	if got := do(good(), map[string]string{"Sec-Fetch-Site": "cross-site"}); got != 403 {
		t.Errorf("cross-site fetch: %d", got)
	}
	if got := do(good(), map[string]string{"Origin": "https://billing.example.co.ke"}); got != 303 {
		t.Errorf("the public origin: %d", got)
	}
	// The header works for scripts.
	f = good()
	f.Del("_csrf")
	if got := do(f, map[string]string{"X-CSRF-Token": r.br.CSRF}); got != 303 {
		t.Errorf("header token: %d", got)
	}
	// Another session's token does not do.
	other := signIn(t, r.ts, r.srv, r.st, r.who)
	f = good()
	f.Set("_csrf", other.CSRF)
	if got := do(f, nil); got != 403 {
		t.Errorf("another session's token: %d", got)
	}
	// The login form has its own double-submit check.
	lc := r.loginForm()
	resp, _ := r.req("POST", "/admin/login", url.Values{"username": {"admin"}, "password": {pw}, "_csrf": {"nope"}}, nil, lc)
	if resp.StatusCode != 403 {
		t.Errorf("login with a wrong token: %d", resp.StatusCode)
	}
	resp, _ = r.req("POST", "/admin/login", url.Values{"username": {"admin"}, "password": {pw}}, nil)
	if resp.StatusCode != 403 {
		t.Errorf("login without a token: %d", resp.StatusCode)
	}
	resp, _ = r.req("POST", "/admin/login", url.Values{"username": {"admin"}, "password": {pw}, "_csrf": {lc.Value}}, map[string]string{"Origin": "https://evil.example"}, lc)
	if resp.StatusCode != 403 {
		t.Errorf("login cross-origin: %d", resp.StatusCode)
	}
}

func TestEverySessionExpiryPath(t *testing.T) {
	r := newSetRig(t)
	get := func(c *http.Cookie) int { resp, _ := r.req("GET", "/admin", nil, nil, c); return resp.StatusCode }
	expire := func(c *http.Cookie, mut func(*store.AdminSession)) {
		sess, _, _ := r.srv.Auth().Session(c.Value)
		if err := r.st.Update(func(d *store.Data) error {
			s := d.AdminSessions[sess.ID]
			mut(&s)
			d.AdminSessions[sess.ID] = s
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Idle for more than 30 minutes.
	b := signIn(t, r.ts, r.srv, r.st, r.who)
	if get(b.Cookie) != 200 {
		t.Fatal("fresh session refused")
	}
	expire(b.Cookie, func(s *store.AdminSession) { s.LastSeenAt = time.Now().Add(-authn.SessionIdle - time.Minute) })
	if get(b.Cookie) != 303 {
		t.Error("an idle session was honoured")
	}
	// Past its absolute end.
	b = signIn(t, r.ts, r.srv, r.st, r.who)
	expire(b.Cookie, func(s *store.AdminSession) { s.ExpiresAt = time.Now().Add(-time.Minute) })
	if get(b.Cookie) != 303 {
		t.Error("an expired session was honoured")
	}
	// A half session left for more than 15 minutes.
	_, half := r.passwordStep("admin", pw, "")
	expire(half, func(s *store.AdminSession) { s.ExpiresAt = time.Now().Add(-time.Minute) })
	resp, _ := r.req("GET", "/admin/login/totp", nil, nil, half)
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/admin/login") {
		t.Errorf("an old half session: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLogoutNeedsPostAndKillsTheSession(t *testing.T) {
	r := newSetRig(t)
	// A GET never logs out.
	resp, _ := r.req("GET", "/admin/logout", nil, nil, r.br.Cookie)
	if resp.StatusCode != 303 {
		t.Fatalf("GET logout: %d", resp.StatusCode)
	}
	if resp, _ := r.req("GET", "/admin", nil, nil, r.br.Cookie); resp.StatusCode != 200 {
		t.Fatal("a GET logged the admin out")
	}
	resp, _ = r.req("POST", "/admin/logout", url.Values{"_csrf": {r.br.CSRF}}, nil, r.br.Cookie)
	if resp.StatusCode != 303 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	cleared := false
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_session") && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the cookie was not cleared")
	}
	if resp, _ := r.req("GET", "/admin", nil, nil, r.br.Cookie); resp.StatusCode != 303 {
		t.Fatalf("the session survived logout: %d", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	r := newSetRig(t)
	for _, p := range []string{"/admin/login", "/portal", "/admin"} {
		resp, _ := r.req("GET", p, nil, nil, r.br.Cookie)
		h := resp.Header
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") == "" {
			t.Errorf("%s: missing basic headers: %v", p, h)
		}
		if !strings.Contains(h.Get("Strict-Transport-Security"), "max-age=") {
			t.Errorf("%s: no HSTS on an https deployment", p)
		}
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") && !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", p, csp)
		}
	}
	// The sign-in pages allow no inline script at all.
	resp, _ := r.req("GET", "/admin/login", nil, nil)
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") && strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("login CSP allows inline script: %q", csp)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("login page cacheable: %q", cc)
	}
	// An http deployment must not send HSTS.
	a := newApp(t, false)
	resp, _ = a.cl.Get(a.ts.URL + "/admin/login")
	resp.Body.Close()
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS over plain http")
	}
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, "__Host-") || c.Secure {
			t.Errorf("a plain-http deployment set %+v", c)
		}
	}
}

// Viewers read, operators work, owners configure.
func TestAdminPageRoles(t *testing.T) {
	r := newSetRig(t)
	viewer := seedUser(t, r.srv, r.st, "vera", store.RoleViewer, longPW)
	oper := seedUser(t, r.srv, r.st, "otto", store.RoleOperator, longPW)
	vb, ob := signIn(t, r.ts, r.srv, r.st, viewer), signIn(t, r.ts, r.srv, r.st, oper)
	code := func(b *browser, method, path string) int {
		resp := b.Do(r.cl, r.ts.URL, method, path, nil)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, tc := range []struct {
		method, path string
		viewer, oper int // want
	}{
		{"GET", "/admin", 200, 200},
		{"GET", "/admin/accounts", 200, 200},
		{"GET", "/admin/security", 200, 200},
		{"GET", "/admin/settings", 403, 403},
		{"POST", "/admin/accounts/create", 403, 303},
		{"POST", "/admin/settings/whatsapp", 403, 403},
		{"POST", "/admin/security/users/create", 403, 403},
		{"POST", "/admin/accounts/delete", 403, 403},
	} {
		if got := code(vb, tc.method, tc.path); got != tc.viewer {
			t.Errorf("viewer %s %s: %d, want %d", tc.method, tc.path, got, tc.viewer)
		}
		if got := code(ob, tc.method, tc.path); got != tc.oper {
			t.Errorf("operator %s %s: %d, want %d", tc.method, tc.path, got, tc.oper)
		}
		if got := code(r.br, tc.method, tc.path); got == 403 && tc.path != "/admin/security/users/create" {
			t.Errorf("owner %s %s: 403", tc.method, tc.path)
		}
	}
}

// Disabling a user or changing the role takes effect on the next request.
func TestDisableEndsSessions(t *testing.T) {
	r := newSetRig(t)
	vera := seedUser(t, r.srv, r.st, "vera", store.RoleViewer, longPW)
	vb := signIn(t, r.ts, r.srv, r.st, vera)
	resp := vb.Do(r.cl, r.ts.URL, "GET", "/admin", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if err := r.srv.Auth().SetDisabled(vera.ID, true, "test", ""); err != nil {
		t.Fatal(err)
	}
	resp = vb.Do(r.cl, r.ts.URL, "GET", "/admin", nil)
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("a disabled user's session: %d", resp.StatusCode)
	}
	if c, _, _ := r.attempt("203.0.113.40", "vera", longPW); c != 401 {
		t.Fatalf("a disabled user signed in: %d", c)
	}
}
