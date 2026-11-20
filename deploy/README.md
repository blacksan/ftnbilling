# Online billing server for many routers (VPS + WireGuard)

One VPS runs billing, the payment page and the admin pages for every router.
Each router is a MikroTik serving its houses over IPoE (one fixed address and one speed queue per house). Routers join a
private WireGuard network at the VPS; the VPS reaches each router's REST API
through its tunnel address.

```
 Router A router ──┐                                 ┌─ ftnbilling 127.0.0.1:8080 (behind Caddy only)
 Router B router ──┼── WireGuard udp/51820 ──► VPS ──┤  Caddy :80/:443  (/pay, /admin, /api, M-Pesa callbacks)
 Router C router ──┘   10.99.0.0/22, wg0             ├─ NetFlow udp/2055 on 10.99.0.1 (tunnel only)
                                                       └─ PostgreSQL (local socket only, no TCP)
 you, phones, the operator app ── https://DOMAIN/admin, /api, /pay ──► Caddy
```

| Path | How |
|---|---|
| VPS to router REST API | Over the tunnel, to the router's tunnel address (`10.99.0.x:8081`). The router allows REST only from `10.99.0.1`. |
| Tenants to the payment page | Public internet, `https://DOMAIN/pay`. A house that has not paid is walled by the router: it can reach only the VPS payment page. |
| Router to NetFlow | Through the tunnel, to `10.99.0.1:2055`. The server tells routers apart by the tunnel source address. |
| You to admin | Public, over HTTPS: `https://DOMAIN/admin` (password, then an authenticator code) and `https://DOMAIN/api` for the operator app. Caddy proxies both; the app listens on `127.0.0.1:8080` only, and refuses `/admin` and `/api` from the routers' tunnel. There is no admin VPN any more. |

Design size: about 40 routers and 1500 houses on a 1 vCPU / 2 GB VPS. The
service runs with `GOMEMLIMIT=600MiB GOGC=50`.

The payment page is public. A tenant finds their house by account number (or by house number when the visit comes from a known router), and the page never shows a phone number, a MAC or a name.

## 1. New server in one command

You need a brand-new, minimal VPS (Ubuntu 22.04/24.04 or Debian 12, 1 vCPU, 2 GB RAM, 60 GB disk is typical) with a public IPv4 address, and a few minutes. Nothing has to be installed on it first: the installer installs every dependency itself.

Before you start:

1. **Domain:** an A record such as `billing.yourdomain.co.ke` pointing to the VPS. HTTPS is required for M-Pesa and for the router paste line (the setup script carries the router's keys, so it is never served over plain HTTP). You can install without it and re-run later with `DOMAIN=...`.
2. **GitHub token:** the repo is private. Create a fine-grained token for `blacksan/ftnbilling` with **Contents: Read-only**. It is used to fetch the installer and the release (or the source).
3. **Routers:** RouterOS 7.13 or newer. The paste line checks that the fetch feature is on in device mode and, if it is not, tells you to press the router's button; nothing else is typed on the router.

Then, as a normal sudo user on the new VPS:

```bash
GH=github_pat_xxx; curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | sudo GH_TOKEN=$GH DOMAIN=billing.yourdomain.co.ke bash
```

It prints numbered steps (`[7/14] ...`), stops with a clear message if one fails, and is safe to run again. At the end it runs the doctor (below), prints the admin address and, once, a **setup code** for the first sign-in.

To preview without changing anything: `DRY_RUN=1` (works without root and prints every action):

```bash
curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | DRY_RUN=1 bash
```

Options (environment variables): `DOMAIN`, `GH_TOKEN`, `VERSION` (a release tag; default is the latest release), `FROM_SOURCE=1` (build on the server instead of using a release), `NO_POSTGRES=1`, `NO_FIREWALL=1`, `DRY_RUN=1`, `PUBLIC_IP`, `ADMIN_PASS`, `BRAND_NAME`, `SUPPORT_PHONE`, `WG_PORT`, `WG_SUBNET`, `MGMT_PORT` (UDP, default 51821) and `MGMT_SUBNET` (default `10.98.0.0/24`) for remote WinBox, `WEBFIG_DOMAIN` for remote WebFig (below), `SWAP_MB`. The header of `install.sh` lists them.

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
| Packages (apt) | `curl ca-certificates gnupg git tar xz-utils unzip jq ufw qrencode wireguard-tools unattended-upgrades cron iputils-ping traceroute`, plus `chrony` if no time sync is running |
| PostgreSQL | The distro's `postgresql` and `postgresql-client`. No TCP listener (`listen_addresses=''`), tuned for 1 vCPU / 2 GB in `conf.d/90-ftnbilling.conf`, `scram-sha-256` for role `ftnbilling` over the socket, database `ftnbilling`, `REVOKE CONNECT ... FROM PUBLIC`, `OOMScoreAdjust=-500` so Postgres outlives the app under memory pressure. The database password is generated once into the settings file. `NO_POSTGRES=1` skips all of it |
| Caddy | From the official Caddy apt repository; automatic HTTPS for `DOMAIN` |
| rclone | For the Google Drive backup (distro package if 1.60 or newer, otherwise the official installer) |
| The app | `/usr/local/bin/ftnbilling`, systemd service `ftnbilling` (`GOMEMLIMIT=600MiB GOGC=50`, `MemoryMax=900M`, runs after PostgreSQL and `wg0`), data in `/var/lib/ftnbilling`, settings in `/etc/ftnbilling/ftnbilling.env` |
| `wg0` | Routers' tunnel: `10.99.0.1/22`, UDP 51820, MTU 1380. The private key is generated once in `/etc/wireguard/wg0.conf` and never regenerated. `ftnbilling-wg-sync` (root, via a systemd path unit) applies the peer list the app writes after validating it (only `[Peer]`, a 44-character `PublicKey`, one `/32` inside the subnet; no symlinks, no file over 256 KB) and `ftnbilling-wg-status.timer` writes handshake times for the admin page |
| `wg-mgmt` | Remote WinBox: a second, separate WireGuard interface for your own laptop and phone: `10.98.0.1/24` (`MGMT_SUBNET`), UDP 51821 (`MGMT_PORT`), MTU 1380, key and devices in `/etc/wireguard/wg-mgmt.conf`, settings for the tool in `/etc/ftnbilling/mgmt.env`. Devices are added with `ftnbilling-mgmt`. See "Remote WinBox" below |
| Firewall (ufw) | Deny incoming and routed (forwarded) traffic by default. Open: SSH, 80, 443, UDP 51820, UDP 51821. On `wg0` only UDP 2055 to `10.99.0.1`. All other input and all routing from the router tunnel is denied. The public UDP 2055 rule is removed (kept on an old install that still has `ROUTER_URL`). `net.ipv4.ip_forward` is 1 (`/etc/sysctl.d/91-ftnbilling-mgmt.conf`) only for remote WinBox, and a block of rules in `/etc/ufw/before.rules` (between `# BEGIN ftnbilling-mgmt` and `# END ftnbilling-mgmt`, replaced on every run) limits it (and drops everything else forwarded from `wg0`, so routers cannot reach each other or the internet through this server), see below. The installer checks the block with `iptables-restore --test` before it touches the file, refuses a `BEGIN` marker without its `END`, and puts the old file back if `ufw reload` fails |
| Swap and logs | A 1 GB `/swapfile` (`vm.swappiness=10`) when RAM is 2 GB or less and there is no swap; journald `SystemMaxUse=500M` |
| Updates | `unattended-upgrades`, security updates only, never reboots by itself |
| Backups | `ftnbilling-dump.timer` every 6 hours (database dump, daily file tar), `ftnbilling-drill.timer` monthly restore drill, `ftnbilling-backup.timer` weekly Google Drive upload once you run `sudo ftnbilling-backup setup`. See [BACKUP.md](BACKUP.md). The old daily cron job is removed |
| Tools | `/usr/local/sbin/`: `ftnbilling-admin-reset`, `ftnbilling-wipe`, `ftnbilling-doctor`, `ftnbilling-backup`, `ftnbilling-wg-sync`, `ftnbilling-mgmt` |

`DATA_KEY` (encrypts national IDs and the secrets saved in Settings) is generated once into the settings file on every install, with or without PostgreSQL. The `MPESA_*`, `WA_*` and `SMTP_*` lines are written commented out: set these up in Admin > Settings instead.

**Upgrading:** run the same line again (or run `./install.sh` from a newer bundle). Data, settings, the database password, `DATA_KEY` and the WireGuard keys are kept. An install that was on the JSON file moves to PostgreSQL: the installer first saves `ispbill.json` to `/var/backups/ftnbilling/pre-postgres-<date>.tgz`, and the next start imports it automatically (the old file is renamed `ispbill.json.imported-<date>`). A v2 data file is converted to the new format on first start; the old file is kept as `ispbill.json.v2.bak`.

### Upgrading a server that still has the admin VPN

Run the installer as above. On such a server it does this, in order, and every step is safe to repeat:

1. Takes a database dump before anything changes and keeps a copy as `/var/backups/ftnbilling/pre-upgrade-<time>.dump`. If the dump fails, it stops.
2. Moves `/etc/wireguard/wg-admin.conf` and `/root/ftnbilling-admin-*.conf` into `/root/ftnbilling-removed-<date>/` (mode 700; delete the folder when you are sure), then runs `systemctl disable --now wg-quick@wg-admin` and removes `/usr/local/sbin/ftnbilling-admin-peer`.
3. Sets `LISTEN_ADDR=127.0.0.1:8080`, rewrites the Caddyfile (`/admin` and `/api` proxied, plain-HTTP catch-all to `https://DOMAIN/pay`) and removes the firewall rules for UDP 51821 and every rule `on wg-admin`.
4. Runs the database migrations, restarts the service, and prints the report of houses that share a MAC address (they are not provisioned until fixed).
5. Prints a one-time admin setup code (needs a program built with the new sign-in; an older program just skips this with a note).

`wg0`, the router peers and their keys are not touched.

### Open the admin

1. Open `https://DOMAIN/admin`.
2. Sign in as `admin` with the admin password and the **setup code** the installer printed (a fresh install prints the password too; an upgrade keeps yours).
3. Scan the QR code with an authenticator app and store the recovery codes offline. From then on you sign in with the password and the 6-digit code.

Lost the code, the phone or the password? On the server:

```bash
sudo ftnbilling-admin-reset --setup-code        # a new one-time setup code
sudo ftnbilling-admin-reset --reset-totp        # forget the authenticator and recovery codes
sudo ftnbilling-admin-reset --generate          # a new random password, printed once
sudo ftnbilling-admin-reset --revoke-sessions   # sign everything out
```

### Start fresh (delete all data)

`sudo ftnbilling-wipe` lists what it would delete, asks you to type `WIPE` (or pass `--yes`), saves a backup to `/var/backups/ftnbilling/pre-wipe-<UTC time>.dump` (`.json` on the JSON file) and aborts if that fails, then stops the service, deletes every customer, house, router, payment and log, starts it again and removes the old routers from `wg0`.
Admin users, Settings (M-Pesa, branding) and `DATA_KEY` are kept; everybody signs in again, and you add the routers again with the one-line bootstrap.

### Check the server: the doctor

```bash
sudo ftnbilling-doctor        # PASS / WARN / FAIL for everything; exit status 1 if anything FAILs
sudo ftnbilling-doctor -q     # only warnings and failures
```

It checks every program the installer put in, every service, timer and port, PostgreSQL (reachable, the app role connects, migrations current or `db-check`), `wg0` and `wg-mgmt` up (a warning, not a failure, on an install from before remote WinBox), `ip_forward` and the remote-WinBox forwarding rules, the public `/admin/login` page (200 over https), no retired admin VPN on UDP 51821, the app bound to loopback, houses that share a MAC address, the schema version, Caddy, the app's `/healthz` (including `db_ok`), the dump, restore-drill and Drive backup timers and their last results, whether Google Drive is set up, the ufw rules, disk, RAM, swap and clock sync. The installer runs it at the end. A fresh install normally shows one warning: Drive backup is not set up yet.

### Restore on a new server (manual steps)

1. Run the installer on the new VPS (section above).
2. Connect Drive with your **saved** passphrases and download the newest snapshot: `sudo ftnbilling-backup setup`, then `sudo ftnbilling-backup restore latest` (details in [BACKUP.md](BACKUP.md)).
3. Follow the numbered steps it prints: stop the app, copy the data and settings back (`DATA_KEY` is in the settings and must come back with the database: it decrypts the national IDs and the secrets saved in Settings), restore both WireGuard files so routers and your admin devices keep working, restore the database with `pg_restore --clean --if-exists --no-owner`, start the app.
4. Point the domain at the new server and run `sudo ftnbilling-doctor`.

To restore from a local dump on the same server instead: `sudo systemctl stop ftnbilling`, then `sudo cat /var/backups/ftnbilling/db/<file>.dump | sudo -u postgres pg_restore --clean --if-exists --no-owner --role=ftnbilling -d ftnbilling`, then `sudo systemctl start ftnbilling`.

## 2. Add a router

1. Open the admin and go to **Routers, Add router**. Give the name; the router layout defaults suit a standard install (WAN `ether1`, OLT port `sfp-sfpplus1`). Change the WAN mode (dhcp, static or pppoe), the admin LAN ports, the VLAN, the customer subnet or the bench port there if this router differs. A PPPoE password is write-only: the page shows that one is set, never the password.
2. The dialog then shows the **one line** to paste. It is shown once, works once, and expires after 30 minutes. (On the router page, Setup, **Make the line** makes a new one.)
3. Paste it in the router's terminal (WinBox, New Terminal). The router fetches its setup script, **erases itself and builds itself again** (WAN, admin LAN, customer network, DHCP, firewall, WireGuard tunnel, the API login), and reports back. The server generates the router's WireGuard private key when the router fetches the script, serves it once inside that script and does not store it (it keeps only the public key); the script deletes itself from the router. Nothing else is typed on the router, and the server never changes its users, services, tunnel, routes or WAN afterwards.
4. The router page, Setup, shows four steps that refresh by themselves: the router fetched the script, the tunnel is up, the server can log in, the checks are green. When all four are done the server gives every house its address and queue.

If the line fails with a certificate error, correct the router's clock; the page also offers the same line without the certificate check for a router that cannot have a right clock.

To rebuild a router, press **Reset router** and paste the new line (the line erases the router first; running the set-up script a second time on a built router is refused). The router is never open while it builds: before any WAN connection exists the script puts a firewall hold at the top of the filter that drops new and invalid traffic from the WAN, the WAN connection is created switched off and is switched on only after the full rule set is in, and the hold is removed last. If the script stops on an error the hold stays and the WAN is switched on behind it so the failure report can reach the server. **Reset router** on the Setup tab (owner only, the router name typed to confirm) makes the server forget the router's keys and join state, so a new line can be made. It does not touch the router.

A router that is "paused" in the admin gets no collection and no router changes, and tenants cannot buy there.

## Remote WebFig

Open a router's WebFig (its own web page) from the admin, through this server, from anywhere on the internet. The router's web page is never opened on its WAN: it still answers only the admin LAN and this server's tunnel address, and the server reaches it over the WireGuard tunnel.

### Turn it on

1. DNS: an `A` record for the WebFig host name to this server's public IP, like the one for `DOMAIN`. For this server: `tech.ftw.co.ke` -> `162.35.47.106` (the same address as `isp.ftw.co.ke`). The owner created this record; the installer does not check it, `sudo ftnbilling-doctor` does afterwards.
2. Run the installer again with the setting (the same line you installed with, plus `WEBFIG_DOMAIN`):

```bash
GH=github_pat_xxx; curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | sudo GH_TOKEN=$GH DOMAIN=isp.ftw.co.ke WEBFIG_DOMAIN=tech.ftw.co.ke bash
```

It writes `WEBFIG_DOMAIN` and `WEBFIG_LISTEN=127.0.0.1:8082` in `/etc/ftnbilling/ftnbilling.env`, adds a second Caddy site for the WebFig host with its own certificate (issued on first use; the DNS record must already exist), and restarts the app, which then listens on `127.0.0.1:8082` for it. Nothing else changes: ufw is untouched (443 is already open; 8082 is loopback only), and a server without `WEBFIG_DOMAIN` gets exactly the files it had before. Leaving the setting out on a later run keeps it; `WEBFIG_DOMAIN=off` switches the feature off again. `WEBFIG_DOMAIN` must be a host name different from `DOMAIN`.

3. Check: `sudo ftnbilling-doctor` has a "Remote WebFig" section (listener on loopback, Caddy site, DNS points here, `https://<WEBFIG_DOMAIN>/_ftn/open` answers 403 without a ticket).

Router side: nothing to do. The bootstrap line (first provisioning and every rebuild) already leaves the `www` service on the router's API port, restricted to the admin LAN and the server's tunnel address, `www-ssl` off, and the firewall accepts that port from the server's tunnel address only. The server never writes `/ip/service` over the API. A router built before this restriction needs a new line (Setup, Reset router) before the server can reach its web page. So WebFig is available for every router from its first provisioning: once the server has the setting, each router that has a tunnel gets the button.

### Use it

Admin, Routers, the router, Overview: **Open WebFig** (owners only; the Android app has the same button on the router page and opens the system browser). The first time in ten minutes it asks for your password and the 6-digit authenticator code, then sends the browser to `https://<WEBFIG_DOMAIN>/` where the router's own WebFig login appears; sign in there with a router account (for example `admin`). The billing API user cannot sign in to WebFig. The router page lists open WebFig sessions with an **End** button and the recent activity.

### Security model

- Owners only. Operators and viewers get 403 on every WebFig page. The browser needs an admin session, and a fresh step-up: password and authenticator code checked again within the last 10 minutes (same attempt limits and back-off as sign-in; a step-up belongs to that one admin session).
- The admin mints a one-time ticket for 60 seconds, bound to the admin user, the router and the client address. It is exchanged on the WebFig host for a session cookie that is host-only (`__Host-` prefix: no `Domain`, `Secure`, `Path=/`), `HttpOnly`, `SameSite=Strict`, bound to exactly one router, 30 minutes idle and 2 hours at most. Opening WebFig again in the same browser ends the earlier session. An owner can end a session from the router page; a session also ends if its user stops being an owner or is disabled, or the router is removed.
- The WebFig host is a different origin from the admin. The admin's cookies are host-only too, so the browser never sends them to the WebFig host and the proxy removes any `ftn_` or `__Host-ftn_` cookie before it talks to the router. No API token or admin cookie is ever forwarded. The admin refuses state-changing requests that say they come from another origin (including the WebFig host, which is "same-site" but not "same-origin"), and needs its CSRF token.
- Only the session's router can be reached, only at its tunnel address and web port. The request target is always rebuilt: absolute-form targets, `CONNECT`, `//` paths, `..` and encoded dot-dot, backslashes and any `Host` other than `WEBFIG_DOMAIN` are refused, and the connection itself refuses any address but that one. Redirects (`Location`) are rewritten to the WebFig host; redirects to anywhere else become `/`. Cookies the router sets lose their `Domain` and are marked `Secure`. `X-Forwarded-*`, `Forwarded`, `Origin` and `Referer` are not passed to the router.
- Proxied responses carry `X-Frame-Options: DENY`, `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. The router's own scripts run on the WebFig host (WebFig needs them), which is why it has an origin of its own.
- A WebFig session is tied to the admin session that opened it: signing out, revoking sessions, a password or second-factor reset, or that session reaching its end closes the WebFig session too (checked on every request, and recorded at once).
- Every router is served from the same WebFig origin, so the host takes extra care: requests with a `Service-Worker` header are refused, `Service-Worker-Allowed` and `Refresh` are removed from responses, the router's `/rest` API is refused, and the open response (and the page shown after a session ended) sends `Clear-Site-Data: "cache", "storage"` so what one router left in the browser (workers, caches, storage) is wiped before the next session. Limit: a tab of router A that is still open when you open router B keeps running and its requests then go to B, because the browser sends the one cookie. Close the old WebFig tab before opening another router. A real fix is one host name per router (a wildcard certificate); not done.
- Treat the WebFig host as hostile to the admin: it is same-site with the admin (the admin's SameSite=Strict cookies go with its requests). The admin has no state-changing GET (a test walks every admin GET route and checks nothing is stored), POSTs need the CSRF token and a same-origin request, and the public pay page's double-submit cookie is `__Host-ftn_pay_csrf` over https. A different registrable domain for `WEBFIG_DOMAIN` (not under the admin's) would remove the same-site relation altogether.
- Ticket redemption: the ticket is bound to the client address. A phone whose address differs between the two names (CGNAT, IPv6 on one name only) gets "Your network changed between the admin and WebFig"; the doctor warns when the admin and WebFig names do not resolve alike.
- At most 4 requests at once per router and 16 overall (WebFig's long requests hold one each; the admin's own REST budget is separate). More get a 503 and "Retry-After".
- Activity: every open and every end (idle, 2 hour limit, replaced, ended by an owner) is written to the audit log against the router with who, when and the client IP, and shown on the router page. Sessions live in the server's memory: a restart ends them (an end then has no record; the open does).
- Behind Caddy only. The client address the ticket is bound to is the one Caddy reports, so a phone that changes network between the admin and the WebFig host must open it again.

## Remote WinBox

Open WinBox on a router from your laptop or phone, from anywhere, through this server:

1. On the server: `sudo ftnbilling-mgmt add <your-device>` (a-z, 0-9 and dash). It prints the device's WireGuard profile once, with a QR code. The server does not keep the device's private key.
2. On the device, import the profile in the WireGuard app (phone: scan the QR code; laptop: save the text as a `.conf` and import it) and connect.
3. In WinBox, connect to the router's **tunnel address** (shown on the router page in the admin, and as `tunnel_address` in the API, for example `10.99.0.2`) and sign in with the router's own user and password.
4. `sudo ftnbilling-mgmt list` shows every device and its last handshake. When a laptop or phone is lost: `sudo ftnbilling-mgmt remove <your-device>`.

How it is kept narrow: the profile routes only the routers' tunnel range (`WG_SUBNET`, never the whole internet) through the VPN. The server lets a device through to the routers on WinBox (TCP 8291) and ping only; the device cannot reach this server's own services, other networks or the internet, and a router cannot open a connection back to a device. There is no NAT: the router sees the device's own `10.98.0.x` address. A router built by the script accepts WinBox from `MGMT_SUBNET` through its tunnel (MAC-WinBox stays on the LAN). The router page and the API (`mgmt_winbox`) show the connection line only for a router whose own join report says it was built with the current `MGMT_SUBNET`; any other router shows "Built without remote WinBox: reset the router and paste a new line to add it."

**Do not run Docker on this server.** Docker publishes container ports by its own iptables rules, which bypass ufw's forward rules and would open a path the rules above are meant to close. If you change `MGMT_PORT`, the installer removes the old port's ufw rule; with `NO_FIREWALL=1` remote WinBox is skipped and the forwarding it switched on earlier is switched off again.

**Routers built before this feature** have no such rule, and the server never changes a router's services or firewall after the bootstrap. Reset each one (Setup, **Reset router**) and paste the new line once; the rebuilt router has it. Until then WinBox works as before on its LAN. Changing `MGMT_SUBNET` later likewise needs every device re-added and every router rebuilt.

## 3. Check

- `sudo ftnbilling-doctor` checks the whole server.
- `curl -s localhost:8080/healthz` on the VPS shows `sites_total`, `sites_online` and `router_ok` (and `db_ok` on builds that report it).
- `wg show wg0` on the VPS lists one peer per joined router.
- The router page shows the router, the WireGuard handshake and the payment page address.
- A house that has not paid, on that router, should reach the payment page and nothing else.

| Symptom | Fix |
|---|---|
| Paste line refused | `PUBLIC_BASE_URL` must start with `https://` (set `DOMAIN` and re-run the installer). For a bench test only, `ALLOW_INSECURE_JOIN=true`. |
| Router says 404 for the line | The line was used, expired or cancelled. Make a new one. A chat or browser preview also fetches links; the server ignores clients that do not identify as MikroTik, so paste into the terminal, not a chat. |
| Tunnel never comes up | UDP 51820 must reach the VPS (`ufw status`, provider firewall). `WG_ENDPOINT` in the env file must be the VPS public address. Check `wg show wg0`. |
| `/admin` does not open | `sudo ftnbilling-doctor` shows which part fails (Caddy, the app, DNS). The app must answer on `127.0.0.1:8080`; `curl -I https://DOMAIN/admin/login` should say 200. Locked out: `sudo ftnbilling-admin-reset --setup-code`. |
| Tunnel up but the router is "not reachable" | The router firewall must accept `10.99.0.1` to port 8081 (the setup script adds this). Paste a new line to rebuild it. |
| A paused house sees an error instead of the payment page | The house must be in the router's `ftn-suspended` address list, and the router must be able to reach `https://DOMAIN/pay`. The router page, Setup, checks list what is wrong; paste a new line if the router drifted. |
| A paying tenant is still walled | Admin, Routers, the router page: Clients shows "On router" for the house. A house is un-walled by the server within about a minute of the payment; if the router is down the change is queued and retried. |
| No traffic charts | Check `/ip/traffic-flow/target/print` (should be `10.99.0.1:2055`). The admin Settings page counts NetFlow packets from unknown sources. |

## 4. Alerts, email and WhatsApp

The server monitors every router from the moment it starts (Admin, Fleet overview). To be told when something breaks, set up the channels you want in the admin: **Settings > Email** and **Settings > WhatsApp**. Each channel is optional, and what you save applies at once (no restart). The environment file (`/etc/ftnbilling/ftnbilling.env`) can still hold `SMTP_*`, `ALERT_EMAIL_TO`, `WA_TOKEN`, `WA_PHONE_NUMBER_ID`, `WA_ADMIN_NUMBERS` and the `MPESA_*` values as a fallback; a value saved in the admin wins over it.

Secrets saved in the admin are encrypted with `DATA_KEY` from the environment file. The installer generates it on every install. Restore it together with the database, or the saved secrets cannot be read.

Then press **Send test** on each settings page, and on the Alerts page. The thresholds, hold times, cooldown, repeat and mute are edited on the Alerts page (defaults and meaning: see "Monitoring, alerts and WhatsApp" in the main README). `/var/lib/ftnbilling` keeps the state in `fleet.json`, `alerts.json` and `outbox.json`; the daily backup includes them. A failed or stale backup also raises a "Backup problem" alert.

If a provider rejects the credentials (an expired WhatsApp token, a changed SMTP password) the channel pauses, messages stay queued, and the Messages page shows a banner. Fix the setting and press the resume button.

Locked out of the admin: `sudo ftnbilling-admin-reset` (or `--generate`). `sudo ftnbilling-doctor` reports whether the password is in the database or the environment file, and which settings sections are saved in the admin.

### WhatsApp: Meta Business Manager checklist

**Meta Business Manager checklist**

1. A Meta Business portfolio and a WhatsApp Business Account (WABA).
2. A dedicated phone number registered to the Cloud API. It cannot also be in use in the WhatsApp app or WhatsApp Business app.
3. A System User with a permanent token that has the `whatsapp_business_messaging` and `whatsapp_business_management` permissions. Put the token in Settings > WhatsApp, and the phone number's ID (not the number) in the phone number ID field.
4. A payment method on the WABA.
5. The tenants' consent to receive messages. Collect it when you take the house's details, and keep a record.
6. Create the six templates below, category **Utility**, language **English (`en`)**, and wait for approval. The wording and the number of parameters must match exactly, or Meta refuses the message (error 132000 or 132001, shown on the Messages page as failed).
7. The five tenant templates carry the footer **Reply STOP to opt out**.
8. Put your own number(s) in the admin numbers field of Settings > WhatsApp, save, and press **Send test** there.

Templates to create (`{{1}}` and so on are the parameters; the wording and the number of parameters must match exactly):

| Name | Body | Parameters |
|---|---|---|
| `ftn_expiry_reminder` | Hello, your {{1}} internet plan for {{2}} ends on {{3}}. To stay connected, pay KES {{4}} via {{5}}, account number {{6}}. Thank you. | package, house number + router, expiry, price, pay hint, account code |
| `ftn_plan_expired` | Hello, the internet plan for {{1}} ended on {{2}}. To reconnect, pay via {{3}}, account number {{4}}. You will be reconnected automatically. | house number + router, expiry, pay hint, code |
| `ftn_payment_received` | Payment received: KES {{1}}, reference {{2}}. Your {{3}} plan for {{4}} is active until {{5}}. Thank you. | amount, receipt, package, house number + router, expiry |
| `ftn_outage_notice` | Hello, we are aware of an internet outage at {{1}} since {{2}}. Our team is working on it. We apologise for the inconvenience. | router, start |
| `ftn_outage_resolved` | Internet service at {{1}} was restored at {{2}}. Thank you for your patience. | router, time |
| `ftn_admin_alert` | Network alert, {{1}}: {{2}} at {{3}}. Details: {{4}}. Time: {{5}}. End of alert. | PROBLEM/RECOVERED, rule, router, detail, time |

A tenant only gets messages if the account has a WhatsApp number (Accounts, the house's page; "same as phone" copies the phone number) and has not been opted out. Messages go out between 07:00 and 21:00 EAT for reminders; payment confirmations and outage notices go at once. At most `WA_DAILY_LIMIT` (240) tenant messages are sent per day; admin alerts are not counted. Failed sends retry with growing waits (30 s up to 1 h, 8 tries). A rejected token pauses the channel and shows a banner on the Messages page.

Optional inbound replies (BALANCE, STATUS, STOP, START): set `WA_APP_SECRET` and `WA_VERIFY_TOKEN`, set the Meta app's webhook to `https://YOUR-DOMAIN/wa/webhook` with the same verify token, and subscribe to `messages`. The webhook is the only part of this server besides the portal and the payment callbacks that is reachable from the internet; it refuses anything without a valid `X-Hub-Signature-256`.

## Upgrading a server that still has a ROUTER_URL router

`ROUTER_URL`, `ROUTER_PASS` and the other single-router settings are gone: a server no longer makes a "Main building" router from the environment. A router that was only configured that way is listed as not set up. Add it with **Routers, Add router** (or **Make the line** on its page), paste the line on it (this rebuilds it), then move its accounts to it (account page, Move to another router).

## Moving data between servers

Stop both apps, copy `/var/lib/ftnbilling/ispbill.json` (and `metrics/` if you
want the history), `chown ftnbilling:` it and start the service.
