package sites

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		os.WriteFile(p, got, 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file (run: go test ./internal/sites -run %s -update)\n--- got ---\n%s", name, t.Name(), got)
	}
}

func baseInput() ScriptInput {
	return ScriptInput{
		Site: store.Site{ID: "sab12cd", Name: "Block A", Mode: store.SiteModeWG, TunnelIP: "10.99.0.2", RESTUser: "ftn-api", RESTPort: 8081,
			HotspotServer: "hotspot1", WANInterface: "ether1", WANList: "WAN", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24"},
		PrivateKey: "mFtRkR0kq3B0hH8xYQ1p0Zk9m3m8wq2Fv5s8y8q1m0A=",
		RESTPass:   "Zx3Kp9Qw7Lm2Vb5Nc8Td1Rf4Gh6Jy0Ua",
		ServerPub:  "kJ3b0m2Zs8Xc1Vn5Bq7Lw9Rt4Yu6Ip0Oa2Sd8Fg1Hk=",
		Endpoint:   "203.0.113.10:51820",
		WGSubnet:   netip.MustParsePrefix("10.99.0.0/22"),
		PublicBase: "https://billing.example.co.ke",
		PublicIP:   "203.0.113.10",
		Generated:  time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC),
		Token:      "TOKEN-abc_123",
	}
}

func TestJoinScriptGolden(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ScriptInput)
	}{
		{"join_plain.rsc", func(in *ScriptInput) {}}, // Provision off: the router already has its hotspot
		{"join_provision_vlan.rsc", func(in *ScriptInput) {
			in.Site.Provision, in.Site.VLANID, in.Site.BenchPort = true, 30, "ether5"
			in.ROSVersion = "7.25.1" // the :onerror wrapper form
		}},
		{"join_provision_vlan0.rsc", func(in *ScriptInput) { in.Site.Provision = true }},
		{"join_nocheck.rsc", func(in *ScriptInput) { in.NoCheck = true; in.ROSVersion = "7.13" }},
		{"join_legacy.rsc", func(in *ScriptInput) {
			in.Site.LegacyContainer = true
			in.Site.Name = `Old "main" building $1`
		}},
	}
	for _, c := range cases {
		t.Run(strings.TrimSuffix(c.name, ".rsc"), func(t *testing.T) {
			in := baseInput()
			c.mut(&in)
			out, err := RenderJoinScript(in)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(out, []byte("{{")) || bytes.Contains(out, []byte("}}")) || bytes.Contains(out, []byte("<no value>")) {
				t.Fatalf("unrendered template text:\n%s", out)
			}
			golden(t, c.name, out)
		})
	}
}

func TestJoinScriptContentRules(t *testing.T) {
	in := baseInput()
	in.Site.Provision, in.Site.VLANID = true, 30
	b, err := RenderJoinScript(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`address=10.99.0.2 network=10.99.0.1 interface=wg-ftn`,
		`allowed-address=10.99.0.1/32 persistent-keepalive=25s`,
		`endpoint-address=203.0.113.10 endpoint-port=51820`,
		`policy=read,write,test,sensitive,api,rest-api,reboot`,
		`/ip/service set www port=8081 disabled=no address=10.99.0.1/32`,
		`:if ([:len [/ip/firewall/filter find where dynamic=no]] > 0)`,
		`place-before=([/ip/firewall/filter find where dynamic=no]->0)`,
		`ranges=10.5.50.10-10.5.50.254`,
		`login-by=http-pap,http-chap,mac-cookie`,
		`https://billing.example.co.ke/routeros/hotspot/sab12cd/`,
		`name=vlan30-customers vlan-id=30`,
		`/file remove [find where name="ftn-join.rsc"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(s, "traffic-flow") {
		t.Error("the join script must not set traffic flow: the server does it over REST")
	}
	if strings.Contains(s, "172.17.0.0/24") || strings.Contains(s, "/container/") {
		t.Error("legacy parts without the legacy flag")
	}
	if strings.Contains(s, ":global") {
		t.Error("no router-side globals")
	}
	// Every object the script creates is commented "ftn:".
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, " add ") && !strings.HasPrefix(strings.TrimSpace(l), "#") && !strings.Contains(l, `comment="ftn:`) && !strings.Contains(l, "name=hotspot1") && !strings.Contains(l, "hsprof") {
			t.Errorf("object without an ftn: comment: %s", l)
		}
	}
	// Section order is fixed.
	last := -1
	for _, m := range []string{"ftn 0/9", "ftn 1/9", "ftn 2/9", "ftn 3/9", "ftn 4/9", "ftn 5/9", "ftn 6/9", "ftn 7/9", "ftn 9/9"} {
		i := strings.Index(s, m)
		if i < last {
			t.Fatalf("%s out of order", m)
		}
		last = i
	}
	// REST access, then the drop-everything-else rule, go in last-to-first so the
	// accept rules end up above the drops.
	if strings.Index(s, "ftn: wg no forward out") > strings.Index(s, "ftn: wg drop other input") ||
		strings.Index(s, "ftn: wg drop other input") > strings.Index(s, "ftn: wg rest") {
		t.Error("firewall rules are not added bottom-up")
	}
}

func TestJoinScriptRefusesBadInput(t *testing.T) {
	bad := map[string]func(*ScriptInput){
		"tunnel outside subnet": func(in *ScriptInput) { in.Site.TunnelIP = "192.168.1.5" },
		"no tunnel ip":          func(in *ScriptInput) { in.Site.TunnelIP = "" },
		"unsafe interface":      func(in *ScriptInput) { in.Site.OLTInterface = `ether1"; /system reset-configuration; "` },
		"unsafe bench":          func(in *ScriptInput) { in.Site.BenchPort = "a b" },
		"subnet too big":        func(in *ScriptInput) { in.Site.CustomerSubnet = "10.0.0.0/16" },
		"subnet too small":      func(in *ScriptInput) { in.Site.CustomerSubnet = "10.5.50.0/25" },
		"bad endpoint":          func(in *ScriptInput) { in.Endpoint = "billing.example.co.ke" },
		"bad base":              func(in *ScriptInput) { in.PublicBase = `https://x"y` },
		"no key":                func(in *ScriptInput) { in.PrivateKey = "" },
	}
	for name, mut := range bad {
		in := baseInput()
		mut(&in)
		if _, err := RenderJoinScript(in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPlanSubnet(t *testing.T) {
	p, err := PlanSubnet("10.5.50.0/24")
	if err != nil || p.Gateway != "10.5.50.1" || p.GatewayIP != "10.5.50.1/24" || p.PoolFrom != "10.5.50.10" || p.PoolTo != "10.5.50.254" || p.Network != "10.5.50.0/24" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = PlanSubnet("10.8.16.77/20") // host bits are masked off
	if err != nil || p.Network != "10.8.16.0/20" || p.Gateway != "10.8.16.1" || p.PoolTo != "10.8.31.254" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range []string{"", "x", "10.0.0.0/19", "10.0.0.0/25", "fd00::/64"} {
		if _, err := PlanSubnet(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestJoinCommand(t *testing.T) {
	got := JoinCommand("https://b.example/", "TOK", true)
	for _, w := range []string{
		`:local o "^(true|yes)$"; :if (`,
		`([:tostr [/system/device-mode/get fetch]] ~ $o) && ([:tostr [/system/device-mode/get scheduler]] ~ $o) && ([:tostr [/system/device-mode/get hotspot]] ~ $o)`,
		`do={ /tool/fetch url="https://b.example/j/TOK" dst-path=ftn-join.rsc check-certificate=yes; :delay 1s; /import ftn-join.rsc }`,
		`else={ :put "ftn: press the router button within 5 minutes (it then reboots), then paste this same line again"; /system/device-mode/update`,
		`/system/device-mode/update fetch=yes scheduler=yes hotspot=yes }`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("command lacks %q:\n%s", w, got)
		}
	}
	// Exactly one device-mode update, and it comes after the fetch branch: the line never loops.
	if strings.Count(got, "/system/device-mode/update") != 1 || strings.Count(got, "/import") != 1 {
		t.Error("one update, one import")
	}
	if long := JoinCommand("https://billing.example.co.ke", strings.Repeat("x", 43), true); len(long) > 560 {
		t.Errorf("the line is %d characters; keep it pasteable", len(long))
	}
	// No hotspot requirement for the join stage of a building that is not (yet) hotspot-mode.
	if j := JoinCommandFor("https://b.example", "T", true, "", false); strings.Contains(j, "get hotspot") {
		t.Error("hotspot=false must not require hotspot")
	}
	// Fresh router: fetch only, so usually no button press; serving router: the full set.
	fresh := JoinCommandIntent("https://b.example", "T", true, StageJoin, IntentFresh, true)
	if !strings.Contains(fresh, "get fetch") || strings.Contains(fresh, "get scheduler") || strings.Contains(fresh, "get hotspot") || !strings.Contains(fresh, "/system/device-mode/update fetch=yes }") {
		t.Errorf("fresh intent needs fetch only: %s", fresh)
	}
	serving := JoinCommandIntent("https://b.example", "TOK", true, "", IntentServing, true)
	if serving != got {
		t.Error("serving intent is the default command")
	}
	if sv := JoinCommandIntent("https://b.example", "T", true, StageJoin, IntentServing, false); !strings.Contains(sv, "get scheduler") || strings.Contains(sv, "get hotspot") {
		t.Errorf("serving, not hotspot mode: fetch+scheduler: %s", sv)
	}
	fb := JoinCommand("https://b.example", "T", false)
	if !strings.Contains(fb, "check-certificate=no") || !strings.Contains(fb, `url="https://b.example/j/T?nocheck=1"`) {
		t.Fatal("fallback: ", fb)
	}
}

func TestJoinScriptCertificateChecks(t *testing.T) {
	render := func(noCheck bool) string {
		in := baseInput()
		in.NoCheck = noCheck
		b, err := RenderJoinScript(in)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := render(false); strings.Contains(s, "check-certificate=no") || strings.Count(s, "check-certificate=yes") != 3 { // the page downloads, and the report on both exits
		t.Error("default script must check certificates for the page downloads")
	}
	if s := render(true); strings.Contains(s, "check-certificate=yes") || strings.Count(s, "check-certificate=no") != 3 {
		t.Error("no-check script must skip the check for the page downloads")
	}
}

// The script file holds the WireGuard private key and the REST password, so it
// must be removed whether the script finishes or stops on an error, and no
// firewall rule may be placed relative to a dynamic rule.
func TestJoinScriptRemovesItselfOnEveryExit(t *testing.T) {
	b, err := RenderJoinScript(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	rm := `/file remove [find where name="ftn-join.rsc"]`
	if strings.Count(s, rm) != 2 {
		t.Fatalf("want a removal on the success path and on the abort path, got %d", strings.Count(s, rm))
	}
	open, handler := strings.Index(s, ":do {\n\n:put \"ftn 0/9"), strings.LastIndex(s, "} on-error={\n")
	if open < 0 || handler < open || strings.Index(s[handler:], rm) < 0 || !strings.Contains(s[handler:], ":error ") {
		t.Fatal("the body is not wrapped in a do/on-error that removes the file and re-raises")
	}
	if strings.Contains(s, "/ip/firewall/filter find]") {
		t.Error("a firewall find without where dynamic=no")
	}
	if strings.Contains(s, `:error "ftn: section`) {
		t.Error("section errors must go through $abort so the wrapper can report them")
	}
}
