package metrics

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// TimeChart is a line chart whose x axis is real time (so a gap in the data is a gap on the chart) and whose y axis
// can sit anywhere (dBm values are negative). It is drawn as inline SVG: no script, and every text in it is escaped.
// It is used for the ONU dashboards (internal/web/onu_metrics.go).

// TimeLine is one line of a TimeChart. Vals is parallel to TimeChartOpts.T; NaN is "no reading" and breaks the line.
type TimeLine struct {
	Name  string
	Color string
	Vals  []float64
	Area  bool // a soft fill down to the base line (throughput)
}

// Band is a shaded range of the y axis (the normal operating range of a reading).
type Band struct {
	Lo, Hi float64
	Label  string
}

// TimeChartOpts configures TimeChart.
type TimeChartOpts struct {
	From, To time.Time
	T        []time.Time // x of each value
	Series   []TimeLine
	Unit     string  // shown above the y axis, e.g. "dBm"
	ZeroBase bool    // y starts at 0 (rates); otherwise it fits the data and the band
	Band     *Band   // shaded healthy range
	CapLine  float64 // dashed line (the line rate), 0 = none
	CapLabel string
	GapSecs  int64 // a gap longer than this between two readings breaks the line (0 = never)
	XLayout  string
	Height   int
	Label    string // what the chart shows (aria-label)
	Empty    string
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// TimeChart draws the chart, or a plain "no data" line when no series has a reading. It never panics.
func TimeChart(o TimeChartOpts) template.HTML {
	const W = 560
	H := o.Height
	if H == 0 {
		H = 220
	}
	if o.Empty == "" {
		o.Empty = "No data yet"
	}
	if o.Label == "" {
		o.Label = "time series chart"
	}
	lo, hi, have := math.Inf(1), math.Inf(-1), false
	for _, s := range o.Series {
		for i, v := range s.Vals {
			if i < len(o.T) && finite(v) {
				have = true
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	span := o.To.Sub(o.From).Seconds()
	if !have || span <= 0 {
		return emptyChart(W, H, o.Label, o.Empty)
	}
	if o.Band != nil {
		lo, hi = math.Min(lo, o.Band.Lo), math.Max(hi, o.Band.Hi)
	}
	if o.CapLine > 0 {
		hi = math.Max(hi, o.CapLine)
	}
	if o.ZeroBase {
		lo = 0
	}
	if hi-lo < 1e-9 {
		hi, lo = hi+1, lo-1
	}
	pad := (hi - lo) * 0.08
	if !o.ZeroBase {
		lo -= pad
	}
	hi += pad
	step := niceStep(hi-lo, 4)
	yLo, yHi := math.Floor(lo/step)*step, math.Ceil(hi/step)*step
	if o.ZeroBase {
		yLo = 0
	}
	const L, R, T, B = 50, 12, 26, 28
	pw, ph := float64(W-L-R), float64(H-T-B)
	x := func(t time.Time) float64 {
		f := t.Sub(o.From).Seconds() / span
		return L + pw*math.Max(0, math.Min(1, f))
	}
	y := func(v float64) float64 { return T + ph*(1-(v-yLo)/(yHi-yLo)) }

	b := svgOpen(W, H, o.Label)
	// the svgOpen text is `...style="--w:560px;..."`; the chart keeps its drawn width on a phone (app.css), like the others
	fmt.Fprintf(b, `<title>%s</title>`, esc(o.Label))
	fmt.Fprintf(b, `<rect x="0.5" y="0.5" width="%d" height="%d" rx="6" class="cbg" fill="#ffffff" stroke="%s"/>`, W-1, H-1, colGrid)
	if o.Band != nil {
		b0, b1 := y(math.Min(yHi, o.Band.Hi)), y(math.Max(yLo, o.Band.Lo))
		if b1 > b0 {
			fmt.Fprintf(b, `<rect x="%d" y="%.1f" width="%.1f" height="%.1f" class="cband" fill="#1E7A4C" fill-opacity="0.13"><title>%s</title></rect>`, L, b0, pw, b1-b0, esc(o.Band.Label))
		}
	}
	for v := yLo; v <= yHi+step/2; v += step {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="cg" stroke="%s" stroke-width="1"/>`, L, W-R, y(v), y(v), colGrid)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" text-anchor="end" font-size="11.5" class="cm" fill="%s">%s</text>`, L-6, y(v)+4, colMuted, esc(tickText(v, step)))
	}
	fmt.Fprintf(b, `<text x="8" y="14" font-size="11.5" class="cm" fill="%s">%s</text>`, colMuted, esc(o.Unit))
	layout := o.XLayout
	if layout == "" {
		layout = "15:04"
	}
	const ticks = 5
	for k := 0; k < ticks; k++ {
		t := o.From.Add(time.Duration(float64(k) / (ticks - 1) * span * float64(time.Second)))
		anchor := "middle"
		if k == 0 {
			anchor = "start"
		} else if k == ticks-1 {
			anchor = "end"
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="%s" font-size="11.5" class="cm" fill="%s">%s</text>`, x(t), H-9, anchor, colMuted, esc(t.In(EAT).Format(layout)))
	}
	if o.CapLine > 0 {
		fmt.Fprintf(b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f"%s stroke="%s" stroke-width="1.5" stroke-dasharray="3 3"/>`, L, W-R, y(o.CapLine), y(o.CapLine), tokClass("ss", ColCap), ColCap)
	}
	for si, s := range o.Series {
		type pt struct{ x, y float64 }
		var runs [][]pt
		var cur []pt
		var lastT time.Time
		for i, v := range s.Vals {
			if i >= len(o.T) {
				break
			}
			if !finite(v) || (len(cur) > 0 && o.GapSecs > 0 && o.T[i].Sub(lastT).Seconds() > float64(o.GapSecs)) {
				if len(cur) > 0 {
					runs = append(runs, cur)
					cur = nil
				}
				if !finite(v) {
					continue
				}
			}
			cur = append(cur, pt{x(o.T[i]), y(v)})
			lastT = o.T[i]
		}
		if len(cur) > 0 {
			runs = append(runs, cur)
		}
		base := y(math.Max(yLo, math.Min(0, yHi)))
		if !o.ZeroBase {
			base = float64(T) + ph
		}
		for _, r := range runs {
			if len(r) == 1 {
				fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="2.5"%s fill="%s"/>`, r[0].x, r[0].y, tokClass("fs", s.Color), s.Color)
				continue
			}
			var d strings.Builder
			for i, p := range r {
				c := "L"
				if i == 0 {
					c = "M"
				}
				fmt.Fprintf(&d, "%s%.1f %.1f", c, p.x, p.y)
			}
			if s.Area {
				fmt.Fprintf(b, `<path d="%sL%.1f %.1f L%.1f %.1f Z"%s fill="%s" fill-opacity="0.15" stroke="none"/>`, d.String(), r[len(r)-1].x, base, r[0].x, base, tokClass("fs", s.Color), s.Color)
			}
			fmt.Fprintf(b, `<path d="%s" fill="none"%s stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"%s/>`, d.String(), tokClass("ss", s.Color), s.Color, seriesDash(si))
		}
	}
	b.WriteString(`</svg>`)

	var lg strings.Builder
	lg.WriteString(`<ul class="clegend">`)
	for si, s := range o.Series {
		fmt.Fprintf(&lg, `<li><svg class="sw" width="26" height="8" viewBox="0 0 26 8" aria-hidden="true"><line x1="1" x2="25" y1="4" y2="4"%s stroke="%s" stroke-width="3" stroke-linecap="round"%s/></svg>%s</li>`, tokClass("ss", s.Color), s.Color, seriesDash(si), esc(s.Name))
	}
	if o.Band != nil {
		fmt.Fprintf(&lg, `<li><svg class="sw" width="26" height="10" viewBox="0 0 26 10" aria-hidden="true"><rect x="1" y="1" width="24" height="8" fill="#1E7A4C" fill-opacity="0.25"/></svg>%s</li>`, esc(o.Band.Label))
	}
	if o.CapLine > 0 {
		fmt.Fprintf(&lg, `<li><svg class="sw" width="26" height="8" viewBox="0 0 26 8" aria-hidden="true"><line x1="1" x2="25" y1="4" y2="4"%s stroke="%s" stroke-width="2" stroke-dasharray="3 3"/></svg>%s</li>`, tokClass("ss", ColCap), ColCap, esc(o.CapLabel))
	}
	lg.WriteString(`</ul>`)
	return template.HTML(string(chartBox(b.String())) + lg.String())
}

// tickText writes a tick with as many decimals as the step needs ("-21", "-21.5", "0.25").
func tickText(v, step float64) string {
	if math.Abs(v) < step/1000 {
		v = 0
	}
	switch {
	case step >= 1:
		return fmt.Sprintf("%.0f", v)
	case step >= 0.1:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}
