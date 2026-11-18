package web

import (
	"encoding/csv"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/metrics"
)

func (s *Server) metricsRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/metrics", s.adminMetrics)
	get("/admin/metrics.csv", s.adminMetricsCSV)
	get("/admin/metrics/flat", s.adminMetricsFlat)
	post("/admin/metrics/demo", s.adminMetricsDemo)
}

// collectorFor returns the collector of the selected building. With "all
// buildings" selected, or a building that is not collecting yet, it redirects
// with an explanation and returns false.
func (s *Server) collectorFor(w http.ResponseWriter, r *http.Request, site string) (*metrics.Collector, string, bool) {
	if site == "" {
		sc := s.scopeOf(r)
		if sc.All {
			s.back(w, r, "/admin/sites", "Choose a building to see its usage.")
			return nil, "", false
		}
		site = sc.ID
	}
	col := s.mgr.Collector(site)
	if col == nil {
		s.back(w, r, "/admin/sites/view?id="+url.QueryEscape(site), "This building is not collecting yet: it is paused or not joined.")
		return nil, "", false
	}
	return col, site, true
}

func mbpsSeries(name, color string, v []float64) metrics.LineSeries {
	return metrics.LineSeries{Name: name, Color: color, Vals: v}
}

func tbText(gb float64) string {
	if gb >= 1000 {
		return fmt.Sprintf("%.2f TB", gb/1000)
	}
	return fmt.Sprintf("%.1f GB", gb)
}

func svcBars(rows []metrics.SvcRow) template.HTML {
	var b []metrics.BarRow
	for _, r := range rows {
		b = append(b, metrics.BarRow{Label: r.Name, Value: r.GB, Text: fmt.Sprintf("%s · %.0f%%", tbText(r.GB), r.SharePct)})
	}
	return metrics.BarsH(b, metrics.ColDown, "No service data yet. It fills when the router sends traffic-flow data (see README).")
}

func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	col, _, ok := s.collectorFor(w, r, "")
	if !ok {
		return
	}
	pg := s.page(r, "metrics")
	period := metrics.ParsePeriod(r.URL.Query().Get("period"))
	sortKey := r.URL.Query().Get("sort")
	rep := col.BuildReport(period, s.svc.Now())
	metrics.SortFlats(rep.Flats, sortKey)
	lay := metrics.XLayout(rep.Tier, period)
	cfg := col.Config()

	wan := metrics.LineChart(metrics.LineOpts{T: rep.WAN.T, XLayout: lay, Unit: "Mbps", CapLine: cfg.WANCapMbps, CapLabel: fmt.Sprintf("capacity %.0f", cfg.WANCapMbps),
		Series: []metrics.LineSeries{mbpsSeries("download", metrics.ColDown, rep.WAN.Vals["down"]), mbpsSeries("upload", metrics.ColUp, rep.WAN.Vals["up"])},
		Empty:  "No WAN data yet. It appears once the router answers."})
	olt := metrics.LineChart(metrics.LineOpts{T: rep.OLT.T, XLayout: lay, Unit: "Mbps",
		Series: []metrics.LineSeries{mbpsSeries("to flats", metrics.ColDown, rep.OLT.Vals["down"]), mbpsSeries("from flats", metrics.ColUp, rep.OLT.Vals["up"])},
		Empty:  "No OLT port data yet."})

	// Top 10 flats by download.
	top := append([]metrics.FlatRow(nil), rep.Flats...)
	metrics.SortFlats(top, "down")
	var bars []metrics.BarRow
	for i, f := range top {
		if i >= 10 || f.DownGB <= 0 {
			break
		}
		bars = append(bars, metrics.BarRow{Label: f.Flat, Value: f.DownGB, Text: fmt.Sprintf("%s · %.0f%%", tbText(f.DownGB), f.SharePct),
			Href: "/admin/metrics/flat?code=" + f.Code + "&period=" + string(period)})
	}
	topChart := metrics.BarsH(bars, metrics.ColDown, "No flat traffic recorded for this period yet.")

	cols := []string{metrics.ColViol, metrics.ColAqua, metrics.ColMag}
	var lat []metrics.LineSeries
	targets := make([]string, 0, len(rep.Latency.Vals))
	for t := range rep.Latency.Vals {
		targets = append(targets, t)
	}
	sortStrings(targets)
	for i, t := range targets {
		lat = append(lat, metrics.LineSeries{Name: t, Color: cols[i%len(cols)], Vals: rep.Latency.Vals[t]})
	}
	latency := metrics.LineChart(metrics.LineOpts{T: rep.Latency.T, XLayout: lay, Unit: "ms (average round trip)", Series: lat, Empty: "No ping data yet."})
	loss := metrics.LineChart(metrics.LineOpts{T: rep.Loss.T, XLayout: lay, Unit: "% packets lost", Height: 200,
		Series: []metrics.LineSeries{{Name: "loss", Color: metrics.ColCap, Vals: rep.Loss.Vals["loss"]}}, Empty: "No ping data yet."})

	pg.Data = map[string]any{
		"R": rep, "Periods": metrics.Periods, "Period": string(period), "Sort": sortKey,
		"WANChart": wan, "OLTChart": olt, "TopChart": topChart, "SvcChart": svcBars(rep.Services),
		"LatChart": latency, "LossChart": loss, "Busy": metrics.HourStrip(rep.Busy),
		"Cfg": cfg, "Mock": s.cfg.RouterMock, "FUPTB": cfg.FUPTB,
		"FUPTBUsed": rep.MonthGB / 1000, "FUPBar": clampPct(rep.FUPPct),
		"C": map[string]any{"WANUtil": metrics.QoEWANUtilPct, "WANHours": metrics.QoEWANHours, "CapShare": metrics.QoECapSharePct,
			"MinOnline": metrics.QoEMinOnlineMin, "Loss": metrics.QoELossPct, "FUPWarn": metrics.FUPWarnPct},
		"Now": s.svc.Now(),
	}
	s.render(w, "admin_metrics", pg)
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func (s *Server) adminMetricsFlat(w http.ResponseWriter, r *http.Request) {
	pg := s.page(r, "metrics")
	period := metrics.ParsePeriod(r.URL.Query().Get("period"))
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	acct, found := s.svc.AccountByCode(code)
	if !found {
		http.NotFound(w, r)
		return
	}
	col, _, ok := s.collectorFor(w, r, acct.SiteID)
	if !ok {
		return
	}
	fr := col.BuildFlat(code, period, s.svc.Now())
	if !fr.Found {
		http.NotFound(w, r)
		return
	}
	lay := metrics.XLayout(fr.Tier, period)
	line := metrics.LineChart(metrics.LineOpts{T: fr.Series.T, XLayout: lay, Unit: "Mbps", CapLine: fr.Row.CapMbps, CapLabel: fmt.Sprintf("package %.0f", fr.Row.CapMbps),
		Series: []metrics.LineSeries{mbpsSeries("download", metrics.ColDown, fr.Series.Vals["down"]), mbpsSeries("upload", metrics.ColUp, fr.Series.Vals["up"])},
		Empty:  "No traffic recorded for this flat in this period."})
	pg.Data = map[string]any{"F": fr, "Row": fr.Row, "Period": string(period), "Periods": metrics.Periods,
		"Chart": line, "SvcChart": svcBars(fr.Services)}
	s.render(w, "admin_metrics_flat", pg)
}

func (s *Server) adminMetricsCSV(w http.ResponseWriter, r *http.Request) {
	col, _, ok := s.collectorFor(w, r, "")
	if !ok {
		return
	}
	period := metrics.ParsePeriod(r.URL.Query().Get("period"))
	rep := col.BuildReport(period, s.svc.Now())
	metrics.SortFlats(rep.Flats, r.URL.Query().Get("sort"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="flat-usage-`+string(period)+`.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"flat", "account_code", "package", "state", "download_gb", "upload_gb", "share_pct", "peak_mbps", "pct_time_at_cap", "online_hours", "current_mbps", "top_services"})
	for _, f := range rep.Flats {
		var tops []string
		for _, t := range f.Top {
			tops = append(tops, fmt.Sprintf("%s %.0f%%", t.Name, t.SharePct))
		}
		cur := ""
		if f.CurrentMbps >= 0 {
			cur = fmt.Sprintf("%.2f", f.CurrentMbps)
		}
		cw.Write([]string{csvSafe(f.Flat), f.Code, csvSafe(f.Package), f.State, fmt.Sprintf("%.3f", f.DownGB), fmt.Sprintf("%.3f", f.UpGB),
			fmt.Sprintf("%.1f", f.SharePct), fmt.Sprintf("%.2f", f.PeakMbps), fmt.Sprintf("%.1f", f.CapPct),
			fmt.Sprintf("%.1f", f.OnlineMin/60), cur, strings.Join(tops, "; ")})
	}
	cw.Flush()
}

// adminMetricsDemo fills synthetic history (ROUTER_MOCK only).
func (s *Server) adminMetricsDemo(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.RouterMock {
		http.Error(w, "demo data is only available with ROUTER_MOCK=true", http.StatusForbidden)
		return
	}
	days := 35
	if n, _ := strconv.Atoi(r.PostFormValue("days")); n > 0 && n <= 60 {
		days = n
	}
	col, _, ok := s.collectorFor(w, r, "")
	if !ok {
		return
	}
	col.SeedDemo(s.svc.Now(), days)
	s.back(w, r, "/admin/metrics?period="+url.QueryEscape(r.PostFormValue("period")), "Demo history generated ("+strconv.Itoa(days)+" days)")
}

var _ = time.Second
var _ = billing.EAT
