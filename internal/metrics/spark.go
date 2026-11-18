package metrics

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

// Small server-side charts for the admin pages. They carry no colour of their
// own: the stylesheet colours them (currentColor for the sparkline, the
// --chart-* tokens for the bars), so one drawing works in light and dark.

// Spark is a trend line with a soft area and a dot on the latest value.
// NaN values are skipped. With fewer than two values it draws a flat baseline,
// so a card never looks broken.
func Spark(vals []float64, w, h int, label string) template.HTML {
	if w <= 0 {
		w = 120
	}
	if h <= 0 {
		h = 32
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label="%s" preserveAspectRatio="none"><title>%s</title>`, w, h, w, h, esc(label), esc(label))
	var pts []float64
	for _, v := range vals {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			pts = append(pts, v)
		}
	}
	if len(pts) < 2 {
		fmt.Fprintf(&b, `<path class="l" d="M2 %d H%d" opacity=".45"/></svg>`, h-3, w-2)
		return template.HTML(b.String())
	}
	lo, hi := pts[0], pts[0]
	for _, v := range pts {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi == lo {
		hi = lo + 1
	}
	const pad = 3.0
	x := func(i int) float64 { return pad + (float64(w)-2*pad)*float64(i)/float64(len(pts)-1) }
	y := func(v float64) float64 { return float64(h) - pad - (float64(h)-2*pad)*(v-lo)/(hi-lo) }
	var line strings.Builder
	for i, v := range pts {
		c := "L"
		if i == 0 {
			c = "M"
		}
		fmt.Fprintf(&line, "%s%.1f %.1f", c, x(i), y(v))
	}
	fmt.Fprintf(&b, `<path class="a" d="%sL%.1f %d L%.1f %d Z"/><path class="l" d="%s"/>`, line.String(), x(len(pts)-1), h, x(0), h, line.String())
	fmt.Fprintf(&b, `<circle class="d" cx="%.1f" cy="%.1f" r="3"/></svg>`, x(len(pts)-1), y(pts[len(pts)-1]))
	return template.HTML(b.String())
}

// VBar is one column of BarsV. Soon marks a column drawn in the second chart
// colour (for example today).
type VBar struct {
	Label string // under the bar
	Hint  string // tooltip, "Mon 20 Nov: 4 renewals"
	Value float64
	Soon  bool
}

// BarsV draws vertical bars on a zero baseline: rounded tops, a faint grid at
// the top and middle, the maximum written once, and a wide invisible hit
// target per column that carries the tooltip.
func BarsV(bars []VBar, w, h int, label string) template.HTML {
	if w <= 0 {
		w = 560
	}
	if h <= 0 {
		h = 170
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="bars" viewBox="0 0 %d %d" role="img" aria-label="%s"><title>%s</title>`, w, h, esc(label), esc(label))
	if len(bars) == 0 {
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle">Nothing to show yet</text></svg>`, w/2, h/2)
		return template.HTML(b.String())
	}
	max := 0.0
	for _, v := range bars {
		max = math.Max(max, v.Value)
	}
	top := niceMax(max)
	const left, bottom, topPad = 26.0, 22.0, 14.0
	plotH := float64(h) - bottom - topPad
	plotW := float64(w) - left - 4
	for _, f := range []float64{1, .5} {
		y := topPad + plotH*(1-f)
		fmt.Fprintf(&b, `<line class="gl" x1="%.0f" x2="%d" y1="%.1f" y2="%.1f"/><text x="%.0f" y="%.1f" text-anchor="end">%s</text>`, left, w-2, y, y, left-5, y+4, fmtTick(top*f))
	}
	fmt.Fprintf(&b, `<line x1="%.0f" x2="%d" y1="%.1f" y2="%.1f" stroke="var(--chart-grid)" stroke-width="1.5"/>`, left, w-2, topPad+plotH, topPad+plotH)
	slot := plotW / float64(len(bars))
	bw := math.Min(slot*0.62, 30)
	every := 1
	if len(bars) > 8 {
		every = 2
	}
	for i, v := range bars {
		x := left + slot*float64(i) + (slot-bw)/2
		bh := plotH * v.Value / top
		cls := "col"
		if v.Soon {
			cls += " soon"
		}
		if v.Value > 0 {
			if bh < 4 {
				bh = 4
			}
			fmt.Fprintf(&b, `<path class="%s" d="M%.1f %.1f v-%.1f a4 4 0 0 1 4 -4 h%.1f a4 4 0 0 1 4 4 v%.1f z"/>`, cls, x, topPad+plotH, math.Max(bh-4, 0), bw-8, math.Max(bh-4, 0))
		}
		hit := v.Hint
		if hit == "" {
			hit = fmt.Sprintf("%s: %s", v.Label, fmtTick(v.Value))
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="transparent"><title>%s</title></rect>`, left+slot*float64(i), topPad, slot, plotH, esc(hit))
		if i%every == 0 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%d" text-anchor="middle">%s</text>`, x+bw/2, h-6, esc(v.Label))
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceMax rounds a maximum up to 1, 2, 5 or 10 times a power of ten (at least 4,
// so a bar of one never fills the plot).
func niceMax(v float64) float64 {
	if v < 4 {
		return 4
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 4, 5, 10} {
		if v <= m*p {
			return m * p
		}
	}
	return 10 * p
}
