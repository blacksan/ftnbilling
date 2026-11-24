package acs

import (
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func (f *fixture) acceptedWiFi(c *cpe) func() bool {
	return func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskAccepted }
}

func allTriedContain(t *testing.T, c *cpe, wantSSID bool) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.tried {
		hasSSID, hasPass := false, false
		for k, v := range m {
			if strings.HasSuffix(k, ".SSID") && v != "" && !strings.HasPrefix(v, "ZTE") {
				hasSSID = true
			}
			if strings.Contains(strings.ToLower(k), "passphrase") && !strings.Contains(k, "ManagementServer") {
				hasPass = true
			}
		}
		if !usesName(m) {
			continue // a Set that only re-applies the security mode
		}
		if (hasSSID != hasPass) || (wantSSID && !hasSSID && len(m) > 0 && usesWiFi(m)) {
			t.Fatalf("a Set dropped the name or the passphrase: %v", m)
		}
	}
}

func usesName(m map[string]string) bool {
	for k := range m {
		if strings.HasSuffix(k, ".SSID") || strings.Contains(strings.ToLower(k), "passphrase") || strings.HasSuffix(k, ".PreSharedKey") {
			return true
		}
	}
	return false
}

func usesWiFi(m map[string]string) bool {
	for k := range m {
		if strings.Contains(k, "WLANConfiguration") {
			return true
		}
	}
	return false
}

// M3: a CPE that does not keep the cookie logs in again for every request; the one live session of its address is its session.
func TestCookielessCPECompletesAChange(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNNOCOOKIE", "10.6.3.20")
	c.noCookie = true
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "NoCookies", Pass: "nocookie99", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	if c.get(c.wlan("1")+"SSID") != "NoCookies" {
		t.Fatal("the change did not reach the box")
	}
}

// The CPE opens its session before it answers the connection request: the ACS neither resends the request nor loses the task.
func TestEarlySessionBeforeConnectionRequestAnswer(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNEARLY", "10.6.3.20")
	setupLinked(t, c)
	c.crFromParams = true
	c.mu.Lock()
	c.crEarly = true
	c.mu.Unlock()
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "EarlyBird", Pass: "earlybird9", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	f.svc.Wait()
	if c.hits() != 1 {
		t.Fatalf("the connection request was sent %d times", c.hits())
	}
}

// Odd but real Informs: an empty OUI, a hyphenated product class, a time that is not a date.
func TestTolerantInform(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SN-ODD-1", "10.6.3.20")
	c.OUI, c.ProductClass = "", "G-1425-MA"
	body := string(c.inform("0 BOOTSTRAP"))
	body = strings.Replace(body, "2026-11-20T09:00:00Z", "0-1-1T99:61:61", 1)
	if _, err := parseMessage([]byte(body)); err != nil {
		t.Fatalf("an odd Inform did not parse: %v", err)
	}
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if o := f.onuBySerial("SN-ODD-1"); o.ID == "" || o.ProductClass != "G-1425-MA" || o.OUI != "" {
		t.Fatalf("%+v", o)
	}
}

// M6/GPN: Wi-Fi is read with a handful of requests; a box without GetParameterNames or without partial paths is still read.
func TestDiscoveryUsesPartialPathsAndFallsBack(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*cpe)
		maxGV int
	}{
		{"partial paths", func(c *cpe) {}, 6},
		{"no partial paths", func(c *cpe) { c.noPartial = true }, 40},
		{"no GetParameterNames", func(c *cpe) { c.noGPN = true }, 60},
	}
	for _, tc := range cases {
		for _, mk := range []struct {
			n string
			f func(*testing.T, *fixture, string, string) *cpe
		}{{"tr098", newZTE}, {"tr181", newTR181}} {
			t.Run(tc.name+"/"+mk.n, func(t *testing.T) {
				f := newFixture(t)
				c := mk.f(t, f, "SNDISC", "10.6.3.20")
				tc.setup(c)
				if err := c.session("0 BOOTSTRAP"); err != nil {
					t.Fatal(err)
				}
				o := f.onuBySerial("SNDISC")
				if len(o.WiFi) < 2 || primaryBand(o, "2.4") == "" || primaryBand(o, "5") == "" {
					t.Fatalf("wifi not found: %+v", o.WiFi)
				}
				if tc.name == "partial paths" && mk.n == "tr098" && c.count("GetParameterValues")+c.count("GetParameterNames") > 16 {
					t.Fatalf("too many requests for a partial-path read: gv=%d gpn=%d", c.count("GetParameterValues"), c.count("GetParameterNames"))
				}
				if c.count("GetParameterValues") > tc.maxGV+40 {
					t.Fatalf("gv=%d", c.count("GetParameterValues"))
				}
			})
		}
	}
}

func primaryBand(o store.ONU, band string) string {
	if w, ok := primary(o.WiFi, band); ok {
		return w.Path
	}
	return ""
}

// The band comes from the box's own words (standards, channels), never from the instance number.
func TestBandByDataNotByInstanceNumber(t *testing.T) {
	for _, swap := range []bool{false, true} {
		f := newFixture(t)
		c := newVSOL(t, f, "SNVSOL", "10.6.3.20", swap)
		setupLinked(t, c)
		o := f.onuBySerial("SNVSOL")
		want2, want5 := "1.", "5."
		if swap {
			want2, want5 = "5.", "1."
		}
		if p := primaryBand(o, "2.4"); !strings.HasSuffix(p, "WLANConfiguration."+want2) {
			t.Fatalf("swap=%v 2.4 GHz is %s", swap, p)
		}
		if p := primaryBand(o, "5"); !strings.HasSuffix(p, "WLANConfiguration."+want5) {
			t.Fatalf("swap=%v 5 GHz is %s", swap, p)
		}
		if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Two", Pass: "twotwotwo22", Same: false, SSID5: "Five", Pass5: "fivefive555"}); err != nil {
			t.Fatal(err)
		}
		eventually(t, "accepted", f.acceptedWiFi(c))
		if c.get(c.root+"LANDevice.1.WLANConfiguration."+want2+"SSID") != "Two" || c.get(c.root+"LANDevice.1.WLANConfiguration."+want5+"SSID") != "Five" {
			t.Fatalf("swap=%v names went to the wrong radios", swap)
		}
		if c.get(c.root+"LANDevice.1.WLANConfiguration."+want5+"PreSharedKey.1.KeyPassphrase") != "fivefive555" {
			t.Fatalf("swap=%v wrong password on 5 GHz", swap)
		}
	}
}

// A box that says nothing about its bands is not guessed at: nothing is written and the task says why.
func TestUnknownBandWritesNothing(t *testing.T) {
	f := newFixture(t)
	c := newVSOL(t, f, "SNNOBAND", "10.6.3.20", false)
	for _, n := range []string{"1", "5"} {
		p := c.root + "LANDevice.1.WLANConfiguration." + n + "."
		delete(c.params, p+"Standard")
		delete(c.params, p+"PossibleChannels")
	}
	setupLinked(t, c)
	before := len(c.tried)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Nope", Pass: "nopenopenope", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskFailed })
	tk := f.lastTask(f.onuBySerial(c.Serial).ID)
	if !strings.Contains(tk.Fault, "2.4 or 5 GHz") {
		t.Fatalf("fault %q", tk.Fault)
	}
	for _, m := range c.tried[before:] {
		if usesWiFi(m) {
			t.Fatalf("something was written to a box whose bands are unknown: %v", m)
		}
	}
}

// L7: a box with no Wi-Fi is not discovered again at every Inform.
func TestNoWiFiResultIsCached(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNSFU", "10.6.3.20")
	for k := range c.params {
		if strings.Contains(k, "WLANConfiguration") {
			delete(c.params, k)
		}
	}
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if !f.onuBySerial("SNSFU").State.NoWiFi {
		t.Fatalf("%+v", f.onuBySerial("SNSFU").State)
	}
	if err := c.session("2 PERIODIC"); err != nil { // the first full-trust session may look for an admin login once
		t.Fatal(err)
	}
	n := c.count("GetParameterNames")
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if c.count("GetParameterNames") != n {
		t.Fatalf("a Wi-Fi-less box was discovered again: %d -> %d", n, c.count("GetParameterNames"))
	}
}

// L2/task states: status 1 is "accepted, applies after a restart"; nothing is ever "applied" on the box's word alone.
func TestStatus1IsAcceptedAfterRestart(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNST1", "10.6.3.20")
	setupLinked(t, c)
	c.mu.Lock()
	c.status1 = true
	c.mu.Unlock()
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Later", Pass: "laterlater1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	tv, _ := f.svc.TaskStatus("a1")
	if tv.Status != "accepted" || !tv.AfterReboot || !strings.Contains(tv.StatusText, "restart") || tv.CanConfirm != true {
		t.Fatalf("%+v", tv)
	}
}

// The owner's "It works" teaches the per-model table: the next box of that model tries the confirmed name first.
func TestConfirmTeachesTheModelTable(t *testing.T) {
	mk := func(f *fixture, serial, ip string) *cpe {
		c := newZTE(t, f, serial, ip)
		for _, w := range []string{"1", "2", "5"} { // this model only knows the plain KeyPassphrase
			p := c.wlan(w)
			delete(c.params, p+"PreSharedKey.1.KeyPassphrase")
			c.set(p+"KeyPassphrase", "x")
		}
		c.finish(f, ip)
		return c
	}
	f := newFixture(t)
	a := mk(f, "SNMODEL1", "10.6.3.20")
	setupLinked(t, a)
	base := a.count("SetParameterValues")
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "ModelA", Pass: "modelapass1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(a))
	if n := a.count("SetParameterValues") - base; n != 4 { // each band: the first name refused, the second taken
		t.Fatalf("expected 4 Sets without a table, got %d", n)
	}
	if err := f.svc.ConfirmWiFi("owner", "", "a1"); err != nil {
		t.Fatal(err)
	}
	if tv, _ := f.svc.TaskStatus("a1"); tv.Status != "confirmed" {
		t.Fatalf("%+v", tv)
	}
	if err := f.svc.ConfirmWiFi("owner", "", "a1"); err == nil {
		t.Fatal("confirmed twice")
	}
	if w := f.svc.Works(); len(w) != 1 || w[0].PassLeaf != "KeyPassphrase" || w[0].Count != 1 {
		t.Fatalf("%+v", w)
	}
	// another box of the same model, another house
	f.st.Update(func(d *store.Data) error {
		x := d.Accounts["a2"]
		x.MAC = "AA:BB:CC:00:11:77"
		d.Accounts["a2"] = x
		return nil
	})
	b := mk(f, "SNMODEL2", "10.6.3.21")
	b.set(b.root+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.MACAddress", "AA:BB:CC:00:11:77")
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := b.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	b.listen()
	cu, cp := b.get(b.root+"ManagementServer.ConnectionRequestUsername"), b.get(b.root+"ManagementServer.ConnectionRequestPassword")
	b.mu.Lock()
	b.crUser, b.crPass = cu, cp
	b.mu.Unlock()
	base = b.count("SetParameterValues")
	if _, err := f.svc.SetWiFi("owner", "", "a2", WiFiInput{SSID: "ModelB", Pass: "modelbpass1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(b))
	if n := b.count("SetParameterValues") - base; n != 2 {
		t.Fatalf("the confirmed name was not tried first: %d Sets", n)
	}
}

// A box that accepted a name that changed nothing: "It didn't work" tries the next name and does not trust the first again.
func TestRejectTriesAnotherName(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNFALSE", "10.6.3.20")
	c.set(c.wlan("1")+"KeyPassphrase", "x") // both names exist; the first is a decoy
	c.set(c.wlan("5")+"KeyPassphrase", "x")
	c.finish(f, "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Decoy", Pass: "decoypass11", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	settle(f, c) // the session that sent the first name ends before the owner says it did not work
	if _, err := f.svc.RejectWiFi("owner", "", "a1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second try accepted", func() bool {
		ok := false
		id := f.onuBySerial(c.Serial).ID // not inside the View below: a nested read lock deadlocks behind a waiting writer
		f.st.View(func(d *store.Data) {
			for _, tk := range d.ONUTasksOf(id) {
				if tk.Status == store.TaskAccepted && tk.PassLeaf == "KeyPassphrase" {
					ok = true
				}
			}
		})
		return ok
	})
	if o := f.onuBySerial(c.Serial); len(o.Profile.BadLeaves) != 1 || o.Profile.BadLeaves[0] != "PreSharedKey.1.KeyPassphrase" {
		t.Fatalf("%+v", o.Profile)
	}
}

// M2: the name and the passphrase are never dropped from a Set, and a network that cannot be made WPA2 is Failed.
func TestSecurityReadBackFailsAnOpenNetwork(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNOPEN", "10.6.3.20")
	for _, w := range []string{"1", "2", "5"} {
		for _, l := range []string{"BeaconType", "IEEE11iAuthenticationMode", "IEEE11iEncryptionModes"} {
			c.readonly[c.wlan(w)+l] = true
		}
	}
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "WasOpen", Pass: "wasopen1234", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskFailed })
	tk := f.lastTask(f.onuBySerial(c.Serial).ID)
	if !strings.Contains(tk.Fault, "not protected with WPA2") {
		t.Fatalf("fault %q", tk.Fault)
	}
	allTriedContain(t, c, true)
	if v, _ := f.svc.TaskStatus("a1"); v.CanConfirm {
		t.Fatal("an open network can be confirmed")
	}
}

// ...and a box that takes the security settings is read back as WPA2 and the task is accepted.
func TestSecurityReadBackAccepts(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNWPA", "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Secure1", Pass: "securepass12", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	if c.get(c.wlan("1")+"BeaconType") != "11i" || tkFault(f, c) != "" {
		t.Fatalf("beacon %q fault %q", c.get(c.wlan("1")+"BeaconType"), tkFault(f, c))
	}
	allTriedContain(t, c, true)
	// TR-181 reads ModeEnabled
	g := newFixture(t)
	d := newTR181(t, g, "SNWPA181", "10.6.3.20")
	setupLinked(t, d)
	if _, err := g.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Secure2", Pass: "securepass34", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted 181", g.acceptedWiFi(d))
	if d.get("Device.WiFi.AccessPoint.1.Security.ModeEnabled") != "WPA2-Personal" {
		t.Fatal("mode not set")
	}
}

func tkFault(f *fixture, c *cpe) string { return f.lastTask(f.onuBySerial(c.Serial).ID).Fault }

// The name is never dropped to get past a refusal: a box that refuses the name fails the task.
func TestRefusedNameIsNotDropped(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNSSID", "10.6.3.20")
	setupLinked(t, c)
	c.refuse = func(n string) bool { return strings.HasSuffix(n, "WLANConfiguration.1.SSID") }
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Refused", Pass: "refusedpass1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskFailed })
	allTriedContain(t, c, true)
	for _, m := range c.tried {
		for k, v := range m {
			if v == "refusedpass1" && strings.Contains(k, "WLANConfiguration.1.") {
				if _, ok := m[c.wlan("1")+"SSID"]; !ok {
					t.Fatalf("the passphrase went without the name: %v", m)
				}
			}
		}
	}
}

// L4: a box that echoes the value in its fault text never gets that text shown.
func TestFaultTextNeverEchoesThePassword(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNECHO", "10.6.3.20")
	setupLinked(t, c)
	c.mu.Lock()
	c.echoValue, c.refuseCode = true, 9007
	c.refuse = func(n string) bool { return strings.Contains(n, "KeyPassphrase") }
	c.mu.Unlock()
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "EchoNet", Pass: "echo-secret-99", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskFailed })
	tk := f.lastTask(f.onuBySerial(c.Serial).ID)
	if strings.Contains(tk.Fault, "echo-secret-99") || strings.Contains(strings.ToLower(tk.Fault), "bad value") {
		t.Fatalf("the password is in the task text: %q", tk.Fault)
	}
	if v, _ := f.svc.TaskStatus("a1"); strings.Contains(fmtAny(v), "echo-secret-99") {
		t.Fatal("the view carries it")
	}
	if strings.Contains(f.logs.String(), "echo-secret-99") {
		t.Fatal("logs carry it")
	}
	// the redaction also works on a top-level text that echoes it without naming a parameter
	ft := (&Fault{Code: 9003, String: "value Echo-Secret-99 rejected"}).SafeText("echo-secret-99")
	if strings.Contains(strings.ToLower(ft), "secret") {
		t.Fatalf("%q", ft)
	}
}

// M4: queued tasks expire (reboot 1 h, Wi-Fi 24 h, factory reset 7 days) and are never sent afterwards.
func TestQueuedTasksExpire(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNEXP", "10.6.3.20")
	setupLinked(t, c)
	c.crFromParams = true
	// the box is unreachable: no connection-request listener answers (the stored URL points nowhere useful)
	o := f.onuBySerial(c.Serial)
	var wifi, reboot, reset store.ONUTask
	queueReset := func() {
		f.st.Update(func(d *store.Data) error {
			reset = d.QueueONUTask(d.ONUs[o.ID], store.TaskFactoryReset, "owner", f.now)
			return nil
		})
	}
	if err := f.st.Update(func(d *store.Data) error {
		reboot = d.QueueONUTask(d.ONUs[o.ID], store.TaskReboot, "owner", f.now)
		wifi = d.QueueONUTask(d.ONUs[o.ID], store.TaskWiFi, "owner", f.now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := func(id string) string {
		var s string
		f.st.View(func(d *store.Data) { s = d.ONUTasks[id].Status })
		return s
	}
	f.advance(90 * time.Minute)
	f.svc.Upkeep()
	if state(reboot.ID) != store.TaskExpired || state(wifi.ID) != store.TaskPending {
		t.Fatalf("after 90 min: %s %s", state(reboot.ID), state(wifi.ID))
	}
	f.advance(24 * time.Hour)
	f.svc.Upkeep()
	if state(wifi.ID) != store.TaskExpired {
		t.Fatalf("after a day: %s", state(wifi.ID))
	}
	// an expired task is never sent when the box comes back
	rb := c.count("Reboot")
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if c.count("Reboot") != rb {
		t.Fatal("an expired reboot was sent")
	}
	queueReset()
	f.advance(6 * 24 * time.Hour)
	f.svc.Upkeep()
	if state(reset.ID) != store.TaskPending {
		t.Fatalf("a factory reset must outlive six days: %s", state(reset.ID))
	}
	f.advance(25 * time.Hour)
	f.svc.Upkeep()
	if state(reset.ID) != store.TaskExpired {
		t.Fatalf("factory reset after a week: %s", state(reset.ID))
	}
	if v, _ := f.svc.OfAccount("a1"); v.LastTask == nil || v.LastTask.Status == "" {
		t.Fatal("no last task in the view")
	}
	var tv TaskView
	f.st.View(func(d *store.Data) { tv = taskView(d.ONUTasks[reboot.ID]) })
	if tv.Status != "expired" || !strings.Contains(tv.StatusText, "Expired") {
		t.Fatalf("%+v", tv)
	}
}

// A connection request answered 401: the login is written to the box again at its next Inform.
func TestConnectionRequestLoginIsRewrittenAfter401(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNCR401", "10.6.3.20")
	setupLinked(t, c)
	c.crFromParams = true
	ms := c.root + "ManagementServer."
	stored := c.get(ms + "ConnectionRequestUsername")
	c.mu.Lock()
	c.params[ms+"ConnectionRequestUsername"] = "forgotten" // the box lost our login without telling anyone
	c.mu.Unlock()
	if err := f.svc.ConnectionRequest(t.Context(), f.onuBySerial(c.Serial).ID); err == nil {
		t.Fatal("the request should have been refused")
	}
	if !f.onuBySerial(c.Serial).State.CRRefused {
		t.Fatal("not noted")
	}
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if got := c.get(ms + "ConnectionRequestUsername"); got != stored {
		t.Fatalf("login not written again: %q want %q", got, stored)
	}
	if f.onuBySerial(c.Serial).State.CRRefused {
		t.Fatal("still flagged")
	}
	if err := f.svc.ConnectionRequest(t.Context(), f.onuBySerial(c.Serial).ID); err != nil {
		t.Fatalf("the request after the rewrite: %v", err)
	}
}

// A box that does not let the ACS write the connection-request login keeps what it has, and is not asked at every Inform.
func TestConnectionRequestLoginNonWritableIsSkipped(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNCRRO", "10.6.3.20")
	ms := c.root + "ManagementServer."
	c.readonly[ms+"ConnectionRequestUsername"] = true
	c.readonly[ms+"ConnectionRequestPassword"] = true
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	o := f.onuBySerial(c.Serial)
	if !o.State.CRReadOnly || c.get(ms+"PeriodicInformEnable") != "1" {
		t.Fatalf("%+v periodic=%s", o.State, c.get(ms+"PeriodicInformEnable"))
	}
	n := c.count("SetParameterValues")
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if c.count("SetParameterValues") != n {
		t.Fatal("a non-writable login is asked again at every Inform")
	}
}

// A task is never reported as "applied": a Wi-Fi change ends at accepted, a reboot at done.
func TestWiFiTaskNeverEndsAsDone(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNDONE", "10.6.3.20")
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "NotDone", Pass: "notdone1234", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	f.st.View(func(d *store.Data) {
		for _, tk := range d.ONUTasks {
			if tk.Kind == store.TaskWiFi && tk.Status == store.TaskDone {
				t.Fatal("a Wi-Fi task ended as done")
			}
		}
	})
}

// The bench V-SOL V2804N-Z is 2.4 GHz only and has SSID1 to SSID4, all 2.4 GHz (2 to 4 off). The house's Wi-Fi goes
// to SSID1 only; nothing is written for a 5 GHz band it does not have, and the other SSIDs are left alone.
func TestVSOLTwoPointFourOnlyWithFourSSIDs(t *testing.T) {
	f := newFixture(t)
	c := newVSOL(t, f, "VSOL00127E0F", "10.6.3.20", false)
	r := c.root + "LANDevice.1.WLANConfiguration."
	c.mu.Lock()
	for k := range c.params {
		if strings.HasPrefix(k, r+"5.") {
			delete(c.params, k)
		}
	}
	c.mu.Unlock()
	for _, n := range []string{"2", "3", "4"} {
		p := r + n + "."
		c.set(p+"SSID", "VSOL-2G-"+n)
		c.set(p+"Enable", "0")
		c.set(p+"Channel", "0")
		c.set(p+"Standard", "b,g,n")
		c.set(p+"PossibleChannels", "1-13")
		c.set(p+"BeaconType", "None")
		c.set(p+"IEEE11iAuthenticationMode", "None")
		c.set(p+"IEEE11iEncryptionModes", "None")
		c.set(p+"PreSharedKey.1.KeyPassphrase", "factorypass")
	}
	setupLinked(t, c)
	o := f.onuBySerial("VSOL00127E0F")
	if p := primaryBand(o, "2.4"); !strings.HasSuffix(p, "WLANConfiguration.1.") {
		t.Fatalf("2.4 GHz is %q (%+v)", p, o.WiFi)
	}
	if p := primaryBand(o, "5"); p != "" {
		t.Fatalf("a 5 GHz band was found on a 2.4 GHz-only box: %s", p)
	}
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "House24", Pass: "house24pass", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", f.acceptedWiFi(c))
	if c.get(r+"1.SSID") != "House24" || c.get(r+"1.PreSharedKey.1.KeyPassphrase") != "house24pass" {
		t.Fatal("SSID1 not set")
	}
	for _, n := range []string{"2", "3", "4"} {
		if c.get(r+n+".SSID") != "VSOL-2G-"+n || c.get(r+n+".Enable") != "0" {
			t.Fatalf("SSID%s was touched", n)
		}
	}
}
