package billing

import (
	"context"
	"ispbill/internal/store"
	"testing"
)

func TestAccountWhatsAppFields(t *testing.T) {
	e := newEnv(t)
	a, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 1", MAC: "AA:BB:CC:00:00:01", Phone: "0712345678", WASamePhone: true})
	if err != nil || a.WhatsApp != "254712345678" || a.WAOptOut {
		t.Fatalf("same as phone: %+v %v", a, err)
	}
	b, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 2", MAC: "AA:BB:CC:00:00:02", Phone: "0712345678", WhatsApp: "+254 722 000 111"})
	if err != nil || b.WhatsApp != "254722000111" {
		t.Fatalf("typed number: %+v %v", b, err)
	}
	c, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 3", MAC: "AA:BB:CC:00:00:03"})
	if err != nil || c.WhatsApp != "" {
		t.Fatalf("none: %+v %v", c, err)
	}
	if _, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 4", MAC: "AA:BB:CC:00:00:04", WhatsApp: "123"}); err == nil {
		t.Fatal("a bad WhatsApp number is refused")
	}
	if err := e.svc.SetWhatsApp(c.ID, "0733111222", false, true); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.GetAccount(c.ID)
	if got.WhatsApp != "254733111222" || !got.WAOptOut {
		t.Fatalf("%+v", got)
	}
	if err := e.svc.SetWhatsApp(c.ID, "", true, false); err != nil { // same as phone, but there is no phone
		t.Fatal(err)
	}
	if got, _ = e.svc.GetAccount(c.ID); got.WhatsApp != "" || got.WAOptOut {
		t.Fatalf("%+v", got)
	}
	if err := e.svc.SetWhatsApp("nope", "", false, false); err == nil {
		t.Fatal("unknown account")
	}
}

func TestCreditHookRunsAfterPayment(t *testing.T) {
	e := newEnv(t)
	a, _ := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 1", MAC: "AA:BB:CC:00:00:01"})
	n := 0
	e.svc.SetCreditHook(func() { n++ })
	var pid string
	e.st.View(func(d *store.Data) {
		for _, p := range d.PackageList() {
			pid = p.ID
			break
		}
	})
	if _, created, err := e.svc.Credit(context.Background(), CreditInput{Key: "HOOK1", Method: "mpesa-manual", AccountID: a.ID, PackageID: pid, AmountKES: 1}); err != nil || !created {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hook called %d times", n)
	}
	e.svc.Credit(context.Background(), CreditInput{Key: "HOOK1", Method: "mpesa-manual", AccountID: a.ID, PackageID: pid, AmountKES: 1})
	if n != 1 {
		t.Fatal("a duplicate payment must not call the hook")
	}
}
