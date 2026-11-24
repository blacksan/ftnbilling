package acs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

// strict turns ACS_REGISTERED_ONLY on for a test fixture (the fixture's config leaves it at the zero value = off).
func (f *fixture) strict(on bool) { f.svc.cfg.ACSRegisteredOnly = on }

func (f *fixture) onuCount() int {
	n := 0
	f.st.View(func(d *store.Data) { n = len(d.ONUs) })
	return n
}

// the management MAC the fake box gets for 10.6.3.20 is 02:00:0A:06:03:14
const (
	leaseMAC20 = "02:00:0A:06:03:14"
	nearMAC20  = "02:00:0A:06:03:13" // the V-SOL case: label ...0F, WAN ...10: one apart
	farMAC20   = "02:00:0A:06:03:5F"
)

// crLogin lets the fake box's connection-request server accept the login the ACS wrote.
func crLogin(c *cpe) {
	cu, cp := c.get(c.root+"ManagementServer.ConnectionRequestUsername"), c.get(c.root+"ManagementServer.ConnectionRequestPassword")
	c.mu.Lock()
	c.crUser, c.crPass = cu, cp
	c.mu.Unlock()
}

func TestMACNear(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"02:00:0A:06:03:14", "02:00:0A:06:03:13", true},
		{"02:00:0A:06:03:14", "02:00:0A:06:03:14", true},
		{"02:00:0A:06:03:14", "02:00:0A:06:03:1C", true},  // 8 apart
		{"02:00:0A:06:03:14", "02:00:0A:06:03:1D", false}, // 9 apart
		{"02:00:0A:06:03:14", "02:00:0B:06:03:14", false}, // other OUI
		{"02-00-0a-06-03-14", "02:00:0A:06:03:0C", true},
		{"02:00:0A:06:03:14", "", false},
	} {
		if got := macNear(tc.a, tc.b); got != tc.want {
			t.Errorf("macNear(%q,%q)=%v", tc.a, tc.b, got)
		}
	}
}

func TestUnregisteredSerialIsRefusedAndListed(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	c := newZTE(t, f, "SNSTRANGER", "10.6.3.20")
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	if c.setCount() != 0 || c.count("GetParameterNames") != 0 || c.count("GetParameterValues") != 0 {
		t.Fatalf("an unregistered serial got more than an InformResponse: %v", c.rpcs)
	}
	if f.onuCount() != 0 {
		t.Fatal("an unregistered serial was stored as a device")
	}
	l := f.svc.UnknownONUs("")
	if len(l) != 1 || l[0].Serial != "SNSTRANGER" || l[0].Model != "F670L" || l[0].SiteID != "s1" {
		t.Fatalf("unknown list: %+v", l)
	}
	if f.alertCount() != 1 || !strings.Contains(f.alerts[0], "SNSTRANGER") || !strings.Contains(f.alerts[0], "Block A") {
		t.Fatalf("alerts %v", f.alerts)
	}
	if len(f.svc.UnknownONUs("s2")) != 0 {
		t.Fatal("listed for the wrong router")
	}
	// the same serial again is not announced again, and an invented flood cannot grow the list without bound
	f.advance(2 * time.Minute)
	_ = c.session("2 PERIODIC")
	if f.alertCount() != 1 {
		t.Fatalf("repeated alerts: %v", f.alerts)
	}
	// once it is in the inventory it is accepted, and it leaves the list
	f.inventory("SNSTRANGER", "", nearMAC20, "")
	if len(f.svc.UnknownONUs("")) != 0 {
		t.Fatal("a registered serial is still listed as unknown")
	}
	_ = c.session("0 BOOTSTRAP")
	if f.onuBySerial("SNSTRANGER").ID == "" {
		t.Fatal("a registered serial was not accepted")
	}
}

func TestUnknownListExpiresAndIsBounded(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	c := newZTE(t, f, "SNOLD", "10.6.3.20")
	_ = c.session("0 BOOTSTRAP")
	f.advance(8 * 24 * time.Hour)
	if len(f.svc.UnknownONUs("")) != 0 {
		t.Fatal("an entry older than 7 days is still listed")
	}
	f.svc.Upkeep()
	f.st.View(func(d *store.Data) {
		if n := len(readUnknown(d)); n != 0 {
			t.Fatalf("upkeep kept %d entries", n)
		}
	})
	// a flood of invented serials: the list stays bounded and the owners get at most unknownNotifyN messages an hour
	before := f.alertCount()
	for i := 0; i < unknownMax+20; i++ {
		_ = f.st.Update(func(d *store.Data) error {
			if a, _ := f.svc.noteUnknown(d, Inform{Serial: fmt.Sprintf("FAKE%04d", i), OUI: "0019C6"}, store.Site{ID: "s1"}, "10.6.3.9", f.svc.now()); a != "" {
				f.mu.Lock()
				f.alerts = append(f.alerts, a)
				f.mu.Unlock()
			}
			return nil
		})
	}
	if n := len(f.svc.UnknownONUs("")); n != unknownMax {
		t.Fatalf("list has %d entries", n)
	}
	if n := f.alertCount() - before; n != unknownNotifyN {
		t.Fatalf("%d owner messages for a flood", n)
	}
}

func TestRegisteredSerialWithNearMACGetsTheWiFi(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNNEAR", "", nearMAC20, "a1")
	c := linkedWithOwnLogin(t, f, "SNNEAR", "10.6.3.20") // first contact, a second session, then the house's Wi-Fi is sent
	o := f.onuBySerial("SNNEAR")
	if o.ID == "" || o.Sec.MACCheck != store.MACOK || o.Sec.MgmtMAC != leaseMAC20 || o.AccountID != "a1" || o.Sec.ACSCred != store.CredDevice {
		t.Fatalf("not accepted: %+v", o.Sec)
	}
	if c.get(c.wlan("1")+"SSID") != "HouseNet" {
		t.Fatal("the house's Wi-Fi was not sent")
	}
	if f.alertCount() != 0 {
		t.Fatalf("alerts %v", f.alerts)
	}
}

func TestRegisteredSerialWithFarMACIsHeldUntilTheOwnerTrustsIt(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNFAR", "", farMAC20, "a1")
	if _, err := f.svc.SetWiFi("owner", "", "a2", WiFiInput{SSID: "x", Pass: "xxxxxxxx1", Same: true}); err == nil {
		_ = err // a2 has no ONU: refused, not needed
	}
	c := newZTE(t, f, "SNFAR", "10.6.3.20")
	c.listen()
	for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC", "2 PERIODIC"} {
		if err := c.session(ev); err != nil {
			t.Fatal(err)
		}
	}
	if c.setCount() != 0 || c.count("GetParameterValues") != 0 {
		t.Fatalf("a held box got more than an InformResponse: %v", c.rpcs)
	}
	o := f.onuBySerial("SNFAR")
	if o.ID == "" || o.Sec.MACCheck != store.MACWaiting || o.Sec.MgmtMAC != "" || o.Sec.ACSCred != store.CredNone || o.Sec.MACWaitLease != leaseMAC20 {
		t.Fatalf("not held: %+v", o.Sec)
	}
	if f.alertCount() != 1 || !strings.Contains(f.alerts[0], "SNFAR") || !strings.Contains(f.alerts[0], farMAC20) || !strings.Contains(f.alerts[0], leaseMAC20) {
		t.Fatalf("alerts %v", f.alerts)
	}
	v, ok := f.svc.OfAccount("a1")
	if !ok || v.MACWait == nil || v.MACWait.LeaseMAC != leaseMAC20 || v.MACWait.WANMAC != farMAC20 || !v.MACWait.CanTrust ||
		v.MACWait.Text != "Waiting: management MAC doesn't match the registered ONU yet" {
		t.Fatalf("view: %+v", v.MACWait)
	}
	// the house's Wi-Fi is set while the box waits: it still gets nothing
	if _, err := f.svc.SetWiFi("owner", "", "a1", WiFiInput{SSID: "HouseNet", Pass: "housepass99", Same: true}); err != nil {
		t.Fatal(err)
	}
	settle(f, c)
	_ = c.session("2 PERIODIC")
	if c.setCount() != 0 {
		t.Fatalf("Sets went to a held box: %v", c.sets)
	}
	// the owner trusts this one box, for the MAC that was shown; the next check-ins release it, and the house's Wi-Fi follows
	if err := f.svc.TrustONU("owner", "", o.ID, farMAC20); err != ErrLeaseMoved {
		t.Fatalf("trusted for a MAC that was not shown: %v", err)
	}
	if err := f.svc.TrustONU("owner", "", o.ID, strings.ToLower(leaseMAC20)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.TrustONU("owner", "", "nope", ""); err != ErrONUGone {
		t.Fatalf("got %v", err)
	}
	settle(f, c)
	_ = c.session("2 PERIODIC")
	settle(f, c)
	_ = c.session("2 PERIODIC")
	settle(f, c)
	crLogin(c)
	_ = c.session("2 PERIODIC")
	eventually(t, "wifi after the override", func() bool { return c.get(c.wlan("1")+"SSID") == "HouseNet" })
	o = f.onuBySerial("SNFAR")
	if o.Sec.MACCheck != store.MACOK || o.Sec.MgmtMAC != leaseMAC20 || o.Sec.MACTrusted != leaseMAC20 {
		t.Fatalf("not released: %+v", o.Sec)
	}
	if err := f.svc.TrustONU("owner", "", o.ID, ""); err != ErrNotWaiting {
		t.Fatalf("got %v", err)
	}
	audited := false
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" && strings.Contains(e.Msg, "trusts the management MAC") && e.Actor == "owner" {
				audited = true
			}
		}
	})
	if !audited {
		t.Fatal("the override was not audited")
	}
	// a factory reset later is let through for the same trusted MAC
	c.factoryReset()
	_ = c.session("0 BOOTSTRAP")
	if f.onuBySerial("SNFAR").Sec.MACCheck != store.MACOK {
		t.Fatal("a reset of a trusted ONU was held again")
	}
}

func TestOverrideIsForTheMACItWasHeldWith(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNMOVE", "", farMAC20, "a1")
	c := newZTE(t, f, "SNMOVE", "10.6.3.20")
	_ = c.session("0 BOOTSTRAP")
	if err := f.svc.TrustONU("owner", "", f.onuBySerial("SNMOVE").ID, ""); err != nil {
		t.Fatal(err)
	}
	f.setLease("10.6.3.20", "02:00:0A:AA:AA:AA") // another device now holds the address
	_ = c.session("2 PERIODIC")
	if o := f.onuBySerial("SNMOVE"); o.Sec.MACCheck != store.MACWaiting || o.Sec.MACWaitLease != "02:00:0A:AA:AA:AA" {
		t.Fatalf("the override moved with the address: %+v", o.Sec)
	}
}

func TestNoRegisteredWANMACNeedsTheOverride(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNNOMAC", "", "", "a1")
	c := newZTE(t, f, "SNNOMAC", "10.6.3.20")
	_ = c.session("0 BOOTSTRAP")
	_ = c.session("2 PERIODIC")
	o := f.onuBySerial("SNNOMAC")
	if o.Sec.MACCheck != store.MACWaiting || c.setCount() != 0 || !strings.Contains(o.Sec.MACWaitNote, "no WAN MAC") {
		t.Fatalf("%+v", o.Sec)
	}
	if err := f.svc.TrustONU("owner", "", o.ID, ""); err != nil {
		t.Fatal(err)
	}
}

func TestHeldWhileTheRouterHasNoLeaseThenReleased(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNLATE", "", nearMAC20, "a1")
	c := newZTE(t, f, "SNLATE", "10.6.3.20")
	f.mu.Lock()
	f.leaseDown = true
	f.mu.Unlock()
	_ = c.session("0 BOOTSTRAP")
	if o := f.onuBySerial("SNLATE"); o.Sec.MACCheck != store.MACWaiting || f.alertCount() != 0 {
		t.Fatalf("%+v alerts %v", o.Sec, f.alerts)
	}
	v, _ := f.svc.OfAccount("a1")
	if v.MACWait == nil || v.MACWait.CanTrust {
		t.Fatalf("view %+v", v.MACWait)
	}
	if err := f.svc.TrustONU("owner", "", f.onuBySerial("SNLATE").ID, ""); err != ErrNoLeaseMAC {
		t.Fatalf("got %v", err)
	}
	f.mu.Lock()
	f.leaseDown = false
	f.mu.Unlock()
	_ = c.session("2 PERIODIC")
	if o := f.onuBySerial("SNLATE"); o.Sec.MACCheck != store.MACOK {
		t.Fatalf("%+v", o.Sec)
	}
}

func TestFactoryResetWithAFarMACIsHeld(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNRESET", "", nearMAC20, "a1")
	c := linkedWithOwnLogin(t, f, "SNRESET", "10.6.3.20")
	// the inventory entry is corrected to another box's MAC after the install, then the box is reset
	f.st.Update(func(d *store.Data) error {
		e := d.Equipment["eq-SNRESET"]
		e.MAC = farMAC20
		d.Equipment[e.ID] = e
		return nil
	})
	before := c.setCount()
	c.factoryReset()
	_ = c.session("0 BOOTSTRAP")
	o := f.onuBySerial("SNRESET")
	if o.Sec.MACCheck != store.MACWaiting || c.setCount() != before || o.Sec.ACSCred != store.CredDevice {
		t.Fatalf("%+v sets %d", o.Sec, c.setCount())
	}
}

func TestGPONSerialMatchesTheRegisteredONU(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("V26051300697", "VSOL00127E0F", nearMAC20, "a1")
	c := newZTE(t, f, "VSOL00127E0F", "10.6.3.20") // the box reports its PON serial
	setupLinked(t, c)
	o := f.onuBySerial("VSOL00127E0F")
	if o.ID == "" || o.Sec.MACCheck != store.MACOK || o.AccountID != "a1" {
		t.Fatalf("%+v", o)
	}
	f.inventory("V2605OTHER", "VSOL00AAAAAA", "02:00:0A:06:03:1F", "") // another box, in stock, reports its device serial
	c2 := newZTE(t, f, "V2605OTHER", "10.6.3.31")
	_ = c2.session("0 BOOTSTRAP")
	if f.onuBySerial("V2605OTHER").ID == "" || len(f.svc.UnknownONUs("")) != 0 {
		t.Fatalf("the device serial of a registered ONU was refused: %+v", f.svc.UnknownONUs(""))
	}
}

func TestRetiredONUIsNotRegistered(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNGONE", "", nearMAC20, "")
	f.st.Update(func(d *store.Data) error {
		e := d.Equipment["eq-SNGONE"]
		e.Status = store.EqRetired
		d.Equipment[e.ID] = e
		return nil
	})
	c := newZTE(t, f, "SNGONE", "10.6.3.20")
	_ = c.session("0 BOOTSTRAP")
	if f.onuCount() != 0 || len(f.svc.UnknownONUs("")) != 1 {
		t.Fatal("a retired ONU checked in")
	}
}

func TestSettingOffKeepsTheOldBehaviour(t *testing.T) {
	f := newFixture(t)
	f.strict(false)
	f.inventory("SNFARBOX", "", farMAC20, "a1")
	c := newZTE(t, f, "SNFARBOX", "10.6.3.20") // far MAC: not held with the setting off
	setupLinked(t, c)
	o := f.onuBySerial("SNFARBOX")
	if o.Sec.MACCheck != "" || o.Sec.ACSCred != store.CredDevice || o.AccountID != "a1" {
		t.Fatalf("%+v", o.Sec)
	}
	c2 := newZTE(t, f, "SNNOBODY", "10.6.3.30") // not registered: accepted and stored
	_ = c2.session("0 BOOTSTRAP")
	if f.onuBySerial("SNNOBODY").ID == "" || len(f.svc.UnknownONUs("")) != 0 {
		t.Fatal("an unregistered serial was refused with the setting off")
	}
	if f.alertCount() != 0 {
		t.Fatalf("alerts %v", f.alerts)
	}
}

// A second device with another spelling of a registered serial (or another OUI) is a new record of the same registered
// ONU: a near MAC does not let it through, and it must not take the house from the real box.
func TestSecondRecordOfARegisteredONUIsHeldAndDoesNotTakeTheHouse(t *testing.T) {
	for _, tc := range []struct{ name, serial, oui string }{{"case", "sndup", ""}, {"oui", "SNDUP", "AAAAAA"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.strict(true)
			f.inventory("SNDUP", "", nearMAC20, "a1")
			c := linkedWithOwnLogin(t, f, "SNDUP", "10.6.3.20")
			real := f.onuBySerial("SNDUP")
			a := newZTE(t, f, tc.serial, "10.6.3.21") // its lease MAC ...03:15 is within 8 of the registered WAN MAC
			if tc.oui != "" {
				a.OUI = tc.oui
			}
			for _, ev := range []string{"0 BOOTSTRAP", "2 PERIODIC", "2 PERIODIC"} {
				if err := a.session(ev); err != nil {
					t.Fatal(err)
				}
			}
			settle(f, c, a)
			if a.setCount() != 0 || a.count("GetParameterValues") != 0 {
				t.Fatalf("the second record got more than an InformResponse: %v", a.rpcs)
			}
			var dup store.ONU
			f.st.View(func(d *store.Data) {
				for _, o := range d.ONUs {
					if o.ID != real.ID {
						dup = o
					}
				}
			})
			if dup.ID == "" || dup.Sec.MACCheck != store.MACWaiting || dup.AccountID != "" || !strings.Contains(dup.Sec.MACWaitNote, "second device record") {
				t.Fatalf("second record: %+v", dup)
			}
			var now store.ONU
			var tasks []store.ONUTask
			f.st.View(func(d *store.Data) { now, tasks = d.ONUs[real.ID], d.ONUTasksOf(real.ID) })
			if now.AccountID != "a1" || now.State.Returned {
				t.Fatalf("the real box lost its house: %+v", now)
			}
			for _, tk := range tasks {
				if tk.Kind == store.TaskFactoryReset && tk.Active() {
					t.Fatal("a factory reset was queued for the real box")
				}
			}
			if f.alertCount() == 0 || !strings.Contains(f.alerts[len(f.alerts)-1], "second device record") {
				t.Fatalf("alerts %v", f.alerts)
			}
		})
	}
}

// A held return of a known box records only the wait: the place and profile of the record stay those of the box.
func TestHeldResetDoesNotMoveTheRecord(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNKEEP", "", nearMAC20, "a1")
	c := linkedWithOwnLogin(t, f, "SNKEEP", "10.6.3.20")
	before := f.onuBySerial("SNKEEP")
	f.st.Update(func(d *store.Data) error {
		e := d.Equipment["eq-SNKEEP"]
		e.MAC = farMAC20
		d.Equipment[e.ID] = e
		return nil
	})
	c.factoryReset()
	c.set(c.root+"ManagementServer.ConnectionRequestURL", "http://10.6.3.20:9999/elsewhere")
	_ = c.session("0 BOOTSTRAP")
	o := f.onuBySerial("SNKEEP")
	if o.Sec.MACCheck != store.MACWaiting || o.ConnReqURL != before.ConnReqURL || o.Root != before.Root || !o.Profile.Discovered {
		t.Fatalf("held, but the record moved: %+v", o)
	}
}

func TestTrustedMACIsClearedWhenTheONUIsUnlinked(t *testing.T) {
	f := newFixture(t)
	f.strict(true)
	f.inventory("SNUNL", "", farMAC20, "a1")
	c := newZTE(t, f, "SNUNL", "10.6.3.20")
	_ = c.session("0 BOOTSTRAP")
	o := f.onuBySerial("SNUNL")
	if err := f.svc.TrustONU("owner", "", o.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.onuBySerial("SNUNL").Sec.MACTrusted; got != leaseMAC20 {
		t.Fatalf("not trusted: %q", got)
	}
	if err := f.svc.UnlinkONU("owner", "", o.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.onuBySerial("SNUNL"); got.Sec.MACTrusted != "" || !got.State.Returned {
		t.Fatalf("trust survived the unlink: %+v returned=%v", got.Sec, got.State.Returned)
	}
	// the same lease MAC no longer passes: the next check-in waits for the owner again
	_ = c.session("2 PERIODIC")
	if got := f.onuBySerial("SNUNL"); got.Sec.MACCheck != store.MACWaiting {
		t.Fatalf("still let through after the unlink: %+v", got.Sec)
	}
}
