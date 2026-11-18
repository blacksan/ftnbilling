package pgstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"ispbill/internal/store"
)

// macVectors are spellings both the Go code (store.CanonMAC) and migration 0004
// must treat the same way.
var macVectors = []string{
	"aa:bb:cc:dd:ee:01", "AA-BB-CC-DD-EE-02", "aabb.ccdd.ee03", "aabbccddee04", "AA BB CC DD EE 05", " AABBCCDDEE06",
	"AA::BB:CC:DD:EE:07", "AABBCCDDEE08 ", "01:00:5e:00:00:09", // multicast: canonical, not judged
	"AA:BB:CC:DD:EE", "AA:BB:CC:DD:EE:0G", "AA:BB:CC:DD:EE:01:02", "AABBCCDDEE0A\t", " AABBCCDDEE0B", "ＡＡＢＢＣＣＤＤＥＥ０１", "xyz", "",
}

func TestMigration0004SpellingsMatchGo(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range macVectors {
		site := fmt.Sprintf("s%d", i)
		for _, q := range []struct {
			sql string
			arg []any
		}{
			{`INSERT INTO sites (id, name, mode, created_at) VALUES ($1, $1, 'env', now())`, []any{site}},
			{`INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ($1,$2,$3,'Flat',$4,now())`, []any{"a" + site, site, fmt.Sprintf("C%05d", i), v}},
			{`INSERT INTO equipment (id,kind,mac,status,created_at) VALUES ($1,'ont',$2,'in_stock',now())`, []any{"e" + site, v}},
		} {
			if _, err := old.pool.Exec(ctx, q.sql, q.arg...); err != nil {
				t.Fatalf("seed %q: %v", v, err)
			}
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
	for i, v := range macVectors {
		site := fmt.Sprintf("s%d", i)
		want, ok := store.CanonMAC(v)
		if !ok {
			want = v
		}
		a, e := d.Accounts["a"+site], d.Equipment["e"+site]
		if a.MAC != want || e.MAC != want {
			t.Errorf("%q: Go says %q (ok=%v); database wrote account %q equipment %q", v, want, ok, a.MAC, e.MAC)
		}
		// Malformed non-empty MACs are flagged. (Distinct vectors may canonicalise to one MAC: then
		// the later ones are flagged as duplicates, which is also fine.)
		if v != "" && !ok && !a.MACConflict {
			t.Errorf("%q: malformed MAC not flagged", v)
		}
	}
}

// Old databases must not break db-check.
func TestCountsOnAnOlderSchema(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	old.pool.Exec(ctx, `INSERT INTO sites (id, name, mode, created_at) VALUES ('s1','A','env',now())`)
	c, err := old.Counts(ctx)
	if err != nil {
		t.Fatalf("Counts on schema 2: %v", err)
	}
	if c.SchemaVersion != 2 || c.Rows["sites"] != 1 || c.Rows["admin_users"] != 0 || c.Rows["settings"] != 0 || c.MACConflicts != 0 {
		t.Fatalf("%+v", c)
	}
	// An empty schema (no migrations at all) works too.
	_, schema2 := newTestSchema(t)
	b2, err := Open(ctx, url, Options{Schema: schema2, NoMigrate: true, NoLock: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if c, err := b2.Counts(ctx); err != nil || c.SchemaVersion != 0 {
		t.Fatalf("empty schema: %+v %v", c, err)
	}
}

// Duplicate equipment MACs that include an installed item attached to a flat.
func TestMigration0004RetiresDuplicateEquipmentButKeepsTheRow(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sites (id, name, mode, created_at) VALUES ('s1','A','env',now())`,
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES ('a1','s1','AAA111','F1','AA:BB:CC:DD:EE:01',now()), ('a2','s1','BBB222','F2','AA:BB:CC:DD:EE:02',now())`,
		`INSERT INTO equipment (id,kind,mac,status,site_id,account_id,created_at) VALUES
		   ('k1','ont','AA:BB:CC:DD:EE:20','in_stock',NULL,NULL,'2026-01-01'),
		   ('k2','ont','AA:BB:CC:DD:EE:20','installed','s1','a2','2026-02-01'),
		   ('k3','ont','AA:BB:CC:DD:EE:20','installed','s1','a1','2026-03-01'),
		   ('k4','ont','AA:BB:CC:DD:EE:20','retired',NULL,NULL,'2026-01-01')`,
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
	d, _ := b.LoadAll(ctx)
	// An installed item wins over an older one in stock; of two installed, the older.
	if d.Equipment["k2"].Status != "installed" || d.Equipment["k2"].AccountID != "a2" {
		t.Fatalf("keeper: %+v", d.Equipment["k2"])
	}
	for _, id := range []string{"k1", "k3"} {
		e := d.Equipment[id]
		if e.Status != "retired" || e.AccountID != "" || e.MAC != "AA:BB:CC:DD:EE:20" {
			t.Errorf("%s: %+v", id, e)
		}
	}
	if n := d.Equipment["k3"].Notes; n != "migration 0004: MAC AA:BB:CC:DD:EE:20 is on another live item; retired (was installed), detached from account a1" {
		t.Errorf("k3 notes: %q", n)
	}
	if d.Equipment["k4"].Notes != "" {
		t.Errorf("an already retired item is not touched: %+v", d.Equipment["k4"])
	}
	n := 0
	for _, e := range d.Events {
		if e.Kind == "audit" && e.EquipmentID != "" {
			n++
		}
	}
	if len(d.Equipment) != 4 || n != 2 {
		t.Fatalf("%d items, %d audit events", len(d.Equipment), n)
	}
}

// Every integrity rule maps to its own error and none of them degrades the store.
func TestCommitMapsIntegrityRefusals(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	b, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	now := time.Now()
	commit := func(cs *store.ChangeSet) error { return b.Commit(ctx, nil, cs) }
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("seed", commit(&store.ChangeSet{
		Sites:      store.Delta[store.Site]{Upsert: []store.Site{{ID: "s1", Name: "A", Mode: "env", AccessMode: "hotspot", CreatedAt: now}}},
		Equipment:  store.Delta[store.Equipment]{Upsert: []store.Equipment{{ID: "e1", Kind: "ont", MAC: "AA:BB:CC:DD:EE:10", Status: "in_stock", CreatedAt: now}}},
		AdminUsers: store.Delta[store.AdminUser]{Upsert: []store.AdminUser{{ID: "u1", Username: "Brian", Role: "owner", CreatedAt: now}}},
		APIDevices: store.Delta[store.APIDevice]{Upsert: []store.APIDevice{{ID: "d1", UserID: "u1", AccessHash: "ah", RefreshHash: "rh", CreatedAt: now}}},
	}))
	cases := []struct {
		name string
		cs   *store.ChangeSet
		want error
	}{
		{"equipment", &store.ChangeSet{Equipment: store.Delta[store.Equipment]{Upsert: []store.Equipment{{ID: "e2", Kind: "ont", MAC: "AA:BB:CC:DD:EE:10", Status: "in_stock", CreatedAt: now}}}}, store.ErrDuplicateEquipmentMAC},
		{"username", &store.ChangeSet{AdminUsers: store.Delta[store.AdminUser]{Upsert: []store.AdminUser{{ID: "u2", Username: "BRIAN", Role: "viewer", CreatedAt: now}}}}, store.ErrDuplicateUsername},
		{"access token", &store.ChangeSet{APIDevices: store.Delta[store.APIDevice]{Upsert: []store.APIDevice{{ID: "d2", UserID: "u1", AccessHash: "ah", CreatedAt: now}}}}, store.ErrDuplicateToken},
		{"refresh token", &store.ChangeSet{APIDevices: store.Delta[store.APIDevice]{Upsert: []store.APIDevice{{ID: "d3", UserID: "u1", RefreshHash: "rh", CreatedAt: now}}}}, store.ErrDuplicateToken},
		{"mac shape", &store.ChangeSet{Accounts: store.Delta[store.Account]{Upsert: []store.Account{{ID: "a1", SiteID: "s1", Code: "AAA111", Flat: "1", MAC: "aa:bb", CreatedAt: now}}}}, store.ErrBadMAC},
		{"access mode", &store.ChangeSet{Sites: store.Delta[store.Site]{Upsert: []store.Site{{ID: "s1", Name: "A", Mode: "env", AccessMode: "bogus", CreatedAt: now}}}}, store.ErrBadAccessMode},
		{"foreign key", &store.ChangeSet{Accounts: store.Delta[store.Account]{Upsert: []store.Account{{ID: "a9", SiteID: "nope", Code: "ZZZ999", Flat: "1", CreatedAt: now}}}}, nil},
	}
	for _, c := range cases {
		err := commit(c.cs)
		if !errors.Is(err, store.ErrIntegrity) {
			t.Errorf("%s: %v is not store.ErrIntegrity", c.name, err)
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v does not match %v", c.name, err, c.want)
		}
	}
}
