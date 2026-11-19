package billing

import (
	"context"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// B4: with ftn-dhcp disabled (not switched yet, or rolled back) the reconciler
// writes nothing and leaves the hotspot's dynamic leases alone.
func TestIPoEWritesNothingWhileFtnDHCPIsDisabled(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "address": "10.5.50.60", "status": "bound", "dynamic": "true", "server": "dhcp-hotspot"})
	for _, r := range e.rt.Table("/ip/dhcp-server") {
		e.rt.RemoveRow(context.Background(), "/ip/dhcp-server", r[".id"])
	}
	e.rt.ResetWrites()
	run := e.reconcile(t)
	if !run.Waiting || e.rt.Writes() != 0 {
		t.Fatalf("%+v writes=%d", run, e.rt.Writes())
	}
	if got := rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "server", "dhcp-hotspot"); len(got) != 1 {
		t.Fatal("the hotspot's dynamic lease was removed")
	}
	if ps := e.svc.ProvisionStatus(e.site); ps.InSync {
		t.Fatalf("never seen enabled: waiting, not rolled back: %+v", ps)
	}
}

// Only dynamic leases on ftn-dhcp are removed to make room for a static one.
func TestIPoEOnlyRemovesDynamicLeasesOfFtnDHCP(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "address": "10.5.50.70", "status": "bound", "dynamic": "true", "server": "dhcp-hotspot"})
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macB2, "address": "10.5.50.71", "status": "bound", "dynamic": "true", "server": DHCPServerName})
	e.flat(t, "Flat 1", macA)
	e.flat(t, "Flat 2", macB2)
	e.reconcile(t)
	dyn := e.rt.Table("/ip/dhcp-server/lease")
	if len(rowsBy(dyn, "server", "dhcp-hotspot")) != 1 {
		t.Fatalf("hotspot lease touched: %v", dyn)
	}
	for _, r := range dyn {
		if r["dynamic"] == "true" && r["server"] == DHCPServerName {
			t.Fatalf("dynamic lease on ftn-dhcp still blocks the static one: %v", r)
		}
	}
}

// A flagged (or MAC-less) flat that has paid time must not stay walled.
func TestIPoEFlaggedEntitledFlatIsUnsuspended(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a1 := e.flat(t, "Flat 1", macA)
	a2 := e.flat(t, "Flat 2", macB2)
	e.reconcile(t) // both suspended
	e.payAt(t, a2, "1 Day")
	e.flag(t, a2.ID, macA)
	e.reconcile(t)
	for _, r := range e.rt.Table("/ip/firewall/address-list") {
		if strings.HasPrefix(r["comment"], "ftn:acct:"+a2.Code) {
			t.Fatalf("a paid flat is still walled: %v", r)
		}
	}
	if len(rowsBy(e.rt.Table("/ip/firewall/address-list"), "comment", AcctComment(a1.Code, a1.Flat))) != 1 {
		t.Fatal("the unpaid flat must stay walled")
	}
}

// A suspended entry the cutover created disabled is turned on when needed.
func TestIPoEEnablesDisabledSuspendedEntries(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 1", macA)
	e.reconcile(t)
	row := find(t, e.rt.Table("/ip/firewall/address-list"), "address", a.IP)
	e.rt.SetRow(context.Background(), "/ip/firewall/address-list", row[".id"], map[string]string{"disabled": "true"})
	e.reconcile(t)
	if r := find(t, e.rt.Table("/ip/firewall/address-list"), "address", a.IP); r["disabled"] == "true" || r["disabled"] == "yes" {
		t.Fatalf("still disabled: %v", r)
	}
}

// Static ARP rows follow the flats, so an interface on reply-only still reaches them.
func TestIPoEManagesStaticARP(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 1", macA)
	e.reconcile(t)
	r := find(t, e.rt.Table("/ip/arp"), "address", a.IP)
	if r["mac-address"] != macA || r["interface"] != "br-hotspot" || r["comment"] != AcctComment(a.Code, a.Flat) {
		t.Fatalf("%v", r)
	}
	if n := e.pass(t); n != 0 {
		t.Fatalf("%d writes", n)
	}
	// An ONT swap moves the ARP entry too.
	if err := e.svc.ChangeMAC(context.Background(), a.ID, macC); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if r := find(t, e.rt.Table("/ip/arp"), "address", a.IP); r["mac-address"] != macC {
		t.Fatalf("%v", r)
	}
	// A deleted flat's ARP row goes.
	if err := e.svc.DeleteAccount(context.Background(), a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if len(rowsBy(e.rt.Table("/ip/arp"), "address", a.IP)) != 0 {
		t.Fatal("stray ARP row")
	}
}

func TestHostNameIsCapped(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "00:15:EB:00:00:01", "address": "10.5.50.30", "status": "bound", "dynamic": "true", "host-name": strings.Repeat("x", 300), "server": DHCPServerName})
	e.reconcile(t)
	for _, d := range e.svc.Unregistered(e.site, false) {
		if len(d.HostName) > 64 {
			t.Fatalf("host name of %d bytes", len(d.HostName))
		}
	}
}

// Static leases are written with a one-hour lease time (the customer DHCP
// server's own lease time is a minute, for the unregistered devices).
func TestIPoEStaticLeasesGetOneHourLeaseTime(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.reconcile(t)
	var n int
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		if r["mac-address"] == macA {
			n++
			if r["lease-time"] != "1h" {
				t.Fatalf("lease-time %q", r["lease-time"])
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d leases", n)
	}
}
