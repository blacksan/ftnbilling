# OLT management (V-SOL), server side

The server can read a V-SOL OLT over SSH (or Telnet) and, only when the owner allows it per OLT, configure it and authorise ONUs by itself. The OLT is reached over the router, through a dedicated OLT-management VLAN (default 98) that only the server can reach. When OLT management is off on the server the endpoints answer 404 `not_found`.

**No password is ever returned**: not the OLT login, not the enable password. Saved configurations come back with secrets redacted, to owners only.

| Method and path | Role | What |
|---|---|---|
| `GET /olts/{id}/status` | viewer | `{olt_id, name, configured, protocol, connected, checked_at, error, version, model, sw_version, hw_version, version_note, findings, online, total, onus, auto_found, not_in_inventory, provisioning:{enabled, dialect_checked}, host_key, unparsed}`. `onus` rows: `{pon, id, serial, state, online, rx_power, tx_power, distance, last_down}` (strings as the OLT printed them; empty when not read). `auto_found` rows: `{pon, serial, verdict, detail, first_seen}`; `verdict` is `added`, `ready`, `unknown`, `held` or `failed`. `not_in_inventory` is the `unknown` rows. `version_note` is a warning when the OLT's model or software is not the one the commands were checked against (V1600GS-R, V1.0.5). `findings` are read-only statements about the OLT's own settings (auto-learn on, default SNMP communities, ...) and ONUs on the OLT that are not in the inventory. `unparsed` is raw output the parser could not read (kept so nothing is lost). Read from the last poll (about every 5 minutes) |
| `POST /olts/{id}/test` | operator | Connects, logs in, reads the version. `{ok, version}`. 502 `olt_error` with a plain sentence on failure |
| `POST /olts/{id}/poll` | operator | Reads the OLT now; answers like `status` |
| `GET /olts/{id}/backups` | owner | `{items:[{id, at, why, by, size}]}`, newest first, the last 20 are kept |
| `GET /olts/{id}/backups/{bid}` | owner | `{id, at, why, text, redacted:true}`, `no-store`, audit-logged |
| `PUT /olts/{id}/provisioning` | owner | Body `{enabled?, dialect_checked?, password}`. Needs the owner's password again (403 `invalid_credentials`, 429 `rate_limited` after repeated wrong ones). Provisioning is off by default; it only works when `dialect_checked` is true. Answers like `status` |
| `POST /olts/{id}/plan` | owner | Reads the OLT and answers the exact command list the server would send: `{hash, empty, lines, steps:[{title, lines}], adds:[{pon, id, serial, label}], notes, warnings}`. Sends nothing |
| `POST /olts/{id}/apply` | owner | Body `{hash, password}`. Backs up, sends the plan, re-reads and verifies: `{backup_id, sent, verified, remaining}`. 409 `bad_state` when provisioning is off or the plan changed since (look again, send the new hash). A command the OLT refuses stops the batch and is audit-logged |

Rules the server keeps for every line it sends: a built-in deny list (erase, factory reset, reboot, format, startup-config delete, `no` on the management interface or VLAN, user or password changes) is checked in the driver and cannot be switched off. ONU auto-provisioning authorises only a GPON serial that is in the inventory (not retired) and assigned to a house served by that OLT and PON; unknown serials are never added (they appear in `not_in_inventory` and the owner is alerted, rate limited), an inventory serial seen on the wrong OLT or PON is held and the owner is alerted. Removing an ONU from the OLT is only ever an explicit owner action.

The TR-069 ACS URL and the shared first-contact ACS login go to the OLT over the CLI, in a Pri profile (`tr069_mng`) that new ONUs are bound to (ONUs already on the OLT get the same per ONU). The login is hidden in every plan, result and audit text.
