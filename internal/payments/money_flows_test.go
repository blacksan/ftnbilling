package payments_test

// Regression tests for the money-flow review (flow group 3): payments that
// used to vanish, double-credit or land on the wrong day.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/payments"
	"ispbill/internal/store"
)

func (r *rig) setNow(t time.Time) { r.cmu.Lock(); r.now = t; r.cmu.Unlock() }

func (r *rig) ledger() (rows []store.Payment) {
	r.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			rows = append(rows, p)
		}
	})
	return
}

func (r *rig) revenue() (kes int) {
	for _, p := range r.ledger() {
		if billing.CountsAsRevenue(p) {
			kes += p.AmountKES
		}
	}
	return
}

func (r *rig) expectExpiry(t *testing.T, want time.Time, why string) {
	t.Helper()
	if got := r.expiry(); !got.Equal(want) {
		t.Fatalf("%s: expiry %v, want %v", why, got, want)
	}
}

// stkCBAt is stkCB with a chosen TransactionDate (EAT, yyyymmddhhmmss) and phone.
func stkCBAt(checkout, receipt string, amount int, date string) []byte {
	items := []map[string]any{{"Name": "Amount", "Value": amount}, {"Name": "MpesaReceiptNumber", "Value": receipt},
		{"Name": "PhoneNumber", "Value": 254712345678}}
	if date != "" {
		items = append(items, map[string]any{"Name": "TransactionDate", "Value": json.Number(date)})
	}
	b, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"MerchantRequestID": "m1", "CheckoutRequestID": checkout, "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": items}}}})
	return b
}

func c2bAt(trans, ref, amount, transTime string) []byte {
	b, _ := json.Marshal(map[string]any{"TransactionType": "Pay Bill", "TransID": trans, "TransAmount": amount,
		"BillRefNumber": ref, "MSISDN": "254712345678", "TransTime": transTime})
	return b
}

// ---- H1: a successful STK result is never dropped ---------------------------------

func TestSTKSuccessForUnknownCheckoutIsRecordedAsUnmatched(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_NEVER_STORED", 0, "RKTLOST001", 50)); err != nil {
		t.Fatal(err)
	}
	p, ok := r.payment("RKTLOST001")
	if !ok {
		t.Fatal("KES 50 was paid and acknowledged, but the ledger is empty")
	}
	if p.Status != store.PayUnmatched || p.AmountKES != 50 || p.Phone != "254712345678" || p.Method != "mpesa-stk" ||
		!strings.Contains(p.Note, "never stored") {
		t.Fatalf("%+v", p)
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_NEVER_STORED", 0, "RKTLOST001", 50)) // Daraja redelivers
	if n := len(r.ledger()); n != 1 {
		t.Fatalf("ledger rows %d after a redelivery", n)
	}
}

func TestSTKUnknownCheckoutMatchesTheOnlyRecentOrderByPhoneAndAmount(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day") // phone 254712345678, KES 50, no checkout id stored
	if err := r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_FIRST", 0, "RKTMATCH01", 50)); err != nil {
		t.Fatal(err)
	}
	got, _ := r.svc.GetOrder(o.ID)
	if got.Status != store.OrderPaid || got.CheckoutRequestID != "ws_CO_FIRST" || got.Receipt != "RKTMATCH01" {
		t.Fatalf("order %+v", got)
	}
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "credited once")
	if r.countStatus(store.PayUnmatched) != 0 {
		t.Fatal("a matched payment must not also wait in Unmatched")
	}
}

func TestSTKUnknownCheckoutWithTwoCandidateOrdersGoesToUnmatched(t *testing.T) {
	r := newRig(t)
	r.order(t, "1 Day")
	r.order(t, "1 Day")
	r.mp.HandleSTKCallback(context.Background(), stkCB("ws_CO_AMBIG", 0, "RKTAMBIG01", 50))
	p, ok := r.payment("RKTAMBIG01")
	if !ok || p.Status != store.PayUnmatched || !strings.Contains(p.Note, "2 recent orders") {
		t.Fatalf("%+v %v", p, ok)
	}
	if !r.expiry().IsZero() {
		t.Fatal("an ambiguous payment must not credit either flat")
	}
}

func TestSTKSuccessAfterFailureStillCredits(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 1, "", 0)) // a timeout result first
	if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderFailed {
		t.Fatal(g.Status)
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTOOO0001", 50))
	if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderPaid {
		t.Fatalf("a late success after a failure was not honoured: %s", g.Status)
	}
	exp := r.expiry()
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 1, "", 0)) // a failure after a success changes nothing
	if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderPaid || !r.expiry().Equal(exp) {
		t.Fatal("a failure after a success changed state")
	}
}

// fakeSTKServer answers the push after a hook ran (cancel the caller, sleep...).
func fakeSTKServer(t *testing.T, hook func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/oauth/v1/generate":
			w.Write([]byte(`{"access_token":"TOK","expires_in":"3599"}`))
		case "/mpesa/stkpush/v1/processrequest":
			io.ReadAll(q.Body)
			hook()
			w.Write([]byte(`{"MerchantRequestID":"m1","CheckoutRequestID":"ws_CO_LOST","ResponseCode":"0","ResponseDescription":"ok"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newMpesaAt(r *rig, url string) *payments.Mpesa {
	mp := payments.NewMpesa(payments.MpesaConfig{BaseURL: url, Key: "key", Secret: "secret", Shortcode: "174379",
		Passkey: "P", PublicBaseURL: "https://example.org", Enabled: true}, r.svc, r.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mp.SetClock(r.clock)
	return mp
}

func TestSTKPushSurvivesTheBrowserRequestEnding(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var cancel context.CancelFunc
	srv := fakeSTKServer(t, func() {
		mu.Lock()
		c := cancel
		mu.Unlock()
		c() // the payer's browser went away while Daraja was answering
		time.Sleep(50 * time.Millisecond)
	})
	mp := newMpesaAt(r, srv.URL)
	o := r.order(t, "1 Day")
	ctx, c := context.WithCancel(context.Background())
	mu.Lock()
	cancel = c
	mu.Unlock()
	if err := mp.Start(ctx, o); err != nil {
		t.Fatalf("Start must not die with the browser request: %v", err)
	}
	if got, _ := r.svc.GetOrder(o.ID); got.CheckoutRequestID != "ws_CO_LOST" || got.Status != store.OrderPending {
		t.Fatalf("%+v", got)
	}
	mp.HandleSTKCallback(context.Background(), stkCB("ws_CO_LOST", 0, "RKTLOST002", 50))
	if got, _ := r.svc.GetOrder(o.ID); got.Status != store.OrderPaid {
		t.Fatalf("paid KES 50 but the order is %s", got.Status)
	}
}

func TestSTKPushTimeoutLeavesTheOrderPendingAndTheCallbackSettlesIt(t *testing.T) {
	r := newRig(t)
	srv := fakeSTKServer(t, func() { time.Sleep(400 * time.Millisecond) })
	mp := newMpesaAt(r, srv.URL)
	mp.SetPushTimeout(60 * time.Millisecond)
	o := r.order(t, "1 Day")
	err := mp.Start(context.Background(), o)
	if !errors.Is(err, payments.ErrPushUncertain) {
		t.Fatalf("a push that timed out after Daraja may have accepted it must be uncertain, got %v", err)
	}
	if got, _ := r.svc.GetOrder(o.ID); got.Status != store.OrderPending {
		t.Fatalf("order %s", got.Status)
	}
	// The customer got the prompt anyway and paid.
	mp.HandleSTKCallback(context.Background(), stkCB("ws_CO_LOST", 0, "RKTTMO0001", 50))
	if got, _ := r.svc.GetOrder(o.ID); got.Status != store.OrderPaid || got.CheckoutRequestID != "ws_CO_LOST" {
		t.Fatalf("%+v", got)
	}
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "credited once")
}

func TestSTKPushRejectedByDarajaIsAHardFailure(t *testing.T) {
	r := newRig(t)
	r.fd.respCode = "1"
	err := r.mp.Start(context.Background(), r.order(t, "1 Day"))
	if err == nil || errors.Is(err, payments.ErrPushUncertain) {
		t.Fatalf("a refused push is a failure, not uncertain: %v", err)
	}
}

// ---- M2/M3: one receipt, one credit -------------------------------------------------

func TestC2BUnmatchedThenSTKCreditsTheAccountOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day") // KES 50
	r.mp.Start(ctx, o)
	r.order(t, "1 Day") // a second open order: the Paybill confirmation cannot tell which one it is for
	p := r.pkg("1 Day") // admin re-prices before the confirmation lands
	p.PriceKES = 60
	if _, err := r.svc.SavePackage(ctx, p); err != nil {
		t.Fatal(err)
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("RKT9ABCDEF", r.acct.Code, "50.00"))
	if r.countStatus(store.PayUnmatched) != 1 || !r.expiry().IsZero() {
		t.Fatal("setup: the C2B confirmation should wait in Unmatched")
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKT9ABCDEF", 50))
	got, _ := r.svc.GetOrder(o.ID)
	if got.Status != store.OrderPaid {
		t.Fatal(got.Status)
	}
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "the unmatched row is applied")
	row, _ := r.payment("RKT9ABCDEF")
	if row.Status != store.PayApplied || row.AccountID != r.acct.ID || row.OrderID != o.ID || len(r.ledger()) != 1 {
		t.Fatalf("%+v rows=%d", row, len(r.ledger()))
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKT9ABCDEF", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKT9ABCDEF", r.acct.Code, "60.00"))
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "redeliveries change nothing")
}

func TestSTKThenC2BForTheSameReceiptCreditsOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTBOTH001", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTBOTH001", r.acct.Code, "50.00"))
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "credited once")
	if n := len(r.ledger()); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

func TestRefundedReceiptIsNotCreditedByALaterCallback(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTREF0001", "NOSUCH", "50.00")) // unknown account number: Unmatched
	if err := r.svc.ResolvePayment("RKTREF0001", store.PayRefunded, "sent back"); err != nil {
		t.Fatal(err)
	}
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTREF0001", 50))
	if !r.expiry().IsZero() {
		t.Fatal("a refunded receipt credited an account")
	}
	if g, _ := r.svc.GetOrder(o.ID); g.Status == store.OrderPaid {
		t.Fatal("the order was marked paid without a credit")
	}
}

func markPaid(t *testing.T, r *rig, o store.Order) {
	t.Helper()
	if err := r.svc.CompleteOrder(context.Background(), o.ID, billing.ManualPrefix+strings.ToUpper(o.ID)); err != nil { // what the admin "Mark paid" does
		t.Fatal(err)
	}
}

func TestMarkPaidThenTheRealSTKResultIsLinkedNotCreditedTwice(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	first := r.expiry()
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTREAL001", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTREAL001", r.acct.Code, "50.00"))
	r.expectExpiry(t, first, "one KES 50 payment gave more than one day")
	r.wantLinked(t, o, "RKTREAL001")
}

// wantLinked: the manual row is the one applied payment, the real receipt sits
// beside it as an ignored duplicate, and the money counts once.
func (r *rig) wantLinked(t *testing.T, o store.Order, receipt string) {
	t.Helper()
	manual, _ := r.payment(billing.ManualPrefix + strings.ToUpper(o.ID))
	real, ok := r.payment(receipt)
	if len(r.ledger()) != 2 || manual.Status != store.PayApplied || !ok || real.Status != store.PayIgnored || r.revenue() != 50 {
		t.Fatalf("rows=%+v revenue=%d", r.ledger(), r.revenue())
	}
	if !strings.Contains(real.Note, "duplicate") || !strings.Contains(manual.Note, receipt) {
		t.Fatalf("the two rows should point at each other:\n%q\n%q", manual.Note, real.Note)
	}
}

func TestMarkPaidThenThePaybillConfirmationWaitsForAPersonThenLinksToTheOrder(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	first := r.expiry()
	// The confirmation carries the account code, not the order: it may be the same money or a new payment.
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTREAL002", r.acct.Code, "50.00"))
	r.expectExpiry(t, first, "no second credit without a person")
	p, ok := r.payment("RKTREAL002")
	if !ok || p.Status != store.PayUnmatched || !strings.Contains(p.Note, "possible duplicate of manual payment MANUAL-") {
		t.Fatalf("must be visible in Unmatched, never silently ignored: %+v", p)
	}
	// The STK result of the same order proves it is the confirmation of the manual payment.
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTREAL002", 50))
	r.expectExpiry(t, first, "double credit")
	r.wantLinked(t, o, "RKTREAL002")
}

// The review's probe: Mark paid on order A; the same flat buys again (order B,
// same amount); the Paybill confirmation for B arrives before the STK result.
func TestMarkPaidOnAThenPaybillFirstForBCreditsB(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	r.mp.Start(ctx, a)
	markPaid(t, r, a)
	afterA := r.expiry()
	b := r.order(t, "1 Day")
	if err := r.svc.SetCheckoutID(b.ID, "ws_CO_B"); err != nil {
		t.Fatal(err)
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTBBBB001", r.acct.Code, "50.00"))
	if g, _ := r.svc.GetOrder(b.ID); g.Status != store.OrderPaid {
		t.Fatalf("order B is %s after the Paybill confirmation: B's money was swallowed", g.Status)
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B", 0, "RKTBBBB001", 50))
	r.expectExpiry(t, afterA.Add(24*time.Hour), "B adds its own day")
	if r.revenue() != 100 || r.countStatus(store.PayUnmatched) != 0 {
		t.Fatalf("revenue %d, unmatched %d, rows %+v", r.revenue(), r.countStatus(store.PayUnmatched), r.ledger())
	}
}

// Same, but the STK result for B arrives first.
func TestMarkPaidOnAThenSTKFirstForBCreditsB(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	r.mp.Start(ctx, a)
	markPaid(t, r, a)
	afterA := r.expiry()
	b := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b.ID, "ws_CO_B")
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B", 0, "RKTBBBB002", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTBBBB002", r.acct.Code, "50.00"))
	r.expectExpiry(t, afterA.Add(24*time.Hour), "B adds its own day, once")
	if r.revenue() != 100 {
		t.Fatalf("revenue %d", r.revenue())
	}
}

// A plain Paybill payment of the same amount after a Mark paid, with no open
// order, is not credited blindly and not hidden: a person decides in Unmatched.
func TestPlainPaybillAfterMarkPaidWaitsInUnmatched(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	first := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTPLAIN01", r.acct.Code, "50.00"))
	r.expectExpiry(t, first, "held")
	if p, _ := r.payment("RKTPLAIN01"); p.Status != store.PayUnmatched || !strings.Contains(p.Note, "possible duplicate") {
		t.Fatalf("%+v", p)
	}
	// The admin decides it is a new payment: assigning needs the explicit yes.
	pk := r.pkg("1 Day")
	err := r.svc.AssignPayment(ctx, "RKTPLAIN01", r.acct.ID, pk.ID, "")
	var dup *billing.DuplicateError
	if !errors.As(err, &dup) || !errors.Is(err, billing.ErrPossibleDuplicate) {
		t.Fatalf("assign must warn about the manual payment: %v", err)
	}
	r.expectExpiry(t, first, "the warning changed nothing")
	if err := r.svc.AssignPaymentOpts(ctx, "RKTPLAIN01", r.acct.ID, pk.ID, "paid again", billing.AssignOpts{AcceptDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	r.expectExpiry(t, first.Add(24*time.Hour), "assigned on purpose")
}

// A receipt an older build filed as the ignored duplicate of another order's
// manual payment is applied when its own order completes.
func TestIgnoredDuplicateOfADifferentOrderIsAppliedToThisOrder(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	a := r.order(t, "1 Day")
	markPaid(t, r, a)
	afterA := r.expiry()
	mk := billing.ManualPrefix + strings.ToUpper(a.ID)
	r.st.Update(func(d *store.Data) error {
		m := d.Payments[mk]
		m.Note = "confirmed by M-Pesa receipt RKTWRONG01"
		d.Payments[mk] = m
		d.Payments["RKTWRONG01"] = store.Payment{Key: "RKTWRONG01", SiteID: m.SiteID, Method: "mpesa-c2b", Status: store.PayIgnored, AmountKES: 50,
			AccountID: m.AccountID, OrderID: a.ID, Note: "duplicate: already credited when an admin marked the order paid (" + mk + "); not credited twice", PaidAt: m.PaidAt, CreatedAt: m.CreatedAt}
		return nil
	})
	b := r.order(t, "1 Day")
	r.svc.SetCheckoutID(b.ID, "ws_CO_B")
	if err := r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_B", 0, "RKTWRONG01", 50)); err != nil {
		t.Fatal(err)
	}
	if g, _ := r.svc.GetOrder(b.ID); g.Status != store.OrderPaid {
		t.Fatalf("B is %s", g.Status)
	}
	r.expectExpiry(t, afterA.Add(24*time.Hour), "B credited")
	row, _ := r.payment("RKTWRONG01")
	m, _ := r.payment(mk)
	if row.Status != store.PayApplied || row.OrderID != b.ID || strings.Contains(m.Note, "RKTWRONG01") || r.revenue() != 100 {
		t.Fatalf("row %+v\nmanual %+v revenue %d", row, m, r.revenue())
	}
}

// A refused (refunded or ignored) receipt on an STK success is not credited, but it is visible.
func TestStkSuccessForADecidedReceiptRaisesAnAlert(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	var alerts []string
	r.mp.SetAlert(func(m string) { alerts = append(alerts, m) })
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTDEC0001", "NOSUCH", "50.00"))
	r.svc.ResolvePayment("RKTDEC0001", store.PayRefunded, "sent back")
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	if err := r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTDEC0001", 50)); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "RKTDEC0001") {
		t.Fatalf("alerts %v", alerts)
	}
	if g, _ := r.svc.GetOrder(o.ID); g.Status == store.OrderPaid || !strings.Contains(g.Reason, "RKTDEC0001") {
		t.Fatalf("%+v", g)
	}
	if !r.expiry().IsZero() {
		t.Fatal("credited")
	}
}

// F2: the Paybill confirmation credited first and the checkout id was never
// stored: the STK result still closes the order.
func TestUnknownCheckoutAfterPaybillStillMarksTheOrderPaid(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day") // no checkout id: the push answer was lost
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTF20001", r.acct.Code, "50.00"))
	exp := r.expiry()
	if exp.IsZero() {
		t.Fatal("setup: the Paybill payment must be credited")
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_LOST", 0, "RKTF20001", 50))
	if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderPaid {
		t.Fatalf("the wait page would say the payment never arrived: %s", g.Status)
	}
	r.expectExpiry(t, exp, "credited once")
	if n := len(r.ledger()); n != 1 {
		t.Fatalf("rows %d", n)
	}
}

// F3: an order Daraja refused outright is not a candidate for an unknown checkout id.
func TestUnknownCheckoutIgnoresOrdersTheDarajaRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	refused := r.order(t, "1 Day")
	r.svc.FailOrder(ctx, refused.ID, billing.ReasonPromptNotSent)
	retry := r.order(t, "1 Day") // its push timed out
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_LOST", 0, "RKTF30001", 50))
	if g, _ := r.svc.GetOrder(retry.ID); g.Status != store.OrderPaid {
		t.Fatalf("the retry should be matched: %s", g.Status)
	}
	if g, _ := r.svc.GetOrder(refused.ID); g.Status != store.OrderFailed {
		t.Fatalf("the refused order must stay failed: %s", g.Status)
	}
	// An expired order is still a candidate.
	r2 := newRig(t)
	e := r2.order(t, "1 Day")
	r2.advance(20 * time.Minute)
	r2.svc.ExpireOrders()
	r2.mp.HandleSTKCallback(ctx, stkCB("ws_CO_LOST", 0, "RKTF30002", 50))
	if g, _ := r2.svc.GetOrder(e.ID); g.Status != store.OrderPaid {
		t.Fatalf("expired order not matched: %s", g.Status)
	}
}

// F5 / nits: Order.PaidAt, the legacy deadline fallback, the C2B label.
func TestOrderPaidAtIsTheTransactionTime(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCBAt("ws_CO_123", "RKTNIT0001", 50, "20261117213000"))
	g, _ := r.svc.GetOrder(o.ID)
	if got := g.PaidAt.In(billing.EAT).Format("15:04:05"); got != "21:30:00" {
		t.Fatalf("Order.PaidAt(EAT)=%s", got)
	}
}

func TestSecondPaybillPaymentForAPaidOrderIsLabelledC2B(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTONE00002", 50))
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTTWO00002", strings.ToLower(o.ID), "50.00")) // BillRef = the order id
	if p, _ := r.payment("RKTTWO00002"); p.Method != "mpesa-c2b" || p.Status != store.PayUnmatched {
		t.Fatalf("%+v", p)
	}
}

func TestLegacyDeadlineFallsBackWhenTheDirIsNotWritable(t *testing.T) {
	var warned string
	dir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(dir, []byte("x"), 0o600) // a file where the directory should be
	got := payments.LegacyQueryDeadline(dir, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), func(m string, _ ...any) { warned = m })
	if !got.Equal(payments.LegacyQueryFallback) || warned == "" {
		t.Fatalf("%v %q", got, warned)
	}
}
func TestSecondDifferentReceiptForAPaidOrderIsKeptInUnmatched(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTONE00001", 50))
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTTWO00001", 50))
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "only the first credits")
	p, ok := r.payment("RKTTWO00001")
	if !ok || p.Status != store.PayUnmatched || !strings.Contains(p.Note, "second payment") {
		t.Fatalf("%+v %v", p, ok)
	}
}

func TestConcurrentDuplicateCallbacksCreditOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTCONC001", 50)) }()
		go func() { defer wg.Done(); r.mp.HandleC2BConfirmation(ctx, c2b("RKTCONC001", r.acct.Code, "50.00")) }()
	}
	wg.Wait()
	r.expectExpiry(t, r.clock().Add(24*time.Hour), "applied more than once?")
	if n := len(r.ledger()); n != 1 {
		t.Fatalf("ledger rows %d", n)
	}
}

// ---- low: pending orders expire; the validation URL checks the amount ---------------

func TestPendingOrdersExpireAfterFifteenMinutesAndStillCreditLate(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.advance(14 * time.Minute)
	if n, err := r.svc.ExpireOrders(); err != nil || n != 0 {
		t.Fatalf("too early: %d %v", n, err)
	}
	r.advance(2 * time.Minute)
	if n, err := r.svc.ExpireOrders(); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderExpired {
		t.Fatalf("status %q (expired is not failed: the customer may still pay)", g.Status)
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTLATE001", 50))
	if g, _ := r.svc.GetOrder(o.ID); g.Status != store.OrderPaid {
		t.Fatalf("a late payment must still complete an expired order: %s", g.Status)
	}
}

func TestC2BValidationRejectsAmountsBelowTheCheapestPackage(t *testing.T) {
	r := newRig(t)
	if res := r.mp.HandleC2BValidation(c2b("V1", r.acct.Code, "7.00")); res.ResultCode == "0" {
		t.Fatal("KES 7 for a flat when no package costs less than KES 50 was accepted")
	}
	if res := r.mp.HandleC2BValidation(c2b("V2", r.acct.Code, "50.00")); res.ResultCode != "0" {
		t.Fatal("the price of the cheapest package must be accepted")
	}
	if res := r.mp.HandleC2BValidation(c2b("V3", r.acct.Code, "75.00")); res.ResultCode != "0" {
		t.Fatal("an amount above the cheapest package must be accepted")
	}
}

// ---- M4: revenue is booked when the customer paid -----------------------------------

func TestPaidAtIsDarajaTransactionTime(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Paid 23:59:50 EAT on the 17th (20:59:50 UTC); we hear of it two hours later.
	r.setNow(time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)) // 02:00 EAT on the 18th
	r.mp.HandleC2BConfirmation(ctx, c2bAt("LATE000001", r.acct.Code, "50.00", "20261117235950"))
	p, _ := r.payment("LATE000001")
	if got := p.PaidAt.In(billing.EAT).Format("2006-01-02 15:04:05"); got != "2026-11-17 23:59:50" {
		t.Fatalf("C2B PaidAt(EAT)=%s", got)
	}
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCBAt("ws_CO_123", "LATE000002", 50, "20261117235955"))
	p, _ = r.payment("LATE000002")
	if got := p.PaidAt.In(billing.EAT).Format("2006-01-02 15:04:05"); got != "2026-11-17 23:59:55" {
		t.Fatalf("STK PaidAt(EAT)=%s", got)
	}
	// Unmatched payments keep their time too.
	r.mp.HandleC2BConfirmation(ctx, c2bAt("LATE000003", "NOSUCH", "50.00", "20261117235900"))
	p, _ = r.payment("LATE000003")
	if got := p.PaidAt.In(billing.EAT).Format("15:04:05"); got != "23:59:00" {
		t.Fatalf("unmatched PaidAt(EAT)=%s", got)
	}
}

func TestPaidAtFallsBackToArrivalWhenTheTimeIsMissingOrImplausible(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	now := r.clock()
	r.mp.HandleC2BConfirmation(ctx, c2b("ARR0000001", r.acct.Code, "50.00"))
	r.mp.HandleC2BConfirmation(ctx, c2bAt("ARR0000002", "NOSUCH", "50.00", "not-a-time"))
	r.mp.HandleC2BConfirmation(ctx, c2bAt("ARR0000003", "NOSUCH", "50.00", "20300101000000")) // the future
	r.mp.HandleC2BConfirmation(ctx, c2bAt("ARR0000004", "NOSUCH", "50.00", "20200101000000")) // years ago
	for _, k := range []string{"ARR0000001", "ARR0000002", "ARR0000003", "ARR0000004"} {
		if p, _ := r.payment(k); !p.PaidAt.Equal(now) {
			t.Errorf("%s PaidAt %v, want arrival %v", k, p.PaidAt, now)
		}
	}
}

func TestSpoolReplayKeepsTheTransactionTime(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.mp.SetSpool(payments.NewSpool(filepath.Join(t.TempDir(), "spool")))
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.h.Fault.FailNextCommit(errors.New("database down"))
	r.setNow(time.Date(2026, 11, 17, 20, 59, 0, 0, time.UTC)) // 23:59 EAT
	r.mp.HandleSTKCallback(ctx, stkCBAt("ws_CO_123", "SPLREV0001", 50, "20261117235900"))
	r.setNow(time.Date(2026, 11, 17, 23, 59, 0, 0, time.UTC)) // outage over: 02:59 EAT the next day
	if _, err := r.mp.Replay(ctx); err != nil {
		t.Fatal(err)
	}
	p, ok := r.payment("SPLREV0001")
	if !ok {
		t.Fatal("the spooled callback was not applied")
	}
	if got := p.PaidAt.In(billing.EAT).Format("2006-01-02 15:04"); got != "2026-11-17 23:59" {
		t.Fatalf("booked on the replay day: PaidAt(EAT)=%s", got)
	}
}

// ---- owner: Test receipt confirmation ------------------------------------------------

func TestReceiptTestShowsDarajasRawAnswer(t *testing.T) {
	r := newRig(t)
	c := r.mp.Config()
	if _, err := r.mp.TestReceipt(context.Background(), "RKT1TESTAB"); err == nil {
		t.Fatal("without the initiator settings the test must say what is missing")
	}
	c.CallbackToken = "tok-0123456789abcdef"
	c.Initiator, c.SecurityCredential = "apiop", strings.Repeat("Q", 64)
	r.mp.SetConfig(c) // ConfirmReceipts stays off: the test is how it gets switched on
	st, err := r.mp.TestReceipt(context.Background(), "rkt1testab")
	if err != nil || st.Status != "waiting" || st.Receipt != "RKT1TESTAB" {
		t.Fatalf("%+v %v", st, err)
	}
	if q := r.txQueries(); len(q) != 1 || q[0]["TransactionID"] != "RKT1TESTAB" {
		t.Fatalf("queries %v", q)
	}
	r.mp.HandleTxStatusResult(context.Background(), txResult("1", "RKT1TESTAB", "Completed", "0", 50))
	got, ok := r.mp.ReceiptTestStatus("RKT1TESTAB")
	if !ok || got.Status != "confirmed" || !strings.Contains(got.Raw, "ReceiptNo") || !strings.Contains(got.Raw, "RKT1TESTAB") {
		t.Fatalf("%+v", got)
	}
	if len(r.ledger()) != 0 || !r.expiry().IsZero() {
		t.Fatal("a receipt test must never credit anything")
	}
	// A refusal is shown, with the reason.
	if _, err := r.mp.TestReceipt(context.Background(), "RKT2TESTAB"); err != nil {
		t.Fatal(err)
	}
	r.mp.HandleTxStatusResult(context.Background(), txResult("2", "RKT2TESTAB", "Completed", "2001", 0))
	if got, _ := r.mp.ReceiptTestStatus("RKT2TESTAB"); got.Status != "not_confirmed" || !strings.Contains(got.Verdict, "would not credit") {
		t.Fatalf("%+v", got)
	}
}
