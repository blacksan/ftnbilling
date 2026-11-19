package billing

import (
	"context"
	"fmt"
	"sort"
	"time"

	"ispbill/internal/store"
)

// The overview of GET /api/v1/overview (docs/release-plan-routers.md section 3).
// Billing owns the numbers that come from the store; what only a router or the
// fleet monitor knows (is it online, how loaded, who is online now) is given in
// OverviewInput by the web layer.

// OverviewSetupStep is one line of the setup checklist.
type OverviewSetupStep struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
	Href  string `json:"href"`
}

// OverviewSetup is the checklist and its progress.
type OverviewSetup struct {
	Done  int                 `json:"done"`
	Total int                 `json:"total"`
	Pct   int                 `json:"pct"`
	Steps []OverviewSetupStep `json:"steps"`
}

// OverviewRevenue is KES for the EAT day, the day before and the month, plus
// the last seven days (oldest first, today last).
type OverviewRevenue struct {
	TodayKES     int   `json:"today_kes"`
	YesterdayKES int   `json:"yesterday_kes"`
	MonthKES     int   `json:"month_kes"`
	Spark7d      []int `json:"spark_7d"`
}

// OverviewRouters counts the routers in scope.
type OverviewRouters struct {
	Total  int `json:"total"`
	Online int `json:"online"`
}

// OverviewFeedItem is one line of "today".
type OverviewFeedItem struct {
	Time      string `json:"time"`
	Kind      string `json:"kind"` // payment | unmatched | expired | reminder | info
	Text      string `json:"text"`
	AccountID string `json:"account_id"`
	RouterID  string `json:"router_id"`
}

// OverviewNext is one "what's next" item.
type OverviewNext struct {
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	Count int    `json:"count"`
	Href  string `json:"href"`
}

// OverviewRenewal is a plan that ends within 48 hours.
type OverviewRenewal struct {
	AccountID  string `json:"account_id"`
	Flat       string `json:"flat"`
	Tenant     string `json:"tenant"`
	RouterName string `json:"router_name"`
	ExpiresAt  string `json:"expires_at"`
	Package    string `json:"package"`
}

// OverviewBusy is one router in "busiest routers".
type OverviewBusy struct {
	RouterID string  `json:"router_id"`
	Name     string  `json:"name"`
	DownBps  int64   `json:"down_bps"`
	UtilPct  float64 `json:"util_pct"`
	Online   int     `json:"online"`
}

// OverviewHealth is subscriber health.
type OverviewHealth struct {
	AtRisk     int `json:"at_risk"`
	Churned    int `json:"churned"`
	Expiring7d int `json:"expiring_7d"`
}

// OverviewExpiry is the number of plans that end on one EAT day.
type OverviewExpiry struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// Overview is the answer of GET /overview.
type Overview struct {
	GreetingPeriod  string             `json:"greeting_period"` // morning | afternoon | evening | late
	Setup           OverviewSetup      `json:"setup"`
	Revenue         OverviewRevenue    `json:"revenue"`
	Active          int                `json:"active"`
	OnlineNow       int                `json:"online_now"`
	Routers         OverviewRouters    `json:"routers"`
	Today           []OverviewFeedItem `json:"today"`
	Next            []OverviewNext     `json:"next"`
	Renewals48h     []OverviewRenewal  `json:"renewals_48h"`
	BusiestRouters  []OverviewBusy     `json:"busiest_routers"`
	Health          OverviewHealth     `json:"health"`
	ExpiriesPerDay  []OverviewExpiry   `json:"expiries_per_day"`
	UnmatchedAmount int                `json:"-"`
}

// RouterLoad is a router's WAN rate now.
type RouterLoad struct {
	DownBps int64
	UtilPct float64
}

// OverviewInput is what billing cannot know itself.
type OverviewInput struct {
	SiteID string // "" = every router
	// RouterOnline and RouterLive are by router id: reachable now; setup finished.
	RouterOnline map[string]bool
	RouterLive   map[string]bool
	RouterLoad   map[string]RouterLoad
	// Online says whether the account's flat is online now (nil = nobody).
	Online func(store.Account) bool
	// LastSeen is when the flat was last seen (zero = unknown; those are not at risk).
	LastSeen func(store.Account) time.Time

	MpesaConfigured, WhatsAppConfigured bool
}

// GreetingPeriod names the part of the day in Africa/Nairobi.
func GreetingPeriod(t time.Time) string {
	switch h := t.In(EAT).Hour(); {
	case h >= 5 && h < 12:
		return "morning"
	case h >= 12 && h < 17:
		return "afternoon"
	case h >= 17 && h < 22:
		return "evening"
	}
	return "late"
}

func kes(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "KES " + s
}

// Overview builds the overview for one router or all.
func (s *Service) Overview(in OverviewInput) Overview {
	now := s.now()
	ne := now.In(EAT)
	dayStart := time.Date(ne.Year(), ne.Month(), ne.Day(), 0, 0, 0, 0, EAT)
	monthStart := time.Date(ne.Year(), ne.Month(), 1, 0, 0, 0, 0, EAT)
	has := func(id string) bool { return in.SiteID == "" || id == in.SiteID }

	var sitesIn []store.Site
	var accts []store.Account
	var pkgs map[string]store.Package
	var feed []OverviewFeedItem
	var pays []store.Payment
	unmatched, macConflicts, newAccts, pendingTasks, packages, anyAccount := 0, 0, 0, 0, 0, false
	joined := false
	s.st.View(func(d *store.Data) {
		pkgs = make(map[string]store.Package, len(d.Packages))
		for k, p := range d.Packages {
			pkgs[k] = p
			if !p.Deleted && !p.Disabled {
				packages++
			}
		}
		for _, st := range d.SiteList() {
			if has(st.ID) {
				sitesIn = append(sitesIn, st)
			}
			if !st.JoinedAt.IsZero() {
				joined = true
			}
		}
		for _, a := range d.AccountList() {
			anyAccount = true
			if has(a.SiteID) {
				accts = append(accts, a)
			}
		}
		for _, p := range d.Payments {
			if p.Status == store.PayUnmatched && (p.SiteID == "" || has(p.SiteID)) {
				unmatched++
			}
			if has(p.SiteID) && !p.PaidAt.Before(monthStart.AddDate(0, -1, 0)) {
				pays = append(pays, p)
			}
		}
		for _, t := range d.Tasks {
			if has(t.SiteID) {
				pendingTasks++
			}
		}
		for _, e := range d.Events {
			if e.Time.Before(dayStart) || e.Kind == "audit" || (e.SiteID != "" && !has(e.SiteID)) {
				continue
			}
			kind := e.Kind
			switch kind {
			case "reminder_due":
				kind = "reminder"
			case "expired", "info":
			default:
				continue
			}
			feed = append(feed, OverviewFeedItem{Time: e.Time.UTC().Format(time.RFC3339), Kind: kind, Text: e.Msg, AccountID: e.AccountID, RouterID: e.SiteID})
		}
	})

	o := Overview{GreetingPeriod: GreetingPeriod(now)}
	// revenue, feed of payments
	o.Revenue.Spark7d = make([]int, 7)
	acctFlat := map[string]store.Account{}
	for _, a := range accts {
		acctFlat[a.ID] = a
	}
	for _, p := range pays {
		if p.Status == store.PayUnmatched && !p.PaidAt.Before(dayStart) {
			feed = append(feed, OverviewFeedItem{Time: p.PaidAt.UTC().Format(time.RFC3339), Kind: "unmatched", Text: kes(p.AmountKES) + " received, not matched to a flat yet", RouterID: p.SiteID})
		}
		if !CountsAsRevenue(p) {
			continue
		}
		if !p.PaidAt.Before(monthStart) {
			o.Revenue.MonthKES += p.AmountKES
		}
		switch days := int(dayStart.Sub(time.Date(p.PaidAt.In(EAT).Year(), p.PaidAt.In(EAT).Month(), p.PaidAt.In(EAT).Day(), 0, 0, 0, 0, EAT)).Hours() / 24); {
		case days == 0 && !p.PaidAt.After(now.Add(time.Hour)):
			o.Revenue.TodayKES += p.AmountKES
			o.Revenue.Spark7d[6] += p.AmountKES
		case days >= 1 && days <= 6:
			o.Revenue.Spark7d[6-days] += p.AmountKES
			if days == 1 {
				o.Revenue.YesterdayKES += p.AmountKES
			}
		}
		if !p.PaidAt.Before(dayStart) {
			who := "a flat"
			if a, ok := acctFlat[p.AccountID]; ok {
				who = a.Flat
			}
			feed = append(feed, OverviewFeedItem{Time: p.PaidAt.UTC().Format(time.RFC3339), Kind: "payment", Text: kes(p.AmountKES) + " from " + who, AccountID: p.AccountID, RouterID: p.SiteID})
		}
	}
	sort.SliceStable(feed, func(i, j int) bool { return feed[i].Time > feed[j].Time })
	if len(feed) > 20 {
		feed = feed[:20]
	}
	if feed == nil {
		feed = []OverviewFeedItem{}
	}
	o.Today = feed

	// accounts
	siteName := map[string]string{}
	routerOnlineCount := map[string]int{}
	for _, st := range sitesIn {
		siteName[st.ID] = st.Name
		o.Routers.Total++
		if in.RouterOnline[st.ID] {
			o.Routers.Online++
		}
	}
	perDay := make([]int, 14)
	o.Renewals48h = []OverviewRenewal{}
	for _, a := range accts {
		switch AccountState(a, now) {
		case StateActive:
			o.Active++
			left := a.ExpiresAt.Sub(now)
			if left <= 7*24*time.Hour {
				o.Health.Expiring7d++
			}
			if left <= 48*time.Hour {
				o.Renewals48h = append(o.Renewals48h, OverviewRenewal{AccountID: a.ID, Flat: a.Flat, Tenant: a.Tenant, RouterName: siteName[a.SiteID],
					ExpiresAt: a.ExpiresAt.UTC().Format(time.RFC3339), Package: pkgs[a.PackageID].Name})
			}
			if i := int(time.Date(a.ExpiresAt.In(EAT).Year(), a.ExpiresAt.In(EAT).Month(), a.ExpiresAt.In(EAT).Day(), 0, 0, 0, 0, EAT).Sub(dayStart).Hours() / 24); i >= 0 && i < 14 {
				perDay[i]++
			}
			if in.LastSeen != nil {
				if ls := in.LastSeen(a); !ls.IsZero() && now.Sub(ls) > 7*24*time.Hour {
					o.Health.AtRisk++
				}
			}
		case StateExpired:
			if now.Sub(a.ExpiresAt) > 30*24*time.Hour {
				o.Health.Churned++
			}
		case StateNew:
			newAccts++
		}
		if a.MACConflict {
			macConflicts++
		}
		if in.Online != nil && in.Online(a) {
			o.OnlineNow++
			routerOnlineCount[a.SiteID]++
		}
	}
	sort.SliceStable(o.Renewals48h, func(i, j int) bool { return o.Renewals48h[i].ExpiresAt < o.Renewals48h[j].ExpiresAt })
	if len(o.Renewals48h) > 50 {
		o.Renewals48h = o.Renewals48h[:50]
	}
	o.ExpiriesPerDay = make([]OverviewExpiry, 14)
	for i := range perDay {
		o.ExpiriesPerDay[i] = OverviewExpiry{Date: dayStart.AddDate(0, 0, i).Format("2006-01-02"), Count: perDay[i]}
	}

	// busiest routers
	o.BusiestRouters = []OverviewBusy{}
	for _, st := range sitesIn {
		l := in.RouterLoad[st.ID]
		o.BusiestRouters = append(o.BusiestRouters, OverviewBusy{RouterID: st.ID, Name: st.Name, DownBps: l.DownBps, UtilPct: l.UtilPct, Online: routerOnlineCount[st.ID]})
	}
	sort.SliceStable(o.BusiestRouters, func(i, j int) bool {
		a, b := o.BusiestRouters[i], o.BusiestRouters[j]
		if a.DownBps != b.DownBps {
			return a.DownBps > b.DownBps
		}
		return a.Online > b.Online
	})
	if len(o.BusiestRouters) > 5 {
		o.BusiestRouters = o.BusiestRouters[:5]
	}

	// setup checklist
	live := false
	for _, st := range sitesIn {
		if in.RouterLive[st.ID] {
			live = true
		}
	}
	steps := []OverviewSetupStep{
		{"mpesa", "Connect M-Pesa", in.MpesaConfigured, "/admin/settings/mpesa"},
		{"first_plan", "Create your first plan", packages > 0, "/admin/plans"},
		{"router_joined", "Link a router", joined, "/admin/routers"},
		{"router_live", "Take a router live", live, "/admin/routers"},
		{"first_customer", "Add your first customer", anyAccount, "/admin/subscribers"},
		{"whatsapp", "Connect WhatsApp", in.WhatsAppConfigured, "/admin/settings/whatsapp"},
	}
	o.Setup = OverviewSetup{Total: len(steps), Steps: steps}
	for _, st := range steps {
		if st.Done {
			o.Setup.Done++
		}
	}
	o.Setup.Pct = o.Setup.Done * 100 / o.Setup.Total

	// what's next
	o.Next = []OverviewNext{}
	add := func(kind, text string, n int, href string) {
		if n > 0 {
			o.Next = append(o.Next, OverviewNext{Kind: kind, Text: text, Count: n, Href: href})
		}
	}
	offline := o.Routers.Total - o.Routers.Online
	add("routers_offline", countText(offline, "router is", "routers are")+" offline", offline, "/admin/routers?tab=offline")
	add("unmatched_payments", countText(unmatched, "payment is", "payments are")+" waiting to be matched", unmatched, "/admin/payments?tab=unmatched")
	add("new_devices", countText(s.UnregisteredOnline(has), "new device is", "new devices are")+" online without an account", s.UnregisteredOnline(has), "/admin/devices")
	add("renewals", countText(len(o.Renewals48h), "plan ends", "plans end")+" in the next 48 hours", len(o.Renewals48h), "/admin/subscribers?tab=active")
	add("mac_conflicts", countText(macConflicts, "flat needs", "flats need")+" a MAC address fixed", macConflicts, "/admin/subscribers?tab=mac_conflict")
	add("no_plan", countText(newAccts, "flat has", "flats have")+" no plan yet", newAccts, "/admin/subscribers?tab=new")
	add("router_tasks", countText(pendingTasks, "change is", "changes are")+" waiting to reach a router", pendingTasks, "/admin/routers")
	return o
}

func countText(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// SiteCountsLive are the per-router flat counts used by the live view.
type SiteCountsLive struct {
	Total, Active, Expired, Suspended, Expiring7d int
	Equipment                                     struct{ OLT, ONT, ONTBound int }
	TodayKES, MonthKES                            int
}

// LiveFacts reads the store-side facts of one router for the live view.
func (s *Service) LiveFacts(siteID string) SiteCountsLive {
	now := s.now()
	ne := now.In(EAT)
	dayStart := time.Date(ne.Year(), ne.Month(), ne.Day(), 0, 0, 0, 0, EAT)
	monthStart := time.Date(ne.Year(), ne.Month(), 1, 0, 0, 0, 0, EAT)
	var out SiteCountsLive
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.SiteID != siteID {
				continue
			}
			out.Total++
			switch AccountState(a, now) {
			case StateActive:
				out.Active++
				if a.ExpiresAt.Sub(now) <= 7*24*time.Hour {
					out.Expiring7d++
				}
			case StateExpired:
				out.Expired++
			case StateSuspended:
				out.Suspended++
			}
		}
		for _, p := range d.Payments {
			if p.SiteID != siteID || !CountsAsRevenue(p) || p.PaidAt.Before(monthStart) {
				continue
			}
			out.MonthKES += p.AmountKES
			if !p.PaidAt.Before(dayStart) {
				out.TodayKES += p.AmountKES
			}
		}
		for _, e := range d.Equipment {
			if e.SiteID != siteID || e.Status == store.EqRetired {
				continue
			}
			switch e.Kind {
			case store.KindOLT:
				out.Equipment.OLT++
			case store.KindONT:
				out.Equipment.ONT++
				if e.AccountID != "" && e.Status == store.EqInstalled {
					out.Equipment.ONTBound++
				}
			}
		}
	})
	return out
}

// ResyncSite starts a full router sync for one router in the background: the
// reconcile, the queued per-account tasks
// and a refresh of the device list. It does not wait for the router.
func (s *Service) ResyncSite(siteID string) error {
	if _, ok := s.site(siteID); !ok {
		return ErrNoSite
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := s.ReconcileSite(ctx, siteID); err != nil {
			s.log.Warn("resync: reconcile", "site", siteID, "err", err)
		}
		s.RunSiteTasks(ctx, siteID)
		if err := s.RefreshHosts(ctx, siteID); err != nil {
			s.log.Warn("resync: refresh hosts", "site", siteID, "err", err)
		}
	}()
	return nil
}
