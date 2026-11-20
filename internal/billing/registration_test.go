package billing

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	for i := 0; i < MaxPendingPerRouter; i++ {
		ph := "07" + strings.Repeat("1", 2) + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "0000"
		if _, err := e.svc.SubmitRegistration(e.regIn("Person X", ph, "H"+ph)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	_, err := e.svc.SubmitRegistration(e.regIn("One More", "0799888777", "LAST"))
	wantRegErr(t, err, "Too many registrations")
}

func TestRegistrationGuessesOnlyASingleOnlineDevice(t *testing.T) {
	e := newEnv(t)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	r, err := e.svc.SubmitRegistration(e.regIn("Guess Me", "0712345678", "G1"))
	if err != nil || r.GuessMAC != macHuawei || r.MAC != "" {
		t.Fatalf("one online device: %+v %v", r, err)
	}
	e.dynLease(macNew, "10.5.50.12", "phone", "bound")
	e.reconcile(t)
	r, err = e.svc.SubmitRegistration(e.regIn("No Guess", "0722222222", "G2"))
	if err != nil || r.GuessMAC != "" {
		t.Fatalf("two online devices must give no guess: %+v %v", r, err)
	}
	// a MAC named by the caller is kept only when it is a real, free, non-router MAC
	x := e.regIn("Named", "0733333333", "G3")
	x.MAC = "aa-bb-cc-dd-ee-01"
	if r, err = e.svc.SubmitRegistration(x); err != nil || r.MAC != "AA:BB:CC:DD:EE:01" || r.GuessMAC != "" {
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
	a, err := e.svc.ApproveRegistration(context.Background(), reg.ID, reg.GuessMAC, "owner")
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
