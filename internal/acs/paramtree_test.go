package acs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func tp(paths ...string) []store.TreeParam {
	var out []store.TreeParam
	for _, p := range paths {
		out = append(out, store.TreeParam{Path: p, Secret: secretLeaf(p)})
	}
	return out
}

func tpv(p, v string) store.TreeParam { return store.TreeParam{Path: p, Value: v} }

func has(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

func hasPrefix(list []string, pre string) bool {
	for _, x := range list {
		if strings.HasPrefix(x, pre) {
			return true
		}
	}
	return false
}

func TestResolveTR098FindsTheRealPaths(t *testing.T) {
	r := store.RootTR098
	w := r + "WANDevice.1."
	ps := tp(
		r+"DeviceInfo.UpTime",
		w+"X_GponInterafceConfig.RXPower", w+"X_GponInterafceConfig.TXPower", w+"X_GponInterafceConfig.TransceiverTemperature",
		w+"X_GponInterafceConfig.SupplyVottage", w+"X_GponInterafceConfig.BiasCurrent", w+"X_GponInterafceConfig.RXPowerHighAlarm",
		w+"WANCommonInterfaceConfig.TotalBytesReceived", w+"WANCommonInterfaceConfig.TotalBytesSent", w+"WANCommonInterfaceConfig.PhysicalLinkStatus",
		w+"WANConnectionDevice.1.WANIPConnection.1.ConnectionStatus", w+"WANConnectionDevice.1.WANIPConnection.1.Stats.EthernetBytesSent",
		r+"LANDevice.1.Hosts.Host.1.PhysAddress", r+"LANDevice.1.Hosts.Host.1.IPAddress",
		r+"LANDevice.1.WLANConfiguration.1.SSID", r+"LANDevice.1.WLANConfiguration.1.PreSharedKey.1.KeyPassphrase", r+"LANDevice.1.WLANConfiguration.1.PreSharedKey.1.PreSharedKey",
		r+"LANDevice.1.WLANConfiguration.5.SSID", r+"LANDevice.1.WLANConfiguration.5.KeyPassphrase",
		r+"IPPingDiagnostics.State")
	got := Resolve(r, ps)
	want := map[string]string{
		"optical.object": w + "X_GponInterafceConfig.", "optical.rx": w + "X_GponInterafceConfig.RXPower", "optical.tx": w + "X_GponInterafceConfig.TXPower",
		"optical.temp": w + "X_GponInterafceConfig.TransceiverTemperature", "optical.volt": w + "X_GponInterafceConfig.SupplyVottage", "optical.bias": w + "X_GponInterafceConfig.BiasCurrent",
		"wan.rx_bytes": w + "WANCommonInterfaceConfig.TotalBytesReceived", "wan.tx_bytes": w + "WANCommonInterfaceConfig.TotalBytesSent",
		"wan.link": w + "WANCommonInterfaceConfig.PhysicalLinkStatus", "wan.status": w + "WANConnectionDevice.1.WANIPConnection.1.ConnectionStatus",
		"wan.devices": w, "hosts.table": r + "LANDevice.1.Hosts.Host.",
	}
	for k, v := range want {
		if got.One(k) != v {
			t.Errorf("%s = %q, want %q", k, got.One(k), v)
		}
	}
	// the alarm is not a level; the two networks each have their passphrase leaf, and the PreSharedKey one is not chosen
	if pp := got.Paths["wifi.passphrase"]; len(pp) != 2 || pp[0] != r+"LANDevice.1.WLANConfiguration.1.PreSharedKey.1.KeyPassphrase" || pp[1] != r+"LANDevice.1.WLANConfiguration.5.KeyPassphrase" {
		t.Errorf("passphrase leaves: %v", pp)
	}
	if len(got.Paths["wifi.ssid"]) != 2 {
		t.Errorf("ssid leaves: %v", got.Paths["wifi.ssid"])
	}
	if d := got.Paths["diag"]; len(d) != 1 || d[0] != "IPPingDiagnostics" {
		t.Errorf("diag: %v", d)
	}
	if len(got.Gaps) != 0 {
		t.Errorf("unexpected gaps: %v", got.Gaps)
	}
	if len(got.Notes) == 0 || !strings.Contains(got.Notes[0], "both a KeyPassphrase and a PreSharedKey") {
		t.Errorf("the ambiguity should be noted: %v", got.Notes)
	}
}

func TestResolveTR181AndVendorNames(t *testing.T) {
	r := store.RootTR181
	ps := []store.TreeParam{
		tpv(r+"Optical.Interface.1.OpticalSignalLevel", ""), tpv(r+"Optical.Interface.1.TransmitOpticalLevel", ""), tpv(r+"Optical.Interface.1.LowerOpticalThreshold", ""),
		tpv(r+"IP.Interface.1.Name", "br0"), tpv(r+"IP.Interface.1.Stats.BytesReceived", ""), tpv(r+"IP.Interface.1.Stats.BytesSent", ""), tpv(r+"IP.Interface.1.Status", ""),
		tpv(r+"IP.Interface.2.Name", "veip0.1 WAN"), tpv(r+"IP.Interface.2.Stats.BytesReceived", ""), tpv(r+"IP.Interface.2.Stats.BytesSent", ""), tpv(r+"IP.Interface.2.Status", ""),
		tpv(r+"Hosts.Host.1.MACAddress", ""), tpv(r+"Hosts.Host.1.IPAddress", ""),
		tpv(r+"WiFi.SSID.1.SSID", ""), tpv(r+"WiFi.AccessPoint.1.Security.KeyPassphrase", ""), tpv(r+"WiFi.AccessPoint.1.Security.PreSharedKey", ""),
		tpv(r+"IP.Diagnostics.DownloadDiagnostics.State", ""),
	}
	got := Resolve(r, ps)
	if got.One("optical.rx") != r+"Optical.Interface.1.OpticalSignalLevel" || got.One("optical.tx") != r+"Optical.Interface.1.TransmitOpticalLevel" {
		t.Errorf("optical: %v", got.Paths)
	}
	if got.One("wan.devices") != r+"IP.Interface.2." || got.One("wan.rx_bytes") != r+"IP.Interface.2.Stats.BytesReceived" || got.One("wan.status") != r+"IP.Interface.2.Status" {
		t.Errorf("the WAN interface is the one named WAN, not br0: %v", got.Paths)
	}
	if got.One("hosts.table") != r+"Hosts.Host." {
		t.Errorf("hosts: %q", got.One("hosts.table"))
	}
	if got.One("wifi.passphrase") != r+"WiFi.AccessPoint.1.Security.KeyPassphrase" || got.One("wifi.ssid") != r+"WiFi.SSID.1.SSID" {
		t.Errorf("wifi: %v", got.Paths)
	}
	if d := got.Paths["diag"]; len(d) != 1 || d[0] != "DownloadDiagnostics" {
		t.Errorf("diag: %v", d)
	}
	// bias, temperature and voltage are not there: gaps, and no path is made up for them
	for _, f := range []string{"temp", "volt", "bias"} {
		if got.One("optical."+f) != "" || !hasPrefix(got.Gaps, "optical: no "+f+" leaf") {
			t.Errorf("%s should be a gap: %v %v", f, got.One("optical."+f), got.Gaps)
		}
	}
	// two interfaces with counters and no names read: nothing is chosen
	amb := Resolve(r, []store.TreeParam{tpv(r+"IP.Interface.1.Stats.BytesReceived", ""), tpv(r+"IP.Interface.2.Stats.BytesReceived", "")})
	if amb.One("wan.devices") != "" || !hasPrefix(amb.Gaps, "wan: no IP interface identified") {
		t.Errorf("ambiguous WAN: %v %v", amb.Paths, amb.Gaps)
	}
	// ... one interface with counters and no names: that one
	one := Resolve(r, []store.TreeParam{tpv(r+"IP.Interface.3.Stats.BytesReceived", ""), tpv(r+"IP.Interface.3.Stats.BytesSent", "")})
	if one.One("wan.devices") != r+"IP.Interface.3." {
		t.Errorf("single interface: %v", one.Paths)
	}
}

func TestResolveRecordsGapsAndNeverInventsPaths(t *testing.T) {
	r := store.RootTR098
	got := Resolve(r, tp(r+"DeviceInfo.UpTime", r+"DeviceInfo.SoftwareVersion"))
	if len(got.Paths) != 0 {
		t.Fatalf("paths were invented: %v", got.Paths)
	}
	for _, g := range []string{"optical: no PON or optical object", "wan: no byte counters", "hosts: no Hosts.Host table", "wifi: no SSID leaf"} {
		if !hasPrefix(got.Gaps, g) {
			t.Errorf("missing gap %q in %v", g, got.Gaps)
		}
	}
	// a "Temperature" leaf outside a PON object (the CPU, say) is not the optical temperature
	cpu := Resolve(r, tp(r+"DeviceInfo.X_Vendor_CPU.Temperature", r+"DeviceInfo.X_Vendor_Component.Temperature"))
	if cpu.One("optical.temp") != "" {
		t.Fatalf("CPU temperature taken as optical: %v", cpu.Paths)
	}
}

func TestResolveWiFiPassphraseAmbiguity(t *testing.T) {
	r := store.RootTR098
	n := func(i string) string { return r + "LANDevice.1.WLANConfiguration." + i + "." }
	got := Resolve(r, tp(
		n("1")+"SSID", n("1")+"PreSharedKey.1.PreSharedKey", // only the raw key: not a passphrase
		n("2")+"SSID", n("2")+"X_HW_KeyPassphrase", n("2")+"PreSharedKey.1.PreSharedKey", // a vendor spelling is better than the key
		n("3")+"SSID", n("3")+"KeyPassphrase", n("3")+"X_HW_KeyPassphrase", // the plain one beats the vendor one
	))
	pp := got.Paths["wifi.passphrase"]
	if len(pp) != 2 || pp[0] != n("2")+"X_HW_KeyPassphrase" || pp[1] != n("3")+"KeyPassphrase" {
		t.Fatalf("passphrase leaves: %v", pp)
	}
	if k := got.Paths["wifi.preshared_key"]; len(k) != 1 || k[0] != n("1")+"PreSharedKey.1.PreSharedKey" {
		t.Fatalf("raw key leaf: %v", k)
	}
	if !hasPrefix(got.Gaps, "wifi: LANDevice.1.WLANConfiguration.1. has a PreSharedKey leaf but no KeyPassphrase") {
		t.Fatalf("gaps: %v", got.Gaps)
	}
}

func TestSecretLeaves(t *testing.T) {
	r := store.RootTR098
	for _, p := range []string{r + "LANDevice.1.WLANConfiguration.1.PreSharedKey.1.KeyPassphrase", r + "ManagementServer.Password", r + "ManagementServer.ConnectionRequestPassword",
		r + "LANDevice.1.WLANConfiguration.1.PreSharedKey.1.PreSharedKey", r + "LANDevice.1.WLANConfiguration.1.WEPKey.1.WEPKey", r + "X_Vendor.Secret",
		r + "LANDevice.1.WLANConfiguration.1.PreSharedKey.1.AssociatedDeviceMACAddress"} {
		if !secretLeaf(p) {
			t.Errorf("%s should be secret", p)
		}
	}
	for _, p := range []string{r + "DeviceInfo.UpTime", r + "LANDevice.1.WLANConfiguration.1.SSID", r + "WANDevice.1.X_Gpon.RXPower"} {
		if secretLeaf(p) {
			t.Errorf("%s should not be secret", p)
		}
	}
}

func TestWalkReason(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	o := store.ONU{ID: "o1", Root: store.RootTR098, SWVersion: "V1", HWVersion: "H1"}
	tree := store.ONUTree{ID: "o1", Root: store.RootTR098, SWVersion: "V1", HWVersion: "H1"}
	cases := []struct {
		name  string
		o     store.ONU
		t     store.ONUTree
		have  bool
		reset bool
		want  string
	}{
		{"no root", store.ONU{}, tree, false, false, ""},
		{"first walk", o, tree, false, false, WalkFirst},
		{"up to date", o, tree, true, false, ""},
		{"software changed", func() store.ONU { x := o; x.SWVersion = "V2"; return x }(), tree, true, false, WalkVersion},
		{"hardware changed", func() store.ONU { x := o; x.HWVersion = "H2"; return x }(), tree, true, false, WalkVersion},
		{"factory reset", o, tree, true, true, WalkReset},
		{"reset with no tree", o, tree, false, true, WalkReset},
		{"marked", func() store.ONU { x := o; x.State.WalkWanted = true; return x }(), tree, false, false, WalkMarked},
		{"failed an hour ago is not tried", func() store.ONU { x := o; x.State.WalkTry = now.Add(-10 * time.Minute); return x }(), tree, false, false, ""},
		{"failed long ago is tried", func() store.ONU { x := o; x.State.WalkTry = now.Add(-2 * time.Hour); return x }(), tree, false, false, WalkFirst},
		{"a reset overrides the pause", func() store.ONU { x := o; x.State.WalkTry = now.Add(-time.Minute); return x }(), tree, false, true, WalkReset},
	}
	for _, tc := range cases {
		if got := walkReason(tc.o, tc.t, tc.have, tc.reset, now); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEncodeTreeBoundsAndSecrets(t *testing.T) {
	var ps []store.TreeParam
	for i := 0; i < store.MaxTreeParams+500; i++ {
		ps = append(ps, store.TreeParam{Path: fmt.Sprintf("Device.X.%d.Val", i), Value: "v\twith\ttabs\nand newline"})
	}
	ps = append(ps, store.TreeParam{Path: "Device.Y.Password", Secret: true, Value: "hunter2"})
	text, n := store.EncodeTree(ps)
	if n != store.MaxTreeParams || strings.Count(text, "\n") != n {
		t.Fatalf("n=%d lines=%d", n, strings.Count(text, "\n"))
	}
	back := store.ONUTree{Text: text}.Params()
	if len(back) != n || back[0].Path != "Device.X.0.Val" || back[0].Value != "v with tabs and newline" {
		t.Fatalf("round trip: %d %+v", len(back), back[0])
	}
	// a secret parameter's value is never kept, even when the caller passes one
	text, _ = store.EncodeTree([]store.TreeParam{{Path: "Device.Y.Password", Secret: true, Value: "hunter2"}, {Path: "Device.Z", Writable: true, Value: strings.Repeat("a", 500)}})
	if strings.Contains(text, "hunter2") {
		t.Fatalf("a secret value was stored: %q", text)
	}
	ps2 := store.ONUTree{Text: text}.Params()
	if len(ps2) != 2 || !ps2[0].Secret || ps2[0].Value != "" || !ps2[1].Writable || len(ps2[1].Value) != store.MaxTreeValue {
		t.Fatalf("%+v", ps2)
	}
}

// ---- the walk, against a fake box ----

func treeOf(f *fixture, id string) (store.ONUTree, bool) {
	var t store.ONUTree
	var ok bool
	f.st.View(func(d *store.Data) { t, ok = d.ONUTreeOf(id); t = t.Clone() })
	return t, ok
}

func waitTree(t *testing.T, f *fixture, id string) store.ONUTree {
	t.Helper()
	eventually(t, "a parameter tree", func() bool { _, ok := treeOf(f, id); return ok })
	tr, _ := treeOf(f, id)
	return tr
}

func TestWalkStoresTheTreeAndTheMetricsUseIt(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	// a vendor object two levels below WANDevice: the standard probe (children of WANDevice and of the root) does not find it
	c.set(c.root+"WANDevice.1.X_FOO.Optics.RxPower", "-19.25")
	c.set(c.root+"WANDevice.1.X_FOO.Optics.TxPower", "3.1")
	for k := range c.params {
		if strings.Contains(k, "X_GponInterafceConfig") {
			delete(c.params, k)
		}
	}
	c.set(c.root+"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.ConnectionStatus", "Connected")
	c.maxReq = 1000
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	tr := waitTree(t, f, o.ID)
	if tr.Root != store.RootTR098 || tr.Reason != WalkFirst || tr.Count < 30 || tr.Truncated || tr.SWVersion != "V1.2.3" {
		t.Fatalf("tree: %+v", tr)
	}
	if !strings.Contains(tr.Text, c.root+"LANDevice.1.Hosts.Host.1.PhysAddress\t") {
		t.Fatalf("the host table is not in the tree:\n%.600s", tr.Text)
	}
	// the passphrase leaf is listed by path, flagged secret, and its value never reached the store
	if !strings.Contains(tr.Text, "WLANConfiguration.1.PreSharedKey.1.KeyPassphrase\tws\t") && !strings.Contains(tr.Text, "WLANConfiguration.1.PreSharedKey.1.KeyPassphrase\ts\t") {
		t.Fatalf("the passphrase leaf is not flagged:\n%.800s", tr.Text)
	}
	if strings.Contains(tr.Text, "factorypass") {
		t.Fatalf("a passphrase value is in the stored tree")
	}
	// values of ordinary parameters are kept (the resolved ones first)
	var gotUptime bool
	for _, p := range tr.Params() {
		if p.Path == c.root+"DeviceInfo.SoftwareVersion" && p.Value == "V1.2.3" {
			gotUptime = true
		}
	}
	if !gotUptime {
		t.Fatalf("values were not read")
	}
	res := tr.Resolved
	if res.One("optical.object") != c.root+"WANDevice.1.X_FOO.Optics." || res.One("optical.rx") != c.root+"WANDevice.1.X_FOO.Optics.RxPower" {
		t.Fatalf("resolved: %v gaps=%v", res.Paths, res.Gaps)
	}
	if res.One("hosts.table") != c.root+"LANDevice.1.Hosts.Host." || res.One("wan.status") == "" || len(res.Paths["wifi.passphrase"]) != 2 {
		t.Fatalf("resolved: %v", res.Paths)
	}
	if !hasPrefix(res.Gaps, "optical: no temp leaf") || !hasPrefix(res.Gaps, "optical: no bias leaf") {
		t.Fatalf("the missing levels should be gaps: %v", res.Gaps)
	}
	// the metrics read the resolved paths: the vendor object is found, and its levels are read
	var m store.ONUMetrics
	get := func() store.ONUMetrics { f.st.View(func(d *store.Data) { m = d.ONUMetricsOf(o.ID).Clone() }); return m }
	eventually(t, "a sample from the tree paths", func() bool { return get().Paths.Tree != "" && get().Optical != nil && get().Optical.Rx != nil })
	m = get()
	if *m.Optical.Rx != -19.25 || m.Optical.Tx == nil || *m.Optical.Tx != 3.1 || m.Paths.Optical != c.root+"WANDevice.1.X_FOO.Optics." {
		t.Fatalf("optical from the tree: %+v paths=%+v", m.Optical, m.Paths)
	}
	if m.WAN == nil || m.WAN.Conn != "Connected" || m.WAN.RxBytes == nil || len(m.Hosts) != 2 {
		t.Fatalf("wan/hosts from the tree: %+v hosts=%d", m.WAN, len(m.Hosts))
	}
	if !hasPrefix(m.Gaps, "optical: no temp leaf") {
		t.Fatalf("gaps are the tree's: %v", m.Gaps)
	}
	if v, _ := f.svc.Metrics(o.ID, time.Hour); v.QoS.ConnStatus != "Connected" {
		t.Fatalf("connection status in the view: %+v", v.QoS)
	}
	// the dump: secret leaves are redacted whatever the row holds
	v, err := f.svc.ParamTree(o.ID)
	if err != nil || v.Count != tr.Count || v.Serial != c.Serial {
		t.Fatalf("view: %v %+v", err, v)
	}
	redacted := 0
	for _, p := range v.Params {
		if strings.Contains(p.Path, "KeyPassphrase") {
			if !p.Redacted || p.Value != RedactedValue {
				t.Fatalf("%s: %+v", p.Path, p)
			}
			redacted++
		}
	}
	if redacted != 2 {
		t.Fatalf("redacted leaves: %d", redacted)
	}
	txt := TreeText(v)
	if strings.Contains(txt, "factorypass") || !strings.Contains(txt, "KeyPassphrase\trw\t<redacted>") {
		t.Fatalf("text dump:\n%.900s", txt)
	}
	if b, ok := f.svc.TreeInfo(o.ID); !ok || b.Count != tr.Count || len(b.Resolved) == 0 {
		t.Fatalf("brief: %+v", b)
	}
}

func TestWalkRunsOnFirstReportVersionChangeAndFactoryResetOnly(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	c.maxReq = 1000
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	first := waitTree(t, f, o.ID)
	settle(f, c)
	gpn := c.count("GetParameterNames")
	f.advance(5 * time.Minute)
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if again, _ := treeOf(f, o.ID); !again.WalkedAt.Equal(first.WalkedAt) {
		t.Fatalf("an ordinary check-in walked the tree again")
	}
	if n := c.count("GetParameterNames"); n != gpn {
		t.Fatalf("an ordinary check-in listed parameters: %d -> %d", gpn, n)
	}
	// a new software version: walked again
	c.set(c.root+"DeviceInfo.SoftwareVersion", "V1.3.0")
	f.advance(5 * time.Minute)
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a walk after the version change", func() bool { x, _ := treeOf(f, o.ID); return x.Reason == WalkVersion })
	second, _ := treeOf(f, o.ID)
	if second.SWVersion != "V1.3.0" || !second.WalkedAt.After(first.WalkedAt) {
		t.Fatalf("second: %+v", second)
	}
	// a factory reset (0 BOOTSTRAP): walked again
	f.advance(5 * time.Minute)
	if err := c.session("0 BOOTSTRAP"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a walk after the reset", func() bool { x, _ := treeOf(f, o.ID); return x.Reason == WalkReset })
	// a plain boot is not a reset
	third, _ := treeOf(f, o.ID)
	f.advance(5 * time.Minute)
	if err := c.session("1 BOOT"); err != nil {
		t.Fatal(err)
	}
	if x, _ := treeOf(f, o.ID); !x.WalkedAt.Equal(third.WalkedAt) {
		t.Fatalf("a plain boot walked the tree")
	}
}

func TestWalkIsBounded(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	c.maxReq = 3000
	r := c.root
	for _, obj := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"} { // ten wide objects: more than the tree may hold
		for i := 0; i < 1400; i++ {
			c.set(fmt.Sprintf("%sX_Wide%s.P%d", r, obj, i), "1")
		}
	}
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	tr := waitTree(t, f, o.ID)
	if tr.Count > store.MaxTreeParams || !tr.Truncated {
		t.Fatalf("count %d truncated=%v", tr.Count, tr.Truncated)
	}
	perObject := map[string]int{}
	for _, p := range tr.Params() {
		perObject[parentOf(p.Path)]++
	}
	for obj, n := range perObject {
		if n > maxLeavesPerObject {
			t.Fatalf("%s has %d parameters", obj, n)
		}
	}
}

func TestWalkReadsAFewInstancesOfATable(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	c.maxReq = 1000
	r := c.root
	for i := 1; i <= 30; i++ {
		c.set(fmt.Sprintf("%sX_Table.Row.%d.Value", r, i), "x")
	}
	setupLinked(t, c)
	tr := waitTree(t, f, f.onuBySerial(c.Serial).ID)
	rows := map[string]bool{}
	for _, p := range tr.Params() {
		if strings.Contains(p.Path, "X_Table.Row.") {
			rows[strings.Split(strings.TrimPrefix(p.Path, r+"X_Table.Row."), ".")[0]] = true
		}
	}
	if len(rows) != walkInstances {
		t.Fatalf("rows walked: %d (%v)", len(rows), rows)
	}
}

func TestWalksWaitForAFreeSlot(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	c.maxReq = 1000
	for i := 0; i < maxConcurrentWalks; i++ { // every slot is taken by other boxes
		f.svc.walkSlots <- struct{}{}
	}
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	settle(f, c)
	if _, ok := treeOf(f, o.ID); ok {
		t.Fatal("walked without a free slot")
	}
	for i := 0; i < maxConcurrentWalks; i++ {
		<-f.svc.walkSlots
	}
	f.advance(5 * time.Minute)
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	waitTree(t, f, o.ID)
	if len(f.svc.walkSlots) != 0 {
		t.Fatalf("a slot was not given back")
	}
}

func TestBoxThatCannotListParametersIsNotWalkedAgainForAnHour(t *testing.T) {
	f := newFixture(t)
	c := vsolMetricsBox(t, f)
	c.noGPN = true
	c.maxReq = 1000
	setupLinked(t, c)
	o := f.onuBySerial(c.Serial)
	settle(f, c)
	if _, ok := treeOf(f, o.ID); ok {
		t.Fatal("a tree from a box that lists nothing")
	}
	if o = f.onuBySerial(c.Serial); o.State.WalkTry.IsZero() {
		t.Fatal("the attempt was not remembered")
	}
	n := c.count("GetParameterNames")
	f.advance(5 * time.Minute)
	if err := c.session("2 PERIODIC"); err != nil {
		t.Fatal(err)
	}
	if got := c.count("GetParameterNames"); got != n {
		t.Fatalf("tried again after 5 minutes: %d -> %d", n, got)
	}
	// the metrics still work by the standard probe
	eventually(t, "a sample without a tree", func() bool {
		var ok bool
		f.st.View(func(d *store.Data) { ok = !d.ONUMetricsOf(o.ID).Updated.IsZero() })
		return ok
	})
}

func TestMarkWalksIsRetroactiveAndIdempotent(t *testing.T) {
	f := newFixture(t)
	now := f.now
	f.st.Update(func(d *store.Data) error {
		d.ONUs["o1"] = store.ONU{ID: "o1", Serial: "S1", Root: store.RootTR098, SiteID: "s1", LastInform: now}
		d.ONUs["o2"] = store.ONU{ID: "o2", Serial: "S2", Root: store.RootTR098, SiteID: "s1", LastInform: now}
		d.ONUs["o3"] = store.ONU{ID: "o3", Serial: "S3", SiteID: "s1"} // never reported
		d.ONUTrees["o2"] = store.ONUTree{ID: "o2", WalkedAt: now}
		return nil
	})
	if n := f.svc.MarkWalks(); n != 1 {
		t.Fatalf("marked %d", n)
	}
	if n := f.svc.MarkWalks(); n != 0 {
		t.Fatalf("marked again: %d", n)
	}
	f.st.View(func(d *store.Data) {
		if !d.ONUs["o1"].State.WalkWanted || d.ONUs["o2"].State.WalkWanted || d.ONUs["o3"].State.WalkWanted {
			t.Fatalf("marks: %v %v %v", d.ONUs["o1"].State.WalkWanted, d.ONUs["o2"].State.WalkWanted, d.ONUs["o3"].State.WalkWanted)
		}
	})
}

func TestParamTreeViewRedactsSecretNamesWhateverTheRowHolds(t *testing.T) {
	f := newFixture(t)
	f.st.Update(func(d *store.Data) error {
		d.ONUs["o1"] = store.ONU{ID: "o1", Serial: "S1", Root: store.RootTR098, SiteID: "s1"}
		// a row written by something that did not flag the secret (an older build, a hand edit)
		d.ONUTrees["o1"] = store.ONUTree{ID: "o1", WalkedAt: f.now, Root: store.RootTR098, Count: 2,
			Text: "InternetGatewayDevice.ManagementServer.Password\t-\thunter2\nInternetGatewayDevice.DeviceInfo.UpTime\t-\t99\n"}
		return nil
	})
	v, err := f.svc.ParamTree("o1")
	if err != nil || len(v.Params) != 2 || v.Params[0].Value != RedactedValue || !v.Params[0].Redacted || v.Params[1].Value != "99" {
		t.Fatalf("%v %+v", err, v.Params)
	}
	if strings.Contains(TreeText(v), "hunter2") {
		t.Fatal("secret in the text dump")
	}
	if _, err := f.svc.ParamTree("nope"); err != ErrONUGone {
		t.Fatalf("%v", err)
	}
	f.st.Update(func(d *store.Data) error { d.ONUs["o2"] = store.ONU{ID: "o2"}; return nil })
	if _, err := f.svc.ParamTree("o2"); err != ErrNoTree {
		t.Fatalf("%v", err)
	}
}
