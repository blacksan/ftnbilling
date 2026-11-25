package acs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/store"
)

// The metrics as the API and the screens see them. Nothing here is secret.

// ErrBadRange: the range parameter is not one of the forms the API takes.
var ErrBadRange = errors.New("range must be a number and h or d (for example 6h, 24h, 7d), at most 90d")

// ParseRange reads "90m", "6h", "24h", "7d" (empty = 24h). The longest range is 90 days.
func ParseRange(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 24 * time.Hour, nil
	}
	if len(s) < 2 {
		return 0, ErrBadRange
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 1 {
		return 0, ErrBadRange
	}
	var d time.Duration
	switch s[len(s)-1] {
	case 'm':
		d = time.Duration(n) * time.Minute
	case 'h':
		d = time.Duration(n) * time.Hour
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	default:
		return 0, ErrBadRange
	}
	if d < time.Minute || d > store.HourlyKeep {
		return 0, ErrBadRange
	}
	return d, nil
}

// OpticalPoint is one point of the optical history.
type OpticalPoint struct {
	T       int64    `json:"t"` // unix seconds
	RxDBm   *float64 `json:"rx_dbm,omitempty"`
	TxDBm   *float64 `json:"tx_dbm,omitempty"`
	TempC   *float64 `json:"temp_c,omitempty"`
	VoltV   *float64 `json:"voltage_v,omitempty"`
	BiasMA  *float64 `json:"bias_ma,omitempty"`
	Samples int      `json:"samples,omitempty"`
}

// RatePoint is one point of the throughput series.
type RatePoint struct {
	T       int64    `json:"t"`
	DownBps *float64 `json:"down_bps,omitempty"`
	UpBps   *float64 `json:"up_bps,omitempty"`
	DownPk  *float64 `json:"down_peak_bps,omitempty"`
	UpPk    *float64 `json:"up_peak_bps,omitempty"`
	Samples int      `json:"samples,omitempty"`
}

// OpticalNow is the last optical reading.
type OpticalNow struct {
	RxDBm  *float64          `json:"rx_dbm"`
	TxDBm  *float64          `json:"tx_dbm"`
	TempC  *float64          `json:"temp_c"`
	VoltV  *float64          `json:"voltage_v"`
	BiasMA *float64          `json:"bias_ma"`
	Path   string            `json:"path,omitempty"`
	Raw    map[string]string `json:"raw,omitempty"` // the parameters as the ONU returned them
}

// RateNow is the last WAN reading.
type RateNow struct {
	DownBps   *float64 `json:"down_bps"`
	UpBps     *float64 `json:"up_bps"`
	RxBytes   *uint64  `json:"rx_bytes"`
	TxBytes   *uint64  `json:"tx_bytes"`
	LineDown  *float64 `json:"line_down_bps"`
	LineUp    *float64 `json:"line_up_bps"`
	Path      string   `json:"path,omitempty"`
	CounterAt int64    `json:"at,omitempty"`
}

// DeviceView is one host behind the ONU.
type DeviceView struct {
	Name    string   `json:"name"`
	MAC     string   `json:"mac"`
	IP      string   `json:"ip"`
	Iface   string   `json:"interface"`
	Active  bool     `json:"active"`
	DownBps *float64 `json:"down_bps"`
	UpBps   *float64 `json:"up_bps"`
	RxBytes *uint64  `json:"rx_bytes"`
	TxBytes *uint64  `json:"tx_bytes"`
}

// QoSView is the line and error side of the sample.
type QoSView struct {
	LinkStatus string            `json:"link_status,omitempty"`
	AccessType string            `json:"access_type,omitempty"`
	LineDown   *float64          `json:"line_down_bps"`
	LineUp     *float64          `json:"line_up_bps"`
	UptimeS    int64             `json:"uptime_s"`
	Reboots    int               `json:"reboots"`
	LastBootAt *time.Time        `json:"last_boot_at,omitempty"`
	Counters   map[string]uint64 `json:"counters"`        // totals since the ONU started: packets, errors, discards
	Deltas     map[string]uint64 `json:"deltas"`          // growth since the previous sample
	Diag       []string          `json:"tr143_available"` // diagnostics objects the ONU has; the server never runs them
}

// MetricsView is the answer of GET /api/v1/onus/{id}/metrics.
type MetricsView struct {
	ONUID      string       `json:"onu_id"`
	Serial     string       `json:"serial"`
	Range      string       `json:"range"`
	StepS      int          `json:"step_s"` // the spacing of the series: 0 = every sample, 300 or 3600 = buckets
	UpdatedAt  *time.Time   `json:"updated_at"`
	Optical    OpticalBlock `json:"optical"`
	Throughput RateBlock    `json:"throughput"`
	Devices    DevicesBlock `json:"devices"`
	QoS        QoSView      `json:"qos"`
	Gaps       []string     `json:"gaps"` // what the ONU did not give (for the operator to check against the box)
}

type OpticalBlock struct {
	Current *OpticalNow    `json:"current"`
	History []OpticalPoint `json:"history"`
}

type RateBlock struct {
	Current *RateNow    `json:"current"`
	Series  []RatePoint `json:"series"`
}

type DevicesBlock struct {
	Count int          `json:"count"` // hosts the ONU lists as active
	Total int          `json:"total"` // every row of its host table
	List  []DeviceView `json:"list"`
}

// seriesFor picks the points of a range at one spacing: raw samples up to 48 hours, 5-minute buckets up to 14
// days, hourly buckets beyond (finer points are folded into the buckets of the chosen step).
func seriesFor(m store.ONUMetrics, rng time.Duration, now time.Time) (pts []store.MetricPoint, step int) {
	since := now.Add(-rng).Unix()
	switch {
	case rng <= store.FineKeep:
		step = 0
	case rng <= store.MidKeep:
		step = store.MidStep
	default:
		step = store.HourlyStep
	}
	add := func(list []store.MetricPoint) {
		for _, p := range list {
			if p.T < since-int64(step) {
				continue
			}
			if step > 0 {
				pts = store.FoldInto(pts, p, int64(step))
			} else {
				pts = append(pts, p)
			}
		}
	}
	if step == store.HourlyStep {
		add(m.Hourly)
	}
	if step >= store.MidStep {
		add(m.Mid)
	}
	add(m.Fine)
	return pts, step
}

// Metrics returns the metrics of an ONU for a range. ok is false when the ONU is unknown.
func (s *Service) Metrics(onuID string, rng time.Duration) (MetricsView, bool) {
	var o store.ONU
	var m store.ONUMetrics
	var found bool
	s.st.View(func(d *store.Data) {
		o, found = d.ONUs[onuID]
		m = d.ONUMetricsOf(onuID).Clone()
	})
	if !found {
		return MetricsView{}, false
	}
	now := s.now()
	v := MetricsView{ONUID: o.ID, Serial: o.Serial, Range: rangeText(rng), Gaps: []string{}}
	v.Optical.History, v.Throughput.Series = []OpticalPoint{}, []RatePoint{}
	if !m.Updated.IsZero() {
		t := m.Updated
		v.UpdatedAt = &t
	}
	if m.Optical != nil {
		v.Optical.Current = &OpticalNow{RxDBm: m.Optical.Rx, TxDBm: m.Optical.Tx, TempC: m.Optical.Temp, VoltV: m.Optical.Volt, BiasMA: m.Optical.Bias,
			Path: m.Optical.Path, Raw: m.Optical.Raw}
	}
	pts, step := seriesFor(m, rng, now)
	v.StepS = step
	for _, p := range pts {
		n := int(p.N)
		if n == 0 {
			n = 1
		}
		if p.Rx != nil || p.Tx != nil || p.Temp != nil || p.Volt != nil || p.Bias != nil {
			v.Optical.History = append(v.Optical.History, OpticalPoint{T: p.T, RxDBm: p.Rx, TxDBm: p.Tx, TempC: p.Temp, VoltV: p.Volt, BiasMA: p.Bias, Samples: n})
		}
		if p.Dn != nil || p.Up != nil {
			v.Throughput.Series = append(v.Throughput.Series, RatePoint{T: p.T, DownBps: p.Dn, UpBps: p.Up, DownPk: p.DnPk, UpPk: p.UpPk, Samples: n})
		}
	}
	v.QoS = QoSView{UptimeS: m.Uptime, Reboots: m.Reboots, Counters: map[string]uint64{}, Deltas: map[string]uint64{}, Diag: append([]string{}, m.Diag...)}
	if !m.LastBoot.IsZero() {
		t := m.LastBoot
		v.QoS.LastBootAt = &t
	}
	if m.WAN != nil {
		v.Throughput.Current = &RateNow{DownBps: m.WAN.Dn, UpBps: m.WAN.Up, RxBytes: m.WAN.RxBytes, TxBytes: m.WAN.TxBytes, LineDown: m.WAN.LineDn, LineUp: m.WAN.LineUp, Path: m.WAN.Path}
		v.QoS.LinkStatus, v.QoS.AccessType, v.QoS.LineDown, v.QoS.LineUp = m.WAN.Link, m.WAN.Access, m.WAN.LineDn, m.WAN.LineUp
		for k, n := range m.WAN.Counters {
			v.QoS.Counters[k] = n
		}
		for k, n := range m.WAN.Deltas {
			v.QoS.Deltas[k] = n
		}
	}
	v.Devices.List = []DeviceView{}
	for _, h := range sortedHosts(m.Hosts) {
		v.Devices.List = append(v.Devices.List, DeviceView{Name: h.Name, MAC: h.MAC, IP: h.IP, Iface: h.Iface, Active: h.Active, DownBps: h.Dn, UpBps: h.Up, RxBytes: h.RxBytes, TxBytes: h.TxBytes})
		v.Devices.Total++
		if h.Active {
			v.Devices.Count++
		}
	}
	v.Gaps = append(v.Gaps, m.Gaps...)
	return v, true
}

func rangeText(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}

// MetricsBrief is the small read-out of the ONU page: the current optical levels, the number of devices behind the
// ONU and the current throughput, as text.
type MetricsBrief struct {
	Optical string    // "Rx -21.5 dBm, Tx 2.5 dBm, 41 C" ("" = not read)
	Devices int       // active hosts
	HasDev  bool      // the ONU gave a host table
	Rate    string    // "12.3 Mbit/s down, 1.2 Mbit/s up" ("" = not computed yet)
	At      time.Time // when it was read
}

// BitrateText writes a bit rate: "950 bit/s", "12.3 kbit/s", "12.3 Mbit/s", "1.2 Gbit/s".
func BitrateText(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.1f Gbit/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.1f Mbit/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1f kbit/s", bps/1e3)
	}
	return fmt.Sprintf("%.0f bit/s", bps)
}

// Brief returns the read-out for the ONU of an account.
func (s *Service) Brief(onuID string) (MetricsBrief, bool) {
	var m store.ONUMetrics
	var ok bool
	s.st.View(func(d *store.Data) { m, ok = d.ONUMetrics[onuID]; m = m.Clone() })
	if !ok || m.Updated.IsZero() {
		return MetricsBrief{}, false
	}
	b := MetricsBrief{At: m.Updated, HasDev: m.Paths.Hosts != "" && m.Hosts != nil}
	if o := m.Optical; o != nil {
		var parts []string
		if o.Rx != nil {
			parts = append(parts, fmt.Sprintf("Rx %.1f dBm", *o.Rx))
		}
		if o.Tx != nil {
			parts = append(parts, fmt.Sprintf("Tx %.1f dBm", *o.Tx))
		}
		if o.Temp != nil {
			parts = append(parts, fmt.Sprintf("%.0f C", *o.Temp))
		}
		b.Optical = strings.Join(parts, ", ")
	}
	for _, h := range m.Hosts {
		if h.Active {
			b.Devices++
		}
	}
	if w := m.WAN; w != nil && (w.Dn != nil || w.Up != nil) {
		var parts []string
		if w.Dn != nil {
			parts = append(parts, BitrateText(*w.Dn)+" down")
		}
		if w.Up != nil {
			parts = append(parts, BitrateText(*w.Up)+" up")
		}
		b.Rate = strings.Join(parts, ", ")
	}
	return b, true
}
