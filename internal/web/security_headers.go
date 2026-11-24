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

// cspNetwork is the admin policy plus what the Network page's map needs, and only for /admin/network. The map is
// MapLibre GL JS 6 (static/vendor/maplibre, served from this origin) with OpenFreeMap vector tiles, so:
//   - script-src stays 'self': the library is an ES module with no eval or new Function, loaded by a dynamic import()
//     from /static, and no inline script is needed. There is no 'unsafe-eval', no 'unsafe-inline' and no remote script.
//   - worker-src 'self': the tile worker is the self-hosted maplibre-gl-worker.mjs (a module worker), so blob: is not needed.
//     The worker runs under the CSP of its own response (cspMapWorker), which is where its tile fetches are allowed.
//   - connect-src adds https://tiles.openfreemap.org: the style JSON, the TileJSON and the .pbf tiles, the sprite sheet
//     and the glyph (font) ranges are all fetched by XHR/fetch from that one host.
//   - img-src adds https://tiles.openfreemap.org (the sprite sheet is decoded from a fetched blob, hence blob:) and keeps data:
//     (MapLibre's own control icons are data: SVG in its CSS).
//   - style-src is the admin one ('unsafe-inline' is already there; the map sets inline styles on its markers and canvas).
//   - font-src stays 'self': map text is drawn from SDF glyphs on a canvas, not web fonts.
const cspNetwork = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: https://tiles.openfreemap.org; " +
	"font-src 'self'; " +
	"connect-src 'self' https://tiles.openfreemap.org; " +
	"worker-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'self'; object-src 'none'"

// cspMapWorker is the policy of the MapLibre tile worker script itself. A dedicated worker is governed by the CSP
// delivered with its own script response, not by the page that started it (CSP3, and what Firefox and current Chrome
// do), and /static otherwise gets cspPages (connect-src 'self'), under which the worker's fetches of the vector tiles
// and glyphs from tiles.openfreemap.org would be refused and the map would stay blank. So the worker file, and only it,
// carries the map's connect-src; it runs no eval (script-src 'self' blocks the importScripts fallback it never uses).
const cspMapWorker = "default-src 'none'; script-src 'self'; connect-src 'self' https://tiles.openfreemap.org; " +
	"img-src 'self' data: blob: https://tiles.openfreemap.org; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// mapWorkerPath is where netmap.js points setWorkerUrl (static/vendor/maplibre).
const mapWorkerPath = "/static/vendor/maplibre/maplibre-gl-worker.mjs"

// networkCSP is cspNetwork with the remote WebFig host in form-action when it is on (like adminCSP).
func (s *Server) networkCSP() string {
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
	if p == "/admin/network" {
		// OpenFreeMap needs no key and no Referer, so the site-wide Referrer-Policy (same-origin) stays.
		h.Set("Content-Security-Policy", s.networkCSP())
		return
	}
	if p == mapWorkerPath {
		h.Set("Content-Security-Policy", cspMapWorker)
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
