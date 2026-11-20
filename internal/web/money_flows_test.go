package web_test

// Regression tests for the money-flow review (flow group 3), web side.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/payments"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

func (a *app) flat(t *testing.T) store.Account {
	t.Helper()
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	return a.account(t, "Flat 3B")
}

// Vouchers must outlive their package being switched off: the delete-package
// message tells the admin to disable the package "so they still work".
func TestVoucherOfDisabledPackageStillRedeems(t *testing.T) {
	web.ResetPayLimiters()
	t.Cleanup(web.ResetPayLimiters)
	a := newApp(t, false)
	acct := a.flat(t)
	day := a.pkgID("1 Day")
	_, vs, err := a.svc.GenerateVouchers(day, acct.SiteID, 2)
	if err != nil {
		t.Fatal(err)
	}
	var p store.Package
	a.st.View(func(d *store.Data) { p = d.Packages[day] })
	p.Disabled = true
	if _, err := a.svc.SavePackage(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r := a.payPost("/pay/voucher", url.Values{"code": {acct.Code}, "voucher": {vs[0].Code}})
	html := body(t, r)
	if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/portal/success") {
		t.Fatalf("a voucher for a DISABLED package was refused: %d %s", r.StatusCode, html)
	}
	if cur := a.account(t, "Flat 3B"); cur.ExpiresAt.IsZero() || cur.PackageID != day {
		t.Fatalf("nothing granted: %+v", cur)
	}
	// Disabling does not make the package buyable with money.
	r = a.payPost("/pay/start", url.Values{"code": {acct.Code}, "package": {day}, "method": {"test"}, "phone": {"0712345678"}})
	if r.StatusCode == 303 {
		t.Fatal("a disabled package can be bought")
	}
	r.Body.Close()
	// A deleted package never redeems; the used voucher stays used.
	if err := a.svc.DeletePackage(context.Background(), day); err == nil {
		t.Fatal("the delete must be refused while a voucher is unused")
	}
	if v, _ := a.svc.LookupVoucher(vs[0].Code); !v.Used {
		t.Fatal("redeemed voucher not marked used")
	}
	// A voucher hidden at this building redeems there too.
	a.st.Update(func(d *store.Data) error {
		s := d.Sites[acct.SiteID]
		s.PkgOverrides = map[string]store.PkgOverride{day: {Hidden: true}}
		d.Sites[s.ID] = s
		return nil
	})
	r = a.payPost("/pay/voucher", url.Values{"code": {acct.Code}, "voucher": {vs[1].Code}})
	r.Body.Close()
	if r.StatusCode != 303 {
		t.Fatalf("a voucher for a package hidden at this building was refused: %d", r.StatusCode)
	}
}

// The Payments page "M-Pesa" tab, its count, the share and the API filter must
// see the stored methods mpesa-stk, mpesa-c2b and mpesa-manual.
func TestPaymentsMpesaTabCountShareAndFilter(t *testing.T) {
	a := newApp(t, false)
	acct := a.flat(t)
	day := a.pkgID("1 Day")
	ctx := context.Background()
	for _, c := range []struct{ key, method string }{{"RKTMP00001", "mpesa-c2b"}, {"RKTMP00002", "mpesa-stk"}, {"RKTMP00003", "mpesa-manual"}, {"CASH000001", "cash"}} {
		if _, _, err := a.svc.Credit(ctx, billing.CreditInput{Key: c.key, Method: c.method, AccountID: acct.ID, PackageID: day, AmountKES: 50}); err != nil {
			t.Fatal(err)
		}
	}
	html := a.adminGet(t, "/admin/payments?method=mpesa")
	for _, k := range []string{"RKTMP00001", "RKTMP00002", "RKTMP00003"} {
		if !strings.Contains(html, k) {
			t.Errorf("M-Pesa tab does not list %s", k)
		}
	}
	if strings.Contains(html, "CASH000001") {
		t.Error("the M-Pesa tab lists a cash payment")
	}
	if !strings.Contains(html, `?method=mpesa" aria-current="page">M-Pesa<span class="n">3</span>`) {
		i := strings.Index(html, `?method=mpesa"`)
		t.Errorf("M-Pesa tab count should be 3: %s", html[i:i+200])
	}
	// 150 of 200 this month.
	if !strings.Contains(html, "75%") {
		t.Error("M-Pesa share should be 75%")
	}
	// An exact method still filters exactly.
	if h := a.adminGet(t, "/admin/payments?method=mpesa-c2b"); !strings.Contains(h, "RKTMP00001") || strings.Contains(h, "RKTMP00002") {
		t.Error("an exact method filter must stay exact")
	}
}

// A payment for a plan slower than the active one is not downgraded silently.
func TestAssignLowerPlanAsksBeforeDowngrading(t *testing.T) {
	a := newApp(t, false)
	acct := a.flat(t)
	ctx := context.Background()
	big, small := a.pkgID("Monthly 80 Mbps"), a.pkgID("1 Day")
	if _, _, err := a.svc.Credit(ctx, billing.CreditInput{Key: "BIG0000001", Method: "mpesa-c2b", AccountID: acct.ID, PackageID: big, AmountKES: 4000}); err != nil {
		t.Fatal(err)
	}
	a.svc.RecordUnmatched(billing.UnmatchedInput{Key: "SML0000001", Method: "mpesa-c2b", AmountKES: 50, BillRef: acct.Code, AccountID: acct.ID, Note: "lower plan while active"})
	a.svc.RecordUnmatched(billing.UnmatchedInput{Key: "SML0000002", Method: "mpesa-c2b", AmountKES: 50, BillRef: acct.Code, AccountID: acct.ID, Note: "lower plan while active"})
	before := a.account(t, "Flat 3B")
	form := func(key string) url.Values {
		return url.Values{"key": {key}, "account": {acct.ID}, "package": {small}}
	}
	// 1. Without a choice: a warning page with both options, and nothing changes.
	r := a.admin("POST", "/admin/unmatched/assign", form("SML0000001"))
	html := body(t, r)
	if r.StatusCode != http.StatusConflict || !strings.Contains(html, "slower than the flat") || !strings.Contains(html, `value="extend"`) || !strings.Contains(html, `value="switch"`) {
		t.Fatalf("no warning: %d %s", r.StatusCode, html)
	}
	if cur := a.account(t, "Flat 3B"); cur.PackageID != big || !cur.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("the refusal changed the account: %+v", cur)
	}
	// 2. Add time at the current plan: the speed stays, the expiry moves by a day.
	f := form("SML0000001")
	f.Set("lower", "extend")
	a.adminPost(t, "/admin/unmatched/assign", f)
	cur := a.account(t, "Flat 3B")
	if cur.PackageID != big || cur.ExpiresAt.Sub(before.ExpiresAt) != 24*time.Hour {
		t.Fatalf("extend: package %s, expiry moved by %v", cur.PackageID, cur.ExpiresAt.Sub(before.ExpiresAt))
	}
	// 3. Switch plan on purpose.
	f = form("SML0000002")
	f.Set("lower", "switch")
	a.adminPost(t, "/admin/unmatched/assign", f)
	if cur := a.account(t, "Flat 3B"); cur.PackageID != small {
		t.Fatalf("switch: package %s", cur.PackageID)
	}
}

// A start whose answer we could not read leaves the order pending and sends the
// payer to the waiting page: the prompt may be on the phone.
func TestPayStartWithUnclearPushLeavesOrderPending(t *testing.T) {
	web.ResetPayLimiters()
	t.Cleanup(web.ResetPayLimiters)
	a := newApp(t, false)
	acct := a.flat(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/oauth/v1/generate" {
			w.Write([]byte(`{"access_token":"TOK","expires_in":"3599"}`))
			return
		}
		io.ReadAll(q.Body)
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	a.mp.SetConfig(payments.MpesaConfig{BaseURL: slow.URL, Key: "key", Secret: "secret", Shortcode: "174379", Passkey: "P",
		PublicBaseURL: "https://example.org", CallbackToken: "tok-0123456789abcdef", Enabled: true})
	a.mp.SetPushTimeout(50 * time.Millisecond)
	r := a.payPost("/pay/start", url.Values{"code": {acct.Code}, "package": {a.pkgID("1 Day")}, "method": {"mpesa"}, "phone": {"0712345678"}})
	r.Body.Close()
	loc := r.Header.Get("Location")
	if r.StatusCode != 303 || !strings.HasPrefix(loc, "/portal/wait?order=") {
		t.Fatalf("%d %s", r.StatusCode, loc)
	}
	o, ok := a.svc.GetOrder(strings.TrimPrefix(loc, "/portal/wait?order="))
	if !ok || o.Status != store.OrderPending {
		t.Fatalf("order %+v", o)
	}
	// The waiting page of an expired order says so and keeps listening for a late payment.
	if _, err := a.svc.ExpireOrders(); err != nil {
		t.Fatal(err)
	}
	a.st.Update(func(d *store.Data) error {
		o := d.Orders[o.ID]
		o.CreatedAt = time.Now().Add(-time.Hour)
		d.Orders[o.ID] = o
		return nil
	})
	if n, err := a.svc.ExpireOrders(); err != nil || n != 1 {
		t.Fatalf("expire %d %v", n, err)
	}
	code, html := a.portal(t, "/portal/wait?order="+o.ID)
	if code != 200 || !strings.Contains(html, "not received your payment yet") || strings.Contains(html, "Payment was not completed") {
		t.Fatalf("%d %s", code, html)
	}
	if _, js := a.portal(t, "/api/orders/"+o.ID); !strings.Contains(js, `"expired"`) {
		t.Fatalf("api: %s", js)
	}
}

// Voucher rows are labelled: their KES is the plan price, not money in.
func TestVoucherTabSaysItIsNotRevenue(t *testing.T) {
	a := newApp(t, false)
	acct := a.flat(t)
	if _, _, err := a.svc.GenerateVouchers(a.pkgID("1 Day"), acct.SiteID, 1); err != nil {
		t.Fatal(err)
	}
	if h := a.adminGet(t, "/admin/payments?method=voucher"); !strings.Contains(h, "not counted in") {
		t.Fatal("the voucher tab must say its amounts are not revenue")
	}
}

// The API's method=mpesa filter is the group of the stored mpesa-* methods, and
// assigning a lower plan needs an explicit choice.
func TestAPIPaymentsMpesaGroupAndLowerPlanChoice(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "M-1", "AA:BB:CC:00:61:01")
	ctx := context.Background()
	day, big := r.pkgID(t, "1 Day"), r.pkgID(t, "Monthly 80 Mbps")
	for _, c := range []struct{ key, method string }{{"APIMP00001", "mpesa-c2b"}, {"APIMP00002", "mpesa-stk"}, {"APIMP00003", "mpesa-manual"}, {"APICASH001", "cash"}} {
		if _, _, err := r.svc.Credit(ctx, billing.CreditInput{Key: c.key, Method: c.method, AccountID: a.ID, PackageID: day, AmountKES: 50}); err != nil {
			t.Fatal(err)
		}
	}
	list := r.call(r.view.Access, "GET", "/payments?method=mpesa", nil, nil).JSON(t)
	if items := list["items"].([]any); len(items) != 3 {
		t.Fatalf("method=mpesa matched %d payments, want 3: %s", len(items), jsonNum(list))
	}
	if list := r.call(r.view.Access, "GET", "/payments?method=mpesa-stk", nil, nil).JSON(t); len(list["items"].([]any)) != 1 {
		t.Fatalf("an exact method filter must stay exact: %s", jsonNum(list))
	}
	// Lower plan: refused without a choice, with the choice it is applied as asked.
	if _, _, err := r.svc.Credit(ctx, billing.CreditInput{Key: "APIBIG0001", Method: "mpesa-c2b", AccountID: a.ID, PackageID: big, AmountKES: 4000}); err != nil {
		t.Fatal(err)
	}
	r.svc.RecordUnmatched(billing.UnmatchedInput{Key: "APILOW0001", Method: "mpesa-c2b", AmountKES: 50, AccountID: a.ID})
	body := map[string]any{"account_id": a.ID, "package_id": day}
	if x := r.call(r.oper.Access, "POST", "/payments/APILOW0001/assign", body, nil); x.Status != 409 || x.errCode(t) != "lower_plan_active" {
		t.Fatalf("a lower plan without a choice: %d %s", x.Status, x.Raw)
	}
	body["lower_plan"] = "bogus"
	if x := r.call(r.oper.Access, "POST", "/payments/APILOW0001/assign", body, nil); x.Status != 400 {
		t.Fatalf("a bad choice: %d %s", x.Status, x.Raw)
	}
	before, _ := r.svc.GetAccount(a.ID)
	body["lower_plan"] = "extend"
	if x := r.call(r.oper.Access, "POST", "/payments/APILOW0001/assign", body, nil); x.Status != 200 {
		t.Fatalf("extend: %d %s", x.Status, x.Raw)
	}
	if after, _ := r.svc.GetAccount(a.ID); after.PackageID != big || after.ExpiresAt.Sub(before.ExpiresAt) != 24*time.Hour {
		t.Fatalf("extend kept the plan and added a day: %+v", after)
	}
}

// Assigning an Unmatched payment warns when the flat has a "Mark paid" payment
// of the same amount from the last hours, and needs a yes.
func TestAssignWarnsAboutAPossibleDuplicateOfMarkPaid(t *testing.T) {
	a := newApp(t, false)
	acct := a.flat(t)
	ctx := context.Background()
	day := a.pkgID("1 Day")
	o, err := a.svc.CreateOrder(billing.OrderInput{AccountID: acct.ID, PackageID: day, Phone: "254712345678", Method: "mpesa"})
	if err != nil {
		t.Fatal(err)
	}
	a.adminPost(t, "/admin/orders/markpaid", url.Values{"id": {o.ID}})
	a.svc.RecordUnmatched(billing.UnmatchedInput{Key: "RKTDUP0001", Method: "mpesa-c2b", AmountKES: 50, BillRef: acct.Code, AccountID: acct.ID})
	before := a.account(t, "Flat 3B")
	f := url.Values{"key": {"RKTDUP0001"}, "account": {acct.ID}, "package": {day}}
	r := a.admin("POST", "/admin/unmatched/assign", f)
	html := body(t, r)
	if r.StatusCode != http.StatusConflict || !strings.Contains(html, "may be money you already credited") || !strings.Contains(html, "MANUAL-") {
		t.Fatalf("no warning: %d %s", r.StatusCode, html)
	}
	if cur := a.account(t, "Flat 3B"); !cur.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("the warning credited the flat")
	}
	f.Set("dup", "1")
	a.adminPost(t, "/admin/unmatched/assign", f)
	if cur := a.account(t, "Flat 3B"); cur.ExpiresAt.Sub(before.ExpiresAt) != 24*time.Hour {
		t.Fatalf("assigned on purpose: +%v", cur.ExpiresAt.Sub(before.ExpiresAt))
	}
	_ = ctx
}
