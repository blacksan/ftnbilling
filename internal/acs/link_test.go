package acs

import (
	"errors"
	"strings"
	"testing"

	"ispbill/internal/store"
)

// The V-SOL case in the fake's addressing: the inventory WAN MAC is ...03:13, the label MAC ...03:12, the box's management
// MAC (lease of 10.6.3.20) is ...03:14, and its firmware reports "12345" + the label MAC as its serial.
const (
	vsolWAN    = "02:00:0A:06:03:13"
	vsolSerial = "1234502000A060312"
)

func (f *fixture) addONT(id, serial, gpon, mac, model string) {
	f.t.Helper()
	if err := f.st.Update(func(d *store.Data) error {
		d.Equipment[id] = store.Equipment{ID: id, Kind: store.KindONT, Serial: serial, GPONSerial: gpon, MAC: mac, Model: model, Status: store.EqInStock, CreatedAt: f.now}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestSuggestRules(t *testing.T) {
	f := newFixture(t)
	f.addONT("eq-v", "V26051300697", "VSOL00127E0F", vsolWAN, "V2804N-Z")
	f.addONT("eq-far", "OTHER1", "", "AA:BB:CC:00:00:10", "XYZ")
	f.addONT("eq-model", "OTHER2", "", "AA:BB:CC:00:00:50", "VSOL V414")
	f.st.Update(func(d *store.Data) error {
		d.Equipment["eq-ret"] = store.Equipment{ID: "eq-ret", Kind: store.KindONT, Serial: "RET", MAC: vsolWAN, Status: store.EqRetired}
		d.Equipment["eq-rt"] = store.Equipment{ID: "eq-rt", Kind: store.KindRouter, Serial: "RT", MAC: vsolWAN, Status: store.EqInstalled}
		return nil
	})
	sug := func(u UnknownONU) []Suggestion {
		var out []Suggestion
		f.st.View(func(d *store.Data) { out = Suggest(d, u) })
		return out
	}
	// the MAC is in the serial (label MAC, one below the WAN MAC)
	s := sug(UnknownONU{Serial: vsolSerial, Model: "?"})
	if len(s) != 1 || s[0].EquipmentID != "eq-v" || s[0].Strength != 3 || !strings.Contains(s[0].Why[0], "02:00:0A:06:03:12 in the serial") {
		t.Fatalf("%+v", s)
	}
	// lease MAC near the WAN MAC (no MAC in the serial)
	s = sug(UnknownONU{Serial: "RANDOMSERIAL1", LeaseMAC: "02:00:0A:06:03:14"})
	if len(s) != 1 || s[0].EquipmentID != "eq-v" || s[0].Strength != 2 || !strings.Contains(s[0].Why[0], "management MAC 02:00:0A:06:03:14 near WAN MAC "+vsolWAN) {
		t.Fatalf("%+v", s)
	}
	// the model, weakest
	s = sug(UnknownONU{Serial: "RANDOMSERIAL2", Model: "VSOL V414"})
	if len(s) != 1 || s[0].EquipmentID != "eq-model" || s[0].Strength != 1 {
		t.Fatalf("%+v", s)
	}
	// a random serial, a far lease and an unrelated model: nothing (and retired items and routers are never offered)
	if s = sug(UnknownONU{Serial: "SNSTRANGER", Model: "F670L", OUI: "0019C6", LeaseMAC: "02:00:0A:06:03:5F"}); len(s) != 0 {
		t.Fatalf("a random serial got suggestions: %+v", s)
	}
	// 9 apart (…1C) is not near; another OUI is not near
	if s = sug(UnknownONU{Serial: "X", LeaseMAC: "02:00:0A:06:03:1B"}); len(s) != 1 {
		t.Fatalf("8 apart should match: %+v", s)
	}
	if s = sug(UnknownONU{Serial: "X", LeaseMAC: "02:00:0A:06:03:1C"}); len(s) != 0 {
		t.Fatalf("9 apart matched: %+v", s)
	}
	// an item that already has a TR-069 serial is not offered again
	f.st.Update(func(d *store.Data) error {
		e := d.Equipment["eq-v"]
		e.TR069Serial = "SOMETHING"
		d.Equipment["eq-v"] = e
		return nil
	})
	if s = sug(UnknownONU{Serial: vsolSerial}); len(s) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestLinkUnknownEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	// the V-SOL case: installed at house a1, its TR-069 serial is neither its serial nor its GPON serial
	f.inventory("V26051300697", "VSOL00127E0F", vsolWAN, "a1")
	c := newZTE(t, f, vsolSerial, "10.6.3.20")
	c.ProductClass = "VSOL V414"
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	if f.onuCount() != 0 {
		t.Fatal("the unknown serial was stored as a device")
	}
	l := f.svc.UnknownONUs("")
	if len(l) != 1 || len(l[0].Suggestions) != 1 || l[0].Suggestions[0].EquipmentID != "eq-V26051300697" || l[0].LeaseMAC != leaseMAC20 {
		t.Fatalf("unknown list: %+v", l)
	}
	if len(l[0].Suggestions[0].Why) != 2 {
		t.Fatalf("reasons: %v", l[0].Suggestions[0].Why)
	}
	// nothing was linked on its own
	f.st.View(func(d *store.Data) {
		if d.Equipment["eq-V26051300697"].TR069Serial != "" {
			t.Fatal("linked without the owner")
		}
	})
	// wrong inputs
	if _, err := f.svc.LinkUnknown("owner", "1.2.3.4", "eq-V26051300697", "NOTLISTED"); err != ErrNotUnknown {
		t.Fatalf("%v", err)
	}
	if _, err := f.svc.LinkUnknown("owner", "1.2.3.4", "nope", vsolSerial); err != ErrNoItem {
		t.Fatalf("%v", err)
	}
	e, err := f.svc.LinkUnknown("owner", "1.2.3.4", "eq-V26051300697", " "+strings.ToLower(vsolSerial))
	if err != nil || e.TR069Serial != vsolSerial {
		t.Fatalf("%v %+v", err, e)
	}
	if len(f.svc.UnknownONUs("")) != 0 {
		t.Fatal("still listed as unknown after linking")
	}
	// audited
	audited := false
	f.st.View(func(d *store.Data) {
		for _, ev := range d.Events {
			if ev.Kind == "audit" && ev.Actor == "owner" && strings.Contains(ev.Msg, vsolSerial) && strings.Contains(ev.Msg, "linked") {
				audited = true
			}
		}
	})
	if !audited {
		t.Fatal("the link was not audited")
	}
	// the next Inform is the registered ONU: the device record is made, the MAC check passes (...14 near ...13), and the
	// house comes from the inventory item although the match was via the TR-069 serial
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	o := f.onuBySerial(vsolSerial)
	if o.ID == "" || o.Sec.MACCheck != store.MACOK || o.AccountID != "a1" || o.Link != store.LinkInventory {
		t.Fatalf("not registered after the link: %+v link=%q acct=%q", o.Sec, o.Link, o.AccountID)
	}
	// linking twice is refused (the serial is registered now, so it is no longer unknown)
	if _, err := f.svc.LinkUnknown("owner", "", "eq-V26051300697", vsolSerial); err != ErrNotUnknown {
		t.Fatalf("%v", err)
	}
}

func TestLinkUnknownRefusesTakenSerialsAndBadItems(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.addONT("eq-a", "AAA1", "", vsolWAN, "")
	f.addONT("eq-b", "BBB1", "", "AA:BB:CC:00:00:10", "")
	c := newZTE(t, f, vsolSerial, "10.6.3.20")
	_ = c.session("0 BOOTSTRAP")
	if _, err := f.svc.LinkUnknown("o", "", "eq-a", vsolSerial); err != nil {
		t.Fatal(err)
	}
	// a second unknown with the same serial cannot be linked to another item: it is registered now
	f.st.Update(func(d *store.Data) error { // a stale entry for the same serial, as if two lists raced
		writeUnknown(d, []UnknownONU{{Serial: vsolSerial, LastSeen: f.now, FirstSeen: f.now}}, f.now)
		return nil
	})
	if _, err := f.svc.LinkUnknown("o", "", "eq-b", vsolSerial); err == nil {
		t.Fatal("the same serial was linked to two items")
	}
	f.st.Update(func(d *store.Data) error {
		d.Equipment["eq-r"] = store.Equipment{ID: "eq-r", Kind: store.KindONT, Serial: "RRR", Status: store.EqRetired}
		d.Equipment["eq-rt"] = store.Equipment{ID: "eq-rt", Kind: store.KindRouter, Serial: "RT", Status: store.EqInstalled}
		writeUnknown(d, []UnknownONU{{Serial: "NEWSERIAL1", LastSeen: f.now, FirstSeen: f.now}}, f.now)
		return nil
	})
	for _, id := range []string{"eq-r", "eq-rt", "eq-a"} {
		if _, err := f.svc.LinkUnknown("o", "", id, "NEWSERIAL1"); err != ErrItemUnfit {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestTR069SerialFindsTheONT(t *testing.T) {
	d := &store.Data{Equipment: map[string]store.Equipment{}}
	d.Equipment["e"] = store.Equipment{ID: "e", Kind: store.KindONT, Serial: "S1", GPONSerial: "G1", TR069Serial: "T1 X", Status: store.EqInStock}
	for _, s := range []string{"S1", "g1", "t1x"} {
		if e, ok := d.ONTBySerial(s); !ok || e.ID != "e" {
			t.Fatalf("%s not found", s)
		}
	}
	if _, ok := d.ONTBySerial("T2"); ok {
		t.Fatal("found a serial nobody has")
	}
	e := d.Equipment["e"]
	e.Status = store.EqRetired
	d.Equipment["e"] = e
	if _, ok := d.ONTBySerial("T1X"); ok {
		t.Fatal("a retired item was found")
	}
}

// A box already checks in as the item (under its GPON serial): an unknown check-in whose serial embeds the item's MAC is
// another box. It is not suggested and cannot be linked; and if the owner sets the TR-069 serial anyway (edit form or
// PUT), the copy is held as a second device record and the real box keeps its house.
func TestAForgedSerialCannotTakeAnItemWhoseBoxAlreadyChecksIn(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("V26051300697", "VSOL00127E0F", vsolWAN, "a1")
	c := linkedWithOwnLogin(t, f, "VSOL00127E0F", "10.6.3.20")
	real := f.onuBySerial("VSOL00127E0F")
	if real.AccountID != "a1" {
		t.Fatalf("setup: %+v", real)
	}
	a := newZTE(t, f, vsolSerial, "10.6.3.21") // "12345" + the label MAC, lease ...03:15 near the WAN MAC
	if err := a.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	l := f.svc.UnknownONUs("")
	if len(l) != 1 || len(l[0].Suggestions) != 0 {
		t.Fatalf("the item whose box checks in was suggested: %+v", l)
	}
	if _, err := f.svc.LinkUnknown("owner", "", "eq-V26051300697", vsolSerial); !errors.Is(err, ErrItemInUse) {
		t.Fatalf("linked a second box to an item in use: %v", err)
	}
	f.st.Update(func(d *store.Data) error { // the owner's edit form / PUT /equipment/{id}/tr069-serial
		e := d.Equipment["eq-V26051300697"]
		e.TR069Serial = vsolSerial
		d.Equipment[e.ID] = e
		return nil
	})
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := a.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	settle(f, c, a)
	if a.setCount() != 0 {
		t.Fatalf("the copy got settings: %v", a.rpcs)
	}
	dup := f.onuBySerial(vsolSerial)
	if dup.ID == "" || dup.Sec.MACCheck != store.MACWaiting || dup.AccountID != "" || !strings.Contains(dup.Sec.MACWaitNote, "second device record") {
		t.Fatalf("copy: %+v acct=%q", dup.Sec, dup.AccountID)
	}
	if now := f.onuBySerial("VSOL00127E0F"); now.AccountID != "a1" || now.State.Returned {
		t.Fatalf("the real box lost its house: %+v", now)
	}
}
