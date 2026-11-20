package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"ispbill/internal/store"
)

// A lower plan assigned from Unmatched is refused unless the admin chooses.
func TestAssignPaymentModeLowerPlan(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	ctx := context.Background()
	big, small := e.pkg("Monthly 80 Mbps"), e.pkg("1 Day")
	if _, _, err := e.svc.Credit(ctx, CreditInput{Key: "BIG0000001", Method: "mpesa-c2b", AccountID: a.ID, PackageID: big.ID, AmountKES: big.PriceKES}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"LOW0000001", "LOW0000002", "LOW0000003"} {
		e.svc.RecordUnmatched(UnmatchedInput{Key: k, Method: "mpesa-c2b", AmountKES: small.PriceKES, AccountID: a.ID})
	}
	start := e.get(a)
	if err := e.svc.AssignPayment(ctx, "LOW0000001", a.ID, small.ID, ""); !errors.Is(err, ErrLowerPlan) {
		t.Fatalf("no choice: %v", err)
	}
	if got := e.get(a); got.PackageID != big.ID || !got.ExpiresAt.Equal(start.ExpiresAt) {
		t.Fatal("the refusal changed the account")
	}
	if err := e.svc.AssignPaymentMode(ctx, "LOW0000001", a.ID, small.ID, "", "bogus"); !errors.Is(err, ErrLowerPlan) {
		t.Fatalf("an unknown choice must not apply: %v", err)
	}
	if err := e.svc.AssignPaymentMode(ctx, "LOW0000001", a.ID, small.ID, "", AssignAddTime); err != nil {
		t.Fatal(err)
	}
	got := e.get(a)
	if got.PackageID != big.ID || got.ExpiresAt.Sub(start.ExpiresAt) != 24*time.Hour {
		t.Fatalf("add time: %s, +%v", got.PackageID, got.ExpiresAt.Sub(start.ExpiresAt))
	}
	if err := e.svc.AssignPaymentMode(ctx, "LOW0000002", a.ID, small.ID, "", AssignSwitchPlan); err != nil {
		t.Fatal(err)
	}
	if got := e.get(a); got.PackageID != small.ID {
		t.Fatalf("switch: %s", got.PackageID)
	}
	// A plan that is not lower needs no choice.
	if err := e.svc.AssignPayment(ctx, "LOW0000003", a.ID, big.ID, ""); err != nil {
		t.Fatalf("an upgrade: %v", err)
	}
}

// A receipt that was refunded or ignored is never credited by a later callback.
func TestCreditRefusesAReceiptThatWasDecided(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	ctx := context.Background()
	day := e.pkg("1 Day")
	e.svc.RecordUnmatched(UnmatchedInput{Key: "REF0000001", Method: "mpesa-c2b", AmountKES: 50})
	if err := e.svc.ResolvePayment("REF0000001", store.PayRefunded, ""); err != nil {
		t.Fatal(err)
	}
	if _, created, err := e.svc.Credit(ctx, CreditInput{Key: "REF0000001", Method: "mpesa-stk", AccountID: a.ID, PackageID: day.ID, AmountKES: 50}); created || !errors.Is(err, ErrBadState) {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !e.get(a).ExpiresAt.IsZero() {
		t.Fatal("credited")
	}
}

// Pending orders end as expired (not failed) after the wait, and the store takes the new status.
func TestExpireOrders(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 3B", mac1)
	o, err := e.svc.CreateOrder(OrderInput{AccountID: a.ID, PackageID: e.pkg("1 Day").ID, Phone: "254712345678", Method: "mpesa"})
	if err != nil {
		t.Fatal(err)
	}
	paid, _ := e.svc.CreateOrder(OrderInput{AccountID: a.ID, PackageID: e.pkg("1 Day").ID, Method: "test"})
	if err := e.svc.CompleteOrder(context.Background(), paid.ID, "TEST-"+paid.ID); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(OrderTTL - time.Minute)
	if n, _ := e.svc.ExpireOrders(); n != 0 {
		t.Fatalf("expired too early: %d", n)
	}
	e.now = e.now.Add(2 * time.Minute)
	if n, err := e.svc.ExpireOrders(); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if g, _ := e.svc.GetOrder(o.ID); g.Status != store.OrderExpired || g.Reason == "" {
		t.Fatalf("%+v", g)
	}
	if g, _ := e.svc.GetOrder(paid.ID); g.Status != store.OrderPaid {
		t.Fatalf("a paid order must stay paid: %s", g.Status)
	}
	if err := e.svc.FailOrder(context.Background(), o.ID, "late failure"); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.svc.GetOrder(o.ID); g.Status != store.OrderExpired {
		t.Fatal("a failure must not overwrite expired")
	}
	if n, _ := e.svc.ExpireOrders(); n != 0 {
		t.Fatal("expiry must be idempotent")
	}
	// An expired order is completed by a late payment.
	if err := e.svc.CompleteOrder(context.Background(), o.ID, "RKTLATE002"); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.svc.GetOrder(o.ID); g.Status != store.OrderPaid {
		t.Fatal(g.Status)
	}
}
