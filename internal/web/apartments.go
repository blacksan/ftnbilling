package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// The Apartments pages: list, view, create/edit (with caretaker and landlord),
// router links and delete. An apartment is a building; routers and apartments
// are many-to-many.

func (s *Server) apartmentRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/apartments", s.adminApartments)
	get("/admin/apartments/view", s.adminApartment)
	get("/admin/apartments/new", s.adminApartmentForm)
	get("/admin/apartments/edit", s.adminApartmentForm)
	post("/admin/apartments/save", s.adminApartmentSave)
	post("/admin/apartments/delete", s.adminApartmentDelete)
	post("/admin/apartments/link", s.adminApartmentLink)
	post("/admin/apartments/unlink", s.adminApartmentUnlink)
	post("/admin/accounts/apartment", s.adminAccountApartment)
	get("/admin/apartments.json", s.adminApartmentsJSON)
}

func aptURL(id string) string { return "/admin/apartments/view?id=" + url.QueryEscape(id) }

func (s *Server) adminApartments(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "apartments")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	var rows []billing.ApartmentSummary
	for _, a := range s.svc.Apartments() {
		if q == "" || apartmentMatches(a.Apartment, q) {
			rows = append(rows, a)
		}
	}
	var flats, active, online int
	for _, a := range rows {
		flats += a.Total
		active += a.Active
		online += a.Online
	}
	p.Data["Rows"], p.Data["Q"] = rows, r.URL.Query().Get("q")
	p.Data["Flats"], p.Data["ActiveN"], p.Data["OnlineN"], p.Data["Count"] = flats, active, online, len(rows)
	s.render(w, "admin_apartments", p)
}

func (s *Server) adminApartmentsJSON(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID, Name, Address string
	}
	out := []row{}
	for _, a := range s.svc.Apartments() {
		out = append(out, row{a.ID, a.Name, a.Address})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) adminApartment(w http.ResponseWriter, r *http.Request) {
	det, ok := s.svc.ApartmentByID(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := s.page(r, "apartments")
	p.Detail = det.Name
	p.H1a, p.H1b = det.Name, ""
	p.Lead = det.Address
	p.Data["A"] = det
	p.Data["Issue"] = map[string]string{}
	issues := map[string]string{}
	for _, a := range det.Accounts {
		if msg := s.svc.AccountProblem(a.ID); msg != "" {
			issues[a.ID] = msg
		}
	}
	p.Data["Issue"] = issues
	p.Acts = nil
	if p.CanWrite {
		p.Acts = []heroAct{{Label: "Edit", Href: "/admin/apartments/edit?id=" + url.QueryEscape(det.ID)}, {Label: "Add customer", Modal: "addcustomer", Ghost: true}}
	}
	s.render(w, "admin_apartment_view", p)
}

func (s *Server) adminApartmentForm(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "apartments")
	id := r.URL.Query().Get("id")
	var cur store.Apartment
	linked := map[string]bool{}
	if id != "" {
		det, ok := s.svc.ApartmentByID(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		cur = det.Apartment
		for _, st := range det.Routers {
			linked[st.ID] = true
		}
		p.H1a, p.H1b, p.Lead = "Edit", "apartment.", det.Name
	} else {
		p.H1a, p.H1b, p.Lead = "New", "apartment.", "An apartment block. Link the routers that serve it; add the caretaker and landlord if you have them."
	}
	if v := r.URL.Query().Get("name"); v != "" && id == "" { // kept after a form error
		cur.Name = v
	}
	p.Data["Cur"], p.Data["Linked"], p.Data["IsNew"] = cur, linked, id == ""
	p.Data["GPS"] = ""
	if cur.HasGPS() {
		p.Data["GPS"] = store.FormatGPS(*cur.Lat, *cur.Lng)
	}
	p.Data["AllRouters"] = p.Sites
	s.render(w, "admin_apartment_form", p)
}

func contactForm(r *http.Request, pre string) billing.ContactInput {
	g := func(k string) string { return r.PostFormValue(pre + "_" + k) }
	return billing.ContactInput{Name: g("name"), Phone: g("phone"), Phone2: g("phone2"), Email: g("email"), Notes: g("notes")}
}

func (s *Server) adminApartmentSave(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	in := billing.ApartmentInput{Name: r.PostFormValue("name"), Address: r.PostFormValue("address"), Notes: r.PostFormValue("notes"),
		Caretaker: contactForm(r, "caretaker"), Landlord: contactForm(r, "landlord")}
	var gpsErr error
	in.Lat, in.Lng, gpsErr = store.ParseGPS(r.PostFormValue("gps"))
	fail := func(err error) {
		to := "/admin/apartments/new"
		if id != "" {
			to = "/admin/apartments/edit?id=" + url.QueryEscape(id)
		}
		s.back(w, r, to, "Error: "+err.Error())
	}
	if gpsErr != nil {
		fail(gpsErr)
		return
	}
	if id == "" {
		a, err := s.svc.CreateApartment(in, r.PostForm["router"])
		if err != nil {
			fail(err)
			return
		}
		s.back(w, r, aptURL(a.ID), "Apartment created")
		return
	}
	if err := s.svc.UpdateApartment(id, in); err != nil {
		fail(err)
		return
	}
	// The router checkboxes are the whole set: link new ones, unlink dropped
	// ones (refused while open accounts of this apartment are on them).
	if r.PostFormValue("routers_set") == "1" {
		if err := s.svc.SetApartmentRouters(id, r.PostForm["router"]); err != nil {
			s.back(w, r, aptURL(id), "Saved, but routers not changed: "+err.Error())
			return
		}
	}
	s.back(w, r, aptURL(id), "Apartment saved")
}

func (s *Server) adminApartmentDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.svc.DeleteApartment(id); err != nil {
		s.back(w, r, aptURL(id), "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/apartments", "Apartment removed")
}

func (s *Server) adminApartmentLink(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.LinkRouter(id, r.PostFormValue("site"))
	s.back(w, r, aptURL(id), msgOr(err, "Router linked"))
}

func (s *Server) adminApartmentUnlink(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.UnlinkRouter(id, r.PostFormValue("site"))
	s.back(w, r, aptURL(id), msgOr(err, "Router unlinked"))
}

// adminAccountApartment moves a customer to another apartment (and router,
// chosen from the new apartment's routers; automatic when it has one).
func (s *Server) adminAccountApartment(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	_, err := s.svc.MoveToApartment(r.Context(), billing.MoveInput{AccountID: id,
		ApartmentID: r.PostFormValue("apartment_id"), SiteID: r.PostFormValue("site"), Unit: r.PostFormValue("unit")})
	var ue *billing.UnitTakenError
	if errors.As(err, &ue) {
		err = errors.New("that unit is already taken in this apartment")
	}
	s.back(w, r, s.accountURL(id), msgOr(err, "Moved. The router change is applied within a minute and is in the account history."))
}
