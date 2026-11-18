package sites

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// kitSim plays the safety kit's two scripts on a Mock: it answers a backup
// request, ticks the armed marker, honours "now" and rolls the router back.
type kitSim struct {
	m           *routeros.Mock
	mu          sync.Mutex
	noBackup    bool // never makes the backup
	noTick      bool // the timer never runs
	noBound     bool // customers never get a lease on ftn-dhcp
	ignoreNow   bool // the kit does not honour "now" (so the test can look at what the job did after it)
	dieOnSwitch bool // the tunnel drops the moment ftn-dhcp is enabled
	nowWrites   int  // the router's write count when the marker first said now (-1: not yet)
	rolled      atomic.Int32
	stop        chan struct{}
	done        chan struct{}
}

func startKit(t *testing.T, m *routeros.Mock) *kitSim {
	k := &kitSim{nowWrites: -1, m: m, stop: make(chan struct{}), done: make(chan struct{})}
	go k.loop()
	t.Cleanup(k.Stop)
	return k
}

func (k *kitSim) Stop() {
	select {
	case <-k.stop:
	default:
		close(k.stop)
	}
	<-k.done
}

func (k *kitSim) set(f func(k *kitSim)) { k.mu.Lock(); f(k); k.mu.Unlock() }

func (k *kitSim) loop() {
	defer close(k.done)
	for {
		select {
		case <-k.stop:
			return
		case <-time.After(4 * time.Millisecond):
		}
		k.tick()
	}
}

func (k *kitSim) tick() {
	k.mu.Lock()
	noBackup, noTick, noBound, ignoreNow, die := k.noBackup, k.noTick, k.noBound, k.ignoreNow, k.dieOnSwitch
	k.mu.Unlock()
	ctx := context.Background()
	al := "/ip/firewall/address-list"
	if !noBackup {
		for _, r := range k.m.Table(al) {
			if r["list"] == KitListCmd && r["comment"] == KitCmdBackup {
				k.m.SeedRow("/file", routeros.Row{"name": KitBackupFile + ".backup"})
				_ = k.m.RemoveRow(ctx, al, r[routeros.RowID])
			}
		}
	}
	if !noTick {
		for _, r := range k.m.Table(al) {
			if r["list"] != KitListArmed {
				continue
			}
			if r["comment"] == KitArmedNow {
				if ignoreNow {
					k.mu.Lock()
					if k.nowWrites < 0 {
						k.nowWrites = k.m.Writes()
					}
					k.mu.Unlock()
					continue
				}
				k.rollback()
				return
			}
			n := 0
			for _, c := range r["comment"] {
				n = n*10 + int(c-'0')
			}
			_ = k.m.SetRow(ctx, al, r[routeros.RowID], map[string]string{"comment": strconv.Itoa(n + 1)})
		}
	}
	if die {
		for _, d := range k.m.Table("/ip/dhcp-server") {
			if d["name"] == billing.DHCPServerName && d["disabled"] != "true" && d["disabled"] != "yes" {
				k.m.SetFail(errors.New("tunnel down"))
			}
		}
	}
	if !noBound {
		for _, d := range k.m.Table("/ip/dhcp-server") {
			if d["name"] == billing.DHCPServerName && d["disabled"] != "true" && d["disabled"] != "yes" {
				for _, l := range k.m.Table("/ip/dhcp-server/lease") {
					if l["server"] == billing.DHCPServerName && l["status"] != "bound" {
						_ = k.m.SetRow(ctx, "/ip/dhcp-server/lease", l[routeros.RowID], map[string]string{"status": "bound"})
					}
				}
			}
		}
	}
}

func (k *kitSim) rollback() {
	ctx := context.Background()
	al := "/ip/firewall/address-list"
	for _, p := range []string{"/ip/hotspot", "/ip/dhcp-server"} {
		for _, r := range k.m.Table(p) {
			if r["name"] == billing.DHCPServerName {
				_ = k.m.SetRow(ctx, p, r[routeros.RowID], map[string]string{"disabled": "true"})
			} else if r["disabled"] == "true" || r["disabled"] == "yes" {
				_ = k.m.SetRow(ctx, p, r[routeros.RowID], map[string]string{"disabled": "false"})
			}
		}
	}
	for _, p := range []string{"/ip/firewall/filter", "/ip/firewall/nat"} {
		for _, r := range k.m.Table(p) {
			if strings.HasPrefix(r["comment"], "ftn: ipoe") {
				_ = k.m.RemoveRow(ctx, p, r[routeros.RowID])
			}
		}
	}
	for _, r := range k.m.Table(al) {
		if r["list"] == KitListArmed || r["list"] == KitListUndo {
			_ = k.m.RemoveRow(ctx, al, r[routeros.RowID])
		}
	}
	k.rolled.Add(1)
}

// fakeProv is the billing side of the job.
type fakeProv struct {
	mu      sync.Mutex
	held    int
	freed   int
	bypass  []bool
	recs    int
	holding bool
}

func (p *fakeProv) HoldSite(string) func() {
	p.mu.Lock()
	p.held++
	p.holding = true
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { p.mu.Lock(); p.freed++; p.holding = false; p.mu.Unlock() })
	}
}
func (p *fakeProv) ReconcileSite(context.Context, string) (billing.ProvisionRun, error) {
	p.mu.Lock()
	p.recs++
	p.mu.Unlock()
	return billing.ProvisionRun{}, nil
}
func (p *fakeProv) SetHotspotBypass(_ string, on bool) {
	p.mu.Lock()
	p.bypass = append(p.bypass, on)
	p.mu.Unlock()
}

type announced struct {
	mu   sync.Mutex
	text []string
}

func (a *announced) add(_, text, _ string) { a.mu.Lock(); a.text = append(a.text, text); a.mu.Unlock() }
func (a *announced) all() string           { a.mu.Lock(); defer a.mu.Unlock(); return strings.Join(a.text, "\n") }

func fastTiming() ProvTiming {
	return ProvTiming{Poll: 3 * time.Millisecond, WaitRouter: 300 * time.Millisecond, Backup: 600 * time.Millisecond, Arm: 600 * time.Millisecond,
		Evidence: 400 * time.Millisecond, OnlineWindow: 10 * time.Minute, JoinWindow: 30 * time.Minute, Rollback: 1500 * time.Millisecond}
}

type provRig struct {
	*fixture
	sd   store.Site
	mock *routeros.Mock
	kit  *kitSim
	prov *fakeProv
	ann  *announced
}

// migrateRig is a hotspot router with two live tenants (checkSite's healthy router).
func migrateRig(t *testing.T) *provRig {
	f, s, m := checkSite(t, nil)
	r := &provRig{fixture: f, sd: s, mock: m, prov: &fakeProv{}, ann: &announced{}}
	f.m.SetProvisioner(r.prov)
	f.m.SetAnnouncer(r.ann.add)
	f.m.SetProvTiming(fastTiming())
	r.kit = startKit(t, m)
	return r
}

func (r *provRig) run(o ProvOptions) ProvJob {
	r.t.Helper()
	if err := r.m.StartProvision(r.sd.ID, o); err != nil {
		r.t.Fatal(err)
	}
	r.m.WaitProvision(r.sd.ID)
	j, _ := r.m.ProvJobOf(r.sd.ID)
	return j
}

func (r *provRig) migrate() ProvJob {
	return r.run(ProvOptions{Trigger: "migrate", StartNow: true, Actor: "tester"})
}

func (r *provRig) armedRows() []routeros.Row {
	var out []routeros.Row
	for _, x := range r.mock.Table(pathAddrList) {
		if x["list"] == KitListArmed {
			out = append(out, x)
		}
	}
	return out
}

func TestMigrateFullRun(t *testing.T) {
	r := migrateRig(t)
	j := r.migrate()
	if j.State != JobLive || j.Kind != KindMigrate {
		t.Fatalf("state %s kind %s err %q log %v", j.State, j.Kind, j.LastError, j.Steps)
	}
	if len(r.armedRows()) != 0 {
		t.Fatal("the markers must be gone after a pass")
	}
	for _, x := range r.mock.Table(pathAddrList) {
		if x["list"] == KitListUndo || x["list"] == KitListCmd {
			t.Fatalf("leftover marker %v", x)
		}
	}
	if r.kit.rolled.Load() != 0 {
		t.Fatal("no rollback expected")
	}
	if !j.Evidence.TunnelUp || !j.Evidence.InternetOK || j.Evidence.FlatsRequired != 1 || j.Evidence.FlatsBound != 1 {
		t.Fatalf("evidence %+v", j.Evidence)
	}
	if got := r.f().AccessMode; got != store.AccessIPoE {
		t.Fatalf("mode %s", got)
	}
	if !strings.Contains(r.ann.all(), "now on IPoE") {
		t.Fatalf("announce: %q", r.ann.all())
	}
	// the old servers are off, ftn-dhcp is on
	for _, h := range r.mock.Table("/ip/hotspot") {
		if h["disabled"] != "true" && h["disabled"] != "yes" {
			t.Fatalf("hotspot still on: %v", h)
		}
	}
	var on bool
	for _, d := range r.mock.Table("/ip/dhcp-server") {
		if d["name"] == billing.DHCPServerName && d["disabled"] != "true" && d["disabled"] != "yes" {
			on = true
		}
	}
	if !on {
		t.Fatal("ftn-dhcp not enabled")
	}
	if r.prov.held != 1 || r.prov.freed != 1 {
		t.Fatalf("lock held %d freed %d", r.prov.held, r.prov.freed)
	}
	if n := len(r.prov.bypass); n == 0 || r.prov.bypass[n-1] {
		t.Fatalf("bypass flag must end off: %v", r.prov.bypass)
	}
}

func (r *provRig) f() store.Site { return r.fixture.site(r.sd.ID) }

func (r *provRig) nowSet() bool {
	for _, x := range r.armedRows() {
		if x["comment"] == KitArmedNow {
			return true
		}
	}
	return false
}

func TestRerunIsANoOp(t *testing.T) {
	r := migrateRig(t)
	if j := r.migrate(); j.State != JobLive {
		t.Fatalf("%s %s", j.State, j.LastError)
	}
	r.mock.ResetWrites()
	j := r.run(ProvOptions{Trigger: "run", Actor: "tester"})
	if j.State != JobLive {
		t.Fatalf("rerun: %s %q %v", j.State, j.LastError, j.Steps)
	}
	if n := r.mock.Writes(); n != 0 {
		t.Fatalf("a rerun wrote %d times", n)
	}
	if r.kit.rolled.Load() != 0 {
		t.Fatal("rolled back")
	}
}

func TestA1EvidenceFailsSetsNowAndAnnounces(t *testing.T) {
	r := migrateRig(t)
	r.kit.set(func(k *kitSim) { k.noBound = true })
	j := r.migrate()
	if j.State != JobRolledBack {
		t.Fatalf("state %s err %q", j.State, j.LastError)
	}
	if r.kit.rolled.Load() != 1 {
		t.Fatal("the kit must have rolled back once")
	}
	if got := r.f().AccessMode; got != store.AccessHotspot {
		t.Fatalf("mode %s: the server must go back to hotspot", got)
	}
	txt := r.ann.all()
	if !strings.Contains(txt, "rolled back") || !strings.Contains(txt, "1 of 1") && !strings.Contains(txt, "0 of 1") {
		t.Fatalf("announce: %q", txt)
	}
	if n := len(r.prov.bypass); n == 0 || !r.prov.bypass[n-1] {
		t.Fatalf("the bypass must be switched on after a rollback: %v", r.prov.bypass)
	}
	if r.prov.held != r.prov.freed {
		t.Fatal("the lock leaked")
	}
}

func TestFailureAtEachStepSetsNowAndWritesNothingMore(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"dhcp pool", "/ip/pool"},
		{"dhcp leases", "/ip/dhcp-server/lease"},
		{"firewall nat", "/ip/firewall/nat"},
		{"firewall filter", "/ip/firewall/filter"},
		{"switch hotspot", "/ip/hotspot"},
		{"switch dhcp", "/ip/dhcp-server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := migrateRig(t)
			r.kit.set(func(k *kitSim) { k.ignoreNow = true })
			r.mock.SetWriteFail(tc.path, &routeros.APIError{Status: 400, Message: "Bad Request", Detail: "refused"})
			j := r.migrate()
			if j.State != JobFailed {
				t.Fatalf("state %s err %q steps %v", j.State, j.LastError, j.Steps)
			}
			r.mock.SetWriteFail(tc.path, nil)
			if !r.nowSet() {
				t.Fatalf("the armed marker must say now: %v", r.armedRows())
			}
			r.kit.mu.Lock()
			at := r.kit.nowWrites
			r.kit.mu.Unlock()
			if at < 0 || r.mock.Writes() != at {
				t.Fatalf("writes after the marker: at=%d now=%d", at, r.mock.Writes())
			}
			if !strings.Contains(r.ann.all(), "Router") {
				t.Fatalf("announce: %q", r.ann.all())
			}
		})
	}
}

func TestUnreachableMidRunLeavesTheArmedMarker(t *testing.T) {
	r := migrateRig(t)
	r.kit.set(func(k *kitSim) { k.dieOnSwitch = true })
	j := r.migrate()
	r.mock.SetFail(nil)
	if j.State == JobLive || j.State == JobRolledBack {
		t.Fatalf("state %s", j.State)
	}
	rows := r.armedRows()
	if len(rows) != 1 || rows[0]["comment"] == KitArmedNow {
		t.Fatalf("the armed marker must stay as it was (the router's own timer rolls back): %v", rows)
	}
	if !j.Armed {
		t.Fatal("the job must remember it armed")
	}
}

func TestBackupAndArmFailuresChangeNothing(t *testing.T) {
	for _, tc := range []string{"backup", "tick"} {
		t.Run(tc, func(t *testing.T) {
			r := migrateRig(t)
			r.kit.set(func(k *kitSim) { k.noBackup = tc == "backup"; k.noTick = tc == "tick" })
			j := r.migrate()
			if j.State != JobFailed {
				t.Fatalf("state %s %q", j.State, j.LastError)
			}
			for _, h := range r.mock.Table("/ip/hotspot") {
				if h["disabled"] == "true" || h["disabled"] == "yes" {
					t.Fatal("the hotspot must be untouched")
				}
			}
			for _, d := range r.mock.Table("/ip/dhcp-server") {
				if d["name"] == billing.DHCPServerName {
					t.Fatal("nothing may be staged before the kit is armed")
				}
			}
		})
	}
}

// freshRig is a joined router that has nothing on it but the basics.
func freshRig(t *testing.T) *provRig {
	f := newFixture(t, nil)
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block F")
	if err := f.m.UpdateSite(s.ID, SiteInput{Name: "Block F", Provision: true, BenchPort: "ether5"}); err != nil {
		t.Fatal(err)
	}
	m := f.startedRouter(s.ID)
	m.SeedOne("/system/resource", R{"version": "7.25.1 (stable)", "board-name": "RB5009UG+S+"})
	m.SeedOne("/system/device-mode", R{"mode": "home", "flagged": "false", "scheduler": "true", "fetch": "true", "hotspot": "true", "container": "true"})
	m.SeedOne("/ip/dns", R{"allow-remote-requests": "false"})
	m.SeedOne("/ip/traffic-flow", R{"enabled": "no", "interfaces": ""})
	m.SeedOne("/ipv6/settings", R{"disable-ipv6": "true"})
	m.SeedRow("/ip/address", R{"address": "10.99.0.2/22", "interface": "wg-ftn", "disabled": "false"})
	for _, n := range []string{"ether1", "ether2", "ether5", "sfp-sfpplus1"} {
		m.SeedRow("/interface", R{"name": n})
	}
	m.SeedRow("/interface/list/member", R{"list": "WAN", "interface": "ether1"})
	m.SeedRow("/ip/service", R{"name": "www", "address": "10.99.0.1/32", "disabled": "false"})
	m.SeedRow("/user/group", R{"name": "ftn-api", "policy": "read,write,test,sensitive,api,rest-api,reboot,!ftp,!policy"})
	m.SeedRow("/ip/route", R{"dst-address": "0.0.0.0/0", "gateway": "192.168.1.1", "immediate-gw": "192.168.1.1%ether1"})
	m.SeedRow("/ip/dhcp-client", R{"interface": "ether1", "disabled": "false"})
	r := &provRig{fixture: f, sd: f.site(s.ID), mock: m, prov: &fakeProv{}, ann: &announced{}}
	f.m.SetProvisioner(r.prov)
	f.m.SetAnnouncer(r.ann.add)
	f.m.SetProvTiming(fastTiming())
	r.kit = startKit(t, m)
	return r
}

func TestFreshFullRun(t *testing.T) {
	r := freshRig(t)
	j := r.run(ProvOptions{Trigger: "run", Actor: "tester"})
	if j.State != JobLive || j.Kind != KindFresh {
		t.Fatalf("state %s kind %s err %q blockers %v steps %v", j.State, j.Kind, j.LastError, j.Blockers, j.Steps)
	}
	if got := r.f().AccessMode; got != store.AccessIPoE {
		t.Fatalf("mode %s", got)
	}
	var srv bool
	for _, d := range r.mock.Table("/ip/dhcp-server") {
		if d["name"] == billing.DHCPServerName && d["disabled"] != "true" && d["disabled"] != "yes" {
			srv = true
		}
	}
	if !srv {
		t.Fatal("ftn-dhcp must be on")
	}
	if len(r.mock.Table("/ip/pool")) == 0 || len(r.mock.Table("/ip/dhcp-server/network")) == 0 || len(r.mock.Table("/interface/bridge")) == 0 {
		t.Fatal("the customer network must be built")
	}
	if r.prov.recs == 0 {
		t.Fatal("the flats are staged by the first reconcile")
	}
	if len(r.armedRows()) != 0 {
		t.Fatal("nothing existed to switch off: no markers expected")
	}
	if !strings.Contains(r.ann.all(), "set up and live") {
		t.Fatalf("announce: %q", r.ann.all())
	}
	r.mock.ResetWrites()
	if j := r.run(ProvOptions{Trigger: "run", Actor: "tester"}); j.State != JobLive || r.mock.Writes() != 0 {
		t.Fatalf("rerun: %s, %d writes", j.State, r.mock.Writes())
	}
}

func testWriter(m *routeros.Mock) *provWriter {
	return &provWriter{rt: m, insp: m, sc: provScope{iface: "br-hotspot", bridge: "br-hotspot", gateway: "10.5.50.1/24", network: "10.5.50.0/24",
		wanList: "WAN", ports: map[string]bool{"ether2": true}}}
}

func TestProvWriterRefusesForbiddenPaths(t *testing.T) {
	m := routeros.NewMock()
	w := testWriter(m)
	ctx := context.Background()
	m.SeedRow("/ip/address", R{"address": "192.168.1.5/24", "interface": "ether1", "comment": "wan"})
	m.SeedRow("/user", R{"name": "admin"})
	m.ResetWrites()
	adds := []struct {
		path string
		body map[string]string
	}{
		{"/user", R{"name": "x", "group": "full", "password": "p"}},
		{"/ip/service", R{"name": "api"}},
		{"/system/script", R{"name": "x", "source": "/quit"}},
		{"/system/scheduler", R{"name": "x", "on-event": "x"}},
		{"/interface/wireguard", R{"name": "wg1"}},
		{"/interface/wireguard/peers", R{"public-key": "k"}},
		{"/ip/route", R{"dst-address": "0.0.0.0/0", "gateway": "1.2.3.4", "comment": "ftn: x"}},
		{"/ip/address", R{"address": "192.168.9.1/24", "interface": "ether1", "comment": "ftn: x"}},
		{"/ip/address", R{"address": "10.5.50.1/24", "interface": "ether1", "comment": "ftn: x"}}, // wrong interface
		{"/ip/firewall/filter", R{"chain": "input", "action": "accept", "comment": "not ours"}},
		{"/ip/firewall/filter", R{"chain": "input", "action": "accept", "comment": "ftn: something else"}},
		{"/ip/firewall/address-list", R{"list": "whitelist", "address": "1.1.1.1", "comment": "ftn: x"}},
		{"/ip/firewall/address-list", R{"list": KitListArmed, "address": "1.2.3.4", "comment": "0"}}, // not the marker address
		{"/ip/hotspot/user", R{"name": "x"}},
		{"/ip/dhcp-server", R{"name": "other", "interface": "ether1", "comment": "ftn: x"}},
		{"/interface/bridge/port", R{"bridge": "br-hotspot", "interface": "ether1", "comment": "ftn: x"}}, // not a customer port
		{"/ppp/secret", R{"name": "x"}},
		{"/file", R{"name": "x"}},
	}
	for _, a := range adds {
		if err := w.add(ctx, a.path, a.body); !errors.Is(err, ErrProvNotAllowed) {
			t.Errorf("add %s %v: %v", a.path, a.body, err)
		}
	}
	if m.Writes() != 0 {
		t.Fatalf("%d refused writes reached the router", m.Writes())
	}
	rows := m.Table("/ip/address")
	wan := rows[0]
	if err := w.set(ctx, "/ip/address", wan, map[string]string{"disabled": "yes"}); !errors.Is(err, ErrProvNotAllowed) {
		t.Errorf("set wan address: %v", err)
	}
	if err := w.remove(ctx, "/ip/address", wan); !errors.Is(err, ErrProvNotAllowed) {
		t.Errorf("remove wan address: %v", err)
	}
	user := m.Table("/user")[0]
	if err := w.remove(ctx, "/user", user); !errors.Is(err, ErrProvNotAllowed) {
		t.Errorf("remove user: %v", err)
	}
	if err := w.set(ctx, "/ip/hotspot", R{routeros.RowID: "*1", "interface": "ether1"}, map[string]string{"disabled": "yes"}); !errors.Is(err, ErrProvNotAllowed) {
		t.Errorf("hotspot on another interface: %v", err)
	}
	for _, c := range []struct {
		path string
		body map[string]string
	}{
		{"/user/set", R{"password": "x"}},
		{"/ip/service/set", R{"disabled": "yes"}},
		{"/system/reset-configuration", nil},
		{"/system/reboot", R{"x": "y"}},
		{"/ip/dns/set", R{"servers": "1.1.1.1"}},
		{"/ip/traffic-flow/set", R{"cache-entries": "1"}},
	} {
		if err := w.call(ctx, c.path, c.body); !errors.Is(err, ErrProvNotAllowed) {
			t.Errorf("call %s %v: %v", c.path, c.body, err)
		}
	}
	if m.Writes() != 0 || len(m.Calls()) != 0 {
		t.Fatalf("writes %d calls %d", m.Writes(), len(m.Calls()))
	}
}

func TestProvWriterAllowsTheJobsOwnRows(t *testing.T) {
	m := routeros.NewMock()
	w := testWriter(m)
	ctx := context.Background()
	for _, a := range []struct {
		path string
		body map[string]string
	}{
		{"/ip/address", R{"address": "10.5.50.1/24", "interface": "br-hotspot", "comment": "ftn: customer gateway"}},
		{"/ip/pool", R{"name": "ftn-unreg", "ranges": "10.5.50.200-10.5.50.250", "comment": "ftn: pool"}},
		{"/ip/dhcp-server", R{"name": "ftn-dhcp", "interface": "br-hotspot", "address-pool": "ftn-unreg", "disabled": "yes", "comment": "ftn: ipoe"}},
		{"/ip/firewall/address-list", R{"list": KitListArmed, "address": KitArmedAddress, "comment": "0"}},
		{"/ip/firewall/address-list", R{"list": KitListCmd, "address": KitCmdAddress, "comment": KitCmdBackup}},
		{"/ip/firewall/address-list", R{"list": KitListUndo, "address": KitUndoAddress(1), "comment": KitUndoHotspot("hotspot1")}},
		{"/queue/simple", R{"name": "ftn-K7Q2MX", "target": "10.5.50.60/32", "comment": "ftn:acct:K7Q2MX Flat 3B"}},
		{"/ip/hotspot/ip-binding", R{"mac-address": "AA:BB:CC:DD:EE:01", "type": "bypassed", "comment": "ftn:acct:K7Q2MX Flat 3B"}},
	} {
		if err := w.add(ctx, a.path, a.body); err != nil {
			t.Errorf("add %s: %v", a.path, err)
		}
	}
	if err := w.call(ctx, "/ip/dns/set", R{"allow-remote-requests": "yes"}); err != nil {
		t.Error(err)
	}
	if err := w.call(ctx, "/system/reboot", nil); err != nil {
		t.Error(err)
	}
}

func TestIPoERulesAreStructured(t *testing.T) {
	plan, err := PlanSubnet("10.5.50.0/24")
	if err != nil {
		t.Fatal(err)
	}
	filter, nat := ipoeRuleSet(plan, "203.0.113.5", 443)
	if len(filter) == 0 || len(nat) == 0 {
		t.Fatal("no rules")
	}
	for _, r := range append(append([]ipoeRule(nil), filter...), nat...) {
		if !strings.HasPrefix(r.Comment, "ftn: ipoe") && !strings.HasPrefix(r.Comment, "ftn: customer dns") && !strings.HasPrefix(r.Comment, "ftn: no WAN DNS") {
			t.Errorf("rule outside the allowlist's comments: %q", r.Comment)
		}
		body := r.Body("br-hotspot")
		if strings.Contains(strings.Join(mapVals(body), " "), "$ifc") {
			t.Errorf("unrendered interface in %v", body)
		}
		if body["comment"] != r.Comment {
			t.Errorf("comment missing from the body: %v", body)
		}
	}
	// every rule the job builds passes the writer
	m := routeros.NewMock()
	w := testWriter(m)
	for _, r := range filter {
		if err := w.add(context.Background(), r.Path(), r.Body("br-hotspot")); err != nil {
			t.Errorf("%q: %v", r.Comment, err)
		}
	}
	for _, r := range nat {
		if err := w.add(context.Background(), r.Path(), r.Body("br-hotspot")); err != nil {
			t.Errorf("%q: %v", r.Comment, err)
		}
	}
}

func mapVals(m map[string]string) []string {
	var o []string
	for _, v := range m {
		o = append(o, v)
	}
	return o
}

func TestClassificationTable(t *testing.T) {
	t.Run("live devices without serving intent need a decision", func(t *testing.T) {
		r := migrateRig(t)
		r.mock.ResetWrites()
		j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
		if j.State != JobNeedsAction || j.Kind != KindMigrate || j.LiveDevices != 2 {
			t.Fatalf("%s %s live=%d err=%q", j.State, j.Kind, j.LiveDevices, j.LastError)
		}
		if r.mock.Writes() != 0 {
			t.Fatal("classification must not write to the router")
		}
	})
	t.Run("serving intent with live devices waits for the operator", func(t *testing.T) {
		r := migrateRig(t)
		r.m.SetServingCustomers(r.sd.ID, true)
		r.mock.ResetWrites()
		j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
		if j.State != JobReady || j.Kind != KindMigrate {
			t.Fatalf("%s %s %q", j.State, j.Kind, j.LastError)
		}
		if r.mock.Writes() != 0 {
			t.Fatal("ready must not write to the router")
		}
		if j2, _ := r.m.ProvJobOf(r.sd.ID); !r.m.SetupBusy(r.sd.ID) && j2.State != JobReady {
			t.Fatal("state lost")
		}
	})
	t.Run("an empty router is fresh", func(t *testing.T) {
		r := freshRig(t)
		j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
		if j.Kind != KindFresh || j.LiveDevices != 0 {
			t.Fatalf("%s %d", j.Kind, j.LiveDevices)
		}
	})
	t.Run("a router that already serves is a rerun", func(t *testing.T) {
		r := migrateRig(t)
		if j := r.migrate(); j.State != JobLive {
			t.Fatal(j.State)
		}
		j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
		if j.Kind != KindFresh || j.State != JobLive {
			t.Fatalf("%s %s", j.Kind, j.State)
		}
	})
	t.Run("a router that does not answer waits", func(t *testing.T) {
		r := migrateRig(t)
		r.mock.SetFail(errors.New("no route"))
		j := r.run(ProvOptions{Trigger: "run", Actor: "t"})
		r.mock.SetFail(nil)
		if j.State != JobWaiting {
			t.Fatalf("%s", j.State)
		}
	})
}

func TestAutoStartOnlyInsideTheJoinWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance time.Duration
		serving bool
		want    string
	}{
		{"inside", 5 * time.Minute, true, JobLive},
		{"outside", 31 * time.Minute, true, JobReady},
		{"not serving", time.Minute, false, JobNeedsAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := migrateRig(t)
			r.m.SetServingCustomers(r.sd.ID, tc.serving)
			r.advance(tc.advance)
			j := r.run(ProvOptions{Trigger: "join", Actor: ActorSystem})
			if j.State != tc.want {
				t.Fatalf("%s, want %s (%q)", j.State, tc.want, j.LastError)
			}
		})
	}
}

func TestRestartRules(t *testing.T) {
	t.Run("an unarmed job starts over", func(t *testing.T) {
		r := migrateRig(t)
		r.m.SetServingCustomers(r.sd.ID, true)
		r.m.saveJob(r.sd.ID, ProvJob{State: JobStaging, Kind: KindMigrate, StartNow: true, StartedAt: r.now})
		r.m.resumeProvisioning()
		r.m.WaitProvision(r.sd.ID)
		if j, _ := r.m.ProvJobOf(r.sd.ID); j.State != JobLive {
			t.Fatalf("%s %q", j.State, j.LastError)
		}
	})
	t.Run("an armed job is never resumed, it is rolled back", func(t *testing.T) {
		r := migrateRig(t)
		r.mock.SeedRow(pathAddrList, R{"list": KitListArmed, "address": KitArmedAddress, "comment": "4"})
		r.m.saveJob(r.sd.ID, ProvJob{State: JobSwitching, Kind: KindMigrate, Armed: true, StartedAt: r.now})
		r.m.resumeProvisioning()
		deadline := time.Now().Add(3 * time.Second)
		for r.kit.rolled.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if r.kit.rolled.Load() == 0 {
			t.Fatal("the router was not told to roll back")
		}
		for time.Now().Before(deadline) {
			if j, _ := r.m.ProvJobOf(r.sd.ID); j.State == JobRolledBack || j.State == JobFailed {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		r.m.WaitProvision(r.sd.ID)
	})
}

func TestConfirmCutoverUnderstandsTheKitMarkers(t *testing.T) {
	r := migrateRig(t)
	r.mock.SeedRow(pathAddrList, R{"list": KitListArmed, "address": KitArmedAddress, "comment": "3"})
	r.mock.SeedRow(pathAddrList, R{"list": KitListUndo, "address": KitUndoAddress(1), "comment": KitUndoHotspot("hotspot1")})
	if p, ok := r.m.CutoverPending(context.Background(), r.sd.ID); !p || !ok {
		t.Fatal("armed rows mean a cutover is pending")
	}
	r.kit.Stop()
	r.mock.SeedRow("/ip/dhcp-server", R{"name": billing.DHCPServerName, "interface": "br-hotspot", "disabled": "false"})
	if err := r.m.ConfirmCutover(context.Background(), r.sd.ID); err != nil {
		t.Fatal(err)
	}
	for _, x := range r.mock.Table(pathAddrList) {
		if x["list"] == KitListArmed || x["list"] == KitListUndo {
			t.Fatalf("marker left: %v", x)
		}
	}
	if p, _ := r.m.CutoverPending(context.Background(), r.sd.ID); p {
		t.Fatal("nothing pending after the confirm")
	}
}

func TestRollbackNowIsIdempotentAndSetsTheMarker(t *testing.T) {
	r := migrateRig(t)
	r.kit.set(func(k *kitSim) { k.ignoreNow = true })
	r.mock.SeedRow(pathAddrList, R{"list": KitListArmed, "address": KitArmedAddress, "comment": "2"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.m.RollbackNow(r.sd.ID, "olga"); err != nil {
			t.Error(err)
		}
		if err := r.m.RollbackNow(r.sd.ID, "olga"); err != nil { // must not wait for the first one's follow-up
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the second rollback request blocked")
	}
	if !r.nowSet() {
		t.Fatalf("%v", r.armedRows())
	}
	r.m.WaitProvision(r.sd.ID)
}
