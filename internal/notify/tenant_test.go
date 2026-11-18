package notify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

type trig struct {
	t    *testing.T
	st   *store.Store
	c    *clock
	f    *fake
	box  *Outbox
	ten  *Tenant
	ends map[string]time.Time // "site|startUnix" -> end
	open map[string]time.Time
	mode string
}

func newTrig(t *testing.T) *trig {
	t.Helper()
	st, err := storetest.Open(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &trig{t: t, st: st, c: &clock{t: noon()}, f: &fake{}, ends: map[string]time.Time{}, open: map[string]time.Time{}, mode: "manual"}
	r.box = newBox(t, r.c, r.f, nil)
	st.Update(func(d *store.Data) error {
		d.Sites["s1"] = store.Site{Mode: store.SiteModeEnv, ID: "s1", Name: "Kilimani"}
		d.Sites["s2"] = store.Site{Mode: store.SiteModeEnv, ID: "s2", Name: "Lavington"}
		d.Packages["pm"] = store.Package{ID: "pm", Name: "Monthly 10 Mbps", Slug: "m10", DurationMin: 43200, SpeedMbps: 10, PriceKES: 3000}
		d.Packages["pd"] = store.Package{ID: "pd", Name: "Daily", Slug: "d", DurationMin: 1440, SpeedMbps: 5, PriceKES: 100}
		return nil
	})
	r.ten = NewTenant(TenantOptions{Store: st, Box: r.box, Now: r.c.Now, PayHint: "M-Pesa Paybill 123456", MinConfirmKES: 250,
		Policy: func() billing.Policy { return billing.Policy{ReminderDays: 3, ReminderOnExpiry: true} },
		Mode:   func() string { return r.mode }, OpenOutage: func(id string) (time.Time, bool) { s, ok := r.open[id]; return s, ok },
		OfflineFiredAt: func(id string) (time.Time, time.Time, bool) {
			s, ok := r.open[id]
			return s, s.Add(5 * time.Minute), ok
		},
		OutageEnd: func(id string, s time.Time) (time.Time, bool) {
			e, ok := r.ends[id+"|"+s.UTC().Format(time.RFC3339)]
			return e, ok
		}})
	return r
}

func (r *trig) account(id, site, flat, wa string, mut func(*store.Account)) {
	r.st.Update(func(d *store.Data) error {
		a := store.Account{ID: id, SiteID: site, Code: strings.ToUpper(id), Flat: flat, WhatsApp: wa, PackageID: "pm", ExpiresAt: r.c.Now().Add(2 * 24 * time.Hour), RemindedFor: r.c.Now().Add(2 * 24 * time.Hour)}
		if mut != nil {
			mut(&a)
		}
		d.Accounts[id] = a
		return nil
	})
}

func (r *trig) pump(n int) {
	for i := 0; i < n; i++ {
		r.c.Add(time.Second)
		r.box.Step(context.Background(), ChanWhatsApp)
	}
}

func (r *trig) kinds(kind string) []Message {
	return r.box.Find(func(m Message) bool { return m.Kind == kind })
}

func TestReminderOncePerExpiry(t *testing.T) {
	r := newTrig(t)
	r.account("a1", "s1", "Flat 3B", "254712345678", nil)
	r.ten.Scan()
	r.ten.Scan()
	r.ten.Scan()
	got := r.kinds(KindExpiryReminder)
	if len(got) != 1 {
		t.Fatalf("one reminder per expiry, got %d", len(got))
	}
	m := got[0]
	exp := r.c.Now().Add(48 * time.Hour)
	if m.Template != TplExpiryReminder || m.To != "254712345678" || len(m.Params) != 6 || m.DedupeKey != "rem:a1:"+fmt.Sprint(exp.Unix()) {
		t.Fatalf("%+v", m)
	}
	want := []string{"Monthly 10 Mbps", "Flat 3B, Kilimani", eatText(exp), "3000", "M-Pesa Paybill 123456", "A1"}
	for i, w := range want {
		if m.Params[i] != w {
			t.Fatalf("param %d = %q want %q", i+1, m.Params[i], w)
		}
	}
	r.pump(3)
	if r.f.count() != 1 {
		t.Fatalf("sent %d", r.f.count())
	}
	// A renewal gives a new expiry and, once the sweeper stamps it, a new reminder.
	newExp := exp.Add(30 * 24 * time.Hour)
	r.st.Update(func(d *store.Data) error {
		a := d.Accounts["a1"]
		a.ExpiresAt = newExp
		d.Accounts["a1"] = a
		return nil
	})
	r.ten.Scan()
	if len(r.kinds(KindExpiryReminder)) != 1 {
		t.Fatal("not reminded for an expiry the sweeper has not flagged")
	}
	r.st.Update(func(d *store.Data) error {
		a := d.Accounts["a1"]
		a.RemindedFor = newExp
		d.Accounts["a1"] = a
		return nil
	})
	r.ten.Scan()
	if len(r.kinds(KindExpiryReminder)) != 2 {
		t.Fatal("a new expiry gets a new reminder")
	}
}

func TestReminderSkippedWhenRenewedBeforeSend(t *testing.T) {
	r := newTrig(t)
	r.account("a1", "s1", "Flat 3B", "254712345678", nil)
	r.ten.Scan()
	r.st.Update(func(d *store.Data) error { // the tenant paid in the meantime
		a := d.Accounts["a1"]
		a.ExpiresAt = a.ExpiresAt.Add(30 * 24 * time.Hour)
		d.Accounts["a1"] = a
		return nil
	})
	r.pump(2)
	if r.f.count() != 0 || len(r.box.List(Filter{Status: StatusSkipped})) != 1 {
		t.Fatalf("sent=%d skipped=%d", r.f.count(), len(r.box.List(Filter{Status: StatusSkipped})))
	}
}

func TestOptOutAndNoNumberRespected(t *testing.T) {
	r := newTrig(t)
	r.account("a1", "s1", "Flat 1", "254712345678", func(a *store.Account) { a.WAOptOut = true })
	r.account("a2", "s1", "Flat 2", "", nil)
	r.account("a3", "s1", "Flat 3", "254722000000", nil)
	r.ten.Scan()
	got := r.box.List(Filter{})
	if len(got) != 1 || got[0].AccountID != "a3" {
		t.Fatalf("%+v", got)
	}
	// Opting out after queueing: skipped at send time.
	r.st.Update(func(d *store.Data) error { a := d.Accounts["a3"]; a.WAOptOut = true; d.Accounts["a3"] = a; return nil })
	r.pump(2)
	if r.f.count() != 0 {
		t.Fatal("opt-out must stop a queued message")
	}
}

func TestExpiredOnlyForLongPlans(t *testing.T) {
	r := newTrig(t)
	gone := r.c.Now().Add(-3 * time.Hour)
	r.account("m1", "s1", "Flat 1", "254712345678", func(a *store.Account) { a.ExpiresAt, a.RemindedFor, a.ExpiredFor = gone, time.Time{}, gone })
	r.account("d1", "s1", "Flat 2", "254722000000", func(a *store.Account) {
		a.PackageID, a.ExpiresAt, a.RemindedFor, a.ExpiredFor = "pd", gone, time.Time{}, gone
	})
	r.account("old", "s1", "Flat 3", "254733000000", func(a *store.Account) {
		old := r.c.Now().Add(-10 * 24 * time.Hour)
		a.ExpiresAt, a.RemindedFor, a.ExpiredFor = old, time.Time{}, old
	})
	r.ten.Scan()
	got := r.kinds(KindPlanExpired)
	if len(got) != 1 || got[0].AccountID != "m1" || got[0].Template != TplPlanExpired || len(got[0].Params) != 4 {
		t.Fatalf("only the monthly plan that ended recently: %+v", got)
	}
	// policy off
	r.ten.o.Policy = func() billing.Policy { return billing.Policy{ReminderDays: 3, ReminderOnExpiry: false} }
	r.account("m2", "s1", "Flat 4", "254744000000", func(a *store.Account) { a.ExpiresAt, a.RemindedFor, a.ExpiredFor = gone, time.Time{}, gone })
	r.ten.Scan()
	if len(r.kinds(KindPlanExpired)) != 1 {
		t.Fatal("REMINDER_ON_EXPIRY=false stops it")
	}
}

func TestPaymentConfirmations(t *testing.T) {
	r := newTrig(t)
	r.account("a1", "s1", "Flat 3B", "254712345678", func(a *store.Account) { a.RemindedFor = time.Time{} })
	now := r.c.Now()
	pay := func(key, method string, amount int, age time.Duration) {
		r.st.Update(func(d *store.Data) error {
			d.Payments[key] = store.Payment{Key: key, SiteID: "s1", Method: method, Status: store.PayApplied, AmountKES: amount, AccountID: "a1", PackageID: "pm", CreatedAt: now.Add(-age), PaidAt: now.Add(-age)}
			return nil
		})
	}
	pay("QWE1", "mpesa-c2b", 3000, time.Minute)
	pay("ADMIN-1", "admin", 3000, time.Minute) // free grant: no confirmation
	pay("VOUCHER-X", "voucher", 3000, time.Minute)
	pay("SMALL", "mpesa-c2b", 100, time.Minute) // below WA_MIN_CONFIRM_KES
	pay("OLD", "mpesa-c2b", 3000, 7*time.Hour)  // outside the 6 hours
	r.st.Update(func(d *store.Data) error {     // an unmatched payment has no account
		d.Payments["UNM"] = store.Payment{Key: "UNM", Method: "mpesa-c2b", Status: store.PayUnmatched, AmountKES: 3000, CreatedAt: now}
		return nil
	})
	r.ten.Scan()
	r.ten.Scan()
	got := r.kinds(KindPaymentReceived)
	if len(got) != 1 || got[0].DedupeKey != "pay:QWE1" || got[0].Template != TplPaymentReceived || len(got[0].Params) != 5 {
		t.Fatalf("%+v", got)
	}
	if p := got[0].Params; p[0] != "3000" || p[1] != "QWE1" || p[2] != "Monthly 10 Mbps" || p[3] != "Flat 3B, Kilimani" {
		t.Fatalf("%v", p)
	}
}

func TestOutageNoticeManualAutoAndResolved(t *testing.T) {
	r := newTrig(t)
	r.account("a1", "s1", "Flat 1", "254711000001", func(a *store.Account) { a.RemindedFor = time.Time{} })
	r.account("a2", "s1", "Flat 2", "254711000002", func(a *store.Account) { a.RemindedFor = time.Time{} })
	r.account("a3", "s1", "Flat 3", "254711000003", func(a *store.Account) { a.RemindedFor, a.WAOptOut = time.Time{}, true })
	r.account("b1", "s2", "Flat 9", "254711000009", func(a *store.Account) { a.RemindedFor = time.Time{} })
	start := r.c.Now().Add(-40 * time.Minute)
	r.open["s1"] = start
	// manual mode: nothing happens until the admin presses the button
	r.ten.Scan()
	if len(r.kinds(KindOutageNotice)) != 0 {
		t.Fatal("manual mode must not send by itself")
	}
	if got := r.ten.ManualOutageStart("s1"); !got.Equal(start) {
		t.Fatal(got)
	}
	if n := r.ten.SendOutageNotice("s1", start); n != 2 {
		t.Fatalf("two reachable tenants in s1: %d", n)
	}
	if n := r.ten.SendOutageNotice("s1", start); n != 0 {
		t.Fatal("pressing twice sends nothing more")
	}
	r.pump(4)
	notices := r.kinds(KindOutageNotice)
	if len(notices) != 2 || r.f.count() != 2 {
		t.Fatalf("%d %d", len(notices), r.f.count())
	}
	for _, m := range notices {
		if m.SiteID != "s1" || m.Template != TplOutageNotice || len(m.Params) != 2 || m.Params[0] != "Kilimani" || m.Params[1] != eatText(start) {
			t.Fatalf("%+v", m)
		}
	}
	// restored: only the two tenants who got the notice are told
	r.ten.Scan()
	if len(r.kinds(KindOutageResolved)) != 0 {
		t.Fatal("still down")
	}
	r.ends["s1|"+start.UTC().Format(time.RFC3339)] = r.c.Now()
	r.ten.Scan()
	r.ten.Scan()
	res := r.kinds(KindOutageResolved)
	if len(res) != 2 || res[0].Template != TplOutageResolved || res[0].Params[0] != "Kilimani" {
		t.Fatalf("%+v", res)
	}
	// auto mode for another building
	r.mode = "auto"
	r.account("c1", "s2", "Flat 10", "254711000010", func(a *store.Account) { a.RemindedFor = time.Time{} })
	start2 := r.c.Now().Add(-40 * time.Minute)
	r.open["s2"] = start2 // fake "fired at" is start+5m, so 35 minutes ago: past the default 30
	r.ten.Scan()
	if got := r.box.Find(func(m Message) bool { return m.Kind == KindOutageNotice && m.SiteID == "s2" }); len(got) != 2 {
		t.Fatalf("auto mode notifies s2's two tenants: %d", len(got))
	}
	// an outage that ended before the notice went out is skipped
	r.ends["s2|"+start2.UTC().Format(time.RFC3339)] = r.c.Now()
	r.pump(4)
	if got := r.box.Find(func(m Message) bool {
		return m.Kind == KindOutageNotice && m.SiteID == "s2" && m.Status == StatusSkipped
	}); len(got) != 2 {
		t.Fatalf("stale notices skipped: %d", len(got))
	}
	r.mode = "off"
}
