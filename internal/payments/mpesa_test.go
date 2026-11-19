package payments_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/payments"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

type fakeDaraja struct {
	mu       sync.Mutex
	oauth    int
	stkBody  map[string]any
	stkAuth  string
	respCode string
	regBody  map[string]string   // last RegisterURL body
	regCode  string              // ResponseCode to answer RegisterURL with
	queryRes string              // ResultCode to answer STK queries with ("" = still processing)
	queryRaw string              // when set, the exact body of the STK query answer
	txBodies []map[string]string // Transaction Status requests received
	txReject string              // when set, the Transaction Status API refuses with this message
}

func (f *fakeDaraja) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/oauth/v1/generate":
		f.oauth++
		u, p, _ := r.BasicAuth()
		if u != "key" || p != "secret" || r.URL.Query().Get("grant_type") != "client_credentials" {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(`{"access_token":"TOK","expires_in":"3599"}`))
	case "/mpesa/stkpush/v1/processrequest":
		f.stkAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &f.stkBody)
		code := f.respCode
		if code == "" {
			code = "0"
		}
		w.Write([]byte(`{"MerchantRequestID":"m1","CheckoutRequestID":"ws_CO_123","ResponseCode":"` + code + `","ResponseDescription":"ok"}`))
	case "/mpesa/c2b/v2/registerurl":
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &f.regBody)
		if f.regCode != "" {
			w.WriteHeader(400)
			w.Write([]byte(`{"errorCode":"500.003.1001","errorMessage":"` + f.regCode + `"}`))
			return
		}
		w.Write([]byte(`{"ResponseCode":"0","ResponseDescription":"Success"}`))
	case "/mpesa/transactionstatus/v1/query":
		b, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(b, &req)
		f.txBodies = append(f.txBodies, req)
		if f.txReject != "" {
			w.WriteHeader(400)
			w.Write([]byte(`{"errorCode":"400.002.02","errorMessage":"` + f.txReject + `"}`))
			return
		}
		n := len(f.txBodies)
		fmt.Fprintf(w, `{"OriginatorConversationID":"OR-%d","ConversationID":"AG_%d","ResponseCode":"0","ResponseDescription":"Accept the service request successfully."}`, n, n)
	case "/mpesa/stkpushquery/v1/query":
		if f.queryRaw != "" {
			w.Write([]byte(f.queryRaw))
			return
		}
		if f.queryRes == "" {
			w.WriteHeader(500)
			w.Write([]byte(`{"errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`))
			return
		}
		w.Write([]byte(`{"ResponseCode":"0","ResponseDescription":"ok","ResultCode":"` + f.queryRes + `","ResultDesc":"done"}`))
	default:
		http.NotFound(w, r)
	}
}

type rig struct {
	svc  *billing.Service
	st   *store.Store
	h    *storetest.Handle
	site string
	rt   *routeros.Mock
	mp   *payments.Mpesa
	fd   *fakeDaraja
	now  time.Time
	acct store.Account
	cmu  sync.Mutex // guards now when a test moves the clock while background work runs
}

func (r *rig) clock() time.Time { r.cmu.Lock(); defer r.cmu.Unlock(); return r.now }

// advance moves the clock forward (safe next to the receipt-check goroutines).
func (r *rig) advance(d time.Duration) { r.cmu.Lock(); r.now = r.now.Add(d); r.cmu.Unlock() }

const ontMAC = "AA:BB:CC:DD:EE:01"

func newRig(t *testing.T) *rig {
	t.Helper()
	fd := &fakeDaraja{}
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	h := storetest.NewIn(t, t.TempDir())
	st := h.Store
	rt := routeros.NewMock()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	site := store.NewSiteID()
	st.Update(func(d *store.Data) error {
		d.Sites[site] = store.Site{ID: site, Name: "Block A", Mode: store.SiteModeWG, WGPublicKey: "PUB", TunnelIP: "10.99.0.2", AccessMode: store.AccessIPoE}
		return nil
	})
	svc := billing.NewService(st, billing.MockRouters{site: rt}, log)
	r := &rig{svc: svc, st: st, h: h, site: site, rt: rt, fd: fd, now: time.Date(2026, 11, 17, 21, 30, 5, 0, time.UTC)}
	svc.SetClock(r.clock)
	svc.SeedPackages()
	a, err := svc.CreateAccount(billing.AccountInput{SiteID: site, Flat: "Flat 3B", MAC: ontMAC})
	if err != nil {
		t.Fatal(err)
	}
	r.acct = a
	r.mp = payments.NewMpesa(payments.MpesaConfig{
		BaseURL: srv.URL, Key: "key", Secret: "secret", Shortcode: "174379", Passkey: "PASSKEY",
		PublicBaseURL: "https://example.org", Enabled: true,
	}, svc, st, log)
	r.mp.SetClock(r.clock)
	return r
}

func (r *rig) pkg(name string) store.Package {
	var out store.Package
	r.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if p.Name == name {
				out = p
			}
		}
	})
	return out
}

func (r *rig) expiry() (t time.Time) {
	r.st.View(func(d *store.Data) { t = d.Accounts[r.acct.ID].ExpiresAt })
	return
}

func (r *rig) countStatus(status string) (n int) {
	r.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			if p.Status == status {
				n++
			}
		}
	})
	return
}

func (r *rig) order(t *testing.T, pkg string) store.Order {
	o, err := r.svc.CreateOrder(billing.OrderInput{AccountID: r.acct.ID, PackageID: r.pkg(pkg).ID, Phone: "254712345678", Method: "mpesa"})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func stkCB(checkout string, code int, receipt string, amount int) []byte {
	if code != 0 {
		return []byte(`{"Body":{"stkCallback":{"MerchantRequestID":"m1","CheckoutRequestID":"` + checkout + `","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`)
	}
	b, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"MerchantRequestID": "m1", "CheckoutRequestID": checkout, "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": []map[string]any{
			{"Name": "Amount", "Value": amount}, {"Name": "MpesaReceiptNumber", "Value": receipt},
			{"Name": "TransactionDate", "Value": 20261117213000}, {"Name": "PhoneNumber", "Value": 254712345678},
		}}}}})
	return b
}

func TestSTKPushRequestShape(t *testing.T) {
	r := newRig(t)
	o := r.order(t, "1 Day")
	ctx := context.Background()
	if err := r.mp.Start(ctx, o); err != nil {
		t.Fatal(err)
	}
	b := r.fd.stkBody
	// 21:30:05 UTC is 00:30:05 on the 18th in EAT.
	if b["Timestamp"] != "20261118003005" {
		t.Errorf("timestamp %v", b["Timestamp"])
	}
	wantPW := base64.StdEncoding.EncodeToString([]byte("174379PASSKEY20261118003005"))
	if b["Password"] != wantPW {
		t.Errorf("password %v", b["Password"])
	}
	checks := map[string]any{
		"BusinessShortCode": "174379", "TransactionType": "CustomerPayBillOnline", "Amount": float64(50),
		"PartyA": "254712345678", "PartyB": "174379", "PhoneNumber": "254712345678",
		"CallBackURL": "https://example.org/pay/cb/stk", "AccountReference": r.acct.Code,
	}
	for k, v := range checks {
		if b[k] != v {
			t.Errorf("%s = %v, want %v", k, b[k], v)
		}
	}
	if strings.Contains(strings.ToLower(b["CallBackURL"].(string)), "mpesa") {
		t.Error("callback URL contains mpesa")
	}
	if r.fd.stkAuth != "Bearer TOK" {
		t.Errorf("auth %q", r.fd.stkAuth)
	}
	got, _ := r.svc.GetOrder(o.ID)
	if got.CheckoutRequestID != "ws_CO_123" {
		t.Errorf("checkout id %q", got.CheckoutRequestID)
	}
	r.mp.Start(ctx, r.order(t, "1 Day"))
	if r.fd.oauth != 1 {
		t.Errorf("oauth called %d times", r.fd.oauth)
	}
}

func TestSTKPushRejected(t *testing.T) {
	r := newRig(t)
	r.fd.respCode = "1"
	if err := r.mp.Start(context.Background(), r.order(t, "1 Day")); err == nil {
		t.Fatal("expected error")
	}
}

func TestSTKCallbackPaidIdempotent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	if err := r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKT1ABCDEF", 50)); err != nil {
		t.Fatal(err)
	}
	got, _ := r.svc.GetOrder(o.ID)
	if got.Status != store.OrderPaid || got.Receipt != "RKT1ABCDEF" {
		t.Fatalf("%+v", got)
	}
	exp := r.expiry()
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKT1ABCDEF", 50)) // duplicate delivery
	if !r.expiry().Equal(exp) {
		t.Fatal("duplicate STK callback extended again")
	}
	r.st.View(func(d *store.Data) {
		if len(d.Payments) != 1 {
			t.Fatalf("ledger has %d entries", len(d.Payments))
		}
	})
}

func TestSTKCallbackFailureAndShortAmount(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 1, "", 0))
	if got, _ := r.svc.GetOrder(o.ID); got.Status != store.OrderFailed || got.Reason == "" {
		t.Fatalf("%+v", got)
	}
	o2 := r.order(t, "1 Week")
	r.svc.SetCheckoutID(o2.ID, "ws_CO_999")
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_999", 0, "RKT2ABCDEF", 100))
	if got, _ := r.svc.GetOrder(o2.ID); got.Status == store.OrderPaid {
		t.Fatal("underpayment must not be accepted")
	}
	if r.countStatus(store.PayUnmatched) != 1 {
		t.Fatal("underpayment should be in the unmatched queue")
	}
}

func c2b(trans, ref string, amount string) []byte {
	b, _ := json.Marshal(map[string]any{"TransactionType": "Pay Bill", "TransID": trans, "TransAmount": amount,
		"BusinessShortCode": "174379", "BillRefNumber": ref, "MSISDN": "254712345678", "FirstName": "Jane", "LastName": "Doe"})
	return b
}

func TestC2BValidationAndConfirmation(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	code := r.acct.Code
	if res := r.mp.HandleC2BValidation(c2b("T0", "NOPE12", "50.00")); res.ResultCode != "C2B00012" {
		t.Errorf("unknown account: %+v", res)
	}
	if res := r.mp.HandleC2BValidation(c2b("T0", strings.ToLower(code), "50.00")); res.ResultCode != "0" {
		t.Errorf("known code: %+v", res)
	}

	// Paying 3000 against the account code applies Monthly 40 Mbps (price match).
	res := r.mp.HandleC2BConfirmation(ctx, c2b("TRN1ABCDEF", code, "3000.00"))
	if res.ResultCode != "0" || res.ResultDesc != "Accepted" {
		t.Fatalf("%+v", res)
	}
	want := r.now.Add(30 * 24 * time.Hour)
	if !r.expiry().Equal(want) {
		t.Fatalf("expiry %v want %v", r.expiry(), want)
	}
	// Redelivery is a no-op.
	r.mp.HandleC2BConfirmation(ctx, c2b("TRN1ABCDEF", code, "3000.00"))
	if !r.expiry().Equal(want) {
		t.Error("duplicate confirmation extended twice")
	}
	if r.countStatus(store.PayUnmatched) != 0 {
		t.Error("unexpected unmatched")
	}
	// Amount with no package, and unknown account: both land in the unmatched queue.
	r.mp.HandleC2BConfirmation(ctx, c2b("TRN2ABCDEF", code, "77.00"))
	r.mp.HandleC2BConfirmation(ctx, c2b("TRN3ABCDEF", "ZZZZZZ", "50.00"))
	r.mp.HandleC2BConfirmation(ctx, c2b("TRN3ABCDEF", "ZZZZZZ", "50.00")) // duplicate
	if n := r.countStatus(store.PayUnmatched); n != 2 {
		t.Fatalf("unmatched = %d", n)
	}
}

func TestC2BConfirmationForPendingOrderIsIdempotentWithSTK(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	if res := r.mp.HandleC2BValidation(c2b("T9", o.ID, "50")); res.ResultCode != "0" {
		t.Fatal("order id should validate")
	}
	r.mp.HandleC2BConfirmation(ctx, c2b("RKT7ABCDEF", strings.ToUpper(o.ID), "50.00"))
	exp := r.expiry()
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKT7ABCDEF", 50)) // same receipt arrives via STK
	got, _ := r.svc.GetOrder(o.ID)
	if got.Status != store.OrderPaid || !r.expiry().Equal(exp) || r.countStatus(store.PayUnmatched) != 0 {
		t.Fatal("double processing")
	}
}

// The STK result and the C2B confirmation for the same payment arrive in either order.
func TestStkThenC2BForSameReceiptAppliesOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	o := r.order(t, "1 Day")
	r.mp.Start(ctx, o)
	r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "RKT8ABCDEF", 50))
	exp := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("RKT8ABCDEF", r.acct.Code, "50.00"))
	if !r.expiry().Equal(exp) {
		t.Fatal("C2B for an STK payment extended twice")
	}
}

func TestTestProviderGatedByFlag(t *testing.T) {
	r := newRig(t)
	off := &payments.Test{Enabled_: false, Delay: time.Millisecond, C: r.svc}
	if off.Enabled() || off.Start(context.Background(), r.order(t, "1 Hour")) == nil {
		t.Fatal("must refuse when disabled")
	}
	on := &payments.Test{Enabled_: true, Delay: 20 * time.Millisecond, C: r.svc}
	o := r.order(t, "1 Hour")
	on.Start(context.Background(), o)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := r.svc.GetOrder(o.ID); got.Status == store.OrderPaid {
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("test payment never completed")
}

func TestC2BLowerPlanWhileActiveGoesToUnmatched(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	code := r.acct.Code
	r.mp.HandleC2BConfirmation(ctx, c2b("UPG1ABCDEF", code, "3000.00")) // Monthly 40
	exp := r.expiry()
	// 250 is the 1 Week package (10 Mbps): slower than the active plan.
	r.mp.HandleC2BConfirmation(ctx, c2b("LOW1ABCDEF", code, "250.00"))
	if !r.expiry().Equal(exp) {
		t.Fatal("lower plan must not be applied")
	}
	var row store.Payment
	r.st.View(func(d *store.Data) { row = d.Payments["LOW1ABCDEF"] })
	if row.Status != store.PayUnmatched || !strings.Contains(row.Note, "lower plan while active") || row.AccountID != r.acct.ID {
		t.Fatalf("row: %+v", row)
	}
}

// A Paybill amount is matched against the prices of the building the account
// lives in: site B's override credits a site B account and is unmatched for a
// site A account, whose shared price list does not have that amount.
func TestC2BAmountMatchesTheAccountsOwnSitePrices(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	rtB := routeros.NewMock()
	siteB := store.NewSiteID()
	dayID := r.pkg("1 Day").ID
	r.st.Update(func(d *store.Data) error {
		d.Sites[siteB] = store.Site{ID: siteB, Name: "Block B", Mode: store.SiteModeEnv,
			PkgOverrides: map[string]store.PkgOverride{dayID: {PriceKES: 80}}}
		return nil
	})
	// Rebuild the service so it knows both routers.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := billing.NewService(r.st, billing.MockRouters{r.site: r.rt, siteB: rtB}, log)
	svc.SetClock(func() time.Time { return r.now })
	mp := payments.NewMpesa(payments.MpesaConfig{PublicBaseURL: "https://example.org", Enabled: true}, svc, r.st, log)
	mp.SetClock(func() time.Time { return r.now })
	bAcct, err := svc.CreateAccount(billing.AccountInput{SiteID: siteB, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:0B"})
	if err != nil {
		t.Fatal(err)
	}
	mp.HandleC2BConfirmation(ctx, c2b("BBB1ABCDEF", bAcct.Code, "80.00"))
	var pay store.Payment
	r.st.View(func(d *store.Data) { pay = d.Payments["BBB1ABCDEF"] })
	if pay.Status != store.PayApplied || pay.SiteID != siteB || pay.AccountID != bAcct.ID {
		t.Fatalf("site B account must be credited: %+v", pay)
	}
	// The same amount for the site A account does not match any package there.
	mp.HandleC2BConfirmation(ctx, c2b("AAA1ABCDEF", r.acct.Code, "80.00"))
	r.st.View(func(d *store.Data) { pay = d.Payments["AAA1ABCDEF"] })
	if pay.Status != store.PayUnmatched || pay.SiteID != r.site || pay.AccountID != r.acct.ID {
		t.Fatalf("site A account must get an unmatched row stamped with its site: %+v", pay)
	}
	if !r.expiry().IsZero() {
		t.Fatal("site A account must not be credited")
	}
	// And 50 (the shared price) is no longer a match at B.
	mp.HandleC2BConfirmation(ctx, c2b("BBB2ABCDEF", bAcct.Code, "50.00"))
	r.st.View(func(d *store.Data) { pay = d.Payments["BBB2ABCDEF"] })
	if pay.Status != store.PayUnmatched || pay.SiteID != siteB {
		t.Fatalf("%+v", pay)
	}
	// An unknown account number has no site.
	mp.HandleC2BConfirmation(ctx, c2b("ZZZ1ABCDEF", "QQQQQQ", "50.00"))
	r.st.View(func(d *store.Data) { pay = d.Payments["ZZZ1ABCDEF"] })
	if pay.Status != store.PayUnmatched || pay.SiteID != "" {
		t.Fatalf("%+v", pay)
	}
}
