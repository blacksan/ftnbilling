package web_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestONUWaitingForMACShowsBothMACsAndOwnerOverride(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onuw", "SNW1", "", "AA:BB:CC:DD:EE:90")
	a.st.Update(func(d *store.Data) error {
		d.Equipment["eqw"] = store.Equipment{ID: "eqw", Kind: store.KindONT, Serial: "SNW1", MAC: "AA:BB:CC:DD:EE:10", Status: store.EqInStock, CreatedAt: time.Now()}
		o := d.ONUs["onuw"]
		o.Sec.MACCheck, o.Sec.MACWaitLease, o.Sec.MACWaitNote = store.MACWaiting, "AA:BB:CC:DD:EE:77", "the management MAC is not near the registered WAN MAC"
		o.Sec.MgmtMAC = ""
		d.ONUs["onuw"] = o
		return nil
	})
	for _, path := range []string{"/admin/onus", "/admin/routers/view?id=" + a.site + "&tab=onus"} {
		page := a.adminGet(t, path)
		for _, want := range []string{"Waiting: management MAC doesn&#39;t match the registered ONU yet", "AA:BB:CC:DD:EE:77", "AA:BB:CC:DD:EE:10", "Trust this ONU"} {
			if !strings.Contains(page, want) && !strings.Contains(page, strings.ReplaceAll(want, "&#39;", "'")) {
				t.Fatalf("%s: %q missing", path, want)
			}
		}
	}
	// a wrong password does nothing
	r := a.admin("POST", "/admin/onus/trust", url.Values{"onu": {"onuw"}, "current_password": {"wrong"}})
	_ = body(t, r)
	if a.onu("onuw").Sec.MACTrusted != "" {
		t.Fatal("trusted with a wrong password")
	}
	r = a.admin("POST", "/admin/onus/trust", url.Values{"onu": {"onuw"}, "current_password": {pw}})
	_ = body(t, r)
	if got := a.onu("onuw").Sec.MACTrusted; got != "AA:BB:CC:DD:EE:77" {
		t.Fatalf("not trusted: %q", got)
	}
	audited := false
	a.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" && strings.Contains(e.Msg, "trusts the management MAC") {
				audited = true
			}
		}
	})
	if !audited {
		t.Fatal("not audited")
	}
}

func (a *app) onu(id string) (o store.ONU) {
	a.st.View(func(d *store.Data) { o = d.ONUs[id] })
	return
}

func TestUnknownONUsAreListedWithAnAddToInventoryShortcut(t *testing.T) {
	a := newONUApp(t)
	now := time.Now()
	a.st.Update(func(d *store.Data) error {
		d.Settings["acs.unknown"] = store.Setting{Key: "acs.unknown", UpdatedAt: now, Value: `[{"serial":"V26<b>X","model":"V2804N-Z","site_id":"` + a.site + `","site_name":"Block","ip":"10.6.5.9","first_seen":"` + now.Format(time.RFC3339) + `","last_seen":"` + now.Format(time.RFC3339) + `","count":3}]`}
		return nil
	})
	for _, path := range []string{"/admin/routers/view?id=" + a.site + "&tab=onus", "/admin/equipment?tab=onu"} {
		page := a.adminGet(t, path)
		if !strings.Contains(page, "Unknown ONUs tried to check in") || !strings.Contains(page, "Add to inventory") || !strings.Contains(page, "add_serial=V26%3Cb%3EX") {
			t.Fatalf("%s: unknown ONU panel missing", path)
		}
		if strings.Contains(page, "V26<b>X") {
			t.Fatal("serial not escaped")
		}
	}
	page := a.adminGet(t, "/admin/equipment?tab=onu&add_serial=V26ABC&add_model=V2804N-Z")
	if !strings.Contains(page, `name="serial" value="V26ABC"`) || !strings.Contains(page, `name="model" value="V2804N-Z"`) {
		t.Fatal("the add form is not prefilled")
	}
}
