package web_test

import (
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// An active account on a fast plan sees ONE notice above the packages, and a short tag on each package it cannot
// buy yet, not the same sentence on every card.
func TestPayBlockedPackagesShowOneNotice(t *testing.T) {
	c := newCaptive(t, true)
	if err := c.svc.ExtendAccount(t.Context(), c.acct.ID, c.pkg("Monthly 80 Mbps"), "", false); err != nil {
		t.Fatal(err)
	}
	_, html, _ := c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	if n := strings.Count(html, "Your plan runs until"); n != 1 {
		t.Errorf("want one notice, got %d:\n%s", n, html)
	}
	if strings.Contains(html, "Available when") || !strings.Contains(html, `class="why">From `) {
		t.Errorf("blocked cards should carry only a short tag:\n%s", html)
	}
}

// Tenants see the apartment (building) name under the logo, never the router's name.
func TestPayShowsApartmentNotRouterName(t *testing.T) {
	c := newCaptive(t, true)
	c.st.Update(func(d *store.Data) error {
		s := d.Sites[c.site.ID]
		s.Name = "Riverside RB5009"
		d.Sites[c.site.ID] = s
		return nil
	})
	check := func(what, html string) {
		t.Helper()
		if strings.Contains(html, "RB5009") || !strings.Contains(html, "Riverside Court") {
			t.Errorf("%s: apartment name expected, router name hidden:\n%s", what, html)
		}
	}
	check("landing", body(t, c.visit("GET", "/pay", "isp.ftw.co.ke", nil)))
	_, html, _ := c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	check("package step", html)
	check("register", body(t, c.visit("GET", "/pay/register", "isp.ftw.co.ke", nil)))
	_, _, resp := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone_src": {"file"}})
	check("wait", body(t, c.visit("GET", resp.Header.Get("Location"), "isp.ftw.co.ke", nil)))
}

// "4B" finds "Flat 4B": case, spaces and a leading word (Flat, House, Hse, No.) do not matter, and the typed value
// stays in the box after a miss.
func TestPayFindsHouseNumberLoosely(t *testing.T) {
	c := newCaptive(t, true)
	for _, typed := range []string{"Flat 4B", "4B", "4b", " 4 b ", "House 4B", "hse 4b", "No. 4B", "flat4b", "Flat-4B"} {
		st, html, _ := c.pay(t, "/pay/find", url.Values{"code": {typed}})
		if st != 200 || !strings.Contains(html, "Choose a package") {
			t.Errorf("%q not found: %d", typed, st)
		}
		if strings.Contains(html, c.acct.Code) {
			t.Errorf("%q: the account code must not appear when found by house number", typed)
		}
	}
	for _, typed := range []string{"4C", "B4", "Flat"} {
		st, html, _ := c.pay(t, "/pay/find", url.Values{"code": {typed}})
		if st != 404 || !strings.Contains(html, `value="`+typed+`"`) || !strings.Contains(html, "could not find") {
			t.Errorf("%q: %d (typed value must stay in the box)", typed, st)
		}
	}
}

// Prices on customer pages carry a thousands separator, and "M-Pesa" is kept in one piece.
func TestPayPriceAndMpesaWording(t *testing.T) {
	c := newCaptive(t, true)
	c.st.Update(func(d *store.Data) error {
		for id, p := range d.Packages {
			if p.Name == "Monthly 80 Mbps" {
				p.PriceKES = 12500
				d.Packages[id] = p
			}
		}
		return nil
	})
	_, html, _ := c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	if !strings.Contains(html, "KES 12,500") || strings.Contains(html, "KES 12500") {
		t.Errorf("price without a thousands separator:\n%s", html)
	}
	if !strings.Contains(html, `<span class="nb">M-Pesa</span>`) {
		t.Error("M-Pesa may break at its hyphen")
	}
	// The active account's page says "Top up", the expired one "paused".
	if !strings.Contains(html, "Your internet is paused") || strings.Contains(html, "Top up or change package") {
		t.Error("an expired account is paused")
	}
	if err := c.svc.ExtendAccount(t.Context(), c.acct.ID, c.pkg("1 Day"), "", false); err != nil {
		t.Fatal(err)
	}
	_, html, _ = c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
	if !strings.Contains(html, "Top up or change package") || strings.Contains(html, "Your internet is paused") {
		t.Errorf("an active account is not paused:\n%s", html)
	}
}

// "Try again" after a failed payment comes back to the same account with the same package and paying number chosen.
func TestPayTryAgainIsPrefilled(t *testing.T) {
	c := newCaptive(t, true)
	day := c.pkg("1 Day")
	o, err := c.svc.CreateOrder(billing.OrderInput{AccountID: c.acct.ID, PackageID: day, Phone: "254799111222", Method: "mpesa"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.svc.FailOrder(t.Context(), o.ID, billing.ReasonPromptNotSent); err != nil {
		t.Fatal(err)
	}
	wait := body(t, c.visit("GET", "/portal/wait?order="+o.ID, "isp.ftw.co.ke", nil))
	if !strings.Contains(wait, `href="/pay/again?order=`+o.ID+`"`) {
		t.Fatalf("Try again does not carry the order:\n%s", wait)
	}
	r := c.visit("GET", "/pay/again?order="+o.ID, "isp.ftw.co.ke", nil)
	html := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(html, "Flat 4B") || !strings.Contains(html, `value="`+day+`" checked`) ||
		!strings.Contains(html, `value="0799111222"`) {
		t.Errorf("not prefilled: %d\n%s", r.StatusCode, html)
	}
	// An order id must not turn a house-number visit into an account-code one: no code, no number on file.
	if strings.Contains(html, c.acct.Code) || strings.Contains(html, "number on file") {
		t.Errorf("retry page shows the account code or offers the number on file:\n%s", html)
	}
	// An attempt paid from the number on file does not put that number on the page.
	onFile, err := billing.NormalizePhone(c.acct.Phone)
	if err != nil {
		t.Fatal(err)
	}
	o2, err := c.svc.CreateOrder(billing.OrderInput{AccountID: c.acct.ID, PackageID: day, Phone: onFile, Method: "mpesa"})
	if err != nil {
		t.Fatal(err)
	}
	html = body(t, c.visit("GET", "/pay/again?order="+o2.ID, "isp.ftw.co.ke", nil))
	if strings.Contains(html, "0"+onFile[3:]) || strings.Contains(html, onFile) {
		t.Errorf("retry page carries the number on file:\n%s", html)
	}
	// An unknown order goes to the landing page.
	r = c.visit("GET", "/pay/again?order=nope", "isp.ftw.co.ke", nil)
	r.Body.Close()
	if r.StatusCode != 303 || r.Header.Get("Location") != "/pay" {
		t.Errorf("unknown order: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

// The form addresses opened with GET (history, a shared link, a reload) lead back to the payment page.
func TestPayFormAddressesRedirectOnGet(t *testing.T) {
	c := newCaptive(t, true)
	for _, p := range []string{"/pay/start", "/pay/find", "/pay/voucher"} {
		r := c.visit("GET", p, "isp.ftw.co.ke", nil)
		r.Body.Close()
		if r.StatusCode != 303 || r.Header.Get("Location") != "/pay" {
			t.Errorf("GET %s: %d %q", p, r.StatusCode, r.Header.Get("Location"))
		}
	}
}

// After a voucher the success page does not talk about Paybill; after an M-Pesa payment it does.
func TestSuccessPageAfterVoucherHasNoPaybill(t *testing.T) {
	c := newCaptive(t, true)
	_, vs, err := c.svc.GenerateVouchers(c.pkg("1 Day"), c.site.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, _, resp := c.pay(t, "/pay/voucher", url.Values{"code": {c.acct.Code}, "voucher": {vs[0].Code}})
	html := body(t, c.visit("GET", resp.Header.Get("Location"), "isp.ftw.co.ke", nil))
	if strings.Contains(html, "Paybill") || !strings.Contains(html, "Your account number") || !strings.Contains(html, c.acct.Code) {
		t.Errorf("success after a voucher:\n%s", html)
	}
}

// Registration hints are helper text under the field (visible whole), not long placeholders that get cut off.
func TestRegisterHintsAreHelperText(t *testing.T) {
	c := newCaptive(t, true)
	html := body(t, c.visit("GET", "/pay/register", "isp.ftw.co.ke", nil))
	if strings.Contains(html, `placeholder="as shown`) || strings.Contains(html, `placeholder="for example`) ||
		!strings.Contains(html, `class="hint">As shown in your phone's <span class="nb">Wi-Fi</span> settings, for example Wanjiru`) {
		t.Errorf("hints:\n%s", html)
	}
}
