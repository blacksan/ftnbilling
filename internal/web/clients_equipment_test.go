package web_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

// A customer's account MAC is the WAN MAC of the ONU we installed for them. That ONU is in the register (installed,
// not in stock), so it is not "creatable"; the account must still be listed as the house, and an ONU of the register
// that has no account here (returned, then plugged in again) must still be listed so it can be attached, like on the
// New devices page. Only an OLT is hidden.
func TestAPIClients_InstalledONUIsStillTheCustomer(t *testing.T) {
	r, _ := liveRig(t)
	a := r.pinCode(t, r.acct(t, r.A, "House 707", "AA:BB:CC:DD:EE:01"), "QX7KP2")
	now := time.Now()
	if err := r.st.Update(func(d *store.Data) error {
		d.Equipment["onu707"] = store.Equipment{ID: "onu707", Kind: store.KindONT, Serial: "ZTEG707", MAC: a.MAC, Status: store.EqInstalled,
			AccountID: a.ID, SiteID: r.A.ID, InstalledAt: now, CreatedAt: now}
		d.Equipment["onuret"] = store.Equipment{ID: "onuret", Kind: store.KindONT, Serial: "ZTEGRET", MAC: "3C:07:54:00:00:09", Status: store.EqReturned, CreatedAt: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := r.call(r.view.Access, "GET", "/routers/"+r.A.ID+"/clients", nil, nil)
	if res.Status != 200 {
		t.Fatalf("%d %s", res.Status, res.Raw)
	}
	kinds := map[string]string{}
	var ret map[string]any
	for _, it := range res.JSON(t)["items"].([]any) {
		m := it.(map[string]any)
		kinds[m["mac"].(string)] = m["kind"].(string)
		if m["mac"] == "3C:07:54:00:00:09" {
			ret = m
		}
	}
	if kinds[a.MAC] != "flat" {
		t.Fatalf("the customer on their installed ONU's WAN MAC is not listed as a house: %v\n%s", kinds, res.Raw)
	}
	if kinds["3C:07:54:00:00:09"] != "unregistered" {
		t.Fatalf("a returned ONU seen on the network vanished from Clients (New devices still lists it): %v\n%s", kinds, res.Raw)
	}
	if ret["not_creatable"] != true || ret["equipment_id"] != "onuret" || !strings.Contains(fmt.Sprint(ret["equipment"]), "ZTEGRET") {
		t.Fatalf("the returned ONU must say what it is and offer no Create customer: %v", ret)
	}
	// the router's Clients tab: the house is there, the returned ONU says what it is and offers no Create customer
	h := r.adminGet(t, "/admin/routers/view?id="+r.A.ID+"&tab=clients")
	if !strings.Contains(h, "House 707") || !strings.Contains(h, "ZTEGRET") || !strings.Contains(h, `disabled title="This ONU is not in stock"`) {
		t.Fatalf("clients tab:\n%s", h)
	}
}
