# ispbill

An internet-billing server for ISPs that serve flats over IPoE behind MikroTik
routers (RouterOS 7). It bills one account per flat, takes M-Pesa and voucher
payments on a public payment page, and keeps each router in step with the
bills over the router's REST API: a fixed DHCP lease and a speed queue per
flat, and a walled address list for flats that have not paid. Go, vendored
dependencies, PostgreSQL (or a JSON file for small installs), one static binary.
It runs on a VPS; each router joins it over WireGuard. See
[deploy/README.md](deploy/README.md) for the one-command install and
[docs/plan-v5-bootstrap.md](docs/plan-v5-bootstrap.md) for how a router is set up.

> **TEST BUILD.** With `TEST_MODE=true` the payment page has a "Test payment"
> button that grants free access to anyone who taps it. Never leave it on for
> real customers. The M-Pesa (Daraja) code stays disabled until its credentials are set.

## What it does

- **One account per flat.** The admin creates an account: the apartment (building) it is in, the unit (e.g. "Flat 3B"), the ONT's WAN MAC (required, unique across all routers and apartments: one MAC, one open account), optional tenant, phone and notes. The app generates a 6-character code (no 0/O/1/I/L) that never changes and is also the **Paybill account number**. The flat also gets a fixed address from the router's customer subnet.
- **Every router is IPoE and set up by the server.** The router page gives one line to paste on a new router; the router erases itself and builds the whole layout (WAN, admin LAN, customer network, DHCP, firewall, tunnel, the server's API login). After that the server only changes the flats' leases, queues, address lists and ARP entries. It never writes users, services, the tunnel, routes or the WAN.
- **Payment is by account number.** An unpaid or expired flat is walled by the router: it can reach only the payment page at `/pay`, where the tenant enters the account number (or the flat label when the visit comes from a known router), picks a package and pays by M-Pesa or enters a voucher. The server un-walls the flat within about a minute. Phone numbers are only the M-Pesa payer; they are not identities.
- **Prices are plain KES.** No tax lines or tax wording anywhere.
- Admin UI at `/admin` (sign-in with an authenticator code): Overview, Subscribers (create, edit, change MAC, suspend, extend), New devices, Payments (ledger plus CSV), Record M-Pesa, Unmatched, Routers, Plans, Vouchers, Usage and quality, Settings.
- `/healthz` returns `{ok, router_ok, test_mode, mpesa_enabled}`.

### New devices

Admin, New devices lists MACs the routers have seen that match no account (from the DHCP leases and the ARP table, read only). For each you can **create an account** for that MAC (enter the flat label) or **attach** it to an existing account (an ONT swap: the account's MAC changes and the router's lease follows). Changing a MAC by hand on the account page does the same.

### Payments, ledger and reconciliation

- Every payment is a ledger row with a unique key (M-Pesa receipt code, `VOUCHER-x`, `TEST-x`, `ADMIN-x`). Re-delivered callbacks and re-entered codes are ignored, so nothing is credited twice.
- Paybill payments (C2B) are matched by account code (`BillRefNumber`) and then to the package whose price equals the amount. STK push uses the account code as `AccountReference`. Old order ids are still accepted.
- Anything that cannot be matched (unknown account, amount that fits no package) goes to Admin -> Unmatched. Resolve it with **Assign** (pick the account, the package is applied), **Refunded** or **Ignored**, each with a note.
- **Admin -> Record M-Pesa** is for payments seen on the M-Pesa statement that never reached the app: pick the account, enter the 10-character code (uppercase letters and digits), amount, payer phone and date; the package is preselected from the amount. It applies exactly like an automatic payment, and a code can be recorded once.
- Router updates (a flat's lease, queue and walled state) are queued per flat, answered by one reconcile pass per router, retried with backoff (5 s doubling to 5 min), kept across restarts and shown on the dashboard. A paused or not yet set up router keeps its tasks without using attempts. If the router is down when someone pays, the payment is safe and the router catches up when it returns.
- Lifecycle defaults (shown in Settings): the expiry is calendar based; paying while active adds time to the current expiry; an upgrade (same or higher speed) switches the speed immediately and counts from the current expiry (no pro-rata credit); a **downgrade is not allowed while a plan is active** (the payment page greys out slower packages with "Available when your current plan ends on <date>", checkout refuses them, and Paybill or manual payments for a slower package are put in Unmatched with the reason "lower plan while active" for the admin to decide; the admin extend and Record M-Pesa forms have an "apply even if lower speed" box); expired or suspended accounts can buy any package; an expired flat reconnects automatically when it pays; there is no reconnection fee. Reminder hook points are logged as events 3 days before expiry and on expiry (`REMINDER_DAYS`, `REMINDER_ON_EXPIRY`); no SMS is sent, they only create log events for a future sender.

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
| Simple queues | 60 s | Per-flat bytes (the difference between readings of each flat's queue counters; a counter reset counts from the new value), package speed and queue state, for "time at cap". |
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
- Fasttracked connections skip traffic-flow entirely. The bootstrap script keeps the customer rules above FastTrack; if you add a FastTrack rule by hand, traffic is missing.
- Flow records leave the router only at the flow timeouts, so the newest minute is incomplete.
- Per-flat totals do not use NetFlow, so they are not affected by any of this.
Edit the rules table (`ServiceRules` and `PortRules` in `internal/metrics/netflow.go`) to add services and rebuild.

## Quick local run (no router)

```sh
./build.sh                       # tests, builds, image
ROUTER_MOCK=true TEST_MODE=true ADMIN_PASS=change-me-please DATA_DIR=/tmp/ispbill \
  LISTEN_ADDR=127.0.0.1:8080 DATA_KEY=$(openssl rand -base64 32) build/ispbill-amd64
```
Open http://127.0.0.1:8080/admin, sign in, add a router (the mock serves every router) and a flat, then open http://127.0.0.1:8080/pay, enter the flat's account number and tap "Test payment" (it completes after about 3 s).

## Many routers

One server bills many routers. Account codes are global (they are the Paybill
account numbers) and so are ONT MACs: one MAC can have only one open account, on
any router in any building. A unit label (flat) only has to be unique inside its
apartment. Packages are shared, and each router can hide a plan or give it a
different price (Admin, Routers, the router page). Prices must stay unique
inside a router because Paybill payments are matched by amount. The admin has a
router switcher in the top bar (`All routers` or one) and list pages gain a
Router column when all are shown. An account can be moved to another router
from its page.

### Apartments

An apartment is a building that one or more routers serve (and a router may serve
several apartments). Each account belongs to an apartment and has a unit in it;
the apartment page holds the caretaker and landlord contacts, its routers and its
customers. Creating a customer needs the apartment; with one router it is chosen
for you, with several you pick. A device seen on a router that does not serve its
customer's apartment is not provisioned there (it stays walled and is listed under
New devices, where you can move the customer). Rules, calls and error codes:
[docs/api/v6-apartments.md](docs/api/v6-apartments.md).


Routers join over WireGuard with a one-time line pasted on the router: see
[deploy/README.md](deploy/README.md) for the server setup (PostgreSQL, Caddy,
WireGuard, a public admin behind the app's own sign-in, firewall, backups,
`ftnbilling-doctor`) and for the paste flow. The design target is about 40
routers and 1500 flats on a 1 vCPU / 2 GB VPS.

Per-router REST traffic is budgeted: at most two reads and one write in flight
per router, 10 s per call (`ROUTER_READ_TIMEOUT` sets the read limit).

The settings for the tunnel are `WG_PUBLIC_KEY`, `WG_ENDPOINT` (default
`PUBLIC_IP:51820`), `WG_SUBNET` (`10.99.0.0/22`), `MGMT_SUBNET` (`10.98.0.0/24`,
the remote-WinBox devices' network; the installer also takes `MGMT_PORT`, UDP
`51821`; see `deploy/README.md`, Remote WinBox), `WG_DUMP_PATH`,
`JOIN_TOKEN_TTL` (`30m`), `ALLOW_INSECURE_JOIN` (bench tests only) and
`JOIN_REQUIRE_MIKROTIK_UA` (`true`).

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
| `ROUTER_MOCK` | `false` | one fake router answers for every router (development) |
| `ADMIN_USER` / `ADMIN_PASS` | `admin` / required until a password is saved in Settings | app refuses to start if there is no saved password and PASS is empty or `admin` |
| `SUPPORT_PHONE` | | shown on the payment page |
| `PUBLIC_BASE_URL` | | needed for M-Pesa callbacks |
| `MPESA_ENV` | `sandbox` | or `production`. The M-Pesa, WhatsApp and email variables are a fallback: Settings wins once you save there |
| `MPESA_CONSUMER_KEY`, `MPESA_CONSUMER_SECRET`, `MPESA_SHORTCODE`, `MPESA_PASSKEY` | | all four (with `PUBLIC_BASE_URL`) switch M-Pesa on |
| `MPESA_CALLBACK_TOKEN` | | required once the M-Pesa key, secret and shortcode are set (the app refuses to start without it): callbacks must carry the token (`?t=<token>` or `/pay/cb/t/<token>/...`). Settings > M-Pesa makes one when you save there |
| `REMINDER_DAYS` | `3` | days before expiry for the reminder event (0 = off) |
| `REMINDER_ON_EXPIRY` | `true` | log an event when an account expires |
| `WAN_INTERFACE` / `OLT_INTERFACE` | `ether1` / `sfp-sfpplus1` | defaults for a new router's layout (with the two capacity values, `FUP_TB`, `CUSTOMER_SUBNET`); each router has its own on its page |
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
Safaricom fibre/line --> router ether1 (WAN)
                         router sfp-sfpplus1 --10G DAC--> OLT (pure Layer 2)
                                                           PON --> splitter --> fibre --> ONT per flat
                                                                                      (ONT in ROUTE mode, WAN = DHCP)
router br-customers  10.5.50.0/24 : customer VLAN from the OLT [+ optional bench port]
router wg-ftn        10.99.0.x    : the tunnel to the billing server
```

- Billing and account control is on the server; the router holds the flats' leases, queues and the walled list. The OLT only switches frames.
- OLT side: service VLAN tagged on the OLT uplink; each ONT's WAN port untagged on the ONT and translated to the VLAN on the OLT; ONT-to-ONT isolation enabled so flats cannot see each other (the router also drops flat-to-flat traffic).
- Each ONT is in **route mode** (NAT and Wi-Fi) with a DHCP WAN. The router therefore sees **one client per flat**: the ONT's WAN MAC. The flat's lease is a static one on `ftn-dhcp`, so it always gets its fixed address; unknown MACs get an address from the walled range (.10 to .59).
- The account code (6 characters) is the flat's account number and the M-Pesa Paybill account number.
- Because the MAC is the ONT's, replacing an ONT changes the MAC: the admin edits the account's MAC (or attaches the new MAC from New devices) and the lease moves to the new ONT.

## Setting up a router

See [deploy/README.md](deploy/README.md), "Add a router". In short: Routers, Add router, paste the one line on the router, watch the four steps on the router page turn green. The optional bench port (a spare port on the customer bridge for testing as a flat) is a field of the router form; paste a new line after changing it.

## Vouchers

Admin -> Vouchers -> pick package and count (up to 500) -> "Generate and print". You get a printable page of 8-character codes (no 0/O/1/I/L). A tenant enters the code under "Have a voucher?" on the payment page, after naming their account. A voucher made for one router is refused at every other router. A voucher works once; redeeming it creates a ledger entry with method `voucher` (not counted as revenue).

## Switching M-Pesa on later

1. Get a Paybill/Till, then create a Daraja app at developer.safaricom.co.ke. Use the **sandbox** first (`MPESA_ENV=sandbox`, test shortcode 174379 and the passkey shown in the Daraja simulator).
2. The installer puts Caddy in front of the app with HTTPS at `PUBLIC_BASE_URL`; the app itself speaks plain HTTP on `127.0.0.1`.
3. Open Settings > M-Pesa and save the environment, consumer key and secret, shortcode, passkey and public URL (or put them in the environment file, `MPESA_*`, as a fallback). `/healthz` then shows `mpesa_enabled:true` and the payment page shows "Pay with M-Pesa". Set `TEST_MODE=false`.
4. Callback paths. Daraja rejects URLs containing "mpesa" or "safaricom", so the app uses:
   - STK Push result: `PUBLIC_BASE_URL/pay/cb/stk` (sent automatically with each push)
   - C2B validation: `PUBLIC_BASE_URL/pay/cb/c2b/validate`
   - C2B confirmation: `PUBLIC_BASE_URL/pay/cb/c2b/confirm`

   Each carries the callback token, in the path (`/pay/cb/t/<token>/...`) or as `?t=<token>`. Settings > M-Pesa shows the URLs and reveals the real token after you give the admin password.
5. Press **Register the C2B URLs** on Settings > M-Pesa (it calls Daraja's C2B Register URL with `ResponseType` Completed). See the go-live checklist under "Settings pages" for what Daraja refuses.
6. Customers can then also pay the Paybill directly with the flat's account code as the account number. The app extends that account by the package whose price equals the amount. Anything it cannot match lands in Admin -> Unmatched. Keep prices unique.

STK Push uses the account code as AccountReference; Daraja usually also sends a C2B confirmation for it, and the app de-duplicates by receipt number. Until the credentials are set the Daraja code stays disabled; use Admin -> Record M-Pesa for payments you see on the statement.

## Backup

Everything important is in `ispbill.json` under the data folder; the `metrics/` folder is history that is nice to keep but can be lost. On a VPS with PostgreSQL the data is in the database: the installer sets up a verified `pg_dump` every 6 hours, a monthly restore drill (restore into a scratch database and compare with production) and an encrypted weekly upload to Google Drive (see [deploy/BACKUP.md](deploy/BACKUP.md)); `sudo ftnbilling-doctor` shows their status. Copy both off the USB stick regularly (`/file` download, scp, or a scheduled `/tool/fetch` upload). Restore by putting them back and restarting the container. Writes are atomic, so a live copy is consistent.

## Known limitations

- **Not tested on a real RouterOS device, a real Daraja account or a real OLT.** The REST calls follow the RouterOS 7 documentation; the first real run may need small fixes, especially the bootstrap script on other RouterOS versions and the exact field names RouterOS returns for leases, queues and `/system/health`.
- JSON file storage: fine for a router, not for thousands of users. All data is held in memory.
- Each account is locked to one MAC (the ONT WAN) and one fixed address. Flat isolation relies on the OLT configuration, which this project cannot check.
- No refunds on the router side, no pro-rata upgrades, no SMS or receipts (reminders are log events, and WhatsApp messages when WhatsApp is set up).
- Daraja callbacks are unsigned. `MPESA_CALLBACK_TOKEN` is required. To also restrict the source IPs, run the installer with `CADDY_MPESA_IPS="<ip/cidr> ..."`; get the current Safaricom callback ranges from Safaricom (they are not hard-coded because a wrong list would drop real payments). Caddy must be the only reverse proxy in front of the app, because the app trusts `X-Forwarded-For` from loopback.
- Test payments count in "revenue" while `TEST_MODE=true`.
- Metrics are estimates: see the NetFlow caveats above. The WAN figures are the router's interface counters, not the ISP's invoice.

## Development

```sh
go vet ./... && go test ./...
./build.sh                      # also writes dist/ispbill-arm64.tar
build/mkimage -verify dist/ispbill-arm64.tar
```
Layout: `cmd/ispbill` (main), `internal/config`, `store` (JSON file), `routeros` (REST client + mock), `billing` (rules, sweeper), `payments` (test, voucher, Daraja), `web` (payment page, admin, callbacks, embedded templates), `sites` (router setup, checks, live views), `tools/mkimage` (docker-save tarball builder).
