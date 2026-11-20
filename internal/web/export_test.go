package web

import (
	"net/http"
	"time"

	"ispbill/internal/store"
)

// Hooks for the external tests in this directory.

// SetHashVerifier replaces the pbkdf2 check (to count and slow it down).
func (s *Server) SetHashVerifier(fn func(hash, pass string) bool) { s.auth.verifyHash = fn }

// AuthInjectFailures records n failed logins from ip (and so globally).
func (s *Server) AuthInjectFailures(ip string, n int) {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	for i := 0; i < n; i++ {
		s.auth.login.all = append(s.auth.login.all, time.Now())
	}
	_ = ip
}

// SetServerToolTimeout shortens the server-side tool timeout; it returns a restore func.
func SetServerToolTimeout(d time.Duration) func() {
	old := serverToolTimeout
	serverToolTimeout = d
	return func() { serverToolTimeout = old }
}

// ResetRegLimits forgets recent registration attempts (the limiters are process-wide).
func ResetRegLimits() {
	regIPRL, regIPRLOpen, regPhoneRL, regMACRL = newLimiter(40, 10*time.Minute), newLimiter(4, 10*time.Minute), newLimiter(3, time.Hour), newLimiter(3, time.Hour)
	regGetRL, regGetRLOpen = newLimiter(240, time.Minute), newLimiter(20, time.Minute)
}

// SetCaptiveMAC makes the captive page "identify" a device (what a future router change would allow); it returns a restore func.
func SetCaptiveMAC(mac string) func() {
	old := captiveMACOf
	captiveMACOf = func(*Server, *http.Request, store.Site) string { return mac }
	return func() { captiveMACOf = old }
}

// ResetToolLimiter forgets recent tool runs, so a test can exceed 30 a minute.
func (s *Server) ResetToolLimiter() { s.toolRL = newLimiter(30, time.Minute) }

// SetSecurityAlertSink diverts the owners' security alerts to fn.
func (s *Server) SetSecurityAlertSink(fn func(string)) { s.alertSink = fn }

// AuthInjectAccountFailures records n failed sign-ins at the named account from
// strangers (and so globally), as a guessing campaign would have.
func (s *Server) AuthInjectAccountFailures(name string, n int) {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	k := userKey(name)
	for i := 0; i < n; i++ {
		s.auth.user.perIP[k] = append(s.auth.user.perIP[k], time.Now())
		s.auth.login.all = append(s.auth.login.all, time.Now())
	}
}

// ResetPayLimiters forgets recent /pay attempts (the limiters are package-wide
// and every test arrives from 127.0.0.1).
func ResetPayLimiters() {
	payIPRL, payFindRL = newLimiter(60, 10*time.Minute), newLimiter(120, time.Minute)
	payIPRLOpen, payFindRLOpen = newLimiter(8, 10*time.Minute), newLimiter(15, time.Minute)
	payAcctRL = newLimiter(4, 10*time.Minute)
}
