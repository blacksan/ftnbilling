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

Options (environment variables): `DOMAIN`, `GH_TOKEN`, `VERSION` (a release tag; default is the latest release), `FROM_SOURCE=1` (build on the server instead of using a release), `NO_POSTGRES=1`, `NO_FIREWALL=1`, `DRY_RUN=1`, `PUBLIC_IP`, `ADMIN_PASS`, `BRAND_NAME`, `SUPPORT_PHONE`, `SUPPORT_EMAIL`, `SUPPORT_WHATSAPP`, `WG_PORT`, `WG_SUBNET`, `MGMT_PORT` (UDP, default 51821) and `MGMT_SUBNET` (default `10.98.0.0/24`) for remote WinBox, `WEBFIG_DOMAIN` for remote WebFig (below), `MAC_VENDOR_LOOKUP` and `MAC_VENDOR_DAILY` (below), `SWAP_MB`. The header of `install.sh` lists them.

### Network map

**Admin > Network** draws every apartment that has a GPS point on a map (MapLibre GL JS with [OpenFreeMap](https://openfreemap.org/) tiles, the calm "Positron" style); hovering a marker shows a summary and clicking it opens the equipment installed there. **There is nothing to set up: no API key, no account, no billing.** The page shows the map as soon as one apartment has a GPS point; `?view=list` (the List tab) is the same data as cards, with a "View on Google Maps" link per apartment that opens that point in Google Maps' own app or site.

What it loads, so you can allow it on a locked-down network: the map library is served by this server (`/static/vendor/maplibre/`, version and checksums in its `README.md`); the browser fetches the style, tiles, fonts and icons from `https://tiles.openfreemap.org` only. The Network page (and only that page) widens the Content-Security-Policy by exactly that host (`connect-src` and `img-src`) and `worker-src 'self'`; it needs no `unsafe-eval` and no other host, and every other page is unchanged. If the browser cannot reach that host, the page says so and offers the list. OpenFreeMap is a free community service, so keep the map for admins (a handful of page views, not a public embed) and consider [sponsoring it](https://openfreemap.org/).

An older install may still have `GOOGLE_MAPS_API_KEY` in its env file or a Maps value saved in Settings: both are ignored now and can be deleted (the Settings > Maps page is gone).

### Support contacts on the captive pages

Every page a walled device sees (pay, waiting, receipt, registration) ends with the contacts: a call link, a "WhatsApp us" button (`https://wa.me/<digits>`) and an email link. They come from `SUPPORT_PHONE` (default `0100133133`; a router's own support phone, set in Routers, wins for its building), `SUPPORT_EMAIL` (default `support@ftw.co.ke`) and `SUPPORT_WHATSAPP` (default `254100133133`, international digits; `0100133133` is accepted and turned into `254100133133`). An empty or `none` setting hides that contact. The installer writes the values you pass into `/etc/ftnbilling/ftnbilling.env`; **Settings > Support contacts** edits them later without a restart and wins over the file.

### Self-registration on the captive page

A visitor on a walled device can register at `/pay/register`: full name, phone, apartment (only the apartments the visitor's router serves), house number, an optional email and a tick for the privacy notice (Kenya Data Protection Act, 2019). The request waits as *pending* in **New devices** (and counts on the Overview); the owner picks the device and approves it, which creates the account, or rejects it, which erases the details. The device has no internet until then.

The server cannot see a walled device's MAC: its HTTP is sent to the server through the router's WAN with masquerade, so the server sees the router's address only. A registration is therefore bound to the *building* (the router's WireGuard peer address), and the owner chooses the device when approving; the form is never trusted for a MAC. When approving, the owner sees the router's unknown devices with host name, vendor, port, first-seen and last-seen, the one first seen closest to the request first, and the optional "device name" the visitor typed (devices whose DHCP host name looks like it are highlighted). One is preselected only when exactly one device was first seen within 10 minutes of the request, and the owner still confirms it. Showing the device to the server would need the walled HTTP to go through the tunnel without masquerade (the customer subnet in the peer's AllowedIPs, impossible when two buildings share a subnet) or a conntrack read per page view; neither is done.

**Abuse limits.** The form page hands each browser a signed, HttpOnly, SameSite=Lax token (cookie `ftn_reg`, path `/pay`). Counted only once the form has validated (a typing mistake never uses a try): 5 registrations per hour per browser, 3 per hour per phone and browser, 10 per hour per phone from anywhere. Per address: 200 submissions per 10 minutes for a recognised router address (so one tenant cannot lock the building out; the browser and phone limits do the per-person limiting), 4 for an unknown address; new tokens are limited per address too. At most `max(25, 2 x houses)` requests may wait per router (and 25 for requests that name no router); the owner has **Reject all** per router (web, API `POST /api/v1/registrations/reject-all`, Android). Requests not decided within 30 days, and requests of a removed router, are deleted by the housekeeping sweep; an approval that was interrupted (no end within 5 minutes) is pending again. The honeypot field is `ftn_hp_url`; when it is filled the visitor sees the thank-you page, nothing is stored and only a counter is logged.

**Recognised router address, carrier-grade NAT.** The form identifies the router by the address the request comes from: the public address of the router's WireGuard peer (the client address is the TCP peer, or the last `X-Forwarded-For` entry set by Caddy, so a visitor cannot choose it). This is only an address. (1) Anyone who reaches the server from that same public address is "recognised", which on a carrier-grade NAT can include strangers on the same ISP address; the browser, phone and pending-cap limits and **Reject all** bound what they can do, and only a one-time code by SMS or WhatsApp would remove it. (2) When two routers' peers have the same public address, they cannot be told apart: the form then shows no apartment names and asks for a free-text "name of your apartment", and the request is attached to no router until the owner assigns a router and an apartment (**New devices**, Assign; API `POST /api/v1/registrations/{id}/assign`). Router NAT and tunnel settings are not changed for this.

An approval links the existing customer with the same phone number instead of making a duplicate, unless that customer has an open account in another apartment (then a new customer record is made).

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
3. Plug into the admin port (`ether2`) first: every other port is switched off while the router builds itself, so a WinBox session over any other port is lost. Then paste it in the router's terminal (WinBox, New Terminal). The router fetches its setup script, **erases itself and builds itself again** (WAN, admin LAN, customer network, DHCP, firewall, WireGuard tunnel, the API login), and reports back. The server generates the router's WireGuard private key when the router fetches the script, serves it once inside that script and does not store it (it keeps only the public key); the script deletes itself from the router. Nothing else is typed on the router, and the server never changes its users, services, tunnel, routes or WAN afterwards.

**Ports are off until you enable them.** When the setup script finishes, only the uplink (`ether1`) and the admin port (`ether2`) are up. Every other ethernet and SFP port, the OLT port and the customer ports included, is switched off, so a cable plugged into a spare port gets nothing. Enable each port you use from the **Ports** tab (web or Android): the server switches it on at the router and remembers that you enabled it. The sweep never switches on a port you did not enable, and it switches off any port that is on at the router but that you did not enable (an unused port someone switched on by hand stays off). The OLT port cannot be switched off from the tab ("give the OLT role to another port first") but it does start off, so enable it before the customers can connect. The uplink and the admin port are never switched off by the server.

**The admin port has no internet.** A laptop on `ether2` (192.168.88.0/24 by default) can manage the router (WinBox, WebFig, SSH, DNS, ping) and open the billing site and the WebFig site of the server (tcp 80 and 443 to `DOMAIN`, `WEBFIG_DOMAIN` and `PUBLIC_IP`, held in the address list `ftn-admin-allow`), and nothing else: three firewall rules at the top of the forward chain drop everything else from the admin LAN towards the WAN. The router's own traffic, the WireGuard tunnel, remote WinBox and the customers are not affected. The sweep repairs the three rules, their position and the address list (it follows a change of `DOMAIN`, `WEBFIG_DOMAIN` or `PUBLIC_IP`).

**Upgrading a router that is already in service.** The first sweep after the upgrade records every port that is on at the router and has a role (OLT, customer port) or a link as enabled by you, so nothing in service goes down. Only ports that are on, have no role and no link are switched off. A port with a link and no role stays up and shows "Up with no role - disable it if you don't use it" in the Ports tab. The admin port rules are added by the same sweep. A router that was set up by an older script keeps its old `lan to wan` accept and admin masquerade, which the new rules sit above and make dead.
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

### First fresh-router test (bench checklist)

Not yet run on a real RouterOS 7.25.1 router. On the bench RB5009, once, before a release:

- [ ] Paste the line with the laptop on `ether2`. The terminal does not stop half way and the router reports back.
- [ ] `/interface print`: every ethernet and SFP port except `ether1` and `ether2` is disabled (`:foreach ... disabled=yes` ran), including `sfp-sfpplus1`.
- [ ] `/ip firewall address-list print where list=ftn-admin-allow`: with the WAN down at script time the FQDN adds may fail (the script prints a note); after the first sweep all three entries are there. Check what RouterOS does when a static IP is added while an FQDN entry already resolved to the same IP ("already have such entry" is treated as done).
- [ ] `:typeof [:find $keepUp $n]` gives `"nil"` for a port that is not in the keep-up list (7.25.1 behaviour of the keep-up test).
- [ ] The admin laptop reaches WinBox, WebFig and the billing site, and gets no other internet; a customer port that was enabled in the Ports tab still works.
- [ ] After the sweep, `ether3` and the OLT port stay off until enabled in the Ports tab; enabling the OLT port brings the customers back.
- [ ] A router built with PPPoE on `ether1`: edit nothing, run two sweeps, `ether1` stays up.

## ONU management (TR-069)

The server has a TR-069 ACS built in. It lets the admin web and the Android app change a customer's Wi-Fi name and password and reboot the ONU, and puts the Wi-Fi back by itself after a factory reset. Customers cannot do any of this themselves.

### How it is wired

- Each router gets a **management VLAN** (default 99) on its OLT port with the address `10.6.<n>.1/24`; `n` is unique per router and shown on the router's ONUs tab. ONUs get an address there by DHCP, along with the ACS address in DHCP option 43. The VLAN answers ARP only for its own address (`arp=reply-only`) and the DHCP server adds an ARP entry per lease (`add-arp=yes`); the router check warns on an older router that lacks either, and the snippet on the ONUs tab fixes it.
- The ONU network can reach **only the ACS** (`10.99.0.1` port 7547) and the router's NTP and DHCP. It gets no DNS (the ACS URL is an address) and has no internet. The server can reach the network only on the connection-request ports (7547, 30005 and 58000; V-SOL ONUs use 58000).
- The server adds the router's management subnet to that router's WireGuard peer (`AllowedIPs`), and `ftnbilling-wg-sync` adds a route `10.6.0.0/16 dev wg0`. The router's own WireGuard needs no change.
- The ACS listens on `ACS_LISTEN` (default the server's tunnel address, port 7547) and **refuses to start on a public address**. ufw allows 7547/tcp on the WireGuard interface only. It accepts only connections from inside a known router's management subnet, with HTTP Digest login. Optional TLS: `ACS_TLS_CERT` and `ACS_TLS_KEY` (set both). `ACS_LISTEN` must be the server's WireGuard address or loopback; another private address needs `ACS_LISTEN_ALLOW_PRIVATE=true` (do not use it on a NIC that is NATed to a public address). `ACS_REGISTERED_ONLY` (default `true`, written to the env file by the installer) makes the ACS take a first check-in only from a serial that is in the ONU inventory (Equipment, kind ONU; the TR-069 serial or the GPON serial; not retired). An unknown serial gets only an answer to its Inform: no task, no secret, no device record. It shows as "unknown ONU tried to check in" (serial, model, router; one entry per serial, kept 7 days, owners are told at most once a day) on the router's ONUs tab and on Equipment > ONU, with an "Add to inventory" shortcut. When the entry looks like an item that is already in the inventory (the serial contains its MAC, the management MAC is within 8 of its WAN MAC, or the model matches) it says "Looks like ..." with the reasons, and an owner can press "Link to this ONU" (own password, audited): the reported serial becomes that item's TR-069 serial (also editable on the item), so a box whose firmware reports a made-up serial (V-SOL V2804N-Z: `12345` + its label MAC) is the registered ONU from its next check-in. Nothing is linked automatically. A registered ONU's first contact, and a return on the shared login after a factory reset, also waits until the management-VLAN lease MAC of its address is near the inventory WAN MAC (same OUI, last 24 bits within 8; V-SOL: label MAC ...7E:0F, WAN/TR-069 MAC ...7E:10). Until then it gets no secret and the ONU list says "Waiting: management MAC doesn't match the registered ONU yet" with both MACs; the owner can press "Trust this ONU" (audited), and an inventory item with no WAN MAC always needs that. `ACS_REGISTERED_ONLY=false` gives the previous behaviour (any serial is accepted at first contact, no MAC wait). `ACS_COMPAT_AUTH=true` lets an ONU that can only do Basic authentication in; it is off by default and weaker. Digest needs `qop=auth`; an expired nonce is answered `stale=true`. If `DATA_KEY` changes the stored ACS password cannot be opened and ONU management stays off with a clear message: restore the old key, or start once with `ACS_RESET_CREDENTIALS=true` to make a new login (then every ONU profile on the OLT needs it, and every box a reset or a visit).

### Turning it on for a router

- A router set up from a **new join line** already has the management network (section "ftn 4b").
- A router built earlier: open the router, tab **ONUs**, press **Set up the management network** (VLAN 99 unless you choose another), then paste the lines shown on that tab into the router terminal once. They only touch objects whose comment starts with `ftn: onu`. The server never writes these over the API.

### What you do on the OLT (once per ONU profile)

Open **Settings, ONU management** for the URL and login (the password is shown after you type your own password).

V-SOL (the full steps, with what is still unverified, are in `deploy/V-SOL-OLT.md`):
1. Make the management VLAN (99 by default) on the OLT and allow it tagged on the uplink to the router.
2. In the ONU profile for tenants add a WAN of type TR069 on that VLAN, with DHCP.
3. In the profile's TR-069 settings give the ACS URL, the user name and the password, and turn periodic inform on (300 seconds).
4. Apply the profile. The ONU shows up under ONUs within a few minutes.

ZTE and Huawei: the same WAN of type TR069 on the management VLAN with DHCP and the ACS URL and login, set on the OLT template or on the ONU's own page. Some Huawei boxes take only the URL, from option 43.

Changing the ACS password breaks every ONU's login until the profile has the new one. (After the first check-in a box uses its own login, so only boxes that were reset, or that refused one, need the profile.)

**Isolate the ONUs from each other.** Turn on ONU isolation on the OLT for the management VLAN (the V-SOL menu is not documented where we could read it; see `deploy/V-SOL-OLT.md`). The router side already refuses to answer ARP for anything but its own address. **Tenants must never get an ONU's web-admin login or page**: with it they can change what the server set, including the Wi-Fi security and the ACS address. The server sets a random web-admin password on the box when the model allows it; the owner can show it behind their own password.

**The first claim.** A serial the server has never seen is accepted on its first contact with the shared login. Keep the shared login private, use serial authentication on the OLT, and register ONUs before they go out. A box that later checks in for a serial that is already known, from another router or address, is held back and the owners get an alert with Approve and Block.

### The ONU register

Every customer has an ONU you supply, in routing mode. Register each ONU **before** it goes out: serial, WAN MAC, model (Equipment, or the ONUs list). **The MAC printed on the label is often the base or LAN MAC, not the WAN MAC the router sees.** Confirm the WAN MAC on the bench: it is what appears under New devices (or in the lease) when the ONU first connects, and TR-069 also reports it. Then save the pair. Adding a customer from a register ONU, or "Swap ONU" on a house, sets the house MAC from the pair: you no longer type it. Older houses with a typed MAC keep working and link by MAC; use "Add to the register" in the ONUs list for them.

### What you see

The house page has a Wi-Fi card: the names as the ONU reports them, a Change Wi-Fi form (the password is never shown back; it is kept encrypted so it can be restored), Reboot ONU, and the state of the last request: Queued, Sent, Accepted (or "applies after the ONU restarts"), Confirmed, Failed with the reason, or Expired. "Accepted" only means the box said yes: check the Wi-Fi on a phone and press **It works** (the model then remembers which setting name took) or **It didn't work** (another name is tried). Queued tasks expire if the box never checks in: reboot after 1 hour, Wi-Fi after 24 hours, factory reset after 7 days (except the reset of a returned box, which waits for the box). The Wi-Fi choice belongs to the house, not to the box: Unlink and Swap ONU unlink the old box at once and queue a factory reset for its next check-in (the owner can cancel it), and a returned box never gets a house's Wi-Fi. A warning appears when the WAN MAC an ONU reports is not the one in the register, when a registered ONU is online on another router, or when the tenant changed the name on the box. The mismatch warning has a button "Use the reported MAC".

### Troubleshooting

- ONU never appears: check the OLT profile (VLAN, DHCP on the TR069 WAN, URL, login). On the router, the lease table of `ftn-onu-dhcp` should show it. From the server, `curl http://10.99.0.1:7547/` must answer 401 from the tunnel only.
- Appears but Wi-Fi changes stay Queued: the ONU did not take the connection request. Queued changes are applied at its next periodic inform (up to 5 minutes). Check that the server can reach `10.6.<n>.x` on port 7547, 30005 or 58000 (the router firewall rules `ftn: acs to onu`).
- Failed with a fault such as 9005: the ONU does not have that parameter name. Tell us the model; vendor quirks are a table in `internal/acs/profile.go`.
- Wrong ONU for a house: use the ONUs list, Unlink, then link by hand, or use Swap ONU on the house.
- A red "checked in from an unexpected place" card on an ONU: its serial came from another router, address or MAC than the one it is bound to (or is flapping). It gets nothing until you press Approve (bind it to the new place) or Block.
- "shared login only" on an ONU: it refused to take its own ACS login, so it logs in with the shared one. Works, weaker identity.
- Rotating the ACS password: every ONU stops reporting until its OLT profile has the new one.

## MAC vendor names

The New devices list and the customer pages name the maker of a device from its MAC. The server knows the IEEE registry offline (plus a short list of the names an ISP uses). A prefix that table lacks is looked up online, by one background worker, never while a page is being served: the page shows the name on the next load.

- `MAC_VENDOR_LOOKUP=online` (default) or `off`. Set it with the installer (`sudo MAC_VENDOR_LOOKUP=off ... ./install.sh`) or in `/etc/ftnbilling/ftnbilling.env`, then restart the app. `MAC_VENDOR_DAILY` is the most requests a day (default 500).
- What leaves the server: the first three bytes of the MAC (`AA:BB:CC`) in a request to `https://api.macvendors.com/`, with the User-Agent `ftnbilling`. Never the whole MAC address, a name, a house or a router.
- Limits: one request a second, a daily budget, a 5 second timeout, and a `429` is honoured (Retry-After, then a longer wait each time, up to an hour). A name is kept for good, "no such prefix" for 30 days, and a prefix is never asked twice in a day. The answers are kept in the database (table `oui_cache`, migration 0020), so a restart asks nothing again.
- A randomised (private) MAC, the kind phones use per network, is shown as "Private (randomised) address" and never looked up. Long registry names are shortened (V-SOL, MikroTik, ...); the full name is the tooltip.
- A failure (no internet, a refusal) is silent: the device simply shows no vendor, and a debug log line says why, at most once a minute.

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
