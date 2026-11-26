package web_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func seedONUMetrics(t *testing.T, a *app, id string, now time.Time) {
	t.Helper()
	rx, tx, tp := -21.5, 2.5, 41.0
	dn, up, line := 12.5e6, 1.5e6, 1e9
	var rxb, txb uint64 = 5 << 30, 1 << 20
	var fine []store.MetricPoint
	for i := 0; i < 60; i++ {
		r, d := rx-float64(i)*0.01, dn
		fine = append(fine, store.MetricPoint{T: now.Add(-time.Duration(60-i) * 5 * time.Minute).Unix(), N: 1, Rx: &r, Tx: &tx, Temp: &tp, Dn: &d, Up: &up})
	}
	a.st.Update(func(d *store.Data) error {
		d.ONUMetrics[id] = store.ONUMetrics{ID: id, Updated: now, Uptime: 90000, Reboots: 2, LastBoot: now.Add(-25 * time.Hour),
			Optical: &store.ONUOptical{Rx: &rx, Tx: &tx, Temp: &tp, Path: "InternetGatewayDevice.X_GponInterafceConfig."},
			WAN:     &store.ONUWAN{Dn: &dn, Up: &up, LineDn: &line, Link: "Up", Counters: map[string]uint64{"errors_rx": 7}, Deltas: map[string]uint64{"errors_rx": 1}},
			Hosts: []store.ONUHost{
				{Name: `<script>alert(1)</script>`, MAC: "AA:BB:CC:00:00:01", IP: "192.168.1.10", Iface: `"><img src=x onerror=alert(2)>`, Active: true, Dn: &dn, Up: &up, RxBytes: &rxb, TxBytes: &txb},
				{Name: "tv", MAC: "AA:BB:CC:00:00:02", IP: "192.168.1.11", Iface: "LAN", Active: true, Dn: &up},
			},
			Gaps: []string{"bias: not found"}, Fine: fine}
		return nil
	})
}

func TestONUMetricsDashboardRendersChartsTableAndEscapes(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onu1", "ZTEGC0001", "", "AA:BB:CC:DD:EE:01")
	seedONUMetrics(t, a, "onu1", time.Now())
	page := a.adminGet(t, "/admin/onus/metrics?onu=onu1&range=6h")
	for _, want := range []string{"Speed over time", "Optical levels", "Devices behind the ONU", "Line and errors", "<svg", "<path ", `class="cband"`, "dBm", "Mbit/s",
		"errors_rx", "bias: not found", "tv", "192.168.1.11", "1.0 Gbit/s"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	for _, gone := range []string{"Numbers behind", "Module temperature", "Module voltage", "Laser bias current", "The same data as JSON", "/api/v1/onus/"} {
		if strings.Contains(page, gone) {
			t.Fatalf("page still has removed element %q", gone)
		}
	}
	for _, kept := range []string{"Receive power (Rx)", "Transmit power (Tx)", "Speed over time"} {
		if !strings.Contains(page, kept) {
			t.Fatalf("page lost %q", kept)
		}
	}
	// the devices are one compact table: a header row with every column and one row per host, scrollable (.tw), not stacked
	dev := page[strings.Index(page, "Devices behind the ONU"):strings.Index(page, "Line and errors")]
	for _, h := range []string{">Name</a></th>", ">MAC</a></th>", ">IP</a></th>", ">Connected by</a></th>", "<th>State</th>", ">Down</a></th>", ">Up</a></th>", ">Received</th>", ">Sent</th>"} {
		if !strings.Contains(dev, h) {
			t.Fatalf("devices header lacks %q", h)
		}
	}
	if strings.Count(dev, "<table") != 1 || !strings.Contains(dev, `<div class="tw"><table>`) || strings.Count(dev, "<tr>") != 3 || strings.Contains(dev, "data-label") || strings.Contains(dev, `class="rs"`) {
		t.Fatalf("devices are not one compact table with a header and two rows: %s", dev)
	}
	if strings.Contains(page, "<script>alert(1)") || strings.Contains(page, "<img src=x") || !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("a host string was not escaped")
	}
	if strings.Contains(page, "onclick=") || strings.Contains(page, "<script>") {
		t.Fatal("inline script on the page")
	}
	// the sortable table: by up, the 'tv' row (1.5 Mbit/s as Dn... both up equal) just must still render; by name sorts too
	for _, s := range []string{"name", "ip", "mac", "link", "down", "up", "bogus"} {
		if p := a.adminGet(t, "/admin/onus/metrics?onu=onu1&sort="+s); !strings.Contains(p, "192.168.1.10") {
			t.Fatalf("sort %s lost a row", s)
		}
	}
}

func TestONUMetricsDashboardWithNoDataRendersNoDataNotACrash(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onu2", "ZTEGC0002", "", "AA:BB:CC:DD:EE:02")
	page := a.adminGet(t, "/admin/onus/metrics?onu=onu2&range=7d")
	for _, want := range []string{"No throughput readings", "No Rx power readings", "The ONU has not given a host table", "not read yet"} {
		if !strings.Contains(page, want) {
			t.Fatalf("empty page lacks %q", want)
		}
	}
	if strings.Contains(page, `class="chart"`) {
		t.Fatal("a chart was drawn without data")
	}
}

func TestONUMetricsDashboardIsOperatorOnlyAuditedAndValidatesInput(t *testing.T) {
	a := newONUApp(t)
	a.seedONU(t, "onu1", "ZTEGC0001", "", "AA:BB:CC:DD:EE:01")
	seedONUMetrics(t, a, "onu1", time.Now())
	viewer := seedUser(t, a.srv, a.st, "vera", store.RoleViewer, longPW)
	vb := signIn(t, a.ts, a.srv, a.st, viewer)
	resp := vb.Do(a.cl, a.ts.URL, "GET", "/admin/onus/metrics?onu=onu1", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("a viewer saw the metrics: %d", resp.StatusCode)
	}
	if c := a.admin("GET", "/admin/onus/metrics?onu=nope", nil).StatusCode; c != 404 {
		t.Fatalf("unknown ONU: %d", c)
	}
	if c := a.admin("GET", "/admin/onus/metrics?onu=onu1&range="+url.QueryEscape("999d"), nil).StatusCode; c != 400 {
		t.Fatalf("bad range: %d", c)
	}
	r := a.admin("GET", "/admin/onus/metrics?onu=onu1", nil)
	body(t, r)
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	var audited bool
	a.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.Kind == "audit" && strings.Contains(e.Msg, "ONU ZTEGC0001 metrics viewed") {
				audited = true
			}
		}
	})
	if !audited {
		t.Fatal("the view was not audited")
	}
}

func TestONUCardLinksTheMetricsDashboard(t *testing.T) {
	a := newONUApp(t)
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"Flat 3"}, "mac": {mac1}})
	acct := a.account(t, "Flat 3")
	a.seedONU(t, "onu3", "ZTEGC0003", acct.ID, "AA:BB:CC:DD:EE:03")
	if !strings.Contains(a.adminGet(t, "/admin/accounts/view?code="+acct.Code), "/admin/onus/metrics?onu=onu3") {
		t.Fatal("the card has no link to the metrics")
	}
}
