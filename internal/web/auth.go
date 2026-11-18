package web

import (
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

const (
	loginWindow     = 15 * time.Minute
	loginPerIP      = 5  // failed attempts per client address (IPv6: per /64) per window
	loginPerUser    = 10 // failed attempts per account name per window; more are slowed, not refused
	loginGlobal     = 30 // failed attempts in a window from addresses that never signed in; more are slowed
	loginPerKnown   = 20 // failed attempts per known browser or phone per window
	loginHalfPer    = 6  // password-accepted-but-unfinished sign-ins per user (and per address) per window
	reauthPer       = 5  // failed re-checks (password or code on a form) per user, per session and per address
	loginLimiterCap = 5000
	trustFor        = 7 * 24 * time.Hour // how long a successful sign-in keeps its address trusted
	// maxSlowed is how many slowed (over-limit) attempts per account may wait at once;
	// more are told to come back.
	maxSlowed = 2
)

// adminAuth guards the credential checks of the admin pages and the API.
//
// Every attempt to prove a credential (password, authenticator code, recovery
// code) first reserves a slot, per client address and per account name, and
// counts as a failure while it is in flight, so parallel requests cannot all
// pass a check made before any of them finished. Only a completed sign-in
// (both factors) clears the failures: a correct password alone does not, or an
// attacker holding the password could guess authenticator codes forever by
// signing in again between guesses.
//
// Nobody can lock the owner out:
//   - A browser or phone that signed in fully before (a "known" value, see
//     authn/known.go) is exempt from the stranger limits (global and per
//     account) and has a password-hash slot of its own. Its failures are
//     counted per known value, so a stolen value gets 20 tries per window.
//   - Strangers are refused only per client address (IPv6: per /64). Past the
//     per-account and global limits they are slowed instead: each attempt
//     waits longer, and only a couple per account may wait at once.
//   - Addresses that signed in fully lately are exempt from the global limit.
//
// A name that does not exist is limited exactly like one that does.
//
// Password hashing (PBKDF2, 600k rounds, about 0.3 s) runs one at a time per
// class (known, stranger); a few requests may wait for the slot and the rest
// are told to come back (429).
type adminAuth struct {
	s *Server

	semU, semK chan struct{} // the hash slot of strangers and of known callers

	// verifyHash is settings.VerifyPassword; tests replace it to count runs.
	verifyHash func(hash, pass string) bool

	dummyOnce sync.Once
	dummy     string // a valid hash nobody knows the password of

	mu       sync.Mutex
	waitingU int                  // checks waiting for a stranger's slot
	waitingK int                  // checks waiting for a known caller's slot
	login    tracker              // all sign-in attempts, by address
	user     tracker              // all sign-in attempts, by account name
	known    tracker              // sign-in attempts of known browsers, by known id
	half     tracker              // password accepted, second factor pending: by user and by address
	reauth   tracker              // re-checks on forms, by user, session and address
	slowed   map[string]int       // account name -> attempts waiting out a delay
	okIP     map[string]time.Time // address -> last full sign-in (exempt from the global limit)
	okUser   map[string]time.Time // name|address -> last full sign-in (exempt from the account limit)
	warned   map[string]time.Time // address -> window start already audited
	alerted  map[string]time.Time // user -> last "password accepted" alert
}

// maxWaiters is how many slow checks may queue for a hash slot; waitSlot is how
// long a stranger waits for it (known callers wait twice as long).
const (
	maxWaiters = 4
	waitSlot   = 4 * time.Second
)

// tracker counts failed attempts per key, plus attempts in flight. It holds
// at most loginLimiterCap keys; past that the quietest keys are dropped.
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
	return &adminAuth{s: s, semU: make(chan struct{}, 1), semK: make(chan struct{}, 1), verifyHash: settings.VerifyPassword,
		login: newTracker(loginPerIP, loginGlobal), user: newTracker(loginPerUser, 0), known: newTracker(loginPerKnown, 0),
		half: newTracker(loginHalfPer, 0), reauth: newTracker(reauthPer, 0), slowed: map[string]int{},
		okIP: map[string]time.Time{}, okUser: map[string]time.Time{}, warned: map[string]time.Time{}, alerted: map[string]time.Time{}}
}

// addrKey is the key of a client address: IPv6 clients are grouped by /64 (one
// customer gets a whole /64, so per-address limits on the full address mean
// nothing); IPv4 as it is.
func addrKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
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

// count returns the failures plus attempts in flight for key.
func (t *tracker) count(key string, now time.Time) int {
	if ts, ok := t.perIP[key]; ok {
		ts = prune(ts, now)
		if len(ts) == 0 {
			delete(t.perIP, key)
		} else {
			t.perIP[key] = ts
		}
		return len(ts) + t.inflight[key]
	}
	return t.inflight[key]
}

// reserve claims an attempt slot for key, counting in-flight attempts as
// failures. It reports whether the attempt may go on, and otherwise for how
// long to wait. The global figure is not enforced here (see pressure).
// The caller holds the auth mutex. No entry is made for the key: only
// failures (release) and in-flight counts do that.
func (t *tracker) reserve(key string, now time.Time) (bool, time.Duration) {
	t.all = prune(t.all, now)
	if n := t.count(key, now); n >= t.perLimit {
		if ts := t.perIP[key]; len(ts) > 0 {
			return false, loginWindow - now.Sub(ts[0])
		}
		return false, 5 * time.Second
	}
	t.inflight[key]++
	t.allInflight++
	return true, 0
}

// pressure is how many failed or running attempts count towards the global limit over it.
func (t *tracker) pressure(now time.Time) int {
	t.all = prune(t.all, now)
	if t.globalLimit == 0 {
		return -1
	}
	return len(t.all) + t.allInflight - t.globalLimit
}

// release ends a reservation. Failed attempts stay on the books; others
// (success, or a check that never ran) are dropped, and a success also clears
// the key's earlier failures. Nothing is added for an attempt that did not fail.
func (t *tracker) release(key string, now time.Time, failed, success bool) {
	if t.inflight[key]--; t.inflight[key] <= 0 {
		delete(t.inflight, key)
	}
	t.allInflight--
	switch {
	case failed:
		t.perIP[key] = append(prune(t.perIP[key], now), now)
		t.all = append(prune(t.all, now), now)
		t.evict()
	case success:
		delete(t.perIP, key)
	}
}

// evict drops the keys that were quiet the longest once there are too many.
func (t *tracker) evict() {
	if len(t.perIP) <= loginLimiterCap {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(t.perIP))
	for k, ts := range t.perIP {
		last := time.Time{}
		if len(ts) > 0 {
			last = ts[len(ts)-1]
		}
		all = append(all, kv{k, last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, x := range all[:len(all)-loginLimiterCap*9/10] {
		if t.inflight[x.k] == 0 {
			delete(t.perIP, x.k)
		}
	}
}

// outcome of a refused, slowed or busy credential check.
type outcome struct {
	ok      bool
	limited bool          // refused by the failure limit
	busy    bool          // refused because the pbkdf2 slot stayed busy
	known   bool          // the caller is a known browser or phone
	wait    time.Duration // for Retry-After
	delay   time.Duration // the attempt may go on after this pause
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
	a             *adminAuth
	addr, user    string
	kid           string // known id when the caller is a known browser or phone
	skipUser      bool
	slowedAccount bool
	ended         bool
}

// begin reserves a slot for a credential check by ip against the named
// account. kid is the known id of the caller's known-browser value when it
// belongs to that account ("" = a stranger). The caller must end the attempt
// exactly once, and pause for out.delay before going on.
func (a *adminAuth) begin(ip, name, kid string) (*attempt, outcome) {
	user := userKey(name)
	addr := addrKey(ip)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if kid != "" {
		if ok, _ := a.known.reserve(kid, now); ok {
			return &attempt{a: a, addr: addr, user: user, kid: kid, skipUser: true}, outcome{ok: true, known: true}
		}
		// Too many wrong tries from this known value: it counts as a stranger.
	}
	trustedIP := false
	if at, ok := a.okIP[addr]; ok && now.Sub(at) < trustFor {
		trustedIP = true
	}
	trustedUser := false
	if at, ok := a.okUser[user+"|"+addr]; ok && now.Sub(at) < trustFor {
		trustedUser = true
	}
	ok, wait := a.login.reserve(addr, now)
	if !ok {
		return nil, outcome{limited: true, wait: wait}
	}
	att := &attempt{a: a, addr: addr, user: user}
	var delay time.Duration
	if over := a.login.pressure(now); over >= 0 && !trustedIP {
		// Many strangers are failing at once: slow everybody down, refuse nobody.
		delay = minDur(3*time.Second, time.Duration(over+1)*200*time.Millisecond)
	}
	if user == "" || trustedUser {
		att.skipUser = true
		return att, outcome{ok: true, delay: delay}
	}
	n := a.user.count(user, now)
	if n >= a.user.perLimit {
		// Somebody is guessing at this account. Slow the attempts that come from
		// strangers instead of refusing them, and let only a few wait at once.
		if a.slowed[user] >= maxSlowed {
			a.login.release(addr, now, false, false)
			return nil, outcome{limited: true, wait: 5 * time.Second}
		}
		a.slowed[user]++
		att.slowedAccount = true
		delay = maxDur(delay, minDur(5*time.Second, time.Duration(n-a.user.perLimit+1)*time.Second))
	}
	a.user.inflight[user]++
	a.user.allInflight++
	return att, outcome{ok: true, delay: delay}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// end settles the attempt. failed counts against the address and the account
// (or the known value); success (a completed sign-in) clears them and trusts the
// address; neither leaves the books as they were (a correct password that
// still needs a second factor).
func (t *attempt) end(failed, success bool) {
	if t.ended {
		return
	}
	t.ended = true
	a := t.a
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if t.kid != "" {
		a.known.release(t.kid, now, failed, success)
		if success {
			a.okIP[t.addr] = now
		}
		return
	}
	a.login.release(t.addr, now, failed, success)
	if t.slowedAccount {
		if a.slowed[t.user]--; a.slowed[t.user] <= 0 {
			delete(a.slowed, t.user)
		}
	}
	if !t.skipUser {
		a.user.release(t.user, now, failed, success)
	} else if success && t.user != "" {
		delete(a.user.perIP, t.user)
	}
	if success {
		a.okIP[t.addr] = now
		if t.user != "" {
			a.okUser[t.user+"|"+t.addr] = now
		}
		if len(a.okIP) > 1000 {
			trimTimes(a.okIP)
		}
		if len(a.okUser) > 1000 {
			trimTimes(a.okUser)
		}
	}
}

// trimTimes drops the oldest tenth of a map of times.
func trimTimes(m map[string]time.Time) {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(m))
	for k, t := range m {
		all = append(all, kv{k, t})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, x := range all[:len(all)/10+1] {
		delete(m, x.k)
	}
}

// pause waits out a delay (or until the client goes away).
func pause(r *http.Request, d time.Duration) {
	if d <= 0 {
		return
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
	case <-r.Context().Done():
	}
}

// halfAllowed reports whether another password-accepted-but-unfinished
// sign-in may start for the user from this address, and records it. Known
// callers are not limited.
func (a *adminAuth) halfAllowed(userID, ip string, known bool) bool {
	if known {
		return true
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	uk, ak := "u:"+userID, "a:"+addrKey(ip)
	if a.half.count(uk, now) >= a.half.perLimit || a.half.count(ak, now) >= a.half.perLimit {
		return false
	}
	// Record the event (as a "failure" of the half tracker, which only counts).
	a.half.perIP[uk] = append(prune(a.half.perIP[uk], now), now)
	a.half.perIP[ak] = append(prune(a.half.perIP[ak], now), now)
	a.half.evict()
	return true
}

// acquire takes a password-hash slot: at once if free, else after a short wait
// if fewer than maxWaiters are already waiting. Known callers have a slot of
// their own, so strangers cannot starve them. It reports whether it got it.
func (a *adminAuth) acquire(known bool) (release func(), ok bool) {
	sem, waiting, wait := a.semU, &a.waitingU, waitSlot
	if known {
		sem, waiting, wait = a.semK, &a.waitingK, 2*waitSlot
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
	}
	a.mu.Lock()
	if *waiting >= maxWaiters {
		a.mu.Unlock()
		return nil, false
	}
	*waiting++
	a.mu.Unlock()
	tm := time.NewTimer(wait)
	defer tm.Stop()
	defer func() { a.mu.Lock(); *waiting--; a.mu.Unlock() }()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-tm.C:
		return nil, false
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
// the same time. busy means the hash slot stayed taken.
func (a *adminAuth) password(u store.AdminUser, exists bool, pass string, known bool) (ok, busy bool) {
	hash := a.dummyHash()
	if exists && u.PassHash != "" {
		hash = u.PassHash
	}
	release, got := a.acquire(known)
	if !got {
		return false, true
	}
	good := a.verifyHash(hash, pass)
	release()
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
		trimTimes(a.warned)
	}
	a.mu.Unlock()
	a.s.authn.AuditNow("", ip, "sign-in blocked: too many failed attempts from "+ip)
}

// confirm re-checks the signed-in user before a sensitive form action: the
// current password and, when needCode, a current authenticator code. Failures
// are counted per user, per session and per address; five on one session end
// that session. It returns "" when the user is confirmed, else a message.
func (s *Server) confirm(r *http.Request, needCode bool) string {
	u, ok := userOf(r)
	sess, _ := sessOf(r)
	pass := r.PostFormValue("current_password")
	if !ok || pass == "" {
		return "the current password is missing or wrong"
	}
	if needCode && strings.TrimSpace(r.PostFormValue("totp")) == "" {
		return "enter the 6-digit code from your authenticator app"
	}
	a := s.auth
	ip := realIP(r)
	keys := []string{"u:" + u.ID, "s:" + sess.ID, "a:" + addrKey(ip)}
	now := time.Now()
	a.mu.Lock()
	var held []string
	for _, k := range keys {
		if ok, _ := a.reauth.reserve(k, now); !ok {
			for _, h := range held {
				a.reauth.release(h, now, false, false)
			}
			a.mu.Unlock()
			return "too many wrong attempts; wait a few minutes"
		}
		held = append(held, k)
	}
	a.mu.Unlock()
	finish := func(failed bool) {
		a.mu.Lock()
		for _, k := range keys {
			a.reauth.release(k, time.Now(), failed, false)
		}
		kill := failed && a.reauth.count("s:"+sess.ID, time.Now()) >= reauthPer
		a.mu.Unlock()
		if kill {
			s.authn.DeleteSession(sess.ID)
			s.authn.AuditNow(u.Username, ip, "session ended after repeated wrong confirmations")
		}
	}
	good, busy := a.password(u, true, pass, true)
	if busy {
		finish(false)
		return "the server is busy; try again in a moment"
	}
	if !good {
		finish(true)
		return "the current password is missing or wrong"
	}
	if needCode {
		switch err := s.authn.VerifyTOTPFrom(u.ID, r.PostFormValue("totp"), ip); {
		case err == nil:
		case errors.Is(err, authn.ErrTOTPBackoff), errors.Is(err, authn.ErrTOTPLocked):
			finish(false)
			return "too many wrong authenticator codes; wait, or use a recovery code at sign-in"
		case errors.Is(err, authn.ErrBadCode):
			finish(true)
			return "the authenticator code is not right (each code works once)"
		default:
			finish(false)
			return "something went wrong; try again"
		}
	}
	finish(false)
	return ""
}

// checkPassword verifies the "current password" a form carries, against the
// signed-in user's own password (see confirm).
func (s *Server) checkPassword(r *http.Request, pass string) bool {
	return s.confirm(r, false) == ""
}
