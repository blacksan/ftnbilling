package payments_test

import (
	"context"
	"testing"
	"time"

	"ispbill/internal/store"
)

var eatZ = time.FixedZone("EAT", 3*3600)

func tts(t time.Time) string { return t.In(eatZ).Format("20060102150405") }

// Server clock 3 min AHEAD of Daraja: C2B first, then STK. No double credit, no Unmatched.
func TestR3_SkewServerAhead(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.svc.SetCheckoutID(o.ID, "ws_CO_S1")
	r.advance(30 * time.Second)
	d := tts(r.clock().Add(-3 * time.Minute))
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTSKW0001", r.acct.Code, "50.00", d))
	r.mp.HandleSTKCallback(ctx, stkCBAt("ws_CO_S1", "RKTSKW0001", 50, d))
	g, _ := r.svc.GetOrder(o.ID)
	t.Logf("order %s revenue %d unmatched %d", g.Status, r.revenue(), r.countStatus(store.PayUnmatched))
	if r.revenue() != 50 || g.Status != store.OrderPaid || r.countStatus(store.PayUnmatched) != 0 {
		t.Fail()
	}
}

// Same, STK result lost: money credited to the account, the order stays pending (live-equivalent).
func TestR3_SkewServerAheadSTKLost(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.svc.SetCheckoutID(o.ID, "ws_CO_S2")
	r.advance(30 * time.Second)
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTSKW0002", r.acct.Code, "50.00", tts(r.clock().Add(-3*time.Minute))))
	g, _ := r.svc.GetOrder(o.ID)
	t.Logf("order %s revenue %d", g.Status, r.revenue())
	if r.revenue() != 50 {
		t.Fail()
	}
}

// Server clock 3 min BEHIND Daraja: A paid, A's callbacks late, admin marks A paid 2 min later, then A's C2B.
func TestR3_SkewServerBehindMarkPaid(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	r.svc.SetCheckoutID(a.ID, "ws_CO_S3")
	r.advance(30 * time.Second)
	d := tts(r.clock().Add(3 * time.Minute))
	r.advance(2 * time.Minute)
	markPaid(t, r, a)
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTSKW0003", r.acct.Code, "50.00", d))
	p, _ := r.payment("RKTSKW0003")
	t.Logf("payment %s revenue %d", p.Status, r.revenue())
	if r.revenue() != 50 {
		t.Errorf("double credit: revenue %d", r.revenue())
	}
}

// Customer goes back and starts B 20 s after A, then pays A's prompt at +40 s. A's callbacks are late;
// admin marks A paid at +10 min; A's Paybill confirmation then arrives by account code.
func TestR3_PaidAfterRetryCreated(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	r.svc.SetCheckoutID(a.ID, "ws_CO_A3")
	r.advance(20 * time.Second)
	b := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b.ID, "ws_CO_B3")
	r.advance(20 * time.Second)
	d := tts(r.clock())
	r.advance(10 * time.Minute)
	markPaid(t, r, a)
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTRTY0003", r.acct.Code, "50.00", d))
	r.mp.HandleSTKCallback(ctx, stkCBAt("ws_CO_A3", "RKTRTY0003", 50, d))
	r.dumpOrders(t)
	if r.revenue() != 50 {
		t.Errorf("double credit: revenue %d", r.revenue())
	}
}

// No TransTime, Mark paid 40 min ago, A's C2B arrives: credited again (live-equivalent).
func TestR3_NoTransTimeOldMarkPaid(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	markPaid(t, r, a)
	r.advance(40 * time.Minute)
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTNTT0001", r.acct.Code, "50.00"))
	t.Logf("revenue %d", r.revenue())
}

// Held, alert once; redelivery and replay neither release nor re-alert.
func TestR3_HoldAlertOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	var alerts []string
	r.mp.SetAlert(func(m string) { alerts = append(alerts, m) })
	a := r.order(t, "1 Day")
	d := tts(r.clock())
	r.advance(5 * time.Minute)
	markPaid(t, r, a)
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTHA0001", r.acct.Code, "50.00", d))
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTHA0001", r.acct.Code, "50.00", d))
	r.advance(time.Minute)
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTHA0001", r.acct.Code, "50.00", d))
	p, _ := r.payment("RKTHA0001")
	t.Logf("status %s alerts %d revenue %d", p.Status, len(alerts), r.revenue())
	if p.Status != store.PayUnmatched || len(alerts) != 1 || r.revenue() != 50 {
		t.Fail()
	}
}

// Missing STK TransactionDate, unknown checkout, order created later than the payment can't be checked: matched by phone.
func TestR3_STKNoDateUnknownCheckout(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.HandleSTKCallback(ctx, stkCBAt("ws_CO_UNK", "RKTUNK0001", 50, ""))
	g, _ := r.svc.GetOrder(o.ID)
	t.Logf("order %s revenue %d", g.Status, r.revenue())
	if g.Status != store.OrderPaid || r.revenue() != 50 {
		t.Fail()
	}
}

// R3-2: when the hold check matches, the Paybill payment is held (and the owners
// told) instead of being handed to the single open order.
func TestR3_HoldWinsOverTheOpenOrderRedirect(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	var alerts []string
	r.mp.SetAlert(func(m string) { alerts = append(alerts, m) })
	a := r.order(t, "1 Day")
	r.svc.SetCheckoutID(a.ID, "ws_CO_A9")
	d := tts(r.clock())
	r.advance(time.Minute)
	b := r.order(t, "1 Day") // the retry, still open
	r.svc.SetCheckoutID(b.ID, "ws_CO_B9")
	r.advance(5 * time.Minute)
	markPaid(t, r, a)
	r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTR32001", r.acct.Code, "50.00", d))
	if g, _ := r.svc.GetOrder(b.ID); g.Status == store.OrderPaid {
		t.Fatal("the open retry order was closed with the Mark-paid order's money")
	}
	if p, _ := r.payment("RKTR32001"); p.Status != store.PayUnmatched || len(alerts) != 1 || r.revenue() != 50 {
		t.Fatalf("%+v alerts %v revenue %d", p, alerts, r.revenue())
	}
}

// R3-1: money paid up to 5 minutes after the Mark paid (clock skew) is still held; later money is not.
func TestR3_SkewMarginIsFiveMinutes(t *testing.T) {
	for _, c := range []struct {
		after time.Duration
		held  bool
	}{{4 * time.Minute, true}, {6 * time.Minute, false}} {
		r := newRig(t)
		ctx := context.Background()
		a := r.order(t, "1 Day")
		markPaid(t, r, a)
		d := tts(r.clock().Add(c.after))
		r.advance(c.after)
		r.mp.HandleC2BConfirmation(ctx, c2bAt("RKTSKM0001", r.acct.Code, "50.00", d))
		if p, _ := r.payment("RKTSKM0001"); (p.Status == store.PayUnmatched) != c.held {
			t.Errorf("paid %v after the Mark paid: status %s, want held=%v", c.after, p.Status, c.held)
		}
	}
}
