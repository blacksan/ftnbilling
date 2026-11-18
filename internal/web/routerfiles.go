package web

import (
	"embed"
	"net"
	"net/http"
	"net/url"
	"strings"

	"ispbill/internal/store"
)

// Files the router downloads when it is switched to an online billing server
// (see deploy/README.md). They are generated from this server's PUBLIC_BASE_URL
// and PUBLIC_IP, and hold no secrets. Each building fetches its own copy from
// /routeros/hotspot/<site>/<name> so the pages carry that building's id.
//
//go:embed routerfiles/*.html routerfiles/remote.rsc
var routerFS embed.FS

var hotspotFiles = map[string]bool{"login.html": true, "alogin.html": true, "status.html": true,
	"logout.html": true, "rlogin.html": true, "error.html": true}

func (s *Server) routerFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /routeros/hotspot/{site}/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var ok bool
		s.st.View(func(d *store.Data) { st, found := d.Sites[r.PathValue("site")]; ok = found && !st.Removed })
		if !hotspotFiles[name] || !ok {
			http.NotFound(w, r)
			return
		}
		s.serveRouterFile(w, r, name, r.PathValue("site"), "text/html; charset=utf-8")
	})
	// Legacy, un-prefixed paths: served only while a legacy (env-mode) building exists.
	mux.HandleFunc("GET /routeros/hotspot/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		env, ok := s.envSite()
		if !hotspotFiles[name] || !ok {
			http.NotFound(w, r)
			return
		}
		s.serveRouterFile(w, r, name, env.ID, "text/html; charset=utf-8")
	})
	mux.HandleFunc("GET /routeros/remote.rsc", func(w http.ResponseWriter, r *http.Request) {
		env, ok := s.envSite()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s.cfg.PublicIP == "" || s.portalBase(r) == "" {
			http.Error(w, "PUBLIC_IP and PUBLIC_BASE_URL must be set on the billing server", http.StatusServiceUnavailable)
			return
		}
		s.serveRouterFile(w, r, "remote.rsc", env.ID, "text/plain; charset=utf-8")
	})
}

func (s *Server) envSite() (st store.Site, ok bool) {
	s.st.View(func(d *store.Data) { st, ok = d.EnvSite() })
	return
}

// portalBase is the URL hotspot clients are sent to: PUBLIC_BASE_URL, or the
// in-router container address when running on the router itself.
func (s *Server) portalBase(r *http.Request) string {
	if s.cfg.PublicBaseURL != "" {
		return s.cfg.PublicBaseURL
	}
	return "http://172.17.0.2:8080"
}

func (s *Server) serveRouterFile(w http.ResponseWriter, r *http.Request, name, siteID, ctype string) {
	b, err := routerFS.ReadFile("routerfiles/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base := s.portalBase(r)
	ip := s.cfg.PublicIP
	if ip == "" {
		if u, err := url.Parse(base); err == nil {
			if h := u.Hostname(); net.ParseIP(h) != nil {
				ip = h
			}
		}
	}
	out := strings.NewReplacer("{{.Portal}}", base, "{{PORTAL}}", base, "{{SITE}}", url.QueryEscape(siteID), "{{PUBLICIP}}", ip).Replace(string(b))
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(out))
}
