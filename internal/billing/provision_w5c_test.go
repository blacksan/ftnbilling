package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func TestHoldSiteStopsTheReconcilerWriting(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	release := e.svc.HoldSite(e.site)
	e.rt.ResetWrites()
	if _, err := e.svc.ReconcileSite(context.Background(), e.site); !errors.Is(err, ErrSiteHeld) {
		t.Fatalf("want ErrSiteHeld, got %v", err)
	}
	if !e.svc.SiteHeld(e.site) || e.rt.Writes() != 0 {
		t.Fatalf("held=%v writes=%d", e.svc.SiteHeld(e.site), e.rt.Writes())
	}
	// the same holds for a hotspot building with the bypass flag on
	release()
	release() // idempotent
	if e.svc.SiteHeld(e.site) {
		t.Fatal("still held after release")
	}
	if _, err := e.svc.ReconcileSite(context.Background(), e.site); err != nil {
		t.Fatal(err)
	}
	if e.rt.Writes() == 0 {
		t.Fatal("after the release the reconciler must write the flat")
	}
}

func TestHotspotBypassRowsFollowTheFlagAndEntitlement(t *testing.T) {
	e := newEnv(t)
	paid := e.flat(t, "Flat 1", macA)
	e.flat(t, "Flat 2", macB2) // never paid: not entitled
	e.pay(t, paid, "1 Week", "10.5.50.30")
	bindings := func() []routeros.Row { return e.rt.Table("/ip/hotspot/ip-binding") }

	e.rt.ResetWrites()
	e.reconcile(t)
	if len(bindings()) != 0 || e.rt.Writes() != 0 {
		t.Fatalf("no flag, no rows: %v", bindings())
	}
	e.svc.SetHotspotBypass(e.site, true) // what the job does after a rollback
	e.reconcile(t)
	rows := bindings()
	if len(rows) != 1 || rows[0]["type"] != "bypassed" || rows[0]["mac-address"] != macA || rows[0]["comment"] != AcctComment(paid.Code, paid.Flat) {
		t.Fatalf("want one bypass row for the paying flat, got %v", rows)
	}
	if n := e.pass(t); n != 0 {
		t.Fatalf("a second pass wrote %d times", n)
	}
	// a foreign binding is never touched
	e.rt.SeedRow("/ip/hotspot/ip-binding", routeros.Row{"mac-address": "00:11:22:33:44:55", "type": "bypassed", "comment": "office printer"})
	// time runs out: the row goes
	e.now = e.now.Add(30 * 24 * time.Hour)
	e.reconcile(t)
	if rows := bindings(); len(rows) != 1 || rows[0]["comment"] != "office printer" {
		t.Fatalf("an expired flat must lose its bypass, the printer keeps its row: %v", rows)
	}
	// the flag off removes rows of entitled flats too
	e.pay(t, paid, "1 Week", "10.5.50.30")
	e.reconcile(t)
	if len(bindings()) != 2 {
		t.Fatalf("%v", bindings())
	}
	e.svc.SetHotspotBypass(e.site, false)
	e.reconcile(t)
	if rows := bindings(); len(rows) != 1 || rows[0]["comment"] != "office printer" {
		t.Fatalf("%v", rows)
	}
}

func TestBypassIsDroppedOnceTheSiteServesFromFtnDHCP(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.pay(t, a, "1 Week", "10.5.50.30")
	e.svc.SetHotspotBypass(e.site, true)
	e.reconcile(t)
	if len(e.rt.Table("/ip/hotspot/ip-binding")) != 1 {
		t.Fatal("setup")
	}
	e.ipoe(t, store.AccessIPoE)
	e.reconcile(t)
	if len(e.rt.Table("/ip/hotspot/ip-binding")) != 0 || e.svc.HotspotBypassOn(e.site) {
		t.Fatalf("rows %v flag %v", e.rt.Table("/ip/hotspot/ip-binding"), e.svc.HotspotBypassOn(e.site))
	}
}

func TestCreateCustomer(t *testing.T) {
	ctx := context.Background()
	t.Run("ok, with a package and its time", func(t *testing.T) {
		e := newEnv(t)
		e.ipoe(t, store.AccessIPoE)
		pk := e.pkg("1 Week")
		a, rs, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 4", Tenant: "Amina", MAC: macA, PackageID: pk.ID, Activate: true, Note: "cash"})
		if err != nil {
			t.Fatal(err)
		}
		if a.PackageID != pk.ID || a.IP == "" || !a.ExpiresAt.After(e.now) || rs.PendingTask {
			t.Fatalf("%+v %+v", a, rs)
		}
		if len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)) != 1 {
			t.Fatal("the lease must be on the router after the one synchronous attempt")
		}
	})
	t.Run("activate needs a package", func(t *testing.T) {
		e := newEnv(t)
		if _, _, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 4", MAC: macA, Activate: true}); err == nil {
			t.Fatal("expected an error")
		}
		if _, ok := e.svc.AccountByMAC(macA); ok {
			t.Fatal("nothing may be left behind")
		}
	})
	t.Run("mac in use names the owner", func(t *testing.T) {
		e := newEnv(t)
		e.ipoe(t, store.AccessIPoE)
		first := e.flat(t, "Flat 1", macA)
		_, _, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 2", MAC: macA})
		var dup *DuplicateMACError
		if !errors.As(err, &dup) || dup.Code != first.Code || dup.Flat != "Flat 1" || dup.SiteName != "Block A" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("queued when the router is offline", func(t *testing.T) {
		e := newEnv(t)
		e.ipoe(t, store.AccessIPoE)
		e.rt.SetFail(errors.New("tunnel down"))
		a, rs, err := e.svc.CreateCustomer(ctx, AddCustomerInput{SiteID: e.site, Flat: "Flat 5", MAC: macB2})
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == "" || !rs.PendingTask || rs.InSync {
			t.Fatalf("the account exists and its router work is queued: %+v", rs)
		}
	})
}

func TestSweepAndHotspotTaskDropExpiredBypassRows(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	b := e.flat(t, "Flat 2", macB2)
	e.pay(t, a, "1 Week", "10.5.50.30")
	e.pay(t, b, "Monthly 20 Mbps", "10.5.50.31")
	e.svc.SetHotspotBypass(e.site, true)
	e.reconcile(t)
	if n := len(e.rt.Table("/ip/hotspot/ip-binding")); n != 2 {
		t.Fatalf("%d bypass rows", n)
	}
	// the week ends: the sweep alone removes Flat 1's row
	e.now = e.now.Add(8 * 24 * time.Hour)
	e.svc.Sweep(context.Background())
	rows := e.rt.Table("/ip/hotspot/ip-binding")
	if len(rows) != 1 || rows[0]["mac-address"] != macB2 {
		t.Fatalf("%v", rows)
	}
	// a suspension handled by the per-account hotspot task takes the other row
	if err := e.svc.SetSuspended(context.Background(), b.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.reconcile(context.Background(), store.Task{SiteID: e.site, AccountID: b.ID, Code: b.Code}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.rt.Table("/ip/hotspot/ip-binding")); n != 0 {
		t.Fatalf("%d rows left", n)
	}
}
