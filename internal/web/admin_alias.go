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
