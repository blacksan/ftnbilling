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

// blocking is a router whose Rows call hangs until released.
type blocking struct {
	*routeros.Mock
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blocking) Rows(ctx context.Context, path string, q map[string]string) ([]routeros.Row, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.Mock.Rows(ctx, path, q)
}

type routers map[string]routeros.Client

func (r routers) Router(id string) routeros.Client {
	if c, ok := r[id]; ok {
		return c
	}
	return routeros.NoRouter()
}

func TestSavePackageDoesNotBlockOnDeadRouter(t *testing.T) {
	e := newEnv(t)
	bID := store.NewSiteID()
	e.st.Update(func(d *store.Data) error {
		d.Sites[bID] = store.Site{ID: bID, Name: "B", Mode: store.SiteModeWG, WGPublicKey: "PUBB", TunnelIP: "10.99.0.9", AccessMode: store.AccessIPoE}
		return nil
	})
	slow := &blocking{Mock: routeros.NewMock(), release: make(chan struct{}), entered: make(chan struct{})}
	e.svc = NewService(e.st, routers{e.site: e.rt, bID: slow}, quiet())
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
