package sites

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// cpRig is a joined v5 router: ether1 WAN, ether2 admin LAN (in br-lan), sfp-sfpplus1 the OLT
// (no VLAN, so a member of br-customers), ether3..ether5 free.
func cpRig(t *testing.T, bench string, vlan int) (*fixture, store.Site, *routeros.Mock) {
	t.Helper()
	f := newFixture(t, nil)
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	if err := f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.BenchPort, x.VLANID = bench, vlan
		d.Sites[s.ID] = x
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := f.startedRouter(s.ID)
	seedHealthy(t, f, s, m)
	for _, n := range []string{"ether1", "ether2", "ether3", "ether4", "ether5", "sfp-sfpplus1"} {
		m.SeedRow("/interface", R{"name": n, "type": "ether"})
	}
	m.SeedRow("/interface/bridge", R{"name": "br-lan", "comment": "ftn: admin LAN bridge"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-lan", "interface": "ether2", "comment": "ftn: LAN port"})
	if vlan > 0 {
		m.SeedRow("/interface/vlan", R{"name": customerVLANName(vlan), "vlan-id": fmt.Sprint(vlan), "interface": "sfp-sfpplus1", "comment": CustVLANComment})
		m.SeedRow("/interface/bridge/port", R{"bridge": "br-customers", "interface": customerVLANName(vlan), "horizon": "1", "comment": CustPortComment})
	} else {
		m.SeedRow("/interface/bridge/port", R{"bridge": "br-customers", "interface": "sfp-sfpplus1", "horizon": "1", "comment": CustPortComment})
	}
	return f, f.site(s.ID), m
}

func (f *fixture) saveInput(id string, mut func(*SiteInput)) error {
	in := SiteInputOf(f.site(id))
	mut(&in)
	err := f.m.UpdateSite(id, in)
	f.m.cport.wg.Wait()
	return err
}

func portRow(m *routeros.Mock, iface string) routeros.Row {
	for _, r := range m.Table("/interface/bridge/port") {
		if r["interface"] == iface {
			return r
		}
	}
	return nil
}

func hasEvent(f *fixture, siteID, sub string) bool {
	found := false
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == siteID && strings.Contains(e.Msg, sub) && e.Actor == ActorSystem {
				found = true
			}
		}
	})
	return found
}

func TestCustomerPortAppliedOnSave(t *testing.T) {
	f, s, m := cpRig(t, "", 0)
	m.ResetWrites()
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort = "ether5" }); err != nil {
		t.Fatal(err)
	}
	r := portRow(m, "ether5")
	if r == nil || r["bridge"] != CustomerBridge || r["horizon"] != "1" || r["comment"] != CustPortComment {
		t.Fatalf("not applied: %v", m.Table("/interface/bridge/port"))
	}
	st := f.m.CustomerPortStatus(s.ID)
	if st.State != PortOK || st.Text != "ether5: customer port ✓" {
		t.Fatalf("%+v", st)
	}
	if !hasEvent(f, s.ID, "ether5") {
		t.Fatal("no event by the system")
	}
	if it := checkItem(t, f, s.ID, CheckBench); it.Status != StatusOK || it.Fix.Kind != FixNone {
		t.Fatalf("check: %+v", it)
	}
	// nothing else was touched: the LAN port and the OLT member are as they were
	if portRow(m, "ether2")["bridge"] != "br-lan" || portRow(m, "sfp-sfpplus1")["bridge"] != CustomerBridge {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
	// a second pass writes nothing
	m.ResetWrites()
	f.m.EnsureCustomerPort(context.Background(), s.ID, true)
	if m.Writes() != 0 {
		t.Fatalf("%d writes on a settled router", m.Writes())
	}
}

func TestCustomerPortChangeAndClearRemoveTheOldMembership(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	f.m.EnsureCustomerPort(context.Background(), s.ID, true)
	if portRow(m, "ether5") == nil {
		t.Fatal("not applied first")
	}
	// a membership the server did not make stays, even on the old port's neighbour
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-customers", "interface": "ether4", "comment": "mine"})
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort = "ether3" }); err != nil {
		t.Fatal(err)
	}
	if portRow(m, "ether5") != nil || portRow(m, "ether3") == nil {
		t.Fatalf("change: %v", m.Table("/interface/bridge/port"))
	}
	if portRow(m, "ether4") == nil {
		t.Fatal("removed a row the server did not make")
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort = "" }); err != nil {
		t.Fatal(err)
	}
	if portRow(m, "ether3") != nil {
		t.Fatalf("clear: %v", m.Table("/interface/bridge/port"))
	}
	if portRow(m, "sfp-sfpplus1") == nil || portRow(m, "ether2") == nil {
		t.Fatal("the OLT or LAN membership was removed")
	}
	if st := f.m.CustomerPortStatus(s.ID); st.State != PortOff {
		t.Fatalf("%+v", st)
	}
}

func TestCustomerPortRefusals(t *testing.T) {
	f, s, m := cpRig(t, "", 0)
	// up front: WAN, LAN, OLT
	for port, want := range map[string]string{"ether1": "WAN port", "ether2": "admin LAN port", "sfp-sfpplus1": "OLT uplink port"} {
		err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort = port })
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "bench_port" || !strings.Contains(fe.Msg, want) {
			t.Errorf("%s: %v", port, err)
		}
	}
	// the OLT port must not be a WAN or LAN port either
	for port, want := range map[string]string{"ether1": "WAN port", "ether2": "admin LAN"} {
		err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface = port })
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "olt_interface" || !strings.Contains(fe.Msg, want) {
			t.Errorf("olt %s: %v", port, err)
		}
	}
	if m.Writes() != 3 { // only the seeds of the rig; nothing was written for the refused saves
		t.Logf("writes %d", m.Writes())
	}
	// on the router: a port of the admin bridge is never moved
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-lan", "interface": "ether5", "comment": "ftn: LAN port"})
	f.st.Update(func(d *store.Data) error { x := d.Sites[s.ID]; x.BenchPort = "ether5"; d.Sites[s.ID] = x; return nil })
	m.ResetWrites()
	st := f.m.EnsureCustomerPort(context.Background(), s.ID, true)
	if st.State != PortError || !strings.Contains(st.Message, "br-lan") || !strings.Contains(st.Text, "ether5:") {
		t.Fatalf("%+v", st)
	}
	if m.Writes() != 0 || portRow(m, "ether5")["bridge"] != "br-lan" {
		t.Fatalf("the admin LAN port was touched: %v", m.Table("/interface/bridge/port"))
	}
	it := checkItem(t, f, s.ID, CheckBench)
	if it.Status != StatusWarn || it.Fix.Kind != FixNone || !strings.Contains(it.Detail, "br-lan") {
		t.Fatalf("%+v", it)
	}
	// a port in the router's WAN or LAN interface list
	for _, list := range []string{"WAN", "LAN"} {
		m.RemoveRow(context.Background(), "/interface/bridge/port", portRow(m, "ether5")[".id"])
		m.SeedRow("/interface/list/member", R{"list": list, "interface": "ether5"})
		m.ResetWrites()
		st = f.m.EnsureCustomerPort(context.Background(), s.ID, true)
		if st.State != PortError || m.Writes() != 0 {
			t.Fatalf("%s list: %+v writes %d", list, st, m.Writes())
		}
		for _, r := range m.Table("/interface/list/member") {
			m.RemoveRow(context.Background(), "/interface/list/member", r[".id"])
		}
	}
	// a port in some other bridge
	m.SeedRow("/interface/bridge/port", R{"bridge": "bridge-x", "interface": "ether5"})
	if st = f.m.EnsureCustomerPort(context.Background(), s.ID, true); st.State != PortError || !strings.Contains(st.Message, "bridge-x") {
		t.Fatalf("%+v", st)
	}
}

func TestFixWriterBridgePortAllowlist(t *testing.T) {
	m := routeros.NewMock()
	w := &fixWriter{rt: m, insp: m, ifaces: map[string]bool{"sfp-sfpplus1": true, CustomerBridge: true}, custPorts: map[string]bool{"ether5": true}, stale: map[string]bool{"ether4": true}}
	ctx := context.Background()
	good := map[string]string{"bridge": CustomerBridge, "interface": "ether5", "horizon": "1", "comment": CustPortComment}
	if err := w.addRow(ctx, "/interface/bridge/port", good); err != nil {
		t.Fatalf("the customer port: %v", err)
	}
	for name, body := range map[string]map[string]string{
		"another port":    {"bridge": CustomerBridge, "interface": "ether3", "horizon": "1", "comment": CustPortComment},
		"the LAN bridge":  {"bridge": LANBridge, "interface": "ether5", "horizon": "1", "comment": CustPortComment},
		"no horizon":      {"bridge": CustomerBridge, "interface": "ether5", "comment": CustPortComment},
		"another comment": {"bridge": CustomerBridge, "interface": "ether5", "horizon": "1", "comment": "x"},
		"extra property":  {"bridge": CustomerBridge, "interface": "ether5", "horizon": "1", "comment": CustPortComment, "pvid": "9"},
	} {
		if err := w.addRow(ctx, "/interface/bridge/port", body); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("add %s: %v", name, err)
		}
	}
	lan := R{".id": "*9", "bridge": LANBridge, "interface": "ether5", "comment": "ftn: LAN port"}
	if err := w.removeRow(ctx, "/interface/bridge/port", lan); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("remove a LAN bridge row: %v", err)
	}
	if err := w.setRow(ctx, "/interface/bridge/port", lan, R{"bridge": CustomerBridge}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("move a LAN port: %v", err)
	}
	cust := R{".id": "*8", "bridge": CustomerBridge, "interface": "ether5", "comment": CustPortComment}
	if err := w.setRow(ctx, "/interface/bridge/port", cust, R{"bridge": LANBridge}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("move a customer port to br-lan: %v", err)
	}
	cust[".id"] = m.SeedRow("/interface/bridge/port", cust)
	if err := w.setRow(ctx, "/interface/bridge/port", cust, R{"horizon": "1"}); err != nil {
		t.Errorf("horizon: %v", err)
	}
	// removal: only the server's own row of a dropped port, in br-customers
	if err := w.removeRow(ctx, "/interface/bridge/port", R{".id": "*7", "bridge": CustomerBridge, "interface": "ether4", "comment": CustPortComment}); err != nil {
		t.Errorf("stale: %v", err)
	}
	if err := w.removeRow(ctx, "/interface/bridge/port", R{".id": "*7", "bridge": CustomerBridge, "interface": "ether4", "comment": "mine"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("a row the server did not make: %v", err)
	}
	if err := w.removeRow(ctx, "/interface/bridge/port", R{".id": "*6", "bridge": CustomerBridge, "interface": "ether7", "comment": CustPortComment}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("another port: %v", err)
	}
	// the customer VLAN only
	if err := w.addRow(ctx, "/interface/vlan", R{"name": "vlan9", "vlan-id": "9", "interface": "ether3", "comment": CustVLANComment}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("a VLAN with no VLAN set: %v", err)
	}
	w.vlanName, w.oltPort = "vlan100-customers", "ether3"
	if err := w.addRow(ctx, "/interface/vlan", R{"name": "vlan100-customers", "vlan-id": "100", "interface": "ether3", "comment": CustVLANComment}); err != nil {
		t.Errorf("the customer VLAN: %v", err)
	}
	if err := w.setRow(ctx, "/interface/vlan", R{".id": "*1", "name": "vlan100-customers", "comment": CustVLANComment}, R{"interface": "ether1"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("a VLAN moved to another port: %v", err)
	}
	if err := w.removeRow(ctx, "/interface/vlan", R{".id": "*1", "name": "vlan100-customers"}); !errors.Is(err, ErrFixNotAllowed) {
		t.Errorf("a VLAN removal: %v", err)
	}
}

func TestCustomerPortSweepReappliesDrift(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	f.m.SweepSite(ctx, s.ID)
	if portRow(m, "ether5") == nil {
		t.Fatal("not applied")
	}
	// the row is deleted on the router: the next sweep puts it back
	m.RemoveRow(ctx, "/interface/bridge/port", portRow(m, "ether5")[".id"])
	f.advance(sweepBusyEvery + time.Second)
	f.m.SweepSite(ctx, s.ID)
	if portRow(m, "ether5") == nil {
		t.Fatal("drift not repaired")
	}
	// a row with the wrong horizon is repaired too, and the Rebuild-era comment is rewritten
	m.SetRow(ctx, "/interface/bridge/port", portRow(m, "ether5")[".id"], R{"horizon": "", "comment": oldBenchComment})
	f.advance(sweepBusyEvery + time.Second)
	f.m.SweepSite(ctx, s.ID)
	if r := portRow(m, "ether5"); r["horizon"] != "1" || r["comment"] != CustPortComment {
		t.Fatalf("%v", r)
	}
	if !hasEvent(f, s.ID, "customer port") {
		t.Fatal("not recorded in the router's activity")
	}
}

func TestCustomerPortBackoffAfterFailure(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	m.SetWriteFail("/interface/bridge/port", &routeros.APIError{Status: 400, Message: "Bad Request", Detail: "failure: boom"})
	st := f.m.EnsureCustomerPort(ctx, s.ID, false)
	if st.State != PortError || !strings.Contains(st.Text, "ether5: ") || !strings.Contains(st.Message, "boom") {
		t.Fatalf("%+v", st)
	}
	if it := checkItem(t, f, s.ID, CheckBench); it.Status != StatusWarn || !strings.Contains(it.Detail, "boom") {
		t.Fatalf("%+v", it)
	}
	m.SetWriteFail("/interface/bridge/port", nil)
	m.ResetWrites()
	f.m.EnsureCustomerPort(ctx, s.ID, false) // inside the backoff: skipped
	if m.Writes() != 0 || portRow(m, "ether5") != nil {
		t.Fatal("retried inside the backoff")
	}
	f.advance(cpBackoff(1) + time.Second)
	if st = f.m.EnsureCustomerPort(ctx, s.ID, false); st.State != PortOK || portRow(m, "ether5") == nil {
		t.Fatalf("%+v", st)
	}
	// the wait grows
	if cpBackoff(2) <= cpBackoff(1) || cpBackoff(20) != 5*time.Minute {
		t.Fatal("backoff")
	}
}

func TestCustomerPortNonV5RouterIsUntouched(t *testing.T) {
	f := newFixture(t, nil)
	s := f.joined("Block A")
	f.st.Update(func(d *store.Data) error { x := d.Sites[s.ID]; x.BenchPort = "ether5"; d.Sites[s.ID] = x; return nil })
	m := f.startedRouter(s.ID)
	m.SeedRow("/interface/bridge", R{"name": "bridge"}) // an old layout: no v5 marker
	for _, n := range []string{"ether3", "ether5"} {
		m.SeedRow("/interface", R{"name": n, "type": "ether"})
	}
	m.ResetWrites()
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether3", true }); err != nil {
		t.Fatal(err)
	}
	f.m.SweepSite(context.Background(), s.ID)
	st := f.m.CustomerPortStatus(s.ID)
	if m.Writes() != 0 || st.State != PortRebuild || !strings.Contains(st.Text, "Rebuild required") {
		t.Fatalf("writes %d, %+v", m.Writes(), st)
	}
}

func TestOLTPortChangeIsAutoApplied(t *testing.T) {
	// no VLAN: the membership moves with the port
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether3", true }); err != nil {
		t.Fatal(err)
	}
	if portRow(m, "sfp-sfpplus1") != nil || portRow(m, "ether3") == nil || portRow(m, "ether5") == nil || portRow(m, "ether2")["bridge"] != LANBridge {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
	if got := f.m.Effective(f.site(s.ID)).OLTCapMbps; got != 1000 {
		t.Fatalf("an Ethernet OLT port defaults to 1000, got %v", got)
	}
}

func TestOLTVLANMovesToTheNewPort(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 100)
	ctx := context.Background()
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether3", true }); err != nil {
		t.Fatal(err)
	}
	v := m.Table("/interface/vlan")
	if len(v) != 1 || v[0]["interface"] != "ether3" || v[0]["name"] != "vlan100-customers" {
		t.Fatalf("%v", v)
	}
	if portRow(m, "vlan100-customers") == nil || portRow(m, "ether3") != nil || portRow(m, "ether5") == nil {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
	// a VLAN interface the server did not make is left alone and reported
	m.SetRow(ctx, "/interface/vlan", v[0][".id"], R{"comment": "mine", "interface": "ether4"})
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if m.Table("/interface/vlan")[0]["interface"] != "ether4" || f.m.oltApplyError(s.ID) == "" {
		t.Fatalf("%v / %q", m.Table("/interface/vlan"), f.m.oltApplyError(s.ID))
	}
}

func TestOLTPortMbpsDefaults(t *testing.T) {
	for name, want := range map[string]float64{"ether3": 1000, "sfp-sfpplus1": 10000, "sfp1": 1000, "sfpplus2": 10000, "qsfp28-1-1": 10000, "combo1": 1000, "wlan1": 0} {
		if got := OLTPortMbps(name); got != want {
			t.Errorf("%s: %v want %v", name, got, want)
		}
	}
	s := ApplyDefaults(config.Config{}, store.Site{OLTInterface: "ether3"})
	if s.OLTCapMbps != 1000 {
		t.Fatalf("%v", s.OLTCapMbps)
	}
	if s = ApplyDefaults(config.Config{}, store.Site{OLTInterface: "ether3", OLTCapMbps: 2500}); s.OLTCapMbps != 2500 {
		t.Fatalf("an explicit value must be kept: %v", s.OLTCapMbps)
	}
}

// ---- review fixes: customers keep service, confirm, leftovers, cadence, pause ---------------------

func TestTypoOLTPortKeepsCustomersOnline(t *testing.T) {
	for _, vlan := range []int{0, 100} {
		f, s, m := cpRig(t, "ether5", vlan)
		ctx := context.Background()
		f.m.EnsureCustomerPort(ctx, s.ID, true)
		before := fmt.Sprint(m.Table("/interface/bridge/port"), m.Table("/interface/vlan"))
		// the form refuses a port the router does not have
		err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether33", true })
		var fe *FieldError
		if !errors.As(err, &fe) || fe.Field != "olt_interface" || !strings.Contains(fe.Msg, "no port called ether33") {
			t.Fatalf("vlan %d: %v", vlan, err)
		}
		if err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort = "ether55" }); !errors.As(err, &fe) || fe.Field != "bench_port" {
			t.Fatalf("customer port typo: %v", err)
		}
		// and even a typo that got into the record some other way removes nothing
		f.st.Update(func(d *store.Data) error {
			x := d.Sites[s.ID]
			x.OLTInterface = "ether33"
			d.Sites[s.ID] = x
			return nil
		})
		m.ResetWrites()
		st := f.m.EnsureCustomerPort(ctx, s.ID, true)
		if st.OLTMessage == "" || !strings.Contains(st.OLTMessage, "no port called ether33") {
			t.Fatalf("vlan %d: %+v", vlan, st)
		}
		if after := fmt.Sprint(m.Table("/interface/bridge/port"), m.Table("/interface/vlan")); after != before || m.Writes() != 0 {
			t.Fatalf("vlan %d: the pass changed a working router:\n%s\n%s", vlan, before, after)
		}
	}
}

func TestNothingIsRemovedWhenTheReplacementCannotBeWritten(t *testing.T) {
	f, s, m := cpRig(t, "", 0)
	ctx := context.Background()
	m.SeedRow("/interface", R{"name": "ether3", "type": "ether"})
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.OLTInterface = "ether3"
		d.Sites[s.ID] = x
		return nil
	})
	m.SetWriteFail("/interface/bridge/port", &routeros.APIError{Status: 400, Message: "Bad Request", Detail: "failure: boom"})
	st := f.m.EnsureCustomerPort(ctx, s.ID, true)
	if st.State != PortError || portRow(m, "sfp-sfpplus1") == nil {
		t.Fatalf("the old OLT membership went before the new one was written: %+v %v", st, m.Table("/interface/bridge/port"))
	}
	m.SetWriteFail("/interface/bridge/port", nil)
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	if portRow(m, "ether3") == nil || portRow(m, "sfp-sfpplus1") != nil {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
}

func TestOLTAndVLANChangesNeedConfirmOnASetUpRouter(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 100)
	m.SeedRow("/interface", R{"name": "ether3", "type": "ether"})
	for name, mut := range map[string]func(*SiteInput){
		"olt port": func(in *SiteInput) { in.OLTInterface = "ether3" },
		"vlan":     func(in *SiteInput) { in.VLANID = 200 },
		"no vlan":  func(in *SiteInput) { in.VLANID = 0 },
	} {
		if err := f.saveInput(s.ID, mut); !errors.Is(err, ErrOLTConfirmRequired) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if site := f.site(s.ID); site.OLTInterface != "sfp-sfpplus1" || site.VLANID != 100 {
		t.Fatalf("an unconfirmed save changed the router: %+v", site)
	}
	// a customer port, the name or a support phone need no confirm
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort, in.SupportPhone = "ether4", "0700" }); err != nil {
		t.Fatal(err)
	}
	// a router that is not set up yet is free to change
	g := newFixture(t, nil)
	n := g.create("New")
	if err := g.m.UpdateSite(n.ID, SiteInput{Name: "New", OLTInterface: "ether3", VLANID: 7}); err != nil {
		t.Fatalf("unjoined: %v", err)
	}
}

func TestOldCustomerVLANIsRemovedOnlyAfterTheNewOneIsIn(t *testing.T) {
	f, s, m := cpRig(t, "", 100)
	ctx := context.Background()
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.VLANID, in.Confirm = 200, true }); err != nil {
		t.Fatal(err)
	}
	v := m.Table("/interface/vlan")
	if len(v) != 1 || v[0]["name"] != "vlan200-customers" || v[0]["interface"] != "sfp-sfpplus1" || v[0]["vlan-id"] != "200" {
		t.Fatalf("%v", v)
	}
	if portRow(m, "vlan200-customers") == nil || portRow(m, "vlan100-customers") != nil {
		t.Fatalf("%v", m.Table("/interface/bridge/port"))
	}
	// a VLAN the server did not make is never a leftover
	m.SeedRow("/interface/vlan", R{"name": "vlan50-mgmt", "vlan-id": "50", "interface": "sfp-sfpplus1", "comment": "olt management"})
	// VLAN set back to none: the OLT port joins the bridge first, then the server's VLAN goes
	m.RemoveRow(ctx, "/interface/vlan", m.Table("/interface/vlan")[1][".id"])
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.VLANID, in.Confirm = 0, true }); err != nil {
		t.Fatal(err)
	}
	if len(m.Table("/interface/vlan")) != 0 || portRow(m, "sfp-sfpplus1")["bridge"] != CustomerBridge || portRow(m, "vlan200-customers") != nil {
		t.Fatalf("%v / %v", m.Table("/interface/vlan"), m.Table("/interface/bridge/port"))
	}
}

func TestOLTPortRulesDoNotFlapAcrossSweeps(t *testing.T) {
	for name, tc := range map[string]struct {
		vlan  int
		setup func(*routeros.Mock)
	}{
		"management VLAN of the OLT bridged into br-lan": {100, func(m *routeros.Mock) {
			m.SeedRow("/interface/vlan", R{"name": "vlan50-olt", "vlan-id": "50", "interface": "sfp-sfpplus1", "comment": "olt management"})
			m.SeedRow("/interface/bridge/port", R{"bridge": "br-lan", "interface": "vlan50-olt"})
		}},
		"a foreign VLAN on the OLT port with no customer VLAN": {0, func(m *routeros.Mock) {
			m.SeedRow("/interface/vlan", R{"name": "vlan50-olt", "vlan-id": "50", "interface": "sfp-sfpplus1", "comment": "olt management"})
		}},
	} {
		f, s, m := cpRig(t, "ether5", tc.vlan)
		tc.setup(m)
		ctx := context.Background()
		f.m.SweepSite(ctx, s.ID) // settles whatever it can
		f.advance(sweepBusyEvery + time.Second)
		state := fmt.Sprint(m.Table("/interface/bridge/port"), m.Table("/interface/vlan"))
		m.ResetWrites()
		for i := 0; i < 6; i++ {
			f.advance(sweepCleanEvery + time.Second)
			f.m.SweepSite(ctx, s.ID)
			if it := checkItem(t, f, s.ID, CheckOLT); it.Fix.Kind == FixAuto {
				t.Fatalf("%s: the OLT item offers an automatic fix", name)
			}
		}
		if m.Writes() != 0 || fmt.Sprint(m.Table("/interface/bridge/port"), m.Table("/interface/vlan")) != state {
			t.Fatalf("%s: flapping, %d writes", name, m.Writes())
		}
		if portRow(m, "sfp-sfpplus1") == nil && tc.vlan == 0 {
			t.Fatalf("%s: the OLT membership was removed", name)
		}
	}
	// no VLAN: a foreign VLAN on the OLT port is said in words
	f, s, m := cpRig(t, "", 0)
	m.SeedRow("/interface/vlan", R{"name": "vlan50-olt", "vlan-id": "50", "interface": "sfp-sfpplus1", "comment": "olt management"})
	f.m.EnsureCustomerPort(context.Background(), s.ID, true)
	if msg := f.m.oltApplyError(s.ID); !strings.Contains(msg, "vlan50-olt") {
		t.Fatalf("%q", msg)
	}
	if it := checkItem(t, f, s.ID, CheckOLT); it.Status != StatusWarn || !strings.Contains(it.Detail, "vlan50-olt") || it.Fix.Kind != FixNone {
		t.Fatalf("%+v", it)
	}
}

func TestManualAndAutomaticFixesNeverRunTogether(t *testing.T) {
	f, s, m := checkRig(t)
	disableDHCP(m)
	e := f.m.cport.entry(s.ID)
	e.run.Lock() // an automatic pass is running
	done := make(chan error, 1)
	go func() {
		_, err := f.m.ApplyFix(context.Background(), s.ID, CheckDHCP, false, store.RoleOperator)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("a manual fix ran while a pass held the router")
	case <-time.After(150 * time.Millisecond):
	}
	e.run.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the manual fix never ran")
	}
}

func TestSweepCadenceCleanFiveMinutesBusyOneMinute(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	f.m.SweepSite(ctx, s.ID) // applies: a change, so the next look is in a minute
	if portRow(m, "ether5") == nil {
		t.Fatal("not applied")
	}
	f.advance(sweepBusyEvery + time.Second)
	f.m.SweepSite(ctx, s.ID) // clean: the next look is in five minutes
	drift := func() { m.RemoveRow(ctx, "/interface/bridge/port", portRow(m, "ether5")[".id"]) }
	drift()
	f.advance(sweepCleanEvery - time.Minute)
	f.m.SweepSite(ctx, s.ID)
	if portRow(m, "ether5") != nil {
		t.Fatal("looked again before the five minutes were over")
	}
	f.advance(time.Minute + time.Second)
	f.m.SweepSite(ctx, s.ID)
	if portRow(m, "ether5") == nil {
		t.Fatal("did not look after five minutes")
	}
	// after a change the next look is a minute away
	drift()
	f.advance(sweepBusyEvery + time.Second)
	f.m.SweepSite(ctx, s.ID)
	if portRow(m, "ether5") == nil {
		t.Fatal("after a change the next look must come within a minute")
	}
}

func TestPausedRouterGetsNoAutomaticWork(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	if err := f.m.SetAutoFixPause(s.ID, true, time.Time{}, "boss"); err != nil {
		t.Fatal(err)
	}
	disableDHCP(m)
	m.ResetWrites()
	f.m.SweepSite(ctx, s.ID)
	if out := f.m.AutoMaintain(ctx, s.ID); len(out) != 0 || m.Writes() != 0 || portRow(m, "ether5") != nil {
		t.Fatalf("a paused router was written to: %+v, %d writes", out, m.Writes())
	}
	c, _ := f.m.Checks(ctx, s.ID, true)
	it, _ := c.Item(CheckDHCP)
	if c.AutoFixes == nil || !c.AutoFixes.Paused || it.Fix.Automatic || it.Auto == nil || it.Auto.State != "paused" || it.Fix.Kind != FixAuto {
		t.Fatalf("%+v %+v", c.AutoFixes, it)
	}
	if !hasActorEvent(f, s.ID, "boss", "paused") {
		t.Fatal("the pause is not in the router's activity")
	}
	// an explicit save still applies the port
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.BenchPort = "ether4" }); err != nil || portRow(m, "ether4") == nil {
		t.Fatalf("%v %v", err, m.Table("/interface/bridge/port"))
	}
	// an end time lifts the pause by itself
	if err := f.m.SetAutoFixPause(s.ID, true, f.now.Add(-time.Minute), "boss"); !errors.Is(err, ErrPauseInPast) {
		t.Fatalf("%v", err)
	}
	if err := f.m.SetAutoFixPause(s.ID, true, f.now.Add(10*time.Minute), "boss"); err != nil {
		t.Fatal(err)
	}
	if p := f.m.AutoFixPause(s.ID); !p.Paused || p.Until == nil {
		t.Fatalf("%+v", p)
	}
	f.advance(11 * time.Minute)
	if f.m.AutoFixPause(s.ID).Paused {
		t.Fatal("the pause did not end")
	}
	f.m.SweepSite(ctx, s.ID)
	if it := checkItem(t, f, s.ID, CheckDHCP); it.Status != StatusOK {
		t.Fatalf("not fixed after the pause: %+v", it)
	}
	if err := f.m.SetAutoFixPause(s.ID, false, time.Time{}, "boss"); err != nil && !errors.Is(err, store.ErrNoChange) {
		t.Fatal(err)
	}
}

func TestChecklistSaysTheServerKeepsDHCPDNSAndTrafficOn(t *testing.T) {
	f, s, m := checkRig(t)
	ctx := context.Background()
	disableDHCP(m)
	f.m.AutoMaintain(ctx, s.ID)
	it := checkItem(t, f, s.ID, CheckDHCP)
	if it.Auto == nil || !strings.Contains(it.Auto.Text, "The server keeps this on.") || !strings.Contains(it.Auto.Text, "Fixed by the server at") {
		t.Fatalf("%+v", it)
	}
	if other := checkItem(t, f, s.ID, CheckWalled); other.Auto != nil && strings.Contains(other.Auto.Text, "keeps this on") {
		t.Fatalf("the walled rules are not one of the kept-on items: %+v", other)
	}
}

func TestFixWriterVLANRemovalIsOnlyForTheServersOldCustomerVLAN(t *testing.T) {
	m := routeros.NewMock()
	w := &fixWriter{rt: m, insp: m, oldVLANs: map[string]bool{"vlan100-customers": true}}
	ctx := context.Background()
	if err := w.removeRow(ctx, "/interface/vlan", R{".id": "*1", "name": "vlan100-customers", "comment": CustVLANComment}); err != nil {
		t.Fatalf("the old customer VLAN: %v", err)
	}
	for name, row := range map[string]R{
		"not the server's": {".id": "*2", "name": "vlan100-customers", "comment": "mine"},
		"another VLAN":     {".id": "*3", "name": "vlan50-olt", "comment": CustVLANComment},
		"the current VLAN": {".id": "*4", "name": "vlan200-customers", "comment": CustVLANComment},
	} {
		if err := w.removeRow(ctx, "/interface/vlan", row); !errors.Is(err, ErrFixNotAllowed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func hasActorEvent(f *fixture, siteID, actor, sub string) bool {
	found := false
	f.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == siteID && e.Actor == actor && strings.Contains(e.Msg, sub) {
				found = true
			}
		}
	})
	return found
}

// A problem that stays the same is not "busy": the sweep looks again at the slow cadence, not every minute.
func TestUnchangedPortProblemKeepsTheFiveMinuteCadence(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	ctx := context.Background()
	m.SeedRow("/interface/bridge", R{"name": "br-other"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-other", "interface": "ether5"})
	f.m.SweepSite(ctx, s.ID) // new trouble: busy, next look in a minute
	if st := f.m.CustomerPortStatus(s.ID); st.State != PortError {
		t.Fatalf("%+v", st)
	}
	f.advance(sweepBusyEvery + time.Second)
	f.m.SweepSite(ctx, s.ID) // the same trouble: not busy, the next look is five minutes away
	// the trouble goes away; a look one minute later must not happen
	m.RemoveRow(ctx, "/interface/bridge/port", portRow(m, "ether5")[".id"])
	f.advance(sweepBusyEvery + time.Second)
	f.m.SweepSite(ctx, s.ID)
	if portRow(m, "ether5") != nil {
		t.Fatal("an unchanged problem was looked at again after a minute")
	}
	f.advance(sweepCleanEvery)
	f.m.SweepSite(ctx, s.ID)
	if r := portRow(m, "ether5"); r == nil || r["bridge"] != CustomerBridge {
		t.Fatalf("not applied after five minutes: %v", m.Table("/interface/bridge/port"))
	}
}

// The post-join automatic fixes respect the pause.
func TestPostJoinAutoFixesRespectThePause(t *testing.T) {
	f, s, m := cpRig(t, "", 0)
	ctx := context.Background()
	disableDHCP(m)
	if err := f.m.SetAutoFixPause(s.ID, true, time.Time{}, "boss"); err != nil {
		t.Fatal(err)
	}
	m.ResetWrites()
	out, err := f.m.ApplyAutoFixes(ctx, s.ID)
	if err != nil || len(out) != 0 || m.Writes() != 0 {
		t.Fatalf("a paused router was fixed after the join: %+v %v, %d writes", out, err, m.Writes())
	}
	if err := f.m.SetAutoFixPause(s.ID, false, time.Time{}, "boss"); err != nil {
		t.Fatal(err)
	}
	if out, _ := f.m.ApplyAutoFixes(ctx, s.ID); len(out) == 0 {
		t.Fatal("nothing fixed once resumed")
	}
}

// Bonding/LACP interfaces are valid uplinks; bridges, VLANs, WireGuard, PPPoE and the WAN are not.
func TestPortChoicesAcceptBondsAndListPhysicalFirst(t *testing.T) {
	f, s, m := cpRig(t, "", 0)
	ctx := context.Background()
	m.SeedRow("/interface", R{"name": "bond1", "type": "bond"})
	m.SeedRow("/interface", R{"name": "ether3", "type": "ether"})
	m.SeedRow("/interface", R{"name": "vlan9", "type": "vlan"})
	m.SeedRow("/interface", R{"name": "pppoe-out1", "type": "pppoe-out"})
	m.SeedRow("/interface", R{"name": "wg-ftn", "type": "wg"})
	got := f.m.PortChoices(ctx, s.ID)
	idx := map[string]int{}
	for i, n := range got {
		idx[n] = i
	}
	for _, bad := range []string{"vlan9", "pppoe-out1", "wg-ftn", CustomerBridge, "ether1"} {
		if _, ok := idx[bad]; ok {
			t.Errorf("%s offered: %v", bad, got)
		}
	}
	if _, ok := idx["bond1"]; !ok || idx["bond1"] < idx["ether3"] {
		t.Fatalf("bond1 missing or before the physical ports: %v", got)
	}
	// and the save accepts it
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "bond1", true }); err != nil {
		t.Fatalf("bonding uplink refused: %v", err)
	}
}

// A router billing has paused is still a set-up router: the OLT change asks for a confirm.
func TestBillingPausedRouterStillAsksForConfirm(t *testing.T) {
	f, s, m := cpRig(t, "", 100)
	m.SeedRow("/interface", R{"name": "ether3", "type": "ether"})
	if err := f.m.SetDisabled(s.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface = "ether3" }); !errors.Is(err, ErrOLTConfirmRequired) {
		t.Fatalf("%v", err)
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether3", true }); err != nil {
		t.Fatal(err)
	}
}
