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
