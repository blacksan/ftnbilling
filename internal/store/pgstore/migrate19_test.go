package pgstore

import (
	"context"
	"reflect"
	"testing"

	"ispbill/internal/store"
)

// Migration 0019 adds the equipment hierarchy columns. Equipment that exists before it (an ONT, an OLT item with no
// details) loads with every new field empty; the new fields round-trip through the codec.
func TestMigration0019EquipmentHierarchy(t *testing.T) {
	url, schema := newTestSchema(t)
	ctx := context.Background()
	old, err := Open(ctx, url, Options{Schema: schema, maxVersion: 18})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.pool.Exec(ctx, `INSERT INTO sites (id, name, mode, created_at) VALUES ('s1','Before','wg','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.pool.Exec(ctx, `INSERT INTO equipment (id, kind, serial, mac, status, site_id, created_at) VALUES
		('e1','ont','ZTEG1','AA:BB:CC:DD:EE:01','in_stock',NULL,'2026-01-01'),
		('e2','olt','VSOL1','48:21:B8:10:D7:72','installed','s1','2026-01-01')`); err != nil {
		t.Fatal(err)
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
	for _, id := range []string{"e1", "e2"} {
		e := d.Equipment[id]
		if e.Name != "" || e.RouterPort != "" || e.PONPorts != 0 || len(e.PONApartments) != 0 || e.MgmtIP != "" || e.MgmtPass != "" || e.OLTID != "" || e.PONPort != 0 {
			t.Fatalf("an item from before the migration must load with the new fields empty: %+v", e)
		}
	}
	// the new fields round-trip
	want := store.Equipment{ID: "e3", Kind: "olt", Serial: "VSOL2", MAC: "48:21:B8:10:D7:73", Status: "installed", SiteID: "s1", Name: "Block A",
		RouterPort: "ether3", PONPorts: 4, PONApartments: map[string][]string{"1": {"a1", "a2"}, "3": {"a3"}}, MgmtIP: "10.0.0.2", MgmtUser: "admin", MgmtPass: "sealed",
		CreatedAt: d.Equipment["e1"].CreatedAt}
	onu := store.Equipment{ID: "e4", Kind: "ont", Serial: "ZTEG2", Status: "in_stock", OLTID: "e3", PONPort: 2, CreatedAt: want.CreatedAt}
	cs := &store.ChangeSet{Equipment: store.Delta[store.Equipment]{Upsert: []store.Equipment{want, onu}}}
	if err := b.Commit(ctx, nil, cs); err != nil {
		t.Fatal(err)
	}
	d, err = b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Equipment["e3"]; !reflect.DeepEqual(got.Norm(), want.Norm()) {
		t.Fatalf("OLT did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	if got := d.Equipment["e4"]; got.OLTID != "e3" || got.PONPort != 2 {
		t.Fatalf("ONU placement did not round-trip: %+v", got)
	}
	// the check constraint refuses a negative PON port
	if _, err := b.pool.Exec(ctx, `UPDATE equipment SET pon_port = -1 WHERE id = 'e4'`); err == nil {
		t.Fatal("a negative PON port must be refused by the database")
	}
}
