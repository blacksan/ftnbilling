package web_test

import (
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

func TestAPIRegistrationsListApproveReject(t *testing.T) {
	r := newAPIRig(t)
	apt := ""
	r.st.Update(func(d *store.Data) error {
		apt = store.NewApartmentID()
		d.Apartments[apt] = store.Apartment{ID: apt, Name: "Alpha Court"}
		d.ApartmentSites[store.ApartmentSiteKey(apt, r.A.ID)] = store.ApartmentSite{ApartmentID: apt, SiteID: r.A.ID}
		return nil
	})
	mk := func(unit string) string {
		reg, err := r.svc.SubmitRegistration(billing.RegistrationInput{SiteID: r.A.ID, ApartmentID: apt, Unit: unit, Name: "Amina Otieno", Phone: "0722000" + unit, Email: "", Consent: true})
		if err != nil {
			t.Fatal(err)
		}
		return reg.ID
	}
	a, b := mk("111"), mk("222")
	got := r.call(r.view.Access, "GET", "/registrations", nil, nil)
	j := got.JSON(t)
	if got.Status != 200 || j["count"].(float64) != 2 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	// a viewer may not decide
	if v := r.call(r.view.Access, "POST", "/registrations/"+a+"/reject", map[string]any{}, nil); v.Status != 403 {
		t.Errorf("viewer: %d", v.Status)
	}
	// approve needs a MAC
	if v := r.call(r.oper.Access, "POST", "/registrations/"+a+"/approve", map[string]any{}, nil); v.Status < 400 {
		t.Errorf("approve without a MAC: %d %s", v.Status, v.Raw)
	}
	ok := r.call(r.oper.Access, "POST", "/registrations/"+a+"/approve", map[string]any{"mac": "00:E0:4C:68:01:02"}, nil)
	if ok.Status != 200 && ok.Status != 201 {
		t.Fatalf("approve: %d %s", ok.Status, ok.Raw)
	}
	if again := r.call(r.oper.Access, "POST", "/registrations/"+a+"/approve", map[string]any{"mac": "00:E0:4C:68:01:02"}, nil); again.Status < 400 {
		t.Errorf("approving twice: %d", again.Status)
	}
	if rj := r.call(r.oper.Access, "POST", "/registrations/"+b+"/reject", map[string]any{}, nil); rj.Status != 200 && rj.Status != 204 {
		t.Fatalf("reject: %d %s", rj.Status, rj.Raw)
	}
	if n := r.call(r.owner.Access, "GET", "/registrations", nil, nil).JSON(t)["count"].(float64); n != 0 {
		t.Errorf("still pending: %v", n)
	}
	if r.call(r.oper.Access, "POST", "/registrations/nope/reject", map[string]any{}, nil).Status != 404 {
		t.Error("unknown id should be 404")
	}
}

func TestSettingsSupportContactsSaveAndClear(t *testing.T) {
	r := newSetRig(t)
	_, msg, _ := r.post("/admin/settings/support", url.Values{"support.phone": {"0711 222 333"}, "support.email": {"help@example.com"}, "support.whatsapp": {"0711222333"}})
	if !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("save: %q", msg)
	}
	e := r.m.Effective()
	if e.SupportPhone != "0711 222 333" && e.SupportPhone != "0711222333" || e.SupportEmail != "help@example.com" || e.SupportWhatsApp != "254711222333" {
		t.Fatalf("%q %q %q", e.SupportPhone, e.SupportEmail, e.SupportWhatsApp)
	}
	if _, msg, _ = r.post("/admin/settings/support", url.Values{"support.phone": {""}, "support.email": {"not an email"}, "support.whatsapp": {""}}); !strings.HasPrefix(msg, "Error") {
		t.Errorf("a bad email was accepted: %q", msg)
	}
	if _, msg, _ = r.post("/admin/settings/support", url.Values{"support.phone": {""}, "support.email": {""}, "support.whatsapp": {""}}); !strings.HasPrefix(msg, "Saved") {
		t.Fatalf("clear: %q", msg)
	}
	e = r.m.Effective()
	if e.SupportPhone != "" || e.SupportEmail != "" || e.SupportWhatsApp != "" {
		t.Fatalf("a cleared contact came back: %+v", e)
	}
	if page := r.get(t, "/admin/settings/support"); !strings.Contains(page, "WhatsApp") {
		t.Error("the page does not render")
	}
}
