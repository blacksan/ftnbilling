package web

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// API v1, apartments (docs/api/v6-apartments.md).

const (
	codeMACOtherBuilding  = "mac_other_building"
	codeUnitInUse         = "unit_in_use"
	codeApartmentNameUsed = "apartment_name_in_use"
	codeApartmentInUse    = "apartment_in_use"
	codeRouterServes      = "router_serves_accounts"
)

type apiContact struct {
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	Phone2      string `json:"phone2"`
	Email       string `json:"email"`
	Notes       string `json:"notes"`
	WhatsAppURL string `json:"whatsapp_url"`
}

func contactJSON(c store.ApartmentContact) apiContact {
	return apiContact{c.Name, c.Phone, c.Phone2, c.Email, c.Notes, billing.WhatsAppLink(c.Phone)}
}

type apiRouterRef struct {
	SiteID   string `json:"site_id"`
	SiteName string `json:"site_name"`
	Accounts int    `json:"accounts"` // open accounts of this apartment served by the router
}

type apiApartment struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Address   string         `json:"address"`
	Notes     string         `json:"notes"`
	Caretaker apiContact     `json:"caretaker"`
	Landlord  apiContact     `json:"landlord"`
	Routers   []apiRouterRef `json:"routers"`
	Total     int            `json:"accounts_total"`
	Active    int            `json:"accounts_active"`
	Online    int            `json:"online_now"`
	// Lat and Lng are the building's GPS point, both null when unknown. MapURL is
	// the one-click Google Maps link for it (null when there is no point).
	Lat       *float64 `json:"lat"`
	Lng       *float64 `json:"lng"`
	MapURL    *string  `json:"map_url"`
	CreatedAt string   `json:"created_at"`
}

func (s *Server) apartmentJSON(sum billing.ApartmentSummary, perRouter map[string]int) apiApartment {
	out := apiApartment{ID: sum.ID, Name: sum.Name, Address: sum.Address, Notes: sum.Notes,
		Caretaker: contactJSON(sum.Caretaker), Landlord: contactJSON(sum.Landlord), Routers: []apiRouterRef{},
		Total: sum.Total, Active: sum.Active, Online: sum.Online, Lat: sum.Lat, Lng: sum.Lng, CreatedAt: sum.CreatedAt.UTC().Format(time.RFC3339)}
	if u := sum.MapURL(); u != "" {
		out.MapURL = &u
	}
	for _, st := range sum.Routers {
		out.Routers = append(out.Routers, apiRouterRef{st.ID, st.Name, perRouter[st.ID]})
	}
	return out
}

// apartmentBody is the create and edit body.
type apartmentBody struct {
	Name      *string `json:"name"`
	Address   *string `json:"address"`
	Notes     *string `json:"notes"`
	Caretaker *struct {
		Name, Phone, Phone2, Email, Notes string
	} `json:"caretaker"`
	Landlord *struct {
		Name, Phone, Phone2, Email, Notes string
	} `json:"landlord"`
	// Lat and Lng are raw so that absent (leave alone on PATCH) and null (clear) differ.
	Lat       json.RawMessage `json:"lat"`
	Lng       json.RawMessage `json:"lng"`
	RouterIDs []string        `json:"router_ids"`
	SiteIDs   []string        `json:"site_ids"`
}

// gps reads lat and lng from the body. set is false when neither key was sent.
// null on both means "no point" (nil, nil, set). One without the other, a
// non-number or a value out of range is a validation error on that field.
func (b apartmentBody) gps() (lat, lng *float64, set bool, bad map[string]string) {
	if b.Lat == nil && b.Lng == nil {
		return nil, nil, false, nil
	}
	bad = map[string]string{}
	one := func(raw json.RawMessage, key string) (*float64, bool) {
		if raw == nil {
			bad[key] = "give lat and lng together"
			return nil, false
		}
		if string(raw) == "null" {
			return nil, true
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			bad[key] = "must be a number or null"
			return nil, false
		}
		return &f, true
	}
	lat, ok1 := one(b.Lat, "lat")
	lng, ok2 := one(b.Lng, "lng")
	if ok1 && ok2 {
		if (lat == nil) != (lng == nil) {
			bad["lat"], bad["lng"] = "give lat and lng together, or null for both", "give lat and lng together, or null for both"
		} else if lat != nil {
			if math.IsNaN(*lat) || *lat < -90 || *lat > 90 {
				bad["lat"] = "latitude must be between -90 and 90"
			}
			if math.IsNaN(*lng) || *lng < -180 || *lng > 180 {
				bad["lng"] = "longitude must be between -180 and 180"
			}
		}
	}
	if len(bad) > 0 {
		return nil, nil, true, bad
	}
	return lat, lng, true, nil
}

func contactIn(c *struct{ Name, Phone, Phone2, Email, Notes string }, cur store.ApartmentContact) billing.ContactInput {
	if c == nil {
		return billing.ContactInput{Name: cur.Name, Phone: cur.Phone, Phone2: cur.Phone2, Email: cur.Email, Notes: cur.Notes}
	}
	return billing.ContactInput{Name: c.Name, Phone: c.Phone, Phone2: c.Phone2, Email: c.Email, Notes: c.Notes}
}

func (s *Server) apiApartments(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pg, ok := parsePage(w, r)
	if !ok {
		return
	}
	site := r.URL.Query().Get("site")
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	var list []apiApartment
	for _, sum := range s.svc.Apartments() {
		if site != "" {
			served := false
			for _, st := range sum.Routers {
				served = served || st.ID == site
			}
			if !served {
				continue
			}
		}
		if text != "" && !apartmentMatches(sum.Apartment, text) {
			continue
		}
		list = append(list, s.apartmentJSON(sum, nil))
	}
	pageJSON(w, list, pg)
}

// apartmentMatches is the list filter and the search rule: the name, address,
// caretaker or landlord name, or either one's phone numbers.
func apartmentMatches(a store.Apartment, q string) bool {
	if store.ContainsFold(a.Name, q) || store.ContainsFold(a.Address, q) {
		return true
	}
	for _, c := range []store.ApartmentContact{a.Caretaker, a.Landlord} {
		if store.ContainsFold(c.Name, q) {
			return true
		}
		for _, p := range []string{c.Phone, c.Phone2} {
			if p != "" && billing.PhoneMatches(p, q) {
				return true
			}
		}
	}
	return false
}

func (s *Server) apartment404(w http.ResponseWriter, id string) (billing.ApartmentDetail, bool) {
	d, ok := s.svc.ApartmentByID(id)
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such apartment.", nil)
	}
	return d, ok
}

func (s *Server) apiApartment(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d, ok := s.apartment404(w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.apartmentDetailJSON(d))
}

func (s *Server) apartmentDetailJSON(d billing.ApartmentDetail) any {
	return struct {
		apiApartment
		Accounts []apiAccount `json:"accounts"`
	}{s.apartmentJSON(d.ApartmentSummary, d.AccountRouters), s.accountsJSON(d.Accounts)}
}

func (s *Server) apiApartmentCreate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req apartmentBody
	if !decode(w, r, &req, false) {
		return
	}
	in := billing.ApartmentInput{Caretaker: contactIn(req.Caretaker, store.ApartmentContact{}), Landlord: contactIn(req.Landlord, store.ApartmentContact{})}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.Address != nil {
		in.Address = *req.Address
	}
	if req.Notes != nil {
		in.Notes = *req.Notes
	}
	lat, lng, _, bad := req.gps()
	if bad != nil {
		validationErr(w, bad)
		return
	}
	in.Lat, in.Lng = lat, lng
	ids := append(append([]string(nil), req.RouterIDs...), req.SiteIDs...)
	for _, id := range ids {
		if !s.siteExists(id) {
			validationErr(w, map[string]string{"router_ids": "no such router: " + id})
			return
		}
	}
	a, err := s.svc.CreateApartment(in, ids)
	if err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "apartment "+a.Name+" created")
	s.respondApartment(w, http.StatusCreated, a.ID)
}

func (s *Server) respondApartment(w http.ResponseWriter, status int, id string) {
	if d, ok := s.svc.ApartmentByID(id); ok {
		writeJSON(w, status, s.apartmentDetailJSON(d))
		return
	}
	apiErr(w, http.StatusNotFound, codeNotFound, "No such apartment.", nil)
}

func (s *Server) apiApartmentPatch(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d, ok := s.apartment404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req apartmentBody
	if !decode(w, r, &req, false) {
		return
	}
	cur := d.Apartment
	in := billing.ApartmentInput{Name: cur.Name, Address: cur.Address, Notes: cur.Notes,
		Caretaker: contactIn(req.Caretaker, cur.Caretaker), Landlord: contactIn(req.Landlord, cur.Landlord), Lat: cur.Lat, Lng: cur.Lng}
	// Like the other fields, lat and lng left out keep their value; both null clears the point.
	if lat, lng, set, bad := req.gps(); bad != nil {
		validationErr(w, bad)
		return
	} else if set {
		in.Lat, in.Lng = lat, lng
	}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.Address != nil {
		in.Address = *req.Address
	}
	if req.Notes != nil {
		in.Notes = *req.Notes
	}
	if err := s.svc.UpdateApartment(cur.ID, in); err != nil && !errors.Is(err, store.ErrNoChange) {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "apartment "+cur.Name+" edited")
	s.respondApartment(w, http.StatusOK, cur.ID)
}

func (s *Server) apiApartmentDelete(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d, ok := s.apartment404(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.svc.DeleteApartment(d.ID); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "apartment "+d.Name+" deleted")
	w.WriteHeader(http.StatusNoContent)
}

// apiApartmentLink: POST /apartments/{id}/routers {"router_id": "..."}.
func (s *Server) apiApartmentLink(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d, ok := s.apartment404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		SiteID string `json:"site_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if !s.siteExists(req.SiteID) {
		validationErr(w, map[string]string{"router_id": "no such router"})
		return
	}
	if err := s.svc.LinkRouter(d.ID, req.SiteID); err != nil && !errors.Is(err, store.ErrNoChange) {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "apartment "+d.Name+": router "+req.SiteID+" linked")
	s.respondApartment(w, http.StatusOK, d.ID)
}

// apiApartmentUnlink: DELETE /apartments/{id}/routers/{site_id}.
func (s *Server) apiApartmentUnlink(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d, ok := s.apartment404(w, r.PathValue("id"))
	if !ok {
		return
	}
	site := r.PathValue("site_id")
	if err := s.svc.UnlinkRouter(d.ID, site); err != nil && !errors.Is(err, store.ErrNoChange) {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "apartment "+d.Name+": router "+site+" unlinked")
	s.respondApartment(w, http.StatusOK, d.ID)
}

// apiAccountMove: POST /accounts/{id}/move {"apartment_id", "site_id", "unit"}.
func (s *Server) apiAccountMove(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		ApartmentID string `json:"apartment_id"`
		SiteID      string `json:"site_id"`
		Unit        string `json:"unit"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if _, err := s.svc.MoveToApartment(r.Context(), billing.MoveInput{AccountID: a.ID, ApartmentID: req.ApartmentID, SiteID: req.SiteID, Unit: req.Unit}); err != nil && !errors.Is(err, store.ErrNoChange) {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "account "+a.Code+" moved to apartment "+req.ApartmentID)
	s.respondAccount(w, http.StatusOK, a.ID)
}

// apiUnregMove: POST /unregistered/move {"site_id", "mac", "apartment_id"}: move
// the customer who owns this MAC to the router that sees it.
func (s *Server) apiUnregMove(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID      string `json:"site_id"`
		MAC         string `json:"mac"`
		ApartmentID string `json:"apartment_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	mac, ok := macField(w, req.MAC)
	if !ok {
		return
	}
	if !s.siteExists(req.SiteID) {
		validationErr(w, map[string]string{"site_id": "no such router"})
		return
	}
	a, err := s.svc.MoveCustomerHere(r.Context(), req.SiteID, mac, req.ApartmentID)
	if err != nil {
		if errors.Is(err, store.ErrNoChange) {
			apiErr(w, http.StatusConflict, codeBadState, "That device does not belong to a customer of another router.", nil)
			return
		}
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "customer "+a.Code+" moved to follow device "+mac)
	s.respondAccount(w, http.StatusOK, a.ID)
}

// Gone answers the retired router contacts calls.
func apiGone(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiErr(w, http.StatusGone, codeGone, "Router contacts were removed. The caretaker and landlord are fields of the apartment: see /api/v1/apartments.", nil)
}

// apiMACOtherBuilding answers the cross-building refusal.
func apiMACOtherBuilding(w http.ResponseWriter, dup *billing.DuplicateMACError, mac string) {
	apiErr(w, http.StatusConflict, codeMACOtherBuilding,
		"That MAC address belongs to "+dup.CustomerOrCustomer()+" in another apartment ("+dup.ApartmentName+", house number "+dup.Flat+"). Move the customer to this apartment first, or use another device.",
		map[string]any{"mac": mac, "belongs_to": map[string]any{
			"account_id": dup.AccountID, "code": dup.Code, "customer": dup.CustomerName,
			"apartment": map[string]any{"id": dup.ApartmentID, "name": dup.ApartmentName}, "unit": dup.Flat,
			"site_id": dup.SiteID, "site_name": dup.SiteName}})
}
