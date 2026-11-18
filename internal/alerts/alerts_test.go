package alerts

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/store"
)

type fakeIn struct {
	mu    sync.Mutex
	sites []store.Site
	st    map[string]fleet.SiteState
	live  map[string]metrics.Live
	cfg   metrics.Config
}

func (f *fakeIn) Sites() []store.Site {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Site(nil), f.sites...)
}
func (f *fakeIn) Fleet(id string) fleet.SiteState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.st[id]; ok {
		return s
	}
	return fleet.SiteState{Status: fleet.Up}
}
func (f *fakeIn) Live(id string) (metrics.Live, metrics.Config, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.live[id]
	return l, f.cfg, ok
}

type sink struct {
	mu   sync.Mutex
	msgs []notify.Message
	keys map[string]bool
}

func (s *sink) Enqueue(m notify.Message) (notify.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string]bool{}
	}
	if m.DedupeKey != "" {
		if s.keys[m.DedupeKey] {
			return m, false
		}
		s.keys[m.DedupeKey] = true
	}
	s.msgs = append(s.msgs, m)
	return m, true
}

func (s *sink) texts(ch string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.msgs {
		if m.Channel == ch {
			out = append(out, m.Text)
		}
	}
	return out
}

type rig struct {
	t   *testing.T
	in  *fakeIn
	out *sink
	e   *Engine
	now time.Time
}

func newRig(t *testing.T, n int) *rig {
	t.Helper()
	r := &rig{t: t, in: &fakeIn{st: map[string]fleet.SiteState{}, live: map[string]metrics.Live{}, cfg: metrics.Config{WAN: "ether1", OLT: "sfp1", WANCapMbps: 100}},
		out: &sink{}, now: time.Date(2026, 11, 18, 12, 0, 0, 0, metrics.EAT)}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s%d", i)
		r.in.sites = append(r.in.sites, store.Site{Mode: store.SiteModeEnv, ID: id, Name: fmt.Sprintf("Site %d", i)})
	}
	r.e = r.engine(filepath.Join(t.TempDir(), "alerts.json"))
	return r
}

func (r *rig) engine(path string) *Engine {
	return New(Options{Path: path, In: r.in, Out: r.out, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return r.now },
		Targets: func() Targets {
			return Targets{Email: []string{"a@x.example"}, WhatsApp: []string{"254712345678"}}
		}})
}

func (r *rig) step(d time.Duration) {
	r.now = r.now.Add(d)
	r.e.Evaluate(r.now)
}

// down marks a site down since the current time.
func (r *rig) down(id string) {
	r.in.mu.Lock()
	r.in.st[id] = fleet.SiteState{Status: fleet.Down, Since: r.now, LastErr: "connection refused"}
	r.in.mu.Unlock()
}
func (r *rig) up(id string) {
	r.in.mu.Lock()
	r.in.st[id] = fleet.SiteState{Status: fleet.Up, Since: r.now}
	r.in.mu.Unlock()
}
func (r *rig) ping(id string, loss, rtt float64) {
	r.in.mu.Lock()
	l := r.in.live[id]
	if l.Ping == nil {
		l.Ping = map[string]metrics.LivePing{}
	}
	l.Ping["8.8.8.8"] = metrics.LivePing{LossPct: loss, RTTms: rtt, At: r.now}
	r.in.live[id] = l
	r.in.mu.Unlock()
}
func (r *rig) wan(id string, mbps float64) {
	r.in.mu.Lock()
	l := r.in.live[id]
	if l.Ports == nil {
		l.Ports = map[string]metrics.LivePort{}
	}
	l.Ports["ether1"] = metrics.LivePort{RxBps: mbps * 1e6, Running: true, At: r.now}
	r.in.live[id] = l
	r.in.mu.Unlock()
}
func (r *rig) olt(id string, running bool) {
	r.in.mu.Lock()
	l := r.in.live[id]
	if l.Ports == nil {
		l.Ports = map[string]metrics.LivePort{}
	}
	l.Ports["sfp1"] = metrics.LivePort{Running: running, At: r.now}
	r.in.live[id] = l
	r.in.mu.Unlock()
}

// run advances in 30s steps for d, refreshing readings with fn each step.
func (r *rig) run(d time.Duration, fn func()) {
	for el := time.Duration(0); el < d; el += 30 * time.Second {
		if fn != nil {
			fn()
		}
		r.step(30 * time.Second)
	}
}

func (r *rig) problems(ch string) int {
	n := 0
	for _, t := range r.out.texts(ch) {
		if strings.HasPrefix(t, "PROBLEM") {
			n++
		}
	}
	return n
}
func (r *rig) recoveries(ch string) int {
	n := 0
	for _, t := range r.out.texts(ch) {
		if strings.HasPrefix(t, "RECOVERED") {
			n++
		}
	}
	return n
}

func TestHoldTimes(t *testing.T) {
	r := newRig(t, 1)
	// offline: 5 minutes
	r.down("s0")
	r.run(4*time.Minute+30*time.Second, nil)
	if r.e.OpenCount("") != 0 || r.problems(notify.ChanEmail) != 0 {
		t.Fatal("offline must wait 5 minutes")
	}
	r.run(60*time.Second, nil)
	if r.e.OpenCount("s0") != 1 || r.problems(notify.ChanEmail) != 1 {
		t.Fatalf("offline should fire after 5 minutes: open=%d msgs=%v", r.e.OpenCount(""), r.out.texts(notify.ChanEmail))
	}
	r.up("s0")
	r.step(30 * time.Second)
	// loss: 5% for 10 minutes; 4.9% never fires
	r.run(15*time.Minute, func() { r.ping("s0", 4.9, 20) })
	if r.e.OpenCount("") != 0 {
		t.Fatal("4.9% loss is below the threshold")
	}
	r.run(9*time.Minute, func() { r.ping("s0", 6, 20) })
	if r.e.OpenCount("") != 0 {
		t.Fatal("loss must hold 10 minutes")
	}
	r.run(90*time.Second, func() { r.ping("s0", 6, 20) })
	if a, ok := r.e.Firing(RuleLoss, "s0"); !ok || !strings.Contains(a.Detail, "6.0% packet loss") {
		t.Fatalf("%+v %v", a, ok)
	}
	r.run(30*time.Second, func() { r.ping("s0", 0, 20) })
	// latency: 150 ms for 10 min; a dip below resets the timer
	r.run(8*time.Minute, func() { r.ping("s0", 0, 200) })
	r.run(30*time.Second, func() { r.ping("s0", 0, 100) })
	r.run(8*time.Minute, func() { r.ping("s0", 0, 200) })
	if _, ok := r.e.Firing(RuleLatency, "s0"); ok {
		t.Fatal("the dip must reset the hold timer")
	}
	r.run(3*time.Minute, func() { r.ping("s0", 0, 200) })
	if _, ok := r.e.Firing(RuleLatency, "s0"); !ok {
		t.Fatal("latency should fire")
	}
	r.run(30*time.Second, func() { r.ping("s0", 0, 20) })
	// WAN saturation: 85% of 100 Mbps for 15 minutes
	r.run(14*time.Minute, func() { r.wan("s0", 85) })
	if _, ok := r.e.Firing(RuleWANSat, "s0"); ok {
		t.Fatal("not yet")
	}
	r.run(2*time.Minute, func() { r.wan("s0", 85) })
	if _, ok := r.e.Firing(RuleWANSat, "s0"); !ok {
		t.Fatal("wan_sat should fire after 15 minutes")
	}
	r.run(30*time.Second, func() { r.wan("s0", 10) })
	// OLT down: 2 minutes
	r.run(90*time.Second, func() { r.olt("s0", false) })
	if _, ok := r.e.Firing(RuleOLTDown, "s0"); ok {
		t.Fatal("olt_down must wait 2 minutes")
	}
	r.run(60*time.Second, func() { r.olt("s0", false) })
	if _, ok := r.e.Firing(RuleOLTDown, "s0"); !ok {
		t.Fatal("olt_down should fire")
	}
}

func TestRecoveryOnlyAfterFiringWasSent(t *testing.T) {
	r := newRig(t, 1)
	// pending that clears: nothing at all
	r.run(8*time.Minute, func() { r.ping("s0", 20, 20) })
	r.run(time.Minute, func() { r.ping("s0", 0, 20) })
	if len(r.out.msgs) != 0 {
		t.Fatalf("pending alert that clears sends nothing: %d", len(r.out.msgs))
	}
	// firing while muted: no problem message, so no recovery message either
	r.e.Mute("s0", r.now.Add(2*time.Hour))
	r.run(12*time.Minute, func() { r.ping("s0", 20, 20) })
	if r.e.OpenCount("s0") != 1 || len(r.out.msgs) != 0 {
		t.Fatalf("muted: open=%d msgs=%d", r.e.OpenCount("s0"), len(r.out.msgs))
	}
	r.run(time.Minute, func() { r.ping("s0", 0, 20) })
	if len(r.out.msgs) != 0 {
		t.Fatal("no recovery for a problem that was never announced")
	}
	if h := r.e.History(10); len(h) != 1 || h[0].Notified {
		t.Fatalf("history %+v", h)
	}
	// normal: problem then recovery, on both channels
	r.e.Unmute("s0")
	r.run(12*time.Minute, func() { r.ping("s0", 20, 20) })
	for _, ch := range []string{notify.ChanEmail, notify.ChanWhatsApp} {
		if r.problems(ch) != 1 || r.recoveries(ch) != 0 {
			t.Fatalf("%s: %v", ch, r.out.texts(ch))
		}
	}
	r.run(time.Minute, func() { r.ping("s0", 0, 20) })
	for _, ch := range []string{notify.ChanEmail, notify.ChanWhatsApp} {
		if r.problems(ch) != 1 || r.recoveries(ch) != 1 {
			t.Fatalf("%s: %v", ch, r.out.texts(ch))
		}
	}
	// the WhatsApp message is the ftn_admin_alert template with 5 parameters
	for _, m := range r.out.msgs {
		if m.Channel == notify.ChanWhatsApp {
			if m.Template != notify.TplAdminAlert || len(m.Params) != 5 || m.To != "254712345678" || m.Priority == 0 {
				t.Fatalf("%+v", m)
			}
			if m.Params[1] != "Packet loss" || m.Params[2] != "Site 0" {
				t.Fatalf("%v", m.Params)
			}
		}
	}
}

func TestCooldownAndRepeat(t *testing.T) {
	r := newRig(t, 1)
	bad := func() { r.ping("s0", 20, 20) }
	good := func() { r.ping("s0", 0, 20) }
	r.run(11*time.Minute, bad) // fires at ~10 min: first problem message
	if r.problems(notify.ChanEmail) != 1 {
		t.Fatal("first")
	}
	r.run(time.Minute, good)   // recovers
	r.run(11*time.Minute, bad) // fires again about 23 minutes after the start
	if r.problems(notify.ChanEmail) != 1 {
		t.Fatalf("cooldown of 60 minutes must hold back the second problem message: %v", r.out.texts(notify.ChanEmail))
	}
	if _, ok := r.e.Firing(RuleLoss, "s0"); !ok {
		t.Fatal("it is firing, just not announced")
	}
	r.run(50*time.Minute, bad) // 60 minutes after the first message the cooldown is over
	if r.problems(notify.ChanEmail) != 2 {
		t.Fatalf("after the cooldown it is announced: %v", r.out.texts(notify.ChanEmail))
	}
	// no repeats by default
	r.run(8*time.Hour, bad)
	if r.problems(notify.ChanEmail) != 2 {
		t.Fatalf("repeat is off by default: %d", r.problems(notify.ChanEmail))
	}
	s := r.e.Settings()
	s.RepeatEnabled = true
	if err := r.e.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	r.run(time.Minute, bad) // the last message is over 6 hours old: one repeat at once
	if got := r.problems(notify.ChanEmail); got != 3 {
		t.Fatalf("a repeat: %d", got)
	}
	r.run(5*time.Hour, bad)
	if got := r.problems(notify.ChanEmail); got != 3 {
		t.Fatalf("not again before 6 hours: %d", got)
	}
	r.run(time.Hour+time.Minute, bad)
	if got := r.problems(notify.ChanEmail); got != 4 {
		t.Fatalf("the second repeat after 6 hours: %d", got)
	}
	if !strings.Contains(r.out.texts(notify.ChanEmail)[3], "still firing") {
		t.Fatal(r.out.texts(notify.ChanEmail)[3])
	}
}

func TestOfflineFreezesOtherRules(t *testing.T) {
	r := newRig(t, 1)
	r.run(14*time.Minute, func() { r.wan("s0", 95) }) // wan_sat pending, 1 minute short of firing
	r.down("s0")
	r.run(20*time.Minute, func() { r.wan("s0", 95); r.ping("s0", 100, 0) }) // stale readings from a dead router
	if _, ok := r.e.Firing(RuleWANSat, "s0"); ok {
		t.Fatal("wan_sat must not fire while the building is offline")
	}
	if _, ok := r.e.Firing(RuleLoss, "s0"); ok {
		t.Fatal("loss must not fire while the building is offline")
	}
	act := r.e.Active()
	if len(act) != 1 || act[0].Rule != RuleOffline || act[0].State != Firing {
		t.Fatalf("only the offline alert exists: %+v", act)
	}
	if r.problems(notify.ChanEmail) != 1 {
		t.Fatalf("one message: %v", r.out.texts(notify.ChanEmail))
	}
	// A rule that was already firing stays put (neither resolved nor repeated) while offline.
	r2 := newRig(t, 1)
	r2.run(16*time.Minute, func() { r2.wan("s0", 95) })
	if _, ok := r2.e.Firing(RuleWANSat, "s0"); !ok {
		t.Fatal("setup")
	}
	r2.down("s0")
	r2.run(10*time.Minute, func() { r2.wan("s0", 10) })
	if _, ok := r2.e.Firing(RuleWANSat, "s0"); !ok || r2.recoveries(notify.ChanEmail) != 0 {
		t.Fatal("frozen: a firing alert is not resolved by readings taken while offline")
	}
}

func TestStormGuard(t *testing.T) {
	r := newRig(t, 5)
	// 4 of 5 buildings go down within a minute of each other
	r.down("s0")
	r.step(20 * time.Second)
	r.down("s1")
	r.down("s2")
	r.step(20 * time.Second)
	r.down("s3")
	r.run(6*time.Minute, nil)
	for _, ch := range []string{notify.ChanEmail, notify.ChanWhatsApp} {
		if r.problems(ch) != 1 {
			t.Fatalf("%s: a storm is one message, got %v", ch, r.out.texts(ch))
		}
	}
	if txt := r.out.texts(notify.ChanEmail)[0]; !strings.Contains(txt, "4 of 5 buildings") || !strings.Contains(txt, "Site 0") || !strings.Contains(txt, "Site 3") {
		t.Fatal(txt)
	}
	if r.e.OpenCount("") != 4 {
		t.Fatal("all four alerts exist")
	}
	// they come back one by one: a single recovery once the last is back
	r.up("s0")
	r.up("s1")
	r.up("s2")
	r.step(30 * time.Second)
	if r.recoveries(notify.ChanEmail) != 0 {
		t.Fatal("not yet")
	}
	r.up("s3")
	r.step(30 * time.Second)
	if r.recoveries(notify.ChanEmail) != 1 || r.problems(notify.ChanEmail) != 1 {
		t.Fatalf("%v", r.out.texts(notify.ChanEmail))
	}
	// Two of five is not a storm: individual messages.
	r2 := newRig(t, 5)
	r2.down("s0")
	r2.down("s1")
	r2.run(6*time.Minute, nil)
	if r2.problems(notify.ChanEmail) != 2 {
		t.Fatalf("%v", r2.out.texts(notify.ChanEmail))
	}
	// Three of ten is under half: individual messages.
	r3 := newRig(t, 10)
	r3.down("s0")
	r3.down("s1")
	r3.down("s2")
	r3.run(6*time.Minute, nil)
	if r3.problems(notify.ChanEmail) != 3 {
		t.Fatalf("%v", r3.out.texts(notify.ChanEmail))
	}
	// Buildings going down 10 minutes apart are not a storm.
	r4 := newRig(t, 3)
	r4.down("s0")
	r4.run(10*time.Minute, nil)
	r4.down("s1")
	r4.run(10*time.Minute, nil)
	r4.down("s2")
	r4.run(10*time.Minute, nil)
	if r4.problems(notify.ChanEmail) != 3 {
		t.Fatalf("%v", r4.out.texts(notify.ChanEmail))
	}
}

func TestMuteLifecycle(t *testing.T) {
	r := newRig(t, 2)
	r.e.Mute("s0", r.now.Add(30*time.Minute))
	r.down("s0")
	r.down("s1")
	r.run(10*time.Minute, nil)
	if r.problems(notify.ChanEmail) != 1 || !strings.Contains(r.out.texts(notify.ChanEmail)[0], "Site 1") {
		t.Fatalf("only the unmuted building is announced: %v", r.out.texts(notify.ChanEmail))
	}
	if len(r.e.Mutes()) != 1 {
		t.Fatal("mutes")
	}
	// When the mute expires and the building is still down, it is announced.
	r.run(25*time.Minute, nil)
	if r.problems(notify.ChanEmail) != 2 {
		t.Fatalf("after the mute: %v", r.out.texts(notify.ChanEmail))
	}
	if len(r.e.Mutes()) != 0 {
		t.Fatal("expired mute is gone")
	}
}

func TestPersistenceNoDuplicateAfterRestart(t *testing.T) {
	r := newRig(t, 1)
	r.down("s0")
	r.run(6*time.Minute, nil)
	if r.problems(notify.ChanEmail) != 1 {
		t.Fatal("setup")
	}
	s := r.e.Settings()
	s.Rules[RuleLoss] = Rule{Enabled: false, Threshold: 7, HoldMin: 3}
	if err := r.e.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	if err := r.e.Save(); err != nil {
		t.Fatal(err)
	}
	path := r.e.o.Path
	r.out = &sink{}
	e2 := r.engine(path)
	r.e = e2
	r.run(5*time.Minute, nil)
	if len(r.out.msgs) != 0 {
		t.Fatalf("a restart must not announce the same problem again: %d", len(r.out.msgs))
	}
	if e2.Settings().Rules[RuleLoss].Threshold != 7 || e2.OpenCount("s0") != 1 {
		t.Fatalf("%+v", e2.Settings().Rules[RuleLoss])
	}
	r.up("s0")
	r.step(30 * time.Second)
	if r.recoveries(notify.ChanEmail) != 1 {
		t.Fatal("the recovery still goes out after a restart")
	}
}

func TestSettingsValidationAndTestMessage(t *testing.T) {
	set := DefaultSettings()
	if err := set.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*Settings){
		func(s *Settings) { s.Rules[RuleLoss] = Rule{Enabled: true, Threshold: 0, HoldMin: 5} },
		func(s *Settings) { s.Rules[RuleOffline] = Rule{Enabled: true, HoldMin: 0} },
		func(s *Settings) { s.Rules[RuleWANSat] = Rule{Enabled: true, Threshold: 120, HoldMin: 5} },
		func(s *Settings) { s.CooldownMin = -1 },
		func(s *Settings) { s.OutageNotice = "sometimes" },
	} {
		s := DefaultSettings()
		mut(&s)
		if s.Validate() == nil {
			t.Fatal("must be refused")
		}
	}
	d := DefaultSettings()
	if d.Rules[RuleOffline].HoldMin != 5 || d.Rules[RuleWANSat].Threshold != 85 || d.Rules[RuleWANSat].HoldMin != 15 || d.Rules[RuleLoss].Threshold != 5 ||
		d.Rules[RuleLoss].HoldMin != 10 || d.Rules[RuleLatency].Threshold != 150 || d.Rules[RuleLatency].HoldMin != 10 || d.Rules[RuleOLTDown].HoldMin != 2 ||
		d.CooldownMin != 60 || d.RepeatHours != 6 || d.OutageNotice != "manual" {
		t.Fatalf("defaults: %+v", d)
	}
	r := newRig(t, 1)
	m, ok := r.e.TestMessage(notify.ChanWhatsApp)
	if !ok || m.Template != notify.TplAdminAlert || m.To != "254712345678" || m.DedupeKey != "" || m.Kind != notify.KindTest {
		t.Fatalf("%+v", m)
	}
}
