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
	stk := s.cbQuery(true, func(w http.ResponseWriter, b []byte, r *http.Request) {
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
	txres := s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		jsonOut(w, s.mpesa.HandleTxStatusResult(r.Context(), b))
	})
	txtimeout := s.cb(func(w http.ResponseWriter, b []byte, r *http.Request) {
		jsonOut(w, s.mpesa.HandleTxStatusTimeout(b))
	})
	mux.HandleFunc("POST /pay/cb/t/{token}/txstatus/result", txres)
	mux.HandleFunc("POST /pay/cb/t/{token}/txstatus/timeout", txtimeout)
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
	return s.cbQuery(false, h)
}

// cbQuery is cb. With stk set, the old form of the STK callback URL, the token
// in the query string (?t=), is only accepted until the end of the grace period
// (Mpesa.LegacyQueryUntil, 30 days after this version first ran) so STK pushes
// already sent with the old URL still land. Every use is logged.
func (s *Server) cbQuery(stk bool, h func(http.ResponseWriter, []byte, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := r.PathValue("token")
		if tok == "" {
			tok = r.URL.Query().Get("t")
			if tok != "" {
				if stk && !s.mpesa.LegacyQueryOK() {
					http.NotFound(w, r)
					return
				}
				s.log.Warn("M-Pesa callback with the secret in the query string (old form): re-register the URL with the token in the path", "path", r.URL.Path)
			}
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
