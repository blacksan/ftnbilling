package acs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestBootstrapDiscoveryAndAutoLink(t *testing.T) {
	for _, mk := range []struct {
		name string
		new  func(*testing.T, *fixture, string, string) *cpe
		root string
	}{{"tr098", newZTE, store.RootTR098}, {"tr181", newTR181, store.RootTR181}, {"huawei", newHuawei, store.RootTR098}} {
		t.Run(mk.name, func(t *testing.T) {
			f := newFixture(t)
			c := mk.new(t, f, "SN"+mk.name, "10.6.3.20")
			if err := c.session("0 BOOTSTRAP"); err != nil {
				t.Fatal(err)
			}
			o := f.onuBySerial(c.Serial)
			if o.ID == "" || o.Root != mk.root || o.SiteID != "s1" {
				t.Fatalf("onu not recorded: %+v", o)
			}
			if len(o.WiFi) < 2 {
				t.Fatalf("wifi not discovered: %+v", o.WiFi)
			}
			if o.AccountID != "a1" || o.Link != store.LinkAuto {
				t.Fatalf("auto link by WAN MAC failed: acct=%q link=%q", o.AccountID, o.Link)
			}
			if !o.CRApplied {
				t.Fatal("connection request credentials not applied")
			}
			pre := c.root + "ManagementServer."
			if c.get(pre+"PeriodicInformEnable") != "1" || c.get(pre+"PeriodicInformInterval") != "300" {
				t.Fatalf("periodic inform not set: %v %v", c.get(pre+"PeriodicInformEnable"), c.get(pre+"PeriodicInformInterval"))
			}
			if u := c.get(pre + "ConnectionRequestUsername"); u == "" || u == "admin" {
				t.Fatalf("CR username not randomised: %q", u)
			}
			if c.get(pre+"ConnectionRequestPassword") == "" || c.get(pre+"ConnectionRequestPassword") == "admin" {
				t.Fatal("CR password not randomised")
			}
		})
	}
}

func TestSourceOutsideMgmtSubnetRejected(t *testing.T) {
	f := newFixture(t)
	for _, ip := range []string{"10.6.9.20", "192.0.2.5", "10.99.0.2"} {
		c := newZTE(t, f, "SNX", ip)
		if err := c.session("0 BOOTSTRAP"); err == nil {
			t.Fatalf("%s accepted", ip)
		}
	}
	if o := f.onuBySerial("SNX"); o.ID != "" {
		t.Fatal("a device from outside was recorded")
	}
	// subnet of the other router is not this router's, but is a known one: accepted and filed under s2
	c := newZTE(t, f, "SNY", "10.6.4.20")
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if o := f.onuBySerial("SNY"); o.SiteID != "s2" {
		t.Fatalf("site %q", o.SiteID)
	}
}

func TestDigestRequiredAndWrongPassword(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("POST", f.acs.URL+"/", strings.NewReader("<x/>"))
	req.Header.Set("X-Test-IP", "10.6.3.20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Digest") {
		t.Fatalf("got %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	c := newZTE(t, f, "SNW", "10.6.3.20")
	c.acsUser, c.acsPass = "x", "wrong"
	if err := c.session("0 BOOTSTRAP"); err == nil {
		t.Fatal("wrong password accepted")
	}
	// Basic must not be accepted
	req, _ = http.NewRequest("POST", f.acs.URL+"/", strings.NewReader("<x/>"))
	req.Header.Set("X-Test-IP", "10.6.3.20")
	req.SetBasicAuth("a", "b")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("basic auth gave %d", resp.StatusCode)
	}
}

func TestDigestIndependentVector(t *testing.T) {
	got := digestResponse("u", "p", "POST", "/", "n0nce", "00000001", "cn", "auth")
	ha1 := md5hexT("u:" + realm + ":p")
	want := md5hexT(ha1 + ":n0nce:00000001:cn:auth:" + md5hexT("POST:/"))
	if got != want {
		t.Fatalf("digest %s want %s", got, want)
	}
}

func setupLinked(t *testing.T, c *cpe) store.ONU {
	t.Helper()
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	o := c.f.onuBySerial(c.Serial)
	c.listen()
	// a second periodic inform from the same address makes it "stable"
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	cu, cp := c.get(c.root+"ManagementServer.ConnectionRequestUsername"), c.get(c.root+"ManagementServer.ConnectionRequestPassword")
	c.mu.Lock()
	c.crUser, c.crPass = cu, cp
	c.mu.Unlock()
	return c.f.onuBySerial(o.Serial)
}

func TestChangeWiFiEndToEnd(t *testing.T) {
	cases := []struct {
		name string
		new  func(*testing.T, *fixture, string, string) *cpe
		pass string
	}{{"tr098-zte", newZTE, "PreSharedKey.1.KeyPassphrase"}, {"huawei", newHuawei, "X_HW_KeyPassphrase"}, {"tr181", newTR181, ""}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := tc.new(t, f, "SNC"+tc.name, "10.6.3.20")
			setupLinked(t, c)
			task, err := f.svc.SetWiFi("owner", "1.2.3.4", "a1", WiFiInput{SSID: "Home Net", Pass: "s3cretpass!", Same: true})
			if err != nil {
				t.Fatal(err)
			}
			if task.Status != store.TaskPending && task.Status != store.TaskDone {
				t.Fatalf("status %s", task.Status)
			}
			f.svc.Wait()
			eventually(t, "task applied", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskDone })
			if c.hits() == 0 {
				t.Fatal("no connection request received")
			}
			set := c.allSets()
			var ssids, passes int
			for k, v := range set {
				if strings.HasSuffix(k, "SSID") && !strings.HasSuffix(k, "SSIDReference") && v == "Home Net" {
					ssids++
				}
				if v == "s3cretpass!" {
					passes++
					if tc.pass != "" && !strings.HasSuffix(k, tc.pass) {
						t.Errorf("passphrase written to %s", k)
					}
				}
			}
			if ssids != 2 || passes != 2 {
				t.Fatalf("expected both bands set, got ssids=%d passes=%d: %v", ssids, passes, set)
			}
			tv, ok := f.svc.TaskStatus("a1")
			if !ok || tv.Status != "applied" {
				t.Fatalf("status %+v", tv)
			}
			if strings.Contains(f.logs.String(), "s3cretpass!") {
				t.Fatal("password in logs")
			}
			ov, _ := f.svc.OfAccount("a1")
			if strings.Contains(fmtAny(ov), "s3cretpass!") {
				t.Fatal("view carries the password")
			}
		})
	}
}

func fmtAny(v any) string {
	return strings.TrimSpace(strings.Join(strings.Fields(fmt.Sprintf("%+v", v)), " "))
}

func Test9005FallbackToNextCandidate(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SN9005", "10.6.3.20")
	// this unit rejects the ZTE name but knows the generic one
	c.refuse = func(n string) bool { return false }
	for _, w := range []string{"1", "2", "5"} {
		p := c.wlan(w)
		delete(c.params, p+"PreSharedKey.1.KeyPassphrase")
		c.set(p+"KeyPassphrase", "x")
	}
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Net9005", Pass: "password99", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "applied", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskDone })
	if got := c.get(c.wlan("1") + "KeyPassphrase"); got != "password99" {
		t.Fatalf("fallback path not used, value %q", got)
	}
	if c.count("SetParameterValues") < 2 {
		t.Fatal("expected a retry")
	}
}

func TestFactoryResetRestoresAndBootDoesNot(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNRST", "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Mine", Pass: "mypassword1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "applied", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskDone })
	// factory reset: the box forgets everything
	c.mu.Lock()
	for _, w := range []string{"1", "5"} {
		c.params[c.wlan(w)+"SSID"] = "ZTE-default"
		c.params[c.wlan(w)+"PreSharedKey.1.KeyPassphrase"] = "factorypass"
	}
	c.params[c.root+"ManagementServer.ConnectionRequestUsername"] = "admin"
	c.params[c.root+"ManagementServer.PeriodicInformEnable"] = "0"
	c.mu.Unlock()
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if c.get(c.wlan("1")+"SSID") != "Mine" || c.get(c.wlan("5")+"PreSharedKey.1.KeyPassphrase") != "mypassword1" {
		t.Fatal("wifi not restored after factory reset")
	}
	if c.get(c.root+"ManagementServer.PeriodicInformEnable") != "1" {
		t.Fatal("periodic inform not restored")
	}
	// someone changes it on the device, then it just reboots: no overwrite, reported SSID refreshed
	c.mu.Lock()
	c.params[c.wlan("1")+"SSID"] = "Changed on box"
	c.mu.Unlock()
	before := c.count("SetParameterValues")
	if err := c.session("1 BOOT"); err != nil {
		t.Fatal(err)
	}
	if c.get(c.wlan("1")+"SSID") != "Changed on box" {
		t.Fatal("BOOT overwrote the device")
	}
	if c.count("SetParameterValues") != before {
		t.Fatal("BOOT sent a Set")
	}
	o := f.onuBySerial(c.Serial)
	found := false
	for _, w := range o.WiFi {
		if w.SSID == "Changed on box" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reported SSIDs not refreshed: %+v", o.WiFi)
	}
}

func TestRebootQueuesRPCAndCR(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNRB", "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.RebootONU("owner", "", "a1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reboot rpc", func() bool { return c.count("Reboot") == 1 })
	eventually(t, "task done", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskDone })
}

func TestManualLinkAndUnlink(t *testing.T) {
	f := newFixture(t)
	c := newTR181(t, f, "SNML", "10.6.3.20")
	delete(c.params, "Device.Ethernet.Link.1.MACAddress")
	c.set("Device.Ethernet.Link.1.MACAddress", "00:00:00:00:00:01")
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	o := f.onuBySerial(c.Serial)
	if o.AccountID != "" {
		t.Fatal("linked without a MAC match")
	}
	if err := f.svc.LinkONU("owner", "", o.ID, "a2"); err != nil {
		t.Fatal(err)
	}
	if o = f.onuBySerial(c.Serial); o.AccountID != "a2" || o.Link != store.LinkManual {
		t.Fatalf("%+v", o)
	}
	if err := f.svc.UnlinkONU("owner", "", o.ID); err != nil {
		t.Fatal(err)
	}
	if o = f.onuBySerial(c.Serial); o.AccountID != "" {
		t.Fatal("still linked")
	}
}

func TestSecretsNeverLogged(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNLOG", "10.6.3.20")
	setupLinked(t, c)
	_, _ = f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "LogNet", Pass: "unique-pass-77", Same: true})
	f.svc.Wait()
	_, p, _ := f.svc.Credentials()
	out := f.logs.String()
	for _, s := range []string{"unique-pass-77", p, c.get(c.root + "ManagementServer.ConnectionRequestPassword")} {
		if s != "" && strings.Contains(out, s) {
			t.Fatalf("secret %q in logs", s)
		}
	}
}

func TestPerRouterCap(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.ACSMaxPerRouter = 1
	a := newZTE(t, f, "SNCAP1", "10.6.3.20")
	b := newZTE(t, f, "SNCAP2", "10.6.3.21")
	if err := a.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if err := b.session("0 BOOTSTRAP"); err == nil {
		t.Fatal("second device over the cap accepted")
	}
}

func TestXMLLimits(t *testing.T) {
	if _, err := parseMessage([]byte(`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY a "b">]><Envelope><Body/></Envelope>`)); err == nil {
		t.Fatal("DOCTYPE accepted")
	}
	big := make([]byte, maxBody+1)
	if _, err := parseMessage(big); err == nil {
		t.Fatal("oversize accepted")
	}
	deep := strings.Repeat("<a>", 100) + strings.Repeat("</a>", 100)
	if _, err := parseMessage([]byte(deep)); err == nil {
		t.Fatal("deep nesting accepted")
	}
}

func TestSensitiveNames(t *testing.T) {
	for _, n := range []string{"X.PreSharedKey.1.KeyPassphrase", "X.WPAPassword", "Y.PSK", "Z.Key"} {
		if !Sensitive(n) {
			t.Errorf("%s not sensitive", n)
		}
	}
	if Sensitive("X.SSID") {
		t.Error("SSID is sensitive")
	}
}

func TestConnReqSSRFGuard(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNSSRF", "10.6.3.20")
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	try := func(u string) (string, error) {
		f.st.Update(func(d *store.Data) error { x := d.ONUs[o.ID]; x.ConnReqURL = u; d.ONUs[o.ID] = x; return nil })
		var got string
		var err error
		f.st.View(func(d *store.Data) {
			var tu *url.URL
			tu, err = f.svc.crTarget(d, d.ONUs[o.ID])
			if err == nil {
				got = tu.Host
			}
		})
		return got, err
	}
	// hosts outside the subnet are rewritten to the address the ONU connected from, never dialled as given
	for _, u := range []string{"http://127.0.0.1:7547/cr", "http://10.99.0.1:7547/x", "http://169.254.169.254:7547/"} {
		h, err := try(u)
		if err != nil || h != "10.6.3.20:7547" {
			t.Errorf("%s -> %q %v", u, h, err)
		}
	}
	for _, u := range []string{"http://10.6.3.20:22/cr", "http://10.6.3.20:8080/cr", "ftp://10.6.3.20:7547/", "http://u:p@10.6.3.20:7547/"} {
		if h, err := try(u); err == nil {
			t.Errorf("%s allowed as %s", u, h)
		}
	}
}

func TestValueChangeMarksDesiredChanged(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNVC", "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Wanted", Pass: "wantedpass1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "applied", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskDone })
	c.mu.Lock()
	c.params[c.wlan("1")+"SSID"] = "Tenant renamed"
	c.mu.Unlock()
	if err := c.session("4 VALUE CHANGE"); err != nil {
		t.Fatal(err)
	}
	if o := f.onuBySerial(c.Serial); !o.State.WiFiChanged {
		t.Fatalf("desired not marked changed: %+v", o.State)
	}
}

func TestRunRefusesPublicListen(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.ACSListen = "203.0.113.5:7547"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.svc.Run(ctx); err == nil {
		t.Fatal("started on a public address")
	}
}

func (f *fixture) inventory(serial, gpon, mac, account string) {
	f.t.Helper()
	if err := f.st.Update(func(d *store.Data) error {
		e := store.Equipment{ID: "eq-" + serial, Kind: store.KindONT, Serial: serial, GPONSerial: gpon, MAC: mac, Status: store.EqInStock, CreatedAt: f.now}
		if account != "" {
			e.Status, e.AccountID, e.SiteID = store.EqInstalled, account, d.Accounts[account].SiteID
		}
		d.Equipment[e.ID] = e
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestInventorySerialLinksBeforeMAC(t *testing.T) {
	f := newFixture(t)
	// the register says this ONU belongs to a2; the box's WAN MAC is a1's: the register wins, and the mismatch is flagged
	f.inventory("SNINV", "", "AA:BB:CC:00:11:33", "a2")
	c := newZTE(t, f, "SNINV", "10.6.3.20")
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	o := f.onuBySerial("SNINV")
	if o.AccountID != "a2" || o.Link != store.LinkInventory {
		t.Fatalf("link %q %q", o.AccountID, o.Link)
	}
	v, ok := f.svc.OfAccount("a2")
	if !ok || !v.InInventory || len(v.Warnings) == 0 || len(v.UseMACs) == 0 {
		t.Fatalf("expected a MAC mismatch warning with a fix: %+v", v)
	}
	// the account MAC was not touched
	f.st.View(func(d *store.Data) {
		if d.Accounts["a2"].MAC != "AA:BB:CC:00:11:33" {
			t.Fatal("account MAC changed by itself")
		}
	})
}

func TestInventoryGPONSerialMatches(t *testing.T) {
	f := newFixture(t)
	f.inventory("OTHER-SERIAL", "ZTEG-1234", testAccountMAC, "a1")
	c := newZTE(t, f, "zteg1234", "10.6.3.20")
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if o := f.onuBySerial("ZTEG1234"); o.AccountID != "" {
		// "zteg1234" has no hyphen: the register's serial is ZTEG-1234, so this must NOT match
		t.Fatalf("hyphen-insensitive match is not intended: %+v", o)
	}
	c2 := newZTE(t, f, "ZTEG-1234", "10.6.3.21")
	if err := c2.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if o := f.onuBySerial("ZTEG-1234"); o.AccountID != "a1" || o.Link != store.LinkInventory {
		t.Fatalf("%+v", o)
	}
}

func TestInventoryOtherRouterNotLinkedAndWarned(t *testing.T) {
	f := newFixture(t)
	f.st.Update(func(d *store.Data) error {
		d.Accounts["b1"] = store.Account{ID: "b1", SiteID: "s2", Code: "GHJ789", Flat: "C1", Unit: "C1", MAC: "AA:BB:CC:00:22:22", CreatedAt: f.now}
		return nil
	})
	f.inventory("SNELSE", "", testAccountMAC, "b1")
	c := newZTE(t, f, "SNELSE", "10.6.3.20") // on router s1, registered to a house of s2
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	o := f.onuBySerial("SNELSE")
	if o.AccountID == "b1" {
		t.Fatal("linked across routers")
	}
	var warned bool
	for _, v := range f.svc.ONUs("s1") {
		if v.Serial == "SNELSE" && len(v.Warnings) > 0 {
			warned = true
		}
	}
	if !warned {
		t.Fatal("no warning for an ONU on the wrong router")
	}
}

func TestInventorySwapMovesLink(t *testing.T) {
	f := newFixture(t)
	f.inventory("OLDBOX", "", testAccountMAC, "a1")
	old := newZTE(t, f, "OLDBOX", "10.6.3.20")
	if err := old.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	f.st.Update(func(d *store.Data) error { // the owner swapped: new box installed, old one returned
		o := d.Equipment["eq-OLDBOX"]
		o.Status, o.AccountID = store.EqFaulty, ""
		d.Equipment[o.ID] = o
		d.Accounts["a1"] = func() store.Account { a := d.Accounts["a1"]; a.MAC = "AA:BB:CC:00:55:55"; return a }()
		return nil
	})
	f.inventory("NEWBOX", "", "AA:BB:CC:00:55:55", "a1")
	nb := newZTE(t, f, "NEWBOX", "10.6.3.21")
	nb.set(nb.root+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.MACAddress", "AA:BB:CC:00:55:55")
	if err := nb.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if o := f.onuBySerial("NEWBOX"); o.AccountID != "a1" || o.Link != store.LinkInventory {
		t.Fatalf("new box: %+v", o)
	}
	if o := f.onuBySerial("OLDBOX"); o.AccountID != "" {
		t.Fatalf("old box still linked: %+v", o)
	} else if !o.State.Returned || f.lastTask(o.ID).Kind != store.TaskFactoryReset {
		t.Fatalf("old box not marked returned with a factory reset queued: %+v %+v", o.State, f.lastTask(o.ID))
	}
}

// A Wi-Fi change made while the previous one is running gets its own task: the running one may have sent the old
// values already. A reboot still joins the active one.
func TestQueueWiFiJoinsOnlyPending(t *testing.T) {
	d := &store.Data{ONUs: map[string]store.ONU{}, ONUTasks: map[string]store.ONUTask{}}
	o := store.ONU{ID: "o1", SiteID: "s1"}
	d.ONUs[o.ID] = o
	now := time.Now()
	a := queue(d, o, store.TaskWiFi, "x", now)
	if b := queue(d, o, store.TaskWiFi, "x", now); b.ID != a.ID {
		t.Fatal("a pending Wi-Fi task must be joined")
	}
	a.Status = store.TaskRunning
	d.ONUTasks[a.ID] = a
	if b := queue(d, o, store.TaskWiFi, "x", now.Add(time.Second)); b.ID == a.ID || b.Status != store.TaskPending {
		t.Fatalf("a running Wi-Fi task must not swallow a newer change: %+v", b)
	}
	r := queue(d, o, store.TaskReboot, "x", now)
	r.Status = store.TaskRunning
	d.ONUTasks[r.ID] = r
	if b := queue(d, o, store.TaskReboot, "x", now); b.ID != r.ID {
		t.Fatal("a running reboot is joined")
	}
}

// A box that left a house gets a factory reset at its next check-in and never receives any house's Wi-Fi, not even
// when it comes back from the reset with a 0 BOOTSTRAP while the house keeps its choice.
func TestReturnedBoxGetsResetNeverWiFi(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNRET", "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "OldTenant", Pass: "oldtenantpass", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "applied", func() bool {
		return f.lastTask(f.onuBySerial(c.Serial).ID).Status != store.TaskPending && f.lastTask(f.onuBySerial(c.Serial).ID).Status != store.TaskRunning
	})
	if err := f.svc.UnlinkONU("owner", "", f.onuBySerial(c.Serial).ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "x", Pass: "xxxxxxxx"}); err == nil {
		t.Fatal("a Wi-Fi change was queued for a house with no ONU")
	}
	before := len(c.allSets())
	_ = before
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if c.count("FactoryReset") != 1 {
		t.Fatalf("factory reset not sent: %v", c.rpcs)
	}
	// it restarts from factory settings and reports a bootstrap: nothing is written to it
	c.mu.Lock()
	n := len(c.sets)
	c.mu.Unlock()
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for _, set := range c.sets[n:] {
		for k, v := range set {
			if v == "OldTenant" || v == "oldtenantpass" {
				t.Fatalf("the old house's Wi-Fi reached the returned box: %s", k)
			}
		}
	}
	c.mu.Unlock()
	if o := f.onuBySerial(c.Serial); o.AccountID != "" {
		t.Fatalf("relinked by itself: %+v", o)
	}
}
