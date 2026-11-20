package web_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
	"ispbill/internal/webfig"
)

const wfDomain = "tech.example.co.ke"

// wfRig is the admin app with remote WebFig on, a fake WebFig-like router web
// server, and the WebFig host's handler on its own test listener.
type wfRig struct {
	*app
	tech     *httptest.Server
	up       *httptest.Server
	hits     atomic.Int64
	dialMu   sync.Mutex
	dialed   []string
	hold     chan struct{}
	now      atomic.Int64 // unix nanos; 0 = real time
	clientIP string
	holding  atomic.Bool // a test holds requests in flight on purpose
}

func (r *wfRig) clock() time.Time {
	if n := r.now.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Now()
}
func (r *wfRig) advance(d time.Duration) { r.now.Store(r.clock().Add(d).UnixNano()) }

// wfSlotWait keeps the tests quick: in production a request waits webfig.SlotWait.
const wfSlotWait = 400 * time.Millisecond

func newWF(t *testing.T) *wfRig {
	t.Helper()
	r := &wfRig{hold: make(chan struct{}), clientIP: "203.0.113.5"}
	r.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.hits.Add(1)
		switch q.URL.Path {
		case "/jsproxy":
			b, _ := io.ReadAll(q.Body)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(append([]byte("echo:"), b...))
		case "/redir":
			http.Redirect(w, q, "http://"+q.Host+"/webfig/", http.StatusFound)
		case "/redir-bs":
			w.Header().Set("Location", `/\evil.example/x`)
			w.WriteHeader(302)
		case "/refresh":
			w.Header().Set("Refresh", "0; url=http://evil.example/")
			w.Header().Set("Service-Worker-Allowed", "/")
			fmt.Fprint(w, "ok")
		case "/redir-out":
			w.Header().Set("Location", "http://evil.example/steal")
			w.WriteHeader(302)
		case "/setcookie":
			w.Header().Add("Set-Cookie", "sid=abc; Domain=.example.co.ke; Path=/")
			w.Header().Add("Set-Cookie", "__Host-ftn_wf=evil; Path=/")
			w.Header().Set("Cache-Control", "public, max-age=999")
			w.Header().Set("Content-Security-Policy", "frame-ancestors *")
			fmt.Fprint(w, "ok")
		case "/seen":
			h := map[string]any{"host": q.Host, "headers": q.Header, "uri": q.RequestURI}
			json.NewEncoder(w).Encode(h)
		case "/hold":
			<-r.hold
			fmt.Fprint(w, "held")
		case "/webfig/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<!doctype html><title>WebFig</title><button id="go">go</button><pre id="out"></pre>`+
				`<script>document.getElementById("go").onclick=async()=>{const r=await fetch("/jsproxy",{method:"POST",body:"hello-jsproxy"});document.getElementById("out").textContent=await r.text()}</script>`)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<!doctype html><title>WebFig</title><body id="webfig">RouterOS WebFig on %s</body>`, q.Host)
		}
	}))
	t.Cleanup(r.up.Close)
	t.Cleanup(func() {
		select {
		case <-r.hold:
		default:
			close(r.hold)
		}
	})
	r.app = newAppCfg(t, true, func(c *config.Config) {
		c.WebFigDomain, c.WebFigListen = wfDomain, "127.0.0.1:8082"
		c.PublicBaseURL = "https://isp.example.co.ke"
		c.WGSubnet = "10.99.0.0/22"
	})
	r.srv.SetWebFigClock(r.clock)
	r.srv.SetWebFigSlotWait(wfSlotWait)
	upAddr := r.up.Listener.Addr().String()
	r.srv.SetWebFigDial(func(ctx context.Context, network, addr string) (net.Conn, error) {
		r.dialMu.Lock()
		r.dialed = append(r.dialed, addr)
		r.dialMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, upAddr)
	})
	r.tech = httptest.NewServer(r.srv.WebFigHandler())
	t.Cleanup(r.tech.Close)
	return r
}

func (r *wfRig) dials() []string {
	r.dialMu.Lock()
	defer r.dialMu.Unlock()
	return append([]string(nil), r.dialed...)
}

// adminReq is an admin request as browser b from the rig's client address.
func (r *wfRig) adminReq(b *browser, method, path string, form url.Values) *http.Response {
	var rd io.Reader
	if method == "POST" {
		form.Set("_csrf", b.CSRF)
		rd = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, r.ts.URL+path, rd)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("X-Forwarded-For", r.clientIP)
	req.AddCookie(b.Cookie)
	resp, err := r.cl.Do(req)
	if err != nil {
		panic(err)
	}
	return resp
}

// stepUp posts the router's WebFig form with the password and a fresh code.
func (r *wfRig) stepUp(t *testing.T, b *browser, w *who, site string) *http.Response {
	t.Helper()
	return r.adminReq(b, "POST", "/admin/routers/webfig", url.Values{"id": {site}, "password": {w.Pass}, "code": {w.Code(t, r.st)}})
}

func ticketOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("expected a redirect to the WebFig host, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil || u.Scheme != "https" || u.Host != wfDomain || u.Path != "/_ftn/open" || u.Query().Get("t") == "" {
		t.Fatalf("bad WebFig redirect %q", loc)
	}
	return u.Query().Get("t")
}

// tech sends a request to the WebFig host listener.
func (r *wfRig) techReq(method, path string, ip string, body io.Reader, hdr map[string]string, cookies ...*http.Cookie) *http.Response {
	req, _ := http.NewRequest(method, r.tech.URL+path, body)
	req.Host = wfDomain
	if ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		panic(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	// The proxy touches the session after the response; wait for that before the
	// test moves its fake clock (requests held on purpose stay in flight: no wait then).
	if !strings.HasPrefix(path, "/hold") {
		for i := 0; i < 500 && r.srv.WebFigBusy() > 0 && !r.holding.Load(); i++ {
			time.Sleep(2 * time.Millisecond)
		}
	}
	return resp
}

// open does the whole step-up and ticket exchange and returns the WebFig cookie.
func (r *wfRig) open(t *testing.T, b *browser, w *who, site string, old ...*http.Cookie) *http.Cookie {
	t.Helper()
	tok := ticketOf(t, r.stepUp(t, b, w, site))
	resp := r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil, old...)
	defer resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("open: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_wf") && c.Value != "" {
			return c
		}
	}
	t.Fatal("no WebFig cookie")
	return nil
}

func (r *wfRig) routerPage(t *testing.T) string {
	resp := r.adminReq(r.br, "GET", "/admin/routers/view?id="+r.site, nil)
	return body(t, resp)
}

func TestWebFigOffByDefault(t *testing.T) {
	a := newApp(t, true)
	if a.srv.WebFigHandler() != nil {
		t.Fatal("a WebFig handler exists without WEBFIG_DOMAIN")
	}
	resp := a.admin("GET", "/admin/routers/webfig?id="+a.site, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if strings.Contains(a.adminGet(t, "/admin/routers/view?id="+a.site), "Open WebFig") {
		t.Fatal("the router page offers WebFig while it is off")
	}
}

func TestWebFigOwnersOnly(t *testing.T) {
	r := newWF(t)
	for _, role := range []string{store.RoleOperator, store.RoleViewer} {
		w := seedUser(t, r.srv, r.st, "u-"+role, role, "pw-"+role+"-123")
		b := signIn(t, r.ts, r.srv, r.st, w)
		for _, m := range []string{"GET", "POST"} {
			resp := r.adminReq(b, m, "/admin/routers/webfig?id="+r.site, url.Values{"id": {r.site}, "password": {w.Pass}, "code": {w.Code(t, r.st)}})
			if resp.StatusCode != 403 {
				t.Fatalf("%s %s: got %d, want 403", role, m, resp.StatusCode)
			}
			resp.Body.Close()
		}
		resp := r.adminReq(b, "POST", "/admin/routers/webfig/end", url.Values{"id": {r.site}, "sid": {"x"}})
		if resp.StatusCode != 403 {
			t.Fatalf("%s end: %d", role, resp.StatusCode)
		}
		resp.Body.Close()
		if page := body(t, r.adminReq(b, "GET", "/admin/routers/view?id="+r.site, nil)); strings.Contains(page, "Open WebFig") || strings.Contains(page, "data-webfig") {
			t.Fatalf("%s sees WebFig on the router page", role)
		}
	}
	// the old address is gated the same way
	w := seedUser(t, r.srv, r.st, "op2", store.RoleOperator, "pw-op2-123456")
	b := signIn(t, r.ts, r.srv, r.st, w)
	resp := r.adminReq(b, "POST", "/admin/sites/webfig", url.Values{"id": {r.site}})
	if resp.StatusCode != 403 {
		t.Fatalf("old address: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWebFigStepUp(t *testing.T) {
	r := newWF(t)
	page := r.routerPage(t)
	if !strings.Contains(page, "Open WebFig") {
		t.Fatal("owner does not see the Open WebFig button")
	}
	// GET never mints a ticket: it shows the form.
	resp := r.adminReq(r.br, "GET", "/admin/routers/webfig?id="+r.site, nil)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, `name="password"`) || !strings.Contains(b, `name="code"`) {
		t.Fatalf("GET: %d %s", resp.StatusCode, b)
	}
	// POST without credentials: the form again, no redirect.
	resp = r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}})
	if resp.StatusCode != 200 || resp.Header.Get("Location") != "" {
		t.Fatalf("no credentials: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// wrong password, wrong code
	resp = r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}, "password": {"nope-nope-1"}, "code": {r.owner.Code(t, r.st)}})
	if resp.StatusCode != 401 || resp.Header.Get("Location") != "" {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}, "password": {pw}, "code": {"000000"}})
	if resp.StatusCode != 401 || resp.Header.Get("Location") != "" {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// right both: ticket
	ticketOf(t, r.stepUp(t, r.br, r.owner, r.site))
	// fresh: no credentials needed for the next ten minutes
	ticketOf(t, r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}}))
	r.advance(9 * time.Minute)
	ticketOf(t, r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}}))
	r.advance(2 * time.Minute) // 11 minutes since the step-up
	resp = r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}})
	if resp.StatusCode != 200 || resp.Header.Get("Location") != "" {
		t.Fatalf("stale step-up still opened WebFig: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// another admin session of the same user does not inherit the step-up
	b2 := signIn(t, r.ts, r.srv, r.st, r.owner)
	resp = r.adminReq(b2, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}})
	if resp.StatusCode != 200 || resp.Header.Get("Location") != "" {
		t.Fatalf("second session inherited the step-up: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// unknown router
	resp = r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {"s000000"}})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown router: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestWebFigTicketSingleUseExpiryAndAddress(t *testing.T) {
	r := newWF(t)
	tok := ticketOf(t, r.stepUp(t, r.br, r.owner, r.site))
	// another client address cannot use it, and that burns it
	resp := r.techReq("GET", "/_ftn/open?t="+tok, "198.51.100.9", nil, nil)
	if resp.StatusCode != 403 {
		t.Fatalf("other address: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil)
	if resp.StatusCode != 403 {
		t.Fatalf("a ticket survived a wrong-address try: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// single use
	tok = ticketOf(t, r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}}))
	resp = r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" {
		t.Fatalf("first use: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()
	resp = r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil)
	if resp.StatusCode != 403 {
		t.Fatalf("second use: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// expiry at 60 seconds
	tok = ticketOf(t, r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}}))
	r.advance(61 * time.Second)
	resp = r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil)
	if resp.StatusCode != 403 {
		t.Fatalf("expired ticket: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// garbage, empty, POST
	for _, q := range []string{"", "?t=", "?t=abc", "?t=" + strings.Repeat("A", 5000)} {
		resp = r.techReq("GET", "/_ftn/open"+q, r.clientIP, nil, nil)
		if resp.StatusCode != 403 {
			t.Fatalf("open%.10s: %d", q, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = r.techReq("POST", "/_ftn/open?t="+tok, r.clientIP, strings.NewReader("x"), nil)
	if resp.StatusCode != 405 {
		t.Fatalf("POST open: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if r.hits.Load() != 0 {
		t.Fatal("the router was reached without a session")
	}
}

func TestWebFigCookieScopeAndNoLeaks(t *testing.T) {
	r := newWF(t)
	// The admin's own cookies are host-only: __Host- prefix, no Domain.
	if !strings.HasPrefix(r.br.Cookie.Name, "__Host-") {
		t.Fatalf("admin cookie %q is not __Host-", r.br.Cookie.Name)
	}
	for _, c := range []*http.Cookie{r.br.Cookie, r.br.Known} {
		if c == nil {
			continue
		}
		if c.Domain != "" || !c.Secure || !c.HttpOnly || c.Path != "/" || !strings.HasPrefix(c.Name, "__Host-") {
			t.Fatalf("admin cookie %s is not host-only: %+v", c.Name, c)
		}
	}
	wc := r.open(t, r.br, r.owner, r.site)
	if wc.Name != "__Host-ftn_wf" || wc.Domain != "" || !wc.Secure || !wc.HttpOnly || wc.Path != "/" || wc.SameSite != http.SameSiteStrictMode {
		t.Fatalf("WebFig cookie not host-only/Secure/HttpOnly/Strict: %+v", wc)
	}
	if wc.MaxAge != 7200 {
		t.Fatalf("max age %d", wc.MaxAge)
	}
	// Even if a browser sent the admin cookies to the tech host, none reach the router.
	resp := r.techReq("GET", "/seen", r.clientIP, nil, map[string]string{"Authorization": "Basic Zm9v"}, wc, r.br.Cookie, r.br.Known,
		&http.Cookie{Name: "router_pref", Value: "keep"})
	var seen struct {
		Headers map[string][]string `json:"headers"`
	}
	json.Unmarshal([]byte(body(t, resp)), &seen)
	ck := strings.Join(seen.Headers["Cookie"], ";")
	if strings.Contains(ck, "ftn") || strings.Contains(ck, r.br.Cookie.Value) || strings.Contains(ck, wc.Value) {
		t.Fatalf("our cookies reached the router: %q", ck)
	}
	if !strings.Contains(ck, "router_pref=keep") {
		t.Fatalf("the router's own cookie was dropped: %q", ck)
	}
	if got := seen.Headers["Authorization"]; len(got) != 1 {
		t.Fatalf("the router's own login header was touched: %v", got)
	}
	// What the router sets: no Domain, Secure, never our cookie name; headers hardened.
	resp = r.techReq("GET", "/setcookie", r.clientIP, nil, nil, wc)
	resp.Body.Close()
	var names []string
	for _, c := range resp.Cookies() {
		names = append(names, c.Name)
		if c.Domain != "" || !c.Secure {
			t.Fatalf("router cookie %+v keeps a Domain or lacks Secure", c)
		}
	}
	if strings.Join(names, ",") != "sid" {
		t.Fatalf("cookies passed on: %v", names)
	}
	for h, want := range map[string]string{"Cache-Control": "no-store", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer"} {
		if v := resp.Header.Values(h); len(v) != 1 || v[0] != want {
			t.Fatalf("%s: %v (want exactly %q)", h, v, want)
		}
	}
	// The admin side refuses a request that says it is same-site but not same-origin (the WebFig host).
	resp = r.adminReq(r.br, "POST", "/admin/routers/auto-fixes", url.Values{"id": {r.site}, "paused": {"1"}})
	resp.Body.Close()
	req, _ := http.NewRequest("POST", r.ts.URL+"/admin/routers/auto-fixes", strings.NewReader(url.Values{"id": {r.site}, "paused": {"1"}, "_csrf": {r.br.CSRF}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("Origin", "https://"+wfDomain)
	req.AddCookie(r.br.Cookie)
	resp, _ = r.cl.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("a same-site request from the WebFig host was accepted by the admin: %d", resp.StatusCode)
	}
}

func TestWebFigProxy(t *testing.T) {
	r := newWF(t)
	// no session: a plain page, the router is never reached
	resp := r.techReq("GET", "/webfig/", r.clientIP, nil, nil)
	if b := body(t, resp); resp.StatusCode != 401 || !strings.Contains(b, "Open WebFig from the router page in the admin") {
		t.Fatalf("no session: %d %s", resp.StatusCode, b)
	}
	if r.hits.Load() != 0 {
		t.Fatal("reached the router without a session")
	}
	wc := r.open(t, r.br, r.owner, r.site)
	resp = r.techReq("GET", "/", r.clientIP, nil, nil, wc)
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "RouterOS WebFig on 10.99.0.2:8081") {
		t.Fatalf("GET /: %d %s", resp.StatusCode, b)
	}
	// /jsproxy POST body is passed through
	resp = r.techReq("POST", "/jsproxy", r.clientIP, strings.NewReader("sealed-bytes"), nil, wc)
	if b := body(t, resp); b != "echo:sealed-bytes" {
		t.Fatalf("jsproxy: %q", b)
	}
	// Redirects stay on the WebFig host; an outside one is dropped
	resp = r.techReq("GET", "/redir", r.clientIP, nil, nil, wc)
	resp.Body.Close()
	if resp.Header.Get("Location") != "/webfig/" {
		t.Fatalf("Location %q", resp.Header.Get("Location"))
	}
	resp = r.techReq("GET", "/redir-out", r.clientIP, nil, nil, wc)
	resp.Body.Close()
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("outside Location %q", resp.Header.Get("Location"))
	}
	// The router sees its own address as Host and no forwarding or origin details.
	resp = r.techReq("GET", "/seen?x=1", r.clientIP, nil, map[string]string{
		"X-Forwarded-Host": "evil", "X-Forwarded-Proto": "gopher", "Forwarded": "for=evil", "X-Real-Ip": "1.2.3.4",
		"Origin": "https://" + wfDomain, "Referer": "https://" + wfDomain + "/x", "Connection": "X-Secret", "X-Secret": "s",
	}, wc)
	var seen struct {
		Host    string
		Headers map[string][]string
		URI     string
	}
	json.Unmarshal([]byte(body(t, resp)), &seen)
	if seen.Host != "10.99.0.2:8081" || seen.URI != "/seen?x=1" {
		t.Fatalf("upstream saw host %q uri %q", seen.Host, seen.URI)
	}
	for _, h := range []string{"X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded", "Origin", "Referer", "X-Secret"} {
		if len(seen.Headers[h]) != 0 {
			t.Fatalf("header %s reached the router: %v", h, seen.Headers[h])
		}
	}
	if xf := strings.Join(seen.Headers["X-Forwarded-For"], ","); strings.Contains(xf, "evil") {
		t.Fatalf("spoofed forwarding reached the router: %q", xf)
	}
	// everything dialled was the one router address
	for _, d := range r.dials() {
		if d != "10.99.0.2:8081" {
			t.Fatalf("dialled %q", d)
		}
	}
	// reserved paths are never proxied
	resp = r.techReq("GET", "/_ftn/anything", r.clientIP, nil, nil, wc)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("/_ftn/anything: %d", resp.StatusCode)
	}
	// unreachable router: a plain 502
	r.up.Close()
	resp = r.techReq("GET", "/", r.clientIP, nil, nil, wc)
	if b := body(t, resp); resp.StatusCode != 502 || !strings.Contains(b, "did not answer") {
		t.Fatalf("router down: %d %s", resp.StatusCode, b)
	}
}

// rawTech sends raw bytes to the WebFig host listener and reads one response.
func (r *wfRig) rawTech(t *testing.T, req string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", r.tech.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, req)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return 0, err.Error()
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWebFigCannotBeAimedElsewhere(t *testing.T) {
	r := newWF(t)
	wc := r.open(t, r.br, r.owner, r.site)
	ck := "Cookie: " + wc.Name + "=" + wc.Value + "\r\n"
	other := r.up.Listener.Addr().String()
	cases := []struct{ name, req string }{
		{"absolute-form to another host", "GET http://127.0.0.1:1/ HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"absolute-form to the upstream's real address", "GET http://" + other + "/seen HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"absolute-form to a router tunnel address", "GET http://10.99.0.3:8081/ HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"host header of another site", "GET / HTTP/1.1\r\nHost: evil.example\r\n" + ck + "\r\n"},
		{"host header with the router address", "GET / HTTP/1.1\r\nHost: 10.99.0.3:8081\r\n" + ck + "\r\n"},
		{"host header with userinfo", "GET / HTTP/1.1\r\nHost: " + wfDomain + "@evil.example\r\n" + ck + "\r\n"},
		{"host suffix trick", "GET / HTTP/1.1\r\nHost: " + wfDomain + ".evil.example\r\n" + ck + "\r\n"},
		{"CONNECT", "CONNECT 10.99.0.3:22 HTTP/1.1\r\nHost: 10.99.0.3:22\r\n" + ck + "\r\n"},
		{"dot-dot", "GET /../_ftn/x HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"encoded dot-dot", "GET /%2e%2e/etc HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"dot-dot inside", "GET /a/../../b HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"backslash", "GET /a%5c..%5cb HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"double slash host", "GET //evil.example/x HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
		{"asterisk", "OPTIONS * HTTP/1.1\r\nHost: " + wfDomain + "\r\n" + ck + "\r\n"},
	}
	for _, c := range cases {
		before := r.hits.Load()
		code, _ := r.rawTech(t, c.req)
		if c.name == "asterisk" {
			code = 400 // net/http itself answers OPTIONS * before any handler; the check is that the router is not reached
		}
		if code < 400 && code != 0 {
			t.Errorf("%s: accepted with %d", c.name, code)
		}
		if r.hits.Load() != before {
			t.Errorf("%s: reached the router", c.name)
		}
	}
	for _, d := range r.dials() {
		if d != "10.99.0.2:8081" {
			t.Fatalf("dialled %q", d)
		}
	}
	// A session belongs to its own router even when another one exists.
	b := seedSite(t, r.st, "Block B") // 10.99.0.3
	r.mgr.Reload(b)
	resp := r.techReq("GET", "/seen?site="+b, r.clientIP, nil, nil, wc)
	var seen struct{ Host string }
	json.Unmarshal([]byte(body(t, resp)), &seen)
	if seen.Host != "10.99.0.2:8081" {
		t.Fatalf("the session reached %q", seen.Host)
	}
}

func TestWebFigSessionLifetimeReplaceEndAndActivity(t *testing.T) {
	r := newWF(t)
	wc := r.open(t, r.br, r.owner, r.site)
	page := r.routerPage(t)
	for _, want := range []string{"WebFig access", "admin", r.clientIP, "/admin/routers/webfig/end"} {
		if !strings.Contains(page, want) {
			t.Fatalf("router page lacks %q", want)
		}
	}
	// a new session in the same browser replaces the first
	wc2 := r.open(t, r.br, r.owner, r.site, wc)
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc); resp.StatusCode != 401 {
		t.Fatalf("replaced session still works: %d", resp.StatusCode)
	}
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc2); resp.StatusCode != 200 {
		t.Fatalf("new session: %d", resp.StatusCode)
	}
	// idle: 30 minutes
	r.advance(29 * time.Minute)
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc2); resp.StatusCode != 200 {
		t.Fatalf("29 minutes: %d", resp.StatusCode)
	}
	r.advance(29 * time.Minute)
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc2); resp.StatusCode != 200 {
		t.Fatalf("activity should keep it alive: %d", resp.StatusCode)
	}
	r.advance(31 * time.Minute)
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc2); resp.StatusCode != 401 {
		t.Fatalf("idle session still works: %d", resp.StatusCode)
	}
	// 2 hour cap even with constant use
	wc3 := r.open(t, r.br, r.owner, r.site)
	for i := 0; i < 5; i++ {
		r.advance(25 * time.Minute)
		resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc3)
		resp.Body.Close()
		if i == 4 && resp.StatusCode != 401 {
			t.Fatalf("after 2h05m the session still works: %d", resp.StatusCode)
		}
	}
	// owner ends a session from the router page
	wc4 := r.open(t, r.br, r.owner, r.site)
	page = r.routerPage(t)
	i := strings.Index(page, `name="sid" value="`)
	if i < 0 {
		t.Fatal("no End button")
	}
	sid := page[i+len(`name="sid" value="`):]
	sid = sid[:strings.Index(sid, `"`)]
	resp := r.adminReq(r.br, "POST", "/admin/routers/webfig/end", url.Values{"id": {r.site}, "sid": {sid}})
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("end: %d", resp.StatusCode)
	}
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc4); resp.StatusCode != 401 {
		t.Fatalf("ended session still works: %d", resp.StatusCode)
	}
	// ending something that is not there
	resp = r.adminReq(r.br, "POST", "/admin/routers/webfig/end", url.Values{"id": {r.site}, "sid": {"nope"}})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "Error") {
		t.Fatalf("ending a missing session: %q", loc)
	}
	// the activity record: who, when, IP, how it ended
	var msgs []string
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == r.site && strings.HasPrefix(e.Msg, "WebFig ") {
				if e.Actor != "admin" || e.IP != r.clientIP || e.Time.IsZero() {
					t.Errorf("event without who/IP/time: %+v", e)
				}
				msgs = append(msgs, e.Msg)
			}
		}
	})
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"WebFig opened on Block A", "replaced by a new WebFig session", "idle for 30 minutes", "2 hour limit", "ended by an owner (admin)"} {
		if !strings.Contains(all, want) {
			t.Errorf("activity lacks %q in:\n%s", want, all)
		}
	}
	if !strings.Contains(r.routerPage(t), "Recent activity") {
		t.Error("router page has no activity")
	}
}

func TestWebFigSweepEndsTimedOutSessions(t *testing.T) {
	r := newWF(t)
	r.open(t, r.br, r.owner, r.site)
	r.advance(31 * time.Minute)
	r.srv.WebFigSweep()
	n := 0
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if strings.Contains(e.Msg, "ended: idle for 30 minutes") {
				n++
			}
		}
	})
	if n != 1 {
		t.Fatalf("sweep recorded %d idle ends", n)
	}
}

func TestWebFigSessionDiesWithTheOwnerRoleOrRouter(t *testing.T) {
	r := newWF(t)
	w2 := seedUser(t, r.srv, r.st, "second", store.RoleOperator, "pw-second-123")
	r.st.Update(func(d *store.Data) error {
		u := d.AdminUsers[w2.ID]
		u.Role = store.RoleOwner
		d.AdminUsers[w2.ID] = u
		return nil
	})
	b2 := signIn(t, r.ts, r.srv, r.st, w2)
	wc := r.open(t, b2, w2, r.site)
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	r.st.Update(func(d *store.Data) error {
		u := d.AdminUsers[w2.ID]
		u.Role = store.RoleOperator
		d.AdminUsers[w2.ID] = u
		return nil
	})
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc); resp.StatusCode != 401 {
		t.Fatalf("demoted user kept WebFig: %d", resp.StatusCode)
	}
	wc = r.open(t, r.br, r.owner, r.site)
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.site]
		s.Removed = true
		d.Sites[r.site] = s
		return nil
	})
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc); resp.StatusCode != 401 {
		t.Fatalf("removed router kept WebFig: %d", resp.StatusCode)
	}
}

func TestWebFigConnectionLimits(t *testing.T) {
	r := newWF(t)
	wc := r.open(t, r.br, r.owner, r.site)
	r.holding.Store(true)
	var wg sync.WaitGroup
	for i := 0; i < webfig.PerRouterMax; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := r.techReq("GET", "/hold", r.clientIP, nil, nil, wc)
			resp.Body.Close()
		}()
	}
	waitFor(t, func() bool { return len(r.dials()) >= webfig.PerRouterMax })
	// one over the limit waits for a slot, then gives up with 503
	start := time.Now()
	resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc)
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("request over the limit on one router: %d", resp.StatusCode)
	}
	if time.Since(start) < wfSlotWait {
		t.Fatalf("request over the limit was refused without waiting")
	}
	// the REST budget is separate: the admin keeps working while WebFig is busy
	if resp := r.adminReq(r.br, "GET", "/admin/routers/view?id="+r.site, nil); resp.StatusCode != 200 {
		t.Fatalf("admin page while WebFig is busy: %d", resp.StatusCode)
	}
	// another router still has its own slots
	id2 := seedSite(t, r.st, "Block B")
	resp2 := r.techReq("GET", "/", r.clientIP, nil, nil, r.open(t, r.br, r.owner, id2))
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("second router while the first is full: %d", resp2.StatusCode)
	}
	// a request that arrives while full is served once a slot frees up
	got := make(chan int, 1)
	go func() {
		resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc)
		resp.Body.Close()
		got <- resp.StatusCode
	}()
	time.Sleep(wfSlotWait / 4)
	close(r.hold)
	if code := <-got; code != 200 {
		t.Fatalf("waiting request after release: %d", code)
	}
	wg.Wait()
}

func TestWebFigTotalLimit(t *testing.T) {
	r := newWF(t)
	full := webfig.TotalMax / webfig.PerRouterMax
	ids := []string{r.site}
	for i := 0; i < full; i++ {
		ids = append(ids, seedSite(t, r.st, fmt.Sprintf("Block %d", i)))
	}
	cks := map[string]*http.Cookie{}
	for _, id := range ids {
		cks[id] = r.open(t, r.br, r.owner, id)
	}
	r.holding.Store(true)
	var wg sync.WaitGroup
	for _, id := range ids[:full] {
		for i := 0; i < webfig.PerRouterMax; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp := r.techReq("GET", "/hold", r.clientIP, nil, nil, cks[id])
				resp.Body.Close()
			}()
		}
	}
	waitFor(t, func() bool { return len(r.dials()) >= webfig.TotalMax })
	resp := r.techReq("GET", "/", r.clientIP, nil, nil, cks[ids[full]])
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("request over the overall limit: %d", resp.StatusCode)
	}
	close(r.hold)
	wg.Wait()
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestWebFigURLInTheRouterAPI(t *testing.T) {
	on := newMultiOps(t, func(c *config.Config) { c.WebFigDomain = wfDomain }, nil)
	site := on.addSite(t, "Alpha")
	on.st.Update(func(d *store.Data) error {
		x := d.Sites[site.ID]
		x.TunnelIP = "10.99.0.12"
		d.Sites[site.ID] = x
		return nil
	})
	on.mgr.Reload(site.ID)
	tok := on.apiLogin(t, on.owner, "p1")
	got := on.call(tok.Access, "GET", "/sites/"+site.ID, nil, nil).JSON(t)["webfig_url"]
	if got != "https://billing.example.co.ke/admin/routers/webfig?id="+site.ID {
		t.Fatalf("webfig_url %v", got)
	}
	// a router with no tunnel has none; neither does a server with the feature off
	blank := on.addSite(t, "Blank")
	if v := on.call(tok.Access, "GET", "/sites/"+blank.ID, nil, nil).JSON(t)["webfig_url"]; v != "" {
		t.Fatalf("router without a tunnel: %v", v)
	}
	off := newMulti(t)
	s2 := off.addSite(t, "Alpha")
	off.st.Update(func(d *store.Data) error {
		x := d.Sites[s2.ID]
		x.TunnelIP = "10.99.0.12"
		d.Sites[s2.ID] = x
		return nil
	})
	tok2 := off.apiLogin(t, off.owner, "p2")
	if v := off.call(tok2.Access, "GET", "/sites/"+s2.ID, nil, nil).JSON(t)["webfig_url"]; v != "" {
		t.Fatalf("feature off: %v", v)
	}
}

func TestWebFigOneOriginHardening(t *testing.T) {
	r := newWF(t)
	wc := r.open(t, r.br, r.owner, r.site)
	// the open response wipes what an earlier router left in this browser
	tok := ticketOf(t, r.adminReq(r.br, "POST", "/admin/routers/webfig", url.Values{"id": {r.site}}))
	resp := r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil, wc)
	if got := resp.Header.Get("Clear-Site-Data"); got != `"cache", "storage"` {
		t.Fatalf("open: Clear-Site-Data %q", got)
	}
	wc = nil
	for _, c := range resp.Cookies() {
		if strings.HasSuffix(c.Name, "ftn_wf") && c.Value != "" {
			wc = c
		}
	}
	// a service worker script fetch is refused before the router sees it
	before := r.hits.Load()
	resp = r.techReq("GET", "/sw.js", r.clientIP, nil, map[string]string{"Service-Worker": "script"}, wc)
	if resp.StatusCode != 403 || r.hits.Load() != before {
		t.Fatalf("Service-Worker request: %d", resp.StatusCode)
	}
	// and a router cannot widen a worker's scope or refresh the page elsewhere
	resp = r.techReq("GET", "/refresh", r.clientIP, nil, nil, wc)
	if resp.Header.Get("Service-Worker-Allowed") != "" || resp.Header.Get("Refresh") != "" {
		t.Fatalf("headers kept: %v", resp.Header)
	}
	// slash-backslash is another site to a browser
	for _, p := range []string{"/redir-bs"} {
		resp = r.techReq("GET", p, r.clientIP, nil, nil, wc)
		if resp.Header.Get("Location") != "/" {
			t.Fatalf("%s: Location %q", p, resp.Header.Get("Location"))
		}
	}
	// the router's REST API is not WebFig
	before = r.hits.Load()
	for _, p := range []string{"/rest", "/rest/system/resource", "/rest/user"} {
		resp = r.techReq("GET", p, r.clientIP, nil, nil, wc)
		if resp.StatusCode != 403 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}
	if r.hits.Load() != before {
		t.Fatal("/rest reached the router")
	}
	// a session that ended wipes the browser's copy too
	r.advance(31 * time.Minute)
	resp = r.techReq("GET", "/", r.clientIP, nil, nil, wc)
	if resp.StatusCode != 401 || resp.Header.Get("Clear-Site-Data") != `"cache", "storage"` {
		t.Fatalf("ended session: %d %q", resp.StatusCode, resp.Header.Get("Clear-Site-Data"))
	}
	// a request with no cookie at all gets no wipe header
	if resp = r.techReq("GET", "/", r.clientIP, nil, nil); resp.Header.Get("Clear-Site-Data") != "" {
		t.Fatal("wipe header without a session cookie")
	}
}

func TestWebFigSessionEndsWithItsAdminSession(t *testing.T) {
	r := newWF(t)
	count := func(want string) int {
		n := 0
		r.st.View(func(d *store.Data) {
			for _, e := range d.Events {
				if strings.Contains(e.Msg, want) {
					n++
				}
			}
		})
		return n
	}
	// sign-out
	b2 := signIn(t, r.ts, r.srv, r.st, r.owner)
	wc := r.open(t, b2, r.owner, r.site)
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	resp := r.adminReq(b2, "POST", "/admin/logout", url.Values{})
	resp.Body.Close()
	if n := count("admin session that opened it ended"); n != 1 {
		t.Fatalf("sign-out was recorded %d times", n)
	}
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc); resp.StatusCode != 401 {
		t.Fatalf("WebFig survived the admin sign-out: %d", resp.StatusCode)
	}
	// the other admin session's WebFig is untouched
	wc1 := r.open(t, r.br, r.owner, r.site)
	b3 := signIn(t, r.ts, r.srv, r.st, r.owner)
	wc3 := r.open(t, b3, r.owner, r.site)
	_ = wc3
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc1); resp.StatusCode != 200 {
		t.Fatalf("another admin session's WebFig died: %d", resp.StatusCode)
	}
	// revoking every session (password change, revoke) ends it on the next request, with no sweep needed
	if _, err := r.srv.Auth().RevokeSessions(r.owner.ID, "", "test", "test", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Cookie{wc1, wc3} {
		if resp := r.techReq("GET", "/", r.clientIP, nil, nil, c); resp.StatusCode != 401 {
			t.Fatalf("WebFig survived revoked sessions: %d", resp.StatusCode)
		}
	}
	// plain expiry of the admin session
	b4 := signIn(t, r.ts, r.srv, r.st, r.owner)
	wc4 := r.open(t, b4, r.owner, r.site)
	sess, _, _ := r.srv.Auth().Session(b4.Cookie.Value)
	r.st.Update(func(d *store.Data) error {
		x := d.AdminSessions[sess.ID]
		x.ExpiresAt = time.Now().Add(-time.Minute)
		d.AdminSessions[sess.ID] = x
		return nil
	})
	if resp := r.techReq("GET", "/", r.clientIP, nil, nil, wc4); resp.StatusCode != 401 {
		t.Fatalf("WebFig survived the admin session's expiry: %d", resp.StatusCode)
	}
	// the sweep records an end nobody asked for
	b5 := signIn(t, r.ts, r.srv, r.st, r.owner)
	r.open(t, b5, r.owner, r.site)
	sess, _, _ = r.srv.Auth().Session(b5.Cookie.Value)
	r.srv.Auth().DeleteSession(sess.ID)
	before := count("admin session that opened it ended")
	r.srv.WebFigSweep()
	if count("admin session that opened it ended") != before+1 {
		t.Fatal("the sweep did not record the end")
	}
}

func TestWebFigNetworkChangedPage(t *testing.T) {
	r := newWF(t)
	tok := ticketOf(t, r.stepUp(t, r.br, r.owner, r.site))
	resp := r.techReq("GET", "/_ftn/open?t="+tok, "198.51.100.9", nil, nil)
	if b := body(t, resp); resp.StatusCode != 403 || !strings.Contains(b, "Your network changed between the admin and WebFig") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// the ticket is gone all the same
	resp = r.techReq("GET", "/_ftn/open?t="+tok, r.clientIP, nil, nil)
	if b := body(t, resp); resp.StatusCode != 403 || strings.Contains(b, "network changed") {
		t.Fatalf("second try: %d %s", resp.StatusCode, b)
	}
}

func TestWebFigURLOnlyForOwners(t *testing.T) {
	m := newMultiOps(t, func(c *config.Config) { c.WebFigDomain = wfDomain }, nil)
	site := m.addSite(t, "Alpha")
	m.st.Update(func(d *store.Data) error {
		x := d.Sites[site.ID]
		x.TunnelIP = "10.99.0.12"
		d.Sites[site.ID] = x
		return nil
	})
	m.mgr.Reload(site.ID)
	op := seedUser(t, m.srv, m.st, "olga", store.RoleOperator, longPW)
	vw := seedUser(t, m.srv, m.st, "vic", store.RoleViewer, longPW)
	for name, w := range map[string]*who{"owner": m.owner, "operator": op, "viewer": vw} {
		tok := m.apiLogin(t, w, "i-"+name)
		for _, path := range []string{"/sites/" + site.ID, "/sites"} {
			j := m.call(tok.Access, "GET", path, nil, nil).JSON(t)
			item := j
			if its, ok := j["items"].([]any); ok {
				item = its[0].(map[string]any)
				for _, it := range its {
					if it.(map[string]any)["id"] == site.ID {
						item = it.(map[string]any)
					}
				}
			}
			_, has := item["webfig_url"]
			if has != (name == "owner") {
				t.Errorf("%s %s: webfig_url present=%v", name, path, has)
			}
		}
	}
}

// Nothing a browser can be sent to with a plain link (a GET) may change state:
// router script on the WebFig host is same-site with the admin and its
// requests carry the admin's SameSite=Strict cookies.
func TestAdminGETsDoNotChangeState(t *testing.T) {
	r := newWF(t)
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`(?:\bget\(|"GET )"?(/admin[^" ?]*)`)
	paths := map[string]bool{"/admin/logout": true}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			paths[m[1]] = true
		}
	}
	if len(paths) < 40 {
		t.Fatalf("only %d admin GET routes found: the scan is broken", len(paths))
	}
	snap := func(skipAudit bool) string {
		var out string
		r.st.View(func(d *store.Data) {
			cp := *d
			cp.AdminSessions = nil // last-seen times move on every request
			if skipAudit {
				// A voucher sheet print is read-only but writes its audit line (security-fixes M-3).
				cp.Events, cp.EventSeq = nil, 0
			}
			b, _ := json.Marshal(cp)
			out = string(b)
		})
		return out
	}
	var list []string
	for p := range paths {
		list = append(list, p)
	}
	sort.Strings(list)
	t.Logf("%d admin GET routes: %s", len(list), strings.Join(list, " "))
	for _, p := range list {
		if p == "/admin/logout" || strings.Contains(p, "{") {
			continue
		}
		skip := p == "/admin/vouchers/print"
		before := snap(skip)
		resp := r.adminReq(r.br, "GET", p+"?id="+r.site, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if after := snap(skip); after != before {
			t.Errorf("GET %s changed stored data", p)
		}
	}
	// logout by GET only redirects
	resp := r.adminReq(r.br, "GET", "/admin/logout", nil)
	resp.Body.Close()
	if _, _, ok := r.srv.Auth().Session(r.br.Cookie.Value); !ok {
		t.Error("GET /admin/logout ended the session")
	}
}
