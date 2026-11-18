package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"sync"
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

func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "dashboard")
	sc := s.scopeOf(r)
	now := s.svc.Now()
	nowE := now.In(billing.EAT)
	dayStart := time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, billing.EAT)
	monthStart := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, billing.EAT)
	o := overview{Greeting: greetingPeriod(now), TestMode: s.cfg.TestMode, RouterNames: map[string]string{}}

	revDay := make([]float64, 7)
	exp := make([]float64, 14)
	expDates := make([]time.Time, 14)
	for i := range expDates {
		expDates[i] = dayStart.AddDate(0, 0, i)
	}
	var recentPays []store.Payment
	var events []store.Event
	var pkgs map[string]store.Package
	var routers []store.Site
	type renew struct {
		a   store.Account
		pkg string
		at  time.Time
	}
	var renewals []renew
	var activeIDs []store.Account
	pol := s.svc.Policy()
	s.st.View(func(d *store.Data) {
		pkgs = d.Packages
		for _, st := range sc.Sites {
			if sc.Has(st.ID) {
				routers = append(routers, st)
				o.RouterNames[st.ID] = st.Name
			}
		}
		for _, a := range d.Accounts {
			if !sc.Has(a.SiteID) {
				continue
			}
			o.TotalAccts++
			switch billing.AccountState(a, now) {
			case billing.StateActive:
				o.Active++
				activeIDs = append(activeIDs, a)
				if a.ExpiresAt.Sub(now) <= time.Duration(pol.ReminderDays)*24*time.Hour {
					o.Soon++
				}
				if a.ExpiresAt.Sub(now) <= 7*24*time.Hour {
					o.Expiring7++
				}
				if a.ExpiresAt.Sub(now) <= 48*time.Hour {
					renewals = append(renewals, renew{a, d.Packages[a.PackageID].Name, a.ExpiresAt})
				}
				for i, day := range expDates {
					if !a.ExpiresAt.Before(day) && a.ExpiresAt.Before(day.AddDate(0, 0, 1)) {
						exp[i]++
					}
				}
			case billing.StateExpired:
				if !a.ExpiresAt.IsZero() && now.Sub(a.ExpiresAt) > 30*24*time.Hour {
					o.Churned++
				}
			}
		}
		for _, pay := range d.Payments {
			if pay.Status == store.PayUnmatched && (pay.SiteID == "" || sc.Has(pay.SiteID)) {
				o.Unmatched++
			}
			if !countsAsRevenue(pay) || !sc.Has(pay.SiteID) {
				continue
			}
			if !pay.PaidAt.Before(dayStart) {
				o.Today += pay.AmountKES
				recentPays = append(recentPays, pay)
			}
			if !pay.PaidAt.Before(dayStart.AddDate(0, 0, -1)) && pay.PaidAt.Before(dayStart) {
				o.Yesterday += pay.AmountKES
			}
			if !pay.PaidAt.Before(monthStart) {
				o.Month += pay.AmountKES
			}
			for i := 0; i < 7; i++ {
				day := dayStart.AddDate(0, 0, i-6)
				if !pay.PaidAt.Before(day) && pay.PaidAt.Before(day.AddDate(0, 0, 1)) {
					revDay[i] += float64(pay.AmountKES)
				}
			}
		}
		ev := d.Events
		for i := len(ev) - 1; i >= 0 && len(events) < 40; i-- {
			if sc.Has(ev[i].SiteID) && !ev[i].Time.Before(dayStart) && ev[i].Kind != "audit" {
				events = append(events, ev[i])
			}
		}
		for _, t := range d.TaskList() {
			if sc.Has(t.SiteID) {
				o.Tasks = append(o.Tasks, t)
			}
		}
		o.AnyRouter = len(routers) > 0
	})
	o.Spark = metrics.Spark(revDay, 120, 32, "Revenue, last 7 days")

	// Routers online: a quick ping each, in parallel, cached for 15 s by siteOnline.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	joined := false
	for _, st := range routers {
		if st.Mode == store.SiteModeEnv || st.WGPublicKey != "" {
			joined = true
		}
		if !st.Runnable() {
			continue
		}
		o.AnyRunnable = true
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ok := s.siteOnline(ctx, id)
			mu.Lock()
			if ok {
				o.RoutersOnline++
			}
			mu.Unlock()
		}(st.ID)
	}
	wg.Wait()
	o.RoutersTotal = len(routers)

	// Live flats and the busiest routers come from the collectors.
	live := s.liveRows(sc)
	o.Online = live.FlatsOnline
	for _, st := range routers {
		col := s.mgr.Collector(st.ID)
		if col == nil {
			continue
		}
		cfg, snap := col.Config(), col.Snapshot()
		pt := snap.Ports[cfg.WAN]
		rate := pt.RxBps
		if pt.TxBps > rate {
			rate = pt.TxBps
		}
		pct := 0.0
		if cfg.WANCapMbps > 0 {
			pct = clampPct(100 * rate / (cfg.WANCapMbps * 1e6))
		}
		n := 0
		for _, row := range live.Rows {
			if row.SiteID == st.ID && row.Kind == "flat" {
				n++
			}
		}
		o.Busiest = append(o.Busiest, ovBusy{ID: st.ID, Name: st.Name, Rate: fmtBps(rate), Pct: pct, Online: n})
	}
	sort.SliceStable(o.Busiest, func(i, j int) bool { return o.Busiest[i].Pct > o.Busiest[j].Pct })
	if len(o.Busiest) > 5 {
		o.Busiest = o.Busiest[:5]
	}

	// Subscriber health: active flats the router has not seen for a week. Only
	// routers that report a last-seen time (server-driven IPoE) can say.
	for _, a := range activeIDs {
		if seen := s.svc.AccountRouterState(a.ID).LastSeen; !seen.IsZero() && now.Sub(seen) > 7*24*time.Hour {
			o.AtRisk++
		}
	}

	// Setup checklist.
	live2 := s.live()
	custs := o.TotalAccts > 0
	pkgOK := false
	for _, pk := range pkgs {
		if !pk.Deleted && !pk.Disabled {
			pkgOK = true
		}
	}
	o.Steps = []ovStep{
		{Key: "mpesa", Title: "Connect M-Pesa", Href: "/admin/settings/mpesa", Done: live2.MpesaEnabled()},
		{Key: "plan", Title: "Create your first plan", Href: "/admin/packages", Done: pkgOK},
		{Key: "joined", Title: "Join a router", Href: "/admin/routers", Modal: "addrouter", Done: joined},
		{Key: "live", Title: "Get a router online", Href: "/admin/routers", Done: o.RoutersOnline > 0},
		{Key: "customer", Title: "Add your first customer", Href: "/admin/accounts", Modal: "addcustomer", Done: custs},
		{Key: "whatsapp", Title: "Set up WhatsApp messages", Href: "/admin/settings/whatsapp", Done: live2.WhatsAppConfigured()},
	}
	for _, st := range o.Steps {
		if st.Done {
			o.Done++
		}
	}
	o.Total = len(o.Steps)
	o.Pct = 100 * o.Done / o.Total

	// What is next, most urgent first.
	add := func(n int, text, href string) {
		if n > 0 {
			o.Next = append(o.Next, ovNext{Text: text, Href: href, Count: n})
		}
	}
	add(len(o.Tasks), "Router changes waiting to apply", "/admin/accounts")
	add(p.UnregCount, "New devices online with no customer", "/admin/devices")
	add(o.Unmatched, "Payments that need a flat", "/admin/unmatched")
	add(len(renewals), "Plans ending in the next 2 days", "/admin/accounts")
	add(p.FailedMsgs, "Messages that failed to send", "/admin/messages")
	add(p.OpenAlerts, "Open alerts", "/admin/alerts")
	if o.RoutersTotal > o.RoutersOnline && o.AnyRunnable {
		add(o.RoutersTotal-o.RoutersOnline, "Routers not answering", "/admin/routers")
	}

	// Renewals.
	sort.Slice(renewals, func(i, j int) bool { return renewals[i].at.Before(renewals[j].at) })
	for i, rn := range renewals {
		if i >= 6 {
			break
		}
		o.Renewals = append(o.Renewals, ovRenewal{Code: rn.a.Code, Flat: rn.a.Flat, Tenant: rn.a.Tenant, Router: o.RouterNames[rn.a.SiteID], Package: rn.pkg,
			Expires: nowEAT(rn.at), Left: billing.Remaining(rn.at, now)})
	}

	// Today feed: payments and lifecycle events, newest first.
	type fe struct {
		t time.Time
		f ovFeed
	}
	var feed []fe
	acctName := func(id string) string {
		if a, ok := s.svc.GetAccount(id); ok {
			return a.Flat
		}
		return ""
	}
	for _, pay := range recentPays {
		who := acctName(pay.AccountID)
		if who == "" {
			who = pay.BillRef
		}
		feed = append(feed, fe{pay.PaidAt, ovFeed{Kind: "payment", Text: fmt.Sprintf("KES %d from %s (%s)", pay.AmountKES, who, pay.Method), Href: "/admin/payments"}})
	}
	for _, ev := range events {
		href := ""
		if ev.AccountID != "" {
			href = s.accountURL(ev.AccountID)
		}
		feed = append(feed, fe{ev.Time, ovFeed{Kind: ev.Kind, Text: ev.Msg, Href: href}})
	}
	sort.SliceStable(feed, func(i, j int) bool { return feed[i].t.After(feed[j].t) })
	for i, f := range feed {
		if i >= 8 {
			break
		}
		f.f.Time = f.t.In(billing.EAT).Format("15:04")
		o.Feed = append(o.Feed, f.f)
	}

	// Expiries per day, next 14 days.
	var bars []metrics.VBar
	for i, day := range expDates {
		bars = append(bars, metrics.VBar{Label: strconv.Itoa(day.Day()), Value: exp[i], Soon: i == 0,
			Hint: fmt.Sprintf("%s: %.0f plan%s end", day.Format("Mon 2 Jan"), exp[i], plural(int(exp[i])))})
	}
	o.Expiries = metrics.BarsV(bars, 560, 170, "Plans ending per day over the next 14 days")

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
