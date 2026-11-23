package web

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"ispbill/internal/acs"
	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// JSON API of ONU management and the ONU register (docs/api/v8-onu.md). A Wi-Fi password is accepted, never
// returned.

func (s *Server) onuRoutesAPI(h func(string, string, apiFunc)) {
	const (
		viewer   = store.RoleViewer
		operator = store.RoleOperator
	)
	h("GET /api/v1/accounts/{id}/onu", viewer, s.apiAccountONU)
	h("PUT /api/v1/accounts/{id}/wifi", operator, s.apiAccountWiFi)
	h("POST /api/v1/accounts/{id}/onu/reboot", operator, s.apiAccountONUAction("reboot"))
	h("POST /api/v1/accounts/{id}/onu/refresh", operator, s.apiAccountONUAction("refresh"))
	h("POST /api/v1/accounts/{id}/onu/swap", operator, s.apiAccountONUSwap)
	h("POST /api/v1/accounts/{id}/onu/use-reported-mac", operator, s.apiAccountUseMAC)
	h("GET /api/v1/sites/{id}/onus", viewer, s.apiSiteONUs)
	h("GET /api/v1/onus", viewer, s.apiONUs)
	h("POST /api/v1/onus/{id}/link", operator, s.apiONULink)
	h("POST /api/v1/onus/{id}/unlink", operator, s.apiONUUnlink)
	h("POST /api/v1/onus/{id}/inventory", operator, s.apiONUInventory)
	h("GET /api/v1/equipment", viewer, s.apiEquipmentList)
	h("POST /api/v1/equipment", operator, s.apiEquipmentAdd)
	h("POST /api/v1/equipment/import", operator, s.apiEquipmentImport)
}

func (s *Server) apiACS(w http.ResponseWriter) bool {
	if s.acs == nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "ONU management is off on this server.", nil)
		return false
	}
	return true
}

// acsErr maps the ONU service's refusals to the envelope.
func (s *Server) acsErr(w http.ResponseWriter, err error) {
	var fe *acs.FieldError
	switch {
	case errors.As(err, &fe):
		validationErr(w, map[string]string{fe.Field: fe.Msg})
	case errors.Is(err, acs.ErrNoONU), errors.Is(err, acs.ErrONUGone), errors.Is(err, acs.ErrNoAccount):
		apiErr(w, http.StatusNotFound, codeNotFound, err.Error(), nil)
	case errors.Is(err, acs.ErrLinked), errors.Is(err, acs.ErrONULinked), errors.Is(err, acs.ErrSite):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
	default:
		s.billingErr(w, err, "")
	}
}

func (s *Server) apiAccountONU(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok || !s.apiACS(w) {
		return
	}
	out := map[string]any{"onu": nil, "task": nil}
	if v, ok := s.acs.OfAccount(a.ID); ok {
		out["onu"] = v
		if t, ok := s.acs.TaskStatus(a.ID); ok {
			out["task"] = t
		}
	}
	var stock []store.Equipment
	s.st.View(func(d *store.Data) {
		if e, ok := billing.ONTOfAccount(d, a.ID); ok {
			out["register"] = e
		}
	})
	_ = stock
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiAccountWiFi(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok || !s.apiACS(w) {
		return
	}
	var req struct {
		SSID      string `json:"ssid"`
		Password  string `json:"password"`
		Same      *bool  `json:"same_for_both"` // default true
		SSID5     string `json:"ssid_5g"`
		Password5 string `json:"password_5g"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	same := req.Same == nil || *req.Same
	t, err := s.acs.SetWiFi(c.user.Username, c.ip, a.ID, acs.WiFiInput{SSID: req.SSID, Pass: req.Password, Same: same, SSID5: req.SSID5, Pass5: req.Password5})
	if err != nil {
		s.acsErr(w, err)
		return
	}
	c.audit(s, "account "+a.Code+" Wi-Fi change queued (the password is not logged)")
	tv, _ := s.acs.TaskStatus(a.ID)
	_ = t
	writeJSON(w, http.StatusAccepted, map[string]any{"task": tv})
}

func (s *Server) apiAccountONUAction(kind string) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		a, ok := s.accountOr404(w, r.PathValue("id"))
		if !ok || !s.apiACS(w) {
			return
		}
		var err error
		if kind == "reboot" {
			_, err = s.acs.RebootONU(c.user.Username, c.ip, a.ID)
		} else {
			_, err = s.acs.RefreshONU(c.user.Username, c.ip, a.ID)
		}
		if err != nil {
			s.acsErr(w, err)
			return
		}
		c.audit(s, "account "+a.Code+" ONU "+kind+" queued")
		tv, _ := s.acs.TaskStatus(a.ID)
		writeJSON(w, http.StatusAccepted, map[string]any{"task": tv})
	}
}

func (s *Server) apiAccountONUSwap(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		EquipmentID string `json:"equipment_id"`
		OldStatus   string `json:"old_status"`
		Note        string `json:"note"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if strings.TrimSpace(req.EquipmentID) == "" {
		validationErr(w, map[string]string{"equipment_id": "choose the new ONU from the register"})
		return
	}
	if err := s.svc.SwapONU(r.Context(), a.ID, req.EquipmentID, req.OldStatus, req.Note); err != nil {
		s.billingErr(w, err, "")
		return
	}
	if s.acs != nil {
		s.acs.AutoLinkSite(a.SiteID)
	}
	c.audit(s, "account "+a.Code+" ONU swapped")
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiAccountUseMAC(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		MAC string `json:"mac"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	mac, err := billing.NormalizeMAC(req.MAC)
	if err != nil {
		validationErr(w, map[string]string{"mac": "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"})
		return
	}
	if err := s.svc.UseReportedMAC(r.Context(), a.ID, mac); err != nil {
		s.billingErr(w, err, mac)
		return
	}
	if s.acs != nil {
		s.acs.AutoLinkSite(a.SiteID)
	}
	c.audit(s, "account "+a.Code+" MAC set to the one its ONU reported: "+mac)
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiSiteONUs(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok || !s.apiACS(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.acs.ONUs(st.ID), "management_network": st.MgmtSubnet()})
}

func (s *Server) apiONUs(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	list := s.acs.ONUs("")
	if q := r.URL.Query().Get("unlinked"); q == "1" || q == "true" {
		var f []acs.ONUView
		for _, v := range list {
			if v.AccountID == "" {
				f = append(f, v)
			}
		}
		list = f
	}
	if list == nil {
		list = []acs.ONUView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (s *Server) apiONULink(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if err := s.acs.LinkONU(c.user.Username, c.ip, r.PathValue("id"), req.AccountID); err != nil && !errors.Is(err, store.ErrNoChange) {
		s.acsErr(w, err)
		return
	}
	v, _ := s.acs.ONU(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiONUUnlink(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	if err := s.acs.UnlinkONU(c.user.Username, c.ip, r.PathValue("id")); err != nil && !errors.Is(err, store.ErrNoChange) {
		s.acsErr(w, err)
		return
	}
	v, _ := s.acs.ONU(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiONUInventory(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	var req struct {
		MAC string `json:"mac"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	v, ok := s.acs.ONU(r.PathValue("id"))
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such ONU.", nil)
		return
	}
	e, err := s.svc.SaveEquipment(billing.EquipmentInput{Kind: store.KindONT, Serial: v.Serial, MAC: req.MAC, Model: v.Model})
	if err != nil {
		s.billingErr(w, err, req.MAC)
		return
	}
	if v.AccountID != "" {
		if a, ok := s.svc.GetAccount(v.AccountID); ok && strings.EqualFold(a.MAC, e.MAC) {
			if err := s.svc.AssignEquipment(r.Context(), e.ID, a.ID, "", false); err != nil {
				s.billingErr(w, err, e.MAC)
				return
			}
		}
	}
	s.acs.AutoLinkSite(v.SiteID)
	c.audit(s, "ONU "+v.Serial+" added to the register")
	writeJSON(w, http.StatusCreated, e)
}

// ---- the register (ONUs) ----

func (s *Server) apiEquipmentList(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	kind, status := r.URL.Query().Get("kind"), r.URL.Query().Get("status")
	items := []store.Equipment{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if (kind == "" || e.Kind == kind) && (status == "" || e.Status == status) {
				items = append(items, e)
			}
		}
	})
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiEquipmentAdd(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		Kind       string `json:"kind"`
		Serial     string `json:"serial"`
		GPONSerial string `json:"gpon_serial"`
		MAC        string `json:"mac"`
		Model      string `json:"model"`
		Notes      string `json:"notes"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if req.Kind == "" {
		req.Kind = store.KindONT
	}
	fields := map[string]string{}
	if strings.TrimSpace(req.Serial) == "" {
		fields["serial"] = "the serial number is required"
	}
	if req.Kind == store.KindONT && strings.TrimSpace(req.MAC) == "" {
		fields["mac"] = "an ONU needs its WAN MAC (the one the router sees)"
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	e, err := s.svc.SaveEquipment(billing.EquipmentInput{Kind: req.Kind, Serial: req.Serial, GPONSerial: req.GPONSerial, MAC: req.MAC, Model: req.Model, Notes: req.Notes})
	if err != nil {
		if errors.Is(err, billing.ErrDuplicateEquipmentMAC) {
			apiErr(w, http.StatusConflict, codeMACInUse, err.Error(), map[string]any{"mac": req.MAC})
			return
		}
		if strings.Contains(err.Error(), "already in the register") {
			apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
			return
		}
		validationErr(w, map[string]string{"mac": err.Error()})
		return
	}
	c.audit(s, "register: "+e.Kind+" "+e.Serial+" added")
	writeJSON(w, http.StatusCreated, e)
}

func (s *Server) apiEquipmentImport(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		Lines string `json:"lines"` // serial,mac,model[,gpon serial] per line, or kind,serial,mac,model
	}
	if !decode(w, r, &req, false) {
		return
	}
	res, err := s.svc.ImportEquipment(req.Lines)
	if err != nil {
		validationErr(w, map[string]string{"lines": err.Error()})
		return
	}
	if res.Skipped == nil {
		res.Skipped = []string{}
	}
	c.audit(s, "register import: "+itoa(res.Added)+" added")
	writeJSON(w, http.StatusOK, map[string]any{"added": res.Added, "skipped": res.Skipped})
}
