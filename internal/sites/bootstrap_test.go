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
		OLTInterface: "sfp-sfpplus1", VLANID: 100, CustomerSubnet: "10.5.50.0/24", BenchPort: "ether5"}
}

func benchInput(s store.Site) BootstrapInput {
	cfg := config.Config{PublicIP: "203.0.113.10", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24", RouterUser: "ispbill"}
	s.RESTUser = "ispbill"
	return BootstrapInput{Site: ApplyDefaults(cfg, s), PrivateKey: tPriv, RESTPass: tRest, ServerPub: "SERVERPUBKEYSERVERPUBKEYSERVERPUBKEYSERV0=",
		Endpoint: "203.0.113.10:51820", WGSubnet: netip.MustParsePrefix("10.99.0.0/22"), PublicBase: "https://billing.example.co.ke",
		PublicIP: "203.0.113.10", Generated: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), ROSVersion: "7.25.1", Token: tTok, NetflowPort: 2055}
}

func TestBootstrapGolden(t *testing.T) {
	cases := map[string]func(*store.Site, *BootstrapInput){
		"dhcp_vlan_bench": func(*store.Site, *BootstrapInput) {},
		"dhcp_novlan":     func(s *store.Site, _ *BootstrapInput) { s.VLANID, s.BenchPort = 0, "" },
		"static_vlan": func(s *store.Site, _ *BootstrapInput) {
			s.WANMode, s.WANAddress, s.WANGateway, s.WANDNS = WANStatic, "198.51.100.5/24", "198.51.100.1", "9.9.9.9"
		},
		"pppoe_novlan": func(s *store.Site, in *BootstrapInput) {
			s.WANMode, s.PPPoEUser, s.PPPoEPassEnc, s.VLANID, s.BenchPort = WANPPPoE, "user1", "sealed", 0, ""
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
			cfg := config.Config{PublicIP: "203.0.113.10", WANInterface: "ether1", OLTInterface: "sfp-sfpplus1", CustomerSubnet: "10.5.50.0/24", RouterUser: "ispbill"}
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
