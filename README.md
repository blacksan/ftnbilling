# ispbill

> **Two ways to run it.** On the RB5009 as a container (this README), or on a
> VPS with WireGuard tunnels to the routers and PostgreSQL: see
> [deploy/README.md](deploy/README.md) for the one-command install of a new server.

A small internet-billing app with a captive portal. It runs as a container on a
MikroTik RB5009UG+S+IN (RouterOS 7, arm64) and controls the router's Hotspot
through the RouterOS REST API. Go standard library only, JSON-file storage, one
static binary.

> **TEST BUILD.** M-Pesa Paybill is not available yet, so payments use a
> simulated "Test payment" button (`TEST_MODE=true`) and voucher codes. With
> `TEST_MODE=true` **anyone on the hotspot can get free internet by tapping the
> button.** Never leave it on for real customers. The M-Pesa (Daraja) code is
> written but stays disabled until its credentials are set.

## What it does

- **One account per flat.** The admin creates an account: flat label (e.g. "Flat 3B"), the ONT's WAN MAC (required, unique), optional tenant, phone and notes. The app generates a 6-character code (no 0/O/1/I/L) that never changes and is also the **Paybill account number**.
- **Login is driven by the MAC.** The portal looks at the MAC the router reports. A known MAC shows "Flat 3B · Account K7Q2MX", the status (active until ..., expired, suspended), the packages, payment and voucher. If the flat is active the portal logs it in through the REST API on its own. An unknown MAC shows "This connection isn't registered yet. Please contact <SUPPORT_PHONE>", is recorded under Admin -> Unregistered devices, and cannot pay. There is no PIN and no "Already paid?" step; the router-side hotspot password is internal and never shown.
- **One hotspot user per account** on the router: name = code, `mac-address` = ONT MAC, profile = package, mac-cookie kept. Phone numbers are only the M-Pesa payer; they are not identities.
- **Prices are plain KES.** No tax lines or tax wording anywhere.
- Admin UI at `/admin` (basic auth): Dashboard, Accounts (create, edit, change MAC, suspend/unsuspend, extend, kick), Unregistered devices, Payments (ledger plus CSV), Record M-Pesa, Unmatched, Metrics, Orders, Packages, Vouchers, Settings (shows the lifecycle policy).
- `/healthz` returns `{ok, router_ok, test_mode, mpesa_enabled}`.

### Unregistered devices

Admin -> Unregistered devices lists MACs that opened the portal plus hotspot hosts that are not authorised and match no account. For each you can **create an account** for that MAC (enter the flat label) or **attach** it to an existing account (an ONT swap: the account's MAC changes, the router user is updated and the old session is kicked). Changing a MAC by hand on the account page does the same.

### Payments, ledger and reconciliation

- Every payment is a ledger row with a unique key (M-Pesa receipt code, `VOUCHER-x`, `TEST-x`, `ADMIN-x`). Re-delivered callbacks and re-entered codes are ignored, so nothing is credited twice.
- Paybill payments (C2B) are matched by account code (`BillRefNumber`) and then to the package whose price equals the amount. STK push uses the account code as `AccountReference`. Old order ids are still accepted.
- Anything that cannot be matched (unknown account, amount that fits no package) goes to Admin -> Unmatched. Resolve it with **Assign** (pick the account, the package is applied), **Refunded** or **Ignored**, each with a note.
- **Admin -> Record M-Pesa** is for payments seen on the M-Pesa statement that never reached the app: pick the account, enter the 10-character code (uppercase letters and digits), amount, payer phone and date; the package is preselected from the amount. It applies exactly like an automatic payment, and a code can be recorded once.
- Router updates (create user, change profile, remove on expiry, kick) are queued, retried with backoff (5 s doubling to 5 min), kept across restarts and shown on the dashboard. If the router is down when someone pays, the payment is safe and the router catches up when it returns.
- Lifecycle defaults (shown in Settings): the expiry is calendar based; paying while active adds time to the current expiry; an upgrade (same or higher speed) switches the speed immediately and counts from the current expiry (no pro-rata credit); a **downgrade is not allowed while a plan is active** (the portal greys out slower packages with "Available when your current plan ends on <date>", checkout refuses them, and Paybill or manual payments for a slower package are put in Unmatched with the reason "lower plan while active" for the admin to decide; the admin extend and Record M-Pesa forms have an "apply even if lower speed" box); expired or suspended accounts can buy any package; an expired flat reconnects automatically when it pays; there is no reconnection fee. Reminder hook points are logged as events 3 days before expiry and on expiry (`REMINDER_DAYS`, `REMINDER_ON_EXPIRY`); no SMS is sent, they only create log events for a future sender.

Default packages (seeded when none exist; edit in `/admin/packages`):

| Name | Time | Speed | Price |
|---|---|---|---|
| 1 Hour | 60 min | 5M/5M | KES 10 |
| 1 Day | 1440 min | 10M/10M | KES 50 |
| 1 Week | 10080 min | 10M/10M | KES 250 |
| Monthly 20 Mbps | 43200 min | 20M/20M | KES 2000 |
| Monthly 40 Mbps | 43200 min | 40M/40M | KES 3000 |
| Monthly 80 Mbps | 43200 min | 80M/80M | KES 4000 |

Keep package prices unique: Paybill payments are matched to a package by amount. No accounts are seeded.

### Data format

The data file has a format version. A data file from the earlier PIN/subscriber version makes the app **refuse to start** with a clear message (it will not guess a migration). Move the old `ispbill.json` away and start fresh, then create the accounts.

## Metrics and quality of experience (`/admin/metrics`)

Server-rendered, with inline SVG charts, readable on a phone and a laptop. It shows "no data yet" instead of failing when the router cannot be reached. With `ROUTER_MOCK=true` the page has a "Generate demo history" button and the live collector produces synthetic numbers.

What it shows, with a period selector (Today, 7 days, 30 days, This month): tiles (month-to-date WAN use against the fair-use limit with a projection, current WAN speed, peak WAN use, latency and loss, router CPU and temperature, active flats), "Things to look at" flags, WAN and OLT throughput, top 10 flats, a sortable per-flat table (download, upload, share, peak, time at cap, top services), services, latency and packet loss, a busy-hours strip, a ports table, and a "How to read this" box. `/admin/metrics.csv` exports the table; `/admin/metrics/flat?code=K7Q2MX` is the drill-down for one flat.

How the numbers are collected:

| Source | Interval | Notes |
|---|---|---|
| Hotspot sessions | 60 s | Per-flat bytes: the difference between readings per (user, session). A counter reset counts as a new session starting from the new value. |
| Simple queues | 60 s | Package speed and queue state, for "time at cap". |
| Interfaces | 30 s | `WAN_INTERFACE`, `OLT_INTERFACE` and the customer VLAN rx/tx. |
| Traffic flow (NetFlow v5) | as exported | UDP `NETFLOW_ADDR` (default `:2055`). Services are guessed from the router's DNS cache and then from ports. |
| Ping | 60 s | `QOE_PING_TARGETS` (8.8.8.8, 1.1.1.1) from the router: latency and loss. |
| System resource and health | 60 s | CPU, memory, temperature. |

Collection intervals are changeable with `METRICS_FLOW_EVERY`, `METRICS_PORT_EVERY`, `METRICS_PING_EVERY`, `METRICS_DNS_EVERY`, `METRICS_SAVE_EVERY` (Go durations such as `30s`).

Storage: `DATA_DIR/metrics.json`, written atomically every 5 minutes and on shutdown (a crash loses at most 5 minutes; a restart can lose up to a minute of per-flat counters). It is kept in tiers: 5-minute buckets for 48 hours, hourly for 14 days, daily for 400 days, so its size is bounded (a few MB). That is about one small file write per 5 minutes to the USB stick.

QoE flags and their thresholds (named constants in `internal/metrics/report.go`): WAN at or above 85% of capacity for 3 or more hours in the period; a flat spending more than 20% of its online time at 90% or more of its package speed (needs at least 120 minutes online) is an upgrade candidate; average packet loss above 1%; projected month total above `FUP_TB` (amber from 80% used). Month totals count WAN download plus upload for the calendar month (EAT) since collecting began.

**Service classification: accuracy caveats.** The split into YouTube, Netflix and so on is a good guide, not an exact count.
- NetFlow only has IP addresses and ports. The app maps addresses to names using the router's DNS cache. That only works for clients that use the router for DNS. If an ONT or a phone uses its own resolver, or DNS-over-HTTPS, the name is never seen and the bytes are shown as "HTTPS (other)" or "CDN (other)".
- The DNS cache expires, and one IP can serve many sites (CDNs), so some bytes land in the wrong or the generic bucket.
- Fasttracked connections skip traffic-flow entirely. Check the fasttrack rule (see `routeros/setup.rsc`, section 5b), otherwise traffic is missing.
- Flow records leave the router only at the flow timeouts, so the newest minute is incomplete.
- Per-flat totals do not use NetFlow, so they are not affected by any of this.
Edit the rules table (`ServiceRules` and `PortRules` in `internal/metrics/netflow.go`) to add services and rebuild.

## Quick local run (no router)

```sh
./build.sh                       # tests, builds, image
ROUTER_MOCK=true TEST_MODE=true ADMIN_PASS=change-me DATA_DIR=/tmp/ispbill \
  LISTEN_ADDR=127.0.0.1:8080 build/ispbill-amd64
```
Open http://127.0.0.1:8080/admin (user `admin`), create an account with MAC `AA:BB:CC:DD:EE:01`, then http://127.0.0.1:8080/portal?mac=AA:BB:CC:DD:EE:01&ip=10.5.50.20 and tap "Test payment" (it completes after about 3 s). A different MAC shows the "not registered" page. `ROUTER_MOCK=true` uses an in-memory fake router, so nothing is sent anywhere. Screenshots of the pages are in `dist/screenshots/`.

## Many routers (multi-site)

One server can bill many routers. Each router is a MikroTik with
its own hotspot and a `Site` record. Account codes are global (they are the
Paybill account numbers); ONT MACs and flat labels only have to be unique
inside a router. Packages are shared, and each router can hide a plan or
give it a different price (Admin, Routers, the router page). Prices must
stay unique inside a router because Paybill payments are matched by amount.
The admin has a router switcher in the top bar (`All routers` or one) and
list pages gain a Router column when all are shown. An account can be moved
to another router from its page.

Routers join over WireGuard with a one-time command from the admin: see
[deploy/README.md](deploy/README.md) for the server setup (one command on a
fresh VPS: PostgreSQL, Caddy, WireGuard for the routers, a public admin behind
the app's own sign-in, firewall, backups, `ftnbilling-doctor`) and
for the join flow. The
design target is about 40 routers and 1500 flats on a 1 vCPU / 1 GB VPS.

Upgrading from the single-router version needs no admin action: the data file
is converted (the old one is kept as `ispbill.json.v2.bak`), and a server with
`ROUTER_URL`/`ROUTER_PASS` (or the container on the router) gets one router
called "Main building" that keeps working from the environment settings.
`routeros/setup.rsc` and `internal/web/routerfiles/remote.rsc` are the legacy
one-router setup; new routers use the join command.

The extra settings are `WG_PUBLIC_KEY`, `WG_ENDPOINT` (default `PUBLIC_IP:51820`),
`WG_SUBNET` (`10.99.0.0/22`), `WG_DUMP_PATH`, `JOIN_TOKEN_TTL` (`30m`),
`ALLOW_INSECURE_JOIN` (bench tests only), `JOIN_REQUIRE_MIKROTIK_UA` (`true`)
and `SITE_NAME` (renames the legacy router once). `ROUTER_PASS` is only needed
while a legacy router exists.

## Monitoring, alerts and WhatsApp

The server checks every router every 30 seconds (a TCP connection to the router's API through the tunnel). A router is **down** after 3 failed checks in a row and **up** again after 2 good ones. Each outage is logged with a cause: `no-handshake` (the WireGuard handshake is older than 3 minutes: power or internet at the router), `tunnel-up-rest-down` (the tunnel works but the router does not answer) or `unknown`.

- **Fleet overview** (`/admin/fleet`): one tile per router, reloads every 30 seconds. It is the start page when "All routers" is selected and there is more than one router.
- **Uptime** (`/admin/uptime`): 30 or 90 days of daily availability, the tunnel round-trip time and the outage list.
- **Alerts** (`/admin/alerts`): the rules, their settings, a Send test button for each channel, mute, and the history.
- **Messages** (`/admin/messages`): everything the server sent or tried to send, with retry and cancel.

### Alert settings

All of these are edited on the Alerts page. Defaults:

| Rule | Fires when | Default |
|---|---|---|
| Router offline | the router API does not answer | for 5 minutes |
| WAN saturated | WAN use is at or above the threshold | 85% of capacity for 15 minutes |
| Packet loss | average loss to the ping targets | 5% for 10 minutes |
| High latency | average round trip | 150 ms for 10 minutes |
| OLT port down | the OLT interface is not running | for 2 minutes |

An alert is *pending* until its condition has held for the whole time, then *firing* (you get the message), then *resolved*. The recovery message is sent only if the problem message was. The same alert is announced at most once per **cooldown** (60 minutes), and optionally repeated every 6 hours while it stays firing. While a router is offline its other alerts are paused. If more than half of the routers (and at least 3) go offline within 2 minutes you get one message listing them, and one when they are all back. **Mute** silences a router for planned work. Messages go to every channel that is switched on and set up.

The server also raises one server-wide alert, **Backup problem**, when `backup-status.json` shows a dump older than 36 hours, a backup error, a failed restore drill or a failed Drive upload (see [deploy/BACKUP.md](deploy/BACKUP.md)). It is not an editable rule.

The email and WhatsApp settings are edited in the admin under **Settings** (see "Settings pages" below). The environment variables are a fallback that applies until you save a value there:

| Variable | Default | Notes |
|---|---|---|
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `SMTP_FROM`, `SMTP_IMPLICIT` | port `587` | email: STARTTLS on 587, TLS from the first byte on 465 (or when `SMTP_IMPLICIT` is true). The password is only sent over TLS (or to localhost) |
| `ALERT_EMAIL_TO` | | comma separated recipients |
| `WA_TOKEN`, `WA_PHONE_NUMBER_ID` | | WhatsApp Cloud API |
| `WA_ADMIN_NUMBERS` | | numbers that get admin alerts, like `254712345678`, comma separated |
| `WA_API_VERSION`, `WA_LANG` | `v23.0`, `en` | Graph API version; template language |
| `WA_DAILY_LIMIT` | `240` | tenant WhatsApp messages per day (admin alerts are exempt) |
| `WA_MIN_CONFIRM_KES` | `250` | smallest payment that gets a confirmation |
| `WA_QUIET_FROM`, `WA_QUIET_TO` | `21`, `7` | hours (EAT) between which tenant reminders wait |
| `WA_APP_SECRET`, `WA_VERIFY_TOKEN` | | switch the inbound webhook on (see below) |

### Messages to tenants (WhatsApp)

| Message | When | Template |
|---|---|---|
| Expiry reminder | `REMINDER_DAYS` before a plan ends | `ftn_expiry_reminder` |
| Plan ended | when a plan of 7 days or longer ends (`REMINDER_ON_EXPIRY`) | `ftn_plan_expired` |
| Payment received | an M-Pesa payment of at least `WA_MIN_CONFIRM_KES` was applied in the last 6 hours (not free admin grants or vouchers) | `ftn_payment_received` |
| Outage notice | the admin presses "Tell tenants about the outage" on the fleet page, or automatically after the offline alert has fired for 30 minutes (setting: never / manual / automatic, default manual) | `ftn_outage_notice` |
| Service restored | to the tenants who got the notice, when the outage ends | `ftn_outage_resolved` |

Each message is created once per event (an expiry, a payment, an outage), however often the server checks. A message is checked again just before it is sent and skipped if it no longer applies (the tenant paid, opted out or changed number).

**Meta Business Manager checklist**

1. A Meta Business portfolio and a WhatsApp Business Account (WABA).
2. A dedicated phone number registered to the Cloud API. It cannot also be in use in the WhatsApp app or WhatsApp Business app.
3. A System User with a permanent token that has the `whatsapp_business_messaging` and `whatsapp_business_management` permissions. Put the token in Settings > WhatsApp (access token), and the phone number's ID (not the number) in the phone number ID field.
4. A payment method on the WABA.
5. The tenants' consent to receive messages. Collect it when you take the flat's details, and keep a record.
6. Create the six templates below, category **Utility**, language **English (`en`)**, and wait for approval. The wording and the number of parameters must match exactly, or Meta refuses the message (error 132000 or 132001, shown on the Messages page as failed).
7. The five tenant templates carry the footer **Reply STOP to opt out**.
8. Put your own number(s) in the admin numbers field of Settings > WhatsApp, save, and press **Send test** there (it sends one of the `ftn_` templates with sample values to one of your numbers). The Alerts page has its own Send test button too.

Message templates (create these exactly; `{{1}}` and so on are the parameters):

| Name | Body | Parameters |
|---|---|---|
| `ftn_expiry_reminder` | Hello, your {{1}} internet plan for {{2}} ends on {{3}}. To stay connected, pay KES {{4}} via {{5}}, account number {{6}}. Thank you. | package, flat + router, expiry, price, pay hint, account code |
| `ftn_plan_expired` | Hello, the internet plan for {{1}} ended on {{2}}. To reconnect, pay via {{3}}, account number {{4}}. You will be reconnected automatically. | flat + router, expiry, pay hint, code |
| `ftn_payment_received` | Payment received: KES {{1}}, reference {{2}}. Your {{3}} plan for {{4}} is active until {{5}}. Thank you. | amount, receipt, package, flat + router, expiry |
| `ftn_outage_notice` | Hello, we are aware of an internet outage at {{1}} since {{2}}. Our team is working on it. We apologise for the inconvenience. | router, start |
| `ftn_outage_resolved` | Internet service at {{1}} was restored at {{2}}. Thank you for your patience. | router, time |
| `ftn_admin_alert` | Network alert, {{1}}: {{2}} at {{3}}. Details: {{4}}. Time: {{5}}. End of alert. | PROBLEM/RECOVERED, rule, router, detail, time |

A tenant only gets messages if the account has a WhatsApp number (Accounts, the flat's page; "same as phone" copies the phone number) and has not been opted out. Messages go out between 07:00 and 21:00 EAT for reminders; payment confirmations and outage notices go at once. At most `WA_DAILY_LIMIT` (240) tenant messages are sent per day; admin alerts are not counted. Failed sends retry with growing waits (30 s up to 1 h, 8 tries). A rejected token pauses the channel and shows a banner on the Messages page.

### Inbound WhatsApp replies (optional)

When the app secret (of your Meta app) and the verify token (any string you choose) are both saved in Settings > WhatsApp, the server answers `GET/POST /wa/webhook`. It starts and stops with those two values, no restart. Until both are set the address answers 404.

1. In the Meta app, open WhatsApp > Configuration > Webhook.
2. Callback URL: the one shown on Settings > WhatsApp (`PUBLIC_BASE_URL/wa/webhook`). Verify token: the same string you saved. Press Verify and save; the settings page then shows when Meta verified it.
3. Subscribe to the `messages` field.
4. The page shows "last signed event" once Meta has sent something.

Every POST must carry a valid `X-Hub-Signature-256`; others are refused. Delivery statuses (sent, delivered, read, failed) are shown on the Messages page. A tenant whose number is on an account can send `BALANCE` or `STATUS` (plan, expiry, account code and how to pay), `STOP` (opt out) or `START` (opt back in); anything else gets a short help text. A number that is on no account gets only the support phone. At most 5 replies per sender per minute.

## Settings pages

Admin, System, **Settings** has these pages. Everything saved there applies at once; no restart.

| Page | What it holds |
|---|---|
| General and password | read-only facts (policy, database, backups, listen address), the admin password, and where each setting comes from |
| WhatsApp | access token, phone number ID, admin numbers, template language, daily limit, webhook secrets, outage notice mode, a test button |
| M-Pesa | environment, consumer key and secret, shortcode, passkey, public URL, callback token, what to do with an unknown account number, test buttons |
| Email | SMTP server, user, password, From, recipients, a test button |
| Equipment tools | ONT list import and a MAC address finder (asks every connected router) |
| Router tools | ping, traceroute and a restricted console, per router |

**Precedence.** A value saved in the admin wins over the environment file, which wins over the built-in default. Each field shows where its value comes from, and "go back to the environment file value" removes the saved one. Saving a form stores only the values you changed, so a value from the environment file stays there until you really change it. Restart-only values (listen address, WireGuard, `DATABASE_URL`, `DATA_KEY`) stay in `/etc/ftnbilling/ftnbilling.env`.

**Secrets.** Tokens, keys and passwords are encrypted in the database with a key derived from `DATA_KEY`, are never shown again (only the last four characters), and are never written to the audit log (which lists key names only). Setting, replacing or reverting a secret, rotating the callback token and showing the callback URLs need the current admin password. Without `DATA_KEY` the plain settings still save and secret fields are refused. **`DATA_KEY` must be restored together with the database**: a database restored without it keeps its saved secrets, but they cannot be read, and the pages show a banner until you enter them again. The installer generates `DATA_KEY` on every install and the Drive backup keeps it.

**Admin password.** Until you set one in Settings, `ADMIN_PASS` from the environment file is the password. Once one is saved (12 or more characters, hashed with pbkdf2-sha256), the environment value stops working. Logging in is rate limited: 5 failures per 15 minutes per address and 30 overall, then HTTP 429 with `Retry-After`. If you are locked out or forgot it:

```
sudo ftnbilling-admin-reset              # removes the saved password: ADMIN_PASS works again (refused if ADMIN_PASS is empty or "admin")
sudo ftnbilling-admin-reset --generate   # sets a random password and prints it once
sudo ftnbilling-admin-reset --setup-code # a one-time code for the first sign-in / authenticator setup
sudo ftnbilling-admin-reset --reset-totp # forget the authenticator and recovery codes
sudo ftnbilling-admin-reset --revoke-sessions
```

With PostgreSQL this works while the service runs (it then restarts the service). With the JSON file database, stop the service first.

**Router console.** One RouterOS command through `/rest/execute`, 500 characters at most, one line. Read-only commands (`print`, `get`, `find`, `export`, `monitor`, `ping`, `traceroute`) run at once; commands that change something (`add`, `set`, `remove`, `enable`, `disable`, `reboot` and so on) need the router's exact name and the current admin password. These are never run: `/user`, `/password`, `/system script`, `/system scheduler`, `/system backup`, `/system package`, `/system reset-configuration`, `/ip service`, `/certificate`, `/tool fetch`, `/import`, file removal or editing, `show-sensitive`, and `:execute`. Abbreviated words are understood, so `/us pr` is refused too. Private keys, passwords and secrets in the output are replaced by `[hidden]`. Every attempt is written to the audit log with secrets masked. At most 20 runs a minute and 2 at once, and a run is cut off after 25 seconds.

**Daraja go-live checklist.**

1. Sandbox first: Settings > M-Pesa, environment `sandbox`, the sandbox key and secret, shortcode `174379`, the passkey from the Daraja simulator, and the public https address. Press **Test key and secret**.
2. Press **Register the C2B URLs** and **Send KES 1 prompt** (enter your own number). The prompt uses the reserved account reference `FTNTEST`; the confirmation is recorded in Unmatched with the note "settings test" and never credits a flat. The sandbox C2B shortcode differs from 174379: put it in the C2B shortcode field.
3. For production: switch the environment to `production` and enter the live key, secret, Paybill, passkey. **The KES 1 test is real money in production.**
4. Register the C2B URLs. Daraja refuses a second registration ("URLs are already registered") until the old ones are deleted in the Daraja portal or by Safaricom support. So **do not replace the callback token on a live Paybill without that**; after replacing it, register again.
5. Callback URLs must be https and must not contain "mpesa", "safaricom", "exe", "exec", "cmd", "sql" or "query" (the token is random hex, so this holds). The app registers the form `PUBLIC_BASE_URL/pay/cb/t/<token>/c2b/confirm` (and `validate`); the STK result URL travels with each push. The older `?t=<token>` form also works.
6. External validation is off by default on a Paybill: Safaricom enables it on request. Without it, an account number that matches no flat cannot be bounced at validation; it lands in Unmatched. With it, "Payment with an unknown account number" decides: `reject` (default) or `accept`.
7. The payer's phone number may arrive hashed in a C2B confirmation; the pages show whatever arrives.
8. Customers type the account number by hand: spaces, dashes and lower case are ignored (`abc-234` finds ABC234).

## Configuration (environment variables)

| Variable | Default | Notes |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | |
| `DATA_DIR` | `/data` | holds `ispbill.json`, `metrics/` (one file per router), `fleet.json`, `alerts.json` and `outbox.json` |
| `BRAND_NAME` | `Fast Tech Net` | |
| `TEST_MODE` | `false` | enables the Test payment button |
| `ROUTER_URL` | `http://172.17.0.1` | `setup.rsc` sets `http://172.17.0.1:8081` |
| `ROUTER_USER` / `ROUTER_PASS` | `ispbill` / | legacy single-router mode only (the "Main building"); not needed on a server that only uses join commands |
| `ROUTER_MOCK` | `false` | fake router |
| `HOTSPOT_SERVER` | `hotspot1` | |
| `ADMIN_USER` / `ADMIN_PASS` | `admin` / required until a password is saved in Settings | app refuses to start if there is no saved password and PASS is empty or `admin` |
| `SUPPORT_PHONE` | | shown on the portal, including on the "not registered" page |
| `PUBLIC_BASE_URL` | | needed for M-Pesa callbacks |
| `MPESA_ENV` | `sandbox` | or `production`. The M-Pesa, WhatsApp and email variables are a fallback: Settings wins once you save there |
| `MPESA_CONSUMER_KEY`, `MPESA_CONSUMER_SECRET`, `MPESA_SHORTCODE`, `MPESA_PASSKEY` | | all four (with `PUBLIC_BASE_URL`) switch M-Pesa on |
| `MPESA_CALLBACK_TOKEN` | | required once the M-Pesa key, secret and shortcode are set (the app refuses to start without it): callbacks must carry the token (`?t=<token>` or `/pay/cb/t/<token>/...`). Settings > M-Pesa makes one when you save there |
| `REMINDER_DAYS` | `3` | days before expiry for the reminder event (0 = off) |
| `REMINDER_ON_EXPIRY` | `true` | log an event when an account expires |
| `WAN_INTERFACE` / `OLT_INTERFACE` | `ether1` / `sfp-sfpplus1` | for the ports charts; these layout settings (with the two capacity values, `FUP_TB`, `CUSTOMER_SUBNET`) apply to the legacy router only. Join-command routers have them on their own page |
| `WAN_CAPACITY_MBPS` / `OLT_CAPACITY_MBPS` | `500` / `10000` | |
| `FUP_TB` | `15` | monthly fair-use figure used for the projection |
| `SMTP_*`, `ALERT_EMAIL_TO`, `WA_*` | | alerts and tenant messages, see "Monitoring, alerts and WhatsApp" above |
| `DATA_KEY` | | base64 of 32 random bytes (`openssl rand -base64 32`). Encrypts national IDs and the secrets saved in Settings. The installer makes one; back it up with the database |
| `NETFLOW_ADDR` | `:2055` | UDP listen address for NetFlow v5; one socket for all routers, told apart by tunnel source address |
| `CUSTOMER_SUBNET` | `10.5.50.0/24` | flows from here are attributed to flats |
| `QOE_PING_TARGETS` | `8.8.8.8,1.1.1.1` | comma separated |
| `METRICS_FLOW_EVERY`, `METRICS_PORT_EVERY`, `METRICS_PING_EVERY`, `METRICS_DNS_EVERY`, `METRICS_SAVE_EVERY` | `60s`, `30s`, `60s`, `60s`, `15m` | Go durations, at least 1s |

## Topology

```
Safaricom fibre/line --> RB5009 ether1 (WAN)
                         RB5009 sfp-sfpplus1 --10G DAC--> V-SOL V1600GS-R (OLT, pure Layer 2)
                                                           PON --> 1:32 splitter --> router fibre --> ONT per flat
                                                                                      (ONT in ROUTE mode, WAN = DHCP)
RB5009 br-containers 172.17.0.0/24: router .1, ispbill container .2
RB5009 br-hotspot    10.5.50.0/24 : vlan100-customers (from the OLT) [+ optional ether5 bench port]
```

- All billing and account control is on the MikroTik. The OLT only switches frames.
- OLT side (see the comments at the top of `routeros/setup.rsc`): service VLAN 100 tagged on the OLT uplink; each ONT's WAN port untagged on the ONT and translated to VLAN 100 on the OLT; ONT-to-ONT isolation enabled so flats cannot see each other.
- Each ONT is in **route mode** (NAT and Wi-Fi) with a DHCP WAN. The MikroTik therefore sees **one client per flat**: the ONT's WAN MAC and IP. One login on the captive page unlocks every device in that flat, and the hotspot user is bound to that MAC.
- The account code (6 characters) is the flat's account number and the M-Pesa Paybill account number.
- Because the MAC is the ONT's, replacing an ONT changes the MAC: the admin edits the account's MAC (or attaches the new MAC from Unregistered devices) and the router user moves to the new ONT.

## Installing on the RB5009

You need: RouterOS 7 with the **container** extra package and a USB stick. For the first test you do not need the OLT: use the bench port (next section).

1. **Build**: `./build.sh` produces `dist/ispbill-arm64.tar`. (Or use the one in the zip.)
2. **Enable container mode** (physical access needed): install the `container` package for your RouterOS version (upload the `.npk`, reboot), then `/system/device-mode/update container=yes hotspot=yes fetch=yes` (7.20 gates all three) and press the reset button / power-cycle when the router asks. Check `/system/device-mode/print`.
3. **USB stick**: format it ext4 on the router: `/disk/print`, then `/disk/format usb1 file-system=ext4 label=usb1` (7.20; older versions use `format-drive`; check the disk name first, this erases it). Then pin the name so a reboot cannot flip it to `usb2`: `/disk/settings/set default-mount-point-template="[fs-label]"` and `/disk/set [find] mount-point-template="[fs-label]"`.
   **Clock**: the RB5009 has no clock battery. Set `/system/clock/set time-zone-name=Africa/Nairobi`, enable NTP, and until WAN is connected set date and time by hand after every reboot.
4. **Upload**: put `ispbill-arm64.tar` on `usb1`. Upload the folder `routeros/hotspot` as `hotspot-ispbill` on the router root (WinBox Files, FTP, or `scp -r`). See `routeros/hotspot/README.txt`.
5. **Edit `routeros/setup.rsc`**: replace every `CHANGE_ME_*` (router API password in two places, admin password) and check `out-interface-list=WAN` matches your config. Paste it **section by section** into the terminal; read the output after each. The values you may need to change (`sfp-sfpplus1`, `100`, `ether5`) are written literally and marked `>>> CHANGE IF NEEDED <<<`; there are no variables to define. Read the notes above the container section about `/container/envs` and `/file add type=directory` property names on your RouterOS version. Sections: container network and firewall, REST user and service, env list and container, customer network (vlan100-customers on sfp-sfpplus1 in br-hotspot), hotspot and walled garden, then the optional traffic-flow section (5b) for the metrics page.
6. **Start**: `/container/print` until the status is `stopped`, then `/container/start [find comment=ispbill]`. Watch `/log/print where topics~"container"` for "starting ...".
7. **Check**: from the router, `/tool/fetch url=http://172.17.0.2:8080/healthz output=user` should return `"router_ok":true`. `router_ok:false` means the REST user, port 8081 or the firewall input rules are wrong.
8. **Test flow**: see "Bench test" below, then, with the OLT cabled, repeat from an ONT-connected flat.

## Bench test with ether5 (no OLT needed)

1. In section 4 of `setup.rsc`, uncomment the `/interface/bridge/port add ... interface=ether5` line (remove ether5 from any other bridge first). It joins `br-hotspot` next to the OLT VLAN.
2. Plug a laptop directly into ether5, or a spare router/AP (in bridge/AP mode, or a router in route mode to imitate an ONT) and connect a phone to it.
3. The device gets a 10.5.50.x address. Open http://10.5.50.1 (with no WAN, outside names like neverssl.com do not resolve and the phone shows a DNS error instead of the portal; once WAN is up any http:// site redirects).
4. First create an account in Admin -> Accounts with the MAC of the device (or ONT/router) on ether5; the Unregistered devices page shows it once it has opened the portal. Then pick a package, enter a phone number such as 0712345678, tap "Test payment", wait about 3 s. The success page shows the flat, account code and expiry (EAT), and the device is online. With a route-mode router on ether5, every device behind it is online too.
5. Check Admin -> Accounts, and `/ip/hotspot/active/print` and `/ip/hotspot/user/print` on the router.
6. Expiry test: in Admin -> Packages add a package of 2 minutes, buy it, and watch the user disappear within about 3 minutes.

Admin access: the admin UI is served by the same port as the portal. Hotspot clients can technically open `/admin` too (it needs the password). For production, block `/admin` from the hotspot network with a firewall rule or keep the container reachable only from your management LAN.

## Vouchers

Admin -> Vouchers -> pick package and count (up to 500) -> "Generate and print". You get a printable page of 8-character codes (no 0/O/1/I/L). A registered flat enters the code under "Have a voucher?". A voucher works once; redeeming it creates a ledger entry with method `voucher` (not counted as revenue).

## Switching M-Pesa on later

1. Get a Paybill/Till, then create a Daraja app at developer.safaricom.co.ke. Use the **sandbox** first (`MPESA_ENV=sandbox`, test shortcode 174379 and the passkey shown in the Daraja simulator).
2. Make the container reachable from the internet over **HTTPS** at `PUBLIC_BASE_URL` (a public IP plus a TLS reverse proxy, or a tunnel such as Cloudflare Tunnel). The app itself speaks plain HTTP. Expose only `/pay/cb/*`, not `/admin`.
3. Open Settings > M-Pesa and save the environment, consumer key and secret, shortcode, passkey and public URL (or put them in the environment file, `MPESA_*`, as a fallback). `/healthz` then shows `mpesa_enabled:true` and the portal shows "Pay with M-Pesa". Set `TEST_MODE=false`.
4. Callback paths. Daraja rejects URLs containing "mpesa" or "safaricom", so the app uses:
   - STK Push result: `PUBLIC_BASE_URL/pay/cb/stk` (sent automatically with each push)
   - C2B validation: `PUBLIC_BASE_URL/pay/cb/c2b/validate`
   - C2B confirmation: `PUBLIC_BASE_URL/pay/cb/c2b/confirm`

   Each carries the callback token, in the path (`/pay/cb/t/<token>/...`) or as `?t=<token>`. Settings > M-Pesa shows the URLs and reveals the real token after you give the admin password.
5. Press **Register the C2B URLs** on Settings > M-Pesa (it calls Daraja's C2B Register URL with `ResponseType` Completed). See the go-live checklist under "Settings pages" for what Daraja refuses.
6. Customers can then also pay the Paybill directly with the flat's account code as the account number. The app extends that account by the package whose price equals the amount. Anything it cannot match lands in Admin -> Unmatched. Keep prices unique.

STK Push uses the account code as AccountReference; Daraja usually also sends a C2B confirmation for it, and the app de-duplicates by receipt number. Until the credentials are set the Daraja code stays disabled; use Admin -> Record M-Pesa for payments you see on the statement.

## Backup

Everything important is in `ispbill.json` under the data folder (`usb1/ispbill-data` for the container); the `metrics/` folder is history that is nice to keep but can be lost. On a VPS with PostgreSQL the data is in the database: the installer sets up a verified `pg_dump` every 6 hours, a monthly restore drill (restore into a scratch database and compare with production) and an encrypted weekly upload to Google Drive (see [deploy/BACKUP.md](deploy/BACKUP.md)); `sudo ftnbilling-doctor` shows their status. Copy both off the USB stick regularly (`/file` download, scp, or a scheduled `/tool/fetch` upload). Restore by putting them back and restarting the container. Writes are atomic, so a live copy is consistent.

## Known limitations

- **Not tested on a real RouterOS device, a real Daraja account or a real OLT.** The REST calls follow the RouterOS 7 documentation; the first real run may need small fixes, especially container, hotspot and traffic-flow settings in `setup.rsc` and the exact field names RouterOS returns for hosts, queues and `/system/health`.
- JSON file storage: fine for a router, not for thousands of users. All data is held in memory.
- One router and one hotspot server per router. Each account is locked to one MAC (the ONT WAN); `shared-users=1`. Flat isolation relies on the OLT configuration, which this project cannot check.
- No refunds on the router side, no pro-rata upgrades, no SMS or receipts (reminders are log events, and WhatsApp messages when WhatsApp is set up).
- Daraja callbacks are unsigned. `MPESA_CALLBACK_TOKEN` is required. To also restrict the source IPs, run the installer with `CADDY_MPESA_IPS="<ip/cidr> ..."`; get the current Safaricom callback ranges from Safaricom (they are not hard-coded because a wrong list would drop real payments). Caddy must be the only reverse proxy in front of the app, because the app trusts `X-Forwarded-For` from loopback.
- The portal is plain HTTP inside the router. The admin UI has no login rate limit and no HTTPS; keep it off the open internet and block `/admin` from the hotspot network.
- Test payments count in "revenue" while `TEST_MODE=true`.
- Metrics are estimates: see the NetFlow caveats above. The WAN figures are the router's interface counters, not the ISP's invoice.

## Development

```sh
go vet ./... && go test ./...
./build.sh                      # also writes dist/ispbill-arm64.tar
build/mkimage -verify dist/ispbill-arm64.tar
```
Layout: `cmd/ispbill` (main), `internal/config`, `store` (JSON file), `routeros` (REST client + mock), `billing` (rules, sweeper), `payments` (test, voucher, Daraja), `web` (portal, admin, callbacks, embedded templates), `tools/mkimage` (docker-save tarball builder), `routeros/` (setup script and hotspot pages).
