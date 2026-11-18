package sites

import (
	"strings"
	"testing"
)

func ipoeInput(stage string) ScriptInput {
	in := baseInput()
	in.Stage = stage
	in.Site.AccessMode = "ipoe_observe"
	in.Site.WANList = "WAN"
	in.Accounts = []LeaseSeed{
		{Code: "K7Q2MX", Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01", IP: "10.5.50.60", MaxLimit: "20M/20M"},
		{Code: "P4R8TN", Flat: `Flat "10A" $x`, MAC: "00:E0:FC:12:34:56", IP: "10.5.50.61", MaxLimit: "10M/10M", Suspended: true},
	}
	return in
}

func TestIPoEStageGolden(t *testing.T) {
	for _, c := range []struct{ stage, file string }{
		{StageJoin, "join_ipoe_join.rsc"},
		{StageCutover, "join_ipoe_cutover.rsc"},
		{StageCleanup, "join_ipoe_cleanup.rsc"},
	} {
		t.Run(c.stage, func(t *testing.T) {
			out, err := RenderJoinScript(ipoeInput(c.stage))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "{{") || strings.Contains(string(out), "<no value>") {
				t.Fatalf("unrendered template text:\n%s", out)
			}
			golden(t, c.file, out)
		})
	}
}

// Every object a stage creates carries an "ftn:" comment, and no stage
// touches the WAN interface, routes, users or services other than www.
func TestIPoEStagesCommentEverythingAndStayOffTheWAN(t *testing.T) {
	for _, stage := range []string{StageJoin, StageCutover, StageCleanup} {
		out, err := RenderJoinScript(ipoeInput(stage))
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		for _, l := range strings.Split(s, "\n") {
			tl := strings.TrimSpace(l)
			if strings.HasPrefix(tl, "#") {
				continue
			}
			for _, bad := range []string{"/ip/route", "/ip/service disable", "/interface/ethernet", "/ip/dhcp-client", "/interface/bridge/port", "/user/remove", "/ip/firewall/connection", "/system/reset", "/system/reboot"} {
				if strings.Contains(tl, bad) {
					t.Errorf("%s: touches %q: %s", stage, bad, tl)
				}
			}
			if strings.Contains(tl, "/ip/service") && !strings.Contains(tl, "set www") {
				t.Errorf("%s: touches a service other than www: %s", stage, tl)
			}
			// An object-creating command: add with something that is not a find/getter.
			if (strings.Contains(tl, " add ") || strings.HasPrefix(tl, "/system/script add")) && !strings.Contains(tl, `comment="ftn`) && !strings.Contains(tl, "place-before") {
				t.Errorf("%s: object without an ftn: comment: %s", stage, tl)
			}
			// The firewall helper puts the comment after place-before.
			if strings.Contains(tl, "place-before") && strings.Count(tl, `comment="ftn: `) != 2 {
				t.Errorf("%s: firewall rule without an ftn: comment on both branches: %s", stage, tl)
			}
		}
		if strings.Contains(s, "{{") {
			t.Errorf("%s: unrendered", stage)
		}
	}
}

func TestCutoverStageOrderAndSafety(t *testing.T) {
	out, err := RenderJoinScript(ipoeInput(StageCutover))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// The plan's order: backup, safety net, discovery, objects, firewall, switch, checks.
	last := -1
	for _, m := range []string{"cutover 0/7", "cutover 1/7", "/system/backup/save", "/export file=ftn-pre-ipoe", "cutover 2/7", "ftn-rollback-timer", "cutover 3/7",
		"/ip/hotspot/get ($hs->0) interface", "cutover 4/7", "ftn-unreg", "lease-time=10m authoritative=yes disabled=yes", "cutover 5/7", "cutover 6/7", "/ip/hotspot disable", "/ip/dhcp-server enable", "cutover 7/7", "/ping 1.1.1.1", "/ping 10.99.0.1"} {
		i := strings.Index(s, m)
		if i < 0 {
			t.Fatalf("script lacks %q", m)
		}
		if i < last {
			t.Fatalf("%q is out of order", m)
		}
		last = i
	}
	for _, want := range []string{
		`/ip/dhcp-server/lease add address=10.5.50.60 mac-address=AA:BB:CC:DD:EE:01 server=ftn-dhcp comment="ftn:acct:K7Q2MX Flat 3B"`,
		`/queue/simple add name=ftn-K7Q2MX target=10.5.50.60/32 max-limit=20M/20M comment="ftn:acct:K7Q2MX Flat 3B"`,
		`comment="ftn:acct:P4R8TN Flat \"10A\" \$x"`, // quoting survives
		`/ip/firewall/address-list add list=ftn-suspended address=10.5.50.61 disabled=yes`,
		`/ip/firewall/address-list enable [find where list="ftn-suspended"`,
		`protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-unreg`,
		`chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-suspended to-addresses=203.0.113.10 to-ports=80`,
		`chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24`,
		`/ip/firewall/filter remove [find where comment~"^ftn: ipoe"]`,
		`interval=1m on-event=ftn-rollback-tick`,
		`/system/script set ftn-rollback source=$rb`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("cutover lacks %q", want)
		}
	}
	// Walled flats are matched before the established/related accept (open connections of a
	// suspended flat are cut), and the accept sits above where FastTrack would be: rules are
	// added last-first, so the est/rel rule is rendered before the walled ones.
	if strings.Index(s, "ftn: ipoe customer est/rel out") > strings.Index(s, "ftn: ipoe walled reject tcp ftn-suspended") {
		t.Error("walled rules must end up above the established/related accept")
	}
	if !strings.Contains(s, `comment="`+timerPrefix+`0"`) {
		t.Error("the timer's comment must start at the tick count the tick script parses")
	}
	// The old services are only disabled, never removed.
	for _, bad := range []string{"/ip/hotspot remove", "/ip/hotspot/remove", "/ip/dhcp-server remove", "/ip/dhcp-server/remove", "/ip/pool remove"} {
		if strings.Contains(s, bad) {
			t.Errorf("cutover removes: %s", bad)
		}
	}
	// The script removes itself on both exits.
	if strings.Count(s, `/file remove [find where name="ftn-cutover.rsc"]`) != 2 {
		t.Error("self-removal on both exits")
	}
	// The rollback script comes from the discovered names and every line is guarded.
	if rb := rollbackBase("hotspot1"); !strings.Contains(rb, `/ip hotspot enable [find where name="hotspot1"]`) || strings.Count(rb, "on-error={}") != 5 {
		t.Fatalf("rollback script: %s", rb)
	}
	if strings.Index(rollbackBase("h"), "disable [find where name=\"ftn-dhcp\"]") > strings.Index(rollbackBase("h"), "hotspot enable") {
		t.Fatal("ftn-dhcp must be disabled before the old server is enabled")
	}
}

func TestIPoEStageRefusals(t *testing.T) {
	bad := map[string]func(*ScriptInput){
		"cutover without a public ip": func(in *ScriptInput) { in.Stage, in.PublicIP = StageCutover, "" },
		"flat with a bad MAC":         func(in *ScriptInput) { in.Stage = StageCutover; in.Accounts[0].MAC = "nope" },
		"flat outside the subnet":     func(in *ScriptInput) { in.Stage = StageCutover; in.Accounts[0].IP = "192.168.1.5" },
		"injected code":               func(in *ScriptInput) { in.Stage = StageCutover; in.Accounts[0].Code = `X"; /system reset` },
		"injected speed":              func(in *ScriptInput) { in.Stage = StageCutover; in.Accounts[0].MaxLimit = `1M/1M"; x` },
		"join without keys":           func(in *ScriptInput) { in.Stage, in.PrivateKey = StageJoin, "" },
		"unknown stage":               func(in *ScriptInput) { in.Stage = "wipe" },
		"unsafe hotspot name":         func(in *ScriptInput) { in.Stage = StageCleanup; in.Site.HotspotServer = `a"b` },
	}
	for name, mut := range bad {
		in := ipoeInput(StageCutover)
		mut(&in)
		if _, err := RenderJoinScript(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// Cutover and cleanup carry no key material.
	in := ipoeInput(StageCutover)
	in.PrivateKey, in.RESTPass = "", ""
	out, err := RenderJoinScript(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "private-key") || strings.Contains(string(out), baseInput().RESTPass) {
		t.Fatal("cutover must not hold secrets")
	}
}

func TestJoinCommandStages(t *testing.T) {
	got := JoinCommandStage("https://b.example/", "TOK", true, StageCutover)
	want := `/tool/fetch url="https://b.example/j/TOK?stage=cutover" dst-path=ftn-cutover.rsc check-certificate=yes; :delay 1s; /import ftn-cutover.rsc`
	if got != want {
		t.Fatal(got)
	}
	if fb := JoinCommandStage("https://b.example", "T", false, StageJoin); !strings.Contains(fb, `url="https://b.example/j/T?stage=join&nocheck=1"`) || !strings.Contains(fb, "check-certificate=no") {
		t.Fatal(fb)
	}
	if JoinCommandStage("https://b.example", "T", true, "") != JoinCommand("https://b.example", "T", true) {
		t.Fatal("no stage = the legacy command")
	}
}
