# Router onboarding automation (W5 server, W6 Android)

Planned by Fable, 2026-11-18, after the first live bench join (RB5009, RouterOS 7.25.1, device-mode `home`, fetch/hotspot on, scheduler off). Goal: the operator pastes **one** join line, presses the router button **once** if device-mode must change, and does everything else from the web admin or the app.

## 0. Findings
- Home mode blocks fetch, hotspot and scheduler. A device-mode change needs a button press or power cut, reboots the router, and allows only 3 unconfirmed attempts before a power-cycle resets the counter. Never request it silently or repeatedly, and never from inside an imported script.
- Legacy join hard-codes traffic-flow `interfaces={{.Bridge}}` (`join.rsc.tmpl:125`, `Bridge: HotspotBridge` even when Provision is off); likely cause of tonight's section 7 failure. Syntax is valid v7.
- Every section swallows its error (`:do {...} on-error={ "section N failed" }`). RouterOS 7.13+ has `:onerror e in={...} do={...}` (bench-check exact syntax).
- Discovery: `billing/devices.go` skips `h.Authorized || h.Bypassed`; hotspot mode never reads leases/ARP. `NoteDevice` already skips MACs with an open account, so the skip is unnecessary.
- `routeros.REST.Rows` decodes arrays only; `/system/device-mode`, `/ip/dns`, `/ip/traffic-flow`, `/ipv6/settings` are single objects. No generic POST-command call.
- Mode switch and confirm are web-only.
- Unverified: whether `ftn-api` (no `policy`) can remove a scheduler row (today's `ConfirmCutover` relies on it). The new design stops depending on it.

## 1. Decisions
| Topic | Decision |
|---|---|
| Device-mode | Checked in the pasted **one-liner**, not the script. If `fetch` or `scheduler` (plus `hotspot` for hotspot-mode buildings) is off, it runs one `/system/device-mode/update ...` and prints "press the button within 5 minutes; the router reboots; then paste this same line again". Otherwise it fetches and imports. Server never calls device-mode update; checklist shows the command. No retries. |
| Error text | Section wrappers rendered by a template helper: `:onerror` form when the fetch UA version ≥ 7.13, old form otherwise (UA already reaches `ConsumeJoinForce`). Each failure appends `"N name: <error>"` to `$report`. |
| Traffic-flow | Removed from both join scripts; server sets it over REST after the first successful ping, on the detected customer interface. |
| Join result | Script POSTs a short report to `POST /j/{token}/report` (same token, accepted for 30 min after `UsedAt`; ≤2 KB, printable ASCII, `routeros.RedactOutput`, stored as setting `site.<id>.join_report`). Refused-UA fetches with a valid token are recorded in `SiteStatus`. |
| Cutover | **Server-run over REST row operations. No `policy` granted to `ftn-api`.** The join script (run by the router admin) pre-installs a fixed **kit**: always-on scheduler + scripts. The server arms/confirms/rolls back by adding/removing address-list marker rows (needs only `write`). |
| Why not policy | `policy` lets the API user create arbitrary scripts/schedulers and manage users → server compromise = router takeover. The kit limits the server to two fixed behaviours (backup, rollback countdown). Cost: already-joined routers paste the join line once more. |
| Confirm | Human Confirm stays (web + app), gated on evidence. No auto-confirm. |
| Paste cutover | Kept as frozen "Advanced" fallback, with only the error-text and device-mode precheck fixes. |
| Bench | No admin from the bench port. Laptop on ether2 for the paste only, then on ether5 as test tenant; everything else from the phone. |
| Storage | No schema change. Join report + cutover job state in settings (`site.<id>.<name>` pattern, `billing/ips.go`). Hotspot-user names for discovered devices in memory in `provState`. |

## 2. Checks: `GET /api/v1/sites/{id}/checks`
New `internal/sites/checks.go` (read) and `fixes.go` (write). One pass reads in parallel (3 s timeout each): `/system/resource`, `/system/device-mode`, `/ip/dns`, `/ip/address`, `/interface/bridge`, `/interface/bridge/port`, `/interface/bridge/host`, `/interface/vlan`, `/interface/list/member`, `/ip/hotspot`, `/ip/hotspot/user`, `/ip/hotspot/ip-binding`, `/ip/dhcp-server`, `/ip/dhcp-server/lease`, `/ip/arp`, `/ip/pool`, `/ip/traffic-flow`, `/ip/traffic-flow/target`, `/ip/service`, `/container`, `/ipv6/settings`, `/user/group`, `/system/scheduler`, `/system/script`, `/file`. Cached 15 s; `?fresh=1` bypasses. A failed read makes its items `unknown` with the print command to run.

| Check id | Fails/warns when | Fix |
|---|---|---|
| `router.version` | < 7.13 | manual (upgrade) |
| `api_user.policy` | group lacks any of read,write,test,sensitive,api,rest-api | manual: `/user/group set ftn-api policy=...` |
| `device_mode.scheduler` (blocks cutover) | `scheduler=no` or `flagged=yes` | manual, physical |
| `device_mode.fetch`, `device_mode.hotspot` | off (hotspot matters only in hotspot mode) | manual, physical |
| `kit.installed` (blocks) | kit scripts/scheduler missing, disabled, wrong version stamp, or run-count not advancing | manual: paste the join command again |
| `dns.remote_requests` (blocks) | `allow-remote-requests=no` | auto only if the input chain already drops port 53 from the WAN list; else manual |
| `customer.interface` (blocks) | no interface carries `plan.GatewayIP`; no hotspot or DHCP server on it | manual (edit building fields) |
| `olt.bridge_conflict` (blocks) | OLT port is a bridge port while a VLAN interface sits on it | auto if safe, else confirm |
| `bench.port` | bench port set but not on the customer bridge | auto under the same rules |
| `hotspot.orphan_users` | hotspot user/ip-binding holds a MAC with no open account | auto for server-made users (comment `ispbill:` and no open account with that code), confirm for others; never blocks discovery |
| `traffic_flow` | disabled, wrong interface, or no `ftn: netflow` target | auto |
| `service.www` | info: WebFig limited to the billing server; WinBox still works | none (manual command if LAN WebFig wanted) |
| `containers` | lists containers/state; warns only if `LegacyContainer` and an `ispbill` container runs | none |
| `unmanaged.devices` (blocks unless kept) | `Manager.UnmanagedDevices` | "Keep them at cutover" toggle |
| `flats.unseen` (blocks unless forced) | `Manager.UnseenEntitled` | none (fix MAC via Unregistered devices) |
| `ipv6` | info: IPv6 on; flats' IPv6 forwarding dropped at cutover | none |
| `server.public_ip` (blocks) | `PUBLIC_IP` unset | manual (server env) |

Fix registry = allowlist: each fix is a named Go function with fixed REST calls and re-checks its precondition right before writing. Fixes may write only `/ip/dns` (one property), `/ip/traffic-flow` + its target row, `/interface/bridge/port` (OLT and bench rows), `/ip/hotspot/user` and `/ip/hotspot/ip-binding` (orphans). Never `/user`, `/ip/service`, `/system/*`, `/interface/wireguard`, routes, WAN, or firewall rules without an `ftn:` comment.

OLT port removal is automatic only when all hold: a VLAN interface exists on the port (or `VLANID > 0`); not WAN / not in WAN list; no address, DHCP server/client or hotspot directly on it; `/interface/bridge/host` shows no non-local hosts learned on it. Otherwise `confirm` (owner), listing learned MACs.

After a successful join (`Options.OnJoined`, wired in `cmd/ispbill/main.go`) the server runs the non-confirm `auto` fixes once and audits each.

## 3. Join script changes
Files: `internal/sites/join.rsc.tmpl`, `ipoe_join.rsc.tmpl`, `joinscript.go`, `ipoe_script.go`, `join.go`.
1. One-liner (`JoinCommandStage`) with the device-mode precheck (bench-check what `[/system/device-mode/get scheduler]` returns: bool vs "yes").
2. Section helper: `ScriptInput.ROSVersion` parsed from the UA; template funcs `secOpen`/`secClose N "name" fatal`.
3. Delete traffic-flow sections (legacy 7/9, IPoE 4/5), renumber.
4. www service: keep `address={{.ServerIP}}/32`; print that WebFig is limited and WinBox unaffected; 172.17 only if `LegacyContainer`.
5. Kit section (skipped with a clear message if `scheduler=no`): scripts `ftn-kit-tick`, `ftn-kit-rollback`, scheduler `ftn-kit-timer` (30 s), comment `ftn: kit v1`. Names differ from legacy `ftn-rollback*` so the paste cutover and `ipoe_cleanup.rsc.tmpl` keep working.
   - Tick: if address-list row `list=ftn-cmd comment="backup"` exists → save `ftn-pre-ipoe` backup + export (only if absent), remove the row. If a row `list=ftn-armed` exists → increment the counter in its comment; at 30 (15 min) or comment `now` → run `ftn-kit-rollback`.
   - Rollback: `rollbackBase` lines (minus scheduler removal), then for every row in `list=ftn-undo` restore what its comment names (`hotspot|<name>`, `dhcp|<name>`, `arp|<kind>|<iface>|<old mode>`), then remove `ftn-armed` and `ftn-undo` rows.
6. Report POST at the end and in the outer error handler.
7. Refused-fetch visibility in `SiteStatus`.

## 4. Server-run cutover
New `internal/sites/cutover_run.go`. One job per building in a goroutine holding the per-site reconcile lock (new `billing.Service.HoldSite(siteID) (release func())` around `provState.run`). Steps in memory + mirrored to `site.<id>.cutover_job`; each step audited.

| Step | Server does (REST only) |
|---|---|
| 1 preflight | all `blocks_cutover` checks ok; `EnsureIPsLocked`; build `LeaseSeed`s as `renderLaterStage` does |
| 2 backup | add `ftn-cmd` row; wait ≤90 s for `ftn-pre-ipoe.backup` in `/file` |
| 3 arm | write `ftn-undo` rows for enabled hotspot/DHCP servers and interface ARP mode; add `ftn-armed`; wait ≤45 s until counter reaches 1 (timer alive) |
| 4 stage | pool, `ftn-dhcp` (disabled), DHCP network, leases, queues (disabled), list entries (disabled) |
| 5 firewall | `ipoeRules` as REST bodies with `place-before` |
| 6 switch | disable hotspot + old DHCP, enable `ftn-dhcp`, static ARP, kept devices, enable + move queues, enable lists, `arp=reply-only` |
| 7 verify | ping 1.1.1.1 and tunnel ping; state `awaiting_confirm` |

`ipoeRules` refactored to structured rules rendered both to script text (goldens unchanged) and REST bodies.
- Failure before step 6: set the `ftn-armed` comment to `now` → router undoes everything within 30 s.
- Tunnel lost: server stops; router countdown still fires at 15 min.
- Rollback button: same `now` marker.
- Confirm: remove `ftn-armed` + `ftn-undo` rows; `MarkCutoverConfirmed`/`MarkCutoverSeen`. `ConfirmCutover`/`CutoverPending` learn the marker and keep the legacy scheduler path.
- Evidence beside Confirm: tunnel up; ≥1 entitled flat `bound` on `ftn-dhcp` at its fixed IP; router reaches the internet. All true → operators may confirm; else "Confirm anyway" needs owner + typed building name.
- Prompt on `awaiting_confirm` and at 5 min left via the alerts sink (`alerts.Engine` new `Announce(siteID, text, dedupe)`) → WhatsApp/email. App shows a countdown banner. No FCM yet.

## 5. Discovery in every mode
`billing/devices.go`: one `refreshDevices` for all modes reading leases, ARP and hotspot hosts (best-effort each), no `Authorized || Bypassed` skip, feeding `collectDevices`. Hotspot hosts not filtered by `pl.InNetwork`. The hotspot user/binding holding each MAC is kept in `provState` and returned as `router_user` on `GET /unregistered` ("logged in on the router as test1; no server account"). The 60 s loop in `billing/tasks.go` also calls `refreshDevices` for joined hotspot buildings.

## 6. API additions (`/api/v1`; add to `docs/api-v1.md` with a version note)
| Method/path | Role | Body | Response |
|---|---|---|---|
| GET `/sites/{id}/checks?fresh=` | viewer | | `Checks` |
| POST `/sites/{id}/checks/{check_id}/fix` | operator (owner where the item says) | `{confirm?:true}` | `{item:CheckItem,applied:[string]}` |
| PUT `/sites/{id}/mode` | operator | `{mode,override?:false}` | `Site`; 409 `bad_state` with `ErrRouterIsIPoE`/`ErrRouterUnknown` text |
| POST `/sites/{id}/join-command` | owner | | `{command,command_nocheck,expires_at}`; `Cache-Control: no-store`; exempt from Idempotency-Key replay |
| GET `/sites/{id}/cutover` | viewer | | `Cutover` |
| POST `/sites/{id}/cutover/start` | operator | `{confirm_name,keep_unknown?:false}` | 202 `Cutover` |
| POST `/sites/{id}/cutover/confirm` | operator | `{force?:false,confirm_name?}` | `Cutover` |
| POST `/sites/{id}/cutover/rollback` | operator | | 202 `Cutover` |
| POST `/sites/{id}/bench/test-flat` | operator | `{mac,package_id}` | `Account` |
| POST `/sites/{id}/bench/suspend-test` | operator | `{account_id}` | `{steps:[Step]}` |

New errors: 503 `router_unreachable`; 409 `router_refused` with `details.router_error`.
`GET /unregistered` items gain `router_user` (string, may be empty).

```json
Checks {"site_id","generated_at","router":{"reachable":true,"version":"7.25.1","board":""},
 "summary":{"ok":0,"warn":0,"fail":0,"unknown":0,"cutover_ready":false},
 "join_report":{"at","ok":false,"lines":[""]}|null,"items":[CheckItem]}
CheckItem {"id","group":"router|network|devices|services","status":"ok|warn|fail|unknown|info",
 "blocks_cutover":false,"title","detail","evidence":[""],
 "fix":{"kind":"auto|confirm|manual|none","label","confirm_text","command","physical":false,"role":"operator|owner"}}
Cutover {"state":"idle|running|awaiting_confirm|confirmed|rolling_back|rolled_back|failed","method":"server|script",
 "started_at","deadline_at","seconds_left":0,"steps":[Step],
 "evidence":{"tunnel_up":true,"flats_bound":0,"flats_entitled":0,"internet_ok":true},
 "can_confirm":false,"blockers":["check id"],"last_error":""}
Step {"n":1,"key","title","status":"pending|running|ok|failed|skipped","detail","at"}
```
Progress is polled (app every 2 s while `running`/`awaiting_confirm`).
Web admin: same checklist, Fix buttons, cutover panel and countdown on the building page, via session-authenticated `/admin/sites/checks.json`, `/admin/sites/fix`, `/admin/sites/cutover*`.

## 7. Bench helper
- `bench/test-flat`: create or reuse flat "Bench test" with the chosen MAC, activate the package via the existing extend path (note "bench test"). App pre-selects the device learned on `BenchPort` (`/interface/bridge/host`).
- `bench/suspend-test`: suspend → reconcile → read `ftn-suspended` → unsuspend → reconcile → read; returns each step. Refuses in `hotspot`/`ipoe_observe` with a plain explanation.
- Building page guidance: "Paste the join line with the laptop on the admin port, then move it to the bench port and leave it; use the phone for everything else."

## 8. Workstreams
Base branch: `setup-auto` (= main + PR #5 + PR #6).
- **W5a** router access, checks, fixes, discovery — owns `internal/routeros/` (optional `Inspector{ One(ctx,path) (Row,error); Call(ctx,path,body) error }` on REST and Mock, like `Executor` in `exec.go`), `internal/sites/checks.go`, `fixes.go`, `internal/billing/devices.go`, `api_support.go`. Tests: mock tables per check; fixes refuse when the precondition changes; OLT fix refuses with learned hosts; authorized hotspot host with no account appears in Unregistered; one with an account does not.
- **W5b** join scripts, kit, report — owns `internal/sites/*.rsc.tmpl`, `joinscript.go`, `ipoe_script.go`, `join.go`, `testdata/`. Tests: goldens regenerated; both wrapper forms by version; one-liner text; report endpoint (size cap, redaction, token window, wrong token → same 404); kit contains no server-supplied code.
- **W5c** cutover runner + HTTP (after W5a) — owns `internal/sites/cutover_run.go`, `ipoe.go`, `internal/web/api_setup.go` (new), `api.go` route table, `web/sites.go`, `templates/admin_sites.html`, `billing/provision.go` (`HoldSite` only), `alerts/alerts.go` (`Announce` only). Tests: full mock run; failure at each step sets `now` and writes nothing further; mock unreachable mid-run leaves the armed marker; confirm removes markers and sets marks; reconciler doesn't write while the job holds the lock; roles; join-command not replayed.
- **W6** Android (branch from `android-native`) — owns `android/`; apply skill `anthropic-skills:android-m3-expressive`. New `data/sites/SiteSetupRepository.kt`, models, `ui/buildings/setup/SetupScreen.kt` (grouped checklist, Fix buttons, copyable manual commands, mode selector), `CutoverScreen.kt` (steps, countdown, evidence, Confirm/Roll back), ViewModels, routes, entry from `BuildingDetailScreen`, `router_user` line in `NewDevicesScreen`, bench actions. Tests: MockWebServer VM tests for checks, fix (ok / `router_refused`), mode 409, cutover polling.
- **Opus review** focus: fix allowlist, kit scripts (run with admin policy forever), cutover failure paths.

Acceptance (bench RB5009, 7.25.1, from home mode, scheduler off): paste line → one button press → paste again, no other terminal command; checklist + join report on web and app; traffic-flow and OLT-port fixed by button; laptop on ether5 appears in Unregistered in hotspot mode with the old test user still present; test flat, mode switch, cutover, confirm, suspend test all from the phone; an unconfirmed cutover rolls back at 15 min; one with WAN pulled after step 3 also rolls back.

## 9. Risks
Marker mechanism, `:onerror`, `place-before` over REST, and `ftn-api` write access to hotspot disable / bridge ARP are untested until the bench run (paste cutover stays as fallback). Kit scheduler owner deletion behaviour unknown (`kit.installed` check catches a dead timer). Two cutover paths (paste frozen). Enabling scheduler/fetch loosens device-mode slightly. DNS auto-fix falls back to manual when unsure.

## 10. Still manual
Join paste; button press/power-cycle; anything under `/user`, `/ip/service`, WAN, routes; Confirm cutover; `confirm`-kind fixes; moving the laptop cable once.
