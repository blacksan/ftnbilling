package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ispbill/internal/store"
)

func newTestSchema(t *testing.T) (url, schema string) {
	t.Helper()
	url = testURL(t)
	var rb [4]byte
	rand.Read(rb[:])
	schema = "m4_" + hex.EncodeToString(rb[:])
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, url)
		if err == nil {
			c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			c.Close(ctx)
		}
	})
	return url, schema
}

// A database written by schema 3 with duplicate and malformed MACs migrates to
// schema 4: nothing is dropped, the keepers stay clean, the rest is flagged and
// reported, and the new unique index holds.
func TestMigration0004FlagsDuplicateMACs(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := old.pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&v); err != nil || v != 3 {
		t.Fatalf("setup: schema at %d, %v", v, err)
	}
	// The old per-building index lets the same MAC live in two buildings.
	future := time.Now().Add(24 * time.Hour).UTC()
	seed := []string{
		`INSERT INTO sites (id, name, mode, created_at) VALUES ('s1','Block A','env',now()), ('s2','Block B','env',now())`,
		`INSERT INTO packages (id,name,slug,duration_min,speed_mbps,price_kes) VALUES ('p1','1 Day','1-day',1440,10,50)`,
		// m1 is shared by three open accounts: x2 has the live plan (keeper), x1 is older, x3 newer.
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at,package_id,expires_at) VALUES
		   ('x1','s1','AAA111','Flat 1','aa:bb:cc:dd:ee:01','2026-01-01',NULL,NULL),
		   ('x2','s2','BBB222','Flat 1','AA-BB-CC-DD-EE-01','2026-02-01','p1','` + future.Format(time.RFC3339) + `'),
		   ('x3','s2','CCC333','Flat 2','aabb.ccdd.ee01','2026-03-01',NULL,NULL)`,
		// m2 is shared by an open account and a closed one: the closed one is not a conflict.
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at,closed_at) VALUES ('c1','s1','DDD444','Flat 9','AA:BB:CC:DD:EE:02','2026-01-01','2026-02-01')`,
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ('y1','s1','EEE555','Flat 3','AA:BB:CC:DD:EE:02','2026-01-05')`,
		// Malformed MACs are kept as typed and flagged; a closed one is flagged silently.
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ('z1','s1','FFF666','Flat 4','not-a-mac','2026-01-06'), ('z2','s1','GGG777','Flat 5','AA:BB','2026-01-07'), ('z3','s1','HHH888','Flat 6','','2026-01-08'), ('z4','s1','JJJ999','Flat 7','','2026-01-09')`,
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at,closed_at) VALUES ('c2','s1','KKK234','Flat 8','garbage','2026-01-01','2026-02-01')`,
		// Equipment: two live items with one MAC; the second is retired by the migration.
		`INSERT INTO equipment (id,kind,mac,status,created_at) VALUES ('e1','ont','aa:bb:cc:dd:ee:10','in_stock','2026-01-01'), ('e2','ont','AA-BB-CC-DD-EE-10','in_stock','2026-01-01')`,
		`INSERT INTO equipment (id,kind,mac,status,notes,created_at) VALUES ('e3','ont','AA:BB:CC:DD:EE:11','installed','keep','2026-01-01')`,
		`INSERT INTO events (seq,time,kind,msg) VALUES (41,now(),'info','old'),(42,now(),'info','old2')`,
	}
	for _, q := range seed {
		if _, err := old.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	old.Close()

	b, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	d, err := b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		mac      string
		conflict bool
	}{
		"x1": {"AA:BB:CC:DD:EE:01", true}, "x2": {"AA:BB:CC:DD:EE:01", false}, "x3": {"AA:BB:CC:DD:EE:01", true},
		"y1": {"AA:BB:CC:DD:EE:02", false}, "z1": {"not-a-mac", true}, "z2": {"AA:BB", true}, "z3": {"", false}, "z4": {"", false},
	}
	for id, w := range want {
		a, ok := d.Accounts[id]
		if !ok || a.MAC != w.mac || a.MACConflict != w.conflict {
			t.Errorf("account %s: %+v, want mac %q conflict %v", id, a, w.mac, w.conflict)
		}
	}
	if c := d.ClosedAccounts["c1"]; c.MAC != "AA:BB:CC:DD:EE:02" || c.MACConflict {
		t.Errorf("closed account with a shared MAC must stay clean: %+v", c)
	}
	if c := d.ClosedAccounts["c2"]; c.MAC != "garbage" || !c.MACConflict {
		t.Errorf("closed malformed MAC is kept and flagged (the shape check needs it): %+v", c)
	}
	if len(d.Accounts)+len(d.ClosedAccounts) != 10 {
		t.Fatalf("rows were dropped: %d open, %d closed", len(d.Accounts), len(d.ClosedAccounts))
	}
	// One audit event per flagged open account, after the old events.
	got := map[string]string{}
	eqAudit := map[string]string{}
	for _, e := range d.Events {
		if e.Kind == "audit" && e.EquipmentID != "" {
			eqAudit[e.EquipmentID] = e.Msg
			continue
		}
		if e.Kind == "audit" {
			got[e.AccountID] = e.Msg
			if e.Seq <= 42 {
				t.Errorf("audit event reuses a sequence number: %d", e.Seq)
			}
		}
	}
	if len(got) != 4 || got["x1"] == "" || got["x3"] == "" || got["z1"] == "" || got["z2"] == "" || got["x2"] != "" || got["c2"] != "" {
		t.Fatalf("audit events for %v", got)
	}
	if len(eqAudit) != 1 || eqAudit["e2"] == "" {
		t.Fatalf("equipment audit events: %v", eqAudit)
	}
	// Equipment e1/e2 share a MAC: e1 (oldest by id) stays live, e2 is retired, detached and annotated; both rows are kept.
	if e1, e2 := d.Equipment["e1"], d.Equipment["e2"]; len(d.Equipment) != 3 || e1.Status != "in_stock" || e2.Status != "retired" ||
		e2.MAC != "AA:BB:CC:DD:EE:10" || !strings.Contains(e2.Notes, "migration 0004") || e2.AccountID != "" {
		t.Fatalf("equipment after the migration: %+v / %+v", e1, e2)
	}
	if e3 := d.Equipment["e3"]; e3.Status != "installed" || e3.AccountID != "" || e3.Notes != "keep" {
		t.Fatalf("e3 must be untouched: %+v", e3)
	}
	if !strings.Contains(got["x1"], "Flat 1") || !strings.Contains(got["x1"], "AAA111") || !strings.Contains(got["x1"], "AA:BB:CC:DD:EE:01") {
		t.Errorf("audit text: %q", got["x1"])
	}
	if d.EventSeq < 46 {
		t.Errorf("EventSeq %d did not follow the inserted events", d.EventSeq)
	}
	// db-check numbers.
	c, err := b.Counts(ctx)
	if err != nil || c.MACConflicts != 4 {
		t.Fatalf("counts: %+v %v", c, err)
	}
	if lines := store.Conflicts(d); len(lines) != 4 {
		t.Fatalf("conflict report: %v", lines)
	}
	// Indexes: the unique index exists, the old one is gone, the equipment one always exists.
	has := func(name string) bool {
		var n int
		b.pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = $2", schema, name).Scan(&n)
		return n == 1
	}
	if !has("accounts_mac_open") || has("accounts_site_mac_open") || !has("accounts_site_ip_open") || !has("equipment_mac_live") {
		t.Fatal("indexes after the migration are wrong")
	}
	if m := d.Equipment["e1"].MAC; m != "AA:BB:CC:DD:EE:10" {
		t.Errorf("equipment MAC not normalised: %q", m)
	}
	// The database refuses a second open account on a MAC, in any building, whatever the spelling...
	_, err = b.pool.Exec(ctx, `INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ('n1','s1','NNN234','Flat 20','AA:BB:CC:DD:EE:01',now())`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.ConstraintName != "accounts_mac_open" {
		t.Fatalf("second owner: %v", err)
	}
	// ...a lower-case spelling fails the shape check instead...
	_, err = b.pool.Exec(ctx, `INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ('n2','s1','NNN235','Flat 21','aa:bb:cc:dd:ee:77',now())`)
	if !errors.As(err, &pe) || pe.ConstraintName != "accounts_mac_shape" {
		t.Fatalf("lower case: %v", err)
	}
	// ...and Commit turns the index into store.ErrDuplicateMAC.
	err = b.Commit(ctx, nil, &store.ChangeSet{Accounts: store.Delta[store.Account]{Upsert: []store.Account{
		{ID: "n3", SiteID: "s1", Code: "NNN236", Flat: "Flat 22", MAC: "AA:BB:CC:DD:EE:01", CreatedAt: time.Now()}}}})
	if !errors.Is(err, store.ErrDuplicateMAC) {
		t.Fatalf("Commit: %v", err)
	}
	err = b.Commit(ctx, nil, &store.ChangeSet{Accounts: store.Delta[store.Account]{Upsert: []store.Account{
		{ID: "n4", SiteID: "s1", Code: "NNN237", Flat: "Flat 23", IP: "10.0.0.5", CreatedAt: time.Now()},
		{ID: "n5", SiteID: "s1", Code: "NNN238", Flat: "Flat 24", IP: "10.0.0.5", CreatedAt: time.Now()}}}})
	if !errors.Is(err, store.ErrDuplicateIP) {
		t.Fatalf("Commit IP: %v", err)
	}
	// The same data loads and runs through a Store.
	b.Close()
	b2, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenWith(b2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.View(func(d *store.Data) {
		if a, ok := d.AccountByMAC("AA:BB:CC:DD:EE:01"); !ok || a.ID != "x2" {
			t.Errorf("owner of the shared MAC: %+v", a)
		}
	})
	// A flagged account can be fixed: give it a free MAC and clear the flag.
	if err := s.Update(func(d *store.Data) error {
		a := d.Accounts["x1"]
		a.MAC, a.MACConflict = "AA:BB:CC:DD:EE:31", false
		d.Accounts["x1"] = a
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// And clearing the flag without fixing the MAC is refused.
	if err := s.Update(func(d *store.Data) error {
		a := d.Accounts["x3"]
		a.MACConflict = false
		d.Accounts["x3"] = a
		return nil
	}); !errors.Is(err, store.ErrDuplicateMAC) {
		t.Fatalf("clearing the flag: %v", err)
	}
}

// A clean database migrates without flags or events and gets the equipment index.
func TestMigration0004CleanDatabase(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sites (id, name, mode, created_at) VALUES ('s1','Block A','env',now())`,
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ('a1','s1','AAA111','Flat 1','AA:BB:CC:DD:EE:01',now())`,
		`INSERT INTO equipment (id,kind,mac,status,created_at) VALUES ('e1','ont','AA:BB:CC:DD:EE:01','installed',now())`,
	} {
		if _, err := old.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	b, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n, ev int
	b.pool.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE mac_conflict").Scan(&n)
	b.pool.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&ev)
	var idx int
	b.pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND indexname = 'equipment_mac_live'", schema).Scan(&idx)
	if n != 0 || ev != 0 || idx != 1 {
		t.Fatalf("flags %d events %d equipment index %d", n, ev, idx)
	}
	var mode string
	b.pool.QueryRow(ctx, "SELECT access_mode FROM sites WHERE id = 's1'").Scan(&mode)
	if mode != "ipoe" {
		t.Fatalf("access_mode default %q", mode)
	}
}
