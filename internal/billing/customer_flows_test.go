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

func (e *env) tbl(path string) []routeros.Row { return e.rt.Table(path) }

// F2-1: ONT swap while the router is offline, twice, then a server restart, then the router returns.
func TestCustomerFlow_ChangeMACOfflineTwiceThenRestart(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	e.reconcile(t)
	e.rt.SetFail(errors.New("tunnel down"))
	if err := e.svc.ChangeMAC(context.Background(), a.ID, macB2); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ChangeMAC(context.Background(), a.ID, macC); err != nil {
		t.Fatal(err)
	}
	if n := len(e.svc.PendingTasks()); n != 1 {
		t.Fatalf("tasks %d", n)
	}
	e.open(t) // restart
	e.rt.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Sweep(context.Background())
	l := e.tbl("/ip/dhcp-server/lease")
	if len(l) != 1 || l[0]["mac-address"] != macC {
		t.Fatalf("leases %v", l)
	}
	arp := e.tbl("/ip/arp")
	if len(arp) != 1 || arp[0]["mac-address"] != macC {
		t.Fatalf("arp %v", arp)
	}
	if n := len(e.svc.PendingTasks()); n != 0 {
		t.Fatalf("tasks left %d", n)
	}
}

// F2-2: move A->B while A is offline; a new flat on A reuses the moved flat's old IP; A returns.
func TestCustomerFlow_MoveAwayOfflineOldIPReusedThenRouterReturns(t *testing.T) {
	e := newEnv(t)
	b, _ := e.addSite(t, "Block B")
	x := e.flat(t, "Flat 1", macA)
	e.payAt(t, x, "1 Day")
	e.reconcile(t)
	oldIP := e.get(x).IP
	e.rt.SetFail(errors.New("tunnel down"))
	_, err := e.svc.MoveToApartment(context.Background(), MoveInput{AccountID: x.ID, ApartmentID: store.MigratedApartmentID(b), SiteID: b})
	if err != nil {
		t.Fatal(err)
	}
	y := e.flat(t, "Flat 2", macB2) // on A, A offline
	if e.get(y).IP != oldIP {
		t.Logf("y got %s, old %s", e.get(y).IP, oldIP)
	}
	e.rt.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Sweep(context.Background())
	for _, r := range e.tbl("/ip/dhcp-server/lease") {
		t.Logf("lease %v", r)
	}
	l := e.tbl("/ip/dhcp-server/lease")
	if len(l) != 1 || l[0]["mac-address"] != macB2 {
		t.Fatalf("A leases %v", l)
	}
	q := e.tbl("/queue/simple")
	if len(q) != 1 || q[0]["name"] != "ftn-"+y.Code {
		t.Fatalf("A queues %v", q)
	}
	if n := e.pass(t); n != 0 {
		t.Fatalf("not converged: %d writes", n)
	}
}

// F2-3: delete while offline, re-create same MAC on the same router before the router returns.
func TestCustomerFlow_DeleteOfflineThenRecreateSameMAC(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	e.reconcile(t)
	e.rt.SetFail(errors.New("down"))
	if err := e.svc.DeleteAccount(context.Background(), a.ID, a.Code); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.CreateAccount(AccountInput{SiteID: e.site, Flat: "Flat 1", MAC: macA})
	if err != nil {
		t.Fatal(err)
	}
	if n.Code == a.Code {
		t.Fatal("code reused")
	}
	e.rt.SetFail(nil)
	e.now = e.now.Add(10 * time.Minute)
	e.svc.Sweep(context.Background())
	l := e.tbl("/ip/dhcp-server/lease")
	if len(l) != 1 || !strings.Contains(l[0]["comment"], n.Code) {
		t.Fatalf("leases %v", l)
	}
	if n2 := e.pass(t); n2 != 0 {
		t.Fatalf("not converged %d", n2)
	}
	t.Logf("new account paid? state=%s", AccountState(e.get(n), e.now))
}

// F2-4: reconciler runs concurrently with admin actions.
func TestCustomerFlow_ConcurrentSweepAndAdminActions(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.svc.Sweep(ctx)
			}
		}
	}()
	macs := []string{macB2, macC, macA}
	for i := 0; i < 30; i++ {
		_ = e.svc.SetSuspended(ctx, a.ID, i%2 == 0)
		_ = e.svc.ChangeMAC(ctx, a.ID, macs[i%3])
		_ = e.svc.ChangePackage(ctx, a.ID, e.pkg([]string{"1 Day", "1 Week"}[i%2]).ID, true)
	}
	close(stop)
	wg.Wait()
	e.svc.RunSiteTasks(ctx, e.site)
	e.reconcile(t)
	cur := e.get(a)
	l := e.tbl("/ip/dhcp-server/lease")
	if len(l) != 1 || l[0]["mac-address"] != cur.MAC {
		t.Fatalf("lease %v want %s", l, cur.MAC)
	}
	q := e.tbl("/queue/simple")
	if len(q) != 1 || q[0]["max-limit"] != e.pkg([]string{"1 Day", "1 Week"}[1]).RateLimit() && q[0]["max-limit"] != e.pkg("1 Day").RateLimit() {
		t.Fatalf("queue %v", q)
	}
	if n := e.pass(t); n != 0 {
		t.Fatalf("writes %d", n)
	}
}

// F2-5: a pass that read the table before the admin change must not undo it (stale wants).
// ReconcileSite reads accounts, then does router I/O; a router read that blocks lets the admin act in between.
type gate struct {
	*routeros.Mock
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (g *gate) Rows(ctx context.Context, path string, q map[string]string) ([]routeros.Row, error) {
	if path == pathARP { // the last read of a pass
		g.once.Do(func() { close(g.reached); <-g.release })
	}
	return g.Mock.Rows(ctx, path, q)
}

func TestCustomerFlow_StalePassVersusAdminChange(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	e.reconcile(t)
	g := &gate{Mock: e.rt, reached: make(chan struct{}), release: make(chan struct{})}
	e.svc.routers = recSource{map[string]routeros.Client{e.site: g}}
	ctx := context.Background()
	done := make(chan struct{})
	go func() { e.svc.ReconcileSite(ctx, e.site); close(done) }() // the sweeper's pass
	<-g.reached
	adm := make(chan error, 1)
	go func() { adm <- e.svc.SetSuspended(ctx, a.ID, true) }() // admin acts while the pass holds old data; blocks on ps.run
	time.Sleep(200 * time.Millisecond)
	close(g.release)
	<-done
	if err := <-adm; err != nil {
		t.Fatal(err)
	}
	e.svc.RunSiteTasks(ctx, e.site)
	if l := e.tbl("/ip/firewall/address-list"); len(l) != 1 {
		t.Fatalf("not suspended on router: %v", l)
	}
	// the router has the entry; what does the API's router object say right after the action?
	if st := e.svc.AccountRouterState(a.ID); !st.SuspendedOnRouter {
		t.Fatalf("router state says suspended_on_router=%v in_sync=%v although the entry is on the router", st.SuspendedOnRouter, st.InSync)
	}
}

// F2-6: unsuspend: the router object after the action (the entry was removed in the same pass).
func TestCustomerFlow_RouterStateRightAfterUnsuspend(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	ctx := context.Background()
	if err := e.svc.SetSuspended(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if err := e.svc.SetSuspended(ctx, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := len(e.tbl("/ip/firewall/address-list")); n != 0 {
		t.Fatalf("entry still on router: %d", n)
	}
	st := e.svc.AccountRouterState(a.ID)
	t.Logf("after unsuspend: suspended_on_router=%v in_sync=%v pending=%v", st.SuspendedOnRouter, st.InSync, st.PendingTask)
	if st.SuspendedOnRouter {
		t.Fatalf("API would say suspended_on_router=true for an unsuspended, unwalled flat")
	}
}

// slow makes every read wait (a half-dead tunnel) before it fails like an unreachable router.
type slow struct {
	*routeros.Mock
	d time.Duration
}

func (s slow) Rows(ctx context.Context, path string, q map[string]string) ([]routeros.Row, error) {
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
	}
	return nil, errors.New("tunnel timeout")
}

// F2-7: move to a new router while the OLD router is dead: how long until the NEW router gets the flat?
func TestCustomerFlow_MoveNewRouterWaitsForDeadOldRouter(t *testing.T) {
	e := newEnv(t)
	b, brt := e.addSite(t, "Block B")
	x := e.flat(t, "Flat 1", macA)
	e.payAt(t, x, "1 Day")
	e.reconcile(t)
	e.svc.routers = recSource{map[string]routeros.Client{e.site: slow{e.rt, 1500 * time.Millisecond}, b: brt}}
	start := time.Now()
	var tNew time.Duration
	// observe when the new router first gets a lease
	done := make(chan struct{})
	go func() {
		for {
			if len(brt.Table("/ip/dhcp-server/lease")) > 0 {
				tNew = time.Since(start)
				close(done)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, err := e.svc.MoveToApartment(context.Background(), MoveInput{AccountID: x.ID, ApartmentID: store.MigratedApartmentID(b), SiteID: b}); err != nil {
		t.Fatal(err)
	}
	<-done
	t.Logf("new router got the lease after %v (old router read takes 1.5s to fail)", tNew)
	if tNew > 500*time.Millisecond {
		t.Errorf("the customer's new router waited %v behind the dead old router", tNew)
	}
}

// F2-8: the router already has its own static lease on .60, the server has not run a pass yet (fresh start / restart),
// and a flat is created: it gets .60.
func TestCustomerFlow_ForeignLeaseOnAssignedIPIsPermanent(t *testing.T) {
	e := newEnv(t)
	e.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": "00:11:22:33:44:55", "address": "10.5.50.60", "comment": "printer"})
	a := e.flat(t, "Flat 1", macA)
	ip := e.get(a).IP
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(2 * time.Minute)
		e.svc.Sweep(context.Background())
	}
	e.payAt(t, a, "1 Day")
	e.svc.Sweep(context.Background())
	st := e.svc.AccountRouterState(a.ID)
	t.Logf("ip=%s in_sync=%v lease=%s leases=%v queues=%v conflicts=%v", ip, st.InSync, st.LeaseStatus, len(e.tbl("/ip/dhcp-server/lease")), len(e.tbl("/queue/simple")), e.svc.ProvisionStatus(e.site).ForeignConflicts)
	if got := e.get(a).IP; got == ip {
		t.Errorf("flat keeps %s after the sweeps although a foreign static lease owns it: no lease, no queue, paid flat never provisioned", ip)
	}
	e.svc.Sweep(context.Background())
	e.svc.RunSiteTasks(context.Background(), e.site)
	st = e.svc.AccountRouterState(a.ID)
	if !st.InSync || st.LeaseStatus == "none" {
		t.Errorf("after the move to a free address the flat is not provisioned: in_sync=%v lease=%s", st.InSync, st.LeaseStatus)
	}
	if got := e.get(a).IP; got == "10.5.50.60" {
		t.Errorf("still %s", got)
	}
}

// F2-9: a dead router (reads hang ~10 s like routeros.DefaultTimeout) and the admin presses "Change MAC".
// Credit/payment paths cap this at 8 s (runSiteShort); the admin actions do not.
func TestCustomerFlow_AdminActionBlocksOnDeadRouter(t *testing.T) {
	e := newEnv(t)
	a := e.flat(t, "Flat 1", macA)
	e.payAt(t, a, "1 Day")
	e.reconcile(t)
	e.svc.routers = recSource{map[string]routeros.Client{e.site: slow{e.rt, 9500 * time.Millisecond}}}
	start := time.Now()
	if err := e.svc.ChangeMAC(context.Background(), a.ID, macB2); err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	t.Logf("ChangeMAC returned after %v with the router dead (MAC is saved, router task queued)", el)
	if el > 8*time.Second {
		t.Errorf("admin action held the HTTP request %v: no 8 s cap like runSiteShort", el)
	}
}
