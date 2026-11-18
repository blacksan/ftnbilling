package web

import (
	"errors"
	"io"
	"net/http"

	"ispbill/internal/sites"
)

// joinReport serves POST /j/{token}/report: the join script's own account of
// which sections worked, sent with the token it was fetched with. A token that
// is unknown, expired, not yet used, or used more than sites.ReportWindow ago
// gets the same 404 as GET /j/{token}.
func (s *Server) joinReport(w http.ResponseWriter, r *http.Request) {
	if !s.joinRL.allow(realIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, sites.MaxReportBytes+1))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch err := s.mgr.SubmitJoinReport(r.PathValue("token"), body); {
	case errors.Is(err, sites.ErrJoinInvalid):
		http.NotFound(w, r)
	case errors.Is(err, sites.ErrReportTooLarge):
		http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
	case err != nil:
		s.log.Error("join report", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}
