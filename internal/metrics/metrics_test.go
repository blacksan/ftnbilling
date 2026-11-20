package metrics

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ispbill/internal/routeros"
)

type fakeDir []AcctInfo

func (f fakeDir) Accounts() []AcctInfo { return f }

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, EAT)

func testCfg() Config {
	return Config{WAN: "ether1", OLT: "sfp-sfpplus1", WANCapMbps: 500, OLTCapMbps: 10000, FUPTB: 15,
		PingTargets: []string{"8.8.8.8", "1.1.1.1"}, Subnet: netip.MustParsePrefix("10.5.50.0/24"),
		FlowEvery: 60 * time.Second, NetflowAddr: "off"}
}

func newCol(t *testing.T) *Collector {
	t.Helper()
	d := fakeDir{{Code: "K7Q2MX", Flat: "Flat 3B", Package: "Monthly 20 Mbps", SpeedMbps: 20, State: "active"},
		{Code: "P4R8TN", Flat: "Flat 10A", Package: "1 Day", SpeedMbps: 10, State: "active"}}
	return New(filepath.Join(t.TempDir(), "metrics.json"), testCfg(), routeros.NewMock(), d, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fq is one flat's simple queue "ftn-<code>" with its byte counters (up is from the flat,
// down is to the flat) and download max-limit maxB (0 = not set).
func fq(code string, up, down uint64, maxB int64) []routeros.SimpleQueue {
	return []routeros.SimpleQueue{{Name: "ftn-" + code, Target: "10.5.50.20/32", MaxA: maxB, MaxB: maxB, BytesUp: up, BytesDown: down}}
}

// ---- NetFlow v5 --------------------------------------------------------------------

func v5Packet(sampling uint16, recs ...[]byte) []byte {
	h := make([]byte, 24)
	binary.BigEndian.PutUint16(h[0:], 5)
	binary.BigEndian.PutUint16(h[2:], uint16(len(recs)))
	binary.BigEndian.PutUint32(h[8:], uint32(t0.Unix()))
	binary.BigEndian.PutUint32(h[16:], 77)
	binary.BigEndian.PutUint16(h[22:], sampling)
	for _, r := range recs {
		h = append(h, r...)
	}
	return h
}

func v5Rec(src, dst string, sport, dport uint16, proto uint8, octets uint32) []byte {
	r := make([]byte, 48)
	a, b := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(r[0:], a[:])
	copy(r[4:], b[:])
	binary.BigEndian.PutUint32(r[16:], octets/100+1)
	binary.BigEndian.PutUint32(r[20:], octets)
	binary.BigEndian.PutUint16(r[32:], sport)
	binary.BigEndian.PutUint16(r[34:], dport)
	r[37], r[38] = 0x18, proto
	return r
}

func TestParseV5(t *testing.T) {
	pkt := v5Packet(0x4000|10, v5Rec("10.5.50.20", "142.250.0.10", 51000, 443, 6, 1500), v5Rec("142.250.0.10", "10.5.50.20", 443, 51000, 6, 90000))
	h, recs, err := ParseV5(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if h.Count != 2 || h.Sampling != 10 || h.SamplingMode != 1 || h.Sequence != 77 || h.UnixSecs != uint32(t0.Unix()) {
		t.Fatalf("%+v", h)
	}
	r := recs[1]
	if r.Src.String() != "142.250.0.10" || r.Dst.String() != "10.5.50.20" || r.SrcPort != 443 || r.DstPort != 51000 || r.Proto != 6 || r.Octets != 90000 || r.TCPFlags != 0x18 {
		t.Fatalf("%+v", r)
	}
	if _, h2, _ := ParseV5(v5Packet(0, v5Rec("10.5.50.20", "1.1.1.1", 1, 53, 17, 70))); h2[0].Octets != 70 {
		t.Fatal("record")
	}
	// Malformed input is rejected, never panics.
	bad := [][]byte{nil, {1, 2, 3}, pkt[:30], append(append([]byte{}, pkt...), 0), v5Packet(0), make([]byte, 24)}
	v9 := append([]byte{}, pkt...)
	v9[1] = 9
	bad = append(bad, v9)
	for i, b := range bad {
		if _, _, err := ParseV5(b); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	over := v5Packet(0)
	binary.BigEndian.PutUint16(over[2:], 31)
	if _, _, err := ParseV5(over); err == nil {
		t.Error("count 31 accepted")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		hosts []string
		proto uint8
		port  uint16
		want  string
	}{
		{[]string{"rr1---sn-abc.googlevideo.com"}, 6, 443, "YouTube"},
		{[]string{"www.youtube.com"}, 6, 443, "YouTube"},
		{[]string{"ipv4-c001.nflxvideo.net"}, 6, 443, "Netflix"},
		{[]string{"g.whatsapp.net"}, 6, 443, "WhatsApp"},
		{[]string{"scontent.xx.fbcdn.net"}, 6, 443, "Meta"},
		{[]string{"v16.tiktokcdn.com"}, 6, 443, "TikTok"},
		{[]string{"audio-ak.spotify.com"}, 6, 443, "Spotify"},
		{[]string{"gsp.apple.com"}, 6, 443, "Apple"},
		{[]string{"settings-win.data.microsoft.com"}, 6, 443, "Microsoft"},
		{[]string{"us04web.zoom.us"}, 17, 8801, "Zoom"},
		{[]string{"fonts.gstatic.com"}, 6, 443, "Google"},
		{[]string{"vod.showmax.com"}, 6, 443, "Showmax"},
		{[]string{"live.dstv.com"}, 6, 443, "DStv"},
		{[]string{"e1234.a.akamaiedge.net"}, 6, 443, "CDN (other)"},
		{nil, 6, 443, "HTTPS (other)"},
		{nil, 17, 443, "HTTPS (other)"},
		{nil, 6, 80, "HTTP"},
		{nil, 17, 53, "DNS"},
		{nil, 6, 53, "DNS"},
		{nil, 17, 123, "NTP"},
		{nil, 17, 3479, "Calls / STUN"},
		{nil, 17, 19302, "Calls / STUN"},
		{nil, 6, 5223, "Messaging / push"},
		{nil, 6, 6881, "Other"},
		{[]string{"unknown.example"}, 6, 22, "Other"},
		// A hostname wins over the port; first matching rule wins (YouTube before Google).
		{[]string{"youtube-ui.l.google.com"}, 6, 443, "YouTube"},
	}
	for _, c := range cases {
		if got := Classify(c.hosts, c.proto, c.port); got != c.want {
			t.Errorf("%v %d/%d = %q, want %q", c.hosts, c.proto, c.port, got, c.want)
		}
	}
}

func TestFlowAttributionDNSAndSampling(t *testing.T) {
	c := newCol(t)
	// A queue target maps 10.5.50.20 -> K7Q2MX.
	c.SampleQueues(t0, fq("K7Q2MX", 0, 0, 0))
	c.UpdateDNS(t0, []routeros.DNSEntry{
		{Name: "rr1.googlevideo.com", Data: "142.250.0.10", Type: "A"},
		{Name: "www.youtube.com", Data: "youtube-ui.l.google.com", Type: "CNAME"},
		{Name: "youtube-ui.l.google.com", Data: "142.250.9.9", Type: "A"},
	})
	pkt := v5Packet(0x4000|10,
		v5Rec("142.250.0.10", "10.5.50.20", 443, 40000, 6, 1000), // download from googlevideo, 1 in 10 sampled
		v5Rec("10.5.50.20", "151.101.1.1", 40001, 80, 6, 200),    // upload, port-classified HTTP
		v5Rec("10.5.50.50", "142.250.9.9", 40002, 443, 6, 500),   // unattributed client, CNAME -> YouTube
		v5Rec("10.5.50.20", "10.5.50.99", 1, 2, 6, 99999),        // inside the subnet: ignored
		v5Rec("8.8.8.8", "9.9.9.9", 1, 2, 6, 99999))              // not customer traffic: ignored
	c.HandleNetflow(t0, pkt, "10.5.50.1:1234")
	c.HandleNetflow(t0, []byte("garbage"), "x")
	g := c.D.Svc.sum(T5, t0.Add(-time.Hour), t0.Add(time.Hour))
	if g["YouTube"] != 1000*10+500*10 || g["HTTP"] != 200*10 || len(g) != 2 {
		t.Fatalf("global %v", g)
	}
	day := c.D.SvcAcct["K7Q2MX"][BucketStart(TD, t0)]
	if day["YouTube"] != 10000 || day["HTTP"] != 2000 {
		t.Fatalf("per flat %v", day)
	}
	if c.D.NF.Flows != 3 || c.D.NF.Bad != 1 || c.D.NF.Packets != 1 {
		t.Fatalf("%+v", c.D.NF)
	}
}

// ---- counters, deltas ---------------------------------------------------------------

func total(c *Collector, code string) (down, up uint64) {
	s := c.D.Accts[code].Sum(TD, t0.Add(-48*time.Hour), t0.Add(48*time.Hour))
	return s.Down, s.Up
}

func TestQueueDeltasAndCounterReset(t *testing.T) {
	c := newCol(t)
	// A queue seen for the first time is only baselined: its counters hold old traffic.
	c.SampleQueues(t0, fq("K7Q2MX", 100, 1000, 0))
	if d, u := total(c, "K7Q2MX"); d != 0 || u != 0 {
		t.Fatalf("first sight counted old traffic: %d %d", d, u)
	}
	// Normal growth: delta.
	c.SampleQueues(t0.Add(time.Minute), fq("K7Q2MX", 300, 5000, 0))
	if d, u := total(c, "K7Q2MX"); d != 4000 || u != 200 {
		t.Fatalf("delta: %d %d", d, u)
	}
	// Counter goes down (queue recreated or counters reset): delta = new value.
	c.SampleQueues(t0.Add(2*time.Minute), fq("K7Q2MX", 50, 700, 0))
	if d, u := total(c, "K7Q2MX"); d != 4700 || u != 250 {
		t.Fatalf("reset: %d %d", d, u)
	}
	// A queue that goes away is forgotten: when it comes back it is baselined again.
	c.SampleQueues(t0.Add(3*time.Minute), nil)
	c.SampleQueues(t0.Add(4*time.Minute), fq("K7Q2MX", 1<<30, 1<<33, 0))
	if d, u := total(c, "K7Q2MX"); d != 4700 || u != 250 {
		t.Fatalf("returning queue counted old traffic: %d %d", d, u)
	}
	// Queues that are not a flat's (no ftn- prefix, or not an account) are ignored.
	c.SampleQueues(t0.Add(5*time.Minute), []routeros.SimpleQueue{{Name: "guests", BytesDown: 5}, {Name: "ftn-admin", BytesDown: 5}})
	c.SampleQueues(t0.Add(6*time.Minute), []routeros.SimpleQueue{{Name: "guests", BytesDown: 9}, {Name: "ftn-admin", BytesDown: 9}})
	if _, ok := c.D.Accts["admin"]; ok {
		t.Fatal("non-account queue recorded")
	}
}

func TestPeakAndTimeAtCap(t *testing.T) {
	c := newCol(t) // K7Q2MX cap = 20 Mbps (package speed)
	c.SampleQueues(t0, fq("K7Q2MX", 0, 0, 0))
	// 60 s at 19 Mbps average (>= 90% of 20 Mbps).
	bytes := uint64(19e6 / 8 * 60)
	c.SampleQueues(t0.Add(time.Minute), fq("K7Q2MX", 0, bytes, 0))
	// 60 s at 2 Mbps.
	c.SampleQueues(t0.Add(2*time.Minute), fq("K7Q2MX", 0, bytes+uint64(2e6/8*60), 0))
	s := c.D.Accts["K7Q2MX"].Sum(TD, t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
	if math.Abs(s.CapMin-1) > 0.01 || math.Abs(s.OnMin-2) > 0.01 {
		t.Fatalf("cap minutes %.2f online %.2f", s.CapMin, s.OnMin)
	}
	if mb := float64(s.PkDown) / 1e6; math.Abs(mb-19) > 0.2 {
		t.Fatalf("peak %.2f Mbps", mb)
	}
	// The queue's max-limit is the cap (a 10 Mbps cap makes 9.5 Mbps at cap).
	q := fq("K7Q2MX", 0, bytes+uint64(2e6/8*60)+uint64(9.5e6/8*60), 10e6)
	q[0].RateA, q[0].RateB = 1e5, 8e6
	c.SampleQueues(t0.Add(3*time.Minute), q)
	if live := c.L.Flats["K7Q2MX"]; live.CapBps != 10e6 || live.DownBps != 8e6 || live.UpBps != 1e5 {
		t.Fatalf("live %+v", live)
	}
	s = c.D.Accts["K7Q2MX"].Sum(TD, t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
	if math.Abs(s.CapMin-2) > 0.05 {
		t.Fatalf("cap minutes with queue %.2f", s.CapMin)
	}
}

func TestPortsMonthAndResets(t *testing.T) {
	c := newCol(t)
	ifs := func(rx, tx uint64) []routeros.Iface {
		return []routeros.Iface{{Name: "ether1", RxBytes: rx, TxBytes: tx, Running: true}, {Name: "veth-x", RxBytes: 5}, {Name: "sfp-sfpplus1", RxBytes: tx, TxBytes: rx, Running: true}}
	}
	c.SamplePorts(t0, ifs(1000, 100))
	c.SamplePorts(t0.Add(30*time.Second), ifs(1000+37_500_000, 100+3_750_000)) // 10 Mbps down, 1 Mbps up
	if lp := c.L.Ports["ether1"]; math.Abs(lp.RxBps-10e6) > 1 || math.Abs(lp.TxBps-1e6) > 1 {
		t.Fatalf("%+v", lp)
	}
	if _, ok := c.L.Ports["veth-x"]; ok {
		t.Fatal("veth must be ignored")
	}
	// Router reboot: counters restart from zero.
	c.SamplePorts(t0.Add(time.Minute), ifs(500, 50))
	m := c.D.Months[MonthKey(t0)]
	if m.Rx != 37_500_000+500 || m.Tx != 3_750_000+50 {
		t.Fatalf("month %+v", m)
	}
}

// ---- buckets: rollup and pruning -------------------------------------------------------

func TestBucketsRollupAndPrune(t *testing.T) {
	var s Series
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, EAT)
	for i := 0; i < 20*24*12; i++ { // 20 days of 5-minute samples
		at := base.Add(time.Duration(i) * 5 * time.Minute)
		s.Add(at, func(c *Cell) { c.Down += 100; c.N++; c.PkDown = maxU(c.PkDown, uint64(i%50)) })
	}
	// Rollup is consistent between tiers.
	day := s.Sum(TD, base, base.Add(24*time.Hour))
	hour := s.Sum(TH, base, base.Add(24*time.Hour))
	m5 := s.Sum(T5, base, base.Add(24*time.Hour))
	if day.Down != 288*100 || hour.Down != day.Down || m5.Down != day.Down || day.N != 288 {
		t.Fatalf("rollup %d %d %d", day.Down, hour.Down, m5.Down)
	}
	if day.PkDown != 49 {
		t.Fatalf("peak %d", day.PkDown)
	}
	if len(s.D1) != 20 || len(s.H1) != 480 || len(s.M5) != 5760 {
		t.Fatalf("before prune: %d %d %d", len(s.D1), len(s.H1), len(s.M5))
	}
	now := base.Add(20 * 24 * time.Hour)
	s.prune(now)
	if len(s.M5) != 48*12 { // 48 h of 5-minute buckets
		t.Fatalf("5-min tier %d buckets after prune", len(s.M5))
	}
	if len(s.H1) != 14*24 {
		t.Fatalf("hourly tier %d buckets after prune", len(s.H1))
	}
	if len(s.D1) != 20 {
		t.Fatalf("daily tier %d", len(s.D1))
	}
	// Daily buckets are cut at EAT midnight.
	for k := range s.D1 {
		if (k+3*3600)%86400 != 0 {
			t.Fatalf("daily key %d not EAT midnight", k)
		}
	}
	// 400-day retention.
	var old Series
	old.Add(now.Add(-401*24*time.Hour), func(c *Cell) { c.N++ })
	old.Add(now.Add(-399*24*time.Hour), func(c *Cell) { c.N++ })
	if empty := old.prune(now); empty || len(old.D1) != 1 {
		t.Fatalf("daily retention: %d empty=%v", len(old.D1), empty)
	}
}

func TestSaveLoadBoundedAndAtomic(t *testing.T) {
	c := newCol(t)
	c.SetClock(func() time.Time { return t0 })
	c.SampleQueues(t0.Add(-10*24*time.Hour), fq("K7Q2MX", 0, 1000, 0))
	c.SampleQueues(t0.Add(-10*24*time.Hour+time.Minute), fq("K7Q2MX", 0, 2000, 0))
	c.SampleQueues(t0, fq("K7Q2MX", 0, 4000, 0))
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(filepath.Dir(c.path)); len(es) != 1 {
		t.Fatalf("dir has %d files (temp left over?)", len(es))
	}
	d, err := load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	s := d.Accts["K7Q2MX"]
	if s == nil || len(s.M5) != 1 || len(s.D1) != 2 { // the 10-day-old 5-min bucket was pruned on write
		t.Fatalf("loaded %+v", s)
	}
	if err := os.WriteFile(c.path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := New(c.path, testCfg(), routeros.NewMock(), fakeDir{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(c2.D.Accts) != 0 {
		t.Fatal("corrupt file must start empty, not crash")
	}
}

// ---- report and charts ---------------------------------------------------------------------

func TestReportEmptyAndRouterUnreachable(t *testing.T) {
	c := newCol(t)
	for _, p := range Periods {
		r := c.BuildReport(p, t0)
		if r.HasTraffic || r.MonthGB != 0 || len(r.Flags) != 4 || len(r.Flats) != 2 {
			t.Fatalf("%s: %+v", p, r)
		}
		for _, f := range r.Flags {
			if f.Level != "nodata" || f.Rule == "" {
				t.Errorf("%s flag %+v", p, f)
			}
		}
	}
	// Charts must not panic on empty or all-NaN data.
	LineChart(LineOpts{})
	LineChart(LineOpts{T: []time.Time{t0}, Series: []LineSeries{{Name: "x", Vals: []float64{math.NaN()}}}})
	LineChart(LineOpts{T: []time.Time{t0}, Series: []LineSeries{{Name: "x", Vals: []float64{0}}}, CapLine: 500})
	BarsH(nil, ColDown, "")
	BarsH([]BarRow{{Label: "a", Value: 0}}, ColDown, "")
	var nanHours [24]float64
	for i := range nanHours {
		nanHours[i] = math.NaN()
	}
	if !strings.Contains(string(HourStrip(nanHours)), "No hourly data yet") {
		t.Fatal("empty strip")
	}
}

func TestDemoReportAndFlags(t *testing.T) {
	c := newCol(t)
	c.cfg.Synthetic = true
	c.SeedDemo(t0, 35)
	now := t0
	r := c.BuildReport(Period7d, now)
	if !r.HasTraffic || r.MonthGB <= 0 || r.ProjectedGB <= 0 || len(r.Services) == 0 {
		t.Fatalf("%+v", r.Flags)
	}
	var down float64
	for _, f := range r.Flats {
		down += f.DownGB
		if f.SharePct < 0 || f.SharePct > 100 {
			t.Fatalf("%+v", f)
		}
	}
	if down <= 0 {
		t.Fatal("no flat usage")
	}
	if r.WAN.Vals["down"] == nil || len(r.WAN.T) != 7*24+1 && len(r.WAN.T) != 7*24 {
		t.Fatalf("wan buckets %d", len(r.WAN.T))
	}
	// 5-minute tier covers today, daily tier the month.
	if tr := c.BuildReport(PeriodToday, now); tr.Tier != T5 || len(tr.WAN.T) != 12*12+1 && len(tr.WAN.T) != 12*12 {
		t.Fatalf("today buckets %d tier %v", len(tr.WAN.T), tr.Tier)
	}
	if mr := c.BuildReport(PeriodMonth, now); mr.Tier != TD || len(mr.WAN.T) != 17 {
		t.Fatalf("month buckets %d", len(mr.WAN.T))
	}
	fr := c.BuildFlat("K7Q2MX", Period7d, now)
	if !fr.Found || len(fr.Series.T) == 0 || len(fr.Services) == 0 {
		t.Fatalf("%+v", fr.Row)
	}
	if c.BuildFlat("NOSUCH", Period7d, now).Found {
		t.Fatal("unknown flat found")
	}
	if math.IsNaN(r.Busy[20]) || r.Busy[20] <= r.Busy[4] {
		t.Fatalf("evening should be busier than 04:00: %v %v", r.Busy[20], r.Busy[4])
	}
	// Sorting.
	rows := append([]FlatRow(nil), r.Flats...)
	SortFlats(rows, "flat")
	if rows[0].Flat != "Flat 3B" {
		t.Fatal(rows[0].Flat)
	}
	SortFlats(rows, "peak")
	if rows[0].PeakMbps < rows[1].PeakMbps {
		t.Fatal("peak sort")
	}
	SortFlats(rows, "bogus-key") // falls back, no panic
}

func TestFlagsFire(t *testing.T) {
	c := newCol(t)
	// A flat spending most of its time at the cap.
	for i := 0; i < 24*12; i++ {
		at := t0.Add(-time.Duration(i) * 5 * time.Minute)
		c.getAcct("K7Q2MX").Add(at, func(cell *Cell) { cell.Down += 1000; cell.OnMin += 5; cell.CapMin += 4; cell.N++ })
	}
	// WAN above 85% for several hours this week.
	for h := 0; h < 5; h++ {
		at := t0.Add(-time.Duration(h) * time.Hour)
		c.getPort("ether1").Add(at, func(cell *Cell) { cell.Down += uint64(450e6 / 8 * 3600); cell.N++ })
	}
	// Loss.
	for i := 0; i < 10; i++ {
		c.SamplePing(t0.Add(-time.Duration(i)*time.Minute), routeros.PingResult{Target: "8.8.8.8", Sent: 5, Received: 4, AvgRTT: 20 * time.Millisecond, LossPct: 20})
	}
	// FUP: 6 TB in the first 3 days of the month projects far above 15 TB.
	c.D.Months[MonthKey(t0)] = &MonthUse{Rx: 6e12}
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, EAT)
	r := c.BuildReport(Period7d, now)
	lv := map[string]Flag{}
	for _, f := range r.Flags {
		lv[f.Title] = f
	}
	if lv["WAN busy hours"].Level != "ok" { // data is at t0 (Nov 17), outside the 7 days before Nov 4
		t.Fatalf("%+v", lv["WAN busy hours"])
	}
	r = c.BuildReport(Period7d, t0)
	lv = map[string]Flag{}
	for _, f := range r.Flags {
		lv[f.Title] = f
	}
	if lv["WAN busy hours"].Level != "warn" || !strings.Contains(lv["WAN busy hours"].Text, "may feel slow") {
		t.Errorf("%+v", lv["WAN busy hours"])
	}
	if lv["Houses at their speed cap"].Level != "warn" || !strings.Contains(lv["Houses at their speed cap"].Text, "Flat 3B") {
		t.Errorf("%+v", lv["Houses at their speed cap"])
	}
	if lv["Packet loss"].Level != "warn" {
		t.Errorf("%+v", lv["Packet loss"])
	}
	// FUP projection on Nov 4 12:00 EAT (3.5 days in): 6 TB so far.
	c.D.Months[MonthKey(t0)] = &MonthUse{Rx: 6e12}
	r = c.BuildReport(Period7d, time.Date(2026, 11, 4, 12, 0, 0, 0, EAT))
	if r.ProjectedGB < 15000 || !strings.Contains(r.FUPNote, "reached on") || r.FUPWarn {
		t.Errorf("fup: %v %q warn=%v", r.ProjectedGB, r.FUPNote, r.FUPWarn)
	}
	c.D.Months[MonthKey(t0)] = &MonthUse{Rx: 12.5e12, Tx: 0}
	if r2 := c.BuildReport(Period7d, time.Date(2026, 11, 20, 0, 0, 0, 0, EAT)); !r2.FUPWarn {
		t.Error("tile must warn above 80% of the FUP")
	}
	if f := r.Flags[3]; f.Level != "warn" || !strings.Contains(f.Text, "exceeded") {
		t.Errorf("%+v", f)
	}
}

func TestChartsProduceSVGWithPalette(t *testing.T) {
	ts := []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour), t0.Add(3 * time.Hour)}
	svg := string(LineChart(LineOpts{T: ts, Unit: "Mbps", CapLine: 500, CapLabel: "capacity 500", Series: []LineSeries{
		{Name: "download", Color: ColDown, Vals: []float64{10, math.NaN(), 30, 40}}, {Name: "upload", Color: ColUp, Vals: []float64{1, 2, 3, 4}}}}))
	for _, want := range []string{"<svg", "</svg>", "Mbps", "capacity 500", ColDown, ColUp, ColCap, "download", "upload", "viewBox"} {
		if !strings.Contains(svg, want) {
			t.Errorf("line chart lacks %q", want)
		}
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Error("NaN leaked into the SVG")
	}
	bars := string(BarsH([]BarRow{{Label: "Flat 3B <x>", Value: 5, Text: "5 GB"}}, ColDown, ""))
	if !strings.Contains(bars, "&lt;x&gt;") || strings.Contains(bars, "<x>") {
		t.Error("labels must be escaped")
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	c := newCol(t)
	c.SampleQueues(t0, fq("K7Q2MX", 100, 1000, 0))
	c.SamplePing(t0, routeros.PingResult{Target: "8.8.8.8", Received: 5, AvgRTT: 12 * time.Millisecond})
	c.note("ping", errors.New("boom"))
	snap := c.Snapshot()
	if len(snap.Flats) != 1 || snap.Ping["8.8.8.8"].RTTms != 12 || snap.Err["ping"] != "boom" {
		t.Fatalf("%+v", snap)
	}
	delete(snap.Flats, "K7Q2MX")
	snap.Ping["x"] = LivePing{}
	snap.Err["ping"] = "changed"
	if len(c.L.Flats) != 1 || len(c.L.Ping) != 1 || c.L.Err["ping"] != "boom" {
		t.Fatal("Snapshot shares maps with the collector")
	}
	// Safe against concurrent sampling (run with -race).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			c.SampleQueues(t0.Add(time.Duration(i)*time.Second), fq("K7Q2MX", uint64(i), uint64(i), 0))
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = c.Snapshot()
	}
	<-done
}

func TestPortAllowFilter(t *testing.T) {
	ifs := []routeros.Iface{{Name: "ether1", RxBytes: 1, TxBytes: 1, Running: true}, {Name: "ether2"}, {Name: "sfp-sfpplus1"},
		{Name: "br-customers"}, {Name: "vlan30-customers"}, {Name: "bridge"}, {Name: "wg-ftn"}, {Name: "lo"}}
	legacy := newCol(t)
	legacy.SamplePorts(t0, ifs)
	if _, ok := legacy.L.Ports["ether2"]; !ok || len(legacy.L.Ports) != 5 {
		t.Fatalf("legacy prefix filter: %v", legacy.L.Ports)
	}
	cfg := testCfg()
	cfg.PortAllow = []string{"ether1", "sfp-sfpplus1", "br-customers", "vlan30-customers"}
	c := New(filepath.Join(t.TempDir(), "m.json"), cfg, routeros.NewMock(), fakeDir{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SamplePorts(t0, ifs)
	if len(c.L.Ports) != 4 {
		t.Fatalf("%v", c.L.Ports)
	}
	for _, n := range []string{"ether2", "bridge", "wg-ftn"} {
		if _, ok := c.L.Ports[n]; ok {
			t.Fatalf("%s must not be recorded", n)
		}
	}
}

func TestJitterSpreadsSites(t *testing.T) {
	every := 60 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		cfg := testCfg()
		cfg.SiteID = fmt.Sprintf("s%06x", i*7919)
		c := New(filepath.Join(t.TempDir(), "m.json"), cfg, routeros.NewMock(), fakeDir{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		j := c.jitter(every)
		if j < 0 || j >= every || j != c.jitter(every) {
			t.Fatalf("jitter %v", j)
		}
		seen[j] = true
	}
	if len(seen) < 15 {
		t.Fatalf("only %d distinct offsets for 20 sites", len(seen))
	}
	if newCol(t).jitter(every) != 0 {
		t.Fatal("no site id, no jitter")
	}
}

func TestServiceDaysKept62DaysAndSaveCreatesDir(t *testing.T) {
	c := newCol(t)
	c.path = filepath.Join(t.TempDir(), "metrics", "s123456.json") // directory does not exist yet
	c.SetClock(func() time.Time { return t0 })
	k := func(daysAgo int) int64 { return BucketStart(TD, t0.Add(-time.Duration(daysAgo)*24*time.Hour)) }
	c.D.SvcAcct["K7Q2MX"] = map[int64]map[string]uint64{k(70): {"YouTube": 1}, k(61): {"YouTube": 2}, k(1): {"YouTube": 3}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	d, err := load(c.path)
	if err != nil {
		t.Fatal(err)
	}
	days := d.SvcAcct["K7Q2MX"]
	if len(days) != 2 || days[k(70)] != nil || days[k(61)]["YouTube"] != 2 {
		t.Fatalf("service days after save: %v", days)
	}
}

func queue(code string, up, down uint64) routeros.SimpleQueue {
	return routeros.SimpleQueue{Name: "ftn-" + code, Target: "10.5.50.60/32", Comment: "ftn:acct:" + code + " Flat 3B",
		MaxA: 20e6, MaxB: 20e6, RateA: 1e6, RateB: 9e6, BytesUp: up, BytesDown: down}
}

func TestIPoEUsageFromQueueCounters(t *testing.T) {
	c := newCol(t)
	// First sight is only a baseline: the counters hold history.
	c.SampleQueues(t0, []routeros.SimpleQueue{queue("K7Q2MX", 5000, 90000), {Name: "guests", BytesDown: 1 << 40}, queue("NOPE99", 1, 1)})
	if d, u := total(c, "K7Q2MX"); d != 0 || u != 0 {
		t.Fatalf("baseline must not count history: %d %d", d, u)
	}
	c.SampleQueues(t0.Add(time.Minute), []routeros.SimpleQueue{queue("K7Q2MX", 5300, 95000)})
	if d, u := total(c, "K7Q2MX"); d != 5000 || u != 300 {
		t.Fatalf("delta: %d %d", d, u)
	}
	// Counters reset (queue recreated): the new value is the delta.
	c.SampleQueues(t0.Add(2*time.Minute), []routeros.SimpleQueue{queue("K7Q2MX", 20, 700)})
	if d, u := total(c, "K7Q2MX"); d != 5700 || u != 320 {
		t.Fatalf("reset: %d %d", d, u)
	}
	// Queues that are not an account's (or whose flat is unknown) are ignored.
	if d, _ := total(c, "NOPE99"); d != 0 {
		t.Fatal("unknown flat counted")
	}
	live := c.Snapshot().Flats["K7Q2MX"]
	if live.Address != "10.5.50.60" || live.DownBps != 9e6 || live.UpBps != 1e6 || live.CapBps != 20e6 {
		t.Fatalf("%+v", live)
	}
	// The queue's address is how NetFlow finds the flat.
	if got := c.codeForIP("10.5.50.60", t0.Add(2*time.Minute)); got != "K7Q2MX" {
		t.Fatalf("ip->code %q", got)
	}
}
