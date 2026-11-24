package web_test

import (
	"net/url"
	"strings"
	"testing"
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
