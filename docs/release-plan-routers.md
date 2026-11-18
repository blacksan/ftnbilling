# Fast Tech Net release: Routers, auto-provisioning, new UI (W5c, W7, W8, W9)

Planned by Fable 2026-11-18, amended by the main session with the user's later decisions (see "Amendments" first — they override anything below).

## Amendments (user decisions, override the body)
- **A1. No manual Confirm anywhere.** Provisioning is automatic once an account has a package and a MAC.
  - Fresh router (no live tenants): server sets up IPoE directly; no confirm (kit rollback still guards any batch that disables something existing).
  - Live migration: server **auto-confirms** on machine evidence — tunnel up after the switch, router reaches the internet (1.1.1.1), and every entitled account (package+MAC) that was seen online in the 10 minutes before the switch is `bound` on `ftn-dhcp` at its fixed IP within 5 minutes of the switch (accounts not seen online before are not required). Pass → remove markers, mark confirmed, notify operator (WhatsApp via `alerts.Announce` + app). Fail at the deadline → server sets `ftn-armed`=`now` (router rolls back within 30 s; the router's own 15-min countdown is the backstop if the tunnel is gone) and notifies with the failing evidence.
  - Migration start: automatic as the continuation of a join paste (< 30 min old) on a router marked "already serving customers" with all blockers green; otherwise a single **Start migration** button. Keep **Roll back now** (emergency only). `POST …/setup/confirm` and `…/cutover/confirm` are removed from the app/web UI; server keeps the confirm handler only for the legacy paste path (returns the new auto state).
  - After any automatic rollback to hotspot mode, entitled accounts (package+MAC, not suspended) are given a hotspot `ip-binding type=bypassed` (comment `ftn:acct:<code>`) so a paying flat never lands on a hotspot login; the reconciler removes it when the account is not entitled or the site returns to IPoE.
- **A2. Reboot:** grant `reboot` policy to `ftn-api` (never `policy`). Owner only, typed router name, refused while a job runs, 1 per 10 min per router, audited.
- **A3.** D9 (tenant portal keeps the word "building" in 3 phrases) and D14 (Android single module, manual DI) accepted.

## 0. In-flight work
W5a (checks/fixes/Inspector/discovery) and W5b (one-liner, section wrappers, kit, join report) are finishing on `setup-w5a` / `setup-w5b`; W6 committed on `android-setup` (`/home/claude/ftn-w4`). All are kept and merged into `setup-auto` first.

## 1. Decisions
| # | Topic | Decision |
|---|---|---|
| D2 | W5c | General **router provisioning job**, kinds `fresh` and `migrate`, one state machine. |
| D3 | New routers | Created with `AccessMode = ipoe`. Hotspot only for already-joined hotspot routers until migrated. (`reconcileSite` writes nothing while `ftn-dhcp` is missing.) |
| D4 | Fresh path | No kit/arm/confirm when the job only creates `ftn:` objects and no live device is seen. If it must disable an existing hotspot/DHCP with zero live devices, arm the kit when installed and auto-confirm on evidence. |
| D6 | One-liner device-mode | Requires `fetch` always; `scheduler` (and `hotspot`) only when the router is marked "already serving customers". W5b adds `need=`. |
| D8 | Rename | User-facing "Router" everywhere. Internals keep `site` (`store.Site`, `sites` table, `internal/sites`, settings `site.<id>.*`, portal URLs). `/api/v1/sites…` stays and every such route is also served at `/api/v1/routers…`; `?router=` aliases `?site=`; JSON adds `router_id`/`router_name` beside `site_id`/`site_name`. Web moves to `/admin/routers…`, `/admin/sites…` kept (GET redirects, POST on both). |
| D9 | Tenant text | Keep "building" in the 3 `portal.html` tenant phrases. Alert labels change ("Router offline", "Many routers offline", "N of M routers") — template parameter values, no WhatsApp re-approval. |
| D10 | Web stack | Go `html/template` + `/static/app.css`, `/static/app.js` (vanilla), blocking `/static/theme.js`. No framework, no CDN. |
| D11 | Fonts | Self-host variable woff2 (latin, wght) from npm `@fontsource-variable/bricolage-grotesque`, `@fontsource-variable/inter`, `@fontsource-variable/jetbrains-mono` (OFL 1.1) under `internal/web/static/fonts/` with licence files. Comfortaa stays for the logo. If Bricolage lacks tabular figures, table numbers use Inter. |
| D12 | Reference UI | Re-implement IA and patterns only; no CSS/JS/class names/copy/assets from `/home/claude/ui-ref`. No Invoices/SMS/Leads/Tickets pages. |
| D13 | Live router data | Named endpoints only (`live`, `interfaces`, `clients`), fixed read-path lists projected into typed structs. No client-supplied paths, no raw rows. |
| D14 | Android | Single module, manual `AppContainer`, no Room; NiA layering (UiState, `stateIn(WhileSubscribed(5_000))`, repositories, type-safe routes, `NavigationSuiteScaffold`). |
| D15 | Storage | No schema change. Job state `site.<id>.prov_job`, intent `site.<id>.serving_customers`. |

## 2. End-to-end flow
### 2.1 Operator view
1. **Add router**: name; "Already serving customers?" (default No); Advanced (collapsed, prefilled from `sites.ApplyDefaults`): WAN port, OLT port, VLAN, customer subnet, WAN capacity, support phone. Save shows the join line.
2. **Paste one line** (if device-mode blocks: press the button, paste again).
3. Server does the rest; router page shows live steps; ends at **Live** (fresh) or auto-confirmed **Live** (migrate) — see A1.
4. **Add customer**: one form — name, phone, flat/label, package, router, MAC (picked from devices that router sees now, or pasted), "Activate now". Result: "Provisioned on router ✓" or "Queued — router offline, applies automatically".
5. Payment, expiry, suspension already automatic (`SetCreditHook`, `Sweep`, `ReconcileSite`).

### 2.2 Job states (`internal/sites/provision_job.go`)
`waiting_router → checking → fixing → needs_action → staging → (fresh) building → verifying → live`
`staging → (migrate) ready → arming → switching → verifying → live | rolling_back → rolled_back`; any → `failed`. (No `awaiting_confirm` per A1; `verifying` holds the evidence window.)
- Trigger: `Options.OnJoined` then after `watchJoin` records the first ping; also `POST …/setup/run`.
- Classification at `checking` from the W5a snapshot: live devices = hotspot active sessions + bound leases on non-`ftn` DHCP servers on the customer interface + complete ARP entries in `plan.Network`, excluding hosts learned on `BenchPort`. Zero → `fresh`. Non-zero with intent "No" → `needs_action` ("N live devices found: run as migration").
- Job holds the per-site reconcile lock (`billing.Service.HoldSite`, new, around `provState.run`). Every step is an idempotent "ensure" (find by name or `ftn:` comment, add or set the diff like `rowDiff`), audited via `d.AddEvent`.

| Step | Fresh | Migrate |
|---|---|---|
| preflight | checks, `EnsureIPsLocked`, `ApplyAutoFixes` | same + all `blocks_cutover` ok; record which entitled accounts were online in the last 10 min |
| backup | skipped | `ftn-cmd backup` marker; wait ≤90 s for the file |
| arm | only if something existing gets disabled | `ftn-undo` rows (written before each change) + `ftn-armed`; wait for counter = 1 |
| network | if no interface has `plan.GatewayIP`: VLAN (if set), customer bridge, port(s), gateway address | reuse detected interface (`detectCustomer`) |
| dhcp | `ftn-unreg` pool, `ftn-dhcp` enabled, DHCP network | same but disabled; leases, queues, list entries disabled |
| firewall | `ipoeRules` as REST bodies, customer masquerade, DNS-from-WAN drop rules, then `allow-remote-requests=yes` | same |
| switch | unreg list on, `arp=reply-only` | disable hotspot + old DHCP, enable `ftn-dhcp`, static ARP, enable + move queues, enable lists, `arp=reply-only` |
| telemetry | `fixTrafficFlow` | same |
| reconcile | release lock, `ReconcileSite` | after auto-confirm |
| verify | `ftn-dhcp` enabled, ping 1.1.1.1, tunnel ping; `MarkCutoverConfirmed` + `MarkCutoverSeen` | A1 evidence window → auto-confirm or roll back; notify |

Failure: fresh → stop at the failed step, Retry safe. Migrate before switch → `ftn-armed`=`now`. Tunnel lost → server stops; router countdown fires. Server restart: fresh restarts from step 1; migrate past `arming` is never resumed (router rolls back).

Job write allowlist (`provWriter`, style of W5a `fixWriter`): `/interface/bridge`, `/interface/vlan`, `/interface/bridge/port`, `/ip/address`, `/ip/pool`, `/ip/dhcp-server`, `/ip/dhcp-server/network`, `/ip/dhcp-server/lease`, `/ip/arp`, `/queue/simple`, `/ip/firewall/filter|nat|address-list`, `/ip/hotspot` (disable/enable only), `/ip/hotspot/ip-binding` (A1 bypass rows `ftn:acct:` only), `/ip/dns` (one property), `/ip/traffic-flow`. Edited/removed rows must carry `ftn:` except: disabling hotspot/DHCP servers on the customer interface, the interface ARP mode, OLT/bench bridge-port moves under W5a `assessPort`. Never `/user`, `/ip/service`, `/system/*` (except `/system/reboot` per A2), `/interface/wireguard`, routes, WAN.

`ipoeRules` (`sites/ipoe_script.go`) refactored to structured rules rendered both to script text (goldens unchanged) and REST bodies.

### 2.3 Add customer
Generalise `billing.CreateFromDevice` to `CreateCustomer(ctx, CustomerInput{SiteID, Flat, Tenant, Phone, MAC, PackageID, Activate, Note})`; `Activate` uses the existing extend path. Web `adminAccountCreate` and `POST /accounts` both call it. Unpaid new account gets lease + queue + suspended entry, so the tenant lands on `/pay`.

## 3. API additions (v4, additive; D8 aliases apply to every row)
| Method/path | Role | Body | Response |
|---|---|---|---|
| GET `/overview?router=` | viewer | | `Overview` |
| POST `/routers` | owner | `{name,serving_customers?:false,wan_interface?,olt_interface?,vlan_id?,customer_subnet?,wan_cap_mbps?,support_phone?}` | 201 `{router:Site,join:{command,command_nocheck,expires_at}}`, `no-store`, never replayed |
| PATCH `/routers/{id}` | owner | same optional fields | `Site` |
| GET `/routers/{id}/setup` | viewer | | `Setup` |
| POST `/routers/{id}/setup/run` | operator | | 202 `Setup` |
| POST `/routers/{id}/setup/migrate` | operator | `{confirm_name,keep_unknown?:false}` | 202 `Setup` |
| POST `/routers/{id}/setup/rollback` | operator | | 202 `Setup` |
| GET `/routers/{id}/live` | viewer | | `RouterLive` |
| GET `/routers/{id}/interfaces` | viewer | | `{items:[Interface],generated_at}` |
| GET `/routers/{id}/clients?q=&kind=all\|flats\|unregistered&online=` | viewer | | `{items:[Client],counts:{flats,online,unregistered}}` |
| POST `/routers/{id}/actions/reboot` | owner | `{confirm_name}` | 202 `{queued:true}`; 409 `bad_state`; 409 `router_refused` |
| POST `/routers/{id}/actions/recheck` | operator | | `Checks` (fresh) |
| POST `/routers/{id}/actions/resync` | operator | | 202 `{queued:true}` |
| POST `/accounts` (extended) | operator | adds `package_id?`, `activate?:false`, `note?`, `router_id` alias | 201 `Account` with `router` filled after one synchronous attempt |

W5 plan section 6 endpoints stay (`checks`, `fix`, `mode`, `join-command`, `cutover`, `cutover/start`, `cutover/rollback`, `bench/*`); `cutover*` are thin views over the job. `cutover/confirm` is removed from clients (A1).

```json
Setup {"router_id","state","kind":"fresh|migrate|","serving_customers":false,"auto_start":true,
 "join":{"state":"none|waiting|used|expired","expires_at","report":{"at","ok","lines":[""]}|null},
 "next_action":{"kind":"paste_join|press_button|fix|migrate|retry|none","text","check_id":""},
 "steps":[Step],"checks":{"ok":0,"warn":0,"fail":0,"unknown":0},"blockers":[""],
 "live_devices":0,"evidence":{"tunnel_up":true,"internet_ok":true,"flats_required":0,"flats_bound":0},
 "deadline_at":null,"seconds_left":0,"last_error":""}
RouterLive {"router_id","generated_at","reachable":true,
 "system":{"board","model","serial","version","arch","cpu_count":0,"cpu_load_pct":0,"mem_used_bytes":0,"mem_total_bytes":0,"disk_free_bytes":0,"uptime_s":0,"temp_c":null},
 "wan":{"interface","running":true,"down_bps":0,"up_bps":0,"cap_mbps":0,"util_pct":0},
 "tunnel":{"handshake_age_s":0,"rtt_ms":null,"loss_pct":null},
 "throughput":{"step_s":300,"t":[],"down_bps":[],"up_bps":[]},
 "flats":{"total":0,"online":0,"active":0,"expired":0,"suspended":0,"expiring_7d":0},
 "unregistered_online":0,"onts_bound":0,"equipment":{"olt":0,"ont":0},
 "revenue":{"today_kes":0,"month_kes":0},"open_alerts":0,
 "provision":{"in_sync":true,"drift":0,"pending_ops":0,"last_error":""},"setup_state":""}
Interface {"name","type","running":true,"disabled":false,"comment","mac","mtu":0,
 "role":"wan|olt|customer|tunnel|bench|other","rx_bps":null,"tx_bps":null,"rx_bytes":0,"tx_bytes":0,
 "addresses":[""],"last_link_up":null}
Client {"kind":"flat|unregistered","account_id":"","code":"","flat":"","tenant":"","state":"",
 "mac","vendor","host_name","ip","online":true,"lease_status":"bound|waiting|none","last_seen":null,
 "port":"","queue_limit":"","down_bps":0,"up_bps":0,"usage_today_bytes":0,"usage_month_bytes":0,
 "suspended_on_router":false,"in_sync":true,"router_user":""}
Overview {"greeting_period":"morning|afternoon|evening|late","setup":{"done":0,"total":0,"pct":0,"steps":[{"key","title","done":true,"href"}]},
 "revenue":{"today_kes":0,"yesterday_kes":0,"month_kes":0,"spark_7d":[]},"active":0,"online_now":0,
 "routers":{"total":0,"online":0},"today":[{"time","kind","text","account_id","router_id"}],
 "next":[{"kind","text","count":0,"href"}],"renewals_48h":[{"account_id","flat","tenant","router_name","expires_at","package"}],
 "busiest_routers":[{"router_id","name","down_bps","util_pct","online":0}],
 "health":{"at_risk":0,"churned":0,"expiring_7d":0},"expiries_per_day":[{"date","count":0}]}
```
W9 notes (`internal/sites/live.go`): fixed read paths `/system/resource`, `/system/health`, `/system/routerboard`, `/interface`, `/ip/address`, `/ip/dhcp-server/lease`, `/ip/arp`, `/interface/bridge/host`, `/queue/simple` via `Inspector.One` and `Client.Rows`; cache 5 s per router+view, single-flight, 3 s timeout, stale-on-error ≤60 s with `reachable:false`; interface rates from byte-counter deltas between samples (null on first call); WAN/flat rates from `metrics.Collector.Snapshot()`, throughput series from the T5 port series; `clients` merges accounts, `AccountRouterState`, live flats, `billing.Unregistered`, leases, `billing/oui.go`; `q` matches name, flat, code, IP, MAC, vendor, host; `at_risk` = active not seen 7 d; `churned` = expired >30 d.

## 4. Web IA (`/admin`)
Sidebar (navy both themes; active = orange pill; collapsible groups in `localStorage`):
Overview · **Customers**: Subscribers, Contacts, New devices (badge) · **Network**: Live now, Routers, Plans, Network map, Equipment, Usage and quality, Uptime · **Finance**: Payments, Unmatched (badge), Orders, Vouchers · **Communications**: Messages (badge), Alerts (badge) · **System**: Settings (owner), Security, Tools, Audit.
Topbar: menu (phone), breadcrumb, Cmd-K search, router switcher (`ftn_site` cookie, labelled Router), theme toggle, avatar menu.

| Page | Hero | Stat cards | Tabs | Columns | Actions / empty |
|---|---|---|---|---|---|
| Overview (`adminDashboard`, drop fleet redirect) | "Good evening, *Brian.*" (EAT) | Revenue today (7-d spark) · Active · Online now · Routers online · Unmatched | – | Panels: setup checklist %, Today feed, What's next, Renewals 48 h, Busiest routers, Subscriber health, Expiries/day 14-d bars | checklist: M-Pesa, first plan, router joined, router live, first customer, WhatsApp |
| Subscribers | "Your *subscribers.*" | All · Online · Expiring 7 d · New this week · At risk | All/Active/Expired/Suspended/New/MAC conflict | Customer (flat, tenant, code) · Contact · Plan · Expiry · Last seen · Router · On router (sync pill) | **Add customer** modal (one form), CSV; empty CTA |
| Subscriber detail | flat + tenant | Plan · Expires · Usage month · Router state | Overview/Payments/Usage/Events | | Extend, change plan, change MAC (device picker), suspend, move, delete |
| Contacts | "Your *contacts.*" | People · With active plan · Moved out | All/Active/Past | Name · Phone · Flats · Since | existing |
| New devices | "New *devices.*" | Online now · Seen today · Suggested | Online/All | MAC · Vendor · Host · IP · Router · Source · Logged in as · Last seen | Create customer, Attach, Dismiss; empty "Nothing new on the network." |
| Live now (new) | "Online *now.*" | Online · Download · Upload · Busiest flat | All/Flats/Unregistered | Flat · Tenant · IP · MAC · Down · Up · Cap · Router | 10 s poll of `/admin/live.json` |
| Routers (absorbs fleet) | "Your *routers.*" | Routers · Online · Offline · Flats online · Needs attention | All/Online/Offline/Setting up | Router · Status · Mode · Flats · WAN now · Tunnel · Setup · Last seen | **Add router**; empty "No routers linked yet." |
| Router detail | name | CPU · Memory · Uptime · WAN · Tunnel latency · Flats online | Overview/Setup/Clients/Interfaces/Plans/Profile/Danger | | Setup tab: job stepper, checklist + Fix, join line (copy), Start migration, Roll back now; Recheck, Resync, Reboot, Pause, Remove; Advanced: frozen paste scripts |
| Plans | "Your *plans.*" | Plans · Active · Disabled · Subscribers | All/Quick passes/Monthly/Disabled | Name · Speed · Duration · Price · Subscribers · Status | New plan |
| Network map (new) | "Network *map.*" | Routers · With location · Offline | | server SVG topology server→tunnel→router→OLT→flats; lat/lng grid + "open in maps" | no tiles (CSP) |
| Equipment | "Hardware *inventory.*" | In stock · Assigned · Returned · Retired | by status | Item · Type · Serial/MAC · Assigned to · Router · Status | Add, CSV |
| Payments | "Payments *ledger.*" | Today · Week · Month · M-Pesa share · Unmatched | All/M-Pesa/Cash/Voucher/Unmatched/Refunded | Subscriber · Receipt · Method · Status · Amount · Paid at · Router | Record payment, Export |
| Unmatched, Orders, Vouchers, Messages, Alerts, Uptime, Usage, Settings, Security, Tools | two-word hero | 3–4 cards from existing data | existing filters as tabs | existing | existing, restyled |

New session JSON: `/admin/search.json?q=`, `/admin/live.json`, `/admin/routers/setup.json`, `/admin/routers/live.json`, plus W5 `checks.json`, `fix`, `cutover*` under `/admin/routers`.

## 5. Design system
Theme: no attribute = system; `/static/theme.js` (blocking) applies `localStorage["ftn-theme"]` (light/dark/absent); toggle cycles System → Light → Dark. Light tokens on `:root`; dark under `@media (prefers-color-scheme: dark){:root:not([data-theme=light])}` and `:root[data-theme=dark]`.

| Token | Light | Dark |
|---|---|---|
| `--bg` | `#FFF8F2` | `#000C20` |
| `--surface` / `-2` / `-3` | `#FFFFFF` / `#FFF1E6` / `#FBE6D4` | `#071B3B` / `#0E2548` / `#173158` |
| `--line` / `--line-strong` | `#EAD9CA` / `#CDB8A6` | `#223A60` / `#3A5280` |
| `--ink` / `--ink-2` / `--muted` | `#001432` / `#3B4A63` / `#5C6678` | `#EAF0FF` / `#C3CBDD` / `#9AA6BE` |
| `--brand` / `--on-brand` | `#FF8E1C` / `#001432` | `#FF8E1C` / `#001432` |
| `--brand-ink` / `--brand-soft` | `#8F4E00` / `#FFE3C7` | `#FFB77B` / `#4C2700` |
| `--side` / `--on-side` | `#001432` / `#FFFFFF` | `#000A1D` / `#EAF0FF` |
| `--ok` / soft | `#1B6D2F` / `#DDF5DE` | `#8BD88F` / `#0F3A1E` |
| `--warn` / soft | `#7A5900` / `#FFEBC2` | `#F6BE36` / `#4A3500` |
| `--bad` / soft | `#BA1A1A` / `#FFE1DD` | `#FFB4AB` / `#5C1014` |
| `--info` / soft | `#2A4A7F` / `#DDE7FF` | `#AEC6FF` / `#17335F` |
| `--teal` / soft | `#006A62` / `#CFF4EE` | `#82D5CA` / `#00403A` |
| `--plum` / soft | `#7A3E8E` / `#F1DDF7` | `#E2B6F0` / `#48215A` |

Orange is never text on light backgrounds (fill with navy text). A Go test parses tokens and asserts WCAG ratios (4.5 text, 3 UI).
Type: display Bricolage Grotesque 800 (hero `clamp(30px,4.2vw,46px)`, tracking −0.02em), 700 for card numbers (34px) and h2 (22px); body Inter 15/1.5, 600 labels, table headers 12px uppercase 700; mono JetBrains Mono 13px (MAC, IP, codes, receipts); `tabular-nums`.
Radius 10/14/20/28; 4-px grid; focus ring 3 px `--brand` 2 px offset.

| Component | Spec |
|---|---|
| Sidebar | 264 px navy; group headers 11px uppercase; items 44 px; active orange pill navy text 700; count badges; off-canvas < 900 px with scrim; `<details>` groups |
| Topbar | 60 px sticky; breadcrumb; search button ⌘K/Ctrl K; router select; theme button; avatar initials on orange |
| Hero | second word on an orange highlighter block (navy text, 10 px radius); lead in `--ink-2`; primary right, ghost secondaries; stacks on phone |
| Stat card | 12px upper label, big number, sub-line, optional 120×32 server sparkline; tones brand/info/teal/ok/warn/bad/plum (soft fill + solid icon chip); first card "feature" (solid navy light / solid orange dark); `repeat(auto-fit,minmax(200px,1fr))`, 2 per row on phone |
| Filter tabs | pill links + count chip; active navy (light) / orange (dark); horizontal scroll on phone; `?tab=` |
| Table | card container, sticky header, 48 px rows, hover tint, right-aligned numerics; < 640 px rows become cards via `data-label` |
| Pills | soft fill, strong text, 6 px dot: active, expired, suspended, new, online, offline, in sync, drift, unmatched, pending |
| Empty state | inline brand SVG, bold title, one line, primary + optional secondary |
| Forms | 44 px inputs, label above, hint below, inline error `--bad`; segmented control; MAC field with "pick from router" list |
| Modal | native `<dialog>` 560 px, bottom sheet on phone (Add customer, Add router, Record payment, typed-name confirms) |
| Toast | bottom-right, 5 s, `role=status`; `?msg=` flashes render as toasts with inline fallback |
| Cmd-K | `<dialog>`; nav entries + debounced `/admin/search.json`; groups Go to/Subscribers/Routers/Payments; arrows/Enter/Esc; also `/` |
| Charts | server SVG; new `metrics.Spark`, `metrics.BarsV` |

CSP: static from `GET /static/{path...}` (embed, `public, max-age=31536000, immutable`, `?v=<build hash>`). `cspStrict` gains `style-src 'self'` and `font-src 'self'` for sign-in pages. Stretch: move inline handlers to `app.js` and drop `'unsafe-inline'` from `script-src`.

## 6. Android (branch from `android-setup`)
Shell: `NavigationSuiteScaffold` (bar on phone, rail ≥600 dp). Top bar on every tab: **router switcher** (All routers + each router with status dot; saved as `selectedRouterId`), alerts bell with badge.

| Tab | Content |
|---|---|
| Home | Per-router cards, 2-column grid: Online flats · Throughput (now + sparkline) · CPU · Memory · Uptime · RouterOS + board · WAN status/util · Tunnel latency · OLT/ONT count · Revenue today · Expiring soon · Alerts · Setup state (opens Setup when not live). "All routers" = fleet totals + router list. Pull to refresh; 5 s poll while resumed. |
| Network | Sub-tabs **Clients** (search name/flat/IP/MAC/vendor; chips All/Flats/Unregistered/Online; expandable rows: lease status, queue speed, usage today/month, port, last seen; actions Open customer, Suspend/Unsuspend, Resync, Change MAC; unregistered: Create customer/Attach/Dismiss) and **Interfaces** (expandable: running dot, role chip, rx/tx rates, totals, addresses, MAC, MTU). |
| Customers | List + detail restyled; extended FAB **Add customer**: one scrolling screen — name, phone, flat, package cards, router (preselected), MAC (live device list with search, or paste; `MacAddress` validation), "Activate now"; result card with live provisioning status polled from `GET /accounts/{id}`. |
| Payments | Restyled; cards Today/Month/Unmatched. |
| More | Router actions: Setup and checks (W6 screens, without Confirm per A1), Recheck, Resync, Join command, Reboot (owner, typed name). Routers list + Add router. Alerts. App settings (theme System/Light/Dark, dynamic colour, lock, devices, sign out). |

Rules: load skill `anthropic-skills:android-m3-expressive` first; shared theme (`AppTheme`/`MaterialExpressiveTheme`); no code from LuCI Mobile (GPL-3.0), ideas only; rename `ui/buildings` → `ui/routers` and every string to "Router"; type-safe `@Serializable` routes. New files: `data/routers/RouterLiveRepository.kt`, `RouterModels.kt`; `ui/home/*`, `ui/network/{ClientsScreen,InterfacesScreen,NetworkViewModel}.kt`, `ui/customers/add/*`, `ui/more/*`, `ui/common/{RouterSwitcher,StatCard,Sparkline}.kt`.

## 7. Workstreams
Order: merge W5a + W5b into `setup-auto` → **P0** (serial, small) → W5c, W7, W9 in parallel → W8 starts now against section 3 with MockWebServer → integrate.

| Stream | Owns (exclusive) | Tests |
|---|---|---|
| W5b delta | add `reboot` to the group policy, `need=` in the one-liner | goldens; one-liner for both intents |
| P0 | `web/api.go` (alias registration: each pattern served under `/sites` and `/routers`; one call line per new route file), empty `web/api_setup.go`, `api_live.go`, `api_overview.go`, `web/static.go` + `static/` embed, string rename in `alerts/alerts.go`, `sites/manager.go`, `web/*.go` messages, `README.md`, `deploy/*`, `docs/*`, `docs/api-v1.md` v4 header | alias test; string-guard test |
| W5c | `sites/provision_job.go`, `prov_writer.go`, `sites/ipoe.go`, `ipoe_script.go` (rules refactor only), `web/api_setup.go`, `web/setup_admin.go` (JSON only), `billing/provision.go` (`HoldSite` + A1 bypass reconciliation), `billing/service.go` + `devices.go` (`CreateCustomer` only), `web/api_accounts.go` (create only), `alerts/alerts.go` (`Announce` only) | mock runs of both kinds; rerun no-op; classification; A1 auto-confirm pass/fail; failure at each migrate step sets `now`; unreachable mid-run leaves marker; writer refuses forbidden paths; reconciler blocked while held; auto-start window; roles; hotspot bypass rows for entitled accounts |
| W9 | `sites/live.go`, `web/api_live.go`, `web/api_overview.go`, `billing/overview.go`, `routeros/mock.go` seeds | fixed path list; no secret-looking field; cache/stale; rate delta; search; reboot guards/rate limit |
| W7 | all `web/templates/*.html`, `web/static/*`, `web/security_headers.go`, `web/admin.go`, `sites.go`, `devices.go`, `fleet.go`, `metrics.go`, `crm.go` (handler data only), new `web/overview.go`, `live.go`, `map.go`, `search_json.go`, `metrics/spark.go`; load skills `anthropic-skills:frontend-design` and `dataviz` | every page renders; contrast test; "uilding" guard (3 portal exceptions); CSP test; old URLs redirect; no external URL |
| W8 | `android/` | MockWebServer VM tests: live, clients search, interfaces, add-customer (ok, `mac_in_use`, queued), reboot guard; existing tests green; lint |
| Opus review | | `provWriter` allowlist + DNS/WAN rule; kit; migrate failure paths; A1 evidence rule; reboot grant; live endpoints for leaks; contrast + keyboard |

Conflict rule: only W7 edits templates; only P0 edits the `apiRoutes` table; each stream writes API notes to `docs/api/v4-<stream>.md`, the integrator folds them into `docs/api-v1.md`.

## 8. Acceptance
1. Bench RB5009 with no customer network: Add router (name only) → paste once → page reaches **Live**, no further paste/button (fetch on).
2. Router with hotspot + test tenant, "already serving customers": paste (button press if scheduler off, paste again) → server stages, switches, **auto-confirms** when the test flat is bound; a failing evidence window rolls back and notifies; WAN pulled after arming rolls back.
3. Add customer (web and phone) with MAC picked from the router's devices: lease, queue, walled state on the router within 10 s, form shows "Provisioned on router"; router offline → "Queued", applies on return.
4. M-Pesa payment un-walls; expiry walls; no operator action.
5. No user-facing "Building" except the 3 portal phrases.
6. Web: light/dark/system; usable at 360 px; no request leaves the origin; Cmd-K finds a subscriber, a router, a payment.
7. App: router switcher, Home cards live, client search, interfaces, reboot with typed confirm.
8. `ftn-api` has no `policy`; a test proves the server cannot write `/user`, `/ip/service`, `/system/*` (except reboot), routes, WAN.

## 9. Risks
Auto-confirm evidence rule (A1) is the key safety call — Opus reviews it. Fresh-path bridge/OLT port handling with factory `defconf` bridge may fall to `needs_action`. Server adding DNS-from-WAN drop rules is new firewall authority (`ftn:` rows only). Unverified on hardware: marker kit, `place-before` over REST, `reboot` alone, hotspot disable/ARP mode via `ftn-api`; paste cutover stays as frozen fallback. W7 starts only after W5a/W5b land. Fonts add ~150–250 KB. Interface rates null on first poll. Job state in settings; restart mid-migration lets the router roll back.

## Addendum B (live bench, 2026-11-18 23:00)
- After the auto-rollback the bench laptop on the hotspot (bypassed) could ping 1.1.1.1 but not resolve names: the defconf input chain drops traffic from interfaces outside the LAN list, so DNS to the router from the customer interface was dropped once the cutover's `ftn: ipoe dns` accepts were removed. Required:
  - W5a/W5c checks: new item `customer.dns_input` (blocks) — the input chain must accept udp/tcp 53 from the customer interface before any drop; auto fix adds `chain=input in-interface=<customer> protocol=udp|tcp dst-port=53 action=accept place-before=<first non-dynamic>` with comment `ftn: customer dns` (allowed in `fixWriter` / `provWriter` as `ftn:` rows).
  - These `ftn: customer dns` rules are permanent (not `ftn: ipoe`), so neither the kit rollback nor the paste rollback removes them.
- The legacy paste cutover worked end to end on RouterOS 7.25.1 once `scheduler` was enabled in device-mode; the 15-min rollback restored hotspot, dhcp-hotspot and `arp=enabled` correctly.
