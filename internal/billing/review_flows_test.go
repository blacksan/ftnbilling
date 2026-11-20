package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"ispbill/internal/routeros"
)

// Review: the router already has a hand-made static lease (no ftn comment) for the flat's OWN MAC on the flat's
// address (WinBox "Make static" before ftnbilling managed the flat). The customer is online on that address and
// the queue and suspended entry follow it. The new readdress moves the account to another IP.
func TestReview_ReaddressOwnMACForeignLease(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	ip := e.get(a).IP
	e.payAt(t, a, "1 Day")
	e.reconcile(t)
	// replace our lease by a hand-made one for the same MAC and address (as WinBox "make static" would leave it)
	for _, r := range e.tbl("/ip/dhcp-server/lease") {
		_ = e.rt.RemoveRow(context.Background(), "/ip/dhcp-server/lease", r[routeros.RowID])
	}
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": strings.ToUpper(macA), "address": ip, "server": "ftn-dhcp", "comment": "Brian flat 1"})
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(2 * time.Minute)
		e.svc.Sweep(context.Background())
	}
	got := e.get(a).IP
	var qt []string
	for _, q := range e.tbl("/queue/simple") {
		qt = append(qt, q["target"])
	}
	t.Logf("account IP before %s after %s; leases %v; queue targets %v", ip, got, e.tbl("/ip/dhcp-server/lease"), qt)
	e.now = e.now.Add(48 * time.Hour) // the plan ends: the flat must be walled
	e.svc.Sweep(context.Background())
	var sus []string
	for _, r := range e.tbl("/ip/firewall/address-list") {
		sus = append(sus, r["list"]+"="+r["address"])
	}
	t.Logf("after expiry: address lists %v", sus)
	if got != ip {
		t.Errorf("a flat whose own MAC holds the foreign lease was re-addressed %s -> %s: the device keeps %s from the router's lease, the queue and suspended entry now point at %s", ip, got, ip, got)
	}
}

// A different device's static lease on the flat's address still moves the flat (the original intent).
func TestReview_ReaddressForeignDeviceLease(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	ip := e.get(a).IP
	for _, r := range e.tbl("/ip/dhcp-server/lease") {
		_ = e.rt.RemoveRow(context.Background(), "/ip/dhcp-server/lease", r[routeros.RowID])
	}
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "02:AA:BB:CC:DD:EE", "address": ip, "server": "ftn-dhcp", "comment": "printer"})
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(2 * time.Minute)
		e.svc.Sweep(context.Background())
	}
	if got := e.get(a).IP; got == ip {
		t.Errorf("a flat whose address belongs to another device's static lease was not moved off %s", ip)
	}
}

// L1: an admin action's wait for a pass that already runs on the building ends with its context, and the task
// stays queued without a failed attempt.
func TestReview_TaskLockWaitHonoursContext(t *testing.T) {
	e := newEnv(t)
	st := e.svc.state(e.site)
	if !st.taskMu.LockCtx(context.Background()) { // a sweep holding the building
		t.Fatal("lock")
	}
	defer st.taskMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	n := e.svc.RunSiteTasks(ctx, e.site)
	if n != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("RunSiteTasks waited %v (n=%d) behind a running pass instead of giving up with its context", time.Since(start), n)
	}
}
