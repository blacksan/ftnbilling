package sites

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// ---- the sweep never switches off an interface that carries the uplink ----------------------------------

func uplinkRig(t *testing.T) (*fixture, store.Site, *routeros.Mock) {
	f, s, m := cpRig(t, "ether3", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts = "" // the owner enabled nothing: every port that is not the uplink or the admin port is off
		d.Sites[s.ID] = x
		return nil
	})
	return f, s, m
}

func wanStays(t *testing.T, f *fixture, s store.Site, m *routeros.Mock, port string) {
	t.Helper()
	sweepNow(f, s.ID)
	sweepNow(f, s.ID)
	if r := ifaceRow(m, port); r["disabled"] == "yes" {
		t.Fatalf("the sweep switched off %s, which carries the uplink", port)
	}
	if r := ifaceRow(m, "ether4"); r["disabled"] != "yes" {
		t.Fatalf("the sweep did not run: ether4 is still on")
	}
}

// PPPoE runs on ether1 and the stored WAN name was changed in the form: the WAN list member is pppoe-wan, so only
// the router's own PPPoE client says that ether1 carries the uplink.
func TestSweepKeepsThePPPoEParentWhenTheStoredWANNameIsChanged(t *testing.T) {
	f, s, m := uplinkRig(t)
	m.SeedRow("/interface", R{"name": "pppoe-wan", "type": "pppoe-out", "running": "true"})
	m.SeedRow("/interface/pppoe-client", R{"name": "pppoe-wan", "interface": "ether1", "comment": "ftn: WAN pppoe"})
	m.SeedRow("/interface/list/member", R{"list": "WAN", "interface": "pppoe-wan", "comment": "ftn: WAN member"})
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.WANInterface, x.WANMode = "ether9", WANPPPoE // the name was edited; the router still uses ether1
		d.Sites[s.ID] = x
		return nil
	})
	m.ResetWrites()
	wanStays(t, f, s, m, "ether1")
}

func TestSweepKeepsEveryInterfaceThatCarriesTheUplink(t *testing.T) {
	cases := map[string]func(m *routeros.Mock){
		"dhcp client": func(m *routeros.Mock) {
			m.SeedRow("/ip/dhcp-client", R{"interface": "ether1", "comment": "ftn: WAN dhcp"})
		},
		"address": func(m *routeros.Mock) {
			m.SeedRow("/ip/address", R{"address": "203.0.113.2/29", "interface": "ether1", "comment": "ftn: WAN address", "disabled": "false"})
		},
		"address added by hand": func(m *routeros.Mock) {
			m.SeedRow("/ip/address", R{"address": "198.51.100.2/30", "interface": "ether1", "disabled": "false"})
		},
		"default route by interface": func(m *routeros.Mock) {
			m.SeedRow("/ip/route", R{"dst-address": "0.0.0.0/0", "gateway": "ether1", "active": "true", "disabled": "false"})
		},
		"default route by gateway status": func(m *routeros.Mock) {
			m.SeedRow("/ip/route", R{"dst-address": "0.0.0.0/0", "gateway": "10.9.9.1", "gateway-status": "10.9.9.1 reachable via  ether1", "active": "true"})
		},
		"vlan under a wan list member": func(m *routeros.Mock) {
			m.SeedRow("/interface", R{"name": "vlan100", "type": "vlan", "running": "true"})
			m.SeedRow("/interface/vlan", R{"name": "vlan100", "vlan-id": "100", "interface": "ether1"})
			m.SeedRow("/interface/list/member", R{"list": "WAN", "interface": "vlan100"})
		},
		"bridge holding the wan address": func(m *routeros.Mock) {
			m.SeedRow("/interface/bridge", R{"name": "br-wan"})
			m.SeedRow("/interface/bridge/port", R{"bridge": "br-wan", "interface": "ether1"})
			m.SeedRow("/ip/address", R{"address": "203.0.113.2/29", "interface": "br-wan", "disabled": "false"})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f, s, m := uplinkRig(t)
			setup(m)
			f.st.Update(func(d *store.Data) error {
				x := d.Sites[s.ID]
				x.WANInterface = "ether9"
				d.Sites[s.ID] = x
				return nil
			})
			wanStays(t, f, s, m, "ether1")
		})
	}
}

// When the router read that says what carries the uplink is incomplete, no port is changed at all.
func TestSweepChangesNoPortWhenTheUplinkReadIsIncomplete(t *testing.T) {
	for _, path := range []string{"/interface/pppoe-client", "/ip/route"} {
		f, s, m := uplinkRig(t)
		m.SetReadFail(path, errors.New("boom"))
		m.ResetWrites()
		sweepNow(f, s.ID)
		for _, r := range m.Table("/interface") {
			if r["disabled"] == "yes" {
				t.Fatalf("%s unreadable: %s was switched off", path, r["name"])
			}
		}
		m.SetReadFail(path, nil)
		sweepNow(f, s.ID)
		if ifaceRow(m, "ether4")["disabled"] != "yes" {
			t.Fatalf("%s readable again: the sweep did not resume", path)
		}
	}
}

func TestFixWriterRefusesToSwitchAnUplinkInterface(t *testing.T) {
	m := routeros.NewMock()
	w := &fixWriter{rt: m, wan: "ether9", offPorts: map[string]bool{"ether1": true, "ether4": true}, uplink: map[string]bool{"ether1": true}}
	if err := w.setRow(context.Background(), "/interface", R{"name": "ether1", "type": "ether", ".id": "*1"}, map[string]string{"disabled": "yes"}); err == nil {
		t.Fatal("an uplink interface was allowed")
	}
	if err := w.setRow(context.Background(), "/interface", R{"name": "ether4", "type": "ether", ".id": "*2"}, map[string]string{"disabled": "yes"}); err != nil && !strings.Contains(err.Error(), "no such") {
		t.Fatalf("a plain port was refused: %v", err)
	}
}

// The WAN port of a router that has joined cannot be edited in the form, unless a new script is being made.
func TestWANPortOfAJoinedRouterIsLocked(t *testing.T) {
	f, s, _ := cpRig(t, "ether3", 0)
	if !siteIsSetUp(f.site(s.ID)) {
		t.Fatal("the rig router is not set up")
	}
	err := f.saveInput(s.ID, func(in *SiteInput) { in.WANInterface = "ether9" })
	if err == nil || !strings.Contains(err.Error(), "cannot be changed here") {
		t.Fatalf("the WAN port was changed: %v", err)
	}
	if f.site(s.ID).WANInterface == "ether9" {
		t.Fatal("stored")
	}
	// the same value is fine, and so is a change while a new script is pending
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.Notes = "x" }); err != nil {
		t.Fatalf("an unrelated save was refused: %v", err)
	}
	if _, _, err := f.m.MintJoin(s.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.WANInterface = "ether9" }); err != nil {
		t.Fatalf("a re-bootstrap must allow it: %v", err)
	}
}

// ---- the admin firewall fixer -------------------------------------------------------------------------

func TestAdminRulesAreWrittenEvenWhenTheAllowListWriteFails(t *testing.T) {
	f, s, m := adminRig(t)
	m.SetWriteFail("/ip/firewall/address-list", errors.New("no space"))
	out := heal(t, f, s)
	fw := forwardOrder(m)
	if len(fw) < 3 || fw[0] != AdminAllowComment || fw[1] != AdminDropComment || fw[2] != AdminNetDropComment {
		t.Fatalf("the rules were not written: %v", fw)
	}
	noted := false
	for _, o := range out {
		for _, a := range o.Applied {
			noted = noted || strings.Contains(a, "could not update the admin allow list")
		}
	}
	if !noted {
		t.Fatalf("the list failure was not logged: %+v", out)
	}
	if len(listEntries(m)) != 0 {
		t.Fatalf("list written despite the failure: %v", listEntries(m))
	}
	// the next sweep retries the list
	m.SetWriteFail("/ip/firewall/address-list", nil)
	heal(t, f, s)
	if len(listEntries(m)) != 3 {
		t.Fatalf("the list was not retried: %v", listEntries(m))
	}
}

func TestAdminAllowListHasIPsBeforeNames(t *testing.T) {
	e := AdminAllowFor("https://isp.ftw.co.ke", "tech.ftw.co.ke", "162.35.47.106")
	if len(e) != 3 || e[0].Address != "162.35.47.106" || e[1].Address != "isp.ftw.co.ke" || e[2].Address != "tech.ftw.co.ke" {
		t.Fatalf("%+v", e)
	}
}

func TestAdminAllowListTreatsAlreadyHaveSuchEntryAsDone(t *testing.T) {
	if !alreadyHaveEntry(errors.New("failure: already have such entry")) || alreadyHaveEntry(errors.New("timeout")) || alreadyHaveEntry(nil) {
		t.Fatal("alreadyHaveEntry")
	}
	f, s, m := adminRig(t)
	// RouterOS already holds the static IP row under another comment: the add is refused, and that is fine
	m.SeedRow("/ip/firewall/address-list", R{"list": AdminAllowList, "address": "203.0.113.10", "comment": "by hand"})
	out := heal(t, f, s)
	fw := forwardOrder(m)
	if len(fw) < 3 || fw[0] != AdminAllowComment {
		t.Fatalf("rules: %v", fw)
	}
	if len(out) == 0 {
		t.Fatal("nothing was done")
	}
	if got := listEntries(m); got["isp.ftw.co.ke"] == "" || got["tech.ftw.co.ke"] == "" {
		t.Fatalf("the names after the refused IP row were not written: %v", got)
	}
}

// ---- the router form: OLT and customer ports must be on first --------------------------------------------

func TestMovingTheOLTOrACustomerPortNeedsThePortEnabledFirst(t *testing.T) {
	f, s, m := cpRig(t, "ether3", 0)
	m.SeedRow("/interface", R{"name": "ether6", "type": "ether", "running": "true"})
	m.SeedRow("/interface", R{"name": "ether7", "type": "ether", "running": "true"})
	err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether6", true })
	if err == nil || err.Error() != "Enable ether6 in the Ports tab first, then move the role to it." {
		t.Fatalf("OLT move: %v", err)
	}
	if f.site(s.ID).OLTInterface == "ether6" {
		t.Fatal("stored")
	}
	err = f.saveInput(s.ID, func(in *SiteInput) { in.CustomerPorts = "ether3,ether7" })
	if err == nil || err.Error() != "Enable ether7 in the Ports tab first, then move the role to it." {
		t.Fatalf("customer port: %v", err)
	}
	// once the owner has enabled them the same saves go through
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.EnabledPorts += ",ether6,ether7"
		d.Sites[s.ID] = x
		return nil
	})
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.CustomerPorts = "ether3,ether7" }); err != nil {
		t.Fatal(err)
	}
	if err := f.saveInput(s.ID, func(in *SiteInput) { in.OLTInterface, in.Confirm = "ether6", true }); err != nil {
		t.Fatal(err)
	}
}

// ---- the upgrade record ------------------------------------------------------------------------------------

func TestUpgradeRecordsTheConfiguredPortsEvenWhenTheyAreNotInThatRead(t *testing.T) {
	f, s, m := cpRig(t, "ether5", 0)
	f.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.PortsPolicy, x.EnabledPorts, x.DisabledPorts = "", "", ""
		d.Sites[s.ID] = x
		return nil
	})
	// the OLT row is missing from the read, the customer port is disabled by hand at that moment, and a second admin
	// port (not in LANPorts) sits in br-lan with no link
	for _, r := range m.Table("/interface") {
		if r["name"] == "sfp-sfpplus1" {
			m.RemoveRow(context.Background(), "/interface", r[".id"])
		}
	}
	setField(t, m, "/interface", "ether5", "disabled", "true")
	m.SeedRow("/interface", R{"name": "ether6", "type": "ether", "running": "false"})
	m.SeedRow("/interface/bridge/port", R{"bridge": "br-lan", "interface": "ether6", "comment": "ftn: LAN port"})
	sweepNow(f, s.ID)
	got := SplitPorts(f.site(s.ID).EnabledPorts)
	for _, p := range []string{"sfp-sfpplus1", "ether5", "ether6"} {
		if !containsStr(got, p) {
			t.Errorf("%s not recorded: %v", p, got)
		}
	}
	for _, p := range []string{"ether1", "ether2"} {
		if containsStr(got, p) {
			t.Errorf("%s must not be recorded: %v", p, got)
		}
	}
	// the port that reappears is not switched off
	m.SeedRow("/interface", R{"name": "sfp-sfpplus1", "type": "ether", "running": "true"})
	sweepNow(f, s.ID)
	if ifaceRow(m, "sfp-sfpplus1")["disabled"] == "yes" {
		t.Fatal("the OLT was switched off when it came back")
	}
}
