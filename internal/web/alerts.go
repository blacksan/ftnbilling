package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/alerts"
	"ispbill/internal/billing"
	"ispbill/internal/notify"
)

type alertRow struct {
	A        alerts.Alert
	Label    string
	Building string
	For      string
	Muted    bool
}

type histRow struct {
	E        alerts.Event
	Label    string
	Building string
	Start    string
	Dur      string
}

type ruleRow struct {
	Key, Label, Unit string
	R                alerts.Rule
	HasThreshold     bool
}

type chanInfo struct {
	Name, Label, To string
	Configured      bool
	On              bool
	Paused          string
}

func (s *Server) channels() []chanInfo {
	cfg := s.live()
	var pauses map[string]notify.Pause
	if s.ops.Box != nil {
		pauses = s.ops.Box.Paused()
	}
	set := alerts.DefaultSettings()
	if s.ops.Alerts != nil {
		set = s.ops.Alerts.Settings()
	}
	mail := strings.Join(cfg.AlertEmailTo, ", ")
	if mail == "" {
		mail = "no address set (Settings > Email)"
	}
	wa := strings.Join(cfg.WAAdminNumbers, ", ")
	if wa == "" {
		wa = "no number set (Settings > WhatsApp)"
	}
	out := []chanInfo{
		{Name: notify.ChanEmail, Label: "Email", To: mail, Configured: cfg.EmailConfigured() && len(cfg.AlertEmailTo) > 0, On: set.Email},
		{Name: notify.ChanWhatsApp, Label: "WhatsApp", To: wa, Configured: cfg.WhatsAppConfigured() && len(cfg.WAAdminNumbers) > 0, On: set.WhatsApp},
	}
	for i := range out {
		if p, ok := pauses[out[i].Name]; ok {
			out[i].Paused = p.Reason
		}
	}
	return out
}

func (s *Server) adminAlerts(w http.ResponseWriter, r *http.Request) {
	if s.ops.Alerts == nil {
		s.notReady(w, r, "alerts", "Alerts")
		return
	}
	p := s.page(r, "alerts")
	sc := s.scopeOf(r)
	names := map[string]string{"": alerts.ServerName}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
	}
	now := s.svc.Now()
	mutes := s.ops.Alerts.Mutes()
	var act []alertRow
	for _, a := range s.ops.Alerts.Active() {
		if !sc.Has(a.SiteID) {
			continue
		}
		_, muted := mutes[a.SiteID]
		act = append(act, alertRow{A: a, Label: alerts.Label(a.Rule), Building: names[a.SiteID], For: ago(now.Sub(a.Since)), Muted: muted})
	}
	var hist []histRow
	for _, e := range s.ops.Alerts.History(300) {
		if !sc.Has(e.SiteID) || len(hist) >= 40 {
			continue
		}
		hist = append(hist, histRow{E: e, Label: alerts.Label(e.Rule), Building: names[e.SiteID],
			Start: e.Start.In(billing.EAT).Format("Mon 2 Jan, 15:04"), Dur: ago(e.End.Sub(e.Start))})
	}
	set := s.ops.Alerts.Settings()
	var rules []ruleRow
	for _, k := range alerts.RuleOrder {
		rules = append(rules, ruleRow{Key: k, Label: alerts.Label(k), Unit: alerts.Unit(k), R: set.Rules[k], HasThreshold: alerts.Unit(k) != ""})
	}
	type muteRow struct{ ID, Name, Until string }
	var mrows []muteRow
	for id, until := range mutes {
		mrows = append(mrows, muteRow{id, names[id], until.In(billing.EAT).Format("Mon 2 Jan, 15:04")})
	}
	p.Data["Active"], p.Data["History"], p.Data["Rules"], p.Data["Set"] = act, hist, rules, set
	p.Data["Channels"], p.Data["Mutes"], p.Data["Sites"] = s.channels(), mrows, sc.Sites
	p.Data["Tenant"] = s.cfg.WhatsAppConfigured()
	s.render(w, "admin_alerts", p)
}

func (s *Server) adminAlertSettings(w http.ResponseWriter, r *http.Request) {
	if s.ops.Alerts == nil {
		http.NotFound(w, r)
		return
	}
	set := alerts.DefaultSettings()
	set.Rules = map[string]alerts.Rule{}
	num := func(k string) float64 {
		f, _ := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue(k)), 64)
		return f
	}
	for _, k := range alerts.RuleOrder {
		set.Rules[k] = alerts.Rule{Enabled: r.PostFormValue("rule_"+k+"_on") == "1", Threshold: num("rule_" + k + "_thr"), HoldMin: int(num("rule_" + k + "_hold"))}
	}
	set.Email, set.WhatsApp = r.PostFormValue("ch_email") == "1", r.PostFormValue("ch_whatsapp") == "1"
	set.CooldownMin = int(num("cooldown"))
	set.RepeatEnabled = r.PostFormValue("repeat_on") == "1"
	set.RepeatHours = int(num("repeat_hours"))
	set.OutageNotice = r.PostFormValue("notice")
	set.AutoMin = int(num("auto_min"))
	err := s.ops.Alerts.SetSettings(set)
	if err == nil {
		err = s.ops.Alerts.Save()
	}
	s.back(w, r, "/admin/alerts", msgOr(err, "Alert settings saved"))
}

func (s *Server) adminAlertTest(w http.ResponseWriter, r *http.Request) {
	if s.ops.Alerts == nil || s.ops.Box == nil {
		http.NotFound(w, r)
		return
	}
	ch := r.PostFormValue("channel")
	m, ok := s.ops.Alerts.TestMessage(ch)
	if !ok {
		s.back(w, r, "/admin/alerts", "Error: "+ch+" has no recipient: add one in Settings")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := s.ops.Box.Direct(ctx, m); err != nil {
		s.back(w, r, "/admin/alerts", "Error: the test "+ch+" message failed: "+err.Error())
		return
	}
	s.back(w, r, "/admin/alerts", "Test message sent on "+ch+" to "+m.To)
}

func (s *Server) adminAlertMute(w http.ResponseWriter, r *http.Request) {
	if s.ops.Alerts == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PostFormValue("site")
	hours, _ := strconv.Atoi(r.PostFormValue("hours"))
	if id == "" || hours < 1 || hours > 24*30 {
		s.back(w, r, "/admin/alerts", "Error: choose a router and a time")
		return
	}
	s.ops.Alerts.Mute(id, s.svc.Now().Add(time.Duration(hours)*time.Hour))
	_ = s.ops.Alerts.Save()
	s.back(w, r, "/admin/alerts", fmt.Sprintf("Messages for this router are muted for %d hours", hours))
}

func (s *Server) adminAlertUnmute(w http.ResponseWriter, r *http.Request) {
	if s.ops.Alerts == nil {
		http.NotFound(w, r)
		return
	}
	s.ops.Alerts.Unmute(r.PostFormValue("site"))
	_ = s.ops.Alerts.Save()
	s.back(w, r, "/admin/alerts", "Mute lifted")
}

// adminOutageNotice tells a building's tenants about the outage (WhatsApp).
func (s *Server) adminOutageNotice(w http.ResponseWriter, r *http.Request) {
	back := "/admin/routers"
	if s.ops.Tenant == nil || s.ops.Alerts == nil {
		s.back(w, r, back, "Error: tenant messages are not running")
		return
	}
	if !s.live().WhatsAppConfigured() {
		s.back(w, r, back, "Error: WhatsApp is not configured (Settings > WhatsApp)")
		return
	}
	if s.ops.Alerts.Settings().OutageNotice == "off" {
		s.back(w, r, back, "Error: tenant outage notices are switched off on the Alerts page")
		return
	}
	id := r.PostFormValue("site")
	var name string
	for _, st := range s.scopeOf(r).Sites {
		if st.ID == id {
			name = st.Name
		}
	}
	if name == "" {
		s.back(w, r, back, "Error: choose a router")
		return
	}
	n := s.ops.Tenant.SendOutageNotice(id, s.ops.Tenant.ManualOutageStart(id))
	if n == 0 {
		s.back(w, r, back, "No new notices: every tenant with a WhatsApp number in "+name+" was already told about this outage")
		return
	}
	s.back(w, r, back, fmt.Sprintf("Outage notice queued for %d tenants in %s", n, name))
}
