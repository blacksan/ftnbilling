package billing

import (
	"context"
	"testing"
	"time"

	"ispbill/internal/routeros"
)

// Discovery works in every mode: the bench laptop that already logged in on
// the hotspot (authorised, bound to the old test user) must still be listed.
func TestRefreshDevicesHotspotListsAuthorisedHostWithNoAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const laptop = "3C:97:0E:11:22:33"
	e.rt.AddHostRow(routeros.Host{MAC: laptop, Address: "10.5.50.31", HostName: "bench-laptop", Authorized: true})
	e.rt.AddHostRow(routeros.Host{MAC: "3C:97:0E:11:22:44", Address: "10.5.50.32", Bypassed: true})
	e.rt.SeedRow("/ip/hotspot/active", routeros.Row{"user": "test1", "mac-address": laptop})
	e.rt.SeedRow("/ip/hotspot/user", routeros.Row{"name": "test1"}) // no MAC of its own
	e.rt.SeedRow("/ip/hotspot/ip-binding", routeros.Row{"mac-address": "3C:97:0E:11:22:44", "type": "bypassed"})
	e.rt.ResetWrites()
	if err := e.svc.RefreshHosts(ctx, e.site); err != nil {
		t.Fatal(err)
	}
	if e.rt.Writes() != 0 {
		t.Fatal("refresh must not write")
	}
	list := e.svc.Unregistered(e.site, false)
	by := map[string]UnregDevice{}
	for _, u := range list {
		by[u.MAC] = u
	}
	l, ok := by[laptop]
	if !ok || !l.Online || l.RouterUser != "test1" || l.Source != "hotspot-host" || l.HostName != "bench-laptop" || l.IP != "10.5.50.31" {
		t.Fatalf("%+v", list)
	}
	if b := by["3C:97:0E:11:22:44"]; b.RouterUser != "ip-binding (bypassed)" {
		t.Fatalf("%+v", b)
	}
	if got := e.svc.RouterUser(e.site, "3c-97-0e-11-22-33"); got != "test1" {
		t.Fatalf("lookup normalises the MAC: %q", got)
	}
	if e.svc.RouterUser(e.site, "garbage") != "" || e.svc.RouterUser(e.site, "00:00:00:00:00:01") != "" {
		t.Fatal("unknown")
	}
}

func TestRefreshDevicesSkipsHostsThatHaveAnAccount(t *testing.T) {
	e := newEnv(t)
	e.flat(t, "Flat 1", macA)
	e.rt.AddHostRow(routeros.Host{MAC: macA, Address: "10.5.50.40", Authorized: true})
	e.rt.SeedRow("/ip/hotspot/active", routeros.Row{"user": "K7Q2MX", "mac-address": macA})
	if err := e.svc.RefreshHosts(context.Background(), e.site); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.Unregistered(e.site, false); len(got) != 0 {
		t.Fatalf("a flat's own MAC must not be listed: %+v", got)
	}
}

func TestRefreshDevicesHotspotReadsLeasesAndARPToo(t *testing.T) {
	e := newEnv(t)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": macNew, "address": "10.5.50.13", "status": "reachable"})
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": "00:15:EB:00:00:0A", "address": "192.168.99.5", "status": "reachable"}) // other network
	if err := e.svc.RefreshHosts(context.Background(), e.site); err != nil {
		t.Fatal(err)
	}
	got := e.svc.Unregistered(e.site, true)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	// The next refresh after the lease vanished marks it offline.
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		e.rt.RemoveRow(context.Background(), "/ip/dhcp-server/lease", r[".id"])
	}
	e.now = e.now.Add(time.Minute)
	if err := e.svc.RefreshHosts(context.Background(), e.site); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.Unregistered(e.site, true); len(got) != 1 || got[0].MAC != macNew {
		t.Fatalf("%+v", got)
	}
}

func TestSweepRefreshesDevicesOfJoinedHotspotBuildings(t *testing.T) {
	e := newEnv(t)
	e.rt.AddHostRow(routeros.Host{MAC: "3C:97:0E:11:22:33", Address: "10.5.50.31", Authorized: true})
	e.svc.Sweep(context.Background())
	if got := e.svc.Unregistered(e.site, false); len(got) != 1 {
		t.Fatalf("the sweep should have listed the host: %+v", got)
	}
}
