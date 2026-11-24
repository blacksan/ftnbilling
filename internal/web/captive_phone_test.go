package web_test

import (
	"net/url"
	"strings"
	"testing"
)

// Money: a number typed into the "other number" box is never ignored. It is used when the visitor chose "another
// number" (or the form sent no choice), and refused with a clear message when "the number on file" is selected, so
// a prompt never goes to a number the visitor did not choose.
// (Four requests at most: the per-account limit is four.)
func TestPayTypedNumberIsNeverIgnored(t *testing.T) {
	c := newCaptive(t, true)
	day := c.pkg("1 Day")
	start := func(extra url.Values) (int, string) {
		f := url.Values{"code": {c.acct.Code}, "package": {day}, "method": {"mpesa"}}
		for k, v := range extra {
			f[k] = v
		}
		st, html, _ := c.pay(t, "/pay/start", f)
		return st, html
	}

	// "Use the number on file" with another number typed: refused, nothing sent, the typed number stays on screen.
	st, html := start(url.Values{"phone_src": {"file"}, "phone": {"0799 111 222"}})
	if st != 400 || c.daraja.pubs != 0 {
		t.Fatalf("file + typed other number: %d, pushes %d", st, c.daraja.pubs)
	}
	if !strings.Contains(html, "Choose which number pays") || !strings.Contains(html, `value="0799 111 222"`) {
		t.Errorf("no clear message or typed number lost:\n%s", html)
	}
	// Another number chosen: that number.
	if st, _ = start(url.Values{"phone_src": {"other"}, "phone": {"0799 111 222"}}); st != 303 {
		t.Fatalf("other: %d", st)
	}
	if got := c.daraja.lastSTK()["PhoneNumber"]; got != "254799111222" {
		t.Errorf("pushed to %v, want the typed number", got)
	}
	// No choice sent (a form from before) but a number typed: the typed number is the choice.
	if st, _ = start(url.Values{"phone": {"0733 000 111"}}); st != 303 {
		t.Fatalf("no choice + typed: %d", st)
	}
	if got := c.daraja.lastSTK()["PhoneNumber"]; got != "254733000111" {
		t.Errorf("pushed to %v, want the typed number", got)
	}
	// "Another number" chosen but nothing typed: asked, not charged to the number on file.
	n := c.daraja.pubs
	if st, html = start(url.Values{"phone_src": {"other"}}); st != 400 || c.daraja.pubs != n || !strings.Contains(html, "Type the number") {
		t.Errorf("other without a number: %d pushes %d->%d", st, n, c.daraja.pubs)
	}
}
