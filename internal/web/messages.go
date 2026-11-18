package web

import (
	"net/http"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/notify"
	"ispbill/internal/store"
)

type msgRow struct {
	M        notify.Message
	Created  string
	Building string
	Who      string
	Info     string
}

func (s *Server) adminMessages(w http.ResponseWriter, r *http.Request) {
	if s.ops.Box == nil {
		s.notReady(w, r, "messages", "The message outbox")
		return
	}
	p := s.page(r, "messages")
	sc := s.scopeOf(r)
	f := notify.Filter{Status: r.URL.Query().Get("status"), Channel: r.URL.Query().Get("channel"), Limit: 400}
	names := map[string]string{}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
	}
	flat := map[string]string{}
	s.st.View(func(d *store.Data) {
		for id, a := range d.Accounts {
			flat[id] = a.Flat
		}
	})
	var rows []msgRow
	for _, m := range s.ops.Box.List(f) {
		if m.SiteID != "" && !sc.Has(m.SiteID) {
			continue
		}
		who := m.To
		if m.AccountID != "" {
			who = flat[m.AccountID] + " " + m.To
		}
		info := m.LastErr
		if m.Status == notify.StatusQueued && !m.NextTry.IsZero() && m.NextTry.After(s.svc.Now()) {
			info = "retry " + m.NextTry.In(billing.EAT).Format("15:04") + ". " + info
		}
		if m.Status == notify.StatusQueued && m.NotBefore.After(s.svc.Now()) {
			info = "held until " + m.NotBefore.In(billing.EAT).Format("Mon 15:04")
		}
		rows = append(rows, msgRow{M: m, Created: m.CreatedAt.In(billing.EAT).Format("Mon 2 Jan, 15:04"), Building: names[m.SiteID], Who: who, Info: info})
	}
	p.Data["Rows"], p.Data["Counts"] = rows, s.ops.Box.Counts()
	p.Data["Status"], p.Data["Channel"] = f.Status, f.Channel
	p.Data["Pauses"] = s.ops.Box.Paused()
	p.Data["WAToday"], p.Data["WALimit"] = s.ops.Box.WASentToday(), s.live().WADailyLimit
	p.Data["Statuses"] = []string{"", notify.StatusQueued, notify.StatusSent, notify.StatusFailed, notify.StatusSkipped, notify.StatusCancelled}
	p.Data["Channels"] = []string{"", notify.ChanWhatsApp, notify.ChanEmail}
	p.Data["Now"] = time.Now()
	s.render(w, "admin_messages", p)
}

func (s *Server) adminMessageRetry(w http.ResponseWriter, r *http.Request) {
	if s.ops.Box == nil {
		http.NotFound(w, r)
		return
	}
	ok := s.ops.Box.Retry(r.PostFormValue("id"))
	msg := "Message queued again"
	if !ok {
		msg = "Error: only failed, skipped or cancelled messages can be retried"
	}
	s.back(w, r, "/admin/messages", msg)
}

func (s *Server) adminMessageCancel(w http.ResponseWriter, r *http.Request) {
	if s.ops.Box == nil {
		http.NotFound(w, r)
		return
	}
	ok := s.ops.Box.Cancel(r.PostFormValue("id"))
	msg := "Message cancelled"
	if !ok {
		msg = "Error: only a queued message can be cancelled"
	}
	s.back(w, r, "/admin/messages", msg)
}

func (s *Server) adminMessageResume(w http.ResponseWriter, r *http.Request) {
	if s.ops.Box == nil {
		http.NotFound(w, r)
		return
	}
	s.ops.Box.Resume(r.PostFormValue("channel"))
	s.back(w, r, "/admin/messages", "Channel resumed. If the credentials are still wrong it will pause again.")
}
