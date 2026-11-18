package web

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"ispbill/internal/settings"
	"ispbill/internal/store"
)

const (
	loginWindow     = 15 * time.Minute
	loginPerIP      = 5  // failed attempts per client address per window
	loginPerUser    = 10 // failed attempts per account name per window
	loginGlobal     = 30 // failed attempts in a window from addresses that never signed in
	loginLimiterCap = 5000
	trustFor        = 7 * 24 * time.Hour // how long a successful sign-in keeps its address trusted
)

// adminAuth guards the credential checks of the admin pages and the API.
//
// Every attempt to prove a credential (password, authenticator code, recovery
// code, setup code) first reserves a slot, per client address and per account
// name, and counts as a failure while it is in flight, so parallel requests
// cannot all pass a check made before any of them finished. Only a completed
// sign-in (both factors) clears the failures: a correct password alone does
// not, or an attacker holding the password could guess authenticator codes
// forever by signing in again between guesses.
//
// The per-account limit blocks addresses that have not signed in as that
// account lately; an address that has keeps working, so somebody hammering the
// owner's name cannot lock the owner out of the office. A name that does not
// exist is limited exactly like one that does.
//
// Password hashing (PBKDF2, 600k rounds, about 0.3 s) runs one at a time; a
// few requests may wait for the slot and the rest are told to come back (429),
// so a flood cannot exhaust the CPU.
type adminAuth struct {
	s *Server

	sem chan struct{}

	// verifyHash is settings.VerifyPassword; tests replace it to count runs.
	verifyHash func(hash, pass string) bool

	dummyOnce sync.Once
	dummy     string // a valid hash nobody knows the password of

	mu      sync.Mutex
	waiting int                  // checks waiting for the pbkdf2 slot
	login   tracker              // all sign-in attempts, by address
	user    tracker              // all sign-in attempts, by account name
	reauth  tracker              // "current password" fields on forms
	okIP    map[string]time.Time // address -> last full sign-in (exempt from the global limit)
	okUser  map[string]time.Time // name|address -> last full sign-in (exempt from the account limit)
	warned  map[string]time.Time // address -> window start already audited
}

// maxWaiters is how many slow checks may queue for the pbkdf2 slot; waitSlot
// is how long one waits before it is told to come back.
const (
	maxWaiters = 4
	waitSlot   = 4 * time.Second
)

// tracker counts failed attempts per key, plus attempts in flight.
type tracker struct {
	perIP       map[string][]time.Time
	inflight    map[string]int
	all         []time.Time
	allInflight int
	perLimit    int
	globalLimit int // 0 = none
}

func newTracker(per, global int) tracker {
	return tracker{perIP: map[string][]time.Time{}, inflight: map[string]int{}, perLimit: per, globalLimit: global}
}

func newAdminAuth(s *Server) *adminAuth {
	return &adminAuth{s: s, sem: make(chan struct{}, 1), verifyHash: settings.VerifyPassword,
		login: newTracker(loginPerIP, loginGlobal), user: newTracker(loginPerUser, 0), reauth: newTracker(loginPerIP, 0),
		okIP: map[string]time.Time{}, okUser: map[string]time.Time{}, warned: map[string]time.Time{}}
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

// reserve claims an attempt slot for key, counting in-flight attempts as
// failures. It reports whether the attempt may go on, and otherwise for how
// long to wait. trusted callers are exempt from the global limit.
// The caller holds the auth mutex.
func (t *tracker) reserve(ip string, now time.Time, trusted bool) (bool, time.Duration) {
	t.perIP[ip] = prune(t.perIP[ip], now)
	t.all = prune(t.all, now)
	if n := len(t.perIP[ip]) + t.inflight[ip]; n >= t.perLimit {
		if len(t.perIP[ip]) > 0 {
			return false, loginWindow - now.Sub(t.perIP[ip][0])
		}
		return false, 5 * time.Second
	}
	if t.globalLimit > 0 && !trusted && len(t.all)+t.allInflight >= t.globalLimit {
		if len(t.all) > 0 {
			return false, loginWindow - now.Sub(t.all[0])
		}
		return false, 5 * time.Second
	}
	t.inflight[ip]++
	t.allInflight++
	return true, 0
}

// release ends a reservation. failed attempts stay on the books; others
// (success, or a check that never ran) are dropped, and a success also clears
// the key's earlier failures.
func (t *tracker) release(ip string, now time.Time, failed, success bool) {
	if t.inflight[ip]--; t.inflight[ip] <= 0 {
		delete(t.inflight, ip)
	}
	t.allInflight--
	if failed {
		t.perIP[ip] = append(prune(t.perIP[ip], now), now)
		t.all = append(prune(t.all, now), now)
		if len(t.perIP) > loginLimiterCap {
			t.perIP = map[string][]time.Time{ip: t.perIP[ip]}
		}
	} else if success {
		delete(t.perIP, ip)
	}
}

// outcome of a refused or busy credential check.
type outcome struct {
	ok      bool
	limited bool          // refused by the failure limit
	busy    bool          // refused because the pbkdf2 slot stayed busy
	wait    time.Duration // for Retry-After
}

func userKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// attempt is one reserved credential check.
type attempt struct {
	a        *adminAuth
	ip, user string
	skipUser bool
	ended    bool
}

// begin reserves a slot for a credential check by ip against the named
// account. The caller must end the attempt exactly once.
func (a *adminAuth) begin(ip, name string) (*attempt, outcome) {
	user := userKey(name)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	trustedIP := false
	if at, ok := a.okIP[ip]; ok && now.Sub(at) < trustFor {
		trustedIP = true
	}
	trustedUser := false
	if at, ok := a.okUser[user+"|"+ip]; ok && now.Sub(at) < trustFor {
		trustedUser = true
	}
	ok, wait := a.login.reserve(ip, now, trustedIP)
	if !ok {
		return nil, outcome{limited: true, wait: wait}
	}
	att := &attempt{a: a, ip: ip, user: user}
	if user == "" || trustedUser {
		att.skipUser = true
		return att, outcome{ok: true}
	}
	if ok, wait := a.user.reserve(user, now, false); !ok {
		a.login.release(ip, now, false, false)
		return nil, outcome{limited: true, wait: wait}
	}
	return att, outcome{ok: true}
}

// end settles the attempt. failed counts against the address and the account;
// success (a completed sign-in) clears both and trusts the address; neither
// leaves the books as they were (a correct password that still needs a second
// factor).
func (t *attempt) end(failed, success bool) {
	if t.ended {
		return
	}
	t.ended = true
	a := t.a
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.login.release(t.ip, now, failed, success)
	if !t.skipUser {
		a.user.release(t.user, now, failed, success)
	} else if success && t.user != "" {
		delete(a.user.perIP, t.user)
	}
	if success {
		a.okIP[t.ip] = now
		if t.user != "" {
			a.okUser[t.user+"|"+t.ip] = now
		}
		if len(a.okIP) > 1000 {
			a.okIP = map[string]time.Time{t.ip: now}
		}
		if len(a.okUser) > 1000 {
			a.okUser = map[string]time.Time{t.user + "|" + t.ip: now}
		}
	}
}

// acquire takes the pbkdf2 slot: at once if free, else after a short wait if
// fewer than maxWaiters are already waiting. It reports whether it got it.
func (a *adminAuth) acquire() bool {
	select {
	case a.sem <- struct{}{}:
		return true
	default:
	}
	a.mu.Lock()
	if a.waiting >= maxWaiters {
		a.mu.Unlock()
		return false
	}
	a.waiting++
	a.mu.Unlock()
	tm := time.NewTimer(waitSlot)
	defer tm.Stop()
	defer func() { a.mu.Lock(); a.waiting--; a.mu.Unlock() }()
	select {
	case a.sem <- struct{}{}:
		return true
	case <-tm.C:
		return false
	}
}

// dummyHash is a real hash of a random password: unknown and disabled users
// are checked against it, so the time taken does not tell whether the name
// exists.
func (a *adminAuth) dummyHash() string {
	a.dummyOnce.Do(func() {
		pw, _ := settings.RandomPassword()
		if h, err := settings.HashPassword(pw); err == nil {
			a.dummy = h
		}
	})
	return a.dummy
}

// password reports whether pass is the password of u. exists=false (no such
// user), a disabled user and a user without a password never succeed, but cost
// the same time. busy means the pbkdf2 slot stayed taken.
func (a *adminAuth) password(u store.AdminUser, exists bool, pass string) (ok, busy bool) {
	hash := a.dummyHash()
	if exists && u.PassHash != "" {
		hash = u.PassHash
	}
	if !a.acquire() {
		return false, true
	}
	good := a.verifyHash(hash, pass)
	<-a.sem
	return good && exists && !u.Disabled && u.PassHash != "", false
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
	a.s.authn.AuditNow("", ip, "sign-in blocked: too many failed attempts from "+ip)
}

// checkPassword verifies the "current password" a form carries, against the
// signed-in user's own password. Its failures are counted apart from sign-ins,
// so a mistyped field never blocks the admin's own page loads. An empty
// password is refused without counting.
func (s *Server) checkPassword(r *http.Request, pass string) bool {
	u, ok := userOf(r)
	if pass == "" || !ok {
		return false
	}
	a := s.auth
	ip := realIP(r)
	now := time.Now()
	a.mu.Lock()
	ok2, _ := a.reauth.reserve(ip, now, true)
	a.mu.Unlock()
	if !ok2 {
		return false
	}
	good, busy := a.password(u, true, pass)
	a.mu.Lock()
	a.reauth.release(ip, time.Now(), !good && !busy, good)
	a.mu.Unlock()
	return good
}
