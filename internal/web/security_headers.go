package web

import (
	"net/http"
	"strings"
)

// Content-Security-Policy values. The sign-in pages, the API and every admin page run no
// inline script and no inline handler (only /static scripts; the admin pages keep inline
// styles for small layout tweaks). The tenant portal and its payment pages still carry
// small inline scripts, so those keep 'unsafe-inline' for script. Everything is limited
// to this site: no external script, style, font, image or connection.
const (
	cspStrict = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	cspAdmin  = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
	cspPages = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"
)

// cspNetwork is the admin policy plus what the Google Maps JavaScript API needs, and only for the Network page and
// only when a key is set (see networkCSP). Nothing else changes: script-src stays free of 'unsafe-inline', so the page
// still runs no inline script (the Maps loader is a script element created by /static/netmap.js).
const cspNetwork = "default-src 'self'; script-src 'self' https://maps.googleapis.com https://maps.gstatic.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"img-src 'self' data: blob: https://*.googleapis.com https://*.gstatic.com https://*.google.com https://*.googleusercontent.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"connect-src 'self' https://maps.googleapis.com https://*.googleapis.com; " +
	"worker-src blob:; frame-src https://*.google.com; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"

// networkCSP is cspNetwork for the Network page when a Maps key is set, else the normal admin policy (the page then
// shows the list view and loads nothing from Google).
func (s *Server) networkCSP() string {
	if s.live().GoogleMapsAPIKey == "" {
		return s.adminCSP()
	}
	return strings.Replace(cspNetwork, "form-action 'self'", "form-action 'self'"+s.webFigFormAction(), 1)
}

// securityHeaders sets the headers every response carries.
func (s *Server) securityHeaders(h http.Header, r *http.Request) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	// Geolocation is off everywhere except the apartment form, whose "Use this device's location" button needs it.
	geo := "()"
	if p := r.URL.Path; p == "/admin/apartments/new" || p == "/admin/apartments/edit" {
		geo = "(self)"
	}
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation="+geo+", payment=()")
	if s.secureCookies() {
		// Browsers ignore this over plain http; Caddy terminates TLS in front.
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/admin/login") || strings.HasPrefix(p, "/admin/security/enrol") || strings.HasPrefix(p, "/api/v1") {
		h.Set("Content-Security-Policy", cspStrict)
		return
	}
	if p == "/admin/network" && s.live().GoogleMapsAPIKey != "" {
		h.Set("Content-Security-Policy", s.networkCSP())
		// A key restricted by HTTP referrer needs the page's origin on the requests to Google: with the site-wide
		// "same-origin" policy they carry no Referer and Google refuses the key (RefererNotAllowedMapError).
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		return
	}
	if strings.HasPrefix(p, "/admin") {
		h.Set("Content-Security-Policy", s.adminCSP())
		return
	}
	h.Set("Content-Security-Policy", cspPages)
}

// adminCSP is cspAdmin, plus the remote WebFig host in form-action when it is on.
// "Open WebFig" posts to /admin/routers/webfig, which answers with a redirect to
// https://<WEBFIG_DOMAIN>/_ftn/open; Chrome checks form-action against the
// redirect target too, so without this it blocks the redirect (Firefox lets it through).
func (s *Server) adminCSP() string {
	if f := s.webFigFormAction(); f != "" {
		return strings.Replace(cspAdmin, "form-action 'self'", "form-action 'self'"+f, 1)
	}
	return cspAdmin
}

// webFigFormAction is " https://<WEBFIG_DOMAIN>" when remote WebFig is on, else "".
func (s *Server) webFigFormAction() string {
	d := s.cfg.WebFigDomain
	if d == "" || !hostnameOK(d) {
		return ""
	}
	return " https://" + d
}

// hostnameOK accepts a plain DNS name (letters, digits, dots and hyphens), so a
// misconfigured value can never add anything else to the header.
func hostnameOK(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}
