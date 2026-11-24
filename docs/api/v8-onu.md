# ONU management (TR-069) and the ONU register

The server runs a TR-069 ACS (see `deploy/README.md`, "ONU management (TR-069)"). The endpoints below are for the admin app. When ONU management is off (`ACS_LISTEN=off`) the ONU endpoints answer 404 `not_found`; the register endpoints always work.

Roles: viewer reads, operator (the "admin" role) and owner change. Every change is audit-logged. **A Wi-Fi password is accepted and never returned or logged.**

## Identity: the ONU serial

Every customer has an ONU the owner supplies, in routing mode. The ONU is a pair in the equipment register (kind `ont`): **serial** (the TR-069 serial), optional **gpon_serial** (the OLT-side serial, if different) and **mac** (the WAN MAC the router sees). Giving a register ONU to a house sets the house MAC from the pair. A device that reports itself over TR-069 is linked to the house its serial is installed at (`link: "inventory"`); houses with no register entry fall back to matching the WAN MAC (`link: "auto"`); the owner can link by hand (`link: "manual"`). A MAC printed on an ONU label is often the base/LAN MAC, not the WAN MAC: confirm the WAN MAC on the bench before saving a pair.

## Per house

| Method and path | Role | What |
|---|---|---|
| `GET /accounts/{id}/onu` | viewer | `{onu, task, register}`. `onu` is null when no ONU is linked. `onu` has `serial`, `manufacturer`, `model`, `data_model` (`TR-098`/`TR-181`), `online`, `last_inform`, `wifi` (`[{band, ssid, enabled}]` as reported), `desired` (`set`, `same`, `ssid`, `ssid5`, `has_password`, `changed_on_device`; never the password), `last_task`, `link`, `in_inventory`, `inventory_status`, `inventory_mac`, `warnings` (strings), `use_macs` (reported MACs the house can be switched to). `task` is the newest task: `status` is `queued`, `sending`, `accepted`, `confirmed`, `failed`, `expired` or `done` (reboot, refresh, reset), `status_text` is the matching sentence ("Accepted: applies after the ONU restarts" when the box answered status 1), `after_reboot`, `can_confirm` (an accepted Wi-Fi change the owner can confirm), `can_cancel` (queued, not sent yet). **Accepted only means the box said yes; the owner confirms with "It works".** `onu` also has `quarantine` (`{reason, since, last_at, count, ip, mac, site_name, places}`; `places` > 1 means two devices claim the serial; while a check-in from an unexpected place waits for an answer), `login` (`own`, `shared`, `pending`), `shared_login_only` (the box refused its own ACS login: weaker identity), `admin_lock` and `has_admin_password` (the random web-admin password; shown on the web screen only, behind the owner's password), `returned` (left a house; reset queued, never gets a house's Wi-Fi), `blocked` |
| `PUT /accounts/{id}/wifi` | operator | Body `{ssid, password, same_for_both (default true), ssid_5g, password_5g}`. SSID 1 to 32 bytes, printable; password 8 to 63 printable ASCII. `202 {task}`. 400 `validation_failed` with the field. 404 when the house has no ONU |
| `POST /accounts/{id}/onu/reboot` | operator | Queues a Reboot and asks the ONU to connect now. `202 {task}` |
| `POST /accounts/{id}/onu/refresh` | operator | Asks the ONU to report its Wi-Fi. `202 {task}` |
| `POST /accounts/{id}/onu/swap` | operator | Body `{equipment_id, old_status (returned (default), faulty, in_stock, retired), note}`. The house gets the register ONU; its MAC becomes that ONU's WAN MAC. **The old ONU is unlinked at once and a factory reset is queued for its next check-in; it never gets a house's Wi-Fi again.** The new ONU gets the house's Wi-Fi on its first verified session. Returns the account |
| `POST /accounts/{id}/onu/use-reported-mac` | operator | Body `{mac}`: the one-click answer to a MAC mismatch warning; sets the MAC on the house and its register item. 400 unless the MAC was reported by a verified session of this house's ONU (not quarantined, bound to a management MAC, own or accepted ACS login) |
| `POST /accounts/{id}/onu/confirm` | operator | "It works": the newest accepted Wi-Fi change becomes `confirmed`, and the password name that took is remembered for this ONU model. `200 {task}`. 409 `bad_state` when nothing is waiting |
| `POST /accounts/{id}/onu/reject` | operator | "It didn't work": that password name is not trusted again for this box, and the change is queued again another way. `202 {task}` |
| `POST /accounts` | operator | New optional `equipment_id`: an in-stock register ONU; `mac` may then be omitted |

## Task states and expiry

A task is `queued` until the ONU checks in, `sending` while the ACS talks to it, then for a Wi-Fi change `accepted` (status 0 from the box, or "applies after the ONU restarts" for status 1) and `confirmed` once the owner says "It works". Reboot, refresh and factory reset end as `done`. A queued task that waited too long becomes `expired` and is never sent: reboot 1 hour, Wi-Fi 24 hours, factory reset 7 days (the reset of a returned box never expires: it waits for the box). An ONU that is not linked to a house, not in the register and not returned, and silent for 7 days, is forgotten.

## Lists and links

| Method and path | Role | What |
|---|---|---|
| `GET /sites/{id}/onus` | viewer | `{items: [ONU], management_network}` |
| `GET /onus?unlinked=1` | viewer | All ONUs, optionally only those with no house |
| `POST /onus/{id}/link` | operator | `{account_id}`. 409 when the ONU or the house is already linked, or they are on different routers |
| `POST /onus/{id}/unlink` | operator | Drops the link. The box is marked returned and a factory reset is queued for its next check-in (the house's Wi-Fi choice belongs to the house and stays) |
| `POST /onus/{id}/approve` | operator | Answer to the alert "ONU <serial> checked in from an unexpected place": the ONU is bound to that router, address and MAC and gets its own ACS login again. 409 `bad_state` when it is not quarantined, or when held-back check-ins came from more than one place (`places` > 1: block the wrong place first). Returns the ONU |
| `POST /onus/{id}/block` | operator | The other answer: that place is turned away for this serial. 409 when not quarantined. Returns the ONU |
| `POST /onus/{id}/trust` | owner | The owner's override for an ONU that waits (`mac_wait` is set on the ONU: `{text: "Waiting: management MAC doesn't match the registered ONU yet", lease_mac, registered_mac, reason, can_trust}`): the management MAC it checked in from is trusted for this serial, audited. Body `{mac}` (optional, sent by the app): the `lease_mac` that was shown; 409 `bad_state` when the ONU has since checked in from another MAC. 409 `bad_state` also when it is not waiting or the router has not said which MAC holds its address (`can_trust` false). Returns the ONU. With `ACS_REGISTERED_ONLY` on, a registered ONU's first contact (and a return on the shared login after a factory reset) gets no secret until the management lease MAC is near the inventory WAN MAC (same OUI, last 24 bits within 8); an inventory item with no WAN MAC always needs this override |
| `GET /onus/unknown?site_id=` | viewer | `{items: [{serial, oui, manufacturer, model, site_id, site_name, ip, first_seen, last_seen, count}]}`: serials not in the ONU inventory that tried to check in (`ACS_REGISTERED_ONLY`, default on). They get nothing and are not stored as devices; an entry goes after 7 days without a check-in or once the serial is added to the inventory |
| `POST /onus/{id}/unblock` | operator | Lifts every block for the serial |
| `POST /onu-tasks/{id}/cancel` | operator | Cancels a queued task before it is sent. 409 `bad_state` when it was already sent or expired |
| `POST /onus/{id}/inventory` | operator | `{mac}`: adds the ONU's serial and the chosen reported MAC to the register (and installs it at its house when that house's MAC is the same). 201 the item |

## The register

| Method and path | Role | What |
|---|---|---|
| `GET /equipment?kind=ont&status=in_stock` | viewer | `{items: [Equipment]}` (`id, kind, serial, gpon_serial, mac, model, status, account_id, site_id`) |
| `POST /equipment` | operator | `{kind (default ont), serial, gpon_serial, mac, model, notes}`. An ONT needs a MAC. 409 `mac_in_use` / `bad_state` for a duplicate MAC or serial |
| `POST /equipment/import` | operator | `{lines}`: one ONU per line, `serial,mac,model[,gpon serial]` (or `kind,serial,mac,model`). `{added, skipped: ["line 2: ..."]}` |

Status values: `in_stock`, `installed`, `faulty`, `returned`, `retired`. A serial (either kind) and a MAC cannot be on two items. A house MAC that is another apartment's still falls under the MAC-in-another-apartment rule when the ONU is given out.
