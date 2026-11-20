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
