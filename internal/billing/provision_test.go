package billing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// recorder is a router that logs every row write, in order.
type recorder struct {
	*routeros.Mock
	mu  sync.Mutex
	log []string
}

func (r *recorder) note(kind, path, what string) {
	r.mu.Lock()
	r.log = append(r.log, kind+" "+path+" "+what)
	r.mu.Unlock()
}
func (r *recorder) AddRow(ctx context.Context, path string, body map[string]string) (string, error) {
	r.note("add", path, body["comment"])
	return r.Mock.AddRow(ctx, path, body)
}
func (r *recorder) SetRow(ctx context.Context, path, id string, diff map[string]string) error {
	r.note("set", path, id)
	return r.Mock.SetRow(ctx, path, id, diff)
}
func (r *recorder) RemoveRow(ctx context.Context, path, id string) error {
	r.note("rm", path, id)
	return r.Mock.RemoveRow(ctx, path, id)
}
func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.log
	r.log = nil
	return out
}

type recSource struct{ rt map[string]routeros.Client }

func (s recSource) Router(id string) routeros.Client {
	if c, ok := s.rt[id]; ok {
		return c
	}
	return routeros.NoRouter()
}

const (
	macA  = "AA:BB:CC:DD:EE:A1"
	macB2 = "AA:BB:CC:DD:EE:A2"
	macC  = "AA:BB:CC:DD:EE:A3"
)

// ipoe is kept so the tests read the same: every building is IPoE and addSite
// already gave its router the ftn-dhcp server.
func (e *env) ipoe(t *testing.T, _ string) { t.Helper() }

func (e *env) reconcile(t *testing.T) ProvisionRun {
	t.Helper()
	run, err := e.svc.ReconcileSite(context.Background(), e.site)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return run
}

func (e *env) pass(t *testing.T) int {
	t.Helper()
	e.rt.ResetWrites()
	e.reconcile(t)
	return e.rt.Writes()
}

func rowsBy(rows []routeros.Row, key, val string) (out []routeros.Row) {
	for _, r := range rows {
		if r[key] == val {
			out = append(out, r)
		}
	}
	return
}

func find(t *testing.T, rows []routeros.Row, key, val string) routeros.Row {
	t.Helper()
	got := rowsBy(rows, key, val)
	if len(got) != 1 {
		t.Fatalf("want one row with %s=%s, got %d: %v", key, val, len(got), rows)
	}
	return got[0]
}

func TestIPoEConvergesFromEmptyRouterAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a1 := e.flat(t, "Flat 1", macA)
	a2 := e.flat(t, "Flat 2", macB2)
	e.flat(t, "Flat 3", macC)
	e.payAt(t, a1, "1 Day")
	_ = a2
	// A fresh account already has its fixed address (static range starts at .60).
	if got := e.get(a1).IP; got != "10.5.50.60" {
		t.Fatalf("first flat address %q", got)
	}
	if got := e.get(a2).IP; got != "10.5.50.61" {
		t.Fatalf("second flat address %q", got)
	}
	run := e.reconcile(t)
	if run.Failed != 0 || run.Waiting || len(run.Conflicts) != 0 {
		t.Fatalf("%+v", run)
	}
	leases := e.rt.Table("/ip/dhcp-server/lease")
	queues := e.rt.Table("/queue/simple")
	list := e.rt.Table("/ip/firewall/address-list")
	if len(leases) != 3 || len(queues) != 3 {
		t.Fatalf("leases %v queues %v", leases, queues)
	}
	l1 := find(t, leases, "mac-address", macA)
	if l1["address"] != "10.5.50.60" || l1["server"] != DHCPServerName || !strings.HasPrefix(l1["comment"], "ftn:acct:"+a1.Code) {
		t.Fatalf("%v", l1)
	}
	q1 := find(t, queues, "name", "ftn-"+a1.Code)
	pk := e.pkg("1 Day")
	if q1["target"] != "10.5.50.60/32" || q1["max-limit"] != pk.RateLimit() {
		t.Fatalf("%v want %s", q1, pk.RateLimit())
	}
	// Only the two unpaid flats are suspended; the paid one is not.
	if len(list) != 2 {
		t.Fatalf("suspended list %v", list)
	}
	for _, r := range list {
		if r["list"] != SuspendedList || r["address"] == "10.5.50.60" {
			t.Fatalf("%v", r)
		}
	}
	// Every object we created carries an ftn: comment.
	for _, tbl := range [][]routeros.Row{leases, queues, list} {
		for _, r := range tbl {
			if !strings.HasPrefix(r["comment"], "ftn:") {
				t.Fatalf("row without an ftn: comment: %v", r)
			}
		}
	}
	// A second pass over an unchanged router writes nothing.
	if n := e.pass(t); n != 0 {
		t.Fatalf("second pass wrote %d rows", n)
	}
	ps := e.svc.ProvisionStatus(e.site)
	if !ps.InSync || ps.Drift != 0 || ps.Mode != store.AccessIPoE || ps.LastRunAt.IsZero() {
		t.Fatalf("%+v", ps)
	}
	if st := e.svc.AccountRouterState(a1.ID); !st.InSync || st.LeaseStatus != "waiting" || st.SuspendedOnRouter {
		t.Fatalf("%+v", st)
	}
	if st := e.svc.AccountRouterState(a2.ID); !st.SuspendedOnRouter {
		t.Fatalf("%+v", st)
	}
}

func TestIPoEDriftRepair(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a1 := e.flat(t, "Flat 1", macA)
	a2 := e.flat(t, "Flat 2", macB2)
	e.payAt(t, a1, "1 Day")
	e.payAt(t, a2, "1 Day")
	e.reconcile(t)
	ctx := context.Background()
	// delete a lease, edit a queue, drop a suspended entry's owner, add stray ftn rows.
	l2 := find(t, e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macB2)
	e.rt.RemoveRow(ctx, "/ip/dhcp-server/lease", l2[".id"])
	q1 := find(t, e.rt.Table("/queue/simple"), "name", "ftn-"+a1.Code)
	e.rt.SetRow(ctx, "/queue/simple", q1[".id"], map[string]string{"max-limit": "1M/1M", "target": "10.5.50.99/32"})
	e.rt.AddRow(ctx, "/ip/dhcp-server/lease", map[string]string{"mac-address": "AA:BB:CC:00:00:99", "address": "10.5.50.90", "server": DHCPServerName, "comment": "ftn:acct:GONE99 Flat 9"})
	e.rt.AddRow(ctx, "/queue/simple", map[string]string{"name": "ftn-GONE99", "target": "10.5.50.90/32", "comment": "ftn:acct:GONE99 Flat 9"})
	e.rt.AddRow(ctx, "/ip/firewall/address-list", map[string]string{"list": SuspendedList, "address": "10.5.50.90", "comment": "ftn:acct:GONE99 Flat 9"})
	e.rt.AddRow(ctx, "/ip/firewall/address-list", map[string]string{"list": SuspendedList, "address": e.get(a1).IP, "comment": "ftn:acct:" + a1.Code + " Flat 1"}) // paid flat wrongly suspended
	e.rt.ResetWrites()
	run := e.reconcile(t)
	if run.Failed != 0 {
		t.Fatalf("%+v", run)
	}
	leases := e.rt.Table("/ip/dhcp-server/lease")
	if len(leases) != 2 || len(rowsBy(leases, "mac-address", macB2)) != 1 || len(rowsBy(leases, "mac-address", "AA:BB:CC:00:00:99")) != 0 {
		t.Fatalf("leases not repaired: %v", leases)
	}
	if q := find(t, e.rt.Table("/queue/simple"), "name", "ftn-"+a1.Code); q["max-limit"] != e.pkg("1 Day").RateLimit() || q["target"] != "10.5.50.60/32" {
		t.Fatalf("queue not repaired: %v", q)
	}
	if len(e.rt.Table("/queue/simple")) != 2 || len(e.rt.Table("/ip/firewall/address-list")) != 0 {
		t.Fatalf("strays left: %v %v", e.rt.Table("/queue/simple"), e.rt.Table("/ip/firewall/address-list"))
	}
	if n := e.pass(t); n != 0 {
		t.Fatalf("not idempotent after repair: %d", n)
	}
}

func TestIPoEForeignRowsAreReportedAndNeverTouched(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	_ = e.flat(t, "Flat 1", macA)
	a2 := e.flat(t, "Flat 2", macB2)
	// Someone's own static lease on flat 1's MAC, a queue named like flat 2's, an unrelated row, and a foreign suspended entry.
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "address": "10.5.50.200", "server": "other", "comment": "manual"})
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "00:11:22:33:44:55", "address": "10.5.50.201", "comment": "printer"})
	e.rt.SeedRow("/queue/simple", routeros.Row{"name": "ftn-" + a2.Code, "target": "10.5.50.77/32", "max-limit": "3M/3M", "comment": "hand made"})
	e.rt.SeedRow("/queue/simple", routeros.Row{"name": "guests", "target": "10.5.50.0/24", "max-limit": "1M/1M"})
	e.rt.ResetWrites()
	run := e.reconcile(t)
	if len(run.Conflicts) < 2 {
		t.Fatalf("conflicts %v", run.Conflicts)
	}
	for _, c := range run.Conflicts {
		if !strings.Contains(c, "left alone") {
			t.Fatalf("%s", c)
		}
	}
	// Foreign rows are exactly as they were.
	if r := find(t, e.rt.Table("/ip/dhcp-server/lease"), "comment", "manual"); r["address"] != "10.5.50.200" || r["server"] != "other" {
		t.Fatalf("%v", r)
	}
	if r := find(t, e.rt.Table("/queue/simple"), "comment", "hand made"); r["max-limit"] != "3M/3M" || r["target"] != "10.5.50.77/32" {
		t.Fatalf("%v", r)
	}
	if len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "comment", "printer")) != 1 || len(rowsBy(e.rt.Table("/queue/simple"), "name", "guests")) != 1 {
		t.Fatal("unrelated rows must stay")
	}
	// No lease of ours was written for the clashing flat, and nothing of ours was removed.
	if n := len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)); n != 1 {
		t.Fatalf("flat 1 has %d leases on its MAC", n)
	}
	ps := e.svc.ProvisionStatus(e.site)
	if ps.InSync || len(ps.ForeignConflicts) < 2 || ps.Drift < 2 {
		t.Fatalf("%+v", ps)
	}
	if n := e.pass(t); n != 0 {
		t.Fatalf("a standing conflict must not cause writes: %d", n)
	}
}

func TestIPoEUnsuspendFirstAndSuspendLast(t *testing.T) {
	e := newEnv(t)
	rec := &recorder{Mock: e.rt}
	e.svc.routers = recSource{map[string]routeros.Client{e.site: rec}}
	e.ipoe(t, store.AccessIPoE)
	a1 := e.flat(t, "Flat 1", macA)
	a2 := e.flat(t, "Flat 2", macB2)
	e.payAt(t, a2, "1 Day")
	e.reconcile(t) // flat 1 suspended, flat 2 not
	rec.take()
	// Flat 2's plan ends by the clock; flat 1 pays. The one pass that follows
	// must write the un-suspend before the suspend.
	e.now = e.now.Add(48 * time.Hour)
	e.payAt(t, a1, "1 Week")
	log := rec.take()
	iRm, iAdd := -1, -1
	for i, l := range log {
		if strings.HasPrefix(l, "rm /ip/firewall/address-list") && iRm < 0 {
			iRm = i
		}
		if strings.HasPrefix(l, "add /ip/firewall/address-list") {
			iAdd = i
		}
	}
	if iRm < 0 || iAdd < 0 || iRm > iAdd {
		t.Fatalf("un-suspend must come before suspend: %v", log)
	}
	// Every un-suspend is before every other write except removals; suspends are last.
	if !strings.HasPrefix(log[0], "rm /ip/firewall/address-list") || !strings.HasPrefix(log[len(log)-1], "add /ip/firewall/address-list") {
		t.Fatalf("order: %v", log)
	}
}

func TestIPoEConflictFlaggedAccountsAreSkipped(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a1 := e.flat(t, "Flat 1", macA)
	a2 := e.flat(t, "Flat 2", macB2)
	e.flag(t, a2.ID, macA) // flat 2 duplicates flat 1's MAC (as a migration would find it)
	run := e.reconcile(t)
	if len(run.Conflicts) != 1 || !strings.Contains(run.Conflicts[0], "Flat 2") {
		t.Fatalf("%v", run.Conflicts)
	}
	if got := e.rt.Table("/ip/dhcp-server/lease"); len(got) != 1 || got[0]["mac-address"] != macA {
		t.Fatalf("%v", got)
	}
	if got := e.rt.Table("/queue/simple"); len(got) != 1 || got[0]["name"] != "ftn-"+a1.Code {
		t.Fatalf("%v", got)
	}
	if ps := e.svc.ProvisionStatus(e.site); ps.InSync || len(ps.ForeignConflicts) != 1 {
		t.Fatalf("%+v", ps)
	}
}

func TestIPoEPaymentUnsuspendsAndFailureBacksOff(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	a := e.flat(t, "Flat 1", macA)
	e.reconcile(t)
	if len(e.rt.Table("/ip/firewall/address-list")) != 1 {
		t.Fatal("unpaid flat must be suspended")
	}
	// The router is down when the payment arrives: the payment is recorded, the task stays queued with backoff.
	e.rt.SetFail(errors.New("tunnel down"))
	e.payAt(t, a, "1 Day")
	if e.get(a).ExpiresAt.IsZero() {
		t.Fatal("payment must be applied even though the router is down")
	}
	tasks := e.svc.PendingTasks()
	if len(tasks) != 1 || tasks[0].Attempts == 0 || tasks[0].LastErr == "" || !tasks[0].NextTry.After(e.now) {
		t.Fatalf("task not backed off: %+v", tasks)
	}
	if st := e.svc.AccountRouterState(a.ID); st.InSync || !st.PendingTask {
		t.Fatalf("%+v", st)
	}
	if ps := e.svc.ProvisionStatus(e.site); ps.InSync || ps.LastError == "" || ps.PendingOps != 1 {
		t.Fatalf("%+v", ps)
	}
	// Not due yet: nothing is attempted.
	if n := e.svc.RunSiteTasks(context.Background(), e.site); n != 0 {
		t.Fatalf("ran %d before the backoff ended", n)
	}
	// Router back, backoff over: one task run converges the building.
	e.rt.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	if n := e.svc.RunSiteTasks(context.Background(), e.site); n != 1 {
		t.Fatalf("ran %d", n)
	}
	if len(e.rt.Table("/ip/firewall/address-list")) != 0 {
		t.Fatalf("flat still suspended after payment: %v", e.rt.Table("/ip/firewall/address-list"))
	}
	if len(e.svc.PendingTasks()) != 0 {
		t.Fatal("task must be gone")
	}
	if st := e.svc.AccountRouterState(a.ID); !st.InSync || st.SuspendedOnRouter {
		t.Fatalf("%+v", st)
	}
}

func TestIPoEEventsReachTheRouterInOneTaskRun(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	ctx := context.Background()
	a := e.flat(t, "Flat 1", macA)
	b := e.flat(t, "Flat 2", macB2)
	e.payAt(t, a, "1 Day")
	e.payAt(t, b, "1 Day")
	e.reconcile(t)
	if len(e.rt.Table("/ip/firewall/address-list")) != 0 {
		t.Fatal("both paid")
	}
	// suspend
	if err := e.svc.SetSuspended(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := e.rt.Table("/ip/firewall/address-list"); len(got) != 1 || got[0]["address"] != e.get(a).IP {
		t.Fatalf("suspend: %v", got)
	}
	if err := e.svc.SetSuspended(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(e.rt.Table("/ip/firewall/address-list")) != 0 {
		t.Fatal("unsuspend")
	}
	// package change: the queue follows
	pk := e.pkg("1 Week")
	if err := e.svc.ChangePackage(ctx, b.ID, pk.ID, true); err != nil {
		t.Fatal(err)
	}
	if q := find(t, e.rt.Table("/queue/simple"), "name", "ftn-"+b.Code); q["max-limit"] != pk.RateLimit() {
		t.Fatalf("%v", q)
	}
	// MAC change: the lease follows
	if err := e.svc.ChangeMAC(ctx, b.ID, "AA:BB:CC:DD:EE:B9"); err != nil {
		t.Fatal(err)
	}
	if l := find(t, e.rt.Table("/ip/dhcp-server/lease"), "address", e.get(b).IP); l["mac-address"] != "AA:BB:CC:DD:EE:B9" {
		t.Fatalf("%v", l)
	}
	// expiry by the clock is picked up by the next sweep
	e.now = e.now.Add(30 * 24 * time.Hour)
	e.svc.Sweep(ctx)
	if got := e.rt.Table("/ip/firewall/address-list"); len(got) != 2 {
		t.Fatalf("expired flats must be suspended by the sweep: %v", got)
	}
	// delete: its rows go and its address is free again
	if err := e.svc.DeleteAccount(ctx, a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	if len(rowsBy(e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)) != 0 || len(rowsBy(e.rt.Table("/queue/simple"), "name", "ftn-"+a.Code)) != 0 {
		t.Fatal("deleted flat's rows must go")
	}
	c := e.flat(t, "Flat 3", macC)
	if c.IP != "10.5.50.60" {
		t.Fatalf("freed address not reused: %s", c.IP)
	}
}

func TestAdoptLeasesAndAddressReuse(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	b := e.flat(t, "Flat 2", macB2)
	e.flat(t, "Flat 3", macC)
	// These flats were registered before the router was reachable: no address yet.
	e.st.Update(func(d *store.Data) error {
		for id, x := range d.Accounts {
			x.IP = ""
			d.Accounts[id] = x
		}
		return nil
	})
	// Flat 1 holds .23, inside the unregistered pool (.10-.59): it must not keep it.
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macA, "active-address": "10.5.50.23", "address": "10.5.50.23", "status": "bound", "dynamic": "true", "server": "dhcp-other"})
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": macB2, "active-address": "10.5.50.74", "address": "10.5.50.74", "status": "bound", "dynamic": "true"})
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "00:11:22:33:44:99", "address": "10.5.50.80", "status": "bound", "dynamic": "true"})
	res, err := e.svc.AdoptLeases(context.Background(), e.site)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 2 || len(res.Skipped) != 1 {
		t.Fatalf("%+v", res)
	}
	if got := e.get(a).IP; got != "10.5.50.60" {
		t.Fatalf("a lease in the unregistered pool must become the first free static address, got %q", got)
	}
	if e.get(b).IP != "10.5.50.74" {
		t.Fatalf("%q", e.get(b).IP)
	}
	// Flat 3 has none and later gets one from the static range, which skips the used ones.
	if n, err := e.svc.EnsureIPs(e.site); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := e.svc.AccountByMAC(macC); got.IP != "10.5.50.61" {
		t.Fatalf("%q", got.IP)
	}
	// Adopting again changes nothing.
	if res, _ := e.svc.AdoptLeases(context.Background(), e.site); len(res.Adopted) != 0 || res.Unchanged != 1 {
		t.Fatalf("%+v", res)
	}
}

// An address outside the static range is never kept: the reconciler gives the flat a fresh one.
func TestEnsureIPsReallocatesAddressesOutsideTheStaticRange(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.st.Update(func(d *store.Data) error {
		x := d.Accounts[a.ID]
		x.IP = "10.5.50.23"
		d.Accounts[a.ID] = x
		return nil
	})
	e.ipoe(t, store.AccessIPoE)
	e.reconcile(t)
	if got := e.get(a).IP; got != "10.5.50.60" {
		t.Fatalf("%q", got)
	}
	l := find(t, e.rt.Table("/ip/dhcp-server/lease"), "mac-address", macA)
	if l["address"] != "10.5.50.60" {
		t.Fatalf("lease %v", l)
	}
}

// A foreign static lease's address is not handed out to a new flat.
func TestFreeIPAvoidsForeignStaticLeaseAddresses(t *testing.T) {
	e := newEnv(t)
	e.ipoe(t, store.AccessIPoE)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "00:11:22:33:44:99", "address": "10.5.50.60", "dynamic": "false", "server": "ftn-dhcp"})
	e.reconcile(t)
	a := e.flat(t, "Flat 1", macA)
	if a.IP == "10.5.50.60" {
		t.Fatalf("the router's own static lease holds %s", a.IP)
	}
}

func TestMoveAccountReallocatesAddress(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	a := e.flat(t, "Flat 1", macA)
	other := e.flatAt(t, b, "Flat X", macC)
	if a.IP != "10.5.50.60" || other.IP != "10.5.50.60" {
		t.Fatalf("addresses are per building: %q %q", a.IP, other.IP)
	}
	if err := e.svc.MoveAccount(context.Background(), a.ID, b); !errors.Is(err, ErrRouterNotLinked) {
		t.Fatalf("a router that does not serve the apartment is refused: %v", err)
	}
	if err := e.svc.LinkRouter(store.MigratedApartmentID(e.site), b); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MoveAccount(context.Background(), a.ID, b); err != nil {
		t.Fatal(err)
	}
	if got := e.get(a); got.SiteID != b || got.IP != "10.5.50.61" {
		t.Fatalf("moved flat: %+v", got)
	}
	// Deleting frees the MAC and the address.
	if err := e.svc.DeleteAccount(context.Background(), a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.CreateAccount(AccountInput{ApartmentID: store.MigratedApartmentID(e.site), SiteID: b, Flat: "Flat Y", MAC: macA})
	if err != nil || n.IP != "10.5.50.61" {
		t.Fatalf("%+v %v", n, err)
	}
}

func TestEquipmentMACCannotClaimAnotherFlatsMAC(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	a := e.flat(t, "Flat 1", macA)
	bf := e.flatAt(t, b, "Flat 1", macC)
	ont, err := e.svc.SaveEquipment(EquipmentInput{Kind: store.KindONT, Serial: "ZX1", MAC: macA})
	if err != nil {
		t.Fatal(err)
	}
	var dup *DuplicateMACError
	if err := e.svc.AssignEquipment(context.Background(), ont.ID, bf.ID, "", true); !errors.As(err, &dup) || dup.AccountID != a.ID || dup.SiteName == "" {
		t.Fatalf("assigning an ONT whose MAC is another building's flat: %v", err)
	}
	if got := e.get(bf); got.MAC != macC {
		t.Fatal("MAC must not change on refusal")
	}
	// A second equipment item with the same MAC is refused too.
	if _, err := e.svc.SaveEquipment(EquipmentInput{Kind: store.KindONT, Serial: "ZX2", MAC: macA}); !errors.Is(err, ErrDuplicateEquipmentMAC) {
		t.Fatalf("%v", err)
	}
}
