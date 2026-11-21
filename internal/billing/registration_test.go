package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func (e *env) aptID() string { return store.MigratedApartmentID(e.site) }

func (e *env) regIn(name, phone, unit string) RegistrationInput {
	return RegistrationInput{SiteID: e.site, ApartmentID: e.aptID(), Unit: unit, Name: name, Phone: phone, Consent: true}
}

func wantRegErr(t *testing.T, err error, contains string) {
	t.Helper()
	var re *RegError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, contains) {
		t.Fatalf("want a refusal containing %q, got %v", contains, err)
	}
}

func TestSubmitRegistrationValidates(t *testing.T) {
	e := newEnv(t)
	e.flat(t, "B2", "AA:BB:CC:00:00:02")
	in := e.regIn("Wanjiru Kamau", "0712 345 678", "B4")
	bad := []struct {
		name string
		mod  func(*RegistrationInput)
		want string
	}{
		{"no consent", func(i *RegistrationInput) { i.Consent = false }, "privacy notice"},
		{"short name", func(i *RegistrationInput) { i.Name = "W" }, "full name"},
		{"long name", func(i *RegistrationInput) { i.Name = strings.Repeat("a", 81) }, "full name"},
		{"control char", func(i *RegistrationInput) { i.Name = "Wan\x00jiru" }, "plain text"},
		{"bad phone", func(i *RegistrationInput) { i.Phone = "12345" }, "Kenyan phone"},
		{"no unit", func(i *RegistrationInput) { i.Unit = "  " }, "house number"},
		{"long unit", func(i *RegistrationInput) { i.Unit = strings.Repeat("9", 41) }, "house number"},
		{"bad email", func(i *RegistrationInput) { i.Email = "not an email" }, "email"},
		{"named email", func(i *RegistrationInput) { i.Email = "Joe <joe@example.com>" }, "email"},
		{"no apartment", func(i *RegistrationInput) { i.ApartmentID = "" }, "apartment"},
		{"apartment of nobody", func(i *RegistrationInput) { i.ApartmentID = "zzz" }, "apartment"},
		{"unit taken", func(i *RegistrationInput) { i.Unit = "b2" }, "already has an account"},
		{"unknown router", func(i *RegistrationInput) { i.SiteID = "nope" }, "which building"},
	}
	for _, c := range bad {
		x := in
		c.mod(&x)
		_, err := e.svc.SubmitRegistration(x)
		wantRegErr(t, err, c.want)
	}
	if n := e.svc.PendingRegistrationCount(nil); n != 0 {
		t.Fatalf("a refused form was kept: %d", n)
	}
	// every Kenyan format is accepted and normalised
	for i, ph := range []string{"0712345678", "0112345678", "+254712345678", "254 712 345 678", "712345678"} {
		x := e.regIn("Person "+string(rune('A'+i)), ph, "U"+string(rune('1'+i)))
		x.Email = "p@example.com"
		x.Phone = ph
		if i > 0 { // the same number again is the "already registered" refusal, so use another number each time
			x.Phone = []string{"", "0112345678", "+254722000111", "254 733 000 111", "744000111"}[i]
		}
		r, err := e.svc.SubmitRegistration(x)
		if err != nil {
			t.Fatalf("%q: %v", ph, err)
		}
		if !strings.HasPrefix(r.Phone, "254") || len(r.Phone) != 12 {
			t.Fatalf("%q normalised to %q", ph, r.Phone)
		}
	}
	// the same number, or the same house, while one is waiting
	_, err := e.svc.SubmitRegistration(e.regIn("Other Name", "0712345678", "Z9"))
	wantRegErr(t, err, "already have your registration")
	_, err = e.svc.SubmitRegistration(e.regIn("Other Name", "0755111222", "u1"))
	wantRegErr(t, err, "already have your registration")
}

func TestSubmitRegistrationCapsPendingPerRouter(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MinPendingPerRouter; i++ {
		ph := "07" + strings.Repeat("1", 2) + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "0000"
		if _, err := e.svc.SubmitRegistration(e.regIn("Person X", ph, "H"+ph)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	_, err := e.svc.SubmitRegistration(e.regIn("One More", "0799888777", "LAST"))
	wantRegErr(t, err, "Too many registrations")
}

func TestRegistrationPreselectsOnlyOneDeviceFirstSeenNearTheRequest(t *testing.T) {
	e := newEnv(t)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	r, err := e.svc.SubmitRegistration(e.regIn("Guess Me", "0712345678", "G1"))
	if err != nil || r.MAC != "" || r.GuessMAC != "" {
		t.Fatalf("nothing is guessed at submission: %+v %v", r, err)
	}
	if v := e.svc.PendingRegistrations(nil); len(v) != 1 || v[0].GuessMAC != macHuawei || !v[0].Candidates[0].Likely {
		t.Fatalf("one device first seen at the request: %+v", v)
	}
	// a second device first seen a minute later: two within the window, nothing is preselected
	e.now = e.now.Add(time.Minute)
	e.dynLease(macNew, "10.5.50.12", "phone", "bound")
	e.reconcile(t)
	if v := e.svc.PendingRegistrations(nil); v[0].GuessMAC != "" || len(v[0].Candidates) != 2 {
		t.Fatalf("two devices near the request must give no preselection: %+v", v[0])
	}
}

func TestRegistrationCandidatesAreSortedByFirstSeenAndMatchTheDeviceName(t *testing.T) {
	reg := store.Registration{CreatedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC), DeviceName: "Wanjiru's iPhone"}
	at := func(m int) time.Time { return reg.CreatedAt.Add(time.Duration(m) * time.Minute) }
	dev := func(mac, host string, first time.Time) UnregDevice {
		return UnregDevice{Device: store.Device{MAC: mac, HostName: host, FirstSeen: first, LastSeen: first}}
	}
	devs := []UnregDevice{dev("M1", "android-1", at(-300)), dev("M2", "Wanjirus-iPhone", at(40)), dev("M3", "laptop", at(-3)), dev("M4", "", at(25))}
	got, pre := registrationCandidates(devs, reg)
	if got[0].MAC != "M3" || got[1].MAC != "M4" || got[2].MAC != "M2" || got[3].MAC != "M1" {
		t.Fatalf("order: %v %v %v %v", got[0].MAC, got[1].MAC, got[2].MAC, got[3].MAC)
	}
	if pre != "M3" || !got[0].Likely {
		t.Fatalf("one device within 10 minutes should be preselected: %q", pre)
	}
	for _, c := range got {
		if c.NameMatch != (c.MAC == "M2") {
			t.Errorf("name match of %s (%s): %v", c.MAC, c.HostName, c.NameMatch)
		}
	}
	// two within the window: no preselection; none within the window: none
	devs = append(devs, dev("M5", "x", at(9)))
	if _, pre := registrationCandidates(devs, reg); pre != "" {
		t.Errorf("two near devices: %q", pre)
	}
	if _, pre := registrationCandidates(devs[:2], reg); pre != "" {
		t.Errorf("none near: %q", pre)
	}
	for host, want := range map[string]bool{"Wanjiru-s-iPhone": true, "iphone": true, "Wanjiru-laptop": true, "": false, "Galaxy-S21": false} {
		if got := deviceNameMatches("Wanjiru's iPhone", host); got != want {
			t.Errorf("%q: %v want %v", host, got, want)
		}
	}
}

func TestPendingCapGrowsWithTheBuilding(t *testing.T) {
	if PendingCap(0) != 25 || PendingCap(12) != 25 || PendingCap(13) != 26 || PendingCap(100) != 200 {
		t.Fatalf("%d %d %d %d", PendingCap(0), PendingCap(12), PendingCap(13), PendingCap(100))
	}
	e := newEnv(t)
	for i := 0; i < 15; i++ { // 15 houses: the cap is 30
		e.flat(t, "H"+string(rune('A'+i)), "AA:BB:CC:00:10:"+string(rune('0'+i/10))+string(rune('0'+i%10)))
	}
	for i := 0; i < 30; i++ {
		ph := "07" + strings.Repeat("1", 2) + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "0000"
		if _, err := e.svc.SubmitRegistration(e.regIn("Person X", ph, "Q"+ph)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	_, err := e.svc.SubmitRegistration(e.regIn("One More", "0799888777", "LAST"))
	wantRegErr(t, err, "Too many registrations")
}

// The rate-limit hook runs only once the form has validated, and a refusal by it stores nothing.
func TestAdmitIsCalledOnlyForValidForms(t *testing.T) {
	e := newEnv(t)
	calls := 0
	in := e.regIn("Valid Name", "0712345678", "A1")
	in.Admit = func(string) bool { calls++; return calls < 2 }
	bad := in
	bad.Consent = false
	if _, err := e.svc.SubmitRegistration(bad); err == nil || calls != 0 {
		t.Fatalf("a form without consent reached Admit: %v %d", err, calls)
	}
	if _, err := e.svc.SubmitRegistration(in); err != nil || calls != 1 {
		t.Fatalf("valid form: %v %d", err, calls)
	}
	in2 := e.regIn("Valid Two", "0722222222", "A2")
	in2.Admit = in.Admit
	if _, err := e.svc.SubmitRegistration(in2); !errors.Is(err, ErrRegRateLimited) {
		t.Fatalf("refused by Admit: %v", err)
	}
	if n := e.svc.PendingRegistrationCount(nil); n != 1 {
		t.Fatalf("pending %d", n)
	}
}

// A registration that names only an apartment (two routers behind one address) is held, assigned by the owner, then approved.
func TestUnassignedRegistrationIsAssignedThenApproved(t *testing.T) {
	e := newEnv(t)
	in := RegistrationInput{Unit: "B4", Name: "Free Text", Phone: "0712345678", Building: "  Riverside   Court ", DeviceName: "Free's phone", Consent: true}
	reg, err := e.svc.SubmitRegistration(in)
	if err != nil || reg.SiteID != "" || reg.Building != "Riverside Court" || reg.ApartmentID != "" || reg.DeviceName != "Free's phone" {
		t.Fatalf("%+v %v", reg, err)
	}
	if _, err := e.svc.SubmitRegistration(RegistrationInput{Unit: "B5", Name: "No Name", Phone: "0722222222", Consent: true}); err == nil {
		t.Fatal("a registration with neither router nor apartment name was accepted")
	}
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macHuawei, "owner"); !errors.Is(err, ErrRegUnassigned) {
		t.Fatalf("approve before assigning: %v", err)
	}
	if v := e.svc.PendingRegistrations(func(string) bool { return false }); len(v) != 1 || !v[0].Unassigned {
		t.Fatalf("unassigned rows are listed in every view: %+v", v)
	}
	if err := e.svc.AssignRegistration(reg.ID, e.site, "nope", "owner"); err == nil {
		t.Fatal("assigned to an apartment the router does not serve")
	}
	if err := e.svc.AssignRegistration(reg.ID, e.site, e.aptID(), "owner"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.GetRegistration(reg.ID)
	if got.SiteID != e.site || got.ApartmentID != e.aptID() || got.Building != "" {
		t.Fatalf("%+v", got)
	}
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	if a, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macHuawei, "owner"); err != nil || a.Unit != "B4" {
		t.Fatalf("%+v %v", a, err)
	}
	// the unassigned pool has its own cap
	for i := 0; i < MaxPendingUnassigned; i++ {
		ph := "07" + strings.Repeat("3", 2) + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "0000"
		if _, err := e.svc.SubmitRegistration(RegistrationInput{Unit: "U" + ph, Name: "Person X", Phone: ph, Building: "Somewhere", Consent: true}); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	_, err = e.svc.SubmitRegistration(RegistrationInput{Unit: "LAST", Name: "Person X", Phone: "0799000111", Building: "Somewhere", Consent: true})
	wantRegErr(t, err, "Too many registrations")
}

func TestRejectAllPendingOfOneRouterErasesThem(t *testing.T) {
	e := newEnv(t)
	other, _ := e.addSite(t, "Block B")
	otherApt := store.MigratedApartmentID(other)
	for i, ph := range []string{"0711111111", "0722222222", "0733333333"} {
		if _, err := e.svc.SubmitRegistration(e.regIn("Junk "+string(rune('A'+i)), ph, "J"+string(rune('1'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	keep, err := e.svc.SubmitRegistration(RegistrationInput{SiteID: other, ApartmentID: otherApt, Unit: "K1", Name: "Keep Me", Phone: "0744444444", Consent: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.RejectAllPending(e.site, "owner")
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	for _, r := range e.svc.PendingRegistrations(nil) {
		if r.ID != keep.ID {
			t.Fatalf("a rejected row is still pending: %+v", r)
		}
	}
	e.st.View(func(d *store.Data) {
		for _, r := range d.Registrations {
			if r.ID != keep.ID && (r.Status != store.RegRejected || r.Name != "" || r.Phone != "" || r.DecidedBy != "owner") {
				t.Errorf("not erased: %+v", r)
			}
		}
	})
	if n, _ := e.svc.RejectAllPending(e.site, "owner"); n != 0 {
		t.Fatalf("second call rejected %d", n)
	}
}

// An approval whose claim is older than five minutes (a crash in between) is pending again, on read and in the sweep.
func TestStaleApprovingReturnsToPending(t *testing.T) {
	e := newEnv(t)
	reg, err := e.svc.SubmitRegistration(e.regIn("Stuck Row", "0712345678", "S1"))
	if err != nil {
		t.Fatal(err)
	}
	e.st.Update(func(d *store.Data) error {
		r := d.Registrations[reg.ID]
		r.Status, r.DecidedAt = store.RegApproving, e.now
		d.Registrations[reg.ID] = r
		return nil
	})
	if n := e.svc.PendingRegistrationCount(nil); n != 0 {
		t.Fatalf("a fresh claim must not count: %d", n)
	}
	e.now = e.now.Add(6 * time.Minute)
	if n := e.svc.PendingRegistrationCount(nil); n != 1 || len(e.svc.PendingRegistrations(nil)) != 1 {
		t.Fatalf("a stale claim is pending on read: %d", n)
	}
	e.svc.MaintainRegistrations()
	if got, _ := e.svc.GetRegistration(reg.ID); got.Status != store.RegPending || !got.DecidedAt.IsZero() {
		t.Fatalf("after the sweep: %+v", got)
	}
	// and it can be approved or rejected again
	if err := e.svc.RejectRegistration(reg.ID, "owner"); err != nil {
		t.Fatal(err)
	}
}

// Old rows go on a timer, not only when someone submits: decided, undecided and the rows of a removed router.
func TestMaintainRegistrationsDeletesOldAndOrphanedRows(t *testing.T) {
	e := newEnv(t)
	gone, _ := e.addSite(t, "Block Gone")
	oldPending, _ := e.svc.SubmitRegistration(e.regIn("Old Pending", "0711111111", "O1"))
	orphan, _ := e.svc.SubmitRegistration(RegistrationInput{SiteID: gone, ApartmentID: store.MigratedApartmentID(gone), Unit: "G1", Name: "Orphan", Phone: "0722222222", Consent: true})
	e.now = e.now.Add(20 * 24 * time.Hour)
	fresh, _ := e.svc.SubmitRegistration(e.regIn("Fresh One", "0733333333", "F1"))
	e.st.Update(func(d *store.Data) error {
		s := d.Sites[gone]
		s.Removed = true
		d.Sites[gone] = s
		return nil
	})
	e.now = e.now.Add(11 * 24 * time.Hour) // oldPending is 31 days old, fresh 11
	e.svc.MaintainRegistrations()
	for id, want := range map[string]bool{oldPending.ID: false, orphan.ID: false, fresh.ID: true} {
		if _, ok := e.svc.GetRegistration(id); ok != want {
			t.Errorf("registration %s kept=%v want %v", id, ok, want)
		}
	}
}

// A returning tenant is linked to the customer record they have, not duplicated, unless that customer lives elsewhere.
func TestApproveLinksTheExistingCustomerWithTheSamePhone(t *testing.T) {
	e := newEnv(t)
	old, err := e.svc.SaveCustomer(CustomerInput{Name: "Returning Tenant", Phone: "0712345678"})
	if err != nil {
		t.Fatal(err)
	}
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	reg, _ := e.svc.SubmitRegistration(e.regIn("Returning T", "0712345678", "B4"))
	a, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macHuawei, "owner")
	if err != nil || a.CustomerID != old.ID {
		t.Fatalf("existing customer not linked: %+v %v", a, err)
	}
	n := 0
	e.st.View(func(d *store.Data) { n = len(d.Customers) })
	if n != 1 {
		t.Fatalf("customers: %d", n)
	}
	// the same number, but that customer already has an open account in ANOTHER apartment: a new customer
	other, _ := e.addSite(t, "Block B")
	oc, _ := e.svc.SaveCustomer(CustomerInput{Name: "Lives Elsewhere", Phone: "0722222222"})
	acc := e.flatAt(t, other, "Z1", macB)
	if err := e.svc.LinkCustomer(acc.ID, oc.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.dynLease(macNew, "10.5.50.12", "phone", "bound")
	e.reconcile(t)
	reg2, err := e.svc.SubmitRegistration(e.regIn("Lives Elsewhere", "0722222222", "B5"))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := e.svc.ApproveRegistration(context.Background(), reg2.ID, macNew, "owner")
	if err != nil || a2.CustomerID == "" || a2.CustomerID == oc.ID {
		t.Fatalf("a customer of another apartment must not be reused: %+v %v", a2, err)
	}
}

func TestRegistrationKeepsOnlyAValidFreeNamedMAC(t *testing.T) {
	e := newEnv(t)
	var r store.Registration
	var err error
	// a MAC named by the caller is kept only when it is a real, free, non-router MAC
	x := e.regIn("Named", "0733333333", "G3")
	x.MAC = "aa-bb-cc-dd-ee-01"
	if r, err = e.svc.SubmitRegistration(x); err != nil || r.MAC != "AA:BB:CC:DD:EE:01" {
		t.Fatalf("named MAC: %+v %v", r, err)
	}
	e.flat(t, "TAKEN", macA)
	x = e.regIn("Taken", "0744444444", "G4")
	x.MAC = macA
	_, err = e.svc.SubmitRegistration(x)
	wantRegErr(t, err, "already registered")
}

// pending -> approve: the account, the customer record, the device leaves the unregistered list and the next pass
// writes its lease (the wall is lifted).
func TestApproveRegistrationMakesTheAccountAndLiftsTheWall(t *testing.T) {
	e := newEnv(t)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	in := e.regIn("Wanjiru Kamau", "0712345678", "B4")
	in.Email = "w@example.com"
	reg, err := e.svc.SubmitRegistration(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, "", "owner"); !errors.Is(err, ErrRegNeedsMAC) {
		t.Fatalf("approving without a device: %v", err)
	}
	a, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macHuawei, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if a.MAC != macHuawei || a.Unit != "B4" || a.ApartmentID != e.aptID() || a.SiteID != e.site || a.Phone != "254712345678" {
		t.Fatalf("account %+v", a)
	}
	var cust store.Customer
	e.st.View(func(d *store.Data) { cust = d.Customers[a.CustomerID] })
	if cust.Name != "Wanjiru Kamau" || cust.Phone != "254712345678" || cust.Email != "w@example.com" {
		t.Fatalf("customer %+v", cust)
	}
	got, _ := e.svc.GetRegistration(reg.ID)
	if got.Status != store.RegApproved || got.AccountID != a.ID || got.Name != "" || got.Phone != "" || got.Email != "" || got.DecidedBy != "owner" {
		t.Fatalf("registration after approval (details must be gone): %+v", got)
	}
	if n := e.svc.PendingRegistrationCount(nil); n != 0 {
		t.Fatalf("still pending: %d", n)
	}
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macHuawei, "owner"); !errors.Is(err, ErrRegDecided) {
		t.Fatalf("second approval: %v", err)
	}
	if _, ok := unregByMAC(e)[macHuawei]; ok {
		t.Fatal("the approved device is still listed as new")
	}
	// the next pass puts the flat on the router: a static lease for its MAC at its own address
	e.reconcile(t)
	found := false
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		if strings.EqualFold(r["mac-address"], macHuawei) && r["address"] == a.IP && r["dynamic"] != "true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no static lease for %s at %s after the pass: %v", macHuawei, a.IP, e.rt.Table("/ip/dhcp-server/lease"))
	}
}

func TestApproveRegistrationKeepsTheAccountRules(t *testing.T) {
	e := newEnv(t)
	other, _ := e.addSite(t, "Block B")
	e.flatAt(t, other, "Z1", macB)
	e.ownIface("ether5", macRouterPort)
	e.reconcile(t)
	reg, err := e.svc.SubmitRegistration(e.regIn("Rule Tester", "0712345678", "R1"))
	if err != nil {
		t.Fatal(err)
	}
	var dup *DuplicateMACError
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macB, "owner"); !errors.As(err, &dup) || !dup.OtherBuilding {
		t.Fatalf("a MAC of another apartment's customer: %v", err)
	}
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macRouterPort, "owner"); !errors.Is(err, ErrRouterOwnMAC) {
		t.Fatalf("the router's own MAC: %v", err)
	}
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, "garbage", "owner"); !errors.Is(err, ErrBadMAC) {
		t.Fatalf("bad MAC: %v", err)
	}
	// every refusal left the registration waiting, and no account was made
	if got, _ := e.svc.GetRegistration(reg.ID); got.Status != store.RegPending || got.Name == "" {
		t.Fatalf("a refused approval changed the registration: %+v", got)
	}
	nAcct := 0
	e.st.View(func(d *store.Data) { nAcct = len(d.Accounts) })
	if nAcct != 1 {
		t.Fatalf("accounts: %d", nAcct)
	}
	// a free MAC works afterwards
	if _, err := e.svc.ApproveRegistration(context.Background(), reg.ID, macNew, "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestRejectRegistrationErasesTheDetails(t *testing.T) {
	e := newEnv(t)
	reg, err := e.svc.SubmitRegistration(e.regIn("Reject Me", "0712345678", "X1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RejectRegistration(reg.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.GetRegistration(reg.ID)
	if got.Status != store.RegRejected || got.Name != "" || got.Phone != "" || got.Email != "" || got.DecidedBy != "owner" {
		t.Fatalf("%+v", got)
	}
	if err := e.svc.RejectRegistration(reg.ID, "owner"); !errors.Is(err, ErrRegDecided) {
		t.Fatalf("twice: %v", err)
	}
	if err := e.svc.RejectRegistration("nope", "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// the house and the number are free to register again
	if _, err := e.svc.SubmitRegistration(e.regIn("Reject Me", "0712345678", "X1")); err != nil {
		t.Fatal(err)
	}
}

func TestPendingRegistrationsListCandidatesAndFilter(t *testing.T) {
	e := newEnv(t)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	if _, err := e.svc.SubmitRegistration(e.regIn("List Me", "0712345678", "L1")); err != nil {
		t.Fatal(err)
	}
	all := e.svc.PendingRegistrations(nil)
	if len(all) != 1 || all[0].SiteName != "Block A" || all[0].ApartmentName != "Block A" || len(all[0].Candidates) != 1 || all[0].Candidates[0].MAC != macHuawei {
		t.Fatalf("%+v", all)
	}
	if got := e.svc.PendingRegistrations(func(id string) bool { return id == "another" }); len(got) != 0 {
		t.Fatalf("filter: %+v", got)
	}
	if e.svc.PendingRegistrationCount(func(id string) bool { return id == e.site }) != 1 {
		t.Fatal("count")
	}
}
