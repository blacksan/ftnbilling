package acs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

// linkedWithOwnLogin brings a fake box up the way production does: first contact, then a second session on its own
// login; the house has a Wi-Fi choice that has been accepted by the box.
func linkedWithOwnLogin(t *testing.T, f *fixture, serial, ip string) *cpe {
	t.Helper()
	c := newZTE(t, f, serial, ip)
	setupLinked(t, c)
	if o := f.onuBySerial(serial); o.Sec.ACSCred != store.CredDevice || o.Sec.MgmtMAC != c.mgmtMAC {
		t.Fatalf("not bound with its own login: %+v", o.Sec)
	}
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "HouseNet", Pass: "housepass99", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", func() bool { return f.lastTask(f.onuBySerial(serial).ID).Status == store.TaskAccepted })
	return c
}

func (c *cpe) setCount() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.sets) }

// H1: a tenant who knows the shared login and a victim's serial gets nothing: no Wi-Fi, no credentials, no task; the
// victim's record stays where it was; the owner is told.
func TestSpoofedSerialWithSharedLoginIsQuarantined(t *testing.T) {
	for _, tc := range []struct{ name, ip string }{{"other address of the same router", "10.6.3.99"}, {"other router", "10.6.4.50"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := linkedWithOwnLogin(t, f, "SNVICTIM", "10.6.3.20")
			victim := f.onuBySerial("SNVICTIM")
			a := newZTE(t, f, "SNVICTIM", tc.ip) // the attacker's box: shared login only
			for i := 0; i < 2; i++ {             // a bootstrap, then a second inform from the same place
				ev := "2 PERIODIC"
				if i == 0 {
					ev = "0 BOOTSTRAP"
				}
				if err := a.session(ev); err != nil {
					t.Fatal(err)
				}
			}
			if a.setCount() != 0 || a.count("GetParameterNames") != 0 || a.count("GetParameterValues") != 0 {
				t.Fatalf("the attacker got more than an InformResponse: %v", a.rpcs)
			}
			got := f.onuBySerial("SNVICTIM")
			if got.SiteID != victim.SiteID || got.MgmtIP != victim.MgmtIP || got.Sec.MgmtMAC != victim.Sec.MgmtMAC || got.AccountID != "a1" {
				t.Fatalf("the victim's record moved: %+v", got)
			}
			if !got.Sec.Quarantine.Active || got.Sec.Quarantine.IP != tc.ip || got.Sec.Quarantine.Count != 2 {
				t.Fatalf("not quarantined: %+v", got.Sec.Quarantine)
			}
			if f.alertCount() != 1 || !strings.Contains(f.alerts[0], "SNVICTIM") || !strings.Contains(f.alerts[0], "approve or block") && !strings.Contains(f.alerts[0], "Approve or block") {
				t.Fatalf("alerts %v", f.alerts)
			}
			v, _ := f.svc.OfAccount("a1")
			if v.Quarantine == nil || len(v.UseMACs) != 0 {
				t.Fatalf("view: %+v", v.Quarantine)
			}
			// the real box is unaffected: its own login still works, and a Wi-Fi change still reaches it
			if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Second", Pass: "secondpass1", Same: true}); err != nil {
				t.Fatal(err)
			}
			eventually(t, "second change", func() bool { return c.get(c.wlan("1")+"SSID") == "Second" })
			if a.setCount() != 0 {
				t.Fatal("a Set went to the attacker")
			}
		})
	}
}

// A known serial on the shared login from the same router whose address is leased to another MAC is not the box.
func TestSharedLoginFromAddressLeasedToAnotherMACIsQuarantined(t *testing.T) {
	f := newFixture(t)
	c := linkedWithOwnLogin(t, f, "SNLEASE", "10.6.3.20")
	c.factoryReset() // back on the shared login...
	f.setLease("10.6.3.20", "02:00:0A:FF:FF:FF")
	n := c.setCount()
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if c.setCount() != n || !f.onuBySerial("SNLEASE").Sec.Quarantine.Active {
		t.Fatalf("expected quarantine: %+v", f.onuBySerial("SNLEASE").Sec.Quarantine)
	}
}

// A legitimate factory reset: the box is back on the shared login (the OLT pushed its profile), it comes from the same
// router and its address is leased to the recorded MAC: the house's Wi-Fi, its own login and the connection-request
// login are restored in that session.
func TestLegitFactoryResetIsRestored(t *testing.T) {
	for _, ev := range []string{"0 BOOTSTRAP", "1 BOOT"} { // some boxes send only a boot after a reset
		t.Run(ev, func(t *testing.T) {
			f := newFixture(t)
			c := linkedWithOwnLogin(t, f, "SNRESET", "10.6.3.20")
			c.factoryReset()
			if err := c.session(ev); err != nil {
				t.Fatal(err)
			}
			if c.get(c.wlan("1")+"SSID") != "HouseNet" || c.get(c.wlan("5")+"PreSharedKey.1.KeyPassphrase") != "housepass99" {
				t.Fatal("the house's Wi-Fi was not restored")
			}
			ms := c.root + "ManagementServer."
			if c.get(ms+"PeriodicInformEnable") != "1" || c.get(ms+"ConnectionRequestUsername") == "admin" || c.get(ms+"ConnectionRequestUsername") == "" {
				t.Fatal("periodic inform or the connection-request login not restored")
			}
			o := f.onuBySerial("SNRESET")
			if o.Sec.Quarantine.Active || o.Sec.ACSCred != store.CredPending || c.nextUser == "" {
				t.Fatalf("own login not re-set: %+v nextUser=%q", o.Sec, c.nextUser)
			}
			if f.alertCount() != 0 {
				t.Fatalf("a legitimate reset raised an alert: %v", f.alerts)
			}
			// and it logs in with the new login next time
			if err := c.session("2 PERIODIC"); err != nil {
				t.Fatal(err)
			}
			if f.onuBySerial("SNRESET").Sec.ACSCred != store.CredDevice {
				t.Fatal("the new login was not used")
			}
		})
	}
}

// Reset detection by ParameterKey: a box that kept its own login but reports another (empty) key forgot what was written.
func TestResetDetectedByParameterKey(t *testing.T) {
	f := newFixture(t)
	c := linkedWithOwnLogin(t, f, "SNKEY", "10.6.3.20")
	if !f.onuBySerial("SNKEY").State.KeyWorks {
		t.Fatalf("the box was not seen reporting the key: %+v", f.onuBySerial("SNKEY").State)
	}
	// a plain boot with the key intact: nothing is written
	n := c.setCount()
	if err := c.session("1 BOOT"); err != nil {
		t.Fatal(err)
	}
	if c.setCount() != n {
		t.Fatal("a boot with a matching key wrote to the box")
	}
	// the key is gone (settings reset, login kept), and no bootstrap is sent
	c.mu.Lock()
	c.params[c.wlan("1")+"SSID"] = "ZTE-default"
	c.params[c.root+"ManagementServer.ParameterKey"] = ""
	c.mu.Unlock()
	if err := c.session("1 BOOT"); err != nil {
		t.Fatal(err)
	}
	if c.get(c.wlan("1")+"SSID") != "HouseNet" {
		t.Fatal("a box that lost its key was not restored")
	}
}

// A box that never reports the key is not mistaken for a reset at every inform.
func TestBoxWithoutParameterKeyIsNotResetEveryTime(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNNOKEY", "10.6.3.20")
	c.noKey = true
	setupLinked(t, c)
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "Mine1", Pass: "mypassword1", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", func() bool { return f.lastTask(f.onuBySerial(c.Serial).ID).Status == store.TaskAccepted })
	c.mu.Lock()
	c.params[c.wlan("1")+"SSID"] = "Tenant"
	c.mu.Unlock()
	if err := c.session("1 BOOT"); err != nil {
		t.Fatal(err)
	}
	if c.get(c.wlan("1")+"SSID") != "Tenant" {
		t.Fatal("overwrote a tenant's change on a plain boot")
	}
}

// A box that takes the ACS user name but refuses the password must not be left with half a login: the user name and
// the password go in one Set, so a refusal changes neither and the box keeps working on the shared login.
func TestOwnLoginHalfRefusedChangesNothing(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNHALF", "10.6.3.20")
	c.readonly[c.root+"ManagementServer.Password"] = true
	for i, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if u := c.get(c.root + "ManagementServer.Username"); u != "" {
		t.Fatalf("the user name was written without its password: %q", u)
	}
	if o := f.onuBySerial("SNHALF"); o.Sec.ACSCred != store.CredShared || o.Sec.ACSUser != "" {
		t.Fatalf("%+v", o.Sec)
	}
}

// Per-device login refused: the box is flagged "shared login only" and goes on working.
func TestOwnLoginRefusedIsFlaggedAndWorks(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNSHARED", "10.6.3.20")
	c.readonly[c.root+"ManagementServer.Username"] = true
	c.readonly[c.root+"ManagementServer.Password"] = true
	for i, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	o := f.onuBySerial("SNSHARED")
	if o.Sec.ACSCred != store.CredShared || o.Sec.ACSUser != "" || !o.CRApplied {
		t.Fatalf("%+v", o.Sec)
	}
	v, _ := f.svc.OfAccount("a1")
	if !v.SharedOnly || v.Login != "shared" {
		t.Fatalf("not flagged in the view: %+v", v)
	}
	c.listen()
	cu, cp := c.get(c.root+"ManagementServer.ConnectionRequestUsername"), c.get(c.root+"ManagementServer.ConnectionRequestPassword")
	c.mu.Lock()
	c.crUser, c.crPass = cu, cp
	c.mu.Unlock()
	// the shared login with the right lease is verified, so the Wi-Fi goes through
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "WeakNet", Pass: "weakpass123", Same: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "accepted", func() bool { return f.lastTask(o.ID).Status == store.TaskAccepted })
	// and the same login from another address is not
	a := newZTE(t, f, "SNSHARED", "10.6.3.77")
	if err := a.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if a.setCount() != 0 || !f.onuBySerial("SNSHARED").Sec.Quarantine.Active {
		t.Fatal("a shared-login impostor was served")
	}
}

// When the router cannot say who holds an address, nothing is sent and nobody is accused.
func TestLeaseUnknownMeansNoSecretsAndNoAlert(t *testing.T) {
	f := newFixture(t)
	c := linkedWithOwnLogin(t, f, "SNDOWN", "10.6.3.20")
	c.factoryReset()
	f.mu.Lock()
	f.leaseDown = true
	f.mu.Unlock()
	n := c.setCount()
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if c.setCount() != n || f.alertCount() != 0 || f.onuBySerial("SNDOWN").Sec.Quarantine.Active {
		t.Fatalf("sets=%d alerts=%v q=%+v", c.setCount()-n, f.alerts, f.onuBySerial("SNDOWN").Sec.Quarantine)
	}
	f.mu.Lock()
	f.leaseDown = false
	f.mu.Unlock()
	if err := c.session("0 BOOTSTRAP"); err != nil { // the next inform, router back
		t.Fatal(err)
	}
	if c.get(c.wlan("1")+"SSID") != "HouseNet" {
		t.Fatal("not restored once the lease was readable")
	}
}

func TestApproveAndBlockQuarantine(t *testing.T) {
	f := newFixture(t)
	linkedWithOwnLogin(t, f, "SNAPP", "10.6.3.20")
	o := f.onuBySerial("SNAPP")
	moved := newZTE(t, f, "SNAPP", "10.6.3.88")
	if err := moved.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if !f.onuBySerial("SNAPP").Sec.Quarantine.Active {
		t.Fatal("not quarantined")
	}
	// block: that place is turned away from now on, and the alert is closed
	if err := f.svc.BlockONU("owner", "", o.ID); err != nil {
		t.Fatal(err)
	}
	if err := moved.session("2 PERIODIC"); err == nil {
		t.Fatal("a blocked place was answered")
	}
	if f.onuBySerial("SNAPP").Sec.Quarantine.Active || f.onuBySerial("SNAPP").MgmtIP != "10.6.3.20" {
		t.Fatalf("%+v", f.onuBySerial("SNAPP"))
	}
	if err := f.svc.UnblockONU("owner", "", o.ID); err != nil {
		t.Fatal(err)
	}
	// the owner moved the box for real: approve rebinds it to the new place
	if err := moved.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ApproveONU("owner", "", o.ID); err != nil {
		t.Fatal(err)
	}
	g := f.onuBySerial("SNAPP")
	if g.MgmtIP != "10.6.3.88" || g.Sec.MgmtMAC != moved.mgmtMAC || g.Sec.Quarantine.Active || g.Sec.ACSCred != store.CredNone {
		t.Fatalf("%+v %+v", g, g.Sec)
	}
	if err := moved.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if f.onuBySerial("SNAPP").Sec.Quarantine.Active {
		t.Fatal("the approved place is quarantined again")
	}
	if err := f.svc.ApproveONU("owner", "", o.ID); err == nil {
		t.Fatal("approved twice")
	}
}

// A serial that keeps changing address is quarantined even when it is not at a house.
func TestFlappingSerialIsQuarantined(t *testing.T) {
	f := newFixture(t)
	for i, ip := range []string{"10.6.3.30", "10.6.3.31", "10.6.3.32", "10.6.3.33", "10.6.3.34"} {
		b := newZTE(t, f, "SNFLAP", ip)
		b.set(b.root+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.MACAddress", "00:00:00:00:00:01")
		_ = b.session("2 PERIODIC")
		if i >= 3 && !f.onuBySerial("SNFLAP").Sec.Quarantine.Active {
			t.Fatalf("not quarantined after %d moves", i)
		}
	}
	if f.alertCount() != 1 {
		t.Fatalf("alerts %v", f.alerts)
	}
}

// Quarantined boxes do not take places in the router's quota.
func TestQuarantinedDoNotFillTheQuota(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.ACSMaxPerRouter = 2
	linkedWithOwnLogin(t, f, "SNQ1", "10.6.3.20")
	if err := f.st.Update(func(d *store.Data) error {
		for i, s := range []string{"SNQX", "SNQY"} {
			d.ONUs["oq"+s] = store.ONU{ID: "oq" + s, Serial: s, SiteID: "s1", LastInform: f.now, Sec: store.ONUSec{Quarantine: store.ONUQuarantine{Active: true}}}
			_ = i
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b := newZTE(t, f, "SNQ2", "10.6.3.21")
	if err := b.session("0 BOOTSTRAP"); err != nil {
		t.Fatalf("a real ONU was refused because of quarantined records: %v", err)
	}
}

// M5: unknown, unlinked or quarantined records that have not informed for a week are forgotten; a house's box never is.
func TestStaleRecordsExpire(t *testing.T) {
	f := newFixture(t)
	linkedWithOwnLogin(t, f, "SNKEEP", "10.6.3.20")
	b := newZTE(t, f, "SNSTRAY", "10.6.3.40")
	b.set(b.root+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.MACAddress", "00:00:00:00:00:02")
	if err := b.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	f.advance(6 * 24 * time.Hour)
	f.svc.Upkeep()
	if f.onuBySerial("SNSTRAY").ID == "" {
		t.Fatal("forgotten too early")
	}
	f.advance(2 * 24 * time.Hour)
	f.svc.Upkeep()
	if f.onuBySerial("SNSTRAY").ID != "" {
		t.Fatal("a stray record was kept")
	}
	if f.onuBySerial("SNKEEP").ID == "" {
		t.Fatal("a house's box was forgotten")
	}
}

// Best-effort lock of the box's own web admin: a random password, sealed, shown to the owner on request.
func TestAdminPasswordIsSetAndRevealed(t *testing.T) {
	f := newFixture(t)
	c := newZTE(t, f, "SNADM", "10.6.3.20")
	c.set(c.root+"User.1.Username", "admin")
	c.set(c.root+"User.1.Password", "admin")
	c.set(c.root+"User.2.Username", "user")
	c.set(c.root+"User.2.Password", "user")
	c.finish(f, "10.6.3.20")
	setupLinked(t, c)
	// the lock is a house secret: the first verified session after linking sets it
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	o := f.onuBySerial(c.Serial)
	if o.Sec.AdminPass != store.AdminUnverified || o.Sec.AdminPassEnc == "" {
		t.Fatalf("%+v", o.Sec)
	}
	pw, err := f.svc.AdminPassword(o.ID)
	if err != nil || len(pw) < 12 || c.get(c.root+"User.1.Password") != pw || c.get(c.root+"User.2.Password") != pw {
		t.Fatalf("pw=%q err=%v 1=%q", pw, err, c.get(c.root+"User.1.Password"))
	}
	if strings.Contains(f.logs.String(), pw) || strings.Contains(o.Sec.AdminPassEnc, pw) {
		t.Fatal("the admin password leaked")
	}
	// a box with no known place for it is marked unsupported, silently
	d := newZTE(t, f, "SNADM2", "10.6.3.21")
	setupLinkedNoWiFi(t, f, d)
	if o2 := f.onuBySerial("SNADM2"); o2.Sec.AdminPass == store.AdminUnverified {
		t.Fatalf("claimed a lock that was not set: %+v", o2.Sec)
	}
}

// setupLinkedNoWiFi: two sessions of a second box linked by the register to a2.
func setupLinkedNoWiFi(t *testing.T, f *fixture, c *cpe) {
	t.Helper()
	c.set(c.root+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.MACAddress", "AA:BB:CC:00:11:33")
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatal(err)
		}
	}
}

// L1: qop=auth is required (RFC 2069 digests have no replay protection) unless the compatibility switch is on; an
// expired nonce is answered with stale=true.
func TestDigestRequiresQopAndSaysStale(t *testing.T) {
	f := newFixture(t)
	user, pass, _ := f.svc.Credentials()
	do := func(authz string) *http.Response {
		req, _ := http.NewRequest("POST", f.acs.URL+"/", strings.NewReader("<x/>"))
		req.Header.Set("X-Test-IP", "10.6.3.20")
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	ch := do("").Header.Get("WWW-Authenticate")
	nonce := parseDigest(ch)["nonce"]
	noQop := func(n string) string {
		resp := digestResponse(user, pass, "POST", "/", n, "", "", "")
		return `Digest username="` + user + `", realm="` + realm + `", nonce="` + n + `", uri="/", response="` + resp + `"`
	}
	if r := do(noQop(nonce)); r.StatusCode != 401 {
		t.Fatalf("an RFC 2069 digest was accepted: %d", r.StatusCode)
	}
	f.svc.cfg.ACSCompatAuth = true
	if r := do(noQop(nonce)); r.StatusCode == 401 {
		t.Fatal("the compatibility switch does not allow RFC 2069 digests")
	}
	f.svc.cfg.ACSCompatAuth = false
	// a good qop=auth answer on a nonce that has expired is told "stale"
	good := func(n string) string {
		resp := digestResponse(user, pass, "POST", "/", n, "00000001", "cn", "auth")
		return `Digest username="` + user + `", realm="` + realm + `", nonce="` + n + `", uri="/", response="` + resp + `", qop=auth, nc=00000001, cnonce="cn"`
	}
	f.advance(10 * time.Minute)
	r := do(good(nonce))
	if r.StatusCode != 401 || !strings.Contains(r.Header.Get("WWW-Authenticate"), "stale=true") {
		t.Fatalf("expired nonce: %d %q", r.StatusCode, r.Header.Get("WWW-Authenticate"))
	}
	// a wrong password on an expired nonce is not "stale"
	bad := strings.Replace(good(nonce), "response=\"", "response=\"0", 1)
	if r := do(bad); strings.Contains(r.Header.Get("WWW-Authenticate"), "stale=true") {
		t.Fatal("stale offered to a wrong login")
	}
}

// L5: a stored ACS password that cannot be opened is never replaced silently.
func TestUnreadableACSPasswordIsNotRegenerated(t *testing.T) {
	f := newFixture(t)
	var before string
	f.st.View(func(d *store.Data) { before = d.Settings[KeyPass].Value })
	other := make([]byte, 32)
	for i := range other {
		other[i] = 9
	}
	_, err := New(Options{Store: f.st, Cfg: f.svc.cfg, DataKey: other})
	if err == nil || !strings.Contains(err.Error(), "DATA_KEY") {
		t.Fatalf("expected a refusal naming DATA_KEY, got %v", err)
	}
	f.st.View(func(d *store.Data) {
		if d.Settings[KeyPass].Value != before {
			t.Fatal("the stored password was replaced")
		}
	})
	cfg := f.svc.cfg
	cfg.ACSResetCreds = true
	if _, err := New(Options{Store: f.st, Cfg: cfg, DataKey: other}); err != nil {
		t.Fatalf("the explicit override must make a new login: %v", err)
	}
}

var _ = httptest.NewRecorder

// H2 and M5 together: a box that left its house and then sat on a shelf for more than a week is neither forgotten (it
// still logs in with its own ACS login, which only its record knows) nor let off its factory reset.
func TestReturnedBoxKeepsItsRecordAndItsReset(t *testing.T) {
	f := newFixture(t)
	c := linkedWithOwnLogin(t, f, "SNSHELF", "10.6.3.20")
	o := f.onuBySerial("SNSHELF")
	if err := f.svc.UnlinkONU("owner", "", o.ID); err != nil {
		t.Fatal(err)
	}
	var reset store.ONUTask
	f.st.View(func(d *store.Data) {
		for _, tk := range d.ONUTasksOf(o.ID) {
			if tk.Kind == store.TaskFactoryReset {
				reset = tk
			}
		}
	})
	if reset.ID == "" {
		t.Fatal("no factory reset queued")
	}
	f.advance(10 * 24 * time.Hour)
	f.svc.Upkeep()
	if f.onuBySerial("SNSHELF").ID == "" {
		t.Fatal("a returned box was forgotten: its own ACS login would be refused for ever")
	}
	var st string
	f.st.View(func(d *store.Data) { st = d.ONUTasks[reset.ID].Status })
	if st != store.TaskPending {
		t.Fatalf("the returned box's factory reset expired: %s", st)
	}
	n := c.count("FactoryReset")
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatalf("the returned box could not log in again: %v", err)
	}
	if c.count("FactoryReset") != n+1 {
		t.Fatal("the returned box was not reset when it came back")
	}
}
