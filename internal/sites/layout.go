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

// ApplyDefaults fills the blank layout fields of a site. For an env-mode site
// the router layout comes from the environment (the legacy settings), not from
// the stored record, and the result is for display and the collector only.
func ApplyDefaults(cfg config.Config, s store.Site) store.Site {
	if s.Mode == store.SiteModeEnv {
		s.HotspotServer = nonEmpty(cfg.HotspotServer, "hotspot1")
		s.WANInterface = nonEmpty(cfg.WANInterface, "ether1")
		s.OLTInterface = nonEmpty(cfg.OLTInterface, "sfp-sfpplus1")
		s.CustomerSubnet = nonEmpty(cfg.CustomerSubnet, "10.5.50.0/24")
		s.WANCapMbps, s.OLTCapMbps, s.FUPTB = cfg.WANCapacityMbps, cfg.OLTCapacityMbps, cfg.FUPTB
		s.RESTUser = cfg.RouterUser
	}
	s.HotspotServer = nonEmpty(s.HotspotServer, "hotspot1")
	s.WANInterface = nonEmpty(s.WANInterface, "ether1")
	s.WANList = "WAN" // the script's list names are fixed
	s.WANMode = nonEmpty(s.WANMode, WANDHCP)
	s.LANPorts = nonEmpty(strings.ReplaceAll(s.LANPorts, " ", ""), DefaultLANPorts)
	s.LANSubnet = nonEmpty(s.LANSubnet, DefaultLANSubnet)
	s.OLTInterface = nonEmpty(s.OLTInterface, "sfp-sfpplus1")
	s.CustomerSubnet = nonEmpty(s.CustomerSubnet, "10.5.50.0/24")
	if s.WANCapMbps <= 0 {
		s.WANCapMbps = nonZero(cfg.WANCapacityMbps, 500)
	}
	if s.OLTCapMbps <= 0 {
		s.OLTCapMbps = nonZero(cfg.OLTCapacityMbps, 10000)
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

// ValidateNet is Validate with the tunnel subnet to keep clear of.
func ValidateNet(s store.Site, wgNet netip.Prefix) error {
	name := strings.TrimSpace(s.Name)
	if name == "" || len([]rune(name)) > 60 {
		return errors.New("the router needs a name of 1 to 60 characters")
	}
	for _, f := range []struct{ label, v string }{{"WAN interface", s.WANInterface}, {"OLT interface", s.OLTInterface}} {
		if !ValidIface(f.v) {
			return fmt.Errorf("%s %q: use letters, digits, dot, dash and underscore only", f.label, f.v)
		}
	}
	if s.BenchPort != "" && !ValidIface(s.BenchPort) {
		return fmt.Errorf("bench port %q: use letters, digits, dot, dash and underscore only", s.BenchPort)
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
		if strings.ContainsAny(s.PPPoEUser, "\"\r\n") {
			return errors.New("the PPPoE user name has a character that cannot be used")
		}
	default:
		return fmt.Errorf("WAN type %q: use dhcp, static or pppoe", s.WANMode)
	}
	ports := SplitPorts(s.LANPorts)
	if len(ports) == 0 {
		return errors.New("the admin LAN needs at least one port")
	}
	seen := map[string]bool{s.WANInterface: true, s.OLTInterface: true}
	if s.BenchPort != "" {
		seen[s.BenchPort] = true
	}
	for _, p := range ports {
		if !ValidIface(p) {
			return fmt.Errorf("admin LAN port %q: use letters, digits, dot, dash and underscore only", p)
		}
		if seen[p] {
			return fmt.Errorf("port %s is used twice (WAN, fibre, bench and admin LAN ports must differ)", p)
		}
		seen[p] = true
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
	if wgNet.IsValid() {
		if lp.Overlaps(wgNet) || cp.Overlaps(wgNet) {
			return fmt.Errorf("the LAN and customer subnets must not overlap the tunnel subnet %s", wgNet)
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
