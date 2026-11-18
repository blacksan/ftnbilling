package web

import (
	"net/http"
	"net/url"
	"strings"

	"ispbill/internal/store"
)

const siteCookie = "ftn_site"

// scope is the building the admin is looking at: one building, or all of them.
type scope struct {
	ID    string // "" when All
	All   bool
	Sites []store.Site // live buildings, by name
}

// Has reports whether a row stamped with siteID belongs in the current view.
func (sc scope) Has(siteID string) bool { return sc.All || siteID == sc.ID }

// Multi is true when there is more than one building, which is when the
// Building column and the switcher make sense.
func (sc scope) Multi() bool { return len(sc.Sites) > 1 }

// scopeOf reads the ftn_site cookie. A missing or stale value falls back to
// the only building if there is exactly one, else "all buildings".
func (s *Server) scopeOf(r *http.Request) scope {
	var sc scope
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if !st.Removed {
				sc.Sites = append(sc.Sites, st)
			}
		}
	})
	if c, err := r.Cookie(siteCookie); err == nil {
		if c.Value == "all" && len(sc.Sites) != 1 {
			sc.All = true
			return sc
		}
		for _, st := range sc.Sites {
			if st.ID == c.Value {
				sc.ID = st.ID
				return sc
			}
		}
	}
	if len(sc.Sites) == 1 {
		sc.ID = sc.Sites[0].ID
		return sc
	}
	sc.All = true
	return sc
}

// adminSiteSelect sets the cookie and returns to the page the admin was on.
func (s *Server) adminSiteSelect(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("site")
	ok := v == "all"
	if !ok {
		s.st.View(func(d *store.Data) { st, f := d.Sites[v]; ok = f && !st.Removed })
	}
	if ok {
		http.SetCookie(w, &http.Cookie{Name: siteCookie, Value: v, Path: "/admin", HttpOnly: true,
			Secure: s.secureCookies(), SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
	}
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// safeNext only lets the switcher return to a path under /admin on this host.
func safeNext(n string) string {
	u, err := url.Parse(n)
	if err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(u.Path, "/admin") || strings.HasPrefix(n, "//") || strings.ContainsAny(n, "\r\n\\") {
		return "/admin"
	}
	if u.Path == "/admin/logout" || strings.HasPrefix(u.Path, "/admin/login") {
		return "/admin"
	}
	return u.RequestURI()
}
