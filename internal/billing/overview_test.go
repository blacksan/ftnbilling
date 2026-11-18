package billing

import (
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestGreetingPeriodIsEastAfricaTime(t *testing.T) {
	utc := func(h, m int) time.Time { return time.Date(2026, 11, 17, h, m, 0, 0, time.UTC) }
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{utc(1, 59), "late"},     // 04:59 EAT
		{utc(2, 0), "morning"},   // 05:00
		{utc(8, 59), "morning"},  // 11:59
		{utc(9, 0), "afternoon"}, // 12:00
		{utc(13, 59), "afternoon"},
		{utc(14, 0), "evening"}, // 17:00
		{utc(18, 59), "evening"},
		{utc(19, 0), "late"}, // 22:00
		{utc(21, 0), "late"}, // 00:00 EAT
	} {
		if got := GreetingPeriod(c.t); got != c.want {
			t.Errorf("%s: %s, want %s", c.t.Format("15:04Z"), got, c.want)
		}
	}
}

func TestOverviewNumbersOnASeededStore(t *testing.T) {
	e := newEnv(t) // now = 2026-11-17 12:00 EAT
	siteB, _ := e.addSite(t, "Block B")
	now := e.now
	eat := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, EAT) }
	var pkg store.Package
	e.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if !p.Deleted {
				pkg = p
				break
			}
		}
	})
	acct := func(id, site, flat string, pk string, exp time.Time, mut func(*store.Account)) store.Account {
		a := store.Account{ID: id, SiteID: site, Code: strings.ToUpper(id), Flat: flat, Tenant: "T " + flat, PackageID: pk, ExpiresAt: exp, CreatedAt: now.Add(-90 * 24 * time.Hour)}
		if mut != nil {
			mut(&a)
		}
		return a
	}
	pay := func(key, site, acct string, kes int, at time.Time, method, status string) store.Payment {
		return store.Payment{Key: key, SiteID: site, AccountID: acct, Method: method, Status: status, AmountKES: kes, PaidAt: at, CreatedAt: at}
	}
	err := e.st.Update(func(d *store.Data) error {
		for _, a := range []store.Account{
			acct("a1", e.site, "Flat 1", pkg.ID, now.Add(10*time.Hour), nil),                      // ends tonight: renewal, 7d, day 0
			acct("a2", e.site, "Flat 2", pkg.ID, now.Add(5*24*time.Hour), nil),                    // 7d, day 5
			acct("a3", e.site, "Flat 3", pkg.ID, now.Add(20*24*time.Hour), nil),                   // far
			acct("a4", siteB, "Flat 4", pkg.ID, now.Add(-40*24*time.Hour), nil),                   // churned
			acct("a5", e.site, "Flat 5", pkg.ID, now.Add(-5*24*time.Hour), nil),                   // expired, not churned
			acct("a6", e.site, "Flat 6", "", time.Time{}, nil),                                    // new
			acct("a7", siteB, "Flat 7", pkg.ID, now.Add(30*time.Hour), func(a *store.Account) {}), // day 1, renewal 48h
			acct("a8", e.site, "Flat 8", pkg.ID, now.Add(15*24*time.Hour), func(a *store.Account) { a.MACConflict = true }),
		} {
			d.Accounts[a.ID] = a
		}
		s := d.Sites[e.site]
		s.JoinedAt = now.Add(-24 * time.Hour)
		d.Sites[e.site] = s
		for _, p := range []store.Payment{
			pay("p1", e.site, "a1", 1000, eat(17, 8), "mpesa-c2b", store.PayApplied),
			pay("p2", e.site, "a2", 500, eat(17, 1), "mpesa-c2b", store.PayApplied),
			pay("p3", e.site, "a1", 700, eat(16, 20), "mpesa-c2b", store.PayApplied),
			pay("p4", siteB, "a7", 300, eat(14, 10), "mpesa-c2b", store.PayApplied),
			pay("p5", e.site, "a3", 400, eat(2, 10), "cash", store.PayApplied),
			pay("p6", e.site, "a3", 900, eat(30, 10).AddDate(0, -1, 0), "mpesa-c2b", store.PayApplied), // 30 Oct: last month
			pay("p7", e.site, "a3", 200, eat(17, 9), "voucher", store.PayApplied),                      // free time, not revenue
			pay("p8", e.site, "", 250, eat(17, 7), "mpesa-c2b", store.PayUnmatched),
		} {
			d.Payments[p.Key] = p
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	in := OverviewInput{
		RouterOnline: map[string]bool{e.site: true, siteB: false},
		RouterLive:   map[string]bool{e.site: true},
		RouterLoad:   map[string]RouterLoad{e.site: {DownBps: 40_000_000, UtilPct: 8}, siteB: {DownBps: 1_000_000, UtilPct: 0.2}},
		Online:       func(a store.Account) bool { return a.ID == "a1" || a.ID == "a2" || a.ID == "a7" },
		LastSeen: func(a store.Account) time.Time {
			if a.ID == "a7" {
				return now.Add(-8 * 24 * time.Hour) // not seen for 8 days
			}
			if a.ID == "a1" {
				return now.Add(-time.Minute)
			}
			return time.Time{} // unknown is never "at risk"
		},
		MpesaConfigured: true,
	}
	o := e.svc.Overview(in)
	if o.GreetingPeriod != "afternoon" {
		t.Errorf("greeting %s", o.GreetingPeriod)
	}
	if r := o.Revenue; r.TodayKES != 1500 || r.YesterdayKES != 700 || r.MonthKES != 2900 {
		t.Errorf("revenue %+v", r)
	} else if want := []int{0, 0, 0, 300, 0, 700, 1500}; len(r.Spark7d) != 7 || !equalInts(r.Spark7d, want) {
		t.Errorf("spark %v, want %v", r.Spark7d, want)
	}
	if o.Active != 5 || o.OnlineNow != 3 || o.Routers.Total != 2 || o.Routers.Online != 1 {
		t.Errorf("counts active=%d online=%d routers=%+v", o.Active, o.OnlineNow, o.Routers)
	}
	if o.Health.Churned != 1 || o.Health.AtRisk != 1 || o.Health.Expiring7d != 3 {
		t.Errorf("health %+v", o.Health)
	}
	if len(o.ExpiriesPerDay) != 14 || o.ExpiriesPerDay[0].Date != "2026-11-17" || o.ExpiriesPerDay[13].Date != "2026-11-30" {
		t.Fatalf("expiries %+v", o.ExpiriesPerDay)
	}
	if c := []int{o.ExpiriesPerDay[0].Count, o.ExpiriesPerDay[1].Count, o.ExpiriesPerDay[5].Count, o.ExpiriesPerDay[15-2].Count}; !equalInts(c, []int{1, 1, 1, 0}) {
		t.Errorf("per day %v", c)
	}
	if len(o.Renewals48h) != 2 || o.Renewals48h[0].Flat != "Flat 1" || o.Renewals48h[1].Flat != "Flat 7" || o.Renewals48h[1].RouterName != "Block B" || o.Renewals48h[0].Package == "" {
		t.Errorf("renewals %+v", o.Renewals48h)
	}
	if len(o.BusiestRouters) != 2 || o.BusiestRouters[0].Name != "Block A" || o.BusiestRouters[0].Online != 2 || o.BusiestRouters[0].DownBps != 40_000_000 {
		t.Errorf("busiest %+v", o.BusiestRouters)
	}
	if s := o.Setup; s.Total != 6 || s.Done != 5 || s.Pct != 83 { // everything but WhatsApp
		t.Errorf("setup %+v", s)
	}
	kinds := map[string]int{}
	for _, n := range o.Next {
		kinds[n.Kind] = n.Count
	}
	if kinds["routers_offline"] != 1 || kinds["unmatched_payments"] != 1 || kinds["renewals"] != 2 || kinds["mac_conflicts"] != 1 || kinds["no_plan"] != 1 {
		t.Errorf("next %+v", o.Next)
	}
	feed := ""
	for _, f := range o.Today {
		feed += f.Kind + ":" + f.Text + "|"
	}
	if !strings.Contains(feed, "payment:KES 1,000 from Flat 1") || !strings.Contains(feed, "unmatched:KES 250") || strings.Contains(feed, "KES 700") {
		t.Errorf("feed %s", feed)
	}

	// one router
	in.SiteID = e.site
	a := e.svc.Overview(in)
	if a.Revenue.MonthKES != 2600 || a.Active != 4 || a.Routers.Total != 1 || a.Health.Churned != 0 || a.OnlineNow != 2 {
		t.Errorf("router A: %+v active=%d", a.Revenue, a.Active)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
