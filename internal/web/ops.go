package web

import (
	"fmt"
	"net/http"
	"time"

	"ispbill/internal/alerts"
	"ispbill/internal/fleet"
	"ispbill/internal/notify"
)

// Ops are the optional Phase 2 services behind the fleet, uptime, alerts and
// messages pages. Any of them may be nil (the tests of the single-building
// pages run without them); the pages then say the feature is not running.
type Ops struct {
	Fleet  *fleet.Prober
	Alerts *alerts.Engine
	Box    *notify.Outbox
	Tenant *notify.Tenant
	// Inbound serves /wa/webhook; set only when WA_APP_SECRET and WA_VERIFY_TOKEN are.
	Inbound *notify.Inbound
}

// SetOps installs the Phase 2 services. Call it before Handler.
func (s *Server) SetOps(o Ops) { s.ops = o }

// HasOps is true when the fleet monitor is running; the sidebar then shows the
// fleet, uptime, alerts and messages pages.
func (s *Server) hasOps() bool { return s.ops.Fleet != nil }

func (s *Server) opsRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/fleet", s.adminFleet)
	get("/admin/uptime", s.adminUptime)
	get("/admin/alerts", s.adminAlerts)
	post("/admin/alerts/settings", s.adminAlertSettings)
	post("/admin/alerts/test", s.adminAlertTest)
	post("/admin/alerts/mute", s.adminAlertMute)
	post("/admin/alerts/unmute", s.adminAlertUnmute)
	get("/admin/messages", s.adminMessages)
	post("/admin/messages/retry", s.adminMessageRetry)
	post("/admin/messages/cancel", s.adminMessageCancel)
	post("/admin/messages/resume", s.adminMessageResume)
	post("/admin/sites/outage-notice", s.adminOutageNotice)
}

// ago formats a duration as "5 min", "2 h 10 min", "3 d 4 h".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d d %d h", int(d.Hours())/24, int(d.Hours())%24)
}

func causeText(c string) string {
	switch c {
	case fleet.CauseNoHandshake:
		return "tunnel down (no WireGuard handshake): power or internet at the building"
	case fleet.CauseRESTDown:
		return "tunnel up, router API not answering"
	}
	return "cause unknown"
}

// notReady renders the "feature is not running" page.
func (s *Server) notReady(w http.ResponseWriter, r *http.Request, active, what string) {
	p := s.page(r, active)
	p.Data["What"] = what
	s.render(w, "admin_notready", p)
}
