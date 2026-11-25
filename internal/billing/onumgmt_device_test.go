package billing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

const (
	macONUWAN  = "68:22:9F:12:7E:10" // the register's WAN MAC of the V-SOL
	macONUMgmt = "68:22:9F:12:7E:11" // its TR-069 WAN on the management VLAN
)

func (e *env) mgmtLease(mac, ip string) {
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": mac, "address": ip, "active-address": ip, "status": "bound", "server": onuMgmtDHCPServer, "dynamic": "true"})
}

func TestONUManagementMACIsEquipmentNotACustomerDevice(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: "ont", Serial: "V26051300697", GPONSerial: "VSOL00127E0F", MAC: macONUWAN, Model: "V2804N-Z"}); err != nil {
		t.Fatal(err)
	}
	e.mgmtLease(macONUMgmt, "10.6.1.250")
	e.dynLease(macHuawei, "10.5.50.11", "HG8010H", "bound") // a customer device on the customer network is still a device
	e.reconcile(t)
	got := unregByMAC(e)
	c := got[macONUMgmt].Class
	if c.Class != ClassONUMgmt || c.Creatable || !c.IsEquipment() || !strings.HasPrefix(c.Label, "ONU management (TR-069) — ") || !strings.Contains(c.Label, "V26051300697") || c.EquipmentID == "" {
		t.Fatalf("class %+v", c)
	}
	if got[macHuawei].Class.Class != ClassUnknown || !got[macHuawei].Class.Creatable {
		t.Fatalf("a customer device was reclassified: %+v", got[macHuawei].Class)
	}
	if n := e.svc.UnregisteredOnline(nil); n != 1 {
		t.Fatalf("the management MAC counts as a new device: %d", n)
	}
	// never a customer: create and attach refuse it
	if _, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "P1", MAC: macONUMgmt}); !errors.Is(err, ErrONUMgmtMAC) {
		t.Fatalf("create accepted it: %v", err)
	}
	// the sticky class survives a pass that only sees the MAC at layer 2 (no lease any more)
	e.clearTable("/ip/dhcp-server/lease")
	e.dynLease(macHuawei, "10.5.50.11", "HG8010H", "bound")
	e.reconcile(t)
	if unregByMAC(e)[macONUMgmt].Class.Class != ClassONUMgmt {
		t.Fatal("the class was lost")
	}
}

func TestONUManagementWithoutARegisteredONUIsUnknownONU(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.mgmtLease(macONUMgmt, "10.6.1.250")
	e.reconcile(t)
	c := unregByMAC(e)[macONUMgmt].Class
	if c.Class != ClassONUMgmt || c.Label != "ONU management (TR-069) — unknown ONU" || c.EquipmentID != "" {
		t.Fatalf("%+v", c)
	}
}

func TestONUManagementSeenOnItsVLANInterfaceOnly(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.rt.SeedRow("/interface/bridge/host", routeros.Row{"mac-address": macONUMgmt, "on-interface": "vlan99-onu", "bridge": CustomerBridgeName, "local": "false"})
	e.reconcile(t)
	if c := unregByMAC(e)[macONUMgmt].Class; c.Class != ClassONUMgmt {
		t.Fatalf("%+v", c)
	}
}

func TestAnAccountsMACIsNeverReclassifiedAsONUManagement(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	apt := e.apt(t, "MG One", e.site)
	a, err := e.svc.CreateAccount(AccountInput{ApartmentID: apt.ID, SiteID: e.site, Unit: "P1", MAC: macONUMgmt})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = e.svc.st.Update(func(d *store.Data) error {
		d.Devices[store.DeviceKey(e.site, macONUMgmt)] = store.Device{SiteID: e.site, MAC: macONUMgmt, Source: store.DeviceSourceONUMgmt, Online: true, FirstSeen: now, LastSeen: now}
		return nil
	})
	if c := e.svc.ClassifyMAC(e.site, macONUMgmt); c.Class == ClassONUMgmt {
		t.Fatalf("account %s lost its MAC to the management class", a.Code)
	}
	// and a lease of the management server for an account's MAC does not mark it as management either
	e.mgmtLease(macONUMgmt, "10.6.1.250")
	e.reconcile(t)
	if _, ok := unregByMAC(e)[macONUMgmt]; ok {
		t.Fatal("an account's MAC is listed as a device")
	}
}
