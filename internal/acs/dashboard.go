package acs

import (
	"math"
	"time"
)

// The dashboards draw a few hundred points at most. MetricsView already picks the tier for the range (raw samples up
// to 48 hours, 5-minute buckets up to 14 days, hourly beyond, so at most ~4000 points); DashboardView folds that into
// at most ChartPoints equal time buckets (mean of the readings, peak of the peaks) so the page stays small.

// ChartPoints is the most points a chart is given.
const ChartPoints = 200

// DashboardView is what the ONU dashboard page draws: the metrics of MetricsView with the two series reduced to
// chart size.
type DashboardView struct {
	MetricsView
	From    time.Time
	To      time.Time
	BucketS int // seconds per chart point (0 = the series is already small enough and is used as it is)
}

// Dashboard returns the metrics of an ONU for a range, ready to draw. ok is false when the ONU is unknown.
func (s *Service) Dashboard(onuID string, rng time.Duration) (DashboardView, bool) {
	v, ok := s.Metrics(onuID, rng)
	if !ok {
		return DashboardView{}, false
	}
	to := s.now()
	d := DashboardView{MetricsView: v, To: to, From: to.Add(-rng)}
	d.Throughput.Series, d.BucketS = BucketRates(v.Throughput.Series, d.From.Unix(), to.Unix(), v.StepS, ChartPoints)
	var w int
	d.Optical.History, w = BucketOptical(v.Optical.History, d.From.Unix(), to.Unix(), v.StepS, ChartPoints)
	if w > d.BucketS {
		d.BucketS = w
	}
	return d, true
}

// bucketWidth is the seconds per bucket that fits the window into at most n buckets, never finer than the source step.
func bucketWidth(from, to int64, n int, step int) int64 {
	w := (to - from + int64(n) - 1) / int64(n)
	if w < int64(step) {
		w = int64(step)
	}
	if w < 1 {
		w = 1
	}
	return w
}

// mean accumulates a weighted mean that ignores missing readings.
type mean struct{ sum, w float64 }

func (m *mean) add(v *float64, w float64) {
	if v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
		m.sum += *v * w
		m.w += w
	}
}
func (m mean) ptr() *float64 {
	if m.w == 0 {
		return nil
	}
	v := m.sum / m.w
	return &v
}

type peak struct {
	v  float64
	ok bool
}

func (p *peak) add(v *float64) {
	if v != nil && !math.IsNaN(*v) && (!p.ok || *v > p.v) {
		p.v, p.ok = *v, true
	}
}
func (p peak) ptr() *float64 {
	if !p.ok {
		return nil
	}
	v := p.v
	return &v
}

func weight(n int) float64 {
	if n < 1 {
		return 1
	}
	return float64(n)
}

// BucketRates reduces a throughput series to at most n points over [from, to] (unix seconds). Points older than
// from by more than step (a coarse bucket that starts before the window but runs into it) or later than to are
// dropped. A series that is already at most n points long is returned as it is (width 0). Each output point is at the
// middle of its bucket and carries the sample-weighted mean and the highest peak.
func BucketRates(in []RatePoint, from, to int64, step, n int) ([]RatePoint, int) {
	var pts []RatePoint
	for _, p := range in {
		if p.T >= from-int64(step) && p.T <= to {
			pts = append(pts, p)
		}
	}
	if len(pts) <= n || n < 1 || to <= from {
		return append([]RatePoint{}, pts...), 0
	}
	w := bucketWidth(from, to, n, step)
	type acc struct {
		dn, up   mean
		dnp, upp peak
		samples  int
		used     bool
	}
	cnt := int((to-from)/w) + 1
	accs := make([]acc, cnt)
	for _, p := range pts {
		i := int((p.T - from) / w)
		if i < 0 {
			i = 0
		}
		if i >= cnt {
			i = cnt - 1
		}
		a, k := &accs[i], weight(p.Samples)
		a.dn.add(p.DownBps, k)
		a.up.add(p.UpBps, k)
		if p.DownPk != nil {
			a.dnp.add(p.DownPk)
		} else {
			a.dnp.add(p.DownBps)
		}
		if p.UpPk != nil {
			a.upp.add(p.UpPk)
		} else {
			a.upp.add(p.UpBps)
		}
		a.samples += int(k)
		a.used = true
	}
	out := make([]RatePoint, 0, cnt)
	for i, a := range accs {
		if !a.used {
			continue
		}
		out = append(out, RatePoint{T: from + int64(i)*w + w/2, DownBps: a.dn.ptr(), UpBps: a.up.ptr(), DownPk: a.dnp.ptr(), UpPk: a.upp.ptr(), Samples: a.samples})
	}
	return out, int(w)
}

// BucketOptical is BucketRates for the optical readings.
func BucketOptical(in []OpticalPoint, from, to int64, step, n int) ([]OpticalPoint, int) {
	var pts []OpticalPoint
	for _, p := range in {
		if p.T >= from-int64(step) && p.T <= to {
			pts = append(pts, p)
		}
	}
	if len(pts) <= n || n < 1 || to <= from {
		return append([]OpticalPoint{}, pts...), 0
	}
	w := bucketWidth(from, to, n, step)
	type acc struct {
		rx, tx, tp, v, b mean
		samples          int
		used             bool
	}
	cnt := int((to-from)/w) + 1
	accs := make([]acc, cnt)
	for _, p := range pts {
		i := int((p.T - from) / w)
		if i < 0 {
			i = 0
		}
		if i >= cnt {
			i = cnt - 1
		}
		a, k := &accs[i], weight(p.Samples)
		a.rx.add(p.RxDBm, k)
		a.tx.add(p.TxDBm, k)
		a.tp.add(p.TempC, k)
		a.v.add(p.VoltV, k)
		a.b.add(p.BiasMA, k)
		a.samples += int(k)
		a.used = true
	}
	out := make([]OpticalPoint, 0, cnt)
	for i, a := range accs {
		if !a.used {
			continue
		}
		out = append(out, OpticalPoint{T: from + int64(i)*w + w/2, RxDBm: a.rx.ptr(), TxDBm: a.tx.ptr(), TempC: a.tp.ptr(), VoltV: a.v.ptr(), BiasMA: a.b.ptr(), Samples: a.samples})
	}
	return out, int(w)
}
