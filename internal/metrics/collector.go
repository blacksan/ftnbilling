package metrics

import (
	"context"
	"hash/fnv"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ispbill/internal/routeros"
)

// AcctInfo is what the collector needs to know about a flat.
type AcctInfo struct {
	Code, Flat, Package, State string
	SpeedMbps                  int
}

// Directory lists the flats (implemented by the billing service).
type Directory interface{ Accounts() []AcctInfo }

// Config holds the collector settings (see internal/config).
type Config struct {
	WAN, OLT               string
	WANCapMbps, OLTCapMbps float64
	FUPTB                  float64
	PingTargets            []string
	Subnet                 netip.Prefix
	FlowEvery, PortEvery   time.Duration
	PingEvery, DNSEvery    time.Duration
	SaveEvery              time.Duration
	// NetflowAddr is informational (shown on the usage page): the receiver
	// lives in internal/sites and feeds HandleNetflow. "off" = no NetFlow.
	NetflowAddr string
	Synthetic   bool // ROUTER_MOCK: generate service data from queue deltas
	// SiteID staggers this collector's loops against the other buildings'.
	SiteID string
	// PortAllow, when non-empty, limits SamplePorts to exactly these
	// interface names (joined buildings). Empty keeps the legacy prefix filter.
	PortAllow []string
}

// CapFraction is "at the cap" for the percent-of-time-at-cap measure.
const CapFraction = 0.9

// LiveRate is the latest rate of one flat.
type LiveRate struct {
	DownBps, UpBps, CapBps float64
	Address                string
	At                     time.Time
}

// LivePort is the latest rate of one interface.
type LivePort struct {
	RxBps, TxBps float64
	Running      bool
	At           time.Time
}

// LivePing is the latest ping result for a target.
type LivePing struct {
	RTTms, LossPct float64
	At             time.Time
}

// Live is the in-memory "right now" view.
type Live struct {
	Flats   map[string]LiveRate
	Ports   map[string]LivePort
	Ping    map[string]LivePing
	Sys     routeros.SysInfo
	SysAt   time.Time
	Err     map[string]string // collector name -> last error text ("" when ok)
	ErrAt   map[string]time.Time
	FlowAt  time.Time
	PortsAt time.Time
}

type queuePrev struct {
	up, down uint64
	at       time.Time
}

type ifPrev struct {
	rx, tx uint64
	at     time.Time
}

type dnsInfo struct {
	names []string
	seen  time.Time
}

// Collector gathers samples and owns the metrics data.
type Collector struct {
	cfg  Config
	rt   routeros.Telemetry
	dir  Directory
	log  *slog.Logger
	path string
	now  func() time.Time

	mu      sync.Mutex
	D       Data
	L       Live
	prevQ   map[string]queuePrev
	prevIf  map[string]ifPrev
	ipCode  map[string]ipEntry
	dns     map[netip.Addr]dnsInfo
	failing map[string]bool
}

type ipEntry struct {
	code string
	seen time.Time
}

// New loads the metrics file at path (a corrupt file is logged and replaced).
// Its directory is created when the first save happens.
func New(path string, cfg Config, rt routeros.Telemetry, d Directory, log *slog.Logger) *Collector {
	c := &Collector{cfg: cfg, rt: rt, dir: d, log: log, path: path, now: time.Now,
		prevQ: map[string]queuePrev{}, prevIf: map[string]ifPrev{}, ipCode: map[string]ipEntry{},
		dns: map[netip.Addr]dnsInfo{}, failing: map[string]bool{}}
	data, err := load(c.path)
	if err != nil {
		log.Warn("metrics file unreadable; starting empty", "path", path, "err", err)
	}
	c.D = data
	c.L = Live{Flats: map[string]LiveRate{}, Ports: map[string]LivePort{}, Ping: map[string]LivePing{},
		Err: map[string]string{}, ErrAt: map[string]time.Time{}}
	return c
}

// SetClock overrides the clock (tests).
func (c *Collector) SetClock(f func() time.Time) { c.now = f }

// Snapshot returns a copy of the live view, taken under the lock, so callers
// (fleet prober, alerts) can read it while the collector keeps sampling.
func (c *Collector) Snapshot() Live {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.L
	out.Flats = make(map[string]LiveRate, len(c.L.Flats))
	for k, v := range c.L.Flats {
		out.Flats[k] = v
	}
	out.Ports = make(map[string]LivePort, len(c.L.Ports))
	for k, v := range c.L.Ports {
		out.Ports[k] = v
	}
	out.Ping = make(map[string]LivePing, len(c.L.Ping))
	for k, v := range c.L.Ping {
		out.Ping[k] = v
	}
	out.Err = make(map[string]string, len(c.L.Err))
	for k, v := range c.L.Err {
		out.Err[k] = v
	}
	out.ErrAt = make(map[string]time.Time, len(c.L.ErrAt))
	for k, v := range c.L.ErrAt {
		out.ErrAt[k] = v
	}
	return out
}

// portWanted decides whether SamplePorts records an interface.
func (c *Collector) portWanted(n string) bool {
	if len(c.cfg.PortAllow) > 0 {
		for _, a := range c.cfg.PortAllow {
			if a == n {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(n, "ether") || strings.HasPrefix(n, "sfp") || strings.HasPrefix(n, "vlan") || strings.HasPrefix(n, "bridge")
}

// jitter is this site's fixed offset into a polling interval: it spreads the
// first tick of every loop so many buildings do not all poll in the same second.
func (c *Collector) jitter(every time.Duration) time.Duration {
	if c.cfg.SiteID == "" || every <= 0 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(c.cfg.SiteID))
	return time.Duration(uint64(h.Sum32()) % uint64(every))
}

// Config returns the settings.
func (c *Collector) Config() Config { return c.cfg }

func (c *Collector) inSubnet(a netip.Addr) bool {
	return c.cfg.Subnet.IsValid() && c.cfg.Subnet.Contains(a)
}

func (c *Collector) codeForIP(ip string, now time.Time) string {
	e, ok := c.ipCode[ip]
	if !ok || now.Sub(e.seen) > 24*time.Hour {
		return ""
	}
	return e.code
}

// hostsFor returns the hostnames known for an address (including CNAME sources).
func (c *Collector) hostsFor(a netip.Addr) []string {
	if i, ok := c.dns[a]; ok {
		return i.names
	}
	return nil
}

func (c *Collector) acctMap() map[string]AcctInfo {
	out := map[string]AcctInfo{}
	if c.dir != nil {
		for _, a := range c.dir.Accounts() {
			out[a.Code] = a
		}
	}
	return out
}

// ---- samplers (all take "now" so tests can drive them) -----------------------

// queueCode returns the account code of a flat's queue "ftn-<code>" (""
// when the queue is not one of ours).
func queueCode(q routeros.SimpleQueue) string {
	if !strings.HasPrefix(q.Name, "ftn-") {
		return ""
	}
	return strings.TrimPrefix(q.Name, "ftn-")
}

// SampleQueues turns the byte counters of the per-flat queues (IPoE) into
// per-flat deltas. A queue seen for the first time is only baselined: its
// counters hold traffic from before this server was watching. A counter that
// goes down (the queue was recreated or counters reset) counts from zero.
func (c *Collector) SampleQueues(now time.Time, queues []routeros.SimpleQueue) {
	accts := c.acctMap()
	c.mu.Lock()
	defer c.mu.Unlock()
	live := map[string]LiveRate{}
	seen := map[string]bool{}
	for _, q := range queues {
		code := queueCode(q)
		info, known := accts[code]
		if !known {
			continue
		}
		seen[q.Name] = true
		addr := strings.TrimSuffix(q.Target, "/32")
		if c.ipCode != nil && addr != "" {
			c.ipCode[addr] = ipEntry{code, now}
		}
		capBps := float64(q.MaxB) // download limit
		if capBps <= 0 {
			capBps = float64(info.SpeedMbps) * 1e6
		}
		// RouterOS lists "upload/download" from the target's side.
		live[code] = LiveRate{UpBps: float64(q.RateA), DownBps: float64(q.RateB), CapBps: capBps, Address: addr, At: now}
		prev, had := c.prevQ[q.Name]
		c.prevQ[q.Name] = queuePrev{up: q.BytesUp, down: q.BytesDown, at: now}
		if !had {
			continue
		}
		dUp, dDown := delta(prev.up, q.BytesUp), delta(prev.down, q.BytesDown)
		dt := now.Sub(prev.at)
		if dt < time.Second {
			dt = time.Second
		}
		down, up := float64(dDown)*8/dt.Seconds(), float64(dUp)*8/dt.Seconds()
		onMin := 0.0
		if dUp+dDown > 0 {
			onMin = dt.Minutes()
		}
		atCap := capBps > 0 && down >= CapFraction*capBps
		pkD, pkU := uint64(down), uint64(up)
		c.getAcct(code).Add(now, func(cell *Cell) {
			cell.Down += dDown
			cell.Up += dUp
			cell.PkDown = maxU(cell.PkDown, pkD)
			cell.PkUp = maxU(cell.PkUp, pkU)
			cell.OnMin += onMin
			if atCap {
				cell.CapMin += onMin
			}
			cell.N++
		})
		if c.cfg.Synthetic {
			c.syntheticServices(now, code, dUp+dDown)
		}
	}
	for k := range c.prevQ {
		if !seen[k] {
			delete(c.prevQ, k)
		}
	}
	c.L.Flats = live
	c.L.FlowAt = now
	for k, e := range c.ipCode {
		if now.Sub(e.seen) > 24*time.Hour {
			delete(c.ipCode, k)
		}
	}
}

func delta(prev, cur uint64) uint64 {
	if cur < prev {
		return cur // counter reset
	}
	return cur - prev
}

func (c *Collector) getAcct(code string) *Series { return getSeries(c.D.Accts, code) }

// syntheticServices splits bytes over a fixed mix (ROUTER_MOCK only).
var syntheticMix = []struct {
	Name string
	W    float64
}{{"YouTube", .28}, {"Netflix", .14}, {"HTTPS (other)", .12}, {"Meta", .09}, {"TikTok", .08}, {"Google", .07},
	{"WhatsApp", .05}, {"CDN (other)", .05}, {"Apple", .04}, {"Microsoft", .03}, {"Zoom", .02}, {"Spotify", .02}, {"Other", .01}}

func (c *Collector) syntheticServices(now time.Time, code string, total uint64) {
	days := c.D.SvcAcct[code]
	if days == nil {
		days = map[int64]map[string]uint64{}
		c.D.SvcAcct[code] = days
	}
	k := BucketStart(TD, now)
	if days[k] == nil {
		days[k] = map[string]uint64{}
	}
	// A little per-flat variation so the top services differ between flats.
	h := 0
	for _, r := range code {
		h = h*31 + int(r)
	}
	for i, m := range syntheticMix {
		w := m.W * (0.6 + 0.8*float64((h>>uint(i))&7)/7)
		b := uint64(float64(total) * w)
		c.D.Svc.add(now, m.Name, b)
		days[k][m.Name] += b
	}
}

// SamplePorts records interface counters (ether*, sfp*, vlan*, bridge*).
func (c *Collector) SamplePorts(now time.Time, ifs []routeros.Iface) {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := map[string]LivePort{}
	for _, f := range ifs {
		n := f.Name
		if !c.portWanted(n) {
			continue
		}
		p, had := c.prevIf[n]
		c.prevIf[n] = ifPrev{rx: f.RxBytes, tx: f.TxBytes, at: now}
		lp := LivePort{Running: f.Running, At: now}
		if had && now.After(p.at) {
			dt := now.Sub(p.at)
			dRx, dTx := delta(p.rx, f.RxBytes), delta(p.tx, f.TxBytes)
			rxBps, txBps := float64(dRx)*8/dt.Seconds(), float64(dTx)*8/dt.Seconds()
			lp.RxBps, lp.TxBps = rxBps, txBps
			c.getPort(n).Add(now, func(cell *Cell) {
				cell.Down += dRx
				cell.Up += dTx
				cell.PkDown = maxU(cell.PkDown, uint64(rxBps))
				cell.PkUp = maxU(cell.PkUp, uint64(txBps))
				cell.N++
			})
			if n == c.cfg.WAN {
				mk := MonthKey(now)
				m := c.D.Months[mk]
				if m == nil {
					m = &MonthUse{}
					c.D.Months[mk] = m
				}
				m.Rx += dRx
				m.Tx += dTx
			}
		}
		live[n] = lp
	}
	c.L.Ports = live
	c.L.PortsAt = now
}

func (c *Collector) getPort(n string) *Series { return getSeries(c.D.Ports, n) }

// SamplePing stores latency and loss for one target.
func (c *Collector) SamplePing(now time.Time, r routeros.PingResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ms := float64(r.AvgRTT) / float64(time.Millisecond)
	got := r.Received > 0
	getSeries(c.D.Ping, r.Target).Add(now, func(cell *Cell) {
		cell.N++
		cell.Loss += r.LossPct
		if got {
			cell.RTT += ms
			cell.RTTN++
			cell.RTTMax = math.Max(cell.RTTMax, ms)
		}
	})
	c.L.Ping[r.Target] = LivePing{RTTms: ms, LossPct: r.LossPct, At: now}
}

// SampleSystem stores router CPU and temperature.
func (c *Collector) SampleSystem(now time.Time, si routeros.SysInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.D.Sys.Add(now, func(cell *Cell) {
		cell.N++
		cell.CPU += float64(si.CPULoad)
		if si.HasTemp {
			cell.Temp += si.TempC
			cell.TempN++
		}
	})
	c.L.Sys, c.L.SysAt = si, now
}

// UpdateDNS rebuilds the IP -> hostnames map from /ip/dns/cache. Entries that
// disappear from the cache are kept for 24 h.
func (c *Collector) UpdateDNS(now time.Time, entries []routeros.DNSEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cname := map[string][]string{} // target -> names pointing at it
	for _, e := range entries {
		if strings.EqualFold(e.Type, "CNAME") {
			t := strings.ToLower(strings.TrimSuffix(e.Data, "."))
			cname[t] = append(cname[t], strings.ToLower(e.Name))
		}
	}
	for _, e := range entries {
		if e.Type != "A" && e.Type != "AAAA" {
			continue
		}
		ip, err := netip.ParseAddr(e.Data)
		if err != nil {
			continue
		}
		name := strings.ToLower(e.Name)
		names := []string{name}
		frontier := []string{name}
		for depth := 0; depth < 3; depth++ {
			var next []string
			for _, n := range frontier {
				next = append(next, cname[n]...)
			}
			names = append(names, next...)
			frontier = next
		}
		cur := c.dns[ip]
		merged := cur.names
		for _, n := range names {
			dup := false
			for _, m := range merged {
				dup = dup || m == n
			}
			if !dup && len(merged) < 8 {
				merged = append(merged, n)
			}
		}
		c.dns[ip] = dnsInfo{names: merged, seen: now}
	}
	{
		for k, v := range c.dns {
			if now.Sub(v.seen) > 24*time.Hour {
				delete(c.dns, k)
			}
		}
	}
}

// ---- persistence ----------------------------------------------------------------

// Save prunes and writes the metrics file atomically.
func (c *Collector) Save() error {
	c.mu.Lock()
	now := c.now()
	c.D.Prune(now)
	c.D.SavedAt = now
	data := c.D
	b, err := marshal(&data)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	return writeBytes(c.path, b)
}

// ---- run loop ---------------------------------------------------------------------

func (c *Collector) note(name string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.L.Err[name] = ""
		if c.failing[name] {
			c.failing[name] = false
			c.log.Info("metrics: router reachable again", "collector", name)
		}
		return
	}
	c.L.Err[name], c.L.ErrAt[name] = err.Error(), c.now()
	if !c.failing[name] {
		c.failing[name] = true
		c.log.Warn("metrics: collection failing (logged once until it recovers)", "collector", name, "err", err)
	}
}

// Run starts the collection loops and blocks until ctx is cancelled, then saves.
func (c *Collector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	// wait sleeps the site's jitter; false means ctx ended first.
	wait := func(d time.Duration) bool {
		if d <= 0 {
			return true
		}
		tm := time.NewTimer(d)
		defer tm.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-tm.C:
			return true
		}
	}
	loop := func(every time.Duration, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !wait(c.jitter(every)) {
				return
			}
			t := time.NewTicker(every)
			defer t.Stop()
			fn(ctx)
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fn(ctx)
				}
			}
		}()
	}
	to := func(f func(context.Context)) func(context.Context) {
		return func(ctx context.Context) {
			cx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			f(cx)
		}
	}
	loop(c.cfg.FlowEvery, to(func(cx context.Context) {
		qs, err := c.rt.SimpleQueues(cx)
		c.note("queues", err)
		if err == nil {
			c.SampleQueues(c.now(), qs)
		}
	}))
	loop(c.cfg.PortEvery, to(func(cx context.Context) {
		ifs, err := c.rt.Interfaces(cx)
		c.note("ports", err)
		if err == nil {
			c.SamplePorts(c.now(), ifs)
		}
	}))
	loop(c.cfg.PingEvery, to(func(cx context.Context) {
		for _, t := range c.cfg.PingTargets {
			r, err := c.rt.PingHost(cx, t, 5)
			c.note("ping", err)
			if err == nil {
				c.SamplePing(c.now(), r)
			}
		}
		si, err := c.rt.System(cx)
		c.note("system", err)
		if err == nil {
			c.SampleSystem(c.now(), si)
		}
	}))
	loop(c.cfg.DNSEvery, to(func(cx context.Context) {
		es, err := c.rt.DNSCache(cx)
		c.note("dns", err)
		if err == nil {
			c.UpdateDNS(c.now(), es)
		}
	}))
	if every := c.cfg.SaveEvery; every > 0 { // staggered: first save one interval + jitter from now
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !wait(c.jitter(every)) {
				return
			}
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := c.Save(); err != nil {
						c.log.Warn("metrics save failed", "err", err)
					}
				}
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	if err := c.Save(); err != nil {
		c.log.Warn("final metrics save failed", "err", err)
	}
}
