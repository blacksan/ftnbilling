// Package netplan is the address plan of a building's customer subnet. It is
// plain arithmetic with no dependencies, so the join script (package sites)
// and the IPoE reconciler (package billing) compute the same addresses.
package netplan

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Plan is the addressing computed for a customer subnet.
//
//	.1            gateway
//	.10 - .59     UnregFrom - UnregTo: pool for unregistered MACs (ftn-unreg)
//	.60 - .254    StaticFrom - StaticTo: one fixed address per flat
//
// PoolFrom/PoolTo (.10 to the last address minus one) is the single pool the
// hotspot mode uses; IPoE splits it into the two ranges above.
type Plan struct {
	Network   string // 10.5.50.0/24
	Gateway   string // 10.5.50.1
	GatewayIP string // 10.5.50.1/24
	PoolFrom  string // 10.5.50.10
	PoolTo    string // last address minus one

	UnregFrom  string // 10.5.50.10
	UnregTo    string // 10.5.50.59
	StaticFrom string // 10.5.50.60
	StaticTo   string // same as PoolTo
}

// Plan accepts /20 to /24 IPv4 subnets. The gateway is the first host and the
// pool runs from .10 to the last address minus one.
func PlanSubnet(cidr string) (Plan, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil || !p.Addr().Is4() {
		return Plan{}, fmt.Errorf("customer subnet %q is not an IPv4 network like 10.5.50.0/24", cidr)
	}
	if p.Bits() < 20 || p.Bits() > 24 {
		return Plan{}, errors.New("customer subnet must be between /20 and /24")
	}
	p = p.Masked()
	base := p.Addr()
	gw := base.Next()
	from := base
	for i := 0; i < 10; i++ {
		from = from.Next()
	}
	unregTo := base
	for i := 0; i < 59; i++ {
		unregTo = unregTo.Next()
	}
	static := unregTo.Next()
	// Last address of the prefix.
	b := base.As4()
	hostBits := 32 - p.Bits()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	last := n | (1<<hostBits - 1)
	toAddr := netip.AddrFrom4([4]byte{byte(last >> 24), byte(last >> 16), byte(last >> 8), byte(last)}).Prev() // minus one: .254 for a /24
	return Plan{
		Network: p.String(), Gateway: gw.String(), GatewayIP: fmt.Sprintf("%s/%d", gw, p.Bits()),
		PoolFrom: from.String(), PoolTo: toAddr.String(),
		UnregFrom: from.String(), UnregTo: unregTo.String(),
		StaticFrom: static.String(), StaticTo: toAddr.String(),
	}, nil
}

// InStatic reports whether ip lies in the static range (the flats' addresses).
func (pl Plan) InStatic(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	from, e1 := netip.ParseAddr(pl.StaticFrom)
	to, e2 := netip.ParseAddr(pl.StaticTo)
	return e1 == nil && e2 == nil && a.Compare(from) >= 0 && a.Compare(to) <= 0
}

// InNetwork reports whether ip lies inside the subnet.
func (pl Plan) InNetwork(ip string) bool {
	a, err := netip.ParseAddr(ip)
	p, e2 := netip.ParsePrefix(pl.Network)
	return err == nil && e2 == nil && p.Contains(a)
}

// FirstFree returns the lowest static-range address for which used says false.
func (pl Plan) FirstFree(used func(ip string) bool) (string, bool) {
	a, err := netip.ParseAddr(pl.StaticFrom)
	to, e2 := netip.ParseAddr(pl.StaticTo)
	if err != nil || e2 != nil {
		return "", false
	}
	for ; a.Compare(to) <= 0; a = a.Next() {
		if s := a.String(); !used(s) {
			return s, true
		}
	}
	return "", false
}
