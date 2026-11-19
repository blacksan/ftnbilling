package webfig

import (
	"net/http"
	"net/url"
	"testing"
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
