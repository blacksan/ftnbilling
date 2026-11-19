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

// B4: with ftn-dhcp disabled (not switched yet, or rolled back) the reconciler
// writes nothing and leaves the dynamic leases alone.
func TestIPoEWritesNothingWhileFtnDHCPIsDisabled(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "address": "10.5.50.60", "status": "bound", "dynamic": "true", "server": "dhcp-other"})
	for _, r := range e.rt.Table("/ip/dhcp-server") {
		e.rt.RemoveRow(context.Background(), "/ip/dhcp-server", r[".id"])
	}
	e.rt.ResetWrites()
	run, err := e.svc.ReconcileSite(context.Background(), e.site)
	if !errors.Is(err, ErrNotSetUp) || !run.Waiting || e.rt.Writes() != 0 {
		t.Fatalf("%+v err=%v writes=%d", run, err, e.rt.Writes())
	}
	if got := rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "server", "dhcp-other"); len(got) != 1 {
		t.Fatal("the other server's dynamic lease was removed")
	}
	if ps := e.svc.ProvisionStatus(e.site); ps.InSync {
		t.Fatalf("never seen enabled: waiting, not rolled back: %+v", ps)
	}
}

// Only dynamic leases on ftn-dhcp are removed to make room for a static one.
func TestIPoEOnlyRemovesDynamicLeasesOfFtnDHCP(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "address": "10.5.50.70", "status": "bound", "dynamic": "true", "server": "dhcp-other"})
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macB2, "address": "10.5.50.71", "status": "bound", "dynamic": "true", "server": DHCPServerName})
	e.flat(t, "Flat 1", macA)
	e.flat(t, "Flat 2", macB2)
	e.reconcile(t)
	dyn := e.rt.Table("/ip/dhcp-server/lease")
	if len(rowsBy(dyn, "server", "dhcp-other")) != 1 {
		t.Fatalf("foreign lease touched: %v", dyn)
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

// Static ARP rows follow the flats, so an interface on reply-only still reaches them.
func TestIPoEManagesStaticARP(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 1", macA)
	e.reconcile(t)
	r := find(t, e.rt.Table("/ip/arp"), "address", a.IP)
	if r["mac-address"] != macA || r["interface"] != "br-customers" || r["comment"] != AcctComment(a.Code, a.Flat) {
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

// Static leases are written with a one-hour lease time (the customer DHCP
// server's own lease time is a minute, for the unregistered devices).
func TestIPoEStaticLeasesGetOneHourLeaseTime(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	e.reconcile(t)
	var n int
	for _, r := range e.rt.Table("/ip/dhcp-server/lease") {
		if r["mac-address"] == macA {
			n++
			if r["lease-time"] != "1h" {
				t.Fatalf("lease-time %q", r["lease-time"])
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d leases", n)
	}
}

// A building whose router fails keeps its tasks queued with a backoff, and does
// not stall the other buildings' reconcile.
func TestFailingSiteBacksOffAndDoesNotStallOthers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bID, brt := e.addSite(t, "Block B")
	a := e.flat(t, "Flat 1", mac1)
	b := e.flatAt(t, bID, "Flat 1", macB)
	e.rt.SetFail(errors.New("router down"))
	e.payAt(t, a, "1 Day")
	e.payAt(t, b, "1 Day")
	e.svc.RunTasks(ctx)
	if len(rowsBy(brt.Table("/queue/simple"), "name", QueueName(b.Code))) != 1 {
		t.Fatal("the healthy building was not reconciled")
	}
	var pending []store.Task
	for _, tk := range e.svc.PendingTasks() {
		if tk.SiteID == e.site {
			pending = append(pending, tk)
		}
	}
	if len(pending) != 1 || pending[0].Attempts < 1 || pending[0].LastErr == "" || !pending[0].NextTry.After(e.now) {
		t.Fatalf("A's task must wait with a backoff: %+v", pending)
	}
	for _, tk := range e.svc.PendingTasks() {
		if tk.SiteID == bID {
			t.Fatalf("B's task must be done: %+v", tk)
		}
	}
	// Before the backoff elapses nothing is retried; after it, A converges.
	e.rt.SetFail(nil)
	if n := e.svc.RunTasks(ctx); n != 0 {
		t.Fatalf("retried inside the backoff: %d", n)
	}
	e.now = e.now.Add(backoffMax + time.Second)
	if n := e.svc.RunTasks(ctx); n != 1 {
		t.Fatalf("A not retried after the backoff: %d", n)
	}
	if len(rowsBy(e.rt.Table("/queue/simple"), "name", QueueName(a.Code))) != 1 || len(e.svc.PendingTasks()) != 0 {
		t.Fatal("A did not converge")
	}
}

// A paused building keeps its tasks queued and spends no attempts; resuming
// runs them.
func TestPausedSiteKeepsTasksQueuedWithoutAttempts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.flat(t, "Flat 1", mac1)
	e.rt.SetFail(errors.New("router down"))
	e.payAt(t, a, "1 Day")
	e.rt.SetFail(nil)
	before := e.svc.PendingTasks()
	if len(before) != 1 {
		t.Fatalf("%+v", before)
	}
	e.st.Update(func(d *store.Data) error {
		s := d.Sites[e.site]
		s.Disabled = true
		d.Sites[e.site] = s
		return nil
	})
	e.now = e.now.Add(backoffMax + time.Second)
	e.rt.ResetWrites()
	if n := e.svc.RunTasks(ctx); n != 0 || e.rt.Writes() != 0 {
		t.Fatalf("a paused building must not be touched: %d, %d writes", n, e.rt.Writes())
	}
	after := e.svc.PendingTasks()
	if len(after) != 1 || after[0].Attempts != before[0].Attempts {
		t.Fatalf("a paused building spends no attempts: %+v -> %+v", before, after)
	}
	e.st.Update(func(d *store.Data) error {
		s := d.Sites[e.site]
		s.Disabled = false
		d.Sites[e.site] = s
		return nil
	})
	if n := e.svc.RunTasks(ctx); n != 1 || len(e.svc.PendingTasks()) != 0 {
		t.Fatalf("resumed building must catch up: %d", n)
	}
}

// The gate is the v5 layout marker (the customer bridge the script makes): a router with an ftn-dhcp server but
// no such bridge (an older build) gets no writes, the reconcile says so with ErrNotSetUp, and the queued tasks
// stay queued instead of being deleted as done.
func TestIPoEWritesNothingWithoutTheV5LayoutAndKeepsTasksQueued(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.flat(t, "Flat 1", macA)
	for _, r := range e.rt.Table("/interface/bridge") {
		e.rt.RemoveRow(context.Background(), "/interface/bridge", r[".id"])
	}
	// a bridge of the same name without the script's comment is not the marker either
	e.rt.SeedRow("/interface/bridge", routeros.Row{"name": CustomerBridgeName, "comment": "made by hand"})
	e.rt.ResetWrites()
	e.svc.EnqueueSite(e.site, "test")
	queued := func() int {
		n := 0
		e.st.View(func(d *store.Data) {
			for _, tk := range d.TaskList() {
				if tk.SiteID == e.site {
					n++
				}
			}
		})
		return n
	}
	before := queued()
	if before == 0 {
		t.Fatal("nothing was queued")
	}
	run, err := e.svc.ReconcileSite(context.Background(), e.site)
	if !errors.Is(err, ErrNotSetUp) || !run.Waiting {
		t.Fatalf("run %+v err %v", run, err)
	}
	e.svc.RunSiteTasks(context.Background(), e.site)
	if e.rt.Writes() != 0 {
		t.Fatalf("%d writes to a router without the v5 layout", e.rt.Writes())
	}
	if queued() != before {
		t.Fatalf("tasks were deleted for a router that was never written: %d of %d left", queued(), before)
	}
}
