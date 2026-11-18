package payments_test

import (
	"context"
	"strings"
	"testing"

	"ispbill/internal/payments"
	"ispbill/internal/store"
)

func TestNormalizePhoneAndRef(t *testing.T) {
	for in, want := range map[string]string{"0712 345 678": "254712345678", "+254712345678": "254712345678", "712345678": "254712345678", "0112345678": "254112345678"} {
		if got, err := payments.NormalizePhone(in); err != nil || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "12345", "0712", "255712345678", "abc"} {
		if _, err := payments.NormalizePhone(bad); err == nil {
			t.Errorf("NormalizePhone(%q) accepted", bad)
		}
	}
	if payments.NormalizeRef(" abc-23 4 ") != "ABC234" {
		t.Error("NormalizeRef")
	}
}

func TestOAuthTestReportsBadCredentials(t *testing.T) {
	r := newRig(t)
	if _, err := r.mp.TestOAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := r.mp.Config()
	c.Secret = "wrong-secret-value"
	r.mp.SetConfig(c)
	_, err := r.mp.TestOAuth(context.Background())
	if err == nil {
		t.Fatal("bad credentials accepted")
	}
	if strings.Contains(err.Error(), "wrong-secret-value") || strings.Contains(err.Error(), "PASSKEY") {
		t.Fatalf("a credential leaked into the error: %v", err)
	}
}

func TestRegisterC2BUsesPathTokenURLs(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	c := r.mp.Config()
	c.CallbackToken = ""
	r.mp.SetConfig(c)
	if _, err := r.mp.RegisterC2B(ctx); err == nil {
		t.Fatal("registered without a callback token")
	}
	c.CallbackToken, c.C2BShortcode = "tok123", "600000"
	r.mp.SetConfig(c)
	desc, err := r.mp.RegisterC2B(ctx)
	if err != nil || desc != "Success" {
		t.Fatalf("%q %v", desc, err)
	}
	b := r.fd.regBody
	if b["ShortCode"] != "600000" || b["ResponseType"] != "Completed" ||
		b["ConfirmationURL"] != "https://example.org/pay/cb/t/tok123/c2b/confirm" || b["ValidationURL"] != "https://example.org/pay/cb/t/tok123/c2b/validate" {
		t.Fatalf("register body: %v", b)
	}
	// http:// is refused before any call.
	c.PublicBaseURL = "http://example.org"
	r.mp.SetConfig(c)
	if _, err := r.mp.RegisterC2B(ctx); err == nil {
		t.Fatal("http URL accepted")
	}
	// A Daraja refusal comes back as an error.
	c.PublicBaseURL = "https://example.org"
	r.mp.SetConfig(c)
	r.fd.regCode = "URLs are already registered"
	if _, err := r.mp.RegisterC2B(ctx); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestSettingsTestPayment(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ts, err := r.mp.StartTest(ctx, "0712345678")
	if err != nil || ts.Status != "pending" || ts.Phone != "254712345678" {
		t.Fatalf("%+v %v", ts, err)
	}
	if r.fd.stkBody["AccountReference"] != payments.FTNTestRef || r.fd.stkBody["Amount"] != float64(1) {
		t.Fatalf("push body: %v", r.fd.stkBody)
	}
	// Still processing: nothing is decided.
	code, _, err := r.mp.STKQuery(ctx, ts.CheckoutID)
	if err != nil || code != "" {
		t.Fatalf("query while pending: %q %v", code, err)
	}
	// The callback marks it paid and records an Unmatched payment with a note.
	if err := r.mp.HandleSTKCallback(ctx, stkCB(ts.CheckoutID, 0, "TESTRCPT01", 1)); err != nil {
		t.Fatal(err)
	}
	got, _ := r.mp.TestStatus(ts.CheckoutID)
	if got.Status != "paid" || got.Receipt != "TESTRCPT01" {
		t.Fatalf("%+v", got)
	}
	var note string
	var n int
	r.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			if p.Status == store.PayUnmatched {
				n++
				note = p.Note
			}
		}
	})
	if n != 1 || note != "settings test" {
		t.Fatalf("unmatched=%d note=%q", n, note)
	}
	// No flat was credited.
	if !r.expiry().IsZero() && r.expiry().After(r.now) {
		t.Fatal("the test payment extended a flat")
	}
	// A query result stands in when the callback never comes.
	ts2, _ := r.mp.StartTest(ctx, "0712345678")
	r.fd.queryRes = "1032"
	code, desc, err := r.mp.STKQuery(ctx, ts2.CheckoutID)
	if err != nil || code != "1032" {
		t.Fatalf("%q %q %v", code, desc, err)
	}
	if st := r.mp.ApplyQuery(ts2.CheckoutID, code, desc); st.Status != "failed" {
		t.Fatalf("%+v", st)
	}
	// Disabled M-Pesa refuses.
	c := r.mp.Config()
	c.Enabled = false
	r.mp.SetConfig(c)
	if _, err := r.mp.StartTest(ctx, "0712345678"); err == nil {
		t.Fatal("test payment started while disabled")
	}
}

func TestFTNTESTNeverMatchesAndUnknownRefPolicy(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// The reserved reference is always accepted at validation and lands in Unmatched.
	if res := r.mp.HandleC2BValidation(c2b("T1", "FTNTEST", "1.00")); res.ResultCode != "0" {
		t.Fatalf("FTNTEST validation: %+v", res)
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("TRNTEST001", "ftn test", "1.00"))
	if n := r.countStatus(store.PayUnmatched); n != 1 {
		t.Fatalf("unmatched = %d", n)
	}
	// Unknown reference: reject by default, accept when asked.
	if res := r.mp.HandleC2BValidation(c2b("T2", "NOSUCH", "50.00")); res.ResultCode != "C2B00012" {
		t.Fatalf("default should reject: %+v", res)
	}
	c := r.mp.Config()
	c.UnknownRef = "accept"
	r.mp.SetConfig(c)
	if res := r.mp.HandleC2BValidation(c2b("T2", "NOSUCH", "50.00")); res.ResultCode != "0" {
		t.Fatalf("accept policy: %+v", res)
	}
	// A typed code with spaces and a lower case still finds the flat.
	spaced := strings.ToLower(r.acct.Code[:3]) + " - " + r.acct.Code[3:]
	r.mp.HandleC2BConfirmation(ctx, c2b("TRNSPACE01", spaced, "3000.00"))
	if r.expiry().IsZero() {
		t.Fatalf("payment with a typed code %q was not applied", spaced)
	}
}
