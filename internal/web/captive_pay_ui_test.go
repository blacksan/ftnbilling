package web_test

import (
	"net/url"
	"strings"
	"testing"

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
