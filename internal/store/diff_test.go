package store

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// memBackend is an in-memory Backend that records commits and can fail.
type memBackend struct {
	mu       sync.Mutex
	last     *Data
	commits  []*ChangeSet
	failErr  error // returned by the next Commit (once)
	failAll  error // returned by every Commit until cleared
	loadErr  error
	loadGate chan struct{} // when set, Load waits for it to be closed
}

func (m *memBackend) Kind() string { return "mem" }
func (m *memBackend) Close() error { return nil }
func (m *memBackend) Load(context.Context) (*Data, error) {
	m.mu.Lock()
	gate := m.loadGate
	m.mu.Unlock()
	if gate != nil {
		<-gate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	d := &Data{}
	if m.last != nil {
		c := cloneData(m.last)
		d = &c
	}
	d.init()
	return d, nil
}
func (m *memBackend) Commit(_ context.Context, d *Data, cs *ChangeSet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAll != nil {
		return m.failAll
	}
	if m.failErr != nil {
		e := m.failErr
		m.failErr = nil
		return e
	}
	c := cloneData(d)
	m.last = &c
	m.commits = append(m.commits, cs)
	return nil
}

func openMem(t *testing.T) (*Store, *memBackend) {
	t.Helper()
	Strict = true
	t.Cleanup(func() { Strict = false })
	mb := &memBackend{}
	s, err := OpenWith(mb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, mb
}

func TestDiffUpsertDeleteAndNormalise(t *testing.T) {
	s, mb := openMem(t)
	loc := time.FixedZone("X", 5*3600)
	at := time.Date(2026, 1, 2, 3, 4, 5, 123456789, loc)
	err := s.Update(func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", Name: "A", Mode: SiteModeWG, CreatedAt: at}
		d.Accounts["a1"] = Account{ID: "a1", SiteID: "s1", Flat: "1", CreatedAt: at}
		d.Payments["K1"] = Payment{Key: "K1", PaidAt: time.Now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := mb.commits[0]
	if len(cs.Sites.Upsert) != 1 || len(cs.Accounts.Upsert) != 1 || len(cs.Payments.Upsert) != 1 || len(cs.Orders.Upsert) != 0 {
		t.Fatalf("change set: %s", cs.Summary())
	}
	// Times in live data are normalised to UTC microseconds after the update.
	s.View(func(d *Data) {
		got := d.Sites["s1"].CreatedAt
		if got.Location() != time.UTC || got.Nanosecond() != 123456000 || !got.Equal(at.Truncate(time.Microsecond)) {
			t.Fatalf("not normalised: %v", got)
		}
		if d.Payments["K1"].PaidAt.Round(0) != d.Payments["K1"].PaidAt {
			t.Fatal("monotonic clock reading kept")
		}
	})
	// A second identical update changes nothing, so nothing is committed.
	if err := s.Update(func(d *Data) error { d.Sites["s1"] = d.Sites["s1"]; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(mb.commits) != 1 {
		t.Fatalf("empty change set was committed (%d commits)", len(mb.commits))
	}
	// Delete.
	if err := s.Update(func(d *Data) error { delete(d.Accounts, "a1"); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := mb.commits[1].Accounts.Delete; len(got) != 1 || got[0] != "a1" {
		t.Fatalf("delete not detected: %+v", mb.commits[1].Accounts)
	}
}

func TestSitePkgOverridesDeepCompare(t *testing.T) {
	s, mb := openMem(t)
	s.Update(func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", PkgOverrides: map[string]PkgOverride{"p": {PriceKES: 5}}}
		return nil
	})
	// Reassigning an equal map is no change; a different one is.
	s.Update(func(d *Data) error {
		x := d.Sites["s1"]
		x.PkgOverrides = map[string]PkgOverride{"p": {PriceKES: 5}}
		d.Sites["s1"] = x
		return nil
	})
	if len(mb.commits) != 1 {
		t.Fatalf("equal overrides produced a commit")
	}
	s.Update(func(d *Data) error {
		x := d.Sites["s1"]
		x.PkgOverrides = map[string]PkgOverride{"p": {PriceKES: 6}}
		d.Sites["s1"] = x
		return nil
	})
	if len(mb.commits) != 2 || mb.commits[1].Sites.Upsert[0].PkgOverrides["p"].PriceKES != 6 {
		t.Fatal("changed overrides not committed")
	}
}

func TestRollbackOnErrorPanicAndCommitFailure(t *testing.T) {
	s, mb := openMem(t)
	s.Update(func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", Name: "orig", PkgOverrides: map[string]PkgOverride{"p": {PriceKES: 1}}}
		d.Accounts["a1"] = Account{ID: "a1", SiteID: "s1", Flat: "orig"}
		d.AddEvent(Event{Kind: "info", Msg: "one"})
		return nil
	})
	check := func(what string) {
		t.Helper()
		s.View(func(d *Data) {
			if d.Sites["s1"].Name != "orig" || d.Sites["s1"].PkgOverrides["p"].PriceKES != 1 || d.Accounts["a1"].Flat != "orig" {
				t.Fatalf("%s: edit survived: %+v %+v", what, d.Sites["s1"], d.Accounts["a1"])
			}
			if _, ok := d.Accounts["a2"]; ok {
				t.Fatalf("%s: added account survived", what)
			}
			if len(d.Events) != 1 || d.EventSeq != 1 {
				t.Fatalf("%s: events not reset: %d seq %d", what, len(d.Events), d.EventSeq)
			}
		})
	}
	mutate := func(d *Data) {
		x := d.Sites["s1"]
		x.Name = "changed"
		x.PkgOverrides = map[string]PkgOverride{"p": {PriceKES: 99}}
		d.Sites["s1"] = x
		a := d.Accounts["a1"]
		a.Flat = "changed"
		d.Accounts["a1"] = a
		d.Accounts["a2"] = Account{ID: "a2", SiteID: "s1"}
		d.AddEvent(Event{Kind: "info", Msg: "two"})
	}
	boom := errors.New("boom")
	if err := s.Update(func(d *Data) error { mutate(d); return boom }); !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	check("error")
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Fatalf("panic not propagated: %v", r)
			}
		}()
		s.Update(func(d *Data) error { mutate(d); panic("kaboom") })
	}()
	check("panic")
	mb.failErr = errors.New("disk full")
	err := s.Update(func(d *Data) error { mutate(d); return nil })
	if !errors.Is(err, ErrPersist) || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want ErrPersist, got %v", err)
	}
	check("commit failure")
	// Deleting then failing restores the row.
	mb.failErr = errors.New("again")
	s.Update(func(d *Data) error { delete(d.Accounts, "a1"); return nil })
	check("delete rolled back")
	if err := s.Update(func(d *Data) error { return nil }); err != nil || s.Healthy() != nil {
		t.Fatal("store should still be healthy")
	}
}

func TestStrictNoChangePanics(t *testing.T) {
	s, _ := openMem(t)
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "ErrNoChange after changing data") {
			t.Fatalf("want strict panic, got %v", r)
		}
		s.View(func(d *Data) {
			if len(d.Packages) != 0 {
				t.Fatal("the offending change was not rolled back")
			}
		})
	}()
	s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p"}; return ErrNoChange })
}

func TestAccountClosedAtFollowsMap(t *testing.T) {
	s, mb := openMem(t)
	s.Update(func(d *Data) error {
		d.Sites["s"] = Site{ID: "s"}
		d.Accounts["a"] = Account{ID: "a", SiteID: "s", Flat: "1", ClosedAt: time.Now()} // wrong: open map
		return nil
	})
	s.View(func(d *Data) {
		if !d.Accounts["a"].ClosedAt.IsZero() {
			t.Fatal("open account kept a ClosedAt")
		}
	})
	// Move to the closed map without setting ClosedAt: it is stamped.
	s.Update(func(d *Data) error {
		a := d.Accounts["a"]
		delete(d.Accounts, "a")
		d.ClosedAccounts["a"] = a
		return nil
	})
	s.View(func(d *Data) {
		if d.ClosedAccounts["a"].ClosedAt.IsZero() {
			t.Fatal("closed account has no ClosedAt")
		}
	})
	if n := len(mb.commits[len(mb.commits)-1].Accounts.Upsert); n != 1 {
		t.Fatalf("close should be one upsert, got %d", n)
	}
	// Reopen: back to the open map.
	s.Update(func(d *Data) error {
		a := d.ClosedAccounts["a"]
		delete(d.ClosedAccounts, "a")
		a.ClosedAt = time.Time{}
		d.Accounts["a"] = a
		return nil
	})
	s.View(func(d *Data) {
		if _, ok := d.Accounts["a"]; !ok || len(d.ClosedAccounts) != 0 {
			t.Fatal("reopen failed")
		}
	})
}

func TestEventSequenceAndCap(t *testing.T) {
	s, _ := openMem(t)
	s.Update(func(d *Data) error {
		for i := 0; i < 350; i++ {
			d.AddEvent(Event{Kind: "info", Msg: fmt.Sprint(i)})
		}
		return nil
	})
	s.View(func(d *Data) {
		if len(d.Events) != 300 || d.EventSeq != 350 || d.Events[0].Seq != 51 || d.Events[299].Seq != 350 {
			t.Fatalf("len %d seq %d first %d", len(d.Events), d.EventSeq, d.Events[0].Seq)
		}
	})
	evs, _ := s.Events(context.Background(), EventQuery{Limit: 5})
	if len(evs) != 5 || evs[0].Seq != 350 || evs[4].Seq != 346 {
		t.Fatalf("Events(): %+v", evs)
	}
}

func TestAmbiguousCommitReloads(t *testing.T) {
	s, mb := openMem(t)
	s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: "old"}; return nil })
	mb.failErr = fmt.Errorf("conn lost: %w", ErrAmbiguous)
	err := s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: "new"}; return nil })
	if !errors.Is(err, ErrPersist) {
		t.Fatalf("got %v", err)
	}
	s.View(func(d *Data) {
		if d.Packages["p"].Name != "old" {
			t.Fatal("memory should match the database after the reload")
		}
	})
	if s.Healthy() != nil {
		t.Fatal("reload succeeded, store must be healthy")
	}
}

func TestDegradedAndRecovery(t *testing.T) {
	old := reloadEvery
	reloadEvery = 10 * time.Millisecond
	defer func() { reloadEvery = old }()
	s, mb := openMem(t)
	s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: "kept"}; return nil })
	mb.mu.Lock()
	mb.failErr = fmt.Errorf("conn lost: %w", ErrAmbiguous)
	mb.loadErr = errors.New("db down")
	mb.mu.Unlock()
	s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: "lost"}; return nil })
	if s.Healthy() == nil {
		t.Fatal("expected degraded")
	}
	if err := s.Update(func(d *Data) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	s.View(func(d *Data) { // reads keep working
		if d.Packages["p"].Name != "kept" {
			t.Fatalf("view broken: %+v", d.Packages["p"])
		}
	})
	mb.mu.Lock()
	mb.loadErr = nil
	mb.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for s.Healthy() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Healthy() != nil {
		t.Fatal("did not recover")
	}
	if err := s.Update(func(d *Data) error { d.Packages["q"] = Package{ID: "q"}; return nil }); err != nil {
		t.Fatalf("update after recovery: %v", err)
	}
}

func TestConcurrentUpdatesMem(t *testing.T) {
	s, _ := openMem(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Update(func(d *Data) error {
				id := fmt.Sprint("p", i)
				d.Packages[id] = Package{ID: id}
				return nil
			})
		}()
	}
	wg.Wait()
	s.View(func(d *Data) {
		if len(d.Packages) != 20 {
			t.Fatalf("got %d", len(d.Packages))
		}
	})
}

func benchDiff(b *testing.B, payments int) {
	live, shadow := Data{}, Data{}
	live.init()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < payments; i++ {
		k := fmt.Sprint("K", i)
		live.Payments[k] = Payment{Key: k, AmountKES: i, PaidAt: now, CreatedAt: now}
	}
	for i := 0; i < 300; i++ {
		id := fmt.Sprint("a", i)
		live.Accounts[id] = Account{ID: id, Flat: id, CreatedAt: now}
	}
	shadow = cloneData(&live)
	runtime.GC()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := live.Payments["K1"]
		p.AmountKES++
		live.Payments["K1"] = p
		cs := diffData(&live, &shadow, now)
		if len(cs.Payments.Upsert) != 1 {
			b.Fatal("diff missed the change")
		}
		applyChangeSet(&shadow, &live, cs)
	}
}

// BenchmarkDiff20k is a realistic ledger (about three years of a 600-flat
// estate). BenchmarkDiff200k is the stress size: a map lookup per row is
// memory bound, so it cannot be fast, but it is still well under a second.
func BenchmarkDiff20k(b *testing.B)  { benchDiff(b, 20000) }
func BenchmarkDiff200k(b *testing.B) { benchDiff(b, 200000) }

func TestDiffBudget(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	if raceBuild {
		t.Skip("timing budget is meaningful only without the race detector")
	}
	r20 := testing.Benchmark(BenchmarkDiff20k)
	r200 := testing.Benchmark(BenchmarkDiff200k)
	t.Logf("diff per update: 20k rows %v, 200k rows %v", time.Duration(r20.NsPerOp()), time.Duration(r200.NsPerOp()))
	if per := time.Duration(r20.NsPerOp()); per > 15*time.Millisecond {
		t.Fatalf("diff of 20k rows took %v, budget 15ms", per)
	}
	if per := time.Duration(r200.NsPerOp()); per > 300*time.Millisecond {
		t.Fatalf("diff of 200k rows took %v, sanity limit 300ms", per)
	}
}

func waitHealthy(t *testing.T, s *Store, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for (s.Healthy() == nil) != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if (s.Healthy() == nil) != want {
		t.Fatalf("healthy = %v, want %v", s.Healthy() == nil, want)
	}
}

func TestRepeatedPersistFailuresDegradeThenRecover(t *testing.T) {
	defer SetReloadInterval(10 * time.Millisecond)()
	s, mb := openMem(t)
	set := func(name string) error {
		return s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: name}; return nil })
	}
	if err := set("kept"); err != nil {
		t.Fatal(err)
	}
	mb.mu.Lock()
	mb.failAll = errors.New("write failed")
	mb.mu.Unlock()
	// Below the threshold the store stays healthy, and a success resets the count.
	for i := 0; i < degradeAfter-1; i++ {
		if err := set("lost"); !errors.Is(err, ErrPersist) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if s.Healthy() != nil {
		t.Fatal("degraded too early")
	}
	mb.mu.Lock()
	mb.failAll = nil
	mb.mu.Unlock()
	if err := set("kept2"); err != nil {
		t.Fatal(err)
	}
	mb.mu.Lock()
	mb.failAll = errors.New("write failed")
	mb.loadErr = errors.New("db down")
	mb.mu.Unlock()
	for i := 0; i < degradeAfter-1; i++ {
		set("lost")
	}
	if s.Healthy() != nil {
		t.Fatal("the success should have reset the failure count")
	}
	// The threshold failure degrades the store.
	if err := set("lost"); !errors.Is(err, ErrPersist) {
		t.Fatal(err)
	}
	if s.Healthy() == nil {
		t.Fatal("not degraded after consecutive persist failures")
	}
	// Now writes fail fast without touching the backend, reads keep working.
	before := len(mb.commits)
	start := time.Now()
	if err := set("x"); !errors.Is(err, ErrUnavailable) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("want a fast ErrUnavailable, got %v after %v", err, time.Since(start))
	}
	if len(mb.commits) != before {
		t.Fatal("a refused update reached the backend")
	}
	s.View(func(d *Data) {
		if d.Packages["p"].Name != "kept2" {
			t.Fatalf("view: %+v", d.Packages["p"])
		}
	})
	// The database comes back: the loop restores the store.
	mb.mu.Lock()
	mb.failAll, mb.loadErr = nil, nil
	mb.mu.Unlock()
	waitHealthy(t, s, true)
	if err := set("after"); err != nil {
		t.Fatalf("update after recovery: %v", err)
	}
}

func TestConnectionFailureDegradesAtOnceAndReadsNeverBlock(t *testing.T) {
	defer SetReloadInterval(10 * time.Millisecond)()
	s, mb := openMem(t)
	s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: "kept"}; return nil })
	gate := make(chan struct{})
	mb.mu.Lock()
	mb.failErr = fmt.Errorf("tx: %w: i/o timeout", ErrConnection)
	mb.loadGate = gate // the background reload hangs, as it does against dropped packets
	mb.mu.Unlock()
	err := s.Update(func(d *Data) error { d.Packages["p"] = Package{ID: "p", Name: "lost"}; return nil })
	if !errors.Is(err, ErrPersist) || s.Healthy() == nil {
		t.Fatalf("a connection failure must degrade at once: %v / %v", err, s.Healthy())
	}
	time.Sleep(50 * time.Millisecond) // the retry loop is now stuck inside Load
	done := make(chan string)
	go func() {
		var name string
		s.View(func(d *Data) { name = d.Packages["p"].Name })
		s.Healthy()
		done <- name
	}()
	select {
	case name := <-done:
		if name != "kept" {
			t.Fatalf("view: %q", name)
		}
	case <-time.After(time.Second):
		t.Fatal("View blocked while the reload was waiting on the database")
	}
	if err := s.Update(func(d *Data) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	close(gate)
	waitHealthy(t, s, true)
}
