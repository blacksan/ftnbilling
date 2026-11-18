# API v4 W5c: router set-up job

The server runs the router set-up itself. There is no confirm button (release plan A1): a fresh router needs none,
and a live migration is kept or rolled back by the server on evidence. All paths below are also served under
`/api/v1/sites/...` (the old name). Errors use the standard envelope.

## Endpoints

| Method and path | Role | Answer |
|---|---|---|
| `POST /routers` | owner | 201 `{router, join:{command, command_nocheck, expires_at}}`, `Cache-Control: no-store`. Never replayed from the Idempotency-Key cache (the join line is a one-time secret). Body: `name` (required), `serving_customers`, `wan_interface`, `olt_interface`, `vlan_id`, `customer_subnet`, `wan_cap_mbps`, `support_phone`. The router starts in IPoE mode, `provision` off. |
| `PATCH /routers/{id}` | owner | 200 router JSON plus `serving_customers`. Same fields as POST. |
| `GET /routers/{id}/setup` | viewer | 200 `Setup` (below). Reads the router only when no job runs and the cached checks are older than 15 s. |
| `POST /routers/{id}/setup/run` | operator | 202 `Setup`. Starts the job. 409 `bad_state` if a job is running or the router has not joined. |
| `POST /routers/{id}/setup/migrate` | operator | 202 `Setup`. Body `{confirm_name, keep_unknown}`; 422 if `confirm_name` is not the router's name. |
| `POST /routers/{id}/setup/rollback` | operator | 202 `Setup`. Sets the kit's `ftn-armed` marker to `now`; the router goes back within 30 s. Asking again while a rollback is under way is a no-op. |
| `GET /routers/{id}/checks`, `POST .../checks/{check_id}/fix`, `PUT .../mode`, `POST .../join-command` | as W5 | Unchanged. |
| `GET /routers/{id}/cutover`, `POST .../cutover/start`, `POST .../cutover/confirm`, `POST .../cutover/rollback` | as W5 | Thin views over the job. `cutover/start` is `setup/migrate`; `cutover/confirm` only returns the state (`can_confirm` is always false), except that it still confirms a legacy paste cutover; `cutover/rollback` is `setup/rollback`. |
| `POST /accounts` | operator | Extended body: `package_id`, `activate` (grant the package's time now; needs `package_id`), `note`. One synchronous attempt (8 s) to put the flat on the router; an offline router leaves the work queued. 409 `mac_in_use` names the owner. |

Errors from the router: 503 `router_unreachable`, 409 `router_refused`.

Session JSON for the web admin (tab templates are W7's): `GET /admin/routers/setup.json?id=`, and
`POST /admin/routers/setup/run|migrate|rollback` with form field `id` (`migrate` also `confirm_name`, `keep_unknown=1`).
They answer the same `Setup` JSON (`{"error": "..."}` with 409 or 422 on failure). The session POSTs under
`/admin/routers` are owner-only (the existing `minRole` rule), which is stricter than the API's operator.

## Setup

```
{ router_id, state, kind, serving_customers, auto_start,
  join: {state: none|waiting|used|expired, expires_at, report},
  next_action: {kind: paste_join|press_button|fix|migrate|retry|none, text, check_id},
  steps: [{n, key, title, status: pending|running|ok|failed|skipped, detail, at}],
  checks: {ok, warn, fail, unknown}, blockers: [check ids], live_devices,
  evidence: {tunnel_up, internet_ok, flats_required, flats_bound, missing},
  deadline_at, seconds_left, last_error }
```

`state` is `idle` before any job. `kind` is `fresh` or `migrate`.

## Job states

`waiting_router` -> `checking` -> `fixing` -> `needs_action` (parks until the operator or a fix changes something)
-> `staging` -> (fresh) `building` / (migrate) `ready` (waits for the button, or auto-starts) -> `arming` -> `switching`
-> `verifying` -> `live`. A rollback goes `rolling_back` -> `rolled_back`. Any state can end in `failed`.

Migrate steps map onto states: staging is the preflight, arming is backup + arm + network + dhcp + firewall, switching is
switch + telemetry, verifying is verify. Steps in `steps[].key`: `preflight`, `backup`, `arm`, `network`, `dhcp`,
`firewall`, `switch`, `telemetry`, `verify`, `reconcile`.

Classification (per run): ftn-dhcp already enabled means a rerun (every step is a no-op, no writes); live devices on the
old hotspot or DHCP server with serving intent means migrate; live devices without serving intent parks in
`needs_action` and asks for a migration; nothing live means fresh. Auto-start of a migration happens only for a router
with serving intent, only when the join paste is younger than 30 minutes.

## A1 evidence rule (as implemented)

After the switch the job polls (every 3 s, up to 5 minutes for a migration, 30 s for a fresh router) until all hold:

1. the tunnel is up: the router answers and pings the server's tunnel address;
2. internet: a ping to 1.1.1.1 from the router receives replies;
3. `ftn-dhcp` is enabled;
4. every entitled flat (active, MAC, fixed IP) that was seen online in the 10 minutes before the switch has a `bound`
   lease on `ftn-dhcp` at its fixed IP. Flats not seen online are not required.

Pass: the `ftn-armed`, `ftn-undo` and `ftn-cmd` rows are removed (3 tries), the cutover marks are set, the hotspot bypass
flag is cleared, `Announce` says it is live. Fail at the deadline: `ftn-armed` is set to `now`, the job follows the
rollback, puts the building back on hotspot mode, turns on the hotspot bypass (an ip-binding `ftn:acct:<code>` of type
`bypassed` for every entitled flat) and announces what failed. A lost tunnel does not set `now`: the router's own
15-minute countdown fires. A job that armed the kit is never resumed after a server restart; the router is told to
roll back. A job that never armed starts over from step 1.

## Writer allowlist (`provWriter`)

Everything the job writes goes through `internal/sites/prov_writer.go`; anything else returns `ErrProvNotAllowed`.
Adds (comment starts `ftn:` unless noted): `/interface/bridge`, `/interface/vlan`, `/interface/bridge/port` (customer
ports), `/ip/address` (customer gateway on the customer interface), `/ip/pool` (`ftn-unreg`), `/ip/dhcp-server`
(`ftn-dhcp` on the customer interface), `/ip/dhcp-server/network`, `/ip/dhcp-server/lease`, `/ip/arp` (customer
interface), `/queue/simple` (`ftn-` name, `ftn:acct:` comment), `/ip/firewall/filter` and `nat` (`ftn: ipoe*`,
`ftn: customer dns`, `ftn: no WAN DNS`), `/ip/firewall/address-list` (`ftn-unreg`, `ftn-suspended`, and the kit lists
`ftn-cmd`, `ftn-armed`, `ftn-undo` at their fixed marker addresses), `/ip/hotspot/ip-binding` (`bypassed`, `ftn:acct:`).
Sets: `disabled` on `/ip/hotspot` and old `/ip/dhcp-server` rows of the customer interface only; `arp` on the customer
interface; bridge-port bridge moves; rows whose comment starts `ftn:`; the marker comments. Removes: `ftn:` rows and
marker rows. Calls: `/ip/dns/set` (`allow-remote-requests` only), `/ip/traffic-flow/set` (`enabled`, `interfaces`),
`/system/reboot` (no body). Never: users, services, other `/system`, wireguard, routes, the WAN interface or address, IPv6.

## Other pieces

- `func (m *Manager) SetupBusy(siteID string) bool`: true while a job (including a rollback follow-up) works on the
  router. Reboots and other disruptive actions must be refused while it is true. `m.Jobs()` returns a `*ProvJobs` with
  `Running(siteID) bool` and `State(siteID) string` for W9.
- Settings: `site.<id>.prov_job` (the persisted job), `site.<id>.serving_customers` (`yes` or `no`).
- Billing: `HoldSite(siteID) (release func())` makes `ReconcileSite` return `ErrSiteHeld` without writing; tasks retry
  with backoff. After a rollback `SetHotspotBypass(siteID, true)` makes the hotspot-mode reconcile write the bypass
  rows; they are removed when the flag is cleared or the building is on ftn-dhcp again.
- Alerts: `Announce(siteID, text, dedupe)` sends a one-off notice ("Router setup") on email and WhatsApp.

## Deviations from the plan table

- Both kinds create `ftn-dhcp` disabled and enable it in the switch step.
- The reconcile step runs after verify, for both kinds.
- A telemetry failure is not fatal and never rolls back.
- Queue ordering uses `place-before` at creation instead of a move command.
- A fresh router arms the kit only if the kit is installed and an enabled hotspot or old DHCP server on the customer
  interface will be switched off.
- The IPv6 drop rule is not written (outside the allowlist).
