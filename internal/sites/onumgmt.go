package sites

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"ispbill/internal/store"
)

// ONU management network (TR-069).
//
// The customers' ONUs reach the billing server's built-in ACS over a separate VLAN of the OLT port, never over
// the customer network:
//
//	ONU (TR-069 WAN, VLAN 99) -> router vlanNN-onu (10.6.<n>.1/24) -> wg-ftn -> server 10.99.0.1:7547
//
// The router has an address and a DHCP server on that VLAN and a firewall that lets the management subnet reach
// the ACS (tcp 7547), the router's DNS and NTP, and nothing else (no internet, no customer network). The server
// may reach the subnet on the ONUs' connection-request ports and nothing else. The server owns the routing back:
// the router's management /24 is in that router's AllowedIPs on the server's wg0 (internal/wg).
//
// Router-side: the bootstrap script builds all of it (section 4b). The server never writes /user, /ip/service,
// /system, wireguard, routes or the WAN over REST; the sweep only reads these objects (CheckONUMgmt).
const (
	// ONUMgmtDHCPServer, ONUMgmtPool and ONUMgmtOption are the names of the router objects of the management network.
	ONUMgmtDHCPServer = "ftn-onu-dhcp"
	ONUMgmtPool       = "ftn-onu-pool"
	ONUMgmtOption     = "ftn-onu-acs"

	onuPrefix = "ftn: onu " // every object of the management network has a comment that starts with this
	// DefaultACSPort is the ACS port on the server's tunnel address.
	DefaultACSPort = 7547
)

// Comments of the router objects of the management network.
const (
	ONUMgmtVLANComment    = "ftn: onu mgmt VLAN"
	ONUMgmtGatewayComment = "ftn: onu mgmt gateway"
	ONUMgmtPoolComment    = "ftn: onu mgmt pool"
	ONUMgmtOptComment     = "ftn: onu mgmt option 43"
	ONUMgmtDHCPComment    = "ftn: onu mgmt DHCP"
	ONUMgmtNetComment     = "ftn: onu mgmt DHCP network"
	ONUToACSComment       = "ftn: onu to acs"
	ACSToONUComment       = "ftn: acs to onu"
	ONUDropInComment      = "ftn: onu mgmt drop in"
	ONUDropOutComment     = "ftn: onu mgmt drop out"
	ONUDHCPInComment      = "ftn: onu dhcp"
	ONUDNSInComment       = "ftn: onu dns"
	ONUNTPInComment       = "ftn: onu ntp"
)

// ONUMgmtPlan is the addressing and naming of one router's management network.
type ONUMgmtPlan struct {
	N, VLAN                                        int
	Iface                                          string // vlan99-onu
	Subnet, Gateway, GatewayCIDR, PoolFrom, PoolTo string
	ServerIP                                       string
	ACSURL                                         string
	ACSPort                                        int
	CRPorts                                        []int
	OptionValue                                    string // DHCP option 43 payload, 0x-hex
	NTPServer                                      bool
}

// ONUMgmtVLANName is the VLAN interface the bootstrap builds on the OLT port.
func ONUMgmtVLANName(vlan int) string { return fmt.Sprintf("vlan%d-onu", vlan) }

// ONUMgmtParent is the interface the management VLAN sits on: the OLT port when the customers have their own VLAN
// (the port is then no bridge port), else the customer bridge, because a VLAN interface on a bridge port never
// receives a frame (the bridge takes them all first; MikroTik "Layer2 misconfiguration", VLAN on a slave interface).
func ONUMgmtParent(s store.Site) string {
	if s.VLANID > 0 {
		return s.OLTInterface
	}
	return CustomerBridge
}

// ErrNoONUMgmt: the router has no management subnet allocated.
var ErrNoONUMgmt = errors.New("ONU management is not set up on this router")

// PlanONUMgmt computes the management network of a router. serverIP is the server's tunnel address, acsPort the
// ACS port, acsURL the URL the ONUs are given (empty = http://<serverIP>:<acsPort>/), crPorts the connection-request ports
// the server may use towards the ONUs.
func PlanONUMgmt(s store.Site, serverIP string, acsPort int, acsURL string, crPorts []int) (ONUMgmtPlan, error) {
	var p ONUMgmtPlan
	if s.ONUMgmtN <= 0 || s.ONUMgmtN > 255 {
		return p, ErrNoONUMgmt
	}
	vlan := s.MgmtVLANOrDefault()
	if vlan < 2 || vlan > 4094 {
		return p, fmt.Errorf("the ONU management VLAN %d must be 2 to 4094", vlan)
	}
	if vlan == s.VLANID {
		return p, fmt.Errorf("the ONU management VLAN %d is the customer VLAN: pick another VLAN for the TR-069 management network", vlan)
	}
	if _, err := netip.ParseAddr(serverIP); err != nil {
		return p, fmt.Errorf("the server tunnel address %q is not an address", serverIP)
	}
	if acsPort <= 0 {
		acsPort = DefaultACSPort
	}
	if acsURL == "" {
		acsURL = fmt.Sprintf("http://%s:%d/", serverIP, acsPort)
	}
	if len(crPorts) == 0 {
		crPorts = []int{7547, 30005}
	}
	p = ONUMgmtPlan{
		N: s.ONUMgmtN, VLAN: vlan, Iface: ONUMgmtVLANName(vlan), Subnet: s.MgmtSubnet(), Gateway: s.MgmtGateway(),
		GatewayCIDR: s.MgmtGateway() + "/24", PoolFrom: fmt.Sprintf("10.6.%d.10", s.ONUMgmtN), PoolTo: fmt.Sprintf("10.6.%d.250", s.ONUMgmtN),
		ServerIP: serverIP, ACSURL: acsURL, ACSPort: acsPort, CRPorts: crPorts, NTPServer: true,
	}
	p.OptionValue = option43(acsURL)
	return p, nil
}

// option43 is the DHCP vendor-specific information (option 43) the Broadband Forum TR-111 form carries: sub-option 1 is
// the ACS URL. Many ONUs ignore it (the usual place is the OLT's ONU TR-069 profile); it costs nothing to offer.
func option43(url string) string {
	if len(url) == 0 || len(url) > 250 {
		return ""
	}
	b := append([]byte{1, byte(len(url))}, url...)
	return "0x" + hex.EncodeToString(b)
}

func portList(ports []int) string {
	var s []string
	for _, p := range ports {
		s = append(s, fmt.Sprint(p))
	}
	return strings.Join(s, ",")
}

// SetupLines are the RouterOS lines (section 4b of the bootstrap, and the snippet an operator pastes on a router
// that is already built) that make the management network: VLAN, address, pool, DHCP option 43, DHCP server and
// network (no DNS server: the ACS URL is an address, so the ONUs have nothing to look up; the router also does not
// answer them), NTP server. The VLAN answers ARP only for its own address (arp=reply-only) and the DHCP server adds
// an ARP entry per lease (add-arp=yes), so the lease table is also the ARP table and an ONU cannot take another's address. They are idempotent: an earlier copy of each object is removed first. oltIface is the
// OLT port the VLAN sits on.
func (p ONUMgmtPlan) SetupLines(oltIface string) []string {
	var l []string
	add := func(format string, a ...any) { l = append(l, "  "+fmt.Sprintf(format, a...)) }
	add(`/ip/dhcp-server remove [find where name="%s"]`, ONUMgmtDHCPServer)
	add(`/ip/dhcp-server/network remove [find where comment="%s"]`, ONUMgmtNetComment)
	add(`/ip/dhcp-server/option remove [find where name="%s"]`, ONUMgmtOption)
	add(`/ip/pool remove [find where name="%s"]`, ONUMgmtPool)
	add(`/ip/address remove [find where comment="%s"]`, ONUMgmtGatewayComment)
	add(`/interface/vlan remove [find where name="%s"]`, p.Iface)
	add(`/interface/vlan add name=%s vlan-id=%d interface=%s arp=reply-only comment="%s"`, p.Iface, p.VLAN, oltIface, ONUMgmtVLANComment)
	add(`/ip/address add address=%s interface=%s comment="%s"`, p.GatewayCIDR, p.Iface, ONUMgmtGatewayComment)
	add(`/ip/pool add name=%s ranges=%s-%s comment="%s"`, ONUMgmtPool, p.PoolFrom, p.PoolTo, ONUMgmtPoolComment)
	netOpts := ""
	if p.OptionValue != "" {
		// DHCP option 43 (vendor specific, sub-option 1 = ACS URL). The OLT's ONU TR-069 profile is the usual way to set the ACS URL.
		add(`/ip/dhcp-server/option add name=%s code=43 value=%s comment="%s"`, ONUMgmtOption, p.OptionValue, ONUMgmtOptComment)
		netOpts = " dhcp-option=" + ONUMgmtOption
	}
	add(`/ip/dhcp-server add name=%s interface=%s address-pool=%s lease-time=1d add-arp=yes disabled=no comment="%s"`, ONUMgmtDHCPServer, p.Iface, ONUMgmtPool, ONUMgmtDHCPComment)
	add(`/ip/dhcp-server/network add address=%s gateway=%s ntp-server=%s%s comment="%s"`, p.Subnet, p.Gateway, p.Gateway, netOpts, ONUMgmtNetComment)
	if p.NTPServer {
		// The ONUs take their clock from the router (they have no internet). The router's NTP client keeps the router right.
		add(`:do { /system/ntp/server set enabled=yes } on-error={ :put "   (this RouterOS has no NTP server setting; the ONUs use their own clock)" }`)
	}
	return l
}

// Rules are the firewall rules of the management network, in the order they go into the chains: Forward before
// "ftn: wg no forward in" (those drop everything that crosses the tunnel interface) and Input before the final drop.
func (p ONUMgmtPlan) Rules() (forward, input []ipoeRule) {
	f := func(args, comment string) ipoeRule { return newIpoeRule("filter", args, comment) }
	forward = []ipoeRule{
		f(fmt.Sprintf("chain=forward action=accept protocol=tcp in-interface=%s out-interface=%s src-address=%s dst-address=%s/32 dst-port=%d",
			p.Iface, WGInterface, p.Subnet, p.ServerIP, p.ACSPort), ONUToACSComment),
		f(fmt.Sprintf("chain=forward action=accept protocol=tcp in-interface=%s out-interface=%s src-address=%s/32 dst-address=%s dst-port=%s",
			WGInterface, p.Iface, p.ServerIP, p.Subnet, portList(p.CRPorts)), ACSToONUComment),
		f("chain=forward action=drop in-interface="+p.Iface, ONUDropInComment),
		f("chain=forward action=drop out-interface="+p.Iface, ONUDropOutComment),
	}
	input = []ipoeRule{
		f("chain=input action=accept protocol=udp dst-port=67 in-interface="+p.Iface, ONUDHCPInComment),
		f("chain=input action=accept protocol=udp dst-port=123 in-interface="+p.Iface, ONUNTPInComment),
	}
	return forward, input
}

// Snippet is what the operator pastes (WinBox terminal) on a router that was built before ONU management
// existed: the setup lines, then the firewall rules placed above the rules they must come before. It needs no
// rebuild and touches nothing that is not ours.
func (p ONUMgmtPlan) Snippet(oltIface string) string {
	var b strings.Builder
	b.WriteString("# Fast Tech Net: ONU management network " + p.Subnet + " (VLAN " + fmt.Sprint(p.VLAN) + "). Paste in the router terminal.\n")
	for _, l := range p.SetupLines(oltIface) {
		b.WriteString(strings.TrimPrefix(l, "  ") + "\n")
	}
	b.WriteString(`/ip/firewall/filter remove [find where comment~"^ftn: (onu|acs to onu)"]` + "\n")
	fwd, in := p.Rules()
	for _, r := range fwd {
		b.WriteString(fmt.Sprintf(`/ip/firewall/filter add %s place-before=[find where comment="ftn: wg no forward in"] comment="%s"`+"\n", r.Args(), rq(r.Comment)))
	}
	for _, r := range in {
		b.WriteString(fmt.Sprintf(`/ip/firewall/filter add %s place-before=[find where comment="ftn: input drop rest"] comment="%s"`+"\n", r.Args(), rq(r.Comment)))
	}
	return b.String()
}

// checkONUOverlap refuses a layout where another network of the router overlaps the management subnet.
func checkONUOverlap(p ONUMgmtPlan, cust SubnetPlan, lan LANPlan, wgNet, mgmt netip.Prefix, s store.Site) error {
	mp := netip.MustParsePrefix(p.Subnet)
	for _, c := range []struct{ name, cidr string }{{"customer network", cust.Network}, {"admin LAN", lan.Network}, {"tunnel network", wgNet.String()}, {"remote-WinBox network", mgmt.String()}} {
		if np, err := netip.ParsePrefix(c.cidr); err == nil && np.Overlaps(mp) {
			return fmt.Errorf("the %s %s overlaps the ONU management network %s: change that network", c.name, c.cidr, p.Subnet)
		}
	}
	if s.WANMode == WANStatic {
		if wp, err := netip.ParsePrefix(strings.TrimSpace(s.WANAddress)); err == nil && wp.Masked().Overlaps(mp) {
			return fmt.Errorf("the WAN network %s overlaps the ONU management network %s", wp.Masked(), p.Subnet)
		}
	}
	return nil
}
