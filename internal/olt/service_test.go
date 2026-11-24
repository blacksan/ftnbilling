package olt

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/olt/fake"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

type env struct {
	t      *testing.T
	st     *store.Store
	box    *secretbox.Box
	f      *fake.OLT
	svc    *Service
	mu     sync.Mutex
	alerts []string
}

const (
	oltID  = "olt1"
	siteID = "s1"
)

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secretbox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	e := &env{t: t, st: st, box: box, f: f}
	sealed, _ := box.SealAAD(f.Pass, mgmtAAD)
	err = st.Update(func(d *store.Data) error {
		d.Sites[siteID] = store.Site{ID: siteID, Name: "Block A", TunnelIP: "10.99.0.2", ONUMgmtN: 1, VLANID: 0}
		d.Apartments["a1"] = store.Apartment{ID: "a1", Name: "Maple Gardens"}
		d.Apartments["a2"] = store.Apartment{ID: "a2", Name: "Other Court"}
		d.ApartmentSites[store.ApartmentSiteKey("a1", siteID)] = store.ApartmentSite{ApartmentID: "a1", SiteID: siteID}
		d.ApartmentSites[store.ApartmentSiteKey("a2", siteID)] = store.ApartmentSite{ApartmentID: "a2", SiteID: siteID}
		d.Accounts["acc1"] = store.Account{ID: "acc1", Code: "H1", SiteID: siteID, ApartmentID: "a1", Unit: "A1", Flat: "A1"}
		d.Accounts["acc2"] = store.Account{ID: "acc2", Code: "H2", SiteID: siteID, ApartmentID: "a2", Unit: "B1", Flat: "B1"}
		d.Equipment[oltID] = store.Equipment{ID: oltID, Kind: store.KindOLT, Name: "Block A OLT", MAC: "48:21:B8:10:D7:72", Status: store.EqInstalled, SiteID: siteID,
			RouterPort: "ether3", PONPorts: 2, PONApartments: map[string][]string{"1": {"a1"}, "2": {"a2"}}, MgmtIP: "10.7.1.2", MgmtUser: f.User, MgmtPass: sealed}
		d.Equipment["ont1"] = store.Equipment{ID: "ont1", Kind: store.KindONT, Serial: "VSOL00000001", Status: store.EqInstalled, AccountID: "acc1", SiteID: siteID}
		d.Equipment["ont2"] = store.Equipment{ID: "ont2", Kind: store.KindONT, Serial: "ZTEG00000002", GPONSerial: "VSOL00000002", Status: store.EqInstalled, AccountID: "acc2", SiteID: siteID}
		d.Equipment["ont3"] = store.Equipment{ID: "ont3", Kind: store.KindONT, Serial: "VSOL00000003", Status: store.EqInStock, SiteID: siteID}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = New(Options{Store: st, Box: box, Interval: time.Hour,
		Reach: func(d *store.Data, eq store.Equipment, s store.OLTSettings) (string, error) {
			if s.ProtoOrDefault() == "telnet" {
				return f.TelnetAddr, nil
			}
			return f.SSHAddr, nil
		},
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, a)
		},
		Alert: func(subject, msg string) {
			e.mu.Lock()
			e.alerts = append(e.alerts, subject+": "+msg)
			e.mu.Unlock()
		},
	})
	return e
}

func (e *env) state() store.OLTState {
	var s store.OLTState
	e.st.View(func(d *store.Data) { s = d.OLTStateOf(oltID).Clone() })
	return s
}

func (e *env) auditText() string {
	var b strings.Builder
	e.st.View(func(d *store.Data) {
		for _, ev := range d.Events {
			b.WriteString(ev.Msg + "\n")
		}
	})
	return b.String()
}

func (e *env) alertText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.alerts, "\n")
}

func (e *env) enable(t *testing.T) {
	t.Helper()
	if err := e.svc.SetDialectChecked(oltID, true, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetProvisioning(oltID, true, "owner"); err != nil {
		t.Fatal(err)
	}
}

// base is a running configuration that already has the profiles, as after a first Apply.
const base = `vlan 100
vlan 99
vlan 98
profile dba name ftn_dba
  type 3 assured 10000 maximum 1000000
exit
profile onu name ftn_hgu
  port-num veip 1
exit
`

func TestTestConnectionAndPin(t *testing.T) {
	e := newEnv(t)
	v, err := e.svc.TestConnection(context.Background(), oltID, "owner")
	if err != nil || !strings.Contains(v, "V1600GS-R") {
		t.Fatalf("%q %v", v, err)
	}
	st := e.state()
	if !st.Status.OK || st.Settings.HostKey != e.f.HostKeyFP {
		t.Fatalf("status %+v pin %q", st.Status, st.Settings.HostKey)
	}
	// A different host key later is refused.
	e.f.Close()
	f2 := newFake(t)
	e.svc.o.Reach = func(*store.Data, store.Equipment, store.OLTSettings) (string, error) { return f2.SSHAddr, nil }
	if _, err := e.svc.TestConnection(context.Background(), oltID, "owner"); err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("a changed host key was accepted: %v", err)
	}
}

func TestPollParsesAndClassifies(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base + "interface gpon 0/1\n  onu add 5 profile ftn_hgu sn VSOL00000009\n")
	e.f.AutoFind = [][2]string{{"1", "VSOL00000001"}, {"1", "VSOL99999999"}, {"1", "VSOL00000002"}, {"1", "VSOL00000003"}}
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	st := e.state()
	verdict := map[string]string{}
	for _, f := range st.Found {
		verdict[f.Serial] = f.Verdict
	}
	want := map[string]string{
		"VSOL00000001": store.OLTFoundReady,   // inventory, house in apartment a1, served by PON 1
		"VSOL99999999": store.OLTFoundUnknown, // not in the inventory
		"VSOL00000002": store.OLTFoundHeld,    // house in apartment a2, found on PON 1 which serves a1
		"VSOL00000003": store.OLTFoundHeld,    // in stock, no house
	}
	for k, v := range want {
		if verdict[k] != v {
			t.Errorf("%s: want %s got %s (%+v)", k, v, verdict[k], st.Found)
		}
	}
	if len(st.Status.ONUs) != 1 || st.Status.ONUs[0].ID != 5 || !st.Status.ONUs[0].Online {
		t.Errorf("onu state: %+v", st.Status.ONUs)
	}
	a := e.alertText()
	if !strings.Contains(a, "VSOL99999999") || !strings.Contains(a, "NOT added") || !strings.Contains(a, "VSOL00000002") {
		t.Errorf("alerts: %s", a)
	}
	// Rate limit: a second poll does not alert again.
	n := len(e.alerts)
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	if len(e.alerts) != n {
		t.Errorf("alerted again within 24 hours: %v", e.alerts[n:])
	}
}

func TestProvisioningOffWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base)
	e.f.AutoFind = [][2]string{{"1", "VSOL00000001"}}
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	for _, l := range e.f.Lines() {
		if writeLine(l) {
			t.Fatalf("a write-path line was sent with provisioning off: %q (%v)", l, e.f.Lines())
		}
	}
	// With the toggle on but the dialect not checked: still nothing.
	if err := e.svc.SetProvisioning(oltID, true, "owner"); err != nil {
		t.Fatal(err)
	}
	e.f.Reset()
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	for _, l := range e.f.Lines() {
		if writeLine(l) {
			t.Fatalf("provisioning without a checked dialect wrote %q", l)
		}
	}
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil || p.Empty() {
		t.Fatalf("plan: %v %v", p, err)
	}
	if _, err := e.svc.Apply(context.Background(), oltID, h, "owner"); err != ErrProvisioningOff {
		t.Fatalf("apply without the dialect flag: %v", err)
	}
}

func TestAutoAddsInventoryOnCorrectPON(t *testing.T) {
	e := newEnv(t)
	// ONU id 1 and 2 are taken on PON 1: the next free is 3.
	e.f.SetRunning(base + "interface gpon 0/1\n  onu add 1 profile ftn_hgu sn VSOL00000071\n  onu add 2 profile ftn_hgu sn VSOL00000072\n")
	e.f.AutoFind = [][2]string{{"1", "VSOL00000001"}, {"1", "VSOL99999999"}, {"1", "VSOL00000002"}}
	e.enable(t)
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	lines := e.f.Lines()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "onu add 3 profile ftn_hgu sn VSOL00000001") {
		t.Fatalf("the inventory ONU was not added with the next free id:\n%s", joined)
	}
	for _, bad := range []string{"VSOL99999999", "VSOL00000002", "onu add 4"} {
		if strings.Contains(joined, "onu add") && strings.Contains(strings.Join(addLines(lines), "\n"), bad) {
			t.Fatalf("%s must not be added:\n%s", bad, joined)
		}
	}
	// services, save, and a backup was read before the first config line
	for _, want := range []string{"onu 3 tcont 1 dba ftn_dba", "onu 3 service 1 gemport 1 vlan 100", "onu 3 service 2 gemport 2 vlan 99", "write"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
	ci := lastIndexOf(lines, "configure terminal", len(lines))
	ri := lastIndexOf(lines, "show running-config", ci)
	if ci < 0 || ri < 0 {
		t.Errorf("no show running-config before the first change: %v", lines)
	}
	st := e.state()
	if len(st.Backups) == 0 || st.Backups[0].Why != "before automatic ONU add" || !st.Backups[0].Sealed {
		t.Errorf("backups: %+v", st.Backups)
	}
	var eq store.Equipment
	e.st.View(func(d *store.Data) { eq = d.Equipment["ont1"] })
	if eq.OLTID != oltID || eq.PONPort != 1 {
		t.Errorf("placement not recorded: %+v", eq)
	}
	for _, f := range st.Found {
		if f.Serial == "VSOL00000001" && f.Verdict != store.OLTFoundAdded {
			t.Errorf("verdict %s", f.Verdict)
		}
		if f.Serial == "VSOL99999999" && f.Verdict != store.OLTFoundUnknown {
			t.Errorf("unknown serial verdict %s", f.Verdict)
		}
	}
	if !strings.Contains(e.auditText(), "authorised on PON 1") {
		t.Errorf("no audit of the add:\n%s", e.auditText())
	}
}

// writeLine is a line that changes the OLT (the read-only queries enter an interface to show a table, which is not one).
func writeLine(l string) bool {
	for _, p := range []string{"onu ", "vlan", "profile", "no ", "ip route", "switchport", "ipaddress", "write", "interface vlan", "interface gigabit"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func addLines(l []string) []string {
	var o []string
	for _, x := range l {
		if strings.HasPrefix(x, "onu add") {
			o = append(o, x)
		}
	}
	return o
}
func indexOf(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}
func lastIndexOf(l []string, s string, upTo int) int {
	r := -1
	for i := 0; i < len(l) && i < upTo; i++ {
		if l[i] == s {
			r = i
		}
	}
	return r
}
func TestWrongPONHeldAndAlerted(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base)
	e.f.AutoFind = [][2]string{{"2", "VSOL00000001"}} // house in apartment a1 appears on PON 2 (serves a2)
	e.enable(t)
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(addLines(e.f.Lines()), ""); got != "" {
		t.Fatalf("a wrong-PON ONU was added: %s", got)
	}
	st := e.state()
	if len(st.Found) != 1 || st.Found[0].Verdict != store.OLTFoundHeld || !strings.Contains(st.Found[0].Detail, "does not serve") {
		t.Fatalf("%+v", st.Found)
	}
	if !strings.Contains(e.alertText(), "held") {
		t.Fatalf("no alert: %q", e.alertText())
	}
}

func TestApplyBacksUpFirstAndErrorAborts(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning("")
	e.enable(t)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil || p.Empty() {
		t.Fatalf("%v %v", p, err)
	}
	cmds := p.Commands()
	// The plan creates the VLANs, profiles, uplink tagging and the management address.
	all := strings.Join(cmds, "\n")
	for _, want := range []string{"vlan 100", "vlan 99", "vlan 98", "profile dba name ftn_dba", "profile onu name ftn_hgu", "interface gigabitethernet 0/1",
		"switchport hybrid vlan 100 untagged", "switchport hybrid pvid vlan 100", "switchport hybrid vlan 99 tagged", "switchport hybrid vlan 98 tagged",
		"interface vlan 98", "ipaddress 10.7.1.2 255.255.255.248", "ip route 0.0.0.0 0.0.0.0 10.7.1.1"} {
		if !strings.Contains(all, want) {
			t.Errorf("plan lacks %q:\n%s", want, all)
		}
	}
	// A stale hash is refused before anything is sent.
	e.f.Reset()
	if _, err := e.svc.Apply(context.Background(), oltID, "deadbeef", "owner"); err != ErrPlanChanged {
		t.Fatalf("stale hash: %v", err)
	}
	for _, l := range e.f.Lines() {
		if writeLine(l) {
			t.Fatalf("a stale plan sent %q", l)
		}
	}
	// An error on one line aborts the rest, saves nothing, and leaves an audit entry and an alert.
	e.f.FailOn = "profile onu name"
	e.f.Reset()
	res, err := e.svc.Apply(context.Background(), oltID, h, "owner")
	if err == nil {
		t.Fatal("apply should have failed")
	}
	lines := e.f.Lines()
	if lastIndexOf(lines, "show running-config", lastIndexOf(lines, "configure terminal", len(lines))) < 0 {
		t.Fatalf("no backup read before configuring: %v", lines)
	}
	if indexOf(lines, "write") >= 0 {
		t.Fatalf("saved after a failure: %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "ip route") || strings.Contains(l, "interface vlan 98") {
			t.Fatalf("a line after the failing one was sent: %q", l)
		}
	}
	if res.Backup == "" || len(e.state().Backups) == 0 {
		t.Fatal("no backup before the change")
	}
	a := e.auditText()
	if !strings.Contains(a, "FAILED") || !strings.Contains(a, "profile onu name ftn_hgu") {
		t.Fatalf("audit: %s", a)
	}
	if !strings.Contains(e.alertText(), "configuration stopped") {
		t.Fatalf("alerts: %s", e.alertText())
	}
}

func TestApplyVerifies(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning("")
	e.enable(t)
	_, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Apply(context.Background(), oltID, h, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || len(res.Remaining) != 0 {
		t.Fatalf("not verified: %v", res.Remaining)
	}
	// A second plan is empty: nothing left to do.
	p2, _, err := e.svc.Plan(context.Background(), oltID)
	if err != nil || !p2.Empty() {
		t.Fatalf("second plan not empty: %v %v", p2.Commands(), err)
	}
}

func TestSecretsNeverInAuditAlertsOrBackupText(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base)
	e.f.Secret = "TOPSECRETHASH"
	if err := e.svc.SaveSettings(oltID, SettingsInput{EnablePass: "enable-secret-xyz"}, "owner"); err != nil {
		t.Fatal(err)
	}
	e.f.EnablePass = "enable-secret-xyz"
	e.enable(t)
	if _, err := e.svc.Backup(context.Background(), oltID, "manual", "owner"); err != nil {
		t.Fatal(err)
	}
	st := e.state()
	if len(st.Backups) != 1 {
		t.Fatal("no backup")
	}
	if strings.Contains(st.Backups[0].Text, "TOPSECRETHASH") || strings.Contains(st.Settings.EnablePass, "enable-secret-xyz") {
		t.Fatal("a secret is stored in clear")
	}
	_, text, err := e.svc.BackupText(oltID, st.Backups[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "TOPSECRETHASH") || !strings.Contains(text, "vlan 100") {
		t.Fatalf("backup text not redacted or lost:\n%s", text)
	}
	for _, s := range []string{e.auditText(), e.alertText()} {
		for _, secret := range []string{"TOPSECRETHASH", "enable-secret-xyz", e.f.Pass} {
			if strings.Contains(s, secret) {
				t.Fatalf("%q leaked:\n%s", secret, s)
			}
		}
	}
	// A mismatched backup keeps at most 20.
	for i := 0; i < 25; i++ {
		if _, err := e.svc.Backup(context.Background(), oltID, "manual", "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.svc.BackupList(oltID)); n != store.MaxOLTBackups {
		t.Fatalf("%d backups kept", n)
	}
}

func TestRemoveNeedsPlanAndConfirm(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base + "interface gpon 0/1\n  onu add 3 profile ftn_hgu sn VSOL00000001\n")
	e.enable(t)
	p, h, err := e.svc.PlanRemove(context.Background(), oltID, "VSOL00000001")
	if err != nil || !strings.Contains(strings.Join(p.Commands(), "\n"), "no onu 3") {
		t.Fatalf("%v %v", p.Commands(), err)
	}
	// A poll never removes anything.
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(e.f.Lines(), "\n"), "no onu") {
		t.Fatal("a poll removed an ONU")
	}
	if _, err := e.svc.ApplyRemove(context.Background(), oltID, "VSOL00000001", "wrong", "owner"); err != ErrPlanChanged {
		t.Fatalf("%v", err)
	}
	if _, err := e.svc.ApplyRemove(context.Background(), oltID, "VSOL00000001", h, "owner"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(e.f.Lines(), "\n"), "no onu 3") {
		t.Fatal("not removed")
	}
}

func TestOneSessionPerOLT(t *testing.T) {
	e := newEnv(t)
	lk := e.svc.lockOf(oltID)
	lk <- struct{}{}
	defer func() { <-lk }()
	if err := e.svc.Poll(context.Background(), oltID, false); err != ErrBusy {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}
