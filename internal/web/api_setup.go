package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

const (
	// codeConfirm: the change needs {"confirm": true} (a 409 that names what the operator is accepting).
	codeConfirm           = "confirm_required"
	codeRouterUnreachable = "router_unreachable"
	codeRouterRefused     = "router_refused"
)

// setupRoutes registers the router setup API (docs/api/v5-bootstrap.md).
func (s *Server) setupRoutes(h func(pattern, role string, f apiFunc)) {
	const (
		viewer   = store.RoleViewer
		operator = store.RoleOperator
		owner    = store.RoleOwner
	)
	h("POST /api/v1/routers", owner, s.apiRouterCreate)
	h("PATCH /api/v1/routers/{id}", owner, s.apiRouterPatch)
	h("GET /api/v1/routers/{id}/setup", viewer, s.apiSetup)
	h("POST /api/v1/routers/{id}/actions/reset", owner, s.apiRouterReset)
	h("POST /api/v1/routers/{id}/actions/pause-auto-fixes", owner, s.apiPauseAutoFixes)
	h("POST /api/v1/routers/{id}/script", owner, s.apiRouterScript)

	h("GET /api/v1/sites/{id}/checks", viewer, s.apiChecks)
	h("POST /api/v1/sites/{id}/checks/{check_id}/fix", operator, s.apiCheckFix)
	h("POST /api/v1/sites/{id}/join-command", owner, s.apiJoinCommand)
	h("POST /api/v1/sites/{id}/bench/test-flat", operator, s.apiBenchTestFlat)
	h("POST /api/v1/sites/{id}/bench/suspend-test", operator, s.apiBenchSuspend)

	// Gone in v5 (answered 410 for one release): the set-up job, migration, rollback, access mode and cutover.
	for _, p := range []string{
		"POST /api/v1/routers/{id}/setup/run", "POST /api/v1/routers/{id}/setup/migrate", "POST /api/v1/routers/{id}/setup/rollback",
		"PUT /api/v1/sites/{id}/mode", "GET /api/v1/sites/{id}/cutover", "POST /api/v1/sites/{id}/cutover/start",
		"POST /api/v1/sites/{id}/cutover/confirm", "POST /api/v1/sites/{id}/cutover/rollback",
	} {
		h(p, viewer, s.apiGone)
	}
}

const codeGone = "gone"

// apiGone answers the endpoints v5 removed.
func (s *Server) apiGone(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	apiErr(w, http.StatusGone, codeGone, "This call was removed in v5. A router is set up by pasting one line on it; see docs/api/v5-bootstrap.md.", nil)
}

// idemExempt: calls whose answer holds a one-time secret are never replayed from the Idempotency-Key cache.
func idemExempt(r *http.Request) bool {
	p := strings.TrimRight(r.URL.Path, "/")
	return r.Method == http.MethodPost && (p == "/api/v1/routers" || strings.HasSuffix(p, "/join-command") || strings.HasSuffix(p, "/script"))
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

// ---- add and edit a router -------------------------------------------------------------------

type routerReq struct {
	Name           *string   `json:"name"`
	WANInterface   *string   `json:"wan_interface"`
	WANMode        *string   `json:"wan_mode"`
	WANAddress     *string   `json:"wan_address"`
	WANGateway     *string   `json:"wan_gateway"`
	WANDNS         *string   `json:"wan_dns"`
	PPPoEUser      *string   `json:"pppoe_user"`
	PPPoEPass      *string   `json:"pppoe_pass"`
	PPPoEPassClear *bool     `json:"pppoe_pass_clear"`
	LANPorts       *string   `json:"lan_ports"`
	LANSubnet      *string   `json:"lan_subnet"`
	OLTInterface   *string   `json:"olt_interface"`
	VLANID         *int      `json:"vlan_id"`
	CustomerSubnet *string   `json:"customer_subnet"`
	CustomerPorts  *portList `json:"customer_ports"`
	BenchPort      *string   `json:"bench_port"` // deprecated alias: the first customer port
	WANCapMbps     *float64  `json:"wan_cap_mbps"`
	OLTCapMbps     *float64  `json:"olt_cap_mbps"`
	FUPTB          *float64  `json:"fup_tb"`
	SupportPhone   *string   `json:"support_phone"`
	// Confirm accepts the warning for a change of the OLT port or the VLAN on a router that is set up
	// ("Customers on the OLT lose service until the OLT uses the new setting").
	Confirm *bool `json:"confirm"`
	// ConfirmName is the typed router name that a change of lan_ports on a set-up router needs.
	ConfirmName string `json:"confirm_name"`
}

// portList is a JSON array of port names, or a comma list in one string.
type portList []string

func (p *portList) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		var one string
		if err2 := json.Unmarshal(b, &one); err2 != nil {
			return errors.New("customer_ports must be a list of port names")
		}
		arr = strings.Split(one, ",")
	}
	out := []string{}
	for _, a := range arr {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	*p = out
	return nil
}

func applyRouterReq(in sites.SiteInput, q routerReq) sites.SiteInput {
	str := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	str(&in.Name, q.Name)
	str(&in.WANInterface, q.WANInterface)
	str(&in.WANMode, q.WANMode)
	str(&in.WANAddress, q.WANAddress)
	str(&in.WANGateway, q.WANGateway)
	str(&in.WANDNS, q.WANDNS)
	str(&in.PPPoEUser, q.PPPoEUser)
	str(&in.PPPoEPass, q.PPPoEPass)
	str(&in.LANPorts, q.LANPorts)
	str(&in.LANSubnet, q.LANSubnet)
	str(&in.OLTInterface, q.OLTInterface)
	str(&in.CustomerNet, q.CustomerSubnet)
	switch {
	case q.CustomerPorts != nil:
		in.CustomerPorts = strings.Join(*q.CustomerPorts, ",")
	case q.BenchPort != nil: // the alias replaces (or clears) the first customer port
		rest := sites.SplitPorts(in.CustomerPorts)
		if len(rest) > 0 {
			rest = rest[1:]
		}
		if b := strings.TrimSpace(*q.BenchPort); b != "" {
			rest = append([]string{b}, rest...)
		}
		in.CustomerPorts = strings.Join(rest, ",")
	}
	str(&in.SupportPhone, q.SupportPhone)
	if q.PPPoEPassClear != nil {
		in.PPPoEPassClear = *q.PPPoEPassClear
	}
	in.Confirm = q.Confirm != nil && *q.Confirm
	in.ConfirmName = q.ConfirmName
	if q.VLANID != nil {
		in.VLANID = *q.VLANID
	}
	if q.WANCapMbps != nil {
		in.WANCapMbps = *q.WANCapMbps
	}
	if q.OLTCapMbps != nil {
		in.OLTCapMbps = *q.OLTCapMbps
	}
	if q.FUPTB != nil {
		in.FUPTB = *q.FUPTB
	}
	return in
}

// joinBody mints a new one-time line for the router. It voids the earlier one.
func (s *Server) joinBody(st store.Site) (map[string]any, error) {
	tok, exp, err := s.mgr.MintJoin(st.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"command":         sites.BootstrapCommand(s.cfg.PublicBaseURL, tok, true),
		"command_nocheck": sites.BootstrapCommand(s.cfg.PublicBaseURL, tok, false),
		"expires_at":      exp,
	}, nil
}

// routerFieldErr turns a validation error of the router fields into a 422 on the field it is about.
func routerFieldErr(w http.ResponseWriter, err error, alias ...bool) {
	var fe *sites.FieldError
	if errors.As(err, &fe) {
		fields := map[string]string{fe.Field: fe.Msg}
		if len(alias) > 0 && alias[0] && fe.Field == "customer_ports" {
			fields["bench_port"] = fe.Msg // the deprecated alias the caller used
		}
		for _, f := range fe.Also {
			fields[f] = fe.Msg
		}
		validationErr(w, fields)
		return
	}
	field := "name"
	msg := err.Error()
	for _, f := range []struct{ key, word string }{
		{"pppoe_pass", "PPPoE needs a password"}, {"pppoe_pass", "PPPoE password"}, {"pppoe_user", "PPPoE"}, {"wan_address", "WAN address"}, {"wan_gateway", "WAN gateway"},
		{"wan_dns", "WAN DNS"}, {"wan_mode", "WAN type"}, {"wan_interface", "WAN interface"}, {"olt_interface", "OLT interface"}, {"olt_interface", "OLT uplink port"}, {"customer_ports", "customer port"}, {"customer_ports", "bench port"},
		{"lan_ports", "admin LAN port"}, {"lan_subnet", "admin LAN subnet"}, {"customer_subnet", "customer subnet"}, {"vlan_id", "VLAN"},
		{"lan_subnet", "LAN and customer subnets"}, {"lan_ports", "is used twice"},
	} {
		if strings.Contains(msg, f.word) {
			field = f.key
			break
		}
	}
	validationErr(w, map[string]string{field: msg})
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
	st, err := s.mgr.CreateRouter(applyRouterReq(sites.SiteInput{}, req))
	if err != nil {
		if s.storeErr(w, err) {
			return
		}
		routerFieldErr(w, err, req.BenchPort != nil && req.CustomerPorts == nil)
		return
	}
	join, err := s.joinBody(st)
	if err != nil {
		apiErr(w, http.StatusConflict, codeBadState, "The router was added but no line could be made: "+err.Error(), map[string]any{"router_id": st.ID})
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
	in := sites.SiteInputOf(st)
	base := in // only the fields that were sent change; the rest keep what is stored now, even if the Ports tab moved it meanwhile
	in = applyRouterReq(in, req)
	in.Base = &base
	if err := s.mgr.UpdateSite(st.ID, in); err != nil {
		var pe *sites.PortRefusal
		if errors.Is(err, sites.ErrOLTConfirmRequired) {
			apiErr(w, http.StatusConflict, codeConfirm, err.Error(), nil)
		} else if errors.Is(err, sites.ErrStaleLayout) {
			apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
		} else if errors.Is(err, sites.ErrLANConfirmRequired) {
			apiErr(w, http.StatusUnprocessableEntity, codeValidation, err.Error(),
				map[string]any{"fields": map[string]any{"confirm_name": err.Error()}, "warning": sites.WarnLAN})
		} else if errors.As(err, &pe) {
			s.portFail(w, err)
		} else if !s.storeErr(w, err) {
			routerFieldErr(w, err, req.BenchPort != nil && req.CustomerPorts == nil)
		}
		return
	}
	c.audit(s, "router "+st.Name+" edited")
	st, _ = s.site404(w, st.ID)
	h := s.siteHealths(r.Context(), []store.Site{st})[st.ID]
	writeJSON(w, http.StatusOK, s.siteJSON(st, h, s.siteCounts()[st.ID]))
}

// apiRouterReset is POST /routers/{id}/actions/reset: forget the router on the
// server and make a new line.
func (s *Server) apiRouterReset(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if strings.TrimSpace(req.ConfirmName) != st.Name {
		validationErr(w, map[string]string{"confirm_name": "type the router name to confirm"})
		return
	}
	if err := s.mgr.ResetRouter(st.ID, req.ConfirmName, c.user.Username); err != nil {
		if errors.Is(err, sites.ErrNameMismatch) {
			validationErr(w, map[string]string{"confirm_name": "type the router name to confirm"})
		} else if !s.storeErr(w, err) {
			apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
		}
		return
	}
	c.audit(s, "router "+st.Name+" reset")
	st, _ = s.site404(w, st.ID)
	join, err := s.joinBody(st)
	if err != nil {
		apiErr(w, http.StatusConflict, codeBadState, "The router was reset but no line could be made: "+err.Error(), nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h := s.siteHealths(r.Context(), []store.Site{st})[st.ID]
	writeJSON(w, http.StatusOK, map[string]any{"router": s.siteJSON(st, h, s.siteCounts()[st.ID]), "join": join})
}

// apiRouterScript is POST /routers/{id}/script: the same script as the paste line fetches, as a file download
// for a router that cannot take the line. It makes new keys, so it needs {confirm_name} and is refused (409)
// for a router that is joined with its tunnel up unless {force: true}. API only: the web admin has no button.
func (s *Server) apiRouterScript(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
		Force       bool   `json:"force"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if strings.TrimSpace(req.ConfirmName) != st.Name {
		validationErr(w, map[string]string{"confirm_name": "type the router name to confirm"})
		return
	}
	script, err := s.mgr.DownloadScript(st.ID, "download:"+c.user.Username, req.Force)
	if err != nil {
		if errors.Is(err, sites.ErrCannotRender) || errors.Is(err, sites.ErrInsecureBase) || errors.Is(err, sites.ErrRouterLive) {
			apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
		} else if !s.storeErr(w, err) {
			apiErr(w, http.StatusConflict, codeBadState, err.Error(), nil)
		}
		return
	}
	c.audit(s, "router "+st.Name+" script downloaded"+map[bool]string{true: " (forced)", false: ""}[req.Force])
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sites.SetupFile+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(script)
}

// ---- checks ------------------------------------------------------------------------------

func (s *Server) apiChecks(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
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
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
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

// apiPauseAutoFixes is POST /routers/{id}/actions/pause-auto-fixes {paused, until?}: owner only. While paused the
// server applies no check fix and no self-heal by itself (a save of the customer port still applies). until is an
// RFC 3339 time in the future; without it the pause lasts until it is lifted with {"paused": false}.
func (s *Server) apiPauseAutoFixes(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Paused bool    `json:"paused"`
		Until  *string `json:"until"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	var until time.Time
	if req.Until != nil && strings.TrimSpace(*req.Until) != "" {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*req.Until))
		if err != nil {
			validationErr(w, map[string]string{"until": "use a date and time like 2026-11-20T08:00:00+03:00"})
			return
		}
		until = t
	}
	if err := s.mgr.SetAutoFixPause(st.ID, req.Paused, until, c.user.Username); err != nil {
		switch {
		case errors.Is(err, sites.ErrPauseInPast):
			validationErr(w, map[string]string{"until": err.Error()})
		case errors.Is(err, store.ErrNoChange):
		default:
			if !s.storeErr(w, err) {
				apiErr(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
			}
			return
		}
	}
	c.audit(s, "router "+st.Name+" automatic fixes "+map[bool]string{true: "paused", false: "resumed"}[req.Paused])
	writeJSON(w, http.StatusOK, s.mgr.AutoFixPause(st.ID))
}
