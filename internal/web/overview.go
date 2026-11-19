package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/metrics"
	"ispbill/internal/store"
)

// The overview page (docs/release-plan-routers.md section 4). The numbers
// are the same ones GET /overview will serve; they are computed here from the
// store and the in-memory collectors, never from a router call, so the page
// opens at once.

type ovStep struct {
	Key, Title, Href, Modal string
	Done                    bool
}

type ovFeed struct{ Time, Kind, Text, Href string }

type ovNext struct {
	Text, Href string
	Count      int
}

type ovRenewal struct{ Code, Flat, Tenant, Router, Expires, Left, Package string }

type ovBusy struct {
	ID, Name, Rate string
	Pct            float64
	Online         int
}

type overview struct {
	Greeting                        string
	Done, Total, Pct                int
	Steps                           []ovStep
	Today, Yesterday, Month         int
	Spark                           template.HTML
	Active, TotalAccts, Online      int
	RoutersTotal, RoutersOnline     int
	Unmatched                       int
	Feed                            []ovFeed
	Next                            []ovNext
	Renewals                        []ovRenewal
	Busiest                         []ovBusy
	AtRisk, Churned, Expiring7      int
	Expiries                        template.HTML
	Soon                            int
	TestMode                        bool
	Tasks                           []store.Task
	RouterNames                     map[string]string
	AnyRouter, AnyRunnable, Pending bool
}

// adminDashboard draws billing.Overview (the same answer as GET /api/v1/overview)
// plus the pending router actions table. It computes no numbers of its own.
func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "dashboard")
	sc := s.scopeOf(r)
	now := s.svc.Now()
	site := ""
	if !sc.All {
		site = sc.ID
	}
	ov := s.buildOverview(r.Context(), site)
	o := overview{Greeting: ov.GreetingPeriod, TestMode: s.cfg.TestMode, RouterNames: map[string]string{},
		Today: ov.Revenue.TodayKES, Yesterday: ov.Revenue.YesterdayKES, Month: ov.Revenue.MonthKES,
		Active: ov.Active, Online: ov.OnlineNow, RoutersTotal: ov.Routers.Total, RoutersOnline: ov.Routers.Online,
		AtRisk: ov.Health.AtRisk, Churned: ov.Health.Churned, Expiring7: ov.Health.Expiring7d,
		Done: ov.Setup.Done, Total: ov.Setup.Total, Pct: ov.Setup.Pct}
	spark := make([]float64, len(ov.Revenue.Spark7d))
	paid := false
	for i, v := range ov.Revenue.Spark7d {
		spark[i] = float64(v)
		paid = paid || v != 0
	}
	// A week with no money draws nothing: a flat line on the card reads as a stray rule.
	if paid {
		o.Spark = metrics.Spark(spark, 120, 32, "Revenue, last 7 days")
	}
	modal := map[string]string{"router_joined": "addrouter", "first_customer": "addcustomer"}
	for _, st := range ov.Setup.Steps {
		o.Steps = append(o.Steps, ovStep{Key: st.Key, Title: st.Title, Href: st.Href, Modal: modal[st.Key], Done: st.Done})
	}
	for _, n := range ov.Next {
		o.Next = append(o.Next, ovNext{Text: n.Text, Href: n.Href, Count: n.Count})
		if n.Kind == "unmatched_payments" {
			o.Unmatched = n.Count
		}
	}
	for _, f := range ov.Today {
		item := ovFeed{Kind: f.Kind, Text: f.Text}
		if t, err := time.Parse(time.RFC3339, f.Time); err == nil {
			item.Time = t.In(billing.EAT).Format("15:04")
		}
		switch {
		case f.Kind == "unmatched":
			item.Href = "/admin/unmatched"
		case f.Kind == "payment":
			item.Href = "/admin/payments"
		case f.AccountID != "":
			item.Href = s.accountURL(f.AccountID)
		}
		o.Feed = append(o.Feed, item)
		if len(o.Feed) >= 8 {
			break
		}
	}
	for i, rn := range ov.Renewals48h {
		if i >= 6 {
			break
		}
		rr := ovRenewal{Flat: rn.Flat, Tenant: rn.Tenant, Router: rn.RouterName, Package: rn.Package}
		if a, ok := s.svc.GetAccount(rn.AccountID); ok {
			rr.Code = a.Code
		}
		if t, err := time.Parse(time.RFC3339, rn.ExpiresAt); err == nil {
			rr.Expires, rr.Left = nowEAT(t), billing.Remaining(t, now)
		}
		o.Renewals = append(o.Renewals, rr)
	}
	for _, b := range ov.BusiestRouters {
		o.Busiest = append(o.Busiest, ovBusy{ID: b.RouterID, Name: b.Name, Rate: fmtBps(float64(b.DownBps)), Pct: clampPct(b.UtilPct), Online: b.Online})
	}
	var bars []metrics.VBar
	for i, e := range ov.ExpiriesPerDay {
		label := e.Date
		hint := e.Date
		if d, err := time.ParseInLocation("2006-01-02", e.Date, billing.EAT); err == nil {
			label, hint = strconv.Itoa(d.Day()), d.Format("Mon 2 Jan")
		}
		bars = append(bars, metrics.VBar{Label: label, Value: float64(e.Count), Soon: i == 0,
			Hint: fmt.Sprintf("%s: %d plan%s end", hint, e.Count, plural(e.Count))})
	}
	o.Expiries = metrics.BarsV(bars, 560, 170, "Plans ending per day over the next 14 days")

	// the only thing the overview reads beyond billing.Overview: the router
	// actions waiting to apply, listed in full.
	s.st.View(func(d *store.Data) {
		for _, st := range sc.Sites {
			o.RouterNames[st.ID] = st.Name
		}
		for _, t := range d.TaskList() {
			if sc.Has(t.SiteID) {
				o.Tasks = append(o.Tasks, t)
			}
		}
		for _, a := range d.Accounts {
			if sc.Has(a.SiteID) {
				o.TotalAccts++
			}
		}
	})
	p.Data["O"] = &o
	s.render(w, "admin_dashboard", p)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func nowEAT(t time.Time) string { return t.In(billing.EAT).Format("Mon 2 Jan, 15:04") }

// fmtBps writes a bit rate the way a person reads it.
func fmtBps(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1f Gbps", v/1e9)
	case v >= 1e7:
		return fmt.Sprintf("%.0f Mbps", v/1e6)
	case v >= 1e6:
		return fmt.Sprintf("%.1f Mbps", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0f kbps", v/1e3)
	}
	return "0"
}

var _ = http.StatusOK
