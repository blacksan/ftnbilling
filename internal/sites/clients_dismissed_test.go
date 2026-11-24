package sites

import (
	"testing"
	"time"
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
	olt := "48:21:B8:10:D7:72"
	in := ClientInput{Now: now, IsEquipment: func(m string) bool { return m == olt },
		Devices: []ClientDevice{{MAC: olt, Online: true, LastSeen: now}, {MAC: "AA:BB:CC:00:00:02", Online: true, LastSeen: now}},
		Tables:  LiveTables{Leases: []LiveLease{{MAC: olt, IP: "10.5.50.2", Status: "bound"}}}}
	var macs []string
	for _, c := range MergeClients(in) {
		macs = append(macs, c.MAC)
	}
	if len(macs) != 1 || macs[0] != "AA:BB:CC:00:00:02" {
		t.Fatalf("clients %v: the OLT is listed or the phone vanished", macs)
	}
}
