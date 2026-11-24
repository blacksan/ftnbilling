package olt

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/olt/fake"
	"ispbill/internal/store"
)

// aenv is an environment for the adoption through the router's OLT management port: an inventory OLT with its serial and
// MAC, no login stored, a factory OLT (the vendor's login) behind a fake router port.
type aenv struct {
	*env
	mu2     sync.Mutex
	rows    []SeenMAC
	ensured int
	logs    *bytes.Buffer
	dead    bool // Reach to a closed port (SSH off)
}

const (
	benchSerial = "V2609170360"
	oltMAC      = "48:21:B8:10:D7:72"
)

func newAdoptEnv(t *testing.T) *aenv {
	t.Helper()
	e := newEnv(t)
	a := &aenv{env: e, logs: &bytes.Buffer{}}
	e.f.SetCreds(factoryUser, factoryPass, "")
	if err := e.st.Update(func(d *store.Data) error {
		s := d.Sites[siteID]
		s.OLTMgmtPort = "ether5"
		d.Sites[siteID] = s
		o := d.Equipment[oltID]
		o.Serial, o.MgmtIP, o.MgmtUser, o.MgmtPass = benchSerial, "", "", ""
		d.Equipment[oltID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.rows = []SeenMAC{{MAC: oltMAC, IP: "192.168.8.200", Status: "reachable"}}
	a.rebuild(nil)
	return a
}

// rebuild makes the service again (new options) on the same store and fake.
func (a *aenv) rebuild(mut func(*Options)) {
	o := Options{Store: a.st, Box: a.box, Interval: time.Hour, ProbeWait: 5 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(a.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Reach: func(d *store.Data, eq store.Equipment, s store.OLTSettings) (string, error) {
			if a.dead {
				return "127.0.0.1:1", nil
			}
			return a.f.SSHAddr, nil
		},
		Dial: func(ctx context.Context, n, ad string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, ad)
		},
		Alert: func(subject, msg string) {
			a.env.mu.Lock()
			a.env.alerts = append(a.env.alerts, subject+": "+msg)
			a.env.mu.Unlock()
		},
		PortSeen: func(ctx context.Context, siteID string) (PortSeen, error) {
			a.mu2.Lock()
			defer a.mu2.Unlock()
			return PortSeen{Port: "ether5", Rows: append([]SeenMAC(nil), a.rows...)}, nil
		},
		EnsurePort: func(ctx context.Context, siteID, by string) error {
			a.mu2.Lock()
			a.ensured++
			a.mu2.Unlock()
			return nil
		},
		Vendor: func(mac string) string { return "V-SOL" },
	}
	if mut != nil {
		mut(&o)
	}
	a.svc = New(o)
}

func (a *aenv) adopt(t *testing.T) {
	t.Helper()
	if err := a.svc.AdoptSite(context.Background(), siteID, "system"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

func (a *aenv) adoptState() store.OLTAdopt { return a.state().Settings.Adopt }

func (a *aenv) opened(t *testing.T) (user, login, enable string) {
	t.Helper()
	var e store.Equipment
	a.st.View(func(d *store.Data) { e = d.Equipment[oltID] })
	st := a.state()
	l, err := a.box.OpenAAD(e.MgmtPass, mgmtAAD)
	if err != nil {
		t.Fatalf("the stored password does not open: %v", err)
	}
	var en string
	if st.Settings.EnablePass != "" {
		en, err = a.box.OpenAAD(st.Settings.EnablePass, enableAAD)
		if err != nil {
			t.Fatal(err)
		}
	}
	return e.MgmtUser, l, en
}

func (a *aenv) noLeak(t *testing.T, secrets ...string) {
	t.Helper()
	text := map[string]string{"audit": a.auditText(), "alerts": a.alertText(), "logs": a.logs.String()}
	st := a.state()
	b := new(strings.Builder)
	for _, x := range st.Backups {
		b.WriteString(x.Text)
	}
	text["backups (stored)"] = b.String()
	text["state"] = st.Settings.Adopt.Note + st.Settings.Adopt.Error + st.Settings.Adopt.SNMPNote + st.Settings.Adopt.ACLNote + st.Status.Error
	for where, tx := range text {
		for _, sec := range append(secrets, factoryPass) {
			if sec != "" && strings.Contains(tx, sec) {
				t.Fatalf("a secret leaked into %s:\n%s", where, tx)
			}
		}
	}
}

var alnumRE = regexp.MustCompile(`^[A-Za-z0-9]{24,}$`)

func indexEv(ev []string, sub string, from int) int {
	for i := from; i < len(ev); i++ {
		if strings.Contains(ev[i], sub) {
			return i
		}
	}
	return -1
}

func TestAdoptFactoryDefaultSecuresVerifiesThenWrites(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	ad := a.adoptState()
	if !ad.Adopted || !ad.Secured || ad.Stage != store.AdoptSecured || ad.SerialOK != "ok" || !ad.SSHOK || !ad.MACSeen || !ad.FactoryOK {
		t.Fatalf("adoption state: %+v", ad)
	}
	user, login, enable := a.opened(t)
	if user != "admin" || !alnumRE.MatchString(login) || !alnumRE.MatchString(enable) || login == enable {
		t.Fatalf("stored login %q %q %q", user, login, enable)
	}
	if login == factoryPass {
		t.Fatal("the factory password was stored")
	}
	// The OLT itself changed to those passwords.
	u, p, en := a.f.Creds()
	if u != "admin" || p != login || en != enable {
		t.Fatalf("the OLT has other passwords than the stored ones")
	}
	// Order: factory login, the user manager change, a NEW session with the new login, and only then write.
	ev := a.f.Events()
	iFactory := indexEv(ev, "login admin true", 0)
	iChange := indexEv(ev, "cmd user login-password admin ", 0)
	iEn := indexEv(ev, "cmd user enable-password admin ", 0)
	iNew := indexEv(ev, "login admin true", iFactory+1)
	iWrite := indexEv(ev, "cmd write", 0)
	if iFactory < 0 || iChange < iFactory || iEn < iChange || iNew < iEn || iWrite < iNew {
		t.Fatalf("wrong order of login/change/verify/write:\n%s", strings.Join(ev, "\n"))
	}
	if iUM := indexEv(ev, "cmd user manager", 0); iUM < 0 || iUM > iChange {
		t.Fatalf("user manager not entered first:\n%s", strings.Join(ev, "\n"))
	}
	// SNMP defaults removed, saved again afterwards.
	if len(a.f.SNMPCommunities()) != 0 {
		t.Fatalf("communities left: %v", a.f.SNMPCommunities())
	}
	if !ad.SNMPOff || !strings.Contains(ad.SNMPNote, "SNMP is off") {
		t.Fatalf("no SNMP note: %q", ad.SNMPNote)
	}
	if a.f.Writes() < 2 {
		t.Fatalf("%d writes", a.f.Writes())
	}
	// The ACL step is skipped with a visible note, and nothing dangerous was sent.
	if ad.ACLDone || !strings.Contains(ad.ACLNote, "TODO(bench)") {
		t.Fatalf("ACL note: %q done=%v", ad.ACLNote, ad.ACLDone)
	}
	for _, l := range a.f.Lines() {
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "login-access-list") || strings.HasPrefix(low, "sshd") || strings.Contains(low, "disable") {
			t.Fatalf("a line that must never be sent: %q", l)
		}
	}
	// The vendor login was tried once.
	n := 0
	for _, at := range a.f.Attempts() {
		if at.Pass == factoryPass {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the factory login was tried %d times", n)
	}
	// The host key was pinned after the identification, and the poll now works with the stored login.
	if a.state().Settings.HostKey == "" {
		t.Fatal("host key not pinned")
	}
	if err := a.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatalf("poll with the new login: %v", err)
	}
	if !a.state().Status.OK {
		t.Fatal("status not ok")
	}
	// A second pass changes nothing and does not try the vendor login again.
	a.f.Reset()
	a.adopt(t)
	if l := a.f.Lines(); len(l) != 0 {
		t.Fatalf("a second pass sent %v", l)
	}
	a.noLeak(t, login, enable)
	// The audit says it, without a secret.
	if !strings.Contains(a.auditText(), "secured") {
		t.Fatalf("no audit:\n%s", a.auditText())
	}
}

func TestAdoptStoredLoginNeverTriesFactory(t *testing.T) {
	a := newAdoptEnv(t)
	a.f.SetCreds("admin", "owner-typed-pass", "")
	if err := a.svc.SetCredentials(oltID, "admin", "owner-typed-pass", "", "olga"); err != nil {
		t.Fatal(err)
	}
	a.adopt(t)
	ad := a.adoptState()
	if !ad.Adopted || ad.Secured || ad.Stage != store.AdoptAdopted || ad.CredSource != "owner" {
		t.Fatalf("%+v", ad)
	}
	for _, at := range a.f.Attempts() {
		if at.Pass == factoryPass {
			t.Fatal("the factory login was tried although a login is stored")
		}
	}
	// Nothing was configured: only reads.
	for _, l := range a.f.Lines() {
		if strings.HasPrefix(l, "user ") || l == "write" {
			t.Fatalf("a config line was sent: %q", l)
		}
	}
	// A wrong stored login: no factory attempt either, the owner is asked.
	if err := a.svc.SetCredentials(oltID, "admin", "wrong-pass", "", "olga"); err != nil {
		t.Fatal(err)
	}
	a.adopt(t)
	if ad := a.adoptState(); ad.Stage != store.AdoptNeedLogin {
		t.Fatalf("%+v", ad)
	}
	for _, at := range a.f.Attempts() {
		if at.Pass == factoryPass {
			t.Fatal("the factory login was tried after a stored login failed")
		}
	}
}

func TestAdoptFactoryTriedOnceThenOwnerAsked(t *testing.T) {
	a := newAdoptEnv(t)
	a.f.SetCreds("admin", "the-owner-changed-it", "") // not the vendor default any more
	a.adopt(t)
	ad := a.adoptState()
	if ad.Stage != store.AdoptNeedLogin || ad.FactoryAt.IsZero() || ad.FactoryOK || ad.Adopted {
		t.Fatalf("%+v", ad)
	}
	a.adopt(t)
	a.adopt(t)
	n := 0
	for _, at := range a.f.Attempts() {
		if at.Pass == factoryPass {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the vendor login was tried %d times (once is the rule)", n)
	}
	if strings.Contains(ad.Note+ad.Error, factoryPass) {
		t.Fatal("the vendor password is in the state")
	}
	// The owner types the login; the next pass adopts.
	if err := a.svc.SetCredentials(oltID, "admin", "the-owner-changed-it", "", "olga"); err != nil {
		t.Fatal(err)
	}
	a.adopt(t)
	if ad := a.adoptState(); !ad.Adopted || ad.Stage != store.AdoptAdopted {
		t.Fatalf("%+v", ad)
	}
	a.noLeak(t, "the-owner-changed-it")
}

func TestAdoptSerialMismatchConfiguresNothingAndListsPossibleOLT(t *testing.T) {
	a := newAdoptEnv(t)
	a.f.Version = strings.Replace(fake.BenchVersion, benchSerial, "V9999999999", 1)
	a.adopt(t)
	ad := a.adoptState()
	if ad.Stage != store.AdoptMismatch || ad.SerialOK != "mismatch" || ad.Adopted || ad.Secured {
		t.Fatalf("%+v", ad)
	}
	for _, l := range a.f.Lines() {
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "user") || low == "write" || strings.HasPrefix(low, "no ") || strings.HasPrefix(low, "interface") || strings.HasPrefix(low, "profile") || strings.HasPrefix(low, "snmp") {
			t.Fatalf("a configuration line was sent to a device that is not the inventory OLT: %q", l)
		}
	}
	// The vendor password stays the vendor's: nothing changed.
	if _, p, _ := a.f.Creds(); p != factoryPass {
		t.Fatal("the password of the other device was changed")
	}
	if a.state().Settings.HostKey != "" {
		t.Fatal("the host key of an unidentified device was pinned")
	}
	var dev store.Device
	a.st.View(func(d *store.Data) { dev = d.Devices[store.DeviceKey(siteID, oltMAC)] })
	if dev.Source != store.DeviceSourceOLTMgmt || dev.Port != "ether5" || dev.Vendor != "V-SOL" {
		t.Fatalf("not listed as a possible OLT: %+v", dev)
	}
	if !strings.Contains(a.alertText(), "Possible OLT") {
		t.Fatalf("no alert: %q", a.alertText())
	}
	// Rate limited: a second pass does not alert again.
	before := strings.Count(a.alertText(), "Possible OLT")
	a.adopt(t)
	if strings.Count(a.alertText(), "Possible OLT") != before {
		t.Fatal("the alert was repeated")
	}
	// A wrong model is a mismatch too.
	a.f.Version = strings.Replace(fake.BenchVersion, "V1600GS-R", "V2800X", 1)
	a.adopt(t)
	if ad := a.adoptState(); ad.Stage != store.AdoptMismatch {
		t.Fatalf("model not checked: %+v", ad)
	}
	a.noLeak(t)
}

func TestAdoptNoInventorySerialIsNotAdopted(t *testing.T) {
	a := newAdoptEnv(t)
	if err := a.st.Update(func(d *store.Data) error {
		o := d.Equipment[oltID]
		o.Serial = ""
		d.Equipment[oltID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.adopt(t)
	if ad := a.adoptState(); ad.Stage != store.AdoptMismatch || ad.Adopted {
		t.Fatalf("%+v", ad)
	}
}

func TestUnknownMACIsAPossibleOLTAndNoLoginIsTried(t *testing.T) {
	a := newAdoptEnv(t)
	a.rows = []SeenMAC{{MAC: "AA:BB:CC:00:11:22", IP: "192.168.8.77", Status: "reachable"}}
	a.adopt(t)
	if n := len(a.f.Attempts()); n != 0 {
		t.Fatalf("%d logins were tried although the OLT's MAC is not on the port", n)
	}
	if ad := a.adoptState(); ad.Stage != store.AdoptUnknownMAC || ad.MACSeen {
		t.Fatalf("%+v", ad)
	}
	var dev store.Device
	a.st.View(func(d *store.Data) { dev = d.Devices[store.DeviceKey(siteID, "AA:BB:CC:00:11:22")] })
	if dev.Source != store.DeviceSourceOLTMgmt || dev.IP != "192.168.8.77" {
		t.Fatalf("%+v", dev)
	}
	if !strings.Contains(a.alertText(), "AA:BB:CC:00:11:22") {
		t.Fatalf("no alert: %q", a.alertText())
	}
	// Nothing is listed when the port is empty; no cable is the next action.
	a.rows = nil
	a.adopt(t)
	if ad := a.adoptState(); ad.Stage != store.AdoptNoCable {
		t.Fatalf("%+v", ad)
	}
}

func TestAdoptSSHOff(t *testing.T) {
	a := newAdoptEnv(t)
	a.dead = true
	a.adopt(t)
	ad := a.adoptState()
	if ad.Stage != store.AdoptNoSSH || ad.SSHOK || !strings.Contains(ad.Note, "sshd enable") {
		t.Fatalf("%+v", ad)
	}
	if !ad.FactoryAt.IsZero() {
		t.Fatal("the vendor login counted as tried although SSH was not reached")
	}
	a.dead = false
	a.adopt(t)
	if ad := a.adoptState(); !ad.Secured {
		t.Fatalf("not adopted after SSH came up: %+v", ad)
	}
}

func TestSecureFailureRestoresTheOldPassword(t *testing.T) {
	for _, mode := range []string{"blocked", "ignored", "writefail"} {
		t.Run(mode, func(t *testing.T) {
			a := newAdoptEnv(t)
			a.f.SetCreds("admin", "owner-typed-pass", "owner-enable")
			if err := a.svc.SetCredentials(oltID, "admin", "owner-typed-pass", "owner-enable", "olga"); err != nil {
				t.Fatal(err)
			}
			a.adopt(t)
			if !a.adoptState().Adopted {
				t.Fatalf("not adopted: %+v", a.adoptState())
			}
			a.f.Reset()
			switch mode {
			case "blocked":
				a.f.BlockNewPass = true // the OLT takes the line, then refuses logins with it
			case "ignored":
				a.f.IgnoreUserChange = true
			case "writefail":
				a.f.FailWrite = true
			}
			_, err := a.svc.Secure(context.Background(), oltID, "olga")
			if err == nil || !strings.Contains(err.Error(), "restored") {
				t.Fatalf("expected a failure that says the old password was restored, got %v", err)
			}
			// The OLT is back on its old passwords, and so is the store.
			u, p, en := a.f.Creds()
			if u != "admin" || p != "owner-typed-pass" || en != "owner-enable" {
				t.Fatalf("the OLT was left with %q / %q / %q", u, p, en)
			}
			user, login, enable := a.opened(t)
			if user != "admin" || login != "owner-typed-pass" || enable != "owner-enable" {
				t.Fatalf("the stored login changed: %q %q %q", user, login, enable)
			}
			ad := a.adoptState()
			if ad.Secured || ad.PendingPass != "" || ad.PendingEn != "" || ad.Error == "" {
				t.Fatalf("%+v", ad)
			}
			if mode != "writefail" && indexEv(a.f.Events(), "cmd write", 0) >= 0 {
				t.Fatal("the configuration was saved although the new password did not verify")
			}
			// The old session restored the old login password with the old (typed) line.
			if indexEv(a.f.Events(), "cmd user login-password admin owner-typed-pass", 0) < 0 {
				t.Fatalf("no restore line:\n%s", strings.Join(a.f.Events(), "\n"))
			}
			// Still adopted: the next poll works with the old login.
			if err := a.svc.Poll(context.Background(), oltID, true); err != nil {
				t.Fatalf("poll after a failed secure: %v", err)
			}
			if !strings.Contains(a.auditText(), "Secure this OLT failed") {
				t.Fatalf("no audit of the failure:\n%s", a.auditText())
			}
			a.noLeak(t, "owner-typed-pass", "owner-enable")
		})
	}
}

func TestSecureNeedsAnIdentifiedOLT(t *testing.T) {
	a := newAdoptEnv(t)
	if _, err := a.svc.Secure(context.Background(), oltID, "olga"); !errors.Is(err, ErrNotIdentified) {
		t.Fatalf("got %v", err)
	}
	if n := len(a.f.Attempts()); n != 0 {
		t.Fatalf("%d logins before the OLT was identified", n)
	}
}

func TestSecureAgainReplacesGeneratedPasswords(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	_, old, _ := a.opened(t)
	if _, err := a.svc.Secure(context.Background(), oltID, "olga"); err != nil {
		t.Fatalf("secure again: %v", err)
	}
	_, nw, _ := a.opened(t)
	if nw == old {
		t.Fatal("the password did not change")
	}
	if _, p, _ := a.f.Creds(); p != nw {
		t.Fatal("OLT and store differ")
	}
}

func TestInterruptedChangeIsRecoveredFromThePendingPair(t *testing.T) {
	a := newAdoptEnv(t)
	// The server stored a pending pair, changed the OLT, and stopped before it stored the new login: the stored login
	// is the old one, the OLT has the new one.
	a.f.SetCreds("admin", "owner-typed-pass", "")
	if err := a.svc.SetCredentials(oltID, "admin", "owner-typed-pass", "", "olga"); err != nil {
		t.Fatal(err)
	}
	pend, _ := GenPassword()
	pendEn, _ := GenPassword()
	sp, _ := a.box.SealAAD(pend, mgmtAAD)
	se, _ := a.box.SealAAD(pendEn, enableAAD)
	if err := a.svc.setAdopt(oltID, func(x *store.OLTAdopt, e *store.Equipment, d *store.Data) { x.PendingPass, x.PendingEn = sp, se }); err != nil {
		t.Fatal(err)
	}
	a.f.SetCreds("admin", pend, pendEn)
	a.adopt(t)
	ad := a.adoptState()
	if !ad.Secured || ad.PendingPass != "" {
		t.Fatalf("%+v", ad)
	}
	if _, login, enable := a.opened(t); login != pend || enable != pendEn {
		t.Fatal("the pending pair did not become the stored login")
	}
}

func TestRevealIsAuditedAndSecretFree(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	_, login, enable := a.opened(t)
	u, l, en, err := a.svc.Reveal(oltID, "olga")
	if err != nil || u != "admin" || l != login || en != enable {
		t.Fatalf("%v %q %q %q", err, u, l, en)
	}
	if !strings.Contains(a.auditText(), "revealed to olga") {
		t.Fatalf("not audited:\n%s", a.auditText())
	}
	if ad := a.adoptState(); ad.RevealedBy != "olga" || ad.RevealedAt.IsZero() {
		t.Fatalf("%+v", ad)
	}
	a.noLeak(t, login, enable)
	if _, _, _, err := a.svc.Reveal("nope", "olga"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestAdoptViewChecklist(t *testing.T) {
	a := newAdoptEnv(t)
	v, ok := a.svc.AdoptView(oltID)
	if !ok || len(v.Steps) < 7 || v.CanSecure {
		t.Fatalf("%+v", v)
	}
	a.adopt(t)
	v, _ = a.svc.AdoptView(oltID)
	states := map[string]string{}
	for _, st := range v.Steps {
		states[st.Label] = st.State
	}
	for _, l := range []string{"Cable and port", "OLT seen on the port", "SSH reached", "Serial and model", "Credentials", "Secured", "SNMP"} {
		if states[l] != "ok" {
			t.Fatalf("%s is %q: %+v", l, states[l], v.Steps)
		}
	}
	if states["SSH access list"] != "off" {
		t.Fatalf("the access list step must show as skipped: %+v", v.Steps)
	}
	if v.Stage != store.AdoptSecured || !v.Secured {
		t.Fatalf("%+v", v)
	}
	// Without a router port the next action says so.
	if err := a.st.Update(func(d *store.Data) error {
		s := d.Sites[siteID]
		s.OLTMgmtPort = ""
		d.Sites[siteID] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.svc.AdoptCheck(context.Background(), oltID, "olga"); !errors.Is(err, ErrNoPort) {
		t.Fatalf("got %v", err)
	}
	if v, _ := a.svc.AdoptView(oltID); v.Stage != store.AdoptNoPort || !strings.Contains(v.Note, "OLT management") {
		t.Fatalf("%+v", v)
	}
}

func TestSecureShapesAreTheOnlyUserPasswordSNMPLines(t *testing.T) {
	f := newFake2(t)
	s, err := Open(ctx(t), VSOL(), target(f, "ssh"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	good := "A1b2C3d4E5f6G7h8I9j0K1l2M3n4"
	// The ordinary command path still refuses every one of them.
	for _, l := range []string{
		"user login-password admin " + good, "user enable-password admin " + good, "user manager", "user delete admin",
		"no snmp-server community public", "snmp-server community public rw", "login-access-list deny ssh 0.0.0.0 0.0.0.0", "sshd disable", "sshd enable",
		"password " + good, "enable password " + good,
	} {
		if err := s.Config(ctx(t), []string{l}); !errors.Is(err, ErrDenied) {
			t.Errorf("Config(%q) = %v, want the denylist", l, err)
		}
	}
	f.Reset()
	// RunSecure takes exact shapes only, and sends nothing when one line of the batch is wrong.
	for _, bad := range [][]string{
		{"user delete admin"}, {"user manager", "user delete admin", "exit"}, {"sshd disable"}, {"sshd enable"},
		{"user login-password admin short"}, {"user login-password admin " + good + "?"}, {"user login-password admin " + good + "!x"},
		{"user login-password admin " + good + " extra"}, {"user login-password ad;min " + good},
		{"user role admin admin enable-password " + good}, {"user add bob login-password " + good},
		{"no snmp-server community foo"}, {"no snmp-server community"}, {"snmp-server community public rw"},
		{"login-access-list deny ssh 0.0.0.0 0.0.0.0"}, {"login-access-list permit ssh 10.0.0.1 255.255.255.255"},
		{"user login-password admin " + good + "\nreboot"}, {"write erase"}, {"reboot"},
		{"user manager", "user login-password admin " + good, "reboot"},
	} {
		if err := s.RunSecure(ctx(t), bad); !errors.Is(err, ErrSecureShape) {
			t.Errorf("RunSecure(%q) = %v, want ErrSecureShape", bad, err)
		}
	}
	if l := f.Lines(); len(l) != 0 {
		t.Fatalf("something was sent for a refused batch: %v", l)
	}
	// An old password is put back only through RunRestore, and only with the same two shapes.
	if err := s.RunSecure(ctx(t), VSOL().SecureUserLines("admin", "short", "")); !errors.Is(err, ErrSecureShape) {
		t.Errorf("RunSecure took an old password: %v", err)
	}
	for _, bad := range [][]string{{"user login-password admin sh ort"}, {"user login-password admin sh?ort"}, {"user login-password admin short!"}, {"user delete admin"}, {"sshd disable"}} {
		if err := s.RunRestore(ctx(t), bad); !errors.Is(err, ErrSecureShape) {
			t.Errorf("RunRestore(%q) = %v", bad, err)
		}
	}
	if l := f.Lines(); len(l) != 0 {
		t.Fatalf("something was sent for a refused batch: %v", l)
	}
	if err := s.RunRestore(ctx(t), VSOL().SecureUserLines("admin", "Xpon@Olt9417#", "")); err != nil {
		t.Fatalf("restore: %v", err)
	}
	f.Reset()
	// The exact shapes work.
	if err := s.RunSecure(ctx(t), VSOL().SecureUserLines("admin", good, good+"x")); err != nil {
		t.Fatal(err)
	}
	if err := s.RunSecure(ctx(t), VSOL().SecureSNMPLines([]string{"public", "private"})); err != nil {
		t.Fatal(err)
	}
	// And the transcript never has a password.
	for _, e := range s.Transcript {
		if strings.Contains(e.Cmd+e.Out+e.Err, good) {
			t.Fatalf("a password is in the transcript: %+v", e)
		}
	}
	// A refusal by the OLT names the line without the password.
	f.FailOn = "login-password"
	err = s.RunSecure(ctx(t), VSOL().SecureUserLines("admin", good, ""))
	if err == nil || strings.Contains(err.Error(), good) {
		t.Fatalf("error %v", err)
	}
}

func newFake2(t *testing.T) *fake.OLT { return newFake(t) }

func TestGenPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := GenPassword()
		if err != nil {
			t.Fatal(err)
		}
		if !alnumRE.MatchString(p) || len(p) < 24 || strings.ContainsAny(p, "?! \t#") {
			t.Fatalf("bad password %q", p)
		}
		if !regexp.MustCompile(`[a-z]`).MatchString(p) || !regexp.MustCompile(`[A-Z]`).MatchString(p) || !regexp.MustCompile(`[0-9]`).MatchString(p) {
			t.Fatalf("missing a class: %q", p)
		}
		if seen[p] {
			t.Fatal("repeated password")
		}
		seen[p] = true
	}
}

func TestRedactHidesThePasswordAfterTheUserName(t *testing.T) {
	for _, in := range []string{
		"user login-password admin Sup3rSecretValueXyz", "user enable-password admin Sup3rSecretValueXyz",
		"% Unknown command: user login-password bob Sup3rSecretValueXyz", "USER LOGIN-PASSWORD admin Sup3rSecretValueXyz",
	} {
		if out := Redact(in); strings.Contains(out, "Sup3rSecretValueXyz") {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
}

func TestFactoryPasswordIsNotShownAnywhere(t *testing.T) {
	// The constant stays in this package only; no exported string carries it.
	for _, s := range []string{VSOL().NoPaging, VSOL().VersionCmd, ShowSNMPCmd} {
		if strings.Contains(s, factoryPass) {
			t.Fatal("the vendor password is in a dialect string")
		}
	}
	if Redact("login admin "+factoryPass) == "" {
		t.Fatal("unreachable")
	}
}
