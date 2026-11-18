package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/qr"
	"ispbill/internal/store"
)

// authPage is the data of the login, code-entry and enrolment pages.
type authPage struct {
	Brand, CSRF, Next, Error, Info string
	Username                       string
	NoKey                          bool
	// Enrolment
	Secret, URI string
	Codes       []string
	Done        bool
}

func (s *Server) loginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/login", s.loginPage)
	mux.HandleFunc("POST /admin/login", s.loginPost)
	mux.Handle("GET /admin/login/totp", s.half(s.totpPage))
	mux.Handle("POST /admin/login/totp", s.half(s.totpPost))
	mux.Handle("GET /admin/security/enrol", s.half(s.enrolPage))
	mux.Handle("GET /admin/security/enrol/qr.png", s.half(s.enrolQR))
	mux.Handle("POST /admin/security/enrol", s.half(s.enrolPost))
	mux.Handle("POST /admin/logout", s.half(s.logout))
	// A GET must never log anybody out (an image tag on another site could).
	mux.HandleFunc("GET /admin/logout", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", http.StatusSeeOther) })
}

func (s *Server) authPage(r *http.Request) authPage {
	return authPage{Brand: s.cfg.BrandName, Next: nextParam(r)}
}

// nextParam reads where to go after signing in, from the query or the form.
func nextParam(r *http.Request) string {
	n := r.URL.Query().Get("next")
	if n == "" {
		n = r.PostFormValue("next")
	}
	if n == "" {
		return ""
	}
	return safeNext(n)
}

// ---- login CSRF (before there is a session) ------------------------------------------

// loginCSRF returns the token for a form shown before a session exists
// (double submit: the same value in a SameSite=Strict cookie and in the form).
func (s *Server) loginCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(s.loginCSRFName()); err == nil && len(c.Value) == 48 {
		return c.Value
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	http.SetCookie(w, s.cookie(s.loginCSRFName(), tok, 3600))
	return tok
}

func (s *Server) loginCSRFOK(r *http.Request) bool {
	c, err := r.Cookie(s.loginCSRFName())
	if err != nil || c.Value == "" {
		return false
	}
	return safeEq(r.PostFormValue("_csrf"), c.Value)
}

// ---- pages ---------------------------------------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if sess, u, ok := s.sessionFrom(r); ok {
		if sess.MFA {
			http.Redirect(w, r, orDefault(nextParam(r), "/admin"), http.StatusSeeOther)
		} else {
			http.Redirect(w, r, s.secondStep(u), http.StatusSeeOther)
		}
		return
	}
	p := s.authPage(r)
	p.CSRF = s.loginCSRF(w, r)
	s.render(w, "admin_login", p)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

const genericLoginError = "Wrong user name, password or setup code."

func (s *Server) loginFail(w http.ResponseWriter, r *http.Request, p authPage, code int, msg string) {
	p.Error = msg
	p.CSRF = s.loginCSRF(w, r)
	w.WriteHeader(code)
	s.render(w, "admin_login", p)
}

// tooMany answers a refused attempt.
func (s *Server) tooMany(w http.ResponseWriter, r *http.Request, page string, p authPage, ip string, out outcome) {
	if out.limited {
		s.auth.auditBlocked(ip, time.Now())
	}
	secs := int(out.wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	p.Error = "Too many attempts. Wait a few minutes and try again."
	if out.busy {
		p.Error = "The server is busy checking passwords. Try again in a moment."
	}
	p.CSRF = s.formCSRF(w, r)
	w.WriteHeader(http.StatusTooManyRequests)
	s.render(w, page, p)
}

// formCSRF is the token of the page being shown: the session's, else the login one.
func (s *Server) formCSRF(w http.ResponseWriter, r *http.Request) string {
	if t := csrfOf(r); t != "" {
		return t
	}
	return s.loginCSRF(w, r)
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	ip := realIP(r)
	p := s.authPage(r)
	p.Username = clipStr(r.PostFormValue("username"), 80)
	if !s.loginCSRFOK(r) || !s.sameOrigin(r) {
		s.loginFail(w, r, p, http.StatusForbidden, "The page expired. Please try again.")
		return
	}
	name, pass, setup := strings.TrimSpace(p.Username), r.PostFormValue("password"), r.PostFormValue("setup_code")
	if name == "" || pass == "" || len(pass) > 300 {
		s.loginFail(w, r, p, http.StatusUnauthorized, genericLoginError)
		return
	}
	att, out := s.auth.begin(ip, name)
	if !out.ok {
		s.tooMany(w, r, "admin_login", p, ip, out)
		return
	}
	u, exists := s.authn.UserByName(name)
	good, busy := s.auth.password(u, exists, pass)
	if busy {
		att.end(false, false)
		s.tooMany(w, r, "admin_login", p, ip, outcome{busy: true, wait: 2 * time.Second})
		return
	}
	// Until the user has set up two-factor sign-in, the setup code stands in for
	// the second factor: without it a password alone opens nothing.
	if good && !authn.Enrolled(u) && !s.authn.CheckSetupCode(u.ID, setup) {
		good = false
	}
	if !good {
		att.end(true, false)
		s.loginFail(w, r, p, http.StatusUnauthorized, genericLoginError)
		return
	}
	// Password right; the second factor is still to come, so the books stay as they were.
	att.end(false, false)
	// A new session id on every login: nothing set before it carries over.
	if c, err := r.Cookie(s.sessionCookieName()); err == nil {
		s.authn.DeleteSession(authn.HashToken(c.Value))
	}
	val, _, err := s.authn.CreateSession(u.ID, ip, r.UserAgent(), false)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.setSessionCookie(w, val)
	to := s.secondStep(u)
	if n := nextParam(r); n != "" && authn.Enrolled(u) {
		to += "?next=" + url.QueryEscape(n)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) totpPage(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	if !authn.Enrolled(u) {
		http.Redirect(w, r, "/admin/security/enrol", http.StatusSeeOther)
		return
	}
	p := s.authPage(r)
	p.CSRF, p.Username = csrfOf(r), u.Username
	s.render(w, "admin_totp", p)
}

func (s *Server) totpPost(w http.ResponseWriter, r *http.Request) {
	ip := realIP(r)
	sess, _ := sessOf(r)
	u, _ := userOf(r)
	p := s.authPage(r)
	p.CSRF, p.Username = sess.CSRF, u.Username
	if !authn.Enrolled(u) {
		http.Redirect(w, r, "/admin/security/enrol", http.StatusSeeOther)
		return
	}
	code, rec := authn.NormalizeCode(r.PostFormValue("code")), strings.TrimSpace(r.PostFormValue("recovery_code"))
	if code == "" && rec == "" {
		p.Error = "Enter the 6-digit code from your authenticator app."
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "admin_totp", p)
		return
	}
	att, out := s.auth.begin(ip, u.Username)
	if !out.ok {
		s.tooMany(w, r, "admin_totp", p, ip, out)
		return
	}
	var err error
	if rec != "" {
		err = s.authn.UseRecoveryCode(u.ID, rec, ip)
	} else {
		err = s.authn.VerifyTOTP(u.ID, code)
	}
	if err != nil {
		att.end(!errors.Is(err, store.ErrUnavailable), false)
		if errors.Is(err, authn.ErrBadCode) {
			p.Error = "That code is not right."
			if rec != "" {
				p.Error = "That recovery code is not right or was already used."
			}
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "admin_totp", p)
			return
		}
		s.storeError(w, err)
		return
	}
	att.end(false, true)
	val, _, err := s.authn.Rotate(sess.ID, true)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.setSessionCookie(w, val)
	s.authn.AuditNow(u.Username, ip, "signed in to the admin pages")
	http.Redirect(w, r, orDefault(nextParam(r), "/admin"), http.StatusSeeOther)
}

// ---- enrolment ---------------------------------------------------------------------------------

func groupSecret(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) enrolPage(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	if authn.Enrolled(u) {
		http.Redirect(w, r, "/admin/login/totp", http.StatusSeeOther)
		return
	}
	p := s.authPage(r)
	p.CSRF, p.Username = csrfOf(r), u.Username
	sec, err := s.authn.BeginEnrol(u.ID)
	switch {
	case errors.Is(err, authn.ErrNoDataKey):
		p.NoKey = true
		p.Error = "Two-factor sign-in cannot be set up: DATA_KEY is missing from the server's environment file."
	case err != nil:
		s.storeError(w, err)
		return
	default:
		p.Secret = groupSecret(authn.EncodeSecret(sec))
		p.URI = authn.OTPAuthURI(s.authn.Issuer(), u.Username, sec)
	}
	s.render(w, "admin_enrol", p)
}

func (s *Server) enrolQR(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	sec, err := s.authn.BeginEnrol(u.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	code, err := qr.Encode(authn.OTPAuthURI(s.authn.Issuer(), u.Username, sec))
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(code.PNG(6))
}

func (s *Server) enrolPost(w http.ResponseWriter, r *http.Request) {
	ip := realIP(r)
	sess, _ := sessOf(r)
	u, _ := userOf(r)
	p := s.authPage(r)
	p.CSRF, p.Username = sess.CSRF, u.Username
	att, out := s.auth.begin(ip, u.Username)
	if !out.ok {
		s.tooMany(w, r, "admin_enrol", p, ip, out)
		return
	}
	codes, err := s.authn.ConfirmEnrol(u.ID, r.PostFormValue("code"), u.Username, ip)
	if err != nil {
		att.end(errors.Is(err, authn.ErrBadCode), false)
		if errors.Is(err, authn.ErrBadCode) || errors.Is(err, authn.ErrAlreadyEnrolled) {
			sec, berr := s.authn.BeginEnrol(u.ID)
			if berr == nil {
				p.Secret, p.URI = groupSecret(authn.EncodeSecret(sec)), authn.OTPAuthURI(s.authn.Issuer(), u.Username, sec)
			}
			p.Error = "That code is not right. Check the clock on your phone and try the next code."
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "admin_enrol", p)
			return
		}
		s.storeError(w, err)
		return
	}
	att.end(false, true)
	// Enrolment finished the sign-in: a new session id, with both factors.
	val, nsess, err := s.authn.Rotate(sess.ID, true)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.setSessionCookie(w, val)
	p.Done, p.Codes = true, codes
	p.CSRF = nsess.CSRF
	s.render(w, "admin_enrol", p)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess, ok := sessOf(r); ok {
		s.authn.DeleteSession(sess.ID)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// storeError answers a failure that is not the caller's fault.
func (s *Server) storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrUnavailable) || errors.Is(err, store.ErrPersist) {
		http.Error(w, "the database is unavailable: try again in a moment", http.StatusServiceUnavailable)
		return
	}
	s.log.Error("sign-in", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
