package acs

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Metric collection: on the ONU's periodic Inform (the existing cadence; nothing is polled between check-ins and
// the router is not involved) the ACS reads, in the same session, what a provider wants to know about the box:
//
//   - optical levels (Rx/Tx power, temperature, supply voltage, laser bias),
//   - the WAN byte counters, from which the bit rates are computed between two samples,
//   - the line rate, link state and the packet/error/discard counters of the WAN,
//   - the LAN host table (name, MAC, IP, SSID/port, and per-host bytes where the box has them),
//   - uptime, and a reboot count (an uptime that went down, or a BOOT event).
//
// The readings are stored as a bounded time series (store.ONUMetrics: 48 h of samples, 14 days of 5-minute buckets,
// 90 days of hourly buckets). Nothing here runs an active test: TR-143 diagnostics (download, upload, ping) are only
// noted when the box has them.
//
// The parameter names are the standard ones (TR-098 WANCommonInterfaceConfig, WANConnectionDevice Stats, Hosts.Host;
// TR-181 Optical, IP.Interface, Hosts) plus a search for the vendor object that holds the PON levels, because that
// one has no standard name (V-SOL, Huawei and ZTE each use their own). Whatever the box returned for it is kept raw
// (ONUOptical.Raw); a name that was not found is recorded in Gaps so the operator can check it against the box.

// minSampleGap is the shortest time between two samples of one ONU (an extra session, for instance after a Wi-Fi
// change, does not make a sample).
const minSampleGap = 55 * time.Second

// maxPlausibleBps caps a computed bit rate: a counter that jumped (a reset the uptime did not show) must not become
// a spike on the graph.
const maxPlausibleBps = 10e9

// ---- pure helpers (tested without a box) ----

// counterDelta is the number of units a counter grew from prev to cur. A counter that went down is a 32-bit
// wrap when the previous value was in the 32-bit range and the growth that implies is plausible for the time between
// the samples; otherwise it is a reset (ok = false: no rate for this interval). maxPerSec bounds the plausible
// growth per second (0 = no bound).
func counterDelta(prev, cur uint64, dt time.Duration, maxPerSec float64) (delta uint64, ok bool) {
	if dt <= 0 {
		return 0, false
	}
	secs := dt.Seconds()
	switch {
	case cur >= prev:
		delta = cur - prev
	case prev <= math.MaxUint32 && cur <= math.MaxUint32:
		delta = (math.MaxUint32 - prev) + cur + 1
	default:
		return 0, false
	}
	if maxPerSec > 0 && float64(delta)/secs > maxPerSec {
		return 0, false
	}
	return delta, true
}

// bitrate is bytes over dt as bit/s.
func bitrate(bytes uint64, dt time.Duration) float64 { return float64(bytes) * 8 / dt.Seconds() }

// leadingFloat parses the number at the start of a value ("-18.35", "-18.35 dBm", "+3.30V").
func leadingFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) {
		c := s[end]
		if c >= '0' && c <= '9' || c == '.' || ((c == '-' || c == '+') && end == 0) || ((c == 'e' || c == 'E') && end > 0) {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:end], 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func parseU64(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		f, err2 := strconv.ParseFloat(s, 64)
		if err2 != nil || f < 0 || f > 1.8e19 {
			return 0, false
		}
		return uint64(f), true
	}
	return n, true
}

func f64(v float64) *float64 { return &v }
func u64(v uint64) *uint64   { return &v }

// dBm turns a reported optical power into dBm. Boxes report it in dBm, in 0.1, 0.01 or 0.001 dBm (TR-181 uses
// 0.001); the first scale that gives a believable value (-50 to +10 dBm) wins. milli says the box is a TR-181 one
// that reports in 0.001 dBm, tried first. 0 is "no signal", not a level.
func dBm(v float64, milli bool) (float64, bool) {
	if v == 0 {
		return 0, false
	}
	scales := []float64{1, 10, 100, 1000}
	if milli {
		scales = []float64{1000, 1, 10, 100}
	}
	for _, sc := range scales {
		if x := v / sc; x >= -50 && x <= 10 {
			return math.Round(x*100) / 100, true
		}
	}
	return 0, false
}

func celsius(v float64) (float64, bool) {
	for _, sc := range []float64{1, 256, 100, 10} {
		if x := v / sc; x >= -40 && x <= 150 {
			return math.Round(x*10) / 10, true
		}
	}
	return 0, false
}

func volts(v float64) (float64, bool) {
	for _, sc := range []float64{1, 1000, 10000, 100000} {
		if x := v / sc; x > 0 && x <= 12 {
			return math.Round(x*1000) / 1000, true
		}
	}
	return 0, false
}

// milliamps: laser bias, in mA or µA (a box that says 12345 means 12.345 mA).
func milliamps(v float64) (float64, bool) {
	if v < 0 {
		return 0, false
	}
	for _, sc := range []float64{1, 1000, 1000000} {
		if x := v / sc; x <= 150 {
			return math.Round(x*1000) / 1000, true
		}
	}
	return 0, false
}

// opticalField says which field a leaf of the PON object is, "" when it is none of them. Thresholds and alarms are
// not readings.
func opticalField(leaf string) string {
	l := strings.ToLower(leaf)
	for _, bad := range []string{"threshold", "upper", "lower", "alarm", "max", "min", "enable", "status", "type", "wavelength", "ber", "fec", "rate"} {
		if strings.Contains(l, bad) {
			return ""
		}
	}
	switch {
	case strings.Contains(l, "rxpower"), strings.Contains(l, "receivepower"), strings.Contains(l, "receivedpower"), strings.Contains(l, "opticalsignallevel"), l == "rxopticalpower":
		return "rx"
	case strings.Contains(l, "txpower"), strings.Contains(l, "transmitpower"), strings.Contains(l, "transmitopticallevel"), l == "txopticalpower":
		return "tx"
	case strings.Contains(l, "temperature"):
		return "temp"
	case strings.Contains(l, "voltage"), strings.Contains(l, "vottage"):
		return "volt"
	case strings.Contains(l, "bias"):
		return "bias"
	}
	return ""
}

// parseOptical reads the values below an optical object into the fields it can recognise and keeps every leaf
// as it came (raw). Leaves of secret names are not kept.
func parseOptical(path string, vals []ParamValue) *store.ONUOptical {
	o := &store.ONUOptical{Path: path, Raw: map[string]string{}}
	for _, v := range vals {
		if !strings.HasPrefix(v.Name, path) {
			continue
		}
		leaf := v.Name[len(path):]
		if leaf == "" || Sensitive(leaf) {
			continue
		}
		if len(o.Raw) < store.MaxMetricRaw && len(v.Value) <= 64 {
			o.Raw[leaf] = v.Value
		}
		f, ok := leadingFloat(v.Value)
		if !ok {
			continue
		}
		last := leaf
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			last = leaf[i+1:]
		}
		switch opticalField(last) {
		case "rx":
			if x, ok := dBm(f, strings.HasPrefix(path, store.RootTR181)); ok {
				o.Rx = f64(x)
			}
		case "tx":
			if x, ok := dBm(f, strings.HasPrefix(path, store.RootTR181)); ok {
				o.Tx = f64(x)
			}
		case "temp":
			if x, ok := celsius(f); ok {
				o.Temp = f64(x)
			}
		case "volt":
			if x, ok := volts(f); ok {
				o.Volt = f64(x)
			}
		case "bias":
			if x, ok := milliamps(f); ok {
				o.Bias = f64(x)
			}
		}
	}
	if len(o.Raw) == 0 {
		o.Raw = nil
	}
	return o
}

// opticalName says whether the name of an object looks like the PON/optical one.
func opticalName(name string) bool {
	l := strings.ToLower(name)
	if strings.Contains(l, "wanconnection") || strings.Contains(l, "commoninterface") || strings.Contains(l, "ethernet") || strings.Contains(l, "dsl") {
		return false
	}
	return strings.Contains(l, "gpon") || strings.Contains(l, "epon") || strings.Contains(l, "pon") || strings.Contains(l, "optical")
}

// statLeaf maps a counter leaf of a Stats object to its key ("" = not one we keep).
func statLeaf(leaf string) string {
	l := strings.TrimPrefix(leaf, "Ethernet")
	switch l {
	case "BytesSent", "BytesReceived", "PacketsSent", "PacketsReceived", "ErrorsSent", "ErrorsReceived", "DiscardPacketsSent", "DiscardPacketsReceived":
		return l
	}
	return ""
}

// parseStats adds up the counters of every ".Stats." object in vals (the connections of a WAN device).
func parseStats(vals []ParamValue) (sums map[string]uint64, found bool) {
	sums = map[string]uint64{}
	for _, v := range vals {
		i := strings.LastIndex(v.Name, ".Stats.")
		if i < 0 {
			continue
		}
		k := statLeaf(v.Name[i+len(".Stats."):])
		if k == "" {
			continue
		}
		if n, ok := parseU64(v.Value); ok {
			sums[k] += n
			found = true
		}
	}
	return sums, found
}

// wanReading is what one sample read of the WAN.
type wanReading struct {
	path             string
	rx, tx           *uint64
	lineDn, lineUp   *float64
	link, access     string
	conn             string            // connection status, from the parameter tree's status leaf
	counters         map[string]uint64 // without the byte counters
	haveCommonBytes  bool
	haveStatsCounter bool
}

// parseWANCommon reads WANCommonInterfaceConfig (TR-098).
func parseWANCommon(base string, vals []ParamValue, w *wanReading) {
	get := func(leaf string) string { return valueOf(vals, base+leaf) }
	if n, ok := parseU64(get("TotalBytesReceived")); ok {
		w.rx, w.haveCommonBytes = u64(n), true
	}
	if n, ok := parseU64(get("TotalBytesSent")); ok {
		w.tx, w.haveCommonBytes = u64(n), true
	}
	if f, ok := leadingFloat(get("Layer1DownstreamMaxBitRate")); ok && f > 0 {
		w.lineDn = f64(f)
	}
	if f, ok := leadingFloat(get("Layer1UpstreamMaxBitRate")); ok && f > 0 {
		w.lineUp = f64(f)
	}
	w.link, w.access = get("PhysicalLinkStatus"), get("WANAccessType")
	for _, k := range [][2]string{{"TotalPacketsSent", "PacketsSent"}, {"TotalPacketsReceived", "PacketsReceived"}} {
		if n, ok := parseU64(get(k[0])); ok {
			if w.counters == nil {
				w.counters = map[string]uint64{}
			}
			w.counters[k[1]] = n
		}
	}
}

// applyStats takes the per-connection counters in when the common ones are missing, and always for the error and
// discard counters (the common block does not have them).
func applyStats(sums map[string]uint64, w *wanReading) {
	if !w.haveCommonBytes {
		if n, ok := sums["BytesReceived"]; ok {
			w.rx = u64(n)
		}
		if n, ok := sums["BytesSent"]; ok {
			w.tx = u64(n)
		}
	}
	for k, n := range sums {
		if k == "BytesSent" || k == "BytesReceived" {
			continue
		}
		if w.counters == nil {
			w.counters = map[string]uint64{}
		}
		w.counters[k] = n
	}
}

// hostReading is one row of the host table.
type hostReading struct {
	name, mac, ip, iface string
	active               bool
	rx, tx               *uint64
}

// hostLeaves are the leaves of a host row (TR-098 Hosts.Host.N.*, TR-181 Hosts.Host.N.*).
func parseHosts(base string, vals []ParamValue, wifiOf func(layer2 string) string) []hostReading {
	order, rows := groupInst(vals, base)
	var out []hostReading
	for _, n := range order {
		r := rows[n]
		h := hostReading{name: r["HostName"], ip: r["IPAddress"], active: true}
		h.mac = r["PhysAddress"]
		if h.mac == "" {
			h.mac = r["MACAddress"]
		}
		if c, ok := store.CanonMAC(h.mac); ok {
			h.mac = c
		}
		if a, has := r["Active"]; has {
			h.active = isTrue(a)
		}
		l2 := r["Layer2Interface"]
		if l2 == "" {
			l2 = r["Layer1Interface"]
		}
		h.iface = wifiOf(l2)
		if h.iface == "" {
			switch it := strings.ToLower(r["InterfaceType"]); {
			case strings.Contains(it, "802.11"), strings.Contains(it, "wifi"):
				h.iface = "Wi-Fi"
			case it != "":
				h.iface = "LAN"
			case strings.Contains(strings.ToLower(l2), "wlan"), strings.Contains(strings.ToLower(l2), "wifi"):
				h.iface = "Wi-Fi"
			case l2 != "":
				h.iface = "LAN"
			}
		}
		for leaf, val := range r {
			l := strings.ToLower(leaf)
			if !strings.Contains(l, "bytes") && !strings.Contains(l, "octets") {
				continue
			}
			n, ok := parseU64(val)
			if !ok {
				continue
			}
			switch {
			case strings.Contains(l, "sent"), strings.Contains(l, "tx"), strings.Contains(l, "upstream"):
				h.tx = u64(n)
			case strings.Contains(l, "recv"), strings.Contains(l, "received"), strings.Contains(l, "rx"), strings.Contains(l, "downstream"):
				h.rx = u64(n)
			}
		}
		if h.mac == "" && h.ip == "" {
			continue
		}
		out = append(out, h)
		if len(out) >= store.MaxMetricHosts {
			break
		}
	}
	return out
}

// reading is one complete read of an ONU.
type reading struct {
	uptime    int64
	hasUptime bool
	optical   *store.ONUOptical
	wan       *wanReading
	hosts     []hostReading
	hasHosts  bool
	gaps      []string
	diag      []string
	paths     store.ONUMetricPaths
	boot      bool                // the session carried a BOOT event
	res       *store.TreeResolved // the resolved parameter tree, nil until the ONU has been walked
}

// pathsFromTree takes the object paths the metrics read from the resolved parameter tree. What the tree does not have is
// left empty (and is reported as a gap on every sample): it is never guessed.
func pathsFromTree(res *store.TreeResolved, rd *reading, stamp string) {
	rd.paths.Optical = res.One("optical.object")
	rd.paths.WANs = append([]string(nil), res.Paths["wan.devices"]...)
	rd.paths.Hosts = res.One("hosts.table")
	rd.diag = append([]string(nil), res.Paths["diag"]...)
	rd.paths.Probed, rd.paths.Tree = true, stamp
}

// readConnStatus reads the connection status leaves the tree found (ConnectionStatus, IP.Interface.N.Status) and keeps
// "Connected" when any connection has it, else the first value. Only for a box that has been walked.
func readConnStatus(ctx context.Context, ss *session, rd *reading) error {
	if rd.res == nil || rd.wan == nil {
		return nil
	}
	names := rd.res.Paths["wan.status"]
	if len(names) == 0 {
		return nil
	}
	vals, err := gpvTolerant(ctx, ss, names, "wan status", &rd.gaps)
	if err != nil {
		return err
	}
	for _, v := range vals {
		val := strings.TrimSpace(v.Value)
		if val == "" || len(val) > 40 {
			continue
		}
		if rd.wan.conn == "" || strings.EqualFold(val, "Connected") || strings.EqualFold(val, "Up") {
			rd.wan.conn = val
		}
	}
	return nil
}

// treeGaps adds what the tree says is missing for the metrics (optical, wan, hosts).
func treeGaps(rd *reading) {
	if rd.res == nil {
		return
	}
	for _, g := range rd.res.Gaps {
		if strings.HasPrefix(g, "optical:") || strings.HasPrefix(g, "wan:") || strings.HasPrefix(g, "hosts:") {
			rd.gaps = addGap(rd.gaps, g)
		}
	}
}

func addGap(gaps []string, g string) []string {
	for _, x := range gaps {
		if x == g {
			return gaps
		}
	}
	if len(gaps) >= store.MaxMetricGaps {
		return gaps
	}
	return append(gaps, g)
}

// applyReading folds a reading into the record of the ONU: the current values, the bit rates from the counter
// growth since the previous sample, the reboot count, the new point of the series, and the retention. It is pure.
func applyReading(prev store.ONUMetrics, rd reading, now time.Time) store.ONUMetrics {
	m := prev.Clone()
	first := m.Updated.IsZero()
	dt := now.Sub(m.Updated)
	rebooted := rd.boot && !first
	if rd.hasUptime {
		if !first && prev.Uptime > 0 && rd.uptime+5 < prev.Uptime {
			rebooted = true
		}
		m.Uptime = rd.uptime
	}
	if rebooted {
		m.Reboots++
		m.LastBoot = now
		if rd.hasUptime && rd.uptime > 0 {
			m.LastBoot = now.Add(-time.Duration(rd.uptime) * time.Second)
		}
	}
	rates := !first && !rebooted && dt > 0
	pt := store.MetricPoint{T: now.Unix(), N: 1}
	have := false

	if rd.optical != nil {
		m.Optical = rd.optical
		pt.Rx, pt.Tx, pt.Temp, pt.Volt, pt.Bias = rd.optical.Rx, rd.optical.Tx, rd.optical.Temp, rd.optical.Volt, rd.optical.Bias
		have = rd.optical.Rx != nil || rd.optical.Tx != nil || rd.optical.Temp != nil || rd.optical.Volt != nil || rd.optical.Bias != nil
	}
	if rd.wan != nil {
		w := &store.ONUWAN{Path: rd.wan.path, RxBytes: rd.wan.rx, TxBytes: rd.wan.tx, LineDn: rd.wan.lineDn, LineUp: rd.wan.lineUp,
			Link: rd.wan.link, Conn: rd.wan.conn, Access: rd.wan.access, Counters: rd.wan.counters}
		if rates && prev.WAN != nil {
			if rd.wan.rx != nil && prev.WAN.RxBytes != nil {
				if d, ok := counterDelta(*prev.WAN.RxBytes, *rd.wan.rx, dt, maxPlausibleBps/8); ok {
					w.Dn = f64(bitrate(d, dt))
				}
			}
			if rd.wan.tx != nil && prev.WAN.TxBytes != nil {
				if d, ok := counterDelta(*prev.WAN.TxBytes, *rd.wan.tx, dt, maxPlausibleBps/8); ok {
					w.Up = f64(bitrate(d, dt))
				}
			}
			for k, cur := range rd.wan.counters {
				if p, ok := prev.WAN.Counters[k]; ok {
					if d, ok := counterDelta(p, cur, dt, 0); ok {
						if w.Deltas == nil {
							w.Deltas = map[string]uint64{}
						}
						w.Deltas[k] = d
					}
				}
			}
		}
		m.WAN = w
		pt.Dn, pt.Up = w.Dn, w.Up
		have = have || w.Dn != nil || w.Up != nil
	}
	if rd.hasHosts {
		old := map[string]store.ONUHost{}
		for _, h := range prev.Hosts {
			if h.MAC != "" {
				old[h.MAC] = h
			}
		}
		var hosts []store.ONUHost
		for _, h := range rd.hosts {
			x := store.ONUHost{Name: h.name, MAC: h.mac, IP: h.ip, Iface: h.iface, Active: h.active, RxBytes: h.rx, TxBytes: h.tx}
			if p, ok := old[h.mac]; ok && rates {
				if h.rx != nil && p.RxBytes != nil {
					if d, ok := counterDelta(*p.RxBytes, *h.rx, dt, maxPlausibleBps/8); ok {
						x.Dn = f64(bitrate(d, dt))
					}
				}
				if h.tx != nil && p.TxBytes != nil {
					if d, ok := counterDelta(*p.TxBytes, *h.tx, dt, maxPlausibleBps/8); ok {
						x.Up = f64(bitrate(d, dt))
					}
				}
			}
			hosts = append(hosts, x)
		}
		m.Hosts = hosts
	}
	m.Paths = rd.paths
	if rd.diag != nil { // found when the objects were probed; kept between probes
		m.Diag = rd.diag
	}
	m.Gaps = rd.gaps
	if len(m.Gaps) > store.MaxMetricGaps {
		m.Gaps = m.Gaps[:store.MaxMetricGaps]
	}
	m.Updated = now
	if have {
		m.Fine = append(m.Fine, pt)
	}
	m.Compact(now)
	return m
}

// ---- reading from the box ----

// collectMetrics reads the box in the session and stores the sample. It never fails the session: a read the box
// refuses is recorded as a gap. reprobe makes it look for the vendor objects again.
func (s *Service) collectMetrics(ctx context.Context, ss *session, onuID string, boot, reprobe bool) {
	o, ok := s.loadONU(onuID)
	if !ok || o.Root == "" {
		return
	}
	var prev store.ONUMetrics
	s.st.View(func(d *store.Data) { prev = d.ONUMetricsOf(onuID).Clone() })
	now := s.now()
	if !boot && !prev.Updated.IsZero() && now.Sub(prev.Updated) < minSampleGap {
		return
	}
	// The paths come from the parameter tree once it has been walked (and are resolved again after every new walk);
	// until then they are found by probing the standard names.
	var res *store.TreeResolved
	stamp := ""
	s.st.View(func(d *store.Data) {
		if t, ok := d.ONUTreeOf(onuID); ok && t.Root == o.Root {
			r := t.Clone().Resolved
			res, stamp = &r, strconv.FormatInt(t.WalkedAt.Unix(), 10)
		}
	})
	paths := prev.Paths
	if reprobe || paths.Tree != stamp {
		paths = store.ONUMetricPaths{}
	}
	rd, err := s.readMetrics(ctx, ss, o, paths, res, stamp)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Info("ACS: metrics not read", "onu", onuID, "err", err.Error())
		}
		return
	}
	rd.boot = boot
	_ = s.st.Update(func(d *store.Data) error {
		if _, ok := d.ONUs[onuID]; !ok {
			return store.ErrNoChange
		}
		cur := d.ONUMetricsOf(onuID)
		cur.ID = onuID
		d.ONUMetrics[onuID] = applyReading(cur, rd, s.now())
		return nil
	})
}

// gpvTolerant reads a path; a fault becomes a gap (and nil values), a transport error is returned.
func gpvTolerant(ctx context.Context, ss *session, names []string, what string, gaps *[]string) ([]ParamValue, error) {
	vals, f, err := ss.gpv(ctx, names)
	if err != nil {
		return nil, err
	}
	if f != nil {
		*gaps = addGap(*gaps, what+": "+f.Text())
		return nil, nil
	}
	return vals, nil
}

func (s *Service) readMetrics(ctx context.Context, ss *session, o store.ONU, paths store.ONUMetricPaths, res *store.TreeResolved, stamp string) (reading, error) {
	var rd reading
	rd.paths = paths
	rd.res = res
	if res != nil && !rd.paths.Probed {
		pathsFromTree(res, &rd, stamp)
	}
	r := o.Root
	vals, err := gpvTolerant(ctx, ss, []string{r + "DeviceInfo.UpTime"}, "uptime", &rd.gaps)
	if err != nil {
		return rd, err
	}
	if n, ok := leadingFloat(valueOf(vals, r+"DeviceInfo.UpTime")); ok && n >= 0 {
		rd.uptime, rd.hasUptime = int64(n), true
	}
	if o.Root == store.RootTR181 {
		err = s.readMetrics181(ctx, ss, o, &rd)
	} else {
		err = s.readMetrics098(ctx, ss, o, &rd)
	}
	if err == nil {
		err = readConnStatus(ctx, ss, &rd)
	}
	treeGaps(&rd)
	return rd, err
}

// probe098 finds the WAN devices, the PON object, the host table and the diagnostics objects once.
func (s *Service) probe098(ctx context.Context, ss *session, o store.ONU, rd *reading) error {
	r := o.Root
	wans, err := s.listInst(ctx, ss, r+"WANDevice.", "")
	if err != nil {
		return err
	}
	if len(wans) > 4 {
		wans = wans[:4]
	}
	rd.paths.WANs = wans
	cands := []string{}
	for _, wan := range wans {
		names, f, err := ss.gpn(ctx, wan, true)
		if err != nil {
			return err
		}
		if f != nil {
			continue
		}
		for _, n := range names {
			if strings.HasSuffix(n.Name, ".") && n.Name != wan && opticalName(strings.TrimPrefix(n.Name, wan)) {
				cands = append(cands, n.Name)
			}
		}
	}
	names, f, err := ss.gpn(ctx, r, true)
	if err != nil {
		return err
	}
	if f == nil {
		for _, n := range names {
			leaf := strings.TrimSuffix(strings.TrimPrefix(n.Name, r), ".")
			switch {
			case strings.HasSuffix(n.Name, ".") && !strings.Contains(leaf, ".") && opticalName(leaf) && !strings.HasPrefix(leaf, "WANDevice"):
				cands = append(cands, n.Name)
			case leaf == "DownloadDiagnostics", leaf == "UploadDiagnostics", leaf == "IPPingDiagnostics", leaf == "TraceRouteDiagnostics", leaf == "NSLookupDiagnostics":
				rd.diag = append(rd.diag, leaf)
			}
		}
	}
	if len(cands) > 0 {
		rd.paths.Optical = cands[0]
	}
	lans, err := s.listInst(ctx, ss, r+"LANDevice.", "")
	if err != nil {
		return err
	}
	if len(lans) > 0 {
		rd.paths.Hosts = lans[0] + "Hosts.Host."
	}
	rd.paths.Probed = true
	return nil
}

func (s *Service) readMetrics098(ctx context.Context, ss *session, o store.ONU, rd *reading) error {
	r := o.Root
	if !rd.paths.Probed {
		if err := s.probe098(ctx, ss, o, rd); err != nil {
			return err
		}
		if rd.paths.Optical == "" {
			rd.gaps = addGap(rd.gaps, "optical: no PON or optical object found under "+r+" or its WANDevice (open Refresh again after the ONU is on the PON, or check the vendor object name on the bench)")
		}
	}
	if rd.paths.Optical != "" {
		vals, err := gpvTolerant(ctx, ss, []string{rd.paths.Optical}, "optical "+strings.TrimPrefix(rd.paths.Optical, r), &rd.gaps)
		if err != nil {
			return err
		}
		if vals == nil {
			rd.paths.Probed = false // the object is gone: look again next time
			rd.paths.Optical = ""
		} else {
			rd.optical = parseOptical(rd.paths.Optical, vals)
			if rd.optical.Rx == nil {
				rd.gaps = addGap(rd.gaps, "optical: no Rx power recognised in "+strings.TrimPrefix(rd.paths.Optical, r)+" (raw values are kept)")
			}
		}
	}
	// WAN counters: the common block, then the connections' Stats.
	var tot *wanReading
	for _, wan := range rd.paths.WANs {
		w := &wanReading{path: wan}
		common := wan + "WANCommonInterfaceConfig."
		vals, err := gpvTolerant(ctx, ss, []string{common}, "wan "+strings.TrimPrefix(common, r), &rd.gaps)
		if err != nil {
			return err
		}
		parseWANCommon(common, vals, w)
		cv, err := gpvTolerant(ctx, ss, []string{wan + "WANConnectionDevice."}, "wan connections "+strings.TrimPrefix(wan, r), &rd.gaps)
		if err != nil {
			return err
		}
		if sums, found := parseStats(cv); found {
			applyStats(sums, w)
		}
		if w.rx == nil && w.tx == nil && w.lineDn == nil && w.link == "" && len(w.counters) == 0 {
			continue // the PON-only device: nothing of the WAN here
		}
		if tot == nil {
			tot = w
		}
	}
	if tot != nil {
		rd.wan = tot
		if tot.rx == nil || tot.tx == nil {
			rd.gaps = addGap(rd.gaps, "wan: no byte counters (TotalBytes* or connection Stats) found, so no bit rate")
		}
	} else if len(rd.paths.WANs) > 0 {
		rd.gaps = addGap(rd.gaps, "wan: no WAN counters found, so no bit rate")
	}
	// Hosts.
	if rd.paths.Hosts != "" {
		vals, err := gpvTolerant(ctx, ss, []string{rd.paths.Hosts}, "hosts", &rd.gaps)
		if err != nil {
			return err
		}
		if vals != nil {
			rd.hosts, rd.hasHosts = parseHosts(rd.paths.Hosts, vals, wifiIface(o, "")), true
		}
	}
	return nil
}

// wifiIface turns a host's Layer2Interface/Layer1Interface (a path to a WLANConfiguration or WiFi object) into the
// network name the box reported for it, "" when it is not a Wi-Fi network this server knows.
func wifiIface(o store.ONU, _ string) func(string) string {
	tail := func(p string) string { // "...WLANConfiguration.5" from any spelling of the path
		p = strings.TrimSuffix(p, ".")
		if i := strings.LastIndex(p, "WLANConfiguration"); i >= 0 {
			return p[i:]
		}
		return p
	}
	return func(l2 string) string {
		if l2 == "" {
			return ""
		}
		for _, w := range o.WiFi {
			if tail(w.Path) == tail(l2) {
				if w.SSID != "" {
					return w.SSID
				}
				return "Wi-Fi"
			}
		}
		return ""
	}
}

func (s *Service) readMetrics181(ctx context.Context, ss *session, o store.ONU, rd *reading) error {
	r := o.Root
	if !rd.paths.Probed {
		names, f, err := ss.gpn(ctx, r+"Optical.Interface.", true)
		if err != nil {
			return err
		}
		if f == nil {
			for _, n := range names {
				if strings.HasSuffix(n.Name, ".") && n.Name != r+"Optical.Interface." {
					rd.paths.Optical = n.Name
					break
				}
			}
		}
		if dn, f, err := ss.gpn(ctx, r+"IP.Diagnostics.", true); err != nil {
			return err
		} else if f == nil {
			for _, n := range dn {
				leaf := strings.TrimSuffix(strings.TrimPrefix(n.Name, r+"IP.Diagnostics."), ".")
				if strings.HasSuffix(leaf, "Diagnostics") && !strings.Contains(leaf, ".") {
					rd.diag = append(rd.diag, leaf)
				}
			}
		}
		rd.paths.Hosts = r + "Hosts.Host."
		ifs, err := s.listInst(ctx, ss, r+"IP.Interface.", "")
		if err != nil {
			return err
		}
		rd.paths.WANs = nil
		for _, ip := range ifs {
			vals, f, err := ss.gpv(ctx, []string{ip + "Name", ip + "Alias"})
			if err != nil {
				return err
			}
			if f != nil {
				continue
			}
			nm := strings.ToLower(valueOf(vals, ip+"Name") + " " + valueOf(vals, ip+"Alias"))
			if strings.Contains(nm, "wan") || strings.Contains(nm, "ppp") || strings.Contains(nm, "internet") {
				rd.paths.WANs = append(rd.paths.WANs, ip)
			}
		}
		if len(rd.paths.WANs) == 0 {
			rd.gaps = addGap(rd.gaps, "wan: no IP interface named like WAN/PPP/Internet in "+r+"IP.Interface.; the TR-181 WAN counters are not read until the right interface is known")
		}
		if rd.paths.Optical == "" {
			rd.gaps = addGap(rd.gaps, "optical: no "+r+"Optical.Interface. object")
		}
		rd.paths.Probed = true
	}
	if rd.paths.Optical != "" {
		vals, err := gpvTolerant(ctx, ss, []string{rd.paths.Optical}, "optical", &rd.gaps)
		if err != nil {
			return err
		}
		if vals != nil {
			rd.optical = parseOptical(rd.paths.Optical, vals)
		}
	}
	for _, ip := range rd.paths.WANs {
		vals, err := gpvTolerant(ctx, ss, []string{ip + "Stats."}, "wan stats", &rd.gaps)
		if err != nil {
			return err
		}
		w := &wanReading{path: ip}
		if sums, found := parseStats(vals); found {
			applyStats(sums, w)
			rd.wan = w
		}
		break
	}
	if rd.paths.Hosts != "" {
		vals, err := gpvTolerant(ctx, ss, []string{rd.paths.Hosts}, "hosts", &rd.gaps)
		if err != nil {
			return err
		}
		if vals != nil {
			rd.hosts, rd.hasHosts = parseHosts(rd.paths.Hosts, vals, func(l1 string) string {
				if strings.Contains(strings.ToLower(l1), "wifi") {
					return "Wi-Fi"
				}
				if l1 != "" {
					return "LAN"
				}
				return ""
			}), true
		}
	}
	return nil
}

// sortedHosts orders hosts for display: active first, then by name.
func sortedHosts(h []store.ONUHost) []store.ONUHost {
	out := append([]store.ONUHost(nil), h...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}
