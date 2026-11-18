package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ispbill/internal/settings"
	"ispbill/internal/store"
)

const (
	authCacheTTL    = 12 * time.Hour
	authCacheMax    = 16
	loginWindow     = 15 * time.Minute
	loginPerIP      = 5
	loginGlobal     = 30
	loginLimiterCap = 5000
)

// adminAuth checks the admin's Basic credentials.
//
// The password is the pbkdf2 hash in the database once one is set (the
// ADMIN_PASS environment value is then ignored); until then ADMIN_PASS applies.
// pbkdf2 at 600k rounds takes about 0.3 s on one core, and Basic auth sends the
// password with every request, so a verified login is remembered for 12 hours
// as an HMAC under a per-process random key, and slow checks run one at a time.
// Failures are rate limited before any hashing happens.
type adminAuth struct {
	s *Server

	key []byte
	sem chan struct{}

	mu      sync.Mutex
	cache   map[string]time.Time // hex HMAC(user NUL pass) -> expiry
	ipFails map[string][]time.Time
	allFail []time.Time
	warned  map[string]time.Time // ip -> window start already audited
}

func newAdminAuth(s *Server) *adminAuth {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return &adminAuth{s: s, key: k, sem: make(chan struct{}, 1), cache: map[string]time.Time{},
		ipFails: map[string][]time.Time{}, warned: map[string]time.Time{}}
}

func (a *adminAuth) mac(user, pass string) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(user))
	m.Write([]byte{0})
	m.Write([]byte(pass))
	return m.Sum(nil)
}

func (a *adminAuth) cached(sum []byte, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	hit := false
	for k, exp := range a.cache {
		if !now.Before(exp) {
			delete(a.cache, k)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(k), []byte(fmt.Sprintf("%x", sum))) == 1 {
			hit = true
		}
	}
	return hit
}

func (a *adminAuth) remember(sum []byte, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cache) >= authCacheMax {
		a.cache = map[string]time.Time{}
	}
	a.cache[fmt.Sprintf("%x", sum)] = now.Add(authCacheTTL)
}

// Flush forgets every remembered login (after a password change).
func (a *adminAuth) Flush() {
	a.mu.Lock()
	a.cache = map[string]time.Time{}
	a.mu.Unlock()
}

func prune(ts []time.Time, now time.Time) []time.Time {
	keep := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < loginWindow {
			keep = append(keep, t)
		}
	}
	return keep
}

// blocked reports whether ip may not try now, and for how long.
func (a *adminAuth) blocked(ip string, now time.Time) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ipFails[ip] = prune(a.ipFails[ip], now)
	a.allFail = prune(a.allFail, now)
	if len(a.ipFails[ip]) >= loginPerIP {
		return true, loginWindow - now.Sub(a.ipFails[ip][0])
	}
	if len(a.allFail) >= loginGlobal {
		return true, loginWindow - now.Sub(a.allFail[0])
	}
	return false, 0
}

func (a *adminAuth) fail(ip string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ipFails[ip] = append(prune(a.ipFails[ip], now), now)
	a.allFail = append(prune(a.allFail, now), now)
	if len(a.ipFails) > loginLimiterCap {
		a.ipFails = map[string][]time.Time{ip: a.ipFails[ip]}
	}
}

func (a *adminAuth) succeed(ip string) {
	a.mu.Lock()
	delete(a.ipFails, ip)
	a.mu.Unlock()
}

// auditBlocked writes one audit event per client per window.
func (a *adminAuth) auditBlocked(ip string, now time.Time) {
	a.mu.Lock()
	if t, ok := a.warned[ip]; ok && now.Sub(t) < loginWindow {
		a.mu.Unlock()
		return
	}
	a.warned[ip] = now
	if len(a.warned) > 1000 {
		a.warned = map[string]time.Time{ip: now}
	}
	a.mu.Unlock()
	_ = a.s.st.Update(func(d *store.Data) error {
		d.AddEvent(store.Event{Time: now, Kind: "audit", Msg: "admin login blocked: too many wrong passwords from " + ip})
		return nil
	})
}

// verify checks the credentials without rate limiting (the caller does that).
func (a *adminAuth) verify(user, pass string) bool {
	live := a.s.live()
	userOK := safeEq(user, live.AdminUser)
	var hash string
	if a.s.setm != nil {
		hash = a.s.setm.AdminHash()
	}
	if hash == "" {
		// No database password yet: ADMIN_PASS from the environment, which
		// must be a real one (config refuses an empty or "admin" one at start).
		return a.s.cfg.CheckAdminPass() == nil && safeEq(pass, a.s.cfg.AdminPass) && userOK
	}
	now := time.Now()
	sum := a.mac(user, pass)
	if a.cached(sum, now) {
		return userOK
	}
	a.sem <- struct{}{}
	ok := settings.VerifyPassword(hash, pass)
	<-a.sem
	if ok && userOK {
		a.remember(sum, now)
		return true
	}
	return false
}

// checkPassword verifies the "current password" a form carries, against the
// same source as the login. It counts failures like a login does.
func (s *Server) checkPassword(r *http.Request, pass string) bool {
	ip := realIP(r)
	if blocked, _ := s.auth.blocked(ip, time.Now()); blocked {
		return false
	}
	if pass != "" && s.auth.verify(s.live().AdminUser, pass) {
		return true
	}
	s.auth.fail(ip, time.Now())
	return false
}

// authenticate runs the Basic auth check for the admin. It writes the response
// itself when the request is refused and reports whether to continue.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	challenge := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="ispbill admin", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	if !ok {
		challenge()
		return false
	}
	ip, now := realIP(r), time.Now()
	if blocked, wait := s.auth.blocked(ip, now); blocked {
		s.auth.auditBlocked(ip, now)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		http.Error(w, "too many wrong passwords: try again later", http.StatusTooManyRequests)
		return false
	}
	if !s.auth.verify(u, p) {
		s.auth.fail(ip, now)
		challenge()
		return false
	}
	s.auth.succeed(ip)
	return true
}
