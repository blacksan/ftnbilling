package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"ispbill/internal/sites"
)

// Session-authenticated JSON for the web admin's router setup tab (the templates are W7's).
// Minimum roles come from minRole: POSTs under /admin/routers are owner-only there.
func (s *Server) setupAdminRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/sites/setup.json", s.adminSetupJSON)
	post("/admin/sites/setup/run", s.adminSetupRun)
	post("/admin/sites/setup/migrate", s.adminSetupMigrate)
	post("/admin/sites/setup/rollback", s.adminSetupRollback)
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

func (s *Server) adminSetupErr(w http.ResponseWriter, err error) {
	code := http.StatusConflict
	if errors.Is(err, sites.ErrNameMismatch) {
		code = http.StatusUnprocessableEntity
	}
	s.adminSetupWrite(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) adminSetupRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	if err := s.mgr.StartProvision(st.ID, sites.ProvOptions{Trigger: "run", Actor: u.Username}); err != nil {
		s.adminSetupErr(w, err)
		return
	}
	s.adminSetupSend(w, r, http.StatusAccepted, st.ID)
}

func (s *Server) adminSetupMigrate(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	if err := s.mgr.StartMigration(st.ID, r.PostFormValue("confirm_name"), r.PostFormValue("keep_unknown") == "1", u.Username); err != nil {
		s.adminSetupErr(w, err)
		return
	}
	s.adminSetupSend(w, r, http.StatusAccepted, st.ID)
}

func (s *Server) adminSetupRollback(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	if err := s.mgr.RollbackNow(st.ID, u.Username); err != nil {
		s.adminSetupErr(w, err)
		return
	}
	s.adminSetupSend(w, r, http.StatusAccepted, st.ID)
}
