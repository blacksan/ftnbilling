# v5: one bootstrap script, no Hotspot, server issues everything

Planned by Fable 2026-11-19 after the user's rethink ("router is brand new with no config; one script that covers the deployment including WireGuard; why is hotspot needed; clean up all previous attempts") and the standing rule **"all commands are issued by the server downstream"** (the operator types nothing on the router except the single bootstrap paste, and presses the physical button only if RouterOS device-mode requires it). Base: `main` (PR #7 merged). No paying tenants exist on any router.

## Amendments by the main session (override the body)
- **M1. No separate bench spike.** Build Line A and Line B; the bench run is the spike. Line B stays as the documented fallback.
- **M2. Avoid the interactive reset prompt.** A pasted `/system/reset-configuration` may ask "Dangerous! Reset anyway? [y/N]". So the one-liner does NOT call reset itself: it fetches TWO files with the same token flow — `ftn-setup.rsc` (the full script) and `ftn-reset.rsc` (a tiny fixed script) — then `/import ftn-reset.rsc`. `ftn-reset.rsc` (non-interactive because it runs via /import) checks that `ftn-setup.rsc` exists and is non-empty, then runs `/system/reset-configuration no-defaults=yes skip-backup=yes keep-users=yes run-after-reset=ftn-setup.rsc`. Server serves both from one token (e.g. `/j/<token>` = setup, `/j/<token>/reset` = reset stub; token consumed when setup is fetched, reset stub accepted within 5 min after). BENCH-CHECK items remain as risks.
- **M3. Keep the REST read-budget work** already done on branch `fix-walled-before-switch` commit 4a835c5 (`readSnapshot` concurrency 4, 10 s timeout, one retry, slow → "checking again automatically", `scheduleRecheck`, `ROUTER_READ_TIMEOUT`, `.proplist` on bridge hosts). Cherry-pick it as the starting point of WS1/WS4, then apply section D. Drop the walled-lists WIP commit a65bbe4 (hotspot-era).
- **M4.** Base branch is `main`.

## Decisions
1. Hotspot is removed from the product; every router is IPoE.
2. One script per router (server-rendered from its record) assumes a blank router and builds everything incl. WireGuard and the REST user.
3. The paste performs the reset itself (M2): fetch while the router still has internet, then reset with `run-after-reset`. This also cleans up previous attempts on the router and works unchanged on an out-of-box defconf router.
4. Fallback (router already blank / no internet): owner downloads the same script, drags it into WinBox Files, runs `/import ftn-setup.rsc`.
5. No provisioning job. After the join the server only runs checks, reconciles flats and serves live data.
6. `access_mode` column stays with the single value `ipoe`.
7. REST budget: 2 reads + 1 write in flight per router, 10 s per call, one read retry, `.proplist` on big tables.

## A. Hotspot
Not needed: flats are identified by ONT MAC via static leases on `ftn-dhcp`; unknown/unpaid are walled by `ftn-unreg`/`ftn-suspended` (`ipoeRuleSet` in `internal/sites/ipoe_script.go`); plain HTTP is dst-natted to `PUBLIC_IP:80` where Caddy's `http://` catch-all redirects to `/pay`; `/pay` finds the router by WireGuard peer endpoint (`siteByAddr` in `internal/web/pay.go`).

### Delete
| Area | What goes |
|---|---|
| `internal/sites` scripts | `join.rsc.tmpl`, `ipoe_join.rsc.tmpl`, `ipoe_cutover.rsc.tmpl`, `ipoe_cleanup.rsc.tmpl`, `kit.go`, `kit_section.rsc.tmpl`; in `ipoe_script.go`: stages, `rollbackBase`, `tickSource`, `LeaseSeed`, `renderStage` |
| `internal/sites` job | `provision_job.go`, `provision_steps.go`, `prov_writer.go`, `provision_lease.go`; job parts of `provision_view.go` (`StartMigration`, `ServingIntent`, `JoinIntentOf`, `resumeProvisioning`, `afterJoin` → replaced) |
| `internal/sites/ipoe.go` | `SetAccessMode*`, `ConfirmCutover`, `CutoverPending`, `UnseenEntitled`, `UnmanagedDevices`, `renderLaterStage` |
| `internal/sites/join.go` | stage prefixes, `ConsumeJoinStage/Force`, cutover preflight; `joinscript.go`: `JoinIntent`, scheduler/hotspot device-mode needs, `PageFiles` |
| `internal/sites/checks.go` | checks `device_mode.scheduler`, `device_mode.hotspot`, `kit.installed`, `hotspot.orphan_users`, `containers`, `unmanaged.devices`, `flats.unseen`; `fixOrphans` in `fixes.go`; hotspot/container/scheduler/file tables from `checkTables` |
| `internal/billing/provision.go` | `SetHotspotBypass`, `HotspotBypassOn`, `bypassWants`, `reconcileBypass`, `dropBypass`, `revertToHotspot`, `HoldSite`/`SiteHeld`/`ErrSiteHeld`, rollback detection and `MarkCutoverSeen` in `reconcileSite`, observe mode (`enforce`, `WouldSuspend`) in `planOps` |
| `internal/billing` other | `MarkCutover*`, `ClearCutoverMarks` (`ips.go`); `reconcileHotspotTask` and non-IPoE branches in `tasks.go` |
| `internal/web` | `adminSiteMode`, `adminSiteScript`, `adminSiteAdopt`, `adminSiteConfirm`, `adminSetupRun/Migrate/Rollback`, `apiSiteMode`, `apiCutover*`, `apiSetupRun/Migrate/Rollback`; `modeLabel`; "Advanced: access mode and paste scripts" panel in `admin_site_view.html`; hotspot/provision/legacy fields in `admin_sites.html` |
| Second pass (WS6) | `routerfiles.go` + `routerfiles/`, `routeros/hotspot`, `routeros/setup.rsc`, hotspot login in `portal.go` (`pctx` MAC path, `portalConnect`, `AutoLogin`), `Service.AutoLogin/Kick/HotspotHosts/SyncProfiles`, `HostGuard`, hotspot methods on `routeros.Client` (`EnsureProfile/User`, `RemoveUser/Profile`, `KickUser`, `Login`, `Hosts`, `ActiveSessions`), sessions branch in `internal/metrics/collector.go`, container env-mode (`ROUTER_URL`) |

### Keep
Walled garden (`ipoeRuleSet`), `/pay`, M-Pesa callbacks, `/portal/wait` and `/portal/success` (used by `/pay/start`); reconciler (`ReconcileSite`, `planOps`, `collectDevices`, `CreateCustomer`), `EnsureIPs`, live data (`live.go`), join token + report (`join.go`, `joinreport.go`, `secwrap.go`). Checks: `router.version` (7.13+), `api_user.policy`, `device_mode.fetch`, `dns.remote_requests`, `customer.dns_input`, `customer.interface`, `olt.bridge_conflict`, `bench.port`, `traffic_flow`, `service.www`, `ipv6`, `server.public_ip`, `router.script_hooks`. Fixes: `fixDNS`, `fixCustDNS`, `fixOLT`, `fixBench`, `fixTrafficFlow`; salvage `ensureRules`/`rewriteRules` from `provision_steps.go` into `fixes.go` as a walled-garden repair. `ROUTER_MOCK` dev mode as an IPoE site against `routeros.Mock`.

### Data, API, docs
- Migration `0006_bootstrap.sql`: `UPDATE sites SET access_mode='ipoe'`, default `'ipoe'`, plus the new columns (section B). JSON store `FormatVersion` 7 likewise.
- Settings purge at migration: delete `site.<id>.prov_job`, `.prov_lease`, `.serving_customers`, `.cutover_seen_at`, `.cutover_confirmed_at`, `.hotspot_bypass_at`.
- Unused columns stay this release: `hotspot_server`, `provision`, `legacy_container`, `hs_pass`, `router_active`.
- `billing.IPoE(site)` returns true for every wg site in phase 1; deleted in WS6.
- Removed endpoints answer 410 for one release. `access_mode` stays in site JSON as `"ipoe"`.
- Docs: new `docs/api/v5-bootstrap.md`; mark `v4-w5c.md` and the W5c parts of `release-plan-routers.md` and `setup-automation-plan.md` superseded.

## B. The bootstrap script
### New router record fields (`store.Site`, `SiteInput`, `Validate`, `ApplyDefaults` in `internal/sites/layout.go`, `manager.go`)
| Field | Default | Notes |
|---|---|---|
| `WANMode` | `dhcp` | `dhcp`, `static`, `pppoe` |
| `WANAddress`, `WANGateway`, `WANDNS` | empty | static only |
| `PPPoEUser`, `PPPoEPass` | empty | password sealed with `DATA_KEY` via `internal/secretbox` |
| `LANPorts` | `ether2` | comma list, each `ValidIface` |
| `LANSubnet` | `192.168.88.0/24` | must not overlap customer subnet or `WG_SUBNET` |
Still used: `WANInterface`, `OLTInterface`, `VLANID`, `CustomerSubnet`, `BenchPort`, `SupportPhone`, `RESTUser`, `RESTPort`. Rename const `HotspotBridge` (`br-hotspot`) → `CustomerBridge = "br-customers"`.

### Delivery
Line A (default), new `BootstrapCommand(base, token, checkCert)` replacing `JoinCommandIntent`, per M2: check `device-mode fetch` (if off: print button instruction and `/system/device-mode/update fetch=yes`, nothing else); trust anchors on; remove stale `ftn-setup.rsc`/`ftn-reset.rsc`; fetch both; `/import ftn-reset.rsc`. Second paste is safe (single-use token → fetch fails → nothing resets). `keep-users=yes` keeps the operator's admin login; the script removes old `ispbill`/`ftn-api` users and groups before re-adding. `command_nocheck` variant stays.
Line B (Advanced): owner-only "Download script" (mints + consumes a token server-side, `no-store`), then `/import ftn-setup.rsc`.
Non-reset routers (Line B on a configured router): section 0 aborts if any static address, bridge, DHCP server or firewall rule exists without an `ftn:` comment. No auto-clean of foreign config.
Device-mode: only `fetch` is needed. The script never touches device-mode.
BENCH-CHECK (risks, verified on the first real run): reset non-interactive via /import; files survive reset and `run-after-reset` runs; time limit on the run-after-reset script (keep total < 90 s); `:onerror` and self-deletion in that context; `builtin-trust-anchors` on 7.25.1.

### Sections (`internal/sites/bootstrap.rsc.tmpl`, `RenderBootstrap` in `bootstrap_script.go`; each wrapped with `secOpen`/`secClose`; every object commented `ftn:`; each section removes its own `ftn:` objects before adding)
| # | Section | Fatal | Content |
|---|---|---|---|
| 0 | Wait and guard | yes | wait ≤60 s for WAN and OLT interfaces; RouterOS 7.13+; foreign-config guard |
| 1 | System | no | identity `FTN-<name>`, time zone `Africa/Nairobi`, NTP client on, trust anchors |
| 2 | Lists and WAN | yes | lists `WAN`, `LAN`; DHCP client (`use-peer-dns=no`) / static address + route / `pppoe-wan`; WAN members |
| 3 | Admin LAN | yes | `br-lan` with `LANPorts`, gateway `.1`, pool, `ftn-lan-dhcp`, network; `LAN` member |
| 4 | Customer network | yes | optional `vlan<N>-customers` on the OLT port; `br-customers` `arp=reply-only`; ports OLT/VLAN and bench with `horizon=1`; gateway from `PlanSubnet` |
| 5 | DHCP | yes | pool `ftn-unreg` (.10–.59); `ftn-dhcp` enabled, `authoritative=yes add-arp=yes lease-time=1m`; network; `ftn-unreg` address-list range |
| 6 | Firewall | yes | below |
| 7 | DNS and IPv6 | no | `/ip/dns` servers `1.1.1.1,8.8.8.8`, `allow-remote-requests=yes` (after WAN DNS drops exist); `disable-ipv6=yes` + IPv6 forward drop |
| 8 | Services | no | telnet, ftp, api, api-ssl, www-ssl off; `www` on `RESTPort` restricted to the server tunnel IP; ssh + winbox restricted to `LANSubnet`; MAC server, MAC WinBox, neighbour discovery on `LAN` only (only if `LAN` has a member); bandwidth-server off |
| 9 | WireGuard | yes | `wg-ftn`, tunnel address `network=<server>`, peer `persistent-keepalive=25s` |
| 10 | REST user | yes | group `ftn-api` = `read,write,test,sensitive,api,rest-api,reboot`; user restricted to the server tunnel IP |
| 11 | Traffic-flow | no | on `br-customers`, target from `flowWant` (server tunnel IP, 2055, v5) |
| 12 | Finish | – | wait ≤30 s for a default route, ping server tunnel IP, `:log`, POST the report (`reportPostLine`), remove the files (also in the outer error handler) |

Firewall (script owns the whole table, top to bottom, no FastTrack so queues apply):
- Input: accept est/rel; drop invalid; drop DNS from `WAN` (`ftn: no WAN DNS`); `wg-ftn`: accept REST port + ICMP from the server IP, drop rest; accept `LAN`; customer bridge: accept DNS (`ftn: customer dns`), DHCP, ICMP, then the walled input rules from `ipoeRuleSet`; drop everything else.
- Forward: walled rules from `ipoeRuleSet`; accept est/rel; drop invalid; drop `wg-ftn` in/out; isolate flats; accept `LAN`→`WAN`; accept `LAN`→customers (OLT mgmt); accept customers→`WAN`; drop rest.
- NAT: `ftn: customer masquerade` and `ftn: lan masquerade` out `WAN`; walled DNS redirect and HTTP→payment-page dst-nat from `ipoeRuleSet`.
- Nothing opened on WAN (router is the WireGuard initiator).
- Never unfiltered on a re-paste: section 6 first adds `ftn: hold` at the top of the filter table (drop new and invalid input from `WAN`), then removes the old `ftn:` rules except the hold, adds the new rule set, and removes the hold last. If a rule fails, the section stops with the hold still in place, so the router stays closed.
No secrets printed or reported; `sanitizeReport` stays. Golden test per WAN mode.

### After the join
`watchJoin` (`runtime.go`) records `JoinedAt`, then new `afterJoin` runs `Checks`, `EnsureIPs`, enqueues every flat and calls `ReconcileSite`. `reconcileSite` keeps one gate: no `ftn-dhcp` → "router not set up", nothing written. Static leases get `lease-time=1h`; unregistered renew every 30 s on the 1-min default, so a newly registered flat moves to its fixed IP in ~35 s.
`Setup` JSON keeps `state`, `join`, `next_action`, `steps`, `checks`, `blockers`, `last_error`. States: `not_started`, `waiting_router`, `online`, `ready`. Steps derived: script fetched, tunnel up, server login works, checks green.
New checks: `walled.rules` (auto-fix via `ipoeRuleSet`), `dhcp.ftn` (auto-fix enable), `wan.internet`, `bootstrap.file` (leftover script file; fix removes exactly that file).

## C. Clean-up
1. Router: Line A resets it.
2. Server: `Manager.ResetRouter(siteID, confirmName, actor)` in `internal/sites/reset.go`, owner + typed name: clears `WGPublicKey`, `RESTPass`, `JoinedAt`, `Join`, join report, all obsolete `site.<id>.*` settings, the router's tasks/devices, checks and live caches, in-memory reconcile state (`billing.Service.ForgetSite`); rewrites peers (`ApplyPeers`), stops the runtime, re-enqueues flats, mints a new line. Accounts and tunnel IP kept. `POST /api/v1/routers/{id}/actions/reset` + Danger-tab button; optional CLI `ispbill router-reset`.
3. Test flat HN29BY kept (MAC `84:21:41:F1:70:94`, static-range IP, package).

## D. REST budget (260 ms tunnel)
| Setting | Now | New |
|---|---|---|
| Transport | shared default | per router: `MaxConnsPerHost` 3, keep-alive, 5 s dial |
| In flight per router | 6 in checks, unbounded elsewhere | 2 reads + 1 write, enforced in `routeros.REST`; deadline starts after the slot is taken |
| Per call | 8 s / 3 s / 3 s | 10 s |
| Retries | none | reads: one retry on timeout/reset after 300 ms; writes: only when never sent (`notSent` from `api_live.go`) |
| `.proplist` | none | `RowsProps` for leases, ARP, bridge hosts, queues, interfaces |
| Checks | 30 tables, 25 s | ~18 tables, 45 s overall, 60 s cache, page shows cache and refreshes by JSON |
| Live | TTL 5 s, stale 60 s | TTL 10 s, stale 120 s; clients poll 15 s |
| Reconciler | 7 reads, 8 s budget | 5 reads, 30 s and 20 s |
| "Online" | REST ping 2 s | WireGuard handshake age < 3 min; REST ping only on the router page (6 s, 30 s cache) |
Also: the 3 s timeouts in `internal/web` (`sites_view.go`, `devices.go`, `map.go`, `admin.go`) and `manager.go` → use the shared budget.

## E. Web and app
- Add router form: name, WAN port, WAN type (+ static/PPPoE fields), OLT port, VLAN, customer subnet, admin LAN ports, LAN subnet, bench port, WAN capacity, support phone. Removed: hotspot server, WAN list, "script builds…", legacy container, "already serving customers".
- Setup tab: 1) connect a laptop to the LAN port, open WinBox (by MAC); 2) paste this line (copy; the router erases itself and restarts); 3) watch the four steps go green (5 s poll). Below: checklist with Fix buttons, Recheck, New line, Advanced "Download script". Reset router + Reboot on the Danger tab.
- Removed UI: mode selector, paste scripts, Start migration, Roll back now, Confirm cutover, Adopt leases, "Push speed profiles".
- API: add `actions/reset`; extend `POST/PATCH /routers`; 410 for `mode`, `cutover*`, `setup/run|migrate|rollback`. Keep `bench/test-flat` and `bench/suspend-test` without the mode guard.
- Android (`android-routers`): delete `SetupJobScreen.kt`, `SetupJobViewModel.kt`, migrate/rollback/confirm parts of `SetupScreen.kt`; `JoinCommandScreen.kt` → three-step Setup with polling; `AddRouterScreen.kt` + VM new fields; trim `SiteSetupModels.kt`/`SiteSetupRepository.kt`; reset in `RouterActionsViewModel.kt`; update tests. Load `anthropic-skills:android-m3-expressive` first.

## F. Acceptance (bench)
1. Deploy; Reset router on "MG One Central"; paste Line A once.
2. Router online in admin within 2 min, checks green.
3. HN29BY on ether5 gets its fixed IP and internet within 1 min.
4. End the plan: any HTTP page lands on `/pay`.
5. Pay (test or M-Pesa): online within 1 min.
6. Laptop on ether2 keeps WinBox and internet throughout (after the reset, it gets an address from `br-lan`).
Opus review focus: firewall order and default drops (nothing from WAN); no lockout path; secrets (file removal, report sanitising, PPPoE password at rest); section idempotency; `ResetRouter` races; write-retry safety; nothing left that can disable `ftn-dhcp` or revert a mode.
Risks: run-after-reset behaviour (Line B fallback); wrong WAN settings leave the router offline (operator reconnects via WinBox MAC on the LAN port, fixes the record, re-runs via Line B); upstream subnet overlap with LAN on DHCP WAN (reported, not fatal); WS6 is a large deletion (isolated, after phase 1 ships).
