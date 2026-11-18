package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// QoE thresholds. They are shown on the page next to each flag.
const (
	QoEWANUtilPct   = 85.0 // WAN counts as "busy" in an hour above this utilisation
	QoEWANHours     = 3    // ...and customers may feel slow after this many busy hours in 7 days
	QoECapSharePct  = 20.0 // flats spending more than this share of online time at their cap
	QoEMinOnlineMin = 120  // ...are only judged once they were online this long in the period
	QoELossPct      = 1.0  // average packet loss above this is a problem
	FUPWarnPct      = 80.0 // tile turns warning above this share of the FUP
)

// Period is the selector value.
type Period string

const (
	PeriodToday Period = "today"
	Period7d    Period = "7d"
	Period30d   Period = "30d"
	PeriodMonth Period = "month"
)

// ParsePeriod defaults to today.
func ParsePeriod(s string) Period {
	switch Period(s) {
	case Period7d, Period30d, PeriodMonth:
		return Period(s)
	}
	return PeriodToday
}

// Label is the human name.
func (p Period) Label() string {
	return map[Period]string{PeriodToday: "Today", Period7d: "7 days", Period30d: "30 days", PeriodMonth: "This month"}[p]
}

// Periods is the selector order.
var Periods = []Period{PeriodToday, Period7d, Period30d, PeriodMonth}

// Window gives the time range and the tier that serves it.
func Window(p Period, now time.Time) (from, to time.Time, tier Tier) {
	to = now
	nowE := now.In(EAT)
	switch p {
	case Period7d:
		return time.Unix(BucketStart(TH, now.Add(-7*24*time.Hour)), 0), to, TH
	case Period30d:
		return time.Unix(BucketStart(TD, now.Add(-29*24*time.Hour)), 0), to, TD
	case PeriodMonth:
		return time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, EAT), to, TD
	}
	return time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, EAT), to, T5
}

// SvcRow is one service in a ranking.
type SvcRow struct {
	Name     string
	GB       float64
	SharePct float64
}

// FlatRow is one line of the per-flat table.
type FlatRow struct {
	Code, Flat, Package, State string
	DownGB, UpGB, SharePct     float64
	PeakMbps                   float64
	CapPct                     float64 // % of online time at >=90% of the cap
	OnlineMin                  float64
	CapMbps                    float64
	CurrentMbps                float64 // download now; -1 = not online
	Top                        []SvcRow
}

// Series for a chart: values in Mbps (or ms, %), NaN = no data.
type TS struct {
	T    []time.Time
	Vals map[string][]float64
}

// Flag is one plain-language QoE check.
type Flag struct {
	Level string // ok, warn, nodata
	Title string
	Text  string
	Rule  string // threshold, shown on the page
}

// PortRow is one interface.
type PortRow struct {
	Name         string
	Running      bool
	RxMbps       float64 // now
	TxMbps       float64
	PeakRxMbps   float64
	PeakTxMbps   float64
	UtilPct      float64 // peak vs capacity, -1 = unknown capacity
	CapacityMbps float64
	Role         string
}

// Report is everything the metrics page shows.
type Report struct {
	Period     Period
	From, To   time.Time
	Tier       Tier
	RouterErr  string
	HasTraffic bool

	// Tiles.
	MonthGB, FUPGB            float64
	FUPPct                    float64
	ProjectedGB, ProjectedPct float64
	FUPNote                   string
	FUPWarn                   bool
	WANDownMbps, WANUpMbps    float64
	WANLive                   bool
	WANCapMbps                float64
	PeakUtilPct               float64
	AvgLatencyMs, AvgLossPct  float64
	HasPing                   bool
	CPU                       int
	TempC                     float64
	HasTemp, HasSys           bool
	ActiveFlats, TotalFlats   int

	WAN, OLT TS
	Latency  TS
	Loss     TS
	Busy     [24]float64 // average WAN utilisation % by EAT hour, NaN = no data
	BusyDays int

	Flats    []FlatRow
	Services []SvcRow
	Ports    []PortRow
	Flags    []Flag
	NF       NFStats
	NFOn     bool
	TotalGB  float64
}

func gb(b uint64) float64 { return float64(b) / 1e9 }

func bps2mbps(b float64) float64 { return b / 1e6 }

func cellMbps(bytes uint64, tier Tier) float64 {
	return float64(bytes) * 8 / float64(tier.Seconds()) / 1e6
}

func nan() float64 { return math.NaN() }

// tsFromPorts builds download/upload series (Mbps) for an interface. When the
// interface faces the customers, "download" is its tx.
func (c *Collector) tsFromPort(name string, tier Tier, from, to time.Time, downIsTx bool) TS {
	ts := TS{Vals: map[string][]float64{}}
	s := c.D.Ports[name]
	pts := fillBuckets(s, tier, from, to)
	for _, p := range pts {
		ts.T = append(ts.T, p.T)
		if p.C.N == 0 {
			ts.Vals["down"] = append(ts.Vals["down"], nan())
			ts.Vals["up"] = append(ts.Vals["up"], nan())
			continue
		}
		d, u := p.C.Down, p.C.Up
		if downIsTx {
			d, u = u, d
		}
		ts.Vals["down"] = append(ts.Vals["down"], cellMbps(d, tier))
		ts.Vals["up"] = append(ts.Vals["up"], cellMbps(u, tier))
	}
	return ts
}

// fillBuckets returns one point per bucket in [from,to) so charts show gaps.
func fillBuckets(s *Series, tier Tier, from, to time.Time) []Point {
	have := map[int64]Cell{}
	if s != nil {
		for _, p := range s.Range(tier, from, to) {
			have[p.T.Unix()] = p.C
		}
	}
	var out []Point
	step := tier.Seconds()
	for k := BucketStart(tier, from); k < to.Unix(); k += step {
		if tier == TD {
			// daily buckets are EAT-aligned already; steps of 86400 stay aligned
		}
		out = append(out, Point{T: time.Unix(k, 0), C: have[k]})
	}
	return out
}

func utilPct(mbps, capMbps float64) float64 {
	if capMbps <= 0 {
		return 0
	}
	return 100 * mbps / capMbps
}

// BuildReport assembles the page data for a period.
func (c *Collector) BuildReport(p Period, now time.Time) Report {
	from, to, tier := Window(p, now)
	accts := c.acctMap()
	c.mu.Lock()
	defer c.mu.Unlock()
	r := Report{Period: p, From: from, To: to, Tier: tier, WANCapMbps: c.cfg.WANCapMbps, TotalFlats: len(accts)}
	for _, k := range []string{"sessions", "ports", "ping", "system"} {
		if e := c.L.Err[k]; e != "" {
			r.RouterErr = e
			break
		}
	}
	r.NF, r.NFOn = c.D.NF, c.cfg.NetflowAddr != "" && !strings.EqualFold(c.cfg.NetflowAddr, "off")

	// WAN / OLT.
	r.WAN = c.tsFromPort(c.cfg.WAN, tier, from, to, false)
	r.OLT = c.tsFromPort(c.cfg.OLT, tier, from, to, true)
	if live, ok := c.L.Ports[c.cfg.WAN]; ok && now.Sub(live.At) < 5*time.Minute {
		r.WANLive, r.WANDownMbps, r.WANUpMbps = true, bps2mbps(live.RxBps), bps2mbps(live.TxBps)
	}
	if s := c.D.Ports[c.cfg.WAN]; s != nil {
		sum := s.Sum(tier, from, to)
		r.PeakUtilPct = utilPct(bps2mbps(float64(maxU(sum.PkDown, sum.PkUp))), c.cfg.WANCapMbps)
	}

	// FUP for the calendar month.
	r.FUPGB = c.cfg.FUPTB * 1000
	if m := c.D.Months[MonthKey(now)]; m != nil {
		r.MonthGB = gb(m.Rx + m.Tx)
	}
	r.FUPPct = 100 * r.MonthGB / math.Max(r.FUPGB, 1)
	r.FUPWarn = r.FUPPct > FUPWarnPct
	c.fupProjection(&r, now)

	// Quality.
	c.quality(&r, tier, from, to, now)

	// Router system.
	if c.L.SysAt.After(now.Add(-10 * time.Minute)) {
		r.HasSys, r.CPU = true, c.L.Sys.CPULoad
		r.HasTemp, r.TempC = c.L.Sys.HasTemp, c.L.Sys.TempC
	}
	for _, l := range c.L.Flats {
		if now.Sub(l.At) < 10*time.Minute {
			r.ActiveFlats++
		}
	}

	// Flats and services.
	c.flats(&r, accts, tier, from, to, now)
	r.Services, r.TotalGB = svcRows(c.D.Svc.sum(tier, from, to), 10)
	r.Ports = c.ports(tier, from, to, now)
	c.busyHours(&r, now)
	c.flags(&r, now)
	return r
}

func (c *Collector) fupProjection(r *Report, now time.Time) {
	nowE := now.In(EAT)
	start := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, EAT)
	end := start.AddDate(0, 1, 0)
	elapsed := now.Sub(start).Hours() / 24
	total := end.Sub(start).Hours() / 24
	if r.MonthGB <= 0 || elapsed < 0.25 {
		r.FUPNote = "Not enough data yet this month to project."
		return
	}
	r.ProjectedGB = r.MonthGB / elapsed * total
	r.ProjectedPct = 100 * r.ProjectedGB / math.Max(r.FUPGB, 1)
	if r.ProjectedGB > r.FUPGB {
		daysToFUP := r.FUPGB / (r.MonthGB / elapsed)
		d := start.Add(time.Duration(daysToFUP * 24 * float64(time.Hour)))
		r.FUPNote = "At this pace the FUP is reached on " + d.Format("Mon 2 Jan") + "."
	} else {
		r.FUPNote = fmt.Sprintf("Projected month end: %.1f TB of %.0f TB.", r.ProjectedGB/1000, r.FUPGB/1000)
	}
}

func (c *Collector) quality(r *Report, tier Tier, from, to time.Time, now time.Time) {
	targets := make([]string, 0, len(c.D.Ping))
	for t := range c.D.Ping {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	r.Latency = TS{Vals: map[string][]float64{}}
	r.Loss = TS{Vals: map[string][]float64{"loss": nil}}
	buckets := fillBuckets(nil, tier, from, to)
	for _, b := range buckets {
		r.Latency.T = append(r.Latency.T, b.T)
		r.Loss.T = append(r.Loss.T, b.T)
	}
	var rttSum, lossSum float64
	var rttN, n int
	lossAcc := make([]float64, len(buckets))
	lossCnt := make([]float64, len(buckets))
	for _, t := range targets {
		pts := fillBuckets(c.D.Ping[t], tier, from, to)
		vals := make([]float64, len(pts))
		for i, p := range pts {
			vals[i] = nan()
			if p.C.RTTN > 0 {
				vals[i] = p.C.RTT / float64(p.C.RTTN)
			}
			if p.C.N > 0 {
				lossAcc[i] += p.C.Loss / float64(p.C.N)
				lossCnt[i]++
			}
			rttSum += p.C.RTT
			rttN += p.C.RTTN
			lossSum += p.C.Loss
			n += p.C.N
		}
		r.Latency.Vals[t] = vals
	}
	for i := range buckets {
		v := nan()
		if lossCnt[i] > 0 {
			v = lossAcc[i] / lossCnt[i]
		}
		r.Loss.Vals["loss"] = append(r.Loss.Vals["loss"], v)
	}
	if n > 0 {
		r.HasPing = true
		r.AvgLossPct = lossSum / float64(n)
		if rttN > 0 {
			r.AvgLatencyMs = rttSum / float64(rttN)
		}
	}
}

func (c *Collector) flats(r *Report, accts map[string]AcctInfo, tier Tier, from, to, now time.Time) {
	var totalDown uint64
	rows := map[string]*FlatRow{}
	sums := map[string]Cell{}
	codes := map[string]bool{}
	for code := range accts {
		codes[code] = true
	}
	for code := range c.D.Accts {
		codes[code] = true
	}
	for code := range codes {
		sum := c.D.Accts[code].Sum(tier, from, to)
		sums[code] = sum
		totalDown += sum.Down
	}
	for code := range codes {
		info, known := accts[code]
		sum := sums[code]
		row := &FlatRow{Code: code, Flat: info.Flat, Package: info.Package, State: info.State,
			DownGB: gb(sum.Down), UpGB: gb(sum.Up), PeakMbps: bps2mbps(float64(sum.PkDown)), OnlineMin: sum.OnMin,
			CapMbps: float64(info.SpeedMbps), CurrentMbps: -1}
		if !known {
			row.Flat = "(removed) " + code
		}
		if totalDown > 0 {
			row.SharePct = 100 * float64(sum.Down) / float64(totalDown)
		}
		if sum.OnMin > 0 {
			row.CapPct = 100 * sum.CapMin / sum.OnMin
		}
		if l, ok := c.L.Flats[code]; ok && now.Sub(l.At) < 10*time.Minute {
			row.CurrentMbps = bps2mbps(l.DownBps)
			if l.CapBps > 0 {
				row.CapMbps = bps2mbps(l.CapBps)
			}
		}
		row.Top = c.topServices(code, from, to, 3)
		if sum.Down+sum.Up > 0 {
			r.HasTraffic = true
		}
		rows[code] = row
	}
	for _, row := range rows {
		r.Flats = append(r.Flats, *row)
	}
	SortFlats(r.Flats, "down")
}

func (c *Collector) topServices(code string, from, to time.Time, n int) []SvcRow {
	days := c.D.SvcAcct[code]
	sum := map[string]uint64{}
	f, t := BucketStart(TD, from), to.Unix()
	for k, row := range days {
		if k >= f && k < t {
			for s, b := range row {
				sum[s] += b
			}
		}
	}
	rows, _ := svcRows(sum, n)
	return rows
}

func svcRows(sum map[string]uint64, top int) ([]SvcRow, float64) {
	var total uint64
	for _, b := range sum {
		total += b
	}
	rows := make([]SvcRow, 0, len(sum))
	for n, b := range sum {
		if b == 0 {
			continue
		}
		rows = append(rows, SvcRow{Name: n, GB: gb(b), SharePct: 100 * float64(b) / math.Max(float64(total), 1)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].GB != rows[j].GB {
			return rows[i].GB > rows[j].GB
		}
		return rows[i].Name < rows[j].Name
	})
	if top > 0 && len(rows) > top {
		rows = rows[:top]
	}
	return rows, gb(total)
}

// SortFlats sorts in place. Keys: down, up, share, peak, cap, current, flat, code.
func SortFlats(rows []FlatRow, key string) {
	less := map[string]func(a, b FlatRow) bool{
		"down":    func(a, b FlatRow) bool { return a.DownGB > b.DownGB },
		"up":      func(a, b FlatRow) bool { return a.UpGB > b.UpGB },
		"share":   func(a, b FlatRow) bool { return a.SharePct > b.SharePct },
		"peak":    func(a, b FlatRow) bool { return a.PeakMbps > b.PeakMbps },
		"cap":     func(a, b FlatRow) bool { return a.CapPct > b.CapPct },
		"current": func(a, b FlatRow) bool { return a.CurrentMbps > b.CurrentMbps },
		"flat":    func(a, b FlatRow) bool { return naturalLess(a.Flat, b.Flat) },
		"code":    func(a, b FlatRow) bool { return a.Code < b.Code },
	}
	f, ok := less[key]
	if !ok {
		f = less["down"]
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if f(rows[i], rows[j]) {
			return true
		}
		if f(rows[j], rows[i]) {
			return false
		}
		return naturalLess(rows[i].Flat, rows[j].Flat)
	})
}

func naturalLess(a, b string) bool {
	// small local copy of store.NaturalLess semantics for sorting labels
	a, b = strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] >= '0' && a[i] <= '9' && b[j] >= '0' && b[j] <= '9' {
			si, sj := i, j
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			na, nb := strings.TrimLeft(a[si:i], "0"), strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

func (c *Collector) ports(tier Tier, from, to, now time.Time) []PortRow {
	names := make([]string, 0, len(c.D.Ports))
	for n := range c.D.Ports {
		names = append(names, n)
	}
	for n := range c.L.Ports {
		if _, ok := c.D.Ports[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []PortRow
	for _, n := range names {
		sum := c.D.Ports[n].Sum(tier, from, to)
		row := PortRow{Name: n, PeakRxMbps: bps2mbps(float64(sum.PkDown)), PeakTxMbps: bps2mbps(float64(sum.PkUp)), UtilPct: -1}
		if l, ok := c.L.Ports[n]; ok {
			row.Running, row.RxMbps, row.TxMbps = l.Running, bps2mbps(l.RxBps), bps2mbps(l.TxBps)
		}
		switch n {
		case c.cfg.WAN:
			row.Role, row.CapacityMbps = "WAN (Safaricom)", c.cfg.WANCapMbps
		case c.cfg.OLT:
			row.Role, row.CapacityMbps = "OLT uplink", c.cfg.OLTCapMbps
		}
		if row.CapacityMbps > 0 {
			row.UtilPct = utilPct(math.Max(row.PeakRxMbps, row.PeakTxMbps), row.CapacityMbps)
		}
		out = append(out, row)
	}
	// Roles first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Role != "" && out[j].Role == "" })
	return out
}

func (c *Collector) busyHours(r *Report, now time.Time) {
	for i := range r.Busy {
		r.Busy[i] = nan()
	}
	from := now.Add(-14 * 24 * time.Hour)
	s := c.D.Ports[c.cfg.WAN]
	if s == nil || c.cfg.WANCapMbps <= 0 {
		return
	}
	var sum, cnt [24]float64
	days := map[string]bool{}
	for _, p := range s.Range(TH, from, now) {
		if p.C.N == 0 {
			continue
		}
		h := p.T.In(EAT).Hour()
		u := utilPct(math.Max(cellMbps(p.C.Down, TH), cellMbps(p.C.Up, TH)), c.cfg.WANCapMbps)
		sum[h] += u
		cnt[h]++
		days[p.T.In(EAT).Format("2006-01-02")] = true
	}
	for h := range sum {
		if cnt[h] > 0 {
			r.Busy[h] = sum[h] / cnt[h]
		}
	}
	r.BusyDays = len(days)
}

func (c *Collector) flags(r *Report, now time.Time) {
	// 1. WAN busy hours this week.
	rule := fmt.Sprintf("WAN at or above %.0f%% of %.0f Mbps for %d or more hours in the last 7 days", QoEWANUtilPct, c.cfg.WANCapMbps, QoEWANHours)
	f := Flag{Title: "WAN busy hours", Rule: rule}
	if s := c.D.Ports[c.cfg.WAN]; s == nil {
		f.Level, f.Text = "nodata", "No WAN data yet."
	} else {
		busy := 0
		for _, p := range s.Range(TH, now.Add(-7*24*time.Hour), now) {
			if p.C.N > 0 && utilPct(math.Max(cellMbps(p.C.Down, TH), cellMbps(p.C.Up, TH)), c.cfg.WANCapMbps) >= QoEWANUtilPct {
				busy++
			}
		}
		if busy >= QoEWANHours {
			f.Level, f.Text = "warn", fmt.Sprintf("The WAN was above %.0f%% for %d hours this week. Customers may feel slow at those times.", QoEWANUtilPct, busy)
		} else {
			f.Level, f.Text = "ok", fmt.Sprintf("The WAN was above %.0f%% for %d hour(s) this week.", QoEWANUtilPct, busy)
		}
	}
	r.Flags = append(r.Flags, f)

	// 2. Flats at their cap.
	f = Flag{Title: "Flats at their speed cap", Rule: fmt.Sprintf("more than %.0f%% of online time at 90%% or more of the package speed, with at least %d min online", QoECapSharePct, QoEMinOnlineMin)}
	var names []string
	for _, row := range r.Flats {
		if row.OnlineMin >= QoEMinOnlineMin && row.CapPct > QoECapSharePct {
			names = append(names, fmt.Sprintf("%s (%.0f%%)", row.Flat, row.CapPct))
		}
	}
	switch {
	case !r.HasTraffic:
		f.Level, f.Text = "nodata", "No flat traffic recorded for this period yet."
	case len(names) > 0:
		sort.Strings(names)
		f.Level, f.Text = "warn", "Upgrade candidates, they keep hitting their cap: "+strings.Join(names, ", ")+"."
	default:
		f.Level, f.Text = "ok", "No flat is spending much time at its cap."
	}
	r.Flags = append(r.Flags, f)

	// 3. Loss.
	f = Flag{Title: "Packet loss", Rule: fmt.Sprintf("average loss above %.0f%% to the ping targets", QoELossPct)}
	switch {
	case !r.HasPing:
		f.Level, f.Text = "nodata", "No ping data yet."
	case r.AvgLossPct > QoELossPct:
		f.Level, f.Text = "warn", fmt.Sprintf("Average loss is %.1f%%. Web pages and calls may stall.", r.AvgLossPct)
	default:
		f.Level, f.Text = "ok", fmt.Sprintf("Average loss is %.1f%%.", r.AvgLossPct)
	}
	r.Flags = append(r.Flags, f)

	// 4. FUP.
	f = Flag{Title: "Fair-use limit", Rule: fmt.Sprintf("projected month total above %.0f TB (FUP_TB)", c.cfg.FUPTB)}
	switch {
	case r.MonthGB <= 0 || r.ProjectedGB <= 0:
		f.Level, f.Text = "nodata", "Not enough data this month to project."
	case r.ProjectedGB > r.FUPGB:
		f.Level, f.Text = "warn", "FUP projected to be exceeded. "+r.FUPNote
	default:
		f.Level, f.Text = "ok", r.FUPNote
	}
	r.Flags = append(r.Flags, f)
}

// FlatReport is the drill-down for one flat.
type FlatReport struct {
	Row      FlatRow
	Found    bool
	From, To time.Time
	Tier     Tier
	Series   TS // down, up in Mbps
	Services []SvcRow
}

// BuildFlat gives the usage over time and services of one account code.
func (c *Collector) BuildFlat(code string, p Period, now time.Time) FlatReport {
	rep := c.BuildReport(p, now)
	fr := FlatReport{From: rep.From, To: rep.To, Tier: rep.Tier}
	for _, row := range rep.Flats {
		if row.Code == code {
			fr.Row, fr.Found = row, true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fr.Series = TS{Vals: map[string][]float64{}}
	for _, pt := range fillBuckets(c.D.Accts[code], rep.Tier, rep.From, rep.To) {
		fr.Series.T = append(fr.Series.T, pt.T)
		if pt.C.N == 0 {
			fr.Series.Vals["down"] = append(fr.Series.Vals["down"], nan())
			fr.Series.Vals["up"] = append(fr.Series.Vals["up"], nan())
			continue
		}
		fr.Series.Vals["down"] = append(fr.Series.Vals["down"], cellMbps(pt.C.Down, rep.Tier))
		fr.Series.Vals["up"] = append(fr.Series.Vals["up"], cellMbps(pt.C.Up, rep.Tier))
	}
	fr.Services = c.topServices(code, rep.From, rep.To, 10)
	return fr
}
