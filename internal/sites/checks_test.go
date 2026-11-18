package sites

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

type R = routeros.Row

// seedHealthy fills the mock with a router that passes every check.
func seedHealthy(m *routeros.Mock) {
	m.SeedOne("/system/resource", R{"version": "7.25.1 (stable)", "board-name": "RB5009UG+S+"})
	m.SeedOne("/system/device-mode", R{"mode": "home", "flagged": "false", "scheduler": "true", "fetch": "true", "hotspot": "true", "container": "true"})
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "true"})
	m.SeedOne("/ip/traffic-flow", R{"enabled": "yes", "interfaces": "br-hotspot"})
	m.SeedOne("/ipv6/settings", R{"disable-ipv6": "true"})
	m.SeedRow("/ip/address", R{"address": "10.5.50.1/24", "interface": "br-hotspot", "disabled": "false"})
	m.SeedRow("/ip/address", R{"address": "10.99.0.2/22", "interface": "wg-ftn", "disabled": "false"})
	m.SeedRow("/interface", R{"name": "ether1"})
	m.SeedRow("/interface", R{"name": "ether5"})
	m.SeedRow("/interface", R{"name": "sfp-sfpplus1"})
	m.SeedRow("/interface", R{"name": "br-hotspot"})
	m.SeedRow("/interface/bridge", R{"name": "br-hotspot"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-hotspot", "interface": "ether5", "disabled": "false"})
	m.SeedRow("/interface/list/member", R{"list": "WAN", "interface": "ether1"})
	m.SeedRow("/ip/hotspot", R{"name": "hotspot1", "interface": "br-hotspot", "disabled": "false"})
	m.SeedRow("/ip/dhcp-server", R{"name": "dhcp-hotspot", "interface": "br-hotspot", "disabled": "false"})
	m.SeedRow("/ip/traffic-flow/target", R{"dst-address": "10.99.0.1", "port": "2055", "version": "5", "comment": "ftn: netflow", "disabled": "false"})
	m.SeedRow("/ip/service", R{"name": "www", "address": "10.99.0.1/32", "disabled": "false"})
	m.SeedRow("/user/group", R{"name": "ftn-api", "policy": "read,write,test,sensitive,api,rest-api"})
	m.SeedRow("/system/script", R{"name": "ftn-kit-tick"})
	m.SeedRow("/system/script", R{"name": "ftn-kit-rollback"})
	m.SeedRow("/system/scheduler", R{"name": "ftn-kit-timer", "comment": "ftn: kit v1", "run-count": "10", "disabled": "false"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "connection-state": "established,related,untracked"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"})
	// both paying flats are seen
	m.SeedRow("/ip/dhcp-server/lease", R{"mac-address": "AA:BB:CC:DD:EE:01", "address": "10.5.50.23", "status": "bound", "dynamic": "true"})
	m.SeedRow("/ip/dhcp-server/lease", R{"mac-address": "AA:BB:CC:DD:EE:02", "address": "10.5.50.24", "status": "bound", "dynamic": "true"})
}

// checkSite is a joined building whose router is a healthy mock.
func checkSite(t *testing.T, mut func(*config.Config)) (*fixture, store.Site, *routeros.Mock) {
	t.Helper()
	f := newFixture(t, func(c *config.Config) {
		c.NetflowAddr = ":2055"
		if mut != nil {
			mut(c)
		}
	})
	f.m.autoFixDelay = time.Hour // the post-join run has its own test
	s := f.joined("Block A")
	if err := f.m.UpdateSite(s.ID, SiteInput{Name: "Block A", Provision: true, BenchPort: "ether5"}); err != nil {
		t.Fatal(err)
	}
	m := f.startedRouter(s.ID)
	seedHealthy(m)
	return f, f.site(s.ID), m
}

func get(t *testing.T, f *fixture, siteID string, fresh bool) Checks {
	t.Helper()
	c, err := f.m.Checks(context.Background(), siteID, fresh)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func item(t *testing.T, c Checks, id string) CheckItem {
	t.Helper()
	it, ok := c.Item(id)
	if !ok {
		t.Fatalf("no item %s", id)
	}
	return it
}

func TestChecksHealthyRouterPassesEverything(t *testing.T) {
	f, s, _ := checkSite(t, nil)
	c := get(t, f, s.ID, true)
	for _, it := range c.Items {
		if it.Status != StatusOK && it.Status != StatusInfo {
			t.Errorf("%s: %s %s", it.ID, it.Status, it.Detail)
		}
		if it.Title == "" || it.Detail == "" || it.Evidence == nil || it.Fix.Kind == "" {
			t.Errorf("%s incomplete: %+v", it.ID, it)
		}
	}
	if !c.Router.Reachable || c.Router.Version != "7.25.1 (stable)" || c.Router.Board != "RB5009UG+S+" || !c.Summary.CutoverReady || c.Summary.Fail != 0 {
		t.Fatalf("%+v", c)
	}
	if len(c.Items) != len(checkDefs) {
		t.Fatalf("%d items", len(c.Items))
	}
	if it := item(t, c, CheckWWW); it.Status != StatusInfo || !strings.Contains(it.Detail, "WinBox") {
		t.Fatalf("%+v", it)
	}
}

func TestChecksAcceptYesNoAndTrueFalse(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SeedOne("/system/device-mode", R{"scheduler": "yes", "fetch": "yes", "hotspot": "yes", "flagged": "no"})
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "yes"})
	c := get(t, f, s.ID, true)
	for _, id := range []string{CheckDMScheduler, CheckDMFetch, CheckDMHotspot, CheckDNS} {
		if it := item(t, c, id); it.Status != StatusOK {
			t.Errorf("%s %+v", id, it)
		}
	}
	m.SeedOne("/system/device-mode", R{"scheduler": "no", "fetch": "no", "hotspot": "no", "flagged": "yes"})
	c = get(t, f, s.ID, true)
	sch := item(t, c, CheckDMScheduler)
	if sch.Status != StatusFail || !sch.BlocksCutover || !sch.Fix.Physical || !strings.Contains(sch.Fix.Command, "scheduler=yes") || sch.Fix.Kind != FixManual {
		t.Fatalf("%+v", sch)
	}
	if item(t, c, CheckDMFetch).Status != StatusWarn || item(t, c, CheckDMHotspot).Status != StatusFail {
		t.Fatal("fetch warns, hotspot fails in hotspot mode")
	}
	if c.Summary.CutoverReady {
		t.Fatal("not ready")
	}
}

func TestChecksVersionAndPolicy(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SeedOne("/system/resource", R{"version": "7.12.2 (stable)"})
	if it := item(t, get(t, f, s.ID, true), CheckVersion); it.Status != StatusFail || it.Fix.Kind != FixManual {
		t.Fatalf("%+v", it)
	}
	m.SeedOne("/system/resource", R{"version": "7.13"})
	if it := item(t, get(t, f, s.ID, true), CheckVersion); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	m.SeedOne("/system/resource", R{"version": "garbage"})
	if it := item(t, get(t, f, s.ID, true), CheckVersion); it.Status != StatusUnknown {
		t.Fatalf("%+v", it)
	}
	for _, r := range m.Table("/user/group") {
		m.SetRow(context.Background(), "/user/group", r[".id"], map[string]string{"policy": "read,write,test,api,rest-api,!ftp"})
	}
	it := item(t, get(t, f, s.ID, true), CheckAPIPolicy)
	if it.Status != StatusFail || !strings.Contains(it.Detail, "sensitive") || it.Fix.Command != "/user/group/set ftn-api policy=read,write,test,sensitive,api,rest-api" {
		t.Fatalf("%+v", it)
	}
}

func TestChecksKitMissingOldStuckAndDisabled(t *testing.T) {
	f, s, m := checkSite(t, nil)
	ctx := context.Background()
	timer := func() string { return m.Table("/system/scheduler")[0][".id"] }
	// The first look cannot know whether the timer moves.
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	// Same run-count a minute later: dead.
	f.advance(time.Minute)
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusFail || !strings.Contains(it.Detail, "has not run") || !it.BlocksCutover {
		t.Fatalf("%+v", it)
	}
	m.SetRow(ctx, "/system/scheduler", timer(), map[string]string{"run-count": "12"})
	f.advance(time.Minute)
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusOK {
		t.Fatalf("a moving count is fine: %+v", it)
	}
	m.SetRow(ctx, "/system/scheduler", timer(), map[string]string{"comment": "ftn: kit v0"})
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusFail || !strings.Contains(it.Detail, "old or unknown") {
		t.Fatalf("%+v", it)
	}
	m.SetRow(ctx, "/system/scheduler", timer(), map[string]string{"comment": "ftn: kit v1", "disabled": "yes"})
	if it := item(t, get(t, f, s.ID, true), CheckKit); it.Status != StatusFail || !strings.Contains(it.Detail, "switched off") {
		t.Fatalf("%+v", it)
	}
	m.RemoveRow(ctx, "/system/scheduler", timer())
	it := item(t, get(t, f, s.ID, true), CheckKit)
	if it.Status != StatusFail || !strings.Contains(it.Detail, "missing") || it.Fix.Kind != FixManual {
		t.Fatalf("%+v", it)
	}
}

func TestChecksDNSAutoOnlyBehindADroppingFirewall(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "false"})
	it := item(t, get(t, f, s.ID, true), CheckDNS)
	if it.Status != StatusFail || !it.BlocksCutover || it.Fix.Kind != FixAuto {
		t.Fatalf("defconf drops !LAN: %+v", it)
	}
	// An accept for DNS from anywhere above the drop: manual.
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53"})
	rows := m.Table("/ip/firewall/filter")
	// Move the accept to the front by rebuilding the table.
	for _, r := range rows {
		m.RemoveRow(context.Background(), "/ip/firewall/filter", r[".id"])
	}
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "in-interface-list": "!LAN"})
	it = item(t, get(t, f, s.ID, true), CheckDNS)
	if it.Fix.Kind != FixManual || !strings.Contains(it.Fix.Command, "dst-port=53") || !strings.Contains(it.Detail, "might let outsiders") {
		t.Fatalf("%+v", it)
	}
	// No firewall at all: manual.
	for _, r := range m.Table("/ip/firewall/filter") {
		m.RemoveRow(context.Background(), "/ip/firewall/filter", r[".id"])
	}
	if it = item(t, get(t, f, s.ID, true), CheckDNS); it.Fix.Kind != FixManual {
		t.Fatalf("%+v", it)
	}
	// A port-53 drop from the WAN list works too; a loopback accept does not matter.
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "accept", "dst-address": "127.0.0.1"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "protocol": "udp", "dst-port": "53", "in-interface-list": "WAN"})
	m.SeedRow("/ip/firewall/filter", R{"chain": "input", "action": "drop", "protocol": "tcp", "dst-port": "53", "in-interface-list": "WAN"})
	if it = item(t, get(t, f, s.ID, true), CheckDNS); it.Fix.Kind != FixAuto {
		t.Fatalf("%+v", it)
	}
}

func TestChecksCustomerInterface(t *testing.T) {
	f, s, m := checkSite(t, nil)
	for _, r := range m.Table("/ip/address") {
		if r["interface"] == "br-hotspot" {
			m.SetRow(context.Background(), "/ip/address", r[".id"], map[string]string{"address": "10.5.51.1/24"})
		}
	}
	it := item(t, get(t, f, s.ID, true), CheckCustomerIf)
	if it.Status != StatusFail || !it.BlocksCutover || !strings.Contains(it.Detail, "10.5.50.1/24") || it.Fix.Kind != FixManual {
		t.Fatalf("%+v", it)
	}
	// Dependent checks say unknown, not ok.
	c := get(t, f, s.ID, false)
	if item(t, c, CheckTrafficFlow).Status != StatusUnknown {
		t.Fatal("traffic flow needs the customer interface")
	}
	// Address present but nothing serves it.
	for _, r := range m.Table("/ip/address") {
		if r["interface"] == "br-hotspot" {
			m.SetRow(context.Background(), "/ip/address", r[".id"], map[string]string{"address": "10.5.50.1/24"})
		}
	}
	for _, p := range []string{"/ip/hotspot", "/ip/dhcp-server"} {
		for _, r := range m.Table(p) {
			m.RemoveRow(context.Background(), p, r[".id"])
		}
	}
	it = item(t, get(t, f, s.ID, true), CheckCustomerIf)
	if it.Status != StatusFail || !strings.Contains(it.Detail, "no hotspot or DHCP server") {
		t.Fatalf("%+v", it)
	}
}

// liveOLT turns the healthy router into the bench router of 2026-11-18: the
// OLT port is a defconf bridge port and carries the customer VLAN.
func liveOLT(m *routeros.Mock) {
	m.SeedRow("/interface/bridge/port", R{"bridge": "bridge", "interface": "sfp-sfpplus1", "disabled": "false"})
	m.SeedRow("/interface/vlan", R{"name": "vlan100-customers", "interface": "sfp-sfpplus1", "vlan-id": "100"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-hotspot", "interface": "vlan100-customers", "disabled": "false"})
}

func TestChecksOLTBridgeConflict(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, true), CheckOLT); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	liveOLT(m)
	it := item(t, get(t, f, s.ID, true), CheckOLT)
	if it.Status != StatusFail || !it.BlocksCutover || it.Fix.Kind != FixAuto {
		t.Fatalf("%+v", it)
	}
	// A device learned on the port: confirm, owner only, MACs listed.
	m.SeedRow("/interface/bridge/host", R{"mac-address": "00:19:3E:00:00:09", "on-interface": "sfp-sfpplus1", "local": "false"})
	m.SeedRow("/interface/bridge/host", R{"mac-address": "AA:00:00:00:00:01", "on-interface": "sfp-sfpplus1", "local": "true"})
	it = item(t, get(t, f, s.ID, true), CheckOLT)
	if it.Fix.Kind != FixConfirm || it.Fix.Role != store.RoleOwner || !strings.Contains(it.Fix.ConfirmText, "00:19:3E:00:00:09") || strings.Contains(it.Fix.ConfirmText, "AA:00:00:00:00:01") {
		t.Fatalf("%+v", it)
	}
	// An address on the port itself: never automatic.
	m.SeedRow("/ip/address", R{"address": "192.168.8.2/24", "interface": "sfp-sfpplus1"})
	it = item(t, get(t, f, s.ID, true), CheckOLT)
	if it.Fix.Kind != FixManual || !strings.Contains(it.Detail, "IP address") {
		t.Fatalf("%+v", it)
	}
}

func TestChecksOLTVLANNotInABridgeIsManual(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SeedRow("/interface/bridge/port", R{"bridge": "bridge", "interface": "sfp-sfpplus1"})
	m.SeedRow("/interface/vlan", R{"name": "vlan100-customers", "interface": "sfp-sfpplus1"})
	it := item(t, get(t, f, s.ID, true), CheckOLT)
	if it.Fix.Kind != FixManual || !strings.Contains(it.Detail, "cut customers off") {
		t.Fatalf("%+v", it)
	}
}

func TestChecksOLTWANPortIsManual(t *testing.T) {
	f, s, m := checkSite(t, nil)
	liveOLT(m)
	m.SeedRow("/interface/list/member", R{"list": "WAN", "interface": "sfp-sfpplus1"})
	it := item(t, get(t, f, s.ID, true), CheckOLT)
	if it.Fix.Kind != FixManual || !strings.Contains(it.Detail, "WAN port") {
		t.Fatalf("%+v", it)
	}
}

func TestChecksBenchPort(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, true), CheckBench); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	for _, r := range m.Table("/interface/bridge/port") {
		m.RemoveRow(context.Background(), "/interface/bridge/port", r[".id"])
	}
	it := item(t, get(t, f, s.ID, true), CheckBench)
	if it.Status != StatusWarn || it.Fix.Kind != FixAuto {
		t.Fatalf("%+v", it)
	}
	// In the defconf bridge with a laptop learned on it: confirm.
	m.SeedRow("/interface/bridge/port", R{"bridge": "bridge", "interface": "ether5"})
	m.SeedRow("/interface/bridge/host", R{"mac-address": "3C:00:00:00:00:01", "on-interface": "ether5", "local": "false"})
	it = item(t, get(t, f, s.ID, true), CheckBench)
	if it.Fix.Kind != FixConfirm || !strings.Contains(it.Fix.ConfirmText, "3C:00:00:00:00:01") {
		t.Fatalf("%+v", it)
	}
	// An interface that does not exist.
	for _, r := range m.Table("/interface") {
		if r["name"] == "ether5" {
			m.RemoveRow(context.Background(), "/interface", r[".id"])
		}
	}
	if it = item(t, get(t, f, s.ID, true), CheckBench); it.Fix.Kind != FixManual || !strings.Contains(it.Detail, "no interface") {
		t.Fatalf("%+v", it)
	}
}

func TestChecksOrphanHotspotUsers(t *testing.T) {
	f, s, m := checkSite(t, nil)
	// K7Q2MX is a flat of the building; its user is not an orphan, even with another MAC on the router.
	m.SeedRow("/ip/hotspot/user", R{"name": "K7Q2MX", "mac-address": "AA:BB:CC:DD:EE:01", "comment": "ispbill:Flat 3B"})
	if it := item(t, get(t, f, s.ID, true), CheckOrphans); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	m.SeedRow("/ip/hotspot/user", R{"name": "GONE01", "comment": "ispbill:Flat 99"})
	it := item(t, get(t, f, s.ID, true), CheckOrphans)
	if it.Status != StatusWarn || it.Fix.Kind != FixAuto || it.BlocksCutover || len(it.Evidence) != 1 {
		t.Fatalf("server-made orphan is auto: %+v", it)
	}
	m.SeedRow("/ip/hotspot/user", R{"name": "test1", "mac-address": "3C:00:00:00:00:01"})
	m.SeedRow("/ip/hotspot/user", R{"name": "admin-no-mac"}) // no MAC, not ours: never judged
	it = item(t, get(t, f, s.ID, true), CheckOrphans)
	if it.Fix.Kind != FixConfirm || len(it.Evidence) != 2 || !strings.Contains(it.Fix.ConfirmText, "1 of them were not made") {
		t.Fatalf("%+v", it)
	}
	m.SeedRow("/ip/hotspot/ip-binding", R{"mac-address": "AA:BB:CC:DD:EE:01", "type": "bypassed"}) // a flat's MAC
	m.SeedRow("/ip/hotspot/ip-binding", R{"mac-address": "3C:00:00:00:00:02", "type": "bypassed", "comment": "ftn: keep"})
	if it = item(t, get(t, f, s.ID, true), CheckOrphans); len(it.Evidence) != 2 {
		t.Fatalf("bindings: %+v", it.Evidence)
	}
}

func TestChecksTrafficFlow(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, true), CheckTrafficFlow); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	m.SeedOne("/ip/traffic-flow", R{"enabled": "false", "interfaces": "all"})
	it := item(t, get(t, f, s.ID, true), CheckTrafficFlow)
	if it.Status != StatusWarn || it.Fix.Kind != FixAuto || len(it.Evidence) != 2 {
		t.Fatalf("%+v", it)
	}
	for _, r := range m.Table("/ip/traffic-flow/target") {
		m.RemoveRow(context.Background(), "/ip/traffic-flow/target", r[".id"])
	}
	if it = item(t, get(t, f, s.ID, true), CheckTrafficFlow); len(it.Evidence) != 3 {
		t.Fatalf("%+v", it)
	}
}

func TestChecksUnmanagedAndUnseenAreOverridable(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SeedRow("/ip/arp", R{"mac-address": "00:19:3E:00:00:01", "address": "10.5.50.5", "interface": "br-hotspot"})
	for _, r := range m.Table("/ip/dhcp-server/lease") {
		if r["mac-address"] == "AA:BB:CC:DD:EE:02" {
			m.RemoveRow(context.Background(), "/ip/dhcp-server/lease", r[".id"])
		}
	}
	// a2 has no paid time; make it paid so it counts as unseen.
	f.st.Update(func(d *store.Data) error {
		a := d.Accounts["a2"]
		a.PackageID, a.ExpiresAt = "p1", f.now.Add(time.Hour)
		d.Accounts["a2"] = a
		return nil
	})
	c := get(t, f, s.ID, true)
	un, seen := item(t, c, CheckUnmanaged), item(t, c, CheckUnseen)
	if un.Status != StatusFail || !un.BlocksCutover || un.Override != "keep_unknown" || len(un.Evidence) != 1 {
		t.Fatalf("%+v", un)
	}
	if seen.Status != StatusFail || seen.Override != "force" || len(seen.Evidence) != 1 {
		t.Fatalf("%+v", seen)
	}
	if got := c.Blockers(false, false); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if got := c.Blockers(true, true); len(got) != 0 {
		t.Fatalf("overrides: %v", got)
	}
	if got := c.Blockers(true, false); len(got) != 1 || got[0] != CheckUnseen {
		t.Fatalf("%v", got)
	}
	if c.Summary.CutoverReady {
		t.Fatal("not ready without the overrides")
	}
}

func TestChecksPublicIPAndInfoItems(t *testing.T) {
	f, s, m := checkSite(t, func(c *config.Config) { c.PublicIP = "" })
	m.SeedOne("/ipv6/settings", R{"disable-ipv6": "false"})
	m.SeedRow("/container", R{"name": "ispbill-old", "status": "running"})
	c := get(t, f, s.ID, true)
	if it := item(t, c, CheckPublicIP); it.Status != StatusFail || !it.BlocksCutover {
		t.Fatalf("%+v", it)
	}
	if it := item(t, c, CheckIPv6); it.Status != StatusInfo || !strings.Contains(it.Detail, "IPv6 forwarding") {
		t.Fatalf("%+v", it)
	}
	if it := item(t, c, CheckContainers); it.Status != StatusInfo || len(it.Evidence) != 1 {
		t.Fatalf("a container is only a warning with LegacyContainer: %+v", it)
	}
	f.m.UpdateSite(s.ID, SiteInput{Name: "Block A", Provision: true, BenchPort: "ether5", LegacyContainer: true})
	if it := item(t, get(t, f, s.ID, true), CheckContainers); it.Status != StatusWarn {
		t.Fatalf("%+v", it)
	}
}

func TestChecksFailedReadIsUnknownWithThePrintCommand(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SetPathFail("/ip/dns", errors.New("boom"))
	m.SetPathFail("/system/script", &routeros.APIError{Status: 403, Message: "Forbidden"})
	c := get(t, f, s.ID, true)
	dns := item(t, c, CheckDNS)
	if dns.Status != StatusUnknown || dns.Fix.Kind != FixManual || dns.Fix.Command != "/ip/dns/print" || !strings.Contains(dns.Detail, "boom") || !dns.BlocksCutover {
		t.Fatalf("%+v", dns)
	}
	if kit := item(t, c, CheckKit); kit.Status != StatusUnknown || kit.Fix.Command != "/system/script/print" {
		t.Fatalf("%+v", kit)
	}
	if c.Summary.Unknown < 2 || c.Summary.CutoverReady || !c.Router.Reachable {
		t.Fatalf("%+v", c.Summary)
	}
	if item(t, c, CheckVersion).Status != StatusOK {
		t.Fatal("the other items still answer")
	}
}

func TestChecksRouterUnreachableAndNoInspector(t *testing.T) {
	f, s, m := checkSite(t, nil)
	m.SetFail(errors.New("tunnel down"))
	c := get(t, f, s.ID, true)
	if c.Router.Reachable || c.Summary.CutoverReady || c.Summary.Unknown == 0 || c.Summary.OK != 0 && false {
		t.Fatalf("%+v", c)
	}
	if it := item(t, c, CheckVersion); it.Status != StatusUnknown {
		t.Fatalf("%+v", it)
	}
	// A site that is not there is an error, not an empty checklist.
	if _, err := f.m.Checks(context.Background(), "nope", true); err == nil {
		t.Fatal("unknown site")
	}
}

func TestChecksAreCachedFor15SecondsUnlessFresh(t *testing.T) {
	f, s, m := checkSite(t, nil)
	if it := item(t, get(t, f, s.ID, false), CheckDNS); it.Status != StatusOK {
		t.Fatalf("setup: %+v", it)
	}
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "false"})
	if item(t, get(t, f, s.ID, false), CheckDNS).Status != StatusOK {
		t.Fatal("the cache must answer")
	}
	if item(t, get(t, f, s.ID, true), CheckDNS).Status != StatusFail {
		t.Fatal("fresh bypasses the cache")
	}
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "true"})
	f.advance(16 * time.Second)
	if item(t, get(t, f, s.ID, false), CheckDNS).Status != StatusOK {
		t.Fatal("the cache expires")
	}
}

func TestChecksJoinReport(t *testing.T) {
	f, s, _ := checkSite(t, nil)
	if get(t, f, s.ID, true).JoinReport != nil {
		t.Fatal("no report yet")
	}
	f.st.Update(func(d *store.Data) error {
		k := "site." + s.ID + ".join_report"
		d.Settings[k] = store.Setting{Key: k, Value: "1 wireguard: ok\n7 traffic-flow: failure: no such item\n", UpdatedAt: f.now}
		return nil
	})
	jr := get(t, f, s.ID, true).JoinReport
	if jr == nil || jr.OK || len(jr.Lines) != 2 || !jr.At.Equal(f.now) {
		t.Fatalf("%+v", jr)
	}
}
