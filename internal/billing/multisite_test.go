package billing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func (e *env) flatAt(t *testing.T, site, label, mac string) store.Account {
	t.Helper()
	a, err := e.svc.CreateAccount(AccountInput{SiteID: site, Flat: label, MAC: mac})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) payAt(t *testing.T, a store.Account, pkg string) {
	t.Helper()
	_, _, err := e.svc.Credit(context.Background(), CreditInput{Key: "K" + a.Code + pkg[:3], Method: "mpesa-manual",
		AccountID: a.ID, PackageID: e.pkg(pkg).ID, AmountKES: e.pkg(pkg).PriceKES})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSameFlatInTwoSitesButMACIsGlobal(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	a1 := e.flat(t, "Flat 1", mac1)
	b1 := e.flatAt(t, b, "Flat 1", "AA:BB:CC:DD:EE:21")
	if a1.ID == b1.ID || a1.Code == b1.Code || a1.SiteID != e.site || b1.SiteID != b {
		t.Fatalf("%+v %+v", a1, b1)
	}
	// One MAC, one open account, in any building.
	if got, ok := e.svc.AccountByMAC(mac1); !ok || got.ID != a1.ID {
		t.Fatal("global lookup")
	}
	_, err := e.svc.CreateAccount(AccountInput{SiteID: b, Flat: "Flat 2", MAC: "aa-bb-cc-dd-ee-01"})
	var dup *DuplicateMACError
	if !errors.Is(err, ErrDuplicateMAC) || !errors.Is(err, store.ErrDuplicateMAC) || !errors.As(err, &dup) {
		t.Fatalf("duplicate MAC in another building: %v", err)
	}
	if dup.AccountID != a1.ID || dup.Code != a1.Code || dup.Flat != "Flat 1" || dup.SiteID != e.site || dup.SiteName == "" {
		t.Fatalf("owner details: %+v", dup)
	}
	if err := e.svc.ChangeMAC(context.Background(), b1.ID, mac1); !errors.As(err, &dup) || dup.AccountID != a1.ID {
		t.Fatalf("ChangeMAC onto another building's MAC: %v", err)
	}
	// Closing the owner frees the MAC for any building.
	if err := e.svc.DeleteAccount(context.Background(), a1.ID, a1.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: b, Flat: "Flat 2", MAC: mac1}); err != nil {
		t.Fatalf("MAC of a closed account must be free: %v", err)
	}
	// Still unique inside a building.
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: b, Flat: "flat 1", MAC: "AA:BB:CC:DD:EE:09"}); err == nil {
		t.Fatal("duplicate label inside a building")
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: b, Flat: "Flat 3", MAC: mac1}); !errors.Is(err, ErrDuplicateMAC) {
		t.Fatalf("duplicate MAC inside a building: %v", err)
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: "nope", Flat: "Flat 2", MAC: "AA:BB:CC:DD:EE:0A"}); !errors.Is(err, ErrNoSite) {
		t.Fatalf("unknown site: %v", err)
	}
	if _, err := e.svc.CreateAccount(AccountInput{Flat: "Flat 2", MAC: "AA:BB:CC:DD:EE:0A"}); !errors.Is(err, ErrNoSite) {
		t.Fatalf("missing site: %v", err)
	}
	// Unknown-device lists are per building too.
	e.svc.NoteDevice(b, "AA:BB:CC:DD:EE:77", "10.5.50.9", "portal")
	e.st.View(func(d *store.Data) {
		if _, ok := d.Devices[store.DeviceKey(e.site, "AA:BB:CC:DD:EE:77")]; ok || len(d.Devices) != 1 {
			t.Fatalf("%v", d.Devices)
		}
	})
}

func TestPaymentsTouchOnlyTheirSitesRouter(t *testing.T) {
	e := newEnv(t)
	b, rtB := e.addSite(t, "Block B")
	ctx := context.Background()
	if err := e.svc.SyncProfiles(ctx, b); err != nil {
		t.Fatal(err)
	}
	a1 := e.flatAt(t, e.site, "Flat 1", mac1)
	b1 := e.flatAt(t, b, "Flat 1", macB)
	e.payAt(t, a1, "1 Day")
	if _, ok := e.rt.GetUser(a1.Code); !ok {
		t.Fatal("site A router must hold the user")
	}
	if _, ok := rtB.GetUser(a1.Code); ok {
		t.Fatal("site A's user leaked to site B's router")
	}
	if _, ok := rtB.GetUser(b1.Code); ok {
		t.Fatal("unpaid B flat has a router user")
	}
	e.payAt(t, b1, "1 Day")
	if _, ok := rtB.GetUser(b1.Code); !ok {
		t.Fatal("site B router must hold B's user")
	}
	if _, ok := e.rt.GetUser(b1.Code); ok {
		t.Fatal("site B's user leaked to site A's router")
	}
	e.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			if want := d.Accounts[p.AccountID].SiteID; p.SiteID != want {
				t.Fatalf("payment %s has site %q want %q", p.Key, p.SiteID, want)
			}
		}
	})
}

// blocking is a router whose EnsureUser hangs until released.
type blocking struct {
	*routeros.Mock
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blocking) EnsureUser(ctx context.Context, u routeros.User) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Mock.EnsureUser(ctx, u)
}

type routers map[string]routeros.Client

func (r routers) Router(id string) routeros.Client {
	if c, ok := r[id]; ok {
		return c
	}
	return routeros.NoRouter()
}

func TestSlowSiteDoesNotDelayAnother(t *testing.T) {
	e := newEnv(t)
	bID := store.NewSiteID()
	e.st.Update(func(d *store.Data) error {
		d.Sites[bID] = store.Site{ID: bID, Name: "Block B", Mode: store.SiteModeEnv}
		return nil
	})
	slow := &blocking{Mock: routeros.NewMock(), release: make(chan struct{}), entered: make(chan struct{})}
	e.svc = NewService(e.st, routers{e.site: e.rt, bID: slow}, quiet())
	e.svc.SetClock(func() time.Time { return e.now })
	ctx := context.Background()
	e.svc.SyncProfiles(ctx, e.site)
	e.svc.SyncProfiles(ctx, bID)
	a1 := e.flatAt(t, e.site, "Flat 1", mac1)
	b1 := e.flatAt(t, bID, "Flat 1", macB)
	// Queue work on both (suspend/unsuspend does not need payment: use credit directly in the store).
	dayID := e.pkg("1 Day").ID
	e.st.Update(func(d *store.Data) error {
		for _, id := range []string{a1.ID, b1.ID} {
			a := d.Accounts[id]
			a.PackageID, a.ExpiresAt = dayID, e.now.Add(24*time.Hour)
			d.Accounts[id] = a
			e.svc.enqueueLocked(d, a, false, "test")
		}
		return nil
	})
	done := make(chan int, 1)
	go func() { done <- e.svc.RunTasks(ctx) }()
	<-slow.entered // B is now stuck
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := e.rt.GetUser(a1.Code); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("site A was held up by the stuck site B")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("RunTasks should still be waiting for B")
	default:
	}
	close(slow.release)
	if n := <-done; n != 2 {
		t.Fatalf("both tasks should finish, got %d", n)
	}
}

func TestFailingSiteKeepsItsTasksAndOthersSucceed(t *testing.T) {
	e := newEnv(t)
	b, rtB := e.addSite(t, "Block B")
	e.svc.SyncProfiles(context.Background(), b)
	rtB.SetFail(errors.New("tunnel down"))
	a1 := e.flatAt(t, e.site, "Flat 1", mac1)
	b1 := e.flatAt(t, b, "Flat 1", macB)
	e.payAt(t, a1, "1 Day")
	e.payAt(t, b1, "1 Day") // router fails; the payment still counts
	if _, ok := e.rt.GetUser(a1.Code); !ok {
		t.Fatal("A must be served")
	}
	tasks := e.svc.PendingTasks()
	if len(tasks) != 1 || tasks[0].SiteID != b || tasks[0].LastErr == "" {
		t.Fatalf("%+v", tasks)
	}
	rtB.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Sweep(context.Background())
	if _, ok := rtB.GetUser(b1.Code); !ok || len(e.svc.PendingTasks()) != 0 {
		t.Fatal("B recovers by itself")
	}
}

func TestTasksOfPausedOrUnjoinedSiteWaitWithoutAttempts(t *testing.T) {
	e := newEnv(t)
	b, rtB := e.addSite(t, "Block B")
	e.svc.SyncProfiles(context.Background(), b)
	b1 := e.flatAt(t, b, "Flat 1", macB)
	e.st.Update(func(d *store.Data) error {
		s := d.Sites[b]
		s.Disabled = true
		d.Sites[b] = s
		return nil
	})
	e.payAt(t, b1, "1 Day")
	e.svc.RunTasks(context.Background())
	ts := e.svc.PendingTasks()
	if len(ts) != 1 || ts[0].Attempts != 0 || ts[0].LastErr != "" {
		t.Fatalf("%+v", ts)
	}
	if _, ok := rtB.GetUser(b1.Code); ok {
		t.Fatal("paused site was touched")
	}
	// Not joined yet (wg mode without a key) behaves the same.
	e.st.Update(func(d *store.Data) error {
		s := d.Sites[b]
		s.Disabled, s.Mode = false, store.SiteModeWG
		d.Sites[b] = s
		return nil
	})
	e.svc.RunTasks(context.Background())
	if ts = e.svc.PendingTasks(); len(ts) != 1 || ts[0].Attempts != 0 {
		t.Fatalf("%+v", ts)
	}
	e.st.Update(func(d *store.Data) error {
		s := d.Sites[b]
		s.WGPublicKey = strings.Repeat("A", 43) + "="
		d.Sites[b] = s
		return nil
	})
	e.svc.RunTasks(context.Background())
	if _, ok := rtB.GetUser(b1.Code); !ok || len(e.svc.PendingTasks()) != 0 {
		t.Fatal("task must run once the site is joined")
	}
}

func TestMoveAccount(t *testing.T) {
	e := newEnv(t)
	b, rtB := e.addSite(t, "Block B")
	ctx := context.Background()
	e.svc.SyncProfiles(ctx, b)
	a1 := e.flatAt(t, e.site, "Flat 1", mac1)
	e.payAt(t, a1, "1 Week")
	if _, ok := e.rt.GetUser(a1.Code); !ok {
		t.Fatal("setup")
	}
	before := e.get(a1)
	if err := e.svc.MoveAccount(ctx, a1.ID, b); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.rt.GetUser(a1.Code); ok {
		t.Fatal("old router still holds the user")
	}
	if _, ok := rtB.GetUser(a1.Code); !ok {
		t.Fatal("new router lacks the user")
	}
	after := e.get(a1)
	if after.SiteID != b || after.Code != before.Code || !after.ExpiresAt.Equal(before.ExpiresAt) || after.PackageID != before.PackageID || !after.RouterActive {
		t.Fatalf("%+v", after)
	}
	if len(e.svc.PendingTasks()) != 0 {
		t.Fatalf("%+v", e.svc.PendingTasks())
	}
	// Refused when the target already has the MAC or the label.
	a2 := e.flatAt(t, e.site, "Flat 2", "AA:BB:CC:DD:EE:02")
	e.flatAt(t, b, "Flat 2", "AA:BB:CC:DD:EE:22")
	if err := e.svc.MoveAccount(ctx, a2.ID, b); err == nil || !strings.Contains(err.Error(), "already has an account") {
		t.Fatalf("label clash: %v", err)
	}
	a3 := e.flatAt(t, e.site, "Flat 3", "AA:BB:CC:DD:EE:03")
	// The MAC rule is global, so a building can never already hold a move's MAC.
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: b, Flat: "Flat 9", MAC: "AA:BB:CC:DD:EE:03"}); !errors.Is(err, ErrDuplicateMAC) {
		t.Fatalf("mac clash: %v", err)
	}
	if err := e.svc.MoveAccount(ctx, a3.ID, "nope"); !errors.Is(err, ErrNoSite) {
		t.Fatal(err)
	}
	if err := e.svc.MoveAccount(ctx, "nope", b); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Moving while the old router is down still ends up consistent.
	e.payAt(t, a3, "1 Day")
	e.rt.SetFail(errors.New("down"))
	e.flatAt(t, e.site, "Flat 4", "AA:BB:CC:DD:EE:04")
	a4 := e.flatAt(t, e.site, "Flat 5", "AA:BB:CC:DD:EE:05")
	e.payAt(t, a4, "1 Day")
	e.rt.SetFail(nil)
	e.now = e.now.Add(time.Minute)
	e.svc.Sweep(ctx)
	e.rt.SetFail(errors.New("down"))
	if err := e.svc.MoveAccount(ctx, a4.ID, b); err != nil {
		t.Fatal(err)
	}
	if _, ok := rtB.GetUser(a4.Code); !ok {
		t.Fatal("new site served despite the old router being down")
	}
	e.rt.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Sweep(ctx)
	if _, ok := e.rt.GetUser(a4.Code); ok {
		t.Fatal("old router user must be removed when it comes back")
	}
}

func TestProfilesOKIsPerSite(t *testing.T) {
	e := newEnv(t)
	b, rtB := e.addSite(t, "Block B")
	ctx := context.Background()
	if !e.svc.ProfilesOK(e.site) || e.svc.ProfilesOK(b) {
		t.Fatal("A synced in newEnv, B not yet")
	}
	if err := e.svc.SyncProfiles(ctx, b); err != nil {
		t.Fatal(err)
	}
	rtB.SetFail(errors.New("down"))
	if err := e.svc.SyncProfiles(ctx, b); err == nil || e.svc.ProfilesOK(b) {
		t.Fatal("failed sync must clear the flag")
	}
	if !e.svc.ProfilesOK(e.site) {
		t.Fatal("B's failure changed A's state")
	}
	// A package change marks every site dirty and re-syncs each in the background.
	rtB.SetFail(nil)
	p := e.pkg("1 Hour")
	p.SpeedMbps = 7
	if _, err := e.svc.SavePackage(ctx, p); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if !e.svc.ProfilesOK(e.site) || !e.svc.ProfilesOK(b) {
		t.Fatal("both sites resync")
	}
	if pr, _ := rtB.GetProfile(p.Profile()); pr.RateLimit != "7M/7M" {
		t.Fatalf("%+v", pr)
	}
	// With B down, only B stays dirty.
	rtB.SetFail(errors.New("down"))
	p.SpeedMbps = 8
	e.svc.SavePackage(ctx, p)
	e.svc.Wait()
	if !e.svc.ProfilesOK(e.site) || e.svc.ProfilesOK(b) {
		t.Fatal("only B must be dirty")
	}
	rtB.SetFail(nil)
	e.now = e.now.Add(time.Minute)
	e.svc.Sweep(ctx)
	if !e.svc.ProfilesOK(b) {
		t.Fatal("sweep re-syncs B")
	}
}

func TestSavePackageDoesNotBlockOnDeadRouter(t *testing.T) {
	e := newEnv(t)
	bID := store.NewSiteID()
	e.st.Update(func(d *store.Data) error {
		d.Sites[bID] = store.Site{ID: bID, Name: "B", Mode: store.SiteModeEnv}
		return nil
	})
	slow := &blocking{Mock: routeros.NewMock(), release: make(chan struct{}), entered: make(chan struct{})}
	e.svc = NewService(e.st, routers{e.site: e.rt, bID: &stuckProfiles{slow}}, quiet())
	p := e.pkg("1 Hour")
	p.SpeedMbps = 9
	start := time.Now()
	if _, err := e.svc.SavePackage(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("SavePackage waited for the router")
	}
	close(slow.release)
	e.svc.Wait()
}

type stuckProfiles struct{ *blocking }

func (s *stuckProfiles) EnsureProfile(ctx context.Context, p routeros.Profile) error {
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Mock.EnsureProfile(ctx, p)
}

// ---- packages and prices ----------------------------------------------------

func (e *env) setOverride(t *testing.T, site, pkg string, o store.PkgOverride) error {
	t.Helper()
	var cur map[string]store.PkgOverride
	e.st.View(func(d *store.Data) {
		cur = map[string]store.PkgOverride{}
		for k, v := range d.Sites[site].PkgOverrides {
			cur[k] = v
		}
	})
	cur[e.pkg(pkg).ID] = o
	return e.svc.SaveSiteOverrides(site, cur)
}

func TestHiddenPackagesAndPriceOverrides(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	e.svc.SyncProfiles(context.Background(), b)
	day := e.pkg("1 Day")
	if err := e.setOverride(t, b, "1 Day", store.PkgOverride{PriceKES: 80}); err != nil {
		t.Fatal(err)
	}
	if err := e.setOverride(t, b, "1 Week", store.PkgOverride{Hidden: true}); err != nil {
		t.Fatal(err)
	}
	a1 := e.flatAt(t, e.site, "Flat 1", mac1)
	b1 := e.flatAt(t, b, "Flat 1", macB)
	oa, err := e.svc.CreateOrder(OrderInput{AccountID: a1.ID, PackageID: day.ID, Method: "test"})
	if err != nil || oa.AmountKES != 50 {
		t.Fatalf("A pays the shared price: %+v %v", oa, err)
	}
	ob, err := e.svc.CreateOrder(OrderInput{AccountID: b1.ID, PackageID: day.ID, Method: "test"})
	if err != nil || ob.AmountKES != 80 {
		t.Fatalf("B pays its override: %+v %v", ob, err)
	}
	if _, err := e.svc.CreateOrder(OrderInput{AccountID: b1.ID, PackageID: e.pkg("1 Week").ID, Method: "test"}); err == nil {
		t.Fatal("hidden package must not be orderable")
	}
	if _, err := e.svc.CreateOrder(OrderInput{AccountID: a1.ID, PackageID: e.pkg("1 Week").ID, Method: "test"}); err != nil {
		t.Fatal("hidden at B only")
	}
	// Admin paths accept hidden packages and note the effective price.
	pay, err := e.svc.RecordManual(context.Background(), ManualInput{AccountID: b1.ID, TransCode: "ABCDE12345", AmountKES: 250, PackageID: e.pkg("1 Week").ID})
	if err != nil || pay.PackageID != e.pkg("1 Week").ID || pay.SiteID != b {
		t.Fatalf("%+v %v", pay, err)
	}
	pay, err = e.svc.RecordManual(context.Background(), ManualInput{AccountID: b1.ID, TransCode: "ABCDE12346", AmountKES: 50, PackageID: day.ID})
	if err != nil || !strings.Contains(pay.Note, "KES 80") {
		t.Fatalf("note should compare with the building price: %+v %v", pay, err)
	}
	if err := e.svc.ExtendAccount(context.Background(), b1.ID, e.pkg("1 Week").ID, "grant", false); err != nil {
		t.Fatal(err)
	}
	if got := e.get(a1); got.PackageID != "" {
		t.Fatal("A account must be untouched")
	}
	e.st.View(func(d *store.Data) {
		if d.Packages[day.ID].PriceKES != 50 {
			t.Fatal("override changed the shared package")
		}
	})
}

func TestPackageForAmountPerSite(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	if err := e.setOverride(t, b, "1 Day", store.PkgOverride{PriceKES: 80}); err != nil {
		t.Fatal(err)
	}
	day := e.pkg("1 Day")
	if p, ok := e.svc.PackageForAmount(e.site, 50); !ok || p.ID != day.ID {
		t.Fatal("A: 50 is a day")
	}
	if _, ok := e.svc.PackageForAmount(b, 50); ok {
		t.Fatal("B: 50 matches nothing")
	}
	if p, ok := e.svc.PackageForAmount(b, 80); !ok || p.ID != day.ID || p.PriceKES != 80 {
		t.Fatalf("B: 80 is a day: %+v", p)
	}
	if _, ok := e.svc.PackageForAmount(e.site, 80); ok {
		t.Fatal("A: 80 matches nothing")
	}
}

func TestPriceCollisionIsAnError(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	// Shared list: a new package with an existing price is refused.
	_, err := e.svc.SavePackage(context.Background(), store.Package{Name: "Clone", DurationMin: 60, SpeedMbps: 3, PriceKES: 50})
	if !errors.Is(err, ErrPriceCollision) {
		t.Fatalf("shared collision: %v", err)
	}
	// An override that collides at that building is refused...
	err = e.setOverride(t, b, "1 Week", store.PkgOverride{PriceKES: 50})
	if !errors.Is(err, ErrPriceCollision) || !strings.Contains(err.Error(), "Block B") {
		t.Fatalf("override collision: %v", err)
	}
	// ...unless the other package is hidden there.
	e.setOverride(t, b, "1 Day", store.PkgOverride{Hidden: true})
	if err := e.setOverride(t, b, "1 Week", store.PkgOverride{PriceKES: 50}); err != nil {
		t.Fatalf("hidden package does not collide: %v", err)
	}
	// Now a shared price change that would collide with B's override is refused too.
	day := e.pkg("1 Day")
	hour := e.pkg("1 Hour")
	hour.PriceKES = 250 // fine for A? A has 1 Week at 250 -> collision
	if _, err := e.svc.SavePackage(context.Background(), hour); !errors.Is(err, ErrPriceCollision) {
		t.Fatalf("%v", err)
	}
	_ = day
	// A refused save changes nothing in memory.
	if e.pkg("1 Hour").PriceKES != 10 {
		t.Fatal("a refused save must not modify the package")
	}
	// A disabled package does not collide.
	dup := store.Package{Name: "Spare", DurationMin: 60, SpeedMbps: 3, PriceKES: 50, Disabled: true}
	if _, err := e.svc.SavePackage(context.Background(), dup); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestVoucherSiteRestriction(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	e.svc.SyncProfiles(context.Background(), b)
	a1 := e.flatAt(t, e.site, "Flat 1", mac1)
	b1 := e.flatAt(t, b, "Flat 1", macB)
	day := e.pkg("1 Day")
	_, vs, err := e.svc.GenerateVouchers(day.ID, b, 2)
	if err != nil || vs[0].SiteID != b {
		t.Fatalf("%+v %v", vs, err)
	}
	redeem := func(a store.Account, code string) error {
		o, err := e.svc.CreateOrder(OrderInput{AccountID: a.ID, PackageID: day.ID, Method: "voucher", VoucherCode: code})
		if err != nil {
			return err
		}
		return e.svc.CompleteOrder(context.Background(), o.ID, "VOUCHER-"+code)
	}
	if err := redeem(a1, vs[0].Code); !errors.Is(err, ErrVoucherSite) {
		t.Fatalf("A must not redeem B's voucher: %v", err)
	}
	if v, _ := e.svc.LookupVoucher(vs[0].Code); v.Used {
		t.Fatal("the refused redemption used the voucher up")
	}
	if err := redeem(b1, vs[0].Code); err != nil {
		t.Fatal(err)
	}
	// "" = any building.
	_, anyV, err := e.svc.GenerateVouchers(day.ID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := redeem(a1, anyV[0].Code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.GenerateVouchers(day.ID, "nope", 1); !errors.Is(err, ErrNoSite) {
		t.Fatal(err)
	}
}

func TestDirectoryIsPerSite(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	e.flatAt(t, e.site, "Flat 1", mac1)
	e.flatAt(t, b, "Flat 7", "AA:BB:CC:DD:EE:07")
	if got := e.svc.Directory(b).Accounts(); len(got) != 1 || got[0].Flat != "Flat 7" {
		t.Fatalf("%+v", got)
	}
}
