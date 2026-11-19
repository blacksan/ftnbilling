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
	for _, f := range []string{"1", "10", "0"} {
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
	if strings.Contains(string(b), "bench_port") || !strings.Contains(string(b), `"format": 9`) {
		t.Fatalf("saved file still has bench_port or the old format: %s", b)
	}
}

// v7Apartments is a format 7 file with the cases migration 0008 has to get right:
// two routers, a unit label used twice on one router, a closed account, and
// router contacts of several roles (one section holds the first, the rest go
// into its notes).
const v7Apartments = `{"format":7,
"sites":{"s1":{"id":"s1","name":"Riverside","address":"Ngong Rd","mode":"wg","bench_port":"ether5","created_at":"2026-01-01T00:00:00Z"},
         "s2":{"id":"s2","name":"Hilltop","mode":"wg","created_at":"2026-01-02T00:00:00Z"}},
"accounts":{
 "a1":{"id":"a1","site_id":"s1","code":"AAA111","flat":"Flat 1","mac":"AA:BB:CC:DD:EE:01","created_at":"2026-01-03T00:00:00Z"},
 "a2":{"id":"a2","site_id":"s1","code":"BBB222","flat":"B4","mac":"AA:BB:CC:DD:EE:02","created_at":"2026-01-04T00:00:00Z"},
 "a3":{"id":"a3","site_id":"s2","code":"CCC333","flat":"B4","mac":"AA:BB:CC:DD:EE:03","created_at":"2026-01-04T00:00:00Z"},
 "a4":{"id":"a4","site_id":"s1","code":"DDD444","flat":"flat  1","mac":"AA:BB:CC:DD:EE:04","created_at":"2026-01-05T00:00:00Z"}},
"closed_accounts":{"a5":{"id":"a5","site_id":"s1","code":"EEE555","flat":"Flat 1","mac":"AA:BB:CC:DD:EE:05","created_at":"2026-01-01T00:00:00Z","closed_at":"2026-02-01T00:00:00Z"}},
"contacts":{
 "c1":{"id":"c1","site_id":"s1","role":"caretaker","name":"Otieno","phone":"0722000111","email":"o@x.org","notes":"gate","created_at":"2026-01-01T00:00:00Z"},
 "c2":{"id":"c2","site_id":"s1","role":"landlord","name":"Kamau","phone":"0711222333","created_at":"2026-01-02T00:00:00Z"},
 "c3":{"id":"c3","site_id":"s1","role":"security","name":"Guard","phone":"0700000001","created_at":"2026-01-03T00:00:00Z"},
 "c4":{"id":"c4","site_id":"s1","role":"agent","name":"Wambui","phone":"0733444555","created_at":"2026-01-04T00:00:00Z"},
 "c5":{"id":"c5","site_id":"s2","role":"security","name":"Night guard","phone":"0700000009","notes":"after 8pm","created_at":"2026-01-01T00:00:00Z"}}}`

// MigrationV7Expect is what both backends must give for the fixture above; the
// Postgres test in pgstore uses the same numbers.
func TestMigrateV7ToV9Apartments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ispbill.json")
	if err := os.WriteFile(p, []byte(v7Apartments), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(d *Data) {
		if d.Format != FormatVersion || len(d.Apartments) != 2 || len(d.ApartmentSites) != 2 || len(d.LegacyContacts) != 0 {
			t.Fatalf("format %d, %d apartments, %d links, %d legacy contacts", d.Format, len(d.Apartments), len(d.ApartmentSites), len(d.LegacyContacts))
		}
		a1, a2 := d.Apartments[MigratedApartmentID("s1")], d.Apartments[MigratedApartmentID("s2")]
		if s1 := d.Sites["s1"]; s1.CustomerPorts != "ether5" || s1.BenchPort != "" {
			t.Fatalf("the v7 to v8 step (ports) did not run before apartments: %+v", s1)
		}
		if a1.Name != "Riverside" || a1.Address != "Ngong Rd" || a2.Name != "Hilltop" {
			t.Fatalf("apartments %+v %+v", a1, a2)
		}
		if _, ok := d.ApartmentSites[ApartmentSiteKey(a1.ID, "s1")]; !ok {
			t.Fatal("apartment not linked to its router")
		}
		want := ApartmentContact{Name: "Otieno", Phone: "+254722000111", Email: "o@x.org", Notes: "gate | Also security: Guard, 0700000001"}
		if a1.Caretaker != want {
			t.Errorf("caretaker %+v", a1.Caretaker)
		}
		if w := (ApartmentContact{Name: "Kamau", Phone: "+254711222333", Notes: "Also agent: Wambui, 0733444555"}); a1.Landlord != w {
			t.Errorf("landlord %+v", a1.Landlord)
		}
		if w := (ApartmentContact{Name: "Night guard", Phone: "+254700000009", Notes: "Role: security | after 8pm"}); a2.Caretaker != w || !a2.Landlord.Empty() {
			t.Errorf("hilltop %+v / %+v", a2.Caretaker, a2.Landlord)
		}
		units := map[string]string{"a1": "Flat 1", "a2": "B4", "a3": "B4", "a4": "flat  1 (DDD444)"}
		for id, u := range units {
			a := d.Accounts[id]
			apt := MigratedApartmentID(a.SiteID)
			if a.ApartmentID != apt || a.Unit != u || a.Flat != u {
				t.Errorf("%s: apartment %q unit %q flat %q", id, a.ApartmentID, a.Unit, a.Flat)
			}
		}
		if c := d.ClosedAccounts["a5"]; c.ApartmentID != MigratedApartmentID("s1") || c.Unit != "Flat 1" {
			t.Errorf("closed account %+v", c)
		}
		audited := false
		for _, e := range d.Events {
			audited = audited || (e.AccountID == "a4" && e.Kind == "audit")
		}
		if !audited {
			t.Error("the unit conflict left no audit event")
		}
	})
	// Saved as format 8, the contacts key is gone, and reopening changes nothing.
	s.Close()
	raw, _ := os.ReadFile(p)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["contacts"]; ok || string(top["format"]) != "9" {
		t.Errorf("file keeps contacts or the old format: format=%s contacts=%v", top["format"], ok)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.View(func(d *Data) {
		if len(d.Apartments) != 2 || d.Accounts["a4"].Unit != "flat  1 (DDD444)" {
			t.Errorf("second open changed the data")
		}
	})
}

// A format 8 store refuses a second open account with the same unit in one
// apartment (any case or spacing), like the one-MAC rule, and a duplicate apartment name.
func TestUnitAndApartmentNameInvariants(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	add := func(id, apt, unit string) error {
		return s.Update(func(d *Data) error {
			d.Accounts[id] = Account{ID: id, Code: id, SiteID: "s1", ApartmentID: apt, Unit: unit, Flat: unit, CreatedAt: now}
			return nil
		})
	}
	if err := s.Update(func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", Name: "R", CreatedAt: now}
		d.Apartments["p1"] = Apartment{ID: "p1", Name: "Block A", CreatedAt: now}
		d.Apartments["p2"] = Apartment{ID: "p2", Name: "Block B", CreatedAt: now}
		d.ApartmentSites[ApartmentSiteKey("p1", "s1")] = ApartmentSite{ApartmentID: "p1", SiteID: "s1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := add("x1", "p1", "B4"); err != nil {
		t.Fatal(err)
	}
	if err := add("x2", "p1", " b4 "); !errors.Is(err, ErrDuplicateUnit) {
		t.Fatalf("same unit in one apartment: %v", err)
	}
	if err := add("x3", "p2", "B4"); err != nil {
		t.Fatalf("the same unit in another apartment is fine: %v", err)
	}
	if err := s.Update(func(d *Data) error {
		d.Apartments["p3"] = Apartment{ID: "p3", Name: "block a", CreatedAt: now}
		return nil
	}); !errors.Is(err, ErrDuplicateApartment) {
		t.Fatalf("duplicate apartment name: %v", err)
	}
	if err := s.Update(func(d *Data) error {
		d.ApartmentSites[ApartmentSiteKey("p2", "nope")] = ApartmentSite{ApartmentID: "p2", SiteID: "nope"}
		return nil
	}); !errors.Is(err, ErrBadApartmentLink) {
		t.Fatalf("link to a missing router: %v", err)
	}
}
