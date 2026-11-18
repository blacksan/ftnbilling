package web

import (
	"encoding/json"
	"io"
	"net/http"
)

// Daraja rejects callback URLs containing "mpesa"/"safaricom", so the routes
// live under the neutral prefix /pay/cb/.
func (s *Server) callbackRoutes(mux *http.ServeMux) {
	if s.mpesa == nil || !s.mpesa.Enabled() {
		return
	}
	mux.HandleFunc("POST /pay/cb/stk", s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		if err := s.mpesa.HandleSTKCallback(r.Context(), b); err != nil {
			s.log.Warn("stk callback", "err", err)
		}
		writeJSON(w, map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
	}))
	mux.HandleFunc("POST /pay/cb/c2b/validate", s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		writeJSON(w, s.mpesa.HandleC2BValidation(b))
	}))
	mux.HandleFunc("POST /pay/cb/c2b/confirm", s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		writeJSON(w, s.mpesa.HandleC2BConfirmation(r.Context(), b))
	}))
}

// cb wraps a callback handler: optional shared-secret check, bounded body read.
func (s *Server) cb(h func(http.ResponseWriter, []byte, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := s.cfg.MpesaCallbackToken; tok != "" && !safeEq(r.URL.Query().Get("t"), tok) {
			http.NotFound(w, r)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		h(w, b, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
