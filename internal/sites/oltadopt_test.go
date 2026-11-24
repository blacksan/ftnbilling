package sites

import (
	"context"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func countOLTM(m *routeros.Mock) (n int, bad []string) {
	for _, p := range []string{"/ip/address", "/ip/firewall/filter", "/ip/firewall/nat"} {
		for _, r := range m.Table(p) {
			if strings.HasPrefix(r["comment"], "ftn: oltm") {
				n++
			}
		}
	}
	return n, bad
}

func setOLTPort(t *testing.T, f *fixture, id, port string) {
	t.Helper()
	if err := f.st.Update(func(d *store.Data) error {
		x := d.Sites[id]
		x.OLTMgmtPort = port
		d.Sites[id] = x
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOLTAdoptForwardPort(t *testing.T) {
	if p, err := OLTAdoptForwardPort("192.168.8.200"); err != nil || p != 22200 {
		t.Fatalf("port = %d, %v", p, err)
	}
	if _, err := OLTAdoptForwardPort("nope"); err == nil {
		t.Fatal("bad address accepted")
	}
}

func TestEnsureOLTMgmtWritesOnlyAllowedScopedRules(t *testing.T) {
	f, s, m := portsRig(t)
	setOLTPort(t, f, s.ID, "ether5")
	m.ResetWrites()
	if err := f.m.EnsureOLTMgmt(context.Background(), s.ID, "brian"); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"/ip/address": true, "/ip/firewall/filter": true, "/ip/firewall/nat": true}
	for _, w := range m.WrittenPaths() {
		parts := strings.Fields(w)
		if len(parts) != 2 || !allowed[parts[1]] {
			t.Fatalf("write outside the allowed tables: %q", w)
		}
		for _, bad := range []string{"/ip/route", "wireguard", "/user", "/ip/service", "/system"} {
			if strings.Contains(w, bad) {
				t.Fatalf("forbidden write %q", w)
			}
		}
	}
	addr := m.Table("/ip/address")
	var gotAddr bool
	for _, r := range addr {
		if r["comment"] == oltmAddrComment {
			gotAddr = r["address"] == "192.168.8.1/24" && r["interface"] == "ether5"
		}
	}
	if !gotAddr {
		t.Fatalf("router address missing: %v", addr)
	}
	var nat, drops int
	for _, r := range m.Table("/ip/firewall/nat") {
		switch r["comment"] {
		case oltmNatComment:
			nat++
			if r["action"] != "dst-nat" || r["dst-port"] != "22200" || r["to-addresses"] != "192.168.8.200" || r["to-ports"] != "22" || r["src-address"] != "10.99.0.1" || r["in-interface"] != "wg-ftn" {
				t.Fatalf("dst-nat row: %v", r)
			}
		case oltmMasqComment:
			if r["action"] != "masquerade" || r["out-interface"] != "ether5" {
				t.Fatalf("masquerade row: %v", r)
			}
		}
	}
	if nat != 1 {
		t.Fatalf("dst-nat rows = %d", nat)
	}
	for _, r := range m.Table("/ip/firewall/filter") {
		c := r["comment"]
		if !strings.HasPrefix(c, "ftn: oltm") {
			continue
		}
		switch c {
		case oltmDropIn, oltmDropOut, oltmInputDrop:
			drops++
			if r["action"] != "drop" {
				t.Fatalf("%s is not a drop: %v", c, r)
			}
		case oltmFwdComment:
			if r["src-address"] != "10.99.0.1" || r["action"] != "accept" {
				t.Fatalf("server accept rule not scoped: %v", r)
			}
		}
	}
	if drops != 3 {
		t.Fatalf("drop rules = %d", drops)
	}
}

func TestEnsureOLTMgmtIdempotentAndRemovalExact(t *testing.T) {
	f, s, m := portsRig(t)
	foreign := m.SeedRow("/ip/firewall/filter", R{"chain": "forward", "action": "accept", "comment": "someone else"})
	_ = foreign
	setOLTPort(t, f, s.ID, "ether5")
	if err := f.m.EnsureOLTMgmt(context.Background(), s.ID, "brian"); err != nil {
		t.Fatal(err)
	}
	n1, _ := countOLTM(m)
	if n1 < 7 {
		t.Fatalf("rows = %d", n1)
	}
	m.ResetWrites()
	if err := f.m.EnsureOLTMgmt(context.Background(), s.ID, "system"); err != nil {
		t.Fatal(err)
	}
	if w := m.WrittenPaths(); len(w) != 0 {
		t.Fatalf("second ensure wrote %v", w)
	}
	setOLTPort(t, f, s.ID, "")
	if err := f.m.EnsureOLTMgmt(context.Background(), s.ID, "brian"); err != nil {
		t.Fatal(err)
	}
	if n, _ := countOLTM(m); n != 0 {
		t.Fatalf("%d oltm rows left", n)
	}
	var kept bool
	for _, r := range m.Table("/ip/firewall/filter") {
		if r["comment"] == "someone else" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("a foreign rule was removed")
	}
	for _, w := range m.WrittenPaths() {
		if strings.Contains(w, "/user") || strings.Contains(w, "wireguard") || strings.Contains(w, "/ip/route") {
			t.Fatalf("forbidden write %q", w)
		}
	}
}

func TestPlanOLTAdoptRefusals(t *testing.T) {
	base := store.Site{OLTMgmtPort: "ether5", TunnelIP: "10.99.0.2"}
	if _, err := PlanOLTAdopt(base, "", "10.99.0.1"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(s *store.Site) (string, string){
		"no port":        func(s *store.Site) (string, string) { s.OLTMgmtPort = ""; return "", "10.99.0.1" },
		"bad net":        func(s *store.Site) (string, string) { s.OLTMgmtNet = "nonsense"; return "", "10.99.0.1" },
		"olt outside":    func(s *store.Site) (string, string) { return "10.0.0.5", "10.99.0.1" },
		"olt is router":  func(s *store.Site) (string, string) { return "192.168.8.1", "10.99.0.1" },
		"overlap":        func(s *store.Site) (string, string) { s.CustomerSubnet = "192.168.8.0/24"; return "", "10.99.0.1" },
		"no tunnel":      func(s *store.Site) (string, string) { s.TunnelIP = ""; return "", "10.99.0.1" },
		"bad server ip":  func(s *store.Site) (string, string) { return "", "x" },
		"too big prefix": func(s *store.Site) (string, string) { s.OLTMgmtNet = "10.0.0.1/8"; return "", "10.99.0.1" },
	}
	for name, mod := range cases {
		s := base
		ip, srv := mod(&s)
		if _, err := PlanOLTAdopt(s, ip, srv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
