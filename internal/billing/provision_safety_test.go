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
		e.rt.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	e.rt.ResetWrites()
	run := e.reconcile(t)
	if !run.Waiting || e.rt.Writes() != 0 {
		t.Fatalf("%+v writes=%d", run, e.rt.Writes())
	}
	if got := rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "server", "dhcp-hotspot"); len(got) != 1 {
		t.Fatal("the hotspot's dynamic lease was removed")
	}
	if ps := e.svc.ProvisionStatus(e.site); ps.InSync || ps.RolledBack {
		t.Fatalf("never seen enabled: waiting, not rolled back: %+v", ps)
	}
	if e.site != "" && siteMode(e, e.site) != store.AccessIPoE {
		t.Fatal("mode must not change before a rollback was seen")
	}
}

func siteMode(e *env, id string) (m string) {
	e.st.View(func(d *store.Data) { m = d.Sites[id].AccessMode })
	return
}

// A router that was serving from ftn-dhcp and then rolled back is detected: the
// building goes back to hotspot mode and nothing is written.
func TestIPoERollbackIsDetectedAndStopsIPoE(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.reconcile(t) // enabled: converges, remembers it saw ftn-dhcp on
	for _, r := range e.rt.Table("/ip/dhcp-server") {
		e.rt.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	e.rt.ResetWrites()
	// ftn-dhcp is off but no hotspot is back on: that is not (yet) a rollback.
	e.reconcile(t)
	if got := siteMode(e, e.site); got != store.AccessIPoE || e.svc.ProvisionStatus(e.site).RolledBack {
		t.Fatal("off without a hotspot back is only waiting")
	}
	// The mark survives a restart of this server: it is in the store, not in memory.
	e.svc.prov(e.site).sawOn = false
	e.rt.SeedRow("/ip/hotspot", routeros.Row{"name": "hotspot1", "interface": "br-hotspot", "disabled": "false"})
	e.rt.ResetWrites()
	e.reconcile(t)
	if e.rt.Writes() != 0 {
		t.Fatal("wrote after a rollback")
	}
	ps := e.svc.ProvisionStatus(e.site)
	if !ps.RolledBack || !strings.Contains(ps.Note, "rolled back") {
		t.Fatalf("%+v", ps)
	}
	if got := siteMode(e, e.site); got != store.AccessHotspot {
		t.Fatalf("mode after rollback: %s", got)
	}
	e.st.View(func(d *store.Data) {
		if SiteMark(d, e.site, MarkCutoverSeen) {
			t.Fatal("the rollout marks are cleared by the revert")
		}
	})
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

// The ftn-unreg entry the cutover made follows the mode: on in ipoe, off in observe.
func TestIPoEUnregRangeFollowsMode(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoEObserve)
	e.rt.SeedRow("/ip/firewall/address-list", routeros.Row{"list": UnregList, "address": "10.5.50.10-10.5.50.59", "comment": "ftn: unregistered range", "disabled": "true"})
	e.reconcile(t)
	if r := find(t, e.rt.Table("/ip/firewall/address-list"), "list", UnregList); r["disabled"] != "true" {
		t.Fatalf("observe must keep it off: %v", r)
	}
	e.setMode(t, e.site, store.AccessIPoE)
	e.reconcile(t)
	if r := find(t, e.rt.Table("/ip/firewall/address-list"), "list", UnregList); r["disabled"] == "true" || r["disabled"] == "yes" {
		t.Fatalf("ipoe must wall it: %v", r)
	}
	e.setMode(t, e.site, store.AccessIPoEObserve)
	e.reconcile(t)
	if r := find(t, e.rt.Table("/ip/firewall/address-list"), "list", UnregList); r["disabled"] != "yes" && r["disabled"] != "true" {
		t.Fatalf("back to observe must unwall it: %v", r)
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
