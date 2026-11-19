package billing

import (
	"context"
	"errors"
	"testing"

	"ispbill/internal/store"
)

// flag simulates what a migration leaves behind: account id carries mac and is flagged.
func (e *env) flag(t *testing.T, id, mac string) {
	t.Helper()
	if err := e.st.Update(func(d *store.Data) error {
		a := d.Accounts[id]
		a.MAC, a.MACConflict = mac, true
		d.Accounts[id] = a
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChangeMACClearsTheConflictFlag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, _ := e.addSite(t, "Block B")
	owner := e.flat(t, "Flat 1", mac1)
	flagged := e.flatAt(t, b, "Flat 1", macB)
	e.flag(t, flagged.ID, mac1)
	if a := e.get(flagged); !a.MACConflict || a.MAC != mac1 {
		t.Fatalf("setup: %+v", a)
	}
	// Re-check while the other flat still owns the MAC: refused, still flagged.
	var dup *DuplicateMACError
	if err := e.svc.ChangeMAC(ctx, flagged.ID, mac1); !errors.As(err, &dup) || dup.AccountID != owner.ID {
		t.Fatalf("re-check with an owner: %v", err)
	}
	if !e.get(flagged).MACConflict {
		t.Fatal("a refused re-check must leave the flag")
	}
	// Fix: give it a free MAC. The flag clears, and the MAC is now protected.
	const fixed = "AA:BB:CC:DD:EE:51"
	if err := e.svc.ChangeMAC(ctx, flagged.ID, fixed); err != nil {
		t.Fatal(err)
	}
	if a := e.get(flagged); a.MACConflict || a.MAC != fixed {
		t.Fatalf("after the fix: %+v", a)
	}
	_, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 7", MAC: fixed})
	if !errors.As(err, &dup) || dup.AccountID != flagged.ID || dup.SiteID != b {
		t.Fatalf("the fixed MAC must be protected: %v", err)
	}
	// Re-check where the MAC turned out to be free (the other owner was closed): same MAC clears the flag.
	e.flag(t, flagged.ID, "AA:BB:CC:DD:EE:52")
	if err := e.svc.ChangeMAC(ctx, flagged.ID, "AA:BB:CC:DD:EE:52"); err != nil {
		t.Fatalf("re-check of a free MAC: %v", err)
	}
	if a := e.get(flagged); a.MACConflict || a.MAC != "AA:BB:CC:DD:EE:52" {
		t.Fatalf("after the re-check: %+v", a)
	}
	// An unflagged account given its own MAC again is still "no change".
	if err := e.svc.ChangeMAC(ctx, flagged.ID, "AA:BB:CC:DD:EE:52"); err != nil {
		t.Fatalf("no-op: %v", err)
	}
}

func TestAssignEquipmentClearsTheConflictFlag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b, _ := e.addSite(t, "Block B")
	e.flat(t, "Flat 1", mac1)
	flagged := e.flatAt(t, b, "Flat 1", macB)
	e.flag(t, flagged.ID, mac1)
	ont, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "sn-9", MAC: "AA:BB:CC:DD:EE:61"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AssignEquipment(ctx, ont.ID, flagged.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if a := e.get(flagged); a.MACConflict || a.MAC != "AA:BB:CC:DD:EE:61" {
		t.Fatalf("%+v", a)
	}
}

func TestSaveEquipmentRefusesADuplicateLiveMAC(t *testing.T) {
	e := newEnv(t)
	first, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "A1", MAC: "aa-bb-cc-dd-ee-70"})
	if err != nil || first.MAC != "AA:BB:CC:DD:EE:70" {
		t.Fatalf("%+v %v", first, err)
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "router", Serial: "B1", MAC: "AA:BB:CC:DD:EE:70"}); !errors.Is(err, ErrDuplicateEquipmentMAC) {
		t.Fatalf("new item: %v", err)
	}
	other, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "C1", MAC: "AA:BB:CC:DD:EE:71"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: other.ID, Kind: "ont", Serial: "C1", MAC: "AA:BB:CC:DD:EE:70"}); !errors.Is(err, ErrDuplicateEquipmentMAC) {
		t.Fatalf("edit onto a taken MAC: %v", err)
	}
	// Saving an item with its own MAC is fine.
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: first.ID, Kind: "ont", Serial: "A1", MAC: "AA:BB:CC:DD:EE:70", Model: "x"}); err != nil {
		t.Fatalf("own MAC: %v", err)
	}
}

func TestFlaggedMACIsNotAnUnregisteredDevice(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	e.flat(t, "Flat 1", mac1)
	flagged := e.flatAt(t, b, "Flat 1", macB)
	e.flag(t, flagged.ID, mac1)
	// A MAC held by an account of this router is not an unregistered device.
	e.svc.NoteDevice(e.site, mac1, "10.0.0.9", "portal")
	e.st.View(func(d *store.Data) {
		if len(d.Devices) != 0 {
			t.Fatalf("%v", d.Devices)
		}
	})
	// Seen on another router, it is listed there as belonging to Flat 1, never as a free device.
	e.svc.NoteDevice(b, mac1, "10.0.0.9", "portal")
	list := e.svc.Unregistered(b, false)
	if len(list) != 1 || list[0].BelongsTo == nil || list[0].BelongsTo.Unit != "Flat 1" || list[0].Suggested != nil {
		t.Fatalf("%+v", list)
	}
}
