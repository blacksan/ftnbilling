package sites

import (
	"context"
	"errors"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// ChangePort / UpdateSite / ApplyFix wait for the per-router run lock with a plain mutex, so the
// caller's context (20 s in the API) is not honoured while a sweep pass holds the lock.
func TestPortsFlow_ChangePortHonoursItsContext(t *testing.T) {
	f, s, _ := portsRig(t)
	e := f.m.cport.entry(s.ID)
	e.run.Lock() // a slow sweep pass
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("customer")}, operWho)
		done <- err
	}()
	select {
	case err := <-done:
		var pe *PortRefusal
		if !errors.As(err, &pe) || pe.Kind != PortErrConflict {
			t.Fatalf("expected a busy refusal, got %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		e.run.Unlock()
		err := <-done
		t.Fatalf("ChangePort did not return when its context (150ms) expired; it waited for the lock and then ran with a dead context: %v", err)
	}
	e.run.Unlock()
}

// the Ports cache (4 s) is not dropped by ChangePort, so the answer of the change and the next poll show
// the old enabled/disabled state.
func TestPortsFlow_PortsViewFreshAfterDisable(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	if _, err := f.m.Ports(ctx, s.ID, store.RoleOwner); err != nil { // the tab polls
		t.Fatal(err)
	}
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Disabled: bp(true)}, operWho); err != nil {
		t.Fatal(err)
	}
	if ifaceRow(m, "ether4")["disabled"] != "yes" {
		t.Fatal("router not changed")
	}
	f.advance(time.Second)
	v, _ := f.m.Ports(ctx, s.ID, store.RoleOwner)
	if p := portByName(t, v, "ether4"); !p.Disabled {
		t.Fatalf("the Ports view still says ether4 is enabled 1 s after it was switched off: %+v", p)
	}
}

// role change and disable in one request: the role is stored, then the router write fails; the caller
// gets an error although the role change stays.
func TestPortsFlow_RoleAndDisableAtomic(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	m.SetWriteFail("/interface", &routeros.APIError{Status: 500, Message: "boom"})
	_, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("customer"), Disabled: bp(true)}, operWho)
	f.m.cport.wg.Wait()
	got := f.site(s.ID)
	if err == nil {
		t.Fatalf("the failing router write was not reported")
	}
	if err != nil && containsStr(SplitPorts(got.CustomerPorts), "ether4") {
		t.Fatalf("request failed (%v) but ether4 is now a stored customer port", err)
	}
}

// an OLT uplink that is a bond (supported by PortChoices) is not a physical port: the Ports tab shows no
// OLT row at all, so nobody can see which port holds the role.
func TestPortsFlow_BondOLTUplinkListed(t *testing.T) {
	f, s, m := portsRig(t)
	m.SeedRow("/interface", R{"name": "bond1", "type": "bond", "running": "true"})
	f.st.Update(func(d *store.Data) error { x := d.Sites[s.ID]; x.OLTInterface = "bond1"; d.Sites[s.ID] = x; return nil })
	v, _ := f.m.Ports(context.Background(), s.ID, store.RoleOwner)
	for _, p := range v.Items {
		if p.Role == PortRoleOLT {
			return
		}
	}
	t.Fatalf("no row has the OLT role although the OLT uplink is bond1: %d rows", len(v.Items))
}

// the OLT port or a LAN port that disappeared from the router is not listed with its stored role.
func TestPortsFlow_VanishedPortsListedAndClearable(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	// sfp-sfpplus1 vanishes
	var rows []routeros.Row = m.Table("/interface")
	for _, r := range rows {
		if r["name"] == "sfp-sfpplus1" {
			m.RemoveRow(ctx, "/interface", r[".id"])
		}
	}
	f.m.EnsureCustomerPort(ctx, s.ID, true)
	v, _ := f.m.Ports(ctx, s.ID, store.RoleOwner)
	found := false
	for _, p := range v.Items {
		if p.Name == "sfp-sfpplus1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the OLT port has no row")
	}
	// a vanished customer port can be cleared from the tab
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("customer")}, operWho); err != nil {
		t.Fatal(err)
	}
	m.RemoveRow(ctx, "/interface", ifaceRow(m, "ether4")[".id"])
	f.m.dropPortsView(s.ID)
	v, _ = f.m.Ports(ctx, s.ID, store.RoleOwner)
	p := portByName(t, v, "ether4")
	if !p.Missing || len(p.RoleOptions) != 1 || p.RoleOptions[0] != PortRoleUnused {
		t.Fatalf("vanished customer port row: %+v", p)
	}
	if p.Role == PortRoleOLT || p.CanDisable {
		t.Fatalf("a vanished customer port is not the OLT port and cannot be switched off: %+v", p) // the tab names the OLT only by role
	}
	if _, err := f.m.ChangePort(ctx, s.ID, "ether4", PortChange{Role: sp("unused")}, operWho); err != nil {
		t.Fatalf("clearing a vanished port: %v", err)
	}
	if containsStr(SplitPorts(f.site(s.ID).CustomerPorts), "ether4") {
		t.Fatal("role not cleared")
	}
}

var _ = errors.New

// cadence. A repeated failure (router offline) is "unchanged", so after the second pass the router is
// probed only every 5 minutes, although the doc says every minute after a failure, and cpBackoff (5 s ..
// 5 min) never has an effect.
func TestPortsFlow_OfflineRouterCadence(t *testing.T) {
	f, s, m := portsRig(t)
	ctx := context.Background()
	e := f.m.cport.entry(s.ID)
	gap := func() time.Duration {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.nextSweep.Sub(f.now)
	}
	f.m.SweepSite(ctx, s.ID) // first pass writes the customer port: busy
	f.advance(time.Minute)
	f.m.SweepSite(ctx, s.ID) // now clean
	if gap() != 5*time.Minute {
		t.Fatalf("clean: %v", gap())
	}
	m.SetFail(errors.New("dial tcp 10.99.0.2:443: i/o timeout"))
	f.m.chk.drop(s.ID)
	f.advance(5 * time.Minute)
	f.m.SweepSite(ctx, s.ID)

	f.advance(time.Minute)
	f.m.SweepSite(ctx, s.ID)

	f.advance(5 * time.Minute)
	f.m.SweepSite(ctx, s.ID)

	if gap() != time.Minute {
		t.Fatalf("a router that stays offline is probed every %v, not every minute", gap())
	}
}

// AutoMaintain works from the cached checklist (up to a minute old). If the router was fixed by somebody
// else in between, applyFix finds nothing to do and returns no error; AutoMaintain then records "fixed by the
// server" with no write, and the checklist says "Fixed by the server at ...".
func TestPortsFlow_AutoFixNoCreditForSomebodyElsesFix(t *testing.T) {
	f, s, m := checkRig(t)
	ctx := context.Background()
	disableDHCP(m)
	if it := checkItem(t, f, s.ID, CheckDHCP); it.Status != StatusFail { // the cache now holds "fail"
		t.Fatalf("%+v", it)
	}
	for _, r := range m.Table("/ip/dhcp-server") { // an admin switches it on at the router
		m.SetRow(ctx, "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "false"})
	}
	m.ResetWrites()
	f.m.AutoMaintain(ctx, s.ID) // the cached (not fresh) list is used
	it := checkItem(t, f, s.ID, CheckDHCP)
	if m.Writes() == 0 && it.Auto != nil && it.Auto.State == "fixed" {
		t.Fatalf("the server wrote nothing, yet the checklist says: %s", it.Auto.Text)
	}
}
