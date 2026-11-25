package olt

import (
	"context"
	"strings"
	"testing"

	"ispbill/internal/store"
)

const (
	testACSPass = "Pa55word99ZZ"
	testACSUser = "ftn-a1b2c3"
)

func (e *env) withACS() {
	e.svc.o.ACS = func() *ACS {
		return &ACS{URL: "http://10.99.0.1:7547/", User: testACSUser, Pass: testACSPass, InformSeconds: 300}
	}
}

func (e *env) noSecretsAnywhere(t *testing.T, texts ...string) {
	t.Helper()
	all := append([]string{e.auditText(), e.alertText()}, texts...)
	for _, s := range all {
		for _, secret := range []string{testACSPass, testACSUser} {
			if strings.Contains(s, secret) {
				t.Fatalf("the ACS login (%q) leaked:\n%s", secret, s)
			}
		}
	}
}

// The ACS URL and the shared login go to the OLT in a Pri profile (tr069_mng), are hidden in every plan, audit text and
// result, and do not change the plan's hash.
func TestPlanTR069InPriProfileAndLoginHidden(t *testing.T) {
	e := newEnv(t)
	e.withACS()
	e.f.SetRunning("")
	e.enable(t)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(p.Commands(), "\n")
	for _, want := range []string{
		"profile pri name ftn_pri", "wan_adv index 1 route mode internet mtu 1500", "wan_adv index 1 vlan tag wan_vlan 100 0 qos enable",
		"wan_adv index 2 route mode tr069", "wan_adv index 2 route ipv4 dhcp nat disable", "wan_adv index 2 vlan tag wan_vlan 99 0",
		"tr069_mng enable acs_server url http://10.99.0.1:7547/ username " + testACSUser + " password " + testACSPass +
			" certificate disable inform enable inform_interval 300 reverse_connection username " + testACSUser + " password " + testACSPass,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("plan lacks %q:\n%s", Redact(want), Redact(all))
		}
	}
	if strings.Contains(all, "web UI") || strings.Contains(strings.Join(p.Notes, " "), "web UI") {
		t.Error("the plan still sends the owner to the OLT web UI for TR-069")
	}
	shown := p.Display()
	var b strings.Builder
	for _, st := range shown.Steps {
		b.WriteString(strings.Join(st.Lines, "\n"))
	}
	if strings.Contains(b.String(), testACSPass) || strings.Contains(b.String(), testACSUser) || !strings.Contains(b.String(), "tr069_mng enable acs_server url http://10.99.0.1:7547/") {
		t.Fatalf("display copy not redacted:\n%s", b.String())
	}
	// The hash does not depend on the secret.
	e.svc.o.ACS = func() *ACS {
		return &ACS{URL: "http://10.99.0.1:7547/", User: "ftn-other1", Pass: "AnotherPass12", InformSeconds: 300}
	}
	if _, h2, _ := e.svc.Plan(context.Background(), oltID); h2 != h {
		t.Fatalf("the plan hash changed with the secret: %s %s", h, h2)
	}
	e.withACS()
	res, err := e.svc.Apply(context.Background(), oltID, h, "owner")
	if err != nil {
		t.Fatal(err)
	}
	// The OLT got the real login; nothing the server keeps or shows has it.
	if !strings.Contains(strings.Join(e.f.Lines(), "\n"), "password "+testACSPass) {
		t.Fatal("the OLT was not given the login")
	}
	sent := strings.Join(res.Sent, "\n")
	var tr []string
	for _, en := range res.Entries {
		tr = append(tr, en.Cmd, en.Out, en.Err)
	}
	e.noSecretsAnywhere(t, sent, strings.Join(tr, "\n"), strings.Join(res.Remaining, "\n"))
	if !res.Verified {
		t.Fatalf("not verified: %v", res.Remaining)
	}
	if _, err := e.svc.Backup(context.Background(), oltID, "manual", "owner"); err != nil {
		t.Fatal(err)
	}
	st := e.state()
	_, text, err := e.svc.BackupText(oltID, st.Backups[len(st.Backups)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	e.noSecretsAnywhere(t, text)
	if p2, _, _ := e.svc.Plan(context.Background(), oltID); !p2.Empty() {
		t.Fatalf("second plan not empty: %s", Redact(strings.Join(p2.Commands(), "\n")))
	}
}

// Without an ACS (off, or no login yet) the plan has no TR-069 lines and says so.
func TestPlanWithoutACS(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning("")
	p, _, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(p.Commands(), "\n"), "tr069") && strings.Contains(strings.Join(p.Commands(), "\n"), "tr069_mng") {
		t.Fatal("tr069_mng planned without an ACS")
	}
	if !strings.Contains(strings.Join(p.Notes, " "), "no TR-069 lines") {
		t.Fatalf("notes: %v", p.Notes)
	}
	// A login the CLI cannot take as one word is not sent.
	e.svc.o.ACS = func() *ACS { return &ACS{URL: "http://10.99.0.1:7547/", User: "a b", Pass: "x", InformSeconds: 300} }
	p, _, _ = e.svc.Plan(context.Background(), oltID)
	if strings.Contains(strings.Join(p.Commands(), "\n"), "tr069_mng") || !strings.Contains(strings.Join(p.Notes, " "), "one word") {
		t.Fatalf("a bad login was planned: %v", p.Notes)
	}
}

// An ONU that is on the OLT already (the bench's ONU 1) gets a second WAN index for TR-069 and the OMCI login, line
// by line; it is not bound to the Pri profile (its internet WAN would be doubled).
func TestPlanExistingONUGetsTR069WAN(t *testing.T) {
	e := newEnv(t)
	e.withACS()
	_ = e.st.Update(func(d *store.Data) error {
		x := d.Equipment["ont1"]
		x.OLTID, x.PONPort = oltID, 1
		d.Equipment["ont1"] = x
		return nil
	})
	e.f.SetRunning(base + `interface gpon 0/1
  onu add 1 profile default sn VSOL00000001
  onu 1 profile line name line_hgu
  onu 1 profile srv name srv_hgu
  onu 1 pri wan_adv add route
  onu 1 pri wan_adv index 1 route mode internet mtu 1500
  onu 1 pri wan_adv index 1 vlan tag wan_vlan 100 0 qos enable
`)
	e.enable(t)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(redactAll(p.Commands()), "\n")
	for _, want := range []string{"interface gpon 0/1", "onu 1 pri wan_adv add route", "onu 1 pri wan_adv index 2 route mode tr069", "onu 1 pri wan_adv index 2 route ipv4 dhcp nat disable",
		"onu 1 pri wan_adv index 2 vlan tag wan_vlan 99 0", "onu 1 pri wan_adv commit", "onu 1 pri tr069_mng enable acs_server url http://10.99.0.1:7547/"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "index 1") || strings.Contains(got, "profile pri") || strings.Contains(got, "onu 1 profile") {
		t.Errorf("the plan touches what the ONU already has:\n%s", got)
	}
	if _, err := e.svc.Apply(context.Background(), oltID, h, "owner"); err != nil {
		t.Fatal(err)
	}
	if p2, _, _ := e.svc.Plan(context.Background(), oltID); !p2.Empty() {
		t.Fatalf("not idempotent: %s", Redact(strings.Join(p2.Commands(), "\n")))
	}
	e.noSecretsAnywhere(t)
	// Only the login line is missing when the WAN is there already.
	e.f.SetRunning(base + `interface gpon 0/1
  onu add 1 profile default sn VSOL00000001
  onu 1 profile line name line_hgu
  onu 1 profile srv name srv_hgu
  onu 1 pri wan_adv index 2 route mode tr069
`)
	p, _, _ = e.svc.Plan(context.Background(), oltID)
	var c []string
	for _, st := range p.Steps {
		if st.Title == "PON 1: ONUs already on the OLT" {
			c = st.Lines
			if !st.Independent {
				t.Error("the step of ONUs already on the OLT is not independent")
			}
		}
	}
	if len(c) != 3 || !strings.HasPrefix(c[1], "onu 1 pri tr069_mng enable") {
		t.Fatalf("%s", Redact(strings.Join(c, "\n")))
	}
}

// The OLT's own auto-learn is a finding while provisioning is off and an owner-visible plan step once it is on.
func TestAutoLearnFindingAndStep(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base + "interface gpon 0/1\n  onu auto-learn\n  plug_and_play enable vlan 1\n")
	p, _, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "Auto-learn is on: the OLT accepts any ONU by itself") {
		t.Fatalf("warnings: %v", p.Warnings)
	}
	if strings.Contains(strings.Join(p.Commands(), "\n"), "auto-learn") {
		t.Fatalf("auto-learn is turned off with provisioning off: %v", p.Commands())
	}
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	if f := strings.Join(e.state().Status.Findings, "\n"); !strings.Contains(f, "Auto-learn is on: the OLT accepts any ONU by itself") {
		t.Fatalf("the finding is not on the OLT page: %v", e.state().Status.Findings)
	}
	e.enable(t)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	var step *Step
	for i := range p.Steps {
		if strings.Contains(p.Steps[i].Title, "stop the OLT authorising ONUs by itself") {
			step = &p.Steps[i]
		}
	}
	if step == nil || strings.Join(step.Lines, "|") != "interface gpon 0/1|no onu auto-learn|plug_and_play disable|exit" {
		t.Fatalf("steps: %+v", p.Steps)
	}
	if _, err := e.svc.Apply(context.Background(), oltID, h, "owner"); err != nil {
		t.Fatal(err)
	}
	p, _, _ = e.svc.Plan(context.Background(), oltID)
	if !p.Empty() || len(p.Warnings) != 0 {
		t.Fatalf("after the change: %v %v", p.Commands(), p.Warnings)
	}
}

// The OLT prints the serial in lower-case hex; the inventory has upper case. The line is sent in the OLT's own form
// (vendor letters upper case, hex lower case), and reading it back matches the inventory.
func TestSerialCaseRoundTrip(t *testing.T) {
	e := newEnv(t)
	_ = e.st.Update(func(d *store.Data) error {
		x := d.Equipment["ont1"]
		x.Serial = "VSOL00127E0F"
		d.Equipment["ont1"] = x
		return nil
	})
	e.f.SetRunning(base)
	e.f.AutoFind = [][2]string{{"1", "vsol00127e0f"}}
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	e.enable(t)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil || len(p.Adds) != 1 || p.Adds[0].Serial != "VSOL00127E0F" {
		t.Fatalf("%+v %v", p.Adds, err)
	}
	res, err := e.svc.Apply(context.Background(), oltID, h, "owner")
	if err != nil || !res.Verified {
		t.Fatalf("%v %v", err, res.Remaining)
	}
	joined := strings.Join(e.f.Lines(), "\n")
	if !strings.Contains(joined, "onu add 1 profile default sn VSOL00127e0f") {
		t.Fatalf("not sent in the OLT's form:\n%s", joined)
	}
	if !strings.Contains(e.f.Running(), "sn VSOL00127e0f") {
		t.Fatalf("the fake does not print the serial like the bench OLT:\n%s", e.f.Running())
	}
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	if on := e.state().Status.ONUs; len(on) != 1 || on[0].Serial != "VSOL00127E0F" {
		t.Fatalf("%+v", on)
	}
}

// show version only works in configuration mode and the ONU tables only inside the PON interface: the poll uses them
// that way and no command is refused.
func TestPollUsesBenchModes(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning(base)
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	st := e.state()
	if !st.Status.OK || st.Status.Raw != "" || st.Status.Model != "V1600GS-R" {
		t.Fatalf("%+v", st.Status)
	}
	lines := e.f.Lines()
	for i, l := range lines {
		if strings.HasPrefix(l, "show onu ") {
			ok := false
			for j := i - 1; j >= 0; j-- {
				if strings.HasPrefix(lines[j], "interface gpon") {
					ok = true
					break
				}
				if lines[j] == "exit" || lines[j] == "end" {
					break
				}
			}
			if !ok {
				t.Fatalf("%q was sent outside a PON interface: %v", l, lines)
			}
		}
	}
}

// A default route the OLT already has is never doubled or replaced by the plan; it is a warning instead.
func TestPlanKeepsAnExistingDefaultRoute(t *testing.T) {
	d := VSOL()
	want := Desired{InternetVLAN: 100, MgmtVLAN: 98, MgmtIP: "10.7.3.2", MgmtMask: "255.255.255.248", MgmtGateway: "10.7.3.1"}
	for _, tc := range []struct {
		run   string
		route bool
	}{
		{"vlan 1\nexit\n", true},
		{"ip route 0.0.0.0/0 10.7.3.1\n", false},
		{"ip route 0.0.0.0/0 192.168.8.1\n", false},
		{"ip route 0.0.0.0 0.0.0.0 192.168.8.1\n", false},
	} {
		p := d.BuildPlan(want, ParseRunning(tc.run))
		got := strings.Contains(strings.Join(p.Commands(), "\n"), "ip route 0.0.0.0/0 10.7.3.1")
		if got != tc.route {
			t.Errorf("%q: default route planned %v, want %v", tc.run, got, tc.route)
		}
		if strings.Contains(tc.run, "192.168.8.1") && !strings.Contains(strings.Join(p.Warnings, " "), "already has a default route (via 192.168.8.1)") {
			t.Errorf("%q: no warning: %v", tc.run, p.Warnings)
		}
	}
}

// A batch that stopped right after "onu 1 pri wan_adv add route" leaves a WAN with no index lines: the next plan
// configures that WAN (index 2) instead of adding a third one.
func TestPlanReusesAHalfMadeWAN(t *testing.T) {
	d := VSOL()
	acs := &ACS{URL: "http://10.99.0.1:7547/", User: testACSUser, Pass: testACSPass, InformSeconds: 300}
	want := Desired{InternetVLAN: 100, TR069VLAN: 99, ACS: acs, ONUs: []DesiredONU{{PON: 1, ID: 1, Serial: "VSOL00127E0F"}}, OnlyONUs: true}
	base := "interface gpon 0/1\nonu add 1 profile default sn VSOL00127e0f\nonu 1 pri wan_adv add route\nonu 1 pri wan_adv index 1 route mode internet mtu 1500\n"
	for _, tc := range []struct {
		extra string
		add   bool
	}{{"", true}, {"onu 1 pri wan_adv add route\n", false}} {
		cmds := strings.Join(d.BuildPlan(want, ParseRunning(base+tc.extra+"exit\n")).Commands(), "\n")
		if strings.Contains(cmds, "onu 1 pri wan_adv add route") != tc.add || !strings.Contains(cmds, "onu 1 pri wan_adv index 2 route mode tr069") {
			t.Errorf("%q:\n%s", tc.extra, Redact(cmds))
		}
	}
}

// A Pri profile on the OLT that lacks lines (made by hand, or a batch that stopped half way) is a warning, never edited.
func TestPlanWarnsAboutAHalfMadePriProfile(t *testing.T) {
	d := VSOL()
	acs := &ACS{URL: "http://10.99.0.1:7547/", User: testACSUser, Pass: testACSPass, InformSeconds: 300}
	want := Desired{InternetVLAN: 100, TR069VLAN: 99, PriProfile: d.DefaultPri, ACS: acs}
	full := strings.Join(d.PriProfileLines(d.DefaultPri, 100, 99, &ACS{URL: acs.URL, User: "x", Pass: "y", InformSeconds: 300}), "\n")
	if w := d.BuildPlan(want, ParseRunning(full+"\n")).Warnings; len(w) != 0 {
		t.Errorf("a complete profile (other login) warned: %v", w)
	}
	half := "profile pri name ftn_pri\nwan_adv add route\nwan_adv index 1 route mode internet mtu 1500\nexit\n"
	p := d.BuildPlan(want, ParseRunning(half))
	if !strings.Contains(strings.Join(p.Warnings, " "), "Pri profile ftn_pri on the OLT is incomplete") || strings.Contains(strings.Join(p.Commands(), "\n"), "profile pri") {
		t.Errorf("%v\n%s", p.Warnings, Redact(strings.Join(p.Commands(), "\n")))
	}
}

func TestPlanSkipsVLAN98WhenManagedThroughTheOLTMgmtPort(t *testing.T) {
	for _, via := range []bool{false, true} {
		st := newEnv(t)
		st.st.Update(func(d *store.Data) error {
			s := d.Sites[siteID]
			if via {
				s.OLTMgmtPort = "ether7"
			} else {
				s.OLTMgmtPort = ""
			}
			d.Sites[siteID] = s
			return nil
		})
		var w Desired
		st.st.View(func(d *store.Data) {
			var err error
			w, _, err = BuildDesired(d, d.Equipment[oltID], st.state(), nil, nil, false, VSOL(), nil)
			if err != nil {
				t.Fatal(err)
			}
		})
		txt := strings.Join(VSOL().BuildPlan(w, &Running{VLANs: map[int]bool{}}).Commands(), "\n")
		has := strings.Contains(txt, "vlan 98") // "vlan 98" and "switchport hybrid vlan 98 tagged"
		if via && has {
			t.Errorf("management port: the plan still has VLAN 98 lines:\n%s", txt)
		}
		if !via && !has {
			t.Errorf("manual path: the plan lost VLAN 98 lines:\n%s", txt)
		}
	}
}

// withoutPri is base with its Pri profile block left out (rest unchanged).
func withoutPri(t *testing.T, extra string) string {
	t.Helper()
	i := strings.Index(base, "profile pri id 1 name ftn_pri\n")
	j := strings.Index(base[i:], "  commit\n")
	if i < 0 || j < 0 {
		t.Fatal("base changed")
	}
	return base[:i] + base[i+j+len("  commit\n"):] + extra
}

const onu1 = `interface gpon 0/1
  onu add 1 profile default sn VSOL00000001
  onu 1 profile line name line_hgu
  onu 1 profile srv name srv_hgu
`

// The bench: a backup right before an Apply showed `profile pri id 1 name ftn_pri` / `exit` with no body. The plan warns,
// does not make the profile again (it is never edited), binds no ONU to it, and gives new ONUs TR-069 per ONU. The ONUs
// already on the OLT are their own, independent step.
func TestPlanEmptyPriProfileIsNotBoundToONUs(t *testing.T) {
	d := VSOL()
	acs := &ACS{URL: "http://10.99.0.1:7547/", User: testACSUser, Pass: testACSPass, InformSeconds: 300}
	want := Desired{InternetVLAN: 100, TR069VLAN: 99, LineProfile: d.DefaultLine, SrvProfile: d.DefaultSrv, PriProfile: d.DefaultPri, ONUProfile: "default", ACS: acs,
		ONUs: []DesiredONU{{PON: 1, Serial: "VSOL00000001"}, {PON: 1, Serial: "VSOL00000009", Add: true, Label: "B2"}}}
	run := ParseRunning(withoutPri(t, "profile pri id 1 name ftn_pri\nexit\n"+onu1))
	p := d.BuildPlan(want, run)
	if !strings.Contains(strings.Join(p.Warnings, " "), "Pri profile ftn_pri on the OLT is incomplete") {
		t.Fatalf("no warning: %v", p.Warnings)
	}
	var have, added *Step
	for i := range p.Steps {
		st := &p.Steps[i]
		switch st.Title {
		case "PON 1: ONUs already on the OLT":
			have = st
		case "PON 1: new ONUs":
			added = st
		}
		if strings.HasPrefix(st.Title, "Pri profile") {
			t.Fatalf("the incomplete profile is made again: %v", st.Lines)
		}
	}
	if have == nil || !have.Independent || !strings.Contains(strings.Join(have.Lines, "\n"), "onu 1 pri tr069_mng enable") {
		t.Fatalf("existing ONU step: %+v", have)
	}
	if added == nil || added.Independent || added.NewONUs != 1 {
		t.Fatalf("new ONU step: %+v", added)
	}
	nl := strings.Join(added.Lines, "\n")
	if strings.Contains(nl, "profile pri") || !strings.Contains(nl, "onu 2 pri wan_adv index 1 route mode tr069") || !strings.Contains(nl, "onu 2 pri tr069_mng enable") {
		t.Fatalf("new ONU lines:\n%s", Redact(nl))
	}
	// With no profile at all, the plan makes it (marked unverified) last of the base steps, and binds new ONUs to it.
	p = d.BuildPlan(want, ParseRunning(withoutPri(t, onu1)))
	pri := -1
	for i, st := range p.Steps {
		if strings.HasPrefix(st.Title, "Pri profile") {
			pri = i
			if !strings.Contains(st.Unverified, "commit") {
				t.Errorf("the Pri profile step is not marked unverified: %q", st.Unverified)
			}
		}
	}
	if pri < 0 || !strings.HasPrefix(p.Steps[pri+1].Title, "PON ") {
		t.Fatalf("the Pri profile is not the last base step: %v", p.Steps)
	}
	if !strings.Contains(strings.Join(p.Commands(), "\n"), "onu 2 profile pri") {
		t.Fatal("new ONUs not bound to the profile the plan makes")
	}
}

// The OLT refuses a line of the new Pri profile: the new ONUs (which need it) are skipped, ONU 1 already on the OLT still
// gets its TR-069 lines, and that is saved. The error says which step failed, what was done and what was skipped.
func TestApplyPriProfileRefusedStillGivesExistingONUsTR069(t *testing.T) {
	e := newEnv(t)
	e.withACS()
	_ = e.st.Update(func(d *store.Data) error {
		x := d.Equipment["ont1"]
		x.OLTID, x.PONPort = oltID, 1
		d.Equipment["ont1"] = x
		return nil
	})
	e.f.SetRunning(withoutPri(t, onu1))
	e.f.AutoFind = [][2]string{{"2", "VSOL00000002"}} // a new ONU, ready, on PON 2
	if err := e.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatal(err)
	}
	e.enable(t)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil || len(p.Adds) != 1 {
		t.Fatalf("plan: %v %v", p.Adds, err)
	}
	e.f.FailOn = "wan_adv index 1 bind ssid ssid1" // a line only the Pri profile has
	e.f.Reset()
	writes := e.f.Writes()
	res, err := e.svc.Apply(context.Background(), oltID, h, "owner")
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{"the OLT refused a line in step Pri profile ftn_pri", "PON 1: ONUs already on the OLT (saved)", "skipped: PON 2: new ONUs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	sent := strings.Join(e.f.Lines(), "\n")
	if !strings.Contains(sent, "onu 1 pri tr069_mng enable") || strings.Contains(sent, "onu add") {
		t.Fatalf("sent:\n%s", Redact(sent))
	}
	if e.f.Writes() != writes+1 || !res.SavedPartly {
		t.Fatalf("not saved: %d writes, %+v", e.f.Writes()-writes, res.Failed)
	}
	if !strings.Contains(e.auditText(), "FAILED") || !strings.Contains(e.alertText(), "configuration stopped") {
		t.Fatal("no audit or alert")
	}
	e.noSecretsAnywhere(t, msg)
	// The ONU that was skipped is not recorded as authorised.
	var eq store.Equipment
	e.st.View(func(d *store.Data) { eq = d.Equipment["ont2"] })
	if eq.OLTID == oltID {
		t.Fatal("a skipped ONU was placed on the OLT")
	}
}
