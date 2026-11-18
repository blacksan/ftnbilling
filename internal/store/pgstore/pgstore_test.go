package pgstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
	"ispbill/internal/store/storetest"
)

func open(t *testing.T, url, schema string, mod func(*pgstore.Options)) (*pgstore.Backend, error) {
	o := storetest.PGOptions(schema)
	if mod != nil {
		mod(&o)
	}
	return pgstore.Open(context.Background(), url, o)
}

// TestConformance is the shared suite on Postgres.
func TestConformance(t *testing.T) {
	storetest.RequirePostgres(t)
	if !storetest.Postgres() {
		t.Setenv("TEST_STORE", "postgres")
	}
	storetest.Run(t)
}

func TestMigrationsIdempotentChecksumAndDowngradeGuard(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	ctx := context.Background()
	b, err := open(t, url, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx); err != nil { // second run: nothing to do
		t.Fatalf("migrate twice: %v", err)
	}
	var n int
	b.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n)
	if n != pgstore.SchemaVersion() {
		t.Fatalf("%d migrations recorded, want %d", n, pgstore.SchemaVersion())
	}
	// Edited migration: refuse.
	if _, err := b.Pool().Exec(ctx, "UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want a checksum error, got %v", err)
	}
	b.Pool().Exec(ctx, "UPDATE schema_migrations SET checksum = (SELECT checksum FROM schema_migrations WHERE version = 2) WHERE version = 1")
	b.Close()
	// Database newer than the binary: refuse to open.
	c, err := open(t, url, schema, func(o *pgstore.Options) { o.NoMigrate = true })
	if err != nil {
		t.Fatal(err)
	}
	c.Pool().Exec(ctx, "UPDATE schema_migrations SET checksum = $1 WHERE version = 1", func() string {
		var s string
		c.Pool().QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version = 1").Scan(&s)
		return s
	}())
	c.Pool().Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES (999, 'future', 'x')")
	c.Close()
	if _, err := open(t, url, schema, nil); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("want a downgrade error, got %v", err)
	}
}

func TestReportingViewsHaveNoSecrets(t *testing.T) {
	url := storetest.RequirePostgres(t)
	h := storetest.New(t)
	if h.Kind != "postgres" {
		t.Setenv("TEST_STORE", "postgres")
		h = storetest.New(t)
	}
	storetest.Fixture(t, h.Store)
	b, err := open(t, url, h.Schema(), func(o *pgstore.Options) { o.NoLock = true; o.NoMigrate = true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	for _, v := range []string{"v_payment_history", "v_revenue_monthly", "v_equipment_current", "v_customers"} {
		rows, err := b.Pool().Query(ctx, "SELECT * FROM "+v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		for _, f := range rows.FieldDescriptions() {
			switch f.Name {
			case "rest_pass", "hs_pass", "join_hash", "national_id_enc", "national_id_hmac", "national_id_last4":
				t.Errorf("view %s exposes secret column %s", v, f.Name)
			}
		}
		rows.Close()
	}
	var month string
	var rev int
	if err := b.Pool().QueryRow(ctx, "SELECT month_eat, revenue_kes FROM v_revenue_monthly").Scan(&month, &rev); err != nil || month != "2026-03" || rev != 50 {
		t.Fatalf("v_revenue_monthly: %q %d %v", month, rev, err)
	}
}

func TestForeignKeyViolationRollsBackMemory(t *testing.T) {
	url := storetest.RequirePostgres(t)
	t.Setenv("TEST_STORE", "postgres")
	h := storetest.New(t)
	_ = url
	err := h.Store.Update(func(d *store.Data) error {
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "no-such-site", Flat: "1", CreatedAt: time.Now()}
		return nil
	})
	if !errors.Is(err, store.ErrPersist) || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("want a foreign key failure, got %v", err)
	}
	if _, ok := h.Store.Snapshot().Accounts["a1"]; ok {
		t.Fatal("memory kept the row the database refused")
	}
	if h.Store.Healthy() != nil {
		t.Fatal("a constraint failure must not degrade the store")
	}
	// Unique constraints also come back as plain errors.
	h.Store.Update(func(d *store.Data) error {
		d.Sites["s1"] = store.Site{ID: "s1", Name: "A", Mode: "env", CreatedAt: time.Now()}
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s1", Code: "AAA111", Flat: "1", MAC: "AA:BB:CC:DD:EE:01", CreatedAt: time.Now()}
		return nil
	})
	err = h.Store.Update(func(d *store.Data) error {
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: "s1", Code: "AAA111", Flat: "2", CreatedAt: time.Now()}
		return nil
	})
	if !errors.Is(err, store.ErrPersist) || !strings.Contains(err.Error(), "accounts_code") {
		t.Fatalf("want a duplicate code failure, got %v", err)
	}
	err = h.Store.Update(func(d *store.Data) error {
		d.Accounts["a3"] = store.Account{ID: "a3", SiteID: "s1", Code: "BBB222", Flat: "3", MAC: "aa:bb:cc:dd:ee:01", CreatedAt: time.Now()}
		return nil
	})
	if !errors.Is(err, store.ErrDuplicateMAC) {
		t.Fatalf("want a duplicate MAC failure (case-insensitive), got %v", err)
	}
	if h.Store.Healthy() != nil {
		t.Fatal("a duplicate MAC must not degrade the store")
	}
	// Payments can never be deleted.
	h.Store.Update(func(d *store.Data) error {
		d.Payments["K1"] = store.Payment{Key: "K1", Method: "test", Status: "applied", PaidAt: time.Now(), CreatedAt: time.Now()}
		return nil
	})
	err = h.Store.Update(func(d *store.Data) error { delete(d.Payments, "K1"); return nil })
	if err == nil || !strings.Contains(err.Error(), "never deleted") {
		t.Fatalf("payment delete: %v", err)
	}
	if _, ok := h.Store.Snapshot().Payments["K1"]; !ok {
		t.Fatal("payment vanished from memory")
	}
}

func TestAdvisoryLockBlocksSecondOpener(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	a, err := open(t, url, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, url, schema, nil); err == nil || !strings.Contains(err.Error(), "already using") {
		t.Fatalf("second opener: %v", err)
	}
	// A read-only tool may look without the lock.
	r, err := open(t, url, schema, func(o *pgstore.Options) { o.NoLock = true })
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	a.Close()
	b, err := open(t, url, schema, nil) // released on close
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	b.Close()
}

func TestOrderWindow(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	ctx := context.Background()
	b, err := open(t, url, schema, func(o *pgstore.Options) { o.OrderWindow = 30 * 24 * time.Hour })
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenWith(b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.Update(func(d *store.Data) error {
		d.Sites["s1"] = store.Site{ID: "s1", Name: "A", Mode: "env", CreatedAt: now}
		d.Orders["new"] = store.Order{ID: "new", SiteID: "s1", Method: "test", Status: "paid", CreatedAt: now.Add(-24 * time.Hour)}
		d.Orders["old"] = store.Order{ID: "old", SiteID: "s1", Method: "test", Status: "paid", CreatedAt: now.Add(-90 * 24 * time.Hour)}
		d.Orders["oldpending"] = store.Order{ID: "oldpending", SiteID: "s1", Method: "test", Status: "pending", CreatedAt: now.Add(-90 * 24 * time.Hour)}
		return nil
	})
	s.Close()
	b2, _ := open(t, url, schema, func(o *pgstore.Options) { o.OrderWindow = 30 * 24 * time.Hour })
	s2, err := store.OpenWith(b2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap := s2.Snapshot()
	if _, ok := snap.Orders["new"]; !ok {
		t.Fatal("recent order not loaded")
	}
	if _, ok := snap.Orders["oldpending"]; !ok {
		t.Fatal("pending orders always load")
	}
	if _, ok := snap.Orders["old"]; ok {
		t.Fatal("old finished order should be outside the window")
	}
	// Rows outside the window are never treated as deletes, and stay in the database.
	if err := s2.Update(func(d *store.Data) error {
		o := d.Orders["new"]
		o.Reason = "touched"
		d.Orders["new"] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	b2.Pool().QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n)
	if n != 3 {
		t.Fatalf("orders in the database: %d", n)
	}
	// LoadAll sees them all.
	all, _ := b2.LoadAll(ctx)
	if len(all.Orders) != 3 {
		t.Fatalf("LoadAll: %d orders", len(all.Orders))
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportFromV2AndV3Fixtures(t *testing.T) {
	url := storetest.RequirePostgres(t)
	for name, fixture := range map[string]string{"v2": storetest.V2JSON, "v3": storetest.V3JSON} {
		t.Run(name, func(t *testing.T) {
			schema := storetest.NewSchema(t, url)
			p := writeFile(t, t.TempDir(), "ispbill.json", fixture)
			d, from, err := store.ReadJSON(p)
			if err != nil {
				t.Fatal(err)
			}
			b, err := open(t, url, schema, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			ctx := context.Background()
			want, err := b.Import(ctx, d, pgstore.ImportOptions{From: p})
			if err != nil {
				t.Fatal(err)
			}
			got, err := b.Counts(ctx)
			if err != nil || got.Diff(want) != "" {
				t.Fatalf("counts differ after import: %v %s", err, got.Diff(want))
			}
			if from == 2 && (got.Rows["accounts"] != 1 || got.Rows["payments"] != 1 || got.AppliedKES != 50 || got.Rows["equipment"] != 1) {
				t.Fatalf("v2 counts %+v", got)
			}
			if from == 3 && (got.Rows["accounts"] != 6 || got.Closed != 2 || got.Rows["customers"] != 2 || got.Rows["tenancies"] != 3 || got.AppliedKES != 100) {
				t.Fatalf("v3 counts %+v", got)
			}
			if v, ok, _ := b.Meta(ctx, "imported_at"); !ok || v == "" {
				t.Fatal("imported_at not recorded")
			}
			if v, _, _ := b.Meta(ctx, "imported_from"); v != p {
				t.Fatalf("imported_from %q", v)
			}
			// Loading it back gives the same data (orders are inside the huge window).
			loaded, err := b.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*loaded, *d) {
				t.Fatalf("loaded data differs from the imported data")
			}
			// A second import is refused unless forced.
			if _, err := b.Import(ctx, d, pgstore.ImportOptions{}); !errors.Is(err, pgstore.ErrNotEmpty) {
				t.Fatalf("second import: %v", err)
			}
			if _, err := b.Import(ctx, d, pgstore.ImportOptions{Force: true}); err != nil {
				t.Fatalf("forced import: %v", err)
			}
			if got2, _ := b.Counts(ctx); got2.Diff(want) != "" {
				t.Fatal("forced import changed counts")
			}
		})
	}
}

func TestImportRefusesBadDataInPlainWords(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	p := writeFile(t, t.TempDir(), "ispbill.json", strings.Replace(storetest.V3JSON, `"code":"BBB333"`, `"code":"AAA222"`, 1))
	d, _, err := store.ReadJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := open(t, url, schema, nil)
	defer b.Close()
	_, err = b.Import(context.Background(), d, pgstore.ImportOptions{})
	var ve *pgstore.ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0], "AAA222") || !strings.Contains(ve.Problems[0], "Flat 1") || !strings.Contains(ve.Problems[0], "Flat 2") {
		t.Fatalf("want one plain-words problem naming the code and both flats, got %v", err)
	}
	if empty, _ := b.IsEmpty(context.Background()); !empty {
		t.Fatal("a refused import must leave the database empty")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	url := storetest.RequirePostgres(t)
	t.Setenv("TEST_STORE", "postgres")
	h := storetest.New(t)
	storetest.Fixture(t, h.Store)
	ctx := context.Background()
	b, err := open(t, url, h.Schema(), func(o *pgstore.Options) { o.NoLock = true; o.NoMigrate = true })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	all, err := b.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "export.json")
	if err := store.WriteJSON(out, all); err != nil {
		t.Fatal(err)
	}
	d, from, err := store.ReadJSON(out)
	if err != nil || from != store.FormatVersion {
		t.Fatalf("read export: %v %d", err, from)
	}
	if !reflect.DeepEqual(*d, *all) {
		t.Fatal("export-json then read changed the data")
	}
	schema2 := storetest.NewSchema(t, url)
	b2, _ := open(t, url, schema2, nil)
	defer b2.Close()
	if _, err := b2.Import(ctx, d, pgstore.ImportOptions{From: out}); err != nil {
		t.Fatal(err)
	}
	c1, _ := b.Counts(ctx)
	c2, _ := b2.Counts(ctx)
	if c1.Diff(c2) != "" {
		t.Fatalf("round trip differs: %s", c1.Diff(c2))
	}
	d2, _ := b2.LoadAll(ctx)
	if !reflect.DeepEqual(*d2, *all) {
		t.Fatal("data differs after export, import, load")
	}
}

func TestNoTCPSecretsInErrors(t *testing.T) {
	_, err := pgstore.Open(context.Background(), "postgres://user:s3cr3t@127.0.0.1:1/db?sslmode=disable&connect_timeout=1", pgstore.Options{})
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("password leaked into the error: %v", err)
	}
}

// Partial unique indexes cannot be deferred, so Commit must write updates
// before inserts and break swap cycles itself, whatever the Go map order.
func TestCommitOrderingAgainstUniqueIndexes(t *testing.T) {
	storetest.RequirePostgres(t)
	t.Setenv("TEST_STORE", "postgres")
	h := storetest.New(t)
	now := time.Now()
	const m1, m2, m3 = "AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02", "AA:BB:CC:DD:EE:03"
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("seed", h.Store.Update(func(d *store.Data) error {
		d.Sites["s1"] = store.Site{ID: "s1", Name: "A", Mode: "env", CreatedAt: now}
		d.Sites["s2"] = store.Site{ID: "s2", Name: "B", Mode: "env", CreatedAt: now}
		d.Customers["c0"] = store.Customer{ID: "c0", Name: "C0", CreatedAt: now}
		d.Accounts["a1"] = store.Account{ID: "a1", SiteID: "s1", Code: "AAA111", Flat: "1", MAC: m1, CreatedAt: now}
		d.Accounts["a2"] = store.Account{ID: "a2", SiteID: "s1", Code: "BBB222", Flat: "2", MAC: m2, CreatedAt: now}
		d.Tenancies["t0"] = store.Tenancy{ID: "t0", CustomerID: "c0", AccountID: "a1", MoveIn: now}
		return nil
	}))
	// Tenancy handover, 50 times: close the open one, insert a new open one.
	for i := 0; i < 50; i++ {
		id := "t" + string(rune('A'+i/26)) + string(rune('a'+i%26))
		must("handover", h.Store.Update(func(d *store.Data) error {
			for k, tn := range d.Tenancies {
				if tn.AccountID == "a1" && tn.MoveOut.IsZero() {
					tn.MoveOut = now
					d.Tenancies[k] = tn
				}
			}
			d.Tenancies[id] = store.Tenancy{ID: id, CustomerID: "c0", AccountID: "a1", MoveIn: now}
			return nil
		}))
	}
	// Two accounts swap MACs and codes in one Update.
	for i := 0; i < 20; i++ {
		must("swap", h.Store.Update(func(d *store.Data) error {
			a1, a2 := d.Accounts["a1"], d.Accounts["a2"]
			a1.MAC, a2.MAC = a2.MAC, a1.MAC
			a1.Code, a2.Code = a2.Code, a1.Code
			d.Accounts["a1"], d.Accounts["a2"] = a1, a2
			return nil
		}))
	}
	// One account gives up its MAC and a brand-new account takes it.
	must("hand over a MAC", h.Store.Update(func(d *store.Data) error {
		a1 := d.Accounts["a1"]
		old := a1.MAC
		a1.MAC = m3
		d.Accounts["a1"] = a1
		d.Accounts["a3"] = store.Account{ID: "a3", SiteID: "s1", Code: "CCC333", Flat: "3", MAC: old, CreatedAt: now}
		return nil
	}))
	// Closing an account frees its MAC for a new one; moving it to another site works too.
	must("close and reuse", h.Store.Update(func(d *store.Data) error {
		a3 := d.Accounts["a3"]
		delete(d.Accounts, "a3")
		a3.ClosedAt = now
		d.ClosedAccounts["a3"] = a3
		d.Accounts["a4"] = store.Account{ID: "a4", SiteID: "s1", Code: "DDD444", Flat: "4", MAC: a3.MAC, CreatedAt: now}
		return nil
	}))
	must("move site", h.Store.Update(func(d *store.Data) error {
		a2 := d.Accounts["a2"]
		a2.SiteID = "s2"
		d.Accounts["a2"] = a2
		a4 := d.Accounts["a4"]
		a4.SiteID = "s2"
		a4.MAC = "aa:bb:cc:dd:ee:77"
		d.Accounts["a4"] = a4
		return nil
	}))
	// Memory and database agree after all of it.
	h2 := h.Reopen()
	snap := h2.Snapshot()
	if len(snap.Accounts) != 3 || len(snap.ClosedAccounts) != 1 || len(snap.Tenancies) != 51 {
		t.Fatalf("reloaded: %d open, %d closed accounts, %d tenancies", len(snap.Accounts), len(snap.ClosedAccounts), len(snap.Tenancies))
	}
}
