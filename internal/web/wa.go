package web

import (
	"io"
	"net/http"
)

// waRoutes registers the WhatsApp webhook. It exists only when both
// WA_APP_SECRET and WA_VERIFY_TOKEN are set and the inbound handler runs.
func (s *Server) waRoutes(mux *http.ServeMux) {
	if s.ops.Inbound == nil || s.cfg.WAAppSecret == "" || s.cfg.WAVerifyToken == "" {
		return
	}
	mux.HandleFunc("GET /wa/webhook", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ch, ok := s.ops.Inbound.Verify(q.Get("hub.mode"), q.Get("hub.verify_token"), q.Get("hub.challenge"))
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, ch)
	})
	mux.HandleFunc("POST /wa/webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// The HMAC is over the raw bytes, so check it before parsing anything.
		if !s.ops.Inbound.ValidSignature(body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if err := s.ops.Inbound.Handle(body); err != nil {
			s.log.Warn("whatsapp webhook: bad payload", "err", err)
		}
		w.WriteHeader(http.StatusOK) // always 200 for a signed request: Meta retries anything else
	})
}
