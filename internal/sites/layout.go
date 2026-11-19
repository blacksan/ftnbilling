package sites

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"ispbill/internal/config"
	"ispbill/internal/netplan"
	"ispbill/internal/store"
)

// Fixed names used by the join script and the REST layer.
const (
	WGInterface    = "wg-ftn"
	DefaultREST    = 8081
	DefaultUser    = "ftn-api"
	CustomerBridge = "br-customers"
	LANBridge      = "br-lan"
	TunnelMTU      = 1380

	// WAN modes.
	WANDHCP   = "dhcp"
	WANStatic = "static"
	WANPPPoE  = "pppoe"

	DefaultLANPorts  = "ether2"
	DefaultLANSubnet = "192.168.88.0/24"
)

// ServerTunnelIP is the server's address inside WG_SUBNET: the first host.
func ServerTunnelIP(prefix netip.Prefix) string { return prefix.Masked().Addr().Next().String() }

var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidIface accepts RouterOS interface names that are safe to put in a script.
func ValidIface(s string) bool { return ifaceRe.MatchString(s) }

// SubnetPlan is the addressing Go computes for a customer subnet (see
// package netplan: gateway, unregistered pool, static range).
type SubnetPlan = netplan.Plan

// PlanSubnet accepts /20 to /24 IPv4 subnets. The gateway is the first host,
// the unregistered pool is .10-.59 and the flats' static range starts at .60.
func PlanSubnet(cidr string) (SubnetPlan, error) { return netplan.PlanSubnet(cidr) }

// ApplyDefaults fills the blank layout fields of a site (the result is for the
// script, the collector and display; the stored record keeps its blanks).
func ApplyDefaults(cfg config.Config, s store.Site) store.Site {
	s.WANInterface = nonEmpty(s.WANInterface, "ether1")
	s.WANList = "WAN" // the script's list names are fixed
	s.WANMode = nonEmpty(s.WANMode, WANDHCP)
	s.LANPorts = nonEmpty(strings.ReplaceAll(s.LANPorts, " ", ""), DefaultLANPorts)
	s.LANSubnet = nonEmpty(s.LANSubnet, DefaultLANSubnet)
	s.OLTInterface = nonEmpty(s.OLTInterface, "sfp-sfpplus1")
	s.CustomerPorts = strings.Join(SplitPorts(s.CustomerPorts), ",")
	s.CustomerSubnet = nonEmpty(s.CustomerSubnet, "10.5.50.0/24")
	if s.WANCapMbps <= 0 {
		s.WANCapMbps = nonZero(cfg.WANCapacityMbps, 500)
	}
	if s.OLTCapMbps <= 0 {
		// An explicit value is kept; otherwise the port decides (an SFP+ port is
		// 10G, an Ethernet or 1G SFP port is 1G), and an unknown name gets the global default.
		if d := OLTPortMbps(s.OLTInterface); d > 0 {
			s.OLTCapMbps = d
		} else {
			s.OLTCapMbps = nonZero(cfg.OLTCapacityMbps, 10000)
		}
	}
	if s.FUPTB <= 0 {
		s.FUPTB = nonZero(cfg.FUPTB, 15)
	}
	if s.Mode == store.SiteModeWG {
		s.RESTUser = nonEmpty(s.RESTUser, DefaultUser)
		if s.RESTPort == 0 {
			s.RESTPort = DefaultREST
		}
	}
	return s
}

// OLTPortMbps is the link speed assumed for an OLT uplink port by its name:
// sfp-sfpplus* and qsfp* 10000, sfp* (1G), ether* and combo* 1000; 0 when the
// name says nothing.
func OLTPortMbps(iface string) float64 {
	n := strings.ToLower(strings.TrimSpace(iface))
	switch {
	case strings.HasPrefix(n, "sfp-sfpplus"), strings.HasPrefix(n, "sfpplus"), strings.HasPrefix(n, "qsfp"):
		return 10000
	case strings.HasPrefix(n, "sfp"), strings.HasPrefix(n, "ether"), strings.HasPrefix(n, "combo"):
		return 1000
	}
	return 0
}

// IsPortName says whether an interface name is a physical port the OLT or a
// customer device can be plugged into (ether, sfp, qsfp, combo).
func IsPortName(iface string) bool { return OLTPortMbps(iface) > 0 }

// FieldError is a validation error that belongs to one field of the router
// form; Field is the API name of the field.
type FieldError struct {
	Field, Msg string
	Also       []string // other fields the error is about too
}

func (e *FieldError) Error() string { return e.Msg }

func fieldErr(field, format string, a ...any) error {
	return &FieldError{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// customerVLANName is the VLAN interface the bootstrap script builds on the OLT port.
func customerVLANName(id int) string { return fmt.Sprintf("vlan%d-customers", id) }

// SplitPorts splits a comma list of interface names, dropping blanks.
func SplitPorts(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LANPlan is the addressing of the admin LAN: gateway (the first host), the
// DHCP pool and the network.
type LANPlan struct {
	Network, Gateway, GatewayCIDR, PoolFrom, PoolTo string
}

// PlanLAN accepts IPv4 /16 to /29 networks. The gateway is the first host; the
// pool starts at .10 (or .2 on a network too small for that) and ends at the
// last host.
func PlanLAN(cidr string) (LANPlan, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil || !p.Addr().Is4() {
		return LANPlan{}, fmt.Errorf("admin LAN subnet %q must be an IPv4 network like 192.168.88.0/24", cidr)
	}
	if p.Bits() < 16 || p.Bits() > 29 {
		return LANPlan{}, fmt.Errorf("admin LAN subnet %q: use a /16 to /29 network", cidr)
	}
	p = p.Masked()
	gw := p.Addr().Next()
	b := p.Addr().As4()
	size := 1 << (32 - p.Bits())
	last := addInt(b, size-2) // last host
	from := addInt(b, 10)
	if size < 32 {
		from = addInt(b, 2)
	}
	return LANPlan{Network: p.String(), Gateway: gw.String(), GatewayCIDR: fmt.Sprintf("%s/%d", gw, p.Bits()), PoolFrom: from.String(), PoolTo: last.String()}, nil
}

func addInt(b [4]byte, n int) netip.Addr {
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(n)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Validate checks the editable layout fields of a site. They end up inside the
// bootstrap script, so interface names, addresses and the subnets are held to a
// strict shape. wgNet (may be the zero Prefix) is the WireGuard tunnel subnet
// the LAN and customer subnets must not overlap.
func Validate(s store.Site) error { return ValidateNet(s, netip.Prefix{}) }

// DefaultTunnelRange is the tunnel subnet assumed when the caller has none.
var DefaultTunnelRange = netip.MustParsePrefix("10.99.0.0/22")

// HasControl reports whether s holds a control character (anything below 0x20
// or 0x7f): a CR, LF or TAB inside a value would change the script around it.
func HasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// DefaultMgmtRange is the remote-WinBox (wg-mgmt) subnet assumed when the
// caller has none: MGMT_SUBNET's default.
var DefaultMgmtRange = netip.MustParsePrefix("10.98.0.0/24")

// ValidMgmtSubnet reports why p cannot be the remote-WinBox subnet: it must be
// a private IPv4 prefix from /16 to /29 (the server takes .1, devices the rest).
func ValidMgmtSubnet(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() || !p.Addr().IsPrivate() || p.Bits() < 16 || p.Bits() > 29 {
		return fmt.Errorf("management subnet %s must be a private IPv4 prefix from /16 to /29", p)
	}
	return nil
}

// ValidateNet is Validate with the tunnel subnet to keep clear of. The
// remote-WinBox subnet is the default one (see ValidateNetMgmt).
func ValidateNet(s store.Site, wgNet netip.Prefix) error {
	return ValidateNetMgmt(s, wgNet, netip.Prefix{})
}

// ValidateNetMgmt is ValidateNet with the remote-WinBox (wg-mgmt) subnet too:
// the LAN, customer, tunnel and static WAN networks must not overlap it, since
// the router sends the replies to those addresses into the tunnel. A zero mgmt
// means DefaultMgmtRange.
func ValidateNetMgmt(s store.Site, wgNet, mgmt netip.Prefix) error {
	name := strings.TrimSpace(s.Name)
	if name == "" || len([]rune(name)) > 60 {
		return errors.New("the router needs a name of 1 to 60 characters")
	}
	if HasControl(s.Name) {
		return errors.New("the router name has a control character (a line break or tab) that cannot be used")
	}
	if !ValidIface(s.WANInterface) {
		return fieldErr("wan_interface", "WAN interface %q: use letters, digits, dot, dash and underscore only", s.WANInterface)
	}
	if !ValidIface(s.OLTInterface) {
		return fieldErr("olt_interface", "OLT uplink port %q: use letters, digits, dot, dash and underscore only", s.OLTInterface)
	}
	for _, p := range SplitPorts(s.CustomerPorts) {
		if !ValidIface(p) {
			return fieldErr("customer_ports", "customer port %q: use letters, digits, dot, dash and underscore only", p)
		}
	}
	if s.VLANID < 0 || s.VLANID > 4094 {
		return errors.New("VLAN ID must be 0 (none) to 4094")
	}
	plan, err := PlanSubnet(s.CustomerSubnet)
	if err != nil {
		return err
	}
	switch s.WANMode {
	case "", WANDHCP:
	case WANStatic:
		a, err := netip.ParsePrefix(strings.TrimSpace(s.WANAddress))
		if err != nil || !a.Addr().Is4() || a.Bits() < 8 || a.Bits() > 30 {
			return fmt.Errorf("WAN address %q must be an IPv4 address with its prefix, like 203.0.113.10/29", s.WANAddress)
		}
		g, err := netip.ParseAddr(strings.TrimSpace(s.WANGateway))
		if err != nil || !g.Is4() {
			return fmt.Errorf("WAN gateway %q must be an IPv4 address", s.WANGateway)
		}
		if !a.Masked().Contains(g) {
			return fmt.Errorf("WAN gateway %s is outside the WAN network %s", g, a.Masked())
		}
		for _, d := range SplitPorts(s.WANDNS) {
			if ip, err := netip.ParseAddr(d); err != nil || !ip.Is4() {
				return fmt.Errorf("WAN DNS server %q must be an IPv4 address", d)
			}
		}
	case WANPPPoE:
		if strings.TrimSpace(s.PPPoEUser) == "" {
			return errors.New("PPPoE needs a user name")
		}
		if s.PPPoEPassEnc == "" {
			return errors.New("PPPoE needs a password")
		}
		if strings.Contains(s.PPPoEUser, "\"") || HasControl(s.PPPoEUser) {
			return errors.New("the PPPoE user name has a character that cannot be used (a quote, line break, tab or other control character)")
		}
	default:
		return fmt.Errorf("WAN type %q: use dhcp, static or pppoe", s.WANMode)
	}
	ports := SplitPorts(s.LANPorts)
	if len(ports) == 0 {
		return errors.New("the admin LAN needs at least one port")
	}
	if s.WANInterface == s.OLTInterface {
		return fieldErr("olt_interface", "OLT uplink port %s is also the WAN port: the OLT needs a port of its own", s.OLTInterface)
	}
	custSeen := map[string]bool{}
	for _, c := range SplitPorts(s.CustomerPorts) {
		switch {
		case c == s.WANInterface:
			return fieldErr("customer_ports", "%s is the WAN port, so it cannot also be a customer port", c)
		case c == s.OLTInterface:
			return &FieldError{Field: "customer_ports", Also: []string{"olt_interface"},
				Msg: fmt.Sprintf("%s is the OLT uplink port, so it cannot also be a customer port", c)}
		case custSeen[c]:
			return fieldErr("customer_ports", "customer port %s is listed twice", c)
		}
		custSeen[c] = true
	}
	lanSeen := map[string]bool{}
	for _, p := range ports {
		if !ValidIface(p) {
			return fmt.Errorf("admin LAN port %q: use letters, digits, dot, dash and underscore only", p)
		}
		switch {
		case p == s.OLTInterface:
			return fieldErr("olt_interface", "OLT uplink port %s is an admin LAN port: pick a port that is not in the admin LAN list", p)
		case custSeen[p]:
			return fieldErr("customer_ports", "%s is an admin LAN port, so it cannot also be a customer port", p)
		case p == s.WANInterface, lanSeen[p]:
			return fmt.Errorf("port %s is used twice (WAN, fibre, customer and admin LAN ports must differ)", p)
		}
		lanSeen[p] = true
	}

	lan, err := PlanLAN(s.LANSubnet)
	if err != nil {
		return err
	}
	lp, _ := netip.ParsePrefix(lan.Network)
	cp, _ := netip.ParsePrefix(plan.Network)
	if lp.Overlaps(cp) {
		return fmt.Errorf("admin LAN subnet %s overlaps the customer subnet %s", lp, cp)
	}
	if !wgNet.IsValid() {
		wgNet = DefaultTunnelRange
	}
	wgNet = wgNet.Masked()
	if !mgmt.IsValid() {
		mgmt = DefaultMgmtRange
	}
	if err := ValidMgmtSubnet(mgmt); err != nil {
		return err
	}
	mgmt = mgmt.Masked()
	nets := []struct {
		label string
		p     netip.Prefix
	}{{"admin LAN subnet", lp}, {"customer subnet", cp}, {"tunnel subnet", wgNet}, {"management subnet", mgmt}}
	if s.WANMode == WANStatic {
		if a, err := netip.ParsePrefix(strings.TrimSpace(s.WANAddress)); err == nil {
			nets = append(nets, struct {
				label string
				p     netip.Prefix
			}{"static WAN network", a.Masked()})
		}
	}
	for i := range nets {
		for j := i + 1; j < len(nets); j++ {
			if nets[i].p.Overlaps(nets[j].p) {
				return fmt.Errorf("the %s %s overlaps the %s %s: they must be separate networks", nets[i].label, nets[i].p, nets[j].label, nets[j].p)
			}
		}
	}
	if s.WANCapMbps < 0 || s.OLTCapMbps < 0 || s.FUPTB < 0 {
		return errors.New("capacities cannot be negative")
	}
	if s.RESTPort < 0 || s.RESTPort > 65535 {
		return errors.New("REST port is not valid")
	}
	return nil
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return strings.TrimSpace(s)
}

func nonZero(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}
