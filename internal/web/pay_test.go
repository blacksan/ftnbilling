package web_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const payCSRFValue = "0123456789abcdef0123456789"

// payPost is a visitor's POST to the public /pay flow: the double-submit
// cookie and form field carry the same token.
func (a *app) payPost(path string, form url.Values) *http.Response {
	form.Set("_pcsrf", payCSRFValue)
	req, _ := http.NewRequest("POST", a.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ftn_pay_csrf", Value: payCSRFValue})
	r, err := a.cl.Do(req)
	if err != nil {
		panic(err)
	}
	return r
}

// A voucher made for one building is refused at every other building, and the
// refused attempt does not use it up.
func TestVoucherFromAnotherBuildingIsRefused(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	siteB := seedSite(t, a.st, "Block B")
	day := a.pkgID("1 Day")
	_, vsB, err := a.svc.GenerateVouchers(day, siteB, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := a.payPost("/pay/voucher", url.Values{"code": {acct.Code}, "voucher": {vsB[0].Code}})
	html := body(t, r)
	if r.StatusCode != 400 || !strings.Contains(html, "invalid or already used") {
		t.Fatalf("B's voucher at A: %d %s", r.StatusCode, html)
	}
	if v, _ := a.svc.LookupVoucher(vsB[0].Code); v.Used {
		t.Fatal("the refused attempt used the voucher up")
	}
	if cur := a.account(t, "Flat 3B"); !cur.ExpiresAt.IsZero() {
		t.Fatalf("nothing may be granted: %+v", cur)
	}
	// A voucher of this building (or one for any building) works.
	_, vsA, err := a.svc.GenerateVouchers(day, acct.SiteID, 1)
	if err != nil {
		t.Fatal(err)
	}
	r = a.payPost("/pay/voucher", url.Values{"code": {acct.Code}, "voucher": {vsA[0].Code}})
	r.Body.Close()
	if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/portal/success") {
		t.Fatalf("own voucher: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if cur := a.account(t, "Flat 3B"); cur.ExpiresAt.IsZero() {
		t.Fatal("the voucher granted nothing")
	}
	// Used once, it is refused the second time with the same words.
	r = a.payPost("/pay/voucher", url.Values{"code": {acct.Code}, "voucher": {vsA[0].Code}})
	if r.StatusCode != 400 {
		t.Fatalf("reuse: %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestPayRefusesTestMethodWhenNotTestMode(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	r := a.payPost("/pay/start", url.Values{"code": {acct.Code}, "package": {a.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0712345678"}})
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("status %d", r.StatusCode)
	}
	// Without the double-submit token nothing starts at all.
	req, _ := http.NewRequest("POST", a.ts.URL+"/pay/start", strings.NewReader("code="+acct.Code))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r2, _ := a.cl.Do(req)
	r2.Body.Close()
	if r2.StatusCode != 403 {
		t.Errorf("no token: %d", r2.StatusCode)
	}
}

func TestPayRejectsSlowerPlanWhileActiveServerSide(t *testing.T) {
	a := newApp(t, true)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	if err := a.svc.ExtendAccount(t.Context(), acct.ID, a.pkgID("Monthly 40 Mbps"), "", false); err != nil {
		t.Fatal(err)
	}
	r := a.payPost("/pay/start", url.Values{"code": {acct.Code}, "package": {a.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0712345678"}})
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("server-side downgrade status %d", r.StatusCode)
	}
	r = a.payPost("/pay/start", url.Values{"code": {acct.Code}, "package": {a.pkgID("Monthly 80 Mbps")}, "method": {"test"}, "phone": {"0712345678"}})
	r.Body.Close()
	if r.StatusCode != 303 {
		t.Errorf("upgrade status %d", r.StatusCode)
	}
	// Admin form: the force box applies a slower package.
	a.adminPost(t, "/admin/accounts/extend", url.Values{"id": {acct.ID}, "package": {a.pkgID("1 Hour")}, "force": {"1"}})
	if cur := a.account(t, "Flat 3B"); cur.PackageID != a.pkgID("1 Hour") {
		t.Errorf("forced package %s", cur.PackageID)
	}
}
