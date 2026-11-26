package metrics

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
	"time"
)

// Chart palette (validated reference palette, light surface).
const (
	ColDown  = "#0072b2" // download / primary
	ColUp    = "#009e73" // upload
	ColAqua  = "#1b7fb8"
	ColViol  = "#b8628f"
	ColMag   = "#64748b"
	ColCap   = "#c0283d" // capacity / limit line
	colInk   = "#0f172a"
	colMuted = "#475569"
	colGrid  = "#cbd5e1"
)

// LineSeries is one line of a LineChart. NaN values break the line.
type LineSeries struct {
	Name  string
	Color string
	Vals  []float64
}

// LineOpts configures LineChart.
type LineOpts struct {
	T        []time.Time
	Series   []LineSeries
	Unit     string  // y axis unit, e.g. "Mbps"
	CapLine  float64 // dashed red line, 0 = none
	CapLabel string
	XLayout  string // Go time layout for x labels (EAT)
	YMax     float64
	Height   int
	Empty    string // text when there is no data
}

func esc(s string) string { return html.EscapeString(s) }

// niceStep picks a round tick step for roughly n intervals over max.
func niceStep(max float64, n int) float64 {
	if max <= 0 {
		return 1
	}
	raw := max / float64(n)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if raw <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

func fmtTick(v float64) string {
	switch {
	case v >= 100 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// chartBox wraps a chart so that on a narrow screen it scrolls sideways instead of
// shrinking: the SVG never renders narrower than its drawing width, so axis text keeps
// at least its designed pixel size (11 to 14 px) on every viewport.
// tokClass maps a series colour onto the theme's chart token (and the danger colour for the
// capacity line), so the SVG follows light/dark; the hex stays as the light fallback.
func tokClass(kind, c string) string {
	t := ""
	switch strings.ToLower(c) {
	case ColDown:
		t = "1"
	case ColUp:
		t = "2"
	case ColViol:
		t = "3"
	case ColMag:
		t = "4"
	case ColAqua:
		t = "5"
	case ColCap:
		t = "b"
	}
	if t == "" {
		return ""
	}
	return ` class="` + kind + t + `"`
}

// seriesDash gives each series its own dash pattern, so lines differ by more than colour.
func seriesDash(i int) string {
	switch i % 4 {
	case 1:
		return ` stroke-dasharray="9 3"`
	case 2:
		return ` stroke-dasharray="1 4.5"`
	case 3:
		return ` stroke-dasharray="9 3 2 3"`
	}
	return ""
}

func chartBox(svg string) template.HTML {
	return template.HTML(`<div class="chartbox" tabindex="0" role="region" aria-label="Chart, scrolls sideways on small screens">` + svg + `</div>`)
}

func svgOpen(w, h int, label string) *strings.Builder {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s" style="--w:%dpx;width:100%%;max-width:%dpx;height:auto;display:block;font-family:inherit">`, w, h, esc(label), w, w*5/4)
	return &b
}

// emptyChart is the no-data state: a plain themed message (no SVG, so its text never scales).
func emptyChart(w, h int, label, msg string) template.HTML {
	return template.HTML(`<p class="muted nil">` + esc(msg) + `</p>`)
}

// LineChart draws a time series chart as inline SVG. It never panics on empty
// or all-NaN data.
func LineChart(o LineOpts) template.HTML {
	const W = 560
	H := o.Height
	if H == 0 {
		H = 250
	}
	if o.Empty == "" {
		o.Empty = "No data yet"
	}
	maxV, have := 0.0, false
	for _, s := range o.Series {
		for _, v := range s.Vals {
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				have = true
				maxV = math.Max(maxV, v)
			}
		}
	}
	if !have || len(o.T) < 1 {
		return emptyChart(W, H, "chart", o.Empty)
	}
	top := maxV
	if o.CapLine > 0 {
		top = math.Max(top, o.CapLine)
	}
	if o.YMax > 0 {
		top = o.YMax
	}
	step := niceStep(top*1.05, 4)
	yMax := math.Ceil(top*1.05/step) * step
	if yMax <= 0 {
		yMax = 1
	}
	const L, R, T, B = 46, 12, 30, 28
	pw, ph := float64(W-L-R), float64(H-T-B)
	n := len(o.T)
	x := func(i int) float64 {
		if n == 1 {
			return L + pw/2
		}
		return L + pw*float64(i)/float64(n-1)
	}
	y := func(v float64) float64 { return T + ph*(1-v/yMax) }
	b := svgOpen(W, H, "time series chart")
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" class="cbg" fill="#ffffff" stroke="%s"/>`, W-1, H-1, colGrid)
	// grid + y labels
	for v := 0.0; v <= yMax+step/2; v += step {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="cg" stroke="%s" stroke-width="1"/>`, L, W-R, y(v), y(v), colGrid)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" text-anchor="end" font-size="11.5" class="cm" fill="%s">%s</text>`, L-6, y(v)+4, colMuted, fmtTick(v))
	}
	fmt.Fprintf(b, `<text x="%d" y="14" font-size="11.5" class="cm" fill="%s">%s</text>`, 8, colMuted, esc(o.Unit))
	// x labels
	layout := o.XLayout
	if layout == "" {
		layout = "15:04"
	}
	ticks := 5
	if n < ticks {
		ticks = n
	}
	for k := 0; k < ticks; k++ {
		i := 0
		if ticks > 1 {
			i = int(math.Round(float64(k) * float64(n-1) / float64(ticks-1)))
		}
		anchor := "middle"
		if k == 0 {
			anchor = "start"
		} else if k == ticks-1 && ticks > 1 {
			anchor = "end"
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="%s" font-size="11.5" class="cm" fill="%s">%s</text>`, x(i), H-9, anchor, colMuted, esc(o.T[i].In(EAT).Format(layout)))
	}
	if o.CapLine > 0 {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f"%s stroke="%s" stroke-width="1.5" stroke-dasharray="3 3"/>`, L, W-R, y(o.CapLine), y(o.CapLine), tokClass("ss", ColCap), ColCap)
	}
	// series
	for si, s := range o.Series {
		var d strings.Builder
		pen := false
		runLen := 0
		lastX, lastY := 0.0, 0.0
		flush := func() {
			if runLen == 1 {
				fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="2.5"%s fill="%s"/>`, lastX, lastY, tokClass("fs", s.Color), s.Color)
			}
		}
		for i, v := range s.Vals {
			if i >= n {
				break
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				flush()
				pen, runLen = false, 0
				continue
			}
			cmd := "L"
			if !pen {
				cmd, pen, runLen = "M", true, 0
			}
			runLen++
			lastX, lastY = x(i), y(v)
			fmt.Fprintf(&d, "%s%.1f %.1f", cmd, lastX, lastY)
		}
		flush()
		if d.Len() > 0 {
			fmt.Fprintf(b, `<path d="%s" fill="none"%s stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"%s/>`, d.String(), tokClass("ss", s.Color), s.Color, seriesDash(si))
		}
	}
	// legend: HTML under the chart (not inside the scrolling SVG), so series names stay in
	// view on a phone; each swatch repeats the series' colour and dash pattern.
	var lg strings.Builder
	nl := len(o.Series)
	if o.CapLine > 0 {
		nl++
	}
	if nl > 0 {
		lg.WriteString(`<ul class="clegend">`)
		for si, s := range o.Series {
			fmt.Fprintf(&lg, `<li><svg class="sw" width="26" height="8" viewBox="0 0 26 8" aria-hidden="true"><line x1="1" x2="25" y1="4" y2="4"%s stroke="%s" stroke-width="3" stroke-linecap="round"%s/></svg>%s</li>`, tokClass("ss", s.Color), s.Color, seriesDash(si), esc(s.Name))
		}
		if o.CapLine > 0 {
			fmt.Fprintf(&lg, `<li><svg class="sw" width="26" height="8" viewBox="0 0 26 8" aria-hidden="true"><line x1="1" x2="25" y1="4" y2="4"%s stroke="%s" stroke-width="2" stroke-dasharray="3 3"/></svg>%s</li>`, tokClass("ss", ColCap), ColCap, esc(o.CapLabel))
		}
		lg.WriteString(`</ul>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(string(chartBox(b.String())) + lg.String())
}

// BarRow is one bar of BarsH.
type BarRow struct {
	Label string
	Value float64
	Text  string // right-hand value text
	Href  string // optional link on the label
}

// BarsH draws horizontal bars with the label on the left and the value text
// on the right. Rows keep the order given.
func BarsH(rows []BarRow, color, empty string) template.HTML {
	const W, rowH, labW, valW = 560, 28, 150, 92
	if empty == "" {
		empty = "No data yet"
	}
	maxV := 0.0
	for _, r := range rows {
		maxV = math.Max(maxV, r.Value)
	}
	if len(rows) == 0 || maxV <= 0 {
		return emptyChart(W, 90, "bar chart", empty)
	}
	H := len(rows)*rowH + 12
	b := svgOpen(W, H, "bar chart")
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" class="cbg" fill="#ffffff" stroke="%s"/>`, W-1, H-1, colGrid)
	bw := float64(W - labW - valW - 16)
	for i, r := range rows {
		y := 8 + i*rowH
		lab := r.Label
		if rs := []rune(lab); len(rs) > 24 {
			lab = string(rs[:23]) + "…"
		}
		txt := fmt.Sprintf(`<text x="%d" y="%d" text-anchor="end" font-size="12.5" class="ci" fill="%s">%s</text>`, labW, y+15, colInk, esc(lab))
		if r.Href != "" {
			txt = fmt.Sprintf(`<a href="%s">%s</a>`, esc(r.Href), txt)
		}
		b.WriteString(txt)
		w := bw * r.Value / maxV
		if w < 2 && r.Value > 0 {
			w = 2
		}
		fmt.Fprintf(b, `<rect x="%d" y="%d" width="%.1f" height="16" rx="3"%s fill="%s"/>`, labW+8, y+4, w, tokClass("fs", color), color)
		fmt.Fprintf(b, `<text x="%.1f" y="%d" font-size="12" class="cm" fill="%s">%s</text>`, float64(labW+8)+w+6, y+16, colMuted, esc(r.Text))
	}
	b.WriteString(`</svg>`)
	return chartBox(b.String())
}

// barTick formats a y tick; steps under 1 keep two decimals so 0.25 does not read as 0.2.
func barTick(v, step float64) string {
	if step >= 1 {
		return fmtTick(v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

// HourBars draws a 24-bar chart (EAT hour of day on x, average WAN use as % of capacity on y).
// NaN hours have no bar. The y axis runs from 0 to a round value above the observed peak, with
// a floor of 1% so an all-near-zero day still draws a flat, readable chart without dividing by zero.
func HourBars(vals [24]float64) template.HTML {
	const W, H = 560, 200
	const L, R, T, B = 40, 12, 22, 34
	peak, have := 0.0, false
	for _, v := range vals {
		if finite(v) {
			have = true
			peak = math.Max(peak, math.Max(v, 0))
		}
	}
	if !have {
		return emptyChart(W, H, "busy hours", "No hourly data yet")
	}
	step := niceStep(math.Max(peak, 1), 4)
	yMax := math.Ceil(math.Max(peak, 1)/step) * step
	pw, ph := float64(W-L-R), float64(H-T-B)
	y := func(v float64) float64 { return float64(T) + ph*(1-v/yMax) }
	b := svgOpen(W, H, "average WAN utilisation by hour of day, EAT")
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" class="cbg" fill="#ffffff" stroke="%s"/>`, W-1, H-1, colGrid)
	for v := 0.0; v <= yMax+step/2; v += step {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="cg" stroke="%s" stroke-width="1"/>`, L, W-R, y(v), y(v), colGrid)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" text-anchor="end" font-size="11.5" class="cm" fill="%s">%s</text>`, L-6, y(v)+4, colMuted, barTick(v, step))
	}
	fmt.Fprintf(b, `<text x="8" y="14" font-size="11.5" class="cm" fill="%s">%% of capacity</text>`, colMuted)
	slot := pw / 24
	for h, v := range vals {
		x := float64(L) + float64(h)*slot
		if finite(v) {
			v = math.Max(v, 0)
			bh := ph * v / yMax
			if v > 0 && bh < 1 {
				bh = 1 // a tiny but real value stays visible
			}
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2"%s fill="%s"><title>%s</title></rect>`,
				x+slot*0.15, float64(T)+ph-bh, slot*0.7, bh, tokClass("fs", ColDown), ColDown, esc(fmt.Sprintf("%02d:00 EAT: %.1f%% of capacity", h, v)))
		}
		if h%6 == 0 {
			fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="middle" font-size="11.5" class="cm" fill="%s">%02d</text>`, x+slot/2, H-16, colMuted, h)
		}
	}
	fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="11" class="cm" fill="%s">Hour of day (EAT)</text>`, L+int(pw)/2, H-3, colMuted)
	b.WriteString(`</svg>`)
	return chartBox(b.String())
}

// XLayout picks the x label layout for a tier and period.
func XLayout(t Tier, p Period) string {
	switch {
	case t == T5:
		return "15:04"
	case t == TH:
		return "Mon 15h"
	}
	return "2 Jan"
}
