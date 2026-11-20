package webfig

import (
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
