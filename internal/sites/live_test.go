package sites

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// rec wraps a mock router and records what the live views read.
type rec struct {
	routeros.Client
	mu     sync.Mutex
	reads  map[string]int
	writes int
	delay  time.Duration
	block  bool // wait for the context instead of answering
}

func (r *rec) note(path string) {
	r.mu.Lock()
	if r.reads == nil {
		r.reads = map[string]int{}
	}
	r.reads[strings.TrimRight(path, "/")]++
	r.mu.Unlock()
}

func (r *rec) wait(ctx context.Context) error {
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	return nil
}

func (r *rec) One(ctx context.Context, path string) (routeros.Row, error) {
	r.note(path)
	if err := r.wait(ctx); err != nil {
		return nil, err
	}
	return r.Client.(routeros.Inspector).One(ctx, path)
}

func (r *rec) Rows(ctx context.Context, path string, q map[string]string) ([]routeros.Row, error) {
	r.note(path)
	if err := r.wait(ctx); err != nil {
		return nil, err
	}
	return r.Client.Rows(ctx, path, q)
}

func (r *rec) Call(ctx context.Context, path string, b map[string]string) error {
	r.mu.Lock()
	r.writes++
	r.mu.Unlock()
	return r.Client.(routeros.Inspector).Call(ctx, path, b)
}
func (r *rec) AddRow(ctx context.Context, p string, b map[string]string) (string, error) {
	r.mu.Lock()
	r.writes++
	r.mu.Unlock()
	return r.Client.AddRow(ctx, p, b)
}
func (r *rec) SetRow(ctx context.Context, p, id string, d map[string]string) error {
	r.mu.Lock()
	r.writes++
	r.mu.Unlock()
	return r.Client.SetRow(ctx, p, id, d)
}
func (r *rec) RemoveRow(ctx context.Context, p, id string) error {
	r.mu.Lock()
	r.writes++
	r.mu.Unlock()
	return r.Client.RemoveRow(ctx, p, id)
}

func (r *rec) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for p := range r.reads {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (r *rec) count(path string) int { r.mu.Lock(); defer r.mu.Unlock(); return r.reads[path] }

// liveRig is a started, joined router whose client is a recorder around a seeded mock.
func liveRig(t *testing.T) (*fixture, store.Site, *routeros.Mock, *rec) {
	t.Helper()
	f := newFixture(t, nil)
	f.m.autoFixDelay = time.Hour
	s := f.joined("Block A")
	m := routeros.NewMock()
	m.SeedLive()
	r := &rec{Client: m}
	f.m.opt.NewClient = func(store.Site) routeros.Client { return r }
	f.start()
	f.m.Reload(s.ID)
	return f, f.site(s.ID), m, r
}

func TestLiveReadsOnlyTheFixedPaths(t *testing.T) {
	f, s, _, r := liveRig(t)
	ctx := context.Background()
	if _, err := f.m.LiveRouter(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.LiveInterfaces(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.LiveClientTables(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), LiveReadPaths...)
	sort.Strings(want)
	if got := r.paths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("a live view read a path that is not on the fixed list (or one on the list is never read)\n got  %v\n want %v", got, want)
	}
	if r.writes != 0 {
		t.Fatalf("live views wrote %d times", r.writes)
	}
}

var secretish = regexp.MustCompile(`(?i)password|private|secret|token|passphrase`)

func TestLiveAnswersCarryNoSecrets(t *testing.T) {
	f, s, m, _ := liveRig(t)
	// A router that puts secrets in the rows we read: they must never come out.
	m.SeedRow("/interface", routeros.Row{"name": "wg-bad", "type": "wg", "private-key": "PRIVKEY-123", "password": "PW-HUNTER2", "rx-byte": "1", "tx-byte": "1"})
	m.SeedRow("/ip/dhcp-server/lease", routeros.Row{"address": "10.5.50.90", "mac-address": "AA:AA:AA:00:00:01", "status": "bound", "password": "PW-HUNTER2", "comment": "token=TOK-999"})
	m.SeedRow("/queue/simple", routeros.Row{"name": "ftn-ZZ", "target": "10.5.50.91/32", "max-limit": "1000000/1000000", "secret": "SEC-777"})
	m.SeedOne("/system/resource", routeros.Row{"version": "7.25.1", "board-name": "RB5009", "uptime": "1h", "cpu-count": "4", "cpu-load": "1", "total-memory": "1000", "free-memory": "10", "secret": "SEC-777"})
	ctx := context.Background()
	lv, _ := f.m.LiveRouter(ctx, s.ID)
	iv, _ := f.m.LiveInterfaces(ctx, s.ID)
	tb, _ := f.m.LiveClientTables(ctx, s.ID)
	cl := MergeClients(ClientInput{Now: f.now, Tables: tb})
	for name, v := range map[string]any{"live": lv, "interfaces": iv.Items, "clients": cl} {
		b, _ := json.Marshal(v)
		var keys []string
		var walk func(x any)
		walk = func(x any) {
			switch t := x.(type) {
			case map[string]any:
				for k, y := range t {
					keys = append(keys, k)
					walk(y)
				}
			case []any:
				for _, y := range t {
					walk(y)
				}
			}
		}
		var anyv any
		_ = json.Unmarshal(b, &anyv)
		walk(anyv)
		for _, k := range keys {
			if secretish.MatchString(k) {
				t.Errorf("%s: field %q looks like a secret", name, k)
			}
		}
		for _, bad := range []string{"PRIVKEY-123", "PW-HUNTER2", "TOK-999", "SEC-777"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s leaks %s: %s", name, bad, b)
			}
		}
	}
	if len(cl) == 0 || len(iv.Items) == 0 || !lv.Reachable {
		t.Fatalf("empty views: %d %d %v", len(cl), len(iv.Items), lv.Reachable)
	}
}

func TestLiveCacheStaleAndSingleFlight(t *testing.T) {
	f, s, m, r := liveRig(t)
	ctx := context.Background()
	a, _ := f.m.LiveRouter(ctx, s.ID)
	if !a.Reachable || a.System.Model != "RB5009UG+S+IN" || a.System.Serial != "HF1234ABCDE" || a.System.CPULoadPct != 7 ||
		a.System.MemUsed != 1073741824-805306368 || a.System.UptimeS != 3*86400+4*3600+5*60+6 || a.System.TempC == nil || *a.System.TempC != 41 || !a.WAN.Running {
		t.Fatalf("%+v", a)
	}
	// within 5 s: no new read
	f.advance(9 * time.Second)
	f.m.LiveRouter(ctx, s.ID)
	if n := r.count("/system/resource"); n != 1 {
		t.Fatalf("read again inside the cache window: %d", n)
	}
	f.advance(2 * time.Second)
	f.m.LiveRouter(ctx, s.ID)
	if n := r.count("/system/resource"); n != 2 {
		t.Fatalf("no new read after the window: %d", n)
	}
	// the router fails: the last answer is served, marked unreachable, for 60 s
	m.SetFail(errors.New("boom"))
	f.advance(10 * time.Second)
	b, err := f.m.LiveRouter(ctx, s.ID)
	if err != nil || b.Reachable || b.System.Model != "RB5009UG+S+IN" {
		t.Fatalf("stale: %+v %v", b, err)
	}
	f.advance(121 * time.Second)
	c, _ := f.m.LiveRouter(ctx, s.ID)
	if c.Reachable || c.System.Model != "" {
		t.Fatalf("after 120 s nothing may be served: %+v", c)
	}
	// back again
	m.SetFail(nil)
	d, _ := f.m.LiveRouter(ctx, s.ID)
	if !d.Reachable || d.System.Model == "" {
		t.Fatalf("%+v", d)
	}

	// single flight: many callers at once share one read
	f.advance(10 * time.Second)
	r.delay = 60 * time.Millisecond
	before := r.count("/system/resource")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.m.LiveRouter(ctx, s.ID) }()
	}
	wg.Wait()
	if n := r.count("/system/resource") - before; n != 1 {
		t.Fatalf("12 concurrent callers made %d reads", n)
	}
}

func TestLiveReadIsCutAtTheTimeout(t *testing.T) {
	f, s, _, r := liveRig(t)
	old := liveTimeout
	liveTimeout = 150 * time.Millisecond
	defer func() { liveTimeout = old }()
	r.block = true
	start := time.Now()
	lv, err := f.m.LiveRouter(context.Background(), s.ID)
	if err != nil || lv.Reachable {
		t.Fatalf("%+v %v", lv, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestLiveInterfaceRatesFromCounterDeltas(t *testing.T) {
	f, s, m, _ := liveRig(t)
	ctx := context.Background()
	first, _ := f.m.LiveInterfaces(ctx, s.ID)
	for _, it := range first.Items {
		if it.RxBps != nil || it.TxBps != nil {
			t.Fatalf("first read must have null rates: %+v", it)
		}
	}
	var id string
	for _, r := range m.Table("/interface") {
		if r["name"] == "ether1" {
			id = r[routeros.RowID]
		}
	}
	f.advance(10 * time.Second)
	// +1,250,000 bytes in 10 s = 1 Mbit/s; tx +250,000 = 200 kbit/s
	if err := m.SetRow(context.Background(), "/interface", id, map[string]string{"rx-byte": "2250000", "tx-byte": "450000"}); err != nil {
		t.Fatal(err)
	}
	second, _ := f.m.LiveInterfaces(ctx, s.ID)
	byName := map[string]Interface{}
	for _, it := range second.Items {
		byName[it.Name] = it
	}
	e1 := byName["ether1"]
	if e1.RxBps == nil || *e1.RxBps != 1_000_000 || *e1.TxBps != 200_000 || e1.Role != RoleWAN || e1.RxBytes != 2250000 {
		t.Fatalf("%+v", e1)
	}
	if o := byName["sfp-sfpplus1"]; o.Role != RoleOLT || o.RxBps == nil || *o.RxBps != 0 {
		t.Fatalf("%+v", o)
	}
	if byName["br-hotspot"].Role != RoleCustomer || len(byName["br-hotspot"].Addresses) != 1 || byName["wg-ftn"].Role != RoleTunnel {
		t.Fatalf("%+v", byName)
	}
	// a counter that went back (reboot) gives null, not a huge number
	f.advance(10 * time.Second)
	m.SetRow(context.Background(), "/interface", id, map[string]string{"rx-byte": "10", "tx-byte": "10"})
	third, _ := f.m.LiveInterfaces(ctx, s.ID)
	for _, it := range third.Items {
		if it.Name == "ether1" && (it.RxBps != nil || it.TxBps != nil) {
			t.Fatalf("%+v", it)
		}
	}
}

func TestClientsMergeAndSearch(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	f, s, _, _ := liveRig(t)
	tb, err := f.m.LiveClientTables(context.Background(), s.ID)
	if err != nil || !tb.Reachable || len(tb.Leases) != 3 || len(tb.Queues) != 2 {
		t.Fatalf("%+v %v", tb, err)
	}
	all := MergeClients(ClientInput{Now: now, Tables: tb,
		Accounts: []ClientAccount{
			{ID: "a1", Code: "K7Q2MX", Flat: "Flat 3B", Tenant: "Wanjiru", State: "active", MAC: "aa:bb:cc:dd:ee:01", IP: "10.5.50.60", LeaseStatus: "bound", LastSeen: now.Add(-time.Minute), InSync: true, UsageToday: 5, UsageMonth: 9, LiveDown: 4e6, LiveUp: 1e6, LiveAt: now.Add(-10 * time.Second)},
			{ID: "a2", Code: "P4R8TN", Flat: "Flat 10A", Tenant: "Otieno", State: "expired", MAC: "AA:BB:CC:DD:EE:02", IP: "10.5.50.61", InSync: true},
			{ID: "a3", Code: "ZZ", Flat: "Flat 2", State: "new", InSync: true},
		},
		Devices: []ClientDevice{{MAC: "3C:07:54:00:00:09", IP: "10.5.50.15", Online: true, LastSeen: now}, {MAC: "02:11:22:33:44:55", Vendor: "", LastSeen: now.Add(-48 * time.Hour)}},
	})
	if got := []string{all[0].Flat, all[1].Flat, all[2].Flat}; !reflect.DeepEqual(got, []string{"Flat 2", "Flat 3B", "Flat 10A"}) {
		t.Fatalf("order %v", got)
	}
	var f3, f10 Client
	for _, c := range all {
		switch c.Code {
		case "K7Q2MX":
			f3 = c
		case "P4R8TN":
			f10 = c
		}
	}
	if f3.HostName != "Brians-iPhone" || f3.Port != "sfp-sfpplus1" || f3.QueueLimit != "20M/20M" || !f3.Online || f3.DownBps != 4e6 || f3.UsageMonthBytes != 9 || f3.LeaseStatus != "bound" || f3.Vendor != "Private address (random)" {
		t.Fatalf("%+v", f3)
	}
	if f10.LeaseStatus != "waiting" || f10.QueueLimit != "2M/2M" || f10.Online || f10.State != "expired" {
		t.Fatalf("%+v", f10)
	}
	n := CountClients(all)
	if n.Flats != 3 || n.Unregistered != 2 || n.Online != 2 {
		t.Fatalf("%+v", n)
	}
	count := func(q, kind, online string) int { return len(FilterClients(all, q, kind, online)) }
	// name, flat, code, IP, MAC (any separator, partial), vendor, host name
	for q, want := range map[string]int{
		"wanjiru": 1, "FLAT 10": 1, "k7q2": 1, "10.5.50.61": 1, "aa:bb:cc:dd:ee:02": 1, "aabbccddee02": 1, "ee:02": 1, "apple": 1,
		"brians": 1, "nomatch": 0, "": 5,
	} {
		if got := count(q, "", ""); got != want {
			t.Errorf("q=%q: %d, want %d", q, got, want)
		}
	}
	if count("", "flats", "") != 3 || count("", "unregistered", "") != 2 || count("", "", "true") != 2 || count("", "flats", "true") != 1 || count("", "", "false") != 3 {
		t.Fatal("kind/online filters")
	}
}
