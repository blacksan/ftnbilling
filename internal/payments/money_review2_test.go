package payments_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/payments"
	"ispbill/internal/store"
)

func (r *rig) dumpOrders(t *testing.T) {
	r.st.View(func(d *store.Data) {
		for _, o := range d.Orders {
			t.Logf("order %s status=%s receipt=%s reason=%q", o.ID, o.Status, o.Receipt, o.Reason)
		}
		for _, p := range d.Payments {
			t.Logf("payment %s status=%s order=%s note=%q", p.Key, p.Status, p.OrderID, p.Note)
		}
	})
}

// Two legit STK payments, same flat and amount, Paybill confirmations first.
func TestRV2_TwoOrdersC2BFirst(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	start := r.expiry()
	b1 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b1.ID, "ws_CO_B1")
	b2 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b2.ID, "ws_CO_B2")
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTTWO0001", r.acct.Code, "50.00"))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTTWO0002", r.acct.Code, "50.00"))
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B1", 0, "RKTTWO0001", 50))
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B2", 0, "RKTTWO0002", 50))
	r.dumpOrders(t)
	_ = start
	if r.revenue() != 100 || r.countStatus(store.PayUnmatched) != 0 {
		t.Fatalf("revenue %d unmatched %d", r.revenue(), r.countStatus(store.PayUnmatched))
	}
	for _, o := range []store.Order{b1, b2} {
		if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderPaid {
			t.Errorf("order %s %s", o.ID, g.Status)
		}
	}
}

// Same, interleaved: STK result of B1 first, then its Paybill confirmation, then B2's pair.
func TestRV2_TwoOrdersSTKFirstInterleaved(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	b1 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b1.ID, "ws_CO_B1")
	b2 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b2.ID, "ws_CO_B2")
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B1", 0, "RKTINT0001", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTINT0001", r.acct.Code, "50.00"))
	if g, _ := r.svc.GetOrder(b2.ID); g.Status == store.OrderPaid {
		t.Errorf("order B2 marked paid by B1's receipt: %+v", g)
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B2", 0, "RKTINT0002", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTINT0002", r.acct.Code, "50.00"))
	r.dumpOrders(t)
	if r.revenue() != 100 || r.countStatus(store.PayUnmatched) != 0 {
		t.Fatalf("revenue %d unmatched %d (second real payment not credited)", r.revenue(), r.countStatus(store.PayUnmatched))
	}
}

// A cancelled second prompt (failed order) is closed with the first order's receipt.
func TestRV2_CancelledOrderClosedWithOtherReceipt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	b1 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b1.ID, "ws_CO_B1")
	b2 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b2.ID, "ws_CO_B2")
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B2", 1, "", 0))
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B1", 0, "RKTCAN0001", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTCAN0001", r.acct.Code, "50.00"))
	r.dumpOrders(t)
	if g, _ := r.svc.GetOrder(b2.ID); g.Status == store.OrderPaid {
		t.Errorf("cancelled order B2 marked paid with B1's receipt %s", g.Receipt)
	}
}

// Plain Paybill, no order: credits directly.
func TestRV2_PlainPaybillNoOrderCredits(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	first := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTPLN0001", r.acct.Code, "50.00"))
	if !r.expiry().After(first) || r.revenue() != 50 {
		t.Fatalf("not credited: revenue %d", r.revenue())
	}
}

// Held as a possible duplicate, then Daraja redelivers the same confirmation.
func TestRV2_HeldThenRedeliveredC2B(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	first := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTRED0001", r.acct.Code, "50.00"))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTRED0001", r.acct.Code, "50.00")) // redelivery
	r.dumpOrders(t)
	if r.expiry() != first || r.revenue() != 50 {
		t.Fatalf("held payment credited by a redelivery: expiry moved %v, revenue %d", r.expiry().Sub(first), r.revenue())
	}
}

// A Paybill payment made hours after the Mark paid (TransTime after the manual row) is new money: credited at once.
func TestRV2_NewMoneyHoursAfterMarkPaidCreditsAtOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	r.advance(3 * time.Hour)
	tt := r.clock().In(time.FixedZone("EAT", 3*3600)).Format("20060102150405")
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTNEW0001", r.acct.Code, "50.00", tt))
	p, _ := r.payment("RKTNEW0001")
	t.Logf("payment made 3 h after Mark paid: status=%s note=%q", p.Status, p.Note)
	if p.Status != store.PayApplied {
		t.Errorf("new money (TransTime after the Mark paid) held in Unmatched")
	}
}

// Order A's callbacks are lost; the customer taps "Try again" (order B, never paid); the admin marks A paid;
// then A's real Paybill confirmation (account code) arrives. It is A's money, but B is the single open order.
func TestRV2_RetryOrderThenMarkPaidThenC2B(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	r.svc.SetCheckoutID(a.ID, "ws_CO_A")
	start := r.expiry()
	r.advance(2 * time.Minute)
	b := r.order(t, "1 Day") // the retry, abandoned
	r.svc.SetCheckoutID(b.ID, "ws_CO_B")
	r.advance(5 * time.Minute)
	markPaid(t, r, a)
	// the customer paid A at 21:30:00 EAT-equivalent of the rig clock start
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTRTY0001", r.acct.Code, "50.00", "20261118003005"))
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_A", 0, "RKTRTY0001", 50))
	r.dumpOrders(t)
	_ = start
	if r.revenue() != 50 {
		t.Fatalf("one KES 50 payment credited as %d (double credit)", r.revenue())
	}
}

// Same, but the STK result of A arrives before its Paybill confirmation.
func TestRV2_RetryOrderThenMarkPaidThenSTKThenC2B(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	r.svc.SetCheckoutID(a.ID, "ws_CO_A")
	r.advance(2 * time.Minute)
	b := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b.ID, "ws_CO_B")
	r.advance(5 * time.Minute)
	markPaid(t, r, a)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_A", 0, "RKTRTY0002", 50))
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTRTY0002", r.acct.Code, "50.00", "20261118003005"))
	r.dumpOrders(t)
	if r.revenue() != 50 {
		t.Fatalf("one KES 50 payment credited as %d (double credit)", r.revenue())
	}
}

// N4: a payment made BEFORE the Mark paid (TransTime known) is held and the owners are told.
func TestRV2_PaymentBeforeMarkPaidIsHeldWithAnAlert(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	var alerts []string
	r.mp.SetAlert(func(m string) { alerts = append(alerts, m) })
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	paidAt := r.clock().In(time.FixedZone("EAT", 3*3600)).Format("20060102150405")
	r.advance(5 * time.Minute)
	markPaid(t, r, o)
	first := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTHLD0001", r.acct.Code, "50.00", paidAt))
	if p, _ := r.payment("RKTHLD0001"); p.Status != store.PayUnmatched || r.expiry() != first {
		t.Fatalf("%+v", p)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "RKTHLD0001") {
		t.Fatalf("alerts %v", alerts)
	}
}

// N4: a Mark paid older than 6 h holds nothing.
func TestRV2_MarkPaidOlderThanSixHoursHoldsNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	markPaid(t, r, o)
	first := r.expiry()
	r.advance(7 * time.Hour)
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTOLD0001", r.acct.Code, "50.00"))
	if r.expiry() == first || r.revenue() != 100 {
		t.Fatal("not credited")
	}
}

// N1: an order created after the payment is never its order, even when the
// receipt is not yet in the ledger and no TransTime is known (no redirect then).
func TestRV2_PaybillWithoutTransTimeDoesNotPickAnOrder(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	b := r.order(t, "1 Day")
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTNOT0001", r.acct.Code, "50.00"))
	if g, _ := r.svc.GetOrder(b.ID); g.Status == store.OrderPaid {
		t.Fatal("an order was picked without a payment time to prove it")
	}
	if r.revenue() != 50 {
		t.Fatalf("revenue %d", r.revenue())
	}
}

// N3: B2's own payment, STK only (its Paybill confirmation lost), still credits.
func TestRV2_SecondOrderPaidBySTKOnlyCredits(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	b1 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b1.ID, "ws_CO_B1")
	b2 := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b2.ID, "ws_CO_B2")
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B1", 0, "RKTN30001", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTN30001", r.acct.Code, "50.00"))
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B2", 0, "RKTN30002", 50))
	if g, _ := r.svc.GetOrder(b2.ID); g.Status != store.OrderPaid || r.revenue() != 100 || r.countStatus(store.PayUnmatched) != 0 {
		t.Fatalf("%s revenue %d", g.Status, r.revenue())
	}
}

// Nit: "mark ignored" on the duplicate page, then A's STK result later: no false alert.
func TestRV2_IgnoredAsDuplicateThenSTKRaisesNoAlert(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	var alerts []string
	r.mp.SetAlert(func(m string) { alerts = append(alerts, m) })
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTIGN0001", r.acct.Code, "50.00")) // held
	alerts = nil
	if err := r.svc.ResolvePayment("RKTIGN0001", store.PayIgnored, billing.ManualDupNote+" (MANUAL-X); not credited twice"); err != nil {
		t.Fatal(err)
	}
	first := r.expiry()
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTIGN0001", 50))
	if len(alerts) != 0 || r.expiry() != first || r.revenue() != 50 {
		t.Fatalf("alerts %v revenue %d", alerts, r.revenue())
	}
}

func TestRV2_LegacyFallbackIsBuildTimePlusThirtyDays(t *testing.T) {
	built := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	defer payments.SetLegacyBuildTime(built)()
	dir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(dir, []byte("x"), 0o600)
	got := payments.LegacyQueryDeadline(dir, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil)
	if !got.Equal(built.Add(payments.LegacyQueryWindow)) {
		t.Fatal(got)
	}
}
