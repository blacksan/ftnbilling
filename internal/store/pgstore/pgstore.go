// Package pgstore is the Postgres backend of the store: it loads everything
// into memory at start and writes each Update's change set in one transaction.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ispbill/internal/store"
)

// Options tune Open.
type Options struct {
	// Schema selects a schema through search_path (tests use one per test).
	Schema string
	// OrderWindow: orders older than this and not pending stay in the
	// database but are not loaded. Default 180 days.
	OrderWindow time.Duration
	// Wait: keep retrying the first connection for this long (service
	// start-up while Postgres is still coming up).
	Wait time.Duration
	// NoMigrate skips running migrations (they still must be up to date).
	NoMigrate bool
	// NoLock skips the single-writer advisory lock (read-only tools).
	NoLock bool
	// AppName shows in pg_stat_activity.
	AppName string
	// LockCheck is how often the held lock is verified (default 5 s).
	LockCheck time.Duration

	maxVersion int // tests: migrate only up to this version
}

// Backend implements store.Backend and store.EventReader.
type Backend struct {
	pool   *pgxpool.Pool
	lock   *pgx.Conn
	lockMu sync.Mutex // the lock connection serves one query at a time
	lostCh chan struct{}
	stopCh chan struct{}
	lostMu sync.Mutex
	lostE  error
	opt    Options
	schema string
}

// Open connects, takes the single-writer lock, and migrates the schema.
func Open(ctx context.Context, url string, opt Options) (*Backend, error) {
	if opt.OrderWindow <= 0 {
		opt.OrderWindow = 180 * 24 * time.Hour
	}
	if opt.AppName == "" {
		opt.AppName = "ftnbilling"
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("DATABASE_URL is not valid: %w", redactErr(err))
	}
	if cfg.ConnConfig.ConnectTimeout <= 0 {
		// A dropped-packets network must fail a connection attempt quickly,
		// not after the OS gives up (minutes).
		cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	}
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = 4
	}
	if !strings.Contains(url, "pool_min_conns") {
		cfg.MinConns = 1
	}
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opt.AppName
	}
	if opt.Schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = opt.Schema
	}
	deadline := time.Now().Add(opt.Wait)
	var pool *pgxpool.Pool
	for {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("cannot connect to the database: %w", redactErr(err))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	b := &Backend{pool: pool, opt: opt}
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&b.schema); err != nil {
		pool.Close()
		return nil, err
	}
	if !opt.NoLock {
		lc, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("cannot connect to the database: %w", redactErr(err))
		}
		var got bool
		if err := lc.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext('ispbill:'||current_schema()))").Scan(&got); err != nil {
			lc.Close(ctx)
			pool.Close()
			return nil, err
		}
		if !got {
			lc.Close(ctx)
			pool.Close()
			return nil, errors.New("another ispbill process is already using this database (only one may write at a time)")
		}
		b.lock = lc
		b.lostCh, b.stopCh = make(chan struct{}), make(chan struct{})
		go b.watchLock()
	}
	if !opt.NoMigrate {
		if err := b.Migrate(ctx); err != nil {
			b.Close()
			return nil, err
		}
	}
	return b, nil
}

// redactErr removes anything that looks like a password from a connection error.
func redactErr(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if i := strings.Index(s, "://"); i >= 0 {
		if j := strings.Index(s[i:], "@"); j > 0 {
			s = s[:i+3] + "***" + s[i+j:]
		}
	}
	return errors.New(s)
}

func (b *Backend) Kind() string { return "postgres" }

// Schema is the schema the backend works in.
func (b *Backend) Schema() string { return b.schema }

// Pool exposes the pool to tools and tests.
func (b *Backend) Pool() *pgxpool.Pool { return b.pool }

// Ping checks that the database answers.
func (b *Backend) Ping(ctx context.Context) error {
	if err := b.LockErr(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return b.pool.Ping(ctx)
}

// LockLost is closed when the single-writer lock is gone (the Postgres server restarted or the connection
// that holds it dropped). From then on a second writer may be running, so the caller must stop and let the
// service manager restart it. Nil when this backend holds no lock.
func (b *Backend) LockLost() <-chan struct{} { return b.lostCh }

// LockErr is why the lock was lost, or nil while it is held.
func (b *Backend) LockErr() error {
	b.lostMu.Lock()
	defer b.lostMu.Unlock()
	return b.lostE
}

func (b *Backend) lockHeld() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.lockMu.Lock()
	defer b.lockMu.Unlock()
	if b.lock == nil {
		return errors.New("lock connection closed")
	}
	var n int
	// The session must still exist and still hold the advisory lock (a restarted server loses both).
	if err := b.lock.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted AND pid = pg_backend_pid()`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("the single-writer lock is no longer held")
	}
	return nil
}

func (b *Backend) watchLock() {
	every := b.opt.LockCheck
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-b.stopCh:
			return
		case <-t.C:
		}
		if err := b.lockHeld(); err != nil {
			// One failed check can be a slow server; two in a row, or a clean "not held", is a loss.
			fails++
			if fails < 2 && !strings.Contains(err.Error(), "no longer held") {
				continue
			}
			b.lostMu.Lock()
			b.lostE = fmt.Errorf("database writer lock lost: %w", redactErr(err))
			b.lostMu.Unlock()
			close(b.lostCh)
			return
		}
		fails = 0
	}
}

func (b *Backend) Close() error {
	if b.stopCh != nil {
		select {
		case <-b.stopCh:
		default:
			close(b.stopCh)
		}
	}
	b.lockMu.Lock()
	defer b.lockMu.Unlock()
	if b.lock != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		b.lock.Close(ctx)
		cancel()
		b.lock = nil
	}
	b.pool.Close()
	return nil
}

func queryAll[V any](ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, c *codec[V], where string, args ...any) ([]V, error) {
	rows, err := q.Query(ctx, c.selectSQL(where), args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.name, err)
	}
	return c.scan(rows)
}

func fill[V any](m map[string]V, rows []V, c *codec[V]) {
	for _, r := range rows {
		m[c.key(r)] = r
	}
}

// Load reads the database into memory: everything, except that orders are
// limited to pending ones and those inside the window, and events to the
// newest 300.
func (b *Backend) Load(ctx context.Context) (*store.Data, error) { return b.load(ctx, false) }

// LoadAll reads every row, every order and every event (export, db-check).
func (b *Backend) LoadAll(ctx context.Context) (*store.Data, error) { return b.load(ctx, true) }

func (b *Backend) load(ctx context.Context, all bool) (*store.Data, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	d := &store.Data{Format: store.FormatVersion}
	if all {
		// nothing to exclude
	}
	var loadErr error
	do := func(err error) {
		if loadErr == nil {
			loadErr = err
		}
	}
	sites, err := queryAll(ctx, tx, sitesC, "")
	do(err)
	pk, err := queryAll(ctx, tx, packagesC, "")
	do(err)
	cust, err := queryAll(ctx, tx, customersC, "")
	do(err)
	eq, err := queryAll(ctx, tx, equipmentC, "")
	do(err)
	acc, err := queryAll(ctx, tx, accountsC, "")
	do(err)
	ten, err := queryAll(ctx, tx, tenanciesC, "")
	do(err)
	apts, err := queryAll(ctx, tx, apartmentsC, "")
	do(err)
	aptSites, err := queryAll(ctx, tx, apartmentSitesC, "")
	do(err)
	var ord []store.Order
	if all {
		ord, err = queryAll(ctx, tx, ordersC, "")
	} else {
		ord, err = queryAll(ctx, tx, ordersC, "status = 'pending' OR created_at > $1", time.Now().Add(-b.opt.OrderWindow))
	}
	do(err)
	vou, err := queryAll(ctx, tx, vouchersC, "")
	do(err)
	pay, err := queryAll(ctx, tx, paymentsC, "")
	do(err)
	dev, err := queryAll(ctx, tx, devicesC, "")
	do(err)
	tsk, err := queryAll(ctx, tx, tasksC, "")
	do(err)
	eqe, err := queryAll(ctx, tx, eqEventsC, "")
	do(err)
	nts, err := queryAll(ctx, tx, notesC, "")
	do(err)
	sets, err := queryAll(ctx, tx, settingsC, "")
	do(err)
	ausers, err := queryAll(ctx, tx, adminUsersC, "")
	do(err)
	asess, err := queryAll(ctx, tx, adminSessionsC, "")
	do(err)
	adevs, err := queryAll(ctx, tx, apiDevicesC, "")
	do(err)
	limit := 300
	if all {
		limit = 1 << 30
	}
	evs, err := queryAll(ctx, tx, eventsC, fmt.Sprintf("true ORDER BY seq DESC LIMIT %d", limit))
	do(err)
	if err := tx.QueryRow(ctx, "SELECT coalesce(max(seq), 0) FROM events").Scan(&d.EventSeq); err != nil {
		do(err)
	}
	if loadErr != nil {
		return nil, loadErr
	}
	d.Sites, d.Packages, d.Customers, d.Equipment = map[string]store.Site{}, map[string]store.Package{}, map[string]store.Customer{}, map[string]store.Equipment{}
	d.Accounts, d.ClosedAccounts, d.Tenancies = map[string]store.Account{}, map[string]store.Account{}, map[string]store.Tenancy{}
	d.Apartments, d.ApartmentSites = map[string]store.Apartment{}, map[string]store.ApartmentSite{}
	d.Orders, d.Vouchers, d.Payments, d.Devices = map[string]store.Order{}, map[string]store.Voucher{}, map[string]store.Payment{}, map[string]store.Device{}
	d.Tasks, d.EquipmentEvents, d.Notes = map[string]store.Task{}, map[string]store.EquipmentEvent{}, map[string]store.Note{}
	d.Settings = map[string]store.Setting{}
	d.AdminUsers, d.AdminSessions, d.APIDevices = map[string]store.AdminUser{}, map[string]store.AdminSession{}, map[string]store.APIDevice{}
	fill(d.Sites, sites, sitesC)
	fill(d.Packages, pk, packagesC)
	fill(d.Customers, cust, customersC)
	fill(d.Equipment, eq, equipmentC)
	for _, a := range acc {
		if a.ClosedAt.IsZero() {
			d.Accounts[a.ID] = a
		} else {
			d.ClosedAccounts[a.ID] = a
		}
	}
	fill(d.Tenancies, ten, tenanciesC)
	fill(d.Apartments, apts, apartmentsC)
	fill(d.ApartmentSites, aptSites, apartmentSitesC)
	fill(d.Orders, ord, ordersC)
	fill(d.Vouchers, vou, vouchersC)
	fill(d.Payments, pay, paymentsC)
	fill(d.Devices, dev, devicesC)
	fill(d.Tasks, tsk, tasksC)
	fill(d.EquipmentEvents, eqe, eqEventsC)
	fill(d.Notes, nts, notesC)
	fill(d.Settings, sets, settingsC)
	fill(d.AdminUsers, ausers, adminUsersC)
	fill(d.AdminSessions, asess, adminSessionsC)
	fill(d.APIDevices, adevs, apiDevicesC)
	for i := len(evs) - 1; i >= 0; i-- { // oldest first in memory
		d.Events = append(d.Events, evs[i])
	}
	return d, nil
}

// Commit writes one change set in a single transaction.
func (b *Backend) Commit(ctx context.Context, _ *store.Data, cs *store.ChangeSet) error {
	if err := b.LockErr(); err != nil {
		return err
	}
	if len(cs.Payments.Delete) > 0 {
		return errors.New("payments are never deleted")
	}
	batch := &pgx.Batch{}
	var names []string
	mark := func(n string) {
		for len(names) < batch.Len() {
			names = append(names, n)
		}
	}
	// Deletes first, then upserts parents before children (foreign keys are
	// deferred, so this is for readability, not necessity).
	sitesC.queueDeletes(batch, cs.Sites.Delete)
	packagesC.queueDeletes(batch, cs.Packages.Delete)
	customersC.queueDeletes(batch, cs.Customers.Delete)
	equipmentC.queueDeletes(batch, cs.Equipment.Delete)
	accountsC.queueDeletes(batch, cs.Accounts.Delete)
	tenanciesC.queueDeletes(batch, cs.Tenancies.Delete)
	apartmentSitesC.queueDeletes(batch, cs.ApartmentSites.Delete)
	apartmentsC.queueDeletes(batch, cs.Apartments.Delete)
	ordersC.queueDeletes(batch, cs.Orders.Delete)
	vouchersC.queueDeletes(batch, cs.Vouchers.Delete)
	devicesC.queueDeletes(batch, cs.Devices.Delete)
	tasksC.queueDeletes(batch, cs.Tasks.Delete)
	eqEventsC.queueDeletes(batch, cs.EquipmentEvents.Delete)
	notesC.queueDeletes(batch, cs.Notes.Delete)
	settingsC.queueDeletes(batch, cs.Settings.Delete)
	adminSessionsC.queueDeletes(batch, cs.AdminSessions.Delete)
	apiDevicesC.queueDeletes(batch, cs.APIDevices.Delete)
	adminUsersC.queueDeletes(batch, cs.AdminUsers.Delete)
	mark("delete")
	sitesC.queueUpserts(batch, cs.Sites)
	mark("sites")
	apartmentsC.queueUpserts(batch, cs.Apartments)
	mark("apartments")
	apartmentSitesC.queueUpserts(batch, cs.ApartmentSites)
	mark("apartment_sites")
	packagesC.queueUpserts(batch, cs.Packages)
	mark("packages")
	customersC.queueUpserts(batch, cs.Customers)
	mark("customers")
	equipmentC.queueUpserts(batch, cs.Equipment)
	mark("equipment")
	queueAccountPrepare(batch, cs.Accounts)
	mark("accounts")
	accountsC.queueUpserts(batch, cs.Accounts)
	mark("accounts")
	tenanciesC.queueUpserts(batch, cs.Tenancies)
	mark("tenancies")
	ordersC.queueUpserts(batch, cs.Orders)
	mark("orders")
	vouchersC.queueUpserts(batch, cs.Vouchers)
	mark("vouchers")
	paymentsC.queueUpserts(batch, cs.Payments)
	mark("payments")
	devicesC.queueUpserts(batch, cs.Devices)
	mark("devices")
	tasksC.queueUpserts(batch, cs.Tasks)
	mark("tasks")
	eqEventsC.queueUpserts(batch, cs.EquipmentEvents)
	mark("equipment_events")
	notesC.queueUpserts(batch, cs.Notes)
	mark("notes")
	settingsC.queueUpserts(batch, cs.Settings)
	mark("settings")
	adminUsersC.queueUpserts(batch, cs.AdminUsers)
	mark("admin_users")
	adminSessionsC.queueUpserts(batch, cs.AdminSessions)
	mark("admin_sessions")
	apiDevicesC.queueUpserts(batch, cs.APIDevices)
	mark("api_devices")
	eventsC.queueUpserts(batch, store.Delta[store.Event]{Upsert: cs.NewEvents})
	mark("events")
	if batch.Len() == 0 {
		return nil
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return connErr(err)
	}
	defer tx.Rollback(context.Background())
	br := tx.SendBatch(ctx, batch)
	for i := 0; i < batch.Len(); i++ {
		tag, err := br.Exec()
		if err != nil {
			br.Close()
			return connErr(integrityErr(fmt.Errorf("%s: %w", names[i], err)))
		}
		if (names[i] == "admin_users" || names[i] == "api_devices") && tag.RowsAffected() == 0 {
			br.Close()
			return fmt.Errorf("%s: %w", names[i], store.ErrStaleWrite)
		}
	}
	if err := br.Close(); err != nil {
		return connErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) { // the server answered: the commit definitely failed
			return integrityErr(err)
		}
		return fmt.Errorf("%w: %w", store.ErrAmbiguous, err)
	}
	return nil
}

// integrityErr marks a refusal by the database (SQLSTATE class 23: unique,
// foreign key, check, not null) with store.ErrIntegrity, and, where the rule is
// one the program knows by name, with the specific error too, so callers can
// tell a duplicate MAC from a bad value. The original error stays in the chain
// and in the message. Update treats ErrIntegrity as a refusal, never an outage.
func integrityErr(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || !strings.HasPrefix(pe.Code, "23") {
		return err
	}
	var specific error
	switch pe.ConstraintName {
	case "accounts_mac_open":
		specific = store.ErrDuplicateMAC
	case "accounts_site_ip_open":
		specific = store.ErrDuplicateIP
	case "accounts_mac_shape":
		specific = store.ErrBadMAC
	case "equipment_mac_live":
		specific = store.ErrDuplicateEquipmentMAC
	case "admin_users_username":
		specific = store.ErrDuplicateUsername
	case "api_devices_access", "api_devices_refresh":
		specific = store.ErrDuplicateToken
	case "accounts_apartment_unit_open":
		specific = store.ErrDuplicateUnit
	case "apartments_name_lower":
		specific = store.ErrDuplicateApartment
	case "apartment_sites_apartment_id_fkey", "apartment_sites_site_id_fkey":
		specific = store.ErrBadApartmentLink
	case "sites_access_mode_check":
		specific = store.ErrBadAccessMode
	}
	if specific != nil {
		return fmt.Errorf("%w: %w: %w", store.ErrIntegrity, specific, err)
	}
	return fmt.Errorf("%w: %w", store.ErrIntegrity, err)
}

// connErr marks an error that did not come from the server (timeout, reset,
// no route) with store.ErrConnection. A server error (constraint, syntax)
// passes through unchanged.
func connErr(err error) error {
	var pe *pgconn.PgError
	if err == nil {
		return nil
	}
	if errors.As(err, &pe) && !strings.HasPrefix(pe.Code, "08") && !strings.HasPrefix(pe.Code, "57P") {
		return err // the server answered and refused the change
	}
	return fmt.Errorf("%w: %w", store.ErrConnection, err)
}

// queueAccountPrepare frees the unique slots (code, open MAC, open IP per site) held by
// accounts whose code, MAC, site or open/closed state is changing. Swapping a
// MAC or code between two accounts, or moving a MAC from one account to a new
// one, would otherwise collide in whatever order the rows are written. The
// real values are written right after by the upserts; both statements are in
// the same transaction, so nobody sees the blanks.
func queueAccountPrepare(b *pgx.Batch, d store.Delta[store.Account]) {
	var ids []string
	for _, cur := range d.Upsert {
		old, ok := d.Old[cur.ID]
		if !ok {
			continue
		}
		if old.Code != cur.Code || !strings.EqualFold(old.MAC, cur.MAC) || old.SiteID != cur.SiteID ||
			old.ApartmentID != cur.ApartmentID || old.Unit != cur.Unit || old.ClosedAt.IsZero() != cur.ClosedAt.IsZero() ||
			old.IP != cur.IP || old.MACConflict != cur.MACConflict {
			ids = append(ids, cur.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	b.Queue("UPDATE accounts SET code = '', mac = '', ip = '', unit = '' WHERE id = ANY($1)", ids)
}

// Events reads the full event history, newest first.
func (b *Backend) Events(ctx context.Context, q store.EventQuery) ([]store.Event, error) {
	var conds []string
	var args []any
	add := func(col, v string) {
		if v != "" {
			args = append(args, v)
			conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	add("site_id", q.SiteID)
	add("account_id", q.AccountID)
	add("customer_id", q.CustomerID)
	add("equipment_id", q.EquipmentID)
	add("kind", q.Kind)
	if q.BeforeSeq > 0 {
		args = append(args, q.BeforeSeq)
		conds = append(conds, fmt.Sprintf("seq < $%d", len(args)))
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	where := "true"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}
	return queryAll(ctx, b.pool, eventsC, fmt.Sprintf("%s ORDER BY seq DESC LIMIT %d", where, q.Limit), args...)
}

func init() {
	// Events are append-only: a repeated insert is ignored.
	i := strings.Index(eventsC.upsql, " DO UPDATE SET")
	eventsC.upsql = eventsC.upsql[:i] + " DO NOTHING"
}
