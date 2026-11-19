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
	TicketTTL    = 60 * time.Second
	IdleTimeout  = 30 * time.Minute
	MaxLife      = 2 * time.Hour
	PerRouterMax = 4  // concurrent proxied requests per router
	TotalMax     = 16 // concurrent proxied requests overall
)

// Reasons a session ends (shown in the activity record).
const (
	EndIdle     = "idle for 30 minutes"
	EndMax      = "2 hour limit reached"
	EndReplaced = "replaced by a new WebFig session in the same browser"
	EndOwner    = "ended by an owner"
	EndRouter   = "the router or the user changed"
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
	// OnOpen and OnEnd write the activity record.
	OnOpen func(s Session)
	OnEnd  func(s Session, reason string, at time.Time)
	// ClientIP is the address of the request's client (the web package's realIP).
	ClientIP func(r *http.Request) string
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error) // tests
	Now      func() time.Time
	// Limits (zero = defaults above).
	PerRouter, Total int
}

// Session is one WebFig session: one browser, one router.
type Session struct {
	PublicID  string
	UserID    string
	User      string
	SiteID    string
	IP        string // client address that redeemed the ticket
	Started   time.Time
	LastSeen  time.Time
	hash      string
	connected int
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
	userID, user, siteID, ip string
	exp                      time.Time
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
	px       *proxyState
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
	if o.ClientIP == nil {
		o.ClientIP = func(r *http.Request) string {
			h, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				return r.RemoteAddr
			}
			return h
		}
	}
	m := &Manager{o: o, tickets: map[string]ticket{}, sessions: map[string]*Session{}, perSite: map[string]int{}}
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
func (m *Manager) Mint(userID, user, siteID, ip string) (string, error) {
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
	m.tickets[hashOf(tok)] = ticket{userID: userID, user: user, siteID: siteID, ip: ip, exp: now.Add(TicketTTL)}
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
// is ended: one session per browser.
func (m *Manager) Redeem(tok, ip, oldCookie string) (cookie string, s Session, err error) {
	now := m.o.Now()
	var ended []Session
	m.mu.Lock()
	t, ok := m.tickets[hashOf(tok)]
	delete(m.tickets, hashOf(tok))
	if !ok || !now.Before(t.exp) || subtle.ConstantTimeCompare([]byte(t.ip), []byte(ip)) != 1 {
		m.mu.Unlock()
		return "", Session{}, ErrTicket
	}
	if _, f := m.target(t.siteID); !f || (m.o.UserOK != nil && !m.o.UserOK(t.userID)) {
		m.mu.Unlock()
		return "", Session{}, ErrRouter
	}
	if oldCookie != "" {
		if old, f := m.sessions[hashOf(oldCookie)]; f {
			ended = append(ended, m.dropLocked(old))
		}
	}
	cookie = randToken(32)
	ns := &Session{PublicID: hex.EncodeToString([]byte(randToken(6))[:6]), UserID: t.userID, User: t.user, SiteID: t.siteID, IP: ip,
		Started: now, LastSeen: now, hash: hashOf(cookie)}
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

func (m *Manager) dropLocked(s *Session) Session {
	delete(m.sessions, s.hash)
	return *s
}

func (m *Manager) fireEnd(s Session, reason string, at time.Time) {
	if m.o.OnEnd != nil {
		m.o.OnEnd(s, reason, at)
	}
}

// lookup finds the live session for a cookie value and touches it. Expired
// sessions and sessions whose router or user no longer qualify end here.
func (m *Manager) lookup(cookie string) (Session, Target, bool) {
	if cookie == "" || len(cookie) > 200 {
		return Session{}, Target{}, false
	}
	now := m.o.Now()
	m.mu.Lock()
	s, ok := m.sessions[hashOf(cookie)]
	if !ok {
		m.mu.Unlock()
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
		if tg, f = m.target(s.SiteID); !f || (m.o.UserOK != nil && !m.o.UserOK(s.UserID)) {
			reason = EndRouter
		}
	}
	if reason != "" {
		ended := m.dropLocked(s)
		m.mu.Unlock()
		m.fireEnd(ended, reason, now)
		return Session{}, Target{}, false
	}
	s.LastSeen = now
	out := *s
	m.mu.Unlock()
	return out, tg, true
}

func (m *Manager) touch(hash string) {
	m.mu.Lock()
	if s, ok := m.sessions[hash]; ok {
		s.LastSeen = m.o.Now()
	}
	m.mu.Unlock()
}

// Sweep ends the sessions that ran out of time. Call it every minute.
func (m *Manager) Sweep() {
	now := m.o.Now()
	type e struct {
		s Session
		r string
	}
	var out []e
	m.mu.Lock()
	for _, s := range m.sessions {
		switch {
		case !now.Before(s.Started.Add(MaxLife)):
			out = append(out, e{*s, EndMax})
		case now.Sub(s.LastSeen) >= IdleTimeout:
			out = append(out, e{*s, EndIdle})
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

// acquire takes one proxy slot for the router; release returns it.
func (m *Manager) acquire(siteID string) (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total >= m.o.Total || m.perSite[siteID] >= m.o.PerRouter {
		return nil, false
	}
	m.total++
	m.perSite[siteID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.total--
			if m.perSite[siteID]--; m.perSite[siteID] <= 0 {
				delete(m.perSite, siteID)
			}
			m.mu.Unlock()
		})
	}, true
}

// OpenURL is the address an admin is sent to with a ticket.
func (m *Manager) OpenURL(tok string) string {
	return "https://" + m.o.Domain + "/_ftn/open?t=" + tok
}
