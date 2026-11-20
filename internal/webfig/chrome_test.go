package webfig

// Experimental diagnostics for "WebFig does not load in Chrome" (not for merge as-is).

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeROS is a raw TCP server answering every request with resp(path).
func fakeROS(t *testing.T, resp func(method, path string) string, hang chan struct{}) (addr string, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				if req.URL.Path == "/jsproxy" && hang != nil {
					// a long-poll the router holds until the client goes away
					_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
					_, _ = br.ReadByte()
					close(hang)
					return
				}
				_, _ = io.WriteString(c, resp(req.Method, req.URL.Path))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func diagManager(t *testing.T, addr string) (*Manager, string) {
	host, port, _ := net.SplitHostPort(addr)
	var p int
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	m := New(Options{Domain: "tech.example.co.ke", Secure: true,
		Resolve:    func(string) (Target, bool) { return Target{IP: host, Port: p}, true },
		UserOK:     func(string) bool { return true },
		AdminLive:  func(string) bool { return true },
		CacheBytes: -1})
	tok, err := m.Mint("u", "owner", "s1", "192.0.2.1", "adm")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := m.Redeem(tok, "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	return m, cookie
}

func doReq(m *Manager, method, path, cookie string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	r.Host = "tech.example.co.ke"
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieSecure, Value: cookie})
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	return w
}

// Candidate 1/2: RouterOS-style quirks (HTTP/1.0, Connection: close, Keep-Alive, no Content-Length,
// bare LF, gzip) must not reach the browser as connection-specific headers, and the body must arrive intact.
func TestDiagRouterQuirksAreSanitised(t *testing.T) {
	addr, stop := fakeROS(t, func(method, path string) string {
		switch path {
		case "/webfig/engine.js":
			return "HTTP/1.0 200 OK\nContent-Type: text/javascript; charset=\nConnection: close\nKeep-Alive: timeout=5\nProxy-Connection: close\nUpgrade: foo\nX-Dup: a\nX-Dup: b\n\nvar engine=1;"
		case "/webfig/list":
			return "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Encoding: gzip\r\nContent-Length: 4\r\nConnection: Keep-Alive\r\n\r\n\x1f\x8b\x08\x00"
		}
		return "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"
	}, nil)
	defer stop()
	m, ck := diagManager(t, addr)
	w := doReq(m, "GET", "/webfig/engine.js", ck)
	if w.Code != 200 || w.Body.String() != "var engine=1;" {
		t.Fatalf("engine: %d %q", w.Code, w.Body.String())
	}
	for _, h := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Upgrade", "Transfer-Encoding"} {
		if v := w.Header().Values(h); len(v) > 0 {
			t.Errorf("hop-by-hop %s reached the client: %v", h, v)
		}
	}
	t.Logf("engine headers to Caddy: %v", w.Header())
	w = doReq(m, "GET", "/webfig/list", ck)
	if w.Header().Get("Content-Encoding") != "gzip" || w.Body.Len() != 4 {
		t.Fatalf("gzip passthrough changed: %v %d", w.Header(), w.Body.Len())
	}
	t.Logf("list headers to Caddy: %v", w.Header())
}

// The same over HTTP/2 (TLS) from a Go h2 server, which, like Chrome, is strict about connection headers.
func TestDiagOverHTTP2(t *testing.T) {
	addr, stop := fakeROS(t, func(method, path string) string {
		return "HTTP/1.0 200 OK\nContent-Type: application/javascript\nConnection: close\nKeep-Alive: timeout=5\n\nvar x=1;"
	}, nil)
	defer stop()
	m, ck := diagManager(t, addr)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "tech.example.co.ke"
		m.Handler().ServeHTTP(w, r)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	c := ts.Client()
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	c.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	req, _ := http.NewRequest("GET", ts.URL+"/webfig/a.js", nil)
	req.AddCookie(&http.Cookie{Name: cookieSecure, Value: ck})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 || string(b) != "var x=1;" {
		t.Fatalf("proto %s body %q", resp.Proto, b)
	}
	t.Logf("h2 response headers: %v", resp.Header)
}

// Candidate 3/8: a request still in flight with the OLD session cookie (a tab left from an earlier open, or a
// request racing the new open) gets a 401 that deletes the cookie by name and sends Clear-Site-Data. The browser
// applies that to the NEW cookie (same name, same path): the fresh WebFig session is wiped.
func TestDiagStaleRequestWipesTheNewSession(t *testing.T) {
	addr, stop := fakeROS(t, func(string, string) string { return "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok" }, nil)
	defer stop()
	m, old := diagManager(t, addr)
	tok, _ := m.Mint("u", "owner", "s1", "192.0.2.1", "adm")
	fresh, _, err := m.Redeem(tok, "192.0.2.1", old) // the user opened WebFig again
	if err != nil {
		t.Fatal(err)
	}
	_ = fresh
	// The old tab's /jsproxy call (or a script request that left before the new open) lands now.
	w := doReq(m, "POST", "/jsproxy", old, "Sec-Fetch-Mode", "cors")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stale request: %d", w.Code)
	}
	if sc, csd := w.Header().Values("Set-Cookie"), w.Header().Get("Clear-Site-Data"); len(sc) != 0 || csd != "" {
		t.Fatalf("a stale script request deletes the new session's cookie / clears its storage: Set-Cookie=%v Clear-Site-Data=%q", sc, csd)
	}
	// A page load with the dead cookie still cleans up.
	w = doReq(m, "GET", "/webfig/", old, "Sec-Fetch-Mode", "navigate")
	if sc := w.Header().Values("Set-Cookie"); len(sc) != 1 || !strings.HasPrefix(sc[0], cookieSecure+"=;") {
		t.Fatalf("navigation with a dead cookie: Set-Cookie=%v", sc)
	}
	if csd := w.Header().Get("Clear-Site-Data"); strings.Contains(csd, "cache") {
		t.Fatalf("Clear-Site-Data still asks Chrome to clear its cache: %q", csd)
	}
	_ = fresh
}

// The request log: one safe line, nothing from the query string or cookies.
func TestDiagRequestLog(t *testing.T) {
	addr, stop := fakeROS(t, func(string, string) string { return "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok" }, nil)
	defer stop()
	var buf strings.Builder
	m, ck := diagManager(t, addr)
	m.o.Log = slog.New(slog.NewTextHandler(&buf, nil))
	doReq(m, "GET", "/webfig/engine-abc.js?secret=1", ck, "X-Ftn-Proto", "HTTP/3.0")
	doReq(m, "GET", "/_ftn/open?t=SECRETTICKET", "")
	out := buf.String()
	t.Log(out)
	for _, want := range []string{"path=/webfig/engine-abc.js", "status=200", "bytes=2", "proto=HTTP/3.0", "path=/_ftn/open", "status=403"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	for _, bad := range []string{"secret", "SECRETTICKET", ck} {
		if strings.Contains(out, bad) {
			t.Errorf("log leaks %q", bad)
		}
	}
	// A burst of plain lines is limited.
	buf.Reset()
	for i := 0; i < 100; i++ {
		doReq(m, "GET", "/webfig/x.js", ck)
	}
	if n := strings.Count(buf.String(), "\n"); n > logPlainPerWin {
		t.Errorf("%d plain lines in one window, want at most %d", n, logPlainPerWin)
	}
}

// Candidate 8: a client that goes away (Chrome cancels requests freely) releases its slot.
func TestDiagCancelReleasesSlot(t *testing.T) {
	hang := make(chan struct{})
	addr, stop := fakeROS(t, func(string, string) string { return "" }, hang)
	defer stop()
	m, ck := diagManager(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/jsproxy", strings.NewReader("x")).WithContext(ctx)
	r.Host = "tech.example.co.ke"
	r.AddCookie(&http.Cookie{Name: cookieSecure, Value: ck})
	done := make(chan struct{})
	go func() { m.Handler().ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	time.Sleep(200 * time.Millisecond)
	if m.Busy() != 1 {
		t.Fatalf("busy %d, want 1", m.Busy())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after the client went away")
	}
	if m.Busy() != 0 {
		t.Fatalf("slot leaked: busy %d", m.Busy())
	}
	select {
	case <-hang:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream connection to the router was not closed on cancel")
	}
}
