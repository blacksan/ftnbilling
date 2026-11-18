# Online billing server for many buildings (VPS + WireGuard)

One VPS runs billing, the tenant portal and the admin pages for every building.
Each building keeps its own MikroTik router doing the hotspot. Routers join a
private WireGuard network at the VPS; the VPS reaches each router's REST API
through its tunnel address.

```
 Building A router ──┐                                 ┌─ ftnbilling :8080 (admin: only on 10.98.0.1, wg-admin)
 Building B router ──┼── WireGuard udp/51820 ──► VPS ──┤  Caddy :80/:443  (public portal, M-Pesa callbacks)
 Building C router ──┘   10.99.0.0/22, wg0             ├─ NetFlow udp/2055 on 10.99.0.1 (tunnel only)
 you (phone/laptop) ── WireGuard udp/51821 ───────────┘  PostgreSQL (local socket only, no TCP)
                       10.98.0.0/24, wg-admin
 phones ── https://DOMAIN/portal?site=<id> ─────────────► Caddy
```

| Path | How |
|---|---|
| VPS to router REST API | Over the tunnel, to the router's tunnel address (`10.99.0.x:8081`). The router allows REST only from `10.99.0.1`. |
| Phones to portal | Public internet, `https://DOMAIN/portal?site=<building id>`. The hotspot walled garden lets unpaid phones reach only the VPS. |
| Router to NetFlow | Through the tunnel, to `10.99.0.1:2055`. The server tells buildings apart by the tunnel source address. |
| You to admin | Not published. Join the admin VPN (WireGuard interface `wg-admin`, `10.98.0.0/24`): scan the QR code the installer prints, then open `http://10.98.0.1:8080/admin`. Fallback: an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 you@VPS`). Caddy refuses `/admin` from the internet, and the app only accepts it from `ADMIN_ALLOW_CIDR` (`10.98.0.0/24`), never from the routers' tunnel. |

Design size: about 40 buildings and 1500 flats on a 1 vCPU / 2 GB VPS. The
service runs with `GOMEMLIMIT=600MiB GOGC=50`.

The portal is public, so the server trusts a `mac`/`ip` pair only if that
building's router currently lists it in `/ip/hotspot/host`. This check is
always on for WireGuard buildings. Someone outside the building who guesses a
flat's MAC gets the neutral "open this page from the WiFi login" page.

## 1. New server in one command

You need a brand-new, minimal VPS (Ubuntu 22.04/24.04 or Debian 12, 1 vCPU, 2 GB RAM, 60 GB disk is typical) with a public IPv4 address, and a few minutes. Nothing has to be installed on it first: the installer installs every dependency itself.

Before you start:

1. **Domain:** an A record such as `billing.yourdomain.co.ke` pointing to the VPS. HTTPS is required for M-Pesa and for the router join command (the join script carries the router's keys, so it is never served over plain HTTP). You can install without it and re-run later with `DOMAIN=...`.
2. **GitHub token:** the repo is private. Create a fine-grained token for `blacksan/ftnbilling` with **Contents: Read-only**. It is used to fetch the installer and the release (or the source).
3. **Routers:** RouterOS 7 with the NTP client on, and the hotspot and fetch features enabled: `/system/device-mode/update hotspot=yes fetch=yes` (confirm as the router asks).

Then, as a normal sudo user on the new VPS:

```bash
GH=github_pat_xxx; curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | sudo GH_TOKEN=$GH DOMAIN=billing.yourdomain.co.ke bash
```

It prints numbered steps (`[7/15] ...`), stops with a clear message if one fails, and is safe to run again. At the end it runs the doctor (below), prints the admin password, and shows a **QR code** for the admin VPN.

To preview without changing anything: `DRY_RUN=1` (works without root and prints every action):

```bash
curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | DRY_RUN=1 bash
```

Options (environment variables): `DOMAIN`, `GH_TOKEN`, `VERSION` (a release tag; default is the latest release), `FROM_SOURCE=1` (build on the server instead of using a release), `NO_POSTGRES=1`, `NO_FIREWALL=1`, `DRY_RUN=1`, `PUBLIC_IP`, `ADMIN_PASS`, `BRAND_NAME`, `SUPPORT_PHONE`, `WG_PORT`, `ADMIN_WG_PORT`, `WG_SUBNET`, `SWAP_MB`. The header of `install.sh` lists them.

### Where the program comes from

1. **A GitHub Release (normal path).** Pushing a tag `vX.Y.Z` makes `.github/workflows/release.yml` build `ftnbilling-<version>-linux-amd64.tar.gz` and `...-arm64.tar.gz` (static binary, deploy scripts, systemd units, docs, `VERSION`, `SHA256SUMS`) and attach them, with a `SHA256SUMS` file, to the release. The installer downloads the right one through the GitHub API with `GH_TOKEN`, checks the SHA256, and unpacks it into `/opt/ftnbilling/releases/`. The server never needs Go.
2. **Offline bundle.** Copy the `.tar.gz` to the server by any means (scp, USB, a release you downloaded on your laptop), then:

   ```bash
   tar xzf ftnbilling-v1.2.0-linux-amd64.tar.gz
   cd ftnbilling-v1.2.0-linux-amd64
   sudo DOMAIN=billing.yourdomain.co.ke ./install.sh
   ```

   The bundled program is used; nothing is downloaded from GitHub (apt still needs the distro and Caddy package mirrors). You can make a bundle yourself with `./build.sh bundle` (amd64; `./build.sh bundle all` for amd64 and arm64) in a checkout; the result is `dist/ftnbilling-<version>-linux-<arch>.tar.gz`.
3. **Source build (fallback).** If the repo has no release for that version, the installer clones the branch, installs Go only then, and builds with the vendored modules (`-mod=vendor`) under `nice`, which takes a few minutes on 1 vCPU (the swap file covers the memory).

### What gets installed

| Part | Detail |
|---|---|
| Packages (apt) | `curl ca-certificates gnupg git tar xz-utils unzip jq ufw qrencode wireguard-tools unattended-upgrades cron`, plus `chrony` if no time sync is running |
| PostgreSQL | The distro's `postgresql` and `postgresql-client`. No TCP listener (`listen_addresses=''`), tuned for 1 vCPU / 2 GB in `conf.d/90-ftnbilling.conf`, `scram-sha-256` for role `ftnbilling` over the socket, database `ftnbilling`, `REVOKE CONNECT ... FROM PUBLIC`, `OOMScoreAdjust=-500` so Postgres outlives the app under memory pressure. The password and `DATA_KEY` (encrypts national IDs) are generated once into the settings file. `NO_POSTGRES=1` skips all of it |
| Caddy | From the official Caddy apt repository; automatic HTTPS for `DOMAIN` |
| rclone | For the Google Drive backup (distro package if 1.60 or newer, otherwise the official installer) |
| The app | `/usr/local/bin/ftnbilling`, systemd service `ftnbilling` (`GOMEMLIMIT=600MiB GOGC=50`, `MemoryMax=900M`, runs after PostgreSQL and both WireGuard interfaces), data in `/var/lib/ftnbilling`, settings in `/etc/ftnbilling/ftnbilling.env` |
| `wg0` | Routers' tunnel: `10.99.0.1/22`, UDP 51820, MTU 1380. The private key is generated once in `/etc/wireguard/wg0.conf` and never regenerated. `ftnbilling-wg-sync` (root, via a systemd path unit) applies the peer list the app writes after validating it (only `[Peer]`, a 44-character `PublicKey`, one `/32` inside the subnet; no symlinks, no file over 256 KB) and `ftnbilling-wg-status.timer` writes handshake times for the admin page |
| `wg-admin` | Your tunnel: `10.98.0.1/24`, UDP 51821, its own key in `/etc/wireguard/wg-admin.conf`, managed with `ftnbilling-admin-peer` |
| Firewall (ufw) | Deny incoming by default. Open: SSH, 80, 443, UDP 51820, UDP 51821. On `wg0` only UDP 2055 to `10.99.0.1`. On `wg-admin` only TCP 8080 and SSH to `10.98.0.1`. All other input and all routing from either tunnel is denied. The public UDP 2055 rule is removed (kept on an old install that still has `ROUTER_URL`). Warns if `net.ipv4.ip_forward` is 1 |
| Swap and logs | A 1 GB `/swapfile` (`vm.swappiness=10`) when RAM is 2 GB or less and there is no swap; journald `SystemMaxUse=500M` |
| Updates | `unattended-upgrades`, security updates only, never reboots by itself |
| Backups | `ftnbilling-dump.timer` every 6 hours (database dump, daily file tar), `ftnbilling-drill.timer` monthly restore drill, `ftnbilling-backup.timer` weekly Google Drive upload once you run `sudo ftnbilling-backup setup`. See [BACKUP.md](BACKUP.md). The old daily cron job is removed |
| Tools | `/usr/local/sbin/`: `ftnbilling-admin-peer`, `ftnbilling-doctor`, `ftnbilling-backup`, `ftnbilling-wg-sync` |

On a first install the installer also creates the first admin device, `owner`.

**Upgrading:** run the same line again (or run `./install.sh` from a newer bundle). Data, settings, the database password, `DATA_KEY` and the WireGuard keys are kept. An install that was on the JSON file moves to PostgreSQL: the installer first saves `ispbill.json` to `/var/backups/ftnbilling/pre-postgres-<date>.tgz`, and the next start imports it automatically (the old file is renamed `ispbill.json.imported-<date>`). A v2 data file is converted to the new format on first start; the old file is kept as `ispbill.json.v2.bak`.

### Open the admin (WireGuard QR)

The admin is not on the internet. The installer prints a QR code for the device `owner`:

1. Install the WireGuard app (iOS, Android, Windows, macOS, Linux).
2. Scan the QR code (Add tunnel, Create from QR code), or import `/root/ftnbilling-admin-owner.conf`. Switch the tunnel on.
3. Open `http://10.98.0.1:8080/admin` and log in as `admin` with the password the installer printed.

The tunnel carries only traffic for `10.98.0.1`, so the rest of the device's internet does not go through the VPS. The private key is shown once on screen and saved in `/root/ftnbilling-admin-owner.conf` (mode 0600); delete the file after the device is set up. More devices, a list, and revoking:

```bash
sudo ftnbilling-admin-peer add brian-laptop      # prints the config + QR, saves /root/ftnbilling-admin-brian-laptop.conf
sudo ftnbilling-admin-peer list                  # name, address, last handshake
sudo ftnbilling-admin-peer remove brian-laptop   # the device can no longer connect
```

If the VPN cannot be used, the SSH tunnel still works: `ssh -L 8080:127.0.0.1:8080 you@VPS`, then `http://127.0.0.1:8080/admin`. Lost phone: `remove` its device name.

### Check the server: the doctor

```bash
sudo ftnbilling-doctor        # PASS / WARN / FAIL for everything; exit status 1 if anything FAILs
sudo ftnbilling-doctor -q     # only warnings and failures
```

It checks every program the installer put in, every service, timer and port, PostgreSQL (reachable, the app role connects, migrations current or `db-check`), `wg0` and `wg-admin` up with at least one admin device, Caddy, the app's `/healthz` (including `db_ok`), the dump, restore-drill and Drive backup timers and their last results, whether Google Drive is set up, the ufw rules, disk, RAM, swap and clock sync. The installer runs it at the end. A fresh install normally shows one warning: Drive backup is not set up yet.

### Restore on a new server (manual steps)

1. Run the installer on the new VPS (section above).
2. Connect Drive with your **saved** passphrases and download the newest snapshot: `sudo ftnbilling-backup setup`, then `sudo ftnbilling-backup restore latest` (details in [BACKUP.md](BACKUP.md)).
3. Follow the numbered steps it prints: stop the app, copy the data and settings back (`DATA_KEY` is in the settings), restore both WireGuard files so routers and your admin devices keep working, restore the database with `pg_restore --clean --if-exists --no-owner`, start the app.
4. Point the domain at the new server and run `sudo ftnbilling-doctor`.

To restore from a local dump on the same server instead: `sudo systemctl stop ftnbilling`, then `sudo cat /var/backups/ftnbilling/db/<file>.dump | sudo -u postgres pg_restore --clean --if-exists --no-owner --role=ftnbilling -d ftnbilling`, then `sudo systemctl start ftnbilling`.

## 2. Add a building

1. Open the admin and go to **Buildings, Add building**. Give the name; the router layout defaults suit a standard install (hotspot server `hotspot1`, WAN `ether1`, OLT port `sfp-sfpplus1`).
2. On the building page press **Make the join command**. It is shown once, works once, and expires after 30 minutes.
3. Paste it in the router's terminal (WinBox, New Terminal). The router downloads its setup script, creates its own WireGuard key (the private key never reaches the server), brings the tunnel up, restricts REST to the server, sends NetFlow through the tunnel, installs the hotspot pages for that building and whitelists the VPS in the walled garden.
4. The building page switches to "Connected" when the tunnel is up and the REST API answers.

If the router's clock is wrong or it has no certificate store, the page offers a fallback command with `check-certificate=no`.

The script moves WebFig off port 80 (the hotspot uses it). Keep WinBox or SSH to reach the router.

A building that is "paused" in the admin gets no collection and no router changes, and tenants cannot buy there.

## 3. Check

- `sudo ftnbilling-doctor` checks the whole server.
- `curl -s localhost:8080/healthz` on the VPS shows `sites_total`, `sites_online` and `router_ok` (and `db_ok` on builds that report it).
- `wg show wg0` on the VPS lists one peer per joined building.
- The building page shows the router, the WireGuard handshake and the portal URL.
- A phone on that building's WiFi should open the online portal.

| Symptom | Fix |
|---|---|
| Join command refused | `PUBLIC_BASE_URL` must start with `https://` (set `DOMAIN` and re-run the installer). For a bench test only, `ALLOW_INSECURE_JOIN=true`. |
| Router says 404 for the join link | The command was used, expired or cancelled. Make a new one. A chat or browser preview also fetches links; the server ignores clients that do not identify as MikroTik, so paste into the terminal, not a chat. |
| Tunnel never comes up | UDP 51820 must reach the VPS (`ufw status`, provider firewall). `WG_ENDPOINT` in the env file must be the VPS public address. Check `wg show wg0`. |
| Admin VPN connects but the page does not open | UDP 51821 must reach the VPS (provider firewall too). The tunnel must be on, and the address is `http://10.98.0.1:8080/admin` (plain http, only inside the tunnel). `wg show wg-admin` should list your device with a recent handshake; `sudo ftnbilling-admin-peer list`. |
| Tunnel up but the router is "not reachable" | The router firewall must accept `10.99.0.1` to port 8081 (the join script adds this). Re-run the join command to rebuild it. |
| Phone shows an error instead of the portal | Check `/ip/hotspot/walled-garden/ip/print` on the router and that the domain resolves. |
| Portal says "open this page from the WiFi login" | The router's host table must list the phone: `/ip/hotspot/host/print`. |
| No traffic charts | Check `/ip/traffic-flow/target/print` (should be `10.99.0.1:2055`). The admin Settings page counts NetFlow packets from unknown sources. |

## 4. Alerts, Telegram, email and WhatsApp

The server monitors every building from the moment it starts (Admin, Fleet overview). To be told when something breaks, add the settings of the channels you want to the environment file the installer wrote (`/etc/ftnbilling/ftnbilling.env`) and restart: `systemctl restart ftnbilling`. Each channel is optional.

```
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_USER=alerts@example.com
SMTP_PASS=...
SMTP_FROM=Fast Tech Net <alerts@example.com>
ALERT_EMAIL_TO=you@example.com
TELEGRAM_BOT_TOKEN=123456:ABC...
TELEGRAM_CHAT_ID=-1001234567890
WA_TOKEN=...
WA_PHONE_NUMBER_ID=...
WA_ADMIN_NUMBERS=254712345678
```

Then open Admin, Alerts, and press **Send test** next to each channel. The thresholds, hold times, cooldown, repeat and mute are edited on that page (defaults and meaning: see "Monitoring, alerts and WhatsApp" in the main README). `/var/lib/ftnbilling` keeps the state in `fleet.json`, `alerts.json` and `outbox.json`; the daily backup includes them.

If a provider rejects the credentials (an expired WhatsApp token, a changed SMTP password) the channel pauses, messages stay queued, and the Messages page shows a banner. Fix the setting, restart, and press the resume button.

### WhatsApp: Meta Business Manager checklist

**Meta Business Manager checklist**

1. A Meta Business portfolio and a WhatsApp Business Account (WABA).
2. A dedicated phone number registered to the Cloud API. It cannot also be in use in the WhatsApp app or WhatsApp Business app.
3. A System User with a permanent token that has the `whatsapp_business_messaging` and `whatsapp_business_management` permissions. Put the token in `WA_TOKEN`, and the phone number's ID (not the number) in `WA_PHONE_NUMBER_ID`.
4. A payment method on the WABA.
5. The tenants' consent to receive messages. Collect it when you take the flat's details, and keep a record.
6. Create the six templates below, category **Utility**, language **English (`en`)**, and wait for approval. The wording and the number of parameters must match exactly, or Meta refuses the message (error 132000 or 132001, shown on the Messages page as failed).
7. The five tenant templates carry the footer **Reply STOP to opt out**.
8. Put your own number(s) in `WA_ADMIN_NUMBERS` and press **Send test** next to WhatsApp on the Alerts page: it sends `ftn_admin_alert`.

Templates to create (`{{1}}` and so on are the parameters; the wording and the number of parameters must match exactly):

| Name | Body | Parameters |
|---|---|---|
| `ftn_expiry_reminder` | Hello, your {{1}} internet plan for {{2}} ends on {{3}}. To stay connected, pay KES {{4}} via {{5}}, account number {{6}}. Thank you. | package, flat + building, expiry, price, pay hint, account code |
| `ftn_plan_expired` | Hello, the internet plan for {{1}} ended on {{2}}. To reconnect, pay via {{3}}, account number {{4}}. You will be reconnected automatically. | flat + building, expiry, pay hint, code |
| `ftn_payment_received` | Payment received: KES {{1}}, reference {{2}}. Your {{3}} plan for {{4}} is active until {{5}}. Thank you. | amount, receipt, package, flat + building, expiry |
| `ftn_outage_notice` | Hello, we are aware of an internet outage at {{1}} since {{2}}. Our team is working on it. We apologise for the inconvenience. | building, start |
| `ftn_outage_resolved` | Internet service at {{1}} was restored at {{2}}. Thank you for your patience. | building, time |
| `ftn_admin_alert` | Network alert, {{1}}: {{2}} at {{3}}. Details: {{4}}. Time: {{5}}. End of alert. | PROBLEM/RECOVERED, rule, building, detail, time |

A tenant only gets messages if the account has a WhatsApp number (Accounts, the flat's page; "same as phone" copies the phone number) and has not been opted out. Messages go out between 07:00 and 21:00 EAT for reminders; payment confirmations and outage notices go at once. At most `WA_DAILY_LIMIT` (240) tenant messages are sent per day; admin alerts are not counted. Failed sends retry with growing waits (30 s up to 1 h, 8 tries). A rejected token pauses the channel and shows a banner on the Messages page.

Optional inbound replies (BALANCE, STATUS, STOP, START): set `WA_APP_SECRET` and `WA_VERIFY_TOKEN`, set the Meta app's webhook to `https://YOUR-DOMAIN/wa/webhook` with the same verify token, and subscribe to `messages`. The webhook is the only part of this server besides the portal and the payment callbacks that is reachable from the internet; it refuses anything without a valid `X-Hub-Signature-256`.

## Legacy: one router with the app in a container, or ROUTER_URL

An install that already has `ROUTER_URL`/`ROUTER_PASS` in its settings keeps
working with no change: its data becomes a building called "Main building"
(legacy mode, configured by the environment). The old files `routeros/setup.rsc`
and `internal/web/routerfiles/remote.rsc` (the old router-container setup) are
legacy; new buildings use the join command instead.

To move such a building onto WireGuard, add a new building, run its join
command on that router, then move the accounts to it (account page, Move to
another building) and remove the old one.

## Moving data between servers

Stop both apps, copy `/var/lib/ftnbilling/ispbill.json` (and `metrics/` if you
want the history), `chown ftnbilling:` it and start the service.
