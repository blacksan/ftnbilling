package webfig

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type ctxKey struct{}

// proxyState is the shared transport. It never uses a proxy from the
// environment and refuses to dial anything but the address of the session's
// router, which travels in the request context.
type proxyState struct {
	tr *http.Transport
}

func newProxyState(m *Manager) *proxyState {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	dial := m.o.Dial
	if dial == nil {
		dial = d.DialContext
	}
	return &proxyState{tr: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			want, _ := ctx.Value(ctxKey{}).(string)
			if want == "" || addr != want || network != "tcp" {
				return nil, errors.New("webfig: refused to dial " + addr)
			}
			return dial(ctx, network, addr)
		},
		MaxIdleConnsPerHost:   2,
		MaxIdleConns:          32,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}}
}

// clearSiteData wipes caches, storage and service workers of the WebFig origin
// (not cookies: the open response sets the new one).
const clearSiteData = `"cache", "storage"`

const noSessionPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>WebFig</title>` +
	`<body style="font:16px system-ui;background:#0d1117;color:#e6e9ef;display:grid;place-items:center;height:90vh"><p>Open WebFig from the router page in the admin.</p>`

func plain(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if msg == "" {
		_, _ = w.Write([]byte(noSessionPage))
		return
	}
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>WebFig</title>` +
		`<body style="font:16px system-ui;background:#0d1117;color:#e6e9ef;display:grid;place-items:center;height:90vh"><p>` + msg + `</p>`))
}

func (m *Manager) baseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	if m.o.Secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// hostOK accepts the WebFig host name only (a port is tolerated for tests).
func (m *Manager) hostOK(r *http.Request) bool {
	h := strings.ToLower(r.Host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h == m.o.Domain
}

// badPath refuses request targets that are not a plain origin-form path.
func badPath(r *http.Request) bool {
	if r.URL.IsAbs() || r.URL.Host != "" || r.URL.Opaque != "" || !strings.HasPrefix(r.RequestURI, "/") || strings.HasPrefix(r.RequestURI, "//") {
		return true
	}
	p := r.URL.Path
	if strings.ContainsAny(p, "\\\x00") || strings.Contains(r.URL.RawPath, "\x00") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return true
		}
	}
	if strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2e%2e") || strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%5c") {
		return true
	}
	return false
}

// Handler is the handler of the WebFig host.
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(m.serve)
}

func (m *Manager) serve(w http.ResponseWriter, r *http.Request) {
	m.baseHeaders(w)
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		plain(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	if !m.hostOK(r) {
		plain(w, http.StatusMisdirectedRequest, "Unknown host.")
		return
	}
	// One origin serves every router, so router script must not install a worker
	// that would outlive its session and watch the next router's pages.
	if r.Header.Get("Service-Worker") != "" {
		plain(w, http.StatusForbidden, "Service workers are not allowed on the WebFig host.")
		return
	}
	if badPath(r) {
		plain(w, http.StatusBadRequest, "Bad request.")
		return
	}
	if r.URL.Path == "/_ftn/open" {
		m.open(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_ftn/") || r.URL.Path == "/_ftn" {
		http.NotFound(w, r)
		return
	}
	// WebFig only: the router's REST API is for the billing server alone.
	if r.URL.Path == "/rest" || strings.HasPrefix(r.URL.Path, "/rest/") {
		plain(w, http.StatusForbidden, "The router's API is not available through WebFig.")
		return
	}
	var cv string
	if c, err := r.Cookie(m.CookieName()); err == nil {
		cv = c.Value
	}
	sess, tg, ok := m.lookup(cv)
	if !ok {
		if cv != "" {
			// The session ended: drop the cookie and what the router left in this browser.
			http.SetCookie(w, m.cookie("", -1))
			w.Header().Set("Clear-Site-Data", clearSiteData)
		}
		plain(w, http.StatusUnauthorized, "")
		return
	}
	release, ok := m.acquire(sess.SiteID)
	if !ok {
		w.Header().Set("Retry-After", "5")
		plain(w, http.StatusServiceUnavailable, "Too many WebFig requests at once for this router. Wait a moment and reload.")
		return
	}
	defer release()
	defer m.touch(sess.hash)

	target := &url.URL{Scheme: "http", Host: tg.Addr()}
	ctx, cancel := context.WithDeadline(r.Context(), sess.Started.Add(MaxLife))
	defer cancel()
	ctx = context.WithValue(ctx, ctxKey{}, tg.Addr())
	rp := &httputil.ReverseProxy{
		Transport:     m.px.tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// Nothing about the admin or this host goes to the router.
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
			stripOurCookies(pr.Out)
		},
		ModifyResponse: func(resp *http.Response) error { return m.fixResponse(resp, tg) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			m.baseHeaders(w)
			plain(w, http.StatusBadGateway, "The router did not answer on its web port. Check that the router is online, then reload.")
		},
	}
	// The proxy copies the router's headers onto w (fixResponse has set ours there):
	// start from none so nothing is listed twice.
	for k := range w.Header() {
		delete(w.Header(), k)
	}
	rp.ServeHTTP(w, r.WithContext(ctx))
}

// stripOurCookies removes this server's cookies (the WebFig session and any
// ftn_ cookie) from the request to the router; the router's own stay.
func stripOurCookies(out *http.Request) {
	cs := out.Cookies()
	out.Header.Del("Cookie")
	for _, c := range cs {
		n := strings.TrimPrefix(c.Name, "__Host-")
		if strings.HasPrefix(n, "ftn_") {
			continue
		}
		out.AddCookie(c)
	}
}

func (m *Manager) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: m.CookieName(), Value: value, Path: "/", HttpOnly: true, Secure: m.o.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge}
}

// fixResponse hardens what the router sends back.
func (m *Manager) fixResponse(resp *http.Response, tg Target) error {
	h := resp.Header
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Del("Pragma")
	h.Del("Expires")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	if m.o.Secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	h.Del("Service-Worker-Allowed")
	h.Del("Refresh") // a refresh header can send the browser anywhere
	for _, name := range []string{"Location", "Content-Location"} {
		if loc := h.Get(name); loc != "" {
			h.Set(name, rewriteLocation(loc, tg))
		}
	}
	// Cookies the router sets stay on this host only: no Domain, always Secure.
	if sc := resp.Cookies(); len(sc) > 0 {
		h.Del("Set-Cookie")
		for _, c := range sc {
			if strings.HasPrefix(strings.TrimPrefix(c.Name, "__Host-"), "ftn_") {
				continue
			}
			c.Domain = ""
			c.Secure = m.o.Secure
			if c.Path == "" {
				c.Path = "/"
			}
			h.Add("Set-Cookie", c.String())
		}
	}
	return nil
}

// rewriteLocation keeps a redirect on the WebFig host: a link to the router's
// own address becomes a path; a link anywhere else is dropped to "/".
func rewriteLocation(loc string, tg Target) string {
	u, err := url.Parse(loc)
	if err != nil {
		return "/"
	}
	if strings.ContainsAny(loc, "\\\x00\r\n") || strings.HasPrefix(loc, "//") {
		return "/" // a browser reads "/\\host" and "//host" as another site
	}
	if u.Host == "" && u.Scheme == "" {
		return loc // already relative to this host
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "/"
	}
	host := u.Hostname()
	port := u.Port()
	own := host == tg.IP && (port == "" || port == fmt.Sprint(tg.Port))
	if !own {
		return "/"
	}
	out := u.EscapedPath()
	if out == "" {
		out = "/"
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

// open exchanges a ticket for a session cookie.
func (m *Manager) open(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		plain(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	var old string
	if c, err := r.Cookie(m.CookieName()); err == nil {
		old = c.Value
	}
	cookie, _, err := m.Redeem(r.URL.Query().Get("t"), m.o.ClientIP(r), old)
	if errors.Is(err, ErrAddress) {
		plain(w, http.StatusForbidden, "Your network changed between the admin and WebFig. Open WebFig again from the router page in the admin.")
		return
	}
	if err != nil {
		plain(w, http.StatusForbidden, "This WebFig link expired or was already used. Open WebFig again from the router page in the admin.")
		return
	}
	// Whatever a previous router left behind in this browser goes before the new session starts.
	w.Header().Set("Clear-Site-Data", clearSiteData)
	http.SetCookie(w, m.cookie(cookie, int(MaxLife/time.Second)))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
