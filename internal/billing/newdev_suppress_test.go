package billing

import (
	"context"
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

// A closed/deleted account must NOT keep an ONU suppressed. Once the house is deleted the account leaves d.Accounts
// (it moves to ClosedAccounts, and its AccountID is no longer a live customer), so the server's "account exists"
// guard must return false and the device comes back onto the New devices list — never hidden as if still a customer.
// This covers the existence check against d.Accounts (not merely "AccountID is non-empty"): the ACS ONU record still
// carries the old AccountID after the delete, yet must no longer suppress. It is valid on Postgres too: the closed
// account row stays in the accounts table, so the ONU's account_id foreign key is still satisfied.
func TestNewDevicesClosedAccountDoesNotSuppress(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	o := e.olt(t, OLTInput{Name: "Block A OLT", Model: "V1600GS-R", MAC: macOLT, RouterPort: "ether3", PONPorts: 4, PONApartments: map[int][]string{1: {apt.ID}}})
	onu, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "V26051300697", GPONSerial: "VSOL00127E0F", MAC: macONUWAN, Model: "V2804N-Z", SetPlacement: true, OLTID: o.ID, PONPort: 1})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "H707", MAC: macHuawei})
	if err != nil {
		t.Fatal(err)
	}
	linkONT(t, e, onu.ID, acct.ID)
	// An ACS ONU record of the same house points at the account. Its management MAC is deliberately NOT near any
	// register ONT's WAN MAC, so after the delete only the "account still exists" guard can keep it suppressed —
	// the test fails if that guard checks "AccountID non-empty" instead of "account is in d.Accounts".
	const acsMgmt = "00:1A:2B:3C:4D:5E"
	if err := e.svc.st.Update(func(d *store.Data) error {
		d.ONUs["o1"] = store.ONU{ID: "o1", Serial: "ZZZ", SiteID: e.site, AccountID: acct.ID, Sec: store.ONUSec{MgmtMAC: acsMgmt}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.bridgeHost(macONUWAN, "ether5", "false")
	e.mgmtLease(acsMgmt, "10.6.1.250")
	e.reconcile(t)
	// While the account is open, both MACs are suppressed.
	got := unregByMAC(e)
	for _, m := range []string{macONUWAN, acsMgmt} {
		if _, ok := got[m]; ok {
			t.Fatalf("%s of a linked ONU should be suppressed while the account is open", m)
		}
	}
	// Delete the house. The account leaves d.Accounts; the ACS record keeps its (now closed) AccountID.
	if err := e.svc.DeleteAccount(context.Background(), acct.ID, acct.Code); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	got = unregByMAC(e)
	for _, m := range []string{macONUWAN, acsMgmt} {
		if _, ok := got[m]; !ok {
			t.Fatalf("%s stayed suppressed after the account was closed — a would-be customer hidden by a stale AccountID", m)
		}
	}
}

// An ambiguous management MAC — two register ONTs, both linked, both within 8 of it — must NOT resolve, so it is
// NOT suppressed. A loose near-match that picked either one would hide the management address of a third ONU.
func TestNewDevicesAmbiguousNearMatchDoesNotSuppress(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt1 := e.apt(t, "MG One", e.site)
	apt2 := e.apt(t, "MG Two", e.site)
	// Two in-stock ONTs whose WAN MACs are both within 8 of macONUMgmt (…7E:11): …7E:10 and …7E:18.
	ont1, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "S1", MAC: "68:22:9F:12:7E:10", Model: "V2804N-Z"})
	if err != nil {
		t.Fatal(err)
	}
	ont2, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "S2", MAC: "68:22:9F:12:7E:18", Model: "V2804N-Z"})
	if err != nil {
		t.Fatal(err)
	}
	a1, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt1.ID, SiteID: e.site, Unit: "H1", MAC: macHuawei})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt2.ID, SiteID: e.site, Unit: "H2", MAC: "00:E0:FC:12:34:57"})
	if err != nil {
		t.Fatal(err)
	}
	linkONT(t, e, ont1.ID, a1.ID)
	linkONT(t, e, ont2.ID, a2.ID)
	e.mgmtLease(macONUMgmt, "10.6.1.250")
	e.reconcile(t)
	if _, ok := unregByMAC(e)[macONUMgmt]; !ok {
		t.Fatal("an ambiguous management MAC (two linked near-matches) was suppressed — the match was not unique")
	}
}
