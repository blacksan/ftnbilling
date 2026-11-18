package sites

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
)

// provWriter is the only way the router provisioning job writes to a router.
// Like fixWriter (fixes.go) it is an allowlist behind a narrow interface: the
// job holds a provWriter, never the router client, so a bug in a step cannot
// reach a path that is not listed here.
//
// May be added (always with a comment that starts "ftn:" unless said otherwise):
//
//	/interface/bridge, /interface/vlan          the customer bridge and VLAN
//	/interface/bridge/port                      the customer bridge's own ports (OLT, VLAN, bench)
//	/ip/address                                 the customer gateway address only
//	/ip/pool                                    ftn-unreg
//	/ip/dhcp-server, .../network, .../lease     ftn-dhcp on the customer interface, its network and leases
//	/ip/arp                                     static entries on the customer interface
//	/queue/simple                               "ftn-" queues of flats
//	/ip/firewall/filter, /ip/firewall/nat       "ftn: ipoe" rules, "ftn: customer dns", "ftn: no WAN DNS"
//	/ip/firewall/address-list                   ftn-unreg, ftn-suspended, and the kit's markers
//	                                            (ftn-cmd, ftn-armed, ftn-undo; no comment rule, a fixed address each)
//	/ip/hotspot/ip-binding                      "ftn:acct:" rows of type bypassed (A1)
//
// May be changed: the rows above that carry "ftn:" in their comment, and without that comment
//
//	/ip/hotspot, /ip/dhcp-server                "disabled" only, on the customer interface (switch and rollback)
//	/interface/bridge|vlan|ethernet             "arp" only, of the customer interface
//	/interface/bridge/port                      "bridge" only, of the building's OLT or bench port
//
// May be removed: rows with "ftn:" in the comment (firewall, address-list markers by list, ARP,
// lease, queue, ip-binding "ftn:acct:").
//
// Commands: /ip/dns/set (allow-remote-requests only), /ip/traffic-flow/set (enabled, interfaces),
// /system/reboot (release plan A2; no body).
//
// Never: /user, /ip/service, /system/* (except reboot), /interface/wireguard, routes, the WAN
// interface or its address, IPv6, containers.
type provWriter struct {
	rt   rowWriter
	insp routeros.Inspector
	sc   provScope
	// audit is told about every successful write.
	audit func(string)
}

// provScope is what the job is allowed to touch for one building.
type provScope struct {
	iface   string          // the customer interface (bridge, VLAN or port) the flats are on
	bridge  string          // the customer bridge the job may create ("" = it does not create one)
	vlan    string          // the customer VLAN the job may create ("" = none)
	ports   map[string]bool // interfaces that may join the customer bridge (OLT, VLAN, bench)
	gateway string          // gateway address with prefix, 10.5.50.1/24
	network string          // 10.5.50.0/24
	wanList string
}

// ErrProvNotAllowed is returned for a write outside the provisioning allowlist.
var ErrProvNotAllowed = errors.New("write refused: not on the provisioning allowlist")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrProvNotAllowed, fmt.Sprintf(format, a...))
}

const (
	dnsWANComment = "ftn: no WAN DNS" // permanent: it is what makes allow-remote-requests safe
	ftnPrefix     = "ftn:"
)

func ours(r map[string]string) bool { return strings.HasPrefix(r["comment"], ftnPrefix) }

func isMarkerList(l string) bool {
	return l == billing.UnregList || l == billing.SuspendedList || l == KitListCmd || l == KitListArmed || l == KitListUndo
}

// markerAddress: the kit's marker rows each have their own loopback address.
func markerAddress(list, addr string) bool {
	switch list {
	case KitListCmd:
		return addr == KitCmdAddress
	case KitListArmed:
		return addr == KitArmedAddress
	case KitListUndo:
		a, err := netip.ParseAddr(addr)
		return err == nil && a.Is4() && a.As4()[0] == 127 && a.As4()[1] == 0 && a.As4()[2] >= 1
	}
	return true
}

func disabledOnly(diff map[string]string) bool {
	if len(diff) != 1 {
		return false
	}
	v, ok := diff["disabled"]
	return ok && (v == "yes" || v == "no" || v == "true" || v == "false")
}

func (w *provWriter) done(format string, a ...any) {
	if w.audit != nil {
		w.audit(fmt.Sprintf(format, a...))
	}
}

// add creates a row.
func (w *provWriter) add(ctx context.Context, path string, body map[string]string) error {
	if err := w.allowAdd(path, body); err != nil {
		return err
	}
	id, err := w.rt.AddRow(ctx, path, body)
	if err != nil {
		return err
	}
	w.done("added %s %s", path, rowLabel(body))
	_ = id
	return nil
}

// addID is add returning the new row's id.
func (w *provWriter) addID(ctx context.Context, path string, body map[string]string) (string, error) {
	if err := w.allowAdd(path, body); err != nil {
		return "", err
	}
	id, err := w.rt.AddRow(ctx, path, body)
	if err == nil {
		w.done("added %s %s", path, rowLabel(body))
	}
	return id, err
}

func rowLabel(r map[string]string) string {
	for _, k := range []string{"comment", "name", "address", "interface"} {
		if r[k] != "" {
			return r[k]
		}
	}
	return ""
}

func (w *provWriter) allowAdd(path string, b map[string]string) error {
	sc := w.sc
	switch path {
	case "/interface/bridge":
		if sc.bridge == "" || b["name"] != sc.bridge || !ours(b) {
			return refuse("bridge %q is not the customer bridge", b["name"])
		}
	case "/interface/vlan":
		if sc.vlan == "" || b["name"] != sc.vlan || !ours(b) {
			return refuse("VLAN %q is not the customer VLAN", b["name"])
		}
	case "/interface/bridge/port":
		if sc.bridge == "" || b["bridge"] != sc.bridge || !sc.ports[b["interface"]] || b["interface"] == "" || !ours(b) {
			return refuse("bridge port %q on %q is not one of this router's customer ports", b["interface"], b["bridge"])
		}
	case "/ip/address":
		if b["address"] == "" || b["address"] != sc.gateway || (b["interface"] != sc.iface && b["interface"] != sc.bridge) || !ours(b) {
			return refuse("address %q on %q is not the customer gateway", b["address"], b["interface"])
		}
	case "/ip/pool":
		if b["name"] != billing.UnregList || !ours(b) {
			return refuse("pool %q", b["name"])
		}
	case "/ip/dhcp-server":
		if b["name"] != billing.DHCPServerName || b["interface"] != sc.iface || sc.iface == "" || !ours(b) {
			return refuse("DHCP server %q on %q", b["name"], b["interface"])
		}
	case "/ip/dhcp-server/network":
		if b["address"] != sc.network || !ours(b) {
			return refuse("DHCP network %q", b["address"])
		}
	case "/ip/dhcp-server/lease":
		if b["server"] != billing.DHCPServerName || !ours(b) {
			return refuse("lease on server %q", b["server"])
		}
	case "/ip/arp":
		if b["interface"] != sc.iface || sc.iface == "" || !ours(b) {
			return refuse("ARP entry on %q", b["interface"])
		}
	case "/queue/simple":
		if !strings.HasPrefix(b["name"], "ftn-") || !strings.HasPrefix(b["comment"], billing.AcctPrefix) {
			return refuse("queue %q", b["name"])
		}
	case "/ip/firewall/filter", "/ip/firewall/nat":
		return w.allowRule(path, b)
	case "/ip/firewall/address-list":
		if !isMarkerList(b["list"]) || !markerAddress(b["list"], b["address"]) {
			return refuse("address list %q", b["list"])
		}
		if (b["list"] == billing.UnregList || b["list"] == billing.SuspendedList) && !ours(b) {
			return refuse("address-list row without an ftn: comment")
		}
	case "/ip/hotspot/ip-binding":
		if b["type"] != "bypassed" || !strings.HasPrefix(b["comment"], billing.AcctPrefix) {
			return refuse("hotspot binding that is not an ftn:acct: bypass")
		}
	default:
		return refuse("add to %s", path)
	}
	return nil
}

// allowRule: firewall rules. Every rule the server adds is one of three
// kinds, none of them can open anything to the WAN.
func (w *provWriter) allowRule(path string, b map[string]string) error {
	c := b["comment"]
	for k := range b {
		switch k {
		case "chain", "action", "protocol", "dst-port", "src-port", "in-interface", "out-interface", "in-interface-list", "out-interface-list",
			"src-address", "dst-address", "src-address-list", "dst-address-list", "connection-state", "reject-with", "to-ports", "to-addresses",
			"comment", "place-before", "disabled":
		default:
			return refuse("firewall property %s", k)
		}
	}
	switch {
	case strings.HasPrefix(c, "ftn: ipoe"):
		switch b["action"] {
		case "accept", "drop", "reject", "redirect", "dst-nat", "masquerade":
		default:
			return refuse("action %q in an ipoe rule", b["action"])
		}
		if path == "/ip/firewall/nat" && b["chain"] != "dstnat" && b["chain"] != "srcnat" {
			return refuse("NAT chain %q", b["chain"])
		}
	case c == custDNSComment:
		if path != "/ip/firewall/filter" || b["chain"] != "input" || b["action"] != "accept" || b["dst-port"] != "53" ||
			(b["protocol"] != "udp" && b["protocol"] != "tcp") || b["in-interface"] != w.sc.iface || w.sc.iface == "" {
			return refuse("not a customer DNS accept")
		}
	case c == dnsWANComment:
		if path != "/ip/firewall/filter" || b["chain"] != "input" || b["action"] != "drop" || b["dst-port"] != "53" ||
			(b["protocol"] != "udp" && b["protocol"] != "tcp") || b["in-interface-list"] != w.sc.wanList || w.sc.wanList == "" || len(b) > 8 {
			return refuse("not a WAN DNS drop")
		}
	default:
		return refuse("a firewall rule without an ftn: ipoe, customer dns or no WAN DNS comment")
	}
	return nil
}

// set changes properties of one existing row; row is the row as just read.
func (w *provWriter) set(ctx context.Context, path string, row routeros.Row, diff map[string]string) error {
	if err := w.allowSet(path, row, diff); err != nil {
		return err
	}
	if err := w.rt.SetRow(ctx, path, row[routeros.RowID], diff); err != nil {
		return err
	}
	w.done("changed %s %s", path, rowLabel(row))
	return nil
}

func (w *provWriter) allowSet(path string, row routeros.Row, diff map[string]string) error {
	sc := w.sc
	if c, has := diff["comment"]; has && !strings.HasPrefix(c, ftnPrefix) && path != "/ip/firewall/address-list" { // marker rows carry a count or "now"
		return refuse("comment of %s changed to a non-ftn: text", path)
	}
	switch path {
	case "/ip/hotspot":
		if sc.iface == "" || row["interface"] != sc.iface || !disabledOnly(diff) {
			return refuse("a hotspot server may only be disabled or enabled, on the customer interface")
		}
		return nil
	case "/ip/dhcp-server":
		if sc.iface == "" || row["interface"] != sc.iface {
			return refuse("DHCP server %q is not on the customer interface", row["name"])
		}
		if row["name"] == billing.DHCPServerName && ours(row) {
			for k := range diff {
				switch k {
				case "disabled", "address-pool", "lease-time", "authoritative", "add-arp", "interface", "comment":
				default:
					return refuse("ftn-dhcp property %s", k)
				}
			}
			if v, ok := diff["interface"]; ok && v != sc.iface {
				return refuse("ftn-dhcp moved to %q", v)
			}
			return nil
		}
		if !disabledOnly(diff) {
			return refuse("an old DHCP server may only be disabled or enabled")
		}
		return nil
	case "/interface/bridge", "/interface/vlan", "/interface/ethernet":
		if sc.iface == "" || row["name"] != sc.iface || len(diff) != 1 || !validARP(diff["arp"]) {
			return refuse("an interface may only change the arp mode of the customer interface")
		}
		return nil
	case "/interface/bridge/port":
		if !sc.ports[row["interface"]] || row["interface"] == "" || len(diff) != 1 || diff["bridge"] == "" {
			return refuse("a bridge port may only move one of this router's customer ports to another bridge")
		}
		return nil
	case "/ip/hotspot/ip-binding":
		if !strings.HasPrefix(row["comment"], billing.AcctPrefix) {
			return refuse("a hotspot binding the server did not make")
		}
		return nil
	case "/ip/pool", "/ip/dhcp-server/network", "/ip/dhcp-server/lease", "/ip/arp", "/queue/simple",
		"/ip/firewall/filter", "/ip/firewall/nat", "/ip/address":
		if !ours(row) {
			return refuse("a row of %s the server did not make", path)
		}
		return nil
	case "/ip/firewall/address-list":
		if !isMarkerList(row["list"]) {
			return refuse("address list %q", row["list"])
		}
		if (row["list"] == billing.UnregList || row["list"] == billing.SuspendedList) && !ours(row) {
			return refuse("an address-list row the server did not make")
		}
		return nil
	}
	return refuse("change in %s", path)
}

func validARP(m string) bool {
	switch m {
	case "disabled", "enabled", "proxy-arp", "local-proxy-arp", "reply-only":
		return true
	}
	return false
}

// remove deletes one existing row; row is the row as just read.
func (w *provWriter) remove(ctx context.Context, path string, row routeros.Row) error {
	switch path {
	case "/ip/firewall/filter", "/ip/firewall/nat", "/ip/arp", "/ip/dhcp-server/lease", "/ip/hotspot/ip-binding":
		want := ftnPrefix
		if path == "/ip/hotspot/ip-binding" {
			want = billing.AcctPrefix
		}
		if !strings.HasPrefix(row["comment"], want) {
			return refuse("a row of %s the server did not make", path)
		}
	case "/queue/simple":
		if !strings.HasPrefix(row["comment"], billing.AcctPrefix) {
			return refuse("a queue the server did not make")
		}
	case "/ip/firewall/address-list":
		if !isMarkerList(row["list"]) {
			return refuse("address list %q", row["list"])
		}
		if (row["list"] == billing.UnregList || row["list"] == billing.SuspendedList) && !ours(row) {
			return refuse("an address-list row the server did not make")
		}
	default:
		return refuse("removal in %s", path)
	}
	if err := w.rt.RemoveRow(ctx, path, row[routeros.RowID]); err != nil {
		return err
	}
	w.done("removed %s %s", path, rowLabel(row))
	return nil
}

// call sends a command POST.
func (w *provWriter) call(ctx context.Context, path string, body map[string]string) error {
	switch path {
	case "/ip/dns/set":
		if len(body) != 1 || body["allow-remote-requests"] == "" {
			return refuse("/ip/dns/set may only change allow-remote-requests")
		}
	case "/ip/traffic-flow/set":
		for k := range body {
			if k != "enabled" && k != "interfaces" {
				return refuse("/ip/traffic-flow/set may only change enabled and interfaces")
			}
		}
	case "/system/reboot":
		if len(body) != 0 {
			return refuse("/system/reboot takes no arguments")
		}
	default:
		return refuse("command %s", path)
	}
	if w.insp == nil {
		return routeros.ErrNoInspector
	}
	if err := w.insp.Call(ctx, path, body); err != nil {
		return err
	}
	w.done("called %s", path)
	return nil
}
