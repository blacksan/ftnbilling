package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"ispbill/internal/store"
)

// Tables lists every data table in report order.
var Tables = []string{"sites", "apartments", "apartment_sites", "packages", "customers", "equipment", "accounts", "tenancies", "orders", "vouchers",
	"payments", "devices", "registrations", "onus", "onu_tasks", "oui_cache", "tasks", "equipment_events", "notes", "settings", "events", "admin_users", "admin_sessions", "api_devices"}

// Counts are row counts per table plus the sum of applied payments.
type Counts struct {
	Rows       map[string]int `json:"rows"`
	AppliedKES int64          `json:"applied_kes"`
	// Closed counts closed accounts (they are rows of "accounts").
	Closed int `json:"closed_accounts"`
	// MACConflicts counts open accounts flagged mac_conflict (see store.Conflicts).
	MACConflicts int `json:"mac_conflicts"`
	// SchemaVersion is the database's schema version (0 = none yet). Tables
	// newer than it are counted as zero.
	SchemaVersion int `json:"schema_version"`
}

// CountsOf counts what Import would write for d.
func CountsOf(d *store.Data) Counts {
	c := Counts{Rows: map[string]int{
		"sites": len(d.Sites), "packages": len(d.Packages), "customers": len(d.Customers), "equipment": len(d.Equipment),
		"accounts": len(d.Accounts) + len(d.ClosedAccounts), "tenancies": len(d.Tenancies), "apartments": len(d.Apartments), "apartment_sites": len(d.ApartmentSites),
		"orders": len(d.Orders), "vouchers": len(d.Vouchers), "payments": len(d.Payments), "devices": len(d.Devices), "tasks": len(d.Tasks),
		"equipment_events": len(d.EquipmentEvents), "notes": len(d.Notes), "settings": len(d.Settings), "events": len(d.Events),
		"admin_users": len(d.AdminUsers), "admin_sessions": len(d.AdminSessions), "api_devices": len(d.APIDevices), "registrations": len(d.Registrations), "onus": len(d.ONUs), "onu_tasks": len(d.ONUTasks), "oui_cache": len(d.OUICache),
	}, Closed: len(d.ClosedAccounts), MACConflicts: len(d.MACConflicts())}
	for _, p := range d.Payments {
		if p.Status == store.PayApplied {
			c.AppliedKES += int64(p.AmountKES)
		}
	}
	return c
}

// Counts reads the live row counts and applied sum from the database. It
// also works on a database older than this program (db-check on an old dump,
// the backup drill): tables and columns that do not exist yet count as zero and
// SchemaVersion says how old it is.
func (b *Backend) Counts(ctx context.Context) (Counts, error) {
	c := Counts{Rows: map[string]int{}}
	exists := func(table string) (bool, error) {
		var reg *string
		err := b.pool.QueryRow(ctx, "SELECT to_regclass($1)::text", table).Scan(&reg)
		return reg != nil, err
	}
	if ok, err := exists("schema_migrations"); err != nil {
		return c, err
	} else if ok {
		if err := b.pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&c.SchemaVersion); err != nil {
			return c, err
		}
	}
	have := map[string]bool{}
	for _, t := range Tables {
		ok, err := exists(t)
		if err != nil {
			return c, err
		}
		have[t] = ok
		if !ok {
			c.Rows[t] = 0
			continue
		}
		var n int
		if err := b.pool.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
			return c, err
		}
		c.Rows[t] = n
	}
	if have["payments"] {
		if err := b.pool.QueryRow(ctx, "SELECT coalesce(sum(amount_kes), 0) FROM payments WHERE status = 'applied'").Scan(&c.AppliedKES); err != nil {
			return c, err
		}
	}
	if have["accounts"] {
		if err := b.pool.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE closed_at IS NOT NULL").Scan(&c.Closed); err != nil {
			return c, err
		}
		var hasCol bool
		if err := b.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'accounts' AND column_name = 'mac_conflict')`).Scan(&hasCol); err != nil {
			return c, err
		}
		if hasCol {
			if err := b.pool.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE mac_conflict AND closed_at IS NULL").Scan(&c.MACConflicts); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}

// Diff describes, in words, how two counts differ ("" = identical).
func (c Counts) Diff(want Counts) string {
	var out []string
	for _, t := range Tables {
		if c.Rows[t] != want.Rows[t] {
			out = append(out, fmt.Sprintf("%s: database has %d, expected %d", t, c.Rows[t], want.Rows[t]))
		}
	}
	if c.AppliedKES != want.AppliedKES {
		out = append(out, fmt.Sprintf("applied payments sum: database has KES %d, expected KES %d", c.AppliedKES, want.AppliedKES))
	}
	if c.Closed != want.Closed {
		out = append(out, fmt.Sprintf("closed accounts: database has %d, expected %d", c.Closed, want.Closed))
	}
	return strings.Join(out, "; ")
}

// IsEmpty reports whether the database holds no business data at all.
func (b *Backend) IsEmpty(ctx context.Context) (bool, error) {
	for _, t := range []string{"sites", "packages", "accounts", "customers", "orders", "payments", "vouchers", "equipment"} {
		var n int
		if err := b.pool.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}

// Meta reads a meta value.
func (b *Backend) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := b.pool.QueryRow(ctx, "SELECT value FROM meta WHERE key = $1", key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetMeta writes a meta value.
func (b *Backend) SetMeta(ctx context.Context, key, value string) error {
	_, err := b.pool.Exec(ctx, "INSERT INTO meta (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", key, value)
	return err
}

// ImportOptions control Import.
type ImportOptions struct {
	From  string // recorded in meta, e.g. the file path
	Force bool   // replace whatever the database holds
}

// ErrNotEmpty is returned by Import when the database already has data.
var ErrNotEmpty = errors.New("the database already holds data; use -force to replace it")

// Import loads d into an empty database in one transaction (COPY), records
// meta, and verifies row counts and the applied-payments sum afterwards.
func (b *Backend) Import(ctx context.Context, d *store.Data, opt ImportOptions) (Counts, error) {
	// A JSON file is a copy made elsewhere and possibly long ago: browser
	// sessions are not carried over and every app device must sign in again
	// (its tokens are dropped and it is marked revoked).
	cp := *d
	cp.AdminSessions = map[string]store.AdminSession{}
	cp.APIDevices = make(map[string]store.APIDevice, len(d.APIDevices))
	for k, x := range d.APIDevices {
		x.AccessHash, x.RefreshHash, x.RefreshPrevHash = "", "", ""
		x.AccessExpiresAt, x.RefreshExpiresAt = time.Time{}, time.Time{}
		if x.RevokedAt.IsZero() {
			x.RevokedAt, x.RevokedReason = time.Now().UTC(), "imported from a JSON file: sign in again"
		}
		cp.APIDevices[k] = x
	}
	d = &cp
	want := CountsOf(d)
	if problems := store.Validate(d); len(problems) > 0 {
		return want, &ValidationError{Problems: problems}
	}
	empty, err := b.IsEmpty(ctx)
	if err != nil {
		return want, err
	}
	if !empty && !opt.Force {
		return want, ErrNotEmpty
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return want, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, lockSQL); err != nil {
		return want, err
	}
	if !empty {
		if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(Tables, ", ")+", meta CASCADE"); err != nil {
			return want, err
		}
	}
	accounts := make([]store.Account, 0, len(d.Accounts)+len(d.ClosedAccounts))
	for _, a := range d.Accounts {
		accounts = append(accounts, a)
	}
	for _, a := range d.ClosedAccounts {
		accounts = append(accounts, a)
	}
	if err := copyRows(ctx, tx, sitesC, mapVals(d.Sites)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, apartmentsC, mapVals(d.Apartments)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, apartmentSitesC, mapVals(d.ApartmentSites)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, packagesC, mapVals(d.Packages)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, customersC, mapVals(d.Customers)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, equipmentC, mapVals(d.Equipment)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, accountsC, accounts); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, tenanciesC, mapVals(d.Tenancies)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, ordersC, mapVals(d.Orders)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, vouchersC, mapVals(d.Vouchers)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, paymentsC, mapVals(d.Payments)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, devicesC, mapVals(d.Devices)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, registrationsC, mapVals(d.Registrations)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, onusC, mapVals(d.ONUs)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, onuTasksC, mapVals(d.ONUTasks)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, ouiCacheC, mapVals(d.OUICache)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, tasksC, mapVals(d.Tasks)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, eqEventsC, mapVals(d.EquipmentEvents)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, notesC, mapVals(d.Notes)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, settingsC, mapVals(d.Settings)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, adminUsersC, mapVals(d.AdminUsers)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, adminSessionsC, mapVals(d.AdminSessions)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, apiDevicesC, mapVals(d.APIDevices)); err != nil {
		return want, err
	}
	if err := copyRows(ctx, tx, eventsC, d.Events); err != nil {
		return want, err
	}
	rawCounts, _ := json.Marshal(want)
	for k, v := range map[string]string{
		"imported_from": opt.From, "imported_at": time.Now().UTC().Format(time.RFC3339), "imported_counts": string(rawCounts),
	} {
		if _, err := tx.Exec(ctx, "INSERT INTO meta (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value", k, v); err != nil {
			return want, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return want, err
	}
	got, err := b.Counts(ctx)
	if err != nil {
		return want, err
	}
	if diff := got.Diff(want); diff != "" {
		return want, fmt.Errorf("import verification failed: %s", diff)
	}
	return want, nil
}

// ValidationError carries the plain-words problems found before an import.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("the data cannot be imported yet (%d problem(s)):\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

func mapVals[V any](m map[string]V) []V {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]V, 0, len(m))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func copyRows[V any](ctx context.Context, tx pgx.Tx, c *codec[V], rows []V) error {
	if len(rows) == 0 {
		return nil
	}
	src := make([][]any, len(rows))
	for i, r := range rows {
		src[i] = c.args(r)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{c.name}, c.cols, pgx.CopyFromRows(src)); err != nil {
		return fmt.Errorf("import %s: %w", c.name, err)
	}
	return nil
}
