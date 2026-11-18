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
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/payments"
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
	col       *metrics.Collector
	log       *slog.Logger
	tpl       *template.Template
	rl        *limiter
	guard     *hostGuard // nil unless VERIFY_HOTSPOT_HOST=true
}

func New(cfg config.Config, st *store.Store, svc *billing.Service, providers map[string]payments.Provider, mpesa *payments.Mpesa, col *metrics.Collector, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, st: st, svc: svc, providers: providers, mpesa: mpesa, col: col, log: log, rl: newLimiter(15, time.Minute)}
	if cfg.VerifyHotspotHost {
		s.guard = newHostGuard(svc)
	}
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
		"pct": func(a, b float64) float64 {
			if b <= 0 {
				return 0
			}
			return 100 * a / b
		},
		"hours": func(min float64) float64 { return min / 60 },
		"seq":   func(p ...string) []string { return p },
	}
	s.tpl = template.Must(template.New("").Funcs(funcs).ParseFS(tplFS, "templates/*.html"))
	return s
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Handler returns the full HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/portal", http.StatusFound) })
	mux.HandleFunc("GET /healthz", s.healthz)
	s.portalRoutes(mux)
	s.routerFileRoutes(mux)
	s.callbackRoutes(mux)
	s.adminRoutes(mux)
	return s.middleware(mux)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	routerOK := s.svc.Ping(ctx) == nil
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "router_ok": routerOK, "test_mode": s.cfg.TestMode, "mpesa_enabled": s.cfg.MpesaEnabled(),
	})
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
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				http.Error(sw, "internal error", 500)
			}
			if r.URL.Path != "/healthz" {
				// Query strings are deliberately not logged.
				s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds())
			}
		}()
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
	if len(l.hits) > 5000 { // crude bound on memory
		l.hits = map[string][]time.Time{key: l.hits[key]}
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
