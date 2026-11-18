// Package metrics collects usage and quality-of-experience data from the
// router (hotspot sessions, interface counters, NetFlow v5, ping) into small
// time-bucketed series, stores them in one file per building and builds the admin report.
package metrics

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"time"

	"ispbill/internal/store"
)

// EAT is East Africa Time; days and months are cut in EAT.
var EAT = time.FixedZone("EAT", 3*3600)

// Tier is a bucket resolution.
type Tier int

const (
	T5 Tier = iota // 5 minutes, kept 48 hours
	TH             // 1 hour, kept 14 days
	TD             // 1 day (EAT), kept 400 days
	numTiers
)

// svcAcctRetention is how long per-flat service days are kept (they are the
// largest part of the file, and only the last two months are ever shown).
const svcAcctRetention = 62 * 24 * time.Hour

// Retention per tier. Pruned on every save so the file stays bounded.
var retention = [numTiers]time.Duration{48 * time.Hour, 14 * 24 * time.Hour, 400 * 24 * time.Hour}

// Seconds is the bucket length of the tier.
func (t Tier) Seconds() int64 { return [numTiers]int64{300, 3600, 86400}[t] }

// BucketStart returns the unix start of the bucket holding t.
func BucketStart(tier Tier, t time.Time) int64 {
	u := t.Unix()
	if tier == TD { // EAT midnight
		return (u+3*3600)/86400*86400 - 3*3600
	}
	s := tier.Seconds()
	return u / s * s
}

// Cell is one bucket of one series. Which fields are used depends on the
// series (per flat, per port, ping, system). Every field is additive except
// the Pk*/RTTMax maxima.
type Cell struct {
	Down   uint64  `json:"d,omitempty"`    // bytes to the client (port: rx)
	Up     uint64  `json:"u,omitempty"`    // bytes from the client (port: tx)
	PkDown uint64  `json:"pd,omitempty"`   // peak bit/s
	PkUp   uint64  `json:"pu,omitempty"`   // peak bit/s
	OnMin  float64 `json:"on,omitempty"`   // minutes online
	CapMin float64 `json:"cap,omitempty"`  // minutes at >= 90% of the package cap
	N      int     `json:"n,omitempty"`    // samples
	RTT    float64 `json:"rtt,omitempty"`  // sum of avg RTT ms
	RTTN   int     `json:"rn,omitempty"`   // samples with a reply
	RTTMax float64 `json:"rmax,omitempty"` // worst avg RTT ms
	Loss   float64 `json:"loss,omitempty"` // sum of loss %
	CPU    float64 `json:"cpu,omitempty"`  // sum of cpu %
	Temp   float64 `json:"t,omitempty"`    // sum of C
	TempN  int     `json:"tn,omitempty"`
}

func (c *Cell) merge(o *Cell) {
	c.Down += o.Down
	c.Up += o.Up
	c.PkDown = maxU(c.PkDown, o.PkDown)
	c.PkUp = maxU(c.PkUp, o.PkUp)
	c.OnMin += o.OnMin
	c.CapMin += o.CapMin
	c.N += o.N
	c.RTT += o.RTT
	c.RTTN += o.RTTN
	c.RTTMax = math.Max(c.RTTMax, o.RTTMax)
	c.Loss += o.Loss
	c.CPU += o.CPU
	c.Temp += o.Temp
	c.TempN += o.TempN
}

func maxU(a, b uint64) uint64 {
	if b > a {
		return b
	}
	return a
}

// Series holds the three resolutions of one measurement.
type Series struct {
	M5 map[int64]*Cell `json:"m5,omitempty"`
	H1 map[int64]*Cell `json:"h1,omitempty"`
	D1 map[int64]*Cell `json:"d1,omitempty"`
}

func (s *Series) tier(t Tier) map[int64]*Cell {
	switch t {
	case T5:
		if s.M5 == nil {
			s.M5 = map[int64]*Cell{}
		}
		return s.M5
	case TH:
		if s.H1 == nil {
			s.H1 = map[int64]*Cell{}
		}
		return s.H1
	}
	if s.D1 == nil {
		s.D1 = map[int64]*Cell{}
	}
	return s.D1
}

// Add applies fn to the cell of time t in every tier (so rolling up is
// implicit: the hourly and daily buckets are fed at the same moment).
func (s *Series) Add(t time.Time, fn func(c *Cell)) {
	for tr := T5; tr < numTiers; tr++ {
		m := s.tier(tr)
		k := BucketStart(tr, t)
		c := m[k]
		if c == nil {
			c = &Cell{}
			m[k] = c
		}
		fn(c)
	}
}

// Point is one bucket in a window.
type Point struct {
	T time.Time
	C Cell
}

// Range returns the buckets of tier with start in [from, to), oldest first.
func (s *Series) Range(tier Tier, from, to time.Time) []Point {
	if s == nil {
		return nil
	}
	var out []Point
	for k, c := range s.tier(tier) {
		if k >= from.Unix() && k < to.Unix() {
			out = append(out, Point{T: time.Unix(k, 0), C: *c})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

// Sum merges every bucket in the window.
func (s *Series) Sum(tier Tier, from, to time.Time) Cell {
	var c Cell
	for _, p := range s.Range(tier, from, to) {
		pc := p.C
		c.merge(&pc)
	}
	return c
}

func (s *Series) prune(now time.Time) bool {
	for tr := T5; tr < numTiers; tr++ {
		cut := now.Add(-retention[tr]).Unix()
		for k := range s.tier(tr) {
			if k < cut {
				delete(s.tier(tr), k)
			}
		}
	}
	return len(s.M5)+len(s.H1)+len(s.D1) == 0
}

// SvcSeries is bytes per service name per bucket.
type SvcSeries struct {
	M5 map[int64]map[string]uint64 `json:"m5,omitempty"`
	H1 map[int64]map[string]uint64 `json:"h1,omitempty"`
	D1 map[int64]map[string]uint64 `json:"d1,omitempty"`
}

func (s *SvcSeries) tier(t Tier) *map[int64]map[string]uint64 {
	switch t {
	case T5:
		return &s.M5
	case TH:
		return &s.H1
	}
	return &s.D1
}

func (s *SvcSeries) add(t time.Time, svc string, n uint64) {
	for tr := T5; tr < numTiers; tr++ {
		m := s.tier(tr)
		if *m == nil {
			*m = map[int64]map[string]uint64{}
		}
		k := BucketStart(tr, t)
		if (*m)[k] == nil {
			(*m)[k] = map[string]uint64{}
		}
		(*m)[k][svc] += n
	}
}

// sum adds every bucket in [from,to) into a service->bytes map.
func (s *SvcSeries) sum(tier Tier, from, to time.Time) map[string]uint64 {
	out := map[string]uint64{}
	for k, row := range *s.tier(tier) {
		if k >= from.Unix() && k < to.Unix() {
			for n, b := range row {
				out[n] += b
			}
		}
	}
	return out
}

func (s *SvcSeries) prune(now time.Time) {
	for tr := T5; tr < numTiers; tr++ {
		cut := now.Add(-retention[tr]).Unix()
		for k := range *s.tier(tr) {
			if k < cut {
				delete(*s.tier(tr), k)
			}
		}
	}
}

// MonthUse is WAN bytes in one calendar month (EAT).
type MonthUse struct {
	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`
}

// NFStats are NetFlow receiver counters (informational).
type NFStats struct {
	Packets  uint64    `json:"packets"`
	Flows    uint64    `json:"flows"`
	Bytes    uint64    `json:"bytes"`
	Attrib   uint64    `json:"attributed_bytes"`
	Bad      uint64    `json:"bad_packets"`
	LastAt   time.Time `json:"last_at"`
	Exporter string    `json:"exporter,omitempty"`
}

// Data is everything persisted in a building's metrics file.
type Data struct {
	Version int                                    `json:"version"`
	Accts   map[string]*Series                     `json:"accts"`    // by account code
	Ports   map[string]*Series                     `json:"ports"`    // by interface name
	Ping    map[string]*Series                     `json:"ping"`     // by target
	Sys     *Series                                `json:"sys"`      // router cpu/temp
	Svc     *SvcSeries                             `json:"svc"`      // global services
	SvcAcct map[string]map[int64]map[string]uint64 `json:"svc_acct"` // code -> EAT day -> service -> bytes
	Months  map[string]*MonthUse                   `json:"months"`
	NF      NFStats                                `json:"netflow"`
	SavedAt time.Time                              `json:"saved_at"`
}

func newData() Data {
	d := Data{Version: 1}
	d.fix()
	return d
}

func (d *Data) fix() {
	if d.Accts == nil {
		d.Accts = map[string]*Series{}
	}
	if d.Ports == nil {
		d.Ports = map[string]*Series{}
	}
	if d.Ping == nil {
		d.Ping = map[string]*Series{}
	}
	if d.Sys == nil {
		d.Sys = &Series{}
	}
	if d.Svc == nil {
		d.Svc = &SvcSeries{}
	}
	if d.SvcAcct == nil {
		d.SvcAcct = map[string]map[int64]map[string]uint64{}
	}
	if d.Months == nil {
		d.Months = map[string]*MonthUse{}
	}
}

func getSeries(m map[string]*Series, k string) *Series {
	s := m[k]
	if s == nil {
		s = &Series{}
		m[k] = s
	}
	return s
}

// Prune drops everything older than the retention of its tier.
func (d *Data) Prune(now time.Time) {
	for _, m := range []map[string]*Series{d.Accts, d.Ports, d.Ping} {
		for k, s := range m {
			if s.prune(now) {
				delete(m, k)
			}
		}
	}
	d.Sys.prune(now)
	d.Svc.prune(now)
	cut := BucketStart(TD, now.Add(-svcAcctRetention))
	for code, days := range d.SvcAcct {
		for k := range days {
			if k < cut {
				delete(days, k)
			}
		}
		if len(days) == 0 {
			delete(d.SvcAcct, code)
		}
	}
	if len(d.Months) > 40 {
		keys := make([]string, 0, len(d.Months))
		for k := range d.Months {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-40] {
			delete(d.Months, k)
		}
	}
}

// MonthKey is "2026-11" in EAT.
func MonthKey(t time.Time) string { return t.In(EAT).Format("2006-01") }

func load(path string) (Data, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newData(), nil
	}
	if err != nil {
		return newData(), err
	}
	d := newData()
	if err := json.Unmarshal(b, &d); err != nil {
		return newData(), err
	}
	d.fix()
	return d, nil
}

func save(path string, d *Data) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return store.WriteFileAtomic(path, b)
}

func marshal(d *Data) ([]byte, error) { return json.Marshal(d) }

func writeBytes(path string, b []byte) error { return store.WriteFileAtomic(path, b) }
