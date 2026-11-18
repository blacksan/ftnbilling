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
| `POST /routers/{id}/setup/rollback` | owner | Body `{confirm_name}` (the router's name; 422 otherwise). 202 `Setup`: sets the kit's `ftn-armed` marker to `now`, the router goes back within 30 s. It only ever modifies an existing marker: with no `ftn-armed` or no `ftn-undo` rows (a live, confirmed router) it answers 409 `bad_state` and writes nothing (a stray `now` marker with no undo rows is removed). Asking again while a rollback is under way is a no-op. |
| `GET /routers/{id}/checks`, `POST .../checks/{check_id}/fix`, `PUT .../mode`, `POST .../join-command` | as W5 | Unchanged. |
| `GET /routers/{id}/cutover`, `POST .../cutover/start`, `POST .../cutover/confirm`, `POST .../cutover/rollback` | as W5 | Thin views over the job. `cutover/start` is `setup/migrate`; `cutover/confirm` only returns the state (`can_confirm` is always false), except that it still confirms a legacy paste cutover; `cutover/rollback` is `setup/rollback` (owner, typed name). |
| `POST /accounts` | operator | Extended body: `package_id`, `activate` (grant the package's time now; needs `package_id`), `note`. One synchronous attempt (8 s) to put the flat on the router; an offline router leaves the work queued. 409 `mac_in_use` names the owner. |

Errors from the router: 503 `router_unreachable`, 409 `router_refused`.

Session JSON for the web admin (tab templates are W7's): `GET /admin/routers/setup.json?id=`, and
`POST /admin/routers/setup/run|migrate|rollback` with form field `id` (`migrate` and `rollback` also `confirm_name`; `migrate` also `keep_unknown=1`).
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
switch + telemetry, verifying is verify. Steps in `steps[].key`: `preflight`, `leases`, `backup`, `arm`, `network`, `dhcp`,
`firewall`, `switch`, `telemetry`, `verify`, `reconcile`.

Classification (per run): ftn-dhcp already enabled means a rerun (every step is a no-op, no writes); live devices on the
old hotspot or DHCP server with serving intent means migrate; live devices without serving intent parks in
`needs_action` and asks for a migration; nothing live means fresh. Auto-start of a migration happens only for a router
with serving intent, only when the join paste is younger than 30 minutes.

## Old leases (review B2)

Before the kit is armed the job shortens the lease time of the old DHCP servers on the customer interface to 1 minute
(only `lease-time`, only those servers), reads each bound lease's `expires-after`, and waits until the longest has
passed plus 1 minute (never more than the longest original lease plus 1 minute, capped at 65 minutes); the `leases`
step shows the countdown and the reconcile lock is not held meanwhile. The originals are kept in setting
`site.<id>.prov_lease` and put back when the job is live, failed or rolled back (the kit has no undo row for them). The
evidence window is `max(5 min, longest original lease / 2 + 2 min)`, never more than 12 minutes, and the switch restarts
the kit's 15 minute countdown.

## A1 evidence rule (as implemented)

After the switch the job polls (every 3 s, up to 5 minutes for a migration, 30 s for a fresh router) until all hold:

1. the tunnel is up: the router answers and pings the server's tunnel address;
2. internet: a ping to 1.1.1.1 from the router receives replies, and so does one sent from the customer gateway address
   (`src-address`: proves forwarding and masquerade);
3. `ftn-dhcp` is enabled; customers' DNS works (`allow-remote-requests` is on and the check `customer.dns_input` is ok);
   the fibre port is not stuck in a bridge (`olt.bridge_conflict` ok or no OLT);
4. every entitled flat (active, MAC, fixed IP) that was seen online in the 10 minutes before the switch has a `bound`
   lease on `ftn-dhcp` at its fixed IP, and at least one of them has a replied connection to a public address in
   `/ip/firewall/connection` (read only). Flats not seen online are not required; with none required the ping and DNS
   parts still apply.

Pass: the job is saved as no longer armed first, then the `ftn-armed`, `ftn-undo` and `ftn-cmd` rows are removed (3 tries) (a restart never rolls back a router whose confirmed mark is set), the cutover marks are set, the hotspot bypass
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

## Fresh path guards (review S2, S4)

A fresh set-up that has to build the customer network stops in `needs_action` when the plan network overlaps any
address on the router, when the fibre port is in the `LAN` list, is the default `bridge`, is already a port of another
bridge (with or without a VLAN id), or fails the port assessment (WAN port, own address, DHCP or hotspot). It re-reads
the checks after the network is built. A router with serving intent always takes the migrate path. A fresh run that
would switch off an existing hotspot or DHCP server needs the safety kit (else `needs_action`: paste the join line again).

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
