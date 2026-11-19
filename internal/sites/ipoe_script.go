package sites

import (
	"fmt"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/netplan"
)

// The two address lists that mean "walled": unregistered devices (the
// ftn-unreg pool range) and flats with no paid time.
const listUnreg = "ftn-unreg"

// ipoeRule is one firewall or NAT rule of the walled garden as data: it is
// rendered to a pasteable script line (ipoeRules, the frozen paste scripts) and
// to a REST body (Body, the server-run provisioning job).
type ipoeRule struct {
	Table   string // "filter" or "nat"
	Props   [][2]string
	Comment string
}

// newIpoeRule parses "k=v k=v" (values hold no spaces).
func newIpoeRule(table, args, comment string) ipoeRule {
	r := ipoeRule{Table: table, Comment: comment}
	for _, f := range strings.Fields(args) {
		k, v, _ := strings.Cut(f, "=")
		r.Props = append(r.Props, [2]string{k, v})
	}
	return r
}

// Args is the rule as "k=v k=v" (the script text; $ifc is the script's interface variable).
func (r ipoeRule) Args() string {
	var b strings.Builder
	for i, p := range r.Props {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p[0] + "=" + p[1])
	}
	return b.String()
}

// Path is the REST path of the rule's table.
func (r ipoeRule) Path() string {
	if r.Table == "nat" {
		return "/ip/firewall/nat"
	}
	return "/ip/firewall/filter"
}

// Body is the REST body of the rule for the customer interface iface ($ifc is replaced).
func (r ipoeRule) Body(iface string) map[string]string {
	out := map[string]string{"comment": r.Comment}
	for _, p := range r.Props {
		out[p[0]] = strings.ReplaceAll(p[1], "$ifc", iface)
	}
	return out
}

// ipoeRuleSet builds the walled-garden rules, each table top to bottom.
//
// Forward chain, top to bottom, for each walled list (unregistered devices,
// flats with no paid time): accept tcp 80/443 to PUBLIC_IP (the payment page),
// reject tcp with a reset (HTTPS fails at once), reject everything else. DNS
// is not forwarded: it is redirected to the router (nat). Then established and
// related traffic of the customer network is accepted above FastTrack, so the
// per-flat queues see it, and flat-to-flat traffic is dropped.
// Input chain: the REST port is blocked from the flats and a walled flat may
// only use the router's DNS, DHCP and ping.
//
// Known gap (security review L-1): the router resolves every name for a walled
// flat, so a DNS tunnel (dnstt, iodine) gives slow free internet. RouterOS 7 has
// no per-client DNS policy ( /ip/dns/static FWD entries add names, they do not
// remove the others), and a layer-7 regexp on DNS payloads cannot be tested
// without a router and is heavy on small CPUs, so the rules are left alone and
// the checklist carries a warning (CheckWalledDNS). Do not "fix" this by
// dropping DNS for walled flats: the payment page and the phones' captive-portal
// probes need names.
func ipoeRuleSet(plan netplan.Plan, publicIP string, restPort int) (filter, nat []ipoeRule) {
	lists := []string{listUnreg, billing.SuspendedList}
	f := func(args, comment string) ipoeRule { return newIpoeRule("filter", args, comment) }
	var fwd []ipoeRule
	for _, l := range lists {
		fwd = append(fwd, f("chain=forward action=accept protocol=tcp dst-port=80,443 dst-address="+publicIP+" src-address-list="+l, "ftn: ipoe walled payment page "+l))
	}
	for _, l := range lists {
		fwd = append(fwd, f("chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list="+l, "ftn: ipoe walled reject tcp "+l))
	}
	for _, l := range lists {
		fwd = append(fwd, f("chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list="+l, "ftn: ipoe walled reject "+l))
	}
	fwd = append(fwd,
		f("chain=forward action=accept connection-state=established,related src-address="+plan.Network, "ftn: ipoe customer est/rel out (above FastTrack, so queues apply)"),
		f("chain=forward action=accept connection-state=established,related dst-address="+plan.Network, "ftn: ipoe customer est/rel in (above FastTrack, so queues apply)"),
		f("chain=forward action=drop in-interface=$ifc out-interface=$ifc", "ftn: ipoe isolate flats"),
	)
	in := []ipoeRule{
		f(fmt.Sprintf("chain=input action=drop protocol=tcp in-interface=$ifc dst-port=%d", restPort), "ftn: ipoe block rest"),
		f("chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc", "ftn: ipoe dns udp"),
		f("chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc", "ftn: ipoe dns tcp"),
	}
	for _, l := range lists {
		in = append(in,
			f("chain=input action=accept protocol=udp dst-port=67 src-address-list="+l, "ftn: ipoe walled dhcp "+l),
			f("chain=input action=accept protocol=icmp src-address-list="+l, "ftn: ipoe walled icmp "+l),
			f("chain=input action=drop src-address-list="+l, "ftn: ipoe walled input drop "+l),
		)
	}
	filter = append(fwd, in...) // top to bottom of the table (the chains do not interact)
	n := func(args, comment string) ipoeRule { return newIpoeRule("nat", args, comment) }
	for _, l := range lists {
		nat = append(nat,
			n("chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list="+l+" to-ports=53", "ftn: ipoe walled dns redirect udp "+l),
			n("chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list="+l+" to-ports=53", "ftn: ipoe walled dns redirect tcp "+l),
		)
	}
	for _, l := range lists {
		nat = append(nat, n("chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!"+publicIP+" src-address-list="+l+" to-addresses="+publicIP+" to-ports=80",
			"ftn: ipoe walled http to the payment page "+l))
	}
	return filter, nat
}
