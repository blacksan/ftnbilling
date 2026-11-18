package web

import (
	"net/http"
	"strings"
)

// Content-Security-Policy values. The sign-in pages and the API run no script
// at all. The rest of the admin pages and the portal still carry small inline
// scripts and handlers (theme switch, building picker, polling), so those keep
// 'unsafe-inline' for script; everything else is limited to this site.
const (
	cspStrict = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	cspPages  = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
)

// securityHeaders sets the headers every response carries.
func (s *Server) securityHeaders(h http.Header, r *http.Request) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	if s.secureCookies() {
		// Browsers ignore this over plain http; Caddy terminates TLS in front.
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/admin/login") || strings.HasPrefix(p, "/admin/security/enrol") || strings.HasPrefix(p, "/api/v1") {
		h.Set("Content-Security-Policy", cspStrict)
		return
	}
	h.Set("Content-Security-Policy", cspPages)
}
