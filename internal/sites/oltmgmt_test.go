package sites

import (
	"strings"
	"testing"

	"ispbill/internal/store"
)

func oltSite() store.Site {
	return store.Site{ID: "s1", Name: "Block A", TunnelIP: "10.99.0.2", ONUMgmtN: 3, VLANID: 100}
}

func TestOLTMgmtPlanAddressing(t *testing.T) {
	p, err := PlanOLTMgmt(oltSite(), 0, "10.99.0.1", []OLTTarget{{IP: "10.7.3.2", Proto: "ssh"}, {IP: "10.7.3.3", Proto: "telnet"}, {IP: "10.7.9.2", Proto: "ssh"}, {IP: "10.7.3.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Subnet != "10.7.3.0/29" || p.Gateway != "10.7.3.1" || p.VLAN != 98 || p.Iface != "vlan98-olt" || p.GatewayCIDR != "10.7.3.1/29" {
		t.Fatalf("%+v", p)
	}
	if len(p.Targets) != 2 {
		t.Fatalf("an OLT outside the subnet or on the router's address got a forward: %+v", p.Targets)
	}
	if port, err := p.ForwardPort("10.7.3.2", "ssh"); err != nil || port != 22002 {
		t.Fatalf("%d %v", port, err)
	}
	if port, _ := p.ForwardPort("10.7.3.3", "telnet"); port != 23003 {
		t.Fatalf("%d", port)
	}
	if _, err := p.ForwardPort("10.7.3.7", "ssh"); err == nil {
		t.Fatal(".7 is not an OLT address")
	}
	if _, err := p.ForwardPort("192.168.8.200", "ssh"); err == nil {
		t.Fatal("an address outside the subnet got a port")
	}
	host, port, err := OLTForward(oltSite(), 0, "10.99.0.1", OLTTarget{IP: "10.7.3.2", Proto: "ssh"})
	if err != nil || host != "10.99.0.2" || port != 22002 {
		t.Fatalf("%s %d %v", host, port, err)
	}
}

func TestOLTMgmtPlanRefusals(t *testing.T) {
	s := oltSite()
	s.ONUMgmtN = 0
	if _, err := PlanOLTMgmt(s, 0, "10.99.0.1", nil); err == nil {
		t.Fatal("no ONU management number, no OLT network")
	}
	s = oltSite()
	for _, v := range []int{1, 4095, 100, 99} { // customer VLAN is 100, ONU VLAN defaults to 99
		if _, err := PlanOLTMgmt(s, v, "10.99.0.1", nil); err == nil {
			t.Errorf("VLAN %d accepted", v)
		}
	}
}

func TestOLTMgmtRulesOnlyTheServerReaches(t *testing.T) {
	p, _ := PlanOLTMgmt(oltSite(), 0, "10.99.0.1", []OLTTarget{{IP: "10.7.3.2", Proto: "ssh", Port: 22}})
	fwd, in, nat := p.Rules()
	has := func(rs []ipoeRule, comment, sub string) bool {
		for _, r := range rs {
			if r.Comment == comment && strings.Contains(r.Args(), sub) {
				return true
			}
		}
		return false
	}
	if !has(nat, OLTDstNatComment, "src-address=10.99.0.1") || !has(nat, OLTDstNatComment, "dst-address=10.99.0.2 dst-port=22002 to-addresses=10.7.3.2 to-ports=22") || !has(nat, OLTDstNatComment, "in-interface=wg-ftn") {
		t.Fatalf("dst-nat: %+v", nat)
	}
	if !has(fwd, OLTServerInComment, "src-address=10.99.0.1 dst-address=10.7.3.2 dst-port=22") || !has(fwd, OLTServerInComment, "in-interface=wg-ftn out-interface=vlan98-olt") {
		t.Fatalf("server accept: %+v", fwd)
	}
	if !has(fwd, OLTReplyComment, "connection-state=established,related") {
		t.Fatalf("replies: %+v", fwd)
	}
	// Order: accepts, then the two drops last.
	n := len(fwd)
	if fwd[n-2].Comment != OLTDropInComment || fwd[n-1].Comment != OLTDropOutComment {
		t.Fatalf("the drops must come after the accepts: %+v", fwd)
	}
	if !has(in, OLTInputDropComment, "chain=input action=drop in-interface=vlan98-olt") {
		t.Fatalf("input drop: %+v", in)
	}
	// Nothing in the rules lets VLAN 99, the customers or anything but the tunnel into the OLT VLAN.
	for _, r := range fwd {
		if r.Comment == OLTServerInComment && !strings.Contains(r.Args(), "src-address=10.99.0.1") {
			t.Fatalf("accept not limited to the server: %s", r.Args())
		}
		if strings.Contains(r.Args(), "vlan99") || strings.Contains(r.Args(), "in-interface=bridge") {
			t.Fatalf("rule mentions another network: %s", r.Args())
		}
	}
}

func TestOLTMgmtSnippet(t *testing.T) {
	p, _ := PlanOLTMgmt(oltSite(), 0, "10.99.0.1", []OLTTarget{{IP: "10.7.3.2", Proto: "ssh", Port: 22}})
	s := p.Snippet("ether3")
	for _, want := range []string{
		`/interface/vlan add name=vlan98-olt vlan-id=98 interface=ether3 arp=enabled comment="ftn: olt mgmt VLAN"`,
		`/ip/address add address=10.7.3.1/29 interface=vlan98-olt comment="ftn: olt mgmt gateway"`,
		`place-before=[find where comment="ftn: wg no forward in"]`,
		`/ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-address=10.99.0.2 dst-port=22002 to-addresses=10.7.3.2 to-ports=22`,
		`place-before=0 comment="ftn: olt mgmt input drop"`,
		`/ip/firewall/filter remove [find where comment~"^ftn: olt"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("snippet lacks %q:\n%s", want, s)
		}
	}
	// The server writes none of this to the router: the snippet touches no /user, /ip/service, /system, wireguard or routes.
	for _, bad := range []string{"/user", "/ip/service", "/system", "wireguard", "/ip/route"} {
		if strings.Contains(s, bad) {
			t.Errorf("snippet touches %s", bad)
		}
	}
}
