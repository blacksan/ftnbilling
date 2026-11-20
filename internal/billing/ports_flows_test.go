package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// F4-8: the ARP table is read "best effort"; when that read fails the pass still marks every device that was
// seen only through ARP as offline, and when ARP is back it counts as a new "hit".
func TestPortsFlow_ARPReadFailureDoesNotFlapDevices(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.rt.SeedRow("/ip/arp", routeros.Row{"mac-address": macNew, "address": "10.5.50.13", "interface": "br-customers", "status": "reachable"})
	e.reconcile(t)
	if got := e.svc.Unregistered("", true); len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	e.rt.SetReadFail("/ip/arp", errors.New("timeout"))
	e.now = e.now.Add(time.Minute)
	e.reconcile(t)
	online := len(e.svc.Unregistered("", true))
	e.rt.SetReadFail("/ip/arp", nil)
	e.now = e.now.Add(time.Minute)
	e.reconcile(t)
	got := e.svc.Unregistered("", false)
	if online == 0 || got[0].Hits != 1 {
		t.Fatalf("a device that never left was shown offline or counted a new hit (Hits=%d) because one ARP read failed", got[0].Hits)
	}
}

// F4-9: the device table is capped at 500 rows for ALL buildings together and the stalest row is dropped:
// a dismissed device (or a quiet building's list) is evicted by a noisy building and comes back as new.
func TestPortsFlow_DeviceCapKeepsDismissed(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.dynLease(macHuawei, "10.5.50.11", "ont", "bound")
	e.reconcile(t)
	if err := e.svc.DismissDevice(e.site, macHuawei); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") { // the dismissed device is switched off
		e.rt.RemoveRow(context.Background(), "/ip/dhcp-server/lease", r[".id"])
	}
	// a noisy network: 520 other MACs seen over the next passes
	for i := 0; i < 520; i++ {
		mac := fmt.Sprintf("00:15:EB:%02X:%02X:%02X", i>>16, (i>>8)&255, i&255)
		e.dynLease(mac, fmt.Sprintf("10.5.50.%d", 20+i%200), "", "bound")
	}
	e.now = e.now.Add(time.Minute)
	e.reconcile(t)
	e.now = e.now.Add(time.Minute)
	e.reconcile(t)
	n := 0
	e.svc.st.View(func(d *store.Data) { n = len(d.Devices) })
	var found bool
	e.svc.st.View(func(d *store.Data) { _, found = d.Devices[store.DeviceKey(e.site, macHuawei)] })
	if n > maxDevicesPerSite {
		t.Fatalf("%d devices kept", n)
	}
	if !found {
		t.Fatalf("the dismissed device was evicted by the 500-row global cap; it will be listed again")
	}
}

// F4-11: RunSweeper does Sweep() (which waits for every building's hook, up to 90 s each) in the same loop as the
// 10-second RunTasks retry, so one slow/dead router stops the task retries of every other building.
func TestPortsFlow_SweepDoesNotBlockTaskRetries(t *testing.T) { f4SweepRun(t, 14*time.Second) }

// control: a hook that returns at once
func TestPortsFlow_SweepControlNoSlowHook(t *testing.T) { f4SweepRun(t, 0) }

func f4SweepRun(t *testing.T, hookTime time.Duration) {
	e := newEnv(t)
	e.svc.SetSweepHook(func(ctx context.Context, siteID string) { time.Sleep(hookTime) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.svc.RunSweeper(ctx, time.Hour)
	time.Sleep(500 * time.Millisecond) // first Sweep is inside the hook now
	e.flat(t, "Flat 1", macA)          // queues a due router task
	start := time.Now()
	for time.Since(start) < 12*time.Second {
		time.Sleep(250 * time.Millisecond)
		if len(e.svc.PendingTasks()) == 0 {
			t.Logf("task done after %v", time.Since(start).Round(time.Second))
			return
		}
	}
	t.Fatalf("a due task of the building was not retried within 12 s: RunTasks (every 10 s) is starved while Sweep waits for one slow hook")
}
