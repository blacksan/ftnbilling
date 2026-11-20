// Package webfig is the remote WebFig access: a second HTTP host (WEBFIG_DOMAIN)
// served by the same binary. An owner who passed a fresh password and
// authenticator step-up gets a one-time ticket from the admin; the ticket
// becomes a session cookie bound to exactly one router, and everything else on
// the host is reverse-proxied to that router's WireGuard tunnel address and
// its www port. WebFig itself is never opened on the router's WAN.
package webfig

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Lifetimes and limits.
const (
	TicketTTL   = 60 * time.Second
	IdleTimeout = 30 * time.Minute
	MaxLife     = 2 * time.Hour
	// PerRouterMax is the concurrent proxied requests per router for everything but /jsproxy: WebFig loads its
	// engine, /webfig/list and one file per package (15 to 25) in parallel. RouterOS www max-sessions defaults
	// to 20 and is shared with the server's own REST calls (2 reads + 1 write per router): the bootstrap raises
	// it to 40 on new routers, and 16 here leaves the REST budget room even on an old router with 20.
	PerRouterMax = 16
	TotalMax     = 64 // concurrent proxied requests overall
	// The long-lived /jsproxy calls have their own small pools, so they never take the slots the files need:
	// at most JSProxyPerSession per WebFig session, JSProxyPerRouter per router, JSProxyTotal overall.
	JSProxyPerSession = 4
	JSProxyPerRouter  = 6
	JSProxyTotal      = 24
	SlotWait          = 20 * time.Second // how long a request waits for a free slot before 503
)

// Reasons a session ends (shown in the activity record).
const (
	EndIdle     = "idle for 30 minutes"
	EndMax      = "2 hour limit reached"
	EndReplaced = "replaced by a new WebFig session in the same browser"
	EndOwner    = "ended by an owner"
	EndRouter   = "the router or the user changed"
	EndAdmin    = "the admin session that opened it ended"
)

// Cookie names: the __Host- prefix makes the browser insist on Secure, Path=/
// and no Domain, so the cookie belongs to the WebFig host alone.
const (
	cookieSecure = "__Host-ftn_wf"
	cookiePlain  = "ftn_wf"
)

// Errors of the ticket exchange.
var (
	ErrTicket = errors.New("webfig: ticket invalid, used, expired or from another address")
	ErrRouter = errors.New("webfig: router not available")
	// ErrAddress: the ticket was fine but redeemed from another client address.
	ErrAddress = errors.New("webfig: ticket redeemed from another address")
)

// Target is where a router's WebFig lives: its tunnel address and www port.
type Target struct {
	IP   string
	Port int
	Name string
}

// Addr is "ip:port".
func (t Target) Addr() string { return net.JoinHostPort(t.IP, strconv.Itoa(t.Port)) }

// Options configure a Manager.
type Options struct {
	Domain   string // the WebFig host name, lower case
	Secure   bool   // __Host- cookie names and the Secure attribute
	Tunnel   netip.Prefix
	ServerIP netip.Addr // never a target
	// Resolve gives the router's current target; false when it cannot be opened.
	Resolve func(siteID string) (Target, bool)
	// UserOK says whether the user may still use WebFig (an enabled owner).
	UserOK func(userID string) bool
	// AdminLive says whether the admin session (by id) that opened a WebFig
	// session is still signed in.
	AdminLive func(adminSession string) bool
	// OnOpen and OnEnd write the activity record.
	OnOpen func(s Session)
	OnEnd  func(s Session, reason string, at time.Time)
	// ClientIP is the address of the request's client (the web package's realIP).
	ClientIP func(r *http.Request) string
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error) // tests
	Now      func() time.Time
	// Limits (zero = defaults above).
	PerRouter, Total int
	// Version is the router's RouterOS version (with whatever changes when it is re-bootstrapped), "" if
	// unknown. It is part of the static-file cache key; a change drops the router's cached files.
	Version func(siteID string) string
	// CacheBytes bounds the static-file cache (0 = DefaultCacheBytes, negative = no cache).
	CacheBytes int64
	// SlotWait is how long a request over a limit waits for a free slot (default SlotWait).
	SlotWait time.Duration
}

// Session is one WebFig session: one browser, one router.
type Session struct {
	PublicID     string
	UserID       string
	User         string
	SiteID       string
	IP           string // client address that redeemed the ticket
	AdminSession string // id of the admin session that opened it
	Started      time.Time
	LastSeen     time.Time
	hash         string
	connected    int
}

// Expires is when the session ends if nothing happens: idle or max, the sooner.
func (s Session) Expires() time.Time {
	idle := s.LastSeen.Add(IdleTimeout)
	max := s.Started.Add(MaxLife)
	if max.Before(idle) {
		return max
	}
	return idle
}

type ticket struct {
	userID, user, siteID, ip, adminSess string
	exp                                 time.Time
}

// Manager keeps tickets and sessions in memory (a restart ends them all: the
// owner opens WebFig again from the router page).
type Manager struct {
	o        Options
	mu       sync.Mutex
	tickets  map[string]ticket
	sessions map[string]*Session // by cookie hash
	perSite  map[string]int
	total    int
	// /jsproxy pools (see JSProxyPerSession).
	jsSess  map[string]int
	jsSite  map[string]int
	jsTotal int
	px      *proxyState
	cache   *fileCache
}

// New returns a Manager.
func New(o Options) *Manager {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PerRouter <= 0 {
		o.PerRouter = PerRouterMax
	}
	if o.Total <= 0 {
		o.Total = TotalMax
	}
	if o.SlotWait <= 0 {
		o.SlotWait = SlotWait
	}
	if o.ClientIP == nil {
		o.ClientIP = func(r *http.Request) string {
			h, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				return r.RemoteAddr
			}
			return h
		}
	}
	m := &Manager{o: o, tickets: map[string]ticket{}, sessions: map[string]*Session{}, perSite: map[string]int{},
		jsSess: map[string]int{}, jsSite: map[string]int{}}
	switch {
	case o.CacheBytes == 0:
		m.cache = newFileCache(DefaultCacheBytes, CacheTTL, o.Now)
	case o.CacheBytes > 0:
		m.cache = newFileCache(o.CacheBytes, CacheTTL, o.Now)
	}
	m.px = newProxyState(m)
	return m
}

// CookieName is the session cookie's name.
func (m *Manager) CookieName() string {
	if m.o.Secure {
		return cookieSecure
	}
	return cookiePlain
}

// Domain is the WebFig host.
func (m *Manager) Domain() string { return m.o.Domain }

func hashOf(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Mint makes a one-time ticket bound to the admin user, the router and the
// client address, valid for TicketTTL. The router must be openable now.
func (m *Manager) Mint(userID, user, siteID, ip, adminSess string) (string, error) {
	if _, ok := m.target(siteID); !ok {
		return "", ErrRouter
	}
	tok := randToken(32)
	now := m.o.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, t := range m.tickets {
		if !now.Before(t.exp) {
			delete(m.tickets, k)
		}
	}
	if len(m.tickets) > 2000 {
		return "", errors.New("webfig: too many open tickets")
	}
	m.tickets[hashOf(tok)] = ticket{userID: userID, user: user, siteID: siteID, ip: ip, adminSess: adminSess, exp: now.Add(TicketTTL)}
	return tok, nil
}

// target resolves and validates the router's address: inside the tunnel
// subnet, not the server itself, a real port.
func (m *Manager) target(siteID string) (Target, bool) {
	if m.o.Resolve == nil {
		return Target{}, false
	}
	t, ok := m.o.Resolve(siteID)
	if !ok {
		return Target{}, false
	}
	a, err := netip.ParseAddr(t.IP)
	if err != nil || !a.Is4() || t.Port < 1 || t.Port > 65535 {
		return Target{}, false
	}
	if m.o.Tunnel.IsValid() && !m.o.Tunnel.Contains(a) {
		return Target{}, false
	}
	if m.o.ServerIP.IsValid() && a == m.o.ServerIP {
		return Target{}, false
	}
	return t, true
}

// Redeem exchanges a ticket for a session. The ticket dies whether or not the
// exchange succeeds. oldCookie (the browser's current WebFig cookie, if any)
// is ended: one session per browser. Store lookups happen outside the lock.
func (m *Manager) Redeem(tok, ip, oldCookie string) (cookie string, s Session, err error) {
	now := m.o.Now()
	m.mu.Lock()
	t, ok := m.tickets[hashOf(tok)]
	delete(m.tickets, hashOf(tok))
	m.mu.Unlock()
	if !ok || !now.Before(t.exp) {
		return "", Session{}, ErrTicket
	}
	if subtle.ConstantTimeCompare([]byte(t.ip), []byte(ip)) != 1 {
		return "", Session{}, ErrAddress
	}
	if _, f := m.target(t.siteID); !f || !m.userLive(t.userID, t.adminSess) {
		return "", Session{}, ErrRouter
	}
	cookie = randToken(32)
	ns := &Session{PublicID: hex.EncodeToString([]byte(randToken(6))[:6]), UserID: t.userID, User: t.user, SiteID: t.siteID, IP: ip,
		AdminSession: t.adminSess, Started: now, LastSeen: now, hash: hashOf(cookie)}
	var ended []Session
	m.mu.Lock()
	if oldCookie != "" {
		if old, f := m.sessions[hashOf(oldCookie)]; f {
			ended = append(ended, m.dropLocked(old))
		}
	}
	m.sessions[ns.hash] = ns
	s = *ns
	m.mu.Unlock()
	for _, e := range ended {
		m.fireEnd(e, EndReplaced, now)
	}
	if m.o.OnOpen != nil {
		m.o.OnOpen(s)
	}
	return cookie, s, nil
}

// userLive: the user may still use WebFig and the admin session that opened it is alive.
func (m *Manager) userLive(userID, adminSess string) bool {
	if m.o.UserOK != nil && !m.o.UserOK(userID) {
		return false
	}
	if m.o.AdminLive != nil && !m.o.AdminLive(adminSess) {
		return false
	}
	return true
}

func (m *Manager) dropLocked(s *Session) Session {
	delete(m.sessions, s.hash)
	return *s
}

func (m *Manager) fireEnd(s Session, reason string, at time.Time) {
	if m.o.OnEnd != nil {
		m.o.OnEnd(s, reason, at)
	}
}

// endHash ends a session by its hash if it is still there.
func (m *Manager) endHash(hash, reason string, at time.Time) {
	m.mu.Lock()
	s, ok := m.sessions[hash]
	var ended Session
	if ok {
		ended = m.dropLocked(s)
	}
	m.mu.Unlock()
	if ok {
		m.fireEnd(ended, reason, at)
	}
}

// lookup finds the live session for a cookie value and touches it. Expired
// sessions, sessions whose admin session is over and sessions whose router or
// user no longer qualify end here. The session is copied out under the lock;
// the store is asked outside it.
func (m *Manager) lookup(cookie string) (Session, Target, bool) {
	if cookie == "" || len(cookie) > 200 {
		return Session{}, Target{}, false
	}
	now := m.o.Now()
	h := hashOf(cookie)
	m.mu.Lock()
	sp, ok := m.sessions[h]
	var s Session
	if ok {
		s = *sp
	}
	m.mu.Unlock()
	if !ok {
		return Session{}, Target{}, false
	}
	reason := ""
	switch {
	case !now.Before(s.Started.Add(MaxLife)):
		reason = EndMax
	case now.Sub(s.LastSeen) >= IdleTimeout:
		reason = EndIdle
	}
	var tg Target
	if reason == "" {
		var f bool
		tg, f = m.target(s.SiteID)
		switch {
		case !f:
			reason = EndRouter
		case m.o.UserOK != nil && !m.o.UserOK(s.UserID):
			reason = EndRouter
		case m.o.AdminLive != nil && !m.o.AdminLive(s.AdminSession):
			reason = EndAdmin
		}
	}
	if reason != "" {
		m.endHash(h, reason, now)
		return Session{}, Target{}, false
	}
	m.mu.Lock()
	sp, still := m.sessions[h]
	if still {
		sp.LastSeen = now
		s = *sp
	}
	m.mu.Unlock()
	if !still {
		return Session{}, Target{}, false
	}
	return s, tg, true
}

func (m *Manager) touch(hash string) {
	m.mu.Lock()
	if s, ok := m.sessions[hash]; ok {
		s.LastSeen = m.o.Now()
	}
	m.mu.Unlock()
}

// Sweep ends the sessions that ran out of time or whose admin session or user
// is gone. Call it every minute (and after a sign-out).
func (m *Manager) Sweep() {
	now := m.o.Now()
	type e struct {
		s Session
		r string
	}
	var out []e
	var check []Session
	m.mu.Lock()
	for _, s := range m.sessions {
		switch {
		case !now.Before(s.Started.Add(MaxLife)):
			out = append(out, e{*s, EndMax})
		case now.Sub(s.LastSeen) >= IdleTimeout:
			out = append(out, e{*s, EndIdle})
		default:
			check = append(check, *s)
		}
	}
	for _, x := range out {
		delete(m.sessions, x.s.hash)
	}
	for k, t := range m.tickets {
		if !now.Before(t.exp) {
			delete(m.tickets, k)
		}
	}
	m.mu.Unlock()
	for _, x := range out {
		m.fireEnd(x.s, x.r, now)
	}
	for _, s := range check { // store lookups, outside the lock
		if !m.userLive(s.UserID, s.AdminSession) {
			reason := EndAdmin
			if m.o.UserOK != nil && !m.o.UserOK(s.UserID) {
				reason = EndRouter
			}
			m.endHash(s.hash, reason, now)
		}
	}
}

// List is the live sessions of a router, newest first.
func (m *Manager) List(siteID string) []Session {
	now := m.o.Now()
	var out []Session
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.SiteID == siteID && now.Before(s.Started.Add(MaxLife)) && now.Sub(s.LastSeen) < IdleTimeout {
			out = append(out, *s)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// End ends one session of a router by its public id.
func (m *Manager) End(siteID, publicID, reason string) (Session, bool) {
	m.mu.Lock()
	var found *Session
	for _, s := range m.sessions {
		if s.SiteID == siteID && s.PublicID == publicID {
			found = s
		}
	}
	if found == nil {
		m.mu.Unlock()
		return Session{}, false
	}
	ended := m.dropLocked(found)
	m.mu.Unlock()
	m.fireEnd(ended, reason, m.o.Now())
	return ended, true
}

// acquire takes one proxy slot for the router; release returns it. A /jsproxy
// call (js) takes from its own small pools (per session, per router, overall)
// and never from the pool the static files use.
func (m *Manager) acquire(siteID, sessHash string, js bool) (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var once sync.Once
	if js {
		if m.jsTotal >= JSProxyTotal || m.jsSite[siteID] >= JSProxyPerRouter || m.jsSess[sessHash] >= JSProxyPerSession {
			return nil, false
		}
		m.jsTotal++
		m.jsSite[siteID]++
		m.jsSess[sessHash]++
		return func() {
			once.Do(func() {
				m.mu.Lock()
				m.jsTotal--
				dec(m.jsSite, siteID)
				dec(m.jsSess, sessHash)
				m.mu.Unlock()
			})
		}, true
	}
	if m.total >= m.o.Total || m.perSite[siteID] >= m.o.PerRouter {
		return nil, false
	}
	m.total++
	m.perSite[siteID]++
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.total--
			dec(m.perSite, siteID)
			m.mu.Unlock()
		})
	}, true
}

func dec(mp map[string]int, k string) {
	if mp[k]--; mp[k] <= 0 {
		delete(mp, k)
	}
}

// SetSlotWait changes how long a request waits for a slot (a test hook).
func (m *Manager) SetSlotWait(d time.Duration) {
	m.mu.Lock()
	m.o.SlotWait = d
	m.mu.Unlock()
}

// acquireWait waits for a proxy slot instead of refusing at once: WebFig's page
// fetches its scripts in parallel, and one refused script leaves it stuck on
// "Connecting". It gives up when the request ends or after SlotWait.
func (m *Manager) acquireWait(ctx context.Context, siteID, sessHash string, js bool) (release func(), ok bool) {
	m.mu.Lock()
	wait := m.o.SlotWait
	m.mu.Unlock()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if rel, ok := m.acquire(siteID, sessHash, js); ok {
			return rel, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-deadline.C:
			return nil, false
		case <-tick.C:
		}
	}
}

// Busy is how many proxied requests are in flight now (tests wait for it to
// reach 0 before moving a fake clock).
func (m *Manager) Busy() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total + m.jsTotal
}

// PurgeCache drops a router's cached WebFig files (the router was re-bootstrapped).
func (m *Manager) PurgeCache(siteID string) {
	if m.cache != nil {
		m.cache.purgeSite(siteID)
	}
}

// CacheStats is the number of cached files and their bytes (tests).
func (m *Manager) CacheStats() (entries int, bytes int64) {
	if m.cache == nil {
		return 0, 0
	}
	return m.cache.stats()
}

// OpenURL is the address an admin is sent to with a ticket.
func (m *Manager) OpenURL(tok string) string {
	return "https://" + m.o.Domain + "/_ftn/open?t=" + tok
}
