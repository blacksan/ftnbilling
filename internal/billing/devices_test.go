package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

const (
	macHuawei = "00:E0:FC:12:34:56"
	macPhone  = "DA:A1:19:00:00:01" // locally administered: a randomised phone address
	macNew    = "00:15:EB:AA:BB:CC" // ZTE
)

func (e *env) dynLease(mac, ip, host, status string) {
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": mac, "address": ip, "active-address": ip, "host-name": host,
		"status": status, "dynamic": "true", "server": DHCPServerName})
}

func TestVendorLookup(t *testing.T) {
	for mac, want := range map[string]string{
		macHuawei:           "Huawei",
		macNew:              "ZTE",
		macPhone:            PrivateAddress,
		"00:0c:42:11:22:33": "MikroTik",
		"AA:BB:CC:DD:EE:01": PrivateAddress, // 0xAA has the local bit set
		"04:00:00:00:00:01": "",
		"garbage":           "",
	} {
		if got := Vendor(mac); got != want {
			t.Errorf("Vendor(%s) = %q, want %q", mac, got, want)
		}
	}
}

func TestIPoEPassCollectsUnknownMACsFromLeasesAndARP(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 1", macA)
	_ = a
	e.dynLease(macHuawei, "10.5.50.11", "HG8010H", "bound")
	e.dynLease(macPhone, "10.5.50.12", "Pixel", "waiting")
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": macNew, "address": "10.5.50.13", "interface": "br-customers", "status": "reachable"})
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": "00:15:EB:00:00:09", "address": "10.5.50.14", "status": "stale"})
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": "00:15:EB:00:00:0A", "address": "192.168.99.5", "status": "reachable"}) // other network: ignored
	e.reconcile(t)

	list := e.svc.Unregistered("", false)
	if len(list) != 4 {
		t.Fatalf("%d devices: %+v", len(list), list)
	}
	byMAC := map[string]UnregDevice{}
	for _, u := range list {
		byMAC[u.MAC] = u
	}
	h := byMAC[macHuawei]
	if !h.Online || h.Vendor != "Huawei" || h.HostName != "HG8010H" || h.IP != "10.5.50.11" || h.Source != "dhcp" || h.SiteName != "Block A" {
		t.Fatalf("%+v", h)
	}
	if n := byMAC[macNew]; !n.Online || n.Source != "arp" || n.Vendor != "ZTE" {
		t.Fatalf("%+v", n)
	}
	if p := byMAC[macPhone]; p.Online || p.Vendor != PrivateAddress {
		t.Fatalf("a waiting lease is not online: %+v", p)
	}
	if byMAC["00:15:EB:00:00:09"].Online {
		t.Fatal("a stale ARP entry is not online")
	}
	// Online first.
	if !list[0].Online || !list[1].Online || list[2].Online || list[3].Online {
		t.Fatalf("not online-first: %+v", list)
	}
	if got := e.svc.Unregistered("", true); len(got) != 2 {
		t.Fatalf("online filter: %d", len(got))
	}
	if n := e.svc.UnregisteredOnline(nil); n != 2 {
		t.Fatalf("badge %d", n)
	}
	// The flat's own MAC is never listed.
	if _, ok := byMAC[macA]; ok {
		t.Fatal("a configured MAC must not be listed")
	}
	// The Huawei goes away: it turns offline, it is not forgotten.
	e.rt.ResetWrites()
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		if r["mac-address"] == macHuawei {
			e.rt.RemoveRow(context.Background(), "/ip/dhcp-server/lease", r[".id"])
		}
	}
	e.now = e.now.Add(time.Minute)
	e.reconcile(t)
	for _, u := range e.svc.Unregistered("", false) {
		if u.MAC == macHuawei && u.Online {
			t.Fatal("must be offline")
		}
	}
	// And a device the router stopped reporting is offline even when the pass is old.
	e.now = e.now.Add(10 * time.Minute)
	if n := e.svc.UnregisteredOnline(nil); n != 0 {
		t.Fatalf("stale observations must not count as online: %d", n)
	}
}

func TestRefreshHostsReadsIPoERouterWithoutWriting(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.rt.ResetWrites()
	if err := e.svc.RefreshHosts(context.Background(), e.site); err != nil {
		t.Fatal(err)
	}
	if e.rt.Writes() != 0 {
		t.Fatal("refresh must not write to the router")
	}
	if got := e.svc.Unregistered(e.site, true); len(got) != 1 || got[0].MAC != macHuawei {
		t.Fatalf("%+v", got)
	}
	e.rt.SetFail(errors.New("down"))
	if err := e.svc.RefreshHosts(context.Background(), e.site); err == nil {
		t.Fatal("a router error must surface")
	}
}

func TestPossibleONTSwapSuggestion(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 1", macA)
	e.flat(t, "Flat 2", macB2)
	// Both flats' ONTs are talking at first.
	e.dynLease(macA, "10.5.50.60", "", "bound")
	e.reconcile(t)
	// Flat 2's ONT never shows. Flat 1's lease row is the static one, bound.
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		if r["mac-address"] == macA && r["dynamic"] != "true" {
			e.rt.SetRow(context.Background(), "/ip/dhcp-server/lease", r[".id"], map[string]string{"status": "bound"})
		}
	}
	e.now = e.now.Add(2 * time.Hour)
	e.dynLease(macNew, "10.5.50.15", "ONT", "bound")
	e.reconcile(t)
	if got := e.svc.Unregistered("", true); len(got) != 1 || got[0].Suggested != nil {
		t.Fatalf("no suggestion within the first day: %+v", got)
	}
	// 25 hours later flat 1 is still seen (bound), flat 2 has not been: the new device is probably flat 2's.
	e.now = e.now.Add(25 * time.Hour)
	e.reconcile(t)
	got := e.svc.Unregistered("", true)
	if len(got) != 1 || got[0].Suggested == nil || got[0].Suggested.Flat != "Flat 2" || got[0].Suggested.MAC != macB2 {
		t.Fatalf("%+v", got)
	}
	_ = a
	// One tap: replace the MAC. The flat gets its lease at once.
	acct := got[0].Suggested
	if _, err := e.svc.AttachDevice(context.Background(), e.site, macNew, acct.ID); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		if r["mac-address"] == macNew && r["server"] == DHCPServerName && r["dynamic"] != "true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("static lease for the new MAC: %v", e.rt.Table("/ip/dhcp-server/lease"))
	}
	if got := e.svc.Unregistered("", false); len(got) != 0 {
		t.Fatalf("an attached device leaves the list: %+v", got)
	}
}

func TestSwapIsNotGuessedWhenAmbiguous(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.flat(t, "Flat 2", macB2)
	e.reconcile(t)
	e.now = e.now.Add(30 * time.Hour)
	e.dynLease(macNew, "10.5.50.15", "", "bound")
	e.reconcile(t)
	// Two silent flats, one new device, no vendor match: the picker offers both, no suggestion.
	if got := e.svc.Unregistered("", true); len(got) != 1 || got[0].Suggested != nil {
		t.Fatalf("%+v", got)
	}
	if ids := e.svc.StaleAccountIDs(e.site); len(ids) != 2 {
		t.Fatalf("both silent flats are picker defaults: %v", ids)
	}
}

func TestAttachConflictNamesOwnerAndBuilding(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	owner := e.flat(t, "Flat 1", macA)
	mine := e.flatAt(t, b, "Flat 9", macC)
	var dup *DuplicateMACError
	_, err := e.svc.AttachDevice(context.Background(), b, macA, mine.ID)
	if !errors.As(err, &dup) || dup.AccountID != owner.ID || dup.Flat != "Flat 1" || dup.SiteName != "Block A" {
		t.Fatalf("%v", err)
	}
	// A flat in another building than the device's is refused.
	if _, err := e.svc.AttachDevice(context.Background(), e.site, macNew, mine.ID); err == nil {
		t.Fatal("cross-building attach must be refused")
	}
}

func TestDismissStaysDismissedAndCreateFromDevice(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.dynLease(macNew, "10.5.50.12", "", "bound")
	e.reconcile(t)
	if err := e.svc.DismissDevice(e.site, macHuawei); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	e.reconcile(t)
	got := e.svc.Unregistered("", false)
	if len(got) != 1 || got[0].MAC != macNew || e.svc.UnregisteredOnline(nil) != 1 {
		t.Fatalf("a dismissed device must not come back: %+v", got)
	}
	// Create an account from the remaining device, with a package.
	pk := e.pkg("1 Week")
	a, err := e.svc.CreateFromDevice(context.Background(), DeviceAccountInput{SiteID: e.site, MAC: macNew, Flat: "Flat 7", Tenant: "Wanjiru", PackageID: pk.ID})
	if err != nil {
		t.Fatal(err)
	}
	if a.IP == "" || a.PackageID != pk.ID || a.Tenant != "Wanjiru" {
		t.Fatalf("%+v", a)
	}
	if len(e.svc.Unregistered("", false)) != 0 {
		t.Fatal("device must leave the list")
	}
	// Lease and queue are on the router already; the flat is walled until it pays.
	if len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macNew)) == 0 && len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "comment", AcctComment(a.Code, a.Flat))) != 1 {
		t.Fatal("static lease expected")
	}
	if q := find(t, e.rt.Table("/queue/simple"), "name", "ftn-"+a.Code); q["max-limit"] != pk.RateLimit() {
		t.Fatalf("%v", q)
	}
}

func TestCreateFromDeviceWithABadPackageCreatesNothing(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	_, err := e.svc.CreateFromDevice(context.Background(), DeviceAccountInput{SiteID: e.site, MAC: "00:15:EB:00:00:77", Flat: "Flat 9", PackageID: "nope"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := e.svc.AccountByMAC("00:15:EB:00:00:77"); ok {
		t.Fatal("a half-made account was left behind")
	}
}

// The curated prefixes must not repeat (a repeat would make the vendor depend on map order).
func TestOUITableHasNoDuplicatePrefixes(t *testing.T) {
	seen := map[string]string{}
	for _, row := range curatedOUI {
		for _, p := range strings.Fields(row.Prefixes) {
			if len(p) != 8 || p != strings.ToUpper(p) {
				t.Errorf("%s: %q is not AA:BB:CC", row.Vendor, p)
			}
			if prev, dup := seen[p]; dup {
				t.Errorf("%s is listed for %s and %s", p, prev, row.Vendor)
			}
			seen[p] = row.Vendor
		}
	}
}

func TestVendorKnowsTheCommonMakers(t *testing.T) {
	for mac, want := range map[string]string{
		"00:E0:4C:68:01:02": "Realtek", "6C:1F:F7:BB:46:94": "Ugreen", "00:15:EB:00:00:01": "ZTE", "00:E0:FC:12:34:56": "Huawei",
		"00:03:93:AA:BB:CC": "Apple", "50:C7:BF:00:00:01": "TP-Link", "00:0C:42:00:00:01": "MikroTik", "6C:3B:6B:00:00:01": "MikroTik",
		"6c:1f:f7:bb:46:94": "Ugreen", "DA:A1:19:00:00:01": PrivateAddress, "00:00:5E:00:00:01": "",
	} {
		if got := Vendor(mac); got != want {
			t.Errorf("Vendor(%s) = %q, want %q", mac, got, want)
		}
	}
}

// A prefix the curated table lacks falls back to the compiled-in IEEE registry; the curated name wins.
func TestVendorFallsBackToTheIEEERegistry(t *testing.T) {
	ieeeOUI["12:34:56"] = "Example Makers Ltd"
	ieeeOUI["00:E0:4C"] = "REALTEK SEMICONDUCTOR CORP."
	t.Cleanup(func() { delete(ieeeOUI, "12:34:56"); delete(ieeeOUI, "00:E0:4C") })
	if got := Vendor("10:34:56:00:00:01"); got != "" { // not in either table
		t.Fatalf("got %q", got)
	}
	ieeeOUI["10:34:56"] = "Example Makers Ltd"
	t.Cleanup(func() { delete(ieeeOUI, "10:34:56") })
	if got := Vendor("10:34:56:00:00:01"); got != "Example Makers Ltd" {
		t.Fatalf("fallback: %q", got)
	}
	if got := Vendor("00:E0:4C:00:00:01"); got != "Realtek" {
		t.Fatalf("curated name lost: %q", got)
	}
}
