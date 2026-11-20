package store

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const v2Fixture = `{
 "format": 2,
 "packages": {"p1": {"id":"p1","name":"1 Day","slug":"1-day","duration_min":1440,"speed_mbps":10,"price_kes":50,"sort":10}},
 "accounts": {"a1": {"id":"a1","code":"ABC234","flat":"Flat 1","mac":"AA:BB:CC:DD:EE:01","hs_pass":"x","created_at":"2026-01-01T00:00:00Z"}},
 "orders": {"o1": {"id":"o1","account_id":"a1","package_id":"p1","method":"test","amount_kes":50,"status":"paid","logged_in":false,"created_at":"2026-01-02T00:00:00Z"}},
 "vouchers": {"V1": {"code":"V1","package_id":"p1","batch":"b","used":false,"created_at":"2026-01-01T00:00:00Z"}},
 "payments": {"K1": {"key":"K1","method":"mpesa-c2b","status":"applied","amount_kes":50,"account_id":"a1","paid_at":"2026-01-02T00:00:00Z","created_at":"2026-01-02T00:00:00Z"}},
 "devices": {"AA:BB:CC:DD:EE:02": {"mac":"AA:BB:CC:DD:EE:02","source":"portal","first_seen":"2026-01-03T00:00:00Z","last_seen":"2026-01-03T00:00:00Z","hits":2}},
 "tasks": {"t1": {"id":"t1","account_id":"a1","code":"ABC234","rev":1,"attempts":0,"next_try":"2026-01-03T00:00:00Z","created_at":"2026-01-03T00:00:00Z"}},
 "events": [{"time":"2026-01-03T00:00:00Z","kind":"info","msg":"hello"}]
}`

func TestMigrateV2ToV3(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ispbill.json")
	if err := os.WriteFile(p, []byte(v2Fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var siteID string
	s.View(func(d *Data) {
		if d.Format != FormatVersion || len(d.Sites) != 1 {
			t.Fatalf("format %d sites %d", d.Format, len(d.Sites))
		}
		for id, st := range d.Sites {
			siteID = id
			if st.Name != "Main building" || st.Mode != SiteModeEnv || len(id) != 7 || id[0] != 's' {
				t.Fatalf("bad site %+v", st)
			}
		}
		if d.Accounts["a1"].SiteID != siteID || d.Orders["o1"].SiteID != siteID || d.Payments["K1"].SiteID != siteID ||
			d.Tasks["t1"].SiteID != siteID || d.Events[0].SiteID != siteID {
			t.Fatal("rows not stamped with the site id")
		}
		if d.Vouchers["V1"].SiteID != "" {
			t.Fatal("vouchers must stay valid at any site")
		}
		if len(d.Devices) != 1 {
			t.Fatal("devices lost")
		}
		dev, ok := d.Devices[DeviceKey(siteID, "AA:BB:CC:DD:EE:02")]
		if !ok || dev.SiteID != siteID || dev.Hits != 2 {
			t.Fatalf("device not re-keyed: %+v", d.Devices)
		}
		if a, ok := d.AccountByMAC("AA:BB:CC:DD:EE:01"); !ok || a.SiteID != siteID {
			t.Fatal("AccountByMAC failed")
		}
		if _, ok := d.AccountByMAC("AA:BB:CC:DD:EE:FE"); ok {
			t.Fatal("unknown MAC must not match")
		}
	})
	// The original is backed up byte for byte, mode 0600.
	bak := p + ".v2.bak"
	b, err := os.ReadFile(bak)
	if err != nil || string(b) != v2Fixture {
		t.Fatalf("backup wrong: %v", err)
	}
	if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	// The saved file is format 4 and loads as is (no second migration, same site).
	var probe struct{ Format int }
	raw, _ := os.ReadFile(p)
	json.Unmarshal(raw, &probe)
	if probe.Format != FormatVersion {
		t.Fatalf("saved format %d", probe.Format)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.View(func(d *Data) {
		if _, ok := d.Sites[siteID]; !ok || len(d.Sites) != 1 {
			t.Fatal("site changed on reopen")
		}
	})
}

func TestMigrateV2EmptyCreatesNoSite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ispbill.json"), []byte(`{"format":2,"packages":{}}`), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(d *Data) {
		if d.Format != FormatVersion || len(d.Sites) != 0 {
			t.Fatalf("%d %d", d.Format, len(d.Sites))
		}
	})
}

func TestV3RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	err := s.Update(func(d *Data) error {
		d.Sites["s111111"] = Site{ID: "s111111", Name: "Block A", Mode: SiteModeWG, TunnelIP: "10.99.0.2", RESTPass: "pw", RESTPort: 8081,
			PkgOverrides: map[string]PkgOverride{"p1": {Hidden: true, PriceKES: 70}}, Join: JoinToken{Hash: "h", ExpiresAt: now}, CreatedAt: now}
		d.Accounts["a"] = Account{ID: "a", SiteID: "s111111", WhatsApp: "254711000000", WAOptOut: true}
		d.Devices[DeviceKey("s111111", "AA:BB:CC:DD:EE:01")] = Device{SiteID: "s111111", MAC: "AA:BB:CC:DD:EE:01"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.View(func(d *Data) {
		st := d.Sites["s111111"]
		if st.TunnelIP != "10.99.0.2" || st.RESTPass != "pw" || st.PkgOverrides["p1"].PriceKES != 70 || st.Join.Hash != "h" || !st.Join.ExpiresAt.Equal(now) {
			t.Fatalf("site not round-tripped: %+v", st)
		}
		if a := d.Accounts["a"]; a.WhatsApp != "254711000000" || !a.WAOptOut || a.SiteID != "s111111" {
			t.Fatalf("account %+v", a)
		}
	})
	if _, err := os.Stat(filepath.Join(dir, "ispbill.json.v2.bak")); err == nil {
		t.Fatal("a v4 file must not be backed up as v2")
	}
}

func TestUnknownFormatRefused(t *testing.T) {
	for _, f := range []string{"1", "9", "0"} {
		dir := t.TempDir()
		p := filepath.Join(dir, "ispbill.json")
		raw := []byte(`{"format":` + f + `}`)
		os.WriteFile(p, raw, 0o600)
		if _, err := Open(dir); !errors.Is(err, ErrOldFormat) {
			t.Fatalf("format %s: %v", f, err)
		}
		if b, _ := os.ReadFile(p); string(b) != string(raw) {
			t.Fatal("file touched")
		}
	}
}

func TestNextTunnelIP(t *testing.T) {
	pre := netip.MustParsePrefix("10.99.0.0/22")
	d := &Data{}
	d.init()
	ip, err := d.NextTunnelIP(pre)
	if err != nil || ip != "10.99.0.2" {
		t.Fatalf("%q %v", ip, err)
	}
	d.Sites["a"] = Site{ID: "a", TunnelIP: "10.99.0.2"}
	d.Sites["b"] = Site{ID: "b", TunnelIP: "10.99.0.3"}
	if ip, _ = d.NextTunnelIP(pre); ip != "10.99.0.4" {
		t.Fatalf("got %s", ip)
	}
	// A removed site frees its address; gaps are reused (lowest free).
	b := d.Sites["a"]
	b.Removed = true
	d.Sites["a"] = b
	if ip, _ = d.NextTunnelIP(pre); ip != "10.99.0.2" {
		t.Fatalf("got %s", ip)
	}
	// Exhausted: a /30 has hosts .1 (server) and .2 only.
	small := netip.MustParsePrefix("10.99.0.0/30")
	d2 := &Data{}
	d2.init()
	if ip, err = d2.NextTunnelIP(small); err != nil || ip != "10.99.0.2" {
		t.Fatalf("%s %v", ip, err)
	}
	d2.Sites["x"] = Site{ID: "x", TunnelIP: "10.99.0.2"}
	if _, err = d2.NextTunnelIP(small); err == nil {
		t.Fatal("expected exhaustion")
	}
}

func TestEffectivePackages(t *testing.T) {
	d := &Data{}
	d.init()
	d.Packages["p1"] = Package{ID: "p1", Name: "Day", PriceKES: 50, Sort: 1}
	d.Packages["p2"] = Package{ID: "p2", Name: "Week", PriceKES: 250, Sort: 2}
	d.Packages["p3"] = Package{ID: "p3", Name: "Gone", PriceKES: 1, Deleted: true}
	d.Sites["s1"] = Site{ID: "s1", PkgOverrides: map[string]PkgOverride{"p1": {PriceKES: 60}, "p2": {Hidden: true}}}
	got := d.EffectivePackages("s1")
	if len(got) != 2 || got[0].PriceKES != 60 || got[0].Disabled || !got[1].Disabled || got[1].PriceKES != 250 {
		t.Fatalf("%+v", got)
	}
	if d.Packages["p1"].PriceKES != 50 {
		t.Fatal("shared list modified")
	}
	if p, _ := d.EffectivePackage("other", "p1"); p.PriceKES != 50 {
		t.Fatal("unrelated site must see shared price")
	}
}

func TestSiteHelpers(t *testing.T) {
	d := &Data{}
	d.init()
	d.Sites["a"] = Site{ID: "a", Name: "Block 10", TunnelIP: "10.99.0.2"}
	d.Sites["b"] = Site{ID: "b", Name: "Block 2", TunnelIP: "10.99.0.3"}
	d.Sites["c"] = Site{ID: "c", Name: "Old", TunnelIP: "10.99.0.4", Removed: true}
	l := d.SiteList()
	if len(l) != 2 || l[0].ID != "b" || l[1].ID != "a" {
		t.Fatalf("natural sort %v", l)
	}
	if s, ok := d.SiteByTunnelIP("10.99.0.3"); !ok || s.ID != "b" {
		t.Fatal("by tunnel ip")
	}
	if _, ok := d.SiteByTunnelIP("10.99.0.4"); ok {
		t.Fatal("removed site matched")
	}
}

// A v2 file with devices but no accounts or payments used to keep the devices
// with SiteID "", which Validate (and so a Postgres import) refused.
func TestMigrateV2OrphanDevicesDropped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ispbill.json")
	raw := `{"format":2,"devices":{"AA:BB:CC:DD:EE:02":{"mac":"AA:BB:CC:DD:EE:02","source":"portal","hits":2}}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	d, from, err := ReadJSON(p)
	if err != nil || from != 2 {
		t.Fatalf("ReadJSON: from %d err %v", from, err)
	}
	if len(d.Devices) != 0 || len(d.Sites) != 0 {
		t.Fatalf("devices %d sites %d", len(d.Devices), len(d.Sites))
	}
	if problems := Validate(d); len(problems) != 0 {
		t.Fatalf("Validate: %v", problems)
	}
}

// A format 4 file (from before settings existed) loads, is backed up as
// .v4.bak, is rewritten as the current format and keeps every row.
func TestMigrateV4ToV5(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ispbill.json")
	v4 := `{"format":4,"sites":{"s1":{"id":"s1","name":"A","mode":"env"}},"packages":{},"accounts":{},"orders":{},"vouchers":{},"payments":{},"devices":{},"tasks":{},"events":[],"event_seq":0}`
	os.WriteFile(p, []byte(v4), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(d *Data) {
		if d.Format != FormatVersion || len(d.Sites) != 1 || d.Settings == nil || len(d.Settings) != 0 {
			t.Fatalf("format %d sites %d settings %v", d.Format, len(d.Sites), d.Settings)
		}
	})
	if b, err := os.ReadFile(p + ".v4.bak"); err != nil || string(b) != v4 {
		t.Fatalf("v4 backup: %v", err)
	}
	if err := s.Update(func(d *Data) error {
		d.Settings["admin.pass_hash"] = Setting{Key: "admin.pass_hash", Value: "h"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(dir)
	s2.View(func(d *Data) {
		if d.Settings["admin.pass_hash"].Value != "h" {
			t.Fatal("setting lost over a reopen")
		}
	})
	// An older binary (format 4 only) must refuse, not drop the hash: the
	// file now says 5, and decodeJSON of an older build rejects unknown formats.
	var probe struct{ Format int }
	raw, _ := os.ReadFile(p)
	json.Unmarshal(raw, &probe)
	if probe.Format != FormatVersion {
		t.Fatalf("saved format %d", probe.Format)
	}
}

func TestMigrateV6ToV7MakesRoutersIPoEAndPurgesJobSettings(t *testing.T) {
	dir := t.TempDir()
	raw := `{"format":6,"sites":{"s1":{"id":"s1","name":"A","mode":"wg","access_mode":"hotspot"},"s2":{"id":"s2","name":"B","mode":"wg"}},
	"settings":{"site.s1.prov_job":{"key":"site.s1.prov_job","value":"x"},"site.s1.serving_customers":{"key":"site.s1.serving_customers","value":"yes"},
	"site.s1.join_report":{"key":"site.s1.join_report","value":"keep"},"site.s2.hotspot_bypass_at":{"key":"site.s2.hotspot_bypass_at","value":"x"},
	"site.s2.cutover_confirmed_at":{"key":"site.s2.cutover_confirmed_at","value":"x"},"other":{"key":"other","value":"keep"}}}`
	os.WriteFile(filepath.Join(dir, "ispbill.json"), []byte(raw), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(d *Data) {
		if d.Format != FormatVersion || d.Sites["s1"].AccessMode != AccessIPoE || d.Sites["s2"].AccessMode != AccessIPoE {
			t.Fatalf("format %d, modes %q %q", d.Format, d.Sites["s1"].AccessMode, d.Sites["s2"].AccessMode)
		}
		if len(d.Settings) != 2 || d.Settings["site.s1.join_report"].Value != "keep" || d.Settings["other"].Value != "keep" {
			t.Fatalf("settings %v", d.Settings)
		}
	})
}

func TestMigrateV7ToV8MovesBenchPortIntoCustomerPorts(t *testing.T) {
	dir := t.TempDir()
	raw := `{"format":7,"sites":{
	"s1":{"id":"s1","name":"A","mode":"wg","bench_port":"ether5"},
	"s2":{"id":"s2","name":"B","mode":"wg","bench_port":"ether5","customer_ports":"ether4,ether5"},
	"s3":{"id":"s3","name":"C","mode":"wg"}}}`
	p := filepath.Join(dir, "ispbill.json")
	os.WriteFile(p, []byte(raw), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"s1": "ether5", "s2": "ether4,ether5", "s3": ""}
	s.View(func(d *Data) {
		for id, w := range want {
			if got := d.Sites[id]; got.CustomerPorts != w || got.BenchPort != "" {
				t.Fatalf("%s: customer_ports %q bench %q, want %q and none", id, got.CustomerPorts, got.BenchPort, w)
			}
		}
	})
	if b, err := os.ReadFile(p + ".v7.bak"); err != nil || string(b) != raw {
		t.Fatalf("v7 backup: %v", err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "bench_port") || !strings.Contains(string(b), `"format": 8`) {
		t.Fatalf("saved file still has bench_port or the old format: %s", b)
	}
}
