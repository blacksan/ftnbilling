package acs

import (
	"context"
	"strings"
	"testing"

	"ispbill/internal/store"
)

func (f *fixture) auditMsgs() []string {
	var out []string
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" {
				out = append(out, e.Actor+"|"+e.Msg)
			}
		}
	})
	return out
}

// vsolOnePointFour is the bench box: one 2.4 GHz radio, the house Wi-Fi set on it.
func vsolSingle(t *testing.T, f *fixture) (*cpe, string) {
	c := newVSOL(t, f, "VSOL00127E0F", "10.6.3.20", false)
	r := c.root + "LANDevice.1.WLANConfiguration."
	c.mu.Lock()
	for k := range c.params {
		if strings.HasPrefix(k, r+"5.") {
			delete(c.params, k)
		}
	}
	c.mu.Unlock()
	setupLinked(t, c)
	return c, r
}

// A box with one radio has one Wi-Fi name and password: the views say so, a 5 GHz entry is ignored, and a 5 GHz
// override saved earlier (a dual-band box that was swapped for this one) stays for the next swap.
func TestSingleRadioBoxHasOneNetworkAndKeepsTheOverride(t *testing.T) {
	f := newFixture(t)
	c, r := vsolSingle(t, f)
	v, ok := f.svc.OfAccount("a1")
	if !ok || v.Has5GHz || len(v.Bands) != 1 || v.Bands[0] != "2.4" {
		t.Fatalf("bands: %+v", v)
	}
	// the house has a 5 GHz override from an earlier dual-band box
	enc, _ := f.svc.sealPass("fiveGpass1")
	enc2, _ := f.svc.sealPass("oldpass12")
	f.st.Update(func(d *store.Data) error {
		a := d.Accounts["a1"]
		a.HomeWiFi = store.WiFiDesired{Set: true, SSID: "Old", PassEnc: enc2, SSID5: "Old-5G", Pass5Enc: enc}
		d.Accounts["a1"] = a
		return nil
	})
	// Same=false with no 5 GHz values: accepted, because this box has no 5 GHz radio
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "House24", Pass: "house24pass"}); err != nil {
		t.Fatalf("single radio refused: %v", err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	if c.get(r+"1.SSID") != "House24" {
		t.Fatal("not written")
	}
	var h store.WiFiDesired
	f.st.View(func(d *store.Data) { h = d.Accounts["a1"].HomeWiFi })
	if h.SSID != "House24" || h.Same || h.SSID5 != "Old-5G" || h.Pass5Enc != enc {
		t.Fatalf("the 5 GHz override was not kept: %+v", h)
	}
	// without an override the house is "same for all bands" again
	f.st.Update(func(d *store.Data) error {
		a := d.Accounts["a1"]
		a.HomeWiFi = store.WiFiDesired{}
		d.Accounts["a1"] = a
		return nil
	})
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Plain", Pass: "plainpass1", SSID5: "ignored", Pass5: "ignored12"}); err != nil {
		t.Fatal(err)
	}
	f.st.View(func(d *store.Data) { h = d.Accounts["a1"].HomeWiFi })
	if !h.Same || h.SSID5 != "" || h.Pass5Enc != "" {
		t.Fatalf("one name for all bands: %+v", h)
	}
}

func TestDualBandBoxReportsBothBands(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNZ", "10.6.3.20")
	setupLinked(t, c)
	v, _ := f.svc.OfAccount("a1")
	if !v.Has5GHz || len(v.Bands) != 2 {
		t.Fatalf("%+v", v)
	}
}

func TestRevealWiFiWritesAuditAndShowsTheSavedPassword(t *testing.T) {
	f := newFixture(t)
	c, r := vsolSingle(t, f)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "House24", Pass: "house24pass", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	// the box returns the same password: confirmed from the ONU
	res, err := f.svc.RevealWiFi(context.Background(), "brian", "203.0.113.9", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Networks) != 1 || res.Networks[0].Password != "house24pass" || res.Networks[0].Source != "onu" || res.Networks[0].Band != "2.4" || res.Networks[0].Differs {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, m := range f.auditMsgs() {
		if m == "brian|admin brian viewed the Wi-Fi password for house ABC234" {
			found = true
		}
		if strings.Contains(m, "house24pass") {
			t.Fatalf("the password is in the audit trail: %s", m)
		}
	}
	if !found {
		t.Fatalf("no audit entry: %v", f.auditMsgs())
	}
	// the box returns nothing (it hides the key): the saved one is shown
	c.set(r+"1.PreSharedKey.1.KeyPassphrase", "")
	res, err = f.svc.RevealWiFi(context.Background(), "brian", "203.0.113.9", "a1")
	if err != nil || len(res.Networks) != 1 || res.Networks[0].Password != "house24pass" || res.Networks[0].Source != "saved" || !strings.Contains(res.Note, "saved") {
		t.Fatalf("blank from the box: %+v %v", res, err)
	}
	// the tenant changed it on the box: shown, and said
	c.set(r+"1.PreSharedKey.1.KeyPassphrase", "tenantpass1")
	res, _ = f.svc.RevealWiFi(context.Background(), "brian", "203.0.113.9", "a1")
	if res.Networks[0].Password != "tenantpass1" || !res.Networks[0].Differs || !strings.Contains(res.Note, "not the one saved") {
		t.Fatalf("changed on the box: %+v", res)
	}
	// the key is also read for the fake's raw 64-digit form: not shown as a password
	c.set(r+"1.PreSharedKey.1.KeyPassphrase", strings.Repeat("a", 64))
	res, _ = f.svc.RevealWiFi(context.Background(), "brian", "203.0.113.9", "a1")
	if res.Networks[0].Password != "house24pass" || res.Networks[0].Source != "saved" {
		t.Fatalf("64-digit key: %+v", res)
	}
	if n := len(f.auditMsgs()); n < 4 {
		t.Fatalf("every view is audited: %d", n)
	}
}

func TestRevealWiFiOfflineShowsTheSavedPassword(t *testing.T) {
	f := newFixture(t)
	c, _ := vsolSingle(t, f)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "House24", Pass: "house24pass", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	f.advance(2 * 3600 * 1e9) // the ONU is silent: no session is asked for
	res, err := f.svc.RevealWiFi(context.Background(), "brian", "", "a1")
	if err != nil || res.Asked || res.Networks[0].Password != "house24pass" || res.Networks[0].Source != "saved" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := f.svc.RevealWiFi(context.Background(), "brian", "", "nope"); err == nil {
		t.Fatal("an unknown house")
	}
}
