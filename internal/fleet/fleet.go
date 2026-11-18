// Package fleet probes every building's router, keeps its up/down history and
// outage log, and summarises it for the fleet and uptime pages.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"ispbill/internal/metrics"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
	"ispbill/internal/wg"
)

// Status of a building.
const (
	Unknown = "unknown"
	Up      = "up"
	Down    = "down"
)

// Outage causes.
const (
	CauseNoHandshake = "no-handshake"        // the WireGuard handshake is older than 3 minutes
	CauseRESTDown    = "tunnel-up-rest-down" // the tunnel is alive but the router API does not answer
	CauseUnknown     = "unknown"
)

const (
	failsToDown   = 3
	oksToUp       = 2
	handshakeLate = 3 * time.Minute
	outageKeep    = 400 * 24 * time.Hour
	outageCap     = 2000 // per building
	defaultPort   = 8081
)

// Net is what the prober needs from the building manager.
type Net interface {
	Router(siteID string) routeros.Client
	PeerStatus(siteID string) (ps wg.PeerStatus, found, known bool)
}

// SiteState is the probe state machine of one building.
type SiteState struct {
	Status    string    `json:"status"`
	Since     time.Time `json:"since"`
	Fails     int       `json:"fails"`
	Oks       int       `json:"oks"`
	FirstFail time.Time `json:"first_fail,omitempty"`
	FirstOK   time.Time `json:"first_ok,omitempty"`
	LastProbe time.Time `json:"last_probe,omitempty"`
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastErr   string    `json:"last_err,omitempty"`
	RTTms     float64   `json:"rtt_ms,omitempty"` // last successful probe
	// HandshakeAge is the WireGuard handshake age in seconds; -1 = never or unknown.
	HandshakeAge int64 `json:"handshake_age"`
	WGKnown      bool  `json:"wg_known"`
}

// Outage is one period a building was down. End is zero while it lasts.
type Outage struct {
	SiteID string    `json:"site_id"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end,omitempty"`
	Cause  string    `json:"cause"`
}

// Open reports whether the outage is still going.
func (o Outage) Open() bool { return o.End.IsZero() }

type fileData struct {
	Series  map[string]*metrics.Series `json:"series"`
	Outages []Outage                   `json:"outages"`
	State   map[string]SiteState       `json:"state"`
	SavedAt time.Time                  `json:"saved_at"`
}

// Options configures a Prober.
type Options struct {
	Store *store.Store
	Net   Net
	Path  string // DATA/fleet.json
	Log   *slog.Logger
	Now   func() time.Time
	// Dial checks that a TCP port answers (tests replace it).
	Dial func(ctx context.Context, addr string) error
	// Every is the probe interval (default 30s); WGEvery how often the
	// handshake is read (default 60s); SaveEvery the file flush (default 5m).
	Every, WGEvery, SaveEvery time.Duration
}

// Prober is the fleet monitor.
type Prober struct {
	o   Options
	now func() time.Time

	mu     sync.Mutex
	d      fileData
	lastWG time.Time
	dirty  bool
}

// New loads fleet.json (a corrupt file is logged and replaced).
func New(o Options) *Prober {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Every <= 0 {
		o.Every = 30 * time.Second
	}
	if o.WGEvery <= 0 {
		o.WGEvery = 60 * time.Second
	}
	if o.SaveEvery <= 0 {
		o.SaveEvery = 5 * time.Minute
	}
	if o.Dial == nil {
		o.Dial = func(ctx context.Context, addr string) error {
			d := net.Dialer{Timeout: 3 * time.Second}
			c, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return err
			}
			return c.Close()
		}
	}
	p := &Prober{o: o, now: o.Now}
	p.d = fileData{Series: map[string]*metrics.Series{}, State: map[string]SiteState{}}
	if o.Path != "" {
		if raw, err := os.ReadFile(o.Path); err == nil {
			var fd fileData
			if err := json.Unmarshal(raw, &fd); err != nil {
				o.Log.Warn("fleet file unreadable; starting empty", "path", o.Path, "err", err)
			} else {
				p.d = fd
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			o.Log.Warn("fleet file unreadable; starting empty", "path", o.Path, "err", err)
		}
	}
	if p.d.Series == nil {
		p.d.Series = map[string]*metrics.Series{}
	}
	if p.d.State == nil {
		p.d.State = map[string]SiteState{}
	}
	return p
}

type target struct {
	site store.Site
	addr string // "" = env site, probed with Client.Ping
}

// targets lists the buildings that can be probed right now.
func (p *Prober) targets() []target {
	var out []target
	p.o.Store.View(func(d *store.Data) {
		for _, s := range d.SiteList() {
			if !s.Runnable() {
				continue
			}
			if s.Mode == store.SiteModeEnv {
				out = append(out, target{site: s})
				continue
			}
			if s.TunnelIP == "" {
				continue
			}
			port := s.RESTPort
			if port == 0 {
				port = defaultPort
			}
			out = append(out, target{site: s, addr: net.JoinHostPort(s.TunnelIP, strconv.Itoa(port))})
		}
	})
	return out
}

type result struct {
	id  string
	ok  bool
	rtt time.Duration
	err string
}

// Tick runs one probe round: it probes every building in parallel and then
// applies the results to the state machines.
func (p *Prober) Tick(ctx context.Context) {
	now := p.now()
	ts := p.targets()
	res := make([]result, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pc, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			start := time.Now()
			var err error
			if t.addr == "" {
				err = p.o.Net.Router(t.site.ID).Ping(pc)
			} else {
				err = p.o.Dial(pc, t.addr)
			}
			r := result{id: t.site.ID, ok: err == nil, rtt: time.Since(start)}
			if err != nil {
				r.err = err.Error()
			}
			res[i] = r
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	live := map[string]bool{}
	for _, t := range ts {
		live[t.site.ID] = true
	}
	p.reconcileLocked(live, now)
	if p.lastWG.IsZero() || now.Sub(p.lastWG) >= p.o.WGEvery {
		p.lastWG = now
		for _, t := range ts {
			p.refreshWGLocked(t, now)
		}
	}
	for _, r := range res {
		p.applyLocked(r, now)
	}
	p.dirty = true
}

// reconcileLocked closes the outage of, and forgets, a building that is no
// longer probed (paused, removed, not joined).
func (p *Prober) reconcileLocked(live map[string]bool, now time.Time) {
	exists := map[string]bool{}
	p.o.Store.View(func(d *store.Data) {
		for id, s := range d.Sites {
			exists[id] = !s.Removed
		}
	})
	for id := range p.d.State {
		if live[id] {
			continue
		}
		for i := range p.d.Outages {
			if o := &p.d.Outages[i]; o.SiteID == id && o.Open() {
				o.End = now
			}
		}
		delete(p.d.State, id)
	}
	for id := range p.d.Series { // history stays while a building is only paused
		if !exists[id] {
			delete(p.d.Series, id)
		}
	}
}

func (p *Prober) refreshWGLocked(t target, now time.Time) {
	if t.addr == "" {
		return
	}
	st, had := p.d.State[t.site.ID]
	if !had {
		st.Status = Unknown
	}
	ps, found, known := p.o.Net.PeerStatus(t.site.ID)
	st.WGKnown = known
	st.HandshakeAge = -1
	if known && found {
		if age, ok := ps.HandshakeAge(now); ok {
			st.HandshakeAge = int64(age.Seconds())
		}
	}
	p.d.State[t.site.ID] = st
	if st.Status == Down { // the cause may have become clearer
		for i := range p.d.Outages {
			if o := &p.d.Outages[i]; o.SiteID == t.site.ID && o.Open() {
				o.Cause = causeOf(st)
			}
		}
	}
}

func causeOf(st SiteState) string {
	if !st.WGKnown {
		return CauseUnknown
	}
	if st.HandshakeAge < 0 || time.Duration(st.HandshakeAge)*time.Second > handshakeLate {
		return CauseNoHandshake
	}
	return CauseRESTDown
}

func (p *Prober) seriesLocked(id string) *metrics.Series {
	s := p.d.Series[id]
	if s == nil {
		s = &metrics.Series{}
		p.d.Series[id] = s
	}
	return s
}

func (p *Prober) applyLocked(r result, now time.Time) {
	st, had := p.d.State[r.id]
	if !had {
		st = SiteState{Status: Unknown, HandshakeAge: -1}
	}
	st.LastProbe = now
	p.seriesLocked(r.id).Add(now, func(c *metrics.Cell) {
		c.N++
		if !r.ok {
			c.Loss += 100
			return
		}
		ms := float64(r.rtt) / float64(time.Millisecond)
		c.RTT += ms
		c.RTTN++
		c.RTTMax = math.Max(c.RTTMax, ms)
	})
	if r.ok {
		st.Oks++
		if st.Oks == 1 {
			st.FirstOK = now
		}
		st.Fails, st.LastOK, st.LastErr = 0, now, ""
		st.RTTms = float64(r.rtt) / float64(time.Millisecond)
	} else {
		st.Fails++
		if st.Fails == 1 {
			st.FirstFail = now
		}
		st.Oks, st.LastErr = 0, r.err
	}
	switch {
	case st.Status != Down && st.Fails >= failsToDown:
		st.Status, st.Since = Down, st.FirstFail
		p.d.Outages = append(p.d.Outages, Outage{SiteID: r.id, Start: st.FirstFail, Cause: causeOf(st)})
		p.o.Log.Warn("building down", "site", r.id, "cause", causeOf(st))
	case st.Status == Down && st.Oks >= oksToUp:
		st.Status, st.Since = Up, st.FirstOK
		for i := range p.d.Outages {
			if o := &p.d.Outages[i]; o.SiteID == r.id && o.Open() {
				o.End = st.FirstOK
			}
		}
		p.o.Log.Info("building back up", "site", r.id)
	case st.Status == Unknown && st.Oks >= oksToUp:
		st.Status, st.Since = Up, st.FirstOK
	}
	p.d.State[r.id] = st
}

// State returns one building's probe state (Status unknown if never probed).
func (p *Prober) State(id string) SiteState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.d.State[id]; ok {
		return st
	}
	return SiteState{Status: Unknown, HandshakeAge: -1}
}

// States returns a copy of every state.
func (p *Prober) States() map[string]SiteState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]SiteState, len(p.d.State))
	for k, v := range p.d.State {
		out[k] = v
	}
	return out
}

// Outages lists a building's outages that ended after from (or are open),
// newest first. An empty id means every building.
func (p *Prober) Outages(id string, from time.Time) []Outage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Outage
	for _, o := range p.d.Outages {
		if (id == "" || o.SiteID == id) && (o.Open() || o.End.After(from)) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	return out
}

// OpenOutage returns the outage in progress, if any.
func (p *Prober) OpenOutage(id string) (Outage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.d.Outages) - 1; i >= 0; i-- {
		if o := p.d.Outages[i]; o.SiteID == id && o.Open() {
			return o, true
		}
	}
	return Outage{}, false
}

// OutageEnd tells whether the outage that started at start has ended.
func (p *Prober) OutageEnd(id string, start time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range p.d.Outages {
		if o.SiteID == id && o.Start.Equal(start) && !o.Open() {
			return o.End, true
		}
	}
	return time.Time{}, false
}

// DayAvail is one EAT day of availability.
type DayAvail struct {
	Day     time.Time
	Pct     float64 // -1 = no samples
	Samples int
}

// Availability returns the daily availability of the last days days (oldest
// first, today last) and the overall figure (-1 = no samples). A probe that
// failed counts as 100% loss, so a day is 100 - the average loss.
func (p *Prober) Availability(id string, days int, now time.Time) ([]DayAvail, float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.d.Series[id]
	out := make([]DayAvail, 0, days)
	var n int
	var loss float64
	today := metrics.BucketStart(metrics.TD, now)
	for i := days - 1; i >= 0; i-- {
		k := today - int64(i)*86400
		da := DayAvail{Day: time.Unix(k, 0), Pct: -1}
		if s != nil {
			if c := s.D1[k]; c != nil && c.N > 0 {
				da.Samples = c.N
				da.Pct = 100 - c.Loss/float64(c.N)
				n += c.N
				loss += c.Loss
			}
		}
		out = append(out, da)
	}
	if n == 0 {
		return out, -1
	}
	return out, 100 - loss/float64(n)
}

// RTT returns the tunnel round-trip (TCP connect) series for a chart, NaN
// where nothing answered.
func (p *Prober) RTT(id string, tier metrics.Tier, from, to time.Time) ([]time.Time, []float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.d.Series[id]
	if s == nil {
		return nil, nil
	}
	var ts []time.Time
	var vs []float64
	for _, pt := range s.Range(tier, from, to) {
		ts = append(ts, pt.T)
		if pt.C.RTTN > 0 {
			vs = append(vs, pt.C.RTT/float64(pt.C.RTTN))
		} else {
			vs = append(vs, math.NaN())
		}
	}
	return ts, vs
}

// Recent returns the average connect time and loss over the last window.
func (p *Prober) Recent(id string, window time.Duration, now time.Time) (rttMs, lossPct float64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.d.Series[id]
	if s == nil {
		return 0, 0, false
	}
	c := s.Sum(metrics.T5, now.Add(-window), now.Add(time.Second))
	if c.N == 0 {
		return 0, 0, false
	}
	if c.RTTN > 0 {
		rttMs = c.RTT / float64(c.RTTN)
	}
	return rttMs, c.Loss / float64(c.N), true
}

func (p *Prober) pruneLocked(now time.Time) {
	for id, s := range p.d.Series {
		if s.Prune(now) {
			delete(p.d.Series, id)
		}
	}
	per := map[string]int{}
	kept := make([]Outage, 0, len(p.d.Outages))
	// newest first so the cap keeps the most recent
	sort.SliceStable(p.d.Outages, func(i, j int) bool { return p.d.Outages[i].Start.After(p.d.Outages[j].Start) })
	for _, o := range p.d.Outages {
		if !o.Open() && now.Sub(o.End) > outageKeep {
			continue
		}
		if per[o.SiteID] >= outageCap {
			continue
		}
		per[o.SiteID]++
		kept = append(kept, o)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Start.Before(kept[j].Start) })
	p.d.Outages = kept
}

// Save writes fleet.json.
func (p *Prober) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.o.Path == "" {
		return nil
	}
	now := p.now()
	p.pruneLocked(now)
	p.d.SavedAt = now
	raw, err := json.Marshal(&p.d)
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(p.o.Path, raw); err != nil {
		return err
	}
	p.dirty = false
	return nil
}

// Run probes every Every until ctx ends, saving every SaveEvery and once more
// on the way out.
func (p *Prober) Run(ctx context.Context) {
	p.Tick(ctx)
	probe := time.NewTicker(p.o.Every)
	save := time.NewTicker(p.o.SaveEvery)
	defer probe.Stop()
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := p.Save(); err != nil {
				p.o.Log.Warn("fleet final save failed", "err", err)
			}
			return
		case <-probe.C:
			p.Tick(ctx)
		case <-save.C:
			if err := p.Save(); err != nil {
				p.o.Log.Warn("fleet save failed", "err", err)
			}
		}
	}
}
