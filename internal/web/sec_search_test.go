package web_test

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// natIDRig is an API rig with DATA_KEY set and one customer whose national ID is stored.
func natIDRig(t *testing.T) (*apiRig, string, string) {
	t.Helper()
	r := newAPIRig(t)
	if err := r.svc.SetDataKey([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	c, err := r.svc.SaveCustomer(billing.CustomerInput{Name: "Peter Mwangi", Phone: "0733000222"})
	if err != nil {
		t.Fatal(err)
	}
	secret := "29417305"
	if err := r.svc.SetNationalID(c.ID, secret); err != nil {
		t.Fatal(err)
	}
	return r, c.ID, secret
}

// Ported from the security review (H-1): a viewer who knows the last four digits
// used to recover the whole ID in 2,942 GETs. It must now fail for viewers and
// operators, and the sweep must hit the rate limit long before it could finish.
func TestSearchCannotRecoverNationalIDForViewerOrOperator(t *testing.T) {
	r, _, secret := natIDRig(t)
	last4 := secret[4:]
	for name, tok := range map[string]string{"viewer": r.view.Access, "operator": r.oper.Access} {
		found, limited, tries := "", false, 0
		for p := 0; p < 10000 && found == "" && !limited; p++ {
			q := fmt.Sprintf("%04d%s", p, last4)
			tries++
			rep := r.call(tok, "GET", "/search?q="+url.QueryEscape(q), nil, nil)
			switch rep.Status {
			case 200:
				if cs, _ := rep.JSON(t)["customers"].([]any); len(cs) > 0 {
					found = q
				}
			case 429:
				limited = true
				if rep.errCode(t) != "rate_limited" || rep.Header.Get("Retry-After") == "" {
					t.Fatalf("%s: 429 without the envelope: %s", name, rep.Raw)
				}
			default:
				t.Fatalf("%s search: %d %s", name, rep.Status, rep.Raw)
			}
		}
		if found != "" {
			t.Fatalf("%s recovered national ID %s in %d GETs", name, found, tries)
		}
		if !limited {
			t.Errorf("%s: %d searches were never rate limited", name, tries)
		}
	}
}

func TestSearchByNationalIDIsOwnerOnlyAndAudited(t *testing.T) {
	r, cid, secret := natIDRig(t)
	for name, tok := range map[string]string{"viewer": r.view.Access, "operator": r.oper.Access} {
		rep := r.call(tok, "GET", "/search?q="+secret, nil, nil)
		if rep.Status != 200 || len(rep.JSON(t)["customers"].([]any)) != 0 {
			t.Errorf("%s found the customer by national ID: %d %s", name, rep.Status, rep.Raw)
		}
	}
	rep := r.call(r.owner.Access, "GET", "/search?q="+secret, nil, nil)
	if rep.Status != 200 || len(rep.JSON(t)["customers"].([]any)) != 1 {
		t.Fatalf("owner: %d %s", rep.Status, rep.Raw)
	}
	if !strings.Contains(r.auditText(), "national ID matched by search for customer "+cid) {
		t.Error("the owner's national-ID search is not in the audit trail")
	}
	if strings.Contains(r.auditText(), secret) {
		t.Error("the ID itself must never be written to the audit trail")
	}
}

func (r *apiRig) auditText() string {
	var b strings.Builder
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" {
				b.WriteString(e.Msg + "\n")
			}
		}
	})
	return b.String()
}

func TestSearchAPIRateLimitIsPerUser(t *testing.T) {
	r, _, _ := natIDRig(t)
	n429 := 0
	for i := 0; i < 130; i++ {
		if r.call(r.view.Access, "GET", "/search?q=peter", nil, nil).Status == 429 {
			n429++
		}
	}
	if n429 == 0 {
		t.Error("130 searches in a row were not limited")
	}
	if rep := r.call(r.owner.Access, "GET", "/search?q=peter", nil, nil); rep.Status != 200 {
		t.Errorf("another user was limited too: %d", rep.Status)
	}
}

func (r *apiRig) webGet(t *testing.T, b *browser, path string) (int, string) {
	t.Helper()
	cl := &http.Client{CheckRedirect: noRedirect}
	resp := b.Do(cl, r.ts.URL, "GET", path, nil)
	defer resp.Body.Close()
	bs, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(bs)
}

// The last four digits are shown to owners only; everyone else sees "on file".
func TestLastFourOfNationalIDHiddenFromViewersAndOperators(t *testing.T) {
	r, cid, secret := natIDRig(t)
	last4 := secret[4:]
	for name, w := range map[string]*who{"viewer": r.vwW, "operator": r.operW, "owner": r.ownerW} {
		b := signIn(t, r.ts, r.srv, r.st, w)
		for _, p := range []string{"/admin/customers?state=all", "/admin/customers/view?id=" + cid} {
			code, page := r.webGet(t, b, p)
			if code != 200 {
				t.Fatalf("%s %s: %d", name, p, code)
			}
			if name == "owner" {
				if !strings.Contains(page, last4) {
					t.Errorf("owner should still see the last four on %s", p)
				}
				continue
			}
			if strings.Contains(page, last4) || strings.Contains(page, "ending") {
				t.Errorf("%s sees part of the ID on %s", name, p)
			}
			if !strings.Contains(page, "on file") && !strings.Contains(strings.ToLower(page), "on file") {
				t.Errorf("%s: %s should say the ID is on file", name, p)
			}
		}
	}
}

// Web search: owner-only ID match, and a 429 once a user searches too often.
func TestWebSearchNationalIDAndRateLimit(t *testing.T) {
	r, cid, secret := natIDRig(t)
	vb := signIn(t, r.ts, r.srv, r.st, r.vwW)
	ob := signIn(t, r.ts, r.srv, r.st, r.ownerW)
	code, page := r.webGet(t, vb, "/admin/search?q="+secret)
	if code != 200 || strings.Contains(page, "Peter Mwangi") {
		t.Errorf("viewer web search by ID: %d (found=%v)", code, strings.Contains(page, "Peter Mwangi"))
	}
	code, _ = r.webGet(t, ob, "/admin/search?q="+secret)
	if code != http.StatusSeeOther { // a single hit redirects to the customer
		t.Errorf("owner web search by ID: %d", code)
	}
	if !strings.Contains(r.auditText(), "national ID matched by search for customer "+cid) {
		t.Error("web national-ID search not audited")
	}
	limited := false
	for i := 0; i < 130 && !limited; i++ {
		c, _ := r.webGet(t, vb, "/admin/search.json?q=pe")
		limited = c == http.StatusTooManyRequests
	}
	if !limited {
		t.Error("the palette search was never limited")
	}
}
