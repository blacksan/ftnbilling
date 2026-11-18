#!/usr/bin/env bash
# Fast Tech Net billing server: one-line install / upgrade on a Debian or
# Ubuntu VPS. Safe to run again: it upgrades the app and keeps data + settings.
#
#   curl -fsSL -H "Authorization: Bearer $GH_TOKEN" \
#     https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh \
#     | sudo GH_TOKEN=$GH_TOKEN TS_AUTHKEY=tskey-auth-... ROUTER_PASS=... DOMAIN=billing.example.co.ke bash
#
# Settings (environment variables):
#   GH_TOKEN     GitHub token with read access to the repo (it is private).
#   TS_AUTHKEY   Tailscale auth key (first install only; reusable not needed).
#   ROUTER_PASS  password of the "ispbill" REST user on the MikroTik.
#   DOMAIN       optional, e.g. billing.fasttech.co.ke (DNS A record -> this VPS).
#                With it you get HTTPS (needed for M-Pesa later). Without it the
#                portal is served on http://<VPS IP>.
#   ADMIN_PASS   optional; a random one is generated on first install.
#   SUPPORT_PHONE, BRAND_NAME  optional.
#   REPO (blacksan/ftnbilling), BRANCH (main), NO_FIREWALL=1 to skip ufw.
set -euo pipefail

REPO="${REPO:-blacksan/ftnbilling}"
BRANCH="${BRANCH:-main}"
APP=ftnbilling
SRC=/opt/$APP/src
DATA=/var/lib/$APP
ETC=/etc/$APP
ENVF=$ETC/$APP.env
BIN=/usr/local/bin/$APP
TS_HOST="${TS_HOSTNAME:-ftn-billing}"

rand() { local s; s=$(head -c 96 /dev/urandom | base64 -w0 | tr -dc 'A-Za-z0-9'); printf '%s' "${s:0:$1}"; }
say()  { printf '\n\033[1;33m==> %s\033[0m\n' "$*"; }
die()  { printf '\n\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root (pipe into: sudo ... bash)"
. /etc/os-release 2>/dev/null || true
case "${ID:-}${ID_LIKE:-}" in *debian*|*ubuntu*) ;; *) die "Debian or Ubuntu is required (found ${PRETTY_NAME:-unknown})";; esac
case "$(uname -m)" in x86_64) GOARCH=amd64;; aarch64|arm64) GOARCH=arm64;; *) die "unsupported CPU $(uname -m)";; esac
export DEBIAN_FRONTEND=noninteractive

say "1/8 packages"
apt-get update -qq
apt-get install -y -qq curl git ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https ufw >/dev/null

say "2/8 source code ($REPO@$BRANCH)"
AUTH=()
if [ -n "${GH_TOKEN:-}" ]; then
  AUTH=(-c "http.https://github.com/.extraheader=Authorization: Basic $(printf 'x-access-token:%s' "$GH_TOKEN" | base64 -w0)")
fi
if [ -d "$SRC/.git" ]; then
  git "${AUTH[@]}" -C "$SRC" fetch -q --depth 1 origin "$BRANCH" || die "git fetch failed (is GH_TOKEN set and valid?)"
  git -C "$SRC" reset -q --hard FETCH_HEAD
else
  mkdir -p "$(dirname "$SRC")"
  git "${AUTH[@]}" clone -q --depth 1 --branch "$BRANCH" "https://github.com/$REPO.git" "$SRC" \
    || die "git clone failed. The repo is private: pass GH_TOKEN=<token with Contents: read>"
fi
REV=$(git -C "$SRC" rev-parse --short HEAD)

say "3/8 Go toolchain + build ($REV)"
NEED=$(awk '/^go /{print $2}' "$SRC/go.mod")
have_go() { command -v /usr/local/go/bin/go >/dev/null && /usr/local/go/bin/go version | awk '{print $3}' | sed 's/go//' ; }
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }
CUR=$(have_go || true)
if [ -z "$CUR" ] || ! ver_ge "$CUR" "$NEED"; then
  GOV=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1)
  curl -fsSL "https://go.dev/dl/${GOV}.linux-${GOARCH}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
fi
( cd "$SRC" && CGO_ENABLED=0 GOFLAGS=-trimpath /usr/local/go/bin/go build -ldflags="-s -w" -o "$BIN.new" ./cmd/ispbill )
mv -f "$BIN.new" "$BIN"

say "4/8 user, folders, settings"
id -u $APP >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin $APP
install -d -o $APP -g $APP -m 750 "$DATA"
install -d -o root -g $APP -m 750 "$ETC"
PUBIP=$(curl -4 -fsS --max-time 10 https://api.ipify.org || curl -4 -fsS --max-time 10 https://ifconfig.me || true)
[ -n "$PUBIP" ] || die "could not detect this VPS's public IPv4"
if [ -n "${DOMAIN:-}" ]; then BASE="https://$DOMAIN"; else BASE="http://$PUBIP"; fi
FIRST=0
if [ ! -f "$ENVF" ]; then
  FIRST=1
  ADMIN_PASS="${ADMIN_PASS:-$(rand 18)}"
  cat >"$ENVF" <<EOF
# Fast Tech Net billing server settings. Edit, then: systemctl restart $APP
BRAND_NAME=${BRAND_NAME:-Fast Tech Net}
LISTEN_ADDR=:8080
DATA_DIR=$DATA
TEST_MODE=false

# MikroTik REST API, reached over Tailscale (subnet route 172.17.0.1/32 via ftn-router)
ROUTER_URL=http://172.17.0.1:8081
ROUTER_USER=ispbill
ROUTER_PASS=${ROUTER_PASS:-CHANGE_ME}
HOTSPOT_SERVER=hotspot1

# Admin (only reachable over Tailscale: http://$TS_HOST:8080/admin)
ADMIN_USER=admin
ADMIN_PASS=$ADMIN_PASS

# Public portal
PUBLIC_BASE_URL=$BASE
PUBLIC_IP=$PUBIP
VERIFY_HOTSPOT_HOST=true
SUPPORT_PHONE=${SUPPORT_PHONE:-}

# Metrics: the router sends NetFlow here over the internet
NETFLOW_ADDR=:2055
CUSTOMER_SUBNET=10.5.50.0/24

# M-Pesa (Daraja) - fill in when the Paybill is ready; callbacks need DOMAIN (HTTPS)
MPESA_ENV=sandbox
MPESA_CONSUMER_KEY=
MPESA_CONSUMER_SECRET=
MPESA_SHORTCODE=
MPESA_PASSKEY=
MPESA_CALLBACK_TOKEN=$(rand 24)
EOF
else
  # Upgrade: keep settings, refresh what the installer owns.
  setkv() { if grep -q "^$1=" "$ENVF"; then sed -i "s|^$1=.*|$1=$2|" "$ENVF"; else echo "$1=$2" >>"$ENVF"; fi; }
  setkv PUBLIC_IP "$PUBIP"
  [ -n "${DOMAIN:-}" ] && setkv PUBLIC_BASE_URL "$BASE"
  [ -n "${ROUTER_PASS:-}" ] && setkv ROUTER_PASS "$ROUTER_PASS"
  [ -n "${ADMIN_PASS:-}" ] && setkv ADMIN_PASS "$ADMIN_PASS"
  grep -q '^VERIFY_HOTSPOT_HOST=' "$ENVF" || echo "VERIFY_HOTSPOT_HOST=true" >>"$ENVF"
  BASE=$(grep '^PUBLIC_BASE_URL=' "$ENVF" | cut -d= -f2-)
fi
chown root:$APP "$ENVF"; chmod 640 "$ENVF"

cat >/etc/systemd/system/$APP.service <<EOF
[Unit]
Description=Fast Tech Net billing server
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
User=$APP
Group=$APP
EnvironmentFile=$ENVF
ExecStart=$BIN
Restart=always
RestartSec=3
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=$DATA
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF

say "5/8 Tailscale"
if ! command -v tailscale >/dev/null; then curl -fsSL https://tailscale.com/install.sh | sh >/dev/null; fi
systemctl enable --now tailscaled >/dev/null
if tailscale status >/dev/null 2>&1; then
  tailscale set --accept-routes=true --hostname="$TS_HOST"
else
  [ -n "${TS_AUTHKEY:-}" ] || die "TS_AUTHKEY is required on the first install (Tailscale admin > Settings > Keys)"
  tailscale up --authkey="$TS_AUTHKEY" --hostname="$TS_HOST" --accept-routes
fi
TSIP=$(tailscale ip -4 | head -1)

say "6/8 Caddy (public portal${DOMAIN:+ with HTTPS for $DOMAIN})"
if ! command -v caddy >/dev/null; then
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' >/etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi
SITE="${DOMAIN:-:80}"
cat >/etc/caddy/Caddyfile <<EOF
# Public side of the billing server. The admin pages are NOT published here:
# open them over Tailscale at http://$TS_HOST:8080/admin
$SITE {
	encode gzip
	@admin path /admin /admin/*
	respond @admin "Admin is only available over Tailscale" 403
	reverse_proxy 127.0.0.1:8080
}
EOF
systemctl enable caddy >/dev/null; systemctl reload caddy 2>/dev/null || systemctl restart caddy

say "7/8 firewall + daily backups"
if [ "${NO_FIREWALL:-0}" != 1 ]; then
  SSHP=$(sshd -T 2>/dev/null | awk '/^port /{print $2; exit}'); SSHP=${SSHP:-22}
  ufw allow "$SSHP"/tcp >/dev/null
  ufw allow 80/tcp >/dev/null; ufw allow 443/tcp >/dev/null
  ufw allow 2055/udp >/dev/null                       # NetFlow from the router
  ufw allow in on tailscale0 >/dev/null               # admin :8080 over Tailscale only
  ufw allow 41641/udp >/dev/null                      # Tailscale direct connections
  ufw default deny incoming >/dev/null; ufw --force enable >/dev/null
fi
cat >/etc/cron.daily/$APP-backup <<EOF
#!/bin/sh
# Keeps 30 days of billing data backups in /var/backups/$APP
mkdir -p /var/backups/$APP
tar -czf /var/backups/$APP/$APP-\$(date +%F).tgz -C $DATA . 2>/dev/null
find /var/backups/$APP -name '*.tgz' -mtime +30 -delete
EOF
chmod 755 /etc/cron.daily/$APP-backup

say "8/8 start"
systemctl daemon-reload
systemctl enable $APP >/dev/null
systemctl restart $APP
sleep 3
systemctl is-active --quiet $APP || { journalctl -u $APP -n 30 --no-pager; die "service failed to start (settings in $ENVF)"; }
HEALTH=$(curl -fsS --max-time 5 http://127.0.0.1:8080/healthz || true)

APASS=$(grep '^ADMIN_PASS=' "$ENVF" | cut -d= -f2-)
cat <<EOF

────────────────────────────────────────────────────────────────────
 Fast Tech Net billing is running ($REV)

 Portal (public):   $BASE/portal
 Admin (Tailscale): http://$TS_HOST:8080/admin   or http://$TSIP:8080/admin
 Admin login:       admin / $APASS
 Settings:          $ENVF   (then: systemctl restart $APP)
 Logs:              journalctl -u $APP -f
 Health:            $HEALTH

 Next, on the MikroTik terminal (connects the router; safe to re-run):

   :global tskey "tskey-auth-PASTE-A-KEY"
   /tool/fetch url="$BASE/routeros/remote.rsc" dst-path=ftn-remote.rsc
   /import ftn-remote.rsc

 Then in the Tailscale admin console (login.tailscale.com/admin/machines):
   - ftn-router: Edit route settings -> approve 172.17.0.1/32
   - ftn-router and $TS_HOST: Disable key expiry
────────────────────────────────────────────────────────────────────
EOF
[ "$FIRST" = 1 ] && grep -q '^ROUTER_PASS=CHANGE_ME' "$ENVF" && echo " NOTE: set ROUTER_PASS in $ENVF, then systemctl restart $APP"
exit 0
