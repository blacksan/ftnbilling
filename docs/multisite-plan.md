# Fast Tech Net (ispbill) multi-site refactor: implementation plan

The owner approved this plan. Module `ispbill`, Go 1.24, standard library only.

**Owner decisions:**
- **Tunnel:** native RouterOS WireGuard.
- **Packages:** one shared list, with overrides per site.
- **Admin:** a single admin.
- **QoE features:** fleet overview, per-building drill-down, uptime history, and alerts.
- **Alert channels:** email, Telegram, and the WhatsApp Cloud API. WhatsApp also sends messages to tenants.

## 0. Decisions

| Topic | Decision |
|---|---|
| Billing service | One `billing.Service` and one store, plus a router resolver per site. Account codes stay global. |
| Site record | `store.Site` lives inside `ispbill.json`. The format moves from 2 to 3. |
| Router WireGuard key | The server generates the router's keypair with `crypto/ecdh` X25519 when it renders the script, and stores only the public key. |
| Server WireGuard key | The installer generates it (`wg genkey`). The app only receives `WG_PUBLIC_KEY`. |
| Applying peers | The app writes `DATA/wg/peers.conf` and touches `DATA/wg/apply`. A root `.path` unit then runs a validating sync script that calls `wg syncconf`. |
| wg0 | `wg-quick@wg0`, address `10.99.0.1/22`, UDP 51820, MTU 1380, no IP forwarding. |
| Join token | 32 random bytes, base64url-encoded. Stored as a SHA-256 hash with a 30-minute TTL, usable once, at `GET /j/{token}`. |
| Legacy and container mode | A site with `Mode="env"` takes its router and metrics config from today's environment variables. |
| Metrics storage | One collector and one file per site (`DATA/metrics/<site>.json`). Saved every 15 minutes, staggered across sites. |
| NetFlow | One UDP socket, demultiplexed by the source tunnel IP. Each peer's AllowedIPs is a /32, so the source IP authenticates the site. |
| Notification secrets | Kept in env only. Thresholds and toggles are stored and edited on an admin page. |
| Outbox | A separate `DATA/outbox.json` owned by `internal/notify`. Sends are idempotent by dedupe key. |
| Admin access | A separate WireGuard interface `wg-admin` (10.98.0.0/24) with QR onboarding; SSH tunnel as fallback. `/admin` is never public. |

## 1. Data model (`internal/store/store.go`), with `FormatVersion = 3`

```go
const (SiteModeWG = "wg"; SiteModeEnv = "env")
type Site struct {
    ID   string `json:"id"`   // "s" + 6 hex, immutable, used in portal URLs
    Name string `json:"name"`
    Mode string `json:"mode"` // wg | env
    Notes, SupportPhone string
    Disabled bool // paused: no collection, no router tasks
    Removed  bool // tombstone; keeps ledger names
    // WireGuard / REST (Mode=wg)
    TunnelIP    string    `json:"tunnel_ip"` // 10.99.0.x
    WGPublicKey string    `json:"wg_pub"`
    RESTUser    string    `json:"rest_user"` // "ftn-api"
    RESTPass    string    `json:"rest_pass"` // 32 alnum
    RESTPort    int       `json:"rest_port"` // 8081
    Join        JoinToken `json:"join"`
    JoinedAt    time.Time `json:"joined_at"` // first successful REST ping after a join
    // Router layout (metrics + script)
    HotspotServer  string // "hotspot1"
    WANInterface   string // "ether1"
    WANList        string // "WAN"
    OLTInterface   string // "sfp-sfpplus1"
    VLANID         int    // 0 = untagged
    CustomerSubnet string // "10.5.50.0/24"
    WANCapMbps, OLTCapMbps, FUPTB float64
    Provision       bool   // script builds bridge/DHCP/hotspot
    BenchPort       string // optional
    LegacyContainer bool   // script stops old ispbill/tailscale containers
    PkgOverrides map[string]PkgOverride `json:"pkg_overrides"` // by package ID
    CreatedAt time.Time
}
type PkgOverride struct { Hidden bool `json:"hidden,omitempty"`; PriceKES int `json:"price_kes,omitempty"` } // 0 = default
type JoinToken struct { Hash string; ExpiresAt, CreatedAt, UsedAt time.Time; UsedFrom string }
```

**Changed types:**
- `Account` gains `SiteID`, `WhatsApp` (format 2547XXXXXXXX) and `WAOptOut bool`. Add the last two now, so there is only one format bump.
- `Order`, `Payment`, `Task`, `Event` and `Device` gain `SiteID`.
- `Voucher` gains `SiteID`; `""` means valid at any site.
- `Data` gains `Sites map[string]Site`.
- `Devices` is re-keyed to `siteID + "|" + MAC`.

**Helpers:**
- `AccountByMAC(siteID, mac)`.
- `flatTaken`, scoped to the site.
- `AccountByCode` stays global.
- `SiteList()`: non-removed sites, natural sort by name.
- `SiteByTunnelIP(ip)`.
- `EffectivePackage(siteID, pkgID)` and `EffectivePackages(siteID)`: a copy with the price replaced when the override is non-zero, and `Disabled = Disabled || Hidden`.
- `NextTunnelIP(prefix)`: the lowest free host after `.1`, skipping IPs held by non-removed sites.

**Migration from format 2 to 3** goes in `internal/store/migrate.go`, with a table test using a v2 fixture:
- Format 3 files load as they are.
- For a format 2 file:
  1. Copy it to `ispbill.json.v2.bak` (mode 0600).
  2. Load it and migrate it.
  3. If it has any accounts or payments, create `Site{ID:new, Name:"Main building", Mode:env, HotspotServer:"hotspot1", LegacyContainer:true}`.
  4. Stamp that site's ID on every row. Vouchers keep `SiteID=""`.
  5. Re-key devices.
  6. Save.
- Any other format returns `ErrOldFormat`.

**Behaviour in `main`:**
- `main` fills the env site's display fields (router URL, user and password, WAN/OLT, capacities, subnet) from config at runtime; they are not persisted. `SITE_NAME` renames the env site once, if it is still called "Main building".
- If `DATA/metrics.json` exists and `DATA/metrics/` does not, move the file to `DATA/metrics/<envSiteID>.json`.
- **Fresh data with `ROUTER_URL` or `ROUTER_MOCK` set** (container mode): seed the env site.
- **Fresh VPS** (no `ROUTER_PASS`): start with zero sites.
- `config.Validate` stops requiring `ROUTER_PASS`. `main` fails only when an env site exists, `ROUTER_MOCK` is off and `ROUTER_PASS` is empty.

## 2. Package layout and runtime per site

```
internal/wg      keys.go (X25519 keygen, ValidKey), peers.go (render peers.conf, touch apply), status.go (parse wg dump)
internal/sites   manager.go, runtime.go, hostguard.go (moved from web), joinscript.go + join.rsc.tmpl, netflow.go (UDP demux)
internal/fleet   (P2) prober, outages, summary
internal/alerts  (P2) engine, rules, state
internal/notify  (P2) outbox, smtp.go, telegram.go, whatsapp.go, tenant.go, inbound.go
```

### `billing.Service`

The `rt routeros.Client` field becomes `routers RouterSource`:

```go
type RouterSource interface { Router(siteID string) routeros.Client }
func NewService(st *store.Store, rs RouterSource, log *slog.Logger) *Service
```

**Router source and per-site state:**
- Add `billing.SingleRouter(rt)` for tests.
- `profilesOK` and `taskMu` become per-site state: `siteState{profilesOK atomic.Bool; taskMu sync.Mutex}`, held in a `sync.Map`.

**Tasks:**
- `RunTasks(ctx)` fans out one goroutine per site that has due tasks, so one dead router cannot stall the others.
- Add `RunSiteTasks(ctx, siteID)`. `Credit`, `CompleteOrder` and the admin operations call it for the account's own site.
- Tasks for a disabled or unjoined site stay queued without using up attempts.

**Signatures that take a site:**
- `CreateAccount(in)`, where `AccountInput.SiteID` is required
- `AccountByMAC(siteID, mac)`, `NoteDevice(siteID, mac, ip, source)`, `DismissDevice(siteID, mac)`
- `HotspotHosts(ctx, siteID)`, `RefreshHosts(ctx, siteID)`, `SyncProfiles(ctx, siteID)`
- `PackageForAmount(siteID, amount)`, `Ping(ctx, siteID)`, `Directory(siteID)`
- `GenerateVouchers(pkgID, siteID, n)`

**Moving an account:** `MoveAccount(ctx, id, toSiteID)` removes the account on the old site and creates it on the new one. The `enqueueLocked` merge rule therefore keys on `(AccountID, SiteID)`.

**Packages and pricing:**
- `SavePackage` and `DeletePackage` mark every site `profilesOK=false` and start a background sync per site with a 15-second timeout. They never block the HTTP request.
- `CreateOrder` prices from `EffectivePackage` and refuses hidden packages.
- `RecordManual`, `AssignPayment`, `ExtendAccount` and `ChangePackage` accept hidden packages but compare against the effective price.

**Vouchers:** redemption is rejected when `v.SiteID != "" && v.SiteID != a.SiteID`.

### Payments

- `PackageForAmount(siteID, amount)`. C2B finds the account by its code, then calls `PackageForAmount` with `acct.SiteID`.
- Unmatched rows get a `SiteID` when the account is known.
- Validation: no two enabled effective prices may be the same within one site. Raise this as an error, not a warning.

### `sites.Manager`

```go
type Runtime struct { Site store.Site; Client routeros.Client; Col *metrics.Collector; Guard *HostGuard; cancel func(); done chan struct{} }
type Options struct { Cfg config.Config; Store *store.Store; Dir func(siteID string) metrics.Directory; Log *slog.Logger; NewClient func(store.Site) routeros.Client }
func (m *Manager) Start(ctx)
func (m *Manager) Router(siteID) routeros.Client   // unknown/unjoined -> client returning ErrNoRouter
func (m *Manager) Collector(siteID) *metrics.Collector
func (m *Manager) Guard(siteID) *HostGuard
func (m *Manager) Reload(siteID)                    // stop (cancel, wait, final save), rebuild; no start if Removed/Disabled
func (m *Manager) ApplyPeers() error
```

**Lifecycle by event:**
- **Create:** `ApplyPeers`.
- **Join consumed:** update the store, then `ApplyPeers` and `Reload`.
- **Edit:** `Reload`.
- **Rotate keys:** the same as a join.
- **Disable:** `Reload`, which stops the runtime. The peer is kept.
- **Remove:** drop the peer, stop the runtime, delete the metrics file, free the tunnel IP and wipe the secrets. Keep a tombstone.

**Router client by site mode:**
- **`wg`:** `routeros.NewREST("http://<TunnelIP>:<RESTPort>", RESTUser, RESTPass)`.
- **`env`:** today's `ROUTER_URL` client, or the mock.

### `metrics`

- **Constructor and NetFlow:**
  - `New(path string, ...)` takes the full file path.
  - Remove the collector's own UDP listener and keep `HandleNetflow`.
  - `sites/netflow.go` owns `NETFLOW_ADDR` and maps the source IP to a site by `TunnelIP`.
  - Unknown sources go to the env site if there is one. Otherwise they are counted as `stray` and dropped.
- **Polling:** jitter each loop's first tick by `hash(siteID) % interval`.
- **Ports and retention:**
  - For `wg` sites, `SamplePorts` records only the WAN, OLT, `br-hotspot` and VLAN/bridge interfaces named in the site config.
  - Per-flat service days are kept for 62 days, down from 400.
- **Snapshot:** add `Snapshot()`, which copies `Live` under the lock.

### `cmd/ispbill/main.go` startup order

1. config
2. store (migrates)
3. `sites.Manager`
4. `billing.Service(manager)`
5. seed packages
6. `manager.Start`
7. payments
8. the Phase 2 services
9. web

On shutdown, wait for the manager.

## 3. Routes, pages and sidebar

### Public routes

- **Portal:** `GET /portal?site=&mac=&ip=&dst=`. `PortalCtx` gains `Site`. If the site is missing or unknown, use the only active site if exactly one exists; otherwise show the neutral page.
- **Portal forms** carry a hidden `site` field. The status link in `alogin.html` includes `site=`.
- **Hotspot pages:** `GET /routeros/hotspot/{site}/{name}` renders `{{PORTAL}}` and `{{SITE}}`. The legacy `/routeros/hotspot/{name}` and `/routeros/remote.rsc` are served only while an env site exists.
- **Join script:** `GET /j/{token}`. It returns 404 with the same body for any token that is bad, expired or used.
- **Health:** `/healthz` adds `sites_total` and `sites_online`. `router_ok` comes from cached state.
- **WhatsApp webhook:** `GET/POST /wa/webhook` (Phase 2, optional).

The host guard is always on for `wg` sites; `VERIFY_HOTSPOT_HOST` affects only the env site. There is one guard per site. The portal header shows `Brand · Site.Name`.

### Admin

The selected site is held in the `ftn_site` cookie (Path=/admin); its value is a site ID or `all`. `adminPage` gains `Sites`, `Site` and `AllSites`.

```
GET  /admin/site/select?site=<id|all>&next=<path>
GET  /admin/sites ; GET /admin/sites/new ; POST /admin/sites/create
GET  /admin/sites/view?id= ; POST /admin/sites/update
POST /admin/sites/join (mint/rotate; command shown once) ; POST /admin/sites/join/revoke
GET  /admin/sites/status.json?id=  (token state, handshake age, REST ok)
POST /admin/sites/packages ; POST /admin/sites/sync ; POST /admin/sites/disable
POST /admin/sites/remove (type name to confirm; refused while accounts exist)
POST /admin/accounts/move
```

**Existing list pages:**
- These pages filter by the selected site: accounts, devices, payments, `payments.csv`, unmatched, orders and vouchers.
- When "All buildings" is selected, they show a Building column.
- The create-account and voucher forms get a Building select.

**Metrics pages:** these need a specific site. With "All" selected, `/admin/metrics*` redirects to `/admin/sites` in Phase 1 and to the fleet page in Phase 2.

**Phase 2 routes:**

```
/admin/fleet, /admin/uptime?site=&days=30, /admin/alerts (+settings, test, mute), /admin/messages (+retry, cancel), POST /admin/sites/outage-notice
```

### Sidebar

- Fleet overview (Phase 2)
- Dashboard
- Subscribers ▾ Accounts, Unregistered devices
- Billing ▾ Payments, Record M-Pesa, Unmatched, Portal orders
- Plans ▾ Packages, Vouchers
- Network ▾ Usage and QoE, Uptime (Phase 2), Usage CSV
- Buildings ▾ All buildings, Add building
- Alerts ▾ Alerts, Messages (Phase 2)
- System ▾ Settings and policy, Payments CSV
- Log out

A site switcher `<select>` sits in the top bar.

## 4. Join token and router script

### Token

- **Minting:**
  - `POST /admin/sites/join` creates 32 bytes from `crypto/rand`, base64url-encoded.
  - Store the SHA-256 hash, with `ExpiresAt = now + JOIN_TOKEN_TTL` (default 30 minutes).
  - A new mint replaces any earlier token. The plaintext token is shown once.
  - Refuse to mint unless `PUBLIC_BASE_URL` is `https://`. `ALLOW_INSECURE_JOIN=true` overrides this for bench use.
- **Consuming** (`GET /j/{token}`):
  1. Look the token up with a constant-time hash comparison, then check that it has not expired and has not been used.
  2. In one `store.Update`, generate the router keypair and a new REST password. Set `WGPublicKey`, `RESTPass` and `Mode=wg`. Allocate `TunnelIP` if the site has none. Set `UsedAt` and `UsedFrom`.
  3. After the update, run `ApplyPeers` and `Reload`, then write the script.
- **Limits and logging:**
  - Rate-limit `/j/` by client IP to 10 per minute.
  - Logs show the path as `/j/[redacted]`.
  - A token is consumed only when the User-Agent starts with `Mikrotik`, so link-preview bots can't use it up. `JOIN_REQUIRE_MIKROTIK_UA` (default true) turns this check off.
- **Rotation:** works the same way. The old peer and password stay valid until the new script is fetched.

### The paste

```
/tool/fetch url="https://DOMAIN/j/TOKEN" dst-path=ftn-join.rsc check-certificate=yes; :delay 1s; /import ftn-join.rsc
```

The page also shows the prerequisites:
- RouterOS 7.
- `/system/device-mode/update hotspot=yes fetch=yes`, followed by the button press.
- NTP set up.
- A fallback command with `check-certificate=no`, shown with a warning.

### Script (`internal/sites/join.rsc.tmpl`, rendered with `text/template`)

- The server renders every value; there are no router-side globals.
- Every object's comment starts with `ftn:`.
- Objects that get replaced are removed by comment, then added again. Objects that are kept are added only if a lookup by name finds nothing.

| # | Section | On failure |
|---|---|---|
| 0 | Preflight: print the RouterOS version; `:error` if the major version is below 7 | abort |
| 1 | WireGuard: interface `wg-ftn` (add, or set `private-key`), `mtu=1380`. Address = TunnelIP with `network=10.99.0.1`. Peer: server public key, endpoint address and port, `allowed-address=10.99.0.1/32`, `persistent-keepalive=25s` | abort |
| 2 | Firewall: input accept `in-interface=wg-ftn src-address=10.99.0.1` TCP to RESTPort; input accept ICMP from 10.99.0.1; drop all other input from `wg-ftn`; forward drop in and out of `wg-ftn` | abort |
| 3 | REST: group `ftn-api` (`read,write,test,sensitive,rest-api`); user `ftn-api` with `address=10.99.0.1/32` and the new password; `/ip/service set www port=RESTPort disabled=no address=10.99.0.1/32`, adding `172.17.0.0/24` if `LegacyContainer` | abort |
| 4 | Customer network (only if `Provision`): take the OLT port out of other bridges; VLAN `vlan<ID>-customers` (skipped when the ID is 0); `br-hotspot`; gateway address; `pool-hotspot`; `dhcp-hotspot` and its network; srcnat masquerade to WANList; DNS `allow-remote-requests`; optional bench port | count, continue |
| 5 | Hotspot (only if `Provision`): profile `ispbill-hsprof` (`login-by=http-pap,http-chap,mac-cookie`); server `HotspotServer` on `br-hotspot`; drop hotspot traffic to the REST port; forward drop so flats are isolated from each other | count, continue |
| 6 | Pages: read `html-directory`; fetch the six files from `/routeros/hotspot/<site>/<name>`; walled-garden IP accept for `PUBLIC_IP` | count, continue |
| 7 | Traffic flow: `enabled=yes interfaces=br-hotspot`; target `dst-address=10.99.0.1 port=2055 version=5`; remove the target commented `ispbill metrics`; print a note about fasttrack | count, continue |
| 8 | Legacy (only if `LegacyContainer`): stop and disable the `ispbill` and `tailscale` containers via `[:parse]` inside `:do {} on-error={}` | ignore |
| 9 | Finish: `/ping 10.99.0.1 count=3`; print the count of failed sections; `/file remove ftn-join.rsc` | - |

**Rules for the generated script:**
- **Customer subnet:** Go computes the gateway (`.1`) and the pool (`.10` to the last address minus one). Accept /20 to /24 only.
- **Firewall placement:** place rules with the guard `:if ([:len [find]] > 0) do={ add ... place-before=([find]->0) } else={ add ... }`.
- **Golden-file tests** cover: provisioning on and off, VLAN 0, and the legacy flag. No `{{` may be left in the output.

## 5. WireGuard on the server

**Addressing and endpoint:**
- `WG_SUBNET=10.99.0.0/22`, server at `.1`, sites from `.2`.
- On the server, each site's peer has `AllowedIPs = <TunnelIP>/32` only.
- `WG_ENDPOINT` defaults to `PUBLIC_IP:51820`. Use the IP, not a domain, and document that the VPS IP must stay static.

**`internal/wg`:**
- `GenerateKeypair()` uses `ecdh.X25519().GenerateKey(rand.Reader)`. `ValidKey` checks for 44 characters that decode to 32 bytes.
- `peers.go` renders sorted `[Peer]` blocks, writes them atomically, then rewrites `DATA/wg/apply`.
- `status.go` parses `/run/ftnbilling/wg0.dump` (tab-separated). A missing file means the status is unknown.

**Installer:**
1. Install `wireguard-tools`.
2. On first install, run `wg genkey` and write `/etc/wireguard/wg0.conf` (mode 0600): `Address=10.99.0.1/22`, `ListenPort=51820`, `MTU=1380`, `PrivateKey`, `PostUp=/usr/local/sbin/ftnbilling-wg-sync`.
3. Write `WG_PUBLIC_KEY`, `WG_ENDPOINT` and `WG_SUBNET` to the env file. Never regenerate the key on an upgrade.
4. Install `/usr/local/sbin/ftnbilling-wg-sync`. It copies `peers.conf` to a root-owned temp file. It refuses symlinks, anything that is not a regular file, and files over 256 KB.
5. The sync script accepts only these lines:
   - `[Peer]`
   - `PublicKey = <44 base64>`
   - `AllowedIPs = 10.99.x.y/32` within `WG_SUBNET`
   - blank lines

   Any other line makes it exit non-zero without changing anything. Otherwise it runs `wg syncconf wg0 <(wg-quick strip wg0; cat validated)`.
6. Add the systemd units:
   - `ftnbilling-wg-sync.service` (oneshot);
   - `ftnbilling-wg-sync.path` (`PathChanged=/var/lib/ftnbilling/wg/apply`);
   - `ftnbilling-wg-status.service` and `.timer`. Every 60 seconds the status job runs `wg show wg0 dump | tail -n +2 | cut -f1,3-7 > /run/ftnbilling/wg0.dump`, with mode 0640 and group `ftnbilling`.
7. Enable `wg-quick@wg0`.
8. `ftnbilling.service` gets `After=` and `Wants=wg-quick@wg0.service`, and `Environment=GOMEMLIMIT=600MiB GOGC=50`.
9. ufw:
   - allow 51820/udp;
   - delete the public allow for 2055/udp;
   - `allow in on wg0 to 10.99.0.1 port 2055 proto udp`;
   - `deny in on wg0`;
   - `route deny in on wg0`.
10. Warn if `ip_forward=1`.
11. Set up the `wg-admin` interface and the first admin device (QR code); print the SSH tunnel line as a fallback.
12. Drop `ROUTER_PASS` and `ROUTER_URL` from the fresh env template; keep them on upgrade. The final banner says: Admin → Buildings → Add building.
13. Back up these files daily for 30 days: `ispbill.json`, `outbox.json`, `alerts.json`, `fleet.json`. Back up `metrics/` weekly and keep 4.

## 6. Packages and M-Pesa

- **Overrides:** they live on each building's page, as a table with "Hidden here" and "Price here (KES)" columns.
- **Router profiles:** every package that isn't deleted is pushed to every site.
- **Portal:** it shows `EffectivePackages(site)`.
- **C2B payments:** the account code gives the account, the account gives `SiteID`, and the site's effective packages decide `PackageForAmount`. If nothing matches, the payment goes to Unmatched with its `SiteID` set.

## 7. Phase 2

### 7.1 Fleet prober (`internal/fleet`)

- **Probing:**
  - Every 30 seconds, TCP-dial `TunnelIP:RESTPort` with a 3-second timeout.
  - Every 60 seconds, also read the collector snapshot and the WireGuard dump for the handshake age.
- **Status:** a site is marked down after 3 failed dials in a row, and up again after 2 successes.
- **Outages:** each is recorded as `{SiteID, Start, End, Cause}`. Cause is one of:
  - `no-handshake` (handshake older than 3 minutes);
  - `tunnel-up-rest-down`;
  - `unknown`.
- **Storage:** `DATA/fleet.json` holds a `metrics.Series` per site, using Cell fields N, Loss, RTT, RTTN and RTTMax. Outages are kept 400 days, capped at 2000 per site. Saved every 5 minutes.
- **Env site:** probed with `Client.Ping`.
- **Fleet page:** one tile per building, plus a totals row, auto-refreshing every 30 seconds via meta refresh. Each tile shows:
  - status and since when;
  - handshake age;
  - current WAN load against capacity (bar);
  - router ping RTT and loss;
  - tunnel RTT;
  - active flats out of total;
  - revenue today and this month;
  - open alerts.
- **Uptime page:** a 30/90-day selector, a strip of daily availability %, a tunnel RTT line chart, and an outage table.
- **Drill-down:** the existing `/admin/metrics` page for the selected site.

### 7.2 Alerts (`internal/alerts`)

The engine runs every 30 seconds. Default rules, all editable:

| Key | Default |
|---|---|
| `offline:<site>` | down for 5 minutes |
| `wan_sat:<site>` | WAN at 85% or more of capacity for 15 minutes |
| `loss:<site>` | 5% or more for 10 minutes |
| `latency:<site>` | 150 ms or more for 10 minutes |
| `olt_down:<site>` | OLT interface `Running=false` for 2 minutes |

**How alerts behave:**
- **Lifecycle:** pending, then firing (notify), then resolved. A recovery message goes out only if the firing message was sent.
- **Throttling:**
  - 60-minute cooldown per key.
  - Optional repeat every 6 hours.
- **Offline sites:** while a site is offline, its other rules are frozen.
- **Storm guard:** if more than half of the sites (and at least 3) go offline within 2 minutes, send a single alert instead of one per site.
- **Mute:** per site, with an expiry.
- **Storage:** state lives in `DATA/alerts.json`: Settings, Active, and History (capped at 1000 entries).
- **Delivery:** alerts create `notify.Message`s for each enabled admin channel.

### 7.3 Notify and outbox (`internal/notify`)

```go
type Message struct {
  ID, Channel /*email|telegram|whatsapp*/, Kind, To, SiteID, AccountID string
  Template, Lang string; Params []string
  Subject, Text string
  DedupeKey string; Priority int
  Status string // queued|sent|failed|skipped|cancelled
  Attempts int; NotBefore, NextTry, CreatedAt, SentAt time.Time
  LastErr, ProviderID, Delivery string
}
```

**Queue and retention:**
- `Enqueue` does nothing if the `DedupeKey` already exists. Keys are kept 90 days.
- Sent rows are kept 60 days, capped at 5000.

**Sending and retries:**
- **Workers and rate limits:** one worker per channel. WhatsApp 5 per second, with `WA_DAILY_LIMIT` (default 240) tenant messages per day; admin alerts are exempt. Telegram 1 per second. SMTP 1 every 2 seconds.
- **Backoff:** start at 30 seconds and double up to 1 hour, for at most 8 attempts.
- **Permanent errors:** these fail immediately.
- **Auth errors:** these pause the channel and show a banner.

**Before sending to tenants:**
- A `Validate` hook runs before each tenant send. Messages that no longer apply are marked `skipped`.
- Quiet hours: tenant reminders go out only between 07:00 and 21:00 EAT, using `NotBefore`.

**Channels:**
- **Email (`smtp.go`):** `net/smtp` with STARTTLS on 587, or implicit TLS on 465. Env: `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM`, `ALERT_EMAIL_TO`.
- **Telegram (`telegram.go`):** `POST https://api.telegram.org/bot<token>/sendMessage`. Env: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`.
- **WhatsApp (`whatsapp.go`):**
  - `POST https://graph.facebook.com/<WA_API_VERSION>/<WA_PHONE_NUMBER_ID>/messages` with `Authorization: Bearer WA_TOKEN`.
  - The body is a template payload: `messaging_product`, `to`, `type=template`, `template{name, language{code}, components[body parameters]}`.
  - Env: `WA_TOKEN`, `WA_PHONE_NUMBER_ID`, `WA_ADMIN_NUMBERS`, `WA_API_VERSION`, `WA_LANG` (default `en`), plus optional `WA_APP_SECRET` and `WA_VERIFY_TOKEN`.
  - Parameters are cleaned before sending: no newlines or tabs, runs of spaces collapsed, at most 200 characters.

**Tenant scanners** (`tenant.go`) run every 60 seconds, and also right after `Credit`:

| Kind | Trigger | Dedupe | Template |
|---|---|---|---|
| Expiry reminder | the sweeper's `reminder_due` rule | `rem:<acct>:<expiresUnix>` | `ftn_expiry_reminder` |
| Expired | `REMINDER_ON_EXPIRY`, for plans of 7 days or longer | `exp:<acct>:<expiresUnix>` | `ftn_plan_expired` |
| Payment received | applied in the last 6 hours, method not `admin`, amount at least `WA_MIN_CONFIRM_KES` (default 250) | `pay:<paymentKey>` | `ftn_payment_received` |
| Outage notice | manual, or automatic after `offline` has fired for 30 minutes (setting: off / manual / auto, default manual) | `out:<site>:<startUnix>:<acct>` | `ftn_outage_notice` |
| Outage resolved | only to tenants who got the notice | `outok:<site>:<startUnix>:<acct>` | `ftn_outage_resolved` |

**Recipients:** accounts with a `WhatsApp` number and `WAOptOut=false`. The account form gets a WhatsApp number field (normalised with `billing.NormalizePhone`), a "same as phone" option and an opt-out checkbox.

### 7.4 Meta templates to create

**Prerequisites:**
- A Meta Business portfolio and a WhatsApp Business Account.
- A dedicated number registered to the Cloud API; it can't also be in use in the WhatsApp app.
- A System User with a permanent token that has the `whatsapp_business_messaging` and `whatsapp_business_management` permissions.
- A payment method.
- Tenants' consent to receive messages.

All templates use category Utility and language `en`.

| Name | Body | Params |
|---|---|---|
| `ftn_expiry_reminder` | Hello, your {{1}} internet plan for {{2}} ends on {{3}}. To stay connected, pay KES {{4}} via {{5}}, account number {{6}}. Thank you. | package, flat + building, expiry, price, pay hint, account code |
| `ftn_plan_expired` | Hello, the internet plan for {{1}} ended on {{2}}. To reconnect, pay via {{3}}, account number {{4}}. You will be reconnected automatically. | flat + building, expiry, pay hint, code |
| `ftn_payment_received` | Payment received: KES {{1}}, reference {{2}}. Your {{3}} plan for {{4}} is active until {{5}}. Thank you. | amount, receipt, package, flat + building, expiry |
| `ftn_outage_notice` | Hello, we are aware of an internet outage at {{1}} since {{2}}. Our team is working on it. We apologise for the inconvenience. | building, start |
| `ftn_outage_resolved` | Internet service at {{1}} was restored at {{2}}. Thank you for your patience. | building, time |
| `ftn_admin_alert` | Network alert, {{1}}: {{2}} at {{3}}. Details: {{4}}. Time: {{5}}. End of alert. | PROBLEM/RECOVERED, rule, building, detail, time |

The five tenant templates carry the footer "Reply STOP to opt out". The WhatsApp test button sends `ftn_admin_alert` to `WA_ADMIN_NUMBERS`.

### 7.5 Inbound webhook (optional, last)

- **Verification** (`GET /wa/webhook`): if `hub.mode=subscribe` and `hub.verify_token` equals `WA_VERIFY_TOKEN`, echo `hub.challenge`.
- **Incoming events** (`POST /wa/webhook`):
  - Verify `X-Hub-Signature-256` (`sha256=` plus the hex HMAC of the raw body, keyed with `WA_APP_SECRET`) using a constant-time comparison.
  - Reply 200 quickly.
  - `statuses[]` update `Message.Delivery`.
- **Commands** (`messages[]`), matched by the sender's number against `Account.WhatsApp`:
  - `BALANCE` or `STATUS`: account state, package, expiry, code and how to pay.
  - `STOP` or `START`: toggle `WAOptOut`.
  - Anything else gets a help reply.
- **Reply limits:**
  - Replies are free-form session messages.
  - Unknown numbers get the support phone only.
  - Deduplicate on message ID (last 2000).
  - At most 5 replies per sender per minute.
- **Registration:** the routes exist only when both `WA_APP_SECRET` and `WA_VERIFY_TOKEN` are set.

## 8. Storage budget (20 sites × 30 flats, 1 GB VPS)

**Billing database (`ispbill.json`):** saved on every change.

**Site metrics (`metrics/<site>.json`):**
- Sampling: sessions every 60 seconds, ports every 30 seconds, ping every 60 seconds, DNS every 60 seconds.
- Saved every 15 minutes, staggered across sites.
- Retention tiers: 5-minute buckets for 48 hours, 1-hour buckets for 14 days, 1-day buckets for 400 days. Service days per flat are kept 62 days.

**Other files:**
- `fleet.json`: probe every 30 seconds, save every 5 minutes. Outages kept 400 days.
- `alerts.json`: history capped at 1000.
- `outbox.json`: 60 days, capped at 5000.

**Budget:**
- About 10 MB of memory per site, so about 200 MB for 20 sites. Run with `GOMEMLIMIT=600MiB` and `GOGC=50`.
- The README should say the design is comfortable up to about 40 sites and 1500 flats.

## 9. Security

**Join script:**
- HTTPS only, with a hashed single-use token and a 30-minute TTL.
- The path is redacted in logs, and the endpoint is rate-limited.
- The script file deletes itself on the router after it runs.

**Router credentials:**
- Each site gets its own REST password: 32 random alphanumeric characters.
- The router user is restricted to `address=10.99.0.1/32`, and so is the `www` service.
- The input firewall accepts REST only from `wg-ftn`.

**Isolation between buildings:**
- Each server peer is a /32 and IP forwarding is off. `ufw route deny in on wg0` means routers cannot reach each other.
- `deny in on wg0` sits after the single 2055/udp allow.

**Admin and portal:**
- `admin()` rejects any `RemoteAddr` inside `WG_SUBNET`.
- The portal guard is per site and fails closed. Every lookup is keyed by site, because all sites share `10.5.50.0/24`.

**Data at rest:**
- `ispbill.json` now holds REST passwords. Keep it at mode 0600 and treat backups as secret.

**WhatsApp:**
- The webhook checks the HMAC.
- Replies go only to numbers that match an account.
- Tokens live in env only.

## 10. RouterOS 7.20 risks

1. **Device-mode** controls `fetch` and `hotspot`. The UI must show that step before the paste.
2. **TLS on `/tool/fetch`** needs a CA store and a correct clock. Offer the `check-certificate=no` fallback.
3. **`place-before=0`** fails on an empty table. Use the guard from section 4.
4. **Never write files from script text**: `/file add contents=` has a limit of about 4 KB. Fetch the hotspot pages one by one.
5. **`flash/` on small boards:** create the hotspot profile first, read `html-directory` back, and if a `flash` directory exists, set `html-directory=flash/hotspot-ftn`.
6. **Menus that don't exist** (for example `/container`) break the import, so emit those lines only when the matching flag is set, inside `[:parse]`.
7. **WireGuard properties:** use only `name`, `private-key`, `mtu`, `interface`, `public-key`, `endpoint-address`, `endpoint-port`, `allowed-address` and `persistent-keepalive`.
8. **Point-to-point address:** `address=<tip> network=10.99.0.1`. The fallback is `/32` plus a static route.
9. **`www` service:** moving it to 8081 and restricting it removes WebFig for the LAN. Say so on the join page.
10. **Keep `sensitive` in the REST group policy:** the REST client's upsert compares passwords, so without it users would be rewritten on every sync.
11. **Per-flat totals don't depend on NetFlow.** The script only prints a note about fasttrack.
12. **Check the User-Agent** that `/tool/fetch` sends on a real device.
13. **The endpoint is an IP address**, not a domain name.
14. **The REST layer has never run against a real device.** Do a bench session before the first real building, and use golden-file tests for the script.

## 11. Tests

Adapt the existing helpers: `newEnv` (billing), `newAppCfg` (web), `newRig` (payments) and `newCol` (metrics). Add a test router source of type `map[siteID]*routeros.Mock`.

### Phase 1

- **store:**
  - v2 → v3 migration and the `.v2.bak` copy.
  - A v3 round trip.
  - An unknown format is refused.
  - `NextTunnelIP`.
- **billing:**
  - The same MAC and the same flat label can exist in two sites.
  - Payments touch only their own site's mock.
  - A failing site B does not delay site A.
  - `MoveAccount`.
  - Per-site `profilesOK`.
- **packages:**
  - Hidden packages and price overrides.
  - `PackageForAmount` per site, and collision validation.
  - Voucher site restriction.
- **payments:** a C2B amount that matches site B's override credits a site B account, and goes to Unmatched for a site A account.
- **wg:**
  - The public key derives from the private key.
  - `peers.conf` golden file.
  - The dump parser.
- **sites:**
  - Join token: expiry, single use, a wrong User-Agent does not consume it, only the hash is stored.
  - Script golden files.
  - Manager lifecycle with mocks, run with `-race`.
- **NetFlow demux:** source A goes to collector A only; unknown sources are dropped.
- **web:**
  - The guard refuses site B's MAC when `site=A`.
  - Forms carry `site`, and the site switcher filters the lists.
  - `/admin` is refused from `10.99.x.x`.
  - `/j/` is redacted in the log.

### Phase 2

- **fleet:**
  - State machine, using a fake clock.
  - Outages.
  - Retention.
- **alerts:**
  - Hold times.
  - A recovery is sent only after a firing message was sent.
  - Cooldown.
  - An offline site freezes its other rules.
  - Storm guard.
  - Mute.
- **notify:**
  - Dedupe and backoff.
  - Permanent vs transient errors.
  - Daily cap and quiet hours.
  - `Validate` skips.
  - WhatsApp and Telegram against `httptest`, checking the exact JSON.
  - SMTP against an in-test TCP server.
- **tenant scanners:**
  - One reminder per expiry, and a new expiry gets a new reminder.
  - Opt-out is respected.
  - No confirmation is sent for admin grants.
- **webhook:**
  - Good and bad signatures.
  - The verification handshake.
  - `BALANCE` replies only to the matching number.
  - `STOP` sets the opt-out.
  - A duplicate message ID is ignored.

## 12. Phases

### Phase 1

1. **store:** `store.go`, `migrate.go`, tests.
2. **config:** add `WG_PUBLIC_KEY`, `WG_ENDPOINT`, `WG_SUBNET`, `JOIN_TOKEN_TTL`, `ALLOW_INSECURE_JOIN`, `JOIN_REQUIRE_MIKROTIK_UA` and `SITE_NAME`; relax `ROUTER_PASS`.
3. **internal/wg** and its tests.
4. **billing:** `service.go`, `tasks.go`, `packages.go`, `payments.go`, `admin_ops.go`, `directory.go`, tests.
5. **payments:** `provider.go`, `mpesa.go`, tests.
6. **metrics:** `collector.go`, `model.go`, `netflow.go`, `report.go`.
7. **internal/sites:** `manager.go`, `runtime.go`, `hostguard.go`, `netflow.go`, `joinscript.go`, `join.rsc.tmpl`, tests and golden files.
8. `cmd/ispbill/main.go`.
9. **web:**
   - `server.go`, `portal.go`, `routerfiles.go` and `routerfiles/*.html`;
   - `admin.go`, a new `sites.go`, and `metrics.go`.
10. **templates:**
    - `admin_layout.html`, `admin_pages.html`;
    - a new `admin_sites.html`;
    - `layout.html`, `portal.html`, `success.html`.
11. **docs and scripts:** `deploy/install.sh`, `deploy/README.md`, `README.md`. Mark `setup.rsc` and `remote.rsc` as legacy.

**Phase 1 exit criteria:**
- Existing single-site data starts as one building.
- Container mode still works.
- A second building can be created, joined with one paste, and sold to through the portal and Paybill using its own prices.

### Phase 2

1. **notify:** outbox, smtp, telegram and whatsapp, with config.
2. **fleet:** prober, store, summary.
3. **alerts:** engine, rules, state.
4. **notify:** `tenant.go`, plus the account fields.
5. **main.go** wiring.
6. **web:**
   - `fleet.go`, `alerts.go`, `messages.go`;
   - their templates;
   - the sidebar and settings page.
7. **docs:** the Meta checklist and templates.
8. **optional:** the inbound webhook.

## 13. Open points for the owner

- WebFig on port 80 goes away on joined routers.
- Tenant outage notices default to manual.
- A site can't be removed while it still has accounts.
- The Meta template wording must match the parameter counts exactly.
