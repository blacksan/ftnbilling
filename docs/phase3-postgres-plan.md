# Phase 3 plan: Postgres customer database for ispbill (approved)

Hardware target: 1 vCPU, 2 GB RAM, 60 GB NVMe VPS, about 20 buildings × 30 flats.

## 0. Findings that shape the design

- **Store usage.** `*store.Store` is a concrete type with about 110 `Update`/`View` call sites, so the backend switch goes inside `Store`.
- **Update today.** `Update` mutates `s.data` in place. On error nothing is saved, but there is no rollback either. `ErrNoChange` skips the save; every caller returns it before mutating anything.
- **No dirty tracking possible.** Callbacks write to the maps directly, so there is no interception point.
- **Deletes.** Only `Accounts`, `Tasks` and `Devices` are ever hard-deleted. Sites and packages are tombstoned. Payments, orders and vouchers are never deleted.
- **Dangling references.** `DeleteAccount` leaves `AccountID` on old payments, so foreign keys need care.
- **Row types.** All are flat comparable structs except `Site`, whose `PkgOverrides` map is always reassigned, never mutated in place. `Events` is a slice capped at 300.
- **Lost payments.** `/pay/cb/stk` always answers "Accepted", so a failed persist would silently lose the payment. This plan adds a spool.
- **Build.** The installer builds on the VPS, so pgx is vendored.

## 1. Decisions

| Topic | Decision |
|---|---|
| Architecture | Keep `Update`/`View` and the in-memory model. Each `Update` becomes one Postgres transaction. No SQL rewrite of the services. |
| Change tracking | Snapshot diff by primary key against a permanent **shadow** copy (the last committed state). |
| Rollback | Real rollback from the shadow on an `fn` error, a panic or a commit failure, on both backends. |
| Reads | Everything reads from memory, except older events, which come from SQL through `Events` queries. |
| Backend switch | `DATABASE_URL` set means Postgres; unset means the JSON file. The router build uses `-tags nopg`, so pgx is not linked. |
| Account deletion | `DeleteAccount` moves the row to `Data.ClosedAccounts` (same table, `closed_at` set). |
| Customer vs account | New `Customer`, plus `Tenancy` linking customer and account over time. `Account.Tenant/Phone/WhatsApp/WAOptOut` stay authoritative for Phase 2 messaging and are synced one-way from the customer. |
| Location | Columns on the account: `floor`, `flat`, `odb`, `splitter_id`, `splitter_port`. |
| National ID | Optional. Encrypted in the app with AES-256-GCM using `DATA_KEY` (env), stored as ciphertext plus the last 4 digits plus an HMAC blind index. |
| Kept as files | Metrics, fleet, alerts and the outbox. The outbox moves to Postgres later, as a follow-up. |
| Dependency | pgx v5 only, vendored under `vendor/`. It pulls in puddle, pgpassfile, pgservicefile and golang.org/x/{crypto,text,sync}. |

## 2. Model changes (`internal/store/store.go`, `FormatVersion = 4`)

```go
// Site gains: Address string; Lat, Lng float64 (0 = unset); GoLive time.Time
// Account gains: CustomerID, Floor, ODB, SplitterID string; SplitterPort int; ClosedAt time.Time
// Payment gains: CustomerID string   (occupant at credit time)
// Event gains:   Seq int64; CustomerID, EquipmentID string
type Customer struct {
    ID, Name, Phone, Phone2, WhatsApp, Email, Notes string
    WAOptOut bool
    NationalIDEnc, NationalIDHMAC, NationalIDLast4 string
    AnonymizedAt, CreatedAt, UpdatedAt time.Time
}
type Tenancy struct { ID, CustomerID, AccountID, Note string; MoveIn, MoveOut time.Time } // MoveOut zero = current
type Contact struct { ID, SiteID, Role, Name, Phone, Email, Notes string; CreatedAt time.Time }
type Equipment struct {
    ID, Kind, Serial, MAC, Model, Firmware, Status, SiteID, AccountID, Notes string
    InstalledAt, CreatedAt, UpdatedAt time.Time
}
type EquipmentEvent struct { ID, EquipmentID, Kind, FromStatus, ToStatus, SiteID, AccountID, CustomerID, Note string; Time time.Time }
type Note struct { ID, CustomerID, AccountID, SiteID, EquipmentID, Text string; Time time.Time }
```

**Enum values**

| Field | Values |
|---|---|
| Contact role | `landlord`, `caretaker`, `agent`, `security`, `other` |
| Equipment kind | `ont`, `router`, `olt`, `splitter`, `switch`, `other` |
| Equipment status | `in_stock`, `installed`, `faulty`, `returned`, `retired` |
| Equipment event kind | `received`, `assigned`, `unassigned`, `returned`, `faulty`, `repaired`, `moved`, `edited`, `note` |

**`Data` gains** `ClosedAccounts map[string]Account`, plus maps keyed by ID for `Customers`, `Tenancies`, `Contacts`, `Equipment`, `EquipmentEvents` and `Notes`, and `EventSeq int64`.

**Events.** `AddEvent` assigns `Seq`. Memory keeps the newest 300; Postgres keeps all of them.

**Router and OLT identification** is stored as `Equipment` rows with a `SiteID` and no account.

**Billing changes**
- `DeleteAccount`: set `ClosedAt`, move the row to `ClosedAccounts`, close the open tenancy, and mark its equipment `returned`.
- `uniqueCode`: also check `ClosedAccounts`.
- `creditLocked`, `RecordUnmatched` and `AssignPayment`: stamp `Payment.CustomerID`.
- STOP/START (Phase 2 inbound webhook) and any Phase 2 `WAOptOut` writes on an account also update the linked customer's `WAOptOut` in the same `Update`. Use one helper, e.g. `setWAOptOut(d, accountID, v)`.

**New services** in `internal/billing/crm.go` and `equipment.go`. Each runs in one `st.Update`.

| Area | Functions |
|---|---|
| Customers | `SaveCustomer`, `LinkCustomer(accountID, customerID, moveIn)`, `MoveOut(accountID, date)`, `AnonymizeCustomer`, `AddNote`, `SetNationalID`, `RevealNationalID` |
| Equipment | `SaveEquipment`, `AssignEquipment(id, accountID or siteID)`, `ReturnEquipment(id, status, note)` |
| Building contacts | `SaveContact`, `DeleteContact` |

- Assign and return write an `EquipmentEvent`.
- Assigning an ONT that has a MAC offers "use as the account's ONT MAC", which calls `ChangeMAC`.
- Saving or linking a customer copies its name, phone, WhatsApp number and opt-out to its open accounts.

## 3. Schema (`internal/store/pgstore/migrations/0001_core.sql`)

**Conventions**
- Go strings map to `text NOT NULL DEFAULT ''`, except foreign keys, where `""` becomes NULL.
- A zero `time.Time` is stored as NULL. Times are normalised to UTC microseconds on write and on load.
- All foreign keys are `DEFERRABLE INITIALLY DEFERRED`.
- CHECK enums only where the set of values is closed.
- No `public.` prefix on table names, so tests can use their own schema.

```sql
CREATE TABLE meta (key text PRIMARY KEY, value text NOT NULL);
CREATE TABLE sites (
  id text PRIMARY KEY CHECK (id <> ''), name text NOT NULL, mode text NOT NULL CHECK (mode IN ('wg','env')),
  notes text NOT NULL DEFAULT '', support_phone text NOT NULL DEFAULT '',
  disabled boolean NOT NULL DEFAULT false, removed boolean NOT NULL DEFAULT false,
  tunnel_ip text NOT NULL DEFAULT '', wg_pub text NOT NULL DEFAULT '',
  rest_user text NOT NULL DEFAULT '', rest_pass text NOT NULL DEFAULT '', rest_port integer NOT NULL DEFAULT 0,
  join_hash text NOT NULL DEFAULT '', join_expires_at timestamptz, join_created_at timestamptz,
  join_used_at timestamptz, join_used_from text NOT NULL DEFAULT '', joined_at timestamptz,
  hotspot_server text NOT NULL DEFAULT '', wan_interface text NOT NULL DEFAULT '', wan_list text NOT NULL DEFAULT '',
  olt_interface text NOT NULL DEFAULT '', vlan_id integer NOT NULL DEFAULT 0, customer_subnet text NOT NULL DEFAULT '',
  wan_cap_mbps double precision NOT NULL DEFAULT 0, olt_cap_mbps double precision NOT NULL DEFAULT 0,
  fup_tb double precision NOT NULL DEFAULT 0, provision boolean NOT NULL DEFAULT false,
  bench_port text NOT NULL DEFAULT '', legacy_container boolean NOT NULL DEFAULT false,
  pkg_overrides jsonb NOT NULL DEFAULT '{}',
  address text NOT NULL DEFAULT '', lat double precision NOT NULL DEFAULT 0, lng double precision NOT NULL DEFAULT 0,
  go_live timestamptz, created_at timestamptz NOT NULL,
  CHECK (lat BETWEEN -90 AND 90 AND lng BETWEEN -180 AND 180));
CREATE UNIQUE INDEX sites_tunnel_ip_live ON sites (tunnel_ip) WHERE NOT removed AND tunnel_ip <> '';
CREATE TABLE packages (id text PRIMARY KEY, name text NOT NULL, slug text NOT NULL, duration_min integer NOT NULL,
  speed_mbps integer NOT NULL, price_kes integer NOT NULL, disabled boolean NOT NULL DEFAULT false,
  deleted boolean NOT NULL DEFAULT false, sort integer NOT NULL DEFAULT 0);
CREATE TABLE customers (id text PRIMARY KEY, name text NOT NULL, phone text NOT NULL DEFAULT '', phone2 text NOT NULL DEFAULT '',
  whatsapp text NOT NULL DEFAULT '', wa_opt_out boolean NOT NULL DEFAULT false, email text NOT NULL DEFAULT '',
  notes text NOT NULL DEFAULT '', national_id_enc text NOT NULL DEFAULT '', national_id_hmac text NOT NULL DEFAULT '',
  national_id_last4 text NOT NULL DEFAULT '', anonymized_at timestamptz, created_at timestamptz NOT NULL, updated_at timestamptz);
CREATE INDEX customers_phone ON customers (phone) WHERE phone <> '';
CREATE INDEX customers_whatsapp ON customers (whatsapp) WHERE whatsapp <> '';
CREATE INDEX customers_name_lower ON customers (lower(name));
CREATE INDEX customers_nid ON customers (national_id_hmac) WHERE national_id_hmac <> '';
CREATE TABLE equipment (id text PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('ont','router','olt','splitter','switch','other')),
  serial text NOT NULL DEFAULT '', mac text NOT NULL DEFAULT '', model text NOT NULL DEFAULT '', firmware text NOT NULL DEFAULT '',
  status text NOT NULL CHECK (status IN ('in_stock','installed','faulty','returned','retired')),
  site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED, account_id text,
  installed_at timestamptz, notes text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, updated_at timestamptz,
  CHECK (account_id IS NULL OR status = 'installed'));
CREATE UNIQUE INDEX equipment_serial ON equipment (kind, upper(serial)) WHERE serial <> '';
CREATE INDEX equipment_mac ON equipment (mac) WHERE mac <> '';
CREATE INDEX equipment_site ON equipment (site_id); CREATE INDEX equipment_account ON equipment (account_id);
CREATE INDEX equipment_status ON equipment (status);
CREATE TABLE accounts (id text PRIMARY KEY, site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  code text NOT NULL DEFAULT '', flat text NOT NULL, floor text NOT NULL DEFAULT '', mac text NOT NULL DEFAULT '',
  tenant text NOT NULL DEFAULT '', phone text NOT NULL DEFAULT '', notes text NOT NULL DEFAULT '',
  whatsapp text NOT NULL DEFAULT '', wa_opt_out boolean NOT NULL DEFAULT false,
  customer_id text REFERENCES customers(id) DEFERRABLE INITIALLY DEFERRED,
  package_id text REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED,
  expires_at timestamptz, suspended boolean NOT NULL DEFAULT false, hs_pass text NOT NULL DEFAULT '',
  router_active boolean NOT NULL DEFAULT false, reminded_for timestamptz, expired_for timestamptz,
  odb text NOT NULL DEFAULT '', splitter_id text REFERENCES equipment(id) DEFERRABLE INITIALLY DEFERRED,
  splitter_port integer NOT NULL DEFAULT 0, created_at timestamptz NOT NULL, closed_at timestamptz);
CREATE UNIQUE INDEX accounts_code ON accounts (code) WHERE code <> '';
CREATE UNIQUE INDEX accounts_site_mac_open ON accounts (site_id, upper(mac)) WHERE closed_at IS NULL AND mac <> '';
CREATE INDEX accounts_site_flat ON accounts (site_id, lower(flat));
CREATE INDEX accounts_customer ON accounts (customer_id); CREATE INDEX accounts_phone ON accounts (phone) WHERE phone <> '';
ALTER TABLE equipment ADD CONSTRAINT equipment_account_fk FOREIGN KEY (account_id) REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE tenancies (id text PRIMARY KEY, customer_id text NOT NULL REFERENCES customers(id) DEFERRABLE INITIALLY DEFERRED,
  account_id text NOT NULL REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED,
  move_in timestamptz NOT NULL, move_out timestamptz, note text NOT NULL DEFAULT '',
  CHECK (move_out IS NULL OR move_out >= move_in));
CREATE UNIQUE INDEX tenancies_one_open ON tenancies (account_id) WHERE move_out IS NULL;
CREATE INDEX tenancies_customer ON tenancies (customer_id);
CREATE TABLE site_contacts (id text PRIMARY KEY, site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  role text NOT NULL CHECK (role IN ('landlord','caretaker','agent','security','other')), name text NOT NULL,
  phone text NOT NULL DEFAULT '', email text NOT NULL DEFAULT '', notes text NOT NULL DEFAULT '', created_at timestamptz NOT NULL);
CREATE INDEX site_contacts_site ON site_contacts (site_id);
CREATE TABLE orders (id text PRIMARY KEY, site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  account_id text REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED, package_id text REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED,
  phone text NOT NULL DEFAULT '', method text NOT NULL, amount_kes integer NOT NULL,
  status text NOT NULL CHECK (status IN ('pending','paid','failed')), reason text NOT NULL DEFAULT '',
  checkout_request_id text NOT NULL DEFAULT '', receipt text NOT NULL DEFAULT '', voucher_code text NOT NULL DEFAULT '',
  logged_in boolean NOT NULL DEFAULT false, mac text NOT NULL DEFAULT '', ip text NOT NULL DEFAULT '', dst text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL, paid_at timestamptz);
CREATE INDEX orders_created ON orders (created_at DESC); CREATE INDEX orders_account ON orders (account_id, created_at DESC);
CREATE INDEX orders_checkout ON orders (checkout_request_id) WHERE checkout_request_id <> '';
CREATE INDEX orders_pending ON orders (created_at) WHERE status = 'pending';
CREATE TABLE vouchers (code text PRIMARY KEY, site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED,
  package_id text NOT NULL REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED, batch text NOT NULL DEFAULT '',
  used boolean NOT NULL DEFAULT false, used_at timestamptz, used_by text NOT NULL DEFAULT '', created_at timestamptz NOT NULL);
CREATE INDEX vouchers_batch ON vouchers (batch);
CREATE TABLE payments (key text PRIMARY KEY CHECK (key = upper(key) AND key <> ''),
  site_id text REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED, method text NOT NULL,
  status text NOT NULL CHECK (status IN ('applied','unmatched','refunded','ignored')), amount_kes integer NOT NULL,
  phone text NOT NULL DEFAULT '', name text NOT NULL DEFAULT '', bill_ref text NOT NULL DEFAULT '',
  account_id text REFERENCES accounts(id) DEFERRABLE INITIALLY DEFERRED,
  customer_id text REFERENCES customers(id) DEFERRABLE INITIALLY DEFERRED,
  package_id text REFERENCES packages(id) DEFERRABLE INITIALLY DEFERRED,
  order_id text NOT NULL DEFAULT '', note text NOT NULL DEFAULT '',
  paid_at timestamptz NOT NULL, created_at timestamptz NOT NULL, resolved_at timestamptz);
CREATE INDEX payments_paid ON payments (paid_at DESC); CREATE INDEX payments_account ON payments (account_id, paid_at DESC);
CREATE INDEX payments_customer ON payments (customer_id, paid_at DESC); CREATE INDEX payments_site ON payments (site_id, paid_at DESC);
CREATE INDEX payments_unmatched ON payments (created_at) WHERE status = 'unmatched';
CREATE INDEX payments_phone ON payments (phone) WHERE phone <> '';
CREATE TABLE devices (site_id text NOT NULL REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED, mac text NOT NULL,
  ip text NOT NULL DEFAULT '', source text NOT NULL DEFAULT '', first_seen timestamptz, last_seen timestamptz,
  hits integer NOT NULL DEFAULT 0, PRIMARY KEY (site_id, mac));
CREATE TABLE tasks (id text PRIMARY KEY, site_id text NOT NULL, account_id text NOT NULL, code text NOT NULL DEFAULT '',
  kick boolean NOT NULL DEFAULT false, rev integer NOT NULL DEFAULT 0, reason text NOT NULL DEFAULT '',
  attempts integer NOT NULL DEFAULT 0, next_try timestamptz, last_err text NOT NULL DEFAULT '', created_at timestamptz NOT NULL);
CREATE TABLE events (seq bigint PRIMARY KEY, time timestamptz NOT NULL, site_id text NOT NULL DEFAULT '', kind text NOT NULL,
  account_id text NOT NULL DEFAULT '', customer_id text NOT NULL DEFAULT '', equipment_id text NOT NULL DEFAULT '', msg text NOT NULL);
CREATE INDEX events_account ON events (account_id, seq DESC) WHERE account_id <> '';
CREATE INDEX events_site ON events (site_id, seq DESC);
CREATE TABLE equipment_events (id text PRIMARY KEY, equipment_id text NOT NULL REFERENCES equipment(id) DEFERRABLE INITIALLY DEFERRED,
  time timestamptz NOT NULL,
  kind text NOT NULL CHECK (kind IN ('received','assigned','unassigned','returned','faulty','repaired','moved','edited','note')),
  from_status text NOT NULL DEFAULT '', to_status text NOT NULL DEFAULT '', site_id text NOT NULL DEFAULT '',
  account_id text NOT NULL DEFAULT '', customer_id text NOT NULL DEFAULT '', note text NOT NULL DEFAULT '');
CREATE INDEX equipment_events_eq ON equipment_events (equipment_id, time DESC);
CREATE INDEX equipment_events_account ON equipment_events (account_id) WHERE account_id <> '';
CREATE TABLE notes (id text PRIMARY KEY, time timestamptz NOT NULL, customer_id text NOT NULL DEFAULT '',
  account_id text NOT NULL DEFAULT '', site_id text NOT NULL DEFAULT '', equipment_id text NOT NULL DEFAULT '', text text NOT NULL);
CREATE INDEX notes_customer ON notes (customer_id, time DESC) WHERE customer_id <> '';
CREATE INDEX notes_account ON notes (account_id, time DESC) WHERE account_id <> '';
```

**Reporting views** (`0002_views.sql`). None of them include secret columns.
- **`v_payment_history`**: payments joined with accounts, customers, sites and packages.
- **`v_revenue_monthly`**: revenue per site per EAT month. Revenue means `status='applied' AND method NOT IN ('voucher','admin')`, mirroring `countsAsRevenue` in `internal/web/admin.go`.
- **`v_equipment_current`** and **`v_customers`**.

**Struct-to-table mapping.** Most map types go to a table of the same name.
- `Accounts` and `ClosedAccounts` share the `accounts` table; `closed_at` decides which map a row loads into.
- Contacts are stored in `site_contacts`.
- Devices are keyed `(site_id, mac)`; the in-memory map key is `site|mac`.
- Payments can never be deleted; the backend refuses a delete.
- Events are append-only.

## 4. Store backend and persistence

```go
// internal/store/backend.go
type Backend interface {
    Kind() string // "json" | "postgres"
    Load(ctx context.Context) (*Data, error)
    Commit(ctx context.Context, d *Data, cs *ChangeSet) error
    Close() error
}
type EventReader interface { Events(ctx context.Context, q EventQuery) ([]Event, error) }
type Delta[V any] struct { Upsert []V; Delete []string }
type ChangeSet struct { /* one Delta per table */ NewEvents []Event }
func Open(dir string) (*Store, error)   // unchanged: JSON backend
func OpenWith(b Backend) (*Store, error)
func (s *Store) Events(ctx, q EventQuery) ([]Event, error)
func (s *Store) Healthy() error
func (s *Store) Close() error
```

**Backends**
- **JSON** (`jsonfile.go`): today's code. `Commit` marshals the whole data set and ignores the change set.
- **Postgres** (`internal/store/pgstore`): a pgxpool with `MaxConns=4` and `MinConns=1`.

**Store state:** `data` (live), `shadow` (last committed, with its own maps and its own `Events` slice; `PkgOverrides` is cloned), and `degraded` (an error, or nil).

**`Update(fn)`**, run under the write lock:
1. If degraded, return `ErrUnavailable`.
2. Run `fn` with a `recover`. If it returns `ErrNoChange`, return nil without diffing. With `store.Strict = true` (set in tests), diff anyway and panic if anything was mutated.
3. Diff each table against the shadow:
   - Live keys that are missing from the shadow or differ (`==`; `reflect.DeepEqual` for `Site`) become upserts.
   - Deletes are found only when the matched count is below `len(shadow)`.
   - Changed rows are normalised (times to UTC microseconds) and written back into live.
   - `Accounts` and `ClosedAccounts` are diffed as one key space.
   - New events are those with `Seq > shadow.EventSeq`.
4. If `fn` returned an error or panicked: roll back, then return the error or re-panic.
5. If the change set is empty: return nil.
6. `Commit` with a 5-second context.
   - On success, apply the change set to the shadow.
   - On failure, roll back and return the error wrapped as `ErrPersist`.

**Rollback:** for each upsert key, restore the shadow value or delete the key if the shadow lacks it. For each delete key, restore it. Reset `Events` and `EventSeq` from the shadow.

**Postgres `Commit`:** one transaction, one `pgx.Batch`. Deletes go first, then `INSERT ... ON CONFLICT (pk) DO UPDATE` in the order sites, packages, customers, equipment, accounts, tenancies, contacts, orders, vouchers, payments, devices, tasks, equipment_events, notes, events.

**Ambiguous commit** (the error comes during COMMIT, or the connection drops):
1. Run a full `Load` and replace both live and shadow.
2. If that also fails, set degraded:
   - `Update` returns `ErrUnavailable`; `View` keeps working.
   - A background `Load` retries every 5 seconds.
   - `/healthz` reports `db_ok:false`.

**Single writer.** At open, take `pg_advisory_lock(hashtext('ispbill:'||current_schema()))` on a dedicated connection. A second opener fails fast.

**Load windows (Postgres only):** orders where `status='pending' OR created_at > now()-'180 days'`, and the last 300 events. Everything else loads in full. Rows outside the window are never treated as deletes.

**M-Pesa spool** (`internal/payments/spool.go`):
- When a credit returns `ErrPersist` or `ErrUnavailable`, write the raw callback body to `DATA/spool/<unixnano>-<seq>-<kind>.json` (mode 0600).
- Replay at startup and every 60 seconds while the store is healthy. Replay is idempotent because the receipt is the payment key.

**Codecs** (`pgstore/tables.go`): hand-written per table (upsert SQL, args, delete, scan). Two guard tests catch a forgotten column: a reflection field-count check and a round trip with every field populated.

## 5. Migrations, import, config

**Migration runner** (`pgstore/migrate.go`):
- Migrations are embedded with `//go:embed migrations/*.sql`.
- They are recorded in `schema_migrations(version int PRIMARY KEY, name text, checksum text, applied_at timestamptz)`.
- Each file runs in its own transaction under `pg_advisory_xact_lock`. Forward-only, run at startup.
- Startup refuses to continue on a checksum mismatch or if the database is newer than the binary.

**JSON v3 to v4** (`store/migrate.go`): back up the file to `.v3.bak`, assign event sequence numbers, then run `Data.deriveCRM(now)`:
- **Customers:** one per distinct normalised account phone; otherwise one per account with a tenant name. Vacant accounts get none.
- **Tenancies:** each customer gets an open tenancy starting at `Account.CreatedAt`. `Payment.CustomerID` is back-filled.
- **ONTs:** one `ont` equipment row per account, from `Account.MAC` (status `installed`), plus a `received` event noting "created at import".
- **Missing accounts:** a payment or order whose `AccountID` no longer exists gets a closed stub account. Code and flat are parsed from a `BillRef` of the form "CODE (deleted: Flat)".

**Import into Postgres:**
1. `store.ReadJSON(path)` migrates v2, v3 or v4 in memory, without writing anything.
2. A validator reports duplicate codes and duplicate `(site, mac)` pairs in plain words.
3. Load everything in one transaction with `CopyFrom`.
4. Write `meta` (`imported_from`, `imported_at`, counts).
5. Verify by reloading and comparing row counts and the sum of applied `amount_kes`.
6. Rename the file to `ispbill.json.imported-YYYYMMDD` (mode 0600).

**When import runs:** automatically, when the tables are empty, `meta.imported_at` is absent and `DATA_DIR/ispbill.json` exists. It runs before `SeedPackages`.

**Fork guard:** if `DATABASE_URL` is unset but an `ispbill.json.imported-*` file exists, refuse to start.

**Command-line subcommands** (`cmd/ispbill/cli.go`; with no argument the server runs):
- `migrate`
- `import-json [-file] [-force]`
- `export-json -out`
- `db-check`: prints row counts and sums.

**Config**
- `DATABASE_URL`.
- `DATA_KEY`: base64 of 32 bytes. If empty, the national ID feature is off.
- `DB_COMMIT_TIMEOUT`: default 5s.
- Example URL: `postgres://ftnbilling:PW@/ftnbilling?host=/var/run/postgresql&sslmode=disable&pool_max_conns=4&application_name=ftnbilling`

**Build tags:**
- `cmd/ispbill/backend_pg.go` (`//go:build !nopg`) and `backend_nopg.go`; the latter errors if `DATABASE_URL` is set.
- `build.sh` and the Dockerfile build the arm64 router image with `-tags nopg`.
- In `main.go`, `openStore(cfg, log)` replaces `store.Open`, followed by `defer st.Close()`.

## 6. Installer (`deploy/install.sh`), skipped with `NO_POSTGRES=1`

1. `apt-get install postgresql postgresql-client` (the distro version).
2. Swap: if there is no swap and RAM is 2 GB or less, create a 1 GB `/swapfile` and set `vm.swappiness=10`.
3. Write `conf.d/90-ftnbilling.conf`:
   ```
   listen_addresses=''
   max_connections=20
   shared_buffers=128MB
   effective_cache_size=512MB
   work_mem=4MB
   maintenance_work_mem=64MB
   max_wal_size=256MB
   min_wal_size=80MB
   checkpoint_timeout=15min
   checkpoint_completion_target=0.9
   wal_compression=on
   random_page_cost=1.1
   effective_io_concurrency=200
   max_worker_processes=4
   max_parallel_workers=0
   max_parallel_workers_per_gather=0
   autovacuum_max_workers=2
   jit=off
   password_encryption=scram-sha-256
   log_min_duration_statement=500ms
   log_checkpoints=on
   idle_in_transaction_session_timeout=60s
   temp_file_limit=1GB
   ```
4. In `pg_hba.conf`, add `local ftnbilling ftnbilling scram-sha-256` above `local all all peer`, using a marker comment so it is idempotent. Restart Postgres.
5. On first install only: create role `ftnbilling` with a random 32-character password and `CONNECTION LIMIT 10`; set `statement_timeout='30s'` on the role; create database `ftnbilling` (owner `ftnbilling`, `UTF8`, `template0`); `REVOKE CONNECT ... FROM PUBLIC`. Never regenerate the password.
6. Add `DATABASE_URL` and `DATA_KEY` to the env file if missing. When upgrading from JSON, first tar `ispbill.json` to `/var/backups/ftnbilling/pre-postgres-<date>.tgz`; the next start then imports automatically.
7. App unit:
   - `After=` and `Wants=postgresql.service`.
   - The app also retries the database connection for 60 seconds at startup.
   - `MemoryMax=900M`.
   - `GOMEMLIMIT=600MiB GOGC=50`.
8. Postgres unit drop-in: `OOMScoreAdjust=-500`.
9. Build with `nice -n 15` and `GOFLAGS=-mod=vendor`.
10. Check connectivity: `runuser -u ftnbilling -- psql "$DATABASE_URL" -c 'select 1'`, then confirm `db_ok` in `/healthz`. The final banner shows the backend and row counts.
11. journald: `SystemMaxUse=500M`.

## 7. Backups

**Integrate with the existing `deploy/ftnbilling-backup`** from branch `gdrive-backup` (weekly encrypted rclone upload to Google Drive). Extend it; don't replace it.

**Database dumps** (`ftnbilling-backup dump`):
- A local `pg_dump -Fc` every 6 hours, run as postgres.
- Written to a temp file, checked with `pg_restore -l`, then renamed.
- Mode 0600, owned by root, in `/var/backups/ftnbilling/`.
- Retention: 8 six-hourly, 14 daily, 8 weekly, 12 monthly.
- Run by its own systemd timer, which replaces the daily cron.

**Other local backups:**
- A daily tar of `outbox.json`, `alerts.json`, `fleet.json`, `spool/` and any `ispbill.json*`.
- A weekly tar of `metrics/` (keep 4).

**Google Drive:** the weekly upload contains the newest DB dump, the env file and `wg0.conf`.

**Restore drill** (`ftnbilling-backup drill`, monthly timer, also runnable by hand):
1. `createdb ftnbilling_drill`.
2. `pg_restore` the newest dump.
3. Run `ispbill db-check` against it and compare with production.
4. `dropdb`.
5. Write `DATA/backup-status.json`.

The Settings page shows "last backup" and "last verified restore". Phase 2 alerts can add a `backup_stale` rule (over 36 hours).

**Manual restore steps** go in `deploy/README.md`.

## 8. Admin UI

**Navigation:**
- Sidebar: add "Customers", and "Equipment" under Network.
- Top bar: add a search box (`GET /admin/search`).
- Add `ptitle`/`picon` cases for `customers`, `equipment` and `search`.

**Routes:**
```
GET  /admin/search?q=
GET  /admin/customers?q=&site=&state=current|former|all&page=
GET  /admin/customers/new ; POST /admin/customers/save ; GET /admin/customers/view?id=
POST /admin/customers/link ; POST /admin/customers/moveout ; POST /admin/customers/note
POST /admin/customers/nid ; POST /admin/customers/nid/reveal ; POST /admin/customers/anonymize (type name to confirm)
GET  /admin/customers.csv
GET  /admin/equipment?kind=&status=&site=&q= ; GET /admin/equipment/view?id=
POST /admin/equipment/save ; POST /admin/equipment/assign ; POST /admin/equipment/return
POST /admin/equipment/import (CSV paste kind,serial,mac,model) ; GET /admin/equipment.csv
POST /admin/sites/profile ; POST /admin/sites/contacts/save ; POST /admin/sites/contacts/delete
GET  /admin/payments?from=&to=&site=&account=&customer=&method=&status=&q=&page= ; GET /admin/payments.csv (same filters)
POST /admin/accounts/location (floor, ODB, splitter, port)
```

**Customer profile:**
- Header with contacts and the WhatsApp opt-out.
- Accounts over time (building, floor, flat, state, move-in, move-out).
- Payments with totals.
- Equipment.
- Messages from the Phase 2 outbox for the customer's account IDs.
- A timeline of notes, events, payments and equipment events, newest first, capped at 200.

**Building profile:**
- Address and GPS, with a map link `https://www.google.com/maps?q=lat,lng` (`rel="noopener noreferrer"`).
- Go-live date and contacts.
- Equipment: router and OLT first, then counts by status.
- Revenue: this month, last month, and a 12-month table.
- Occupancy.

**Account page:** add a customer card (link, change, move out), a location form and an equipment card.

**Payments page:** a filter bar, totals (count, revenue, by method), 100 rows per page.

**Search** (`internal/billing/search.go`, in memory). Query normalisation:
- Phone: `07..` and `+254..` become `2547..`, plus a suffix match on 6 or more digits.
- MAC: separators stripped; 4 or more hex characters.
- Code, receipt and serial: uppercased, exact match then prefix.
- Name and flat: case-folded substring.

Results are grouped (Customers, Accounts, Equipment, Payments, Buildings), 20 per group. A single exact hit redirects. The national ID can be searched exactly through its blind index.

**CSV exports:**
- Reuse `csvSafe`.
- The payments CSV adds `customer`, `customer_phone` and `floor`.
- The customers and equipment CSVs leave out the national ID.
- Each export writes an audit event.

**Files:**
- Templates: new `admin_customers.html`, `admin_equipment.html`, `admin_search.html`; edit `admin_layout.html`, `admin_pages.html`, `admin_sites.html`.
- Handlers: `web/customers.go`, `web/equipment.go`, `web/search.go`.

## 9. Privacy

**National ID:**
- Optional. Shown as `••••1234`.
- Revealing it is a POST that writes an audit event.
- It is never logged, exported or shown in a list.

**Data minimisation:** no date of birth and no ID scans.

**Anonymise customer:** scrubs name, contacts, ID and notes. Ledger rows are kept.

**Database access:** Postgres has no TCP listener, uses scram authentication, and `REVOKE CONNECT FROM PUBLIC`. Dumps are mode 0600.

**Logs:** never log row contents.

**Secrets in the database:** `rest_pass`, `hs_pass` and `join_hash` are stored as they are in JSON today, so treat dumps as secret. The reporting views exclude these columns.

## 10. Tests and CI

**`internal/store/storetest`:**
- `New(t) Handle{Store, Reopen()}` uses JSON by default.
- With `TEST_STORE=postgres` and `TEST_DATABASE_URL`, it uses a fresh schema per test (via `search_path`) and drops it at cleanup.
- Switch the test rigs in billing, web, multisite, sites/manager and payments/mpesa to it.

**Conformance suite** (`storetest.Run`), run on both backends:
- Persistence across reopen.
- `ErrNoChange` writes nothing.
- Rollback on `fn` error, on panic, and on an injected commit failure.
- 20 concurrent updates.
- Event sequence numbers and the 300 cap.
- Account close and reopen.
- Time round trip at microsecond precision, and the zero time.
- The strict-mode panic.

**Postgres-only tests** (skipped without `TEST_DATABASE_URL`):
- Migrations: idempotent, with checksum and downgrade guards.
- A codec round trip per table.
- A foreign-key violation rolls back memory.
- An ambiguous commit triggers a reload.
- Degraded mode, and recovery from it.
- The advisory lock blocks a second opener.
- The orders window.
- Import from v2 and v3 fixtures, checking counts and sums.
- Auto-import renames the file.
- The fork guard.
- An `export-json` / `import-json` round trip.

**Tests that need no database:**
- The diff.
- The codec field-count guard.
- `deriveCRM`.
- Search normalisation.
- National-ID crypto.
- Spool idempotency.
- `BenchmarkDiff200k`, with a budget under 15 ms.

**Web tests:**
- Customers and equipment flows.
- Payment filters and totals.
- The CSV injection guard.
- Revealing a national ID writes an audit event.

**CI:**
- A `postgres:16` service with a health check, plus a matrix leg on 14, using `TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/ispbill_test?sslmode=disable`.
- Steps:
  1. `go mod verify`
  2. `go vet`
  3. `go test ./...`
  4. `TEST_STORE=postgres go test -p 1` on billing, payments, sites and web
  5. `go test -race` on `./internal/store/...`
  6. `go build -tags nopg` for arm64
  7. the amd64 build
  8. `bash -n` on the deploy scripts

## 11. Sizing (1 vCPU, 2 GB, 60 GB; 20 × 30 flats)

**Memory**

| Component | Estimate |
|---|---|
| OS | about 200 MB |
| Postgres | about 230–280 MB |
| ispbill | about 350–450 MB, capped at 600 MiB |
| Caddy | about 50 MB |
| wg-admin (kernel WireGuard) | negligible |
| **Total** | **about 0.9–1.0 GB**, leaving about 0.9 GB for cache |

**Disk:** the database grows about 60 MB a year, all dumps together stay under 0.5 GB, and year 1 totals under 10 GB.

**CPU hot spots:**
- NetFlow ingest: set `SO_RCVBUF` to 4 MB in `sites/netflow.go`.
- Metrics saves: staggered.
- `go build` on upgrade: niced, and covered by the swap file.
- Store diff: 1–10 ms.

**Ceiling:** comfortable to about 40 buildings and 1,500 flats. After that, move to 2 vCPU and 4 GB.

## 12. Order of work

1. `go.mod`, `go.sum`, `vendor/`: add pgx v5, pinned to a release compatible with Go 1.24.
2. `internal/store`: `store.go`, `backend.go`, `diff.go`, `jsonfile.go`, `migrate.go`, then `storetest/` and the conformance suite on JSON.
3. `internal/store/pgstore/`: store, tables, migrate, import, migrations and tests.
4. Config, `main.go`, `cli.go`, `backend_pg.go`/`backend_nopg.go`, `build.sh`, Dockerfile.
   - **Milestone: Postgres is the store, with no new UI yet.**
5. Billing: close account, `CustomerID`, `uniqueCode`, then the new `crm.go`, `equipment.go`, `search.go`, `reports.go` and `secretbox.go`.
6. `payments/spool.go`, plus the M-Pesa and callback edits.
7. Test rigs on `storetest`, with the suites passing on Postgres.
8. Web handlers and templates.
9. `deploy/install.sh`, backup script integration, `deploy/README.md`, `README.md`.
10. CI.
11. Follow-up, not in this phase: move the outbox to Postgres.

## 13. Risks

- **Memory and database diverge.** Mitigated by committing inside the lock, rolling back, reloading after an ambiguous commit, the advisory lock and `db-check`. Nothing but `Store` writes these tables.
- **A constraint rejects something the code allows.** The result is a rolled-back update with a visible error, not corruption.
- **Phase 2 `WAOptOut` writes.** They must go through the shared helper so the customer copy stays in sync.
- **Time precision.** Microsecond truncation; no code compares a stored time with `time.Now()` for equality.
- **Postgres down means no writes.** Mitigated by the spool, alerts and OOM protection for Postgres.
- **Losing `DATA_KEY`** makes stored national IDs unreadable. It goes into the weekly secrets backup.
