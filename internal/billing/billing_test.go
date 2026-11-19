package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

const mac1 = "AA:BB:CC:DD:EE:01"

// macB is the second building's flat: a MAC is unique across all buildings.
const macB = "AA:BB:CC:DD:EE:0B"

type env struct {
	svc     *Service
	st      *store.Store
	rt      *routeros.Mock // building A's router
	routers MockRouters
	site    string // building A
	now     time.Time
	dir     string
	h       *storetest.Handle
}

// addSite creates another building in the store and gives it its own mock router.
func (e *env) addSite(t *testing.T, name string) (id string, rt *routeros.Mock) {
	t.Helper()
	id = store.NewSiteID()
	rt = routeros.NewMock()
	err := e.st.Update(func(d *store.Data) error {
		d.Sites[id] = store.Site{ID: id, Name: name, Mode: store.SiteModeWG, WGPublicKey: "PUB" + id, TunnelIP: "10.99.0." + fmt.Sprint(len(d.Sites)+2),
			AccessMode: store.AccessIPoE, CreatedAt: e.now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seedRouter(rt)
	e.routers[id] = rt
	return id, rt
}

// seedRouter gives a mock what the bootstrap script builds: the ftn-dhcp
// server on the customer bridge.
func seedRouter(rt *routeros.Mock) {
	rt.SeedRow("/ip/dhcp-server", routeros.Row{"name": DHCPServerName, "interface": "br-customers", "disabled": "false"})
	rt.ResetWrites()
}

// setRT swaps building A's router (a "wiped router").
func (e *env) setRT(rt *routeros.Mock) {
	seedRouter(rt)
	e.rt = rt
	e.routers[e.site] = rt
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{dir: t.TempDir(), rt: routeros.NewMock(), now: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	e.routers = MockRouters{}
	e.open(t)
	e.site, _ = e.addSite(t, "Block A")
	seedRouter(e.rt)
	e.routers[e.site] = e.rt
	if err := e.svc.SeedPackages(); err != nil {
		t.Fatal(err)
	}
	return e
}

// open (re)opens the store and service on the same directory: a "restart".
func (e *env) open(t *testing.T) {
	t.Helper()
	if e.h == nil {
		e.h = storetest.NewIn(t, e.dir)
	} else {
		e.h.Reopen()
	}
	st := e.h.Store
	e.st = st
	e.svc = NewService(st, e.routers, quiet())
	e.svc.SetClock(func() time.Time { return e.now })
}

func (e *env) pkg(name string) store.Package {
	var out store.Package
	e.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if p.Name == name {
				out = p
			}
		}
	})
	return out
}

func (e *env) flat(t *testing.T, label, mac string) store.Account {
	t.Helper()
	a, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: label, MAC: mac})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) get(a store.Account) store.Account {
	cur, _ := e.svc.GetAccount(a.ID)
	return cur
}

// pay runs a test-payment order for the account (as the portal does).
func (e *env) pay(t *testing.T, a store.Account, pkg, ip string) store.Order {
	t.Helper()
	o, err := e.svc.CreateOrder(OrderInput{AccountID: a.ID, PackageID: e.pkg(pkg).ID, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteOrder(context.Background(), o.ID, "TEST-"+o.ID); err != nil {
		t.Fatal(err)
	}
	o, _ = e.svc.GetOrder(o.ID)
	return o
}

func TestNormalizePhone(t *testing.T) {
	good := map[string]string{
		"0712345678": "254712345678", "0112345678": "254112345678", "+254712345678": "254712345678",
		"254712345678": "254712345678", "712345678": "254712345678", "0712 345-678": "254712345678",
	}
	for in, want := range good {
		if got, err := NormalizePhone(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "071234567", "0212345678", "abc", "+255712345678"} {
		if _, err := NormalizePhone(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestNormalizeMACAndTransCode(t *testing.T) {
	for _, in := range []string{"aa:bb:cc:dd:ee:01", "AA-BB-CC-DD-EE-01", "aabb.ccdd.ee01", "aabbccddee01"} {
		if got, err := NormalizeMAC(in); err != nil || got != "AA:BB:CC:DD:EE:01" {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "AA:BB:CC:DD:EE", "AA:BB:CC:DD:EE:0G", "01:00:5E:00:00:01", "FF:FF:FF:FF:FF:FF", "00:00:00:00:00:00"} {
		if _, err := NormalizeMAC(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
	if c, err := NormalizeTransCode(" sgl7k2m9xa "); err != nil || c != "SGL7K2M9XA" {
		t.Error(c, err)
	}
	for _, in := range []string{"", "ABC", "SGL7K2M9X", "SGL7K2M9XAB", "SGL7K2M9X!"} {
		if _, err := NormalizeTransCode(in); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestCodes(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := RandomCode(8)
		if len(c) != 8 {
			t.Fatal(c)
		}
		for _, r := range c {
			if r == '0' || r == 'O' || r == '1' || r == 'I' || r == 'L' {
				t.Fatalf("ambiguous char in %s", c)
			}
		}
	}
}

func TestSeededPackages(t *testing.T) {
	e := newEnv(t)
	want := map[string]struct{ min, mbps, kes int }{
		"1 Hour": {60, 5, 10}, "1 Day": {1440, 10, 50}, "1 Week": {10080, 10, 250},
		"Monthly 20 Mbps": {43200, 20, 2000}, "Monthly 40 Mbps": {43200, 40, 3000}, "Monthly 80 Mbps": {43200, 80, 4000},
	}
	for name, w := range want {
		p := e.pkg(name)
		if p.ID == "" || p.DurationMin != w.min || p.SpeedMbps != w.mbps || p.PriceKES != w.kes {
			t.Errorf("%s: %+v", name, p)
		}
	}
	e.st.View(func(d *store.Data) {
		if len(d.Accounts) != 0 {
			t.Error("no accounts may be seeded")
		}
	})
}

func TestCreateAccountRules(t *testing.T) {
	e := newEnv(t)
	a, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: " Flat   3B ", MAC: "aa-bb-cc-dd-ee-01", Tenant: "Jane", Phone: "0712345678", Notes: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Flat != "Flat 3B" || a.MAC != mac1 || len(a.Code) != 6 || a.Phone != "254712345678" {
		t.Fatalf("%+v", a)
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 4A", MAC: mac1}); err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Fatalf("duplicate MAC: %v", err)
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "flat 3b", MAC: "AA:BB:CC:DD:EE:02"}); err == nil {
		t.Fatal("duplicate flat label")
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "", MAC: "AA:BB:CC:DD:EE:02"}); err == nil {
		t.Fatal("flat required")
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "X", MAC: "nonsense"}); err == nil {
		t.Fatal("mac required")
	}
	// Code is immutable through every edit path.
	if err := e.svc.UpdateAccount(a.ID, "Flat 3B", "Joe", "", "x"); err != nil {
		t.Fatal(err)
	}
	if e.get(a).Code != a.Code || e.get(a).Tenant != "Joe" || e.get(a).Phone != "" {
		t.Fatalf("%+v", e.get(a))
	}
}

func TestUnknownMACGetsNoAccountAndIsRecorded(t *testing.T) {
	e := newEnv(t)
	if _, ok := e.svc.AccountByMAC("AA:BB:CC:DD:EE:77"); ok {
		t.Fatal("unknown MAC must not match")
	}
	e.svc.NoteDevice(e.site, "AA:BB:CC:DD:EE:77", "10.5.50.9", "portal")
	e.svc.NoteDevice(e.site, "AA:BB:CC:DD:EE:77", "10.5.50.10", "portal")
	e.st.View(func(d *store.Data) {
		dev := d.Devices[store.DeviceKey(e.site, "AA:BB:CC:DD:EE:77")]
		if dev.Hits != 2 || dev.IP != "10.5.50.10" {
			t.Fatalf("%+v", dev)
		}
	})
	// Becoming an account removes it from the list; known MACs are never listed.
	a := e.flat(t, "Flat 1A", "AA:BB:CC:DD:EE:77")
	e.svc.NoteDevice(e.site, a.MAC, "10.5.50.9", "portal")
	e.st.View(func(d *store.Data) {
		if len(d.Devices) != 0 {
			t.Fatalf("devices %v", d.Devices)
		}
	})
}

func TestExtendAndUpgrade(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	e.pay(t, a, "1 Day", "")
	first := e.get(a).ExpiresAt
	e.now = e.now.Add(6 * time.Hour)
	e.pay(t, a, "1 Day", "")
	if want := first.Add(24 * time.Hour); !e.get(a).ExpiresAt.Equal(want) {
		t.Fatalf("got %v want %v", e.get(a).ExpiresAt, want)
	}
	before := e.get(a).ExpiresAt
	e.pay(t, a, "Monthly 80 Mbps", "")
	cur := e.get(a)
	if cur.PackageID != e.pkg("Monthly 80 Mbps").ID || !cur.ExpiresAt.Equal(before.Add(30*24*time.Hour)) {
		t.Fatalf("%+v", cur)
	}
}

func TestRouterRetryQueueSurvivesOutageAndRestart(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 3B", mac1)
	e.rt.SetFail(context.DeadlineExceeded)
	o := e.pay(t, a, "1 Day", "10.5.50.20")
	if o.Status != store.OrderPaid {
		t.Fatalf("payment must succeed while the router is down: %+v", o)
	}
	tasks := e.svc.PendingTasks()
	if len(tasks) != 1 || tasks[0].Code != a.Code || tasks[0].Attempts != 1 || tasks[0].LastErr == "" {
		t.Fatalf("tasks %+v", tasks)
	}
	// Backoff: not retried before NextTry.
	e.rt.SetFail(nil)
	if n := e.svc.RunTasks(context.Background()); n != 0 {
		t.Fatal("retried before backoff elapsed")
	}
	// "Restart": new store + service on the same files; the task is still there.
	e.open(t)
	if len(e.svc.PendingTasks()) != 1 {
		t.Fatal("queue lost on restart")
	}
	e.now = e.now.Add(10 * time.Second)
	if n := e.svc.RunTasks(context.Background()); n != 1 {
		t.Fatalf("task not run after restart: %d", n)
	}
	if len(e.svc.PendingTasks()) != 0 {
		t.Fatal("task not cleared")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	prev := time.Duration(0)
	for i := 1; i < 12; i++ {
		d := backoff(i)
		if d < prev || d > backoffMax {
			t.Fatalf("attempt %d: %v", i, d)
		}
		prev = d
	}
	if backoff(1) != 5*time.Second || backoff(20) != backoffMax {
		t.Fatal(backoff(1), backoff(20))
	}
}

func TestVoucherAppliesToMACsAccountAndIsSingleUse(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	b := e.flat(t, "Flat 4A", "AA:BB:CC:DD:EE:02")
	_, vs, err := e.svc.GenerateVouchers(e.pkg("1 Day").ID, "", 2)
	if err != nil || len(vs) != 2 || len(vs[0].Code) != 8 {
		t.Fatal(err)
	}
	redeem := func(acct store.Account, code string) error {
		o, err := e.svc.CreateOrder(OrderInput{AccountID: acct.ID, PackageID: vs[0].PackageID, Method: "voucher", VoucherCode: code})
		if err != nil {
			t.Fatal(err)
		}
		return e.svc.CompleteOrder(context.Background(), o.ID, "VOUCHER-"+code)
	}
	if err := redeem(a, vs[0].Code); err != nil {
		t.Fatal(err)
	}
	if err := redeem(b, vs[0].Code); err != ErrVoucherUsed {
		t.Fatalf("second redemption: %v", err)
	}
	if !e.get(a).ExpiresAt.Equal(e.now.Add(24*time.Hour)) || !e.get(b).ExpiresAt.IsZero() {
		t.Fatal("voucher applied to the wrong account")
	}
	if _, err := e.svc.LookupVoucher(vs[0].Code); err == nil {
		t.Fatal("used voucher still valid")
	}
}

func TestManualMpesaAppliesAndRejectsDuplicates(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	ctx := context.Background()
	paid := time.Date(2026, 11, 16, 18, 0, 0, 0, time.UTC)
	p, err := e.svc.RecordManual(ctx, ManualInput{AccountID: a.ID, TransCode: "sgl7k2m9xa", AmountKES: 3000, Phone: "0712345678", PaidAt: paid})
	if err != nil {
		t.Fatal(err)
	}
	if p.Key != "SGL7K2M9XA" || p.Method != "mpesa-manual" || p.PackageID != e.pkg("Monthly 40 Mbps").ID || !p.PaidAt.Equal(paid) || p.Phone != "254712345678" {
		t.Fatalf("%+v", p)
	}
	if want := e.now.Add(30 * 24 * time.Hour); !e.get(a).ExpiresAt.Equal(want) {
		t.Fatalf("expiry %v", e.get(a).ExpiresAt)
	}
	exp := e.get(a).ExpiresAt
	if _, err := e.svc.RecordManual(ctx, ManualInput{AccountID: a.ID, TransCode: "SGL7K2M9XA", AmountKES: 3000}); err != ErrDuplicateKey {
		t.Fatalf("duplicate code: %v", err)
	}
	if !e.get(a).ExpiresAt.Equal(exp) {
		t.Fatal("duplicate extended")
	}
	if _, err := e.svc.RecordManual(ctx, ManualInput{AccountID: a.ID, TransCode: "SHORT", AmountKES: 3000}); err != ErrBadTransCode {
		t.Fatalf("bad code: %v", err)
	}
	if _, err := e.svc.RecordManual(ctx, ManualInput{AccountID: a.ID, TransCode: "ABCDE12345", AmountKES: 77}); err == nil {
		t.Fatal("amount with no package and no explicit package must be refused")
	}
	// Explicit package with a different amount is allowed and noted. The flat is
	// active on Monthly 40, so a slower package needs the explicit force.
	p, err = e.svc.RecordManual(ctx, ManualInput{AccountID: a.ID, TransCode: "ABCDE12346", AmountKES: 100, PackageID: e.pkg("1 Day").ID, Force: true})
	if err != nil || !strings.Contains(p.Note, "differs") {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestCreditIsIdempotentPerKey(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	in := CreditInput{Key: "RKT1ABCDEF", Method: "mpesa-c2b", AccountID: a.ID, PackageID: e.pkg("1 Day").ID, AmountKES: 50}
	_, created, err := e.svc.Credit(context.Background(), in)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	exp := e.get(a).ExpiresAt
	_, created, err = e.svc.Credit(context.Background(), in)
	if err != nil || created || !e.get(a).ExpiresAt.Equal(exp) {
		t.Fatal("duplicate key must be a no-op", created, err)
	}
}

func TestUnmatchedAssignAndResolve(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	ctx := context.Background()
	for _, k := range []string{"UNM1ABCDEF", "UNM2ABCDEF", "UNM3ABCDEF"} {
		if c, err := e.svc.RecordUnmatched(UnmatchedInput{Key: k, Method: "mpesa-c2b", AmountKES: 77, BillRef: "ZZZ", Note: "unknown account"}); err != nil || !c {
			t.Fatal(err, c)
		}
	}
	if c, _ := e.svc.RecordUnmatched(UnmatchedInput{Key: "UNM1ABCDEF", Method: "mpesa-c2b", AmountKES: 77}); c {
		t.Fatal("duplicate must be a no-op")
	}
	if err := e.svc.AssignPayment(ctx, "UNM1ABCDEF", a.ID, e.pkg("1 Week").ID, "customer called"); err != nil {
		t.Fatal(err)
	}
	if !e.get(a).ExpiresAt.Equal(e.now.Add(7 * 24 * time.Hour)) {
		t.Fatal("not applied")
	}
	e.st.View(func(d *store.Data) {
		p := d.Payments["UNM1ABCDEF"]
		if p.Status != store.PayApplied || p.AccountID != a.ID || !strings.Contains(p.Note, "customer called") {
			t.Fatalf("%+v", p)
		}
	})
	if err := e.svc.AssignPayment(ctx, "UNM1ABCDEF", a.ID, e.pkg("1 Week").ID, ""); err != ErrBadState {
		t.Fatalf("assigning twice: %v", err)
	}
	if err := e.svc.ResolvePayment("UNM2ABCDEF", store.PayRefunded, "refunded via M-Pesa"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResolvePayment("UNM3ABCDEF", store.PayIgnored, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResolvePayment("UNM3ABCDEF", "bogus", ""); err == nil {
		t.Fatal("bad status")
	}
	e.st.View(func(d *store.Data) {
		if d.Payments["UNM2ABCDEF"].Status != store.PayRefunded || d.Payments["UNM3ABCDEF"].Status != store.PayIgnored {
			t.Fatal("not resolved")
		}
	})
}

func TestReminderEventsAndPolicy(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	e.pay(t, a, "1 Week", "")
	ctx := context.Background()
	count := func(kind string) (n int) {
		e.st.View(func(d *store.Data) {
			for _, ev := range d.Events {
				if ev.Kind == kind {
					n++
				}
			}
		})
		return
	}
	e.svc.Sweep(ctx)
	if count("reminder_due") != 0 {
		t.Fatal("too early")
	}
	e.now = e.now.Add(4*24*time.Hour + time.Hour) // 2 days 23 h left
	e.svc.Sweep(ctx)
	e.svc.Sweep(ctx)
	if count("reminder_due") != 1 {
		t.Fatalf("reminder events %d", count("reminder_due"))
	}
	e.now = e.now.Add(4 * 24 * time.Hour)
	e.svc.Sweep(ctx)
	e.svc.Sweep(ctx)
	if count("expired") != 1 {
		t.Fatalf("expired events %d", count("expired"))
	}
	// A 1-hour pass never raises a 3-day reminder.
	b := e.flat(t, "Flat 4A", "AA:BB:CC:DD:EE:02")
	e.pay(t, b, "1 Hour", "")
	e.svc.Sweep(ctx)
	if count("reminder_due") != 1 {
		t.Fatal("short pass raised a reminder")
	}
}

func TestSavePackageUpdatesQueue(t *testing.T) {
	e := newEnv(t)
	p := e.pkg("1 Day")
	p.SpeedMbps, p.PriceKES = 15, 60
	if _, err := e.svc.SavePackage(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait() // the reconcile runs in the background
	if got := e.pkg("1 Day"); got.SpeedMbps != 15 || got.PriceKES != 60 {
		t.Fatalf("%+v", got)
	}
}

func TestNaturalFlatOrdering(t *testing.T) {
	e := newEnv(t)
	e.flat(t, "Flat 10A", "AA:BB:CC:DD:EE:0A")
	e.flat(t, "Flat 2B", "AA:BB:CC:DD:EE:0C")
	e.flat(t, "Flat 2A", "AA:BB:CC:DD:EE:0E")
	var got []string
	e.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			got = append(got, a.Flat)
		}
	})
	if strings.Join(got, ",") != "Flat 2A,Flat 2B,Flat 10A" {
		t.Fatal(got)
	}
}

func TestResyncAllRebuildsRouterAfterRouterReset(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	e.pay(t, a, "1 Day", "")
	e.setRT(routeros.NewMock()) // router wiped
	e.open(t)
	e.svc.ResyncAll()
	e.svc.Sweep(context.Background())
}

func TestUpgradeAllowedDowngradeRefusedWhileActive(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	ctx := context.Background()
	e.pay(t, a, "Monthly 40 Mbps", "")
	exp := e.get(a).ExpiresAt

	// Same or higher speed is fine.
	e.pay(t, a, "Monthly 80 Mbps", "")
	if cur := e.get(a); cur.PackageID != e.pkg("Monthly 80 Mbps").ID || !cur.ExpiresAt.Equal(exp.Add(30*24*time.Hour)) {
		t.Fatalf("upgrade: %+v", cur)
	}
	exp = e.get(a).ExpiresAt

	// A slower package is refused at checkout, with the date.
	_, err := e.svc.CreateOrder(OrderInput{AccountID: a.ID, PackageID: e.pkg("Monthly 20 Mbps").ID, Method: "test"})
	if err == nil || !strings.Contains(err.Error(), "current plan ends on") {
		t.Fatalf("downgrade order: %v", err)
	}
	if _, blocked := e.svc.LowerPlanBlocked(e.get(a), e.pkg("1 Hour")); !blocked {
		t.Fatal("1 Hour should be blocked")
	}
	if _, blocked := e.svc.LowerPlanBlocked(e.get(a), e.pkg("Monthly 80 Mbps")); blocked {
		t.Fatal("same speed must not be blocked")
	}
	// Credit paths refuse too (and leave the voucher alone).
	if _, _, err := e.svc.Credit(ctx, CreditInput{Key: "ABCDE12399", Method: "mpesa-c2b", AccountID: a.ID, PackageID: e.pkg("1 Day").ID}); !errors.Is(err, ErrLowerPlan) {
		t.Fatalf("credit: %v", err)
	}
	if cur := e.get(a); cur.PackageID != e.pkg("Monthly 80 Mbps").ID || !cur.ExpiresAt.Equal(exp) {
		t.Fatalf("account changed: %+v", cur)
	}
	// Manual entry: refused, saved as unmatched, nothing applied.
	if _, err := e.svc.RecordManual(ctx, ManualInput{AccountID: a.ID, TransCode: "MAN1ABCDEF", AmountKES: 50, PackageID: e.pkg("1 Day").ID}); !errors.Is(err, ErrLowerPlan) {
		t.Fatalf("manual: %v", err)
	}
	var un store.Payment
	e.st.View(func(d *store.Data) { un = d.Payments["MAN1ABCDEF"] })
	if un.Status != store.PayUnmatched || !strings.Contains(un.Note, "lower plan while active") || un.AccountID != a.ID {
		t.Fatalf("unmatched row: %+v", un)
	}
	if !e.get(a).ExpiresAt.Equal(exp) {
		t.Fatal("manual lower plan was applied")
	}

	// Admin forced downgrade works and changes the router profile now.
	if err := e.svc.ExtendAccount(ctx, a.ID, e.pkg("Monthly 20 Mbps").ID, "forced", true); err != nil {
		t.Fatal(err)
	}
	if cur := e.get(a); cur.PackageID != e.pkg("Monthly 20 Mbps").ID || !cur.ExpiresAt.Equal(exp.Add(30*24*time.Hour)) {
		t.Fatalf("forced: %+v", cur)
	}
	// Without force the admin grant is refused.
	if err := e.svc.ExtendAccount(ctx, a.ID, e.pkg("1 Hour").ID, "", false); !errors.Is(err, ErrLowerPlan) {
		t.Fatalf("grant: %v", err)
	}

	// Expired accounts may buy anything.
	e.now = e.get(a).ExpiresAt.Add(time.Hour)
	e.pay(t, a, "1 Hour", "")
	if e.get(a).PackageID != e.pkg("1 Hour").ID {
		t.Fatal("expired account should be able to pick a slower plan")
	}
}
