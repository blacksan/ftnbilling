package web

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"ispbill/internal/olt"
	"ispbill/internal/store"
)

// JSON API of OLT management (docs/api/v9-olt.md). Nothing here ever returns a password, an enable password or a
// saved configuration with its secrets: backups come back redacted, to owners only. Changing what the server may do
// to an OLT (provisioning, plan, apply) is owner-only and needs the owner's password again in the request.

func (s *Server) oltRoutesAPI(h func(string, string, apiFunc)) {
	const (
		viewer   = store.RoleViewer
		operator = store.RoleOperator
		owner    = store.RoleOwner
	)
	h("GET /api/v1/olts/{id}/status", viewer, s.apiOLTStatus)
	h("POST /api/v1/olts/{id}/test", operator, s.apiOLTTest)
	h("POST /api/v1/olts/{id}/poll", operator, s.apiOLTPoll)
	h("GET /api/v1/olts/{id}/backups", owner, s.apiOLTBackups)
	h("GET /api/v1/olts/{id}/backups/{bid}", owner, s.apiOLTBackup)
	h("PUT /api/v1/olts/{id}/provisioning", owner, s.apiOLTProvisioning)
	h("POST /api/v1/olts/{id}/plan", owner, s.apiOLTPlan)
	h("POST /api/v1/olts/{id}/apply", owner, s.apiOLTApply)
}

func (s *Server) apiOLT(w http.ResponseWriter, r *http.Request) (store.Equipment, store.OLTState, bool) {
	if s.olt == nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "OLT management is off on this server.", nil)
		return store.Equipment{}, store.OLTState{}, false
	}
	e, st, ok := s.olt.Load(r.PathValue("id"))
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
		return e, st, false
	}
	return e, st, true
}

type apiOLTONU struct {
	PON      int    `json:"pon"`
	ID       int    `json:"id"`
	Serial   string `json:"serial,omitempty"`
	State    string `json:"state"`
	Online   bool   `json:"online"`
	RxPower  string `json:"rx_power,omitempty"`
	TxPower  string `json:"tx_power,omitempty"`
	Distance string `json:"distance,omitempty"`
	LastDown string `json:"last_down,omitempty"`
}

type apiOLTFound struct {
	PON       int       `json:"pon"`
	Serial    string    `json:"serial"`
	Verdict   string    `json:"verdict"`
	Detail    string    `json:"detail,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
}

func (s *Server) apiOLTStatus(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	e, st, ok := s.apiOLT(w, r)
	if !ok {
		return
	}
	onus := []apiOLTONU{}
	online := 0
	for _, o := range st.Status.ONUs {
		if o.Online {
			online++
		}
		onus = append(onus, apiOLTONU{PON: o.PON, ID: o.ID, Serial: o.Serial, State: o.State, Online: o.Online, RxPower: o.RxPower, TxPower: o.TxPower, Distance: o.Distance, LastDown: o.LastDown})
	}
	found, unknown := []apiOLTFound{}, []apiOLTFound{}
	for _, f := range st.Found {
		row := apiOLTFound{PON: f.PON, Serial: f.Serial, Verdict: f.Verdict, Detail: f.Detail, FirstSeen: f.FirstSeen}
		found = append(found, row)
		if f.Verdict == store.OLTFoundUnknown {
			unknown = append(unknown, row)
		}
	}
	sort.Slice(onus, func(i, j int) bool {
		if onus[i].PON != onus[j].PON {
			return onus[i].PON < onus[j].PON
		}
		return onus[i].ID < onus[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"olt_id": e.ID, "name": store.OLTLabel(e), "configured": e.MgmtIP != "" && e.MgmtUser != "" && e.MgmtPass != "",
		"protocol": st.Settings.ProtoOrDefault(), "connected": st.Status.OK, "checked_at": tp(st.Status.At), "error": st.Status.Error, "version": st.Status.Version, "model": st.Status.Model, "sw_version": st.Status.SWVersion, "hw_version": st.Status.HWVersion, "findings": nz(st.Status.Findings), "version_note": s.olt.Dialect().VersionNote(st.Status.Model, st.Status.SWVersion),
		"online": online, "total": len(onus), "onus": onus, "auto_found": found, "not_in_inventory": unknown,
		"provisioning": map[string]any{"enabled": st.Settings.Provisioning, "dialect_checked": st.Settings.DialectChecked},
		"host_key":     st.Settings.HostKey, "unparsed": st.Status.Raw,
	})
}

func (s *Server) apiOLTErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, olt.ErrNotFound):
		apiErr(w, http.StatusNotFound, codeNotFound, "Not found.", nil)
	case errors.Is(err, olt.ErrBusy), errors.Is(err, olt.ErrPlanChanged):
		apiErr(w, http.StatusConflict, codeBadState, oltErrText(err), nil)
	case errors.Is(err, olt.ErrProvisioningOff):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
	case errors.Is(err, olt.ErrNotConfigured), errors.Is(err, olt.ErrNoKey):
		validationErr(w, map[string]string{"olt": err.Error()})
	default:
		apiErr(w, http.StatusBadGateway, "olt_error", oltErrText(err), nil)
	}
}

func (s *Server) apiOLTTest(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	v, err := s.olt.TestConnection(ctx, r.PathValue("id"), c.user.Username)
	if err != nil {
		s.apiOLTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": v})
}

func (s *Server) apiOLTPoll(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	if err := s.olt.Poll(ctx, r.PathValue("id"), true); err != nil {
		s.apiOLTErr(w, err)
		return
	}
	s.apiOLTStatus(w, r, c)
}

func (s *Server) apiOLTBackups(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	items := []map[string]any{}
	for _, b := range s.olt.BackupList(r.PathValue("id")) {
		items = append(items, map[string]any{"id": b.ID, "at": b.At, "why": b.Why, "by": b.By, "size": b.Size})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiOLTBackup(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	b, text, err := s.olt.BackupText(r.PathValue("id"), r.PathValue("bid"))
	if err != nil {
		s.apiOLTErr(w, err)
		return
	}
	c.audit(s, "OLT saved configuration "+b.ID+" viewed")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id": b.ID, "at": b.At, "why": b.Why, "text": text, "redacted": true})
}

// apiStepUp checks the owner's password sent in the body: five wrong ones in ten minutes lock the checks out.
func (s *Server) apiStepUp(w http.ResponseWriter, c *apiCtx, password string) bool {
	if !s.oltRL.allow("olt-step:" + c.user.ID) {
		apiErr(w, http.StatusTooManyRequests, "rate_limited", "Too many password checks; wait a few minutes.", nil)
		return false
	}
	if password == "" {
		validationErr(w, map[string]string{"password": "type your password again to do this"})
		return false
	}
	good, busy := s.auth.password(c.user, true, password, true)
	if busy {
		apiErr(w, http.StatusServiceUnavailable, "busy", "The server is busy; try again in a moment.", nil)
		return false
	}
	if !good {
		apiErr(w, http.StatusForbidden, codeInvalidCreds, "The password is wrong.", nil)
		return false
	}
	return true
}

func (s *Server) apiOLTProvisioning(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		Enabled        *bool  `json:"enabled"`
		DialectChecked *bool  `json:"dialect_checked"`
		Password       string `json:"password"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	if req.Enabled == nil && req.DialectChecked == nil {
		validationErr(w, map[string]string{"enabled": "say enabled or dialect_checked"})
		return
	}
	if !s.apiStepUp(w, c, req.Password) {
		return
	}
	id := r.PathValue("id")
	if req.DialectChecked != nil {
		if err := s.olt.SetDialectChecked(id, *req.DialectChecked, c.user.Username); err != nil {
			s.apiOLTErr(w, err)
			return
		}
	}
	if req.Enabled != nil {
		if err := s.olt.SetProvisioning(id, *req.Enabled, c.user.Username); err != nil {
			s.apiOLTErr(w, err)
			return
		}
	}
	s.apiOLTStatus(w, r, c)
}

func planJSON(p olt.Plan, hash string) map[string]any {
	p = p.Display() // the login in a TR-069 line is hidden
	steps := []map[string]any{}
	for _, st := range p.Steps {
		steps = append(steps, map[string]any{"title": st.Title, "lines": st.Lines})
	}
	adds := []map[string]any{}
	for _, a := range p.Adds {
		adds = append(adds, map[string]any{"pon": a.PON, "id": a.ID, "serial": a.Serial, "label": a.Label})
	}
	return map[string]any{"hash": hash, "empty": p.Empty(), "lines": len(p.Commands()), "steps": steps, "adds": adds, "notes": nz(p.Notes), "warnings": nz(p.Warnings)}
}

func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (s *Server) apiOLTPlan(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	p, hash, err := s.olt.Plan(ctx, r.PathValue("id"))
	if err != nil {
		s.apiOLTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, planJSON(p, hash))
}

func (s *Server) apiOLTApply(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		Hash     string `json:"hash"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if _, _, ok := s.apiOLT(w, r); !ok {
		return
	}
	if req.Hash == "" {
		validationErr(w, map[string]string{"hash": "send the hash of the plan you looked at"})
		return
	}
	if !s.apiStepUp(w, c, req.Password) {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	res, err := s.olt.Apply(ctx, r.PathValue("id"), req.Hash, c.user.Username)
	if err != nil {
		s.apiOLTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup_id": res.Backup, "sent": len(res.Sent), "verified": res.Verified, "remaining": nz(res.Remaining)})
}
