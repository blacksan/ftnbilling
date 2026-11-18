package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestChangePackageKeepAndFresh(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 1A", mac1)
	e.pay(t, a, "Monthly 20 Mbps", "")
	exp := e.get(a).ExpiresAt

	m40 := e.pkg("Monthly 40 Mbps")
	if err := e.svc.ChangePackage(ctx, a.ID, m40.ID, true); err != nil {
		t.Fatal(err)
	}
	cur := e.get(a)
	if cur.PackageID != m40.ID || !cur.ExpiresAt.Equal(exp) {
		t.Fatalf("keep: got pkg %s exp %v", cur.PackageID, cur.ExpiresAt)
	}
	if u, ok := e.rt.GetUser(a.Code); !ok || u.Profile != m40.Profile() {
		t.Fatalf("router profile not updated: %+v", u)
	}

	day := e.pkg("1 Day")
	if err := e.svc.ChangePackage(ctx, a.ID, day.ID, false); err != nil {
		t.Fatal(err)
	}
	cur = e.get(a)
	if !cur.ExpiresAt.Equal(e.now.Add(24 * time.Hour)) {
		t.Fatalf("fresh: exp %v", cur.ExpiresAt)
	}

	if err := e.svc.EndPlan(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.rt.GetUser(a.Code); ok {
		t.Fatal("router user should be removed after EndPlan")
	}
	if err := e.svc.ChangePackage(ctx, a.ID, m40.ID, true); err == nil {
		t.Fatal("keep with no time left should fail")
	}
}

func TestDeleteAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 2B", mac1)
	e.pay(t, a, "1 Day", "")
	if err := e.svc.DeleteAccount(ctx, a.ID, "WRONG"); err == nil {
		t.Fatal("wrong confirm accepted")
	}
	if err := e.svc.DeleteAccount(ctx, a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.svc.GetAccount(a.ID); ok {
		t.Fatal("account still there")
	}
	if _, ok := e.rt.GetUser(a.Code); ok {
		t.Fatal("router user still there")
	}
	n := 0
	e.st.View(func(d *store.Data) { n = len(d.Payments) })
	if n != 1 {
		t.Fatalf("payments kept = %d", n)
	}
	// MAC is free again
	e.flat(t, "Flat 2B", mac1)
}

func TestDeleteAccountRouterDownRetries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 3C", mac1)
	e.pay(t, a, "1 Day", "")
	e.rt.SetFail(errors.New("router down"))
	if err := e.svc.DeleteAccount(ctx, a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	e.rt.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Sweep(ctx)
	if _, ok := e.rt.GetUser(a.Code); ok {
		t.Fatal("router user not cleaned up after retry")
	}
}

func TestLatePaymentForDeletedAccountGoesUnmatched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 4D", mac1)
	o, err := e.svc.CreateOrder(OrderInput{AccountID: a.ID, PackageID: e.pkg("1 Day").ID, Method: "mpesa", Phone: "254712345678"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteAccount(ctx, a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteOrder(ctx, o.ID, "SGL7K2M9XA"); err != nil {
		t.Fatal(err)
	}
	var p store.Payment
	e.st.View(func(d *store.Data) { p = d.Payments["SGL7K2M9XA"] })
	if p.Status != store.PayUnmatched {
		t.Fatalf("late payment status = %q", p.Status)
	}
}

func TestDeletePackageRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 5E", mac1)
	e.pay(t, a, "1 Week", "")
	wk := e.pkg("1 Week")
	if err := e.svc.DeletePackage(ctx, wk.ID); err == nil {
		t.Fatal("deleted a package in use")
	}
	hr := e.pkg("1 Hour")
	if _, _, err := e.svc.GenerateVouchers(hr.ID, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePackage(ctx, hr.ID); err == nil {
		t.Fatal("deleted a package with unused vouchers")
	}
	m80 := e.pkg("Monthly 80 Mbps")
	if err := e.svc.DeletePackage(ctx, m80.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if _, ok := e.rt.GetProfile(m80.Profile()); ok {
		t.Fatal("router profile not removed")
	}
	var listed bool
	e.st.View(func(d *store.Data) {
		for _, p := range d.PackageList() {
			listed = listed || p.ID == m80.ID
		}
	})
	if listed {
		t.Fatal("deleted package still listed")
	}
	if err := e.svc.ExtendAccount(ctx, a.ID, m80.ID, "", true); err == nil {
		t.Fatal("granted a deleted package")
	}
}
