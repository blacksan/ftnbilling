package web

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ispbill/internal/acs"
	"ispbill/internal/metrics"
	"ispbill/internal/store"
)

// The ONU metrics dashboards: speed over time, optical history, the devices behind the ONU and the line/QoS read-out.
// Read-only: it draws what the ACS already collected (store.ONUMetrics). The charts are server-drawn SVG (no script),
// and every device-supplied string (host names, interface names) goes through html/template as text.

// dashRanges are the windows the page offers.
var dashRanges = []struct{ Key, Label string }{{"1h", "1 hour"}, {"6h", "6 hours"}, {"24h", "24 hours"}, {"7d", "7 days"}, {"30d", "30 days"}, {"90d", "90 days"}}

// Normal operating bands drawn behind the optical charts. They are the usual GPON class B+ figures, a guide for the eye
// and not something the ONU reports.
var (
	rxBand = metrics.Band{Lo: -25, Hi: -8, Label: "usual healthy Rx range (-25 to -8 dBm)"}
	txBand = metrics.Band{Lo: 0.5, Hi: 5, Label: "usual ONU Tx range (0.5 to 5 dBm)"}
)

type dashDevice struct {
	Name, MAC, IP, Iface string
	Active               bool
	Down, Up             string
	DownBps, UpBps       float64
	Rx, Tx               string
}

type dashRow struct {
	When, A, B string
}

type dashCounter struct {
	Name         string
	Total, Delta uint64
}

func (s *Server) onuMetricsPage(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	u, _ := userOf(r)
	if u.Role != store.RoleOwner && u.Role != store.RoleOperator {
		s.notice(w, r, http.StatusForbidden, "Not allowed", "Only an operator or an owner can see an ONU's metrics.", "/admin/onus", "Back")
		return
	}
	q := r.URL.Query()
	rangeKey := q.Get("range")
	if rangeKey == "" {
		rangeKey = "24h"
	}
	valid := false
	for _, k := range dashRanges {
		valid = valid || k.Key == rangeKey
	}
	rng, err := acs.ParseRange(rangeKey)
	if err != nil || !valid {
		s.notice(w, r, http.StatusBadRequest, "Unknown range", "Choose 1h, 6h, 24h, 7d, 30d or 90d.", "/admin/onus", "Back")
		return
	}
	id := q.Get("onu")
	v, ok := s.acs.Dashboard(id, rng)
	if !ok {
		s.notice(w, r, http.StatusNotFound, "ONU not found", "That ONU is not known.", "/admin/onus", "Back")
		return
	}
	s.audit(r, "ONU "+v.Serial+" metrics viewed by "+u.Username)
	w.Header().Set("Cache-Control", "no-store")

	p := s.page(r, "onus")
	p.eqCrumb(eqTabONU)
	p.Detail = "Metrics of ONU " + v.Serial
	d := p.Data
	d["V"], d["Range"], d["Ranges"] = v, rangeKey, dashRanges
	d["ONUID"] = id
	d["UpdatedText"] = ""
	if v.UpdatedAt != nil {
		d["UpdatedText"] = v.UpdatedAt.In(metrics.EAT).Format("2 Jan 2006, 15:04") + " EAT"
	}
	d["BucketText"] = bucketText(v.StepS, v.BucketS)
	d["SpeedChart"], d["SpeedUnit"], d["SpeedRows"] = speedChart(v)
	d["RxChart"], d["TxChart"], d["OptRows"] = opticalCharts(v)
	d["TempChart"], d["VoltChart"], d["BiasChart"] = minorCharts(v)
	sortKey := q.Get("sort")
	d["Sort"] = sortKey
	d["Devices"] = deviceRows(v.Devices.List, sortKey)
	d["Counters"] = counterRows(v.QoS)
	d["UptimeText"] = uptimeText(v.QoS.UptimeS)
	d["LineDown"], d["LineUp"] = bitrateOrDash(v.QoS.LineDown), bitrateOrDash(v.QoS.LineUp)
	s.render(w, "admin_onu_metrics", p)
}

func bitrateOrDash(v *float64) string {
	if v == nil {
		return "not reported"
	}
	return acs.BitrateText(*v)
}

func bucketText(step, bucket int) string {
	w := step
	if bucket > w {
		w = bucket
	}
	switch {
	case w <= 0:
		return "every reading"
	case w < 3600:
		return fmt.Sprintf("%d-minute averages", (w+59)/60)
	}
	return fmt.Sprintf("%d-hour averages", (w+3599)/3600)
}

func uptimeText(sec int64) string {
	if sec <= 0 {
		return "not reported"
	}
	dd, h, m := sec/86400, sec/3600%24, sec/60%60
	switch {
	case dd > 0:
		return fmt.Sprintf("%dd %dh", dd, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func xLayoutFor(v acs.DashboardView) string {
	if v.To.Sub(v.From) <= 36*time.Hour {
		return "15:04"
	}
	return "2 Jan"
}

func gapFor(v acs.DashboardView) int64 {
	g := int64(3 * v.BucketS)
	if s := int64(3 * v.StepS); s > g {
		g = s
	}
	if g < 1800 {
		g = 1800
	}
	return g
}

func nan(p *float64, scale float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p * scale
}

// speedChart draws download and upload over the window, in the unit that fits the peak, with the line rate as a dashed cap.
func speedChart(v acs.DashboardView) (template.HTML, string, []dashRow) {
	maxBps := 0.0
	for _, p := range v.Throughput.Series {
		for _, x := range []*float64{p.DownBps, p.UpBps} {
			if x != nil {
				maxBps = math.Max(maxBps, *x)
			}
		}
	}
	unit, scale := "Mbit/s", 1e-6
	switch {
	case maxBps < 1e6 && maxBps > 0:
		unit, scale = "kbit/s", 1e-3
	case maxBps >= 1e9:
		unit, scale = "Gbit/s", 1e-9
	}
	n := len(v.Throughput.Series)
	t := make([]time.Time, n)
	dn, up := make([]float64, n), make([]float64, n)
	rows := make([]dashRow, 0, 24)
	for i, p := range v.Throughput.Series {
		t[i] = time.Unix(p.T, 0)
		dn[i], up[i] = nan(p.DownBps, scale), nan(p.UpBps, scale)
	}
	for _, i := range evenIdx(n, 24) {
		p := v.Throughput.Series[i]
		rows = append(rows, dashRow{When: whenText(p.T, v), A: rateCell(p.DownBps), B: rateCell(p.UpBps)})
	}
	opts := metrics.TimeChartOpts{From: v.From, To: v.To, T: t, Unit: unit, ZeroBase: true, GapSecs: gapFor(v), XLayout: xLayoutFor(v),
		Label: "WAN throughput over time, download and upload", Empty: "No throughput readings in this window yet. They are computed from the ONU's WAN byte counters at each check-in.",
		Series: []metrics.TimeLine{{Name: "download", Color: metrics.ColDown, Vals: dn, Area: true}, {Name: "upload", Color: metrics.ColUp, Vals: up, Area: true}}}
	if v.QoS.LineDown != nil && *v.QoS.LineDown > 0 && maxBps > 0 && *v.QoS.LineDown*scale <= 4*maxBps*scale {
		opts.CapLine, opts.CapLabel = *v.QoS.LineDown*scale, "line rate (down)"
	}
	return metrics.TimeChart(opts), unit, rows
}

func rateCell(p *float64) string {
	if p == nil {
		return "-"
	}
	return acs.BitrateText(*p)
}

func whenText(unix int64, v acs.DashboardView) string {
	return time.Unix(unix, 0).In(metrics.EAT).Format(map[bool]string{true: "15:04", false: "2 Jan 15:04"}[v.To.Sub(v.From) <= 36*time.Hour])
}

// evenIdx picks at most n indices spread evenly over a list of length total (always the first and the last).
func evenIdx(total, n int) []int {
	if total <= 0 {
		return nil
	}
	if total <= n {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, n)
	for k := range out {
		out[k] = int(math.Round(float64(k) * float64(total-1) / float64(n-1)))
	}
	return out
}

func opticalSeries(v acs.DashboardView, get func(acs.OpticalPoint) *float64) ([]time.Time, []float64) {
	t := make([]time.Time, len(v.Optical.History))
	vals := make([]float64, len(t))
	for i, p := range v.Optical.History {
		t[i], vals[i] = time.Unix(p.T, 0), nan(get(p), 1)
	}
	return t, vals
}

func opticalCharts(v acs.DashboardView) (rx, tx template.HTML, rows []dashRow) {
	mk := func(label, unit, name, color string, band metrics.Band, get func(acs.OpticalPoint) *float64) template.HTML {
		t, vals := opticalSeries(v, get)
		return metrics.TimeChart(metrics.TimeChartOpts{From: v.From, To: v.To, T: t, Unit: unit, Band: &band, GapSecs: gapFor(v), XLayout: xLayoutFor(v), Label: label,
			Empty:  "No " + name + " readings in this window. The ONU may not report its optical levels (see what the ONU did not give, below).",
			Series: []metrics.TimeLine{{Name: name, Color: color, Vals: vals}}})
	}
	rx = mk("Optical receive power over time (dBm)", "dBm", "Rx power", metrics.ColDown, rxBand, func(p acs.OpticalPoint) *float64 { return p.RxDBm })
	tx = mk("Optical transmit power over time (dBm)", "dBm", "Tx power", metrics.ColUp, txBand, func(p acs.OpticalPoint) *float64 { return p.TxDBm })
	for _, i := range evenIdx(len(v.Optical.History), 24) {
		p := v.Optical.History[i]
		rows = append(rows, dashRow{When: whenText(p.T, v), A: dbm(p.RxDBm), B: dbm(p.TxDBm)})
	}
	return
}

func dbm(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f dBm", *p)
}

// minorCharts are the small optical-module charts (temperature, voltage, laser bias): only what the ONU gave.
func minorCharts(v acs.DashboardView) (temp, volt, bias template.HTML) {
	mk := func(label, unit string, color string, get func(acs.OpticalPoint) *float64) template.HTML {
		t, vals := opticalSeries(v, get)
		return metrics.TimeChart(metrics.TimeChartOpts{From: v.From, To: v.To, T: t, Unit: unit, GapSecs: gapFor(v), XLayout: xLayoutFor(v), Label: label, Height: 170,
			Empty: "Not reported by this ONU in this window.", Series: []metrics.TimeLine{{Name: label, Color: color, Vals: vals}}})
	}
	temp = mk("Module temperature", "°C", metrics.ColCap, func(p acs.OpticalPoint) *float64 { return p.TempC })
	volt = mk("Module voltage", "V", metrics.ColViol, func(p acs.OpticalPoint) *float64 { return p.VoltV })
	bias = mk("Laser bias current", "mA", metrics.ColMag, func(p acs.OpticalPoint) *float64 { return p.BiasMA })
	return
}

func byteText(p *uint64) string {
	if p == nil {
		return "-"
	}
	f := float64(*p)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.1f kB", f/(1<<10))
	}
	return fmt.Sprintf("%d B", *p)
}

// deviceRows is the host table, sorted by ?sort= (name, ip, mac, link, down, up; the rates are biggest first).
func deviceRows(in []acs.DeviceView, key string) []dashDevice {
	out := make([]dashDevice, 0, len(in))
	for _, h := range in {
		dv := dashDevice{Name: h.Name, MAC: h.MAC, IP: h.IP, Iface: h.Iface, Active: h.Active, Down: rateCell(h.DownBps), Up: rateCell(h.UpBps), Rx: byteText(h.RxBytes), Tx: byteText(h.TxBytes)}
		if h.DownBps != nil {
			dv.DownBps = *h.DownBps
		}
		if h.UpBps != nil {
			dv.UpBps = *h.UpBps
		}
		out = append(out, dv)
	}
	txt := func(f func(dashDevice) string) func(i, j int) bool {
		return func(i, j int) bool { return strings.ToLower(f(out[i])) < strings.ToLower(f(out[j])) }
	}
	switch key {
	case "name":
		sort.SliceStable(out, txt(func(d dashDevice) string { return d.Name }))
	case "ip":
		sort.SliceStable(out, txt(func(d dashDevice) string { return d.IP }))
	case "mac":
		sort.SliceStable(out, txt(func(d dashDevice) string { return d.MAC }))
	case "link":
		sort.SliceStable(out, txt(func(d dashDevice) string { return d.Iface }))
	case "down":
		sort.SliceStable(out, func(i, j int) bool { return out[i].DownBps > out[j].DownBps })
	case "up":
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpBps > out[j].UpBps })
	}
	return out
}

func counterRows(q acs.QoSView) []dashCounter {
	out := make([]dashCounter, 0, len(q.Counters))
	for k, n := range q.Counters {
		out = append(out, dashCounter{Name: k, Total: n, Delta: q.Deltas[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// dashHref keeps the ONU and window while changing one parameter.
func dashHref(id, rng, sortKey string) string {
	h := "/admin/onus/metrics?onu=" + url.QueryEscape(id) + "&range=" + url.QueryEscape(rng)
	if sortKey != "" {
		h += "&sort=" + url.QueryEscape(sortKey)
	}
	return h
}
