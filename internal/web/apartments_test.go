package web_test

import (
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// ---- API v1: apartments (docs/api/v6-apartments.md) ---------------------------------

func TestAPIApartmentsCRUDAndContacts(t *testing.T) {
	r := newAPIRig(t)
	body := map[string]any{"name": "MG One Central", "address": "Ngong Road", "router_ids": []string{r.A.ID},
		"caretaker": map[string]any{"Name": "Mzee Otieno", "Phone": "0722 000 111", "Phone2": "0733000222", "Email": "m@example.org", "Notes": "gate"},
		"landlord":  map[string]any{"Name": "Agent Kamau", "Phone": "+254711222333"}}
	x := r.call(r.oper.Access, "POST", "/apartments", body, nil)
	if x.Status != 201 {
		t.Fatalf("create: %d %s", x.Status, x.Raw)
	}
	apt := x.JSON(t)
	ct := apt["caretaker"].(map[string]any)
	if ct["phone"] != "+254722000111" || ct["phone2"] != "+254733000222" || ct["whatsapp_url"] != "https://wa.me/254722000111" {
		t.Fatalf("caretaker not normalised: %v", ct)
	}
	if apt["landlord"].(map[string]any)["phone"] != "+254711222333" {
		t.Fatalf("landlord: %v", apt["landlord"])
	}
	id := str(apt, "id")
	if rt := apt["routers"].([]any); len(rt) != 1 || rt[0].(map[string]any)["site_id"] != r.A.ID {
		t.Fatalf("routers: %v", apt["routers"])
	}
	// Duplicate name, bad phone, missing name.
	if x := r.call(r.oper.Access, "POST", "/apartments", map[string]any{"name": "mg one central"}, nil); x.Status != 409 || x.errCode(t) != "apartment_name_in_use" {
		t.Errorf("duplicate name: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/apartments", map[string]any{"name": "Other", "caretaker": map[string]any{"Name": "X", "Phone": "12"}}, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
		t.Errorf("bad phone: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/apartments", map[string]any{"address": "x"}, nil); x.Status != 400 {
		t.Errorf("no name: %d %s", x.Status, x.Raw)
	}
	// Roles: viewers read, cannot write.
	if x := r.call(r.view.Access, "POST", "/apartments", map[string]any{"name": "Nope"}, nil); x.Status != 403 {
		t.Errorf("viewer create: %d", x.Status)
	}
	if x := r.call(r.view.Access, "GET", "/apartments/"+id, nil, nil); x.Status != 200 {
		t.Errorf("viewer read: %d", x.Status)
	}
	// Search by caretaker phone, landlord name.
	for _, q := range []string{"0722000111", "kamau", "ngong"} {
		x := r.call(r.view.Access, "GET", "/apartments?q="+url.QueryEscape(q), nil, nil)
		if x.Status != 200 || len(x.JSON(t)["items"].([]any)) != 1 {
			t.Errorf("q=%s: %d %s", q, x.Status, x.Raw)
		}
	}
	// Patch keeps what is not sent; contacts can be cleared.
	x = r.call(r.oper.Access, "PATCH", "/apartments/"+id, map[string]any{"notes": "north gate", "landlord": map[string]any{}}, nil)
	if x.Status != 200 || x.JSON(t)["notes"] != "north gate" || x.JSON(t)["caretaker"].(map[string]any)["name"] != "Mzee Otieno" || x.JSON(t)["landlord"].(map[string]any)["name"] != "" {
		t.Fatalf("patch: %d %s", x.Status, x.Raw)
	}
	// A router serves several apartments; an apartment several routers.
	other := r.call(r.oper.Access, "POST", "/apartments", map[string]any{"name": "Second Block", "router_ids": []string{r.A.ID}}, nil).JSON(t)
	if r.call(r.oper.Access, "POST", "/apartments/"+id+"/routers", map[string]any{"site_id": r.B.ID}, nil).Status != 200 {
		t.Fatal("link second router")
	}
	if rt := r.call(r.view.Access, "GET", "/apartments/"+id, nil, nil).JSON(t)["routers"].([]any); len(rt) != 2 {
		t.Fatalf("two routers: %v", rt)
	}
	// Unlinking refused while open accounts of the apartment are on the router.
	a, err := r.svc.CreateAccount(billing.AccountInput{ApartmentID: id, SiteID: r.B.ID, Unit: "B4", MAC: "AA:BB:CC:00:A1:01"})
	if err != nil {
		t.Fatal(err)
	}
	if x := r.call(r.oper.Access, "DELETE", "/apartments/"+id+"/routers/"+r.B.ID, nil, nil); x.Status != 409 || x.errCode(t) != "router_serves_accounts" {
		t.Errorf("unlink: %d %s", x.Status, x.Raw)
	}
	// Delete refused while accounts belong to it.
	if x := r.call(r.owner.Access, "DELETE", "/apartments/"+id, nil, nil); x.Status != 409 || x.errCode(t) != "apartment_in_use" {
		t.Errorf("delete in use: %d %s", x.Status, x.Raw)
	}
	// Account JSON carries apartment and unit; unit is unique per apartment.
	acc := r.call(r.view.Access, "GET", "/accounts/"+a.ID, nil, nil).JSON(t)
	if ap := acc["apartment"].(map[string]any); ap["id"] != id || ap["name"] != "MG One Central" || acc["unit"] != "B4" {
		t.Fatalf("account: %v", acc)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": id, "site_id": r.B.ID, "unit": "b4", "mac": "AA:BB:CC:00:A1:02"}, nil); x.Status != 409 || x.errCode(t) != "unit_in_use" {
		t.Errorf("duplicate unit: %d %s", x.Status, x.Raw)
	}
	// Create needs an apartment; the router is automatic with one router.
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"unit": "C1", "mac": "AA:BB:CC:00:A1:03"}, nil); x.Status != 400 {
		t.Errorf("no apartment: %d %s", x.Status, x.Raw)
	}
	oid := str(other, "id")
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": oid, "unit": "C1", "mac": "AA:BB:CC:00:A1:03"}, nil); x.Status != 201 || x.JSON(t)["site_id"] != r.A.ID {
		t.Errorf("auto router: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": id, "unit": "C2", "mac": "AA:BB:CC:00:A1:04"}, nil); x.Status != 400 {
		t.Errorf("two routers, none chosen: %d %s", x.Status, x.Raw)
	}
	// Move the customer to the other apartment: router changes with it.
	x = r.call(r.oper.Access, "POST", "/accounts/"+a.ID+"/move", map[string]any{"apartment_id": oid, "unit": "D1"}, nil)
	if x.Status != 200 || x.JSON(t)["site_id"] != r.A.ID || x.JSON(t)["unit"] != "D1" || x.JSON(t)["apartment"].(map[string]any)["id"] != oid {
		t.Fatalf("move: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "DELETE", "/apartments/"+id+"/routers/"+r.B.ID, nil, nil); x.Status != 200 {
		t.Errorf("unlink after the move: %d %s", x.Status, x.Raw)
	}
}

func TestAPIRouterContactsAreGone(t *testing.T) {
	r := newAPIRig(t)
	for _, c := range [][2]string{{"GET", "/sites/" + r.A.ID + "/contacts"}, {"POST", "/sites/" + r.A.ID + "/contacts"},
		{"PATCH", "/sites/" + r.A.ID + "/contacts/x"}, {"DELETE", "/sites/" + r.A.ID + "/contacts/x"}, {"GET", "/routers/" + r.A.ID + "/contacts"}} {
		x := r.call(r.owner.Access, c[0], c[1], map[string]any{}, nil)
		if x.Status != 410 || x.errCode(t) != "gone" {
			t.Errorf("%s %s: %d %s", c[0], c[1], x.Status, x.Raw)
		}
	}
}

func TestAPIDeviceOfAnotherApartmentBelongsTo(t *testing.T) {
	r := newAPIRig(t)
	aptA, aptB := aptOf(t, r.st, r.A.ID), aptOf(t, r.st, r.B.ID)
	own := r.acct(t, r.A, "A-1", "AA:BB:CC:00:B1:01")
	r.acct(t, r.B, "B-1", "AA:BB:CC:00:B1:02")
	r.svc.NoteDevice(r.B.ID, own.MAC, "10.5.50.40", "dhcp") // Alpha's customer seen on Beta
	r.svc.NoteDevice(r.A.ID, own.MAC, "10.5.50.41", "dhcp") // seen on its own router: not unknown at all
	r.svc.NoteDevice(r.B.ID, "AA:BB:CC:00:B1:99", "10.5.50.42", "dhcp")

	x := r.call(r.view.Access, "GET", "/unregistered", nil, nil)
	var found, plain bool
	for _, it := range x.JSON(t)["items"].([]any) {
		m := it.(map[string]any)
		switch m["mac"] {
		case own.MAC:
			if m["site_id"] != r.B.ID {
				t.Errorf("own router listed it: %v", m)
			}
			b := m["belongs_to"].(map[string]any)
			if b["account_id"] != own.ID || b["unit"] != "A-1" || b["apartment"].(map[string]any)["id"] != aptA || b["apartment"].(map[string]any)["name"] != "Alpha" {
				t.Errorf("belongs_to: %v", b)
			}
			found = true
		case "AA:BB:CC:00:B1:99":
			plain = m["belongs_to"] == nil
		}
	}
	if !found || !plain {
		t.Fatalf("found=%v plain=%v %s", found, plain, x.Raw)
	}
	// Attach or create elsewhere is refused with mac_other_building.
	other := r.acct(t, r.B, "B-2", "AA:BB:CC:00:B1:03")
	if x := r.call(r.oper.Access, "POST", "/unregistered/attach", map[string]any{"site_id": r.B.ID, "mac": own.MAC, "account_id": other.ID}, nil); x.Status != 409 || x.errCode(t) != "mac_other_building" {
		t.Errorf("attach: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.oper.Access, "POST", "/unregistered/create", map[string]any{"site_id": r.B.ID, "apartment_id": aptB, "mac": own.MAC, "unit": "B-9"}, nil); x.Status != 409 || x.errCode(t) != "mac_other_building" {
		t.Errorf("create: %d %s", x.Status, x.Raw)
	}
	// Viewers cannot move; operators can, and the customer follows the device.
	if x := r.call(r.view.Access, "POST", "/unregistered/move", map[string]any{"site_id": r.B.ID, "mac": own.MAC}, nil); x.Status != 403 {
		t.Errorf("viewer move: %d", x.Status)
	}
	x = r.call(r.oper.Access, "POST", "/unregistered/move", map[string]any{"site_id": r.B.ID, "mac": own.MAC, "apartment_id": aptB}, nil)
	if x.Status != 200 || x.JSON(t)["site_id"] != r.B.ID || x.JSON(t)["apartment"].(map[string]any)["id"] != aptB {
		t.Fatalf("move: %d %s", x.Status, x.Raw)
	}
	for _, it := range r.call(r.view.Access, "GET", "/unregistered", nil, nil).JSON(t)["items"].([]any) {
		if it.(map[string]any)["mac"] == own.MAC {
			t.Errorf("a moved customer's device is still listed: %v", it)
		}
	}
}

// ---- admin pages --------------------------------------------------------------------

func TestAdminApartmentPages(t *testing.T) {
	m := newMulti(t)
	A := m.addSite(t, "Alpha")
	// Create with a caretaker and a landlord.
	loc := m.adminPost(t, "/admin/apartments/save", url.Values{"name": {"MG One Central"}, "address": {"Ngong Road"}, "router": {A.ID},
		"caretaker_name": {"Mzee Otieno"}, "caretaker_phone": {"0722000111"}, "landlord_name": {"Agent Kamau"}, "landlord_phone": {"0711222333"}})
	if strings.Contains(loc, "Error") || !strings.Contains(loc, "/admin/apartments/view?id=") {
		t.Fatalf("create: %s", loc)
	}
	id := loc[strings.Index(loc, "id=")+3 : strings.Index(loc, "&msg")]
	page := m.adminGet(t, "/admin/apartments/view?id="+id)
	for _, want := range []string{"MG One Central", "Mzee Otieno", `href="tel:&#43;254722000111"`, `href="https://wa.me/254722000111"`, "Agent Kamau", "Alpha"} {
		if !strings.Contains(page, want) {
			t.Errorf("view page lacks %q", want)
		}
	}
	// Bad phone: refused with the reason in words.
	loc = m.adminPost(t, "/admin/apartments/save", url.Values{"name": {"Bad"}, "caretaker_name": {"X"}, "caretaker_phone": {"12"}})
	if !strings.Contains(loc, "Error") {
		t.Errorf("bad phone accepted: %s", loc)
	}
	// List shows flats and the nav tab.
	m.svc.CreateAccount(billing.AccountInput{ApartmentID: id, Unit: "B4", MAC: "AA:BB:CC:00:C1:01"})
	list := m.adminGet(t, "/admin/apartments")
	if !strings.Contains(list, "MG One Central") || !strings.Contains(list, `href="/admin/apartments"`) || !strings.Contains(list, "0 / 1") {
		t.Errorf("list: missing name, nav or flats")
	}
	// Edit form carries the values; unlinking the router is refused while it serves flats.
	if form := m.adminGet(t, "/admin/apartments/edit?id="+id); !strings.Contains(form, "Mzee Otieno") || !strings.Contains(form, "Landlord / agent") {
		t.Error("edit form")
	}
	if l := m.adminPost(t, "/admin/apartments/unlink", url.Values{"id": {id}, "site": {A.ID}}); !strings.Contains(l, "Error") {
		t.Errorf("unlink with flats: %s", l)
	}
	// The router page lists the apartments it serves; search finds the apartment by caretaker name and phone.
	if rp := m.adminGet(t, "/admin/routers/view?id="+A.ID+"&tab=profile"); !strings.Contains(rp, "Apartments served") || !strings.Contains(rp, "MG One Central") {
		t.Error("router page lacks its apartments")
	}
	for _, q := range []string{"Otieno", "0722000111", "Kamau", "MG One"} {
		js := m.adminGet(t, "/admin/search.json?q="+url.QueryEscape(q))
		if !strings.Contains(js, `"Apartments"`) || !strings.Contains(js, "MG One Central") {
			t.Errorf("search %q: %s", q, js)
		}
	}
	// The retired contacts routes answer 410.
	if r := m.admin("POST", "/admin/routers/contacts/save", url.Values{"site": {A.ID}, "name": {"x"}}); r.StatusCode != 410 {
		t.Errorf("router contacts: %d", r.StatusCode)
	}
}

func TestAdminDevicesBelongsToAndMove(t *testing.T) {
	m := newMulti(t)
	A, B := m.addSite(t, "Alpha"), m.addSite(t, "Beta")
	acct, err := m.svc.CreateAccount(billing.AccountInput{SiteID: A.ID, Flat: "A-1", Tenant: "Wanjiru", MAC: "AA:BB:CC:00:D1:01"})
	if err != nil {
		t.Fatal(err)
	}
	m.svc.NoteDevice(B.ID, acct.MAC, "10.5.50.40", "dhcp")
	page := m.adminGet(t, "/admin/devices")
	if !strings.Contains(page, "Belongs to Wanjiru (Alpha, house number A-1)") || !strings.Contains(page, "Move customer here") {
		t.Fatalf("devices page: %s", page)
	}
	if strings.Contains(page, `data-open="addcustomer" data-mac="`+acct.MAC) {
		t.Error("Create customer is offered for a MAC of another apartment")
	}
	// Without the tick nothing moves; with it the account follows the device.
	if l := m.adminPost(t, "/admin/devices/move", url.Values{"site": {B.ID}, "mac": {acct.MAC}, "apartment_id": {aptOf(t, m.st, B.ID)}}); !strings.Contains(l, "Error") {
		t.Errorf("unconfirmed move: %s", l)
	}
	if l := m.adminPost(t, "/admin/devices/move", url.Values{"site": {B.ID}, "mac": {acct.MAC}, "apartment_id": {aptOf(t, m.st, B.ID)}, "confirm": {"1"}}); strings.Contains(l, "Error") {
		t.Fatalf("move: %s", l)
	}
	got, _ := m.svc.GetAccount(acct.ID)
	if got.SiteID != B.ID || got.ApartmentID != aptOf(t, m.st, B.ID) {
		t.Fatalf("account not moved: %+v", got)
	}
	// Attach from the form is refused the same way for another customer's MAC.
	other, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: A.ID, Flat: "A-2", MAC: "AA:BB:CC:00:D1:02"})
	if l := m.adminPost(t, "/admin/devices/attach", url.Values{"site": {A.ID}, "mac": {got.MAC}, "account": {other.ID}}); !strings.Contains(l, "Error") {
		t.Errorf("attach a MAC of another apartment: %s", l)
	}
	var _ store.Account
}

// ---- GPS location of an apartment ------------------------------------------------------

func TestAdminApartmentGPSForm(t *testing.T) {
	m := newMulti(t)
	A := m.addSite(t, "Alpha")
	save := func(name, gps string, extra ...string) string {
		v := url.Values{"name": {name}, "router": {A.ID}, "gps": {gps}}
		for i := 0; i+1 < len(extra); i += 2 {
			v.Set(extra[i], extra[i+1])
		}
		return m.adminPost(t, "/admin/apartments/save", v)
	}
	idOf := func(loc string) string { return loc[strings.Index(loc, "id=")+3 : strings.Index(loc, "&msg")] }
	const mapLink = `href="https://www.google.com/maps/search/?api=1&amp;query=-1.2921%2C36.8219"`

	// A plain pair, as pasted from Google Maps.
	loc := save("Plain", "-1.2921, 36.8219")
	if strings.Contains(loc, "Error") {
		t.Fatalf("plain pair: %s", loc)
	}
	id := idOf(loc)
	page := m.adminGet(t, "/admin/apartments/view?id="+id)
	if !strings.Contains(page, mapLink) || !strings.Contains(page, `target="_blank"`) || !strings.Contains(page, `rel="noopener"`) || !strings.Contains(page, "View on map") {
		t.Errorf("detail page lacks the map link:\n%s", page)
	}
	if list := m.adminGet(t, "/admin/apartments"); !strings.Contains(list, mapLink) || !strings.Contains(list, "View on map") {
		t.Error("list lacks the map link")
	}
	// The edit form shows the stored point, and the helper script is an external file (no inline script).
	form := m.adminGet(t, "/admin/apartments/edit?id="+id)
	if !strings.Contains(form, `value="-1.2921,36.8219"`) || !strings.Contains(form, `src="/static/gps.js`) || strings.Contains(form, "onclick=") {
		t.Errorf("edit form: %s", form)
	}
	if strings.Contains(form, "<script>") {
		t.Error("inline script in the form")
	}

	// A Google Maps URL: @lat,lng and q=lat,lng.
	for i, in := range []string{"https://www.google.com/maps/place/Kilimani/@-1.2921,36.8219,17z/data=!3m1", "https://www.google.com/maps?q=-1.2921,36.8219"} {
		loc := save("Url"+string(rune('A'+i)), in)
		if strings.Contains(loc, "Error") {
			t.Fatalf("url %q: %s", in, loc)
		}
		if !strings.Contains(m.adminGet(t, "/admin/apartments/view?id="+idOf(loc)), mapLink) {
			t.Errorf("url %q: point not stored", in)
		}
	}

	// Garbage and out-of-range are refused in words, back to the form, and nothing is created.
	for _, in := range []string{"somewhere near the mall", "95, 36", "-1.29"} {
		loc := save("Bad "+in, in)
		if !strings.Contains(loc, "Error") || !strings.Contains(loc, "/admin/apartments/new") {
			t.Errorf("%q accepted: %s", in, loc)
		}
	}
	if list := m.adminGet(t, "/admin/apartments"); strings.Contains(list, "Bad ") {
		t.Error("a refused apartment was created")
	}
	loc = save("Bad", "nope", "id", id)
	if !strings.Contains(loc, "Error") || !strings.Contains(loc, "GPS") || !strings.Contains(loc, "/admin/apartments/edit") {
		t.Errorf("edit with garbage: %s", loc)
	}

	// Empty clears the point; the link goes away.
	if loc := save("Plain", "", "id", id); strings.Contains(loc, "Error") {
		t.Fatalf("clear: %s", loc)
	}
	if page := m.adminGet(t, "/admin/apartments/view?id="+id); strings.Contains(page, "View on map") || !strings.Contains(page, "not recorded") {
		t.Error("link still shown after clearing")
	}

	// 0,0 is a point, not "unset".
	loc = save("Null island", "0.0, 0.0")
	if !strings.Contains(m.adminGet(t, "/admin/apartments/view?id="+idOf(loc)), "query=0%2C0") {
		t.Error("0,0 not kept as a point")
	}
}

func TestAPIApartmentGPS(t *testing.T) {
	r := newAPIRig(t)
	// Without GPS the fields are null.
	x := r.call(r.oper.Access, "POST", "/apartments", map[string]any{"name": "Plain"}, nil)
	if x.Status != 201 {
		t.Fatalf("create: %d %s", x.Status, x.Raw)
	}
	j := x.JSON(t)
	if v, has := j["lat"]; !has || v != nil || j["lng"] != nil || j["map_url"] != nil {
		t.Errorf("unset GPS must be null: %v %v %v", j["lat"], j["lng"], j["map_url"])
	}
	// With GPS: in on create, out on create, get and list.
	x = r.call(r.oper.Access, "POST", "/apartments", map[string]any{"name": "Pinned", "lat": -1.2921, "lng": 36.8219}, nil)
	if x.Status != 201 {
		t.Fatalf("create with GPS: %d %s", x.Status, x.Raw)
	}
	j = x.JSON(t)
	id := str(j, "id")
	if j["lat"] != -1.2921 || j["lng"] != 36.8219 || j["map_url"] != "https://www.google.com/maps/search/?api=1&query=-1.2921%2C36.8219" {
		t.Errorf("response: %v %v %v", j["lat"], j["lng"], j["map_url"])
	}
	if x := r.call(r.view.Access, "GET", "/apartments/"+id, nil, nil); x.JSON(t)["lat"] != -1.2921 {
		t.Errorf("get: %s", x.Raw)
	}
	found := false
	for _, it := range r.call(r.view.Access, "GET", "/apartments?q=Pinned", nil, nil).JSON(t)["items"].([]any) {
		found = found || it.(map[string]any)["lng"] == 36.8219
	}
	if !found {
		t.Error("list lacks lng")
	}
	// PATCH without lat/lng keeps the point (like every other field); a name change must not clear it.
	if x := r.call(r.oper.Access, "PATCH", "/apartments/"+id, map[string]any{"notes": "gate"}, nil); x.Status != 200 || x.JSON(t)["lat"] != -1.2921 {
		t.Errorf("patch without gps: %d %s", x.Status, x.Raw)
	}
	// PATCH with a new point replaces it; 0,0 is accepted.
	if x := r.call(r.oper.Access, "PATCH", "/apartments/"+id, map[string]any{"lat": 0, "lng": 0}, nil); x.Status != 200 || x.JSON(t)["lat"] != float64(0) || x.JSON(t)["map_url"] == nil {
		t.Errorf("patch 0,0: %d %s", x.Status, x.Raw)
	}
	// Validation: half, range, wrong type. Nothing changes.
	for _, body := range []map[string]any{
		{"lat": 1.0}, {"lng": 1.0}, {"lat": 91, "lng": 0}, {"lat": 0, "lng": -181}, {"lat": "x", "lng": 1}, {"lat": nil, "lng": 5},
	} {
		x := r.call(r.oper.Access, "PATCH", "/apartments/"+id, body, nil)
		if x.Status != 400 || x.errCode(t) != "validation_failed" {
			t.Errorf("%v: %d %s", body, x.Status, x.Raw)
		}
	}
	if x := r.call(r.oper.Access, "POST", "/apartments", map[string]any{"name": "Off", "lat": 100, "lng": 0}, nil); x.Status != 400 {
		t.Errorf("create out of range: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.view.Access, "GET", "/apartments/"+id, nil, nil); x.JSON(t)["lat"] != float64(0) {
		t.Errorf("a refused patch changed the point: %s", x.Raw)
	}
	// Both null clears.
	x = r.call(r.oper.Access, "PATCH", "/apartments/"+id, map[string]any{"lat": nil, "lng": nil}, nil)
	if j := x.JSON(t); x.Status != 200 || j["lat"] != nil || j["lng"] != nil || j["map_url"] != nil {
		t.Errorf("clear: %d %s", x.Status, x.Raw)
	}
}

// ---- apartment picker pop-up -----------------------------------------------------------

// The pop-up is progressive enhancement: the page still carries a plain select
// with every apartment (and the details the pop-up lists), and loads the
// picker from an external script (CSP: no inline script).
func TestAdminApartmentPickerMarkup(t *testing.T) {
	m := newMulti(t)
	A := m.addSite(t, "Alpha")
	loc := m.adminPost(t, "/admin/apartments/save", url.Values{"name": {"Rift View"}, "address": {"Ngong Road"}, "router": {A.ID}})
	if strings.Contains(loc, "Error") {
		t.Fatalf("save: %s", loc)
	}
	page := m.adminGet(t, "/admin/accounts")
	if !strings.Contains(page, `src="/static/apt-picker.js`) {
		t.Error("picker script not loaded")
	}
	if !strings.Contains(page, `<select name="apartment_id" required data-apt-select>`) {
		t.Error("plain select fallback is missing")
	}
	for _, want := range []string{`data-address="Ngong Road"`, `data-subs="0"`, `data-router-names="Alpha"`, `>Rift View</option>`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(page, "<script>") || strings.Contains(page, "onclick=") {
		t.Error("inline script")
	}
	js := m.adminGet(t, "/static/apt-picker.js")
	if !strings.Contains(js, "showModal") || !strings.Contains(js, "data-apt-select") {
		t.Error("picker script not served")
	}
}

// Geolocation is blocked on every page except the apartment form, which hosts the
// "Use this device's location" button.
func TestGeolocationPolicyOnlyOnApartmentForm(t *testing.T) {
	m := newMulti(t)
	m.addSite(t, "Alpha")
	policy := func(path string) string {
		r := m.admin("GET", path, nil)
		defer r.Body.Close()
		return r.Header.Get("Permissions-Policy")
	}
	for _, p := range []string{"/admin/apartments/new", "/admin/apartments/edit"} {
		if got := policy(p); !strings.Contains(got, "geolocation=(self)") || !strings.Contains(got, "camera=()") {
			t.Errorf("%s: Permissions-Policy = %q, want geolocation=(self)", p, got)
		}
	}
	for _, p := range []string{"/admin/apartments", "/admin", "/admin/accounts", "/admin/login"} {
		if got := policy(p); !strings.Contains(got, "geolocation=()") || strings.Contains(got, "geolocation=(self)") {
			t.Errorf("%s: Permissions-Policy = %q, want geolocation=()", p, got)
		}
	}
}

// The subscribers list leads with the customer name (the link); the building, the
// house number and the code sit below it, smaller.
func TestAdminAccountsNameOnTopPlaceBelow(t *testing.T) {
	m := newMulti(t)
	A := m.addSite(t, "Alpha")
	apt, err := m.svc.CreateApartment(billing.ApartmentInput{Name: "MG One Central"}, []string{A.ID})
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.svc.CreateAccount(billing.AccountInput{ApartmentID: apt.ID, SiteID: A.ID, Unit: "A-1", Tenant: "Wanjiru", MAC: "AA:BB:CC:00:D1:02"})
	if err != nil {
		t.Fatal(err)
	}
	page := m.adminGet(t, "/admin/accounts")
	want := `<td class="who"><b><a href="/admin/accounts/view?code=` + a.Code + `">Wanjiru</a></b><span>MG One Central &middot; House A-1 &middot; <code>` + a.Code + `</code></span></td>`
	if !strings.Contains(page, want) {
		t.Errorf("subscribers cell: want %s", want)
	}
}
