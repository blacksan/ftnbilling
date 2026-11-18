// Package web serves the captive portal, admin UI and payment callbacks.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/payments"
	"ispbill/internal/settings"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static/favicon.svg
var faviconSVG []byte

//go:embed static/logo-*.svg
var logoFS embed.FS

// Comfortaa Bold (SIL Open Font License), used for the brand wordmark so it
// looks the same on every phone without needing internet.
//
//go:embed static/comfortaa-700.woff2
var brandFont []byte

// Server wires HTTP handlers to the billing service.
type Server struct {
	cfg       config.Config
	st        *store.Store
	svc       *billing.Service
	providers map[string]payments.Provider
	mpesa     *payments.Mpesa // may be nil
	mgr       *sites.Manager
	log       *slog.Logger
	tpl       *template.Template
	rl        *limiter
	joinRL    *limiter // /j/ by client address
	testRL    *limiter // settings test buttons
	stkRL     *limiter // KES 1 test payments
	toolRL    *limiter // ping, traceroute, console
	toolSem   chan struct{}
	toolRun   ToolRunner // runs ping and traceroute from this server; nil = the real programs
	findRL    *limiter   // equipment finder
	wgNet     netip.Prefix
	ops       Ops
	setm      *settings.Manager // nil = environment configuration only
	auth      *adminAuth
	authn     *authn.Service
	alertSink func(string) // tests: take security alerts instead of the outbox
	idem      *idemStore
	refreshRL *limiter // /api/v1/auth/refresh by client address

	onlineMu sync.Mutex
	online   map[string]onlineEntry
}

type onlineEntry struct {
	ok bool
	at time.Time
}

func New(cfg config.Config, st *store.Store, svc *billing.Service, providers map[string]payments.Provider, mpesa *payments.Mpesa, mgr *sites.Manager, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, st: st, svc: svc, providers: providers, mpesa: mpesa, mgr: mgr, log: log,
		rl: newLimiter(15, time.Minute), joinRL: newLimiter(10, time.Minute), testRL: newLimiter(5, time.Minute), stkRL: newLimiter(3, 10*time.Minute), toolRL: newLimiter(30, time.Minute), toolSem: make(chan struct{}, 2), findRL: newLimiter(5, time.Minute), online: map[string]onlineEntry{}}
	s.auth = newAdminAuth(s)
	if p, err := netip.ParsePrefix(cfg.WGSubnet); err == nil {
		s.wgNet = p.Masked()
	}
	a, err := authn.New(st, cfg.DataKey, cfg.BrandName)
	if err != nil {
		log.Error("sign-in: DATA_KEY unusable, two-factor sign-in cannot be set up", "err", err)
		a, _ = authn.New(st, nil, cfg.BrandName)
	}
	s.authn = a
	a.SetAlert(s.securityAlert)
	s.idem = newIdemStore()
	s.refreshRL = newLimiter(30, time.Minute)
	funcs := template.FuncMap{
		"kes": func(n int) string { return "KES " + itoa(n) },
		// brandParts splits "Fast Tech Net" into {"Fast Tech", "Net"} so the
		// last word can be drawn in the accent colour.
		"brandParts": func(b string) []string {
			b = strings.TrimSpace(b)
			if i := strings.LastIndex(b, " "); i > 0 {
				return []string{b[:i], b[i+1:]}
			}
			return []string{b, ""}
		},
		"dur":    billing.DescribeMinutes,
		"eat":    func(t time.Time) string { return t.In(billing.EAT).Format("Mon 2 Jan 2006, 15:04") + " EAT" },
		"left":   func(t time.Time) string { return billing.Remaining(t, svc.Now()) },
		"pkgOf":  func(m map[string]store.Package, id string) store.Package { return m[id] },
		"acctOf": func(m map[string]store.Account, id string) store.Account { return m[id] },
		"state":  func(a store.Account) string { return billing.AccountState(a, svc.Now()) },
		"f1":     func(v float64) string { return fmt.Sprintf("%.1f", v) },
		"f0":     func(v float64) string { return fmt.Sprintf("%.0f", v) },
		"f2":     func(v float64) string { return fmt.Sprintf("%.2f", v) },
		"eatNow": func() string { return svc.Now().In(billing.EAT).Format("2006-01-02T15:04") },
		"inc":    func(i int) int { return i + 1 },
		"day":    day,
		"stat":   func(v string) string { return strings.ReplaceAll(v, "_", " ") },
		"deref":  func(b *bool) bool { return b != nil && *b },
		"last4":  lastFour,
		"pct": func(a, b float64) float64 {
			if b <= 0 {
				return 0
			}
			return 100 * a / b
		},
		"hours": func(min float64) float64 { return min / 60 },
		"seq":   func(p ...string) []string { return p },
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"lines": func(v string) string { return strings.ReplaceAll(v, ",", "\n") },
	}
	s.tpl = template.Must(template.New("").Funcs(funcs).ParseFS(tplFS, "templates/*.html"))
	return s
}

// SetSettings installs the settings manager. Call it before Handler. Without
// one the server uses its boot configuration only.
func (s *Server) SetSettings(m *settings.Manager) { s.setm = m }

// live is the configuration in force now: the boot configuration with the
// admin-edited settings laid over it. Use it for everything Settings can
// change (M-Pesa, WhatsApp, email, the admin user); s.cfg is for boot-only
// values (listen address, WireGuard, data directory).
func (s *Server) live() config.Config {
	if s.setm != nil {
		return s.setm.Effective()
	}
	return s.cfg
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Handler returns the full HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/portal", http.StatusFound) })
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /j/{token}", s.joinScript)
	s.portalRoutes(mux)
	s.routerFileRoutes(mux)
	s.callbackRoutes(mux)
	s.waRoutes(mux)
	s.loginRoutes(mux)
	s.adminRoutes(mux)
	s.apiRoutes(mux)
	return s.middleware(mux)
}

// siteOnline reports whether a building's router answered a ping recently.
// The answer is cached for 15 seconds, so health checks never pile up pings.
func (s *Server) siteOnline(ctx context.Context, siteID string) bool {
	s.onlineMu.Lock()
	e, ok := s.online[siteID]
	s.onlineMu.Unlock()
	if ok && time.Since(e.at) < 15*time.Second {
		return e.ok
	}
	pc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	up := s.svc.Ping(pc, siteID) == nil
	s.onlineMu.Lock()
	s.online[siteID] = onlineEntry{up, time.Now()}
	s.onlineMu.Unlock()
	return up
}

// healthz: sites_total counts the buildings, sites_online those whose router
// answered recently (cached for 15 s). router_ok is true when every building
// that should be running is online (and trivially true with no buildings), so
// a single legacy router behaves as before.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	var all, runnable []string
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			all = append(all, st.ID)
			if st.Runnable() {
				runnable = append(runnable, st.ID)
			}
		}
	})
	var wg sync.WaitGroup
	var mu sync.Mutex
	online := 0
	for _, id := range runnable {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.siteOnline(r.Context(), id) {
				mu.Lock()
				online++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "router_ok": online == len(runnable), "sites_total": len(all), "sites_online": online,
		"test_mode": s.cfg.TestMode, "mpesa_enabled": s.live().MpesaEnabled(),
		"db": s.st.Kind(), "db_ok": s.st.Healthy() == nil,
	})
}

// redactPath hides secrets in a request path before it is logged: a join token
// is a credential.
func redactPath(p string) string {
	if strings.HasPrefix(p, "/j/") {
		return "/j/[redacted]"
	}
	// An API path may carry a device id or payment key, never a token, but the
	// sign-in endpoints are kept out of the log entirely beyond their name.
	if strings.HasPrefix(p, "/api/v1/auth/") {
		if rest := strings.TrimPrefix(p, "/api/v1/auth/"); strings.HasPrefix(rest, "devices/") {
			return "/api/v1/auth/devices/[id]"
		}
	}
	// /pay/cb/t/<token>/stk: the token is a credential.
	if rest, ok := strings.CutPrefix(p, "/pay/cb/t/"); ok {
		if i := strings.Index(rest, "/"); i >= 0 {
			return "/pay/cb/t/[redacted]" + rest[i:]
		}
		return "/pay/cb/t/[redacted]"
	}
	return p
}

// realIP is the address of the client: the connection's, unless it comes from
// the local reverse proxy, which appends the real one to X-Forwarded-For.
func realIP(r *http.Request) string {
	ip := clientIP(r)
	if a, err := netip.ParseAddr(ip); err == nil && a.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return ip
}

// ---- middleware ---------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		s.securityHeaders(sw.Header(), r)
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", redactPath(r.URL.Path), "panic", rec)
				http.Error(sw, "internal error", 500)
			}
			if r.URL.Path != "/healthz" {
				// Query strings, headers and bodies are deliberately not logged:
				// they carry passwords, codes and tokens.
				s.log.Info("http", "method", r.Method, "path", redactPath(r.URL.Path), "status", sw.code, "ms", time.Since(start).Milliseconds())
			}
		}()
		if s.fromTunnel(r) && (r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") || strings.HasPrefix(r.URL.Path, "/api/v1/") || r.URL.Path == "/api/v1") {
			// Routers reach the server over their tunnel and must never see the
			// admin pages or the API. (The customer portal's /api/orders/ poll
			// is not part of the API and stays.)
			http.NotFound(sw, r)
			return
		}
		r.Body = http.MaxBytesReader(sw, r.Body, 1<<20)
		next.ServeHTTP(sw, r)
	})
}

// render executes a template into a buffer first so errors never produce half pages.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("template", "name", name, "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// ---- helpers ------------------------------------------------------------

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func safeEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// limiter is a small per-key sliding-window rate limiter.
type limiter struct {
	mu   sync.Mutex
	n    int
	win  time.Duration
	hits map[string][]time.Time
}

const limiterCap = 5000

// evictLocked drops the keys that were quiet the longest (never all of them,
// which would hand every busy client a fresh allowance).
func (l *limiter) evictLocked(now time.Time) {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(l.hits))
	for k, ts := range l.hits {
		last := time.Time{}
		if len(ts) > 0 {
			last = ts[len(ts)-1]
		}
		if now.Sub(last) >= l.win {
			delete(l.hits, k)
			continue
		}
		all = append(all, kv{k, last})
	}
	if len(l.hits) <= limiterCap {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for _, x := range all[:len(all)-limiterCap*9/10] {
		delete(l.hits, x.k)
	}
}

func newLimiter(n int, win time.Duration) *limiter {
	return &limiter{n: n, win: win, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.win {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.n {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	if len(l.hits) > limiterCap {
		l.evictLocked(now)
	}
	return true
}

// packageGroups splits enabled packages into short-term passes and monthly
// plans, each cheapest first.
type packageGroup struct {
	Title    string
	Packages []store.Package
}

func packageGroups(all []store.Package) []packageGroup {
	var short, monthly []store.Package
	for _, p := range all {
		if p.Disabled {
			continue
		}
		if p.DurationMin >= 43200 {
			monthly = append(monthly, p)
		} else {
			short = append(short, p)
		}
	}
	byPrice := func(x []store.Package) {
		sort.SliceStable(x, func(i, j int) bool { return x[i].PriceKES < x[j].PriceKES })
	}
	byPrice(short)
	byPrice(monthly)
	var out []packageGroup
	if len(short) > 0 {
		out = append(out, packageGroup{"Quick passes", short})
	}
	if len(monthly) > 0 {
		out = append(out, packageGroup{"Monthly plans", monthly})
	}
	return out
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(s) <= 1024
}

var _ = strings.TrimSpace

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// fromTunnel reports whether the request came from inside the WireGuard
// subnet (a router, not a person). The admin pages and the API are refused
// there (404, as if they did not exist).
func (s *Server) fromTunnel(r *http.Request) bool {
	if !s.wgNet.IsValid() {
		return false
	}
	a, err := netip.ParseAddr(realIP(r))
	return err == nil && s.wgNet.Contains(a.Unmap())
}

// Auth is the sign-in service (users, sessions, devices); main seeds the owner
// through it and the tests sign in with it.
func (s *Server) Auth() *authn.Service { return s.authn }
