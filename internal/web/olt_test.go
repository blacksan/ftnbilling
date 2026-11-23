package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

const (
	tOLTMAC = "48:21:B8:10:D7:72"
	tONUMAC = "68:22:9F:00:00:01"
)

// aptID is the building's apartment (the test app has one).
func (a *app) aptID(t *testing.T) string {
	t.Helper()
	var id string
	a.st.View(func(d *store.Data) {
		if ids := d.SiteApartmentIDs(a.site); len(ids) > 0 {
			id = ids[0]
		}
	})
	if id == "" {
		t.Fatal("no apartment")
	}
	return id
}

func (a *app) seedBridgeDevice(t *testing.T, mac, port string) {
	t.Helper()
	now := time.Now()
	if err := a.st.Update(func(d *store.Data) error {
		d.Devices[store.DeviceKey(a.site, mac)] = store.Device{SiteID: a.site, MAC: mac, Source: "bridge", Port: port, FirstSeen: now, LastSeen: now, Online: true, Hits: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (a *app) oltID(t *testing.T) string {
	t.Helper()
	var id string
	a.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindOLT {
				id = e.ID
			}
		}
	})
	if id == "" {
		t.Fatal("no OLT saved")
	}
	return id
}

// makeOLT saves the OLT of the story: a V-SOL V1600GS-R on ether3 serving the building's apartment on PON 1.
func (a *app) makeOLT(t *testing.T, extra url.Values) {
	t.Helper()
	if err := a.svc.SetDataKey([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	apt := a.aptID(t)
	f := url.Values{"name": {"Block A OLT"}, "model": {"V1600GS-R"}, "serial": {"VS0001"}, "mac": {tOLTMAC}, "site": {a.site}, "router_port": {"ether3"},
		"pon_ports": {"4"}, "pon_form": {"1"}, "pon_apt_1": {apt}, "mgmt_ip": {"192.168.1.10"}, "mgmt_user": {"admin"}, "mgmt_pass": {"Olt-Secret-99"}, "status": {"installed"}}
	for k, v := range extra {
		f[k] = v
	}
	if l := a.adminPost(t, "/admin/olts/save", f); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
}

func TestOLTFormTreeRouterTabAndApartmentPage(t *testing.T) {
	a := newONUApp(t)
	a.makeOLT(t, nil)
	oid := a.oltID(t)
	// an ONU on PON 1 and a house on it
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEG1"}, "mac": {tONUMAC}, "model": {"F601"}, "placement": {"1"}, "olt": {oid}, "pon_port": {"1"}})
	var eq string
	a.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Serial == "ZTEG1" {
				eq = e.ID
				if e.OLTID != oid || e.PONPort != 1 {
					t.Errorf("placement not saved: %+v", e)
				}
			}
		}
	})
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"A1"}, "tenant": {"Jane Wanjiru"}, "equipment_id": {eq}, "apartment_id": {a.aptID(t)}})

	tree := a.adminGet(t, "/admin/equipment")
	for _, want := range []string{"Block A", "Apartments", "(logical)", "(physical)", "Block A OLT", tOLTMAC, "on ether3", "PON 1", "serves: ", "ZTEG1", "Jane Wanjiru", "A1", "F601", "ONUs not under an OLT"} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree page lacks %q", want)
		}
	}
	if strings.Contains(tree, "Olt-Secret-99") {
		t.Fatal("password on the tree")
	}
	if !strings.Contains(a.adminGet(t, "/admin/equipment?view=list"), "ZTEG1") {
		t.Fatal("list view lost")
	}
	rt := a.adminGet(t, "/admin/routers/view?id="+a.site+"&tab=equipment")
	if !strings.Contains(rt, "Apartments") || !strings.Contains(rt, "Block A OLT") || !strings.Contains(rt, "PON ports") {
		t.Fatal("router equipment tab")
	}
	apt := a.adminGet(t, "/admin/apartments/view?id="+a.aptID(t))
	if !strings.Contains(apt, "OLT and PON") && !strings.Contains(apt, "Block A OLT") || !strings.Contains(apt, "PON 1") {
		t.Fatal("apartment page has no OLT/PON")
	}
	acct := a.adminGet(t, "/admin/accounts/view?code="+a.account(t, "A1").Code)
	if !strings.Contains(acct, "Block A OLT, PON 1") {
		t.Fatal("account card has no OLT/PON")
	}
	// the OLT page: password never rendered, reveal needs the owner's password
	page := a.adminGet(t, "/admin/olts/view?id="+oid)
	if strings.Contains(page, "Olt-Secret-99") || !strings.Contains(page, "stored, leave empty to keep") || !strings.Contains(page, tOLTMAC) {
		t.Fatal("OLT page")
	}
	r := a.admin("POST", "/admin/olts/reveal", url.Values{"id": {oid}, "current_password": {"wrong"}})
	if strings.Contains(body(t, r), "Olt-Secret-99") {
		t.Fatal("revealed with a wrong password")
	}
	r = a.admin("POST", "/admin/olts/reveal", url.Values{"id": {oid}, "current_password": {pw}})
	if b := body(t, r); !strings.Contains(b, "Olt-Secret-99") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("not revealed after step-up")
	}
	// the generic equipment view hands an OLT to its own form
	if r := a.admin("GET", "/admin/equipment/view?id="+oid, nil); r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), "/admin/olts/view") {
		t.Fatalf("redirect %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// a PON mapped to an apartment of another router is refused, nothing is saved
	if l := a.adminPost(t, "/admin/olts/save", url.Values{"id": {oid}, "name": {"Block A OLT"}, "mac": {tOLTMAC}, "site": {a.site}, "pon_ports": {"4"}, "pon_form": {"1"}, "pon_apt_2": {"nope"}}); !strings.Contains(l, "Error") {
		t.Fatal("a bad PON mapping was saved")
	}
}

func TestNewDevicesClassificationAndOLTGuard(t *testing.T) {
	a := newONUApp(t)
	a.makeOLT(t, nil)
	oid := a.oltID(t)
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEG1"}, "mac": {tONUMAC}, "model": {"F601"}, "placement": {"1"}, "olt": {oid}, "pon_port": {"1"}})
	a.seedBridgeDevice(t, tOLTMAC, "ether3")
	a.seedBridgeDevice(t, tONUMAC, "ether5")
	a.seedBridgeDevice(t, "6C:1F:F7:BB:46:94", "ether5")
	page := a.adminGet(t, "/admin/devices")
	for _, want := range []string{"OLT V1600GS-R Block A OLT", "ONU F601 · ZTEG1", "This is an OLT, not a customer", `data-equipment=`, `data-noaddr="1"`} {
		if !strings.Contains(page, want) {
			t.Errorf("devices page lacks %q", want)
		}
	}
	// the OLT row has no Create customer / Attach: cut its row out and look
	i := strings.Index(page, tOLTMAC)
	row := page[i : i+strings.Index(page[i:], "</tr>")]
	if strings.Contains(row, "Create customer") || strings.Contains(row, "Attach") || !strings.Contains(row, "Dismiss") {
		t.Fatalf("OLT row actions: %s", row)
	}
	// server guard: a customer on the OLT's MAC is refused, in the dialog's JSON and for attach
	apt := a.aptID(t)
	r := a.admin("POST", "/admin/accounts/create", url.Values{"unit": {"X1"}, "mac": {tOLTMAC}, "apartment_id": {apt}})
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "That+MAC+belongs+to+OLT+Block+A+OLT") {
		t.Fatalf("create on OLT MAC: %d %s", r.StatusCode, loc)
	}
	a.adminPost(t, "/admin/accounts/create", url.Values{"unit": {"X2"}, "mac": {"AA:BB:CC:DD:EE:02"}, "apartment_id": {apt}})
	if l := a.adminPost(t, "/admin/devices/attach", url.Values{"site": {a.site}, "mac": {tOLTMAC}, "acct": {"X2"}}); !strings.Contains(l, "belongs to OLT Block A OLT") {
		t.Fatalf("attach: %s", l)
	}
	if got := a.account(t, "X2"); got.MAC != "AA:BB:CC:DD:EE:02" {
		t.Fatalf("MAC changed to %s", got.MAC)
	}
}

func TestCreateOnABridgeOnlyDeviceAsksForConfirmation(t *testing.T) {
	a := newONUApp(t)
	const mac = "6C:1F:F7:BB:46:94"
	a.seedBridgeDevice(t, mac, "ether5")
	apt := a.aptID(t)
	form := url.Values{"unit": {"C1"}, "mac": {mac}, "apartment_id": {apt}}
	r := a.adminJSON(t, "POST", "/admin/accounts/create", form)
	var j map[string]any
	if err := json.Unmarshal([]byte(body(t, r)), &j); err != nil {
		t.Fatal(err)
	}
	if j["ok"] != false || j["confirm"] != true || !strings.Contains(j["error"].(string), "never asked for an address") {
		t.Fatalf("%v", j)
	}
	a.st.View(func(d *store.Data) {
		if _, ok := d.AccountByMAC(mac); ok {
			t.Fatal("created without confirming")
		}
	})
	form.Set("confirm_no_address", "1")
	r = a.adminJSON(t, "POST", "/admin/accounts/create", form)
	if err := json.Unmarshal([]byte(body(t, r)), &j); err != nil || j["ok"] != true {
		t.Fatalf("confirmed: %v %v", j, err)
	}
}

func TestAddCustomerDialogOffersTheONUPonApartments(t *testing.T) {
	a := newONUApp(t)
	a.makeOLT(t, nil)
	oid := a.oltID(t)
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEG1"}, "mac": {tONUMAC}, "placement": {"1"}, "olt": {oid}, "pon_port": {"1"}})
	page := a.adminGet(t, "/admin/devices")
	apt := a.aptID(t)
	if !strings.Contains(page, `data-apts="`+apt+`"`) {
		t.Fatal("the ONU option carries no apartments to preselect")
	}
}

// adminJSON posts a form the way the Add customer dialog does (it asks for JSON back).
func (a *app) adminJSON(t *testing.T, method, path string, form url.Values) *http.Response {
	t.Helper()
	form.Set("_csrf", a.br.CSRF)
	req, _ := http.NewRequest(method, a.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(a.br.Cookie)
	resp, err := a.cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
