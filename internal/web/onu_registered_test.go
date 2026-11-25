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
	// without the MAC that was shown, or after the ONU moved on to another MAC, nothing is trusted
	for _, mac := range []string{"", "AA:BB:CC:DD:EE:78"} {
		r = a.admin("POST", "/admin/onus/trust", url.Values{"onu": {"onuw"}, "mac": {mac}, "current_password": {pw}})
		_ = body(t, r)
		if a.onu("onuw").Sec.MACTrusted != "" {
			t.Fatalf("trusted with the shown MAC %q", mac)
		}
	}
	if page := a.adminGet(t, "/admin/onus"); !strings.Contains(page, `name="mac" value="AA:BB:CC:DD:EE:77"`) {
		t.Fatal("the trust form does not carry the MAC it shows")
	}
	r = a.admin("POST", "/admin/onus/trust", url.Values{"onu": {"onuw"}, "mac": {"aa-bb-cc-dd-ee-77"}, "current_password": {pw}})
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

func TestUnknownONUSuggestsTheInventoryItemAndTheOwnerLinksIt(t *testing.T) {
	a := newONUApp(t)
	now := time.Now()
	a.st.Update(func(d *store.Data) error {
		d.Equipment["eqv"] = store.Equipment{ID: "eqv", Kind: store.KindONT, Serial: "V26051300697", GPONSerial: "VSOL00127E0F", MAC: "68:22:9F:12:7E:10", Model: "V2804N-Z", Status: store.EqInStock, CreatedAt: now}
		d.Settings["acs.unknown"] = store.Setting{Key: "acs.unknown", UpdatedAt: now, Value: `[{"serial":"1234568229F127E0F","oui":"00E0DF","model":"VSOL V414","site_id":"` + a.site + `","site_name":"Block","ip":"10.6.5.9","lease_mac":"68:22:9F:12:7E:11","first_seen":"` + now.Format(time.RFC3339) + `","last_seen":"` + now.Format(time.RFC3339) + `","count":3}]`}
		return nil
	})
	for _, path := range []string{"/admin/routers/view?id=" + a.site + "&tab=onus", "/admin/equipment?tab=onu", "/admin/onus"} {
		page := a.adminGet(t, path)
		for _, want := range []string{"Looks like", "VSOL00127E0F", "68:22:9F:12:7E:0F in the serial", "management MAC 68:22:9F:12:7E:11 near WAN MAC 68:22:9F:12:7E:10", "Link to this ONU", "would make a duplicate", "Add to inventory"} {
			if !strings.Contains(page, want) {
				t.Fatalf("%s: %q missing", path, want)
			}
		}
	}
	link := func(pass string) {
		_ = body(t, a.admin("POST", "/admin/onus/link-unknown", url.Values{"item": {"eqv"}, "serial": {"1234568229F127E0F"}, "current_password": {pass}}))
	}
	tr := func() (s string) {
		a.st.View(func(d *store.Data) { s = d.Equipment["eqv"].TR069Serial })
		return
	}
	link("wrong")
	if tr() != "" {
		t.Fatal("linked with a wrong password")
	}
	link(pw)
	if tr() != "1234568229F127E0F" {
		t.Fatalf("not linked: %q", tr())
	}
	if page := a.adminGet(t, "/admin/onus"); strings.Contains(page, "Looks like") {
		t.Fatal("still listed as unknown")
	}
	// the item's edit form shows it and changing it needs the password
	page := a.adminGet(t, "/admin/equipment/view?id=eqv")
	if !strings.Contains(page, `name="tr069_serial" value="1234568229F127E0F"`) {
		t.Fatal("the edit form does not show the TR-069 serial")
	}
	save := func(v, pass string) {
		_ = body(t, a.admin("POST", "/admin/equipment/save", url.Values{"id": {"eqv"}, "kind": {"ont"}, "serial": {"V26051300697"}, "gpon_serial": {"VSOL00127E0F"}, "mac": {"68:22:9F:12:7E:10"},
			"model": {"V2804N-Z"}, "tr069_serial": {v}, "current_password": {pass}}))
	}
	save("", "wrong")
	if tr() == "" {
		t.Fatal("cleared with a wrong password")
	}
	save("", pw)
	if tr() != "" {
		t.Fatalf("not cleared: %q", tr())
	}
}
