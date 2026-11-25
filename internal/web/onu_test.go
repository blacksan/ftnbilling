package web_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

func newONUApp(t *testing.T) *app {
	return newAppCfg(t, false, func(c *config.Config) {
		c.ACSListen, c.ACSCRPorts, c.ACSInformInterval, c.ACSMaxPerRouter = "10.99.0.1:7547", []int{7547, 30005}, 300, 200
		c.WGSubnet = "10.99.0.0/24"
	})
}

// seedONU stores a device the ACS would have recorded.
func (a *app) seedONU(t *testing.T, id, serial, accountID string, macs ...string) {
	t.Helper()
	if err := a.st.Update(func(d *store.Data) error {
		s := d.Sites[a.site]
		s.ONUMgmtN = 5
		d.Sites[a.site] = s
		o := store.ONU{ID: id, Serial: serial, Manufacturer: "ZTE", ProductClass: "F670L", Root: store.RootTR098, SiteID: a.site, MgmtIP: "10.6.5.20",
			ConnReqURL: "http://10.6.5.20:7547/cr", MACs: macs, LastInform: time.Now(), FirstSeen: time.Now(), AccountID: accountID,
			Sec:  store.ONUSec{ACSCred: store.CredDevice, MgmtMAC: "AA:BB:CC:00:00:99"},
			WiFi: []store.ONUWiFi{{Path: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.", AP: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.", Band: "2.4", SSID: "Old2G", Enabled: true}}}
		if accountID != "" {
			o.Link = store.LinkManual
		}
		d.ONUs[id] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestONUSettingsPageHidesAndRevealsLogin(t *testing.T) {
	a := newONUApp(t)
	page := a.adminGet(t, "/admin/settings/onu")
	if !strings.Contains(page, "http://10.99.0.1:7547") {
		t.Fatal("ACS URL missing")
	}
	_, pass, err := a.acs.Credentials()
	if err != nil || pass == "" {
		t.Fatal(err)
	}
	if strings.Contains(page, pass) {
		t.Fatal("the password is on the page before step-up")
	}
	r := a.admin("POST", "/admin/settings/onu/reveal", url.Values{"current_password": {"wrong"}})
	b := body(t, r)
	if strings.Contains(b, pass) {
		t.Fatal("revealed with a wrong password")
	}
	r = a.admin("POST", "/admin/settings/onu/reveal", url.Values{"current_password": {pw}})
	if b = body(t, r); !strings.Contains(b, pass) {
		t.Fatalf("not revealed after step-up: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if !strings.Contains(a.adminGet(t, "/admin/settings/onu"), "V-SOL") {
		t.Fatal("OLT instructions missing")
	}
}

func TestONUWiFiFromAccountPage(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 9"}, "mac": {mac1}})
	acct := a.account(t, "Flat 9")
	a.seedONU(t, "onu1", "ZTEGC0001", acct.ID, "AA:BB:CC:DD:EE:01")
	page := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	if !strings.Contains(page, "ZTEGC0001") || !strings.Contains(page, "Old2G") || !strings.Contains(page, "Change Wi-Fi") {
		t.Fatal("the Wi-Fi card is missing")
	}
	loc := a.adminPost(t, "/admin/accounts/wifi", url.Values{"id": {acct.ID}, "ssid": {"Home Net"}, "password": {"short"}, "same": {"1"}})
	if !strings.Contains(loc, "Error") {
		t.Fatalf("a 5 character password was accepted: %s", loc)
	}
	loc = a.adminPost(t, "/admin/accounts/wifi", url.Values{"id": {acct.ID}, "ssid": {"Home Net"}, "password": {"supersecret99"}, "same": {"1"}})
	if !strings.Contains(loc, "queued") {
		t.Fatalf("%s", loc)
	}
	a.acs.Wait()
	page = a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	if strings.Contains(page, "supersecret99") {
		t.Fatal("the password is shown back")
	}
	if !strings.Contains(page, "Queued") {
		t.Fatal("task status missing")
	}
	// the password is stored encrypted
	a.st.View(func(d *store.Data) {
		w := d.Accounts[acct.ID].HomeWiFi
		if w.PassEnc == "" || strings.Contains(w.PassEnc, "supersecret99") {
			t.Fatalf("not encrypted: %q", w.PassEnc)
		}
	})
	if l := a.adminPost(t, "/admin/accounts/onu/reboot", url.Values{"id": {acct.ID}}); !strings.Contains(l, "reboot queued") {
		t.Fatal(l)
	}
}

func TestONURoutesNeedOperatorAndShowRouterTab(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onu2", "SN2", "", "AA:BB:CC:DD:EE:02")
	tab := a.adminGet(t, "/admin/routers/view?id="+a.site+"&tab=onus")
	if !strings.Contains(tab, "SN2") || !strings.Contains(tab, "10.6.5.0/24") {
		t.Fatal("router ONUs tab missing the device or the subnet")
	}
	if !strings.Contains(tab, "ftn: onu") {
		t.Fatal("the paste snippet is missing")
	}
	if !strings.Contains(a.adminGet(t, "/admin/onus"), "SN2") {
		t.Fatal("global list")
	}
}

func TestONUInventoryFlowsOnWeb(t *testing.T) {
	a := newONUApp(t)
	// register an ONU, then make a house from it without typing the MAC
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEGC7777"}, "gpon_serial": {"ZTEG-7777"}, "mac": {"AA:BB:CC:DD:EE:77"}, "model": {"F670L"}})
	var eqID string
	a.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Serial == "ZTEGC7777" {
				eqID = e.ID
				if e.GPONSerial != "ZTEG-7777" {
					t.Errorf("GPON serial %q", e.GPONSerial)
				}
			}
		}
	})
	if eqID == "" {
		t.Fatal("not saved")
	}
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 7"}, "equipment_id": {eqID}})
	acct := a.account(t, "Flat 7")
	if !strings.EqualFold(acct.MAC, "AA:BB:CC:DD:EE:77") {
		t.Fatalf("MAC from the pair: %q", acct.MAC)
	}
	// the box reports itself: linked by serial
	a.seedONU(t, "onu7", "ZTEGC7777", "", "AA:BB:CC:DD:EE:99")
	a.acs.AutoLinkSite(a.site)
	page := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	if !strings.Contains(page, "ZTEGC7777") {
		t.Fatal("not linked by serial")
	}
	if !strings.Contains(page, "not the one in the register") || !strings.Contains(page, "AA:BB:CC:DD:EE:99") {
		t.Fatal("the MAC mismatch warning is missing")
	}
	if l := a.adminPost(t, "/admin/onus/usemac", url.Values{"account": {acct.ID}, "mac": {"AA:BB:CC:DD:EE:99"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	if got := a.account(t, "Flat 7"); !strings.EqualFold(got.MAC, "AA:BB:CC:DD:EE:99") {
		t.Fatalf("MAC not switched: %s", got.MAC)
	}
	// swap to a second ONU
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEGC8888"}, "mac": {"AA:BB:CC:DD:EE:88"}})
	var eq2 string
	a.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Serial == "ZTEGC8888" {
				eq2 = e.ID
			}
		}
	})
	if l := a.adminPost(t, "/admin/accounts/swaponu", url.Values{"id": {acct.ID}, "equipment": {eq2}, "old": {"faulty"}}); !strings.Contains(l, "swapped") {
		t.Fatal(l)
	}
	if got := a.account(t, "Flat 7"); !strings.EqualFold(got.MAC, "AA:BB:CC:DD:EE:88") {
		t.Fatalf("MAC after swap: %s", got.MAC)
	}
	// add an existing house's ONU to the register from the ONU list
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 8"}, "mac": {"AA:BB:CC:DD:EE:08"}})
	a.seedONU(t, "onu8", "LEGACY8", "", "AA:BB:CC:DD:EE:08")
	a.acs.AutoLinkSite(a.site)
	if l := a.adminPost(t, "/admin/onus/inventory", url.Values{"onu": {"onu8"}, "mac": {"AA:BB:CC:DD:EE:08"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	a.st.View(func(d *store.Data) {
		e, ok := d.ONTBySerial("LEGACY8")
		if !ok || e.Status != store.EqInstalled {
			t.Fatalf("not registered and installed: %+v", e)
		}
	})
}

func TestONUQuarantineApproveBlockOnWeb(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onuq", "SNQ1", "", "AA:BB:CC:DD:EE:31")
	q := func(id string) {
		a.st.Update(func(d *store.Data) error {
			o := d.ONUs[id]
			o.Sec.Quarantine = store.ONUQuarantine{Active: true, Since: time.Now(), LastAt: time.Now(), Count: 1, Reason: "another place", SiteID: a.site, IP: "10.6.5.77", MAC: "AA:BB:CC:00:00:77"}
			d.ONUs[id] = o
			return nil
		})
	}
	q("onuq")
	page := a.adminGet(t, "/admin/onus")
	if !strings.Contains(page, "checked in from an unexpected place") || !strings.Contains(page, "Approve") || !strings.Contains(page, "Block") {
		t.Fatal("the quarantine card with Approve / Block is missing")
	}
	if l := a.adminPost(t, "/admin/onus/approve", url.Values{"onu": {"onuq"}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	a.st.View(func(d *store.Data) {
		o := d.ONUs["onuq"]
		if o.Sec.Quarantine.Active || o.MgmtIP != "10.6.5.77" || o.Sec.MgmtMAC != "AA:BB:CC:00:00:77" {
			t.Fatalf("%+v", o.Sec)
		}
	})
	if l := a.adminPost(t, "/admin/onus/approve", url.Values{"onu": {"onuq"}}); !strings.Contains(l, "Error") {
		t.Fatal("approved twice")
	}
	q("onuq")
	a.adminPost(t, "/admin/onus/block", url.Values{"onu": {"onuq"}})
	a.st.View(func(d *store.Data) {
		o := d.ONUs["onuq"]
		if o.Sec.Quarantine.Active || len(o.Sec.BlockedFrom) == 0 {
			t.Fatalf("%+v", o.Sec)
		}
	})
	if !strings.Contains(a.adminGet(t, "/admin/onus"), "Unblock") {
		t.Fatal("no Unblock")
	}
}

func TestONUUseMACNeedsAVerifiedBox(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 5"}, "mac": {mac1}})
	acct := a.account(t, "Flat 5")
	a.seedONU(t, "onuv", "SNV1", acct.ID, "AA:BB:CC:DD:EE:41")
	a.st.Update(func(d *store.Data) error {
		o := d.ONUs["onuv"]
		o.Sec.ACSCred = store.CredNone
		d.ONUs["onuv"] = o
		return nil
	})
	if l := a.adminPost(t, "/admin/onus/usemac", url.Values{"account": {acct.ID}, "mac": {"AA:BB:CC:DD:EE:41"}}); !strings.Contains(l, "Error") {
		t.Fatalf("a box that has not proved itself set the house MAC: %s", l)
	}
}

func TestONUAdminPasswordRevealIsOwnerStepUp(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onua", "SNA1", "", "AA:BB:CC:DD:EE:51")
	a.st.Update(func(d *store.Data) error {
		o := d.ONUs["onua"]
		box, err := secretbox.NewLabelled(testKey, "ispbill/acs", "ispbill/acs-idx")
		if err != nil {
			t.Fatal(err)
		}
		enc, err := box.SealAAD("Secret-Admin-77", []byte("onu.admin"))
		if err != nil {
			t.Fatal(err)
		}
		o.Sec.AdminPassEnc, o.Sec.AdminPass = enc, store.AdminUnverified
		d.ONUs["onua"] = o
		return nil
	})
	if strings.Contains(a.adminGet(t, "/admin/onus"), "Secret-Admin-77") {
		t.Fatal("shown on the list")
	}
	r := a.admin("POST", "/admin/onus/adminpw", url.Values{"onu": {"onua"}, "current_password": {"wrong"}})
	if strings.Contains(body(t, r), "Secret-Admin-77") {
		t.Fatal("revealed with a wrong password")
	}
	r = a.admin("POST", "/admin/onus/adminpw", url.Values{"onu": {"onua"}, "current_password": {pw}})
	if b := body(t, r); !strings.Contains(b, "Secret-Admin-77") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("not revealed after step-up: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestONUConfirmFromAccountPage(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 6"}, "mac": {mac1}})
	acct := a.account(t, "Flat 6")
	a.seedONU(t, "onuc", "SNC1", acct.ID, "AA:BB:CC:DD:EE:61")
	a.st.Update(func(d *store.Data) error {
		d.ONUTasks["tc1"] = store.ONUTask{ID: "tc1", ONUID: "onuc", SiteID: a.site, AccountID: acct.ID, Kind: store.TaskWiFi, Status: store.TaskAccepted, CreatedAt: time.Now(), PassLeaf: "KeyPassphrase"}
		return nil
	})
	page := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	if !strings.Contains(page, "It works") || !strings.Contains(page, "Accepted by the ONU") {
		t.Fatal("the confirm buttons are missing")
	}
	if l := a.adminPost(t, "/admin/accounts/onu/confirm", url.Values{"id": {acct.ID}}); strings.Contains(l, "Error") {
		t.Fatal(l)
	}
	if !strings.Contains(a.adminGet(t, "/admin/accounts/view?code="+acct.Code), "Confirmed working") {
		t.Fatal("not confirmed")
	}
}

// The Wi-Fi card: one Wi-Fi name and password for a box with a single 2.4 GHz radio, the 5 GHz options only when the
// box has a 5 GHz radio, and the new password is a visible text field (not a masked one) so it can be checked.
func TestWiFiCardShowsFiveGHzOnlyWithA5GHzRadioAndNeverMasksTheNewPassword(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 9"}, "mac": {mac1}})
	acct := a.account(t, "Flat 9")
	a.seedONU(t, "onu1", "ZTEGC0001", acct.ID, "AA:BB:CC:DD:EE:01")
	url0 := "/admin/accounts/view?code=" + acct.Code
	page := a.adminGet(t, url0)
	if strings.Contains(page, "Same name and password on 2.4 and 5 GHz") || strings.Contains(page, "Different 5 GHz") || strings.Contains(page, `name="ssid5"`) {
		t.Fatal("a 2.4 GHz-only box shows the 5 GHz options")
	}
	if !strings.Contains(page, "one 2.4 GHz radio") || !strings.Contains(page, `name="ssid"`) {
		t.Fatal("the single-network card is missing")
	}
	check := func(page string) {
		t.Helper()
		for _, name := range []string{"password", "password5"} {
			i := strings.Index(page, `name="`+name+`"`)
			if i < 0 {
				continue
			}
			tag := page[strings.LastIndex(page[:i], "<input"):]
			tag = tag[:strings.Index(tag, ">")]
			if strings.Contains(tag, `type="password"`) {
				t.Fatalf("the %s input is masked: %s", name, tag)
			}
		}
		if !strings.Contains(page, `name="password" type="text"`) {
			t.Fatal("the new password input is not a plain text field")
		}
	}
	check(page)
	// the box gets a 5 GHz radio (a swap): the options appear
	a.st.Update(func(d *store.Data) error {
		o := d.ONUs["onu1"]
		o.WiFi = append(o.WiFi, store.ONUWiFi{Path: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.5.", Band: "5", SSID: "Old5G", Enabled: true})
		d.ONUs["onu1"] = o
		return nil
	})
	page = a.adminGet(t, url0)
	if !strings.Contains(page, "Same name and password on 2.4 and 5 GHz") || !strings.Contains(page, `name="ssid5"`) || !strings.Contains(page, `name="password5"`) {
		t.Fatal("a dual-band box lacks the 5 GHz options")
	}
	check(page)
}

func TestShowCurrentWiFiPasswordIsAuditedAndShowsTheSavedOne(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 9"}, "mac": {mac1}})
	acct := a.account(t, "Flat 9")
	a.seedONU(t, "onu1", "ZTEGC0001", acct.ID, "AA:BB:CC:DD:EE:01")
	a.st.Update(func(d *store.Data) error { // silent ONU: nothing to ask, the saved password is shown
		o := d.ONUs["onu1"]
		o.LastInform = time.Now().Add(-48 * time.Hour)
		d.ONUs["onu1"] = o
		return nil
	})
	a.adminPost(t, "/admin/accounts/wifi", url.Values{"id": {acct.ID}, "ssid": {"Home Net"}, "password": {"supersecret99"}, "same": {"1"}})
	a.acs.Wait()
	page := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	if strings.Contains(page, "supersecret99") || !strings.Contains(page, "Show current Wi-Fi password") {
		t.Fatal("the card must have the button and never print the password by itself")
	}
	r := a.admin("POST", "/admin/accounts/wifi/show", url.Values{"id": {acct.ID}})
	b := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, "supersecret99") || !strings.Contains(b, "Old2G") {
		t.Fatalf("not shown: %d %s", r.StatusCode, b)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control %q", r.Header.Get("Cache-Control"))
	}
	var audited bool
	a.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" && strings.Contains(e.Msg, "viewed the Wi-Fi password for house "+acct.Code) && e.Actor != "" {
				audited = true
			}
			if strings.Contains(e.Msg, "supersecret99") {
				t.Fatalf("the password is in the audit trail: %s", e.Msg)
			}
		}
	})
	if !audited {
		t.Fatal("no audit entry for viewing the Wi-Fi password")
	}
	// the OLT / ACS system logins keep their masking: the settings page still hides the ACS password before step-up
	_, pass, _ := a.acs.Credentials()
	if strings.Contains(a.adminGet(t, "/admin/settings/onu"), pass) {
		t.Fatal("a system password is shown without step-up")
	}
}

// The ONU card links the parameter tree (count, time, download) once it has been walked, and the download is audited and
// never carries a passphrase.
func TestONUCardLinksTheParameterTreeAndTheDownloadIsAudited(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 9"}, "mac": {mac1}})
	acct := a.account(t, "Flat 9")
	a.seedONU(t, "onu1", "ZTEGC0001", acct.ID, "AA:BB:CC:DD:EE:01")
	page := a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	if !strings.Contains(page, "has not been walked yet") || strings.Contains(page, "/admin/onus/paramtree") {
		t.Fatal("the card should say the tree is not walked yet")
	}
	root := store.RootTR098
	txt, n := store.EncodeTree([]store.TreeParam{{Path: root + "DeviceInfo.UpTime", Value: "5"},
		{Path: root + "LANDevice.1.WLANConfiguration.1.PreSharedKey.1.KeyPassphrase", Secret: true, Value: "supersecret99"}})
	a.st.Update(func(d *store.Data) error {
		d.ONUTrees["onu1"] = store.ONUTree{ID: "onu1", WalkedAt: time.Now(), Root: root, Reason: "first", Count: n, Text: txt,
			Resolved: store.TreeResolved{Paths: map[string][]string{"hosts.table": {root + "LANDevice.1.Hosts.Host."}}, Gaps: []string{"optical: no PON or optical object found in the parameter tree"}}}
		return nil
	})
	page = a.adminGet(t, "/admin/accounts/view?code="+acct.Code)
	for _, want := range []string{"Parameter tree (2 params, walked", "/admin/onus/paramtree?onu=onu1", "hosts.table", "optical: no PON or optical object"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the card lacks %q", want)
		}
	}
	if strings.Contains(page, "supersecret99") || strings.Contains(page, "onclick=") {
		t.Fatal("secret or inline script on the card")
	}
	r := a.admin("GET", "/admin/onus/paramtree?onu=onu1", nil)
	b := body(t, r)
	if r.StatusCode != 200 || strings.Contains(b, "supersecret99") || !strings.Contains(b, "KeyPassphrase\tro\t<redacted>") || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %q %s", r.StatusCode, r.Header.Get("Content-Disposition"), b)
	}
	if r.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("headers: %v", r.Header)
	}
	var audited bool
	a.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" && strings.Contains(e.Msg, "parameter tree downloaded by") {
				audited = true
			}
		}
	})
	if !audited {
		t.Fatal("the download was not audited")
	}
	if nf := a.admin("GET", "/admin/onus/paramtree?onu=nope", nil); nf.StatusCode != 404 {
		t.Fatalf("unknown ONU: %d", nf.StatusCode)
	}
}
