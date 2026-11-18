package web

import (
	"io"
	"net/http"
	"time"

	"ispbill/internal/settings"
)

// waRoutes registers the WhatsApp webhook. The routes always exist; they
// answer 404 until both the app secret and the verify token are saved, so
// Settings > WhatsApp takes effect without a restart.
func (s *Server) waRoutes(mux *http.ServeMux) {
	live := func() bool { return s.ops.Inbound != nil && s.ops.Inbound.Active() }
	mux.HandleFunc("GET /wa/webhook", func(w http.ResponseWriter, r *http.Request) {
		if !live() {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		ch, ok := s.ops.Inbound.Verify(q.Get("hub.mode"), q.Get("hub.verify_token"), q.Get("hub.challenge"))
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.setm != nil {
			_ = s.setm.SetMarker(settings.KeyWebhookVerified, s.svc.Now().UTC().Format(time.RFC3339))
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, ch)
	})
	mux.HandleFunc("POST /wa/webhook", func(w http.ResponseWriter, r *http.Request) {
		if !live() {
			http.NotFound(w, r)
			return
		}
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
