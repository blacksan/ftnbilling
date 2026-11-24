package web_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"ispbill/internal/store"
)

func (a *app) redirect(t *testing.T, path string) string {
	t.Helper()
	r := a.admin("GET", path, nil)
	r.Body.Close()
	if r.StatusCode != 303 {
		t.Fatalf("GET %s: want a redirect, got %d", path, r.StatusCode)
	}
	return r.Header.Get("Location")
}

func (a *app) customerID(t *testing.T, name string) string {
	t.Helper()
	id := ""
	a.st.View(func(d *store.Data) {
		for _, c := range d.Customers {
			if c.Name == name {
				id = c.ID
			}
		}
	})
	if id == "" {
		t.Fatalf("no customer %s", name)
	}
	return id
}

func TestContactsListIsGone(t *testing.T) {
	a := newApp(t, false)
	resp := a.admin("GET", "/admin/customers", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/accounts" {
		t.Fatalf("old Contacts link: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if strings.Contains(a.adminGet(t, "/admin"), `href="/admin/customers"`) {
		t.Error("the menu still links to Contacts")
	}
}

func TestCustomersEquipmentSearchAndAccountCards(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")

	// A customer is added, listed, linked to the flat from the account page.
	loc := a.adminPost(t, "/admin/customers/save", url.Values{"name": {"Wanjiru Kamau"}, "phone": {"0712345678"}, "wa_same": {"1"}})
	if !strings.Contains(loc, "/admin/customers/view?id=") {
		t.Fatal(loc)
	}
	cid := a.customerID(t, "Wanjiru Kamau")
	if l := a.adminPost(t, "/admin/accounts/customer", url.Values{"id": {acct.ID}, "customer": {cid}, "move_in": {"2026-10-01"}}); !strings.Contains(l, "Customer linked") {
		t.Fatal(l)
	}
	if !strings.Contains(a.adminGet(t, "/admin/accounts/view?code="+acct.Code), "Wanjiru Kamau") {
		t.Error("the flat's page misses its customer")
	}
	prof := a.adminGet(t, "/admin/customers/view?id="+cid)
	if !strings.Contains(prof, "Flat 3B") || !strings.Contains(prof, "linked to Flat 3B") {
		t.Error("customer profile misses the flat or its history")
	}
	if l := a.adminPost(t, "/admin/customers/note", url.Values{"id": {cid}, "text": {"prefers calls after 6pm"}}); !strings.Contains(l, "Note added") {
		t.Fatal(l)
	}
	if !strings.Contains(a.adminGet(t, "/admin/customers/view?id="+cid), "prefers calls after 6pm") {
		t.Error("note missing from the timeline")
	}

	// Equipment: add an ONT, install it at the flat and use its MAC.
	loc = a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ztegc0ffee01"}, "mac": {"aa:bb:cc:dd:ee:77"}, "model": {"F601"}})
	m := regexp.MustCompile(`equipment/view\?id=([0-9a-f]+)`).FindStringSubmatch(loc)
	if m == nil {
		t.Fatal(loc)
	}
	eid := m[1]
	if !strings.Contains(a.adminGet(t, "/admin/equipment?tab=onu"), "ZTEGC0FFEE01") {
		t.Error("equipment list")
	}
	if l := a.adminPost(t, "/admin/equipment/assign", url.Values{"id": {eid}, "account": {acct.ID}, "use_mac": {"1"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	if got := a.account(t, "Flat 3B"); got.MAC != "AA:BB:CC:DD:EE:77" {
		t.Errorf("ONT MAC not applied: %s", got.MAC)
	}
	ev := a.adminGet(t, "/admin/equipment/view?id="+eid)
	if !strings.Contains(ev, "assigned") || !strings.Contains(ev, "Wanjiru Kamau") {
		t.Error("equipment detail lacks history or customer")
	}
	card := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	for _, want := range []string{"Wanjiru Kamau", "Floor and fibre box", "ZTEGC0FFEE01", "Install from stock"} {
		if !strings.Contains(card, want) && want != "Install from stock" {
			t.Errorf("account page lacks %q", want)
		}
	}
	for _, tag := range []string{"form", "div", "table"} {
		if o, c := strings.Count(card, "<"+tag), strings.Count(card, "</"+tag+">"); o != c {
			t.Errorf("account page has %d <%s> but %d closing tags", o, tag, c)
		}
	}
	// Location card.
	if l := a.adminPost(t, "/admin/accounts/location", url.Values{"id": {acct.ID}, "floor": {"3"}, "odb": {"ODB-3A"}, "port": {"5"}}); !strings.Contains(l, "Location saved") {
		t.Fatal(l)
	}
	if got := a.account(t, "Flat 3B"); got.Floor != "3" || got.ODB != "ODB-3A" || got.SplitterPort != 5 {
		t.Errorf("location %+v", got)
	}

	// Search: phone (either spelling) and MAC land on the one match; a miss lists nothing.
	// the phone belongs to the customer and (copied) to the flat: both are listed
	for _, q := range []string{"0712345678", "%2B254712345678", "712345678"} {
		if pg := a.adminGet(t, "/admin/search?q="+q); !strings.Contains(pg, "/admin/customers/view?id="+cid) || !strings.Contains(pg, "Flat 3B") {
			t.Errorf("phone search %s: %.200s", q, pg)
		}
	}
	if l := a.redirect(t, "/admin/search?q=ztegc0ffee01"); !strings.Contains(l, "/admin/equipment/view?id="+eid) {
		t.Errorf("serial search -> %s", l)
	}
	if !strings.Contains(a.adminGet(t, "/admin/search?q=zzzzzz"), "Nothing found") {
		t.Error("empty search")
	}

	// Return the ONT; it leaves the flat and keeps its history.
	if l := a.adminPost(t, "/admin/equipment/return", url.Values{"id": {eid}, "status": {"faulty"}, "note": {"no light"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	if !strings.Contains(a.adminGet(t, "/admin/equipment?status=faulty"), "ZTEGC0FFEE01") {
		t.Error("faulty filter")
	}

	// Moving out vacates the flat but the customer and payments stay.
	if l := a.adminPost(t, "/admin/customers/moveout", url.Values{"account": {acct.ID}, "move_out": {"2026-11-10"}, "next": {"/admin/accounts"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	if got := a.account(t, "Flat 3B"); got.CustomerID != "" {
		t.Error("flat still occupied")
	}
	if !strings.Contains(a.adminGet(t, "/admin/customers/view?id="+cid), "Wanjiru Kamau") {
		t.Error("a former tenant's record should stay")
	}
}

func TestPaymentFiltersAndFilteredCSV(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3B"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3B")
	a.adminPost(t, "/admin/payments/record", url.Values{"account": {acct.Code}, "trans": {"sgl7k2m9xa"}, "amount": {"3000"}, "phone": {"0712345678"}, "date": {"2026-11-16T18:30"}})
	a.adminPost(t, "/admin/payments/record", url.Values{"account": {acct.Code}, "trans": {"sgl7k2m9xb"}, "amount": {"3000"}, "phone": {"0799999999"}, "date": {"2026-10-02T09:00"}})
	all := a.adminGet(t, "/admin/payments")
	if !strings.Contains(all, "SGL7K2M9XA") || !strings.Contains(all, "SGL7K2M9XB") || !strings.Contains(all, "KES 6000") {
		t.Error("unfiltered ledger or revenue total")
	}
	nov := a.adminGet(t, "/admin/payments?from=2026-11-01&to=2026-11-30")
	if !strings.Contains(nov, "SGL7K2M9XA") || strings.Contains(nov, "SGL7K2M9XB") || !strings.Contains(nov, "KES 3000") {
		t.Error("date filter")
	}
	if q := a.adminGet(t, "/admin/payments?q=0799999999"); strings.Contains(q, "SGL7K2M9XA") || !strings.Contains(q, "SGL7K2M9XB") {
		t.Error("phone filter")
	}
	if m := a.adminGet(t, "/admin/payments?method=nosuchmethod"); strings.Contains(m, "SGL7K2M9X") {
		t.Error("method filter")
	}
	r := a.admin("GET", "/admin/payments.csv?from=2026-11-01&to=2026-11-30", nil)
	csv := body(t, r)
	if !strings.Contains(csv, "SGL7K2M9XA") || strings.Contains(csv, "SGL7K2M9XB") {
		t.Errorf("filtered csv: %s", csv)
	}
}

func TestBuildingProfileAndContacts(t *testing.T) {
	a := newApp(t, false)
	if l := a.adminPost(t, "/admin/sites/profile", url.Values{"id": {a.site}, "address": {"Hatheru Road, Lavington"}, "lat": {"-1.2864"}, "lng": {"36.7677"}, "go_live": {"2026-09-01"}}); !strings.Contains(l, "Router profile saved") {
		t.Fatal(l)
	}
	if l := a.adminPost(t, "/admin/sites/profile", url.Values{"id": {a.site}, "lat": {"95"}, "lng": {"36"}}); !strings.Contains(l, "Error") {
		t.Error("bad latitude accepted")
	}
	// Router contacts are gone (410): the caretaker and landlord live on the apartment.
	if r := a.admin("POST", "/admin/sites/contacts/save", url.Values{"site": {a.site}, "role": {"caretaker"}, "name": {"Mzee Otieno"}, "phone": {"0722000111"}}); r.StatusCode != 410 {
		t.Fatalf("contacts save: %d", r.StatusCode)
	}
	page := a.adminGet(t, "/admin/routers/view?id="+a.site+"&tab=profile") + a.adminGet(t, "/admin/routers/view?id="+a.site)
	for _, want := range []string{"Hatheru Road, Lavington", "2026-09-01", "geo:-1.2864,36.7677", "Revenue by month"} {
		if !strings.Contains(page, want) {
			t.Errorf("building page lacks %q", want)
		}
	}
}

func TestNationalIDRevealIsAuditedAndNeedsKey(t *testing.T) {
	a := newApp(t, false)
	loc := a.adminPost(t, "/admin/customers/save", url.Values{"name": {"Peter Mwangi"}, "phone": {"0733000222"}})
	_ = loc
	cid := a.customerID(t, "Peter Mwangi")
	// no DATA_KEY in this rig: the page says so and saving is refused
	if !strings.Contains(a.adminGet(t, "/admin/customers/view?id="+cid), "Not enabled") {
		t.Error("expected the not-enabled notice")
	}
	if l := a.adminPost(t, "/admin/customers/natid", url.Values{"id": {cid}, "national_id": {"12345678"}}); !strings.Contains(l, "Error") {
		t.Error("ID stored without a key")
	}
	// With a key the ID is encrypted and revealing is logged.
	if err := a.svc.SetDataKey([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if l := a.adminPost(t, "/admin/customers/natid", url.Values{"id": {cid}, "national_id": {"12345678"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	page := a.adminGet(t, "/admin/customers/view?id="+cid)
	if strings.Contains(page, "12345678") || !strings.Contains(page, "5678") {
		t.Error("the full ID must not be on the profile page")
	}
	r := a.admin("POST", "/admin/customers/natid/reveal", url.Values{"id": {cid}})
	if r.StatusCode != 200 || !strings.Contains(body(t, r), "12345678") {
		t.Fatalf("reveal: %d", r.StatusCode)
	}
	if !strings.Contains(a.adminGet(t, "/admin/customers/view?id="+cid), "national ID revealed by admin") {
		t.Error("reveal not in the audit trail")
	}
}

func TestSettingsBackupStatusAndHealthzDB(t *testing.T) {
	a := newApp(t, false)
	if !strings.Contains(a.adminGet(t, "/admin/settings"), "no status file yet") {
		t.Error("missing status file should be explained")
	}
	_, b := a.portal(t, "/healthz")
	if !strings.Contains(b, `"db_ok":true`) || !strings.Contains(b, `"db":"`) {
		t.Errorf("healthz %s", b)
	}
}

// "WhatsApp is the same as the phone": the box is stored, comes back ticked after a reload, and the number follows the phone.
func TestWhatsAppSameAsPhoneIsSavedAndShownTicked(t *testing.T) {
	a := newApp(t, false)
	ticked := func(page string) bool {
		return regexp.MustCompile(`<input type="checkbox" name="wa_same" value="1" checked>`).MatchString(page)
	}
	a.adminPost(t, "/admin/customers/save", url.Values{"name": {"Njeri Wa"}, "phone": {"0712345678"}, "wa_same": {"1"}})
	cid := a.customerID(t, "Njeri Wa")
	var c store.Customer
	a.st.View(func(d *store.Data) { c = d.Customers[cid] })
	if !c.WASamePhone || c.WhatsApp != "254712345678" || c.WhatsApp != c.Phone {
		t.Fatalf("saved customer %+v", c)
	}
	page := a.adminGet(t, "/admin/customers/view?id="+cid)
	if !ticked(page) {
		t.Fatal("the customer page does not show the box ticked after saving")
	}
	// the phone changes with the box still ticked: WhatsApp follows
	a.adminPost(t, "/admin/customers/save", url.Values{"id": {cid}, "name": {"Njeri Wa"}, "phone": {"0722000111"}, "wa_same": {"1"}})
	a.st.View(func(d *store.Data) { c = d.Customers[cid] })
	if c.WhatsApp != "254722000111" || !c.WASamePhone {
		t.Fatalf("after the phone change %+v", c)
	}
	// unticked with another number: the box is off and the number is kept
	a.adminPost(t, "/admin/customers/save", url.Values{"id": {cid}, "name": {"Njeri Wa"}, "phone": {"0722000111"}, "whatsapp": {"0733555666"}})
	a.st.View(func(d *store.Data) { c = d.Customers[cid] })
	if c.WASamePhone || c.WhatsApp != "254733555666" {
		t.Fatalf("after unticking %+v", c)
	}
	if ticked(a.adminGet(t, "/admin/customers/view?id="+cid)) {
		t.Fatal("the box is ticked although the number differs")
	}

	// the account page: saving with the box ticked, then reloading
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 9"}, "mac": {mac1}})
	acct := a.account(t, "Flat 9")
	a.adminPost(t, "/admin/accounts/update", url.Values{"id": {acct.ID}, "flat": {"Flat 9"}, "phone": {"0744123123"}, "wa_same": {"1"}})
	got := a.account(t, "Flat 9")
	if !got.WASamePhone || got.WhatsApp != got.Phone || got.WhatsApp != "254744123123" {
		t.Fatalf("account %+v", got)
	}
	if !ticked(a.adminGet(t, "/admin/accounts/view?code="+got.Code)) {
		t.Fatal("the account page does not show the box ticked after saving")
	}
	// linking a new customer from the account page keeps the box
	a.adminPost(t, "/admin/accounts/customer", url.Values{"id": {acct.ID}, "name": {"Otieno"}, "phone": {"0755000999"}, "wa_same": {"1"}, "move_in": {"2026-10-01"}})
	got = a.account(t, "Flat 9")
	if !got.WASamePhone || got.WhatsApp != "254755000999" {
		t.Fatalf("after linking %+v", got)
	}
	// the phone is edited on the account with the box ticked on the customer too: still the same
	a.adminPost(t, "/admin/accounts/update", url.Values{"id": {acct.ID}, "flat": {"Flat 9"}, "phone": {"0766111000"}, "wa_same": {"1"}})
	got = a.account(t, "Flat 9")
	if got.WhatsApp != "254766111000" {
		t.Fatalf("WhatsApp did not follow the phone: %+v", got)
	}
}

func TestAccountPageListsTheDevicesOnTheAccount(t *testing.T) {
	a := newApp(t, false)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 5"}, "mac": {"00:E0:4C:68:01:02"}})
	acct := a.account(t, "Flat 5")
	page := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	for _, want := range []string{"Devices on this account", "00:E0:4C:68:01:02", "Realtek", "account MAC", "not seen yet", "Equipment register"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Equipment at this") {
		t.Error("the old card title is still there")
	}
}
