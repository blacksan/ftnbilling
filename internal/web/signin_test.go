package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/settings"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

// testKey seals the TOTP secrets in every test server.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// A fast password hash: the production 600k rounds would make every test slow.
func fastHash(t testing.TB, pass string) string {
	t.Helper()
	h, err := settings.HashPasswordCost(pass, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// who is an admin user with a working authenticator app.
type who struct {
	Name, Role, Pass string
	ID               string
	Secret           []byte
}

// Code is the current 6-digit code. The replay guard is rewound first, so a
// test can sign in as often as it likes within one 30 second step.
func (w *who) Code(t testing.TB, st *store.Store) string {
	t.Helper()
	if err := st.Update(func(d *store.Data) error {
		u := d.AdminUsers[w.ID]
		u.TOTPLastStep = 0
		d.AdminUsers[w.ID] = u
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return authn.TOTPCode(w.Secret, authn.TOTPStep(time.Now()))
}

// seedUser makes the user (the first one is the owner) and enrols its
// authenticator app through the service, as the enrol page would.
func seedUser(t testing.TB, srv *web.Server, st *store.Store, name, role, pass string) *who {
	t.Helper()
	au := srv.Auth()
	hash := fastHash(t, pass)
	u, ok := au.UserByName(name)
	if !ok {
		if role == store.RoleOwner {
			if _, err := au.SeedOwner(name, hash); err != nil {
				t.Fatal(err)
			}
			u, _ = au.UserByName(name)
		} else {
			var err error
			if u, err = au.CreateUser(name, role, hash, "test", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	secret, err := au.BeginEnrol(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := au.ConfirmEnrol(u.ID, authn.TOTPCode(secret, authn.TOTPStep(time.Now())), "test", ""); err != nil {
		t.Fatal(err)
	}
	return &who{Name: name, Role: role, Pass: pass, ID: u.ID, Secret: secret}
}

// browser is a signed-in admin session: the cookie and its CSRF token.
type browser struct {
	Cookie *http.Cookie
	CSRF   string
}

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// signIn does the whole login over HTTP: password, then the authenticator code.
func signIn(t testing.TB, ts *httptest.Server, srv *web.Server, st *store.Store, w *who) *browser {
	t.Helper()
	return signInFrom(t, ts, srv, st, w, "")
}

// signInFrom is signIn as a client at address ip (via X-Forwarded-For).
func signInFrom(t testing.TB, ts *httptest.Server, srv *web.Server, st *store.Store, w *who, ip string) *browser {
	t.Helper()
	cl := &http.Client{CheckRedirect: noRedirect}
	greq, _ := http.NewRequest("GET", ts.URL+"/admin/login", nil)
	if ip != "" {
		greq.Header.Set("X-Forwarded-For", ip)
	}
	resp, err := cl.Do(greq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var lc *http.Cookie
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_lc") {
			lc = c
		}
	}
	if lc == nil {
		t.Fatalf("no login CSRF cookie (status %d)", resp.StatusCode)
	}
	post := func(path string, form url.Values, cookies ...*http.Cookie) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if ip != "" {
			req.Header.Set("X-Forwarded-For", ip)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		r, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r
	}
	r := post("/admin/login", url.Values{"username": {w.Name}, "password": {w.Pass}, "_csrf": {lc.Value}}, lc)
	sc := sessionCookie(r)
	if r.StatusCode != 303 || sc == nil {
		t.Fatalf("password step: %d", r.StatusCode)
	}
	sess, _, ok := srv.Auth().Session(sc.Value)
	if !ok {
		t.Fatal("no session after the password step")
	}
	r = post("/admin/login/totp", url.Values{"code": {w.Code(t, st)}, "_csrf": {sess.CSRF}}, sc)
	if r.StatusCode != 303 {
		t.Fatalf("code step: %d", r.StatusCode)
	}
	sc = sessionCookie(r)
	if sc == nil {
		t.Fatal("the session id did not rotate after the code")
	}
	sess, _, ok = srv.Auth().Session(sc.Value)
	if !ok || !sess.MFA {
		t.Fatal("not fully signed in")
	}
	return &browser{Cookie: sc, CSRF: sess.CSRF}
}

func sessionCookie(r *http.Response) *http.Cookie {
	for _, c := range r.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_session") && c.Value != "" {
			return c
		}
	}
	return nil
}

// Do sends a request as this browser; POST forms get the CSRF token.
func (b *browser) Do(cl *http.Client, base, method, path string, form url.Values) *http.Response {
	var rd io.Reader
	if method == "POST" {
		if form == nil {
			form = url.Values{}
		}
		form.Set("_csrf", b.CSRF)
		rd = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(b.Cookie)
	resp, err := cl.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}
