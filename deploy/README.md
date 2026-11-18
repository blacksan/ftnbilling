# Online billing server (VPS + Tailscale)

The same app that runs as a container on the RB5009 can run on a VPS instead.
The router keeps doing the hotspot; the VPS does billing, the portal and the
admin pages.

```
 Flats ── ONT ── OLT ── RB5009 (hotspot, no public IP) ── Safaricom ── internet
                           │  Tailscale container 172.17.0.3                │
                           │   ◄── tailnet: VPS → 172.17.0.1:8081 (REST) ──►│ VPS
                           │                                                │  ftnbilling :8080
                           └── phones (walled garden) ── https://DOMAIN ───►│  Caddy :80/:443
                           └── NetFlow UDP 2055 ───────────────────────────►│
```

| Path | How |
|---|---|
| VPS → router REST API (create users, log flats in, read counters) | Tailscale. The router has no public IP, so the VPS reaches `172.17.0.1:8081` through the `ftn-router` subnet route. |
| Phones → portal | Public internet, `https://DOMAIN/portal`. The hotspot walled garden lets unpaid phones reach only the VPS IP. |
| Router → NetFlow | Public internet, UDP 2055. This is needed because Tailscale on RouterOS 7.20 runs in userspace mode and cannot carry traffic *from* the router. |
| You → admin | Tailscale only: `http://ftn-billing:8080/admin`. Caddy refuses `/admin` from the internet. |

Because the portal is now public, the server only trusts a `mac`/`ip` pair
that the router currently has in `/ip/hotspot/host` (`VERIFY_HOTSPOT_HOST=true`).
Someone outside the building who guesses a flat's MAC gets the neutral "open
this page from the WiFi login" page.

## 1. Before you start

1. **VPS:** Ubuntu 22.04/24.04 or Debian 12, with 1 vCPU and 1 GB RAM. Any Nairobi or EU provider works.
2. **Domain (recommended):** add an A record, e.g. `billing.yourdomain.co.ke`, pointing to the VPS IP. You need it for HTTPS, and M-Pesa callbacks need HTTPS.
3. **Tailscale:** at https://login.tailscale.com/admin/settings/keys create **two** auth keys, one for the VPS and one for the router. "Reusable" is not needed. Turn on "Pre-approved" if your tailnet uses device approval.
4. **GitHub token:** the repo is private, so the VPS needs a token to download it. In GitHub, go to Settings → Developer settings → Fine-grained tokens. Give it access to `blacksan/ftnbilling` only, with **Contents: Read-only**.

## 2. Install on the VPS (one line)

```bash
GH=github_pat_xxx; curl -fsSL -H "Authorization: Bearer $GH" https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh | sudo GH_TOKEN=$GH TS_AUTHKEY=tskey-auth-VPSKEY ROUTER_PASS='router-api-pass' DOMAIN=billing.yourdomain.co.ke bash
```

Leave out `DOMAIN=...` to run on `http://<VPS IP>` for now.

The installer does the following:
- Installs Go and builds the app from this repo.
- Installs it as the `ftnbilling` systemd service, with data in `/var/lib/ftnbilling` and settings in `/etc/ftnbilling/ftnbilling.env`.
- Joins Tailscale as `ftn-billing` with `--accept-routes`.
- Sets up Caddy as the public proxy, with automatic HTTPS when you give `DOMAIN`.
- Sets up the ufw firewall: SSH, 80, 443 and UDP 2055 are open; everything else is reachable only over Tailscale.
- Adds daily backups to `/var/backups/ftnbilling`, kept for 30 days.

At the end it prints the admin password and the router commands.

**Upgrading:** run the same line again; the Tailscale key is not needed. Data and settings are kept.

## 3. Connect the router (one paste)

Use a terminal on the RB5009 (WAN up, USB stick mounted as `usb1`):

```
:global tskey "tskey-auth-ROUTERKEY"
/tool/fetch url="https://billing.yourdomain.co.ke/routeros/remote.rsc" dst-path=ftn-remote.rsc
/import ftn-remote.rsc
```

The script does the following:
- Starts a Tailscale container at 172.17.0.3, pulled from Docker Hub.
- Lets that container (and only it) use the REST API.
- Downloads hotspot login pages that point at the online portal.
- Whitelists the VPS IP in the walled garden.
- Sends NetFlow to the VPS.
- **Stops the local ispbill container**, so two billing servers never fight over the hotspot users.

Then, at https://login.tailscale.com/admin/machines:
- **ftn-router** → ⋯ → *Edit route settings* → approve **172.17.0.1/32**.
- **ftn-router** and **ftn-billing** → ⋯ → *Disable key expiry*.

## 4. Check

- On the VPS, `curl -s localhost:8080/healthz` should show `"router_ok":true`.
- On your laptop, joined to the same tailnet, open `http://ftn-billing:8080/admin`.
- On a phone on the building WiFi, the sign-in page should open the online portal.

| Symptom | Fix |
|---|---|
| `router_ok:false` | Is the route approved? Run `tailscale status` on the VPS: ftn-router must be online. On the router, `/container/print` should show tailscale running. Check `ROUTER_PASS`. |
| Phone shows a DNS or connection error instead of the portal | Check that the walled garden has the VPS IP: `/ip/hotspot/walled-garden/ip/print`. Check that the domain resolves. |
| Portal says "open this page from the WiFi login" on a building phone | The router's hotspot host table must list the phone's MAC/IP: `/ip/hotspot/host/print`. |
| No "Services" charts | Check `/ip/traffic-flow/target/print` (VPS IP, port 2055) and `ufw status`. |

## Going back to the router container

On the router:
1. Set the local container to start on boot and start it: `/container/set [find comment=ispbill] start-on-boot=yes` and `/container/start [find comment=ispbill]`.
2. Re-upload the original `routeros/hotspot` pages.
3. Remove the walled-garden entry `ftn: online portal`.

## Moving data

The two servers do not share data. To move flats and payments from the router to the VPS:
1. Stop both apps.
2. Copy `usb1/ispbill-data/ispbill.json` from the router (Winbox → Files → drag to your PC) to `/var/lib/ftnbilling/ispbill.json` on the VPS.
3. Run `chown ftnbilling: /var/lib/ftnbilling/ispbill.json`.
4. Run `systemctl start ftnbilling`.
