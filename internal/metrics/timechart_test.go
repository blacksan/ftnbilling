package metrics

import (
	"encoding/xml"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

// wellFormed parses the SVG/HTML fragment and fails on any XML error (unclosed tags, bad attribute quoting).
func wellFormed(t *testing.T, frag string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader("<root>" + frag + "</root>"))
	dec.Strict = true
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("not well-formed: %v\n%s", err, frag)
		}
	}
}

func TestTimeChartDrawsAxesBandPathAndLegend(t *testing.T) {
	from := time.Unix(1_700_000_000, 0)
	to := from.Add(24 * time.Hour)
	var ts []time.Time
	var rx []float64
	for i := 0; i < 10; i++ {
		ts = append(ts, from.Add(time.Duration(i)*2*time.Hour))
		rx = append(rx, -20-float64(i)*0.5)
	}
	out := string(TimeChart(TimeChartOpts{From: from, To: to, T: ts, Unit: "dBm", Band: &Band{Lo: -25, Hi: -8, Label: "healthy <b>"},
		Series: []TimeLine{{Name: "Rx <i>power</i>", Color: ColDown, Vals: rx}}, Label: "Rx over time"}))
	wellFormed(t, out)
	for _, want := range []string{"<svg", `class="chart"`, "<path ", "dBm", `class="cband"`, "-20", `aria-label="Rx over time"`, "clegend"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "<b>") || strings.Contains(out, "<i>") || strings.Contains(out, "<script") {
		t.Fatal("a label was not escaped")
	}
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Fatal("NaN in the drawing")
	}
}

func TestTimeChartEmptyAndAllMissingSayNoData(t *testing.T) {
	from := time.Unix(1_700_000_000, 0)
	for name, o := range map[string]TimeChartOpts{
		"no rows": {From: from, To: from.Add(time.Hour), Empty: "No data here"},
		"all NaN": {From: from, To: from.Add(time.Hour), T: []time.Time{from}, Series: []TimeLine{{Vals: []float64{math.NaN()}}}, Empty: "No data here"},
		"no span": {From: from, To: from, T: []time.Time{from}, Series: []TimeLine{{Vals: []float64{1}}}, Empty: "No data here"},
	} {
		out := string(TimeChart(o))
		if !strings.Contains(out, "No data here") || strings.Contains(out, "<svg") {
			t.Fatalf("%s: %s", name, out)
		}
		wellFormed(t, out)
	}
}

func TestTimeChartBreaksTheLineAtAGapAndKeepsOneReadingAsADot(t *testing.T) {
	from := time.Unix(1_700_000_000, 0)
	ts := []time.Time{from.Add(1 * time.Minute), from.Add(2 * time.Minute), from.Add(50 * time.Minute), from.Add(51 * time.Minute), from.Add(59 * time.Minute)}
	out := string(TimeChart(TimeChartOpts{From: from, To: from.Add(time.Hour), T: ts, GapSecs: 600, ZeroBase: true,
		Series: []TimeLine{{Name: "down", Color: ColDown, Vals: []float64{1, 2, 3, 4, 5}}}}))
	wellFormed(t, out)
	// three runs: [1,2], [3,4,5]: two path commands "M"; with a gap of 600s between 2min and 50min
	if got := strings.Count(out, `d="M`); got != 2 {
		t.Fatalf("want 2 runs, got %d: %s", got, out)
	}
	lone := string(TimeChart(TimeChartOpts{From: from, To: from.Add(time.Hour), T: ts[:1], Series: []TimeLine{{Name: "x", Color: ColDown, Vals: []float64{3}}}}))
	if !strings.Contains(lone, "<circle") {
		t.Fatal("a single reading should be a dot")
	}
}

func TestTimeChartNegativeAxisAndAreaStayInsideTheFrame(t *testing.T) {
	from := time.Unix(1_700_000_000, 0)
	ts := []time.Time{from.Add(time.Minute), from.Add(2 * time.Minute)}
	out := string(TimeChart(TimeChartOpts{From: from, To: from.Add(time.Hour), T: ts, ZeroBase: true, CapLine: 100, CapLabel: "line rate",
		Series: []TimeLine{{Name: "down", Color: ColDown, Vals: []float64{10, 20}, Area: true}}}))
	wellFormed(t, out)
	if !strings.Contains(out, "fill-opacity=\"0.15\"") || !strings.Contains(out, "line rate") {
		t.Fatal("area or cap line missing")
	}
}
