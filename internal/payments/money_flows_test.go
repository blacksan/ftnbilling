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

func TestMarkPaidThenTheRealPaybillConfirmationIsLinkedNotCreditedTwice(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	markPaid(t, r, o)
	first := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("RKTREAL002", r.acct.Code, "50.00")) // BillRef = the account code, a new key
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKTREAL002", 50))
	r.expectExpiry(t, first, "double credit")
	r.wantLinked(t, o, "RKTREAL002")
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
