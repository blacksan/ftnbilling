package sites

import (
	"regexp"
	"strings"
	"testing"
)

// extractKit returns the unescaped kit script sources from a rendered join script.
func extractKit(t *testing.T, script string) (tick, rollback string) {
	t.Helper()
	un := strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\$`, "$", `\\`, `\`)
	re := regexp.MustCompile(`/system/script add name=(ftn-kit-[a-z]+) policy=\S+ source="(.*)" comment="ftn: kit v1"`)
	for _, m := range re.FindAllStringSubmatch(script, -1) {
		if m[1] == KitTickScript {
			tick = un.Replace(m[2])
		} else if m[1] == KitRollback {
			rollback = un.Replace(m[2])
		}
	}
	if tick == "" || rollback == "" {
		t.Fatal("kit scripts not found in the join script")
	}
	return
}

func TestKitInstalledByBothJoinScripts(t *testing.T) {
	for _, c := range []struct {
		name string
		in   ScriptInput
	}{{"legacy", baseInput()}, {"join", ipoeInput(StageJoin)}} {
		out, err := RenderJoinScript(c.in)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		for _, w := range []string{
			`/system/scheduler add name=ftn-kit-timer interval=30s on-event=ftn-kit-tick`,
			`comment="ftn: kit v1"`,
			`:if ([:tostr [/system/device-mode/get scheduler]] ~ "^(true|yes)$") do={`,
			`cutover kit: skipped, the scheduler is off in device-mode`,
			`cutover kit NOT installed`,
		} {
			if !strings.Contains(s, w) {
				t.Errorf("%s: lacks %q", c.name, w)
			}
		}
		// Legacy names stay free for the paste cutover and the cleanup script.
		if strings.Contains(s, "ftn-rollback") {
			t.Errorf("%s: the kit must not use the legacy ftn-rollback names", c.name)
		}
	}
}

func TestKitTickAndRollbackText(t *testing.T) {
	tick := kitTickSource()
	for _, w := range []string{
		`list="ftn-cmd" and comment="backup"`,
		`/system backup save name=ftn-pre-ipoe`, `/export file=ftn-pre-ipoe`,
		`[/file find where name="ftn-pre-ipoe.backup"]] = 0`,
		`list="ftn-armed"`, `$c = "now"`, `$n >= 30`, `/system script run ftn-kit-rollback`,
	} {
		if !strings.Contains(tick, w) {
			t.Errorf("tick lacks %q", w)
		}
	}
	rb := kitRollbackSource("hotspot1")
	for _, w := range []string{
		`/ip dhcp-server disable [find where name="ftn-dhcp"]`, `/ip hotspot enable [find where name="hotspot1"]`,
		`/queue simple remove [find where comment~"^ftn:acct:"]`,
		`list="ftn-undo"`, `$k = "hotspot"`, `$k = "dhcp"`, `$k = "arp"`,
		`/interface bridge set [find where name=$ifn] arp=$mode`,
		`/ip firewall address-list remove [find where list="ftn-armed"]`, `/ip firewall address-list remove [find where list="ftn-undo"]`,
	} {
		if !strings.Contains(rb, w) {
			t.Errorf("rollback lacks %q", w)
		}
	}
	// The old timer removal is not part of the kit's rollback (the kit timer must survive it).
	if strings.Contains(rb, "/system scheduler remove") || strings.Contains(rb, "ftn-rollback-timer") {
		t.Error("kit rollback removes a scheduler")
	}
	// An undo row's comment is only ever data: never evaluated as code.
	for _, bad := range []string{":parse", ":execute", "[[", "/system script run $", "/system script add"} {
		if strings.Contains(rb, bad) || strings.Contains(tick, bad) {
			t.Errorf("kit evaluates data as code: %q", bad)
		}
	}
	if KitUndoHotspot("hs") != "hotspot|hs" || KitUndoDHCP("d") != "dhcp|d" || KitUndoARP("bridge", "br-x", "enabled") != "arp|bridge|br-x|enabled" {
		t.Fatal("undo comment formats")
	}
}

// The kit is fixed text: nothing the server or a customer supplies (keys,
// passwords, tokens, names, flats) reaches it. Only the hotspot server's name,
// already restricted to safe characters, appears in its first lines.
func TestKitHasNoServerSuppliedCode(t *testing.T) {
	a := ipoeInput(StageJoin)
	b := ipoeInput(StageJoin)
	b.PrivateKey, b.RESTPass, b.Token = "ZZZZkey", "ZZZZpass", "ZZZZtoken"
	b.Site.Name, b.Site.CustomerSubnet, b.PublicBase = `Other "name" $x`, "10.7.0.0/24", "https://other.example"
	b.Accounts = nil
	oa, err := RenderJoinScript(a)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := RenderJoinScript(b)
	if err != nil {
		t.Fatal(err)
	}
	ta, ra := extractKit(t, string(oa))
	tb, rb := extractKit(t, string(ob))
	if ta != tb || ra != rb {
		t.Fatal("the kit changed with server-supplied input")
	}
	for _, secret := range []string{"ZZZZ", "billing.example", "Block A", "10.5.50", "10.99.0", "wg-ftn", "Flat"} {
		if strings.Contains(ta+ra, secret) {
			t.Errorf("kit contains %q", secret)
		}
	}
	// Another hotspot name changes only that name.
	c := ipoeInput(StageJoin)
	c.Site.HotspotServer = "hs-other"
	oc, _ := RenderJoinScript(c)
	_, rc := extractKit(t, string(oc))
	if strings.ReplaceAll(rc, "hs-other", "hotspot1") != ra {
		t.Fatal("the hotspot name is not the only variable part of the kit")
	}
}

func TestKitScriptsGetNoServerNamesInTheTimer(t *testing.T) {
	out, _ := RenderJoinScript(ipoeInput(StageJoin))
	s := string(out)
	if strings.Count(s, "ftn-kit-timer") < 2 || !strings.Contains(s, `/system/scheduler remove [find where name="ftn-kit-timer"]`) {
		t.Fatal("re-running the join must replace the kit, not duplicate it")
	}
}

// A first cutover run can stop after it created ftn-rollback (the scheduler was
// blocked); the second run then sees the script and must still record what it
// will change, before it changes it.
func TestCutoverRecordsRestoreLinesOnEveryRun(t *testing.T) {
	out, err := RenderJoinScript(ipoeInput(StageCutover))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	add := strings.Index(s, "/system/script add name=ftn-rollback policy")
	closeIf := strings.Index(s[add:], "\n    }\n") + add // end of ":if (!$rbExists)"
	rec := strings.Index(s, "    :foreach h in=$hs do={ :set lines")
	if add < 0 || rec < closeIf {
		t.Fatalf("the restore lines are recorded inside the first-run-only branch (add %d, close %d, rec %d)", add, closeIf, rec)
	}
	for _, k := range []string{"bridge", "vlan", "ethernet"} {
		line := `:do { /interface ` + k + ` set [find where name=\"" . $ifc . "\"] arp=")`
		r := strings.Index(s, line)
		sw := strings.Index(s, "/interface/"+k+"/find where name=$ifc] do={ /interface/"+k+" set $x arp=reply-only")
		if r < 0 || sw < 0 || r > sw {
			t.Errorf("%s: arp restore line missing or after the change (%d, %d)", k, r, sw)
		}
	}
	if !strings.Contains(s, `:if ($cur = "reply-only") do={ :set cur "enabled" }`) {
		t.Error("no safety value when the mode is already reply-only")
	}
	if strings.Contains(s, ":local rbs") {
		t.Error("the old record-at-switch path is still there")
	}
}
