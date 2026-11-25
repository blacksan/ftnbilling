package olt

import (
	"context"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

// The tests' saves wait less for late output after the prompt (no fake OLT sends any, except where a test says so).
func init() { settleQuiet, resyncQuiet = 100*time.Millisecond, 30*time.Millisecond }

// The `show snmp-server community` table of the bench OLT (V1600GS-R, V1.0.5), exactly as captured.
const benchSNMPTable = `community               access
 public                    ro 
 private                   rw 
`

func TestParseSNMPCommunitiesBenchTable(t *testing.T) {
	got := ParseSNMPCommunities(benchSNMPTable)
	if len(got) != 2 || got[0] != (SNMPCommunity{"public", "ro"}) || got[1] != (SNMPCommunity{"private", "rw"}) {
		t.Fatalf("parsed %+v", got)
	}
	def, other := defaultCommunities(benchSNMPTable)
	if strings.Join(def, ",") != "public,private" || len(other) != 0 {
		t.Fatalf("defaults %v other %v", def, other)
	}
	// Another community next to them.
	def, other = defaultCommunities(benchSNMPTable + " monitor                   ro \n")
	if len(def) != 2 || len(other) != 1 || other[0] != "monitor" {
		t.Fatalf("defaults %v other %v", def, other)
	}
	// After the removal only the header is left (or nothing at all): no row, and nothing is reported as left.
	for _, empty := range []string{"community               access\n", "", "\n\n", "% No community.\n"} {
		if r := ParseSNMPCommunities(empty); len(r) != 0 {
			t.Errorf("%q parsed as %+v", empty, r)
		}
		if def, other := defaultCommunities(empty); len(def) != 0 || len(other) != 0 {
			t.Errorf("%q: defaults %v other %v", empty, def, other)
		}
	}
	// The header alone must not make "community" or "access" a community.
	if def, other := defaultCommunities("community               access\n"); len(def)+len(other) != 0 {
		t.Fatal("the header was read as a row")
	}
}

func TestParseACLLenient(t *testing.T) {
	run := "snmp-server start\nlogin-access-list deny telnet 0.0.0.0 0.0.0.0\nlogin-access-list ipv6 deny telnet ::/0\nlogin-access-list deny snmp 0.0.0.0 0.0.0.0\nlogin-access-list ipv6 deny snmp ::/0\nlogin-access-list enable\n"
	st := ParseACL(run, "")
	if len(st.Rules) != 4 || st.Enabled == nil || !*st.Enabled {
		t.Fatalf("%+v", st)
	}
	if !st.Has(ACLRule{Action: "deny", Proto: "telnet", Addr: "0.0.0.0", Mask: "0.0.0.0"}) || !st.Has(ACLRule{V6: true, Action: "deny", Proto: "snmp", Addr: "::/0"}) {
		t.Fatalf("%+v", st.Rules)
	}
	if st.HasRule("deny", "web") || st.HasRule("deny", "ssh") {
		t.Fatal("rules that are not there")
	}
	// `show login-access-list` in a few possible layouts; rules found in both places count once.
	for name, show := range map[string]string{
		"lines": "login-access-list permit ssh 192.168.8.1 255.255.255.255\nlogin-access-list deny ssh 0.0.0.0 0.0.0.0\nlogin-access-list ipv6 deny ssh ::/0\nlogin-access-list enable\n",
		"table": "Login access list: enabled\nType    Service  Address     Mask\npermit  ssh      192.168.8.1 255.255.255.255\ndeny    ssh      0.0.0.0     0.0.0.0\nipv6 deny ssh ::/0\n",
		"cols":  "ssh permit 192.168.8.1 255.255.255.255\nssh deny 0.0.0.0 0.0.0.0\nssh deny ipv6 ::/0\n",
	} {
		st := ParseACL("", show)
		for _, r := range []ACLRule{{Action: "permit", Proto: "ssh", Addr: "192.168.8.1", Mask: "255.255.255.255"}, {Action: "deny", Proto: "ssh", Addr: "0.0.0.0", Mask: "0.0.0.0"}, {V6: true, Action: "deny", Proto: "ssh", Addr: "::/0"}} {
			if !st.Has(r) {
				t.Errorf("%s: %v not found in %q", name, r, st.RuleStrings())
			}
		}
	}
	both := ParseACL(run, run)
	if len(both.Rules) != 4 {
		t.Fatalf("rules counted twice: %v", both.RuleStrings())
	}
	// Disabled, from either place.
	if st := ParseACL("login-access-list disable\n", ""); st.Enabled == nil || *st.Enabled {
		t.Fatal("disable not read")
	}
	if st := ParseACL("", "Login access list: disabled\n"); st.Enabled == nil || *st.Enabled {
		t.Fatal("disabled not read")
	}
	if st := ParseACL("", "something unreadable\n% x\n"); len(st.Rules) != 0 || st.Enabled != nil {
		t.Fatalf("%+v", st)
	}
	// "no ..." lines are not rules.
	if st := ParseACL("no login-access-list deny ssh 0.0.0.0 0.0.0.0\n", ""); len(st.Rules) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestSecureACLPlanRefusesUnusableSources(t *testing.T) {
	d := VSOL()
	for _, bad := range []string{"", "0.0.0.0", "255.255.255.255", "127.0.0.1", "224.0.0.1", "::1", "10.0.0.1/24", "x", "300.1.1.1", "10.0.0.1 255.0.0.0"} {
		if _, err := d.SecureACLPlan(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	p, err := d.SecureACLPlan("10.99.0.1")
	if err != nil || p.Permit != "login-access-list permit ssh 10.99.0.1 255.255.255.255" {
		t.Fatalf("%+v %v", p, err)
	}
}

// aclRun runs the SSH restriction step again on an OLT that is already secured (the passwords are the stored ones).
func (a *aenv) aclRun(t *testing.T) SecureResult {
	t.Helper()
	user, login, enable := a.opened(t)
	var res SecureResult
	err := a.svc.withSession(context.Background(), oltID, true, func(o *opened) error {
		vi := ParseVersion(mustShow(t, o))
		a.svc.secureACL(context.Background(), o, user, login, enable, vi.Serial, "olga", &res)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustShow(t *testing.T, o *opened) string {
	t.Helper()
	out, err := o.sess.ShowAt(context.Background(), ModeConfig, "show version")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// secured makes an adopted and secured OLT, then forgets the lines the securing sent.
func securedEnv(t *testing.T, mut func(*aenv)) *aenv {
	t.Helper()
	a := newAdoptEnv(t)
	if mut != nil {
		mut(a)
	}
	a.adopt(t)
	return a
}

func TestSecureACLPermitOnlyVerifyThenWrite(t *testing.T) {
	a := securedEnv(t, nil)
	ev := a.f.Events()
	iPermit := indexEv(ev, "cmd login-access-list permit ssh "+routerAddr+" 255.255.255.255", 0)
	iVerify := indexEv(ev, "login admin true", iPermit+1)
	iWrite := indexEv(ev, "cmd write", iVerify+1)
	if iPermit < 0 || iVerify < iPermit || iWrite < iVerify {
		t.Fatalf("wrong order (permit, new session, write):\n%s", strings.Join(ev, "\n"))
	}
	// Only the permit: no deny line (the OLT refuses one beside a permit), no enable (the list was on), no telnet/snmp/web.
	for _, l := range a.f.Lines() {
		if strings.Contains(l, "deny") || l == "login-access-list enable" || strings.Contains(l, "telnet") || strings.Contains(l, " web ") || (strings.Contains(l, "snmp") && !strings.HasPrefix(l, "no snmp-server community ") && l != ShowSNMPCmd) {
			t.Fatalf("unexpected line %q", l)
		}
	}
	ad := a.adoptState()
	if !ad.ACLDone || strings.Join(ad.ACLRules, "|") != strings.Join(a.f.ACLRules(), "|") {
		t.Fatalf("stored rules %v, OLT %v", ad.ACLRules, a.f.ACLRules())
	}
	checkACLRules(t, a.f.ACLRules(), routerAddr)
	if !strings.HasPrefix(ad.ACLNote, "SSH limited to the router (the OLT allows SSH only from listed addresses): "+routerAddr) {
		t.Fatalf("note %q", ad.ACLNote)
	}
	if !strings.Contains(ad.ACLFinding, "web") || !strings.Contains(ad.ACLFinding, "HTTPS") || strings.Contains(ad.ACLFinding, "IPv6") {
		t.Fatalf("finding: %q", ad.ACLFinding)
	}
	// The allowlist is in force: another address is refused, the router's is not.
	a.f.ClientAddr = "10.77.0.9"
	if err := a.svc.Poll(context.Background(), oltID, true); err == nil {
		t.Fatal("a session from another address logged in")
	}
	a.f.ClientAddr = ""
	if err := a.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// Two writes in the whole Secure: the passwords' own, and the access list's (which also saves the SNMP removal).
	if w := a.f.Writes(); w != 2 {
		t.Fatalf("%d writes", w)
	}
	v, _ := a.svc.AdoptView(oltID)
	if len(v.ACLRules) != 5 || v.ACLFinding == "" {
		t.Fatalf("view: %+v", v)
	}
}

// The V1600GS-R refuses a deny beside a permit (and the reverse) for one protocol, with this text: a refusal for the driver.
func TestMixedACLRefusalIsAKnownOLTError(t *testing.T) {
	const msg = "add failed, protocol has permit rule,please detele it first!"
	if !VSOL().ErrorRE.MatchString(msg) || !mixedRE.MatchString(msg) || !mixedRE.MatchString("Add failed, protocol has DENY rule") {
		t.Fatal("not recognised")
	}
	a := newAdoptEnv(t)
	a.f.SetRunningACL("permit ssh 10.1.1.1 255.255.255.255")
	a.f.SetCreds("admin", "plainpassword1", "")
	f := a.f
	f.Reset()
	if out := f.ACLLine("login-access-list deny ssh 0.0.0.0 0.0.0.0"); out != msg+"\n" {
		t.Fatalf("fake: %q", out)
	}
	if out := f.ACLLine("login-access-list ipv6 deny ssh ::/0"); !strings.Contains(out, "has permit rule") {
		t.Fatalf("fake v6: %q", out)
	}
}

// An OLT whose list already denies SSH by rule (deny-list mode) is left alone: a permit cannot go beside a deny.
func TestSecureACLLeavesADenyListAlone(t *testing.T) {
	a := securedEnv(t, func(a *aenv) { a.f.SetRunningACL("deny ssh 10.5.5.0 255.255.255.0") })
	ad := a.adoptState()
	if ad.ACLDone || !strings.HasPrefix(ad.ACLNote, "Couldn't restrict SSH; left open.") || !strings.Contains(ad.ACLNote, "already denies SSH by rule (deny ssh 10.5.5.0 255.255.255.0)") {
		t.Fatalf("note %q", ad.ACLNote)
	}
	for _, l := range a.f.Lines() {
		if strings.Contains(l, "login-access-list") && !strings.HasPrefix(l, "show ") {
			t.Fatalf("sent %q", l)
		}
	}
	// The SNMP removal is still saved, by the one save at the end.
	if w := a.f.Writes(); w != 2 || !strings.Contains(ad.SNMPNote, "SNMP is off") {
		t.Fatalf("%d writes, %q", w, ad.SNMPNote)
	}
}

// IPv6: nothing is sent; a finding only when the OLT has an IPv6 address.
func TestSecureACLIPv6FindingOnlyWithAnIPv6Address(t *testing.T) {
	a := securedEnv(t, func(a *aenv) { a.f.SetRunning("interface vlan 98\n ipv6 address 2001:db8::2/64\n") })
	ad := a.adoptState()
	if !ad.ACLDone || !strings.Contains(ad.ACLFinding, "IPv6 SSH is not restricted") {
		t.Fatalf("%q / %q", ad.ACLNote, ad.ACLFinding)
	}
	for _, l := range a.f.Lines() {
		if strings.Contains(l, "ipv6") && strings.Contains(l, "login-access-list") {
			t.Fatalf("sent %q", l)
		}
	}
}

// failedRestrict checks the restore path: SSH left open, the access list not saved, the rules back to the factory ones,
// and the new passwords still the ones in use.
func failedRestrict(t *testing.T, a *aenv, wantWhy string) {
	t.Helper()
	ad := a.adoptState()
	if ad.ACLDone || !strings.HasPrefix(ad.ACLNote, "Couldn't restrict SSH; left open.") || !strings.Contains(ad.ACLNote, wantWhy) {
		t.Fatalf("note %q done=%v (want %q)", ad.ACLNote, ad.ACLDone, wantWhy)
	}
	if got := strings.Join(a.f.ACLRules(), "|"); got != "deny telnet 0.0.0.0 0.0.0.0|ipv6 deny telnet ::/0|deny snmp 0.0.0.0 0.0.0.0|ipv6 deny snmp ::/0" {
		t.Fatalf("rules left on the OLT: %s", got)
	}
	// Writes: one for the passwords and one at the end for the SNMP removal, after the rules were undone.
	if w := a.f.Writes(); w != 2 {
		t.Fatalf("%d writes (want 2: passwords and SNMP)", w)
	}
	ev := a.f.Events()
	iP := indexEv(ev, "cmd login-access-list permit ssh ", 0)
	iUnP := indexEv(ev, "cmd no login-access-list permit ssh ", 0)
	iW := indexEv(ev, "cmd write", iUnP+1)
	if iP < 0 || iUnP < iP || iW < iUnP {
		t.Fatalf("the permit must be removed before the last write:\n%s", strings.Join(ev, "\n"))
	}
	if ad.ACLRules != nil || strings.Contains(a.auditText(), "SSH limited to") {
		t.Fatalf("a failed restriction was recorded as done: %v", ad.ACLRules)
	}
	if !strings.Contains(a.auditText(), "left open") {
		t.Fatalf("no audit:\n%s", a.auditText())
	}
	if !ad.Secured {
		t.Fatal("the secured state was lost")
	}
	if err := a.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatalf("poll after the restore: %v", err)
	}
}

func TestSecureACLRestoresWhenNewSessionIsBlocked(t *testing.T) {
	// The OLT sees the server as another address than the one that was permitted: the allowlist blocks the new session.
	a := securedEnv(t, func(a *aenv) { a.f.ClientAddr = "10.77.0.9" })
	failedRestrict(t, a, "could not log in")
}

func TestSecureACLWrongRouterAddressInStoreIsRefusedBeforeSending(t *testing.T) {
	a := newAdoptEnv(t)
	a.rebuild(func(o *Options) {
		o.SSHSource = func(d *store.Data, eq store.Equipment, s store.OLTSettings) (string, error) { return "0.0.0.0", nil }
	})
	a.adopt(t)
	ad := a.adoptState()
	if ad.ACLDone || !strings.HasPrefix(ad.ACLNote, "Couldn't restrict SSH; left open.") {
		t.Fatalf("%q", ad.ACLNote)
	}
	for _, l := range a.f.Lines() {
		if strings.Contains(l, "login-access-list") && !strings.HasPrefix(l, "show ") {
			t.Fatalf("sent %q", l)
		}
	}
	// No source function at all: skipped with a note, nothing sent.
	b := newAdoptEnv(t)
	b.rebuild(func(o *Options) { o.SSHSource = nil })
	b.adopt(t)
	if b.adoptState().ACLDone || b.adoptState().ACLNote == "" {
		t.Fatal("the step should be skipped with a note")
	}
}

func TestSecureACLRefusedLineRestores(t *testing.T) {
	// The list is off; the OLT refuses the enable line: the permit that went in is removed again.
	a := securedEnv(t, func(a *aenv) { a.f.SetACLEnabled(false); a.f.FailOn = "login-access-list enable" })
	ad := a.adoptState()
	if ad.ACLDone || !strings.Contains(ad.ACLNote, "refused") || !strings.HasPrefix(ad.ACLNote, "Couldn't restrict SSH; left open.") {
		t.Fatalf("%q", ad.ACLNote)
	}
	if got := strings.Join(a.f.ACLRules(), "|"); strings.Contains(got, "ssh") {
		t.Fatalf("ssh rules left: %s", got)
	}
}

func TestSecureACLWriteFailureRestores(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	// Second run on the secured OLT with the rule removed again by hand, and write now failing.
	a.f.Reset()
	removeSSHRules(t, a)
	a.f.FailWrite = true
	res := a.aclRun(t)
	if res.ACLDone || !strings.Contains(res.ACLNote, "would not save") || !strings.HasPrefix(res.ACLNote, "Couldn't restrict SSH; left open.") {
		t.Fatalf("%+v", res)
	}
	if got := strings.Join(a.f.ACLRules(), "|"); strings.Contains(got, "ssh") {
		t.Fatalf("ssh rules left: %s", got)
	}
}

func removeSSHRules(t *testing.T, a *aenv) {
	t.Helper()
	// Done through the fake's own CLI via a session, as an owner would on the console.
	err := a.svc.withSession(context.Background(), oltID, true, func(o *opened) error {
		return o.sess.RunSecure(context.Background(), []string{"no login-access-list permit ssh " + routerAddr + " 255.255.255.255"})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSecureACLIdempotentAndPartial(t *testing.T) {
	a := securedEnv(t, nil)
	// Everything is there: nothing is sent but reads, no write, still verified with a new session.
	a.f.Reset()
	writes := a.f.Writes()
	res := a.aclRun(t)
	if !res.ACLDone || !strings.Contains(res.ACLNote, "already the listed address") {
		t.Fatalf("%+v", res)
	}
	for _, l := range a.f.Lines() {
		if strings.Contains(l, "login-access-list") && !strings.HasPrefix(l, "show ") {
			t.Fatalf("a second run sent %q", l)
		}
	}
	if a.f.Writes() != writes {
		t.Fatal("a second run wrote")
	}
	// The permit is gone: only it is sent, and saved once.
	removeSSHRules(t, a)
	a.f.Reset()
	res = a.aclRun(t)
	if !res.ACLDone {
		t.Fatalf("%+v", res)
	}
	var sent []string
	for _, l := range a.f.Lines() {
		if strings.Contains(l, "login-access-list") && !strings.HasPrefix(l, "show ") {
			sent = append(sent, l)
		}
	}
	if len(sent) != 1 || sent[0] != "login-access-list permit ssh "+routerAddr+" 255.255.255.255" {
		t.Fatalf("sent %v", sent)
	}
	if a.f.Writes() != writes+1 {
		t.Fatalf("writes %d, want one more than %d", a.f.Writes(), writes)
	}
	// Another permit the owner added stays, and the note says so.
	a.f.SetRunningACL("permit ssh 10.20.0.5 255.255.255.255")
	res = a.aclRun(t)
	if !res.ACLDone || !strings.Contains(res.ACLNote, "also allowed from rules the server did not add: permit ssh 10.20.0.5 255.255.255.255") {
		t.Fatalf("%+v", res)
	}
}

func TestSecureACLSwitchesTheListOnLast(t *testing.T) {
	a := newAdoptEnv(t)
	a.f.SetACLEnabled(false)
	a.adopt(t)
	if !a.adoptState().ACLDone || !a.f.ACLEnabled() {
		t.Fatalf("%q enabled=%v", a.adoptState().ACLNote, a.f.ACLEnabled())
	}
	ev := a.f.Events()
	iEn := indexEv(ev, "cmd login-access-list enable", 0)
	iP := indexEv(ev, "cmd login-access-list permit ssh ", 0)
	if iEn < 0 || iEn < iP {
		t.Fatalf("enable must come after the rule:\n%s", strings.Join(ev, "\n"))
	}
	if strings.Join(a.adoptState().ACLRules, "|") != strings.Join(a.f.ACLRules(), "|") {
		t.Fatal("read back differs")
	}
}

func TestSecureACLReadsTheConfigLineLayout(t *testing.T) {
	a := securedEnv(t, func(a *aenv) { a.f.ACLShow = "lines" })
	if !a.adoptState().ACLDone {
		t.Fatalf("%q", a.adoptState().ACLNote)
	}
}

func TestSecureACLWebRuleSilencesNothingAndIsNeverTouched(t *testing.T) {
	// A web deny that the owner has put there stays, and the finding is gone; it is never added or removed by the server.
	a := newAdoptEnv(t)
	a.f.SetRunningACL("deny web 0.0.0.0 0.0.0.0")
	a.adopt(t)
	if !a.adoptState().ACLDone || a.adoptState().ACLFinding != "" {
		t.Fatalf("%q / %q", a.adoptState().ACLNote, a.adoptState().ACLFinding)
	}
	for _, l := range a.f.Lines() {
		if strings.Contains(l, " web ") {
			t.Fatalf("a web rule was sent: %q", l)
		}
	}
	if !strings.Contains(strings.Join(a.f.ACLRules(), "|"), "deny web 0.0.0.0 0.0.0.0") {
		t.Fatal("the web rule is gone")
	}
}

// ---- review additions: lockout safety ----

func TestSecureACLOldSessionDroppedDuringRestoreAlertsPowerCycle(t *testing.T) {
	// The new session is blocked (the OLT sees another address), and the OLT drops the old session at the first undo line:
	// the rule stays in the running configuration, nothing more is saved, and the owners are told to power-cycle the OLT.
	a := securedEnv(t, func(a *aenv) { a.f.ClientAddr = "10.77.0.9"; a.f.DropOn = "no login-access-list" })
	ad := a.adoptState()
	if ad.ACLDone || !strings.HasPrefix(ad.ACLNote, "Couldn't restrict SSH; left open.") || !strings.Contains(ad.ACLNote, "power-cycle the OLT") {
		t.Fatalf("note %q", ad.ACLNote)
	}
	if !strings.Contains(a.alertText(), "power-cycle the OLT") || !strings.Contains(a.alertText(), "Nothing was saved") {
		t.Fatalf("no power-cycle alert: %q", a.alertText())
	}
	if w := a.f.Writes(); w != 1 {
		t.Fatalf("%d writes (want 1: the passwords; never one with the access list)", w)
	}
	if !strings.Contains(ad.SNMPNote, "not saved") {
		t.Fatalf("SNMP note %q", ad.SNMPNote)
	}
	if !strings.Contains(strings.Join(a.f.ACLRules(), "|"), "permit ssh "+routerAddr) {
		t.Fatal("the fake should still hold the unsaved rule")
	}
}

// The undo is refused but the session lives: the final save must not run either (it would keep the unverified rule).
func TestSecureACLStuckUndoBlocksTheFinalSave(t *testing.T) {
	a := securedEnv(t, func(a *aenv) { a.f.ClientAddr = "10.77.0.9"; a.f.FailOn = "no login-access-list" })
	ad := a.adoptState()
	if !strings.Contains(ad.ACLNote, "power-cycle the OLT") || !strings.Contains(ad.SNMPNote, "not saved") {
		t.Fatalf("%q / %q", ad.ACLNote, ad.SNMPNote)
	}
	if w := a.f.Writes(); w != 1 {
		t.Fatalf("%d writes", w)
	}
}

func TestSecureACLSwitchedOnListIsSwitchedOffAgainOnFailure(t *testing.T) {
	// The list was off and already held the router's permit; only `enable` is sent. The new session is then blocked (the
	// OLT sees another address): the undo switches the list off again (no added rule to remove), the list is not saved.
	a := newAdoptEnv(t)
	a.f.SetACLEnabled(false)
	a.f.SetRunningACL("permit ssh " + routerAddr + " 255.255.255.255")
	a.f.ClientAddr = "10.77.0.9"
	a.adopt(t)
	ad := a.adoptState()
	if ad.ACLDone || !strings.Contains(ad.ACLNote, "switched off again") {
		t.Fatalf("note %q", ad.ACLNote)
	}
	if a.f.ACLEnabled() {
		t.Fatal("the list was left on: the server stays locked out until a power cycle")
	}
	if w := a.f.Writes(); w != 2 {
		t.Fatalf("%d writes", w)
	}
	if err := a.svc.Poll(context.Background(), oltID, true); err != nil {
		t.Fatalf("poll after the restore: %v", err)
	}
}

func TestSecureACLRefusesWhenStateUnknownOrWebRulesWouldTurnOn(t *testing.T) {
	sentACL := func(a *aenv) []string {
		var out []string
		for _, l := range a.f.Lines() {
			if strings.Contains(l, "login-access-list") && !strings.HasPrefix(l, "show ") {
				out = append(out, l)
			}
		}
		return out
	}
	a := securedEnv(t, func(a *aenv) { a.f.ACLNoState = true })
	if ad := a.adoptState(); ad.ACLDone || !strings.Contains(ad.ACLNote, "does not show whether the access list is on") {
		t.Fatalf("%q", ad.ACLNote)
	}
	if s := sentACL(a); len(s) != 0 {
		t.Fatalf("sent %v", s)
	}
	b := newAdoptEnv(t)
	b.f.SetACLEnabled(false)
	b.f.SetRunningACL("permit web 10.0.0.5 255.255.255.255")
	b.adopt(t)
	if ad := b.adoptState(); ad.ACLDone || !strings.Contains(ad.ACLNote, "web interface") {
		t.Fatalf("%q", ad.ACLNote)
	}
	if s := sentACL(b); len(s) != 0 {
		t.Fatalf("sent %v", s)
	}
}

func TestSecureACLRestoreOutlivesTheCallersDeadline(t *testing.T) {
	// The web request's context is already cancelled when the step runs: the step and its undo still complete.
	a := securedEnv(t, nil)
	removeSSHRules(t, a)
	user, login, enable := a.opened(t)
	var res SecureResult
	err := a.svc.withSession(context.Background(), oltID, true, func(o *opened) error {
		vi := ParseVersion(mustShow(t, o))
		a.f.ClientAddr = "10.77.0.9" // the new session will come from an address the permit does not list
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		a.svc.secureACL(ctx, o, user, login, enable, vi.Serial, "olga", &res)
		return nil
	})
	a.f.ClientAddr = ""
	if err != nil {
		t.Fatal(err)
	}
	if res.ACLDone || !strings.Contains(res.ACLNote, "removed again") {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(strings.Join(a.f.ACLRules(), "|"), "ssh") {
		t.Fatalf("ssh rules left: %v", a.f.ACLRules())
	}
}

func TestACLDisableOnlyInTheRestorePath(t *testing.T) {
	d := VSOL()
	if d.SecureLineOK("login-access-list disable", false) || d.SecureLineOK("login-access-list clear", false) || d.SecureLineOK("login-access-list clear", true) {
		t.Fatal("disable/clear allowed as a Secure line")
	}
	if !d.SecureLineOK("login-access-list disable", true) {
		t.Fatal("the restore path cannot switch the list off again")
	}
	for _, l := range []string{"login-access-list deny ssh 0.0.0.0 0.0.0.0", "login-access-list ipv6 deny ssh ::/0", "login-access-list deny telnet 0.0.0.0 0.0.0.0", "no login-access-list deny telnet 0.0.0.0 0.0.0.0", "login-access-list permit web 1.2.3.4 255.255.255.255",
		"no login-access-list ipv6 deny snmp ::/0", "login-access-list permit ssh 1.2.3.0 255.255.255.0", "login-access-list deny ssh 10.0.0.0 255.0.0.0"} {
		if d.SecureLineOK(l, true) || d.SecureLineOK(l, false) {
			t.Errorf("allowed: %q", l)
		}
	}
	// A show output that says "enabled" somewhere does not make the state known; "disabled" does (it only adds an enable).
	if st := ParseACL("", "Login access list: enabled\n"); st.Enabled != nil {
		t.Fatal("enabled taken from the show output")
	}
}

// Two writes in one session with the output real OLTs give: "Building configuration...", the prompt before the flash write
// ends, then "[OK]" redrawn with ESC[K and the prompt on the same line. Both saves finish, nothing in between is read one
// prompt behind (the SNMP and access-list steps parse their own output), and there are two writes in the whole Secure.
func TestSecureTwoWritesWithLateOKAndRedraw(t *testing.T) {
	defer func(q time.Duration) { settleQuiet = q }(settleQuiet)
	defer func(q time.Duration) { resyncQuiet = q }(resyncQuiet)
	settleQuiet, resyncQuiet = 700*time.Millisecond, 200*time.Millisecond // the production waits: longer than the fake's flash write below
	a := newAdoptEnv(t)
	a.f.WriteLate, a.f.WriteRedraw, a.f.WriteDelay = true, true, 400*time.Millisecond
	a.adopt(t)
	ad := a.adoptState()
	if !ad.Secured || !ad.ACLDone || !strings.Contains(ad.SNMPNote, "SNMP is off") {
		t.Fatalf("%+v", ad)
	}
	if w := a.f.Writes(); w != 2 {
		t.Fatalf("%d writes", w)
	}
	// And a later save in the same kind of session, at the # prompt after reads, also finishes.
	err := a.svc.withSession(context.Background(), oltID, true, func(o *opened) error {
		_ = mustShow(t, o)
		if err := o.sess.Save(context.Background()); err != nil {
			return err
		}
		_ = mustShow(t, o)
		return o.sess.Save(context.Background())
	})
	if err != nil {
		t.Fatalf("saves: %v", err)
	}
	if v := ParseVersion(a.mustVersion(t)); v.Serial == "" {
		t.Fatal("the session is out of step after the saves")
	}
}

func (a *aenv) mustVersion(t *testing.T) string {
	t.Helper()
	var out string
	err := a.svc.withSession(context.Background(), oltID, true, func(o *opened) error { out = mustShow(t, o); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A save that does not finish says what the OLT showed last (redacted), so the page tells what happened; the SNMP removal
// is then reported unsaved, the access list undone.
func TestSaveTimeoutShowsTheLastOutput(t *testing.T) {
	a := newAdoptEnv(t)
	a.rebuild(func(o *Options) {
		d := VSOL()
		d.SaveTimeout = time.Second
		o.Dialect, o.Timeout = d, time.Second
	})
	a.f.WriteBuilding, a.f.WriteHangAfter = true, 1
	a.adopt(t)
	ad := a.adoptState()
	if !ad.Secured {
		t.Fatalf("the first save should work: %+v", ad)
	}
	if !strings.Contains(ad.ACLNote, "would not save") || !strings.Contains(ad.ACLNote, `the OLT showed last: "write Building configuration..."`) {
		t.Fatalf("ACL note %q", ad.ACLNote)
	}
	if !strings.Contains(ad.SNMPNote, "the save did not finish within 1s") || !strings.Contains(ad.SNMPNote, "Building configuration") {
		t.Fatalf("SNMP note %q", ad.SNMPNote)
	}
	if strings.Contains(strings.Join(a.f.ACLRules(), "|"), "ssh") {
		t.Fatal("the unsaved rule was not undone")
	}
}

func TestOutputTailIsShortOneLineAndRedacted(t *testing.T) {
	long := strings.Repeat("x", 300) + "\r\nuser login-password admin Secret123456789012345678\r\n gpon-olt"
	got := outputTail(long, 200)
	if len([]rune(got)) > 203 || strings.Contains(got, "\n") || strings.Contains(got, "Secret1234") || !strings.HasPrefix(got, "...") {
		t.Fatalf("%q", got)
	}
}
