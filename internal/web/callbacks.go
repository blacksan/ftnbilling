package web

import (
	"encoding/json"
	"io"
	"net/http"
)

// Daraja rejects callback URLs containing "mpesa"/"safaricom", so the routes
// live under the neutral prefix /pay/cb/. They are always registered and
// answer 404 until M-Pesa is enabled and the request carries the callback
// token, either as ?t=<token> or in the path as /pay/cb/t/<token>/... (the
// form registered for C2B, in case RegisterURL refuses a query string). A
// Settings save therefore takes effect without a restart.
func (s *Server) callbackRoutes(mux *http.ServeMux) {
	stk := s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		if err := s.mpesa.HandleSTKCallback(r.Context(), b); err != nil {
			s.log.Warn("stk callback", "err", err)
		}
		jsonOut(w, map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
	})
	validate := s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		jsonOut(w, s.mpesa.HandleC2BValidation(b))
	})
	confirm := s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		jsonOut(w, s.mpesa.HandleC2BConfirmation(r.Context(), b))
	})
	mux.HandleFunc("POST /pay/cb/stk", stk)
	mux.HandleFunc("POST /pay/cb/c2b/validate", validate)
	mux.HandleFunc("POST /pay/cb/c2b/confirm", confirm)
	mux.HandleFunc("POST /pay/cb/t/{token}/stk", stk)
	mux.HandleFunc("POST /pay/cb/t/{token}/c2b/validate", validate)
	mux.HandleFunc("POST /pay/cb/t/{token}/c2b/confirm", confirm)
}

// cb wraps a callback handler: live enabled check, shared-secret check, bounded
// body read. Every refusal is the same 404.
func (s *Server) cb(h func(http.ResponseWriter, []byte, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.PathValue("token")
		if tok == "" {
			tok = r.URL.Query().Get("t")
		}
		if s.mpesa == nil || !s.mpesa.CheckCallbackToken(tok) {
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

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
