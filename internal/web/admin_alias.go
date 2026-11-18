package web

import (
	"net/http"
	"strings"
)

// routerAdminPath maps /admin/sites… to its /admin/routers… twin (D8).
func routerAdminPath(p string) (string, bool) {
	const old = "/admin/sites"
	if !strings.HasPrefix(p, old) {
		return "", false
	}
	rest := p[len(old):]
	if rest != "" && rest[0] != '/' {
		return "", false
	}
	return "/admin/routers" + rest, true
}

// redirectToRouters sends a GET on an old /admin/sites… address to the new one,
// keeping the query string.
func redirectToRouters(w http.ResponseWriter, r *http.Request) {
	np, _ := routerAdminPath(r.URL.Path)
	if r.URL.RawQuery != "" {
		np += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, np, http.StatusFound)
}

// overviewAliases are the addresses billing.Overview hands out (docs/api/v4-w9.md)
// that the web admin serves under other names: /admin/plans is Plans (packages),
// /admin/subscribers is the subscribers list (accounts), whose MAC conflict tab
// is called "conflict" here.
func (s *Server) overviewAliases(get func(string, http.HandlerFunc)) {
	to := func(dest string, tabs map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if t, ok := tabs[q.Get("tab")]; ok {
				q.Set("tab", t)
			}
			u := dest
			if len(q) > 0 {
				u += "?" + q.Encode()
			}
			http.Redirect(w, r, u, http.StatusFound)
		}
	}
	get("/admin/plans", to("/admin/packages", nil))
	get("/admin/subscribers", to("/admin/accounts", map[string]string{"mac_conflict": "conflict"}))
}
