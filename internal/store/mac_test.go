package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMACMovedToStore(t *testing.T) {
	for _, in := range []string{"aa:bb:cc:dd:ee:01", "AA-BB-CC-DD-EE-01", "aabb.ccdd.ee01", " aabbccddee01 "} {
		if got, err := NormalizeMAC(in); err != nil || got != "AA:BB:CC:DD:EE:01" {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "AA:BB", "01:00:5E:00:00:01", "00:00:00:00:00:00", "GG:BB:CC:DD:EE:01"} {
		if _, err := NormalizeMAC(in); !errors.Is(err, ErrBadMAC) {
			t.Errorf("%q must be refused: %v", in, err)
		}
	}
	// The separators are exactly : - . and the ASCII space (migration 0004 strips the same set).
	for in, want := range map[string]string{"AA::BB:CC:DD:EE:07": "AA:BB:CC:DD:EE:07", " AABBCCDDEE06": "AA:BB:CC:DD:EE:06", "aabb.ccdd.ee03": "AA:BB:CC:DD:EE:03"} {
		if m, ok := CanonMAC(in); !ok || m != want {
			t.Errorf("CanonMAC(%q) = %q %v", in, m, ok)
		}
	}
	for _, in := range []string{"AABBCCDDEE0A\t", "\u00a0AABBCCDDEE0B", "ＡＡＢＢＣＣＤＤＥＥ０１", "AA:BB:CC:DD:EE:01:02"} {
		if m, ok := CanonMAC(in); ok {
			t.Errorf("CanonMAC(%q) = %q, want refused", in, m)
		}
	}
	// CanonMAC only fixes the spelling.
	if m, ok := CanonMAC("01-00-5e-00-00-01"); !ok || m != "01:00:5E:00:00:01" {
		t.Errorf("CanonMAC: %q %v", m, ok)
	}
	if _, ok := CanonMAC("not a mac"); ok {
		t.Error("CanonMAC accepted garbage")
	}
}

// A format 5 file with the same MAC in two buildings, in three spellings and a
// malformed one: it loads, keeps every account, flags the extras and says so.
const v5Dupes = `{
 "format": 5,
 "sites": {"s1": {"id":"s1","name":"Block A","mode":"env","created_at":"2026-01-01T00:00:00Z"},
           "s2": {"id":"s2","name":"Block B","mode":"env","created_at":"2026-01-01T00:00:00Z"}},
 "packages": {"p1": {"id":"p1","name":"1 Day","slug":"1-day","duration_min":1440,"speed_mbps":10,"price_kes":50}},
 "accounts": {
  "x1": {"id":"x1","site_id":"s1","code":"AAA111","flat":"Flat 1","mac":"aa:bb:cc:dd:ee:01","hs_pass":"x","created_at":"2026-01-01T00:00:00Z"},
  "x2": {"id":"x2","site_id":"s2","code":"BBB222","flat":"Flat 1","mac":"AA-BB-CC-DD-EE-01","hs_pass":"x","package_id":"p1","expires_at":"2999-01-01T00:00:00Z","created_at":"2026-02-01T00:00:00Z"},
  "x3": {"id":"x3","site_id":"s2","code":"CCC333","flat":"Flat 2","mac":"AABBCCDDEE01","hs_pass":"x","created_at":"2026-03-01T00:00:00Z"},
  "y1": {"id":"y1","site_id":"s1","code":"DDD444","flat":"Flat 3","mac":"AA:BB:CC:DD:EE:02","hs_pass":"x","created_at":"2026-01-05T00:00:00Z"},
  "z1": {"id":"z1","site_id":"s1","code":"EEE555","flat":"Flat 4","mac":"not-a-mac","hs_pass":"x","created_at":"2026-01-06T00:00:00Z"},
  "z2": {"id":"z2","site_id":"s1","code":"FFF666","flat":"Flat 5","mac":"","hs_pass":"x","created_at":"2026-01-07T00:00:00Z"}
 },
 "closed_accounts": {
  "c1": {"id":"c1","site_id":"s1","code":"GGG777","flat":"Flat 9","mac":"AA:BB:CC:DD:EE:02","closed_at":"2026-02-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z"},
  "c2": {"id":"c2","site_id":"s1","code":"HHH888","flat":"Flat 8","mac":"garbage","closed_at":"2026-02-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z"}
 },
 "events": [{"seq":7,"time":"2026-01-03T00:00:00Z","kind":"info","msg":"hello"}],
 "event_seq": 7
}`

func TestMigrateV5ToV6FlagsDuplicateMACs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ispbill.json")
	os.WriteFile(p, []byte(v5Dupes), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.View(func(d *Data) {
		if d.Format != FormatVersion || len(d.Accounts) != 6 || len(d.ClosedAccounts) != 2 {
			t.Fatalf("format %d, %d open, %d closed", d.Format, len(d.Accounts), len(d.ClosedAccounts))
		}
		want := map[string]struct {
			mac      string
			conflict bool
		}{"x1": {"AA:BB:CC:DD:EE:01", true}, "x2": {"AA:BB:CC:DD:EE:01", false}, "x3": {"AA:BB:CC:DD:EE:01", true},
			"y1": {"AA:BB:CC:DD:EE:02", false}, "z1": {"not-a-mac", true}, "z2": {"", false}}
		for id, w := range want {
			if a := d.Accounts[id]; a.MAC != w.mac || a.MACConflict != w.conflict {
				t.Errorf("%s: %+v want %+v", id, a, w)
			}
		}
		if c := d.ClosedAccounts["c1"]; c.MACConflict || c.MAC != "AA:BB:CC:DD:EE:02" {
			t.Errorf("closed duplicate must stay clean: %+v", c)
		}
		if c := d.ClosedAccounts["c2"]; !c.MACConflict || c.MAC != "garbage" {
			t.Errorf("closed malformed: %+v", c)
		}
		audit := map[string]string{}
		for _, e := range d.Events {
			if e.Kind == "audit" {
				audit[e.AccountID] = e.Msg
				if e.Seq <= 7 {
					t.Errorf("audit seq %d reuses a number", e.Seq)
				}
			}
		}
		if len(audit) != 3 || audit["x1"] == "" || audit["x3"] == "" || audit["z1"] == "" {
			t.Fatalf("audit events: %v", audit)
		}
		if !strings.Contains(audit["x3"], "Flat 2") || !strings.Contains(audit["x3"], "CCC333") || !strings.Contains(audit["x3"], "AA:BB:CC:DD:EE:01") {
			t.Errorf("audit text %q", audit["x3"])
		}
		if a, ok := d.AccountByMAC("AA:BB:CC:DD:EE:01"); !ok || a.ID != "x2" {
			t.Errorf("the keeper owns the MAC, got %+v", a)
		}
		if got := len(d.MACConflicts()); got != 3 {
			t.Errorf("MACConflicts: %d", got)
		}
		if problems := Validate(d); len(problems) != 0 {
			t.Errorf("flagged data must be importable: %v", problems)
		}
		if lines := Conflicts(d); len(lines) != 3 || !strings.Contains(strings.Join(lines, "\n"), "used by house Flat 1 (BBB222) in Block B") {
			t.Errorf("conflict report: %v", lines)
		}
	})
	if b, err := os.ReadFile(p + ".v5.bak"); err != nil || string(b) != v5Dupes {
		t.Fatalf("v5 backup: %v", err)
	}
	var probe struct{ Format int }
	raw, _ := os.ReadFile(p)
	json.Unmarshal(raw, &probe)
	if probe.Format != FormatVersion {
		t.Fatalf("saved format %d", probe.Format)
	}
	// A second start changes nothing (no second migration, no new events).
	n := 0
	s.View(func(d *Data) { n = len(d.Events) })
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.View(func(d *Data) {
		if len(d.Events) != n || len(d.MACConflicts()) != 3 {
			t.Fatalf("reopen: %d events (was %d), %d conflicts", len(d.Events), n, len(d.MACConflicts()))
		}
	})
}

func TestUpdateRefusesSecondOwnerOfAMAC(t *testing.T) {
	s, _ := Open(t.TempDir())
	now := time.Now()
	seed := func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", Name: "A", Mode: SiteModeEnv, CreatedAt: now}
		d.Sites["s2"] = Site{ID: "s2", Name: "B", Mode: SiteModeEnv, CreatedAt: now}
		d.Accounts["a1"] = Account{ID: "a1", SiteID: "s1", Flat: "1", MAC: "AA:BB:CC:DD:EE:01", IP: "10.0.0.5", CreatedAt: now}
		return nil
	}
	if err := s.Update(seed); err != nil {
		t.Fatal(err)
	}
	err := s.Update(func(d *Data) error {
		d.Accounts["a2"] = Account{ID: "a2", SiteID: "s2", Flat: "1", MAC: "aa-bb-cc-dd-ee-01", CreatedAt: now}
		return nil
	})
	if !errors.Is(err, ErrDuplicateMAC) || errors.Is(err, ErrPersist) {
		t.Fatalf("got %v", err)
	}
	if _, ok := s.Snapshot().Accounts["a2"]; ok {
		t.Fatal("rolled back account is still in memory")
	}
	err = s.Update(func(d *Data) error {
		d.Accounts["a3"] = Account{ID: "a3", SiteID: "s1", Flat: "3", MAC: "AA:BB:CC:DD:EE:03", IP: "10.0.0.5", CreatedAt: now}
		return nil
	})
	if !errors.Is(err, ErrDuplicateIP) {
		t.Fatalf("same IP in a building: %v", err)
	}
	// The same IP in another building is fine, a flagged account does not count, a closed one frees the MAC.
	if err := s.Update(func(d *Data) error {
		d.Accounts["a3"] = Account{ID: "a3", SiteID: "s2", Flat: "3", MAC: "AA:BB:CC:DD:EE:03", IP: "10.0.0.5", CreatedAt: now}
		d.Accounts["a4"] = Account{ID: "a4", SiteID: "s2", Flat: "4", MAC: "AA:BB:CC:DD:EE:01", MACConflict: true, CreatedAt: now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if a, _ := snap.AccountByMAC("AA:BB:CC:DD:EE:01"); a.ID != "a1" {
		t.Fatalf("flagged account took the MAC: %+v", a)
	}
	if err := s.Update(func(d *Data) error {
		a := d.Accounts["a1"]
		delete(d.Accounts, "a1")
		a.ClosedAt = now
		d.ClosedAccounts["a1"] = a
		d.Accounts["a5"] = Account{ID: "a5", SiteID: "s2", Flat: "5", MAC: "AA:BB:CC:DD:EE:01", CreatedAt: now}
		return nil
	}); err != nil {
		t.Fatalf("closing frees the MAC: %v", err)
	}
	// Writing a lower-case MAC stores the canonical form.
	s.Update(func(d *Data) error {
		a := d.Accounts["a3"]
		a.MAC = "aa:bb:cc:dd:ee:33"
		d.Accounts["a3"] = a
		return nil
	})
	if got := s.Snapshot().Accounts["a3"].MAC; got != "AA:BB:CC:DD:EE:33" {
		t.Fatalf("stored %q", got)
	}
}

func TestValidateReportsGlobalDuplicates(t *testing.T) {
	d := &Data{}
	d.init()
	d.Sites["s1"] = Site{ID: "s1", Name: "A", Mode: SiteModeEnv}
	d.Sites["s2"] = Site{ID: "s2", Name: "B", Mode: SiteModeEnv}
	d.Accounts["a1"] = Account{ID: "a1", SiteID: "s1", Code: "AAA111", Flat: "1", MAC: "AA:BB:CC:DD:EE:01"}
	d.Accounts["a2"] = Account{ID: "a2", SiteID: "s2", Code: "BBB222", Flat: "1", MAC: "AA:BB:CC:DD:EE:01"}
	problems := Validate(d)
	if len(problems) != 1 || !strings.Contains(problems[0], "more than one open house") {
		t.Fatalf("%v", problems)
	}
	a := d.Accounts["a2"]
	a.MACConflict = true
	d.Accounts["a2"] = a
	if p := Validate(d); len(p) != 0 {
		t.Fatalf("flagged: %v", p)
	}
}

func TestIPIsStoredCanonically(t *testing.T) {
	s, _ := Open(t.TempDir())
	now := time.Now()
	s.Update(func(d *Data) error {
		d.Sites["s1"] = Site{ID: "s1", Name: "A", Mode: SiteModeEnv, CreatedAt: now}
		d.Accounts["a1"] = Account{ID: "a1", SiteID: "s1", Flat: "1", IP: "::ffff:10.0.0.5", CreatedAt: now}
		d.Accounts["a2"] = Account{ID: "a2", SiteID: "s1", Flat: "2", IP: "not an ip", CreatedAt: now}
		return nil
	})
	snap := s.Snapshot()
	if snap.Accounts["a1"].IP != "10.0.0.5" || snap.Accounts["a2"].IP != "not an ip" {
		t.Fatalf("%+v %+v", snap.Accounts["a1"], snap.Accounts["a2"])
	}
	// The same address in another spelling is a duplicate.
	err := s.Update(func(d *Data) error {
		d.Accounts["a3"] = Account{ID: "a3", SiteID: "s1", Flat: "3", IP: "::ffff:10.0.0.5", CreatedAt: now}
		return nil
	})
	if !errors.Is(err, ErrDuplicateIP) {
		t.Fatalf("%v", err)
	}
}

func TestMigrateV5ToV6EquipmentMACs(t *testing.T) {
	dir := t.TempDir()
	raw := `{"format":5,
 "sites":{"s1":{"id":"s1","name":"A","mode":"env","created_at":"2026-01-01T00:00:00Z"}},
 "accounts":{"a1":{"id":"a1","site_id":"s1","code":"AAA111","flat":"1","mac":"AA:BB:CC:DD:EE:01","hs_pass":"x","created_at":"2026-01-01T00:00:00Z"}},
 "equipment":{
  "k1":{"id":"k1","kind":"ont","mac":"aa:bb:cc:dd:ee:20","status":"in_stock","created_at":"2026-01-01T00:00:00Z"},
  "k2":{"id":"k2","kind":"ont","mac":"AA-BB-CC-DD-EE-20","status":"installed","site_id":"s1","account_id":"a1","created_at":"2026-02-01T00:00:00Z"},
  "k3":{"id":"k3","kind":"ont","mac":"AA:BB:CC:DD:EE:20","status":"installed","site_id":"s1","notes":"hi","created_at":"2026-03-01T00:00:00Z"},
  "k4":{"id":"k4","kind":"ont","mac":"AA:BB:CC:DD:EE:20","status":"retired","created_at":"2026-01-01T00:00:00Z"}},
 "event_seq":0}`
	os.WriteFile(filepath.Join(dir, "ispbill.json"), []byte(raw), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := s.Snapshot()
	if len(d.Equipment) != 4 || d.Equipment["k2"].Status != EqInstalled || d.Equipment["k2"].AccountID != "a1" || d.Equipment["k2"].MAC != "AA:BB:CC:DD:EE:20" {
		t.Fatalf("keeper: %+v", d.Equipment["k2"])
	}
	for _, id := range []string{"k1", "k3"} {
		if e := d.Equipment[id]; e.Status != EqRetired || e.AccountID != "" || e.MAC != "AA:BB:CC:DD:EE:20" {
			t.Errorf("%s: %+v", id, e)
		}
	}
	if n := d.Equipment["k3"].Notes; n != "hi | migration 0004: MAC AA:BB:CC:DD:EE:20 is on another live item; retired (was installed)" {
		t.Errorf("notes %q", n)
	}
	if d.Equipment["k4"].Notes != "" {
		t.Error("a retired item must be left alone")
	}
	n := 0
	for _, e := range d.Events {
		if e.Kind == "audit" && e.EquipmentID != "" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d audit events", n)
	}
	if p := Validate(&d); len(p) != 0 {
		t.Fatalf("%v", p)
	}
}
