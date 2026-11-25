package acs

import (
	"math"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestCounterDeltaWrapAndReset(t *testing.T) {
	dt := 300 * time.Second
	const max = maxPlausibleBps / 8
	if d, ok := counterDelta(1000, 4000, dt, max); !ok || d != 3000 {
		t.Fatalf("plain growth: %d %v", d, ok)
	}
	// a 32-bit counter that wrapped: 4294967000 -> 1000 is 1296 bytes, not a reset
	if d, ok := counterDelta(4294967000, 1000, dt, max); !ok || d != 1296 {
		t.Fatalf("32-bit wrap: %d %v", d, ok)
	}
	// a counter that restarted (the box rebooted): the previous value was far from the top of 32 bits... but it is
	// still inside 32 bits, so the growth that a wrap would mean is checked against the line rate
	if _, ok := counterDelta(3000000000, 5, 1*time.Second, max); ok {
		t.Fatal("a drop to 5 from 3 GB in a second is not a plausible wrap (it is 1.29 GB/s)")
	}
	// a 64-bit counter that went down: a reset
	if _, ok := counterDelta(1<<40, 7, dt, max); ok {
		t.Fatal("a 64-bit counter that went down is a reset")
	}
	// a jump above the line rate is a glitch
	if _, ok := counterDelta(0, 1<<40, dt, max); ok {
		t.Fatal("an impossible jump was accepted")
	}
	if _, ok := counterDelta(1, 2, 0, max); ok {
		t.Fatal("no time between samples")
	}
	if got := bitrate(37500000, 300*time.Second); math.Abs(got-1e6) > 1 {
		t.Fatalf("bitrate %v", got)
	}
}

func TestOpticalParsing(t *testing.T) {
	p := "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig."
	o := parseOptical(p, []ParamValue{
		{Name: p + "RXPower", Value: "-21.53"}, {Name: p + "TXPower", Value: "2.4"}, {Name: p + "TransceiverTemperature", Value: "41"},
		{Name: p + "SupplyVottage", Value: "3300"}, {Name: p + "BiasCurrent", Value: "12345"}, {Name: p + "RXPowerHighAlarm", Value: "-5"},
		{Name: p + "Password", Value: "secret"},
	})
	if o.Rx == nil || *o.Rx != -21.53 || o.Tx == nil || *o.Tx != 2.4 || o.Temp == nil || *o.Temp != 41 {
		t.Fatalf("%+v", o)
	}
	if o.Volt == nil || *o.Volt != 3.3 {
		t.Fatalf("voltage in mV: %+v", o.Volt)
	}
	if o.Bias == nil || *o.Bias != 12.345 {
		t.Fatalf("bias in uA: %+v", o.Bias)
	}
	if _, has := o.Raw["Password"]; has || o.Raw["RXPower"] != "-21.53" || o.Raw["RXPowerHighAlarm"] != "-5" {
		t.Fatalf("raw: %v", o.Raw)
	}
	// 0.01 dBm units
	o = parseOptical(p, []ParamValue{{Name: p + "RxPower", Value: "-2150"}})
	if o.Rx == nil || *o.Rx != -21.5 {
		t.Fatalf("0.01 dBm: %v", o.Rx)
	}
	// TR-181 reports in 0.001 dBm
	q := store.RootTR181 + "Optical.Interface.1."
	o = parseOptical(q, []ParamValue{{Name: q + "OpticalSignalLevel", Value: "-21500"}, {Name: q + "TransmitOpticalLevel", Value: "2500"}})
	if o.Rx == nil || *o.Rx != -21.5 || o.Tx == nil || *o.Tx != 2.5 {
		t.Fatalf("TR-181: %+v %+v", o.Rx, o.Tx)
	}
}

func rd(wanRx, wanTx uint64, uptime int64) reading {
	return reading{uptime: uptime, hasUptime: true, wan: &wanReading{path: "w.", rx: u64(wanRx), tx: u64(wanTx), counters: map[string]uint64{"ErrorsSent": 3}}}
}

func TestApplyReadingComputesBitratesFromCounterDeltas(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	m := applyReading(store.ONUMetrics{ID: "o1"}, rd(1000, 1000, 100), t0)
	if m.WAN.Dn != nil || len(m.Fine) != 0 {
		t.Fatalf("the first sample has no rate: %+v", m.WAN)
	}
	// 300 s later: 37.5 MB down (1 Mbit/s), 3.75 MB up (100 kbit/s)
	m = applyReading(m, rd(1000+37500000, 1000+3750000, 400), t0.Add(300*time.Second))
	if m.WAN.Dn == nil || math.Abs(*m.WAN.Dn-1e6) > 1 || math.Abs(*m.WAN.Up-1e5) > 1 {
		t.Fatalf("rates: %+v", m.WAN)
	}
	if len(m.Fine) != 1 || m.Fine[0].Dn == nil {
		t.Fatalf("the point: %+v", m.Fine)
	}
	// the counter wrapped (32 bit): 4294967000 -> 100000000 +... still a rate
	a := applyReading(store.ONUMetrics{ID: "o1"}, rd(4294967000, 0, 100), t0)
	b := applyReading(a, rd(37500000-296, 0, 400), t0.Add(300*time.Second))
	if b.WAN.Dn == nil || math.Abs(*b.WAN.Dn-1e6) > 1 {
		t.Fatalf("wrapped counter: %v", b.WAN.Dn)
	}
	// error counters grow
	c := applyReading(m, func() reading { r := rd(1000+37500000, 1000+3750000, 700); r.wan.counters["ErrorsSent"] = 8; return r }(), t0.Add(600*time.Second))
	if c.WAN.Deltas["ErrorsSent"] != 5 {
		t.Fatalf("error delta %v", c.WAN.Deltas)
	}
	// a reboot (uptime went down): counted, and no rate for that interval
	d := applyReading(c, rd(10, 10, 20), t0.Add(900*time.Second))
	if d.Reboots != 1 || d.WAN.Dn != nil || d.LastBoot.IsZero() {
		t.Fatalf("reboot: %d %v %v", d.Reboots, d.WAN.Dn, d.LastBoot)
	}
	// a BOOT event counts too, once
	r := rd(2000, 2000, 5000)
	r.boot = true
	e := applyReading(c, r, t0.Add(900*time.Second))
	if e.Reboots != 1 {
		t.Fatalf("boot event: %d", e.Reboots)
	}
}

func TestApplyReadingHostRates(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	mk := func(rx uint64) reading {
		return reading{hasHosts: true, hosts: []hostReading{{name: "phone", mac: "AA:BB:CC:00:00:09", ip: "192.168.1.5", iface: "Home", active: true, rx: u64(rx), tx: u64(0)}}}
	}
	m := applyReading(store.ONUMetrics{ID: "o1"}, mk(0), t0)
	m = applyReading(m, mk(3750000), t0.Add(30*time.Second+time.Second*270))
	if len(m.Hosts) != 1 || m.Hosts[0].Dn == nil || math.Abs(*m.Hosts[0].Dn-100000) > 1 {
		t.Fatalf("host rate: %+v", m.Hosts)
	}
}

func TestRetentionTiersAndCaps(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	m := store.ONUMetrics{ID: "o1"}
	// 120 days of one sample every 5 minutes, fed in order, compacted as the ACS does on each write
	start := now.Add(-120 * 24 * time.Hour)
	n := 0
	for ts := start; !ts.After(now); ts = ts.Add(5 * time.Minute) {
		m.Fine = append(m.Fine, store.MetricPoint{T: ts.Unix(), N: 1, Dn: f64(1e6), Rx: f64(-20)})
		m.Compact(ts)
		n++
	}
	if len(m.Fine) > store.MaxFine || len(m.Mid) > store.MaxMid || len(m.Hourly) > store.MaxHourly {
		t.Fatalf("caps: %d %d %d", len(m.Fine), len(m.Mid), len(m.Hourly))
	}
	// raw samples only for 48 h (5-minute spacing here: 576)
	if len(m.Fine) < 570 || len(m.Fine) > 580 {
		t.Fatalf("fine tier %d", len(m.Fine))
	}
	if oldest := time.Unix(m.Fine[0].T, 0); now.Sub(oldest) > store.FineKeep+time.Minute {
		t.Fatalf("fine keeps %v", now.Sub(oldest))
	}
	// 5-minute buckets from 48 h to 14 days: about 12 days of them
	if len(m.Mid) < 12*288-300 || len(m.Mid) > 12*288+300 {
		t.Fatalf("mid tier %d", len(m.Mid))
	}
	if oldest := time.Unix(m.Mid[0].T, 0); now.Sub(oldest) > store.MidKeep+time.Hour {
		t.Fatalf("mid keeps %v", now.Sub(oldest))
	}
	// hourly from 14 days to 90 days: 76 days of them, nothing older than 90 days
	if len(m.Hourly) < 76*24-30 || len(m.Hourly) > 76*24+30 {
		t.Fatalf("hourly tier %d", len(m.Hourly))
	}
	if oldest := time.Unix(m.Hourly[0].T, 0); now.Sub(oldest) > store.HourlyKeep+time.Hour {
		t.Fatalf("hourly keeps %v", now.Sub(oldest))
	}
	// an hourly bucket is the mean of its samples and counts them
	for _, p := range m.Hourly {
		if p.Dn == nil || *p.Dn != 1e6 || p.N < 12 {
			t.Fatalf("hourly bucket: %+v", p)
		}
	}
	// the record stays small
	if total := len(m.Fine) + len(m.Mid) + len(m.Hourly); total > store.MaxFine+store.MaxMid+store.MaxHourly {
		t.Fatalf("total %d", total)
	}
	// every sample is accounted for: nothing was dropped by the tiers before the 90-day limit
	var samples int
	for _, p := range m.Hourly {
		samples += int(p.N)
	}
	for _, p := range m.Mid {
		samples += int(p.N)
	}
	samples += len(m.Fine)
	if want := 90 * 24 * 12; samples < want-400 || samples > want+400 {
		t.Fatalf("samples kept %d, want about %d", samples, want)
	}
	_ = n
}

// A V-SOL-like box with the PON object under WANDevice.1, the WAN counters, a host table and error counters: the
// ACS reads them on the Inform, stores the sample and serves it over the API view.
func vsolMetricsBox(t *testing.T, f *fixture) *cpe {
	c := newVSOL(t, f, "VSOL00127E0F", "10.6.3.20", false)
	r := c.root
	w := r + "WANDevice.1."
	c.set(r+"DeviceInfo.UpTime", "1000")
	c.set(w+"X_GponInterafceConfig.RXPower", "-21.5")
	c.set(w+"X_GponInterafceConfig.TXPower", "2.5")
	c.set(w+"X_GponInterafceConfig.TransceiverTemperature", "41")
	c.set(w+"X_GponInterafceConfig.SupplyVottage", "3300")
	c.set(w+"X_GponInterafceConfig.BiasCurrent", "12")
	c.set(w+"WANCommonInterfaceConfig.TotalBytesReceived", "1000")
	c.set(w+"WANCommonInterfaceConfig.TotalBytesSent", "2000")
	c.set(w+"WANCommonInterfaceConfig.Layer1DownstreamMaxBitRate", "2488000000")
	c.set(w+"WANCommonInterfaceConfig.PhysicalLinkStatus", "Up")
	c.set(w+"WANConnectionDevice.1.WANIPConnection.1.Stats.EthernetErrorsSent", "2")
	c.set(w+"WANConnectionDevice.1.WANIPConnection.1.Stats.EthernetDiscardPacketsReceived", "1")
	h := r + "LANDevice.1.Hosts.Host."
	c.set(h+"1.HostName", "phone")
	c.set(h+"1.PhysAddress", "aa:bb:cc:00:00:09")
	c.set(h+"1.IPAddress", "192.168.1.5")
	c.set(h+"1.InterfaceType", "802.11")
	c.set(h+"1.Layer2Interface", r+"LANDevice.1.WLANConfiguration.1")
	c.set(h+"1.Active", "1")
	c.set(h+"2.HostName", "tv")
	c.set(h+"2.PhysAddress", "aa:bb:cc:00:00:0a")
	c.set(h+"2.IPAddress", "192.168.1.6")
	c.set(h+"2.InterfaceType", "Ethernet")
	c.set(h+"2.Active", "0")
	c.set(r+"IPPingDiagnostics.DiagnosticsState", "None")
	return c
}

func TestMetricsAreCollectedOnTheInformAndServed(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	var m store.ONUMetrics
	get := func() store.ONUMetrics { f.st.View(func(d *store.Data) { m = d.ONUMetricsOf(o.ID).Clone() }); return m }
	eventually(t, "first sample", func() bool { return !get().Updated.IsZero() })
	m = get()
	if m.Optical == nil || m.Optical.Rx == nil || *m.Optical.Rx != -21.5 || *m.Optical.Tx != 2.5 || *m.Optical.Volt != 3.3 || *m.Optical.Bias != 12 {
		t.Fatalf("optical: %+v", m.Optical)
	}
	if !strings.HasSuffix(m.Paths.Optical, "WANDevice.1.X_GponInterafceConfig.") || m.Optical.Raw["RXPower"] != "-21.5" {
		t.Fatalf("path/raw: %q %v", m.Paths.Optical, m.Optical.Raw)
	}
	if m.WAN == nil || *m.WAN.RxBytes != 1000 || m.WAN.Dn != nil || m.WAN.Counters["ErrorsSent"] != 2 {
		t.Fatalf("wan: %+v", m.WAN)
	}
	if len(m.Hosts) != 2 || m.Hosts[0].Iface != "VSOL-2G" || m.Hosts[0].MAC != "AA:BB:CC:00:00:09" || m.Hosts[1].Active {
		t.Fatalf("hosts: %+v", m.Hosts)
	}
	if len(m.Diag) != 1 || m.Diag[0] != "IPPingDiagnostics" {
		t.Fatalf("diagnostics: %v", m.Diag)
	}
	// 5 minutes later the counters grew: 37.5 MB down = 1 Mbit/s
	f.advance(5 * time.Minute)
	c.set(c.root+"DeviceInfo.UpTime", "1300")
	c.set(c.root+"WANDevice.1.WANCommonInterfaceConfig.TotalBytesReceived", "37501000")
	c.set(c.root+"WANDevice.1.WANCommonInterfaceConfig.TotalBytesSent", "3752000")
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second sample", func() bool { return get().WAN != nil && get().WAN.Dn != nil })
	m = get()
	if math.Abs(*m.WAN.Dn-1e6) > 1 || math.Abs(*m.WAN.Up-1e5) > 1 || len(m.Fine) != 2 && len(m.Fine) != 1 {
		t.Fatalf("rates: %+v fine=%d", m.WAN, len(m.Fine))
	}
	// the API view
	v, ok := f.svc.Metrics(o.ID, 24*time.Hour)
	if !ok || v.Optical.Current == nil || *v.Optical.Current.RxDBm != -21.5 || v.Devices.Count != 1 || v.Devices.Total != 2 || v.QoS.LinkStatus != "Up" ||
		v.QoS.LineDown == nil || v.QoS.Counters["ErrorsSent"] != 2 || len(v.QoS.Diag) != 1 || len(v.Throughput.Series) == 0 || len(v.Optical.History) == 0 || v.StepS != 0 {
		t.Fatalf("view: %+v", v)
	}
	if b, ok := f.svc.Brief(o.ID); !ok || !strings.Contains(b.Optical, "Rx -21.5 dBm") || b.Devices != 1 || !strings.Contains(b.Rate, "1.0 Mbit/s down") {
		t.Fatalf("brief: %+v", b)
	}
	// a sample sooner than a minute after the last one is not taken
	before := len(get().Fine)
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if after := len(get().Fine); after != before {
		t.Fatalf("an extra session made a sample: %d -> %d", before, after)
	}
}

// A box that does not have the vendor object or a host table records what it lacks; nothing fails.
func TestMetricsGapsAreRecorded(t *testing.T) {
	f := newFixture(t)
	c := newVSOL(t, f, "VSOL00127E0F", "10.6.3.20", false)
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	eventually(t, "sample", func() bool {
		var ok bool
		f.st.View(func(d *store.Data) { ok = !d.ONUMetricsOf(o.ID).Updated.IsZero() })
		return ok
	})
	v, _ := f.svc.Metrics(o.ID, time.Hour)
	var optical, hosts bool
	for _, g := range v.Gaps {
		optical = optical || strings.HasPrefix(g, "optical: no PON")
		hosts = hosts || strings.HasPrefix(g, "hosts")
	}
	if !optical {
		t.Fatalf("gaps: %v", v.Gaps)
	}
	_ = hosts
	if _, err := ParseRange("90d"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"91d", "0h", "x", "5", "-1h"} {
		if _, err := ParseRange(bad); err == nil {
			t.Fatalf("range %q accepted", bad)
		}
	}
}
