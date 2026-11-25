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
	h("POST /api/v1/accounts/{id}/onu/confirm", operator, s.apiAccountONUConfirm)
	h("POST /api/v1/accounts/{id}/onu/reject", operator, s.apiAccountONUReject)
	h("POST /api/v1/onu-tasks/{id}/cancel", operator, s.apiONUTaskCancel)
	h("POST /api/v1/onus/{id}/approve", operator, s.apiONUDecide("approve"))
	h("POST /api/v1/onus/{id}/block", operator, s.apiONUDecide("block"))
	h("POST /api/v1/onus/{id}/trust", store.RoleOwner, s.apiONUDecide("trust"))
	h("GET /api/v1/onus/unknown", viewer, s.apiUnknownONUs)
	h("POST /api/v1/onus/unknown/link", store.RoleOwner, s.apiLinkUnknownONU)
	h("PUT /api/v1/equipment/{id}/tr069-serial", store.RoleOwner, s.apiEquipmentTR069Serial)
	h("POST /api/v1/onus/{id}/unblock", operator, s.apiONUDecide("unblock"))
	h("GET /api/v1/sites/{id}/onus", viewer, s.apiSiteONUs)
	h("GET /api/v1/onus", viewer, s.apiONUs)
	h("POST /api/v1/onus/{id}/link", operator, s.apiONULink)
	h("POST /api/v1/onus/{id}/unlink", operator, s.apiONUUnlink)
	h("POST /api/v1/onus/{id}/inventory", operator, s.apiONUInventory)
	h("GET /api/v1/equipment", viewer, s.apiEquipmentList)
	h("POST /api/v1/equipment", operator, s.apiEquipmentAdd)
	h("POST /api/v1/equipment/import", operator, s.apiEquipmentImport)
	s.apiHierarchyRoutes(h)
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
	case errors.Is(err, acs.ErrNoONU), errors.Is(err, acs.ErrONUGone), errors.Is(err, acs.ErrNoAccount), errors.Is(err, acs.ErrNoItem):
		apiErr(w, http.StatusNotFound, codeNotFound, err.Error(), nil)
	case errors.Is(err, acs.ErrNothingToConfirm), errors.Is(err, acs.ErrNotQuarantined), errors.Is(err, acs.ErrTaskGone), errors.Is(err, acs.ErrTwoPlaces), errors.Is(err, acs.ErrNotWaiting), errors.Is(err, acs.ErrNoLeaseMAC), errors.Is(err, acs.ErrLeaseMoved), errors.Is(err, acs.ErrNotUnknown), errors.Is(err, acs.ErrItemUnfit):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
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

func (s *Server) apiAccountONUConfirm(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok || !s.apiACS(w) {
		return
	}
	if err := s.acs.ConfirmWiFi(c.user.Username, c.ip, a.ID); err != nil {
		s.acsErr(w, err)
		return
	}
	c.audit(s, "account "+a.Code+" Wi-Fi change confirmed working")
	tv, _ := s.acs.TaskStatus(a.ID)
	writeJSON(w, http.StatusOK, map[string]any{"task": tv})
}

// apiAccountONUReject: the owner says the accepted change did not take; the ACS tries another way.
func (s *Server) apiAccountONUReject(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok || !s.apiACS(w) {
		return
	}
	if _, err := s.acs.RejectWiFi(c.user.Username, c.ip, a.ID); err != nil {
		s.acsErr(w, err)
		return
	}
	c.audit(s, "account "+a.Code+" Wi-Fi change reported as not working")
	tv, _ := s.acs.TaskStatus(a.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"task": tv})
}

func (s *Server) apiONUTaskCancel(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	if err := s.acs.CancelTask(c.user.Username, c.ip, r.PathValue("id")); err != nil {
		s.acsErr(w, err)
		return
	}
	c.audit(s, "ONU task cancelled")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiONUDecide is the owner's answer to "ONU <serial> checked in from an unexpected place": approve (bind it
// to the new place) or block it. Unblock lifts a block.
func (s *Server) apiONUDecide(what string) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		if !s.apiACS(w) {
			return
		}
		var err error
		switch what {
		case "approve":
			err = s.acs.ApproveONU(c.user.Username, c.ip, r.PathValue("id"))
		case "block":
			err = s.acs.BlockONU(c.user.Username, c.ip, r.PathValue("id"))
		case "trust":
			// {mac}: optional, the lease MAC the owner was shown; refused with 409 when the ONU has moved on since.
			var req struct {
				MAC string `json:"mac"`
			}
			if !decode(w, r, &req, true) {
				return
			}
			err = s.acs.TrustONU(c.user.Username, c.ip, r.PathValue("id"), req.MAC)
		default:
			err = s.acs.UnblockONU(c.user.Username, c.ip, r.PathValue("id"))
		}
		if err != nil {
			s.acsErr(w, err)
			return
		}
		c.audit(s, "ONU "+r.PathValue("id")+" "+what)
		v, _ := s.acs.ONU(r.PathValue("id"))
		writeJSON(w, http.StatusOK, v)
	}
}

// apiUnknownONUs lists the serials that tried to check in and are not in the ONU inventory (kept a week).
func (s *Server) apiUnknownONUs(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	items := s.acs.UnknownONUs(r.URL.Query().Get("site_id"))
	if items == nil {
		items = []acs.UnknownONU{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// apiLinkUnknownONU: POST /onus/unknown/link {serial, equipment_id} (owner): the serial an unknown ONU reported becomes
// the TR-069 serial of the inventory item. The serial must be on the unknown list now.
func (s *Server) apiLinkUnknownONU(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if !s.apiACS(w) {
		return
	}
	var req struct {
		Serial      string `json:"serial"`
		EquipmentID string `json:"equipment_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	fields := map[string]string{}
	if strings.TrimSpace(req.Serial) == "" {
		fields["serial"] = "the serial the ONU reported is required"
	}
	if strings.TrimSpace(req.EquipmentID) == "" {
		fields["equipment_id"] = "choose the inventory item"
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	e, err := s.acs.LinkUnknown(c.user.Username, c.ip, req.EquipmentID, req.Serial)
	if err != nil {
		s.acsErr(w, err)
		return
	}
	c.audit(s, "unknown ONU "+e.TR069Serial+" linked to inventory item "+e.ID)
	writeJSON(w, http.StatusOK, s.equipDTO(e))
}

// apiEquipmentTR069Serial: PUT /equipment/{id}/tr069-serial {tr069_serial} (owner) sets or clears (empty) the serial an
// ONT reports over TR-069 when it is neither its serial nor its GPON serial.
func (s *Server) apiEquipmentTR069Serial(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		TR069Serial string `json:"tr069_serial"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	var cur store.Equipment
	var ok bool
	s.st.View(func(d *store.Data) { cur, ok = d.Equipment[r.PathValue("id")] })
	if !ok || cur.Kind != store.KindONT {
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
		return
	}
	e, err := s.svc.SaveEquipment(billing.EquipmentInput{ID: cur.ID, Kind: cur.Kind, Serial: cur.Serial, GPONSerial: cur.GPONSerial, TR069Serial: &req.TR069Serial,
		MAC: cur.MAC, Model: cur.Model, Firmware: cur.Firmware, SiteID: cur.SiteID, Notes: cur.Notes})
	if err != nil {
		if strings.Contains(err.Error(), "already in the register") {
			apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
			return
		}
		validationErr(w, map[string]string{"tr069_serial": err.Error()})
		return
	}
	c.audit(s, "register: ONT "+e.Serial+" TR-069 serial set to "+e.TR069Serial)
	writeJSON(w, http.StatusOK, s.equipDTO(e))
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
	if s.acs != nil && !s.acs.CanUseMAC(a.ID, mac) {
		validationErr(w, map[string]string{"mac": "that MAC was not reported by a verified session of this house's ONU"})
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
	raw := []store.Equipment{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if (kind == "" || e.Kind == kind) && (status == "" || e.Status == status) {
				raw = append(raw, e)
			}
		}
	})
	sort.Slice(raw, func(i, j int) bool { return raw[i].CreatedAt.After(raw[j].CreatedAt) })
	items := make([]apiEquipment, 0, len(raw))
	for _, e := range raw {
		items = append(items, s.equipDTO(e)) // never the sealed OLT password
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiEquipmentAdd(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		Kind       string `json:"kind"`
		Serial     string `json:"serial"`
		GPONSerial string `json:"gpon_serial"`
		// TR069Serial (owner only; ignored for others): the serial the box reports over TR-069 when it differs.
		TR069Serial *string `json:"tr069_serial"`
		MAC         string  `json:"mac"`
		Model       string  `json:"model"`
		Notes       string  `json:"notes"`
		OLTID       string  `json:"olt_id"`   // an ONU's OLT (optional)
		PONPort     int     `json:"pon_port"` // and its PON port (optional)
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
	if c.user.Role != store.RoleOwner {
		req.TR069Serial = nil
	}
	e, err := s.svc.SaveEquipment(billing.EquipmentInput{Kind: req.Kind, Serial: req.Serial, GPONSerial: req.GPONSerial, TR069Serial: req.TR069Serial, MAC: req.MAC, Model: req.Model, Notes: req.Notes,
		SetPlacement: req.OLTID != "" || req.PONPort != 0, OLTID: req.OLTID, PONPort: req.PONPort})
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
	writeJSON(w, http.StatusCreated, s.equipDTO(e))
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
