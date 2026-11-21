package billing

import (
	"context"
	"errors"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

const (
	macRouterPort = "38:32:7A:07:B6:CD" // ether5's own MAC on the bench router
	macThinkPad   = "6C:1F:F7:BB:46:94" // Ugreen USB adapter: on ether5 at layer 2, never asks for an address
)

func (e *env) ownIface(name, mac string) {
	e.rt.SeedRow("/interface", routeros.Row{"name": name, "type": "ether", "mac-address": mac})
}

func (e *env) bridgeHost(mac, port, local string) {
	e.rt.SeedRow("/interface/bridge/host", routeros.Row{"mac-address": mac, "on-interface": port, "bridge": CustomerBridgeName, "local": local})
}

func (e *env) clearTable(path string) {
	for _, r := range e.rt.Table(path) {
		_ = e.rt.RemoveRow(context.Background(), path, r[routeros.RowID])
	}
}

func unregByMAC(e *env) map[string]UnregDevice {
	out := map[string]UnregDevice{}
	for _, u := range e.svc.Unregistered("", false) {
		out[u.MAC] = u
	}
	return out
}

// The router's own interface MACs (also seen as local bridge hosts, in leases or in ARP) are never new devices.
func TestRouterOwnMACsAreNeverNewDevices(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.ownIface("ether5", macRouterPort)
	e.bridgeHost(macRouterPort, "ether5", "true") // as on the bench: listed as local
	e.dynLease(macRouterPort, "10.5.50.20", "", "bound")
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": macRouterPort, "address": "10.5.50.21", "interface": "br-customers", "status": "reachable"})
	e.dynLease(macHuawei, "10.5.50.11", "HG8010H", "bound")
	e.reconcile(t)
	got := unregByMAC(e)
	if _, ok := got[macRouterPort]; ok {
		t.Fatalf("the router's own MAC was recorded as a device: %+v", got[macRouterPort])
	}
	if _, ok := got[macHuawei]; !ok || len(got) != 1 {
		t.Fatalf("devices: %+v", got)
	}
	// the manual refresh does the same
	if err := e.svc.RefreshHosts(context.Background(), e.site); err != nil {
		t.Fatal(err)
	}
	if _, ok := unregByMAC(e)[macRouterPort]; ok {
		t.Fatal("refresh recorded the router's own MAC")
	}
	// only the router's own MACs are skipped: a local-only host row counts even with no /interface row
	e2 := newEnv(t)
	e2.ipoe(t, store.AccessIPoE)
	e2.bridgeHost("38:32:7A:00:00:01", "ether6", "true")
	e2.dynLease("38:32:7A:00:00:01", "10.5.50.22", "", "bound")
	e2.reconcile(t)
	if len(e2.svc.Unregistered("", false)) != 0 {
		t.Fatal("a local bridge host became a device")
	}
}

// A device that is on a customer port at layer 2 but never asked for an address is listed, with its port.
func TestBridgeOnlyDeviceIsListedAndOnline(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.ownIface("ether5", macRouterPort)
	e.bridgeHost(macRouterPort, "ether5", "true")
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.rt.SeedRow("/interface/bridge/host", routeros.Row{"mac-address": "00:15:EB:00:00:78", "on-interface": "ether2", "bridge": "br-lan", "local": "false"})
	e.reconcile(t)
	got := unregByMAC(e)
	d, ok := got[macThinkPad]
	if !ok {
		t.Fatalf("not listed: %+v", got)
	}
	if d.Source != "bridge" || d.Port != "ether5" || d.IP != "" || !d.Online {
		t.Fatalf("%+v", d)
	}
	if want := "On ether5, no address yet (it hasn't asked for one)"; d.AddrNote() != want {
		t.Fatalf("note %q", d.AddrNote())
	}
	if _, ok := got[macRouterPort]; ok {
		t.Fatal("own MAC listed")
	}
	if _, ok := got["00:15:EB:00:00:78"]; ok {
		t.Fatal("a host of another bridge was listed")
	}
	// while the entry is there it stays online; when it ages out of the table it goes offline
	e.clearTable("/interface/bridge/host")
	e.reconcile(t)
	if d := unregByMAC(e)[macThinkPad]; d.Online {
		t.Fatalf("still online with no bridge entry: %+v", d)
	}
}

// An address from DHCP or ARP replaces "no address yet", and a later bridge-only sighting does not undo it.
func TestBridgeDeviceMergesWithLeaseAndARPByMAC(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.reconcile(t)
	if d := unregByMAC(e)[macThinkPad]; d.Source != "bridge" || d.IP != "" {
		t.Fatalf("%+v", d)
	}
	e.dynLease(macThinkPad, "10.5.50.30", "thinkpad", "bound")
	e.reconcile(t)
	d := unregByMAC(e)[macThinkPad]
	if d.Source != "dhcp" || d.IP != "10.5.50.30" || d.HostName != "thinkpad" || d.Port != "ether5" || d.AddrNote() != "" {
		t.Fatalf("lease did not replace 'no address yet': %+v", d)
	}
	if n := len(e.svc.Unregistered("", false)); n != 1 {
		t.Fatalf("one MAC, %d rows", n)
	}
	e.clearTable("/ip/dhcp-server/lease")
	e.reconcile(t)
	d = unregByMAC(e)[macThinkPad]
	if d.Source != "dhcp" || d.IP != "10.5.50.30" || !d.Online {
		t.Fatalf("a layer-2 sighting undid the address: %+v", d)
	}
}

// A MAC that already has an account here is not a new device even if only the bridge sees it.
func TestBridgeHostOfAnAccountIsNotNew(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.bridgeHost(macA, "sfp-sfpplus1", "false")
	e.reconcile(t)
	if got := e.svc.Unregistered("", false); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// A failed bridge-host read must not flip bridge-only devices offline.
type hostFail struct{ *routeros.Mock }

func (h hostFail) RowsProps(ctx context.Context, p string, q map[string]string, props []string) ([]routeros.Row, error) {
	if p == pathBridgeHost {
		return nil, errors.New("boom")
	}
	return h.Mock.RowsProps(ctx, p, q, props)
}

func TestFailedBridgeHostReadKeepsDevicesAsTheyWere(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.bridgeHost(macThinkPad, "ether5", "false")
	e.reconcile(t)
	if !unregByMAC(e)[macThinkPad].Online {
		t.Fatal("setup")
	}
	e.svc.routers = recSource{map[string]routeros.Client{e.site: hostFail{e.rt}}}
	e.reconcile(t)
	if !unregByMAC(e)[macThinkPad].Online {
		t.Fatal("a failed read flipped the device offline")
	}
}

func TestAccountsCannotUseTheRoutersOwnMAC(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.ownIface("ether5", macRouterPort)
	e.reconcile(t) // the pass reads the router's interfaces
	a := e.flat(t, "Flat 1", macA)
	ctx := context.Background()
	if err := e.svc.ChangeMAC(ctx, a.ID, macRouterPort); err == nil || err.Error() != "That MAC belongs to the router itself" {
		t.Fatalf("ChangeMAC: %v", err)
	}
	if _, err := e.svc.AttachDevice(ctx, e.site, macRouterPort, a.ID); !errors.Is(err, ErrRouterOwnMAC) {
		t.Fatalf("AttachDevice: %v", err)
	}
	if _, err := e.svc.CreateFromDevice(ctx, DeviceAccountInput{SiteID: e.site, Unit: "B2", MAC: macRouterPort}); !errors.Is(err, ErrRouterOwnMAC) {
		t.Fatalf("CreateFromDevice: %v", err)
	}
	if got, _ := e.svc.GetAccount(a.ID); got.MAC != macA {
		t.Fatalf("MAC changed to %s", got.MAC)
	}
	if err := e.svc.ChangeMAC(ctx, a.ID, macHuawei); err != nil {
		t.Fatalf("an ordinary MAC was refused: %v", err)
	}
}

// The account's own MAC: vendor, online state, last seen and the port it sits on come from the pass.
func TestAccountDeviceHasVendorStateAndPort(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	const realtek = "00:E0:4C:68:01:02"
	a := e.flat(t, "Flat 1", realtek)
	if d, ok := e.svc.AccountDevice(a.ID); !ok || d.Vendor != "Realtek" || d.Online || d.Port != "" || !d.LastSeen.IsZero() {
		t.Fatalf("before any pass: %+v %v", d, ok)
	}
	e.bridgeHost(realtek, "ether3", "false")
	e.reconcile(t)
	d, ok := e.svc.AccountDevice(a.ID)
	if !ok || d.MAC != realtek || d.Vendor != "Realtek" || !d.Online || d.Port != "ether3" || d.LastSeen.IsZero() {
		t.Fatalf("after the pass: %+v", d)
	}
	if rs := e.svc.AccountRouterState(a.ID); rs.Port != "ether3" {
		t.Fatalf("router state port %q", rs.Port)
	}
	// the entry ages out of the bridge table: no port any more (the read worked)
	e.clearTable("/interface/bridge/host")
	e.reconcile(t)
	if d, _ := e.svc.AccountDevice(a.ID); d.Port != "" {
		t.Fatalf("stale port %q", d.Port)
	}
	// a failed bridge-host read keeps what was known
	e.bridgeHost(realtek, "ether4", "false")
	e.reconcile(t)
	e.svc.routers = recSource{map[string]routeros.Client{e.site: hostFail{e.rt}}}
	e.reconcile(t)
	if d, _ := e.svc.AccountDevice(a.ID); d.Port != "ether4" {
		t.Fatalf("a failed read dropped the port: %q", d.Port)
	}
	if _, ok := e.svc.AccountDevice("nope"); ok {
		t.Fatal("unknown account")
	}
}
