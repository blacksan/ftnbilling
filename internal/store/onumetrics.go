package store

import (
	"sort"
	"time"
)

// ONU metrics: what the ACS reads from an ONU over TR-069 on its periodic Inform (optical levels, WAN counters and
// the bit rates computed from them, the hosts behind the ONU, line and error counters), as a bounded time series.
//
// The series has three tiers so that it cannot grow without limit on a small disk:
//
//	Fine    every sample (one per Inform; never closer than a minute)   kept FineKeep (48 h), at most MaxFine points
//	Mid     5-minute buckets (the mean, and the peak bit rate)          kept MidKeep (14 days), at most MaxMid
//	Hourly  1-hour buckets                                               kept HourlyKeep (90 days), at most MaxHourly
//
// A sample that leaves a tier is merged into a bucket of the next one (Compact). The record is one row per ONU
// (key: the ONU id), written once per sample. Nothing in it is a secret.
const (
	FineKeep   = 48 * time.Hour
	MidKeep    = 14 * 24 * time.Hour
	HourlyKeep = 90 * 24 * time.Hour

	MidStep    = 300  // seconds
	HourlyStep = 3600 // seconds

	MaxFine   = 2880 // 1-minute samples for 48 h
	MaxMid    = 4032 // 5-minute buckets for 14 days
	MaxHourly = 2160 // 1-hour buckets for 90 days

	MaxMetricHosts = 64 // hosts kept per ONU
	MaxMetricRaw   = 40 // raw optical/line parameters kept per ONU
	MaxMetricGaps  = 24
)

// MetricPoint is one sample, or one bucket of samples. Fields the ONU did not give are nil.
type MetricPoint struct {
	T    int64    `json:"t"`           // unix seconds (the start of the bucket for Mid and Hourly)
	N    uint16   `json:"n,omitempty"` // samples merged into this point (0 = 1)
	Rx   *float64 `json:"rx,omitempty"`
	Tx   *float64 `json:"tx,omitempty"`
	Temp *float64 `json:"tp,omitempty"`
	Volt *float64 `json:"v,omitempty"`
	Bias *float64 `json:"b,omitempty"`
	Dn   *float64 `json:"dn,omitempty"`
	Up   *float64 `json:"up,omitempty"`
	DnPk *float64 `json:"dnp,omitempty"` // peak, for buckets
	UpPk *float64 `json:"upp,omitempty"`
}

// ONUOptical is the last optical reading. Raw keeps the parameters as the ONU returned them (name relative to
// Path), so a name this server did not map to a field is not lost.
type ONUOptical struct {
	Path string            `json:"path,omitempty"`
	Rx   *float64          `json:"rx,omitempty"` // dBm
	Tx   *float64          `json:"tx,omitempty"` // dBm
	Temp *float64          `json:"temp,omitempty"`
	Volt *float64          `json:"volt,omitempty"` // volts
	Bias *float64          `json:"bias,omitempty"` // mA
	Raw  map[string]string `json:"raw,omitempty"`
}

// ONUWAN is the last WAN reading: the byte counters, the bit rates computed from them, the line rate and the
// packet/error/discard counters.
type ONUWAN struct {
	Path     string            `json:"path,omitempty"`
	RxBytes  *uint64           `json:"rx_bytes,omitempty"`
	TxBytes  *uint64           `json:"tx_bytes,omitempty"`
	Dn       *float64          `json:"dn,omitempty"` // bit/s received (download)
	Up       *float64          `json:"up,omitempty"` // bit/s sent (upload)
	LineDn   *float64          `json:"line_dn,omitempty"`
	LineUp   *float64          `json:"line_up,omitempty"`
	Link     string            `json:"link,omitempty"`
	Access   string            `json:"access,omitempty"`
	Counters map[string]uint64 `json:"counters,omitempty"` // packets, errors, discards (totals)
	Deltas   map[string]uint64 `json:"deltas,omitempty"`   // the same since the previous sample
}

// ONUHost is one host behind the ONU (its LAN host table).
type ONUHost struct {
	Name    string   `json:"name,omitempty"`
	MAC     string   `json:"mac,omitempty"`
	IP      string   `json:"ip,omitempty"`
	Iface   string   `json:"iface,omitempty"` // the SSID, or "LAN"/"Ethernet"
	Active  bool     `json:"active,omitempty"`
	RxBytes *uint64  `json:"rx_bytes,omitempty"`
	TxBytes *uint64  `json:"tx_bytes,omitempty"`
	Dn      *float64 `json:"dn,omitempty"`
	Up      *float64 `json:"up,omitempty"`
}

// ONUMetricPaths are the object paths found once and used on every sample.
type ONUMetricPaths struct {
	Optical string   `json:"optical,omitempty"`
	WANs    []string `json:"wans,omitempty"`  // WANDevice.N. (or the TR-181 equivalent)
	Hosts   string   `json:"hosts,omitempty"` // ...Hosts.Host.
	Probed  bool     `json:"probed,omitempty"`
}

// ONUMetrics is the record of one ONU.
type ONUMetrics struct {
	ID       string         `json:"id"` // the ONU id
	Updated  time.Time      `json:"updated"`
	Uptime   int64          `json:"uptime,omitempty"` // seconds, as the ONU reports
	Reboots  int            `json:"reboots,omitempty"`
	LastBoot time.Time      `json:"last_boot,omitempty"`
	Optical  *ONUOptical    `json:"optical,omitempty"`
	WAN      *ONUWAN        `json:"wan,omitempty"`
	Hosts    []ONUHost      `json:"hosts,omitempty"`
	Paths    ONUMetricPaths `json:"paths"`
	Gaps     []string       `json:"gaps,omitempty"` // what the ONU did not give (for the operator to check)
	Diag     []string       `json:"diag,omitempty"` // TR-143 style diagnostics objects the ONU has (never run by the server)
	Fine     []MetricPoint  `json:"fine,omitempty"`
	Mid      []MetricPoint  `json:"mid,omitempty"`
	Hourly   []MetricPoint  `json:"hourly,omitempty"`
}

func (x ONUMetrics) norm() ONUMetrics {
	x.Updated, x.LastBoot = nt(x.Updated), nt(x.LastBoot)
	if len(x.Hosts) == 0 {
		x.Hosts = nil
	}
	if len(x.Gaps) == 0 {
		x.Gaps = nil
	}
	if len(x.Diag) == 0 {
		x.Diag = nil
	}
	if len(x.Fine) == 0 {
		x.Fine = nil
	}
	if len(x.Mid) == 0 {
		x.Mid = nil
	}
	if len(x.Hourly) == 0 {
		x.Hourly = nil
	}
	if len(x.Paths.WANs) == 0 {
		x.Paths.WANs = nil
	}
	return x
}

// Norm is the form the store keeps (for tests).
func (x ONUMetrics) Norm() ONUMetrics { return x.norm() }

// Clone copies the slices and maps, so a copy can be changed without touching the stored value.
func (x ONUMetrics) Clone() ONUMetrics { return cloneONUMetrics(x) }

func cloneONUMetrics(x ONUMetrics) ONUMetrics {
	x.Fine = append([]MetricPoint(nil), x.Fine...)
	x.Mid = append([]MetricPoint(nil), x.Mid...)
	x.Hourly = append([]MetricPoint(nil), x.Hourly...)
	x.Hosts = append([]ONUHost(nil), x.Hosts...)
	x.Gaps = append([]string(nil), x.Gaps...)
	x.Diag = append([]string(nil), x.Diag...)
	x.Paths.WANs = append([]string(nil), x.Paths.WANs...)
	if x.Optical != nil {
		o := *x.Optical
		o.Raw = cloneStrMap(o.Raw)
		x.Optical = &o
	}
	if x.WAN != nil {
		w := *x.WAN
		w.Counters, w.Deltas = cloneU64Map(w.Counters), cloneU64Map(w.Deltas)
		x.WAN = &w
	}
	return x
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneU64Map(m map[string]uint64) map[string]uint64 {
	if m == nil {
		return nil
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ONUMetricsOf returns the record of an ONU (an empty one when there is none yet).
func (d *Data) ONUMetricsOf(id string) ONUMetrics {
	if m, ok := d.ONUMetrics[id]; ok {
		return m
	}
	return ONUMetrics{ID: id}
}

// ---- retention ----

func meanPtr(a *float64, an float64, b *float64, bn float64) *float64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil:
		v := *a
		return &v
	}
	v := (*a*an + *b*bn) / (an + bn)
	return &v
}

func maxPtr(a, b *float64) *float64 {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil:
		v := *a
		return &v
	}
	v := *a
	if *b > v {
		v = *b
	}
	return &v
}

func pointN(p MetricPoint) float64 {
	if p.N == 0 {
		return 1
	}
	return float64(p.N)
}

// mergePoint folds b into a (the same bucket): means weighted by the number of samples, the largest peak.
func mergePoint(a, b MetricPoint) MetricPoint {
	an, bn := pointN(a), pointN(b)
	out := MetricPoint{T: a.T}
	out.Rx, out.Tx = meanPtr(a.Rx, an, b.Rx, bn), meanPtr(a.Tx, an, b.Tx, bn)
	out.Temp, out.Volt, out.Bias = meanPtr(a.Temp, an, b.Temp, bn), meanPtr(a.Volt, an, b.Volt, bn), meanPtr(a.Bias, an, b.Bias, bn)
	out.Dn, out.Up = meanPtr(a.Dn, an, b.Dn, bn), meanPtr(a.Up, an, b.Up, bn)
	pk := func(v, pv *float64) *float64 {
		if pv != nil {
			return pv
		}
		return v
	}
	out.DnPk = maxPtr(pk(a.Dn, a.DnPk), pk(b.Dn, b.DnPk))
	out.UpPk = maxPtr(pk(a.Up, a.UpPk), pk(b.Up, b.UpPk))
	n := an + bn
	if n > 65535 {
		n = 65535
	}
	out.N = uint16(n)
	return out
}

// foldInto puts p into the bucket (of step seconds) it belongs to in list (sorted by T), merging with the point
// already there.
func foldInto(list []MetricPoint, p MetricPoint, step int64) []MetricPoint {
	p.T -= p.T % step
	i := sort.Search(len(list), func(i int) bool { return list[i].T >= p.T })
	if i < len(list) && list[i].T == p.T {
		list[i] = mergePoint(list[i], p)
		return list
	}
	list = append(list, MetricPoint{})
	copy(list[i+1:], list[i:])
	list[i] = p
	return list
}

// Compact applies the retention: samples older than FineKeep move into 5-minute buckets, buckets older than MidKeep
// into hourly buckets, hourly buckets older than HourlyKeep go, and every tier has a hard cap (the oldest go first).
func (m *ONUMetrics) Compact(now time.Time) {
	sort.Slice(m.Fine, func(i, j int) bool { return m.Fine[i].T < m.Fine[j].T })
	cutF := now.Add(-FineKeep).Unix()
	keep := make([]MetricPoint, 0, len(m.Fine))
	for _, p := range m.Fine {
		if p.T < cutF {
			m.Mid = foldInto(m.Mid, p, MidStep)
		} else {
			keep = append(keep, p)
		}
	}
	m.Fine = keep
	cutM := now.Add(-MidKeep).Unix()
	midKeep := make([]MetricPoint, 0, len(m.Mid))
	for _, p := range m.Mid {
		if p.T < cutM {
			m.Hourly = foldInto(m.Hourly, p, HourlyStep)
		} else {
			midKeep = append(midKeep, p)
		}
	}
	m.Mid = midKeep
	cutH := now.Add(-HourlyKeep).Unix()
	hk := make([]MetricPoint, 0, len(m.Hourly))
	for _, p := range m.Hourly {
		if p.T >= cutH {
			hk = append(hk, p)
		}
	}
	m.Hourly = hk
	if len(m.Fine) > MaxFine {
		m.Fine = append([]MetricPoint(nil), m.Fine[len(m.Fine)-MaxFine:]...)
	}
	if len(m.Mid) > MaxMid {
		m.Mid = append([]MetricPoint(nil), m.Mid[len(m.Mid)-MaxMid:]...)
	}
	if len(m.Hourly) > MaxHourly {
		m.Hourly = append([]MetricPoint(nil), m.Hourly[len(m.Hourly)-MaxHourly:]...)
	}
}

// FoldInto puts p into the bucket of step seconds it belongs to in list (sorted by T), merging with the point
// already there. It is for building a coarser series of a range on the fly.
func FoldInto(list []MetricPoint, p MetricPoint, step int64) []MetricPoint {
	return foldInto(list, p, step)
}
