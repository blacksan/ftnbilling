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
	WGInterface   = "wg-ftn"
	DefaultREST   = 8081
	DefaultUser   = "ftn-api"
	HotspotBridge = "br-hotspot"
	HotspotProf   = "ispbill-hsprof"
	TunnelMTU     = 1380
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
	s.WANList = nonEmpty(s.WANList, "WAN")
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

// Validate checks the editable layout fields of a site. They end up inside the
// join script, so interface names and the subnet are held to a strict shape.
func Validate(s store.Site) error {
	name := strings.TrimSpace(s.Name)
	if name == "" || len([]rune(name)) > 60 {
		return errors.New("the building needs a name of 1 to 60 characters")
	}
	for _, f := range []struct{ label, v string }{
		{"WAN interface", s.WANInterface}, {"OLT interface", s.OLTInterface}, {"WAN interface list", s.WANList}, {"hotspot server", s.HotspotServer},
	} {
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
	if _, err := PlanSubnet(s.CustomerSubnet); err != nil {
		return err
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
