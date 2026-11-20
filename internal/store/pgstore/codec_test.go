package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"ispbill/internal/store"
)

// fillValue sets every field of v (recursively) to a non-zero value.
func fillValue(v reflect.Value, name string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x" + strings.ToLower(name))
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice: // []string (recovery codes)
		sl := reflect.MakeSlice(v.Type(), 1, 1)
		sl.Index(0).SetString("x" + strings.ToLower(name))
		v.Set(sl)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		k.SetString("p1")
		e := reflect.New(v.Type().Elem()).Elem()
		fillValue(e, "ov")
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Date(2026, 2, 3, 4, 5, 6, 789000000, time.UTC)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fillValue(v.Field(i), v.Type().Field(i).Name)
		}
	default:
		panic("fillValue: unsupported kind " + v.Kind().String())
	}
}

// eachField calls f once per leaf field of V (Site.Join is flattened), with a
// fresh zero value in which only that field is set.
func eachField[V any](t *testing.T, f func(name string, v V)) {
	var walk func(path string, typ reflect.Type, idx []int)
	walk = func(path string, typ reflect.Type, idx []int) {
		for i := 0; i < typ.NumField(); i++ {
			ft := typ.Field(i)
			p := append(append([]int(nil), idx...), i)
			if ft.Type.Kind() == reflect.Struct && ft.Type != reflect.TypeOf(time.Time{}) {
				walk(path+ft.Name+".", ft.Type, p)
				continue
			}
			var v V
			fillValue(reflect.ValueOf(&v).Elem().FieldByIndex(p), ft.Name)
			f(path+ft.Name, v)
		}
	}
	walk("", reflect.TypeOf((*V)(nil)).Elem(), nil)
}

// guardCodec: every Go field must reach the database arguments.
func guardCodec[V any](t *testing.T, c *codec[V]) {
	t.Helper()
	var zero V
	base := c.args(zero)
	if len(base) != len(c.cols) {
		t.Fatalf("%s: %d args for %d columns", c.name, len(base), len(c.cols))
	}
	if n := len(c.dst(&zero)); n != len(c.cols) {
		t.Fatalf("%s: %d scan targets for %d columns", c.name, n, len(c.cols))
	}
	eachField(t, func(name string, v V) {
		if reflect.DeepEqual(base, c.args(v)) {
			t.Errorf("%s: field %s is not written to the database (add it to the codec and the migration)", c.name, name)
		}
	})
}

func TestCodecsCoverEveryField(t *testing.T) {
	guardCodec(t, sitesC)
	guardCodec(t, packagesC)
	guardCodec(t, customersC)
	guardCodec(t, equipmentC)
	guardCodec(t, accountsC)
	guardCodec(t, tenanciesC)
	guardCodec(t, apartmentsC)
	guardCodec(t, apartmentSitesC)
	guardCodec(t, ordersC)
	guardCodec(t, vouchersC)
	guardCodec(t, paymentsC)
	guardCodec(t, devicesC)
	guardCodec(t, tasksC)
	guardCodec(t, eqEventsC)
	guardCodec(t, notesC)
	guardCodec(t, settingsC)
	guardCodec(t, eventsC)
	guardCodec(t, adminUsersC)
	guardCodec(t, adminSessionsC)
	guardCodec(t, apiDevicesC)
}

// alterAdds lists the column names added to table by "ALTER TABLE name ... ADD COLUMN col".
func alterAdds(sql, table string) []string {
	var out []string
	for _, stmt := range strings.Split(sql, ";") {
		stmt = strings.Join(strings.Fields(stmt), " ")
		i := strings.Index(stmt, "ALTER TABLE "+table+" ")
		if i < 0 {
			continue
		}
		for _, part := range strings.Split(stmt[i:], "ADD COLUMN ")[1:] {
			out = append(out, strings.Fields(part)[0])
		}
	}
	return out
}

func TestCodecColumnsExist(t *testing.T) {
	// Every column the codec writes appears in the migrations (a typo guard that needs no database).
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, m := range ms {
		all.WriteString(m.sql)
	}
	for name, cols := range map[string][]string{"sites": sitesC.cols, "packages": packagesC.cols, "customers": customersC.cols, "equipment": equipmentC.cols,
		"accounts": accountsC.cols, "tenancies": tenanciesC.cols, "apartments": apartmentsC.cols, "apartment_sites": apartmentSitesC.cols, "orders": ordersC.cols, "vouchers": vouchersC.cols,
		"payments": paymentsC.cols, "devices": devicesC.cols, "tasks": tasksC.cols, "equipment_events": eqEventsC.cols, "notes": notesC.cols, "settings": settingsC.cols, "events": eventsC.cols,
		"admin_users": adminUsersC.cols, "admin_sessions": adminSessionsC.cols, "api_devices": apiDevicesC.cols} {
		i := strings.Index(all.String(), "CREATE TABLE "+name+" (")
		if i < 0 {
			t.Fatalf("table %s not in migrations", name)
		}
		body := all.String()[i:]
		body = body[:strings.Index(body, ");")+1]
		// Columns added later by ALTER TABLE (migration 0004 and after).
		for _, a := range alterAdds(all.String(), name) {
			body += ", " + a + " "
		}
		for _, c := range cols {
			if !strings.Contains(body, " "+c+" ") && !strings.Contains(body, "("+c+" ") && !strings.Contains(body, ", "+c+" ") && !strings.Contains(body, "\n  "+c+" ") {
				t.Errorf("table %s has no column %s", name, c)
			}
		}
	}
}

// ---- database round trip ---------------------------------------------------

func testURL(t *testing.T) string {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set: Postgres test skipped")
	}
	return u
}

func roundTrip[V any](t *testing.T, ctx context.Context, tx pgx.Tx, c *codec[V], fix func(*V), norm func(V) V) {
	t.Helper()
	var v V
	fillValue(reflect.ValueOf(&v).Elem(), "")
	if fix != nil {
		fix(&v)
	}
	v = norm(v)
	if _, err := tx.Exec(ctx, c.upsql, c.args(v)...); err != nil {
		t.Fatalf("%s: insert: %v", c.name, err)
	}
	// Second upsert of the same key must update, not fail.
	if _, err := tx.Exec(ctx, c.upsql, c.args(v)...); err != nil {
		t.Fatalf("%s: upsert: %v", c.name, err)
	}
	rows, err := tx.Query(ctx, c.selectSQL(""))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.scan(rows)
	if err != nil || len(got) != 1 {
		t.Fatalf("%s: read back %d rows: %v", c.name, len(got), err)
	}
	if !reflect.DeepEqual(got[0], v) {
		t.Errorf("%s: row changed in the database round trip\n got %+v\nwant %+v", c.name, got[0], v)
	}
}

func TestCodecRoundTripEveryField(t *testing.T) {
	url := testURL(t)
	ctx := context.Background()
	var rb [4]byte
	rand.Read(rb[:])
	schema := "codec_" + hex.EncodeToString(rb[:])
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	b, err := Open(ctx, url, Options{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	// One transaction with foreign keys deferred and never committed, so rows
	// can reference things that do not exist.
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	roundTrip(t, ctx, tx, sitesC, func(v *store.Site) { v.Mode = "wg"; v.Lat, v.Lng = 12.5, 40.25; v.AccessMode = store.AccessIPoE }, store.Site.Norm)
	roundTrip(t, ctx, tx, packagesC, nil, store.Package.Norm)
	roundTrip(t, ctx, tx, customersC, nil, store.Customer.Norm)
	roundTrip(t, ctx, tx, equipmentC, func(v *store.Equipment) { v.Kind, v.Status = "ont", "installed" }, store.Equipment.Norm)
	roundTrip(t, ctx, tx, accountsC, func(v *store.Account) { v.MAC = "AA:BB:CC:DD:EE:FF"; v.IP = "10.20.0.7" }, store.Account.Norm)
	roundTrip(t, ctx, tx, tenanciesC, func(v *store.Tenancy) { v.MoveOut = v.MoveIn.Add(time.Hour) }, store.Tenancy.Norm)
	roundTrip(t, ctx, tx, apartmentsC, nil, store.Apartment.Norm)
	roundTrip(t, ctx, tx, apartmentSitesC, nil, store.ApartmentSite.Norm)
	roundTrip(t, ctx, tx, ordersC, func(v *store.Order) { v.Status = "paid" }, store.Order.Norm)
	roundTrip(t, ctx, tx, vouchersC, nil, store.Voucher.Norm)
	roundTrip(t, ctx, tx, paymentsC, func(v *store.Payment) { v.Key, v.Status = "KEY1", "applied" }, store.Payment.Norm)
	roundTrip(t, ctx, tx, devicesC, nil, store.Device.Norm)
	roundTrip(t, ctx, tx, tasksC, nil, store.Task.Norm)
	roundTrip(t, ctx, tx, eqEventsC, func(v *store.EquipmentEvent) { v.Kind = "assigned" }, store.EquipmentEvent.Norm)
	roundTrip(t, ctx, tx, notesC, nil, store.Note.Norm)
	roundTrip(t, ctx, tx, settingsC, nil, store.Setting.Norm)
	roundTrip(t, ctx, tx, eventsC, nil, store.Event.Norm)
	roundTrip(t, ctx, tx, adminUsersC, func(v *store.AdminUser) { v.Role = "operator" }, store.AdminUser.Norm)
	roundTrip(t, ctx, tx, adminSessionsC, nil, store.AdminSession.Norm)
	roundTrip(t, ctx, tx, apiDevicesC, nil, store.APIDevice.Norm)
}
