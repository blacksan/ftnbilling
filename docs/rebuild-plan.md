# Fast Tech Net rebuild: implementation plan (server-centric IPoE)

Source: Fable planner, 2026-11-18. User decisions: server is central (billing, data, metrics, provisioning downstream); MikroTik does OLT + uplink + DHCP/queues/lists as told; no WireGuard for admin/app (wg-admin removed); public /admin with session login + TOTP; website gets "Admin login" link; router-initiated site tunnel wg0 stays; IPoE (DHCP by ONT MAC, no Hotspot); **one MAC can never be used by more than one account**; native Android operator app over HTTPS.

Code facts that shape the plan:
- `internal/store` is an in-memory DB (`store.Data`); Postgres gets per-table `ChangeSet` deltas via codecs in `internal/store/pgstore/tables.go`. New tables need a `Data` map, a `Delta`, a diff entry and a codec (shared files) → serial W0 first.
- `internal/billing/tasks.go` (`enqueueLocked`, `RunSiteTasks`, `reconcile`, `Sweep`) is a durable router task queue with backoff; the IPoE reconciler reuses it.
- MAC uniqueness is per building today: index `accounts_site_mac_open (site_id, upper(mac))`, `Data.AccountByMAC(siteID, mac)`, `store.Validate`. `billing.NormalizeMAC` already gives upper-case colon form.

## A. Decisions

| Topic | Decision | Why |
|---|---|---|
| RADIUS | None. API-only. | RouterOS can't disconnect DHCP leases, so RADIUS adds no enforcement. Static leases, queues and the suspended list live on the router, so a dead VPS/tunnel changes nothing for flats. Usage from simple-queue byte counters (collector already polls `SimpleQueues`). |
| Site tunnel | Keep wg0 as is. | Router-initiated, keepalive 25 s, per-site join token (`sites.ConsumeJoinOpts`), REST over `http://<tunnelIP>:8081` restricted to the server's tunnel address. Only customer-network sections of the script change. |
| Password hash | Keep PBKDF2-SHA256 600k (`internal/settings/password.go`). | Stdlib, existing hashes keep working; Argon2id at 64 MB/attempt on a public login is a memory-exhaustion lever on a 2 GB VPS. Basic-auth HMAC cache in `internal/web/auth.go` goes; keep single-slot semaphore and `tracker`. |
| Web session | Cookie `__Host-ftn_session` (Secure, HttpOnly, SameSite=Strict, Path=/). 32 random bytes, stored as SHA-256. Idle 30 min, absolute 12 h, new ID after login and after TOTP. CSRF bound to session, keeps `_csrf` field name; POSTs also check `Origin`/`Sec-Fetch-Site`. If `PUBLIC_BASE_URL` not https (tests), cookie `ftn_session` without prefix. | |
| 2FA | TOTP RFC 6238 (SHA-1, 6 digits, 30 s, ±1 step), stdlib. Secret sealed with `internal/secretbox` (`DATA_KEY`). Replay blocked by last accepted step. 10 single-use recovery codes, hashed. QR server-side PNG via vendored `rsc.io/qr`. TOTP mandatory: password-only session reaches only enrol page. | |
| Bootstrap | One-time setup code printed by installer (hash in settings); required at login until TOTP enrolled. | Closes the password-only gap after /admin goes public. |
| Users | `admin_users` table, roles `owner`, `operator`, `viewer`. Existing `admin.pass_hash` setting seeds owner on first boot. | |
| Rate limiting | Reuse `tracker`: 5 failures / 15 min per IP. Per account: 10 failures / 15 min blocks logins from addresses with no recent success; never hard-lock the owner. TOTP failures count. Same response whether or not user exists. | |
| Audit log | Extend `events` (kind `audit`) with `actor`, `ip`. | Reuses `Data.AddEvent`, `EventReader`, PG history. |
| API tokens | Opaque random, SHA-256 at rest, one `api_devices` row per phone. Access 15 min; refresh 30 d sliding, 90 d absolute; rotate on every use; reuse of older refresh revokes device, 60 s grace. | |
| MAC uniqueness | Global across all buildings among open accounts. Closing/deleting frees the MAC; suspended/expired keep it. Equipment MACs normalised and checked in service layer; unique index only if data already clean. | |
| Existing duplicates | Never dropped/blanked. Migration flags all but one keeper with `mac_conflict = true` + audit event. Flagged accounts excluded from unique index and provisioning; listed in admin, API and `db-check` until fixed. | |
| Unknown MACs | `ftn-unreg` pool, 10-min leases, walled; redirect to `/pay` ("new here? call 0100133133"). Reconciler records them via `NoteDevice` so the Unregistered devices page/attach flow keeps working. | |
| Landing page | One public `/pay` for suspended and unregistered. Building found by request source IP vs site WG endpoint (`Manager.PeerStatus`); customer enters flat/account code. STK push arrives over cellular. | |
| Reconciler | Desired state = `accounts` table. Per account: static lease, simple queue `ftn-<code>`, `ftn-suspended` entry when not entitled. Triggers: task queue (instant after payment/admin action) + full per-site diff every 60 s. Sequential REST calls, un-suspends first. Touches only rows whose comment starts `ftn:acct:`. Foreign row with same MAC/IP → reported as drift, left alone. | |
| Per-site mode | `sites.access_mode`: `hotspot`, `ipoe_observe`, `ipoe`. Observe computes/reports but never suspends. Hotspot code stays compiled and gated; delete in a follow-up. | |
| Android | No Hilt, no Room. Manual `AppContainer` + ViewModel factories. OkHttp + kotlinx.serialization (fallback org.json if plugin fails with AGP 9.1.1). Skeleton with green CI first. | |

Removed: wg-admin (interface, UDP 51821, `deploy/ftnbilling-admin-peer`, doctor checks, backup of `wg-admin.conf`), `ADMIN_ALLOW_CIDR`/`AdminAllowCIDR`/`adminAllowed`/`adminNet`, Basic auth + cache, Caddy `@admin` 403 block; in the app `tunnel/`, `web/`, WireGuard + zxing deps, VpnService, CAMERA permission, cleartext exception for 10.98.0.1. Kept: `fromTunnel` (routers must never reach admin or API).

## B. API contract: `https://isp.ftw.co.ke/api/v1`

Conventions: JSON UTF-8; RFC 3339 UTC times; money integer KES; MACs `AA:BB:CC:DD:EE:FF` (input accepts any common form). Auth `Authorization: Bearer <access_token>`; cookies ignored on `/api/`. Mutating calls accept `Idempotency-Key` (UUID); server replays stored response for 24 h per device.

Errors: `{"error":{"code":"...","message":"plain words","details":{}}}`

| HTTP | code | Notes |
|---|---|---|
| 400 | `bad_request`, `validation_failed` | `details.fields` = `{"mac":"..."}` |
| 401 | `invalid_credentials`, `totp_invalid`, `token_expired`, `token_revoked` | expired → refresh; revoked → log in again |
| 403 | `totp_enrol_required`, `forbidden_role` | |
| 404 | `not_found` | |
| 409 | `mac_in_use`, `flat_in_use`, `duplicate_payment`, `lower_plan_active`, `bad_state` | |
| 429 | `rate_limited` | `Retry-After` + `details.retry_after_s` |
| 503 | `db_unavailable` | maps `store.ErrUnavailable` |

`mac_in_use` details: `{"mac":"AA:..","account":{"id","code","flat","site_id","site_name"}}`.

Pagination: `?limit=` (default 50, max 200), `?cursor=` opaque. Response `{"items":[...],"next_cursor":"..."|null}`.

Auth:
| Method/path | Request | Response |
|---|---|---|
| POST `/auth/login` | `{username,password,totp?,recovery_code?,setup_code?,device:{install_id,name,model,platform:"android",app_version}}` | `{access_token,access_expires_at,refresh_token,refresh_expires_at,device_id,user:{id,username,role}}` |
| POST `/auth/refresh` | `{refresh_token}` | same token fields |
| POST `/auth/logout` | – | 204, revokes this device |
| GET `/me` | | `{user,device:{id,name,created_at}}` |
| GET `/auth/devices` | | `{items:[{id,name,model,app_version,created_at,last_seen_at,last_ip,current,revoked_at}]}` |
| DELETE `/auth/devices/{id}` | | 204 (owner, or own device) |

Read:
| Path | Response |
|---|---|
| GET `/dashboard?site=` | `{sites_total,sites_online,accounts:{active,expired,suspended,new,mac_conflict},revenue:{today_kes,month_kes,last_month_kes},unmatched_payments,unregistered_devices,open_alerts,pending_router_tasks,generated_at}` |
| GET `/sites` | `{items:[Site]}` |
| GET `/sites/{id}` | `Site` + `provision` + `uptime_30d_pct` |
| GET `/sites/{id}/qoe?period=24h\|7d\|30d` | `{latency_ms,loss_pct,wan:{down_bps,up_bps,cap_mbps,peak_util_pct},series:{t:[],down_bps:[],up_bps:[],rtt_ms:[]},flags:[{kind,text}]}` from `metrics.BuildReport` |
| GET `/sites/{id}/onts` | `{items:[{account_id,code,flat,mac,ip,lease_status:"bound"\|"waiting"\|"none",last_seen,host_name,down_bps,up_bps,suspended_on_router,optical:null}]}` |
| GET `/accounts?site=&state=&q=&cursor=&limit=` | page of `Account` |
| GET `/accounts/{id}` | `Account` + `customer`, `payments` (last 20), `events` (last 20), `usage:{month_bytes}` |
| GET `/packages?site=` | `{items:[{id,name,speed_mbps,duration_min,price_kes,disabled}]}` |
| GET `/payments?site=&status=&method=&from=&to=&q=&cursor=&limit=` | page of `Payment` + `totals:{count,revenue_kes}` |
| GET `/alerts` | `{active:[{key,rule,label,site_id,site_name,state,since,detail}],history:[...],mutes:[{site_id,until}]}` |
| GET `/unregistered?site=` | `{items:[{site_id,mac,ip,source,first_seen,last_seen,hits}]}` |
| GET `/search?q=` | `{accounts:[...],customers:[...],payments:[...]}` from `billing.Search` |
| GET `/audit?cursor=` | page of `{seq,time,actor,ip,msg,account_id,site_id}` (owner only) |

Write (role operator/owner):
| Path | Body | Result |
|---|---|---|
| POST `/accounts` | `{site_id,flat,mac,tenant?,phone?,notes?}` | 201 `Account`; 409 `mac_in_use`/`flat_in_use` |
| PATCH `/accounts/{id}` | `{flat?,tenant?,phone?,notes?}` | `Account` |
| PUT `/accounts/{id}/mac` | `{mac}` | `Account`; 409 `mac_in_use`; 400 `validation_failed` |
| POST `/accounts/{id}/suspend`, `/unsuspend` | – | `Account` |
| POST `/accounts/{id}/package` | `{package_id,keep_time}` | `Account` |
| POST `/accounts/{id}/extend` | `{package_id,note?,force?}` | `Account`; 409 `lower_plan_active` (this is also "activate") |
| POST `/accounts/{id}/reconcile` | – | 202 `{queued:true}` |
| POST `/payments` | `{account_id,amount_kes,trans_code?,package_id?,paid_at?,note?,force?}` | 201 `Payment`; 409 `duplicate_payment` |
| POST `/payments/{key}/assign` | `{account_id,package_id,note?}` | `Payment` |
| POST `/payments/{key}/resolve` | `{status:"refunded"\|"ignored",note?}` | `Payment` |
| POST `/alerts/mute` | `{site_id,minutes}` (0 = unmute) | 204 |

Shapes:
```json
Account {"id","code","site_id","site_name","flat","floor","mac","mac_conflict":false,"ip",
 "tenant","phone","whatsapp","state":"active|expired|suspended|new","suspended":false,
 "package":{"id","name","speed_mbps","price_kes"}|null,"expires_at":null,
 "router":{"in_sync":true,"suspended_on_router":false,"lease_status":"bound","last_seen":null,"pending_task":false,"last_error":""},
 "customer_id","created_at"}
Site {"id","name","access_mode","online":true,"cause":"","tunnel":{"handshake_age_s":12},
 "accounts":{"total","active","expired","suspended"},"open_alerts":0,
 "provision":{"last_run_at","in_sync":true,"drift":0,"pending_ops":0,"last_error":"","foreign_conflicts":[]}}
Payment {"key","site_id","method","status","amount_kes","phone","name","bill_ref","account_id","account_code","flat","package_id","note","paid_at","created_at"}
```
Checked in as `docs/api-v1.md` in W0; later changes need a version-bump note there.

## C. Database

### `internal/store/pgstore/migrations/0004_ipoe_auth.sql`
```sql
-- Forward-only; never edit once released.
ALTER TABLE accounts ADD COLUMN mac_conflict boolean NOT NULL DEFAULT false,
                     ADD COLUMN ip text NOT NULL DEFAULT '';
UPDATE accounts SET mac = regexp_replace(upper(regexp_replace(mac,'[^0-9A-Fa-f]','','g')),
       '^(..)(..)(..)(..)(..)(..)$','\1:\2:\3:\4:\5:\6') WHERE mac <> '';
UPDATE accounts SET mac_conflict = true
 WHERE mac <> '' AND mac !~ '^([0-9A-F]{2}:){5}[0-9A-F]{2}$';
WITH r AS (
  SELECT id, row_number() OVER (PARTITION BY mac ORDER BY
           (expires_at IS NOT NULL AND expires_at > now() AND NOT suspended) DESC, created_at, id) AS rn
  FROM accounts WHERE closed_at IS NULL AND mac <> '' AND NOT mac_conflict)
UPDATE accounts a SET mac_conflict = true FROM r WHERE a.id = r.id AND r.rn > 1;
INSERT INTO events (seq, time, site_id, kind, account_id, msg)
SELECT (SELECT coalesce(max(seq),0) FROM events) + row_number() OVER (ORDER BY id), now(), site_id, 'audit', id,
       'MAC conflict found by migration 0004: flat ' || flat || ' (' || code || ') has MAC ' || mac ||
       ', already used or malformed. Not provisioned until fixed.'
FROM accounts WHERE mac_conflict;
DROP INDEX accounts_site_mac_open;
CREATE UNIQUE INDEX accounts_mac_open ON accounts (mac)
  WHERE closed_at IS NULL AND mac <> '' AND NOT mac_conflict;
CREATE UNIQUE INDEX accounts_site_ip_open ON accounts (site_id, ip)
  WHERE closed_at IS NULL AND ip <> '';
ALTER TABLE accounts ADD CONSTRAINT accounts_mac_shape
  CHECK (mac_conflict OR mac = '' OR mac ~ '^([0-9A-F]{2}:){5}[0-9A-F]{2}$');

UPDATE equipment SET mac = regexp_replace(upper(regexp_replace(mac,'[^0-9A-Fa-f]','','g')),
       '^(..)(..)(..)(..)(..)(..)$','\1:\2:\3:\4:\5:\6') WHERE mac <> '';
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM equipment WHERE mac <> '' AND status <> 'retired' GROUP BY mac HAVING count(*) > 1) THEN
    CREATE UNIQUE INDEX equipment_mac_live ON equipment (mac) WHERE mac <> '' AND status <> 'retired';
  END IF; END $$;

ALTER TABLE sites ADD COLUMN access_mode text NOT NULL DEFAULT 'hotspot'
  CHECK (access_mode IN ('hotspot','ipoe_observe','ipoe'));
ALTER TABLE events ADD COLUMN actor text NOT NULL DEFAULT '', ADD COLUMN ip text NOT NULL DEFAULT '';
CREATE INDEX events_audit ON events (seq DESC) WHERE kind = 'audit';

CREATE TABLE admin_users (id text PRIMARY KEY, username text NOT NULL,
  role text NOT NULL CHECK (role IN ('owner','operator','viewer')),
  pass_hash text NOT NULL DEFAULT '', totp_secret_enc text NOT NULL DEFAULT '',
  totp_confirmed_at timestamptz, totp_last_step bigint NOT NULL DEFAULT 0,
  recovery_codes jsonb NOT NULL DEFAULT '[]',
  disabled boolean NOT NULL DEFAULT false, pass_changed_at timestamptz,
  created_at timestamptz NOT NULL, updated_at timestamptz);
CREATE UNIQUE INDEX admin_users_username ON admin_users (lower(username));
CREATE TABLE admin_sessions (id text PRIMARY KEY,
  user_id text NOT NULL REFERENCES admin_users(id) DEFERRABLE INITIALLY DEFERRED,
  csrf text NOT NULL, mfa boolean NOT NULL DEFAULT false,
  ip text NOT NULL DEFAULT '', user_agent text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL, expires_at timestamptz NOT NULL);
CREATE TABLE api_devices (id text PRIMARY KEY,
  user_id text NOT NULL REFERENCES admin_users(id) DEFERRABLE INITIALLY DEFERRED,
  install_id text NOT NULL DEFAULT '', name text NOT NULL DEFAULT '', model text NOT NULL DEFAULT '',
  platform text NOT NULL DEFAULT '', app_version text NOT NULL DEFAULT '',
  access_hash text NOT NULL DEFAULT '', access_expires_at timestamptz,
  refresh_hash text NOT NULL DEFAULT '', refresh_prev_hash text NOT NULL DEFAULT '',
  refresh_rotated_at timestamptz, refresh_expires_at timestamptz, absolute_expires_at timestamptz,
  last_ip text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, last_seen_at timestamptz,
  revoked_at timestamptz, revoked_reason text NOT NULL DEFAULT '');
CREATE UNIQUE INDEX api_devices_access ON api_devices (access_hash) WHERE access_hash <> '';
CREATE UNIQUE INDEX api_devices_refresh ON api_devices (refresh_hash) WHERE refresh_hash <> '';
```
Notes: verify on PG 14, 16 and 18; check how `EventSeq` is loaded (expected `max(seq)`) so inserted audit rows don't collide; PG 23505 on `accounts_mac_open` in `pgstore.Commit` → new `store.ErrDuplicateMAC`. Adjust column names to the real 0001 schema where they differ (e.g. `closed_at`, `suspended`, `expires_at`, `equipment.status`).

W0 implementation choices (recorded after review):
- Only text that is exactly 12 hex digits once `[-:. ]` is stripped is rewritten to `AA:BB:CC:DD:EE:FF`; anything else is left as typed and flagged. Go `CanonMAC` and the SQL strip the same set; a test runs the same vectors through both.
- The old `accounts_site_mac_open` index is dropped first (rewriting spellings can make rows equal), and `SET CONSTRAINTS ALL IMMEDIATE` runs before each `CREATE INDEX`.
- Audit events are written only for flagged accounts with `closed_at IS NULL`. Closed accounts with a malformed MAC are flagged silently, because the `accounts_mac_shape` CHECK covers every row and needs the flag.
- Equipment: `equipment_mac_live` is always created. Of live (not retired) items that share a MAC after normalising, the keeper is an installed one, then the oldest, then the lowest id. The others are set to `retired`, detached from their flat (a retired item cannot have an account), keep their MAC, get a note saying what they were, and each gets an audit event. Nothing is deleted.
- Integrity refusals (SQLSTATE class 23) are `store.ErrIntegrity`: Update rolls back and never counts them towards degrading. Specific errors: `ErrDuplicateMAC`, `ErrDuplicateIP`, `ErrBadMAC` (`accounts_mac_shape`), `ErrDuplicateEquipmentMAC`, `ErrDuplicateUsername`, `ErrDuplicateToken`, `ErrBadAccessMode`. The JSON backend enforces the same rules in Update.
- Writing a MAC with `ChangeMAC` clears `mac_conflict`; the same MAC on a flagged account is a re-check (cleared if free, `DuplicateMACError` if not).
- Import from JSON drops browser sessions and revokes app devices (users sign in again).

### Store format 6 (JSON path + shared structs)
- `store.FormatVersion = 6`. New fields: `Account.MACConflict`, `Account.IP`, `Site.AccessMode`, `Event.Actor`, `Event.IP`. New maps + deltas: `AdminUsers`, `AdminSessions`, `APIDevices`.
- `internal/store/migrate.go`: `migrateV5toV6` with same normalise/flag/audit behaviour. Existing `.v5.bak` copy in `jsonBackend.Load` protects the original.
- Move `NormalizeMAC` into `internal/store` (billing keeps a wrapper).
- `Store.Update` pre-commit invariant when the accounts delta is non-empty: no two open, non-conflict accounts share a MAC; none share `(site, ip)`. Failure → rollback, `store.ErrDuplicateMAC`. Enforces the rule on the JSON backend.
- `Data.AccountByMAC` becomes global (`AccountByMAC(mac)`); `store.Validate` reports global duplicates and conflict-flagged rows (printed by `db-check`).
- `admin_sessions.last_seen_at` written at most once per 60 s per session.
- First boot (W1): bootstrap creates owner in `admin_users` from live `AdminUser` + `admin.pass_hash`.

## D. Workstreams

Branching: integration branch `rebuild` (cut from `admin-settings`, PR #3). W0 lands on `rebuild` first; W1, W2, W3 fork from it. W4 forks from `android-app`, starts now against section B. Opus reviews W0 before the fork, then each PR. Merge order W0, W1, W2 (rebases on W1), W3; W4 independent.

### W0 foundation (serial, small) — branch `rebuild-w0-foundation`
- Create: `migrations/0004_ipoe_auth.sql`, `docs/api-v1.md` (section B verbatim).
- Change: `internal/store/{store.go,backend.go,diff.go,core.go,migrate.go,validate.go}`, `internal/store/pgstore/{tables.go,pgstore.go,codec_test.go}`, `internal/store/storetest/*`.
- Frozen interfaces (stubs, so W1/W2 compile against same signatures):
  - `billing.DuplicateMACError{AccountID, Code, Flat, SiteID, SiteName}` matching `ErrDuplicateMAC` via `errors.Is`.
  - `billing.ProvisionStatus` and `(*Service).ProvisionStatus(siteID)`, `(*Service).AccountRouterState(accountID)`.
  - `internal/routeros/client.go`: generic row API on `Client`: `Rows(ctx, path, query)`, `AddRow(ctx, path, body)`, `SetRow(ctx, path, id, diff)`, `RemoveRow(ctx, path, id)`, with `norouter.go` stubs and in-memory tables in `mock.go`.
- Tests: codec round-trip for every new field; migration test with seeded duplicates and malformed MACs (keeper rule, flags, events, index present); JSON v5→v6 migration test; the Update invariant; conformance suite on both backends.
- Accept: existing CI matrix green; a DB with a cross-building duplicate MAC migrates, starts, and reports the conflict.

### W1 auth, public admin, /api/v1 — branch `rebuild-w1-auth-api`
- Create: `internal/authn/` (`totp.go`, `tokens.go`, `sessions.go`, `users.go`, `recovery.go`, tests); `internal/web/session.go`, `login.go` (login, TOTP, enrol, recovery, logout), `api.go` (router, bearer middleware, error envelope, pagination, idempotency), `api_auth.go`, `api_accounts.go`, `api_payments.go`, `api_sites.go`, `api_misc.go`, `security_headers.go`; templates `admin_login.html`, `admin_security.html` (2FA, sessions, devices, audit).
- Change: `internal/web/auth.go` (drop Basic auth + cache; keep `tracker`, semaphore, `checkPassword`); `internal/web/admin.go` (`admin()` wrapper → session, role, CSRF; logout is POST); `internal/web/server.go` (remove `adminNet`/`adminAllowed`; keep `fromTunnel` and apply to `/api/` too; HSTS + CSP; redact `/api/v1/auth/*` bodies from logs); `internal/web/scope.go` (`safeNext`); `internal/web/settings.go` and `templates/admin_pages.html` AdminCIDR row; `templates/layout.html` or `portal.html` ("Admin login" link); `templates/admin_layout.html` (nav); `internal/config/config.go` (delete `AdminAllowCIDR`); `internal/settings/{password.go,manager.go}` (admin hash lives in `admin_users`); `cmd/ispbill/cli_settings.go` (`admin-reset-password` gains `-reset-totp`, `-setup-code`, `-revoke-sessions`); `go.mod` + `vendor/` for `rsc.io/qr`; all web tests using `SetBasicAuth` move to a shared login helper.
- Tests: TOTP RFC 6238 vectors, replay rejected; session idle/absolute expiry, ID rotation, cookie attributes; CSRF missing/wrong/cross-origin; lockout per IP and per account; enrol-only gate; setup code; recovery code single use; refresh rotation, reuse detection, grace, device revoke; every API endpoint incl. role matrix and 409 `mac_in_use` body; requests from `WG_SUBNET` get 404 on `/admin` and `/api`.
- Accept: no Basic auth path remains; `/admin` works from a public address with password + TOTP; `docs/api-v1.md` implemented exactly; `ftnbilling-admin-reset --generate` still recovers access.

### W2 IPoE provisioning, reconciler, join script, MAC rules — branch `rebuild-w2-ipoe`
- Create: `internal/billing/provision.go` (`desiredFor(site)`, `ReconcileSite`, diff, IP allocation, `AdoptLeases`, status) + `provision_test.go`; `internal/routeros/rows.go` (REST row API reusing PATCH-then-`/set` fallback from `rest.go` `upsert`); `internal/web/pay.go` + `templates/pay.html`; golden files `internal/sites/testdata/join_ipoe_join.rsc`, `join_ipoe_cutover.rsc`.
- Change: `internal/billing/tasks.go` (`reconcile` branches on `AccessMode`; `Sweep` runs per-site diff; `ResyncAll` skipped for IPoE); `internal/billing/service.go` (`CreateAccount`/`ChangeMAC` use global lookup, return `DuplicateMACError`; `CreateAccount` allocates IP); `internal/billing/equipment.go` (`changeMACLocked`, `SaveEquipment` MAC check) and `admin_ops.go` (`MoveAccount` reallocates IP; `DeleteAccount` frees MAC and IP); `internal/routeros/telemetry.go` (`SimpleQueue` gains `Comment`, `BytesUp/BytesDown`); `internal/metrics/collector.go` (IPoE path: per-flat usage from queue counters `ftn-<code>`, skip `ActiveSessions`); `internal/sites/layout.go` (`PlanSubnet` gains `UnregFrom`/`UnregTo` base+10..base+59, `StaticFrom` base+60); `internal/sites/join.rsc.tmpl`, `joinscript.go` (`ScriptInput` gains `Accounts []LeaseSeed`, `Stage`), `join.go`; `internal/web/sites.go` + `templates/admin_sites.html` (Join only / Cut over to IPoE commands, Adopt leases, mode switch, Confirm cutover, provisioning status); `internal/web/portal.go` + `routerfiles.go` (hotspot pages only for hotspot-mode sites); devices section of `templates/admin_pages.html` (MAC-conflict list).
- Join script, IPoE (every object commented `ftn:`):
  - Stage `join` (no customer impact): sections 0–3 as today (WireGuard, wg firewall, REST user and `www` restricted to tunnel address) + traffic flow.
  - Stage `cutover`: 1) `/system backup save` + `/export file=ftn-pre-ipoe`. 2) script `ftn-rollback` (disable `ftn-dhcp`, re-enable hotspot server and old DHCP server, remove ftn NAT/filter rules) + one-shot scheduler running it after 15 min. 3) discover hotspot interface/gateway from the router (don't assume `br-hotspot`). 4) pool `ftn-unreg`, DHCP server `ftn-dhcp` (disabled, `authoritative=yes`, lease 10 m), one static lease + one simple queue per flat rendered from DB, address-list entries for unreg range and currently suspended flats. 5) firewall/NAT: DNS allowed; walled lists may reach `PUBLIC_IP` tcp 80/443; tcp 80 from walled lists dst-nat to `PUBLIC_IP:80`; everything else from them rejected; established/related for customer subnet accepted above FastTrack so queues apply. 6) switch: disable hotspot server and old DHCP server, enable `ftn-dhcp`. 7) ping WAN and server tunnel address, print result.
  - Never touches WAN interface, routes, admin users, or services other than `www`. Nothing removed; old objects disabled. Scheduler removed by "Confirm cutover" (server calls REST) or by hand. Later stage `cleanup` deletes disabled hotspot objects.
- Tests: reconciler on mock — idempotent second pass zero writes; drift repair (deleted lease, edited queue, stray `ftn:acct:` row removed); foreign rows untouched and reported; un-suspend first; observe mode never writes suspended list; conflict-flagged accounts skipped; payment via `Credit` removes from list; router failure leaves task queued with backoff; MAC uniqueness across buildings in create/change/assign-equipment/move on both backends; golden files for both stages; every created object has an `ftn:` comment.
- Accept: with `ROUTER_MOCK`, an `ipoe` site converges from empty router to full state; expiry, payment, suspend and MAC change each reach the router within one task run.

### W3 installer, doctor, Caddy, wg-admin removal — branch `rebuild-w3-deploy`
- Delete `deploy/ftnbilling-admin-peer`.
- `deploy/install.sh`: remove wg-admin step (`TOTAL` −1) and `ADM_*` vars; `LISTEN_ADDR=127.0.0.1:8080`. In-place migration block: pre-upgrade dump; archive `/etc/wireguard/wg-admin.conf` and `/root/ftnbilling-admin-*.conf` to `/root/ftnbilling-removed-<date>/`; `systemctl disable --now wg-quick@wg-admin`; delete 51821 and `on wg-admin` ufw rules; run `migrate`; print MAC-conflict report; generate and print setup code. Caddyfile: drop `@admin` block; `http://` catch-all redirect to `https://$DOMAIN/pay`; keep X-Forwarded-For warning.
- `deploy/ftnbilling-doctor`: remove wg-admin/51821/`ADMIN_ALLOW_CIDR` checks; add `/admin/login` 200 over https, nothing on 51821, app bound to loopback, TOTP enrolled, schema version 4, MAC-conflict count.
- `deploy/ftnbilling-backup` (no `wg-admin.conf`), `deploy/ftnbilling-admin-reset` (pass new flags), `deploy/README.md`, `deploy/BACKUP.md`, `README.md`, `.github/workflows/ci.yml` (drop `admin-peer` from shell checks), `deploy/install_test.go`.
- Depends only on W1 CLI flag names (fixed above).
- Tests: `DRY_RUN=1` installer run, shellcheck, dry-run output contains no `wg-admin`.
- Accept: one-liner on a copy of the live server keeps data, serves `/admin` publicly, leaves wg0 untouched, passes doctor.

### W4 Android native app — branch `android-native` (from `android-app`)
- Touches only `android/**` and `.github/workflows/android.yml`. Must invoke skill `anthropic-skills:android-m3-expressive`.
- Delete: `tunnel/`, `web/`, `net/AdminHttp.kt`, `net/AdminApi.kt`, `net/PublicHealth.kt`, `ui/common/TunnelImportPanel.kt`, `ui/onboarding/*`, `ui/admin/*`, their tests, manifest/Gradle entries listed under Removed.
- Keep: `ui/theme/*` unchanged, `core/crypto/*` (Keystore AES-GCM `SecretStore` holds tokens), `auth/AppLock.kt`, `ui/lock/`, `core/prefs/Settings.kt`.
- Layers (single `:app`, package `ke.co.ftw.ftnadmin`): `core/network` (`ApiClient` on OkHttp, bearer interceptor, `Authenticator` with single-flight refresh, `ApiError` from envelope, idempotency-key interceptor); `data/<feature>` (DTOs, remote source, repository for auth, dashboard, accounts, payments, sites, alerts); `domain` (models, MAC normaliser/validator, a few use cases); `ui/<feature>` (screen, ViewModel with `StateFlow` UI state, navigation).
- Screens: Login (password, TOTP, recovery); Dashboard; Customers list/search/filter + detail (activate/extend, suspend, change package, edit MAC showing `mac_in_use` with owning flat); Payments (list, record, assign unmatched); Buildings (list, detail, ONT, QoE); Alerts (mute); Settings (devices, app lock, logout).
- `network_security_config.xml`: system CAs only, no cleartext; `API_BASE` = `https://isp.ftw.co.ke`, overridable in debug.
- Tests: MockWebServer for login, refresh, reuse-revoked, 409 mapping; ViewModel tests; MAC validator tests. CI `lint testDebugUnitTest assembleDebug`.
- Accept: first commit is a green-CI skeleton; no WireGuard/WebView code remains; tokens never written outside `SecretStore`.

### Shared files
| File | Owner | Others |
|---|---|---|
| `internal/store/**`, migration 0004, `routeros/client.go` signatures | W0 | read-only after W0 |
| `internal/web/server.go`, `admin.go`, `auth.go`, `config.go` | W1 | W2 adds routes only in its own files |
| `templates/admin_pages.html` | W1 (AdminCIDR row), W2 (devices section) | W2 rebases after W1 |
| `templates/admin_layout.html` | W1 | W2 must not edit |
| `cmd/ispbill/main.go` | W1 | W2 via `Sweep` |
| `go.mod`, `vendor/` | W1 | |
| `.github/workflows/ci.yml` | W3 | |

## E. Live migration runbook (summary)
Server: fresh dump + VPS snapshot; copy `/etc/ftnbilling/`, `/etc/wireguard/`, Caddyfile off box; confirm public SSH; run installer one-liner; note MAC-conflict report and setup code; log in at /admin with password + setup code, enrol TOTP, store recovery codes offline; doctor; KES 1 test payment; install APK and log in. Rollback: previous release + pre-upgrade dump.

Router: 1) all 27 flats on the server with right MAC, package, expiry (currently on router local billing — export and import first). 2) router backup + export. 3) Join only, watch a day. 4) Adopt current leases. 5) pilot one flat (hotspot ip-binding bypassed + manual lease/queue/list) and run P0 tests. 6) announce window. 7) set `ipoe_observe`, paste Cutover. 8) verify within 15 min. 9) Confirm cutover (else auto-rollback after 15 min). 10) check observe report matches reality, switch to `ipoe`, suspend/pay a test flat. 11) after 7 days cleanup stage, stop router local billing container.

## F. Verification on RB5009 / V-SOL (P0 before cutover)
1. RouterOS 7.20 `authoritative=yes`: static lease MAC gets fixed IP, unknown MAC gets pool address, changed lease picked up at renewal. 2. Outage tolerance: stop wg0, reboot router; leases/queues/list persist. 3. Suspended list: DNS ok, HTTP → /pay, HTTPS fails fast, Android + iOS, ONT router mode. 4. Simple queues shape correctly with FastTrack bypass; direction right. 5. REST add/patch/delete on leases/queues/address-lists incl. PATCH fallback, 100 queued calls after outage. 6. ONT WAN MAC stable per flat; note bridge mode and ONT swap. 7. Rollback scheduler really restores Hotspot. 8. Tunnel recovers after uplink flap.
P1: OLT refuses unauthorised ONT; OLT management only from router/tunnel; queue counters monotonic; RB5009 CPU at peak; Daraja production behaviours; /pay building detection.
P2: option 82 on V-SOL, SNMP optical OIDs; ARP reply-only.

## G. Risks
Public admin attack surface (Opus reviews W1 hardest); wrong expiries at cutover (use observe); MAC cloning (OLT ONT authorisation is the real control); HTTPS can't be redirected (WhatsApp + STK carry payment); shared IP/CGNAT for /pay detection; unverified RouterOS behaviour until P0; sessions in store → DB outage blocks logins; new Android toolchain; two code paths until hotspot removal; single VPS.

## H. Addendum (user, 15:52): pick the MAC from what actually asked for an address
The MAC on an ONT sticker is often its LAN MAC, not the WAN MAC the router sees. The operator must see every MAC that is connected but not configured, and attach it to an account in one tap, on web and in the app. Owners: W0 (contract), W2 (server/web), W1 (API handlers), W4 (app).
- Source of truth (W2): the reconciler's per-site pass reads `/ip/dhcp-server/lease` (all servers on the customer VLAN, dynamic leases, and leases in `ftn-unreg`) plus `/ip/arp` on the customer interface, and calls `NoteDevice` for every MAC that has no open account. `RefreshHosts` gets an IPoE branch doing the same on demand (the page's Refresh button). Record per device: MAC, IP, DHCP host-name, client-id/vendor-class if present, OUI vendor (small embedded OUI table or prefix lookup; unknown → blank), first/last seen, hits, `online` (lease bound or ARP complete within last 2 min), source (`dhcp`, `arp`, `portal`, `hotspot-host`).
- Also surface mismatches: an account whose configured MAC has not been seen for >24 h while an unregistered MAC is online in the same building is shown as "Possible ONT swap / wrong MAC" with one-click "Replace MAC with this one".
- Web page `/admin/devices` (W2 owns the template section): sorted online-first then last seen; columns MAC (copy), vendor, host-name, IP, last seen, online dot; actions: **Attach to account** (searchable picker of accounts in that building, defaulting to accounts with no MAC or with a not-seen MAC), **Create account** (flat, tenant, phone, package prefilled), Dismiss. Attaching goes through `ChangeMAC` so the one-MAC-per-account rule applies and a conflict shows "MAC already used by flat X (code) in building Y". After attach the reconciler pushes the static lease immediately (task queue) so the flat is online within seconds.
- Badge count of online unregistered devices in nav and on the dashboard.
- API (add to docs/api-v1.md; W1 implements): `GET /unregistered?site=&online=` items `{site_id,site_name,mac,vendor,host_name,ip,source,online,first_seen,last_seen,hits,suggested_account:{id,code,flat}|null}`; `POST /sites/{id}/unregistered/refresh` → 202; `POST /unregistered/attach` `{site_id,mac,account_id}` → `Account` (409 `mac_in_use`); `POST /unregistered/create` `{site_id,mac,flat,tenant?,phone?,package_id?}` → 201 `Account` (409 `mac_in_use`/`flat_in_use`); `POST /unregistered/dismiss` `{site_id,mac}` → 204.
- App (W4): "New devices" screen reachable from dashboard badge and building detail; list as above with pull-to-refresh (calls refresh then reloads); tap → sheet with Attach to account (searchable) / Create account / Dismiss; conflict error names the owning flat.
