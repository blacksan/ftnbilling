package fleet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ispbill/internal/metrics"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
	"ispbill/internal/wg"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time      { return c.t }
func (c *clock) Add(d time.Duration) { c.t = c.t.Add(d) }

type fakeNet struct {
	mu     sync.Mutex
	hs     map[string]time.Time // siteID -> last handshake
	known  bool
	pingOK bool
}

func (f *fakeNet) Router(string) routeros.Client { return pinger{routeros.NoRouter(), f} }
func (f *fakeNet) PeerStatus(id string) (wg.PeerStatus, bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known {
		return wg.PeerStatus{}, false, false
	}
	h, ok := f.hs[id]
	return wg.PeerStatus{LastHandshake: h}, ok, true
}

type pinger struct {
	routeros.Client
	f *fakeNet
}

func (p pinger) Ping(context.Context) error {
	if p.f.pingOK {
		return nil
	}
	return errors.New("no answer")
}

type rig struct {
	t    *testing.T
	st   *store.Store
	clk  *clock
	net  *fakeNet
	p    *Prober
	up   map[string]bool // TunnelIP:port -> answers
	a, b store.Site
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, clk: &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, metrics.EAT)}, net: &fakeNet{hs: map[string]time.Time{}, known: true}, up: map[string]bool{}}
	r.a = store.Site{ID: "sa", Name: "Alpha", Mode: store.SiteModeWG, TunnelIP: "10.99.0.2", RESTPort: 8081, WGPublicKey: "KA"}
	r.b = store.Site{ID: "sb", Name: "Beta", Mode: store.SiteModeWG, TunnelIP: "10.99.0.3", WGPublicKey: "KB"}
	st.Update(func(d *store.Data) error { d.Sites["sa"], d.Sites["sb"] = r.a, r.b; return nil })
	r.up["10.99.0.2:8081"], r.up["10.99.0.3:8081"] = true, true // Beta has RESTPort 0: default 8081
	r.p = New(Options{Store: st, Net: r.net, Path: filepath.Join(dir, "fleet.json"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: r.clk.Now,
		Dial: func(_ context.Context, addr string) error {
			if r.up[addr] {
				return nil
			}
			return errors.New("connection refused")
		}})
	return r
}

// tick advances 30s and runs one probe round.
func (r *rig) tick() {
	r.clk.Add(30 * time.Second)
	r.p.Tick(context.Background())
}

func TestStateMachineThreeFailsDownTwoOkUp(t *testing.T) {
	r := newRig(t)
	r.tick()
	if st := r.p.State("sa"); st.Status != Unknown {
		t.Fatalf("one ok is not enough: %+v", st)
	}
	r.tick()
	if st := r.p.State("sa"); st.Status != Up {
		t.Fatalf("two oks bring it up: %+v", st)
	}
	r.up["10.99.0.2:8081"] = false
	r.tick()
	r.tick()
	if st := r.p.State("sa"); st.Status != Up || st.Fails != 2 {
		t.Fatalf("two fails keep it up: %+v", st)
	}
	r.tick()
	st := r.p.State("sa")
	if st.Status != Down {
		t.Fatalf("three fails take it down: %+v", st)
	}
	if st := r.p.State("sb"); st.Status != Up {
		t.Fatal("other site unaffected")
	}
	// one ok is not enough to come up; a following fail resets the ok streak
	r.up["10.99.0.2:8081"] = true
	r.tick()
	if r.p.State("sa").Status != Down {
		t.Fatal("one ok must not bring it up")
	}
	r.up["10.99.0.2:8081"] = false
	r.tick()
	r.up["10.99.0.2:8081"] = true
	r.tick()
	if r.p.State("sa").Status != Down {
		t.Fatal("the ok streak was reset by the failure")
	}
	r.tick()
	if r.p.State("sa").Status != Up {
		t.Fatal("two consecutive oks bring it up")
	}
}

func TestOutageRecordedWithStartEndAndCause(t *testing.T) {
	r := newRig(t)
	r.tick()
	r.tick()
	r.net.hs["sa"] = r.clk.Now() // handshake is fresh
	r.p.lastWG = time.Time{}
	r.up["10.99.0.2:8081"] = false
	firstFail := r.clk.Now().Add(30 * time.Second)
	r.tick()
	r.tick()
	r.tick() // down: handshake fresh, so the tunnel is alive
	o, ok := r.p.OpenOutage("sa")
	if !ok || !o.Start.Equal(firstFail) || o.Cause != CauseRESTDown || !o.Open() {
		t.Fatalf("%+v %v (want start %v)", o, ok, firstFail)
	}
	// 4 minutes on with no new handshake: the cause is refined to no-handshake
	for i := 0; i < 9; i++ {
		r.tick()
	}
	if o, _ := r.p.OpenOutage("sa"); o.Cause != CauseNoHandshake {
		t.Fatalf("stale handshake: %+v", o)
	}
	// back
	r.up["10.99.0.2:8081"] = true
	firstOK := r.clk.Now().Add(30 * time.Second)
	r.tick()
	r.tick()
	if _, open := r.p.OpenOutage("sa"); open {
		t.Fatal("outage must be closed")
	}
	all := r.p.Outages("sa", r.clk.Now().Add(-time.Hour))
	if len(all) != 1 || !all[0].End.Equal(firstOK) || all[0].Cause != CauseNoHandshake {
		t.Fatalf("%+v", all)
	}
	if end, ok := r.p.OutageEnd("sa", firstFail); !ok || !end.Equal(firstOK) {
		t.Fatal("OutageEnd")
	}
}

func TestCauseUnknownWithoutDump(t *testing.T) {
	r := newRig(t)
	r.net.known = false
	r.tick()
	r.tick()
	r.up["10.99.0.3:8081"] = false
	for i := 0; i < 3; i++ {
		r.tick()
	}
	if o, ok := r.p.OpenOutage("sb"); !ok || o.Cause != CauseUnknown {
		t.Fatalf("%+v", o)
	}
}

func TestEnvSiteProbedWithPing(t *testing.T) {
	r := newRig(t)
	r.st.Update(func(d *store.Data) error {
		d.Sites["env"] = store.Site{ID: "env", Name: "Main", Mode: store.SiteModeEnv}
		return nil
	})
	r.net.pingOK = true
	r.tick()
	r.tick()
	if r.p.State("env").Status != Up {
		t.Fatal("env site up via Ping")
	}
	r.net.pingOK = false
	for i := 0; i < 3; i++ {
		r.tick()
	}
	if r.p.State("env").Status != Down {
		t.Fatal("env site down when Ping fails")
	}
}

func TestPausedSiteNotProbedAndOutageClosed(t *testing.T) {
	r := newRig(t)
	r.up["10.99.0.2:8081"] = false
	for i := 0; i < 3; i++ {
		r.tick()
	}
	if _, ok := r.p.OpenOutage("sa"); !ok {
		t.Fatal("outage expected")
	}
	r.st.Update(func(d *store.Data) error { s := d.Sites["sa"]; s.Disabled = true; d.Sites["sa"] = s; return nil })
	r.tick()
	if _, ok := r.p.OpenOutage("sa"); ok {
		t.Fatal("a paused building has no open outage")
	}
	if r.p.State("sa").Status != Unknown {
		t.Fatal("state is forgotten while paused")
	}
}

func TestAvailabilityAndSeries(t *testing.T) {
	r := newRig(t)
	// 10 probes ok, 2 failed (a fail also counts as 100% loss)
	for i := 0; i < 10; i++ {
		r.tick()
	}
	r.up["10.99.0.2:8081"] = false
	r.tick()
	r.tick()
	days, overall := r.p.Availability("sa", 3, r.clk.Now())
	if len(days) != 3 || days[0].Pct != -1 || days[1].Pct != -1 {
		t.Fatalf("%+v", days)
	}
	if want := 100 - 100.0*2/12; math.Abs(days[2].Pct-want) > 0.01 || math.Abs(overall-want) > 0.01 || days[2].Samples != 12 {
		t.Fatalf("today %+v overall %v want %v", days[2], overall, want)
	}
	ts, vs := r.p.RTT("sa", metrics.T5, r.clk.Now().Add(-time.Hour), r.clk.Now().Add(time.Minute))
	if len(ts) == 0 || len(ts) != len(vs) || math.IsNaN(vs[0]) {
		t.Fatalf("%v %v", ts, vs)
	}
	if _, _, ok := r.p.Recent("sa", 10*time.Minute, r.clk.Now()); !ok {
		t.Fatal("Recent")
	}
}

func TestSaveLoadRetention(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		r.tick()
	}
	// An old closed outage (>400d), plenty of recent ones, and one open outage.
	now := r.clk.Now()
	r.p.mu.Lock()
	r.p.d.Outages = append(r.p.d.Outages, Outage{SiteID: "sa", Start: now.Add(-500 * 24 * time.Hour), End: now.Add(-499 * 24 * time.Hour)})
	for i := 0; i < outageCap+50; i++ {
		s := now.Add(-time.Duration(i+1) * time.Hour)
		r.p.d.Outages = append(r.p.d.Outages, Outage{SiteID: "sb", Start: s, End: s.Add(time.Minute)})
	}
	r.p.d.Outages = append(r.p.d.Outages, Outage{SiteID: "sa", Start: now.Add(-10 * 24 * time.Hour)})
	r.p.mu.Unlock()
	if err := r.p.Save(); err != nil {
		t.Fatal(err)
	}
	p2 := New(Options{Store: r.st, Net: r.net, Path: r.p.o.Path, Now: r.clk.Now})
	var sa, sb int
	for _, o := range p2.d.Outages {
		switch o.SiteID {
		case "sa":
			sa++
		case "sb":
			sb++
		}
	}
	if sa != 1 || sb != outageCap {
		t.Fatalf("sa=%d (old one must go, open one stays) sb=%d (cap %d)", sa, sb, outageCap)
	}
	if !p2.d.Outages[len(p2.d.Outages)-1].Start.After(now.Add(-2*time.Hour)) || p2.State("sa").Status != r.p.State("sa").Status {
		t.Fatal("state and ordering must survive a reload")
	}
	// Series tiers are pruned: a 5-minute bucket older than 48h goes, the daily one stays.
	r.p.mu.Lock()
	old := now.Add(-100 * time.Hour)
	r.p.d.Series["sa"].Add(old, func(c *metrics.Cell) { c.N++ })
	r.p.mu.Unlock()
	r.p.Save()
	s := r.p.d.Series["sa"]
	if _, has := s.M5[metrics.BucketStart(metrics.T5, old)]; has {
		t.Fatal("5-minute bucket older than 48h must be pruned")
	}
	if _, has := s.D1[metrics.BucketStart(metrics.TD, old)]; !has {
		t.Fatal("daily bucket must stay")
	}
}
