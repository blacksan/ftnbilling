package sites

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func (r *provRig) oldServer() routeros.Row {
	for _, d := range r.mock.Table("/ip/dhcp-server") {
		if d["name"] == "dhcp-hotspot" {
			return d
		}
	}
	r.t.Fatal("no old server")
	return nil
}

func TestParseROSDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"10m": 10 * time.Minute, "1d": 24 * time.Hour, "1w2d3h4m5s": 7*24*time.Hour + 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second,
		"00:10:00": 10 * time.Minute, "1d00:30:00": 24*time.Hour + 30*time.Minute, "30": 30 * time.Second, "2s": 2 * time.Second,
	} {
		got, ok := parseROSDuration(in)
		if !ok || got != want {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "abc", "10x", "1m2", "m"} {
		if _, ok := parseROSDuration(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestEvidenceWindowCoversHalfTheLongestLease(t *testing.T) {
	tm := DefaultProvTiming()
	if w := tm.evidenceWindow(0); w != 5*time.Minute {
		t.Fatalf("minimum 5 minutes, got %v", w)
	}
	if w := tm.evidenceWindow(10 * time.Minute); w != 7*time.Minute {
		t.Fatalf("10m lease: half plus two minutes, got %v", w)
	}
	if w := tm.evidenceWindow(24 * time.Hour); w != 12*time.Minute {
		t.Fatalf("the kit's countdown caps the window at 12 minutes, got %v", w)
	}
}

// ---- B1 ---------------------------------------------------------------------------------------

func TestRollbackNowOnALiveRouterWritesNothingAndCreatesNoMarker(t *testing.T) {
	r := migrateRig(t)
	if j := r.migrate(); j.State != JobLive {
		t.Fatalf("%s %q", j.State, j.LastError)
	}
	r.mock.ResetWrites()
	if err := r.m.RollbackNow(r.sd.ID, "olga"); !errors.Is(err, ErrNothingToRollBack) {
		t.Fatalf("%v", err)
	}
	if r.mock.Writes() != 0 || len(r.armedRows()) != 0 {
		t.Fatalf("writes %d armed %v", r.mock.Writes(), r.armedRows())
	}
	if r.kit.rolled.Load() != 0 {
		t.Fatal("rolled back a live router")
	}
}

func TestRollbackNowCleansAStrayNowMarker(t *testing.T) {
	r := migrateRig(t)
	r.kit.Stop()
	r.mock.SeedRow(pathAddrList, R{"list": KitListArmed, "address": KitArmedAddress, "comment": "now"})
	if err := r.m.RollbackNow(r.sd.ID, "olga"); !errors.Is(err, ErrNothingToRollBack) {
		t.Fatalf("%v", err)
	}
	if len(r.armedRows()) != 0 {
		t.Fatalf("the stray marker stays: %v", r.armedRows())
	}
}

func TestSetNowNeverCreatesTheMarkerWithoutUndoRows(t *testing.T) {
	r := migrateRig(t)
	r.kit.Stop()
	j := &jobCtl{m: r.m, ctx: context.Background(), siteID: r.sd.ID, rt: r.m.Router(r.sd.ID), tm: fastTiming()}
	j.site = r.m.Effective(r.f())
	j.insp, _ = j.rt.(routeros.Inspector)
	j.plan, _ = PlanSubnet(j.site.CustomerSubnet)
	j.job.Armed = true
	if err := j.setNow(context.Background()); !errors.Is(err, ErrNothingToRollBack) {
		t.Fatalf("%v", err)
	}
	// while the job holds undo rows between arming and the switch it may create it
	r.mock.SeedRow(pathAddrList, R{"list": KitListUndo, "address": KitUndoAddress(1), "comment": KitUndoHotspot("hotspot1")})
	if err := j.setNow(context.Background()); err != nil || !r.nowSet() {
		t.Fatalf("%v %v", err, r.armedRows())
	}
}

func TestAConfirmedRouterIsNeverRolledBackByARestart(t *testing.T) {
	r := migrateRig(t)
	if j := r.migrate(); j.State != JobLive {
		t.Fatal(j.State)
	}
	// the crash window: the job says armed and verifying, the confirmed mark is set
	r.m.saveJob(r.sd.ID, ProvJob{State: JobVerifying, Kind: KindMigrate, Armed: true, StartedAt: r.now})
	r.mock.SeedRow(pathAddrList, R{"list": KitListArmed, "address": KitArmedAddress, "comment": "6"})
	r.mock.SeedRow(pathAddrList, R{"list": KitListUndo, "address": KitUndoAddress(1), "comment": KitUndoHotspot("hotspot1")})
	r.m.resumeProvisioning()
	r.m.WaitProvision(r.sd.ID)
	time.Sleep(50 * time.Millisecond)
	if r.kit.rolled.Load() != 0 || r.nowSet() {
		t.Fatal("a confirmed router was rolled back")
	}
	if j, _ := r.m.ProvJobOf(r.sd.ID); j.State != JobLive {
		t.Fatalf("the restart must finish the job: %s %q", j.State, j.LastError)
	}
	if len(r.armedRows()) != 0 {
		t.Fatal("the leftover markers must be removed by the rerun")
	}
}

func TestKeepDecisionIsPersistedBeforeTheMarkersGo(t *testing.T) {
	r := migrateRig(t)
	// removing the armed row fails: the evidence held, so no rollback is asked for, and the job no longer counts as armed
	r.kit.set(func(k *kitSim) { k.failMarkers = true })
	j := r.migrate()
	r.mock.SetWriteFail(pathAddrList, nil)
	if j.Armed {
		t.Fatalf("Armed must be false once the evidence held: %s %q %v", j.State, j.LastError, j.Steps)
	}
	if r.nowSet() || r.kit.rolled.Load() != 0 {
		t.Fatalf("a passing router must not be rolled back: %v", r.armedRows())
	}
	if j.State != JobFailed || !strings.Contains(j.LastError, "markers could not be removed") {
		t.Fatalf("%s %q", j.State, j.LastError)
	}
}

// ---- B2 ---------------------------------------------------------------------------------------

func TestOldLeasesAreShortenedWaitedForAndRestored(t *testing.T) {
	r := migrateRig(t)
	r.kit.set(func(k *kitSim) { k.lateRenew = true })
	old := r.oldServer()
	_ = r.mock.SetRow(context.Background(), "/ip/dhcp-server", old[routeros.RowID], map[string]string{"lease-time": "4s"})
	for _, l := range r.mock.Table("/ip/dhcp-server/lease") {
		_ = r.mock.SetRow(context.Background(), "/ip/dhcp-server/lease", l[routeros.RowID], map[string]string{"server": "dhcp-hotspot", "expires-after": "1s"})
	}
	start := time.Now()
	j := r.migrate()
	if j.State != JobLive {
		t.Fatalf("clients that renew late must still pass: %s %q ev=%+v", j.State, j.LastError, j.Evidence)
	}
	if el := time.Since(start); el < time.Second {
		t.Fatalf("the job must wait for the running lease to renew: %v", el)
	}
	r.kit.mu.Lock()
	at := r.kit.leaseAtSwitch
	r.kit.mu.Unlock()
	if at != "1s" {
		t.Fatalf("the old server's lease time at the switch was %q, want the shortened 1s", at)
	}
	if got := r.oldServer()["lease-time"]; got != "4s" {
		t.Fatalf("the lease time was not put back: %q", got)
	}
	if j.LongestLeaseSec != 4 {
		t.Fatalf("%d", j.LongestLeaseSec)
	}
	if r.m.loadLeaseOrig(r.sd.ID) != nil && len(r.m.loadLeaseOrig(r.sd.ID)) != 0 {
		t.Fatal("the saved original must be cleared")
	}
}

func TestLeaseTimeIsPutBackAfterARollbackAndAfterAFailure(t *testing.T) {
	t.Run("rollback", func(t *testing.T) {
		r := migrateRig(t)
		r.kit.set(func(k *kitSim) { k.noBound = true })
		if j := r.migrate(); j.State != JobRolledBack {
			t.Fatal(j.State)
		}
		if got := r.oldServer()["lease-time"]; got != "2s" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("a failure before the arm", func(t *testing.T) {
		r := migrateRig(t)
		r.kit.set(func(k *kitSim) { k.noBackup = true })
		if j := r.migrate(); j.State != JobFailed {
			t.Fatal(j.State)
		}
		if got := r.oldServer()["lease-time"]; got != "2s" {
			t.Fatalf("%q", got)
		}
	})
}

func TestEvidenceWindowGrowsWithTheLongestLease(t *testing.T) {
	r := migrateRig(t)
	r.kit.set(func(k *kitSim) { k.lateRenew = true; k.noBound = true })
	old := r.oldServer()
	_ = r.mock.SetRow(context.Background(), "/ip/dhcp-server", old[routeros.RowID], map[string]string{"lease-time": "10s"})
	start := time.Now()
	j := r.migrate()
	if j.State != JobRolledBack {
		t.Fatal(j.State)
	}
	// 10s / 2 + 2 graces = 5.04s, far above the configured 400ms
	if el := time.Since(start); el < 5*time.Second {
		t.Fatalf("the evidence window was only %v", el)
	}
}

// ---- S1 ---------------------------------------------------------------------------------------

func TestEvidenceNeedsTheCustomerPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(r *provRig)
		want string
	}{
		{"customer ping", func(r *provRig) { r.mock.SetCustomerPathDown(true) }, "customer network does not get out"},
		{"answered connection", func(r *provRig) { r.kit.set(func(k *kitSim) { k.noConn = true }) }, "answered"},
		{"dns", func(r *provRig) { r.kit.set(func(k *kitSim) { k.breakDNS = true }) }, "DNS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := migrateRig(t)
			tc.set(r)
			j := r.migrate()
			if j.State != JobRolledBack {
				t.Fatalf("%s %q", j.State, j.LastError)
			}
			if !strings.Contains(r.ann.all(), tc.want) {
				t.Fatalf("announce %q lacks %q", r.ann.all(), tc.want)
			}
		})
	}
}

func TestFreshVerifyStillChecksThePathWithNoFlats(t *testing.T) {
	r := freshRig(t)
	r.mock.SetCustomerPathDown(true)
	j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
	if j.State == JobLive {
		t.Fatal("a fresh router whose customers cannot get out is not live")
	}
	if j.Evidence.FlatsRequired != 0 || j.Evidence.CustomerPath {
		t.Fatalf("%+v", j.Evidence)
	}
}

// ---- S2 ---------------------------------------------------------------------------------------

func TestFreshNetworkGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(m *routeros.Mock)
		want string
		set  func(r *provRig)
	}{
		{"overlapping address", func(m *routeros.Mock) {
			m.SeedRow("/ip/address", R{"address": "10.5.0.1/16", "interface": "ether2", "disabled": "false"})
		}, "overlaps", nil},
		{"fibre port in the LAN list", func(m *routeros.Mock) {
			m.SeedRow("/interface/list/member", R{"list": "LAN", "interface": "ether2"})
		}, "LAN", nil},
		{"fibre port already in the default bridge", func(m *routeros.Mock) {
			m.SeedRow("/interface/bridge", R{"name": "bridge"})
			m.SeedRow("/interface/bridge/port", R{"bridge": "bridge", "interface": "ether2", "disabled": "false"})
		}, "already a port of bridge bridge", nil},
		{"fibre port with its own address", func(m *routeros.Mock) {
			m.SeedRow("/ip/address", R{"address": "192.168.99.1/24", "interface": "ether2", "disabled": "false"})
		}, "itself has an IP address", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := freshRig(t)
			if err := r.m.UpdateSite(r.sd.ID, SiteInput{Name: "Block F", Provision: true, BenchPort: "ether5", OLTInterface: "ether2"}); err != nil {
				t.Fatal(err)
			}
			tc.seed(r.mock)
			r.mock.ResetWrites()
			j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
			if j.State != JobNeedsAction || !strings.Contains(j.LastError, tc.want) {
				t.Fatalf("%s %q", j.State, j.LastError)
			}
			for _, name := range []string{"/interface/bridge/port", "/ip/pool", "/ip/dhcp-server"} {
				for _, row := range r.mock.Table(name) {
					if strings.HasPrefix(row["comment"], "ftn:") {
						t.Fatalf("a row was written before the guard: %s %v", name, row)
					}
				}
			}
		})
	}
}

// ---- S4 ---------------------------------------------------------------------------------------

func TestServingIntentForcesTheMigratePathEvenWithNobodyOnline(t *testing.T) {
	r := freshRig(t)
	r.m.SetServingCustomers(r.sd.ID, true)
	j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
	if j.Kind != KindMigrate || j.State == JobLive {
		t.Fatalf("%s %s", j.Kind, j.State)
	}
}

func TestFreshRunThatSwitchesOffAServerNeedsTheKit(t *testing.T) {
	r := freshRig(t)
	// an old hotspot is on the customer network, no leases, and the router has no kit
	r.mock.SeedRow("/ip/address", R{"address": "10.5.50.1/24", "interface": "br-hotspot", "disabled": "false"})
	r.mock.SeedRow("/interface", R{"name": "br-hotspot"})
	r.mock.SeedRow("/interface/bridge", R{"name": "br-hotspot"})
	r.mock.SeedRow("/ip/hotspot", R{"name": "hotspot1", "interface": "br-hotspot", "disabled": "false"})
	r.mock.SeedRow("/ip/dhcp-server", R{"name": "dhcp-hotspot", "interface": "br-hotspot", "disabled": "false"})
	r.mock.ResetWrites()
	j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
	if j.State != JobNeedsAction || !strings.Contains(j.LastError, "safety kit") {
		t.Fatalf("%s %q blockers=%v", j.State, j.LastError, j.Blockers)
	}
	for _, h := range r.mock.Table("/ip/hotspot") {
		if h["disabled"] != "false" {
			t.Fatal("the hotspot was switched off without a kit")
		}
	}
}

// ---- nits -------------------------------------------------------------------------------------

func TestRuleDriftIsRepairedRuleByRuleWithoutAnUnwalledMoment(t *testing.T) {
	r := migrateRig(t)
	if j := r.migrate(); j.State != JobLive {
		t.Fatal(j.State)
	}
	count := func() int {
		n := 0
		for _, x := range r.mock.Table(pathFilter) {
			if strings.HasPrefix(x["comment"], "ftn: ipoe") {
				n++
			}
		}
		return n
	}
	full := count()
	// one rule is gone and one has drifted
	rows := r.mock.Table(pathFilter)
	var gone, drift routeros.Row
	for _, x := range rows {
		if strings.HasPrefix(x["comment"], "ftn: ipoe walled reject ") && gone == nil {
			gone = x
		} else if strings.HasPrefix(x["comment"], "ftn: ipoe isolate") {
			drift = x
		}
	}
	if gone == nil || drift == nil {
		t.Fatal("setup")
	}
	_ = r.mock.RemoveRow(context.Background(), pathFilter, gone[routeros.RowID])
	_ = r.mock.SetRow(context.Background(), pathFilter, drift[routeros.RowID], map[string]string{"action": "accept"})
	r.mock.ResetWrites()
	if j := r.run(ProvOptions{Trigger: "run", Actor: "t"}); j.State != JobLive {
		t.Fatalf("%s %q", j.State, j.LastError)
	}
	if n := r.mock.Writes(); n != 2 {
		t.Fatalf("one add and one set expected, %d writes", n)
	}
	if count() != full {
		t.Fatalf("%d rules, want %d", count(), full)
	}
}

func TestPayingFlatWithAMACConflictStopsAMigration(t *testing.T) {
	r := migrateRig(t)
	_ = r.st.Update(func(d *store.Data) error {
		a := d.Accounts["a3"]
		a.PackageID = "p1"
		a.ExpiresAt = r.now.Add(24 * time.Hour)
		d.Accounts["a3"] = a
		return nil
	})
	r.mock.ResetWrites()
	j := r.migrate()
	if j.State != JobNeedsAction || !strings.Contains(j.LastError, "Flat 11") {
		t.Fatalf("%s %q", j.State, j.LastError)
	}
	if r.mock.Writes() != 0 {
		t.Fatal("nothing may be written")
	}
}

func TestRollbackNowReservesTheRunBeforeStoppingAnything(t *testing.T) {
	r := migrateRig(t)
	r.kit.set(func(k *kitSim) { k.ignoreNow = true })
	r.mock.SeedRow(pathAddrList, R{"list": KitListArmed, "address": KitArmedAddress, "comment": "2"})
	r.mock.SeedRow(pathAddrList, R{"list": KitListUndo, "address": KitUndoAddress(1), "comment": KitUndoHotspot("hotspot1")})
	if err := r.m.RollbackNow(r.sd.ID, "olga"); err != nil {
		t.Fatal(err)
	}
	if !r.m.SetupBusy(r.sd.ID) {
		t.Fatal("the run entry must be reserved when RollbackNow returns")
	}
	if err := r.m.StartProvision(r.sd.ID, ProvOptions{Trigger: "run"}); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("a job started next to the rollback: %v", err)
	}
	r.m.WaitProvision(r.sd.ID)
}

var _ = billing.DHCPServerName

func TestProvWriterSetRulesAreTightened(t *testing.T) {
	m := routeros.NewMock()
	w := testWriter(m)
	ctx := context.Background()
	w.sc.network = "10.5.50.0/24"
	filter := R{routeros.RowID: "*1", "chain": "forward", "action": "accept", "comment": "ftn: ipoe isolate flats", "in-interface": "br-hotspot"}
	for _, tc := range []struct {
		name string
		path string
		row  routeros.Row
		diff map[string]string
	}{
		{"an ipoe rule turned into a masquerade-less accept of the WAN", "/ip/firewall/filter", filter, R{"unknown-prop": "x"}},
		{"a customer dns rule opened to the WAN interface", "/ip/firewall/filter",
			R{routeros.RowID: "*2", "chain": "input", "action": "accept", "protocol": "udp", "dst-port": "53", "in-interface": "br-hotspot", "comment": "ftn: customer dns"}, R{"in-interface": "ether1"}},
		{"the WAN DNS drop turned into an accept", "/ip/firewall/filter",
			R{routeros.RowID: "*3", "chain": "input", "action": "drop", "protocol": "udp", "dst-port": "53", "in-interface-list": "WAN", "comment": dnsWANComment}, R{"action": "accept"}},
		{"an rule renamed away from ftn:", "/ip/firewall/filter", filter, R{"comment": "mine"}},
		{"an address moved to the WAN interface", "/ip/address", R{routeros.RowID: "*4", "address": "10.5.50.1/24", "interface": "br-hotspot", "comment": "ftn: customer gateway"}, R{"interface": "ether1"}},
		{"a marker row moved to another list", "/ip/firewall/address-list", R{routeros.RowID: "*5", "list": KitListArmed, "address": KitArmedAddress, "comment": "3"}, R{"list": "whitelist"}},
		{"a bridge port moved to the default bridge", "/interface/bridge/port", R{routeros.RowID: "*6", "bridge": "br-hotspot", "interface": "ether2"}, R{"bridge": "bridge"}},
		{"a suspended-list address outside the plan", "/ip/firewall/address-list", R{routeros.RowID: "*7", "list": "ftn-suspended", "address": "10.5.50.60", "comment": "ftn:acct:K7Q2MX Flat 3B"}, R{"address": "192.168.1.5"}},
		{"a queue retargeted outside the plan", "/queue/simple", R{routeros.RowID: "*8", "name": "ftn-K7Q2MX", "target": "10.5.50.60/32", "comment": "ftn:acct:K7Q2MX Flat 3B"}, R{"target": "192.168.1.5/32"}},
	} {
		if err := w.set(ctx, tc.path, tc.row, tc.diff); !errors.Is(err, ErrProvNotAllowed) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name string
		path string
		body map[string]string
	}{
		{"a suspended address outside the plan", "/ip/firewall/address-list", R{"list": "ftn-suspended", "address": "192.168.1.5", "comment": "ftn:acct:X"}},
		{"an unreg range outside the plan", "/ip/firewall/address-list", R{"list": "ftn-unreg", "address": "10.9.0.1-10.9.0.9", "comment": "ftn: unregistered range"}},
		{"a queue outside the plan", "/queue/simple", R{"name": "ftn-X", "target": "192.168.1.5/32", "comment": "ftn:acct:X"}},
		{"a queue without a target", "/queue/simple", R{"name": "ftn-X", "comment": "ftn:acct:X"}},
	} {
		if err := w.add(ctx, tc.path, tc.body); !errors.Is(err, ErrProvNotAllowed) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// what the job does stays allowed
	for _, tc := range []struct {
		path string
		row  routeros.Row
		diff map[string]string
	}{
		{"/ip/firewall/filter", filter, R{"disabled": "no"}},
		{"/ip/firewall/address-list", R{routeros.RowID: "*5", "list": KitListArmed, "address": KitArmedAddress, "comment": "3"}, R{"comment": "now"}},
		{"/interface/bridge/port", R{routeros.RowID: "*6", "bridge": "bridge", "interface": "ether2"}, R{"bridge": "br-hotspot"}},
		{"/ip/dhcp-server", R{routeros.RowID: "*9", "name": "dhcp-hotspot", "interface": "br-hotspot", "lease-time": "30m"}, R{"lease-time": "1m"}},
		{"/ip/dhcp-server", R{routeros.RowID: "*9", "name": "dhcp-hotspot", "interface": "br-hotspot"}, R{"disabled": "yes"}},
	} {
		row := routeros.Row{}
		for k, v := range tc.row {
			row[k] = v
		}
		row[routeros.RowID] = m.SeedRow(tc.path, tc.row)
		if err := w.set(ctx, tc.path, row, tc.diff); err != nil {
			t.Errorf("%s %v: %v", tc.path, tc.diff, err)
		}
	}
	// an old server may change its lease time and nothing else, and only on the customer interface
	if err := w.set(ctx, "/ip/dhcp-server", R{routeros.RowID: "*9", "name": "dhcp-hotspot", "interface": "br-hotspot"}, R{"lease-time": "1m", "address-pool": "x"}); !errors.Is(err, ErrProvNotAllowed) {
		t.Error(err)
	}
	if err := w.set(ctx, "/ip/dhcp-server", R{routeros.RowID: "*10", "name": "lan-dhcp", "interface": "ether9"}, R{"lease-time": "1m"}); !errors.Is(err, ErrProvNotAllowed) {
		t.Error(err)
	}
	if err := w.set(ctx, "/ip/dhcp-server", R{routeros.RowID: "*9", "name": "dhcp-hotspot", "interface": "br-hotspot"}, R{"lease-time": "1;/quit"}); !errors.Is(err, ErrProvNotAllowed) {
		t.Error(err)
	}
}

func TestConsoleRefusesRebootAndShutdown(t *testing.T) {
	// covered in routeros/policy_test.go; here the guard text must point to the guarded button
	d := routeros.Classify("/system reboot")
	if d.Verdict != routeros.Deny || !strings.Contains(d.Reason, "Reboot") {
		t.Fatalf("%+v", d)
	}
}
