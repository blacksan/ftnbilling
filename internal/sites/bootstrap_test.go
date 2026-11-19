package sites

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const (
	tPriv = "PRIVATEKEYPRIVATEKEYPRIVATEKEYPRIVATEKEY0="
	tRest = "RESTPASSWORD0123456789abcdefghij"
	tPPP  = "pp-secret-9"
	tTok  = "TOKEN0123456789TOKEN0123456789TOKEN01234"
)

func benchSite() store.Site {
	return store.Site{ID: "s1", Name: "Block A", Mode: store.SiteModeWG, TunnelIP: "10.99.0.2", RESTPort: 8729,
		WANMode: WANDHCP, WANInterface: "ether1", LANPorts: "ether2", LANSubnet: "192.168.88.0/24",
		OLTInterface: "sfp-sfpplus1", VLANID: 100, CustomerSubnet: "10.5.50.0/24", CustomerPorts: "ether4,ether5"}
}

func benchInput(s store.Site) BootstrapInput {
	cfg := config.Config{PublicIP: "203.0.113.10", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24"}
	s.RESTUser = "ispbill"
	return BootstrapInput{Site: ApplyDefaults(cfg, s), PrivateKey: tPriv, RESTPass: tRest, ServerPub: "SERVERPUBKEYSERVERPUBKEYSERVERPUBKEYSERV0=",
		Endpoint: "203.0.113.10:51820", WGSubnet: netip.MustParsePrefix("10.99.0.0/22"), PublicBase: "https://billing.example.co.ke",
		PublicIP: "203.0.113.10", Generated: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), ROSVersion: "7.25.1", Token: tTok, NetflowPort: 2055}
}

func TestBootstrapGolden(t *testing.T) {
	cases := map[string]func(*store.Site, *BootstrapInput){
		"dhcp_vlan_two_customer_ports": func(*store.Site, *BootstrapInput) {},
		"ether_olt_vlan":               func(s *store.Site, _ *BootstrapInput) { s.OLTInterface, s.CustomerPorts = "ether3", "ether5" },
		"sfp_olt_novlan":               func(s *store.Site, _ *BootstrapInput) { s.VLANID, s.CustomerPorts = 0, "ether5" },
		"ether_olt_novlan":             func(s *store.Site, _ *BootstrapInput) { s.OLTInterface, s.VLANID = "ether3", 0 },
		"dhcp_novlan":                  func(s *store.Site, _ *BootstrapInput) { s.VLANID, s.CustomerPorts = 0, "" },
		"static_vlan": func(s *store.Site, _ *BootstrapInput) {
			s.WANMode, s.WANAddress, s.WANGateway, s.WANDNS = WANStatic, "198.51.100.5/24", "198.51.100.1", "9.9.9.9"
		},
		"pppoe_novlan": func(s *store.Site, in *BootstrapInput) {
			s.WANMode, s.PPPoEUser, s.PPPoEPassEnc, s.VLANID, s.CustomerPorts = WANPPPoE, "user1", "sealed", 0, ""
			in.PPPoEPass = tPPP
		},
		"old_ros": func(_ *store.Site, in *BootstrapInput) { in.ROSVersion = "7.12" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := benchSite()
			in := benchInput(s)
			ss := s
			mut(&ss, &in)
			cfg := config.Config{PublicIP: "203.0.113.10", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24"}
			ss.RESTUser = "ispbill"
			in.Site = ApplyDefaults(cfg, ss)
			out, err := RenderBootstrap(in)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "bootstrap_"+name+".rsc")
			if *update {
				os.MkdirAll("testdata", 0o755)
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(out) {
				t.Fatalf("golden %s differs (run go test -update)", path)
			}
			again, _ := RenderBootstrap(in)
			if string(again) != string(out) {
				t.Fatal("not deterministic")
			}
		})
	}
}

// Every "$" that RouterOS would expand inside a string must be escaped; the
// only unescaped dollar signs are variable uses. A "$" followed by a quote or a
// closing bracket is a RouterOS expansion bug.
var bareDollar = regexp.MustCompile(`[^\\]\$["\)]`)

func TestBootstrapDollarEscaping(t *testing.T) {
	in := benchInput(benchSite())
	in.RESTPass = `a$b"c` // the generated password is alphanumeric, but the script must survive anything
	in.RESTPass = "Pa$$w0rd"
	in.Site.WANMode, in.Site.PPPoEUser, in.Site.PPPoEPassEnc = WANPPPoE, `us$er`, "sealed"
	in.PPPoEPass = `p$"w`
	out, err := RenderBootstrap(in)
	if err != nil {
		t.Fatal(err)
	}
	if m := bareDollar.FindString(string(out)); m != "" {
		t.Fatalf("unescaped dollar: %q", m)
	}
	if !strings.Contains(string(out), `Pa\$\$w0rd`) || !strings.Contains(string(out), `p\$\"w`) {
		t.Fatal("secrets are not escaped")
	}
}

func TestBootstrapKeepsSecretsOutOfLogsAndReport(t *testing.T) {
	in := benchInput(benchSite())
	out, err := RenderBootstrap(in)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, ":log") || strings.Contains(line, ":put") || strings.Contains(line, "http-data") {
			for _, secret := range []string{tPriv, tRest, tPPP} {
				if strings.Contains(line, secret) {
					t.Fatalf("secret on a log/report line: %s", line)
				}
			}
		}
	}
	if !strings.Contains(s, "ftn-setup.rsc") || !strings.Contains(s, "ftn-reset.rsc") {
		t.Fatal("the script does not remove its files")
	}
}

func TestBootstrapRefusals(t *testing.T) {
	good := benchInput(benchSite())
	bad := map[string]func(*BootstrapInput){
		"no public ip": func(in *BootstrapInput) { in.PublicIP = "" },
		"admin rest":   func(in *BootstrapInput) { in.Site.RESTUser = "admin" },
		"pppoe no pass": func(in *BootstrapInput) {
			in.Site.WANMode, in.Site.PPPoEUser, in.Site.PPPoEPassEnc = WANPPPoE, "u", "sealed"
		},
		"bad base":       func(in *BootstrapInput) { in.PublicBase = `https://x"y` },
		"lan overlaps":   func(in *BootstrapInput) { in.Site.LANSubnet = "10.5.50.0/24" },
		"lan hits wg":    func(in *BootstrapInput) { in.Site.LANSubnet = "10.99.1.0/24" },
		"no credentials": func(in *BootstrapInput) { in.RESTPass = "" },
	}
	for name, mut := range bad {
		in := good
		mut(&in)
		if _, err := RenderBootstrap(in); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestBootstrapCommand(t *testing.T) {
	c := BootstrapCommand("https://b.example/", "TOK", true)
	for _, want := range []string{`/j/TOK"`, `/j/TOK/reset"`, "check-certificate=yes", "/import ftn-reset.rsc", "device-mode"} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in %s", want, c)
		}
	}
	// the fetches and the import are wrapped: a failure removes both files and says nothing was changed
	wrap := strings.Index(c, ":do { /tool/fetch")
	imp := strings.Index(c, "/import ftn-reset.rsc")
	onErr := strings.Index(c, `on-error={ /file/remove [/file/find where name="ftn-setup.rsc"]; /file/remove [/file/find where name="ftn-reset.rsc"]; :put "ftn: setup failed, nothing was changed, files removed"; :if ($d = 1) do={ :do { /ip/dns/set servers="" } on-error={}; :put "ftn: the DNS servers this line set were put back to empty" } }`)
	if wrap < 0 || imp < wrap || onErr < imp || strings.Count(c, "/tool/fetch") != 2 || wrap > strings.Index(c, "/tool/fetch") {
		t.Fatalf("fetch+fetch+import not wrapped in :do/on-error: %s", c)
	}
	if strings.Contains(c, "\n") || strings.Contains(c, "\r") {
		t.Fatal("the line must be a single line")
	}
	if !strings.Contains(c, `:local o "^(true|yes)\$"`) || strings.Contains(c, `(true|yes)$"`) {
		t.Fatalf("the dollar sign must stay escaped: %s", c)
	}
	// braces balance: the wrapper did not break the structure
	if strings.Count(c, "{") != strings.Count(c, "}") {
		t.Fatalf("unbalanced braces: %s", c)
	}
	n := BootstrapCommand("https://b.example", "TOK", false)
	if !strings.Contains(n, "/j/TOK?nocheck=1") || !strings.Contains(n, "check-certificate=no") || strings.Contains(n, "\n") {
		t.Fatal(n)
	}
}

func TestValidateLayout(t *testing.T) {
	wg := netip.MustParsePrefix("10.99.0.0/22")
	ok := benchSite()
	ok.RESTUser = "ispbill"
	cfg := config.Config{}
	ok = ApplyDefaults(cfg, ok)
	if err := ValidateNet(ok, wg); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*store.Site){
		"static needs address": func(s *store.Site) { s.WANMode = WANStatic },
		"static bad gateway":   func(s *store.Site) { s.WANMode, s.WANAddress, s.WANGateway = WANStatic, "1.2.3.4/24", "nope" },
		"pppoe needs user":     func(s *store.Site) { s.WANMode = WANPPPoE },
		"unknown wan mode":     func(s *store.Site) { s.WANMode = "carrier-pigeon" },
		"lan port is wan":      func(s *store.Site) { s.LANPorts = "ether1" },
		"lan port is olt":      func(s *store.Site) { s.LANPorts = "sfp-sfpplus1" },
		"lan overlaps cust":    func(s *store.Site) { s.LANSubnet = "10.5.50.0/25" },
		"lan overlaps wg":      func(s *store.Site) { s.LANSubnet = "10.99.2.0/24" },
		"vlan too big":         func(s *store.Site) { s.VLANID = 5000 },
	}
	for name, mut := range cases {
		s := ok
		mut(&s)
		if err := ValidateNet(s, wg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// joined makes a router that has fetched its script, with two flats.
func (f *fixture) joined(name string) store.Site {
	f.t.Helper()
	s := f.create(name)
	tok, _, err := f.m.MintJoin(s.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.m.ConsumeJoin(tok, mt, "1.2.3.4", false); err != nil {
		f.t.Fatal(err)
	}
	err = f.st.Update(func(d *store.Data) error {
		p := store.Package{ID: "p1", Name: "Monthly 20", Slug: "m20", SpeedMbps: 20, DurationMin: 43200, PriceKES: 2000}
		d.Packages[p.ID] = p
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: s.ID, Code: "K7Q2MX", Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01", PackageID: "p1", ExpiresAt: f.now.Add(24 * time.Hour), CreatedAt: f.now}
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: s.ID, Code: "P4R8TN", Flat: "Flat 10A", MAC: "AA:BB:CC:DD:EE:02", CreatedAt: f.now}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.site(s.ID)
}

// A second paste must never leave the router unfiltered: the hold rule goes in
// first, the old rules are removed behind it, and the hold comes out last.
func TestBootstrapFirewallIsNeverOpenOnRerun(t *testing.T) {
	for name, mut := range map[string]func(*store.Site){"dhcp": func(*store.Site) {}, "vlan_pppoe": func(s *store.Site) { s.WANMode, s.PPPoEUser, s.PPPoEPassEnc = WANPPPoE, "u", "sealed" }} {
		s := benchSite()
		mut(&s)
		in := benchInput(s)
		in.PPPoEPass = tPPP
		in.Site = ApplyDefaults(config.Config{PublicIP: "203.0.113.10", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24"}, s)
		out, err := RenderBootstrap(in)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(out), "\n")
		hold, drop, lastAdd, release := -1, -1, -1, -1
		for i, l := range lines {
			l = strings.TrimSpace(l)
			switch {
			case strings.Contains(l, "/ip/firewall/filter add") && strings.Contains(l, `comment="ftn: hold"`) && hold < 0:
				hold = i
			case strings.HasPrefix(l, "/ip/firewall/filter remove") && strings.Contains(l, `comment~"^ftn:"`):
				if !strings.Contains(l, `comment!="ftn: hold"`) {
					t.Fatalf("%s: line %d removes every ftn rule, hold included: %s", name, i, l)
				}
				drop = i
			case strings.HasPrefix(l, "/ip/firewall/filter add") && !strings.Contains(l, "ftn: hold"):
				lastAdd = i
			case l == `/ip/firewall/filter remove [find where comment="ftn: hold"]`:
				release = i
			}
		}
		if hold < 0 || drop < 0 || lastAdd < 0 || !(hold < drop && drop < lastAdd && lastAdd < release) {
			t.Fatalf("%s: wrong order: hold %d, remove old %d, last new rule %d, release %d", name, hold, drop, lastAdd, release)
		}
		if !strings.Contains(string(out), "in-interface-list=WAN") || !strings.Contains(string(out), "connection-state=new,invalid") {
			t.Fatalf("%s: the hold must drop new input from the WAN", name)
		}
	}
}

// bootstrapVariants renders the three WAN modes.
func bootstrapVariants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, mut := range map[string]func(*BootstrapInput){
		"dhcp": func(*BootstrapInput) {},
		"static": func(in *BootstrapInput) {
			in.Site.WANMode, in.Site.WANAddress, in.Site.WANGateway = WANStatic, "198.51.100.5/24", "198.51.100.1"
		},
		"pppoe": func(in *BootstrapInput) {
			in.Site.WANMode, in.Site.PPPoEUser, in.Site.PPPoEPassEnc = WANPPPoE, "user1", "sealed"
			in.PPPoEPass = tPPP
		},
	} {
		in := benchInput(benchSite())
		mut(&in)
		b, err := RenderBootstrap(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = string(b)
	}
	return out
}

func lineIdx(lines []string, pred func(string) bool, last bool) int {
	idx := -1
	for i, l := range lines {
		if pred(l) {
			idx = i
			if !last {
				return i
			}
		}
	}
	return idx
}

// The router is never open: the hold goes in before any WAN client, the WAN clients are created switched off,
// the WAN is switched on only after the last filter rule, and the hold is removed last of all.
func TestBootstrapHoldComesBeforeTheWANAndGoesLast(t *testing.T) {
	for name, script := range bootstrapVariants(t) {
		lines := strings.Split(script, "\n")
		has := func(sub string) func(string) bool { return func(l string) bool { return strings.Contains(l, sub) } }
		holdAdd := lineIdx(lines, func(l string) bool {
			return strings.Contains(l, "/ip/firewall/filter add") && strings.Contains(l, `comment="ftn: hold"`)
		}, false)
		wanLine := func(l string) bool {
			t := strings.TrimSpace(l)
			return strings.HasPrefix(t, "/ip/dhcp-client add") || strings.HasPrefix(t, "/interface/pppoe-client add") ||
				strings.HasPrefix(t, "/ip/address add address=198.51.100.5") || strings.HasPrefix(t, "/ip/route add dst-address=0.0.0.0/0")
		}
		v6 := lineIdx(lines, has("/ipv6/settings set disable-ipv6=yes"), false)
		if v6 < 0 || v6 > holdAdd || v6 > lineIdx(lines, has("/interface/list add name=WAN"), false) {
			t.Errorf("%s: IPv6 must be disabled (line %d) before the WAN list and the hold (%d)", name, v6, holdAdd)
		}
		firstWAN := lineIdx(lines, wanLine, false)
		if holdAdd < 0 || firstWAN < 0 || holdAdd > firstWAN {
			t.Fatalf("%s: hold at line %d, first WAN client at %d", name, holdAdd, firstWAN)
		}
		for i, l := range lines {
			if wanLine(l) && !strings.Contains(l, "disabled=yes") {
				t.Errorf("%s: WAN client created switched on (line %d): %s", name, i, l)
			}
		}
		// both directions are held
		if !strings.Contains(script, `chain=input action=drop connection-state=new,invalid in-interface-list=WAN`) ||
			!strings.Contains(script, `chain=forward action=drop connection-state=new,invalid in-interface-list=WAN`) {
			t.Errorf("%s: the hold must cover input and forward", name)
		}
		lastRuleAdd := lineIdx(lines, func(l string) bool {
			return strings.Contains(l, "/ip/firewall/filter add") && !strings.Contains(l, `"ftn: hold"`)
		}, true)
		lastNAT := lineIdx(lines, has("/ip/firewall/nat add"), true)
		holdRemove := lineIdx(lines, has(`/ip/firewall/filter remove [find where comment="ftn: hold"]`), true)
		enable := lineIdx(lines, func(l string) bool { return strings.Contains(l, " enable [find where comment=\"ftn: WAN") }, false)
		if enable < lastRuleAdd || enable < lastNAT {
			t.Errorf("%s: the WAN is switched on (line %d) before the last rule (%d / nat %d)", name, enable, lastRuleAdd, lastNAT)
		}
		if holdRemove < lastRuleAdd || holdRemove < enable {
			t.Errorf("%s: the hold is removed at line %d, but the last rule is at %d and the WAN switch-on at %d", name, holdRemove, lastRuleAdd, enable)
		}
		// nothing after section 6 closes removes the hold again or adds filter rules
		for _, l := range lines[holdRemove+1:] {
			if strings.Contains(l, "/ip/firewall/filter add") {
				t.Errorf("%s: a filter rule is added after the hold was removed: %s", name, l)
			}
		}
		// the error path: the WAN is switched on behind the hold before the report is sent, and the hold is not removed there
		stop := lineIdx(lines, has("the bootstrap script stopped early"), false)
		rep := lineIdx(lines[stop:], has("/tool/fetch"), false) + stop
		en2 := lineIdx(lines[stop:], func(l string) bool { return strings.Contains(l, " enable [find where comment=\"ftn: WAN") }, false) + stop
		if stop < 0 || en2 <= stop || en2 > rep {
			t.Errorf("%s: the error path must switch the WAN on before the report (stop %d enable %d report %d)", name, stop, en2, rep)
		}
		if lineIdx(lines[stop:], has(`"ftn: hold"`), false) >= 0 {
			t.Errorf("%s: the error path must leave the hold in place", name)
		}
	}
}

// The script runs on a blank router only: ftn: objects of an earlier run are refused too, and the refusal says
// how to rebuild. The old "place-before=0" is gone.
func TestBootstrapRefusesToRunTwiceAndHasNoPlaceBeforeZero(t *testing.T) {
	for name, script := range bootstrapVariants(t) {
		if strings.Contains(script, "place-before=0") {
			t.Errorf("%s: place-before=0", name)
		}
		if !strings.Contains(script, "place-before=[:pick $fst 0]") {
			t.Errorf("%s: no guarded place-before", name)
		}
		refuse := strings.Index(script, "has already been set up by a Fast Tech Net script")
		sec1 := strings.Index(script, `:put "ftn 1/12`)
		if refuse < 0 || refuse > sec1 || !strings.Contains(script, `/interface/bridge/find where comment~"^ftn:"`) || !strings.Contains(script, "/system/reset-configuration no-defaults=yes keep-users=yes") {
			t.Errorf("%s: the script does not refuse a second run before it changes anything", name)
		}
		// nothing in the script tears down the admin LAN bridge any more
		if strings.Contains(script, "/interface/bridge remove") {
			t.Errorf("%s: the script still removes bridges", name)
		}
	}
}

func TestValidateRejectsControlCharactersAndOverlaps(t *testing.T) {
	wg := netip.MustParsePrefix("10.99.0.0/24")
	ok := ApplyDefaults(config.Config{}, benchSite())
	ok.RESTUser = "ispbill"
	if err := ValidateNet(ok, wg); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*store.Site){
		"user LF":            func(s *store.Site) { s.WANMode, s.PPPoEUser, s.PPPoEPassEnc = WANPPPoE, "us\ner", "x" },
		"user CR":            func(s *store.Site) { s.WANMode, s.PPPoEUser, s.PPPoEPassEnc = WANPPPoE, "us\rer", "x" },
		"user TAB":           func(s *store.Site) { s.WANMode, s.PPPoEUser, s.PPPoEPassEnc = WANPPPoE, "us\ter", "x" },
		"user DEL":           func(s *store.Site) { s.WANMode, s.PPPoEUser, s.PPPoEPassEnc = WANPPPoE, "us\x7fer", "x" },
		"user 0x01":          func(s *store.Site) { s.WANMode, s.PPPoEUser, s.PPPoEPassEnc = WANPPPoE, "us\x01er", "x" },
		"name LF":            func(s *store.Site) { s.Name = "Block\nA" },
		"lan = customer":     func(s *store.Site) { s.LANSubnet = "10.5.50.0/24" },
		"lan in tunnel":      func(s *store.Site) { s.LANSubnet = "10.99.0.0/25" },
		"customer in tunnel": func(s *store.Site) { s.CustomerSubnet = "10.99.0.0/24" },
		"static wan = lan": func(s *store.Site) {
			s.WANMode, s.WANAddress, s.WANGateway = WANStatic, "192.168.88.20/24", "192.168.88.1"
		},
		"static wan = customer": func(s *store.Site) {
			s.WANMode, s.WANAddress, s.WANGateway = WANStatic, "10.5.50.20/24", "10.5.50.1"
		},
		"static wan = tunnel": func(s *store.Site) {
			s.WANMode, s.WANAddress, s.WANGateway = WANStatic, "10.99.0.20/24", "10.99.0.1"
		},
	} {
		s := ok
		mut(&s)
		if err := ValidateNet(s, wg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a clean static WAN passes
	s := ok
	s.WANMode, s.WANAddress, s.WANGateway = WANStatic, "198.51.100.5/24", "198.51.100.1"
	if err := ValidateNet(s, wg); err != nil {
		t.Errorf("clean static WAN: %v", err)
	}
	// the control characters are also refused at the render and for the clear-text password
	in := benchInput(benchSite())
	in.Site.WANMode, in.Site.PPPoEUser, in.Site.PPPoEPassEnc = WANPPPoE, "u", "sealed"
	in.PPPoEPass = "pw\nrm"
	if _, err := RenderBootstrap(in); err == nil {
		t.Error("a password with a line break was rendered")
	}
	f := newFixture(t, func(c *config.Config) { c.DataKey = []byte(strings.Repeat("k", 32)) })
	if _, err := f.m.CreateRouter(SiteInput{Name: "P", WANMode: WANPPPoE, PPPoEUser: "u1", PPPoEPass: "a\tb"}); err == nil {
		t.Error("a password with a tab was stored")
	}
}

func TestSanitizeReportMasksThePPPoEPasswordToo(t *testing.T) {
	out := sanitizeReport([]byte("2 WAN: login failed for user1 with pp-secret-9 and RESTPASSWORD0123456789abcdefghij"), tRest, tPPP)
	if strings.Contains(out, tPPP) || strings.Contains(out, tRest) || !strings.Contains(out, "[hidden]") {
		t.Fatal(out)
	}
	if out := sanitizeReport([]byte("pp-secret-9"), "", "ab"); !strings.Contains(out, "pp-secret-9") {
		t.Fatal("a too short secret must not mangle the text")
	}
}

// Remote WinBox over the server's wg-mgmt interface: the router accepts WinBox
// from the management subnet through the tunnel, before the final input drop;
// the tunnel peer carries the subnet; MAC-WinBox stays LAN-only.
func TestBootstrapMgmtWinbox(t *testing.T) {
	render := func(mgmt netip.Prefix) string {
		in := benchInput(benchSite())
		in.MgmtSubnet = mgmt
		out, err := RenderBootstrap(in)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	for _, c := range []struct {
		name string
		p    netip.Prefix
		want string
	}{{"default", netip.Prefix{}, "10.98.0.0/24"}, {"custom", netip.MustParsePrefix("172.20.9.0/24"), "172.20.9.0/24"}} {
		out := render(c.p)
		lines := strings.Split(out, "\n")
		idx := func(sub string) int {
			for i, l := range lines {
				if strings.Contains(l, sub) {
					return i
				}
			}
			t.Fatalf("%s: no line with %q", c.name, sub)
			return -1
		}
		accept := idx(`comment="ftn: mgmt winbox"`)
		if l := lines[accept]; !strings.Contains(l, "chain=input action=accept protocol=tcp dst-port=8291 src-address="+c.want+" in-interface=wg-ftn") {
			t.Errorf("%s: mgmt accept rule is wrong: %s", c.name, l)
		}
		if drop := idx(`comment="ftn: input drop rest"`); accept >= drop {
			t.Errorf("%s: the mgmt accept (line %d) must come before the final input drop (line %d)", c.name, accept, drop)
		}
		// It must also come before the rule that drops everything else from the tunnel, and after the hold stays at the top.
		if wgdrop := idx(`comment="ftn: wg drop other input"`); accept >= wgdrop {
			t.Errorf("%s: the mgmt accept must precede 'wg drop other input'", c.name)
		}
		if hold := idx(`comment="ftn: hold"`); hold >= accept {
			t.Errorf("%s: the hold must stay ahead of the mgmt accept", c.name)
		}
		if peer := lines[idx(`peers add interface=wg-ftn`)]; !strings.Contains(peer, "allowed-address=10.99.0.1/32,"+c.want+" ") {
			t.Errorf("%s: peer allowed-address lacks the mgmt subnet: %s", c.name, peer)
		}
		if svc := lines[idx("/ip/service set winbox ")]; !strings.Contains(svc, "address=192.168.88.0/24,"+c.want+" ") {
			t.Errorf("%s: winbox service address: %s", c.name, svc)
		}
		if route := lines[idx("/ip/route add dst-address=")]; !strings.Contains(route, "dst-address="+c.want+" gateway=wg-ftn") {
			t.Errorf("%s: mgmt route: %s", c.name, route)
		}
		// MAC-WinBox stays on the LAN list only.
		if mac := lines[idx("/tool/mac-server/mac-winbox set")]; !strings.Contains(mac, "allowed-interface-list=LAN") || strings.Contains(mac, c.want) {
			t.Errorf("%s: mac-winbox must stay LAN only: %s", c.name, mac)
		}
		// Nothing else opens WinBox: the only 8291 rule is the mgmt one, and it never names the WAN.
		if n := strings.Count(out, "dst-port=8291"); n != 1 {
			t.Errorf("%s: %d rules mention port 8291, want 1", c.name, n)
		}
	}
}

func TestValidateNetMgmtOverlap(t *testing.T) {
	wg := netip.MustParsePrefix("10.99.0.0/22")
	mgmt := netip.MustParsePrefix("10.98.0.0/24")
	ok := ApplyDefaults(config.Config{}, benchSite())
	ok.RESTUser = "ispbill"
	if err := ValidateNetMgmt(ok, wg, mgmt); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*store.Site){
		"lan overlaps mgmt":      func(s *store.Site) { s.LANSubnet = "10.98.0.0/25" },
		"lan contains mgmt":      func(s *store.Site) { s.LANSubnet = "10.98.0.0/16" },
		"customer overlaps mgmt": func(s *store.Site) { s.CustomerSubnet = "10.98.0.0/24" },
		"static wan in mgmt": func(s *store.Site) {
			s.WANMode, s.WANAddress, s.WANGateway = WANStatic, "10.98.0.20/24", "10.98.0.1"
		},
	} {
		s := ok
		mut(&s)
		if err := ValidateNetMgmt(s, wg, mgmt); err == nil || !strings.Contains(err.Error(), "management subnet") {
			t.Errorf("%s: want an overlap error naming the management subnet, got %v", name, err)
		}
		// the default subnet applies when none is passed
		if err := ValidateNet(s, wg); err == nil {
			t.Errorf("%s: accepted with the default management subnet", name)
		}
	}
	// the tunnel and the management subnet must be separate, and the subnet must be private IPv4
	if err := ValidateNetMgmt(ok, netip.MustParsePrefix("10.98.0.0/22"), mgmt); err == nil {
		t.Error("tunnel overlapping the management subnet was accepted")
	}
	for _, bad := range []string{"8.8.8.0/24", "10.98.0.0/8", "10.98.0.0/30", "fd00::/64"} {
		if err := ValidateNetMgmt(ok, wg, netip.MustParsePrefix(bad)); err == nil {
			t.Errorf("management subnet %s was accepted", bad)
		}
	}
	// a changed management subnet moves the check with it
	s := ok
	s.LANSubnet = "10.98.0.0/24"
	if err := ValidateNetMgmt(s, wg, netip.MustParsePrefix("172.20.9.0/24")); err != nil {
		t.Errorf("LAN on the old default is fine when the management subnet is elsewhere: %v", err)
	}
}

// The router's report carries the management subnet it was built with; only a
// real private IPv4 prefix is kept, and it survives the stored text.
func TestJoinReportMgmtField(t *testing.T) {
	at := time.Now()
	for line, want := range map[string]string{
		"result ok ros 7.25.1 mgmt 10.98.0.0/24":   "10.98.0.0/24",
		"result ok ros 7.25.1 mgmt 10.98.0.77/24":  "10.98.0.0/24",
		"result ok ros 7.25.1 mgmt ":               "",
		"result ok ros 7.25.1 mgmt":                "",
		"result ok ros 7.25.1":                     "",
		"result fail ros 7.25.1 mgmt not-a-subnet": "",
		"result ok ros 7.25.1 mgmt fd00::/64":      "",
		"result ok ros 7.25.1 mgmt 10.98.0.0/24 x": "10.98.0.0/24",
	} {
		r := parseReport(line+"\n1 problem", at)
		if r.Mgmt != want || len(r.Lines) != 1 {
			t.Errorf("%q: mgmt %q (want %q), lines %v", line, r.Mgmt, want, r.Lines)
		}
		back, ok := parseStoredReport(formatStoredReport(r))
		if !ok || back.Mgmt != want || back.ROS != "7.25.1" {
			t.Errorf("%q: stored round trip %+v", line, back)
		}
	}
}

// RouterOS refuses a filter rule that names an interface that does not exist yet
// ("input does not match any value of interface"). Every interface a rule names
// must be created on an earlier line of the script.
func TestBootstrapCreatesInterfacesBeforeRulesNameThem(t *testing.T) {
	for _, name := range []string{"bootstrap_dhcp_vlan_two_customer_ports.rsc", "bootstrap_dhcp_novlan.rsc", "bootstrap_static_vlan.rsc", "bootstrap_pppoe_novlan.rsc", "bootstrap_old_ros.rsc"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		created := map[string]int{}
		mk := regexp.MustCompile(`^\s*/interface/(?:wireguard|bridge|vlan|pppoe-client)(?:\s+)add\b.*\bname=("?)([A-Za-z0-9._-]+)`)
		use := regexp.MustCompile(`^\s*/ip/firewall/(?:filter|nat)\s+add\b.*\b(?:in|out)-interface=("?)([A-Za-z0-9._-]+)`)
		for i, l := range lines {
			if m := mk.FindStringSubmatch(l); m != nil {
				if _, ok := created[m[2]]; !ok {
					created[m[2]] = i
				}
			}
		}
		for i, l := range lines {
			m := use.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			ifc := m[2]
			if regexp.MustCompile(`^(ether|sfp|combo|wlan|wifi)`).MatchString(ifc) {
				continue // physical ports exist from the start
			}
			at, ok := created[ifc]
			if !ok || at > i {
				t.Errorf("%s:%d: rule names %q before the script creates it (created at line %d, ok=%v)", name, i+1, ifc, at+1, ok)
			}
		}
	}
}

// RouterOS 7.25.1 refuses `comment` on /ip/traffic-flow/target ("bad parameter comment"),
// so the script must neither set nor look up a comment there.
func TestBootstrapTrafficFlowTargetHasNoComment(t *testing.T) {
	for _, name := range []string{"bootstrap_dhcp_vlan_two_customer_ports.rsc", "bootstrap_dhcp_novlan.rsc", "bootstrap_static_vlan.rsc", "bootstrap_pppoe_novlan.rsc", "bootstrap_old_ros.rsc"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, "/ip/traffic-flow/target") && strings.Contains(l, "comment") {
				t.Errorf("%s:%d: traffic-flow target line uses a comment: %s", name, i+1, strings.TrimSpace(l))
			}
		}
	}
}

// A router left without DNS servers (a half-built earlier attempt) could not fetch its setup.
// The paste line gives it DNS servers first, and only when it has none of its own.
func TestBootstrapCommandGivesDNSWhenNone(t *testing.T) {
	c := BootstrapCommand("https://isp.example.co.ke/", "TOK", true)
	dns := `/ip/dns/set servers=` + PasteLineDNS
	i, f := strings.Index(c, dns), strings.Index(c, "/tool/fetch")
	if i < 0 || f < 0 || i > f {
		t.Fatalf("the DNS step must come before the first fetch: dns at %d, fetch at %d", i, f)
	}
	if !strings.Contains(c, `[:len [/ip/dns/get servers]] = 0`) || !strings.Contains(c, `[:len [/ip/dns/get dynamic-servers]] = 0`) {
		t.Fatal("DNS servers must only be set when the router has neither static nor dynamic ones")
	}
	if !strings.Contains(c, ":resolve isp.example.co.ke") {
		t.Fatal("the paste line should check that the server's name can be looked up")
	}
	if strings.ContainsAny(c, "\r\n") {
		t.Fatal("the paste line must stay one line")
	}
}

// A line that gave the router DNS servers puts them back to empty when the setup fails, and only then.
func TestBootstrapCommandRestoresDNSOnFailure(t *testing.T) {
	c := BootstrapCommand("https://isp.example.co.ke/", "TOK", true)
	set, flag := strings.Index(c, ":set d 1"), strings.Index(c, ":local d 0")
	onErr := strings.Index(c, `:if ($d = 1) do={ :do { /ip/dns/set servers="" } on-error={}`)
	fail := strings.Index(c, "ftn: setup failed")
	_ = fail
	dns := strings.Index(c, "/ip/dns/set servers="+PasteLineDNS)
	if flag < 0 || set < 0 || onErr < 0 || !(flag < dns && dns < set && set < onErr && fail < onErr) {
		t.Fatalf("flag %d dns %d set %d restore %d failure %d", flag, dns, set, onErr, fail)
	}
	if strings.Count(c, ":set d 1") != 1 || strings.ContainsAny(c, "\r\n") {
		t.Fatal("the flag is set once, only where the line sets DNS, and the line stays one line")
	}
	if n := BootstrapCommand("https://isp.example.co.ke", "TOK", false); !strings.Contains(n, `servers=""`) {
		t.Fatal("the no-check line restores DNS too")
	}
}

// The captive page works because of these rules, in this order: a walled flat (unregistered range, or expired and
// on ftn-suspended) reaches only the router's DNS and the server on 80/443; its plain HTTP to any other host is
// dst-nat'd to the server's port 80 (so the device's own probe lands on the payment page); everything else is
// refused at once (tcp-reset), above established/related so a connection that was open when the plan ended is cut.
func TestBootstrapWalledGardenCaptiveRules(t *testing.T) {
	out, err := RenderBootstrap(benchInput(benchSite()))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	find := func(sub ...string) int {
		t.Helper()
		for i, l := range lines {
			ok := true
			for _, s := range sub {
				ok = ok && strings.Contains(l, s)
			}
			if ok {
				return i
			}
		}
		t.Fatalf("no line with %q", sub)
		return -1
	}
	const pub = "203.0.113.10"
	for _, list := range []string{"ftn-unreg", "ftn-suspended"} {
		// Plain HTTP to anywhere but the server goes to the server's port 80; the server itself is excluded.
		nat := find("/ip/firewall/nat add chain=dstnat action=dst-nat", "protocol=tcp dst-port=80", "dst-address=!"+pub, "src-address-list="+list, "to-addresses="+pub, "to-ports=80")
		// HTTPS is never redirected (a redirect would only give a certificate error).
		for _, l := range lines {
			if strings.Contains(l, "/ip/firewall/nat") && strings.Contains(l, "src-address-list="+list) && strings.Contains(l, "dst-port=443") {
				t.Errorf("HTTPS must not be redirected: %s", l)
			}
		}
		dnsU := find("/ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53", "src-address-list="+list)
		dnsT := find("/ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53", "src-address-list="+list)
		if dnsU > nat || dnsT > nat {
			t.Errorf("%s: DNS redirects must come before the HTTP dst-nat", list)
		}
		// Forward chain, top to bottom: server 80/443 accepted, other TCP reset, the rest refused, then est/rel.
		acc := find("chain=forward action=accept protocol=tcp dst-port=80,443 dst-address="+pub, "src-address-list="+list)
		rst := find("chain=forward action=reject reject-with=tcp-reset protocol=tcp", "src-address-list="+list)
		rej := find("chain=forward action=reject reject-with=icmp-admin-prohibited", "src-address-list="+list)
		custER := find("chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24")
		allER := find("chain=forward action=accept connection-state=established,related", `comment="ftn: forward est/rel"`)
		if !(acc < rst && rst < rej && rej < custER && custER < allER) {
			t.Errorf("%s: forward order wrong: accept %d, tcp-reset %d, reject %d, customer est/rel %d, est/rel %d", list, acc, rst, rej, custER, allER)
		}
		// Router input: DNS (all customers) is accepted above the walled drop; DHCP and ping for the walled; no WebFig.
		dnsIn := find("chain=input action=accept protocol=udp dst-port=53 in-interface=br-customers")
		drop := find("chain=input action=drop src-address-list=" + list)
		if dnsIn > drop {
			t.Errorf("%s: the router's DNS must stay reachable for a walled flat", list)
		}
	}
	// The forward chain sees the address after dst-nat: a rule for the server's 80/443 therefore also admits the
	// redirected probe. The router must resolve names for the walled (the probe hosts, isp.ftw.co.ke).
	find("/ip/dns set servers=", "allow-remote-requests=yes")
	// Router rules are written only after the tunnel interface exists, and the hold is lifted last.
	if find(`/interface/wireguard add name=`) > find("chain=forward action=accept protocol=tcp dst-port=80,443") {
		t.Error("a rule may not name an interface that does not exist yet")
	}
	lastHold := -1
	for i, l := range lines {
		if l == `/ip/firewall/filter remove [find where comment="ftn: hold"]` {
			lastHold = i
		}
	}
	if lastHold < find("/ip/firewall/nat add chain=dstnat action=dst-nat") {
		t.Error("the hold must stay until the walled rules are in")
	}
	// RouterOS 7.25.1: no comment on traffic-flow targets.
	if l := lines[find("/ip/traffic-flow/target add")]; strings.Contains(l, "comment") {
		t.Errorf("traffic-flow target takes no comment: %s", l)
	}
}
