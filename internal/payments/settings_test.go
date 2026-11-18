package payments_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

func (r *rig) unmatchedNote(key string) (note string, found bool) {
	r.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			if p.Status == store.PayUnmatched && strings.Contains(p.Key, key) {
				note, found = p.Note, true
			}
		}
	})
	return
}

func TestFTNTESTNeverMatchesAndUnknownRefPolicy(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// Anyone can type FTNTEST as an account number. With no test payment under
	// way it is an unknown reference: bounced at validation, and a confirmation
	// is filed as such.
	if res := r.mp.HandleC2BValidation(c2b("T1", "FTNTEST", "1.00")); res.ResultCode != "C2B00012" {
		t.Fatalf("FTNTEST without a test under way: %+v", res)
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("TRNSTRANGER", "FTNTEST", "1.00"))
	if note, ok := r.unmatchedNote("TRNSTRANGER"); !ok || note != "unknown account number" {
		t.Fatalf("stranger FTNTEST: %q %v", note, ok)
	}
	// While a test is pending (and only for its KES 1) it is the test.
	if _, err := r.mp.StartTest(ctx, "0712345678"); err != nil {
		t.Fatal(err)
	}
	if res := r.mp.HandleC2BValidation(c2b("T1", "FTNTEST", "1.00")); res.ResultCode != "0" {
		t.Fatalf("FTNTEST validation during a test: %+v", res)
	}
	if res := r.mp.HandleC2BValidation(c2b("T1", "FTNTEST", "500.00")); res.ResultCode != "C2B00012" {
		t.Fatalf("FTNTEST with another amount: %+v", res)
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("TRNTEST001", "ftn test", "1.00"))
	if note, ok := r.unmatchedNote("TRNTEST001"); !ok || note != "settings test" {
		t.Fatalf("test payment: %q %v", note, ok)
	}
	// Ten minutes later the test is stale and the reference is unknown again.
	r.now = r.now.Add(11 * time.Minute)
	if res := r.mp.HandleC2BValidation(c2b("T1", "FTNTEST", "1.00")); res.ResultCode != "C2B00012" {
		t.Fatalf("stale test still accepted: %+v", res)
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

func TestCallbackTokenGraceForPreviousToken(t *testing.T) {
	r := newRig(t)
	c := r.mp.Config()
	c.CallbackToken, c.PrevToken, c.PrevUntil = "current-token-123456", "previous-token-12345", r.now.Add(time.Hour)
	r.mp.SetConfig(c)
	for tok, want := range map[string]bool{"current-token-123456": true, "previous-token-12345": true, "other": false, "": false} {
		if got := r.mp.CheckCallbackToken(tok); got != want {
			t.Errorf("%q: %v want %v", tok, got, want)
		}
	}
	r.now = r.now.Add(2 * time.Hour) // the grace is over
	if r.mp.CheckCallbackToken("previous-token-12345") || !r.mp.CheckCallbackToken("current-token-123456") {
		t.Error("previous token accepted after its grace period, or the current one refused")
	}
	// An empty current token never matches an empty candidate.
	c.CallbackToken, c.PrevToken = "", ""
	r.mp.SetConfig(c)
	if r.mp.CheckCallbackToken("") {
		t.Error("empty token accepted")
	}
}

// Daraja sends ResponseCode and ResultCode as strings or as numbers; both
// decode, and a type surprise must not turn a result into a bogus error.
func TestSTKQueryAcceptsNumericAndStringCodes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	ts, err := r.mp.StartTest(ctx, "0712345678")
	if err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		`{"ResponseCode":0,"ResponseDescription":"ok","ResultCode":1032,"ResultDesc":"cancelled"}`:   "1032",
		`{"ResponseCode":"0","ResponseDescription":"ok","ResultCode":"0","ResultDesc":"paid"}`:       "0",
		`{"ResponseCode":0,"ResponseDescription":"ok","ResultCode":0,"ResultDesc":"paid"}`:           "0",
		`{"ResponseCode":"0","ResponseDescription":"ok","ResultCode":"1037","ResultDesc":"timeout"}`: "1037",
	} {
		r.fd.queryRaw = body
		code, _, err := r.mp.STKQuery(ctx, ts.CheckoutID)
		if err != nil || code != want {
			t.Errorf("%s: code %q err %v", body, code, err)
		}
	}
	r.fd.queryRaw = `{"ResponseCode":0,"ResponseDescription":"accepted, no result yet"}`
	if code, _, err := r.mp.STKQuery(ctx, ts.CheckoutID); err != nil || code != "" {
		t.Errorf("no result yet: %q %v", code, err)
	}
	r.fd.queryRaw = `{"ResponseCode":1,"ResponseDescription":"nope"}`
	if _, _, err := r.mp.STKQuery(ctx, ts.CheckoutID); err == nil {
		t.Error("a non-zero ResponseCode must be an error")
	}
}
