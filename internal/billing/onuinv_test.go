package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ispbill/internal/store"
)

func TestONUInventoryPairSwapAndCreate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o1, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "zte001", GPONSerial: "ZTEG-0001", MAC: macA, Model: "F670L"})
	if err != nil {
		t.Fatal(err)
	}
	o2, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "zte002", MAC: macB})
	if err != nil {
		t.Fatal(err)
	}
	// a serial, or a GPON serial, cannot be on two items
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "zteg-0001", MAC: macC}); err == nil {
		t.Fatal("a GPON serial was accepted twice")
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "x", GPONSerial: "zte002", MAC: macC}); err == nil {
		t.Fatal("a serial was accepted as a GPON serial of another item")
	}
	// creating a house from an inventory ONU: no MAC typed
	a, err := e.svc.CreateAccount(AccountInput{ApartmentID: store.MigratedApartmentID(e.site), SiteID: e.site, Flat: "F1", EquipmentID: o1.ID})
	if err != nil || !strings.EqualFold(a.MAC, macA) {
		t.Fatalf("%+v %v", a, err)
	}
	e.svc.st.View(func(d *store.Data) {
		if g := d.Equipment[o1.ID]; g.Status != store.EqInstalled || g.AccountID != a.ID {
			t.Fatalf("not installed: %+v", g)
		}
		if g, ok := d.ONTBySerial("ztEg- 0001"); !ok || g.ID != o1.ID {
			t.Fatal("serial lookup by GPON serial failed")
		}
	})
	// the same ONU cannot be given again
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: store.MigratedApartmentID(e.site), SiteID: e.site, Flat: "F2", EquipmentID: o1.ID}); err == nil {
		t.Fatal("an installed ONU was given to a second house")
	}
	// swap: the house takes the other ONU, its MAC follows, the old one goes back in stock
	if err := e.svc.SwapONU(ctx, a.ID, o2.ID, store.EqFaulty, "dead radio"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(a); !strings.EqualFold(got.MAC, macB) {
		t.Fatalf("MAC after swap %s", got.MAC)
	}
	e.svc.st.View(func(d *store.Data) {
		if d.Equipment[o1.ID].Status != store.EqFaulty || d.Equipment[o1.ID].AccountID != "" {
			t.Fatalf("old: %+v", d.Equipment[o1.ID])
		}
		if ont, ok := ONTOfAccount(d, a.ID); !ok || ont.ID != o2.ID {
			t.Fatal("new ONU not installed")
		}
	})
	// the reported MAC replaces both the register and the house
	if err := e.svc.UseReportedMAC(ctx, a.ID, macC); err != nil {
		t.Fatal(err)
	}
	if got := e.get(a); !strings.EqualFold(got.MAC, macC) {
		t.Fatalf("MAC %s", got.MAC)
	}
	e.svc.st.View(func(d *store.Data) {
		if !strings.EqualFold(d.Equipment[o2.ID].MAC, macC) {
			t.Fatal("register MAC not updated")
		}
	})
	// an ONU without a WAN MAC cannot be given out
	bare, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "bare"})
	if err := e.svc.SwapONU(ctx, a.ID, bare.ID, "", ""); err == nil || errors.Is(err, store.ErrNoChange) {
		t.Fatalf("swap to an ONU with no MAC: %v", err)
	}
}

func TestImportONUShorthand(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.ImportEquipment("serial,mac,model,gpon\nZ1," + macA + ",F670L,HWTC1\nZ2,not-a-mac,F670L\n")
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || len(res.Skipped) != 1 {
		t.Fatalf("%+v", res)
	}
}

// A retired ONT keeps its GPON serial: a new item cannot take it (the Postgres unique index refuses it too, so
// both stores answer the same).
func TestONUGPONSerialOfRetiredItemStaysTaken(t *testing.T) {
	e := newEnv(t)
	o, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "hw001", GPONSerial: "HWTC0001", MAC: macA})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReturnEquipment(o.ID, store.EqRetired, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "hw002", GPONSerial: "hwtc0001", MAC: macB}); err == nil || !strings.Contains(err.Error(), "already in the register") {
		t.Fatalf("the GPON serial of a retired item was taken again: %v", err)
	}
}

// A swap takes the old box off the house at once: its ONU row is unlinked and marked returned, a factory reset waits
// for its next check-in, the house keeps its Wi-Fi choice, and the register says returned unless the owner said otherwise.
func TestSwapONUUnlinksTheOldBox(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	o1, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "zte001", MAC: macA})
	o2, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "zte002", MAC: macB})
	a, err := e.svc.CreateAccount(AccountInput{ApartmentID: store.MigratedApartmentID(e.site), SiteID: e.site, Flat: "F1", EquipmentID: o1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.st.Update(func(d *store.Data) error {
		d.ONUs["onuold"] = store.ONU{ID: "onuold", Serial: "zte001", SiteID: e.site, AccountID: a.ID, Link: store.LinkInventory}
		ac := d.Accounts[a.ID]
		ac.HomeWiFi = store.WiFiDesired{Set: true, Same: true, SSID: "Home", PassEnc: "c2VhbGVk"}
		d.Accounts[a.ID] = ac
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SwapONU(ctx, a.ID, o2.ID, "", "swapped"); err != nil {
		t.Fatal(err)
	}
	e.svc.st.View(func(d *store.Data) {
		if d.Equipment[o1.ID].Status != store.EqReturned {
			t.Fatalf("old item status %q", d.Equipment[o1.ID].Status)
		}
		old := d.ONUs["onuold"]
		if old.AccountID != "" || !old.State.Returned {
			t.Fatalf("old ONU still linked: %+v", old)
		}
		var reset bool
		for _, tk := range d.ONUTasksOf("onuold") {
			if tk.Kind == store.TaskFactoryReset && tk.Status == store.TaskPending {
				reset = true
			}
		}
		if !reset {
			t.Fatal("no factory reset queued for the returned box")
		}
		if !d.Accounts[a.ID].HomeWiFi.Set {
			t.Fatal("the house lost its Wi-Fi choice")
		}
	})
}

// trustedONU puts an ONU row that the owner trusted ("Trust this ONU") in the store for the serial.
func (e *env) trustedONU(t *testing.T, id, serial, acct string) {
	t.Helper()
	if err := e.st.Update(func(d *store.Data) error {
		d.ONUs[id] = store.ONU{ID: id, Serial: serial, AccountID: acct, Sec: store.ONUSec{MACTrusted: "02:00:0A:06:03:14", MACCheck: store.MACOK}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) trustOf(id string) (m string) {
	e.st.View(func(d *store.Data) { m = d.ONUs[id].Sec.MACTrusted })
	return m
}

func TestTrustedMACIsClearedWhenTheRegisteredWANMACChanges(t *testing.T) {
	e := newEnv(t)
	it, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "TRUST1", MAC: "AA:BB:CC:DD:EE:81"})
	if err != nil {
		t.Fatal(err)
	}
	e.trustedONU(t, "o1", "TRUST1", "")
	// an edit that keeps the MAC keeps the trust
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: it.ID, Kind: "ont", Serial: "TRUST1", MAC: "aa-bb-cc-dd-ee-81", Model: "F601"}); err != nil {
		t.Fatal(err)
	}
	if e.trustOf("o1") == "" {
		t.Fatal("trust cleared by an edit that did not change the MAC")
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: it.ID, Kind: "ont", Serial: "TRUST1", MAC: "AA:BB:CC:DD:EE:82"}); err != nil {
		t.Fatal(err)
	}
	if got := e.trustOf("o1"); got != "" {
		t.Fatalf("trust survived a WAN MAC change: %q", got)
	}
}

func TestTrustedMACIsClearedWhenTheItemIsReturnedOrRetired(t *testing.T) {
	for _, status := range []string{store.EqReturned, store.EqRetired} {
		e := newEnv(t)
		it, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "TRUST2", MAC: "AA:BB:CC:DD:EE:83"})
		if err != nil {
			t.Fatal(err)
		}
		e.trustedONU(t, "o2", "TRUST2", "")
		if err := e.svc.ReturnEquipment(it.ID, status, "x"); err != nil {
			t.Fatal(err)
		}
		if got := e.trustOf("o2"); got != "" {
			t.Fatalf("%s: trust survived: %q", status, got)
		}
	}
}

// The TR-069 serial is a third serial of an ONT: it finds the item, is unique across all three fields (retired items
// included), is kept by an edit that does not mention it, and is cleared by an explicit empty value.
func TestTR069SerialUniquenessAndKeep(t *testing.T) {
	e := newEnv(t)
	p := func(s string) *string { return &s }
	a, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "V26051300697", GPONSerial: "VSOL00127E0F", TR069Serial: p("1234568229f127e0f"), MAC: macA})
	if err != nil || a.TR069Serial != "1234568229F127E0F" {
		t.Fatalf("%v %+v", err, a)
	}
	// taken as a serial, as a GPON serial or as a TR-069 serial by another item
	for _, in := range []EquipmentInput{
		{Kind: "ont", Serial: "1234568229F127E0F", MAC: macB},
		{Kind: "ont", Serial: "x1", GPONSerial: "1234568229F127E0F", MAC: macB},
		{Kind: "ont", Serial: "x2", TR069Serial: p("1234568229F127E0F"), MAC: macB},
		{Kind: "ont", Serial: "x3", TR069Serial: p("vsol00127e0f"), MAC: macB},
		{Kind: "ont", Serial: "x4", TR069Serial: p("v26051300697"), MAC: macB},
	} {
		if _, err := e.svc.SaveEquipment(in); err == nil || !strings.Contains(err.Error(), "already in the register") {
			t.Fatalf("%+v accepted: %v", in, err)
		}
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "router", Serial: "r1", TR069Serial: p("T9")}); err == nil {
		t.Fatal("a router got a TR-069 serial")
	}
	// an edit that leaves the field out keeps it
	a2, err := e.svc.SaveEquipment(EquipmentInput{ID: a.ID, Kind: "ont", Serial: a.Serial, GPONSerial: a.GPONSerial, MAC: macA, Model: "V2804N-Z"})
	if err != nil || a2.TR069Serial != a.TR069Serial {
		t.Fatalf("kept? %v %+v", err, a2)
	}
	// and an explicit empty value clears it
	a3, err := e.svc.SaveEquipment(EquipmentInput{ID: a.ID, Kind: "ont", Serial: a.Serial, GPONSerial: a.GPONSerial, TR069Serial: p(""), MAC: macA})
	if err != nil || a3.TR069Serial != "" {
		t.Fatalf("cleared? %v %+v", err, a3)
	}
}
