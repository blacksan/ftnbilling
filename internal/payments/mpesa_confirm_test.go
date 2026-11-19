package payments_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ispbill/internal/payments"
	"ispbill/internal/store"
)

// confirmRig is the M-Pesa rig with receipt confirmation switched on against the fake Daraja.
func confirmRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	c := r.mp.Config()
	c.CallbackToken = "tok-0123456789abcdef"
	c.Initiator, c.SecurityCredential, c.ConfirmReceipts = "apiop", strings.Repeat("Q", 64), true
	r.mp.SetConfig(c)
	r.mp.SetSpool(payments.NewSpool(t.TempDir()))
	t.Cleanup(r.mp.Close)
	return r
}

func txResult(conv, receipt, status, code string, amount int) []byte {
	b, _ := json.Marshal(map[string]any{"Result": map[string]any{
		"ResultType": 0, "ResultCode": code, "ResultDesc": "The service request is processed successfully.",
		"OriginatorConversationID": "OR-" + conv, "ConversationID": "AG_" + conv,
		"ResultParameters": map[string]any{"ResultParameter": []map[string]any{
			{"Key": "ReceiptNo", "Value": receipt}, {"Key": "TransactionStatus", "Value": status}, {"Key": "Amount", "Value": amount},
		}}}})
	return b
}

func (r *rig) txQueries() []map[string]string {
	r.fd.mu.Lock()
	defer r.fd.mu.Unlock()
	return append([]map[string]string(nil), r.fd.txBodies...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 600; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// replayUntil runs the spool (as the minute ticker does) until ok, since a pass
// already running in the background makes a second one a no-op.
func (r *rig) replayUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	waitFor(t, what, func() bool { r.mp.Replay(context.Background()); return ok() })
}

func (r *rig) payment(key string) (p store.Payment, ok bool) {
	r.st.View(func(d *store.Data) { p, ok = d.Payments[key] })
	return
}

func TestC2BIsHeldUntilDarajaConfirmsTheReceipt(t *testing.T) {
	r := confirmRig(t)
	ctx := context.Background()
	before := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("CONF1ABCDE", r.acct.Code, "3000.00"))
	waitFor(t, "the Transaction Status query", func() bool { return len(r.txQueries()) == 1 })
	q := r.txQueries()[0]
	if q["TransactionID"] != "CONF1ABCDE" || q["Initiator"] != "apiop" || q["CommandID"] != "TransactionStatusQuery" || q["IdentifierType"] != "4" ||
		q["PartyA"] != "174379" || !strings.Contains(q["ResultURL"], "/pay/cb/t/tok-0123456789abcdef/txstatus/result") {
		t.Fatalf("query: %v", q)
	}
	if !r.expiry().Equal(before) || r.countStatus(store.PayUnmatched) != 0 {
		t.Fatal("the payment was credited before Daraja confirmed it")
	}
	// A forged answer (the conversation id is not one Daraja gave us) changes nothing.
	r.mp.HandleTxStatusResult(ctx, txResult("999", "CONF1ABCDE", "Completed", "0", 3000))
	time.Sleep(50 * time.Millisecond)
	if !r.expiry().Equal(before) {
		t.Fatal("a forged result credited the payment")
	}
	// The real answer credits it, once.
	r.mp.HandleTxStatusResult(ctx, txResult("1", "CONF1ABCDE", "Completed", "0", 3000))
	waitFor(t, "the credit", func() bool { return r.expiry().After(before) })
	if got, want := r.expiry(), r.now.Add(30*24*time.Hour); !got.Equal(want) {
		t.Fatalf("expiry %v want %v", got, want)
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("CONF1ABCDE", r.acct.Code, "3000.00")) // redelivery
	time.Sleep(100 * time.Millisecond)
	if len(r.txQueries()) != 1 {
		t.Errorf("redelivery of a credited receipt asked Daraja again: %d queries", len(r.txQueries()))
	}
}

func TestUnconfirmedReceiptGoesToUnmatchedWithReason(t *testing.T) {
	r := confirmRig(t)
	ctx := context.Background()
	before := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("FAKE000001", r.acct.Code, "3000.00"))
	waitFor(t, "the query", func() bool { return len(r.txQueries()) == 1 })
	// Daraja says it has no such transaction.
	b, _ := json.Marshal(map[string]any{"Result": map[string]any{"ResultCode": "2001", "ResultDesc": "The transaction does not exist",
		"OriginatorConversationID": "OR-1", "ConversationID": "AG_1"}})
	r.mp.HandleTxStatusResult(ctx, b)
	time.Sleep(50 * time.Millisecond)
	if !r.expiry().Equal(before) {
		t.Fatal("credited")
	}
	// Still unconfirmed when the window ends: not credited, listed for review with the reason.
	r.advance(16 * time.Minute)
	r.replayUntil(t, "the unmatched entry", func() bool { _, ok := r.payment("FAKE000001"); return ok })
	p, ok := r.payment("FAKE000001")
	if !ok || p.Status != store.PayUnmatched || !strings.Contains(p.Note, "not credited") || !strings.Contains(p.Note, "does not exist") {
		t.Fatalf("unmatched entry: %+v ok=%v", p, ok)
	}
	if !r.expiry().Equal(before) {
		t.Fatal("credited after all")
	}
}

func TestReceiptCheckAskedAgainWhenTheQueryFails(t *testing.T) {
	r := confirmRig(t)
	ctx := context.Background()
	r.fd.mu.Lock()
	r.fd.txReject = "Invalid Access Token"
	r.fd.mu.Unlock()
	r.mp.HandleC2BConfirmation(ctx, c2b("RETRY1ABCD", r.acct.Code, "3000.00"))
	waitFor(t, "first query", func() bool { return len(r.txQueries()) == 1 })
	r.fd.mu.Lock()
	r.fd.txReject = ""
	r.fd.mu.Unlock()
	r.mp.Replay(ctx) // too soon: no second query yet
	time.Sleep(1200 * time.Millisecond)
	if len(r.txQueries()) != 1 {
		t.Fatalf("asked again at once: %d", len(r.txQueries()))
	}
	r.advance(90 * time.Second)
	r.replayUntil(t, "the second query", func() bool { return len(r.txQueries()) == 2 })
	r.mp.HandleTxStatusResult(ctx, txResult("2", "RETRY1ABCD", "Completed", "0", 3000))
	waitFor(t, "the credit", func() bool { _, ok := r.payment("RETRY1ABCD"); return ok })
}

func TestAmountMismatchIsNotConfirmed(t *testing.T) {
	r := confirmRig(t)
	ctx := context.Background()
	before := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("AMT1ABCDEF", r.acct.Code, "3000.00"))
	waitFor(t, "query", func() bool { return len(r.txQueries()) == 1 })
	r.mp.HandleTxStatusResult(ctx, txResult("1", "AMT1ABCDEF", "Completed", "0", 50)) // Daraja says KES 50
	time.Sleep(50 * time.Millisecond)
	if !r.expiry().Equal(before) {
		t.Fatal("credited KES 3000 for a KES 50 receipt")
	}
}

func TestSTKResultIsHeldUntilConfirmed(t *testing.T) {
	r := confirmRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	if err := r.mp.Start(ctx, o); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.fd.stkBody["CallBackURL"].(string); !strings.Contains(u, "/pay/cb/t/tok-0123456789abcdef/stk") || strings.Contains(u, "?t=") {
		t.Fatalf("the STK callback URL must carry the token in the path: %q", u)
	}
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "STK1ABCDEF", 50))
	waitFor(t, "query", func() bool { return len(r.txQueries()) == 1 })
	if got, _ := r.svc.GetOrder(o.ID); got.Status == store.OrderPaid {
		t.Fatal("order paid before confirmation")
	}
	r.mp.HandleTxStatusResult(ctx, txResult("1", "STK1ABCDEF", "Completed", "0", 50))
	waitFor(t, "the order to be paid", func() bool { got, _ := r.svc.GetOrder(o.ID); return got.Status == store.OrderPaid })
}

func TestUnconfirmedSTKFailsTheOrderAndIsListed(t *testing.T) {
	r := confirmRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "STKFAKE001", 50))
	waitFor(t, "query", func() bool { return len(r.txQueries()) == 1 })
	r.advance(20 * time.Minute)
	r.replayUntil(t, "the unmatched entry", func() bool { _, ok := r.payment("STKFAKE001"); return ok })
	p, ok := r.payment("STKFAKE001")
	if !ok || p.Status != store.PayUnmatched || !strings.Contains(p.Note, "not credited") {
		t.Fatalf("%+v %v", p, ok)
	}
	if got, _ := r.svc.GetOrder(o.ID); got.Status == store.OrderPaid {
		t.Fatal("order paid")
	}
}

func TestConfirmationOffCreditsAtOnce(t *testing.T) {
	r := confirmRig(t)
	c := r.mp.Config()
	c.ConfirmReceipts = false
	r.mp.SetConfig(c)
	before := r.expiry()
	r.mp.HandleC2BConfirmation(context.Background(), c2b("OFF1ABCDEF", r.acct.Code, "3000.00"))
	if !r.expiry().After(before) || len(r.txQueries()) != 0 {
		t.Fatal("with the check off a callback is credited immediately and Daraja is not asked")
	}
}

func TestLegacyQueryDeadlineIsStable(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a := payments.LegacyQueryDeadline(dir, t0)
	if want := t0.Add(30 * 24 * time.Hour); !a.Equal(want) {
		t.Fatalf("%v want %v", a, want)
	}
	if b := payments.LegacyQueryDeadline(dir, t0.Add(10*24*time.Hour)); !b.Equal(a) {
		t.Fatalf("a restart moved the deadline: %v", b)
	}
}
