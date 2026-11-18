package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

const (
	codeRouterUnreachable = "router_unreachable"
	codeRouterRefused     = "router_refused"
)

// setupRoutes registers the router setup API (docs/release-plan-routers.md
// section 3 and docs/api/v4-w5c.md).
func (s *Server) setupRoutes(h func(pattern, role string, f apiFunc)) {
	const (
		viewer   = store.RoleViewer
		operator = store.RoleOperator
		owner    = store.RoleOwner
	)
	h("POST /api/v1/routers", owner, s.apiRouterCreate)
	h("PATCH /api/v1/routers/{id}", owner, s.apiRouterPatch)
	h("GET /api/v1/routers/{id}/setup", viewer, s.apiSetup)
	h("POST /api/v1/routers/{id}/setup/run", operator, s.apiSetupRun)
	h("POST /api/v1/routers/{id}/setup/migrate", operator, s.apiSetupMigrate)
	h("POST /api/v1/routers/{id}/setup/rollback", operator, s.apiSetupRollback)

	// W5 endpoints, kept as thin views over the job (aliased under /routers by h).
	h("GET /api/v1/sites/{id}/checks", viewer, s.apiChecks)
	h("POST /api/v1/sites/{id}/checks/{check_id}/fix", operator, s.apiCheckFix)
	h("PUT /api/v1/sites/{id}/mode", operator, s.apiSiteMode)
	h("POST /api/v1/sites/{id}/join-command", owner, s.apiJoinCommand)
	h("GET /api/v1/sites/{id}/cutover", viewer, s.apiCutover)
	h("POST /api/v1/sites/{id}/cutover/start", operator, s.apiCutoverStart)
	h("POST /api/v1/sites/{id}/cutover/confirm", operator, s.apiCutoverConfirm)
	h("POST /api/v1/sites/{id}/cutover/rollback", operator, s.apiSetupRollback)
	h("POST /api/v1/sites/{id}/bench/test-flat", operator, s.apiBenchTestFlat)
	h("POST /api/v1/sites/{id}/bench/suspend-test", operator, s.apiBenchSuspend)
}

// idemExempt: calls whose answer holds a one-time secret are never replayed from the Idempotency-Key cache.
func idemExempt(r *http.Request) bool {
	p := strings.TrimRight(r.URL.Path, "/")
	return r.Method == http.MethodPost && (p == "/api/v1/routers" || strings.HasSuffix(p, "/join-command"))
}

func (s *Server) routerFail(w http.ResponseWriter, err error) bool {
	var rr *sites.RouterRefusedError
	switch {
	case err == nil:
		return false
	case errors.As(err, &rr):
		apiErr(w, http.StatusConflict, codeRouterRefused, "The router refused the change.", map[string]any{"router_error": rr.Msg})
	case errors.Is(err, sites.ErrRouterUnreachable):
		apiErr(w, http.StatusServiceUnavailable, codeRouterUnreachable, "The router does not answer. Check the tunnel and try again.", nil)
	default:
		return false
	}
	return true
}

func (s *Server) setupJSON(w http.ResponseWriter, r *http.Request, status int, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	v, ok := s.mgr.SetupView(ctx, id)
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}

func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	s.setupJSON(w, r, http.StatusOK, st.ID)
}

func (s *Server) jobStartErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sites.ErrJobRunning):
		apiErr(w, http.StatusConflict, codeBadState, "A setup job is already running for this router.", nil)
	case errors.Is(err, sites.ErrNotJoined):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
	case errors.Is(err, sites.ErrNameMismatch):
		validationErr(w, map[string]string{"confirm_name": "type the router name to confirm"})
	default:
		return s.routerFail(w, err)
	}
	return true
}

func (s *Server) apiSetupRun(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	if s.jobStartErr(w, s.mgr.StartProvision(st.ID, sites.ProvOptions{Trigger: "run", Actor: c.user.Username})) {
		return
	}
	c.audit(s, "router "+st.Name+" setup run")
	s.setupJSON(w, r, http.StatusAccepted, st.ID)
}

func (s *Server) apiSetupMigrate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
		KeepUnknown bool   `json:"keep_unknown"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if s.jobStartErr(w, s.mgr.StartMigration(st.ID, req.ConfirmName, req.KeepUnknown, c.user.Username)) {
		return
	}
	c.audit(s, "router "+st.Name+" migration started")
	s.setupJSON(w, r, http.StatusAccepted, st.ID)
}

func (s *Server) apiSetupRollback(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.mgr.RollbackNow(st.ID, c.user.Username); err != nil {
		if !s.routerFail(w, err) {
			apiErr(w, http.StatusServiceUnavailable, codeRouterUnreachable, "The router does not answer, so it could not be told to roll back. Its own timer rolls it back if a change is waiting.", nil)
		}
		return
	}
	c.audit(s, "router "+st.Name+" rollback now")
	s.setupJSON(w, r, http.StatusAccepted, st.ID)
}

// ---- add and edit a router -------------------------------------------------------------------

type routerReq struct {
	Name             *string  `json:"name"`
	ServingCustomers *bool    `json:"serving_customers"`
	WANInterface     *string  `json:"wan_interface"`
	OLTInterface     *string  `json:"olt_interface"`
	VLANID           *int     `json:"vlan_id"`
	CustomerSubnet   *string  `json:"customer_subnet"`
	WANCapMbps       *float64 `json:"wan_cap_mbps"`
	SupportPhone     *string  `json:"support_phone"`
}

func applyRouterReq(in sites.SiteInput, q routerReq) sites.SiteInput {
	if q.Name != nil {
		in.Name = *q.Name
	}
	if q.WANInterface != nil {
		in.WANInterface = *q.WANInterface
	}
	if q.OLTInterface != nil {
		in.OLTInterface = *q.OLTInterface
	}
	if q.VLANID != nil {
		in.VLANID = *q.VLANID
	}
	if q.CustomerSubnet != nil {
		in.CustomerNet = *q.CustomerSubnet
	}
	if q.WANCapMbps != nil {
		in.WANCapMbps = *q.WANCapMbps
	}
	if q.SupportPhone != nil {
		in.SupportPhone = *q.SupportPhone
	}
	return in
}

func (s *Server) joinBody(st store.Site) (map[string]any, error) {
	tok, exp, err := s.mgr.MintJoin(st.ID)
	if err != nil {
		return nil, err
	}
	intent := s.mgr.JoinIntentOf(st.ID)
	hs := intent == sites.IntentServing
	return map[string]any{
		"command":         sites.JoinCommandIntent(s.cfg.PublicBaseURL, tok, true, "", intent, hs),
		"command_nocheck": sites.JoinCommandIntent(s.cfg.PublicBaseURL, tok, false, "", intent, hs),
		"expires_at":      exp,
	}, nil
}

func (s *Server) apiRouterCreate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req routerReq
	if !decode(w, r, &req, false) {
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		validationErr(w, map[string]string{"name": "a router name is required"})
		return
	}
	serving := req.ServingCustomers != nil && *req.ServingCustomers
	st, err := s.mgr.CreateRouter(applyRouterReq(sites.SiteInput{}, req), serving)
	if err != nil {
		if s.storeErr(w, err) {
			return
		}
		validationErr(w, map[string]string{"name": err.Error()})
		return
	}
	join, err := s.joinBody(st)
	if err != nil {
		apiErr(w, http.StatusConflict, codeBadState, "The router was added but no join line could be made: "+err.Error(), map[string]any{"router_id": st.ID})
		return
	}
	c.audit(s, "router "+st.Name+" added")
	w.Header().Set("Cache-Control", "no-store")
	h := s.siteHealths(r.Context(), []store.Site{st})[st.ID]
	writeJSON(w, http.StatusCreated, map[string]any{"router": s.siteJSON(st, h, nil), "join": join})
}

func (s *Server) apiRouterPatch(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req routerReq
	if !decode(w, r, &req, false) {
		return
	}
	in := sites.SiteInput{Name: st.Name, Notes: st.Notes, SupportPhone: st.SupportPhone, HotspotServer: st.HotspotServer, WANInterface: st.WANInterface,
		WANList: st.WANList, OLTInterface: st.OLTInterface, CustomerNet: st.CustomerSubnet, VLANID: st.VLANID, WANCapMbps: st.WANCapMbps,
		OLTCapMbps: st.OLTCapMbps, FUPTB: st.FUPTB, Provision: st.Provision, LegacyContainer: st.LegacyContainer, BenchPort: st.BenchPort}
	if err := s.mgr.UpdateSite(st.ID, applyRouterReq(in, req)); err != nil {
		if !s.storeErr(w, err) {
			validationErr(w, map[string]string{"name": err.Error()})
		}
		return
	}
	if req.ServingCustomers != nil {
		s.mgr.SetServingCustomers(st.ID, *req.ServingCustomers)
	}
	c.audit(s, "router "+st.Name+" edited")
	st, _ = s.site404(w, st.ID)
	h := s.siteHealths(r.Context(), []store.Site{st})[st.ID]
	out := s.siteJSON(st, h, s.siteCounts()[st.ID])
	out["serving_customers"] = s.mgr.ServingCustomers(st.ID)
	writeJSON(w, http.StatusOK, out)
}

// ---- W5 endpoints ------------------------------------------------------------------------------

func (s *Server) apiChecks(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	ck, err := s.mgr.Checks(ctx, st.ID, r.URL.Query().Get("fresh") == "1" || r.URL.Query().Get("fresh") == "true")
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ck)
}

func (s *Server) apiCheckFix(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if !decode(w, r, &req, true) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := s.mgr.ApplyFix(ctx, st.ID, r.PathValue("check_id"), req.Confirm, c.user.Role)
	switch {
	case err == nil:
		c.audit(s, "router "+st.Name+" fix "+r.PathValue("check_id"))
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, sites.ErrUnknownCheck):
		apiErr(w, http.StatusNotFound, codeNotFound, err.Error(), nil)
	case errors.Is(err, sites.ErrNeedsOwner), errors.Is(err, sites.ErrFixForbidden):
		apiErr(w, http.StatusForbidden, codeForbidden, err.Error(), nil)
	case errors.Is(err, sites.ErrNoFix), errors.Is(err, sites.ErrConfirmRequired), errors.Is(err, sites.ErrPreconditionChanged), errors.Is(err, sites.ErrFixNotAllowed):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
	default:
		if !s.routerFail(w, err) {
			apiErr(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
		}
	}
}

func (s *Server) apiSiteMode(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Mode     string `json:"mode"`
		Override bool   `json:"override"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if s.mgr.SetupBusy(st.ID) {
		apiErr(w, http.StatusConflict, codeBadState, "A setup job is running for this router.", nil)
		return
	}
	err := s.mgr.SetAccessModeForce(st.ID, req.Mode, req.Override)
	switch {
	case err == nil, errors.Is(err, store.ErrNoChange):
	case errors.Is(err, store.ErrBadAccessMode):
		validationErr(w, map[string]string{"mode": "must be hotspot, ipoe_observe or ipoe"})
		return
	case errors.Is(err, sites.ErrRouterIsIPoE), errors.Is(err, sites.ErrRouterUnknown):
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
		return
	default:
		apiErr(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
		return
	}
	c.audit(s, "router "+st.Name+" mode "+req.Mode)
	st, _ = s.site404(w, st.ID)
	h := s.siteHealths(r.Context(), []store.Site{st})[st.ID]
	writeJSON(w, http.StatusOK, s.siteJSON(st, h, s.siteCounts()[st.ID]))
}

func (s *Server) apiJoinCommand(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	join, err := s.joinBody(st)
	if err != nil {
		apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
		return
	}
	c.audit(s, "router "+st.Name+" join command made")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, join)
}

// cutoverView is the Cutover of the W5 contract, read off the job.
func (s *Server) cutoverView(ctx context.Context, id string) map[string]any {
	v, _ := s.mgr.SetupView(ctx, id)
	state := "idle"
	switch v.State {
	case sites.JobArming, sites.JobSwitching, sites.JobStaging, sites.JobBuilding, sites.JobChecking, sites.JobFixing:
		state = "running"
	case sites.JobVerifying:
		state = "awaiting_confirm" // the server decides by itself; nobody confirms
	case sites.JobLive:
		state = "confirmed"
	case sites.JobRollingBack:
		state = "rolling_back"
	case sites.JobRolledBack:
		state = "rolled_back"
	case sites.JobFailed:
		state = "failed"
	}
	return map[string]any{"state": state, "method": "server", "started_at": nil, "deadline_at": v.DeadlineAt, "seconds_left": v.SecondsLeft,
		"steps": v.Steps, "can_confirm": false, "blockers": v.Blockers, "last_error": v.LastError,
		"evidence": map[string]any{"tunnel_up": v.Evidence.TunnelUp, "flats_bound": v.Evidence.FlatsBound, "flats_entitled": v.Evidence.FlatsRequired, "internet_ok": v.Evidence.InternetOK},
		"auto":     true}
}

func (s *Server) apiCutover(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.cutoverView(r.Context(), st.ID))
}

func (s *Server) apiCutoverStart(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
		KeepUnknown bool   `json:"keep_unknown"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if s.jobStartErr(w, s.mgr.StartMigration(st.ID, req.ConfirmName, req.KeepUnknown, c.user.Username)) {
		return
	}
	c.audit(s, "router "+st.Name+" migration started")
	writeJSON(w, http.StatusAccepted, s.cutoverView(r.Context(), st.ID))
}

// apiCutoverConfirm: the server confirms by itself on evidence; this only returns the state. For the legacy
// paste cutover (a router timer, or the kit's markers set by hand) it still confirms.
func (s *Server) apiCutoverConfirm(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.mgr.SetupBusy(st.ID) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if pend, known := s.mgr.CutoverPending(ctx, st.ID); known && pend {
			if err := s.mgr.ConfirmCutover(ctx, st.ID); err != nil && !s.routerFail(w, err) {
				apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
				return
			}
			c.audit(s, "router "+st.Name+" legacy cutover confirmed")
		}
	}
	writeJSON(w, http.StatusOK, s.cutoverView(r.Context(), st.ID))
}

// ---- bench ------------------------------------------------------------------------------------

const benchFlat = "Bench test"

func (s *Server) apiBenchTestFlat(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		MAC       string `json:"mac"`
		PackageID string `json:"package_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	mac, err := billing.NormalizeMAC(req.MAC)
	if err != nil {
		validationErr(w, map[string]string{"mac": "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"})
		return
	}
	if req.PackageID == "" {
		validationErr(w, map[string]string{"package_id": "choose a package"})
		return
	}
	var existing store.Account
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == st.ID && a.Flat == benchFlat {
				existing = a
			}
		}
	})
	var id string
	if existing.ID != "" {
		if existing.MAC != mac {
			if err := s.svc.ChangeMAC(r.Context(), existing.ID, mac); err != nil {
				s.billingErr(w, err, mac)
				return
			}
		}
		if err := s.svc.ExtendAccount(r.Context(), existing.ID, req.PackageID, "bench test", false); err != nil {
			s.billingErr(w, err, mac)
			return
		}
		id = existing.ID
	} else {
		a, _, err := s.svc.CreateCustomer(r.Context(), billing.AddCustomerInput{SiteID: st.ID, Flat: benchFlat, MAC: mac, PackageID: req.PackageID, Activate: true, Note: "bench test"})
		if err != nil {
			s.billingErr(w, err, mac)
			return
		}
		id = a.ID
	}
	c.audit(s, "router "+st.Name+" bench test flat")
	s.respondAccount(w, http.StatusOK, id)
}

func (s *Server) apiBenchSuspend(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	a, ok := s.accountOr404(w, req.AccountID)
	if !ok {
		return
	}
	if a.SiteID != st.ID {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such account on this router.", nil)
		return
	}
	if st.AccessMode != store.AccessIPoE {
		apiErr(w, http.StatusConflict, codeBadState, "The suspend test needs the router in IPoE mode: in hotspot and observe mode nobody is walled.", nil)
		return
	}
	type step struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var steps []step
	listed := func() bool {
		rows, err := s.svc.RouterRows(r.Context(), st.ID, "/ip/firewall/address-list", map[string]string{"list": billing.SuspendedList})
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row["address"] == a.IP {
				return true
			}
		}
		return false
	}
	for _, sus := range []bool{true, false} {
		name := "suspend"
		if !sus {
			name = "unsuspend"
		}
		if err := s.svc.SetSuspended(r.Context(), a.ID, sus); err != nil {
			steps = append(steps, step{name, false, err.Error()})
			break
		}
		_, rerr := s.svc.ReconcileSite(r.Context(), st.ID)
		on := listed()
		steps = append(steps, step{name, rerr == nil && on == sus, "on the walled list: " + map[bool]string{true: "yes", false: "no"}[on]})
	}
	c.audit(s, "router "+st.Name+" suspend test")
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps})
}
