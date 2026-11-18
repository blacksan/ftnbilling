package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
// as an HMAC, under a per-process random key, of the user, the password and
// the stored hash it was verified against (a password change therefore
// invalidates it, even if a slow check was running at that moment). Slow
// checks run one at a time, a few may wait for their turn, and the rest are
// told to come back (429).
//
// Failed attempts are limited per client address (and, for addresses that
// have never logged in, globally). An attempt is reserved atomically before
// any hashing starts and counts as a failure while it is in flight, so
// parallel requests cannot all pass a check made before any of them finished.
type adminAuth struct {
	s *Server

	key []byte
	sem chan struct{}

	// verifyHash is settings.VerifyPassword; tests replace it to count runs.
	verifyHash func(hash, pass string) bool

	mu      sync.Mutex
	cache   map[string]time.Time // hex HMAC(user NUL pass NUL hash) -> expiry
	waiting int                  // checks waiting for the pbkdf2 slot
	login   tracker              // Basic auth attempts
	reauth  tracker              // "current password" fields on forms
	okIP    map[string]time.Time // address -> last success (exempt from the global limit)
	warned  map[string]time.Time // ip -> window start already audited
}

// maxWaiters is how many slow checks may queue for the pbkdf2 slot; waitSlot
// is how long one waits before it is told to come back.
const (
	maxWaiters = 4
	waitSlot   = 4 * time.Second
)

// tracker counts failed attempts per address, plus attempts in flight.
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
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return &adminAuth{s: s, key: k, sem: make(chan struct{}, 1), cache: map[string]time.Time{}, verifyHash: settings.VerifyPassword,
		login: newTracker(loginPerIP, loginGlobal), reauth: newTracker(loginPerIP, 0),
		okIP: map[string]time.Time{}, warned: map[string]time.Time{}}
}

func (a *adminAuth) mac(user, pass, hash string) string {
	m := hmac.New(sha256.New, a.key)
	for _, p := range []string{user, pass, hash} {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

func (a *adminAuth) cached(sum string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.cache[sum]
	if ok && !now.Before(exp) {
		delete(a.cache, sum)
		return false
	}
	return ok
}

func (a *adminAuth) remember(sum string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cache) >= authCacheMax {
		a.cache = map[string]time.Time{}
	}
	a.cache[sum] = now.Add(authCacheTTL)
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

// reserve claims an attempt slot for ip, counting in-flight attempts as
// failures. It reports whether the attempt may go on, and otherwise for how
// long ip must wait. trusted addresses are exempt from the global limit.
// The caller holds a.mu.
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
// the address's earlier failures.
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

// outcome of one credential check.
type outcome struct {
	ok      bool
	limited bool          // refused by the failure limit
	busy    bool          // refused because the pbkdf2 slot stayed busy
	wait    time.Duration // for Retry-After
}

// hashNow returns the stored password hash ("" = ADMIN_PASS from the environment).
func (a *adminAuth) hashNow() string {
	if a.s.setm != nil {
		return a.s.setm.AdminHash()
	}
	return ""
}

// check verifies user and pass for the client ip against tracker t. An empty
// password is refused at once and counts for nothing. A cache hit succeeds
// without touching the limits.
func (a *adminAuth) check(user, pass, ip string, t *tracker) outcome {
	if pass == "" {
		return outcome{}
	}
	live := a.s.live()
	userOK := safeEq(user, live.AdminUser)
	hash := a.hashNow()
	var sum string
	if hash != "" {
		sum = a.mac(user, pass, hash)
		if userOK && a.cached(sum, time.Now()) {
			a.noteOK(ip, time.Now())
			return outcome{ok: true}
		}
	}
	now := time.Now()
	a.mu.Lock()
	trusted := false
	if at, ok := a.okIP[ip]; ok && now.Sub(at) < authCacheTTL {
		trusted = true
	}
	ok, wait := t.reserve(ip, now, trusted)
	a.mu.Unlock()
	if !ok {
		return outcome{limited: true, wait: wait}
	}
	done := func(failed, success bool) {
		a.mu.Lock()
		t.release(ip, time.Now(), failed, success)
		if success {
			a.okIP[ip] = time.Now()
			if len(a.okIP) > 1000 {
				a.okIP = map[string]time.Time{ip: time.Now()}
			}
		}
		a.mu.Unlock()
	}
	if hash == "" {
		// No database password yet: ADMIN_PASS from the environment, which
		// must be a real one (config refuses an empty or "admin" one at start).
		good := a.s.cfg.CheckAdminPass() == nil && safeEq(pass, a.s.cfg.AdminPass) && userOK
		done(!good, good)
		return outcome{ok: good}
	}
	if !userOK {
		done(true, false)
		return outcome{}
	}
	// The slow path: one pbkdf2 at a time, a few may wait, the rest come back later.
	if !a.acquire() {
		done(false, false)
		return outcome{busy: true, wait: 2 * time.Second}
	}
	// Another request may have verified the same login while this one waited.
	if a.cached(sum, time.Now()) {
		<-a.sem
		done(false, true)
		return outcome{ok: true}
	}
	good := a.verifyHash(hash, pass)
	<-a.sem
	if good {
		a.remember(sum, time.Now())
	}
	done(!good, good)
	return outcome{ok: good}
}

// noteOK records that ip has presented the right password lately.
func (a *adminAuth) noteOK(ip string, now time.Time) {
	a.mu.Lock()
	a.okIP[ip] = now
	if len(a.okIP) > 1000 {
		a.okIP = map[string]time.Time{ip: now}
	}
	a.mu.Unlock()
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

// checkPassword verifies the "current password" a form carries, against the
// same source as the login. Its failures are counted apart from logins, so a
// mistyped field never blocks the admin's own page loads. An empty password
// is refused without counting.
func (s *Server) checkPassword(r *http.Request, pass string) bool {
	return s.auth.check(s.live().AdminUser, pass, realIP(r), &s.auth.reauth).ok
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
	ip := realIP(r)
	out := s.auth.check(u, p, ip, &s.auth.login)
	switch {
	case out.ok:
		return true
	case out.limited:
		s.auth.auditBlocked(ip, time.Now())
		w.Header().Set("Retry-After", strconv.Itoa(int(out.wait.Seconds())+1))
		http.Error(w, "too many wrong passwords: try again later", http.StatusTooManyRequests)
	case out.busy:
		w.Header().Set("Retry-After", strconv.Itoa(int(out.wait.Seconds())+1))
		http.Error(w, "the server is busy checking passwords: try again in a moment", http.StatusTooManyRequests)
	default:
		challenge()
	}
	return false
}
