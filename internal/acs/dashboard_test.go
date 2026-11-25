package acs

import "testing"

func f(v float64) *float64 { return &v }

func TestBucketRatesFoldsToTheWindowAndKeepsPeaks(t *testing.T) {
	from, to := int64(1_000_000), int64(1_000_000+24*3600)
	var in []RatePoint
	for i := int64(0); i < 1440; i++ { // one per minute for a day
		in = append(in, RatePoint{T: from + i*60, DownBps: f(float64(i % 10)), UpBps: f(1), DownPk: f(float64(i%10) + 100*boolf(i == 700))})
	}
	// outside the window: before (beyond the step) and after
	in = append([]RatePoint{{T: from - 7200, DownBps: f(9e9)}}, in...)
	in = append(in, RatePoint{T: to + 600, DownBps: f(9e9)})
	out, w := BucketRates(in, from, to, 0, 200)
	if w == 0 || len(out) > 200 || len(out) < 100 {
		t.Fatalf("width %d, %d points", w, len(out))
	}
	var peak float64
	for _, p := range out {
		if p.T < from || p.T > to {
			t.Fatalf("point outside the window: %d", p.T)
		}
		if *p.DownBps > 9 {
			t.Fatalf("an out-of-window sample leaked in: %v", *p.DownBps)
		}
		if p.DownPk != nil && *p.DownPk > peak {
			peak = *p.DownPk
		}
	}
	if peak < 100 {
		t.Fatalf("the peak was averaged away: %v", peak)
	}
	for i := 1; i < len(out); i++ {
		if out[i].T <= out[i-1].T {
			t.Fatal("not increasing")
		}
	}
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func TestBucketRatesSmallAndEmptyAreUntouched(t *testing.T) {
	from, to := int64(1000), int64(5000)
	if out, w := BucketRates(nil, from, to, 0, 200); len(out) != 0 || w != 0 || out == nil {
		t.Fatalf("empty: %v %d", out, w)
	}
	in := []RatePoint{{T: 2000, DownBps: f(5)}, {T: 3000, DownBps: f(7)}}
	if out, w := BucketRates(in, from, to, 0, 200); len(out) != 2 || w != 0 || *out[1].DownBps != 7 {
		t.Fatalf("small: %v %d", out, w)
	}
}

func TestBucketRatesMeanIsWeightedAndMissingStaysMissing(t *testing.T) {
	from, to := int64(0), int64(1000)
	in := []RatePoint{{T: 10, DownBps: f(10), Samples: 1}, {T: 20, DownBps: f(40), Samples: 3}, {T: 30, UpBps: f(8)}}
	out, w := BucketRates(in, from, to, 0, 1) // one bucket
	if w == 0 || len(out) != 1 {
		t.Fatalf("%d %v", w, out)
	}
	if got := *out[0].DownBps; got != 32.5 {
		t.Fatalf("weighted mean: %v", got)
	}
	if out[0].UpBps == nil || *out[0].UpBps != 8 {
		t.Fatal("up lost")
	}
	out2, _ := BucketRates([]RatePoint{{T: 10, DownBps: f(1)}, {T: 20, DownBps: f(2)}}, from, to, 0, 1)
	if out2[0].UpBps != nil {
		t.Fatal("a reading was invented for a series that has none")
	}
}

func TestBucketOpticalAveragesAndRespectsStep(t *testing.T) {
	from, to := int64(0), int64(90*86400)
	var in []OpticalPoint
	for i := int64(0); i < 2160; i++ {
		in = append(in, OpticalPoint{T: i * 3600, RxDBm: f(-20), Samples: 12})
	}
	out, w := BucketOptical(in, from, to, 3600, 200)
	if len(out) > 200 || w < 3600 {
		t.Fatalf("%d points, width %d", len(out), w)
	}
	for _, p := range out {
		if p.RxDBm == nil || *p.RxDBm != -20 || p.TxDBm != nil {
			t.Fatalf("%+v", p)
		}
	}
	if o, _ := BucketOptical(nil, from, to, 0, 200); o == nil || len(o) != 0 {
		t.Fatal("empty should be an empty list")
	}
}
