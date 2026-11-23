package web

import (
	"time"

	"ispbill/internal/notify"
	"ispbill/internal/store"
)

// securityAlert tells the owners (WhatsApp and e-mail, the same targets as the
// monitoring alerts) that something about sign-in needs a look. It never
// carries a secret. Without the outbox (tests, or no messaging set up) it only
// leaves the audit trail the caller already wrote.
func (s *Server) securityAlert(msg string) { s.ownerAlert("Admin sign-in", msg) }

// PaymentAlert tells the owners about a payment that needs a person (it was not
// credited, and nothing else will say so).
func (s *Server) PaymentAlert(msg string) { s.ownerAlert("Payment needs attention", msg) }

// ONUAlert tells the owners about the ONU management service (a box checked in from an unexpected place). The
// message never carries a secret.
func (s *Server) ONUAlert(subject, msg string) { s.ownerAlert(subject, msg) }

func (s *Server) ownerAlert(subject, msg string) {
	if s.alertSink != nil { // tests
		s.alertSink(msg)
		return
	}
	box := s.ops.Box
	if box == nil {
		return
	}
	live := s.live()
	now := time.Now()
	stamp := now.In(time.FixedZone("EAT", 3*3600)).Format("2 Jan 15:04") + " EAT"
	base := notify.Message{Kind: notify.KindAdminAlert, Priority: 10}
	for _, to := range live.WAAdminNumbers {
		m := base
		m.Channel, m.To, m.Template, m.Lang = notify.ChanWhatsApp, to, notify.TplAdminAlert, live.WALang
		m.Params = []string{"PROBLEM", subject, "Fast Tech Net", msg, stamp}
		m.Text = "PROBLEM: " + subject + "\n" + msg + "\nTime: " + stamp
		box.Enqueue(m)
	}
	for _, to := range live.AlertEmailTo {
		m := base
		m.Channel, m.To, m.Subject = notify.ChanEmail, to, "[Fast Tech Net] "+subject+": "+msg
		m.Text = msg + "\nTime: " + stamp
		box.Enqueue(m)
	}
}

// passwordAccepted records that a stranger got the password right and now has
// to give the second factor, and tells the owners (at most once per user per
// window, so a flood of attempts cannot flood their phones).
func (s *Server) passwordAccepted(u store.AdminUser, ip string) {
	msg := "password accepted for " + u.Username + ", second factor pending from " + ip
	s.authn.AuditNow(u.Username, ip, msg)
	a := s.auth
	a.mu.Lock()
	last, seen := a.alerted[u.ID]
	due := !seen || time.Since(last) >= loginWindow
	if due {
		a.alerted[u.ID] = time.Now()
		if len(a.alerted) > 1000 {
			trimTimes(a.alerted)
		}
	}
	a.mu.Unlock()
	if due {
		s.securityAlert(msg)
	}
}
