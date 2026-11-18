#!/usr/bin/env bash
# Fast Tech Net billing server: one-line install / upgrade on a Debian or
# Ubuntu VPS. Safe to run again: it upgrades the app and keeps data + settings.
#
#   curl -fsSL -H "Authorization: Bearer $GH_TOKEN" \
#     https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh \
#     | sudo GH_TOKEN=$GH_TOKEN DOMAIN=billing.example.co.ke bash
#
# Routers connect to this server over WireGuard (UDP 51820). Each building is
# added in the admin (Buildings > Add building), which gives a one-line join
# command to paste into the router.
#
# Settings (environment variables):
#   GH_TOKEN     GitHub token with read access to the repo (it is private).
#   DOMAIN       e.g. billing.fasttech.co.ke (DNS A record -> this VPS). HTTPS is
#                needed for M-Pesa and for the router join command. Without it
#                the portal is served on http://<VPS IP> and join commands are
#                refused.
#   TS_AUTHKEY   optional Tailscale auth key. Tailscale is only for you to reach
#                the admin; without it use the SSH tunnel printed at the end.
#   ROUTER_PASS  only for a legacy single-router install that still uses
#                ROUTER_URL/ROUTER_PASS (kept on upgrade, never added on a new one).
#   ADMIN_PASS   optional; a random one is generated on first install.
#   SUPPORT_PHONE, BRAND_NAME  optional.
#   WG_PORT (51820), WG_SUBNET (10.99.0.0/22), REPO (blacksan/ftnbilling),
#   BRANCH (main), NO_FIREWALL=1 to skip ufw.
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
WG_IF=wg0
WG_PORT="${WG_PORT:-51820}"
WG_SUBNET="${WG_SUBNET:-10.99.0.0/22}"
WG_NET=${WG_SUBNET%/*}
WG_SERVER_IP=${WG_NET%.*}.1
WG_CONF=/etc/wireguard/$WG_IF.conf

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
apt-get install -y -qq curl git ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https ufw wireguard-tools >/dev/null

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
install -d -o $APP -g $APP -m 750 "$DATA/wg"
install -d -o root -g $APP -m 750 "$ETC"

# WireGuard server. The private key is generated once, lives only in
# $WG_CONF (root, mode 600) and is never regenerated: every router has this
# server's public key baked into its join script.
if [ ! -f "$WG_CONF" ]; then
  ( umask 077
    cat >"$WG_CONF" <<WGEOF
[Interface]
Address = $WG_SERVER_IP/${WG_SUBNET#*/}
ListenPort = $WG_PORT
MTU = 1380
PrivateKey = $(wg genkey)
PostUp = /usr/local/sbin/$APP-wg-sync
WGEOF
  )
fi
chmod 600 "$WG_CONF"
WG_PUB=$(awk '/^PrivateKey *=/{print $3}' "$WG_CONF" | wg pubkey)
[ -n "$WG_PUB" ] || die "could not read the WireGuard key from $WG_CONF"

# Root helper that applies the app's peer list (it validates the file, because
# the app is not root), and the status job the admin page reads handshakes from.
install -m 0755 "$SRC/deploy/$APP-wg-sync" /usr/local/sbin/$APP-wg-sync
cat >$ETC/wg-sync.env <<WGEOF
DATA=$DATA
WG_SUBNET=$WG_SUBNET
WG_IFACE=$WG_IF
WGEOF
chmod 644 $ETC/wg-sync.env
echo "d /run/$APP 0750 root $APP -" >/etc/tmpfiles.d/$APP.conf
systemd-tmpfiles --create /etc/tmpfiles.d/$APP.conf
cat >/etc/systemd/system/$APP-wg-sync.service <<WGEOF
[Unit]
Description=Apply the billing app's WireGuard peers
After=wg-quick@$WG_IF.service
Wants=wg-quick@$WG_IF.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/$APP-wg-sync
WGEOF
cat >/etc/systemd/system/$APP-wg-sync.path <<WGEOF
[Unit]
Description=Watch for WireGuard peer changes from the billing app

[Path]
PathChanged=$DATA/wg/apply
Unit=$APP-wg-sync.service

[Install]
WantedBy=multi-user.target
WGEOF
cat >/etc/systemd/system/$APP-wg-status.service <<WGEOF
[Unit]
Description=Write WireGuard handshake status for the billing app
After=wg-quick@$WG_IF.service

[Service]
Type=oneshot
UMask=0027
ExecStart=/bin/sh -c 'wg show $WG_IF dump | tail -n +2 | cut -f1,3-7 >/run/$APP/$WG_IF.dump.tmp && chgrp $APP /run/$APP/$WG_IF.dump.tmp && chmod 0640 /run/$APP/$WG_IF.dump.tmp && mv -f /run/$APP/$WG_IF.dump.tmp /run/$APP/$WG_IF.dump'
WGEOF
cat >/etc/systemd/system/$APP-wg-status.timer <<WGEOF
[Unit]
Description=Refresh WireGuard handshake status every minute

[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
AccuracySec=5s

[Install]
WantedBy=timers.target
WGEOF
systemctl daemon-reload
systemctl enable wg-quick@$WG_IF >/dev/null 2>&1
systemctl is-active --quiet wg-quick@$WG_IF || systemctl start wg-quick@$WG_IF || die "could not start $WG_IF (see: journalctl -u wg-quick@$WG_IF)"
systemctl enable --now $APP-wg-sync.path $APP-wg-status.timer >/dev/null
if [ "$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo 0)" = 1 ]; then
  echo "NOTE: net.ipv4.ip_forward is 1 on this server. The firewall below blocks routing from the tunnel, but leave forwarding off unless you need it." >&2
fi

PUBIP=$(curl -4 -fsS --max-time 10 https://api.ipify.org || curl -4 -fsS --max-time 10 https://ifconfig.me || true)
[ -n "$PUBIP" ] || die "could not detect this VPS's public IPv4"
if [ -n "${DOMAIN:-}" ]; then BASE="https://$DOMAIN"; else BASE="http://$PUBIP"; fi
if [ ! -f "$ENVF" ]; then
  ADMIN_PASS="${ADMIN_PASS:-$(rand 18)}"
  cat >"$ENVF" <<EOF
# Fast Tech Net billing server settings. Edit, then: systemctl restart $APP
BRAND_NAME=${BRAND_NAME:-Fast Tech Net}
LISTEN_ADDR=:8080
DATA_DIR=$DATA
TEST_MODE=false

# Admin (not published: use the SSH tunnel or Tailscale, see the end of the install)
ADMIN_USER=admin
ADMIN_PASS=$ADMIN_PASS

# Public portal
PUBLIC_BASE_URL=$BASE
PUBLIC_IP=$PUBIP
SUPPORT_PHONE=${SUPPORT_PHONE:-}

# Routers connect over WireGuard. Add each building in Admin > Buildings.
WG_PUBLIC_KEY=$WG_PUB
WG_ENDPOINT=$PUBIP:$WG_PORT
WG_SUBNET=$WG_SUBNET

# Metrics: routers send NetFlow to the tunnel address only
NETFLOW_ADDR=$WG_SERVER_IP:2055

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
  # Added once and then left alone (the key never changes; the rest is yours to edit).
  addkv() { grep -q "^$1=" "$ENVF" || echo "$1=$2" >>"$ENVF"; }
  addkv WG_PUBLIC_KEY "$WG_PUB"
  addkv WG_ENDPOINT "$PUBIP:$WG_PORT"
  addkv WG_SUBNET "$WG_SUBNET"
  [ -n "${ADMIN_PASS:-}" ] && setkv ADMIN_PASS "$ADMIN_PASS"
  BASE=$(grep '^PUBLIC_BASE_URL=' "$ENVF" | cut -d= -f2-)
fi
chown root:$APP "$ENVF"; chmod 640 "$ENVF"

cat >/etc/systemd/system/$APP.service <<EOF
[Unit]
Description=Fast Tech Net billing server
After=network-online.target wg-quick@$WG_IF.service
Wants=network-online.target wg-quick@$WG_IF.service

[Service]
User=$APP
Group=$APP
EnvironmentFile=$ENVF
ExecStart=$BIN
Environment=GOMEMLIMIT=600MiB GOGC=50
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

say "5/8 Tailscale (optional, only to reach the admin)"
TSIP=""
if command -v tailscale >/dev/null && tailscale status >/dev/null 2>&1; then
  TSIP=$(tailscale ip -4 | head -1)
elif [ -n "${TS_AUTHKEY:-}" ]; then
  command -v tailscale >/dev/null || curl -fsSL https://tailscale.com/install.sh | sh >/dev/null
  systemctl enable --now tailscaled >/dev/null
  tailscale up --authkey="$TS_AUTHKEY" --hostname="$TS_HOST"
  TSIP=$(tailscale ip -4 | head -1)
else
  echo "Skipped (no TS_AUTHKEY and Tailscale is not already running). Use the SSH tunnel shown at the end."
fi

say "6/8 Caddy (public portal${DOMAIN:+ with HTTPS for $DOMAIN})"
if ! command -v caddy >/dev/null; then
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' >/etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi
SITE="${DOMAIN:-:80}"
cat >/etc/caddy/Caddyfile <<EOF
# Public side of the billing server. The admin pages are NOT published here:
# open them over an SSH tunnel or Tailscale (see the end of the install)
$SITE {
	encode gzip
	@admin path /admin /admin/*
	respond @admin "Admin is not published" 403
	reverse_proxy 127.0.0.1:8080
}
EOF
systemctl enable caddy >/dev/null; systemctl reload caddy 2>/dev/null || systemctl restart caddy

say "7/8 firewall + daily backups"
if [ "${NO_FIREWALL:-0}" != 1 ]; then
  SSHP=$(sshd -T 2>/dev/null | awk '/^port /{print $2; exit}'); SSHP=${SSHP:-22}
  ufw allow "$SSHP"/tcp >/dev/null
  ufw allow 80/tcp >/dev/null; ufw allow 443/tcp >/dev/null
  ufw allow "$WG_PORT"/udp >/dev/null                 # routers (WireGuard)
  # NetFlow arrives only through the tunnel. A legacy install that still has a
  # router on ROUTER_URL sends it over the internet, so keep that rule there.
  if ! grep -q '^ROUTER_URL=' "$ENVF"; then ufw delete allow 2055/udp >/dev/null 2>&1 || true; fi
  ufw allow in on $WG_IF to "$WG_SERVER_IP" port 2055 proto udp >/dev/null
  # Routers get nothing else: no admin, no SSH, no routing to other routers.
  # Re-added last so it stays below the allow rule above on upgrades.
  ufw delete deny in on $WG_IF >/dev/null 2>&1 || true
  ufw delete deny route in on $WG_IF >/dev/null 2>&1 || true
  ufw deny in on $WG_IF >/dev/null
  ufw route deny in on $WG_IF >/dev/null
  if [ -n "$TSIP" ]; then
    ufw allow in on tailscale0 >/dev/null             # admin :8080 over Tailscale only
    ufw allow 41641/udp >/dev/null                    # Tailscale direct connections
  fi
  ufw default deny incoming >/dev/null; ufw --force enable >/dev/null
fi
cat >/etc/cron.daily/$APP-backup <<EOF
#!/bin/sh
# Daily: the billing files, kept 30 days. Sundays: the metrics folder, kept 4 weeks.
D=/var/backups/$APP
mkdir -p \$D
cd $DATA || exit 0
FILES=""
for f in ispbill.json outbox.json alerts.json fleet.json; do [ -f \$f ] && FILES="\$FILES \$f"; done
[ -n "\$FILES" ] && tar -czf \$D/$APP-\$(date +%F).tgz \$FILES 2>/dev/null
if [ "\$(date +%u)" = 7 ] && [ -d metrics ]; then tar -czf \$D/$APP-metrics-\$(date +%F).tgz metrics 2>/dev/null; fi
find \$D -name '$APP-2*.tgz' -mtime +30 -delete
find \$D -name '$APP-metrics-*.tgz' -mtime +28 -delete
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
if [ -n "$TSIP" ]; then
  ADMIN_HOW=" Admin (Tailscale): http://$TSIP:8080/admin"
else
  ADMIN_HOW=" Admin (SSH tunnel): run on your laptop:  ssh -L 8080:127.0.0.1:8080 <you>@$PUBIP
                    then open http://127.0.0.1:8080/admin"
fi
cat <<EOF

────────────────────────────────────────────────────────────────────
 Fast Tech Net billing is running ($REV)

 Portal (public):   $BASE/portal
$ADMIN_HOW
 Admin login:       admin / $APASS
 Settings:          $ENVF   (then: systemctl restart $APP)
 Logs:              journalctl -u $APP -f
 Health:            $HEALTH
 WireGuard:         $WG_IF listening on udp/$WG_PORT, server key $WG_PUB

 Next: open the admin, then  Buildings -> Add building.  It gives you one
 command to paste into the building's MikroTik terminal; the router then
 joins the tunnel by itself.
EOF
case "$BASE" in https://*) ;; *) echo " NOTE: join commands need HTTPS. Re-run this installer with DOMAIN=your.domain first." ;; esac
if grep -q '^ROUTER_URL=' "$ENVF"; then
  echo " NOTE: this server still has a legacy ROUTER_URL router. It keeps working as the building \"Main building\"."
fi
echo "────────────────────────────────────────────────────────────────────"
exit 0
