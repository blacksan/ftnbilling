package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func (e *env) customer(t *testing.T, name, phone string) store.Customer {
	t.Helper()
	c, err := e.svc.SaveCustomer(CustomerInput{Name: name, Phone: phone, WASamePhone: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCustomerLinkMoveOutAndMessagingCopy(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", mac1)
	alice := e.customer(t, "Alice", "0711000001")
	if alice.Phone != "254711000001" || alice.WhatsApp != "254711000001" {
		t.Fatalf("%+v", alice)
	}
	if err := e.svc.LinkCustomer(a.ID, alice.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got := e.get(a)
	if got.CustomerID != alice.ID || got.Tenant != "Alice" || got.Phone != "254711000001" || got.WhatsApp != "254711000001" {
		t.Fatalf("account not synced: %+v", got)
	}
	// Linking the same person again changes nothing; editing the customer updates the account.
	if err := e.svc.LinkCustomer(a.ID, alice.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SaveCustomer(CustomerInput{ID: alice.ID, Name: "Alice W", Phone: "0711000001", WhatsApp: "0711000009", WAOptOut: true}); err != nil {
		t.Fatal(err)
	}
	got = e.get(a)
	if got.Tenant != "Alice W" || got.WhatsApp != "254711000009" || !got.WAOptOut {
		t.Fatalf("edit not copied: %+v", got)
	}
	// A new tenant replaces her; her tenancy ends the same day.
	bob := e.customer(t, "Bob", "0711000002")
	e.now = e.now.Add(48 * time.Hour)
	if err := e.svc.LinkCustomer(a.ID, bob.ID, e.now); err != nil {
		t.Fatal(err)
	}
	e.st.View(func(d *store.Data) {
		open := 0
		for _, tn := range d.Tenancies {
			if tn.AccountID == a.ID && tn.MoveOut.IsZero() {
				open++
				if tn.CustomerID != bob.ID {
					t.Fatal("wrong open tenant")
				}
			}
			if tn.CustomerID == alice.ID && !tn.MoveOut.Equal(e.now) {
				t.Fatalf("her tenancy should end at the move-in: %+v", tn)
			}
		}
		if open != 1 {
			t.Fatalf("%d open tenancies", open)
		}
	})
	// Move out: the flat is vacant and nothing can message the old tenant.
	if err := e.svc.MoveOut(a.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got = e.get(a)
	if got.CustomerID != "" || got.Tenant != "" || got.Phone != "" || got.WhatsApp != "" {
		t.Fatalf("not vacant: %+v", got)
	}
	if err := e.svc.MoveOut(a.ID, time.Time{}); err != nil {
		t.Fatalf("second move-out is a no-op, got %v", err)
	}
}

func TestSTOPStartUpdatesCustomerToo(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", mac1)
	c := e.customer(t, "Alice", "0711000001")
	e.svc.LinkCustomer(a.ID, c.ID, time.Time{})
	e.st.Update(func(d *store.Data) error {
		if !d.SetWAOptOutByNumber("254711000001", true) {
			t.Fatal("nothing changed")
		}
		return nil
	})
	e.st.View(func(d *store.Data) {
		if !d.Accounts[a.ID].WAOptOut || !d.Customers[c.ID].WAOptOut {
			t.Fatal("STOP must reach both the account and the customer")
		}
	})
	e.svc.SetWhatsApp(a.ID, "", true, false) // admin form on the account
	e.st.View(func(d *store.Data) {
		if d.Customers[c.ID].WAOptOut {
			t.Fatal("START on the account must reach the customer")
		}
	})
}

func TestDeleteAccountClosesInsteadOfErasing(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", mac1)
	c := e.customer(t, "Alice", "0711000001")
	e.svc.LinkCustomer(a.ID, c.ID, time.Time{})
	ont, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "sn-1", MAC: mac1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AssignEquipment(context.Background(), ont.ID, a.ID, "", false); err != nil {
		t.Fatal(err)
	}
	e.pay(t, a, "1 Day", "10.5.50.20")
	if err := e.svc.DeleteAccount(context.Background(), a.ID, "WRONG"); err == nil {
		t.Fatal("needs the code")
	}
	if err := e.svc.DeleteAccount(context.Background(), a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	e.st.View(func(d *store.Data) {
		if _, open := d.Accounts[a.ID]; open {
			t.Fatal("still open")
		}
		cl, ok := d.ClosedAccounts[a.ID]
		if !ok || cl.ClosedAt.IsZero() || cl.Code != a.Code {
			t.Fatalf("not closed: %+v", cl)
		}
		if _, has := d.OpenTenancy(a.ID); has {
			t.Fatal("tenancy still open")
		}
		if eq := d.Equipment[ont.ID]; eq.Status != store.EqReturned || eq.AccountID != "" {
			t.Fatalf("equipment %+v", eq)
		}
		for _, p := range d.Payments {
			if p.AccountID != a.ID || p.CustomerID != c.ID {
				t.Fatalf("ledger row lost its links: %+v", p)
			}
		}
		// The code is never reused, and the MAC can be taken by a new flat.
		if _, taken := d.AccountByCode(a.Code); taken {
			t.Fatal("closed account must not answer code lookups")
		}
	})
	for i := 0; i < 50; i++ {
		if n, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat X", MAC: mac1}); err == nil {
			if n.Code == a.Code {
				t.Fatal("code reused")
			}
			e.st.Update(func(d *store.Data) error { delete(d.Accounts, n.ID); return nil })
		} else {
			t.Fatalf("MAC of a closed account should be free: %v", err)
		}
	}
}

func TestPaymentsCarryTheOccupantAtCreditTime(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", mac1)
	alice, bob := e.customer(t, "Alice", "0711000001"), e.customer(t, "Bob", "0711000002")
	e.svc.LinkCustomer(a.ID, alice.ID, time.Time{})
	e.pay(t, a, "1 Day", "10.5.50.20")
	e.svc.LinkCustomer(a.ID, bob.ID, time.Time{})
	if _, err := e.svc.RecordUnmatched(UnmatchedInput{Key: "UNM0000001", Method: "mpesa-c2b", AmountKES: 40, AccountID: a.ID}); err != nil {
		t.Fatal(err)
	}
	pkg := e.pkg("1 Day")
	if err := e.svc.AssignPayment(context.Background(), "UNM0000001", a.ID, pkg.ID, ""); err != nil {
		t.Fatal(err)
	}
	rows, tot := e.svc.FilterPayments(PaymentFilter{CustomerID: alice.ID})
	if len(rows) != 1 || tot.Count != 1 {
		t.Fatalf("alice has %d payments", len(rows))
	}
	rows, _ = e.svc.FilterPayments(PaymentFilter{CustomerID: bob.ID})
	if len(rows) != 1 || rows[0].Key != "UNM0000001" {
		t.Fatalf("bob: %+v", rows)
	}
	prof, ok := e.svc.Customer(context.Background(), alice.ID)
	if !ok || len(prof.Payments) != 1 || len(prof.Tenancies) != 1 || prof.Total != prof.Payments[0].AmountKES || len(prof.Timeline) == 0 {
		t.Fatalf("profile %+v", prof)
	}
}

func TestEquipmentLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 1", mac1)
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "toaster"}); err == nil {
		t.Fatal("kind validated")
	}
	ont, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "zte123", MAC: "aa-bb-cc-dd-ee-02", Model: "F601"})
	if err != nil || ont.Status != store.EqInStock || ont.Serial != "ZTE123" || ont.MAC != "AA:BB:CC:DD:EE:02" {
		t.Fatalf("%+v %v", ont, err)
	}
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "ZTE123"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("duplicate serial: %v", err)
	}
	if err := e.svc.AssignEquipment(ctx, ont.ID, a.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if got := e.get(a); got.MAC != "AA:BB:CC:DD:EE:02" {
		t.Fatalf("ONT MAC not applied: %s", got.MAC)
	}
	if err := e.svc.AssignEquipment(ctx, ont.ID, a.ID, "", false); err == nil {
		t.Fatal("an installed item cannot be assigned again")
	}
	if err := e.svc.ReturnEquipment(ont.ID, store.EqFaulty, "no light"); err != nil {
		t.Fatal(err)
	}
	router, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "router", Model: "RB5009", SiteID: e.site})
	if router.Status != store.EqInstalled || router.SiteID != e.site {
		t.Fatalf("building router %+v", router)
	}
	sp, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "splitter", Model: "1:8", SiteID: e.site})
	if err := e.svc.SetLocation(a.ID, "3", "ODB-2", sp.ID, 5); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetLocation(a.ID, "3", "ODB-2", ont.ID, 5); err == nil {
		t.Fatal("only splitters")
	}
	res, err := e.svc.ImportEquipment("kind,serial,mac,model\nont,SN-A,,F601\nont,SN-B,,F601\nont,SN-A,,F601\nbogus,x,,")
	if err != nil || res.Added != 2 || len(res.Skipped) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	e.st.View(func(d *store.Data) {
		kinds := map[string]int{}
		for _, ev := range d.EquipmentEvents {
			if ev.EquipmentID == ont.ID {
				kinds[ev.Kind]++
			}
		}
		if kinds[store.EqEvReceived] != 1 || kinds[store.EqEvAssigned] != 1 || kinds[store.EqEvFaulty] != 1 {
			t.Fatalf("history %v", kinds)
		}
	})
	rep, ok := e.svc.Building(e.site)
	if !ok || len(rep.Router) != 1 || rep.Flats != 1 || len(rep.Months) != 12 {
		t.Fatalf("%+v", rep)
	}
}

func TestNationalIDEncryptedRevealAudited(t *testing.T) {
	e := newEnv(t)
	c := e.customer(t, "Alice", "0711000001")
	if err := e.svc.SetNationalID(c.ID, "12345678"); err != ErrNoDataKey {
		t.Fatalf("feature is off without a key: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	if err := e.svc.SetDataKey(key); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetNationalID(c.ID, "12 345-678"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetNationalID(c.ID, "ab"); err == nil {
		t.Fatal("too short")
	}
	e.st.View(func(d *store.Data) {
		cc := d.Customers[c.ID]
		if cc.NationalIDLast4 != "5678" || cc.NationalIDEnc == "" || strings.Contains(cc.NationalIDEnc, "12345678") || cc.NationalIDHMAC == "" {
			t.Fatalf("%+v", cc)
		}
	})
	if hits := e.svc.FindByNationalID("12345678"); len(hits) != 1 || hits[0].ID != c.ID {
		t.Fatal("blind index search")
	}
	if len(e.svc.FindByNationalID("1234567")) != 0 {
		t.Fatal("exact only")
	}
	before := e.st.Snapshot().EventSeq
	id, err := e.svc.RevealNationalID(c.ID)
	if err != nil || id != "12345678" {
		t.Fatalf("%q %v", id, err)
	}
	snap := e.st.Snapshot()
	if snap.EventSeq != before+1 || snap.Events[len(snap.Events)-1].Kind != "audit" || strings.Contains(snap.Events[len(snap.Events)-1].Msg, "1234") {
		t.Fatal("a reveal must write an audit event without the number")
	}
	// A different key cannot read it.
	e.svc.SetDataKey(make([]byte, 32))
	if _, err := e.svc.RevealNationalID(c.ID); err == nil {
		t.Fatal("wrong key must fail")
	}
	e.svc.SetDataKey(key)
	// Anonymise: needs the name, scrubs everything, keeps the ledger link.
	if err := e.svc.AnonymizeCustomer(c.ID, "nope"); err == nil {
		t.Fatal("needs the name")
	}
	e.svc.AddNote(c.ID, "", "", "", "secret note")
	if err := e.svc.AnonymizeCustomer(c.ID, "Alice"); err != nil {
		t.Fatal(err)
	}
	e.st.View(func(d *store.Data) {
		cc := d.Customers[c.ID]
		if cc.Name == "Alice" || cc.Phone != "" || cc.NationalIDEnc != "" || cc.NationalIDHMAC != "" || cc.AnonymizedAt.IsZero() || len(d.Notes) != 0 {
			t.Fatalf("not scrubbed: %+v", cc)
		}
	})
}

func TestSecretboxRoundTrip(t *testing.T) {
	b, err := NewSecretbox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := b.Seal("X")
	s2, _ := b.Seal("X")
	if s1 == s2 {
		t.Fatal("nonce reuse")
	}
	if p, err := b.Open(s1); err != nil || p != "X" {
		t.Fatal(err)
	}
	if b.Index("X") != b.Index("X") || b.Index("X") == b.Index("Y") {
		t.Fatal("index")
	}
	if _, err := NewSecretbox(make([]byte, 5)); err == nil {
		t.Fatal("key size")
	}
}

func TestSearchNormalisation(t *testing.T) {
	e := newEnv(t)
	all := func(string) bool { return true }
	a := e.flat(t, "Flat 12B", "AA:BB:CC:DD:EE:0A")
	c := e.customer(t, "Wanjiku Kamau", "0712345678")
	e.svc.LinkCustomer(a.ID, c.ID, time.Time{})
	ont, _ := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "ZTEGC123"})
	e.pay(t, a, "1 Day", "10.5.50.20")
	for q, want := range map[string]string{
		"0712345678": "customer", "+254712345678": "customer", "712345678": "customer", "345678": "customer", // suffix of 6+
		"wanjiku": "customer", a.Code: "account", strings.ToLower(a.Code): "account", "12b": "account",
		"aabbccddee0a": "account", "AA-BB-CC-DD-EE-0A": "account", "ee0a": "account",
		"ztegc123": "equipment", "ztegc": "equipment", "block a": "building",
	} {
		r := e.svc.Search(q, all)
		found := map[string]bool{"customer": len(r.Customers) > 0, "account": len(r.Accounts) > 0, "equipment": len(r.Equipment) > 0, "building": len(r.Buildings) > 0}
		if !found[want] {
			t.Errorf("search %q should find a %s: %+v", q, want, r)
		}
	}
	if r := e.svc.Search("12345", all); len(r.Customers) != 0 {
		t.Error("fewer than 6 digits is not a phone suffix")
	}
	if r := e.svc.Search(a.Code, all); r.Total() != 1 {
		t.Errorf("exact code should be a single hit: %d", r.Total())
	} else if k, id, _ := r.Single(); k != "account" || id != a.Code {
		t.Errorf("single: %s %s", k, id)
	}
	r := e.svc.Search("ZTEGC123", all)
	if k, id, ok := r.Single(); !ok || k != "equipment" || id != ont.ID {
		t.Errorf("serial single: %v %v %v", k, id, ok)
	}
	if r := e.svc.Search("wanjiku", func(string) bool { return false }); len(r.Accounts) != 0 {
		t.Error("scope must hide other buildings' accounts")
	}
	pay := e.st.Snapshot().Payments
	for k := range pay {
		if r := e.svc.Search(k, all); len(r.Payments) != 1 {
			t.Errorf("receipt %s not found", k)
		}
	}
}

func TestPaymentFiltersAndTotals(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", mac1)
	e.pay(t, a, "1 Day", "10.5.50.20")
	e.svc.ExtendAccount(context.Background(), a.ID, e.pkg("1 Day").ID, "gift", true)
	rows, tot := e.svc.FilterPayments(PaymentFilter{})
	if len(rows) != 2 || tot.Count != 2 || tot.Revenue != e.pkg("1 Day").PriceKES {
		t.Fatalf("grants are free time, not revenue: %+v", tot)
	}
	if rows, _ = e.svc.FilterPayments(PaymentFilter{Method: "admin"}); len(rows) != 1 {
		t.Fatal("method filter")
	}
	day := time.Date(2026, 11, 17, 0, 0, 0, 0, EAT)
	if rows, _ = e.svc.FilterPayments(PaymentFilter{From: day, To: day}); len(rows) != 2 {
		t.Fatalf("same-day range is inclusive: %d", len(rows))
	}
	if rows, _ = e.svc.FilterPayments(PaymentFilter{From: day.AddDate(0, 0, 1)}); len(rows) != 0 {
		t.Fatal("from filter")
	}
	if rows, _ = e.svc.FilterPayments(PaymentFilter{Q: a.Code}); len(rows) != 2 {
		t.Fatal("q matches the account code")
	}
	if rows, _ = e.svc.FilterPayments(PaymentFilter{AccountID: "nope"}); len(rows) != 0 {
		t.Fatal("account filter")
	}
}

func TestBuildingProfileAndContacts(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SaveSiteProfile(SiteProfileInput{SiteID: e.site, Address: "Kilimani Rd", Lat: -1.29, Lng: 36.82, GoLive: e.now}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SaveSiteProfile(SiteProfileInput{SiteID: e.site, Lat: 95, Lng: 1}); err == nil {
		t.Fatal("latitude range")
	}
	if err := e.svc.SaveSiteProfile(SiteProfileInput{SiteID: e.site, Lat: 5}); err == nil {
		t.Fatal("both or neither")
	}
	c, err := e.svc.SaveContact(ContactInput{SiteID: e.site, Role: "caretaker", Name: "Joe", Phone: "0722000000"})
	if err != nil || c.Phone != "254722000000" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := e.svc.SaveContact(ContactInput{SiteID: e.site, Role: "wizard", Name: "x"}); err == nil {
		t.Fatal("role")
	}
	rep, _ := e.svc.Building(e.site)
	if rep.Site.Address != "Kilimani Rd" || len(rep.Contacts) != 1 {
		t.Fatalf("%+v", rep)
	}
	if err := e.svc.DeleteContact("other", c.ID); err == nil {
		t.Fatal("contact of another building")
	}
	if err := e.svc.DeleteContact(e.site, c.ID); err != nil {
		t.Fatal(err)
	}
}
