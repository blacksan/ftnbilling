package webfig

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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

// clearSiteData wipes storage and service workers of the WebFig origin (not
// cookies: the open response sets the new one). No "cache": Chrome blocks the
// navigation while it walks its whole HTTP cache for that directive, which can
// take many seconds (WebFig looked dead and the owner opened it again and
// again); every router response here is no-store anyway, and the static files
// that may be cached are keyed per router on the server, not in the browser.
const clearSiteData = `"storage"`

const noSessionPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>WebFig</title>` +
	`<body style="font:16px/1.45 system-ui,sans-serif;background:#0E1420;color:#F1F5F9;display:grid;place-items:center;height:90vh;margin:0;padding:16px;text-align:center"><p>Open WebFig from the router page in the admin.</p>`

func plain(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if msg == "" {
		_, _ = w.Write([]byte(noSessionPage))
		return
	}
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>WebFig</title>` +
		`<body style="font:16px/1.45 system-ui,sans-serif;background:#0E1420;color:#F1F5F9;display:grid;place-items:center;height:90vh;margin:0;padding:16px;text-align:center"><p>` + msg + `</p>`))
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

func (m *Manager) serve(rw http.ResponseWriter, r *http.Request) {
	lw := &logWriter{ResponseWriter: rw}
	w := http.ResponseWriter(lw)
	rec := reqRecord{start: time.Now()}
	defer func() { m.logRequest(r, lw, &rec) }()
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
		// The session ended: drop the cookie and what the router left in this browser, but only on a page load.
		// A script or /jsproxy call that left with the previous cookie and lands after the owner opened WebFig
		// again would otherwise delete the NEW cookie (same name and path) and clear the new page's storage.
		if cv != "" && isNavigation(r) {
			http.SetCookie(w, m.cookie("", -1))
			w.Header().Set("Clear-Site-Data", clearSiteData)
		}
		rec.note = "no session"
		plain(w, http.StatusUnauthorized, "")
		return
	}
	// Static WebFig files are served from the cache when we have them: no slot, no router call. The
	// session was checked above, before this lookup, and the key holds the router.
	static := m.cache != nil && staticRequest(r)
	var ver string
	if m.o.Version != nil {
		ver = m.o.Version(sess.SiteID)
	}
	if static {
		if e := m.cache.get(sess.SiteID, ver, r); e != nil {
			rec.site, rec.note = sess.SiteID, "cache hit"
			m.serveCached(w, e)
			m.touch(sess.hash)
			return
		}
	}
	// /jsproxy calls are long-lived: they have their own small pools so the files never wait behind them.
	rec.site = sess.SiteID
	waitStart := time.Now()
	release, ok := m.acquireWait(r.Context(), sess.SiteID, sess.hash, isJSProxy(r))
	rec.wait = time.Since(waitStart)
	if !ok {
		rec.note = "no slot"
		if r.Context().Err() != nil {
			rec.note = "client left while waiting for a slot"
		}
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
		ModifyResponse: func(resp *http.Response) error {
			// Decide on the router's own headers before fixResponse replaces them.
			var cc string
			var keep bool
			if static {
				cc, keep = staticPolicy(resp)
			}
			if err := m.fixResponse(resp, tg); err != nil {
				return err
			}
			if cc != "" {
				resp.Header.Set("Cache-Control", cc)
			}
			// Kept only when the end of the body can be checked: a known length, or chunked (a cut chunked body
			// is an error, not EOF). A body that ends when the connection closes could be cut short and still
			// look complete, and would then be served to every session of this router for a day.
			framed := resp.ContentLength >= 0 || (len(resp.TransferEncoding) > 0 && resp.TransferEncoding[0] == "chunked")
			if keep && framed && len(resp.Header.Values("Content-Length")) <= 1 {
				e := &cacheEntry{key: cacheKey(sess.SiteID, ver, r), site: sess.SiteID, ver: ver,
					ctype: resp.Header.Get("Content-Type"), enc: resp.Header.Get("Content-Encoding"),
					etag: resp.Header.Get("Etag"), lastMod: resp.Header.Get("Last-Modified")}
				resp.Body = &captureBody{rc: resp.Body, want: resp.ContentLength, done: func(b []byte) {
					e.body = b
					m.cache.put(e)
				}}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rec.upErr = err
			if r.Context().Err() != nil {
				rec.note = "client went away"
				return // the browser went away (Chrome cancels freely): nothing to answer
			}
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

// isNavigation: a top-level page load (Sec-Fetch-* are sent by every current browser; a
// browser without them is treated as a navigation, as before).
func isNavigation(r *http.Request) bool {
	mode := r.Header.Get("Sec-Fetch-Mode")
	return mode == "" || mode == "navigate"
}

func isJSProxy(r *http.Request) bool {
	return r.URL.Path == "/jsproxy" || strings.HasPrefix(r.URL.Path, "/jsproxy/")
}

// serveCached answers from the cache. baseHeaders already ran; the router's
// own responses carry no nosniff (see fixResponse), so neither do these.
func (m *Manager) serveCached(w http.ResponseWriter, e *cacheEntry) {
	h := w.Header()
	h.Del("X-Content-Type-Options")
	h.Set("Cache-Control", StaticCacheControl)
	if e.ctype != "" {
		h.Set("Content-Type", e.ctype)
	}
	if e.enc != "" {
		h.Set("Content-Encoding", e.enc)
		h.Add("Vary", "Accept-Encoding")
	}
	if e.etag != "" {
		h.Set("Etag", e.etag)
	}
	if e.lastMod != "" {
		h.Set("Last-Modified", e.lastMod)
	}
	h.Set("Content-Length", strconv.Itoa(len(e.body)))
	h.Set("X-Ftn-Cache", "hit")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.body)
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
	// No nosniff on the router's own responses: RouterOS serves some WebFig scripts
	// with a content type Chrome would then refuse to run, leaving WebFig on
	// "Connecting". The WebFig host is its own origin, so this exposes nothing else.
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
