package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"ispbill/internal/store"
)

// Schema 6 data migrates to schema 7 with the same result the JSON upgrade
// gives for the same data (store.TestMigrateV7ToV8Apartments): one apartment
// per router named after it, accounts take its apartment and keep their flat
// label as unit, a repeated unit is renamed with its code, router contacts
// move onto the apartment, and site_contacts is gone.
func TestMigration0008Apartments(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 6})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sites (id, name, mode, address, created_at) VALUES ('s1','Riverside','env','Ngong Rd','2026-01-01'), ('s2','Hilltop','env','','2026-01-02')`,
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at) VALUES
		  ('a1','s1','AAA111','Flat 1','AA:BB:CC:DD:EE:01','2026-01-03'),
		  ('a2','s1','BBB222','B4','AA:BB:CC:DD:EE:02','2026-01-04'),
		  ('a3','s2','CCC333','B4','AA:BB:CC:DD:EE:03','2026-01-04'),
		  ('a4','s1','DDD444','flat  1','AA:BB:CC:DD:EE:04','2026-01-05')`,
		`INSERT INTO accounts (id,site_id,code,flat,mac,created_at,closed_at) VALUES ('a5','s1','EEE555','Flat 1','AA:BB:CC:DD:EE:05','2026-01-01','2026-02-01')`,
		`INSERT INTO site_contacts (id,site_id,role,name,phone,email,notes,created_at) VALUES
		  ('c1','s1','caretaker','Otieno','0722000111','o@x.org','gate','2026-01-01'),
		  ('c2','s1','landlord','Kamau','0711222333','','','2026-01-02'),
		  ('c3','s1','security','Guard','0700000001','','','2026-01-03'),
		  ('c4','s1','agent','Wambui','0733444555','','','2026-01-04'),
		  ('c5','s2','security','Night guard','0700000009','','after 8pm','2026-01-01')`,
	} {
		if _, err := old.pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
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
	if len(d.Apartments) != 2 || len(d.ApartmentSites) != 2 {
		t.Fatalf("%d apartments, %d links", len(d.Apartments), len(d.ApartmentSites))
	}
	a1, a2 := d.Apartments[store.MigratedApartmentID("s1")], d.Apartments[store.MigratedApartmentID("s2")]
	if a1.Name != "Riverside" || a1.Address != "Ngong Rd" || a2.Name != "Hilltop" {
		t.Fatalf("apartments %+v %+v", a1, a2)
	}
	if _, ok := d.ApartmentSites[store.ApartmentSiteKey(a1.ID, "s1")]; !ok {
		t.Fatal("apartment not linked to its router")
	}
	if w := (store.ApartmentContact{Name: "Otieno", Phone: "+254722000111", Email: "o@x.org", Notes: "gate | Also security: Guard, 0700000001"}); a1.Caretaker != w {
		t.Errorf("caretaker %+v", a1.Caretaker)
	}
	if w := (store.ApartmentContact{Name: "Kamau", Phone: "+254711222333", Notes: "Also agent: Wambui, 0733444555"}); a1.Landlord != w {
		t.Errorf("landlord %+v", a1.Landlord)
	}
	if w := (store.ApartmentContact{Name: "Night guard", Phone: "+254700000009", Notes: "Role: security | after 8pm"}); a2.Caretaker != w || !a2.Landlord.Empty() {
		t.Errorf("hilltop %+v / %+v", a2.Caretaker, a2.Landlord)
	}
	for id, u := range map[string]string{"a1": "Flat 1", "a2": "B4", "a3": "B4", "a4": "flat  1 (DDD444)"} {
		a := d.Accounts[id]
		if a.ApartmentID != store.MigratedApartmentID(a.SiteID) || a.Unit != u || a.Flat != u {
			t.Errorf("%s: apartment %q unit %q flat %q", id, a.ApartmentID, a.Unit, a.Flat)
		}
	}
	if c := d.ClosedAccounts["a5"]; c.ApartmentID != store.MigratedApartmentID("s1") || c.Unit != "Flat 1" {
		t.Errorf("closed account %+v", c)
	}
	audited := false
	for _, e := range d.Events {
		audited = audited || (e.AccountID == "a4" && e.Kind == "audit")
	}
	if !audited {
		t.Error("the unit conflict left no audit event")
	}
	var n int
	if err := b.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'site_contacts'`, schema).Scan(&n); err != nil || n != 0 {
		t.Errorf("site_contacts still exists (%d, %v)", n, err)
	}
	// The unique index holds: a second open account with the same unit is refused as a typed error.
	_, err = b.pool.Exec(ctx, `INSERT INTO accounts (id,site_id,apartment_id,unit,code,flat,mac,created_at) VALUES ('z1','s1',$1,' b4 ','ZZZ111',' b4 ','AA:BB:CC:DD:EE:99','2026-02-01')`, a1.ID)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.ConstraintName != "accounts_apartment_unit_open" {
		t.Fatalf("duplicate unit not refused by the index: %v", err)
	}
}
