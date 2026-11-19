package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/sites"
)

// Session-authenticated JSON for the web admin's router setup tab (the
// templates come from the web builder). Minimum roles come from minRole: POSTs
// under /admin/routers are owner-only there.
func (s *Server) setupAdminRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/sites/setup.json", s.adminSetupJSON)
	post("/admin/sites/setup/reset", s.adminSetupReset)
	post("/admin/sites/setup/run", s.adminSetupGone)
	post("/admin/sites/setup/migrate", s.adminSetupGone)
	post("/admin/sites/setup/rollback", s.adminSetupGone)
}

func (s *Server) adminSetupWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) adminSetupSend(w http.ResponseWriter, r *http.Request, status int, id string) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	v, ok := s.mgr.SetupView(ctx, id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.adminSetupWrite(w, status, v)
}

func (s *Server) adminSetupJSON(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.adminSetupSend(w, r, http.StatusOK, st.ID)
}

// adminSetupGone: the set-up job, the migration and the rollback are gone in v5.
func (s *Server) adminSetupGone(w http.ResponseWriter, r *http.Request) {
	s.adminSetupWrite(w, http.StatusGone, map[string]string{"error": "gone: the router is set up by the line pasted on it; see docs/api/v5-bootstrap.md"})
}

// adminSetupReset is POST /admin/routers/setup/reset: form fields id and confirm_name.
// It answers the new Setup plus a fresh line.
func (s *Server) adminSetupReset(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	if err := s.mgr.ResetRouter(st.ID, strings.TrimSpace(r.PostFormValue("confirm_name")), u.Username); err != nil {
		code := http.StatusConflict
		if errors.Is(err, sites.ErrNameMismatch) {
			code = http.StatusUnprocessableEntity
		}
		s.adminSetupWrite(w, code, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r, "router "+st.Name+" reset")
	s.adminSetupSend(w, r, http.StatusOK, st.ID)
}
