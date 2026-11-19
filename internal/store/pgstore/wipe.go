package pgstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"ispbill/internal/store"
)

// WipeTables are the tables "start fresh" empties. The list is explicit, and
// TestWipeClassifiesEveryTable fails when a migration adds a table that is in
// neither this list nor KeptTables, so a new table is never wiped (or kept)
// by accident.
//
// TRUNCATE ... CASCADE also empties every table that has a foreign key to a
// listed table; all of those are listed too, and none of the kept tables
// references a listed one (admin_sessions and api_devices reference
// admin_users, not the other way round), so CASCADE reaches nothing else.
var WipeTables = []string{
	"sites",            // routers: tunnel keys and addresses, join tokens and REST passwords
	"packages",         // the price list (the service seeds the defaults again at start)
	"customers",        // clients
	"equipment",        // ONTs, splitters, routers as stock items
	"accounts",         // flats, open and closed (the leases/IPs/MACs live on the row)
	"tenancies",        // who lived in which flat
	"site_contacts",    // building contacts
	"orders",           // pay-page orders and M-Pesa callbacks results
	"vouchers",         // voucher codes
	"payments",         // the ledger (M-Pesa receipts, cash, refunds)
	"devices",          // unregistered MACs / devices seen by the routers
	"tasks",            // router task and retry queue
	"equipment_events", // equipment history
	"notes",            // CRM notes
	"events",           // event and audit log
	"admin_sessions",   // browser sessions: everybody signs in again
	"api_devices",      // phone sessions: the phones sign in again
}

// KeptTables are the tables a wipe never truncates, and why.
var KeptTables = map[string]string{
	"admin_users":       "staff accounts: passwords, authenticator secrets and recovery codes are access, not business data",
	"settings":          "configuration (M-Pesa credentials, branding, alerts, admin password hash); only the per-router rows 'site.%' are deleted",
	"meta":              "import bookkeeping (imported_at): deleting it would let an old ispbill.json import again",
	"schema_migrations": "the schema version: the app must still start on the wiped database",
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// wipeCounts are the queries behind the report, in print order. They match
// what WipeBusiness counts for the JSON file (open and closed flats are both
// rows of accounts; router settings are the 'site.%' rows of settings).
var wipeCounts = []struct{ kind, sql string }{
	{store.WipeRouters, "SELECT count(*) FROM sites"},
	{store.WipePackages, "SELECT count(*) FROM packages"},
	{store.WipeCustomers, "SELECT count(*) FROM customers"},
	{store.WipeAccounts, "SELECT count(*) FROM accounts"},
	{store.WipeTenancies, "SELECT count(*) FROM tenancies"},
	{store.WipeEquipment, "SELECT count(*) FROM equipment"},
	{store.WipeEquipmentEvents, "SELECT count(*) FROM equipment_events"},
	{store.WipeContacts, "SELECT count(*) FROM site_contacts"},
	{store.WipeOrders, "SELECT count(*) FROM orders"},
	{store.WipeVouchers, "SELECT count(*) FROM vouchers"},
	{store.WipePayments, "SELECT count(*) FROM payments"},
	{store.WipeDevices, "SELECT count(*) FROM devices"},
	{store.WipeTasks, "SELECT count(*) FROM tasks"},
	{store.WipeEvents, "SELECT count(*) FROM events"},
	{store.WipeNotes, "SELECT count(*) FROM notes"},
	{store.WipeRouterSettings, "SELECT count(*) FROM settings WHERE left(key, 5) = 'site.'"},
	{store.WipeSessions, "SELECT count(*) FROM admin_sessions"},
	{store.WipePhones, "SELECT count(*) FROM api_devices"},
}

func wipeReport(ctx context.Context, q rowQuerier) (store.WipeReport, error) {
	var r store.WipeReport
	for _, c := range wipeCounts {
		var n int
		if err := q.QueryRow(ctx, c.sql).Scan(&n); err != nil {
			return r, fmt.Errorf("%s: %w", c.kind, err)
		}
		r.Delete = append(r.Delete, store.WipeItem{Kind: c.kind, Count: n})
	}
	if err := q.QueryRow(ctx, "SELECT count(*) FROM admin_users").Scan(&r.KeptAdminUser); err != nil {
		return r, err
	}
	if err := q.QueryRow(ctx, "SELECT count(*) FROM settings WHERE left(key, 5) <> 'site.'").Scan(&r.KeptSettings); err != nil {
		return r, err
	}
	return r, nil
}

// WipePreview counts what Wipe would delete. It changes nothing.
func (b *Backend) WipePreview(ctx context.Context) (store.WipeReport, error) {
	return wipeReport(ctx, b.pool)
}

// Wipe deletes every business entry in one transaction and returns what it
// deleted: TRUNCATE of WipeTables (RESTART IDENTITY CASCADE) and the per-router
// rows of settings. Admin users, the other settings, meta and the schema
// version stay. It refuses a database whose schema is not the latest, so the
// table list is known to be right; either everything is deleted or nothing.
func (b *Backend) Wipe(ctx context.Context) (store.WipeReport, error) {
	have, err := b.AppliedVersion(ctx)
	if err != nil {
		return store.WipeReport{}, err
	}
	if have != SchemaVersion() {
		return store.WipeReport{}, fmt.Errorf("the database is at schema version %d but this program is at %d: run the installer (or: ispbill migrate) first", have, SchemaVersion())
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return store.WipeReport{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, lockSQL); err != nil {
		return store.WipeReport{}, err
	}
	rep, err := wipeReport(ctx, tx)
	if err != nil {
		return rep, err
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(WipeTables, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		return rep, err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM settings WHERE left(key, 5) = 'site.'"); err != nil {
		return rep, err
	}
	return rep, tx.Commit(ctx)
}
