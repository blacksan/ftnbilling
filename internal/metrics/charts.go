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
	ColDown  = "#2a78d6" // download / primary
	ColUp    = "#eb6834" // upload
	ColAqua  = "#1baf7a"
	ColViol  = "#6250d6"
	ColMag   = "#e87ba4"
	ColCap   = "#e34948" // capacity / limit line
	colInk   = "#1c2630"
	colMuted = "#5b6670"
	colGrid  = "#e3e7ec"
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

func svgOpen(w, h int, label string) *strings.Builder {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" role="img" aria-label="%s" style="width:100%%;max-width:%dpx;height:auto;display:block;font-family:system-ui,sans-serif">`, w, h, esc(label), w*5/4)
	return &b
}

func emptyChart(w, h int, label, msg string) template.HTML {
	b := svgOpen(w, h, label)
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" fill="#fcfcfb" stroke="%s"/>`, w-1, h-1, colGrid)
	fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" font-size="14" fill="%s">%s</text></svg>`, w/2, h/2+5, colMuted, esc(msg))
	return template.HTML(b.String())
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
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" fill="#fcfcfb" stroke="%s"/>`, W-1, H-1, colGrid)
	// grid + y labels
	for v := 0.0; v <= yMax+step/2; v += step {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`, L, W-R, y(v), y(v), colGrid)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" text-anchor="end" font-size="11.5" fill="%s">%s</text>`, L-6, y(v)+4, colMuted, fmtTick(v))
	}
	fmt.Fprintf(b, `<text x="%d" y="14" font-size="11.5" fill="%s">%s</text>`, 8, colMuted, esc(o.Unit))
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
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="%s" font-size="11.5" fill="%s">%s</text>`, x(i), H-9, anchor, colMuted, esc(o.T[i].In(EAT).Format(layout)))
	}
	if o.CapLine > 0 {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="1.5" stroke-dasharray="6 4"/>`, L, W-R, y(o.CapLine), y(o.CapLine), ColCap)
	}
	// series
	for _, s := range o.Series {
		var d strings.Builder
		pen := false
		runLen := 0
		lastX, lastY := 0.0, 0.0
		flush := func() {
			if runLen == 1 {
				fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="2.5" fill="%s"/>`, lastX, lastY, s.Color)
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
			fmt.Fprintf(b, `<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`, d.String(), s.Color)
		}
	}
	// legend (top right)
	lx := float64(W - R)
	items := len(o.Series)
	if o.CapLine > 0 {
		items++
	}
	_ = items
	type leg struct {
		name, col string
		dash      bool
	}
	var legs []leg
	for _, s := range o.Series {
		legs = append(legs, leg{s.Name, s.Color, false})
	}
	if o.CapLine > 0 {
		legs = append(legs, leg{o.CapLabel, ColCap, true})
	}
	for i := len(legs) - 1; i >= 0; i-- {
		l := legs[i]
		w := float64(len(l.name))*6.4 + 26
		lx -= w
		if l.dash {
			fmt.Fprintf(b, `<line x1="%.1f" x2="%.1f" y1="11" y2="11" stroke="%s" stroke-width="2" stroke-dasharray="4 3"/>`, lx, lx+16, l.col)
		} else {
			fmt.Fprintf(b, `<line x1="%.1f" x2="%.1f" y1="11" y2="11" stroke="%s" stroke-width="3" stroke-linecap="round"/>`, lx, lx+16, l.col)
		}
		fmt.Fprintf(b, `<text x="%.1f" y="15" font-size="12" fill="%s">%s</text>`, lx+20, colInk, esc(l.name))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
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
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" fill="#fcfcfb" stroke="%s"/>`, W-1, H-1, colGrid)
	bw := float64(W - labW - valW - 16)
	for i, r := range rows {
		y := 8 + i*rowH
		lab := r.Label
		if rs := []rune(lab); len(rs) > 24 {
			lab = string(rs[:23]) + "…"
		}
		txt := fmt.Sprintf(`<text x="%d" y="%d" text-anchor="end" font-size="12.5" fill="%s">%s</text>`, labW, y+15, colInk, esc(lab))
		if r.Href != "" {
			txt = fmt.Sprintf(`<a href="%s">%s</a>`, esc(r.Href), txt)
		}
		b.WriteString(txt)
		w := bw * r.Value / maxV
		if w < 2 && r.Value > 0 {
			w = 2
		}
		fmt.Fprintf(b, `<rect x="%d" y="%d" width="%.1f" height="16" rx="3" fill="%s"/>`, labW+8, y+4, w, color)
		fmt.Fprintf(b, `<text x="%.1f" y="%d" font-size="12" fill="%s">%s</text>`, float64(labW+8)+w+6, y+16, colMuted, esc(r.Text))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// HourStrip draws 24 cells (EAT hour of day) shaded by value (percent).
// NaN cells are blank. The scale runs from 0 to the largest value (at least 10).
func HourStrip(vals [24]float64) template.HTML {
	const W, H = 560, 84
	maxV, have := 10.0, false
	for _, v := range vals {
		if !math.IsNaN(v) {
			have = true
			maxV = math.Max(maxV, v)
		}
	}
	if !have {
		return emptyChart(W, H, "busy hours", "No hourly data yet")
	}
	b := svgOpen(W, H, "average WAN utilisation by hour of day")
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" fill="#fcfcfb" stroke="%s"/>`, W-1, H-1, colGrid)
	cw := float64(W-24) / 24
	for h, v := range vals {
		x := 12 + float64(h)*cw
		if math.IsNaN(v) {
			fmt.Fprintf(b, `<rect x="%.1f" y="12" width="%.1f" height="36" rx="3" fill="#f1f3f5"/>`, x+1, cw-2)
		} else {
			t := v / maxV
			// light blue to dark blue
			r := 232 + (31-232)*t
			g := 240 + (95-240)*t
			bl := 251 + (181-251)*t
			txt := "#1c2631" // cell text: kept dark in both themes
			if t > 0.5 {
				txt = "#ffffff"
			}
			fmt.Fprintf(b, `<rect x="%.1f" y="12" width="%.1f" height="36" rx="3" fill="rgb(%.0f,%.0f,%.0f)"><title>%02d:00 EAT: %.1f%%</title></rect>`, x+1, cw-2, r, g, bl, h, v)
			lab := fmt.Sprintf("%.0f", v)
			if v < 10 {
				lab = fmt.Sprintf("%.1f", v)
			}
			fmt.Fprintf(b, `<text x="%.1f" y="35" text-anchor="middle" font-size="10" fill="%s">%s</text>`, x+cw/2, txt, lab)
		}
		if h%3 == 0 {
			fmt.Fprintf(b, `<text x="%.1f" y="64" text-anchor="middle" font-size="11.5" fill="%s">%02d</text>`, x+cw/2, colMuted, h)
		}
	}
	fmt.Fprintf(b, `<text x="%d" y="79" font-size="11" fill="%s">Hour of day (EAT). Number = average WAN use, %% of capacity. Darker = busier.</text>`, 12, colMuted)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
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
