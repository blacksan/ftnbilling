package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/store"
)

// Cookies. With an https PUBLIC_BASE_URL the names carry the __Host- prefix
// (the browser then insists on Secure, Path=/ and no Domain); without one
// (tests, a bench) they are plain names so http://127.0.0.1 works.
const (
	sessionCookiePlain   = "ftn_session"
	sessionCookieHost    = "__Host-ftn_session"
	loginCSRFCookiePlain = "ftn_lc"
	loginCSRFCookieHost  = "__Host-ftn_lc"
)

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.cfg.PublicBaseURL, "https://") }

func (s *Server) sessionCookieName() string {
	if s.secureCookies() {
		return sessionCookieHost
	}
	return sessionCookiePlain
}

func (s *Server) loginCSRFName() string {
	if s.secureCookies() {
		return loginCSRFCookieHost
	}
	return loginCSRFCookiePlain
}

func (s *Server) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.secureCookies(),
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, s.cookie(s.sessionCookieName(), value, 0))
}

// The known-browser cookie (see authn/known.go): set after a full sign-in,
// read at the next one so the owner is never shut out by strangers' failures.
func (s *Server) knownCookieName() string {
	if s.secureCookies() {
		return "__Host-ftn_kb"
	}
	return "ftn_kb"
}

func (s *Server) setKnownCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, s.cookie(s.knownCookieName(), value, int(authn.KnownLife/time.Second)))
}

// knownID is the known id of the request's known-browser cookie when it
// belongs to userID, else "" (a stranger).
func (s *Server) knownID(r *http.Request, userID string) string {
	c, err := r.Cookie(s.knownCookieName())
	if err != nil || userID == "" {
		return ""
	}
	return s.knownValueID(c.Value, userID)
}

func (s *Server) knownValueID(value, userID string) string {
	if value == "" || userID == "" || s.authn.KnownUser(value) != userID {
		return ""
	}
	return authn.KnownID(value)
}

// rememberBrowser marks the browser as known after a full sign-in: a fresh
// value, or a longer life for the one it has.
func (s *Server) rememberBrowser(w http.ResponseWriter, r *http.Request, userID string) {
	if c, err := r.Cookie(s.knownCookieName()); err == nil && s.authn.KnownUser(c.Value) == userID {
		s.authn.RenewKnown(c.Value)
		s.setKnownCookie(w, c.Value)
		return
	}
	if v, err := s.authn.IssueKnown(userID); err == nil {
		s.setKnownCookie(w, v)
	}
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, s.cookie(s.sessionCookieName(), "", -1))
}

type (
	csrfKey    struct{}
	sessionKey struct{}
	userKeyCtx struct{}
)

func csrfOf(r *http.Request) string { t, _ := r.Context().Value(csrfKey{}).(string); return t }

func sessOf(r *http.Request) (store.AdminSession, bool) {
	v, ok := r.Context().Value(sessionKey{}).(store.AdminSession)
	return v, ok
}

func userOf(r *http.Request) (store.AdminUser, bool) {
	v, ok := r.Context().Value(userKeyCtx{}).(store.AdminUser)
	return v, ok
}

// sessionFrom finds the live session of a request.
func (s *Server) sessionFrom(r *http.Request) (store.AdminSession, store.AdminUser, bool) {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return store.AdminSession{}, store.AdminUser{}, false
	}
	return s.authn.Session(c.Value)
}

// sameOrigin is the browser-side CSRF check on top of the token: a request that
// says it came from another site (Sec-Fetch-Site, Origin) is refused. A client
// that sends neither header still needs the token.
func (s *Server) sameOrigin(r *http.Request) bool {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" && v != "same-origin" && v != "none" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if pu, err := url.Parse(s.cfg.PublicBaseURL); err == nil && pu.Host != "" && strings.EqualFold(u.Host, pu.Host) {
		return true
	}
	return false
}

// csrfOK checks the token of a state-changing form against the session's.
func csrfOK(r *http.Request, want string) bool {
	got := r.PostFormValue("_csrf")
	if got == "" {
		got = r.Header.Get("X-CSRF-Token")
	}
	return want != "" && safeEq(got, want)
}

// toLogin sends an unauthenticated browser to the login page.
func (s *Server) toLogin(w http.ResponseWriter, r *http.Request) {
	to := "/admin/login"
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/admin") && !strings.HasPrefix(r.URL.Path, "/admin/login") {
		to += "?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// secondStep is where a session that has not finished signing in has to go.
func (s *Server) secondStep(u store.AdminUser) string {
	if authn.Enrolled(u) {
		return "/admin/login/totp"
	}
	return "/admin/security/enrol"
}

// admin guards a page that needs a finished sign-in (password and second
// factor): the session, the role the page needs, and for POST the origin and
// CSRF checks. A session that has only passed the password reaches nothing here.
func (s *Server) admin(h http.HandlerFunc) http.Handler { return s.guard(h, false) }

// half guards the pages a half-signed-in session may use (code entry, enrolment).
func (s *Server) half(h http.HandlerFunc) http.Handler { return s.guard(h, true) }

func (s *Server) guard(h http.HandlerFunc, allowHalf bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.fromTunnel(r) {
			// Routers reach the server over their tunnel; they never get the admin.
			http.NotFound(w, r)
			return
		}
		sess, user, ok := s.sessionFrom(r)
		if !ok {
			s.clearIfStale(w, r)
			s.toLogin(w, r)
			return
		}
		if !sess.MFA && !allowHalf {
			http.Redirect(w, r, s.secondStep(user), http.StatusSeeOther)
			return
		}
		if sess.MFA && allowHalf && r.Method == http.MethodGet && r.URL.Path != "/admin/security/enrol/qr.png" {
			// Already signed in: the code and enrolment pages have nothing for them.
			if r.URL.Path == "/admin/login/totp" || r.URL.Path == "/admin/security/enrol" {
				http.Redirect(w, r, "/admin", http.StatusSeeOther)
				return
			}
		}
		if sess.MFA {
			if need := s.minRole(r.Method, r.URL.Path); authn.RoleRank(user.Role) < authn.RoleRank(need) {
				s.forbidden(w, r, sess, user)
				return
			}
		}
		post := r.Method != http.MethodGet && r.Method != http.MethodHead
		if post {
			if !s.sameOrigin(r) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if !csrfOK(r, sess.CSRF) {
				http.Error(w, "CSRF check failed", http.StatusForbidden)
				return
			}
		}
		s.authn.Touch(sess)
		ctx := context.WithValue(r.Context(), csrfKey{}, sess.CSRF)
		ctx = context.WithValue(ctx, sessionKey{}, sess)
		ctx = context.WithValue(ctx, userKeyCtx{}, user)
		r = r.WithContext(ctx)
		h(w, r)
		if post && sess.MFA {
			s.auditPost(w, r, user)
		}
	})
}

// clearIfStale removes a cookie that no longer matches a session.
func (s *Server) clearIfStale(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(s.sessionCookieName()); err == nil {
		s.clearSessionCookie(w)
	}
}

// auditPost records every state-changing admin request that went through, with
// who and from where (never the form: it may hold passwords and tokens).
func (s *Server) auditPost(w http.ResponseWriter, r *http.Request, u store.AdminUser) {
	code := 200
	if sw, ok := w.(*statusWriter); ok {
		code = sw.code
	}
	p := r.URL.Path
	if code >= 400 || strings.HasPrefix(p, "/admin/tools/") || strings.HasSuffix(p, "/test") || strings.HasPrefix(p, "/admin/security/") {
		return // security actions write their own, more exact, events
	}
	s.authn.AuditNow(u.Username, realIP(r), "POST "+p)
}

// minRole is the least role that may use a page. Viewers read, operators run
// the day-to-day work, and owners also reach settings, router tools, buildings'
// router set-up, users, national IDs and account deletion.
func (s *Server) minRole(method, path string) string {
	read := method == http.MethodGet || method == http.MethodHead
	if rest, ok := strings.CutPrefix(path, "/admin/routers"); ok && (rest == "" || rest[0] == '/') {
		path = "/admin/sites" + rest // D8: the new address has the old rules
	}
	switch {
	case strings.HasPrefix(path, "/admin/security/users"):
		return store.RoleOwner
	case strings.HasPrefix(path, "/admin/security"):
		return store.RoleViewer // everybody manages their own password, sessions and phones
	case strings.HasPrefix(path, "/admin/settings/equipment"):
		if read {
			return store.RoleViewer
		}
		return store.RoleOperator
	case strings.HasPrefix(path, "/admin/settings"), strings.HasPrefix(path, "/admin/tools"):
		return store.RoleOwner
	case !read && (strings.HasPrefix(path, "/admin/sites/") && path != "/admin/sites/profile" && !strings.HasPrefix(path, "/admin/sites/contacts") &&
		path != "/admin/sites/outage-notice" && path != "/admin/sites/packages" && path != "/admin/sites/sync" &&
		path != "/admin/sites/ports/update"): // operators change customer/unused ports; ChangePort keeps LAN and OLT for owners
		return store.RoleOwner
	case !read && (strings.HasPrefix(path, "/admin/customers/natid") || path == "/admin/customers/anonymize" || path == "/admin/accounts/delete" || path == "/admin/accounts/move"):
		return store.RoleOwner
	case !read:
		return store.RoleOperator
	}
	return store.RoleViewer
}

func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, sess store.AdminSession, u store.AdminUser) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`<!doctype html><meta name="viewport" content="width=device-width"><body style="font:16px system-ui;background:#0d1117;color:#e6e9ef;display:grid;place-items:center;height:90vh"><div>Your role (` +
		u.Role + `) cannot do this. <a style="color:#8b83ff" href="/admin">Back</a></div>`))
}
