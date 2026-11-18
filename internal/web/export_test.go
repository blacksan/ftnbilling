package web

import "time"

// Hooks for the external tests in this directory.

// SetHashVerifier replaces the pbkdf2 check (to count and slow it down).
func (s *Server) SetHashVerifier(fn func(hash, pass string) bool) { s.auth.verifyHash = fn }

// AuthFlush forgets remembered logins.
func (s *Server) AuthFlush() { s.auth.Flush() }

// AuthRemember stores a remembered login as the slow path would after a check.
func (s *Server) AuthRemember(user, pass, hash string) {
	s.auth.remember(s.auth.mac(user, pass, hash), time.Now())
}

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
