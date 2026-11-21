package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// The captive page's self-registrations, as the Android app lists them in New devices.

func (s *Server) registrationRoutes(h func(pattern, role string, f apiFunc)) {
	h("GET /api/v1/registrations", "viewer", s.apiRegistrations)
	h("POST /api/v1/registrations/{id}/approve", "operator", s.apiRegApprove)
	h("POST /api/v1/registrations/{id}/reject", "operator", s.apiRegReject)
	h("POST /api/v1/registrations/{id}/assign", "operator", s.apiRegAssign)
	h("POST /api/v1/registrations/reject-all", "operator", s.apiRegRejectAll)
}

func (s *Server) apiRegistrations(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	site := r.URL.Query().Get("router")
	if site == "" {
		site = r.URL.Query().Get("site")
	}
	items := []map[string]any{}
	for _, g := range s.svc.PendingRegistrations(func(id string) bool { return site == "" || id == site }) {
		cands := []map[string]any{}
		for _, d := range g.Candidates {
			cands = append(cands, map[string]any{"mac": d.MAC, "vendor": d.Vendor, "host_name": d.HostName, "online": d.Online, "ip": d.IP, "port": d.Port,
				"first_seen": d.FirstSeen.UTC().Format(time.RFC3339), "last_seen": d.LastSeen.UTC().Format(time.RFC3339),
				"name_match": d.NameMatch, "likely": d.Likely})
		}
		items = append(items, map[string]any{
			"id": g.ID, "site_id": g.SiteID, "site_name": g.SiteName, "apartment_id": g.ApartmentID, "apartment_name": g.ApartmentName,
			"unit": g.Unit, "name": g.Name, "phone": g.Phone, "email": g.Email, "mac": g.MAC, "guess_mac": g.GuessMAC,
			"device_name": g.DeviceName, "building": g.Building, "unassigned": g.Unassigned,
			"created_at": g.CreatedAt.UTC().Format(time.RFC3339), "candidates": cands,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) apiRegApprove(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		MAC string `json:"mac"`
	}
	if !decode(w, r, &req, true) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.svc.GetRegistration(id); !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such registration.", nil)
		return
	}
	mac := req.MAC
	if mac != "" {
		m, ok := macField(w, mac)
		if !ok {
			return
		}
		mac = m
	}
	a, err := s.svc.ApproveRegistration(r.Context(), id, mac, c.user.Username)
	switch {
	case errors.Is(err, billing.ErrRegDecided):
		apiErr(w, http.StatusConflict, "bad_state", err.Error(), nil)
		return
	case errors.Is(err, billing.ErrRegUnassigned):
		validationErr(w, map[string]string{"site_id": err.Error()})
		return
	case errors.Is(err, billing.ErrRegNeedsMAC):
		validationErr(w, map[string]string{"mac": err.Error()})
		return
	case err != nil:
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "registration approved: account "+a.Code)
	s.respondAccount(w, http.StatusCreated, a.ID)
}

func (s *Server) apiRegReject(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	id := r.PathValue("id")
	if _, ok := s.svc.GetRegistration(id); !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such registration.", nil)
		return
	}
	if err := s.svc.RejectRegistration(id, c.user.Username); err != nil {
		if errors.Is(err, billing.ErrRegDecided) {
			apiErr(w, http.StatusConflict, "bad_state", err.Error(), nil)
			return
		}
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "registration rejected")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiRegAssign gives a registration that named only a building its router and apartment.
func (s *Server) apiRegAssign(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID      string `json:"site_id"`
		ApartmentID string `json:"apartment_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.svc.GetRegistration(id); !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such registration.", nil)
		return
	}
	switch err := s.svc.AssignRegistration(id, req.SiteID, req.ApartmentID, c.user.Username); {
	case errors.Is(err, billing.ErrRegDecided):
		apiErr(w, http.StatusConflict, "bad_state", err.Error(), nil)
	case err != nil:
		validationErr(w, map[string]string{"site_id": err.Error()})
	default:
		c.audit(s, "registration assigned to a router")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// apiRegRejectAll rejects every waiting registration of one router: {"site_id":"..."}; an empty site_id with
// "unassigned": true is the group that names no router yet.
func (s *Server) apiRegRejectAll(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID     string `json:"site_id"`
		Unassigned bool   `json:"unassigned"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if req.SiteID == "" && !req.Unassigned {
		validationErr(w, map[string]string{"site_id": "Say which router, or set unassigned."})
		return
	}
	n, err := s.svc.RejectAllPending(req.SiteID, c.user.Username)
	if err != nil && n == 0 && !errors.Is(err, store.ErrNoChange) {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "registrations rejected: "+strconv.Itoa(n))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rejected": n})
}
