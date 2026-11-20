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
	// The test payment completes after a short delay; let it land first, or it would
	// overwrite the package the admin forces below.
	a.tp.Wait()
	// Admin form: the force box applies a slower package.
	a.adminPost(t, "/admin/accounts/extend", url.Values{"id": {acct.ID}, "package": {a.pkgID("1 Hour")}, "force": {"1"}})
	if cur := a.account(t, "Flat 3B"); cur.PackageID != a.pkgID("1 Hour") {
		t.Errorf("forced package %s", cur.PackageID)
	}
}

// The pay form's double-submit cookie is __Host- over https (a script on a
// sibling host name cannot overwrite it); the old name is only read.
func TestPayCSRFCookieIsHostPrefixedOverHTTPS(t *testing.T) {
	a := newApp(t, false)
	get := func(https bool) *http.Response {
		req, _ := http.NewRequest("GET", a.ts.URL+"/pay", nil)
		if https {
			req.Header.Set("X-Forwarded-Proto", "https")
		}
		r, err := a.cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r
	}
	var names []string
	for _, c := range get(true).Cookies() {
		names = append(names, c.Name)
		if c.Name == "__Host-ftn_pay_csrf" && (c.Path != "/" || c.Domain != "" || !c.Secure || !c.HttpOnly) {
			t.Fatalf("cookie not host-only: %+v", c)
		}
	}
	if strings.Join(names, ",") != "__Host-ftn_pay_csrf" {
		t.Fatalf("over https wrote %v", names)
	}
	// plain http (a walled customer) cannot carry a __Host- cookie and keeps the plain name
	names = nil
	for _, c := range get(false).Cookies() {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "ftn_pay_csrf" {
		t.Fatalf("over http wrote %v", names)
	}
}

func TestPayPostsCheckOriginAndAcceptTheOldCookieOnlyAsARead(t *testing.T) {
	a := newApp(t, false)
	post := func(cookie string, hdr map[string]string) int {
		form := url.Values{"_pcsrf": {payCSRFValue}, "code": {"nope"}}
		req, _ := http.NewRequest("POST", a.ts.URL+"/pay/find", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.AddCookie(&http.Cookie{Name: cookie, Value: payCSRFValue})
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := a.cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	for _, name := range []string{"__Host-ftn_pay_csrf", "ftn_pay_csrf"} { // new name, and the old one read during the transition
		if c := post(name, nil); c == 403 {
			t.Errorf("cookie %s refused", name)
		}
	}
	for name, hdr := range map[string]map[string]string{
		"same-site fetch":  {"Sec-Fetch-Site": "same-site"},
		"cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
		"other origin":     {"Origin": "https://tech.example.co.ke"},
	} {
		if c := post("__Host-ftn_pay_csrf", hdr); c != 403 {
			t.Errorf("%s: got %d, want 403", name, c)
		}
	}
	if c := post("__Host-ftn_pay_csrf", map[string]string{"Sec-Fetch-Site": "same-origin"}); c == 403 {
		t.Error("a same-origin post was refused")
	}
}
