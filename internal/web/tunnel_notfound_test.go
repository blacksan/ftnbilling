package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ispbill/internal/config"
)

// A router on the WireGuard tunnel must get the bare 404 for the admin, never the themed
// page (which carries the brand and a link back to the admin).
func TestTunnelGetsPlain404ForAdmin(t *testing.T) {
	a := newAppCfg(t, true, func(c *config.Config) { c.WGSubnet = "10.99.0.0/22" })
	h := a.srv.Handler()
	for _, path := range []string{"/admin", "/admin/routers", "/admin/nope"} {
		for _, tc := range []struct {
			xff   string
			plain bool
		}{{"10.99.0.5", true}, {"192.0.2.9", false}} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "127.0.0.1:5555"
			req.Header.Set("X-Forwarded-For", tc.xff)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			body, _ := io.ReadAll(rec.Body)
			if tc.plain {
				if rec.Code != 404 || strings.Contains(string(body), "<") || strings.Contains(string(body), "/admin") ||
					!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
					t.Errorf("tunnel GET %s: %d %q (%s), want the plain 404", path, rec.Code, body, rec.Header().Get("Content-Type"))
				}
			} else if path == "/admin/nope" && (rec.Code != 404 || !strings.Contains(string(body), "Page not found")) {
				t.Errorf("outside GET %s: %d %q, want the themed 404", path, rec.Code, body)
			}
		}
	}
}
