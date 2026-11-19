package pgstore_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
	"ispbill/internal/store/storetest"
)

// Every table the migrations create is either wiped or kept on purpose. A new
// migration that adds a table fails this test until the table is listed in
// WipeTables or KeptTables.
func TestWipeClassifiesEveryTable(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	ctx := context.Background()
	b, err := open(t, url, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	rows, err := b.Pool().Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE'", schema)
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		have = append(have, n)
	}
	rows.Close()
	var want []string
	want = append(want, pgstore.WipeTables...)
	for k := range pgstore.KeptTables {
		want = append(want, k)
	}
	sort.Strings(have)
	sort.Strings(want)
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("tables in the schema %v\nwiped + kept %v\nclassify the new table in pgstore/wipe.go", have, want)
	}
	// TRUNCATE ... CASCADE must not reach a kept table: none of them has a foreign key to a wiped one.
	for kept := range pgstore.KeptTables {
		var n int
		err := b.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_constraint c WHERE c.contype = 'f' AND c.conrelid = $1::regclass
			AND c.confrelid::regclass::text = ANY($2)`, schema+"."+kept, pgstore.WipeTables).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("kept table %s has a foreign key to a wiped table: CASCADE would empty it", kept)
		}
	}
	for _, k := range []string{"admin_users", "settings"} {
		if strings.TrimSpace(pgstore.KeptTables[k]) == "" {
			t.Errorf("no reason given for keeping %s", k)
		}
	}
}

func TestWipeKeepsAdminsAndSettings(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	ctx := context.Background()
	b, err := open(t, url, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenWith(b)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Fixture(t, st)
	at := st.Snapshot().Sites["s111111"].CreatedAt
	if err := st.Update(func(d *store.Data) error {
		d.Settings["site.s111111.join_report"] = store.Setting{Key: "site.s111111.join_report", Value: "ok", UpdatedAt: at}
		d.Settings["admin.kb.abc"] = store.Setting{Key: "admin.kb.abc", Value: "u1", UpdatedAt: at}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := st.Snapshot()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = open(t, url, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	prev, err := b.WipePreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := before.WipeReport(); !reflect.DeepEqual(prev, want) {
		t.Fatalf("preview %+v, the same data in memory reports %+v", prev, want)
	}
	if again, _ := b.WipePreview(ctx); !reflect.DeepEqual(again, prev) {
		t.Fatal("the preview changed something")
	}
	rep, err := b.Wipe(ctx)
	if err != nil || !reflect.DeepEqual(rep, prev) {
		t.Fatalf("wipe: %v %+v", err, rep)
	}
	if rep.Total() == 0 {
		t.Fatal("the fixture should have had rows")
	}
	for _, tbl := range pgstore.WipeTables {
		var n int
		b.Pool().QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n)
		if n != 0 {
			t.Errorf("%s still has %d rows", tbl, n)
		}
	}
	var k int
	b.Pool().QueryRow(ctx, "SELECT count(*) FROM settings WHERE left(key,5) = 'site.'").Scan(&k)
	if k != 0 {
		t.Errorf("%d router settings left", k)
	}
	d, err := b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.AdminUsers, before.AdminUsers) {
		t.Errorf("admin users changed:\n%+v\n%+v", d.AdminUsers, before.AdminUsers)
	}
	for _, key := range []string{"wa.token", "wa.lang", "admin.kb.abc"} {
		if !reflect.DeepEqual(d.Settings[key], before.Settings[key]) {
			t.Errorf("setting %s: %+v, was %+v", key, d.Settings[key], before.Settings[key])
		}
	}
	if len(d.Settings) != 3 {
		t.Errorf("settings left: %v", d.Settings)
	}
	have, _ := b.AppliedVersion(ctx)
	if have != pgstore.SchemaVersion() {
		t.Errorf("schema version %d", have)
	}
	// A second wipe on the empty database is a no-op that still works.
	if rep, err := b.Wipe(ctx); err != nil || rep.Total() != 0 {
		t.Errorf("second wipe: %v %+v", err, rep)
	}
}
