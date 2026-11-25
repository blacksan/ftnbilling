package billing

import (
	"testing"

	"ispbill/internal/store"
)

// linkONT puts the register's ONT in a house the way an installed ONU is: Equipment.AccountID of an open account.
func linkONT(t *testing.T, e *env, eqID, accountID string) {
	t.Helper()
	if err := e.svc.st.Update(func(d *store.Data) error {
		q := d.Equipment[eqID]
		q.AccountID, q.Status = accountID, store.EqInstalled
		d.Equipment[eqID] = q
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNewDevicesSuppressWhatTheServerAlreadyKnows(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	o := e.olt(t, OLTInput{Name: "Block A OLT", Model: "V1600GS-R", MAC: macOLT, RouterPort: "ether3", PONPorts: 4, PONApartments: map[int][]string{1: {apt.ID}}})
	onu, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "V26051300697", GPONSerial: "VSOL00127E0F", MAC: macONUWAN, Model: "V2804N-Z", SetPlacement: true, OLTID: o.ID, PONPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.bridgeHost(macOLT, "ether3", "false")
	e.mgmtLease(macONUMgmt, "10.6.1.250")
	e.bridgeHost(macONUWAN, "ether5", "false")
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.reconcile(t)

	// Not linked yet: the ONU, its management address and the unknown device are all still listed. The OLT is not.
	got := unregByMAC(e)
	if _, ok := got[macOLT]; ok {
		t.Fatal("a known OLT is listed as a new device")
	}
	for _, m := range []string{macONUWAN, macONUMgmt, macThinkPad} {
		if _, ok := got[m]; !ok {
			t.Fatalf("%s vanished although nothing positive matches it", m)
		}
	}

	// Link the ONU to a house: its WAN and management MACs go; the unknown device stays.
	acct, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "H707", MAC: macHuawei})
	if err != nil {
		t.Fatal(err)
	}
	linkONT(t, e, onu.ID, acct.ID)
	got = unregByMAC(e)
	for _, m := range []string{macOLT, macONUWAN, macONUMgmt} {
		if _, ok := got[m]; ok {
			t.Fatalf("%s of a linked ONU / known OLT is still listed", m)
		}
	}
	if _, ok := got[macThinkPad]; !ok {
		t.Fatal("a truly unknown MAC was suppressed")
	}
}

func TestNewDevicesSuppressACSKnownMACsOfALinkedONU(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	acct, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "H707", MAC: macHuawei})
	if err != nil {
		t.Fatal(err)
	}
	e.mgmtLease(macONUMgmt, "10.6.1.250")
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.reconcile(t)
	// an ACS record not linked to a house: the mgmt MAC stays listed
	e.svc.st.Update(func(d *store.Data) error {
		d.ONUs["o1"] = store.ONU{ID: "o1", Serial: "ZZZ", SiteID: e.site, Sec: store.ONUSec{MgmtMAC: macONUMgmt}}
		return nil
	})
	if _, ok := unregByMAC(e)[macONUMgmt]; !ok {
		t.Fatal("the management MAC of an unlinked ONU must stay listed")
	}
	e.svc.st.Update(func(d *store.Data) error {
		o := d.ONUs["o1"]
		o.AccountID = acct.ID
		d.ONUs["o1"] = o
		return nil
	})
	got := unregByMAC(e)
	if _, ok := got[macONUMgmt]; ok {
		t.Fatal("the management MAC of a linked ONU is still listed")
	}
	if _, ok := got[macThinkPad]; !ok {
		t.Fatal("unknown MAC suppressed")
	}
}
