package sites

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"ispbill/internal/store"
)

// OLT management network.
//
// The server logs in to an OLT's command line (SSH or Telnet) over the tunnel, through its router:
//
//	server 10.99.0.1 -> wg0 -> router tunnel address :22000+h -> dst-nat -> vlan98-olt 10.7.<n>.<h>:22 (the OLT)
//
// The OLT sits on its own management VLAN (default 98) of the router's OLT-role port, in the small subnet
// 10.7.<n>.0/29 (n is the router's ONU-management number, so the numbering is the same as the 10.6.<n>.0/24 TR-069
// network): the router is .1, the OLTs are .2 to .6. Only the server may start a connection to an OLT, and only to
// its SSH or Telnet port: the VLAN is cut off from VLAN 99, the customers, the admin port and the unregistered pool
// (forward drops both ways, input drop), and an OLT can answer only the connections the server opened.
//
// Why a dst-nat and not a route (as the 10.6.<n>.0/24 network has): a route would need the subnet in the router's
// AllowedIPs on the server, a second entry that the root wg-sync script does not accept, and a new supernet to
// route. The tunnel address of the router is already allowed, so the forwarded port reaches the OLT with no change
// on the server side at all.
//
// Router-side the owner pastes the script once (the same way as the ONU management snippet): the server never writes
// /ip/address, /interface/vlan or the firewall of the router over REST for this.
const (
	// OLTMgmtSupernet is where the per-router OLT subnets come from.
	OLTMgmtSupernet = "10.7.0.0/16"

	oltPrefix = "ftn: olt "
	// OLTMgmtVLANComment and friends are the comments of the router objects.
	OLTMgmtVLANComment    = "ftn: olt mgmt VLAN"
	OLTMgmtGatewayComment = "ftn: olt mgmt gateway"
	OLTDstNatComment      = "ftn: olt mgmt dst-nat"
	OLTServerInComment    = "ftn: olt server to olt"
	OLTReplyComment       = "ftn: olt replies"
	OLTDropInComment      = "ftn: olt mgmt drop in"
	OLTDropOutComment     = "ftn: olt mgmt drop out"
	OLTInputDropComment   = "ftn: olt mgmt input drop"

	oltSSHBase    = 22000
	oltTelnetBase = 23000
)

// OLTTarget is one OLT behind a router: its address on the management VLAN and how the server logs in.
type OLTTarget struct {
	IP    string
	Proto string // ssh or telnet
	Port  int    // the OLT's own port
}

// OLTMgmtPlan is the addressing and naming of one router's OLT management network.
type OLTMgmtPlan struct {
	N, VLAN     int
	Iface       string // vlan98-olt
	Subnet      string // 10.7.<n>.0/29
	Mask        string // 255.255.255.248
	Gateway     string // 10.7.<n>.1
	GatewayCIDR string
	ServerIP    string // the server's tunnel address
	TunnelIP    string // the router's tunnel address: the server connects to it
	Targets     []OLTTarget
}

// OLTMgmtVLANName is the VLAN interface built on the OLT port.
func OLTMgmtVLANName(vlan int) string { return fmt.Sprintf("vlan%d-olt", vlan) }

// ErrNoOLTMgmt: the router has no OLT management subnet because ONU management is not set up on it.
var ErrNoOLTMgmt = errors.New("set up ONU management on this router first: the OLT management network is numbered from it")

// PlanOLTMgmt computes a router's OLT management network. vlan 0 = 98.
func PlanOLTMgmt(s store.Site, vlan int, serverIP string, targets []OLTTarget) (OLTMgmtPlan, error) {
	var p OLTMgmtPlan
	if s.ONUMgmtN <= 0 || s.ONUMgmtN > 255 {
		return p, ErrNoOLTMgmt
	}
	if vlan == 0 {
		vlan = store.DefaultOLTMgmtVLAN
	}
	if vlan < 2 || vlan > 4094 {
		return p, fmt.Errorf("the OLT management VLAN %d must be 2 to 4094", vlan)
	}
	if vlan == s.VLANID || vlan == s.MgmtVLANOrDefault() {
		return p, fmt.Errorf("the OLT management VLAN %d is already the customer or ONU management VLAN of this router", vlan)
	}
	if _, err := netip.ParseAddr(serverIP); err != nil {
		return p, fmt.Errorf("the server tunnel address %q is not an address", serverIP)
	}
	if _, err := netip.ParseAddr(s.TunnelIP); err != nil {
		return p, errors.New("the router has no tunnel address yet")
	}
	sub := netip.MustParsePrefix(fmt.Sprintf("10.7.%d.0/29", s.ONUMgmtN))
	p = OLTMgmtPlan{N: s.ONUMgmtN, VLAN: vlan, Iface: OLTMgmtVLANName(vlan), Subnet: sub.String(), Mask: "255.255.255.248",
		Gateway: fmt.Sprintf("10.7.%d.1", s.ONUMgmtN), GatewayCIDR: fmt.Sprintf("10.7.%d.1/29", s.ONUMgmtN), ServerIP: serverIP, TunnelIP: s.TunnelIP}
	for _, t := range targets {
		a, err := netip.ParseAddr(t.IP)
		if err != nil || !sub.Contains(a) || a == sub.Addr() || a.String() == p.Gateway {
			continue // an OLT outside the subnet (or on the router's own address) gets no forward
		}
		if t.Port <= 0 {
			t.Port = 22
			if t.Proto == store.OLTProtoTelnet {
				t.Port = 23
			}
		}
		p.Targets = append(p.Targets, t)
	}
	return p, nil
}

// Contains reports whether ip is an OLT address of this network (.2 to .6).
func (p OLTMgmtPlan) Contains(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	sub := netip.MustParsePrefix(p.Subnet)
	return sub.Contains(a) && a != sub.Addr() && a.String() != p.Gateway && a.As4()[3] <= 6
}

// ForwardPort is the port of the router's tunnel address that reaches the OLT at ip over proto.
func (p OLTMgmtPlan) ForwardPort(ip, proto string) (int, error) {
	if !p.Contains(ip) {
		return 0, fmt.Errorf("the OLT address %s is not in %s (use .2 to .6)", ip, p.Subnet)
	}
	a, _ := netip.ParseAddr(ip)
	h := int(a.As4()[3])
	if proto == store.OLTProtoTelnet {
		return oltTelnetBase + h, nil
	}
	return oltSSHBase + h, nil
}

// SetupLines are the RouterOS lines that make the VLAN and its address on parent (the OLT port, or the customer
// bridge when the customers have no VLAN, as for the ONU management network). They are idempotent.
func (p OLTMgmtPlan) SetupLines(parent string) []string {
	return []string{
		fmt.Sprintf(`/ip/address remove [find where comment="%s"]`, OLTMgmtGatewayComment),
		fmt.Sprintf(`/interface/vlan remove [find where name="%s"]`, p.Iface),
		fmt.Sprintf(`/interface/vlan add name=%s vlan-id=%d interface=%s arp=reply-only comment="%s"`, p.Iface, p.VLAN, parent, OLTMgmtVLANComment),
		fmt.Sprintf(`/ip/address add address=%s interface=%s comment="%s"`, p.GatewayCIDR, p.Iface, OLTMgmtGatewayComment),
	}
}

// Rules are the firewall and NAT rules. Forward goes before "ftn: wg no forward in", Input at the very top, NAT at the end of
// dstnat.
func (p OLTMgmtPlan) Rules() (forward, input, nat []ipoeRule) {
	f := func(args, comment string) ipoeRule { return newIpoeRule("filter", args, comment) }
	for _, t := range p.Targets {
		fp, err := p.ForwardPort(t.IP, t.Proto)
		if err != nil {
			continue
		}
		nat = append(nat, newIpoeRule("nat", fmt.Sprintf("chain=dstnat action=dst-nat protocol=tcp in-interface=%s src-address=%s dst-address=%s dst-port=%d to-addresses=%s to-ports=%d",
			WGInterface, p.ServerIP, p.TunnelIP, fp, t.IP, t.Port), OLTDstNatComment))
		forward = append(forward, f(fmt.Sprintf("chain=forward action=accept protocol=tcp in-interface=%s out-interface=%s src-address=%s dst-address=%s dst-port=%d",
			WGInterface, p.Iface, p.ServerIP, t.IP, t.Port), OLTServerInComment))
	}
	forward = append(forward,
		f(fmt.Sprintf("chain=forward action=accept connection-state=established,related in-interface=%s out-interface=%s src-address=%s/29 dst-address=%s",
			p.Iface, WGInterface, strings.TrimSuffix(p.Subnet, "/29"), p.ServerIP), OLTReplyComment),
		f("chain=forward action=drop in-interface="+p.Iface, OLTDropInComment),
		f("chain=forward action=drop out-interface="+p.Iface, OLTDropOutComment))
	input = []ipoeRule{f("chain=input action=drop in-interface="+p.Iface, OLTInputDropComment)}
	return forward, input, nat
}

// Snippet is the script the owner pastes once into the router terminal. It removes its own earlier objects (by
// comment) first, so it can be pasted again after an OLT is added.
func (p OLTMgmtPlan) Snippet(parent string) string {
	var b strings.Builder
	b.WriteString("# Fast Tech Net: OLT management network " + p.Subnet + " (VLAN " + fmt.Sprint(p.VLAN) + "). Paste in the router terminal.\n")
	for _, l := range p.SetupLines(parent) {
		b.WriteString(l + "\n")
	}
	b.WriteString(`/ip/firewall/filter remove [find where comment~"^ftn: olt"]` + "\n")
	b.WriteString(`/ip/firewall/nat remove [find where comment~"^ftn: olt"]` + "\n")
	fwd, in, nat := p.Rules()
	for _, r := range fwd {
		b.WriteString(fmt.Sprintf(`/ip/firewall/filter add %s place-before=[find where comment="ftn: wg no forward in"] comment="%s"`+"\n", r.Args(), rq(r.Comment)))
	}
	for _, r := range in {
		b.WriteString(fmt.Sprintf(`/ip/firewall/filter add %s place-before=0 comment="%s"`+"\n", r.Args(), rq(r.Comment)))
	}
	for _, r := range nat {
		b.WriteString(fmt.Sprintf(`/ip/firewall/nat add %s comment="%s"`+"\n", r.Args(), rq(r.Comment)))
	}
	return b.String()
}

// OLTForward is where the server dials to reach an OLT: the router's tunnel address and the forwarded port.
func OLTForward(s store.Site, vlan int, serverIP string, t OLTTarget) (host string, port int, err error) {
	p, err := PlanOLTMgmt(s, vlan, serverIP, nil)
	if err != nil {
		return "", 0, err
	}
	port, err = p.ForwardPort(t.IP, t.Proto)
	return s.TunnelIP, port, err
}

// OLTPlan is the OLT management plan of a router with the server's tunnel address applied.
func (m *Manager) OLTPlan(s store.Site, vlan int, targets []OLTTarget) (OLTMgmtPlan, error) {
	return PlanOLTMgmt(s, vlan, ServerTunnelIP(m.pfx), targets)
}

// OLTForward is where the server dials to reach the OLT t behind router s.
func (m *Manager) OLTForward(s store.Site, vlan int, t OLTTarget) (string, int, error) {
	return OLTForward(s, vlan, ServerTunnelIP(m.pfx), t)
}
