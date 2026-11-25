package sites

import (
	"testing"
	"time"

	"ispbill/internal/billing"
)

// A device the operator dismissed has no row in the device list, but its bound lease still shows on the router:
// the live client list must not bring it back as a new device (Add-customer picker, Network tab).
func TestCustomerFlow_DismissedDeviceStaysOutOfClients(t *testing.T) {
	now := time.Now()
	in := ClientInput{Now: now, Dismissed: []string{"AA:BB:CC:00:00:01"},
		Tables: LiveTables{Leases: []LiveLease{{MAC: "AA:BB:CC:00:00:01", IP: "10.5.50.20", Status: "bound"}, {MAC: "AA:BB:CC:00:00:02", IP: "10.5.50.21", Status: "bound"}}}}
	var macs []string
	for _, c := range MergeClients(in) {
		if c.Kind == "unregistered" {
			macs = append(macs, c.MAC)
		}
	}
	if len(macs) != 1 || macs[0] != "AA:BB:CC:00:00:02" {
		t.Fatalf("unregistered clients %v: the dismissed device is back, or the other one vanished", macs)
	}
}

// Equipment (an OLT) is not a customer: it never shows as "Not registered" with Create customer, whether it came
// from the device list or from a bound lease.
func TestClients_EquipmentIsNotAClient(t *testing.T) {
	now := time.Now()
	olt, mgmt := "48:21:B8:10:D7:72", "68:22:9F:12:7E:11"
	classify := func(m string) billing.DeviceClass {
		if m == olt {
			return billing.DeviceClass{Class: billing.ClassOLT}
		}
		if m == mgmt { // an ONU's TR-069 WAN on the management network
			return billing.DeviceClass{Class: billing.ClassONUMgmt}
		}
		return billing.DeviceClass{Class: billing.ClassUnknown, Creatable: true}
	}
	in := ClientInput{Now: now, Classify: classify,
		Devices: []ClientDevice{{MAC: olt, Online: true, LastSeen: now}, {MAC: mgmt, Online: true, LastSeen: now}, {MAC: "AA:BB:CC:00:00:02", Online: true, LastSeen: now}},
		Tables:  LiveTables{Leases: []LiveLease{{MAC: olt, IP: "10.5.50.2", Status: "bound"}, {MAC: mgmt, IP: "10.6.1.250", Status: "bound"}}}}
	var macs []string
	for _, c := range MergeClients(in) {
		macs = append(macs, c.MAC)
	}
	if len(macs) != 1 || macs[0] != "AA:BB:CC:00:00:02" {
		t.Fatalf("clients %v: the OLT is listed or the phone vanished", macs)
	}
}

// The WAN MAC of an installed ONU is the customer's account MAC: the house is listed, never hidden as equipment. An
// ONU of the register with no account here that is not in stock is listed (to attach or dismiss) without Create
// customer; an in-stock one carries its register item so Create customer can give it to the house.
func TestClients_ONUIsNotEquipment(t *testing.T) {
	now := time.Now()
	house, returned, stock := "68:22:9F:12:7E:10", "68:22:9F:12:7E:20", "68:22:9F:12:7E:30"
	classify := func(m string) billing.DeviceClass {
		switch m {
		case house, returned:
			return billing.DeviceClass{Class: billing.ClassONU, Label: "ONU F670L · ZTEG1", EquipmentID: "e-" + m}
		case stock:
			return billing.DeviceClass{Class: billing.ClassONU, Label: "ONU F670L · ZTEG3", EquipmentID: "e3", Creatable: true}
		}
		return billing.DeviceClass{Class: billing.ClassUnknown, Creatable: true}
	}
	in := ClientInput{Now: now, Classify: classify,
		Accounts: []ClientAccount{{ID: "a1", Code: "QX7KP2", Flat: "House 707", MAC: house, IP: "10.5.50.60"}},
		Devices:  []ClientDevice{{MAC: returned, Online: true, LastSeen: now}, {MAC: stock, Online: true, LastSeen: now}},
		Tables:   LiveTables{Leases: []LiveLease{{MAC: house, IP: "10.5.50.60", Status: "bound"}}}}
	got := map[string]Client{}
	for _, c := range MergeClients(in) {
		got[c.MAC] = c
	}
	if c := got[house]; c.Kind != "flat" || c.Code != "QX7KP2" {
		t.Fatalf("the house on its ONU's WAN MAC is not listed: %+v", got)
	}
	if c := got[returned]; c.Kind != "unregistered" || !c.NotCreatable || c.Equipment == "" {
		t.Fatalf("a returned ONU: %+v", c)
	}
	if c := got[stock]; c.Kind != "unregistered" || c.NotCreatable || c.EquipmentID != "e3" {
		t.Fatalf("an in-stock ONU: %+v", c)
	}
	if len(got) != 3 {
		t.Fatalf("clients %+v", got)
	}
}
