package olt

import (
	"os"
	"strings"
	"testing"

	"ispbill/internal/store"
)

// The sample lines are the bench OLT's own output (V1600GS-R, V1.0.5).

func TestParseONUInfoBenchRow(t *testing.T) {
	rows, bad := ParseONUInfo(1, "GPON0/1:1  V414                 default                sn      VSOL00127e0f\n")
	if bad != "" || len(rows) != 1 {
		t.Fatalf("%+v %q", rows, bad)
	}
	r := rows[0]
	if r.PON != 1 || r.ID != 1 || r.Serial != "VSOL00127E0F" {
		t.Fatalf("%+v", r)
	}
	// A header and an empty table: nothing, and nothing to complain about.
	if rows, bad = ParseONUInfo(1, "OnuIndex  Model  Profile  Mode  SN/LOID\n---------\n\n"); len(rows) != 0 || bad != "" {
		t.Fatalf("%+v %q", rows, bad)
	}
	// Something that is not a row is kept as it came.
	if _, bad = ParseONUInfo(1, "the table is being rebuilt\n"); !strings.Contains(bad, "rebuilt") {
		t.Fatalf("unparsed lost: %q", bad)
	}
}

const benchOptical = `ONU ID                     : 1
Alarm                      : enable
Piggyback DBA rpt mode     : mode 0 only
Rx optical level(ONU)      : -8.94
Lower rx optical threshold : ont internal policy
Upper rx optical threshold : ont internal policy
Tx optical level           : 2.55 
Lower tx optical threshold : ont internal policy
Upper tx optical threshold : ont internal policy
ONU response time          : 0
Power feed voltage         : 3.30(V) 
Laser bias current         : 8.160(mA) 
Temperature                : 24.465(C) 

`

func TestMergeOpticalBlocksBenchSample(t *testing.T) {
	rows := []store.OLTONU{{PON: 1, ID: 1, Serial: "VSOL00127E0F"}, {PON: 1, ID: 2}}
	if bad := MergeOptical(rows, 1, benchOptical); bad != "" {
		t.Fatalf("unparsed %q", bad)
	}
	r := rows[0]
	if r.RxPower != "-8.94" || r.TxPower != "2.55" || r.Voltage != "3.30" || r.Bias != "8.160" || r.Temp != "24.465" {
		t.Fatalf("%+v", r)
	}
	if rows[1].RxPower != "" {
		t.Fatalf("another ONU got the values: %+v", rows[1])
	}
	// Two blocks, one for an ONU the table does not list: reported, not guessed.
	two := benchOptical + strings.Replace(benchOptical, ": 1\n", ": 7\n", 1)
	if bad := MergeOptical(rows, 1, two); !strings.Contains(bad, "ONU 7") {
		t.Fatalf("unknown ONU not reported: %q", bad)
	}
}

func TestMergeStateIsTolerant(t *testing.T) {
	rows := []store.OLTONU{{PON: 1, ID: 1, Serial: "VSOL00127E0F"}}
	// Any layout that leads with the ONU index works; the state word is read from the line.
	rows, bad := MergeState(rows, 1, "OnuIndex Admin State OMCC State Phase State\nGPON0/1:1   enable  enable  working\nGPON0/1:2   enable  enable  LOS\n")
	if bad != "" || len(rows) != 2 {
		t.Fatalf("%+v %q", rows, bad)
	}
	if !rows[0].Online || rows[0].State != "online" || rows[1].Online || rows[1].State != "los" {
		t.Fatalf("%+v", rows)
	}
	// A layout it cannot read is returned as it came, not guessed.
	_, bad = MergeState(rows, 1, "state table unavailable\n")
	if !strings.Contains(bad, "unavailable") {
		t.Fatalf("%q", bad)
	}
}

func TestMergeDistance(t *testing.T) {
	rows := []store.OLTONU{{PON: 1, ID: 1}, {PON: 1, ID: 2}}
	MergeDistance(rows, 1, "GPON0/1:1   120 m\nGPON0/1:2   nothing useful\n")
	if rows[0].Distance != "120" || rows[1].Distance != "" {
		t.Fatalf("%+v", rows)
	}
}

func TestAutoFindEmptyAndSerialCase(t *testing.T) {
	if got := ParseAutoFindOn(1, ""); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	got := ParseAutoFindOn(1, "GPON0/1:--  vsol00127e0f  unknown\n")
	if len(got) != 1 || got[0].Serial != "VSOL00127E0F" || got[0].PON != 1 {
		t.Fatalf("%+v", got)
	}
	// No PON in the line: the PON the command ran in.
	got = ParseAutoFindOn(2, "VSOL00127e0f\n")
	if len(got) != 1 || got[0].PON != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestParseVersionBench(t *testing.T) {
	v := ParseVersion(`  Olt Serial Number:           V2609170360
  Olt Device Model:            V1600GS-R
  Hardware Version:            V7.1.1
  Software Version:            V1.0.5
  Software Created Time:       Tue, 14 Jul 2026 14:35:36 
  Copyright 2020-2026
`)
	if v.Model != "V1600GS-R" || v.Serial != "V2609170360" || v.Hardware != "V7.1.1" || v.Software != "V1.0.5" || !strings.HasPrefix(v.Built, "Tue, 14 Jul 2026") {
		t.Fatalf("%+v", v)
	}
	if got := v.Summary(); got != "V1600GS-R, hw V7.1.1, sw V1.0.5, serial V2609170360" {
		t.Fatal(got)
	}
	d := VSOL()
	if n := d.VersionNote(v.Model, v.Software); n != "" {
		t.Fatalf("a checked OLT got a warning: %q", n)
	}
	if n := d.VersionNote("V1600GS-R", "V1.0.7"); !strings.Contains(n, "checked against V1600GS-R software V1.0.5") || !strings.Contains(n, "V1.0.7") {
		t.Fatalf("%q", n)
	}
	if d.VersionNote("", "") != "" {
		t.Fatal("an OLT that did not say its version got a warning")
	}
	// An unknown layout keeps its first lines.
	if v := ParseVersion("something else\nand more\n"); v.Summary() == "" {
		t.Fatal("raw text lost")
	}
}

func benchRunning(t *testing.T) *Running {
	t.Helper()
	b, err := os.ReadFile("testdata/bench-running-config.txt")
	if err != nil {
		t.Fatal(err)
	}
	return ParseRunning(string(b))
}

func TestParseRunningBenchConfig(t *testing.T) {
	r := benchRunning(t)
	for _, v := range []int{1, 99, 100} {
		if !r.VLANs[v] {
			t.Errorf("vlan %d missing (%v)", v, r.VLANs)
		}
	}
	// Profiles are found by kind and name, whatever their id.
	for _, h := range []string{"profile dba name dba_1g", "profile srv name srv_hgu", "profile line name line_hgu", "profile onu name V2804N-Z", "profile dba name default1"} {
		if !r.HasBlock(h) {
			t.Errorf("no block %q", h)
		}
	}
	if !r.Has("profile line name line_hgu", "service-port 2 gemport 1 uservlan 99 vlan 99") || !r.Has("profile dba name dba_1g", "type 4 maximum 1000000") {
		t.Error("profile lines lost (indentation must be ignored)")
	}
	// The ONU, whatever the case of its serial.
	pon, o, ok := r.ONUBySerial("vsol00127E0F")
	if !ok || pon != 1 || o.ID != 1 || o.Profile != "default" {
		t.Fatalf("%v %+v %v", pon, o, ok)
	}
	if !r.Has("interface gpon 0/1", "onu 1 profile line name line_hgu") || !r.Has("interface gpon 0/1", "onu 1 profile srv name srv_hgu") {
		t.Error("ONU profile bindings lost")
	}
	if len(o.Lines) < 8 {
		t.Errorf("the ONU's lines: %v", o.Lines)
	}
	if !r.AutoLearn[1] || !r.PlugPlay[1] {
		t.Errorf("auto-learn %v plug-and-play %v", r.AutoLearn, r.PlugPlay)
	}
	if !r.Has("interface gigabitethernet 0/1", "switchport hybrid vlan 99 tagged") || !r.Has("interface vlan 1", "ip address 192.168.8.200/24") {
		t.Error("interface lines lost")
	}
}

func TestRunningFindings(t *testing.T) {
	f := strings.Join(benchRunning(t).Findings(), "\n")
	for _, want := range []string{"Auto-learn is on: the OLT accepts any ONU by itself", "default communities", "blocked by the login access list", "Telnet", "SSH server is enabled"} {
		if !strings.Contains(f, want) {
			t.Errorf("no finding %q in:\n%s", want, f)
		}
	}
	// The bench's login and enable passwords differ in the redacted copy only by being the same "***": identical.
	if !strings.Contains(f, "login password and enable password are the same") {
		t.Errorf("same-password finding missing:\n%s", f)
	}
	if strings.Contains(f, "***") {
		t.Error("a password value is in a finding")
	}
}
