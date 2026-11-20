package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func (e *env) apt(t *testing.T, name string, sites ...string) store.Apartment {
	t.Helper()
	a, err := e.svc.CreateApartment(ApartmentInput{Name: name}, sites)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) unitAt(t *testing.T, apt store.Apartment, site, unit, mac string) store.Account {
	t.Helper()
	a, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: site, Unit: unit, MAC: mac})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApartmentContactsAreValidatedAndNormalised(t *testing.T) {
	e := newEnv(t)
	a, err := e.svc.CreateApartment(ApartmentInput{Name: "  MG   One Central ", Address: "Kilimani",
		Caretaker: ContactInput{Name: "Joe", Phone: "0722 000 000", Phone2: "+254733000111", Email: "joe@example.com", Notes: "night shift"},
		Landlord:  ContactInput{Name: "Mama Wanjiru", Phone: "254711222333"}}, []string{e.site})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "MG One Central" || a.Caretaker.Phone != "+254722000000" || a.Caretaker.Phone2 != "+254733000111" || a.Landlord.Phone != "+254711222333" {
		t.Fatalf("%+v", a)
	}
	if got := WhatsAppLink(a.Caretaker.Phone); got != "https://wa.me/254722000000" {
		t.Fatalf("whatsapp link %q", got)
	}
	for name, in := range map[string]ApartmentInput{
		"name":         {Name: " "},
		"bad phone":    {Name: "X1", Caretaker: ContactInput{Name: "J", Phone: "12345"}},
		"bad phone2":   {Name: "X2", Landlord: ContactInput{Name: "J", Phone2: "abc"}},
		"bad email":    {Name: "X3", Caretaker: ContactInput{Name: "J", Email: "nope"}},
		"nameless":     {Name: "X4", Caretaker: ContactInput{Phone: "0722000000"}},
		"duplicate ci": {Name: "mg one CENTRAL"},
	} {
		if _, err := e.svc.CreateApartment(in, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Both sections are optional.
	if _, err := e.svc.CreateApartment(ApartmentInput{Name: "Plain"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UpdateApartment(a.ID, ApartmentInput{Name: "MG One", Caretaker: ContactInput{Name: "Joe"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.ApartmentByID(a.ID)
	if got.Name != "MG One" || !got.Landlord.Empty() || got.Caretaker.Name != "Joe" || len(got.Routers) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestUnitIsUniquePerApartmentAmongOpenAccounts(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	one := e.apt(t, "Tower One", e.site)
	two := e.apt(t, "Tower Two", b)
	a := e.unitAt(t, one, "", "B4", mac1)
	var taken *UnitTakenError
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: one.ID, Unit: " b4 ", MAC: macA}); !errors.As(err, &taken) || !errors.Is(err, store.ErrDuplicateUnit) {
		t.Fatalf("same unit, other case: %v", err)
	}
	// Another apartment may use the same label, even on the same router.
	e.unitAt(t, two, "", "B4", macA)
	// A closed account frees its unit.
	if err := e.svc.DeleteAccount(context.Background(), a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	e.unitAt(t, one, "", "B4", macB2)
	// Editing to a taken unit is refused.
	n := e.unitAt(t, one, "", "B5", macC)
	if err := e.svc.UpdateAccount(n.ID, "b4", "", "", ""); !errors.Is(err, store.ErrDuplicateUnit) {
		t.Fatalf("%v", err)
	}
	// The store itself refuses it too (the rule does not live in the service only).
	err := e.st.Update(func(d *store.Data) error {
		x := d.Accounts[n.ID]
		x.Unit = "B4"
		d.Accounts[n.ID] = x
		return nil
	})
	if !errors.Is(err, store.ErrDuplicateUnit) {
		t.Fatalf("store invariant: %v", err)
	}
}

func TestRouterOfAnAccountMustServeItsApartment(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	c, _ := e.addSite(t, "Block C")
	big := e.apt(t, "Big Tower", e.site, b)
	// Two routers: the router must be named, and must be one of the apartment's.
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: big.ID, Unit: "1", MAC: mac1}); !errors.Is(err, ErrChooseRouter) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: big.ID, SiteID: c, Unit: "1", MAC: mac1}); !errors.Is(err, ErrRouterNotLinked) {
		t.Fatalf("%v", err)
	}
	if a := e.unitAt(t, big, b, "1", mac1); a.SiteID != b || a.ApartmentID != big.ID {
		t.Fatalf("%+v", a)
	}
	// One router: picked automatically.
	solo := e.apt(t, "Solo", c)
	if a := e.unitAt(t, solo, "", "1", macA); a.SiteID != c {
		t.Fatalf("auto router: %+v", a)
	}
	// No router at all.
	none := e.apt(t, "Empty")
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: none.ID, Unit: "1", MAC: macB2}); !errors.Is(err, ErrApartmentNoRoute) {
		t.Fatalf("%v", err)
	}
	// A router serves several apartments.
	if err := e.svc.LinkRouter(solo.ID, b); err != nil {
		t.Fatal(err)
	}
	if ids := e.svc.ApartmentsOfRouter(b); len(ids) != 3 { // its own, Big Tower, Solo
		t.Fatalf("%+v", ids)
	}
}

func TestUnlinkRefusedWhileRouterServesOpenAccounts(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	big := e.apt(t, "Big Tower", e.site, b)
	a := e.unitAt(t, big, b, "7", mac1)
	var blocked *RouterBlockedError
	if err := e.svc.UnlinkRouter(big.ID, b); !errors.As(err, &blocked) || len(blocked.Units) != 1 || blocked.Units[0] != "7" {
		t.Fatalf("%v", err)
	}
	if err := e.svc.SetApartmentRouters(big.ID, []string{e.site}); !errors.As(err, &blocked) {
		t.Fatalf("set routers: %v", err)
	}
	// The other router has no accounts: it can go.
	if err := e.svc.UnlinkRouter(big.ID, e.site); err != nil {
		t.Fatal(err)
	}
	// Move the account to a router that still serves, then unlink.
	if err := e.svc.LinkRouter(big.ID, e.site); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MoveToApartment(context.Background(), MoveInput{AccountID: a.ID, SiteID: e.site}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnlinkRouter(big.ID, b); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteApartment(big.ID); err == nil {
		t.Fatal("an apartment with accounts cannot be deleted")
	}
}

// The rule: nothing is written for an account whose router does not serve its apartment.
func TestNothingIsWrittenWhenTheRouterDoesNotServeTheApartment(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	ok := e.flat(t, "Flat 2", macB2)
	// Break the link behind the service's back (a legacy or imported row).
	err := e.st.Update(func(d *store.Data) error {
		delete(d.ApartmentSites, store.ApartmentSiteKey(store.MigratedApartmentID(e.site), e.site))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.payAt(t, a, "1 Day") // a payment runs the router tasks
	e.payAt(t, ok, "1 Day")
	e.rt.ResetWrites()
	run, err := e.svc.ReconcileSite(context.Background(), e.site)
	if err != nil {
		t.Fatal(err)
	}
	if e.rt.Writes() != 0 || len(e.rt.Table("/ip/dhcp-server/lease")) != 0 || len(e.rt.Table("/queue/simple")) != 0 {
		t.Fatalf("something was written: %d leases %v", e.rt.Writes(), e.rt.Table("/ip/dhcp-server/lease"))
	}
	if len(run.Conflicts) != 2 || !strings.Contains(run.Conflicts[0], "does not serve") {
		t.Fatalf("the problem must be in words: %v", run.Conflicts)
	}
	if why := e.svc.AccountProblem(a.ID); !strings.Contains(why, "Block A does not serve Block A") {
		t.Fatalf("%q", why)
	}
	if st := e.svc.AccountRouterState(a.ID); st.InSync || !strings.Contains(st.LastError, "does not serve") {
		t.Fatalf("%+v", st)
	}
	// Linking again provisions it.
	if err := e.svc.LinkRouter(store.MigratedApartmentID(e.site), e.site); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if len(e.rt.Table("/ip/dhcp-server/lease")) != 2 {
		t.Fatalf("%v", e.rt.Table("/ip/dhcp-server/lease"))
	}
	if why := e.svc.AccountProblem(a.ID); why != "" {
		t.Fatalf("%q", why)
	}
}

func TestAccountWithoutApartmentIsNotProvisioned(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	_ = e.st.Update(func(d *store.Data) error {
		x := d.Accounts[a.ID]
		x.ApartmentID = ""
		d.Accounts[a.ID] = x
		return nil
	})
	e.rt.ResetWrites()
	run, _ := e.svc.ReconcileSite(context.Background(), e.site)
	if e.rt.Writes() != 0 || len(run.Conflicts) != 1 || !strings.Contains(run.Conflicts[0], "no apartment") {
		t.Fatalf("%d %v", e.rt.Writes(), run.Conflicts)
	}
}

// otherBuilding sets up customer F1 in "Block A" (router A) and a second building "Block B" (router B) that
// sees F1's ONT.
func otherBuilding(t *testing.T) (e *env, f1 store.Account, b string, rtB *routeros.Mock) {
	e = newEnv(t)
	f1 = e.flat(t, "Flat 1", macA)
	e.payAt(t, f1, "1 Day")
	e.reconcile(t)
	b, rtB = e.addSite(t, "Block B")
	rtB.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "address": "10.5.50.20", "active-address": "10.5.50.20", "host-name": "ont",
		"status": "bound", "dynamic": "true", "server": DHCPServerName})
	return
}

func TestMACOfACustomerIsNeverProvisionedInAnotherBuilding(t *testing.T) {
	e, f1, b, rtB := otherBuilding(t)
	// Block B also has a customer of its own so the pass has work to do.
	own := e.flatAt(t, b, "Flat 9", macC)
	e.payAt(t, own, "1 Day")
	rtB.ResetWrites()
	if _, err := e.svc.ReconcileSite(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for _, r := range rtB.Table("/ip/dhcp-server/lease") {
		if strings.EqualFold(r["mac-address"], macA) && strings.HasPrefix(r["comment"], AcctPrefix) {
			t.Fatalf("F1's MAC was provisioned on the other building's router: %v", r)
		}
	}
	for _, r := range rtB.Table("/queue/simple") {
		if strings.Contains(r["comment"], f1.Code) {
			t.Fatalf("queue for F1 on the other building's router: %v", r)
		}
	}
	// It stays unregistered there, and the list says whose it is.
	var got *UnregDevice
	for _, u := range e.svc.Unregistered(b, false) {
		if u.MAC == macA {
			u := u
			got = &u
		}
	}
	if got == nil || got.BelongsTo == nil {
		t.Fatalf("the device must be listed with its owner: %+v", got)
	}
	bt := got.BelongsTo
	if bt.AccountID != f1.ID || bt.Apartment != "Block A" || bt.Unit != "Flat 1" || bt.SameApartment || len(bt.ApartmentChoices) != 1 || got.Suggested != nil {
		t.Fatalf("%+v", bt)
	}
	// And on its own router it is not "unregistered" at all.
	for _, u := range e.svc.Unregistered(e.site, false) {
		if u.MAC == macA {
			t.Fatalf("the account's own MAC listed on its own router: %+v", u)
		}
	}
	// The owner's router still has its lease.
	if len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)) != 1 {
		t.Fatal("owner lost its lease")
	}
}

func TestAttachAndCreateRefuseAMACOfAnotherBuilding(t *testing.T) {
	e, f1, b, _ := otherBuilding(t)
	nine := e.flatAt(t, b, "Flat 9", macC)
	ctx := context.Background()
	_, err := e.svc.AttachDevice(ctx, b, macA, nine.ID)
	var dup *DuplicateMACError
	if !errors.As(err, &dup) || !errors.Is(err, ErrMACOtherBuilding) || !errors.Is(err, ErrDuplicateMAC) || dup.AccountID != f1.ID || dup.ApartmentName != "Block A" {
		t.Fatalf("attach: %v", err)
	}
	if got := e.get(nine); got.MAC != macC {
		t.Fatal("MAC changed despite the refusal")
	}
	_, err = e.svc.CreateFromDevice(ctx, DeviceAccountInput{SiteID: b, MAC: macA, Unit: "Flat 10"})
	if !errors.Is(err, ErrMACOtherBuilding) {
		t.Fatalf("create: %v", err)
	}
	// In the same apartment it is the ordinary duplicate.
	_, err = e.svc.CreateAccount(AccountInput{SiteID: e.site, Unit: "Flat 5", MAC: macA})
	if !errors.Is(err, ErrDuplicateMAC) || errors.Is(err, ErrMACOtherBuilding) {
		t.Fatalf("same building: %v", err)
	}
}

func TestMoveCustomerHereReprovisionsAndRecordsHistory(t *testing.T) {
	e, f1, b, rtB := otherBuilding(t)
	ctx := context.Background()
	if len(e.rt.Table("/ip/dhcp-server/lease")) != 1 {
		t.Fatal("setup: the owner router has its lease")
	}
	moved, err := e.svc.MoveCustomerHere(ctx, b, macA, "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.SiteID != b || moved.ApartmentID != store.MigratedApartmentID(b) || moved.Unit != "Flat 1" || moved.Code != f1.Code {
		t.Fatalf("%+v", moved)
	}
	// Written on the new router, removed from the old one, by the reconciler.
	if _, err := e.svc.ReconcileSite(ctx, e.site); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReconcileSite(ctx, b); err != nil {
		t.Fatal(err)
	}
	if n := len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)); n != 0 {
		t.Fatalf("old router still has %d lease rows", n)
	}
	if len(e.rt.Table("/queue/simple")) != 0 {
		t.Fatalf("old router still has queues: %v", e.rt.Table("/queue/simple"))
	}
	var static []routeros.Row
	for _, r := range rtB.Table("/ip/dhcp-server/lease") {
		if strings.HasPrefix(r["comment"], AcctPrefix+f1.Code) {
			static = append(static, r)
		}
	}
	if len(static) != 1 || len(rowsBy(rtB.Table("/queue/simple"), "name", "ftn-"+f1.Code)) != 1 {
		t.Fatalf("new router: %v %v", static, rtB.Table("/queue/simple"))
	}
	// The device is no longer a "belongs to" row.
	for _, u := range e.svc.Unregistered(b, false) {
		if u.MAC == macA {
			t.Fatalf("still listed: %+v", u)
		}
	}
	// History.
	evs, _ := e.st.Events(ctx, store.EventQuery{AccountID: f1.ID})
	found := false
	for _, ev := range evs {
		if strings.Contains(ev.Msg, "moved from Block A unit Flat 1 on Block A to Block B unit Flat 1 on Block B") {
			found = true
		}
	}
	if !found {
		t.Fatalf("history: %+v", evs)
	}
	// Nothing to do the second time.
	if _, err := e.svc.MoveCustomerHere(ctx, b, macA, ""); !errors.Is(err, store.ErrNoChange) {
		t.Fatalf("%v", err)
	}
}

func TestMoveRefusesATakenUnitAndNeedsARouter(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	one := e.apt(t, "Tower One", e.site)
	two := e.apt(t, "Tower Two", b, e.site)
	a := e.unitAt(t, one, "", "A1", mac1)
	e.unitAt(t, two, e.site, "A1", macA)
	ctx := context.Background()
	var taken *UnitTakenError
	if _, err := e.svc.MoveToApartment(ctx, MoveInput{AccountID: a.ID, ApartmentID: two.ID, SiteID: e.site}); !errors.As(err, &taken) {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.MoveToApartment(ctx, MoveInput{AccountID: a.ID, ApartmentID: two.ID}); !errors.Is(err, ErrChooseRouter) {
		t.Fatalf("%v", err)
	}
	got, err := e.svc.MoveToApartment(ctx, MoveInput{AccountID: a.ID, ApartmentID: two.ID, SiteID: b, Unit: "A2"})
	if err != nil || got.Unit != "A2" || got.SiteID != b || got.ApartmentID != two.ID || got.Flat != "A2" {
		t.Fatalf("%+v %v", got, err)
	}
}

// A blocked account does not keep the router state it had: its lease, queue and ARP row go,
// and it stays flagged with the words for why.
func TestBlockedAccountLosesItsRouterState(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	e.reconcile(t)
	if len(e.rt.Table("/ip/dhcp-server/lease")) != 1 || len(e.rt.Table("/queue/simple")) != 1 {
		t.Fatalf("setup: lease %v queue %v", e.rt.Table("/ip/dhcp-server/lease"), e.rt.Table("/queue/simple"))
	}
	arpBefore := len(e.rt.Table("/ip/arp"))
	err := e.st.Update(func(d *store.Data) error {
		delete(d.ApartmentSites, store.ApartmentSiteKey(store.MigratedApartmentID(e.site), e.site))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.svc.ReconcileSite(context.Background(), e.site)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.rt.Table("/ip/dhcp-server/lease")); n != 0 {
		t.Errorf("lease kept: %v", e.rt.Table("/ip/dhcp-server/lease"))
	}
	if n := len(e.rt.Table("/queue/simple")); n != 0 {
		t.Errorf("queue kept: %v", e.rt.Table("/queue/simple"))
	}
	for _, r := range e.rt.Table("/ip/arp") {
		if strings.HasPrefix(r["comment"], AcctPrefix+a.Code) {
			t.Errorf("ARP row kept (%d rows before): %v", arpBefore, r)
		}
	}
	if len(run.Conflicts) != 1 || !strings.Contains(run.Conflicts[0], "does not serve") {
		t.Fatalf("still flagged in words: %v", run.Conflicts)
	}
	if why := e.svc.AccountProblem(a.ID); why == "" {
		t.Fatal("no longer flagged")
	}
	// A second pass writes nothing more, and linking again brings the rows back.
	e.rt.ResetWrites()
	if _, err := e.svc.ReconcileSite(context.Background(), e.site); err != nil || e.rt.Writes() != 0 {
		t.Fatalf("second pass: %v, %d writes", err, e.rt.Writes())
	}
	if err := e.svc.LinkRouter(store.MigratedApartmentID(e.site), e.site); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if len(e.rt.Table("/ip/dhcp-server/lease")) != 1 || len(e.rt.Table("/queue/simple")) != 1 {
		t.Fatalf("not written again: %v %v", e.rt.Table("/ip/dhcp-server/lease"), e.rt.Table("/queue/simple"))
	}
}

// The move recheck: the device the operator looked at must still be the account's.
func TestMoveRefusedWhenTheDeviceChangedMeanwhile(t *testing.T) {
	e, f1, b, _ := otherBuilding(t)
	_, err := e.svc.MoveToApartment(context.Background(), MoveInput{AccountID: f1.ID, ApartmentID: store.MigratedApartmentID(b), SiteID: b, ExpectMAC: "AA:BB:CC:00:00:99"})
	if !errors.Is(err, ErrDeviceChanged) {
		t.Fatalf("a changed MAC must refuse the move: %v", err)
	}
	if cur, _ := e.svc.GetAccount(f1.ID); cur.SiteID != e.site {
		t.Fatalf("the account moved anyway: %+v", cur)
	}
	if _, err := e.svc.MoveToApartment(context.Background(), MoveInput{AccountID: f1.ID, ApartmentID: store.MigratedApartmentID(b), SiteID: b, ExpectMAC: strings.ToLower(macA)}); err != nil {
		t.Fatalf("the same MAC (any case) moves: %v", err)
	}
}
