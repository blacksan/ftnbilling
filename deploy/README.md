# Online billing server for many buildings (VPS + WireGuard)

One VPS runs billing, the tenant portal and the admin pages for every building.
Each building keeps its own MikroTik router doing the hotspot. Routers join a
private WireGuard network at the VPS; the VPS reaches each router's REST API
through its tunnel address.

```
 Building A router ──┐                                 ┌─ ftnbilling :8080 (admin: SSH tunnel / Tailscale)
 Building B router ──┼── WireGuard udp/51820 ──► VPS ──┤  Caddy :80/:443  (public portal, M-Pesa callbacks)
 Building C router ──┘   10.99.0.0/22, wg0             └─ NetFlow udp/2055 on 10.99.0.1 (tunnel only)
 phones ── https://DOMAIN/portal?site=<id> ─────────────► Caddy
```

| Path | How |
|---|---|
| VPS to router REST API | Over the tunnel, to the router's tunnel address (`10.99.0.x:8081`). The router allows REST only from `10.99.0.1`. |
| Phones to portal | Public internet, `https://DOMAIN/portal?site=<building id>`. The hotspot walled garden lets unpaid phones reach only the VPS. |
| Router to NetFlow | Through the tunnel, to `10.99.0.1:2055`. The server tells buildings apart by the tunnel source address. |
| You to admin | Not published. Use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 you@VPS`) or, if you like, Tailscale. Caddy refuses `/admin` from the internet, and the app refuses it from the tunnel subnet. |

Design size: about 40 buildings and 1500 flats on a 1 vCPU / 1 GB VPS. The
service runs with `GOMEMLIMIT=600MiB GOGC=50`.

The portal is public, so the server trusts a `mac`/`ip` pair only if that
building's router currently lists it in `/ip/hotspot/host`. This check is
always on for WireGuard buildings. Someone outside the building who guesses a
flat's MAC gets the neutral "open this page from the WiFi login" page.

## 1. Before you start

1. **VPS:** Ubuntu 22.04/24.04 or Debian 12, 1 vCPU and 1 GB RAM, with a kernel that has WireGuard (any current one does).
2. **Domain:** an A record such as `billing.yourdomain.co.ke` pointing to the VPS. HTTPS is required for M-Pesa and for the router join command (the join script carries the router's keys, so it is never served over plain HTTP).
3. **GitHub token:** the repo is private. Create a fine-grained token for `blacksan/ftnbilling` with **Contents: Read-only**.
4. **Routers:** RouterOS 7 with the NTP client on, and the hotspot and fetch features enabled: `/system/device-mode/update hotspot=yes fetch=yes` (confirm as the router asks).

## 2. Install on the VPS (one line)

```bash
GH=github_pat_xxx; curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | sudo GH_TOKEN=$GH DOMAIN=billing.yourdomain.co.ke bash
```

The installer does the following:
- Installs Go and builds the app; runs it as the `ftnbilling` systemd service (data in `/var/lib/ftnbilling`, settings in `/etc/ftnbilling/ftnbilling.env`, `GOMEMLIMIT=600MiB GOGC=50`).
- Installs `wireguard-tools` and creates `wg0` (`10.99.0.1/22`, UDP 51820, MTU 1380). The server private key is generated once and stays in `/etc/wireguard/wg0.conf`; it is never regenerated. The app only gets the public key (`WG_PUBLIC_KEY`).
- Installs `/usr/local/sbin/ftnbilling-wg-sync`, run as root by a systemd path unit when the app changes `/var/lib/ftnbilling/wg/apply`. It validates `peers.conf` (only `[Peer]`, a 44-character `PublicKey`, and a single `/32` `AllowedIPs` inside the subnet; no symlinks, no files over 256 KB) and then runs `wg syncconf`. The app is not root and cannot do anything else to the interface.
- Installs a timer that writes `wg show wg0 dump` (handshakes) to `/run/ftnbilling/wg0.dump` every minute, so the admin can show whether a tunnel is up.
- Sets up Caddy (automatic HTTPS with `DOMAIN`).
- Sets up ufw: SSH, 80, 443 and UDP 51820 are open. On `wg0` only UDP 2055 to `10.99.0.1` is allowed; all other input and all routing from `wg0` is denied. The public UDP 2055 rule is removed (kept on an old install that still has `ROUTER_URL`, because its router sends NetFlow over the internet).
- Adds daily backups to `/var/backups/ftnbilling`: `ispbill.json`, `outbox.json`, `alerts.json`, `fleet.json` for 30 days, and the `metrics/` folder weekly for 4 weeks.
- Warns if `net.ipv4.ip_forward` is 1 on the server.
- Tailscale is optional: it is only set up if you pass `TS_AUTHKEY=...` or it is already running. Otherwise the installer prints the SSH tunnel line.

At the end it prints the admin password and what to do next.

**Upgrading:** run the same line again. Data, settings and the WireGuard key are kept. A v2 data file is converted to the new format on first start; the old file is kept as `ispbill.json.v2.bak`.

## 3. Add a building

1. Open the admin and go to **Buildings, Add building**. Give the name; the router layout defaults suit a standard install (hotspot server `hotspot1`, WAN `ether1`, OLT port `sfp-sfpplus1`).
2. On the building page press **Make the join command**. It is shown once, works once, and expires after 30 minutes.
3. Paste it in the router's terminal (WinBox, New Terminal). The router downloads its setup script, creates its own WireGuard key (the private key never reaches the server), brings the tunnel up, restricts REST to the server, sends NetFlow through the tunnel, installs the hotspot pages for that building and whitelists the VPS in the walled garden.
4. The building page switches to "Connected" when the tunnel is up and the REST API answers.

If the router's clock is wrong or it has no certificate store, the page offers a fallback command with `check-certificate=no`.

The script moves WebFig off port 80 (the hotspot uses it). Keep WinBox or SSH to reach the router.

A building that is "paused" in the admin gets no collection and no router changes, and tenants cannot buy there.

## 4. Check

- `curl -s localhost:8080/healthz` on the VPS shows `sites_total`, `sites_online` and `router_ok`.
- `wg show wg0` on the VPS lists one peer per joined building.
- The building page shows the router, the WireGuard handshake and the portal URL.
- A phone on that building's WiFi should open the online portal.

| Symptom | Fix |
|---|---|
| Join command refused | `PUBLIC_BASE_URL` must start with `https://` (set `DOMAIN` and re-run the installer). For a bench test only, `ALLOW_INSECURE_JOIN=true`. |
| Router says 404 for the join link | The command was used, expired or cancelled. Make a new one. A chat or browser preview also fetches links; the server ignores clients that do not identify as MikroTik, so paste into the terminal, not a chat. |
| Tunnel never comes up | UDP 51820 must reach the VPS (`ufw status`, provider firewall). `WG_ENDPOINT` in the env file must be the VPS public address. Check `wg show wg0`. |
| Tunnel up but the router is "not reachable" | The router firewall must accept `10.99.0.1` to port 8081 (the join script adds this). Re-run the join command to rebuild it. |
| Phone shows an error instead of the portal | Check `/ip/hotspot/walled-garden/ip/print` on the router and that the domain resolves. |
| Portal says "open this page from the WiFi login" | The router's host table must list the phone: `/ip/hotspot/host/print`. |
| No traffic charts | Check `/ip/traffic-flow/target/print` (should be `10.99.0.1:2055`). The admin Settings page counts NetFlow packets from unknown sources. |

## 5. Alerts, Telegram, email and WhatsApp

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
and `internal/web/routerfiles/remote.rsc` (Tailscale container setup) are
legacy; new buildings use the join command instead.

To move such a building onto WireGuard, add a new building, run its join
command on that router, then move the accounts to it (account page, Move to
another building) and remove the old one.

## Moving data between servers

Stop both apps, copy `/var/lib/ftnbilling/ispbill.json` (and `metrics/` if you
want the history), `chown ftnbilling:` it and start the service.
