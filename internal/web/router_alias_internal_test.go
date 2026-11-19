package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddRouterAliasesUnit(t *testing.T) {
	got := string(addRouterAliases([]byte(`{"a":[{"site_id":"s1","site_name":"X"}],"n":12345678901234567890}`)))
	for _, want := range []string{`"router_id":"s1"`, `"router_name":"X"`, `12345678901234567890`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if in := `{"x":1}`; string(addRouterAliases([]byte(in))) != in {
		t.Error("untouched body changed")
	}
}

func TestStaticAssets(t *testing.T) {
	for p, ct := range map[string]string{"app.css": "text/css", "app.js": "text/javascript", "theme.js": "text/javascript", "fonts/ibm-plex-sans-latin-wght-normal.woff2": "font/woff2", "favicon.svg": "image/svg+xml"} {
		w := httptest.NewRecorder()
		mux := http.NewServeMux()
		(&Server{}).staticRoutes(mux)
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/static/"+p, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ct) {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Content-Type"))
		}
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "31536000") {
			t.Errorf("%s cache: %q", p, cc)
		}
	}
	for _, p := range []string{"nope.css", "../server.go", "fonts", ""} {
		w := httptest.NewRecorder()
		mux := http.NewServeMux()
		(&Server{}).staticRoutes(mux)
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/static/"+p, nil))
		if w.Code != 404 && w.Code != 301 {
			t.Errorf("%q: %d", p, w.Code)
		}
	}
	if u := staticURL("app.css"); !strings.HasPrefix(u, "/static/app.css?v=") || len(u) < 20 {
		t.Errorf("staticURL %q", u)
	}
	// The CSP lets /static scripts, styles and fonts load on every page kind.
	for _, c := range []string{cspStrict, cspPages} {
		for _, d := range []string{"script-src 'self'", "style-src 'self'", "font-src 'self'"} {
			if !strings.Contains(c, d) {
				t.Errorf("csp lacks %s: %s", d, c)
			}
		}
	}
}
