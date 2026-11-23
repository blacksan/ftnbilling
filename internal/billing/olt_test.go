package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ispbill/internal/store"
)

const (
	macOLT = "48:21:B8:10:D7:72" // the V-SOL V1600GS-R on ether3
	macONU = "68:22:9F:00:00:01"
)

func (e *env) olt(t *testing.T, in OLTInput) store.Equipment {
	t.Helper()
	if in.SiteID == "" {
		in.SiteID = e.site
	}
	o, err := e.svc.SaveOLT(in)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestSaveOLTValidationAndMACUniqueness(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SetDataKey(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	aptA := e.apt(t, "MG One", e.site)
	other, _ := e.addSite(t, "Block Z")
	aptZ := e.apt(t, "Elsewhere", other)

	o := e.olt(t, OLTInput{Name: "Block A OLT", Model: "V1600GS-R", Serial: "vs1", MAC: "48-21-b8-10-d7-72", RouterPort: "ether3", PONPorts: 4,
		MgmtIP: "192.168.1.10", MgmtUser: "admin", MgmtPass: "s3cret-pass", PONApartments: map[int][]string{1: {aptA.ID}}})
	if o.MAC != macOLT || o.Serial != "VS1" || o.Status != store.EqInstalled || o.PONApartments["1"][0] != aptA.ID {
		t.Fatalf("%+v", o)
	}
	if o.MgmtPass != "" {
		t.Fatal("the sealed password is returned to the caller")
	}
	// the stored password is sealed, and only RevealOLTPassword opens it (with an audit event)
	e.svc.st.View(func(d *store.Data) {
		g := d.Equipment[o.ID]
		if g.MgmtPass == "" || strings.Contains(g.MgmtPass, "s3cret") {
			t.Fatalf("password not sealed: %q", g.MgmtPass)
		}
	})
	if pw, err := e.svc.RevealOLTPassword(o.ID); err != nil || pw != "s3cret-pass" {
		t.Fatalf("reveal %q %v", pw, err)
	}
	// an edit with an empty password keeps it
	if _, err := e.svc.SaveOLT(OLTInput{ID: o.ID, Name: "Block A OLT", Serial: "VS1", MAC: macOLT, SiteID: e.site, RouterPort: "ether3", PONPorts: 4}); err != nil {
		t.Fatal(err)
	}
	if pw, _ := e.svc.RevealOLTPassword(o.ID); pw != "s3cret-pass" {
		t.Fatal("an edit lost the password")
	}
	if g := e.svc.PlacementOf(o.ID); g.OLTID != "" {
		t.Fatal("an OLT has no placement")
	}

	bad := map[string]OLTInput{
		"no name":                     {MAC: "48:21:B8:10:D7:73", SiteID: e.site},
		"no mac":                      {Name: "x", SiteID: e.site},
		"bad mac":                     {Name: "x", MAC: "nope", SiteID: e.site},
		"dup mac":                     {Name: "x", MAC: macOLT, SiteID: e.site},
		"dup port":                    {Name: "x", MAC: "48:21:B8:10:D7:74", SiteID: e.site, RouterPort: "ETHER3"},
		"dup serial":                  {Name: "x", MAC: "48:21:B8:10:D7:75", Serial: "VS1", SiteID: e.site},
		"bad ip":                      {Name: "x", MAC: "48:21:B8:10:D7:76", SiteID: e.site, MgmtIP: "not-an-ip"},
		"too many pons":               {Name: "x", MAC: "48:21:B8:10:D7:77", SiteID: e.site, PONPorts: 500},
		"pon out of range":            {Name: "x", MAC: "48:21:B8:10:D7:78", SiteID: e.site, PONPorts: 2, PONApartments: map[int][]string{3: {aptA.ID}}},
		"apartment of another router": {Name: "x", MAC: "48:21:B8:10:D7:79", SiteID: e.site, PONPorts: 4, PONApartments: map[int][]string{1: {aptZ.ID}}},
		"deployed without router":     {Name: "x", MAC: "48:21:B8:10:D7:7A", Status: store.EqInstalled},
		"unknown router":              {Name: "x", MAC: "48:21:B8:10:D7:7B", SiteID: "nope"},
		"bad status":                  {Name: "x", MAC: "48:21:B8:10:D7:7C", SiteID: e.site, Status: "retired"},
	}
	for name, in := range bad {
		if _, err := e.svc.SaveOLT(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// an ONT item with the OLT's MAC is refused by the register, an OLT with a house's MAC is refused too
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "z", MAC: macOLT}); !errors.Is(err, ErrDuplicateEquipmentMAC) {
		t.Fatalf("ONT with the OLT's MAC: %v", err)
	}
	e.unitAt(t, aptA, e.site, "H1", macA)
	if _, err := e.svc.SaveOLT(OLTInput{Name: "y", MAC: macA, SiteID: e.site}); err == nil {
		t.Fatal("an OLT took a customer's MAC")
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "olt", Serial: "q", MAC: macA}); err == nil {
		t.Fatal("the generic form let an OLT take a customer's MAC")
	}
	// no data key: a password cannot be stored
	e2 := newEnv(t)
	if _, err := e2.svc.SaveOLT(OLTInput{Name: "n", MAC: macOLT, SiteID: e2.site, MgmtPass: "x"}); !errors.Is(err, ErrNoDataKey) {
		t.Fatalf("no key: %v", err)
	}
	// stock: no router
	st := e.olt(t, OLTInput{Name: "Spare", MAC: "48:21:B8:10:D7:90", SiteID: "-", Status: store.EqInStock})
	_ = st
}

func TestPONMappingAndONUPlacementWarnings(t *testing.T) {
	e := newEnv(t)
	aptA := e.apt(t, "MG One", e.site)
	aptB := e.apt(t, "Block B", e.site)
	o := e.olt(t, OLTInput{Name: "OLT1", MAC: macOLT, RouterPort: "ether3", PONPorts: 4, PONApartments: map[int][]string{1: {aptA.ID}, 2: {aptB.ID}}})
	onu, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "Z1", MAC: macONU, SetPlacement: true, OLTID: o.ID, PONPort: 1})
	if err != nil || onu.OLTID != o.ID || onu.PONPort != 1 {
		t.Fatalf("%+v %v", onu, err)
	}
	// placement validation
	for name, in := range map[string]EquipmentInput{
		"pon without olt": {ID: onu.ID, Kind: "ont", Serial: "Z1", MAC: macONU, SetPlacement: true, PONPort: 2},
		"pon too big":     {ID: onu.ID, Kind: "ont", Serial: "Z1", MAC: macONU, SetPlacement: true, OLTID: o.ID, PONPort: 9},
		"not an olt":      {ID: onu.ID, Kind: "ont", Serial: "Z1", MAC: macONU, SetPlacement: true, OLTID: onu.ID, PONPort: 1},
	} {
		if _, err := e.svc.SaveEquipment(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// an edit without SetPlacement keeps the placement
	if g, err := e.svc.SaveEquipment(EquipmentInput{ID: onu.ID, Kind: "ont", Serial: "Z1", MAC: macONU, Model: "F601"}); err != nil || g.OLTID != o.ID {
		t.Fatalf("placement lost: %+v %v", g, err)
	}
	// creating a customer from the ONU: its PON serves aptA
	if ids := e.svc.PlacementOf(onu.ID).ApartmentIDs; len(ids) != 1 || ids[0] != aptA.ID {
		t.Fatalf("preselect %v", ids)
	}
	a, err := e.svc.CreateAccount(AccountInput{ApartmentID: aptA.ID, SiteID: e.site, Unit: "A1", EquipmentID: onu.ID})
	if err != nil {
		t.Fatal(err)
	}
	if w := e.svc.PlacementOf(onu.ID).Warnings; len(w) != 0 {
		t.Fatalf("a served apartment warns: %v", w)
	}
	// the house moves to the apartment served by PON 2: warn, never block
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: onu.ID, Kind: "ont", Serial: "Z1", MAC: macONU, SetPlacement: true, OLTID: o.ID, PONPort: 2}); err != nil {
		t.Fatalf("a mismatch must not block: %v", err)
	}
	w := e.svc.PlacementOf(onu.ID).Warnings
	if len(w) != 1 || !strings.Contains(w[0], "A1") || !strings.Contains(w[0], "Block B") {
		t.Fatalf("warnings %v", w)
	}
	_ = a
	// reducing the port count under an ONU is refused
	if _, err := e.svc.SaveOLT(OLTInput{ID: o.ID, Name: "OLT1", MAC: macOLT, SiteID: e.site, RouterPort: "ether3", PONPorts: 1}); err == nil {
		t.Fatal("PON ports reduced under an ONU on port 2")
	}
	// removing the mapping: nothing is known, nothing to warn about
	if _, err := e.svc.SaveOLT(OLTInput{ID: o.ID, Name: "OLT1", MAC: macOLT, SiteID: e.site, RouterPort: "ether3", PONPorts: 4, PONApartments: map[int][]string{}}); err != nil {
		t.Fatal(err)
	}
	if w := e.svc.PlacementOf(onu.ID).Warnings; len(w) != 0 {
		t.Fatalf("no mapping, warnings %v", w)
	}
}

func TestAccountsRefuseAnOLTMAC(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	apt := e.apt(t, "MG One", e.site)
	e.olt(t, OLTInput{Name: "Block A OLT", MAC: macOLT, RouterPort: "ether3"})
	var oe *OLTMACError
	_, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "X1", MAC: macOLT})
	if !errors.Is(err, ErrOLTMAC) || !errors.As(err, &oe) || err.Error() != "That MAC belongs to OLT Block A OLT" {
		t.Fatalf("create: %v", err)
	}
	h := e.unitAt(t, apt, e.site, "X2", macA)
	if err := e.svc.ChangeMAC(ctx, h.ID, macOLT); !errors.Is(err, ErrOLTMAC) {
		t.Fatalf("change: %v", err)
	}
	if _, err := e.svc.AttachDevice(ctx, e.site, macOLT, h.ID); !errors.Is(err, ErrOLTMAC) {
		t.Fatalf("attach: %v", err)
	}
	if err := e.svc.UseReportedMAC(ctx, h.ID, macOLT); !errors.Is(err, ErrOLTMAC) {
		t.Fatalf("reported: %v", err)
	}
	spare, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "S1", MAC: macB})
	_ = spare
	// swap to an ONU can never carry the OLT's MAC (the register refuses the duplicate)
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "S2", MAC: macOLT}); err == nil {
		t.Fatal("ONT with OLT mac")
	}
	if got := e.get(h); got.MAC != macA {
		t.Fatalf("MAC changed: %s", got.MAC)
	}
	// a retired OLT frees its MAC
	e.svc.st.Update(func(d *store.Data) error {
		for id, q := range d.Equipment {
			if q.Kind == store.KindOLT {
				q.Status = store.EqRetired
				d.Equipment[id] = q
			}
		}
		return nil
	})
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "X3", MAC: macOLT}); err != nil {
		t.Fatalf("retired OLT still blocks: %v", err)
	}
}

func TestClassificationOfNewDevices(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	o := e.olt(t, OLTInput{Name: "Block A OLT", Model: "V1600GS-R", MAC: macOLT, RouterPort: "ether3", PONPorts: 4, PONApartments: map[int][]string{1: {apt.ID}}})
	onu, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "ZTE001", MAC: macONU, Model: "F601", SetPlacement: true, OLTID: o.ID, PONPort: 1})
	e.ownIface("ether5", macRouterPort)
	e.bridgeHost(macRouterPort, "ether5", "true")
	e.bridgeHost(macOLT, "ether3", "false") // the OLT on ether3 never asks for an address
	e.bridgeHost(macONU, "ether5", "false")
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.dynLease(macHuawei, "10.5.50.11", "HG8010H", "bound")
	e.reconcile(t)
	got := unregByMAC(e)
	if _, ok := got[macRouterPort]; ok {
		t.Fatal("router MAC listed")
	}
	if c := got[macOLT].Class; c.Class != ClassOLT || c.Label != "OLT V1600GS-R Block A OLT" || c.Creatable || c.EquipmentID != o.ID {
		t.Fatalf("olt: %+v", c)
	}
	if c := got[macONU].Class; c.Class != ClassONU || c.Label != "ONU F601 · ZTE001" || !c.Creatable || c.EquipmentID != onu.ID || len(c.ApartmentIDs) != 1 || c.ApartmentIDs[0] != apt.ID {
		t.Fatalf("onu: %+v", c)
	}
	if c := got[macThinkPad].Class; c.Class != ClassUnknown || !c.Creatable || !c.NeverAsked {
		t.Fatalf("unknown bridge-only: %+v", c)
	}
	if c := got[macHuawei].Class; c.Class != ClassUnknown || c.NeverAsked {
		t.Fatalf("unknown with a lease: %+v", c)
	}
	// the sidebar count leaves the OLT out
	if n := e.svc.UnregisteredOnline(nil); n != 3 {
		t.Fatalf("online unregistered %d, want 3 (ONU, unknown x2)", n)
	}
	if c := e.svc.ClassifyMAC(e.site, macOLT); c.Class != ClassOLT {
		t.Fatalf("ClassifyMAC %+v", c)
	}
}

func TestCreateCustomerOnABridgeOnlyDeviceNeedsConfirmation(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.dynLease(macHuawei, "10.5.50.11", "HG8010H", "bound")
	e.reconcile(t)
	ctx := context.Background()
	in := AddCustomerInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "C1", MAC: macThinkPad}
	_, _, err := e.svc.CreateCustomer(ctx, in)
	if !errors.Is(err, ErrConfirmNoAddress) {
		t.Fatalf("want the confirmation error, got %v", err)
	}
	e.svc.st.View(func(d *store.Data) {
		if _, ok := d.AccountByMAC(macThinkPad); ok {
			t.Fatal("an account was made without the confirmation")
		}
	})
	in.ConfirmNoAddress = true
	if _, _, err := e.svc.CreateCustomer(ctx, in); err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	// a device that asked for an address needs no confirmation
	if _, _, err := e.svc.CreateCustomer(ctx, AddCustomerInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "C2", MAC: macHuawei}); err != nil {
		t.Fatalf("lease device: %v", err)
	}
}

func TestEquipmentTreeShape(t *testing.T) {
	e := newEnv(t)
	aptA := e.apt(t, "MG One", e.site)
	aptB := e.apt(t, "Block B", e.site)
	o := e.olt(t, OLTInput{Name: "OLT1", Model: "V1600GS-R", MAC: macOLT, RouterPort: "ether3", PONPorts: 4, PONApartments: map[int][]string{1: {aptA.ID, aptB.ID}}})
	spare := e.olt(t, OLTInput{Name: "Spare", MAC: "48:21:B8:10:D7:90", SiteID: "-", Status: store.EqInStock})
	_ = spare
	onu1, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "Z1", MAC: macONU, SetPlacement: true, OLTID: o.ID, PONPort: 1})
	e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "Z2", MAC: macB, SetPlacement: true, OLTID: o.ID})
	e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "Z3", MAC: macC})
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: aptA.ID, SiteID: e.site, Unit: "A1", Tenant: "Jane", EquipmentID: onu1.ID}); err != nil {
		t.Fatal(err)
	}
	tr := e.svc.Tree(nil)
	if len(tr.Routers) != 1 || len(tr.Routers[0].Apartments) != 3 || len(tr.Routers[0].OLTs) != 1 {
		t.Fatalf("routers %+v", tr.Routers)
	}
	to := tr.Routers[0].OLTs[0]
	if to.ONUCount != 2 || len(to.PONs) < 4 {
		t.Fatalf("olt %+v", to)
	}
	var p1, p0 *TreePON
	for i := range to.PONs {
		switch to.PONs[i].Port {
		case 1:
			p1 = &to.PONs[i]
		case 0:
			p0 = &to.PONs[i]
		}
	}
	if p1 == nil || len(p1.Apartments) != 2 || len(p1.ONUs) != 1 || p1.ONUs[0].House != "A1" || p1.ONUs[0].Customer != "Jane" || p1.PONServesText() == "" {
		t.Fatalf("pon1 %+v", p1)
	}
	if p0 == nil || len(p0.ONUs) != 1 {
		t.Fatalf("ONUs with no PON port: %+v", p0)
	}
	if len(tr.UnplacedOLTs) != 1 || len(tr.UnplacedONUs) != 1 || tr.UnplacedONUs[0].Equip.Serial != "Z3" {
		t.Fatalf("unplaced: %d olts %d onus", len(tr.UnplacedOLTs), len(tr.UnplacedONUs))
	}
	if sb := e.svc.ApartmentServedBy(aptA.ID); len(sb) != 1 || sb[0].Port != 1 || len(sb[0].ONUs) != 1 {
		t.Fatalf("served by %+v", sb)
	}
	if sb := e.svc.ApartmentServedBy(aptB.ID); len(sb) != 1 || len(sb[0].ONUs) != 0 {
		t.Fatalf("served by B %+v", sb)
	}
}

// Review fixes: deleting an apartment drops it from PON maps (store.Validate stays clean), an ONU under a retired
// OLT shows as unplaced, an OLT with ONUs keeps its kind, a retired OLT is brought back only with a free MAC.
func TestOLTDanglingRefs(t *testing.T) {
	e := newEnv(t)
	aptA := e.apt(t, "MG One", e.site)
	aptB := e.apt(t, "MG Two", e.site)
	o := e.olt(t, OLTInput{Name: "Block A OLT", MAC: macOLT, PONPorts: 4, PONApartments: map[int][]string{1: {aptA.ID, aptB.ID}, 2: {aptB.ID}}})
	if err := e.svc.DeleteApartment(aptB.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.st.View(func(d *store.Data) {
		g := d.Equipment[o.ID]
		if len(g.PONApartments) != 1 || len(g.PONApartments["1"]) != 1 || g.PONApartments["1"][0] != aptA.ID {
			t.Fatalf("PON map after delete: %v", g.PONApartments)
		}
		if p := store.Validate(d); len(p) > 0 {
			t.Fatalf("validate: %v", p)
		}
	})

	onu, err := e.svc.SaveEquipment(EquipmentInput{Kind: store.KindONT, Serial: "VSOL0001", MAC: macONU, SetPlacement: true, OLTID: o.ID, PONPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: o.ID, Kind: store.KindSwitch, MAC: macOLT, SiteID: e.site}); err == nil {
		t.Fatal("an OLT with ONUs changed kind")
	}
	if err := e.svc.st.Update(func(d *store.Data) error {
		g := d.Equipment[o.ID]
		g.Status, g.SiteID = store.EqRetired, ""
		d.Equipment[o.ID] = g
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range e.svc.Tree(nil).UnplacedONUs {
		found = found || n.Equip.ID == onu.ID
	}
	if !found {
		t.Fatal("an ONU under a retired OLT is missing from the tree")
	}

	// another item takes the retired OLT's MAC; bringing the OLT back must not duplicate it
	if _, err := e.svc.SaveEquipment(EquipmentInput{ID: onu.ID, Kind: store.KindONT, Serial: "VSOL0001", MAC: "68:22:9F:00:00:02"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: store.KindSwitch, Serial: "SW1", MAC: macOLT}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveOLT(OLTInput{ID: o.ID, Name: "Block A OLT", MAC: macOLT, Status: store.EqInStock}); !errors.Is(err, ErrDuplicateEquipmentMAC) {
		t.Fatalf("un-retire with a taken MAC: %v", err)
	}
}

func TestCleanVendorDropsFormatChars(t *testing.T) {
	if got := cleanVendor([]byte("Hua‮wei​ Tech")); got != "Huawei Tech" {
		t.Fatalf("%q", got)
	}
}
