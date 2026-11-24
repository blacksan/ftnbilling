package web

import (
	"net/http"
	"strconv"
	"strings"

	"ispbill/internal/store"
)

// The customer-facing pages: /pay (pay.go) is where a walled flat lands; the
// pages below show the wait for the M-Pesa prompt and the receipt. Old
// /portal links go to /pay.

func (s *Server) portalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /portal", toPay)
	// The operating systems' captive-portal probes. Behind Caddy on a domain install they never get this far (every
	// plain-HTTP host is redirected to /pay there); on an install without a domain they reach us with the probe's
	// own Host, and the redirect keeps that Host, which the router sends back here again.
	for _, probe := range []string{"/hotspot-detect.html", "/library/test/success.html", "/generate_204", "/gen_204", "/connecttest.txt", "/ncsi.txt", "/success.txt", "/canonical.html"} {
		mux.HandleFunc("GET "+probe, captiveToPay)
	}
	s.payRoutes(mux)
	s.registerRoutes(mux)
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

// captiveToPay answers a probe with a 302 (a 204 or a "Success" page would tell the device it is online).
func captiveToPay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/pay", http.StatusFound)
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
	s.render(w, "wait", map[string]any{"Brand": s.cfg.BrandName, "SiteName": s.apartmentName(s.orderApartment(o.AccountID)), "Order": o, "Test": o.Method == "test",
		"Contacts": s.contacts(o.SiteID)})
}

func (s *Server) apiOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	online := o.Status == store.OrderPaid && s.svc.OrderOnline(o)
	w.Write([]byte(`{"status":"` + o.Status + `","reason":` + jsonStr(o.Reason) + `,"online":` + strconv.FormatBool(online) + `}`))
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
	s.st.View(func(d *store.Data) { pkg = d.Packages[a.PackageID] })
	c := s.contacts(o.SiteID)
	s.render(w, "success", map[string]any{
		"Brand": s.cfg.BrandName, "SiteName": s.apartmentName(a.ApartmentID), "Acct": a, "Pkg": pkg,
		"Support": c.Words(), "Contacts": c, "Expires": a.ExpiresAt, "Order": o.ID,
	})
}

func jsonStr(s string) string {
	b, _ := jsonMarshal(s)
	return string(b)
}

// orderApartment is the apartment of an order's account ("" if unknown).
func (s *Server) orderApartment(accountID string) string {
	a, _ := s.svc.GetAccount(accountID)
	return a.ApartmentID
}
