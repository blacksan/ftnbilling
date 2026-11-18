package web

import "time"

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
