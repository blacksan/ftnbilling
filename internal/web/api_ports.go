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

// The Ports tab API (docs/api/v5-bootstrap.md, "Ports"):
//
//	GET   /routers/{id}/ports            every physical port with its role (viewer)
//	PATCH /routers/{id}/ports/{name}     {role?, disabled?, confirm_name?} (operator; owner for the admin LAN and the OLT uplink)

const (
	codePortLocked = "port_locked"
	codeLastLAN    = "last_lan_port"
	codeNoPort     = "no_such_port"
)

func (s *Server) portsRoutes(h func(pattern, role string, f apiFunc)) {
	h("GET /api/v1/routers/{id}/ports", "viewer", s.apiPorts)
	h("PATCH /api/v1/routers/{id}/ports/{name}", "operator", s.apiPortChange)
}

type portRoleLabel struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

var portRoleList = []portRoleLabel{
	{sites.PortRoleWAN, sites.PortRoleLabels[sites.PortRoleWAN]}, {sites.PortRoleLAN, sites.PortRoleLabels[sites.PortRoleLAN]},
	{sites.PortRoleOLT, sites.PortRoleLabels[sites.PortRoleOLT]}, {sites.PortRoleCustomer, sites.PortRoleLabels[sites.PortRoleCustomer]},
	{sites.PortRoleUnused, sites.PortRoleLabels[sites.PortRoleUnused]},
}

func portsBody(v sites.PortsView) map[string]any {
	items := v.Items
	if items == nil {
		items = []sites.PortInfo{}
	}
	return map[string]any{"items": items, "roles": portRoleList, "generated_at": v.GeneratedAt.UTC().Format(time.RFC3339),
		"reachable": v.Reachable, "customer_port": v.Status}
}

func (s *Server) apiPorts(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	v, err := s.mgr.Ports(r.Context(), st.ID, c.user.Role)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, portsBody(v))
}

type portReq struct {
	Role        *string `json:"role"`
	Disabled    *bool   `json:"disabled"`
	ConfirmName string  `json:"confirm_name"`
}

func (s *Server) apiPortChange(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req portReq
	if !decode(w, r, &req, false) {
		return
	}
	body, status, errBody := s.portChange(r.Context(), st, r.PathValue("name"), req, c.user)
	if errBody != nil {
		errBody(w)
		return
	}
	c.audit(s, "router "+st.Name+" port "+r.PathValue("name")+" changed")
	writeJSON(w, status, body)
}

// portChange runs the change and answers with the new row; errBody writes the error envelope.
func (s *Server) portChange(ctx context.Context, st store.Site, port string, req portReq, u store.AdminUser) (map[string]any, int, func(http.ResponseWriter)) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := s.mgr.ChangePort(ctx, st.ID, port, sites.PortChange{Role: req.Role, Disabled: req.Disabled, ConfirmName: req.ConfirmName},
		sites.PortActor{Name: u.Username, Role: u.Role})
	if err != nil {
		return nil, 0, func(w http.ResponseWriter) { s.portFail(w, err) }
	}
	out := map[string]any{"applied": res.Applied}
	if v, err := s.mgr.Ports(ctx, st.ID, u.Role); err == nil {
		for _, p := range v.Items {
			if p.Name == port {
				out["port"] = p
			}
		}
		out["customer_port"] = v.Status
	}
	if out["applied"] == nil {
		out["applied"] = []string{}
	}
	return out, http.StatusOK, nil
}

// portFail maps a refused port change to the error envelope.
func (s *Server) portFail(w http.ResponseWriter, err error) {
	var pe *sites.PortRefusal
	switch {
	case errors.As(err, &pe):
		switch pe.Kind {
		case sites.PortErrLocked:
			apiErr(w, http.StatusConflict, codePortLocked, pe.Msg, nil)
		case sites.PortErrLastLAN:
			apiErr(w, http.StatusConflict, codeLastLAN, pe.Msg, nil)
		case sites.PortErrConfirm:
			apiErr(w, http.StatusUnprocessableEntity, codeValidation, pe.Msg,
				map[string]any{"fields": map[string]any{"confirm_name": pe.Msg}, "warning": pe.Warning})
		case sites.PortErrForbidden:
			apiErr(w, http.StatusForbidden, codeForbidden, pe.Msg, nil)
		case sites.PortErrNoPort:
			apiErr(w, http.StatusNotFound, codeNoPort, pe.Msg, nil)
		case sites.PortErrInvalid:
			apiErr(w, http.StatusBadRequest, codeBadRequest, pe.Msg, nil)
		default:
			apiErr(w, http.StatusConflict, codeBadState, pe.Msg, nil)
		}
	case errors.Is(err, billing.ErrNoSite):
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
	case s.routerFail(w, err):
	default:
		apiErr(w, http.StatusInternalServerError, "internal", "The change could not be saved: "+strings.TrimSpace(err.Error()), nil)
	}
}

// ---- the web page (session JSON) ------------------------------------------------------------

// adminPorts is GET /admin/routers/ports.json?id=: the same body as the API.
func (s *Server) adminPorts(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	v, err := s.mgr.Ports(r.Context(), st.ID, u.Role)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, portsBody(v))
}

// adminPortChange is POST /admin/routers/ports/update (form: id, port, role, disabled, confirm_name).
func (s *Server) adminPortChange(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	req := portReq{ConfirmName: r.PostFormValue("confirm_name")}
	if v := r.PostFormValue("role"); v != "" {
		req.Role = &v
	}
	switch r.PostFormValue("disabled") {
	case "1", "true":
		t := true
		req.Disabled = &t
	case "0", "false":
		f := false
		req.Disabled = &f
	}
	port := r.PostFormValue("port")
	body, _, errBody := s.portChange(r.Context(), st, port, req, u)
	if errBody != nil {
		rec := &statusRecorder{header: http.Header{}}
		errBody(rec)
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.buf)
		return
	}
	s.audit(r, "router "+st.Name+" ("+st.ID+") port "+port+" changed")
	body["ok"] = true
	writeJSON(w, http.StatusOK, body)
}

// statusRecorder captures an API error envelope so the session endpoint can send it unchanged.
type statusRecorder struct {
	header http.Header
	status int
	buf    []byte
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) WriteHeader(code int)        { r.status = code }
func (r *statusRecorder) Write(b []byte) (int, error) { r.buf = append(r.buf, b...); return len(b), nil }
