package webfig

import (
	"context"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestRewriteLocation(t *testing.T) {
	tg := Target{IP: "10.99.0.2", Port: 8081}
	for in, want := range map[string]string{
		"/webfig/":                          "/webfig/",
		"webfig/":                           "webfig/",
		"http://10.99.0.2:8081/webfig/?a=1": "/webfig/?a=1",
		"http://10.99.0.2/x#f":              "/x#f",
		"https://10.99.0.2:8081":            "/",
		"http://10.99.0.2:22/":              "/",
		"http://10.99.0.3:8081/":            "/",
		"http://evil.example/":              "/",
		"//evil.example/x":                  "/",
		"javascript:alert(1)":               "/",
		"http://10.99.0.2:8081@evil/":       "/",
	} {
		if got := rewriteLocation(in, tg); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestBadPath(t *testing.T) {
	for uri, bad := range map[string]bool{
		"/": false, "/webfig/": false, "/a/b.js?x=1": false, "/jsproxy": false,
		"/../x": true, "/a/./b": true, "/a/%2e%2e/b": true, "//evil/x": true, "/a%5cb": true, "/a\\b": true, "*": true,
	} {
		r := &http.Request{RequestURI: uri, URL: &url.URL{Path: uri}}
		if u, err := url.ParseRequestURI(uri); err == nil {
			r.URL = u
		}
		if got := badPath(r); got != bad {
			t.Errorf("%q: bad=%v, want %v", uri, got, bad)
		}
	}
	r, _ := http.NewRequest("GET", "http://evil.example/x", nil)
	r.RequestURI = "http://evil.example/x"
	if !badPath(r) {
		t.Error("absolute-form accepted")
	}
}

func TestRewriteLocationBackslashForms(t *testing.T) {
	tg := Target{IP: "10.99.0.2", Port: 8081}
	for _, in := range []string{`/\evil.example`, `/\\evil.example`, `\\evil.example`, `/ok\path`, "/a\r\nSet-Cookie: x=1", `//evil.example`, "/a\x00b"} {
		if got := rewriteLocation(in, tg); got != "/" {
			t.Errorf("%q -> %q, want /", in, got)
		}
	}
}

// The store is asked outside the manager's lock: a slow user lookup must not
// stop the manager answering for every other session.
func TestStoreLookupsAreOutsideTheLock(t *testing.T) {
	var slow atomic.Bool
	block, entered := make(chan struct{}), make(chan struct{}, 4)
	m := New(Options{Domain: "tech.example.co.ke", Secure: true,
		Resolve: func(string) (Target, bool) { return Target{IP: "10.99.0.2", Port: 8081}, true },
		UserOK: func(string) bool {
			if slow.Load() {
				entered <- struct{}{}
				<-block
			}
			return true
		},
		AdminLive: func(string) bool { return true }})
	tok, err := m.Mint("u1", "admin", "s1", "1.2.3.4", "a1")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := m.Redeem(tok, "1.2.3.4", "")
	if err != nil {
		t.Fatal(err)
	}
	slow.Store(true)
	done := make(chan bool)
	go func() { _, _, ok := m.lookup(cookie); done <- ok }()
	<-entered
	got := make(chan struct{})
	go func() { m.List("s1"); m.Busy(); _, _ = m.Mint("u1", "admin", "s1", "1.2.3.4", "a1"); close(got) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the manager is locked while a user lookup runs")
	}
	close(block)
	if !<-done {
		t.Fatal("lookup failed")
	}
}

// WebFig's page fetches its scripts in parallel. A request over the per-router
// limit waits for a free slot instead of being refused: one refused script left
// WebFig stuck on "Connecting" on the bench router.
func TestRequestOverTheLimitWaitsForASlot(t *testing.T) {
	m := New(Options{Domain: "tech.example.co.ke", Secure: true,
		Resolve:   func(string) (Target, bool) { return Target{IP: "10.99.0.2", Port: 8081}, true },
		UserOK:    func(string) bool { return true },
		AdminLive: func(string) bool { return true }})
	var held []func()
	for i := 0; i < m.o.PerRouter; i++ {
		rel, ok := m.acquire("s1", "h1", false)
		if !ok {
			t.Fatalf("slot %d refused below the limit", i)
		}
		held = append(held, rel)
	}
	got := make(chan bool, 1)
	go func() {
		rel, ok := m.acquireWait(context.Background(), "s1", "h1", false)
		if ok {
			rel()
		}
		got <- ok
	}()
	select {
	case <-got:
		t.Fatal("the request over the limit did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	held[0]()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("the waiting request was refused after a slot freed up")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting request never got the freed slot")
	}
	for _, rel := range held[1:] {
		rel()
	}
	// A request whose client went away stops waiting.
	for i := 0; i < m.o.PerRouter; i++ {
		rel, _ := m.acquire("s1", "h1", false)
		defer rel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := m.acquireWait(ctx, "s1", "h1", false); ok {
		t.Fatal("a cancelled request got a slot")
	}
}

func TestCacheBoundsExpiryAndVersion(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newFileCache(100, time.Hour, func() time.Time { return now })
	req := func(p string) *http.Request {
		r, _ := http.NewRequest("GET", "http://x"+p, nil)
		return r
	}
	put := func(site, ver, p string, n int) {
		r := req(p)
		c.put(&cacheEntry{key: cacheKey(site, ver, r), site: site, ver: ver, body: make([]byte, n)})
	}
	put("s1", "7.1", "/webfig/a.js", 40)
	put("s1", "7.1", "/webfig/b.js", 40)
	if c.get("s1", "7.1", req("/webfig/a.js")) == nil { // a is now the most recent
		t.Fatal("miss")
	}
	put("s1", "7.1", "/webfig/c.js", 40) // over 100 bytes: least recently used (b) goes
	if c.get("s1", "7.1", req("/webfig/b.js")) != nil || c.get("s1", "7.1", req("/webfig/a.js")) == nil {
		t.Fatal("eviction is not LRU")
	}
	if _, b := c.stats(); b > 100 {
		t.Fatalf("cache holds %d bytes over its bound", b)
	}
	put("s1", "7.1", "/webfig/huge.js", 500) // larger than the whole cache: not kept
	if c.get("s1", "7.1", req("/webfig/huge.js")) != nil {
		t.Fatal("an entry over the bound was kept")
	}
	// another router's files are separate and survive this router's version change
	put("s2", "7.1", "/webfig/a.js", 10)
	if c.get("s1", "7.2", req("/webfig/a.js")) != nil {
		t.Fatal("served after a version change")
	}
	if c.get("s2", "7.1", req("/webfig/a.js")) == nil {
		t.Fatal("another router's file was dropped")
	}
	if n, _ := c.stats(); n != 1 {
		t.Fatalf("version change left %d entries", n)
	}
	// expiry
	now = now.Add(time.Hour + time.Second)
	if c.get("s2", "7.1", req("/webfig/a.js")) != nil {
		t.Fatal("served after 24h expiry")
	}
	// purge
	put("s2", "7.1", "/webfig/a.js", 10)
	c.purgeSite("s2")
	if n, b := c.stats(); n != 0 || b != 0 {
		t.Fatal("purge left entries")
	}
}

func TestStaticRequestAndPolicy(t *testing.T) {
	r := func(m, u string, h ...string) *http.Request {
		q, _ := http.NewRequest(m, "http://x"+u, nil)
		for i := 0; i+1 < len(h); i += 2 {
			q.Header.Set(h[i], h[i+1])
		}
		return q
	}
	for _, c := range []struct {
		r  *http.Request
		ok bool
	}{
		{r("GET", "/webfig/a.js"), true}, {r("GET", "/webfig/x.jg?1"), true}, {r("GET", "/webfig/i.PNG"), true},
		{r("GET", "/webfig/list"), false}, {r("GET", "/webfig/"), false}, {r("GET", "/jsproxy"), false},
		{r("POST", "/webfig/a.js"), false}, {r("GET", "/other/a.js"), false}, {r("GET", "/webfig/a.js", "Range", "bytes=0-1"), false},
	} {
		if got := staticRequest(c.r); got != c.ok {
			t.Errorf("%s %s: %v", c.r.Method, c.r.URL, got)
		}
	}
	resp := func(code int, h ...string) *http.Response {
		x := &http.Response{StatusCode: code, Header: http.Header{}}
		for i := 0; i+1 < len(h); i += 2 {
			x.Header.Add(h[i], h[i+1])
		}
		return x
	}
	if cc, ok := staticPolicy(resp(200)); cc != StaticCacheControl || !ok {
		t.Error("plain 200")
	}
	if _, ok := staticPolicy(resp(200, "Set-Cookie", "a=b")); ok {
		t.Error("Set-Cookie cached")
	}
	if _, ok := staticPolicy(resp(404)); ok {
		t.Error("404 cached")
	}
	if _, ok := staticPolicy(resp(200, "Vary", "Cookie")); ok {
		t.Error("Vary: Cookie cached")
	}
	if cc, ok := staticPolicy(resp(200, "Cache-Control", "no-cache")); cc != "no-store" || ok {
		t.Error("no-cache")
	}
	if cc, ok := staticPolicy(resp(200, "Cache-Control", "max-age=60")); cc != "private, max-age=60" || ok {
		t.Errorf("stricter max-age: %q %v", cc, ok)
	}
}

func TestJSProxyPoolIsSeparate(t *testing.T) {
	m := New(Options{Domain: "x", Resolve: func(string) (Target, bool) { return Target{}, false }})
	var rel []func()
	for i := 0; i < JSProxyPerSession; i++ {
		f, ok := m.acquire("s1", "h1", true)
		if !ok {
			t.Fatal("js slot refused")
		}
		rel = append(rel, f)
	}
	if _, ok := m.acquire("s1", "h1", true); ok {
		t.Fatal("one session took more than its jsproxy share")
	}
	for i := 0; i < m.o.PerRouter; i++ { // the file slots are all free
		f, ok := m.acquire("s1", "h1", false)
		if !ok {
			t.Fatalf("file slot %d taken by jsproxy", i)
		}
		rel = append(rel, f)
	}
	for _, f := range rel {
		f()
	}
	if m.Busy() != 0 {
		t.Fatal("slots leaked")
	}
}
