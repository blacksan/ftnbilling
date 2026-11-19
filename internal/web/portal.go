package web

import (
	"net/http"
	"strings"

	"ispbill/internal/store"
)

// The customer-facing pages: /pay (pay.go) is where a walled flat lands; the
// pages below show the wait for the M-Pesa prompt and the receipt. Old
// /portal links go to /pay.

func (s *Server) portalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /portal", toPay)
	s.payRoutes(mux)
	mux.HandleFunc("GET /brand.woff2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Write(brandFont)
	})
	mux.HandleFunc("GET /brand/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, err := logoFS.ReadFile("static/" + r.PathValue("name"))
		if err != nil || !strings.HasPrefix(r.PathValue("name"), "logo-") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(faviconSVG)
	})
	mux.HandleFunc("GET /portal/wait", s.portalWait)
	mux.HandleFunc("GET /portal/success", s.portalSuccess)
	mux.HandleFunc("GET /portal/status", toPay)
	mux.HandleFunc("GET /api/orders/{id}", s.apiOrder)
}

func toPay(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/pay", http.StatusSeeOther)
}

func (s *Server) portalWait(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.FormValue("order"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if o.Status == store.OrderPaid {
		http.Redirect(w, r, "/portal/success?order="+o.ID, http.StatusSeeOther)
		return
	}
	s.render(w, "wait", map[string]any{"Brand": s.cfg.BrandName, "SiteName": s.siteName(o.SiteID), "Order": o, "Test": o.Method == "test"})
}

func (s *Server) apiOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"` + o.Status + `","reason":` + jsonStr(o.Reason) + `}`))
}

func (s *Server) portalSuccess(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.FormValue("order"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if o.Status != store.OrderPaid {
		http.Redirect(w, r, "/portal/wait?order="+o.ID, http.StatusSeeOther)
		return
	}
	a, _ := s.svc.GetAccount(o.AccountID)
	var pkg store.Package
	support := s.cfg.SupportPhone
	s.st.View(func(d *store.Data) {
		pkg = d.Packages[a.PackageID]
		if sp := d.Sites[o.SiteID].SupportPhone; sp != "" {
			support = sp
		}
	})
	s.render(w, "success", map[string]any{
		"Brand": s.cfg.BrandName, "SiteName": s.siteName(o.SiteID), "Acct": a, "Pkg": pkg,
		"Support": support, "Expires": a.ExpiresAt,
	})
}

func jsonStr(s string) string {
	b, _ := jsonMarshal(s)
	return string(b)
}

// siteName returns a building's name ("" if unknown).
func (s *Server) siteName(id string) (name string) {
	s.st.View(func(d *store.Data) { name = d.Sites[id].Name })
	return
}
