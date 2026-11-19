# API v5: one bootstrap script, no Hotspot

Supersedes `v4-w5c.md` (router set-up job, migrate, rollback, cutover). Design: `docs/plan-v5-bootstrap.md` (amendments M1-M4 apply).
Every router is IPoE. The operator pastes ONE line on a blank router; the server issues everything else. There is no
provisioning job. All paths are also served under `/api/v1/sites/...` (the old name). Errors use the standard envelope.
Roles: `viewer` < `operator` < `owner`.

## Router record (create and patch)

`POST /routers` (owner) answers 201 `{router, join}` with `Cache-Control: no-store` (never replayed from the
Idempotency-Key cache: the line is a one-time secret). `PATCH /routers/{id}` (owner) answers 200 with the router JSON.
Both take the same JSON body; on PATCH every field is optional and only the fields present change. `name` is required on POST.
Unknown fields are ignored. Validation errors are 400 `validation_failed` with a `details.fields` map.

| Field | Type | Default | Rule |
|---|---|---|---|
| `name` | string | - | 1 to 60 characters |
| `wan_interface` | string | `ether1` | RouterOS interface name (letters, digits, `.`, `-`, `_`) |
| `wan_mode` | string | `dhcp` | `dhcp`, `static`, `pppoe` |
| `wan_address` | string | "" | static only: IPv4 with prefix, `203.0.113.10/29` |
| `wan_gateway` | string | "" | static only: IPv4 |
| `wan_dns` | string | "" | static only: comma list of IPv4 (optional; WAN DNS is not used for customers) |
| `pppoe_user` | string | "" | pppoe only |
| `pppoe_pass` | string | "" | pppoe only. Write-only: never returned; stored sealed with `DATA_KEY`. On PATCH an empty string leaves it unchanged; `"pppoe_pass_clear": true` removes it |
| `lan_ports` | string | `ether2` | comma list of interface names: the admin LAN (WinBox / laptop) |
| `lan_subnet` | string | `192.168.88.0/24` | IPv4 /16 to /29 prefix; must not overlap `customer_subnet`, the WireGuard subnet or the management subnet (`MGMT_SUBNET`, default `10.98.0.0/24`); also applies to a static WAN network |
| `olt_interface` | string | `sfp-sfpplus1` | fibre port |
| `vlan_id` | int | 0 | 0 = untagged, else 1 to 4094 (the script makes `vlan<N>-customers` on the OLT port) |
| `customer_subnet` | string | `10.5.50.0/24` | /20 to /24; gateway .1, unregistered pool .10-.59, flats from .60 |
| `bench_port` | string | "" | optional extra customer-side port (e.g. `ether5`) |
| `wan_cap_mbps`, `olt_cap_mbps`, `fup_tb` | number | 500 / 10000 / 15 | capacities |
| `support_phone` | string | "" | shown on the payment page |

Router JSON (list, detail, and the `router` in create/patch answers) also has `tunnel_address` (string): the router's address inside the
WireGuard tunnel (for example `10.99.0.2`), empty until the router has fetched its script. It is what WinBox connects to over the management
WireGuard profile (remote WinBox, `deploy/README.md`); the admin router page shows it with the line "Remote WinBox: connect your management
WireGuard profile, then open WinBox to <tunnel address>". No private key or password is ever in router JSON.

Removed from the body (ignored as unknown fields): `serving_customers`, `hotspot_server`, `wan_list`, `provision`,
`legacy_container`. Router JSON keeps `access_mode` with the single value `"ipoe"`. Router JSON gains `wan_mode`,
`wan_address`, `wan_gateway`, `wan_dns`, `pppoe_user`, `pppoe_pass_set` (bool), `lan_ports`, `lan_subnet`; it never has the password.

## Setup

`GET /routers/{id}/setup` (viewer), `Cache-Control: no-store`. Cheap: it reads the server's own state and cached
checks; the router is only read by the cache refresh (never more often than every 60 s, automatically).

```
{ router_id,
  state: not_started | waiting_router | stalled | online | ready,
  join:  { state: none|waiting|used|expired, expires_at, report: {at, ok, ros, lines[]} | null },
  next_action: { kind: paste_line | fix | wait | none, text, check_id },
  steps: [ {n, key, title, status: pending|running|ok|failed, detail, at} ],
  checks: {ok, warn, fail, unknown}, blockers: [check ids], last_error }
```

`state`:
- `not_started`: no line has been fetched by a router yet (join token pending, expired or never minted).
- `waiting_router`: the setup script was fetched (`steps[0]` ok) but the tunnel is not up yet.
- `stalled`: the script was fetched more than 10 minutes ago, no report arrived and the tunnel is not up: the router never came back (the fetch can rotate its keys before the reset, so it may have stopped halfway). `next_action` is `paste_line`: reset the router with the Reset button and paste a new line.
- `online`: the tunnel is up (WireGuard handshake younger than 3 minutes) but the checks are not all green yet.
- `ready`: tunnel up, server login works, no failing check.

`steps` are always these four, derived (nothing is run by a job):

| n | key | title | ok when |
|---|---|---|---|
| 1 | `script_fetched` | Router fetched the script | the join token was used |
| 2 | `tunnel_up` | Tunnel is up | WireGuard handshake age < 3 min |
| 3 | `server_login` | Server can log in | a REST read of the router succeeded (`JoinedAt` set) |
| 4 | `checks_green` | Checks are green | no `fail` and no blocker |

`status` is `running` for the first step that is not ok when the previous one is; `failed` only for step 4 with failing checks
(and for step 1 if the join report says the script failed). `join.report` is the router's own report of the script run
(`lines` are sanitised; secrets never appear). `next_action.kind`: `paste_line` (not started, expired, `stalled`, or the router has no v5 layout: "Rebuild required: reset the router and paste a new line"), `fix` (a check fails; `check_id` is the first), `wait`, `none`. There is no `press_button`: the server cannot tell that the device-mode button is needed (the paste line prints it on the router).

Clients poll every 5 s while the state is not `ready`, then every 30 s.

## Join line and download

`POST /routers/{id}/join-command` (owner) answers `{command, command_nocheck, expires_at}` (`no-store`, mints a new token and
voids the old one; the router's keys are made when the router fetches the script). `command` is the single RouterOS paste
(Line A). `command_nocheck` is the same with certificate checking off for the fetches (for a router whose clock or trust
anchors are wrong). The same object is `join` in the POST /routers answer.

The line does, in order: check `device-mode` allows `fetch` (if not, it prints the button instruction, runs
`/system/device-mode/update fetch=yes` and stops); turn trust anchors on; remove stale `ftn-setup.rsc` / `ftn-reset.rsc`;
fetch `/j/<token>` as `ftn-setup.rsc` and `/j/<token>/reset` as `ftn-reset.rsc`; `/import ftn-reset.rsc`, which checks the
setup file is present and runs `/system/reset-configuration no-defaults=yes skip-backup=yes keep-users=yes
run-after-reset=ftn-setup.rsc`. The router erases itself and builds the whole configuration (including WireGuard and the
REST user), posts its report and removes the files. A second paste is harmless: the token is single use, so the fetch fails
and nothing is reset.

Public endpoints (no session; the token is the credential):
- `GET /j/<token>`: the setup script (`text/plain`, `no-store`), consumes the token, makes the router's keys and REST password. 404 for anything invalid, with the same body.
- `GET /j/<token>/reset`: the fixed reset stub. Accepted for 5 minutes after the setup fetch, any number of times. 404 otherwise.
- `POST /j/<token>/report` (and the existing report path): the script's run report. Unchanged.

`POST /routers/{id}/script` (owner, API only: the web admin has no button for it), body `{"confirm_name": "<router name>", "force": false}`.
`Content-Disposition: attachment; filename=ftn-setup.rsc`, `no-store`, never replayed from the Idempotency-Key cache. Line B, for a router that cannot take
the paste line: mints a token, consumes it server-side and returns the same script. The owner drags it into WinBox Files
and runs `/import ftn-setup.rsc` on a blank router. 400 `bad_request` without a body, 422 `validation_failed` (field `confirm_name`) on a wrong
name, 409 `bad_state` when the router is not set up on the server side, **and 409 when the router is joined and its tunnel is
fresh** (a new script makes new keys and would cut a live router off) unless `force` is `true`. Every download makes new
keys; the previous script stops working. The script runs only on a blank router: its first section refuses (and changes nothing) when it
finds any object with an `ftn:` comment (an earlier run) or any address, bridge, DHCP server or firewall rule that is not ours. A
rebuild always goes through the paste line (which resets the router first).

## Actions

`POST /routers/{id}/actions/reset` (owner), body `{"confirm_name": "<router name>"}`. 400 `validation_failed` (field `confirm_name`) on a wrong name. Clears the
router's join state (public key, REST password, join report, tasks, cached checks and live data, in-memory reconcile
state), rewrites the WireGuard peers, stops the runtime, keeps accounts and the tunnel IP, queues every flat again and
answers 200 `{router, join:{command, command_nocheck, expires_at}}` with a fresh line (`no-store`). It does not touch the
router (Line A erases it when the new line is pasted).

`POST /routers/{id}/actions/reboot`: unchanged.

## Checks

`GET /routers/{id}/checks[?fresh=1]`, `POST /routers/{id}/checks/{check_id}/fix`: unchanged shape. Cached 60 s; `fresh=1`
re-reads. Checks kept: `router.version`, `api_user.policy`, `device_mode.fetch`, `dns.remote_requests`,
`customer.dns_input`, `customer.interface`, `olt.bridge_conflict`, `bench.port`, `traffic_flow`, `service.www`, `ipv6`,
`server.public_ip`, `router.script_hooks`. New: `router.layout` (the router has the v5 layout: the `br-customers` bridge with the comment `ftn: customer bridge` that only the script makes; on a router without it every fix is off, nothing is written, and the item says "Rebuild required: reset the router and paste a new line"), `walled.rules` (fix rewrites the walled-garden rules), `dhcp.ftn` (fix
enables `ftn-dhcp`), `wan.internet` (the router can reach the internet; no fix), `bootstrap.file` (a leftover
`ftn-setup.rsc`/`ftn-reset.rsc`; the fix removes exactly those files). Checks never carry a command to copy: each either has an auto fix (the server issues it) or explains in words (most say to paste a new line, which rebuilds the item). The fix object has no `command` field. The checks JSON has `layout` (`v5`, `old` or `unknown`) and no cutover fields (`blocks_cutover`, `cutover_ready` and `override` are gone). Removed: `device_mode.scheduler`, `device_mode.hotspot`,
`kit.installed`, `hotspot.orphan_users`, `containers`, `unmanaged.devices`, `flats.unseen`.

## Bench

`POST /routers/{id}/bench/test-flat`, `POST /routers/{id}/bench/suspend-test`: unchanged, without the access-mode guard.

## Removed endpoints (answer 410 `gone` for one release)

`PUT /routers/{id}/mode`, `GET /routers/{id}/cutover`, `POST .../cutover/start|confirm|rollback`,
`POST /routers/{id}/setup/run|migrate|rollback`. Session JSON `GET /admin/routers/setup.json?id=` answers the new `Setup`;
`POST /admin/routers/setup/run|migrate|rollback` answer 410.

## Unchanged

`POST /accounts` (including the one synchronous attempt to put a flat on the router), `/pay`, M-Pesa callbacks, live data.

## Public payment page (WS6 follow-up)

The hotspot portal is gone. `GET /portal` redirects to `/pay`. Vouchers are redeemed on the payment page:
`POST /pay/voucher` (form: `tok` or `code` of the account, `voucher`, the `_pcsrf` double-submit token). A voucher made for
another router, a used voucher and an unknown voucher all answer 400 with the same words. The `router_user` setting
(`ROUTER_USER`), `ROUTER_URL`, `ROUTER_PASS`, `HOTSPOT_SERVER`, `SITE_NAME` and the "Main building" seed from the
environment are removed; a router exists only by being added and set up with the paste line.
