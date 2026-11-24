package web_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/store"
)

// wgRig is a multi-router server whose WireGuard status file says the visitor (127.0.0.1) comes
// through router "visitor": the /pay page then knows where the visitor is.
func wgRig(t *testing.T, visitor string) (*multi, store.Site, store.Site) {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "wg0.dump")
	m := newMultiOps(t, func(c *config.Config) { c.WGDumpPath = dump }, nil)
	r, o := m.addSite(t, visitor), m.addSite(t, "Beta")
	var key string
	m.st.View(func(d *store.Data) { key = d.Sites[r.ID].WGPublicKey })
	line := fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.2/32\t0\t0\t0\n", key)
	if err := os.WriteFile(dump, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return m, r, o
}

// Two apartments of one router both have unit A1: a typed "A1" is never guessed.
func TestSameUnitInTwoApartmentsOfOneRouterIsAskedNotGuessed(t *testing.T) {
	m, R, _ := wgRig(t, "Riverside")
	p1 := aptOf(t, m.st, R.ID)
	p2 := apartmentFor(t, m.st, R.ID, "Riverside Annex")
	a1, err := m.svc.CreateAccount(billing.AccountInput{ApartmentID: p1, SiteID: R.ID, Unit: "A1", MAC: "AA:BB:CC:00:E1:01"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.CreateAccount(billing.AccountInput{ApartmentID: p2, SiteID: R.ID, Unit: "A1", MAC: "AA:BB:CC:00:E1:02"})
	if err != nil {
		t.Fatal(err)
	}
	// /pay: the visitor is asked for the apartment.
	r := m.payPost("/pay/find", url.Values{"code": {"a1"}})
	html := body(t, r)
	if r.StatusCode != 409 || !strings.Contains(html, "Which apartment are you in?") || !strings.Contains(html, "Riverside Annex") || !strings.Contains(html, `name="apartment_id"`) || strings.Contains(html, "enter your account number") || strings.Contains(html, `name="code" placeholder`) {
		t.Fatalf("ambiguous label: %d %s", r.StatusCode, html)
	}
	// A loose spelling of the house number asks the same question (buttons, one per apartment).
	r = m.payPost("/pay/find", url.Values{"code": {"House a 1"}})
	if html = body(t, r); r.StatusCode != 409 || strings.Count(html, `name="apartment_id"`) != 2 {
		t.Fatalf("loose spelling of an ambiguous label: %d %s", r.StatusCode, html)
	}
	// With the apartment it resolves to that account only.
	r = m.payPost("/pay/find", url.Values{"code": {"A1"}, "apartment_id": {p2}})
	html = body(t, r)
	if r.StatusCode != 200 || strings.Contains(html, "Which apartment are you in?") {
		t.Fatalf("with the apartment: %d %s", r.StatusCode, html)
	}
	if strings.Contains(html, a1.Code) {
		t.Error("the other account's code leaked")
	}
	// The account code always works, whatever the label.
	r = m.payPost("/pay/find", url.Values{"code": {a1.Code}})
	if html = body(t, r); r.StatusCode != 200 || !strings.Contains(html, a1.Code) {
		t.Fatalf("by code: %d", r.StatusCode)
	}
	// Admin attach: a typed label that two accounts carry is refused and says so.
	l := m.adminPost(t, "/admin/devices/attach", url.Values{"site": {R.ID}, "mac": {"AA:BB:CC:00:E1:77"}, "acct": {"A1"}})
	if !strings.Contains(l, "Error") || !strings.Contains(l, "more than one") {
		t.Fatalf("attach by an ambiguous label: %s", l)
	}
	if cur, _ := m.svc.GetAccount(a1.ID); cur.MAC != "AA:BB:CC:00:E1:01" {
		t.Fatalf("the MAC was attached anyway: %+v", cur)
	}
}

// A visitor whose device is on another router than the account's is told so, and no payment starts.
func TestPayAtTheWrongRouterShowsWhereYouAreAndTakesNoPayment(t *testing.T) {
	m, R, B := wgRig(t, "Riverside")
	acct, err := m.svc.CreateAccount(billing.AccountInput{ApartmentID: aptOf(t, m.st, B.ID), SiteID: B.ID, Unit: "B1", MAC: "AA:BB:CC:00:E2:01"})
	if err != nil {
		t.Fatal(err)
	}
	r := m.payPost("/pay/find", url.Values{"code": {acct.Code}})
	html := body(t, r)
	if r.StatusCode != 409 || !strings.Contains(html, "Your device is connected at Riverside (Riverside), not at Beta. Call 0700 111 222.") {
		t.Fatalf("find: %d %s", r.StatusCode, html)
	}
	if strings.Contains(html, `name="package"`) {
		t.Error("the package step is offered")
	}
	r = m.payPost("/pay/start", url.Values{"code": {acct.Code}, "package": {m.pkgIDByName(t, "1 Day")}, "method": {"test"}, "phone": {"0712345678"}})
	html = body(t, r)
	if r.StatusCode != 409 || !strings.Contains(html, "not at Beta") {
		t.Fatalf("start: %d %s", r.StatusCode, html)
	}
	m.st.View(func(d *store.Data) {
		if len(d.Orders) != 0 {
			t.Errorf("an order was created: %v", d.Orders)
		}
	})
	// The same account paid from its own router works as before.
	own, _ := m.svc.CreateAccount(billing.AccountInput{ApartmentID: aptOf(t, m.st, R.ID), SiteID: R.ID, Unit: "R1", MAC: "AA:BB:CC:00:E2:02"})
	if r = m.payPost("/pay/find", url.Values{"code": {own.Code}}); r.StatusCode != 200 {
		t.Fatalf("own router: %d", r.StatusCode)
	}
}

func (m *multi) pkgIDByName(t *testing.T, name string) (id string) {
	t.Helper()
	m.st.View(func(d *store.Data) {
		for _, p := range d.Packages {
			if p.Name == name {
				id = p.ID
			}
		}
	})
	if id == "" {
		t.Fatalf("no package %q", name)
	}
	return id
}

// Owner and operator split: deleting an apartment is owner only, moving an account is operator work
// (web and API), and the old contacts addresses answer 410 to every role.
func TestRolesForApartmentDeleteAccountMoveAndGoneContacts(t *testing.T) {
	r := newAPIRig(t)
	apt := r.call(r.owner.Access, "POST", "/apartments", map[string]any{"name": "Spare"}, nil)
	if apt.Status != 201 && apt.Status != 200 {
		t.Fatalf("create: %d %s", apt.Status, apt.Raw)
	}
	spare := str(apt.JSON(t), "id")
	if x := r.call(r.oper.Access, "DELETE", "/apartments/"+spare, nil, nil); x.Status != 403 {
		t.Errorf("API delete as operator: %d", x.Status)
	}
	// Account move as operator, API.
	a := r.acct(t, r.A, "A-1", "AA:BB:CC:00:E3:01")
	if x := r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/move", map[string]any{"unit": "A-2"}, nil); x.Status != 200 {
		t.Errorf("API move as operator: %d %s", x.Status, x.Raw)
	}
	ob := signIn(t, r.ts, r.srv, r.st, r.operW)
	post := func(path string, f url.Values) int {
		resp := ob.Do(r.cl, r.ts.URL, "POST", path, f)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("/admin/apartments/delete", url.Values{"id": {spare}}); c != 403 {
		t.Errorf("web delete as operator: %d", c)
	}
	if c := post("/admin/accounts/move", url.Values{"id": {a.ID}, "apartment_id": {aptOf(t, r.st, r.A.ID)}, "unit": {"A-3"}}); c == 403 {
		t.Errorf("web account move as operator is refused: %d", c)
	}
	if c := post("/admin/sites/contacts/save", url.Values{"site": {r.A.ID}, "role": {"caretaker"}, "name": {"x"}}); c != 410 {
		t.Errorf("old contacts address as operator: %d", c)
	}
	// The owner can delete the (empty) apartment.
	if x := r.call(r.owner.Access, "DELETE", "/apartments/"+spare, nil, nil); x.Status != 204 && x.Status != 200 {
		t.Errorf("API delete as owner: %d %s", x.Status, x.Raw)
	}
}
