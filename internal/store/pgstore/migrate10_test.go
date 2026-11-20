package pgstore

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Migration 0010 adds the optional GPS columns: existing apartments keep NULL (no
// point), a point is stored as a pair, 0,0 is a point, and the database refuses a
// half pair or a point off the globe.
func TestMigration0010ApartmentGPS(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 9})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.pool.Exec(ctx, `INSERT INTO apartments (id, name, created_at) VALUES ('a1','Before','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	b, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var nulls int
	if err := b.pool.QueryRow(ctx, `SELECT count(*) FROM apartments WHERE id = 'a1' AND lat IS NULL AND lng IS NULL`).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("existing apartment: %d %v", nulls, err)
	}
	if d, err := b.LoadAll(ctx); err != nil || d.Apartments["a1"].HasGPS() {
		t.Fatalf("loaded apartment has a point: %v %+v", err, d.Apartments["a1"])
	}
	for _, q := range []string{
		`INSERT INTO apartments (id, name, created_at, lat, lng) VALUES ('a2','Nairobi','2026-01-01', -1.2921, 36.8219)`,
		`INSERT INTO apartments (id, name, created_at, lat, lng) VALUES ('a3','Null island','2026-01-01', 0, 0)`,
	} {
		if _, err := b.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d, err := b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a := d.Apartments["a2"]; !a.HasGPS() || *a.Lat != -1.2921 || *a.Lng != 36.8219 {
		t.Errorf("a2: %+v", a)
	}
	if a := d.Apartments["a3"]; !a.HasGPS() || *a.Lat != 0 || *a.Lng != 0 {
		t.Errorf("a3: %+v", a)
	}
	for _, q := range []string{
		`INSERT INTO apartments (id, name, created_at, lat) VALUES ('b1','Half','2026-01-01', 1)`,
		`INSERT INTO apartments (id, name, created_at, lng) VALUES ('b2','Half2','2026-01-01', 1)`,
		`INSERT INTO apartments (id, name, created_at, lat, lng) VALUES ('b3','North','2026-01-01', 91, 0)`,
		`INSERT INTO apartments (id, name, created_at, lat, lng) VALUES ('b4','East','2026-01-01', 0, 181)`,
	} {
		_, err := b.pool.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.ConstraintName != "apartments_gps_shape" {
			t.Errorf("%s: not refused by the check: %v", q, err)
		}
	}
}
