package sites

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

var (
	ownerWho = PortActor{Name: "brian", Role: store.RoleOwner}
	operWho  = PortActor{Name: "wanjiru", Role: store.RoleOperator}
)

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

// portsRig is cpRig with ether3 as a customer port and the rows of the Ports tab.
func portsRig(t *testing.T) (*fixture, store.Site, *routeros.Mock) {
	f, s, m := cpRig(t, "ether3", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.Name = "Block A"
		d.Sites[s.ID] = x
		return nil
	})
	m.SeedRow("/interface", R{"name": "wlan1", "type": "wlan"})
	m.SeedRow("/interface", R{"name": "br-lan", "type": "bridge"})
	m.SeedRow("/interface/ethernet", R{"name": "ether2", "rate": "1Gbps", "full-duplex": "true", "running": "true"})
	setField(t, m, "/interface", "ether2", "running", "true")
	m.ResetWrites()
	return f, f.site(s.ID), m
}

func ifaceRow(m *routeros.Mock, name string) routeros.Row {
	for _, r := range m.Table("/interface") {
		if r["name"] == name {
			return r
		}
	}
	return nil
}

func portByName(t *testing.T, v PortsView, name string) PortInfo {
	t.Helper()
	for _, p := range v.Items {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no port %s in %+v", name, v.Items)
	return PortInfo{}
}

func TestPortsListsEveryPhysicalPortWithItsRole(t *testing.T) {
	f, s, _ := portsRig(t)
	v, err := f.m.Ports(context.Background(), s.ID, store.RoleOperator)
	if err != nil || !v.Reachable {
		t.Fatalf("%v %+v", err, v)
	}
	want := map[string]string{"ether1": PortRoleWAN, "ether2": PortRoleLAN, "ether3": PortRoleCustomer, "ether4": PortRoleUnused, "ether5": PortRoleUnused,
		"sfp-sfpplus1": PortRoleOLT, "wlan1": PortRoleUnused}
	if len(v.Items) != len(want) {
		t.Fatalf("%d ports, want %d: %+v", len(v.Items), len(want), v.Items)
	}
	for n, r := range want {
		if got := portByName(t, v, n); got.Role != r || got.RoleLabel != PortRoleLabels[r] {
			t.Errorf("%s: %+v", n, got)
		}
	}
	wan := portByName(t, v, "ether1")
	if !wan.Locked || wan.CanChange || len(wan.RoleOptions) != 0 {
		t.Fatalf("the WAN row must be locked with no actions: %+v", wan)
	}
	if p := portByName(t, v, "ether2"); p.Link != "1Gbps full duplex" || p.CanChange {
		t.Fatalf("the operator may not change the admin LAN: %+v", p)
	}
	if p := portByName(t, v, "ether3"); !p.CanChange || strings.Join(p.RoleOptions, ",") != "customer,unused" && strings.Join(p.RoleOptions, ",") != "unused" {
		t.Fatalf("operator options on a customer port: %+v", p)
	}
	if p := portByName(t, v, "wlan1"); p.Kind != "wlan" {
		t.Fatalf("%+v", p)
	}
	ov, _ := f.m.Ports(context.Background(), s.ID, store.RoleOwner)
	if p := portByName(t, ov, "ether2"); !p.CanChange || len(p.RoleOptions) != 0 {
		t.Fatalf("the only admin LAN port cannot leave: %+v", p) // never the last one
	}
	if p := portByName(t, ov, "ether4"); strings.Join(p.RoleOptions, ",") != "lan,olt,customer" {
		t.Fatalf("owner options on an unused port: %+v", p)
	}
	if p := portByName(t, ov, "sfp-sfpplus1"); len(p.RoleOptions) != 0 || p.RoleWarning != WarnOLT || p.ConfirmDisable || p.CanDisable {
		t.Fatalf("%+v", p)
	}
}

func TestPortsUseFewReadsAndRates(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	if _, err := f.m.Ports(ctx, s.ID, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	q := m.LastQuery("/interface")
	if q == nil || !strings.Contains(q[".proplist"], "rx-byte") || strings.Contains(q[".proplist"], "ipv6") {
		t.Fatalf("the interface read must ask for a short proplist: %v", q)
	}
	setField(t, m, "/interface", "ether2", "rx-byte", "1000")
	f.advance(10 * time.Second)
	if _, err := f.m.Ports(ctx, s.ID, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	setField(t, m, "/interface", "ether2", "rx-byte", "126000")
	f.advance(10 * time.Second)
	v, _ := f.m.Ports(ctx, s.ID, store.RoleViewer)
	if p := portByName(t, v, "ether2"); p.RxBps == nil || *p.RxBps != 100000 {
		t.Fatalf("rx rate: %+v", p.RxBps)
	}
}

func TestPortsCountFlatsSeenOnCustomerPorts(t *testing.T) {
	f, s, m := portsRig(t)
	f.st.Update(func(d *store.Data) error {
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: s.ID, Code: "AAA111", Flat: "1", MAC: "aa:bb:cc:00:00:01"}
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: s.ID, Code: "BBB222", Flat: "2", MAC: "aa:bb:cc:00:00:02"}
		return nil
	})
	m.SeedRow("/interface/bridge/host", R{"mac-address": "AA:BB:CC:00:00:01", "on-interface": "ether3", "bridge": "br-customers"})
	m.SeedRow("/interface/bridge/host", R{"mac-address": "AA:BB:CC:00:00:02", "on-interface": "sfp-sfpplus1", "bridge": "br-customers"})
	v, _ := f.m.Ports(context.Background(), s.ID, store.RoleOperator)
	p := portByName(t, v, "ether3")
	if p.Flats == nil || *p.Flats != 1 || !strings.Contains(p.DisableWarning, "1 house") {
		t.Fatalf("%+v", p)
	}
}

func TestChangePortRefusesTheWAN(t *testing.T) {
	f, s, m := portsRig(t)
	for _, ch := range []PortChange{{Disabled: bp(true)}, {Role: sp("customer")}, {Role: sp("unused")}} {
		err := portErrOf(t, f, s.ID, "ether1", ch, ownerWho)
		if err.Kind != PortErrLocked {
			t.Errorf("%+v: %v", ch, err)
		}
	}
	// and nobody can make another port the WAN
	if err := portErrOf(t, f, s.ID, "ether4", PortChange{Role: sp("wan")}, ownerWho); err.Kind != PortErrLocked {
		t.Fatal(err)
	}
	if m.Writes() != 0 || ifaceRow(m, "ether1")["disabled"] == "true" {
		t.Fatal("the WAN was written")
	}
}

func portErrOf(t *testing.T, f *fixture, id, port string, ch PortChange, who PortActor) *PortRefusal {
	t.Helper()
	_, err := f.m.ChangePort(context.Background(), id, port, ch, who)
	var pe *PortRefusal
	if !errors.As(err, &pe) {
		t.Fatalf("want a port refusal, got %v", err)
	}
	return pe
}

func TestChangePortRefusesTheLastAdminLAN(t *testing.T) {
	f, s, _ := portsRig(t)
	for _, ch := range []PortChange{{Role: sp("customer"), ConfirmName: "Block A"}, {Role: sp("unused"), ConfirmName: "Block A"}} {
		if err := portErrOf(t, f, s.ID, "ether2", ch, ownerWho); err.Kind != PortErrLastLAN {
			t.Errorf("%+v: %v", ch, err)
		}
	}
	if got := f.site(s.ID).LANPorts; got != "ether2" && got != "" {
		t.Fatalf("lan ports changed: %q", got)
	}
}

func TestChangePortNeedsTheTypedRouterNameForCoreChanges(t *testing.T) {
	f, s, m := portsRig(t)
	// a second admin LAN port, then leaving the first one
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether4", PortChange{Role: sp("lan")}, ownerWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	for name, tc := range map[string]struct {
		port string
		ch   PortChange
		warn string
	}{
		"leave the admin LAN":   {"ether2", PortChange{Role: sp("unused")}, WarnLAN},
		"disable an admin LAN":  {"ether2", PortChange{Disabled: bp(true)}, WarnLAN},
		"move the OLT uplink":   {"ether5", PortChange{Role: sp("olt")}, WarnOLT},
		"wrong name is refused": {"ether2", PortChange{Role: sp("unused"), ConfirmName: "Block B"}, WarnLAN},
	} {
		m.ResetWrites()
		err := portErrOf(t, f, s.ID, tc.port, tc.ch, ownerWho)
		if err.Kind != PortErrConfirm || err.Warning != tc.warn || !strings.Contains(err.Msg, "type the router name") {
			t.Errorf("%s: %+v", name, err)
		}
		if m.Writes() != 0 {
			t.Errorf("%s: wrote before the confirmation", name)
		}
	}
	// with the name it goes through
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether2", PortChange{Role: sp("unused"), ConfirmName: " Block A "}, ownerWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	if got := f.site(s.ID).LANPorts; got != "ether4" {
		t.Fatalf("lan ports %q", got)
	}
}

func TestChangePortOperatorVersusOwner(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	// an operator may handle customer and unused ports
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("customer")}, operWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(true)}, operWho); err != nil {
		t.Fatal(err)
	}
	if ifaceRow(m, "ether4")["disabled"] != "yes" {
		t.Fatalf("not switched off: %v", ifaceRow(m, "ether4"))
	}
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(false)}, operWho); err != nil {
		t.Fatal(err)
	}
	// but not the admin LAN, the OLT uplink, or taking a port into either
	for name, tc := range map[string]struct {
		port string
		ch   PortChange
	}{
		"disable LAN":     {"ether2", PortChange{Disabled: bp(true), ConfirmName: "Block A"}},
		"enable LAN":      {"ether2", PortChange{Disabled: bp(false)}},
		"disable OLT":     {"sfp-sfpplus1", PortChange{Disabled: bp(true), ConfirmName: "Block A"}},
		"unused to LAN":   {"ether5", PortChange{Role: sp("lan")}},
		"unused to OLT":   {"ether5", PortChange{Role: sp("olt"), ConfirmName: "Block A"}},
		"customer to LAN": {"ether3", PortChange{Role: sp("lan")}},
	} {
		if err := portErrOf(t, f, s.ID, tc.port, tc.ch, operWho); err.Kind != PortErrForbidden {
			t.Errorf("%s: %+v", name, err)
		}
	}
	// a viewer may do nothing
	if err := portErrOf(t, f, s.ID, "ether5", PortChange{Disabled: bp(true)}, PortActor{Name: "v", Role: store.RoleViewer}); err.Kind != PortErrForbidden {
		t.Fatal(err)
	}
	if got := f.site(s.ID); got.LANPorts != "ether2" || got.OLTInterface != "sfp-sfpplus1" {
		t.Fatalf("the core was changed: %+v", got)
	}
}

func TestChangePortOneRolePerPortAndOLTMoves(t *testing.T) {
	f, s, _ := portsRig(t)
	ctx := context.Background()
	// a customer port made an admin LAN port leaves the customer list
	if _, err := f.m.ChangePort(ctx, s.ID, "ether3", PortChange{Role: sp("lan")}, ownerWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	got := f.site(s.ID)
	if got.CustomerPorts != "" || got.LANPorts != "ether2,ether3" {
		t.Fatalf("%+v", got)
	}
	// the OLT uplink moves to a free port; the old one becomes unused
	if _, err := f.m.ChangePort(ctx, s.ID, "ether5", PortChange{Role: sp("olt"), ConfirmName: "Block A"}, ownerWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	got = f.site(s.ID)
	if got.OLTInterface != "ether5" || roleOf(f.m.Effective(got), "sfp-sfpplus1") != PortRoleUnused {
		t.Fatalf("%+v", got)
	}
	// the OLT port itself cannot just become something else
	if err := portErrOf(t, f, s.ID, "ether5", PortChange{Role: sp("customer")}, ownerWho); err.Kind != PortErrConflict {
		t.Fatal(err)
	}
	// a switched off port cannot take a core role
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(true)}, ownerWho); err != nil {
		t.Fatal(err)
	}
	if err := portErrOf(t, f, s.ID, "ether4", PortChange{Role: sp("lan")}, ownerWho); err.Kind != PortErrConflict {
		t.Fatal(err)
	}
	// no such port
	if err := portErrOf(t, f, s.ID, "ether9", PortChange{Role: sp("customer")}, ownerWho); err.Kind != PortErrNoPort {
		t.Fatal(err)
	}
	// wireless: unused only
	if err := portErrOf(t, f, s.ID, "wlan1", PortChange{Role: sp("customer")}, ownerWho); err.Kind != PortErrInvalid {
		t.Fatal(err)
	}
}

func TestChangePortRoleIsAppliedAndRecordedWithTheActor(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("lan")}, ownerWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	r := portRow(m, "ether4")
	if r == nil || r["bridge"] != "br-lan" || r["comment"] != LANPortComment {
		t.Fatalf("not applied: %v", m.Table("/interface/bridge/port"))
	}
	if _, err := f.m.ChangePort(ctx, s.ID, "ether3", PortChange{Role: sp("unused")}, operWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	if portRow(m, "ether3") != nil {
		t.Fatalf("the server's membership of ether3 stayed: %v", m.Table("/interface/bridge/port"))
	}
	var evs []string
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == s.ID && e.Kind == "audit" && strings.HasPrefix(e.Msg, "port ") {
				evs = append(evs, e.Actor+": "+e.Msg)
			}
		}
	})
	joined := strings.Join(evs, "\n")
	if !strings.Contains(joined, "brian: port ether4: role Unused -> Admin LAN") || !strings.Contains(joined, "wanjiru: port ether3: role Customer port -> Unused") {
		t.Fatalf("activity:\n%s", joined)
	}
}

func TestPortPassMovesAdminLANMembersInBeforeOldOnesOut(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	// ether2 leaves and ether4 joins in one save: ether4 must be in br-lan before ether2 goes
	f.st.Update(func(d *store.Data) error { x := d.Sites[s.ID]; x.LANPorts = "ether4"; d.Sites[s.ID] = x; return nil })
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if r := portRow(m, "ether4"); r == nil || r["bridge"] != "br-lan" {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
	if portRow(m, "ether2") != nil {
		t.Fatalf("the old admin LAN port stayed: %v", m.Table("/interface/bridge/port"))
	}
	// when the new member cannot go in (another bridge) the old one is not removed
	m.RemoveRow(ctx, "/interface/bridge/port", portRow(m, "ether4")[".id"])
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-lan", "interface": "ether2", "comment": LANPortComment})
	m.SeedRow("/interface/bridge/port", R{"bridge": "bridge-x", "interface": "ether4"})
	m.ResetWrites()
	st := f.m.EnsureCustomerPort(ctx, s.ID, true)
	if st.LANMessage == "" || portRow(m, "ether2") == nil {
		t.Fatalf("a blocked replacement must keep the old member: %+v %v", st, m.Table("/interface/bridge/port"))
	}
}

func TestSweepHealsASwitchedOffPort(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(true)}, operWho); err != nil {
		t.Fatal(err)
	}
	if got := f.site(s.ID).DisabledPorts; got != "ether4" {
		t.Fatalf("not remembered: %q", got)
	}
	// somebody enables it on the router by hand
	setField(t, m, "/interface", "ether4", "disabled", "false")
	f.advance(10 * time.Minute)
	f.m.SweepSite(ctx, s.ID)
	if ifaceRow(m, "ether4")["disabled"] != "yes" {
		t.Fatalf("the sweep did not switch it off again: %v", ifaceRow(m, "ether4"))
	}
	if !hasEvent(f, s.ID, "switched off ether4") {
		t.Fatal("no activity line from the system")
	}
	// enabling forgets it for good
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(false)}, operWho); err != nil {
		t.Fatal(err)
	}
	f.advance(10 * time.Minute)
	f.m.SweepSite(ctx, s.ID)
	if ifaceRow(m, "ether4")["disabled"] == "yes" || f.site(s.ID).DisabledPorts != "" {
		t.Fatalf("%v %q", ifaceRow(m, "ether4"), f.site(s.ID).DisabledPorts)
	}
}

func TestSweepSelfHealsTheLANMembership(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	m.RemoveRow(ctx, "/interface/bridge/port", portRow(m, "ether2")[".id"])
	f.advance(10 * time.Minute)
	f.m.SweepSite(ctx, s.ID)
	if r := portRow(m, "ether2"); r == nil || r["bridge"] != "br-lan" {
		t.Fatalf("not healed: %v", m.Table("/interface/bridge/port"))
	}
}

func TestFixWriterPortAllowlist(t *testing.T) {
	f, s, m := portsRig(t)
	eff := f.m.Effective(s)
	w := f.m.newFixWriter(m, eff)
	ctx := context.Background()
	rows := map[string]routeros.Row{}
	for _, r := range m.Table("/interface") {
		rows[r["name"]] = r
	}
	// switching: only a port the pass named, never the WAN, bridges or wireguard
	w.offPorts["ether1"], w.offPorts["br-lan"] = true, true
	for _, n := range []string{"ether1", "br-lan", "ether4"} {
		if err := w.setRow(ctx, "/interface", rows[n], map[string]string{"disabled": "yes"}); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("%s: %v", n, err)
		}
	}
	w.offPorts["ether4"] = true
	if err := w.setRow(ctx, "/interface", rows["ether4"], map[string]string{"comment": "x"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("another property: %v", err)
	}
	if err := w.setRow(ctx, "/interface", rows["ether4"], map[string]string{"disabled": "yes"}); err != nil {
		t.Errorf("a plain port: %v", err)
	}
	if err := w.addRow(ctx, "/interface", map[string]string{"name": "x"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("add /interface: %v", err)
	}
	// br-lan rows: only the server's own comment, only wanted ports, a removal only when it is safe
	w.lanPorts["ether4"] = true
	if err := w.addRow(ctx, "/interface/bridge/port", map[string]string{"bridge": "br-lan", "interface": "ether5", "comment": LANPortComment}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("an unwanted LAN port: %v", err)
	}
	if err := w.addRow(ctx, "/interface/bridge/port", map[string]string{"bridge": "br-lan", "interface": "ether4", "comment": "mine"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("another comment: %v", err)
	}
	if err := w.addRow(ctx, "/interface/bridge/port", map[string]string{"bridge": "br-lan", "interface": "ether4", "comment": LANPortComment}); err != nil {
		t.Errorf("a wanted LAN port: %v", err)
	}
	w.staleLAN["ether2"] = true
	row := portRow(m, "ether2")
	if err := w.removeRow(ctx, "/interface/bridge/port", row); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("removing an admin LAN member while no other is confirmed: %v", err)
	}
	w.lanSafe = true
	if err := w.removeRow(ctx, "/interface/bridge/port", row); err != nil {
		t.Errorf("a safe removal: %v", err)
	}
	if err := w.removeRow(ctx, "/interface/bridge/port", routeros.Row{"bridge": "br-lan", "interface": "ether9", "comment": LANPortComment}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("an unlisted port: %v", err)
	}
}

// setField changes one property of the row of a mock table that has the given name.
func setField(t *testing.T, m *routeros.Mock, path, name, key, val string) {
	t.Helper()
	for _, r := range m.Table(path) {
		if r["name"] == name {
			if err := m.SetRow(context.Background(), path, r[".id"], map[string]string{key: val}); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no row %s in %s", name, path)
}

func TestWebFigCheckIsOKForTheAdminLANAndTheServer(t *testing.T) {
	f, s, m := portsRig(t)
	for _, r := range m.Table("/ip/service") {
		m.RemoveRow(context.Background(), "/ip/service", r[".id"])
	}
	srv := ServerTunnelIP(f.m.pfx)
	m.SeedRow("/ip/service", R{"name": "www", "port": "8081", "address": "192.168.88.0/24," + srv + "/32", "disabled": "false"})
	f.m.chk.drop(s.ID)
	if it := checkItem(t, f, s.ID, CheckWWW); it.Status != StatusOK {
		t.Fatalf("%+v", it)
	}
	// the old server-only address is still shown, as information
	for _, r := range m.Table("/ip/service") {
		m.RemoveRow(context.Background(), "/ip/service", r[".id"])
	}
	m.SeedRow("/ip/service", R{"name": "www", "port": "8081", "address": srv + "/32", "disabled": "false"})
	f.m.chk.drop(s.ID)
	if it := checkItem(t, f, s.ID, CheckWWW); it.Status != StatusInfo || !strings.Contains(it.Detail, "new line") {
		t.Fatalf("%+v", it)
	}
}

// twoLANs makes ether2 and ether4 admin LAN ports, both members of br-lan, both with a link.
func twoLANs(t *testing.T) (*fixture, store.Site, *routeros.Mock) {
	f, s, m := portsRig(t)
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether4", PortChange{Role: sp("lan")}, ownerWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	if r := portRow(m, "ether4"); r == nil || r["bridge"] != "br-lan" {
		t.Fatalf("second LAN not applied: %v", m.Table("/interface/bridge/port"))
	}
	m.ResetWrites()
	return f, f.site(s.ID), m
}

func TestDisablingTheOnlyConnectedAdminLANIsRefused(t *testing.T) {
	f, s, m := twoLANs(t)
	setField(t, m, "/interface", "ether4", "running", "false") // unplugged
	m.ResetWrites()
	for _, ch := range []PortChange{{Disabled: bp(true), ConfirmName: "Block A"}, {Role: sp("unused"), ConfirmName: "Block A"}} {
		if pe := portErrOf(t, f, s.ID, "ether2", ch, ownerWho); pe.Kind != PortErrLastLAN {
			t.Errorf("%+v: %+v", ch, pe)
		}
	}
	// the second port switched off is no safety net either
	setField(t, m, "/interface", "ether4", "running", "true")
	setField(t, m, "/interface", "ether4", "disabled", "true")
	m.ResetWrites()
	if pe := portErrOf(t, f, s.ID, "ether2", PortChange{Disabled: bp(true), ConfirmName: "Block A"}, ownerWho); pe.Kind != PortErrLastLAN {
		t.Errorf("disabled neighbour: %+v", pe)
	}
	if m.Writes() != 0 || ifaceRow(m, "ether2")["disabled"] == "yes" || f.site(s.ID).LANPorts != "ether2,ether4" {
		t.Fatalf("something was written: %d %v %q", m.Writes(), ifaceRow(m, "ether2"), f.site(s.ID).LANPorts)
	}
	// the neighbour back with a link: the change goes through, as a one-off (not kept off by the sweep)
	setField(t, m, "/interface", "ether4", "disabled", "false")
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether2", PortChange{Disabled: bp(true), ConfirmName: "Block A"}, ownerWho); err != nil {
		t.Fatal(err)
	}
	if ifaceRow(m, "ether2")["disabled"] != "yes" || f.site(s.ID).DisabledPorts != "" {
		t.Fatalf("%v %q", ifaceRow(m, "ether2"), f.site(s.ID).DisabledPorts)
	}
}

func TestMovingAnAdminLANNeedsAnotherConnectedMember(t *testing.T) {
	f, s, m := twoLANs(t)
	setField(t, m, "/interface", "ether4", "running", "false")
	if pe := portErrOf(t, f, s.ID, "ether2", PortChange{Role: sp("customer"), ConfirmName: "Block A"}, ownerWho); pe.Kind != PortErrLastLAN {
		t.Fatalf("%+v", pe)
	}
	// a LAN port that is not a member of br-lan yet does not count either
	setField(t, m, "/interface", "ether4", "running", "true")
	m.RemoveRow(context.Background(), "/interface/bridge/port", portRow(m, "ether4")[".id"])
	if pe := portErrOf(t, f, s.ID, "ether2", PortChange{Role: sp("unused"), ConfirmName: "Block A"}, ownerWho); pe.Kind != PortErrLastLAN {
		t.Fatalf("not a member: %+v", pe)
	}
	if f.site(s.ID).LANPorts != "ether2,ether4" {
		t.Fatalf("%q", f.site(s.ID).LANPorts)
	}
}

func TestRouterFormLANChangeNeedsTheNameAndAConnectedPort(t *testing.T) {
	f, s, m := portsRig(t)
	setField(t, m, "/interface", "ether5", "running", "false") // a port with no cable
	// no typed name
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.LANPorts = "ether5" }); !errors.Is(err, ErrLANConfirmRequired) {
		t.Fatalf("no name: %v", err)
	}
	// the name, but the new port has no link
	err := f.saveInput(s.ID, func(in *SiteInput) { in.LANPorts, in.ConfirmName = "ether5", "Block A" })
	var pe *PortRefusal
	if !errors.As(err, &pe) || pe.Kind != PortErrLastLAN {
		t.Fatalf("cable-less port: %v", err)
	}
	if f.site(s.ID).LANPorts != "ether2" && f.site(s.ID).LANPorts != "" {
		t.Fatalf("saved anyway: %q", f.site(s.ID).LANPorts)
	}
	// with a link it saves; the old member leaves only after the new one is in
	setField(t, m, "/interface", "ether5", "running", "true")
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.LANPorts, in.ConfirmName = "ether5", "Block A" }); err != nil {
		t.Fatal(err)
	}
	f.m.EnsureCustomerPort(context.Background(), s.ID, true)
	if r := portRow(m, "ether5"); r == nil || r["bridge"] != "br-lan" || portRow(m, "ether2") != nil {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
}

func TestPassKeepsTheOldLANMemberUntilTheNewOneHasALink(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	setField(t, m, "/interface", "ether5", "running", "false")
	f.st.Update(func(d *store.Data) error { x := d.Sites[s.ID]; x.LANPorts = "ether5"; d.Sites[s.ID] = x; return nil })
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if r := portRow(m, "ether2"); r == nil || r["bridge"] != "br-lan" {
		t.Fatalf("the old member was removed with no link on the new one: %v", m.Table("/interface/bridge/port"))
	}
	setField(t, m, "/interface", "ether5", "running", "true")
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if portRow(m, "ether2") != nil || portRow(m, "ether5") == nil {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
}

func TestConcurrentLANChangesCannotRemoveBothPorts(t *testing.T) {
	f, s, m := twoLANs(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, port := range []string{"ether2", "ether4"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.m.ChangePort(ctx, s.ID, port, PortChange{Disabled: bp(true), ConfirmName: "Block A"}, ownerWho)
		}()
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		var pe *PortRefusal
		switch {
		case e == nil:
			ok++
		case errors.As(e, &pe) && pe.Kind == PortErrLastLAN:
		default:
			t.Fatalf("%v", e)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of two concurrent disables went through: %v", ok, errs)
	}
	if ifaceRow(m, "ether2")["disabled"] == "yes" && ifaceRow(m, "ether4")["disabled"] == "yes" {
		t.Fatal("both admin LAN ports were switched off")
	}
}

func TestTheOLTUplinkIsNeverSwitchedOffFromHere(t *testing.T) {
	f, s, m := portsRig(t)
	for _, who := range []PortActor{ownerWho} {
		if pe := portErrOf(t, f, s.ID, "sfp-sfpplus1", PortChange{Disabled: bp(true), ConfirmName: "Block A"}, who); pe.Kind != PortErrOLTOff ||
			pe.Msg != "OLT port: can't be switched off here. Give the OLT role to another port first." {
			t.Fatalf("%+v", pe)
		}
	}
	if m.Writes() != 0 || ifaceRow(m, "sfp-sfpplus1")["disabled"] == "yes" {
		t.Fatal("written")
	}
	v, _ := f.m.Ports(context.Background(), s.ID, store.RoleOwner)
	if p := portByName(t, v, "sfp-sfpplus1"); p.CanDisable {
		t.Fatalf("the list offers it: %+v", p)
	}
}

func TestSweepDoesNotSwitchOffAnAdminLANOrOLTPortAgain(t *testing.T) {
	f, s, m := twoLANs(t)
	ctx := context.Background()
	// a stored "off" for an admin LAN port and for the OLT (an old record) is not enforced
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.DisabledPorts = "ether4,sfp-sfpplus1,ether5"
		x.EnabledPorts = "ether3,ether4,sfp-sfpplus1" // the owner never enabled ether5
		d.Sites[s.ID] = x
		return nil
	})
	f.advance(10 * time.Minute)
	f.m.SweepSite(ctx, s.ID)
	if ifaceRow(m, "ether4")["disabled"] == "yes" || ifaceRow(m, "sfp-sfpplus1")["disabled"] == "yes" {
		t.Fatalf("a core port was switched off by the sweep")
	}
	if ifaceRow(m, "ether5")["disabled"] != "yes" {
		t.Fatalf("an unused port stays off: %v", ifaceRow(m, "ether5"))
	}
	// somebody enables a LAN port again at the router after a one-off disable: it stays on
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(true), ConfirmName: "Block A"}, ownerWho); err != nil {
		t.Fatal(err)
	}
	setField(t, m, "/interface", "ether4", "disabled", "false")
	f.advance(10 * time.Minute)
	f.m.SweepSite(ctx, s.ID)
	if ifaceRow(m, "ether4")["disabled"] == "yes" {
		t.Fatal("the sweep undid the re-enable")
	}
}

func TestMovingAPortToLANOrOLTForgetsItsStoredOff(t *testing.T) {
	f, s, _ := portsRig(t)
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether4", PortChange{Disabled: bp(true)}, operWho); err != nil {
		t.Fatal(err)
	}
	if f.site(s.ID).DisabledPorts != "ether4" {
		t.Fatalf("%q", f.site(s.ID).DisabledPorts)
	}
	if _, err := f.m.ChangePort(context.Background(), s.ID, "ether4", PortChange{Disabled: bp(false)}, operWho); err != nil {
		t.Fatal(err)
	}
	if f.site(s.ID).DisabledPorts != "" {
		t.Fatalf("%q", f.site(s.ID).DisabledPorts)
	}
}

func TestStaleFormSaveKeepsAPortsTabChange(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	// the form is loaded now ...
	base := SiteInputOf(f.site(s.ID))
	stale := base
	stale.Base = &base
	// ... then the Ports tab adds ether4 as an admin LAN port and makes ether5 a customer port
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("lan")}, ownerWho); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ChangePort(ctx, s.ID, "ether5", PortChange{Role: sp("customer")}, operWho); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	// the stale form (which still says ether2 / ether3) only changes the notes
	stale.Notes = "new notes"
	if err := f.m.UpdateSite(s.ID, stale); err != nil {
		t.Fatal(err)
	}
	f.m.cport.wg.Wait()
	got := f.site(s.ID)
	if got.Notes != "new notes" || got.LANPorts != "ether2,ether4" || got.CustomerPorts != "ether3,ether5" {
		t.Fatalf("%q %q %q", got.Notes, got.LANPorts, got.CustomerPorts)
	}
	if r := portRow(m, "ether4"); r == nil || r["bridge"] != "br-lan" {
		t.Fatalf("the Ports change was undone on the router: %v", m.Table("/interface/bridge/port"))
	}
	// a form that CHANGES a port the Ports tab moved meanwhile is refused as stale
	again := base
	again.Base = &base
	again.LANPorts, again.ConfirmName = "ether5", "Block A"
	if err := f.m.UpdateSite(s.ID, again); !errors.Is(err, ErrStaleLayout) {
		t.Fatalf("%v", err)
	}
}

func TestUpdateSiteForgetsStoredOffForANewLANOrOLTPort(t *testing.T) {
	f, s, _ := portsRig(t)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.DisabledPorts = "ether4,ether5"
		d.Sites[s.ID] = x
		return nil
	})
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.LANPorts, in.ConfirmName = "ether2,ether4", "Block A" }); err != nil {
		t.Fatal(err)
	}
	if got := f.site(s.ID).DisabledPorts; got != "ether5" {
		t.Fatalf("LAN: %q", got)
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether5", true }); err != nil {
		t.Fatal(err)
	}
	if got := f.site(s.ID).DisabledPorts; got != "" {
		t.Fatalf("OLT: %q", got)
	}
}

func TestTypedRouterNameIsExact(t *testing.T) {
	f, s, _ := twoLANs(t)
	for _, name := range []string{"block a", "BLOCK A"} {
		if pe := portErrOf(t, f, s.ID, "ether4", PortChange{Role: sp("unused"), ConfirmName: name}, ownerWho); pe.Kind != PortErrConfirm {
			t.Errorf("%q: %+v", name, pe)
		}
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.LANPorts, in.ConfirmName = "ether2", "block a" }); !errors.Is(err, ErrLANConfirmRequired) {
		t.Fatalf("%v", err)
	}
}
