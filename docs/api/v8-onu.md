# ONU management (TR-069) and the ONU register

The server runs a TR-069 ACS (see `deploy/README.md`, "ONU management (TR-069)"). The endpoints below are for the admin app. When ONU management is off (`ACS_LISTEN=off`) the ONU endpoints answer 404 `not_found`; the register endpoints always work.

Roles: viewer reads, operator (the "admin" role) and owner change. Every change is audit-logged. **A Wi-Fi password is accepted and never returned or logged.**

## Identity: the ONU serial

Every customer has an ONU the owner supplies, in routing mode. The ONU is a pair in the equipment register (kind `ont`): **serial** (the TR-069 serial), optional **gpon_serial** (the OLT-side serial, if different) and **mac** (the WAN MAC the router sees). Giving a register ONU to a house sets the house MAC from the pair. A device that reports itself over TR-069 is linked to the house its serial is installed at (`link: "inventory"`); houses with no register entry fall back to matching the WAN MAC (`link: "auto"`); the owner can link by hand (`link: "manual"`). A MAC printed on an ONU label is often the base/LAN MAC, not the WAN MAC: confirm the WAN MAC on the bench before saving a pair.

## Per house

| Method and path | Role | What |
|---|---|---|
| `GET /accounts/{id}/onu` | viewer | `{onu, task, register}`. `onu` is null when no ONU is linked. `onu` has `serial`, `manufacturer`, `model`, `data_model` (`TR-098`/`TR-181`), `online`, `last_inform`, `wifi` (`[{band, ssid, enabled}]` as reported), `desired` (`set`, `same`, `ssid`, `ssid5`, `has_password`, `changed_on_device`; never the password), `last_task`, `link`, `in_inventory`, `inventory_status`, `inventory_mac`, `warnings` (strings), `use_macs` (reported MACs the house can be switched to). `task` is the newest task: `status` is `queued`, `applied` or `failed`, `status_text` is "Queued", "Applied" or "Failed: ..." |
| `PUT /accounts/{id}/wifi` | operator | Body `{ssid, password, same_for_both (default true), ssid_5g, password_5g}`. SSID 1 to 32 bytes, printable; password 8 to 63 printable ASCII. `202 {task}`. 400 `validation_failed` with the field. 404 when the house has no ONU |
| `POST /accounts/{id}/onu/reboot` | operator | Queues a Reboot and asks the ONU to connect now. `202 {task}` |
| `POST /accounts/{id}/onu/refresh` | operator | Asks the ONU to report its Wi-Fi. `202 {task}` |
| `POST /accounts/{id}/onu/swap` | operator | Body `{equipment_id, old_status (in_stock, faulty, returned, retired), note}`. The house gets the register ONU; its MAC becomes that ONU's WAN MAC. Returns the account |
| `POST /accounts/{id}/onu/use-reported-mac` | operator | Body `{mac}`: the one-click answer to a MAC mismatch warning; sets the MAC on the house and its register item |
| `POST /accounts` | operator | New optional `equipment_id`: an in-stock register ONU; `mac` may then be omitted |

## Lists and links

| Method and path | Role | What |
|---|---|---|
| `GET /sites/{id}/onus` | viewer | `{items: [ONU], management_network}` |
| `GET /onus?unlinked=1` | viewer | All ONUs, optionally only those with no house |
| `POST /onus/{id}/link` | operator | `{account_id}`. 409 when the ONU or the house is already linked, or they are on different routers |
| `POST /onus/{id}/unlink` | operator | Drops the link and the saved Wi-Fi choice |
| `POST /onus/{id}/inventory` | operator | `{mac}`: adds the ONU's serial and the chosen reported MAC to the register (and installs it at its house when that house's MAC is the same). 201 the item |

## The register

| Method and path | Role | What |
|---|---|---|
| `GET /equipment?kind=ont&status=in_stock` | viewer | `{items: [Equipment]}` (`id, kind, serial, gpon_serial, mac, model, status, account_id, site_id`) |
| `POST /equipment` | operator | `{kind (default ont), serial, gpon_serial, mac, model, notes}`. An ONT needs a MAC. 409 `mac_in_use` / `bad_state` for a duplicate MAC or serial |
| `POST /equipment/import` | operator | `{lines}`: one ONU per line, `serial,mac,model[,gpon serial]` (or `kind,serial,mac,model`). `{added, skipped: ["line 2: ..."]}` |

Status values: `in_stock`, `installed`, `faulty`, `returned`, `retired`. A serial (either kind) and a MAC cannot be on two items. A house MAC that is another apartment's still falls under the MAC-in-another-apartment rule when the ONU is given out.
