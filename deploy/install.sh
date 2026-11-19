#!/usr/bin/env bash
# Fast Tech Net billing server: one-command install / upgrade on a brand-new
# minimal Ubuntu 22.04 / 24.04 or Debian 12 VPS (1 vCPU, 2 GB RAM, 60 GB disk).
# It installs EVERY dependency itself. Safe to run again: a re-run is an upgrade
# that keeps your data, settings and keys.
#
#   curl -fsSL -H "Authorization: Bearer $GH_TOKEN" \
#     https://raw.githubusercontent.com/blacksan/ftnbilling/main/deploy/install.sh \
#     | sudo GH_TOKEN=$GH_TOKEN DOMAIN=billing.example.co.ke bash
#
# Offline / no GitHub: copy ftnbilling-<version>-linux-<arch>.tar.gz to the
# server, then
#   tar xzf ftnbilling-*.tar.gz && cd ftnbilling-*/ && sudo DOMAIN=... ./install.sh
# (the bundled program is used; apt still needs the distro and Caddy mirrors).
#
# What it sets up: PostgreSQL (local socket only), Caddy (HTTPS; /admin and /api
# are public, protected by the app's own login with a second factor), WireGuard
# wg0 for the routers, WireGuard wg-mgmt for remote WinBox (your laptop or phone
# reaches the routers' WinBox through this server; devices are added with
# ftnbilling-mgmt), ufw, rclone (Google Drive backup), unattended security
# upgrades, a swap file on small servers, 6-hourly database dumps and a monthly
# restore drill. See deploy/README.md.
#
# Upgrading an older server in place (one that still has the admin VPN): the
# installer takes a database dump first, moves the old admin VPN config and
# device files to /root/ftnbilling-removed-<date>/, stops and disables that VPN,
# closes its firewall rules, runs the database migrations, prints the report of
# flats that share a MAC address, and prints a one-time admin setup code. The
# router tunnel (wg0) is not touched.
#
# Settings (environment variables), all optional except where noted:
#   DOMAIN        e.g. billing.fasttech.co.ke (DNS A record -> this VPS). HTTPS is
#                 needed for M-Pesa and for the router join command. Without it
#                 the portal is served on http://<VPS IP> and join commands are
#                 refused.
#   GH_TOKEN      GitHub token with read access to the repo (it is private):
#                 needed to download a release or clone the source.
#   VERSION       release to install, e.g. v1.4.0 (default: latest release; if the
#                 repo has no release, the source is built on the server).
#   FROM_SOURCE=1 skip releases, clone BRANCH and build on the server.
#   NO_POSTGRES=1 keep the JSON-file store; do not install PostgreSQL.
#   NO_FIREWALL=1 do not touch ufw.
#   DRY_RUN=1     print what would be done; change nothing (works without root).
#   ADMIN_PASS    a random one is generated on first install.
#   CADDY_MPESA_IPS  optional: space- or comma-separated IPs/CIDRs. When set, Caddy
#                 answers /pay/cb/* (the M-Pesa callbacks) only to those source
#                 addresses and refuses everyone else with 403. Take the list from
#                 Safaricom (Daraja support / your Paybill onboarding): the
#                 published ranges have changed over time and are NOT hard-coded
#                 here, because a wrong list would silently drop real payments.
#                 Unset = no IP filter (the callback token still protects them).
#   SUPPORT_PHONE, BRAND_NAME, PUBLIC_IP (skip auto-detection of this VPS's IPv4).
#   WG_PORT (51820), WG_SUBNET (10.99.0.0/22),
#   MGMT_PORT (51821, udp: the remote-WinBox WireGuard interface wg-mgmt),
#   MGMT_SUBNET (10.98.0.0/24: a private IPv4 prefix, /16 to /29, apart from
#                WG_SUBNET; the server is .1, each device gets the next free address.
#                Routers built after this setting is made accept WinBox from it).
#   WEBFIG_DOMAIN e.g. tech.ftw.co.ke: opens a router's WebFig through this server, from
#                the router page in the admin (owners only, after a fresh password and
#                code). Needs its own DNS A record to this VPS (like DOMAIN) and DOMAIN
#                set. Adds a second Caddy site with its own certificate and makes the
#                app listen on 127.0.0.1:8082 for it. Unset = off (and an earlier
#                setting is kept when a later run leaves it out); WEBFIG_DOMAIN=off
#                switches it off again. ufw is unchanged (443 is already open).
#   REPO (blacksan/ftnbilling), BRANCH (main), SWAP_MB (1024).
set -Eeuo pipefail

# Where this script lives (empty when piped from curl); resolved before main() changes directory.
SELF_DIR=""
if [ -f "${BASH_SOURCE[0]:-}" ]; then SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd); fi
REPO="${REPO:-blacksan/ftnbilling}"
BRANCH="${BRANCH:-main}"
WANT_VERSION="${VERSION:-latest}"   # user setting; /etc/os-release also defines VERSION, so never read that name later
DRY="${DRY_RUN:-0}"
APP=ftnbilling
OPT=/opt/$APP
SRC=$OPT/src
DATA=/var/lib/$APP
ETC=/etc/$APP
ENVF=$ETC/$APP.env
BIN=/usr/local/bin/$APP
SBIN=/usr/local/sbin
BACKUPS=/var/backups/$APP
WG_IF=wg0
WG_PORT="${WG_PORT:-51820}"
WG_SUBNET="${WG_SUBNET:-10.99.0.0/22}"
WG_NET=${WG_SUBNET%/*}
WG_SERVER_IP=${WG_NET%.*}.1
WG_CONF=/etc/wireguard/$WG_IF.conf
MGMT_IF=wg-mgmt
MGMT_CONF=/etc/wireguard/$MGMT_IF.conf
MGMT_MARK=ftnbilling-mgmt   # BEGIN/END marker of the block in /etc/ufw/before.rules
SWAP_MB="${SWAP_MB:-1024}"
TOTAL=15
STEPN=0
STEPNAME=starting
WROTE=0

rand() { local s; s=$(head -c 96 /dev/urandom | base64 -w0 | tr -dc 'A-Za-z0-9'); printf '%s' "${s:0:$1}"; }
say()  { printf '\n\033[1;33m==> %s\033[0m\n' "$*"; }
step() { STEPN=$((STEPN + 1)); STEPNAME=$1; say "[$STEPN/$TOTAL] $1"; }
note() { printf '    %s\n' "$*"; }
warn() { printf '\033[1;33m    WARNING: %s\033[0m\n' "$*" >&2; }
die()  { printf '\n\033[1;31mERROR in step %s/%s (%s): %s\033[0m\n' "$STEPN" "$TOTAL" "$STEPNAME" "$*" >&2; exit 1; }
trap 'die "command failed (line $LINENO): $BASH_COMMAND"' ERR

# run CMD...: execute, or only print in DRY_RUN mode.
run() { if [ "$DRY" = 1 ]; then printf '    [dry-run] %s\n' "$*"; else "$@"; fi; }
# runq: as run, stdout hidden. try: as run, all output hidden and failure ignored.
runq() { if [ "$DRY" = 1 ]; then printf '    [dry-run] %s\n' "$*"; else "$@" >/dev/null; fi; }
try() { if [ "$DRY" = 1 ]; then printf '    [dry-run] %s\n' "$*"; else "$@" >/dev/null 2>&1 || true; fi; }

# writef MODE OWNER:GROUP PATH  (content on stdin). Replaces the file only when
# the content or mode differs; sets WROTE=1 in that case.
writef() {
  local mode=$1 own=$2 path=$3 tmp
  WROTE=0
  if [ "$DRY" = 1 ]; then printf '    [dry-run] write %s (mode %s)\n' "$path" "$mode"; cat >/dev/null; return 0; fi
  mkdir -p "$(dirname "$path")"
  tmp=$(mktemp "$path.XXXXXX")
  cat >"$tmp"
  chmod "$mode" "$tmp"
  [ -z "$own" ] || chown "$own" "$tmp"
  if [ -f "$path" ] && cmp -s "$tmp" "$path" && [ "$(stat -c %a "$path")" = "$mode" ]; then
    rm -f "$tmp"
  else
    mv -f "$tmp" "$path"; WROTE=1
  fi
}

# Wait (up to 15 min) for the dpkg lock instead of failing: a new VPS often runs
# unattended-upgrades in the background for the first few minutes. APT_CONFIG
# applies to every apt-get call in this script, including the bare ones.
if [ "$DRY" != 1 ]; then
  APT_CONFIG=$(mktemp /tmp/ftn-apt.XXXXXX)
  printf 'DPkg::Lock::Timeout "900";\n' >"$APT_CONFIG"
  export APT_CONFIG
fi
apt_get() { run env DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a NEEDRESTART_SUSPEND=1 apt-get "$@" </dev/null; }
apt_install() { if [ "$DRY" = 1 ]; then printf '    [dry-run] apt-get install %s\n' "$*"; else apt_get install -y -qq --no-install-recommends "$@" >/dev/null; fi; }
have() { command -v "$1" >/dev/null 2>&1; }
envget() { [ -f "$ENVF" ] && grep -m1 "^$1=" "$ENVF" | cut -d= -f2- || true; }
# setkv sets KEY=VAL in the env file (replacing); addkv only adds it when missing.
setkv() {
  if [ "$DRY" = 1 ]; then printf '    [dry-run] env %s=%s\n' "$1" "$2"; return 0; fi
  # awk, not sed: values such as database URLs contain & and | that sed would mangle.
  if grep -q "^$1=" "$ENVF"; then
    local tmp; tmp=$(mktemp "$ENVF.XXXXXX")
    K="$1" V="$2" awk 'BEGIN{k=ENVIRON["K"]; v=ENVIRON["V"]} index($0, k "=")==1 {print k "=" v; next} {print}' "$ENVF" >"$tmp"
    chmod --reference="$ENVF" "$tmp"; chown --reference="$ENVF" "$tmp"; mv -f "$tmp" "$ENVF"
  else echo "$1=$2" >>"$ENVF"; fi
}
addkv() {
  if grep -qs "^$1=" "$ENVF"; then return 0; fi
  if [ "$DRY" = 1 ]; then printf '    [dry-run] env add %s=...\n' "$1"; return 0; fi
  echo "$1=$2" >>"$ENVF"
}

# fillkv gives KEY a value when the file has the line but it is empty (KEY=,
# KEY="" or KEY='' with nothing else). A non-empty value is never replaced.
fillkv() {
  local cur
  cur=$(envget "$1")
  case "$cur" in "" | '""' | "''") ;; *) return 0 ;; esac
  grep -qs "^$1=" "$ENVF" || return 0
  setkv "$1" "$2"
}

PGSU() { run runuser -u postgres -- "$@"; }

# ---------------------------------------------------------------------------
ip2int() { local a b c d; IFS=. read -r a b c d <<<"$1"; echo $(((a << 24) | (b << 16) | (c << 8) | d)); }
# in_private_v4 ADDR: RFC 1918 (10/8, 172.16/12, 192.168/16).
in_private_v4() {
  local n; n=$(ip2int "$1")
  [ $((n >> 24)) -eq 10 ] || [ $((n >> 20)) -eq $(((172 << 4) | 1)) ] || [ $((n >> 16)) -eq $(((192 << 8) | 168)) ]
}
# prefixes_overlap A/N B/M: success when the two IPv4 prefixes share addresses.
prefixes_overlap() {
  local a=${1%/*} an=${1#*/} b=${2%/*} bn=${2#*/} n mask
  n=$an; [ "$bn" -ge "$n" ] || n=$bn
  mask=$(((0xFFFFFFFF << (32 - n)) & 0xFFFFFFFF))
  [ $(($(ip2int "$a") & mask)) -eq $(($(ip2int "$b") & mask)) ]
}
# mgmt_settings: MGMT_SUBNET / MGMT_PORT from the environment, else from the
# settings file of an earlier install, else the defaults; then validated.
mgmt_settings() {
  MGMT_PORT_ENVFILE=$(envget MGMT_PORT)   # the port of the earlier run, before anything is overwritten
  MGMT_PORT="${MGMT_PORT:-$MGMT_PORT_ENVFILE}"; MGMT_PORT="${MGMT_PORT:-51821}"
  MGMT_SUBNET="${MGMT_SUBNET:-$(envget MGMT_SUBNET)}"; MGMT_SUBNET="${MGMT_SUBNET:-10.98.0.0/24}"
  case $MGMT_PORT in ""|*[!0-9]*) die "MGMT_PORT must be a UDP port number, got: $MGMT_PORT" ;; esac
  { [ "$MGMT_PORT" -ge 1 ] && [ "$MGMT_PORT" -le 65535 ]; } || die "MGMT_PORT must be 1 to 65535, got: $MGMT_PORT"
  [ "$MGMT_PORT" != "$WG_PORT" ] || die "MGMT_PORT must differ from WG_PORT ($WG_PORT)"
  [[ $MGMT_SUBNET =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$ ]] || die "MGMT_SUBNET must be an IPv4 prefix like 10.98.0.0/24, got: $MGMT_SUBNET"
  local o; for o in $(printf '%s' "${MGMT_SUBNET%/*}" | tr . ' '); do [ "$o" -le 255 ] || die "MGMT_SUBNET has a bad address: $MGMT_SUBNET"; done
  { [ "${MGMT_SUBNET#*/}" -ge 16 ] && [ "${MGMT_SUBNET#*/}" -le 29 ]; } || die "MGMT_SUBNET must be a /16 to /29 prefix, got: $MGMT_SUBNET"
  in_private_v4 "${MGMT_SUBNET%/*}" || die "MGMT_SUBNET must be a private IPv4 prefix (10/8, 172.16/12 or 192.168/16), got: $MGMT_SUBNET"
  if prefixes_overlap "$MGMT_SUBNET" "$WG_SUBNET"; then die "MGMT_SUBNET ($MGMT_SUBNET) must not overlap WG_SUBNET ($WG_SUBNET)"; fi
  MGMT_NET=$(int2ip_masked "${MGMT_SUBNET%/*}" "${MGMT_SUBNET#*/}")
  MGMT_SUBNET=$MGMT_NET/${MGMT_SUBNET#*/}
  MGMT_SERVER_IP=$(int2ip_masked "$MGMT_NET" 32 1)
}
# webfig_settings: WEBFIG_DOMAIN from the environment, else from the settings file of an
# earlier install, else off. "off" switches it off. WEBFIG_PORT is the app's loopback port.
WEBFIG_PORT=8082
webfig_settings() {
  local given="${WEBFIG_DOMAIN:-}" main="${DOMAIN:-}" base
  WEBFIG_DOMAIN="${given:-$(envget WEBFIG_DOMAIN)}"
  WEBFIG_DOMAIN=$(printf '%s' "$WEBFIG_DOMAIN" | tr '[:upper:]' '[:lower:]')
  WEBFIG_OFF=0
  if [ "$WEBFIG_DOMAIN" = off ]; then WEBFIG_DOMAIN=""; WEBFIG_OFF=1; fi
  [ -n "$WEBFIG_DOMAIN" ] || return 0
  [[ $WEBFIG_DOMAIN =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "WEBFIG_DOMAIN must be a host name like tech.example.co.ke, got: $WEBFIG_DOMAIN"
  if [[ $WEBFIG_DOMAIN =~ ^[0-9.]+$ ]]; then die "WEBFIG_DOMAIN must be a host name, not an IP address: $WEBFIG_DOMAIN"; fi
  if [ -z "$main" ]; then
    base=$(envget PUBLIC_BASE_URL)
    case "$base" in https://*) main=${base#https://}; main=${main%%[/:]*} ;; esac
  fi
  [ -n "$main" ] || die "WEBFIG_DOMAIN needs DOMAIN (HTTPS) for the admin itself: run again with DOMAIN=your.domain WEBFIG_DOMAIN=$WEBFIG_DOMAIN"
  [ "$WEBFIG_DOMAIN" != "$main" ] || die "WEBFIG_DOMAIN must differ from DOMAIN ($main): WebFig needs its own host name, so it never shares cookies with the admin"
}
# render_webfig_block: the Caddy site for WEBFIG_DOMAIN (its own certificate); empty when off.
render_webfig_block() {
  [ -n "${WEBFIG_DOMAIN:-}" ] || return 0
  printf '\n# Remote WebFig: a router'"'"'s own web page, reached through this server only. The app checks\n'
  printf '# the one-time ticket an owner gets after a fresh password and code, and proxies to that one\n'
  printf '# router over the tunnel. Same rule as above: Caddy is the only proxy in front of it.\n'
  printf '%s {\n\tencode gzip\n\treverse_proxy 127.0.0.1:%s {\n\t\tflush_interval -1\n\t}\n}\n' "$WEBFIG_DOMAIN" "${WEBFIG_PORT:-8082}"
}
# int2ip_masked ADDR BITS [N]: the network address of ADDR/BITS, plus N.
int2ip_masked() {
  local n mask
  mask=$(((0xFFFFFFFF << (32 - $2)) & 0xFFFFFFFF))
  n=$(($(ip2int "$1") & mask))
  n=$((n + ${3:-0}))
  echo "$(((n >> 24) & 255)).$(((n >> 16) & 255)).$(((n >> 8) & 255)).$((n & 255))"
}

# ---------------------------------------------------------------------------
preflight() {
  step "checking this server"
  if [ "$DRY" = 1 ]; then note "DRY_RUN=1: nothing below is executed, only printed"; fi
  if [ "$DRY" != 1 ] && [ "$(id -u)" != 0 ]; then die "run as root (pipe into: sudo ... bash)"; fi
  # An existing settings file means this run upgrades a server that is already installed.
  if [ -f "$ENVF" ]; then UPGRADE=1; note "existing install found: this run is an in-place upgrade"; fi
  mgmt_settings
  webfig_settings
  local ID ID_LIKE VERSION_ID PRETTY_NAME
  eval "$( (. /etc/os-release 2>/dev/null; printf 'ID=%q ID_LIKE=%q VERSION_ID=%q PRETTY_NAME=%q' "${ID:-}" "${ID_LIKE:-}" "${VERSION_ID:-}" "${PRETTY_NAME:-}") || true)"
  case "${ID:-}${ID_LIKE:-}" in
    *debian* | *ubuntu*) ;;
    *) if [ "$DRY" = 1 ]; then warn "not Debian/Ubuntu (${PRETTY_NAME:-unknown}); a real run would stop here"; else die "Debian or Ubuntu is required (found ${PRETTY_NAME:-unknown})"; fi ;;
  esac
  case "${ID:-} ${VERSION_ID:-}" in
    "ubuntu 22.04" | "ubuntu 24.04" | "ubuntu 26.04" | "debian 12") note "OS: ${PRETTY_NAME:-}" ;;
    *) warn "tested on Ubuntu 22.04/24.04/26.04 and Debian 12; this is ${PRETTY_NAME:-unknown}. Continuing." ;;
  esac
  case "$(uname -m)" in x86_64) GOARCH=amd64 ;; aarch64 | arm64) GOARCH=arm64 ;; *) die "unsupported CPU $(uname -m)" ;; esac
  MEM_MB=$(awk '/^MemTotal:/{print int($2/1024)}' /proc/meminfo)
  DISK_GB=$(df -BG --output=avail / | awk 'NR==2{gsub("G","",$1); print $1}')
  note "CPU $GOARCH, RAM ${MEM_MB} MB, free disk ${DISK_GB} GB"
  if [ "$DISK_GB" -lt 8 ]; then
    if [ "$DRY" = 1 ]; then warn "only ${DISK_GB} GB free; a real run needs at least 8 GB"; else die "at least 8 GB of free disk is needed (have ${DISK_GB} GB)"; fi
  fi
  if [ "$DRY" != 1 ]; then
    [ -d /run/systemd/system ] || die "systemd is required"
    curl --version >/dev/null 2>&1 || { apt_get update -qq && apt_get install -y -qq curl ca-certificates >/dev/null; }
    curl -fsS --max-time 15 -o /dev/null https://github.com 2>/dev/null || curl -fsS --max-time 15 -o /dev/null https://dl.cloudsmith.io 2>/dev/null \
      || die "no outbound internet (HTTPS) from this server"
  fi
  PUBIP="${PUBLIC_IP:-}"
  if [ -z "$PUBIP" ]; then PUBIP=$(envget PUBLIC_IP); fi
  if [ -z "$PUBIP" ] && [ "$DRY" != 1 ]; then
    PUBIP=$(curl -4 -fsS --max-time 10 https://api.ipify.org 2>/dev/null || curl -4 -fsS --max-time 10 https://ifconfig.me 2>/dev/null || true)
  fi
  if [ -z "$PUBIP" ] && [ "$DRY" = 1 ]; then PUBIP="<public-ip>"; fi
  [ -n "$PUBIP" ] || die "could not detect this VPS's public IPv4; run again with PUBLIC_IP=<ip>"
  note "public IPv4: $PUBIP"
  if [ -n "${DOMAIN:-}" ]; then BASE="https://$DOMAIN"; else BASE="http://$PUBIP"; fi
}

# ---------------------------------------------------------------------------
packages() {
  step "system packages (apt)"
  apt_get update -qq
  apt_install curl ca-certificates gnupg git tar xz-utils unzip jq ufw qrencode wireguard-tools \
    debian-keyring debian-archive-keyring apt-transport-https unattended-upgrades cron iputils-ping traceroute
}

# ---------------------------------------------------------------------------
system_setup() {
  step "swap, logs, clock, security updates"
  # Swap (small servers: Postgres + the app + a Go build must not hit the OOM killer).
  if [ "${MEM_MB:-0}" -le 2200 ] && [ -z "$(swapon --show --noheadings 2>/dev/null)" ]; then
    note "RAM is ${MEM_MB} MB and there is no swap: creating a ${SWAP_MB} MB /swapfile"
    if [ ! -e /swapfile ]; then
      if [ "$DRY" = 1 ]; then printf '    [dry-run] fallocate /swapfile; mkswap; swapon; add to /etc/fstab\n'
      else
        fallocate -l "${SWAP_MB}M" /swapfile 2>/dev/null || dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none
        chmod 600 /swapfile; mkswap /swapfile >/dev/null; swapon /swapfile
        grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >>/etc/fstab
      fi
    else
      run chmod 600 /swapfile
      run mkswap /swapfile
      run swapon /swapfile
    fi
    printf 'vm.swappiness=10\n' | writef 644 root:root /etc/sysctl.d/90-ftnbilling.conf
    run sysctl -q -p /etc/sysctl.d/90-ftnbilling.conf
  else
    note "swap: not needed or already present"
  fi
  # journald: logs cannot fill the disk.
  printf '[Journal]\nSystemMaxUse=500M\n' | writef 644 root:root /etc/systemd/journald.conf.d/$APP.conf
  if [ "$WROTE" = 1 ]; then run systemctl restart systemd-journald; fi
  # Clock: M-Pesa timestamps and WireGuard need a correct one.
  if systemctl is-active --quiet chrony 2>/dev/null || systemctl is-active --quiet chronyd 2>/dev/null || systemctl is-active --quiet systemd-timesyncd 2>/dev/null; then
    note "time sync: already running"
  else
    apt_install chrony
    run systemctl enable --now chrony
  fi
  # Security updates only, no automatic reboots.
  printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' | writef 644 root:root /etc/apt/apt.conf.d/20auto-upgrades
  printf '// Fast Tech Net: security updates only (the distro default origins), never reboot by itself.\nUnattended-Upgrade::Automatic-Reboot "false";\nUnattended-Upgrade::Remove-Unused-Dependencies "true";\n' \
    | writef 644 root:root /etc/apt/apt.conf.d/52ftnbilling-unattended
  run systemctl enable --now unattended-upgrades
}

# ---------------------------------------------------------------------------
caddy_rclone() {
  step "Caddy (official repo) and rclone"
  if ! have caddy; then
    if [ "$DRY" = 1 ]; then printf '    [dry-run] add the Caddy apt repo (dl.cloudsmith.io/public/caddy/stable) and apt install caddy\n'
    else
      curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
      curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' >/etc/apt/sources.list.d/caddy-stable.list
      apt_get update -qq && apt_get install -y -qq caddy >/dev/null
    fi
  else
    note "caddy: already installed"
  fi
  # The Drive backup needs rclone >= 1.60 (older distro packages lack features it uses).
  rclone_ok() { have rclone && [ "$(printf '%s\n1.60\n' "$(rclone version 2>/dev/null | awk 'NR==1{sub(/^rclone v/,""); print $1}')" | sort -V | head -1)" = "1.60" ]; }
  if rclone_ok; then
    note "rclone: ok ($(rclone version 2>/dev/null | awk 'NR==1'))"
  elif [ "$DRY" = 1 ]; then
    printf '    [dry-run] apt install rclone; if older than 1.60, run the official installer (rclone.org/install.sh)\n'
  else
    apt_get install -y -qq rclone >/dev/null 2>&1 || true
    if ! rclone_ok; then
      curl -fsSL https://rclone.org/install.sh | bash >/dev/null || die "could not install rclone"
      hash -r
    fi
    rclone_ok || die "rclone >= 1.60 is required"
  fi
}

# ---------------------------------------------------------------------------
# Sets BUNDLE (directory holding ftnbilling + deploy/) and PROG_VERSION.
get_bundle_from_release() {
  local api="https://api.github.com/repos/$REPO" url tag tmp code id name sid hdr=()
  [ -n "${GH_TOKEN:-}" ] && hdr=(-H "Authorization: Bearer $GH_TOKEN")
  if [ "$WANT_VERSION" = latest ]; then url=$api/releases/latest
  else tag=$WANT_VERSION; case $tag in v*) ;; *) tag=v$tag ;; esac; url=$api/releases/tags/$tag; fi
  tmp=$(mktemp -d)
  code=$(curl -sS -o "$tmp/meta.json" -w '%{http_code}' --max-time 30 "${hdr[@]}" -H 'Accept: application/vnd.github+json' "$url" || true)
  case "$code" in
    200) ;;
    401 | 403) rm -rf "$tmp"; die "GitHub refused the token (HTTP $code). Check GH_TOKEN (needs Contents: read on $REPO)." ;;
    *) note "no release found (HTTP ${code:-none}; for a private repo a missing or wrong GH_TOKEN also looks like this)"; rm -rf "$tmp"; return 1 ;;
  esac
  name=$(jq -r --arg re "^ftnbilling-.*-linux-${GOARCH}\\.tar\\.gz\$" '.assets[]? | select(.name|test($re)) | .name' "$tmp/meta.json" | head -1)
  id=$(jq -r --arg n "$name" '.assets[]? | select(.name==$n) | .id' "$tmp/meta.json" | head -1)
  sid=$(jq -r '.assets[]? | select(.name=="SHA256SUMS") | .id' "$tmp/meta.json" | head -1)
  if [ -z "$name" ] || [ -z "$id" ]; then note "the release has no linux-$GOARCH bundle"; rm -rf "$tmp"; return 1; fi
  [ -n "$sid" ] || { rm -rf "$tmp"; die "the release has no SHA256SUMS: refusing to install an unverified download"; }
  PROG_VERSION=$(jq -r '.tag_name' "$tmp/meta.json")
  note "release $PROG_VERSION: downloading $name"
  curl -fSL --retry 3 --max-time 600 "${hdr[@]}" -H 'Accept: application/octet-stream' "$api/releases/assets/$id" -o "$tmp/$name" || { rm -rf "$tmp"; die "download failed"; }
  curl -fSL --retry 3 --max-time 60 "${hdr[@]}" -H 'Accept: application/octet-stream' "$api/releases/assets/$sid" -o "$tmp/SHA256SUMS" || { rm -rf "$tmp"; die "SHA256SUMS download failed"; }
  awk -v n="$name" '$2==n' "$tmp/SHA256SUMS" >"$tmp/one"
  [ -s "$tmp/one" ] || { rm -rf "$tmp"; die "SHA256SUMS does not list $name"; }
  (cd "$tmp" && sha256sum -c --quiet one) || { rm -rf "$tmp"; die "SHA256 mismatch for $name: the download is damaged or has been tampered with"; }
  note "SHA256 verified"
  install -d -m 755 "$OPT/releases"
  BUNDLE=$OPT/releases/${name%.tar.gz}
  rm -rf "$BUNDLE"
  tar -C "$OPT/releases" --no-same-owner -xzf "$tmp/$name" || { rm -rf "$tmp"; die "could not unpack $name"; }
  rm -rf "$tmp"
  # keep this and the previous release only
  # shellcheck disable=SC2012
  ls -1dt "$OPT"/releases/ftnbilling-*/ 2>/dev/null | tail -n +3 | xargs -r rm -rf
  return 0
}

get_source_and_build() {
  local auth=() need cur gov sha
  [ -n "${GH_TOKEN:-}" ] && auth=(-c "http.https://github.com/.extraheader=Authorization: Basic $(printf 'x-access-token:%s' "$GH_TOKEN" | base64 -w0)")
  if [ -d "$SRC/.git" ]; then
    git "${auth[@]}" -C "$SRC" fetch -q --depth 1 origin "$BRANCH" || die "git fetch failed (is GH_TOKEN set and valid?)"
    git -C "$SRC" reset -q --hard FETCH_HEAD
  else
    install -d "$OPT"
    git "${auth[@]}" clone -q --depth 1 --branch "$BRANCH" "https://github.com/$REPO.git" "$SRC" \
      || die "git clone failed. The repo is private: pass GH_TOKEN=<token with Contents: read>"
  fi
  PROG_VERSION="src-$(git -C "$SRC" rev-parse --short HEAD)"
  need=$(awk '/^go /{print $2}' "$SRC/go.mod")
  cur=""
  if [ -x /usr/local/go/bin/go ]; then cur=$(/usr/local/go/bin/go version | awk '{print $3}' | sed 's/^go//'); fi
  if [ -z "$cur" ] || [ "$(printf '%s\n%s\n' "$need" "$cur" | sort -V | head -1)" != "$need" ]; then
    note "installing the Go toolchain (only needed to build from source)"
    gov=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1)
    curl -fsSL "https://go.dev/dl/${gov}.linux-${GOARCH}.tar.gz" -o /tmp/go.tgz
    sha=$(curl -fsSL "https://dl.google.com/go/${gov}.linux-${GOARCH}.tar.gz.sha256" 2>/dev/null || true)
    if [ -n "$sha" ]; then
      [ "$(sha256sum /tmp/go.tgz | cut -d' ' -f1)" = "$sha" ] || die "Go download checksum mismatch"
    else
      warn "could not fetch the Go checksum; continuing"
    fi
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  fi
  note "building (niced, vendored modules; a few minutes on 1 vCPU)"
  local flags="-trimpath"
  [ -d "$SRC/vendor" ] && flags="-mod=vendor -trimpath"
  ( cd "$SRC" && CGO_ENABLED=0 GOFLAGS="$flags" nice -n 15 /usr/local/go/bin/go build -ldflags="-s -w" -o "$SRC/ftnbilling.build" ./cmd/ispbill )
  BUNDLE=$SRC
  BIN_SRC=$SRC/ftnbilling.build
}

acquire_program() {
  step "program ($REPO)"
  local here="" parent=""
  BUNDLE=""; BIN_SRC=""; PROG_VERSION=""
  if [ -n "$SELF_DIR" ]; then here=$SELF_DIR; parent=$(dirname "$here"); fi
  if [ -n "$parent" ] && [ -f "$parent/ftnbilling" ] && [ -f "$parent/VERSION" ] && [ -f "$parent/SHA256SUMS" ] && [ -d "$parent/deploy" ]; then
    BUNDLE=$parent
    PROG_VERSION=$(head -1 "$BUNDLE/VERSION")
    note "using the bundle in $BUNDLE ($PROG_VERSION), no download"
    [ "$(cat "$BUNDLE/ARCH" 2>/dev/null || echo "$GOARCH")" = "$GOARCH" ] || die "this bundle is for $(cat "$BUNDLE/ARCH"), the server is $GOARCH"
    (cd "$BUNDLE" && sha256sum -c --quiet SHA256SUMS) || die "the bundle's files do not match its SHA256SUMS (damaged copy?)"
    BIN_SRC=$BUNDLE/ftnbilling
  elif [ "$DRY" = 1 ]; then
    note "[dry-run] would look for a GitHub release ($WANT_VERSION) for linux-$GOARCH and verify its SHA256;"
    note "[dry-run] if there is none: clone $REPO@$BRANCH, install Go if needed, build with -mod=vendor under nice"
    BUNDLE="$OPT/releases/ftnbilling-<version>-linux-$GOARCH"
    BIN_SRC=$BUNDLE/ftnbilling
    PROG_VERSION="<version>"
    return 0
  elif [ "${FROM_SOURCE:-0}" != 1 ] && get_bundle_from_release; then
    (cd "$BUNDLE" && sha256sum -c --quiet SHA256SUMS) || die "the release bundle's files do not match its SHA256SUMS"
    BIN_SRC=$BUNDLE/ftnbilling
  else
    note "no usable release: building from source ($BRANCH)"
    get_source_and_build
  fi
  ASSETS=$BUNDLE/deploy
  local f
  for f in $APP-wg-sync $APP-backup $APP-admin-reset $APP-wipe $APP-doctor $APP-mgmt $APP-backup.service $APP-backup.timer $APP-dump.service $APP-dump.timer $APP-drill.service $APP-drill.timer; do
    [ -f "$ASSETS/$f" ] || die "$ASSETS/$f is missing: incomplete bundle or source tree"
  done
  [ -f "$BIN_SRC" ] || die "program binary not found at $BIN_SRC"
  if [ "$DRY" = 1 ]; then note "[dry-run] would install $BIN_SRC as $BIN"; return 0; fi
  install -m 0755 "$BIN_SRC" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
  note "installed $BIN ($PROG_VERSION)"
}

# ---------------------------------------------------------------------------
users_and_tools() {
  step "user, folders, tools"
  if [ "$DRY" = 1 ]; then
    ASSETS="${ASSETS:-$OPT/releases/ftnbilling-<version>-linux-$GOARCH/deploy}"
    printf '    [dry-run] useradd %s; mkdir %s %s/wg %s; install tools into %s\n' "$APP" "$DATA" "$DATA" "$ETC" "$SBIN"
    return 0
  fi
  id -u $APP >/dev/null 2>&1 || useradd --system --home "$DATA" --shell /usr/sbin/nologin $APP
  install -d -o $APP -g $APP -m 750 "$DATA"
  install -d -o $APP -g $APP -m 750 "$DATA/wg"
  install -d -o root -g $APP -m 750 "$ETC"
  install -d -o root -g root -m 700 "$BACKUPS"
  local t
  for t in wg-sync backup admin-reset wipe doctor mgmt; do install -m 0755 "$ASSETS/$APP-$t" "$SBIN/$APP-$t"; done
  printf '%s\n' "$PROG_VERSION" >"$ETC/VERSION"; chmod 644 "$ETC/VERSION"
}

# ---------------------------------------------------------------------------
wireguard_routers() {
  step "WireGuard wg0 for the routers ($WG_SUBNET, udp/$WG_PORT)"
  # The private key is generated once, lives only in $WG_CONF (root, 0600) and is
  # never regenerated: every router has this server's public key in its join script.
  if [ ! -f "$WG_CONF" ]; then
    if [ "$DRY" = 1 ]; then printf '    [dry-run] generate server key, write %s\n' "$WG_CONF"
    else
      ( umask 077
        cat >"$WG_CONF" <<WGEOF
[Interface]
Address = $WG_SERVER_IP/${WG_SUBNET#*/}
ListenPort = $WG_PORT
MTU = 1380
PrivateKey = $(wg genkey)
WGEOF
      )
    fi
  fi
  if [ "$DRY" = 1 ] && [ ! -f "$WG_CONF" ]; then WG_PUB="<wg0-public-key>"
  else
    run chmod 600 "$WG_CONF"
    WG_PUB=$(awk '/^PrivateKey *=/{print $3}' "$WG_CONF" | wg pubkey)
    [ -n "$WG_PUB" ] || die "could not read the WireGuard key from $WG_CONF"
  fi
  # Root helper that applies the app's peer list (it validates the file because
  # the app is not root) and the status job the admin page reads handshakes from.
  printf 'DATA=%s\nWG_SUBNET=%s\nWG_IFACE=%s\n' "$DATA" "$WG_SUBNET" "$WG_IF" | writef 644 root:root $ETC/wg-sync.env
  printf 'd /run/%s 0750 root %s -\n' "$APP" "$APP" | writef 644 root:root /etc/tmpfiles.d/$APP.conf
  run systemd-tmpfiles --create /etc/tmpfiles.d/$APP.conf
  writef 644 root:root /etc/systemd/system/$APP-wg-sync.service <<WGEOF
[Unit]
Description=Apply the billing app's WireGuard peers
After=wg-quick@$WG_IF.service
Wants=wg-quick@$WG_IF.service

[Service]
Type=oneshot
ExecStart=$SBIN/$APP-wg-sync
WGEOF
  writef 644 root:root /etc/systemd/system/$APP-wg-sync.path <<WGEOF
[Unit]
Description=Watch for WireGuard peer changes from the billing app

[Path]
PathChanged=$DATA/wg/apply
Unit=$APP-wg-sync.service

[Install]
WantedBy=multi-user.target
WGEOF
  writef 644 root:root /etc/systemd/system/$APP-wg-status.service <<WGEOF
[Unit]
Description=Write WireGuard handshake status for the billing app
After=wg-quick@$WG_IF.service

[Service]
Type=oneshot
UMask=0027
ExecStart=/bin/sh -c 'wg show $WG_IF dump | tail -n +2 | cut -f1,3-7 >/run/$APP/$WG_IF.dump.tmp && chgrp $APP /run/$APP/$WG_IF.dump.tmp && chmod 0640 /run/$APP/$WG_IF.dump.tmp && mv -f /run/$APP/$WG_IF.dump.tmp /run/$APP/$WG_IF.dump'
WGEOF
  writef 644 root:root /etc/systemd/system/$APP-wg-status.timer <<WGEOF
[Unit]
Description=Refresh WireGuard handshake status every minute

[Timer]
OnBootSec=30s
OnUnitActiveSec=60s
AccuracySec=5s

[Install]
WantedBy=timers.target
WGEOF
  # Peers are re-applied after wg0 comes up by a systemd drop-in, not by a
  # wg-quick PostUp hook: on Ubuntu 26.04 AppArmor confines wg-quick and refuses
  # to run the helper ("Permission denied"). Older installs had the PostUp line.
  if [ "$DRY" != 1 ] && grep -q "^PostUp = $SBIN/$APP-wg-sync" "$WG_CONF" 2>/dev/null; then
    sed -i "\\#^PostUp = $SBIN/$APP-wg-sync\$#d" "$WG_CONF"
  fi
  writef 644 root:root /etc/systemd/system/wg-quick@$WG_IF.service.d/$APP.conf <<WGEOF
[Service]
ExecStartPost=$SBIN/$APP-wg-sync
WGEOF
  run systemctl daemon-reload
  try systemctl enable wg-quick@$WG_IF
  if [ "$DRY" = 1 ]; then printf '    [dry-run] systemctl start wg-quick@%s; enable --now %s-wg-sync.path %s-wg-status.timer\n' "$WG_IF" "$APP" "$APP"
  else
    systemctl is-active --quiet wg-quick@$WG_IF || systemctl start wg-quick@$WG_IF || die "could not start $WG_IF (see: journalctl -u wg-quick@$WG_IF)"
    systemctl enable --now $APP-wg-sync.path $APP-wg-status.timer >/dev/null
    if [ "$(sysctl -n net.ipv4.ip_forward 2>/dev/null || echo 0)" = 1 ] && [ "${NO_FIREWALL:-0}" = 1 ]; then
      warn "net.ipv4.ip_forward is 1 on this server and NO_FIREWALL=1 leaves the forwarding rules unmanaged: this server may route packets between networks."
    fi
  fi
}

# ---------------------------------------------------------------------------
# The forwarding rules of the remote-WinBox interface, as lines of
# /etc/ufw/before.rules (the ufw-before-forward chain runs before ufw's own
# defaults, and ufw's route rules cannot say "ICMP echo only"). They sit first
# in the chain, so nothing ufw adds later can let a device further than this:
#   wg-mgmt -> wg0:    tcp/8291 (WinBox) and ping, to the routers' tunnel range only
#   wg0 -> wg-mgmt:    replies only (established/related); a router cannot open a connection to a device
#   wg0 -> anywhere else: dropped (no router-to-router, no router-to-internet forwarding through this server)
#   wg-mgmt -> anywhere else, and wg-mgmt -> this server: dropped (no internet, no server services)
# No NAT: routers see the device's own address.
mgmt_rules() {
  cat <<EOF
# BEGIN $MGMT_MARK (written by deploy/install.sh; a re-run replaces this block)
# Remote WinBox: devices on $MGMT_IF reach tcp/8291 (WinBox) and ping on the routers' tunnel range $WG_SUBNET, nothing else.
-A ufw-before-input -i $MGMT_IF -j DROP
-A ufw-before-forward -i $MGMT_IF -m conntrack --ctstate INVALID -j DROP
-A ufw-before-forward -i $MGMT_IF -o $WG_IF -s $MGMT_SUBNET -d $WG_SUBNET -p tcp --dport 8291 -j ACCEPT
-A ufw-before-forward -i $MGMT_IF -o $WG_IF -s $MGMT_SUBNET -d $WG_SUBNET -p icmp --icmp-type echo-request -j ACCEPT
-A ufw-before-forward -i $MGMT_IF -j DROP
-A ufw-before-forward -i $WG_IF -o $MGMT_IF -s $WG_SUBNET -d $MGMT_SUBNET -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A ufw-before-forward -i $WG_IF -j DROP
-A ufw-before-forward -o $MGMT_IF -j DROP
# END $MGMT_MARK
EOF
}

# mgmt_before_rules < before.rules: the same file with the block above (replaced
# if already there) right after "# End required lines".
mgmt_before_rules() {
  BLK=$(mgmt_rules) MARK=$MGMT_MARK awk '
    BEGIN { blk = ENVIRON["BLK"]; mark = ENVIRON["MARK"] }
    index($0, "# BEGIN " mark) == 1 { skip = 1 }
    skip { if ($0 == "# END " mark) skip = 0; next }
    { print }
    !done && $0 == "# End required lines" { print blk; done = 1 }
    END { if (skip) { print "a \"# BEGIN " mark "\" line has no matching \"# END " mark "\" line" > "/dev/stderr"; exit 1 } }
  '
}

# mgmt_stale_ports: the ports wg-mgmt had before this run (the settings file's
# MGMT_PORT, and the ListenPort the interface config had), when they differ from
# the new one. Never wg0's port.
mgmt_stale_ports() {
  local p
  for p in "${MGMT_PORT_ENVFILE:-}" "${MGMT_PORT_CONF:-}"; do
    case $p in "" | *[!0-9]*) continue ;; esac
    if [ "$p" != "$MGMT_PORT" ] && [ "$p" != "$WG_PORT" ]; then echo "$p"; fi
  done | sort -un
}

# mgmt_validate_rules: asks iptables-restore (--test: nothing is loaded) whether
# the block is valid on this kernel, in a minimal ruleset of its own (the chains
# it adds to, then the block), before the real file is touched.
mgmt_validate_rules() {
  have iptables-restore || die "iptables-restore not found, so the remote WinBox rules cannot be checked (ufw needs it too)"
  local out
  out=$({ printf '*filter\n:ufw-before-input - [0:0]\n:ufw-before-forward - [0:0]\n'; mgmt_rules | grep -v '^#'; printf 'COMMIT\n'; } | iptables-restore --test 2>&1) \
    || { printf '%s\n' "$out" >&2; return 1; }
}

# ---------------------------------------------------------------------------
wireguard_mgmt() {
  step "remote WinBox: WireGuard $MGMT_IF ($MGMT_SUBNET, udp/$MGMT_PORT)"
  if [ "${NO_FIREWALL:-0}" = 1 ]; then
    note "NO_FIREWALL=1: skipped. Remote WinBox needs this server to forward packets, and its rules live in ufw"
    MGMT_ON=0
    mgmt_forwarding_off
    return 0
  fi
  MGMT_ON=1
  local host=${BASE#*://} cur
  host=${host%%[:/]*}
  if [ "$DRY" = 1 ] && [ -z "$host" ]; then host="<domain>"; fi
  # Only wg-mgmt's own settings are touched here: wg0 and its peers are not.
  if [ ! -f "$MGMT_CONF" ]; then
    if [ "$DRY" = 1 ]; then printf '    [dry-run] generate the %s server key, write %s\n' "$MGMT_IF" "$MGMT_CONF"
    else
      ( umask 077
        cat >"$MGMT_CONF" <<WGEOF
[Interface]
Address = $MGMT_SERVER_IP/${MGMT_SUBNET#*/}
ListenPort = $MGMT_PORT
MTU = 1380
PrivateKey = $(wg genkey)
WGEOF
      )
    fi
  elif [ "$DRY" != 1 ]; then
    # An existing install keeps its key and its devices. The address is the one thing a re-run cannot change.
    cur=$(awk '/^\[Peer\]/{exit} /^Address *=/{print $3; exit}' "$MGMT_CONF")
    [ "$cur" = "$MGMT_SERVER_IP/${MGMT_SUBNET#*/}" ] || die "$MGMT_CONF has Address $cur but MGMT_SUBNET is $MGMT_SUBNET. Changing the subnet means new profiles for every device and rebuilt routers: remove the devices (ftnbilling-mgmt remove), delete $MGMT_CONF and run again, or set MGMT_SUBNET back."
    cur=$(awk '/^\[Peer\]/{exit} /^ListenPort *=/{print $3; exit}' "$MGMT_CONF")
    if [ "$cur" != "$MGMT_PORT" ]; then
      MGMT_PORT_CONF=$cur
      note "changing the $MGMT_IF port from ${cur:-?} to $MGMT_PORT"
      awk -v p="$MGMT_PORT" '/^\[Peer\]/{peers=1} !peers && /^ListenPort *=/{print "ListenPort = " p; next} {print}' "$MGMT_CONF" >"$MGMT_CONF.new"
      chmod 600 "$MGMT_CONF.new"; mv -f "$MGMT_CONF.new" "$MGMT_CONF"
      try wg set "$MGMT_IF" listen-port "$MGMT_PORT"
    fi
  fi
  run chmod 600 "$MGMT_CONF"
  # What the operator tool reads (no secrets: the key stays in $MGMT_CONF).
  printf 'MGMT_IF=%s\nMGMT_SUBNET=%s\nMGMT_PORT=%s\nMGMT_ENDPOINT_HOST=%s\nWG_SUBNET=%s\n' "$MGMT_IF" "$MGMT_SUBNET" "$MGMT_PORT" "$host" "$WG_SUBNET" | writef 644 root:root $ETC/mgmt.env
  try systemctl enable wg-quick@$MGMT_IF
  if [ "$DRY" = 1 ]; then printf '    [dry-run] systemctl start wg-quick@%s\n' "$MGMT_IF"
  else
    systemctl is-active --quiet wg-quick@$MGMT_IF || systemctl start wg-quick@$MGMT_IF || die "could not start $MGMT_IF (see: journalctl -u wg-quick@$MGMT_IF)"
  fi
}

# ---------------------------------------------------------------------------
# Older installs had a second WireGuard interface for the admin pages. It is
# gone: /admin and /api are public behind the app's own login. Nothing here
# creates it; on an upgrade this retires it and keeps its files, for a while,
# in a dated folder. Does nothing (and prints nothing) on a server without it.
remove_legacy_admin_vpn() {
  local old=wg-admin conf=/etc/wireguard/wg-admin.conf dir f found=0
  local -a devs=()
  [ ! -f "$conf" ] || found=1
  for f in /root/ftnbilling-admin-*.conf; do [ -e "$f" ] && devs+=("$f"); done
  [ "${#devs[@]}" -eq 0 ] || found=1
  if [ "$DRY" != 1 ]; then
    if systemctl is-enabled --quiet "wg-quick@$old" 2>/dev/null; then found=1; fi
    if systemctl is-active --quiet "wg-quick@$old" 2>/dev/null; then found=1; fi
  fi
  [ ! -e "$SBIN/$APP-admin-peer" ] || found=1
  if [ "$DRY" != 1 ] && ip link show "$old" >/dev/null 2>&1; then found=1; fi
  [ "$found" = 1 ] || return 0
  dir=/root/ftnbilling-removed-$(date +%F)
  note "retiring the old admin VPN ($old); the router tunnel $WG_IF is not touched"
  # Stop it while its config still exists: wg-quick needs the file to take the
  # interface down. If that fails (or the file is already gone), delete the link.
  try systemctl disable --now "wg-quick@$old"
  if [ "$DRY" != 1 ] && ip link show "$old" >/dev/null 2>&1; then
    try ip link delete "$old"
  fi
  if [ "$DRY" = 1 ]; then
    printf '    [dry-run] archive the old admin VPN config and device files to %s (mode 700)\n' "$dir"
  else
    install -d -m 700 "$dir"
    for f in "$conf" "${devs[@]}"; do
      [ -f "$f" ] || continue
      mv -f "$f" "$dir/"
      note "archived $f -> $dir/"
    done
  fi
  run rm -rf "/etc/systemd/system/wg-quick@$old.service.d"
  run rm -f "$SBIN/$APP-admin-peer"
  run systemctl daemon-reload
  REMOVED_NOTE="The old admin VPN ($old) was turned off and removed. Its config and device files are in $dir (delete them when you are sure). Open the admin at $BASE/admin instead."
  # The firewall rules (udp/51821 and everything "on $old") are removed in the firewall step.
}

# ---------------------------------------------------------------------------
settings() {
  step "settings ($ENVF)"
  if [ "$DRY" = 1 ] && [ ! -f "$ENVF" ]; then
    printf '    [dry-run] create %s (random ADMIN_PASS, DATA_KEY; WG_PUBLIC_KEY)\n' "$ENVF"
    return 0
  fi
  if [ ! -f "$ENVF" ]; then
    local pass="${ADMIN_PASS:-$(rand 18)}"
    ( umask 027
      cat >"$ENVF" <<EOF
# Fast Tech Net billing server settings. Edit, then: systemctl restart $APP
BRAND_NAME=${BRAND_NAME:-Fast Tech Net}
LISTEN_ADDR=127.0.0.1:8080
DATA_DIR=$DATA
TEST_MODE=false

# Admin: https://<your domain>/admin, signed in with a password and an
# authenticator code. The app listens on loopback only; Caddy is the public side.
# First sign-in: this password plus the one-time setup code the installer prints.
ADMIN_USER=admin
ADMIN_PASS=$pass

# Public portal
PUBLIC_BASE_URL=$BASE
PUBLIC_IP=$PUBIP
SUPPORT_PHONE=${SUPPORT_PHONE:-}

# Routers connect over WireGuard. Add each router in Admin > Routers.
WG_PUBLIC_KEY=$WG_PUB
WG_ENDPOINT=$PUBIP:$WG_PORT
WG_SUBNET=$WG_SUBNET

# Metrics: routers send NetFlow to the tunnel address only
NETFLOW_ADDR=$WG_SERVER_IP:2055

# Remote WinBox: devices (sudo ftnbilling-mgmt add <name>) join this separate WireGuard
# interface and reach the routers' WinBox through this server. Routers built from the
# billing admin accept WinBox from MGMT_SUBNET. Changing it needs the routers rebuilt.
MGMT_PORT=$MGMT_PORT
MGMT_SUBNET=$MGMT_SUBNET

# M-Pesa, WhatsApp and email are set in the admin: Settings > M-Pesa, WhatsApp, Email.
# What you save there wins over the lines below. The lines are a fallback only
# (leave them commented, or fill them in once and manage the rest in the admin).
# Daraja callbacks need DOMAIN (HTTPS).
#MPESA_ENV=sandbox
#MPESA_CONSUMER_KEY=
#MPESA_CONSUMER_SECRET=
#MPESA_SHORTCODE=
#MPESA_PASSKEY=
#MPESA_CALLBACK_TOKEN=
#WA_TOKEN=
#WA_PHONE_NUMBER_ID=
#SMTP_HOST=
EOF
    )
  else
    # Upgrade: keep settings, refresh what the installer owns.
    setkv PUBLIC_IP "$PUBIP"
    # The app is only reached through Caddy (older installs listened on every address).
    setkv LISTEN_ADDR "127.0.0.1:8080"
    [ -z "${DOMAIN:-}" ] || setkv PUBLIC_BASE_URL "$BASE"
    # The app refuses to start with M-Pesa set in this file but no callback
    # token. (Settings > M-Pesa makes its own token when you save there.)
    if [ -n "$(envget MPESA_CONSUMER_KEY)" ] && [ -z "$(envget MPESA_CALLBACK_TOKEN)" ]; then
      setkv MPESA_CALLBACK_TOKEN "$(rand 24)"
      warn "MPESA_CALLBACK_TOKEN was empty and has been generated; register the C2B URLs again in Admin > Settings > M-Pesa (Register the C2B URLs)"
    fi
    # Added once and then left alone (the key never changes; the rest is yours to edit).
    addkv WG_PUBLIC_KEY "$WG_PUB"
    addkv WG_ENDPOINT "$PUBIP:$WG_PORT"
    addkv WG_SUBNET "$WG_SUBNET"
    setkv MGMT_PORT "$MGMT_PORT"
    addkv MGMT_SUBNET "$MGMT_SUBNET"
    [ -z "${ADMIN_PASS:-}" ] || setkv ADMIN_PASS "$ADMIN_PASS"
    BASE=$(envget PUBLIC_BASE_URL)
  fi
  # Remote WebFig: written only when set (or switched off); a server without it is left as it was.
  if [ -n "$WEBFIG_DOMAIN" ]; then
    setkv WEBFIG_DOMAIN "$WEBFIG_DOMAIN"
    setkv WEBFIG_LISTEN "127.0.0.1:$WEBFIG_PORT"
  elif [ "$WEBFIG_OFF" = 1 ] && grep -qs '^WEBFIG_DOMAIN=' "$ENVF"; then
    setkv WEBFIG_DOMAIN ""
  fi
  # DATA_KEY encrypts the secrets saved in Settings and national IDs. Every
  # install gets one. Losing it makes both unreadable: it is in the Drive
  # backup, and must be restored together with the database.
  addkv DATA_KEY "$(head -c 32 /dev/urandom | base64 -w0)"
  # An existing but empty "DATA_KEY=" line (a file edited by hand, or an older
  # template) would otherwise stay empty and refuse every secret setting.
  fillkv DATA_KEY "$(head -c 32 /dev/urandom | base64 -w0)"
  if [ "$DRY" != 1 ]; then chown root:$APP "$ENVF"; chmod 640 "$ENVF"; fi
}

# ---------------------------------------------------------------------------
postgres_setup() {
  step "PostgreSQL (local socket only)"
  if [ "${NO_POSTGRES:-0}" = 1 ]; then
    if [ -n "$(envget DATABASE_URL)" ]; then note "NO_POSTGRES=1, but DATABASE_URL is already set: leaving the existing database setup as it is"
    else note "NO_POSTGRES=1: skipped, the app keeps the JSON file store"; return 0; fi
  fi
  apt_install postgresql postgresql-client
  local ver=16 conf hba dburl pw have_role have_db
  if [ "$DRY" != 1 ]; then
    ver=$(pg_lsclusters -h 2>/dev/null | awk 'NR==1{print $1}')
    [ -n "$ver" ] || ver=$(ls /etc/postgresql 2>/dev/null | sort -V | tail -1)
    [ -n "$ver" ] || die "PostgreSQL installed but no cluster found"
  fi
  conf=/etc/postgresql/$ver/main
  hba=$conf/pg_hba.conf
  note "PostgreSQL $ver cluster main"
  local changed=0
  writef 644 root:root "$conf/conf.d/90-ftnbilling.conf" <<'PGEOF'
# Written by the Fast Tech Net installer (deploy/install.sh); re-runs overwrite it.
# Sized for 1 vCPU / 2 GB shared with the app. No TCP listener: socket only.
listen_addresses=''
max_connections=20
shared_buffers=128MB
effective_cache_size=512MB
work_mem=4MB
maintenance_work_mem=64MB
max_wal_size=256MB
min_wal_size=80MB
checkpoint_timeout=15min
checkpoint_completion_target=0.9
wal_compression=on
random_page_cost=1.1
effective_io_concurrency=200
max_worker_processes=4
max_parallel_workers=0
max_parallel_workers_per_gather=0
autovacuum_max_workers=2
jit=off
password_encryption=scram-sha-256
log_min_duration_statement=500ms
log_checkpoints=on
idle_in_transaction_session_timeout=60s
temp_file_limit=1GB
PGEOF
  [ "$WROTE" = 1 ] && changed=1
  if [ "$DRY" != 1 ] && ! grep -Eq "^[[:space:]]*include_dir *= *'conf.d'" "$conf/postgresql.conf"; then
    printf "\ninclude_dir = 'conf.d'\n" >>"$conf/postgresql.conf"; changed=1
  fi
  # pg_hba: the app role uses scram over the socket; above the generic peer line.
  if [ "$DRY" = 1 ]; then printf '    [dry-run] add "local ftnbilling ftnbilling scram-sha-256" (marker: ftnbilling-managed) above "local all all peer" in %s\n' "$hba"
  elif ! grep -q 'ftnbilling-managed' "$hba"; then
    local line='local   ftnbilling      ftnbilling                              scram-sha-256'
    if grep -Eq '^local[[:space:]]+all[[:space:]]+all' "$hba"; then
      awk -v l="$line" '!d && /^local[ \t]+all[ \t]+all/ {print "# ftnbilling-managed: app role over the socket (deploy/install.sh)"; print l; d=1} {print}' "$hba" >"$hba.new"
    else
      { echo "# ftnbilling-managed: app role over the socket (deploy/install.sh)"; echo "$line"; cat "$hba"; } >"$hba.new"
    fi
    chown --reference="$hba" "$hba.new"; chmod --reference="$hba" "$hba.new"
    mv -f "$hba.new" "$hba"; changed=1
  fi
  # Keep Postgres alive under memory pressure (the app is the one to be killed).
  printf '[Service]\nOOMScoreAdjust=-500\n' | writef 644 root:root /etc/systemd/system/postgresql@.service.d/ftnbilling-oom.conf
  [ "$WROTE" = 1 ] && { changed=1; run systemctl daemon-reload; }
  if [ "$changed" = 1 ] || [ "$DRY" = 1 ]; then run systemctl restart postgresql; fi
  try systemctl enable postgresql
  if [ "$DRY" != 1 ]; then
    local i; for i in $(seq 1 30); do pg_isready -q && break; sleep 1; done
    pg_isready -q || die "PostgreSQL did not come up (journalctl -u postgresql@$ver-main)"
  fi

  # Role + database. The password is made once and stored only in the env file.
  dburl=$(envget DATABASE_URL)
  have_role=""; have_db=""
  if [ "$DRY" != 1 ]; then
    have_role=$(runuser -u postgres -- psql -Atc "select 1 from pg_roles where rolname='ftnbilling'")
    have_db=$(runuser -u postgres -- psql -Atc "select 1 from pg_database where datname='ftnbilling'")
  fi
  if [ -n "$dburl" ]; then pw=$(printf '%s' "$dburl" | sed -E 's#^postgres(ql)?://[^:]+:([^@]*)@.*#\2#'); else pw=$(rand 32); fi
  if [ "$have_role" != 1 ]; then
    note "creating role ftnbilling"
    if [ "$DRY" = 1 ]; then printf '    [dry-run] CREATE ROLE ftnbilling LOGIN CONNECTION LIMIT 10 (random password); statement_timeout 30s\n'
    else runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 <<SQL
CREATE ROLE ftnbilling LOGIN CONNECTION LIMIT 10 PASSWORD '$pw';
SQL
    fi
  elif [ -z "$dburl" ]; then
    note "role exists but the settings file has no DATABASE_URL: setting a new password"
    runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 <<SQL
ALTER ROLE ftnbilling PASSWORD '$pw';
SQL
  fi
  if [ "$DRY" = 1 ]; then printf '    [dry-run] ALTER ROLE statement_timeout=30s; createdb -O ftnbilling -E UTF8 -T template0 ftnbilling; REVOKE CONNECT ON DATABASE ftnbilling FROM PUBLIC\n'
  else
    runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 -c "ALTER ROLE ftnbilling SET statement_timeout='30s'"
    if [ "$have_db" != 1 ]; then
      note "creating database ftnbilling"
      runuser -u postgres -- createdb -O ftnbilling -E UTF8 -T template0 ftnbilling
    fi
    runuser -u postgres -- psql -q -v ON_ERROR_STOP=1 -c "REVOKE CONNECT ON DATABASE ftnbilling FROM PUBLIC"
  fi

  # Upgrade from the JSON store: keep a copy first; the next start imports it.
  if [ -z "$dburl" ]; then
    if [ -f "$DATA/ispbill.json" ]; then
      note "existing JSON data found: backing it up before the app imports it into PostgreSQL"
      if [ "$DRY" = 1 ]; then printf '    [dry-run] stop %s; tar ispbill.json* -> %s/pre-postgres-%s.tgz\n' "$APP" "$BACKUPS" "$(date +%F)"
      else
        systemctl stop $APP 2>/dev/null || true
        ( umask 077; install -d -m 700 "$BACKUPS"
          # shellcheck disable=SC2046
          tar -C "$DATA" -czf "$BACKUPS/pre-postgres-$(date +%F).tgz" $(cd "$DATA" && ls -d ispbill.json* outbox.json alerts.json fleet.json spool 2>/dev/null) )
        note "saved $BACKUPS/pre-postgres-$(date +%F).tgz"
      fi
    fi
    addkv DATABASE_URL "postgres://ftnbilling:$pw@/ftnbilling?host=/var/run/postgresql&sslmode=disable&application_name=ftnbilling"
  fi
  # pool_max_conns is pgx-only: psql/pg_dump reject it. The app defaults to 4
  # connections without it, so remove it from env files written by older installers.
  if [ "$DRY" != 1 ]; then
    dburl=$(envget DATABASE_URL)
    case $dburl in *pool_max_conns=*)
      setkv DATABASE_URL "$(printf '%s' "$dburl" | sed -E 's/([?&])pool_max_conns=[0-9]+&?/\1/; s/[?&]$//')" ;;
    esac
  fi
  if [ "$DRY" != 1 ]; then
    dburl=$(envget DATABASE_URL)
    runuser -u $APP -- psql "$dburl" -Atc 'select 1' >/dev/null || die "the app role cannot connect to PostgreSQL (check pg_hba.conf and $ENVF)"
    note "connectivity check: ok (role ftnbilling, database ftnbilling, socket only)"
  fi
}

# The public Caddyfile, from CF_SITE (the domain, or :80), CF_CB (the optional
# M-Pesa callback filter) and CF_CATCHALL (the plain-HTTP redirect, domain installs only).
render_caddyfile() {
  cat <<EOF
# Public side of the billing server. /admin and /api are published: the app
# protects them with its own sign-in (password plus authenticator code) and
# refuses them from the routers' tunnel. The app listens on 127.0.0.1:8080.
#
# IMPORTANT: Caddy must be the ONLY reverse proxy in front of the app. The app
# believes the last X-Forwarded-For entry when the connection comes from
# loopback, and Caddy (v2 default) discards any X-Forwarded-For sent by the
# internet and writes the real client address. Do not add another local proxy
# (nginx, a tunnel agent, a second Caddy) that forwards to 127.0.0.1:8080, and
# do not add header_up / trusted_proxies settings here: a request that reaches
# the app from loopback without a trustworthy X-Forwarded-For would look like
# it came from this machine, and be treated as trusted.
$CF_SITE {
	encode gzip
$CF_CB
	reverse_proxy 127.0.0.1:8080
}
$CF_CATCHALL$(render_webfig_block)
EOF
}

# ---------------------------------------------------------------------------
app_and_caddy() {
  step "app service and Caddy"
  local after="network-online.target wg-quick@$WG_IF.service" pgdep=""
  if [ -n "$(envget DATABASE_URL)" ] || { [ "$DRY" = 1 ] && [ "${NO_POSTGRES:-0}" != 1 ]; }; then
    after="$after postgresql.service"; pgdep="postgresql.service"
  fi
  writef 644 root:root /etc/systemd/system/$APP.service <<EOF
[Unit]
Description=Fast Tech Net billing server
After=$after
Wants=network-online.target wg-quick@$WG_IF.service${pgdep:+ $pgdep}

[Service]
User=$APP
Group=$APP
EnvironmentFile=$ENVF
ExecStart=$BIN
Environment=GOMEMLIMIT=600MiB GOGC=50
MemoryMax=900M
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
  local site="${DOMAIN:-:80}" cbips="" cbblock=$'\t# /pay/cb/* is not limited by source address (set CADDY_MPESA_IPS to limit it).'
  if [ -n "${CADDY_MPESA_IPS:-}" ]; then
    cbips=$(printf '%s' "$CADDY_MPESA_IPS" | tr ',' ' ' | xargs)
    # Only addresses reach the Caddyfile: refuse anything else instead of writing it in.
    case "$cbips" in
      ""|*[!0-9a-fA-F:./\ ]*) die "CADDY_MPESA_IPS must be IPs or CIDRs separated by spaces or commas, got: $CADDY_MPESA_IPS" ;;
    esac
    cbblock=$(printf '\t# M-Pesa callbacks only from the addresses in CADDY_MPESA_IPS (confirm them with Safaricom).\n\t@mpesa_cb_other {\n\t\tpath /pay/cb/*\n\t\tnot remote_ip %s\n\t}\n\trespond @mpesa_cb_other "Forbidden" 403\n' "$cbips")
  fi
  local catchall=""
  if [ -n "${DOMAIN:-}" ]; then
    # Customers suspended on their router have their plain-HTTP
    # requests redirected to this server's public IP, whatever site they asked
    # for. Send them to the payment page. (Caddy already redirects http to https
    # for $DOMAIN itself; this only catches every other host name.)
    catchall=$(printf '\n# Any other plain-HTTP host (customers suspended on the router): to the payment page.\nhttp:// {\n\tredir https://%s/pay 302\n}\n' "$DOMAIN")
  fi
  CF_SITE=$site; CF_CB=$cbblock; CF_CATCHALL=$catchall
  render_caddyfile | writef 644 root:root /etc/caddy/Caddyfile
  if [ "$DRY" != 1 ]; then
    caddy validate --config /etc/caddy/Caddyfile >/dev/null 2>&1 || die "the new Caddyfile is not valid (check: caddy validate --config /etc/caddy/Caddyfile)"
  fi
  runq systemctl enable caddy
  if [ "$DRY" = 1 ]; then printf '    [dry-run] systemctl reload caddy\n'; else systemctl reload caddy 2>/dev/null || systemctl restart caddy; fi
}

# Reads "ufw status numbered" on stdin; prints the numbers of the rules that
# belong to the retired admin VPN (udp/51821, anything on wg-admin), highest first.
# The remote-WinBox interface now uses udp/51821 by default: with MGMT_PORT set to
# that port its rule is not an old one, and nothing "on wg-mgmt" ever is.
legacy_rule_numbers() {
  awk -v keep="${MGMT_PORT:-}" '
    /wg-mgmt/ { next }
    (/wg-admin/ || (/51821/ && keep != "51821")) && match($0, /^\[ *[0-9]+\]/) { n = substr($0, 2, RLENGTH-2); gsub(/ /, "", n); print n }
  ' | sort -rn
}

# ---------------------------------------------------------------------------
firewall() {
  step "firewall (ufw)"
  if [ "${NO_FIREWALL:-0}" = 1 ]; then note "NO_FIREWALL=1: skipped"; return 0; fi
  local sshp i nums
  sshp=$(sshd -T 2>/dev/null | awk '/^port /{print $2; exit}' || true); sshp=${sshp:-22}
  runq ufw allow "$sshp"/tcp
  runq ufw allow 80/tcp
  runq ufw allow 443/tcp
  runq ufw allow "$WG_PORT"/udp          # routers (wg0)
  if [ "${MGMT_ON:-0}" = 1 ]; then
    runq ufw allow "$MGMT_PORT"/udp   # remote WinBox devices (wg-mgmt)
    # The port was changed: the rule for the old one goes (wg0's port never does).
    for i in $(mgmt_stale_ports); do
      if [ "$DRY" = 1 ]; then printf '    [dry-run] ufw delete allow %s/udp (the previous wg-mgmt port)\n' "$i"; else ufw delete allow "$i"/udp >/dev/null 2>&1 || true; fi
    done
  fi
  # NetFlow arrives only through the tunnel. A legacy install that still has a
  # router on ROUTER_URL sends it over the internet, so keep that rule there.
  if ! grep -qs '^ROUTER_URL=' "$ENVF"; then
    if [ "$DRY" = 1 ]; then printf '    [dry-run] ufw delete allow 2055/udp (public)\n'; else ufw delete allow 2055/udp >/dev/null 2>&1 || true; fi
  fi
  runq ufw allow in on $WG_IF to "$WG_SERVER_IP" port 2055 proto udp
  # Routers get nothing but NetFlow: no routing to other peers or the LAN.
  # Re-added last so the deny rules stay below the allow rules on upgrades.
  if [ "$DRY" = 1 ]; then printf '    [dry-run] ufw deny in on %s; ufw route deny in on %s\n' "$WG_IF" "$WG_IF"
  else
    ufw delete deny in on $WG_IF >/dev/null 2>&1 || true
    ufw delete deny route in on $WG_IF >/dev/null 2>&1 || true
    ufw deny in on $WG_IF >/dev/null
    ufw route deny in on $WG_IF >/dev/null
  fi
  # Rules of the retired admin VPN (udp/51821, anything "on wg-admin"). Highest
  # rule number first, so the numbers do not shift while deleting.
  nums=$(ufw status numbered 2>/dev/null | legacy_rule_numbers || true)
  for i in $nums; do
    if [ "$DRY" = 1 ]; then printf '    [dry-run] ufw --force delete %s (a rule of the retired admin VPN)\n' "$i"; else ufw --force delete "$i" >/dev/null || true; fi
  done
  # Rules left by older installs that exposed the admin on another interface.
  if [ "$DRY" != 1 ]; then
    ufw delete allow in on tailscale0 >/dev/null 2>&1 || true
    ufw delete allow 41641/udp >/dev/null 2>&1 || true
  fi
  mgmt_firewall
  runq ufw default deny incoming
  # Routed (forwarded) traffic is denied unless a rule says otherwise: the only forwarding here is the wg-mgmt block.
  runq ufw default deny routed
  runq ufw --force enable
  mgmt_forwarding
}

# Forwarding between the two WireGuard interfaces, kept across reboots. It is
# switched on only here, after the ufw block above is in place and ufw is enabled,
# so there is no moment when this server routes without the rules.
MGMT_SYSCTL=${FTN_MGMT_SYSCTL:-/etc/sysctl.d/91-ftnbilling-mgmt.conf}   # (the override is for the tests)
mgmt_forwarding() {
  [ "${MGMT_ON:-0}" = 1 ] || return 0
  printf '# Fast Tech Net remote WinBox: devices on %s are routed to the routers on %s. Allowed only by the ftnbilling-mgmt block of /etc/ufw/before.rules.\nnet.ipv4.ip_forward=1\n' "$MGMT_IF" "$WG_IF" | writef 644 root:root "$MGMT_SYSCTL"
  run sysctl -q -p "$MGMT_SYSCTL"
  if [ -r /etc/ufw/sysctl.conf ] && grep -Eq '^net/ipv4/ip_forward *= *0' /etc/ufw/sysctl.conf; then
    warn "/etc/ufw/sysctl.conf sets net/ipv4/ip_forward=0, which ufw applies when it starts and which switches forwarding off again: change it to 1 for remote WinBox"
  fi
}

# Remote WinBox is off (NO_FIREWALL=1): do not leave forwarding on without the ufw
# rules. Only what this installer turned on is turned off: its own sysctl file
# is the evidence, and it is removed together with the setting.
mgmt_forwarding_off() {
  [ -f "$MGMT_SYSCTL" ] && grep -q 'Fast Tech Net remote WinBox' "$MGMT_SYSCTL" || return 0
  note "NO_FIREWALL=1: switching forwarding off again (the remote WinBox setting of an earlier run: $MGMT_SYSCTL)"
  run rm -f "$MGMT_SYSCTL"
  run sysctl -q -w net.ipv4.ip_forward=0
}

# The wg-mgmt forwarding rules in /etc/ufw/before.rules (see mgmt_rules). Written
# only when they differ, so a re-run changes nothing; loaded by ufw's reload/enable.
mgmt_firewall() {
  [ "${MGMT_ON:-0}" = 1 ] || return 0
  local f=${FTN_UFW_BEFORE_RULES:-/etc/ufw/before.rules} tmp   # (the override is for the tests)
  if [ "$DRY" = 1 ]; then printf '    [dry-run] ufw before.rules: %s forwarding block (wg-mgmt -> wg0 tcp/8291 + ping only; everything else dropped), checked with iptables-restore --test first\n' "$MGMT_IF"; return 0; fi
  [ -f "$f" ] || die "$f not found: ufw is not set up as expected"
  mgmt_validate_rules || die "the remote WinBox firewall rules were refused by iptables-restore --test (output above). $f was not changed. Is the kernel missing the conntrack or icmp match modules?"
  tmp=$(mktemp "$f.XXXXXX")
  mgmt_before_rules <"$f" >"$tmp" || { rm -f "$tmp"; die "$f was not changed: it has a '# BEGIN $MGMT_MARK' line without its '# END $MGMT_MARK' line. Fix or delete that block by hand, then run the installer again."; }
  grep -q "^# BEGIN $MGMT_MARK" "$tmp" || { rm -f "$tmp"; die "could not add the remote WinBox rules: no '# End required lines' in $f (it was not changed)"; }
  chmod --reference="$f" "$tmp"; chown --reference="$f" "$tmp"
  if cmp -s "$tmp" "$f"; then rm -f "$tmp"
  else
    cp -p "$f" "$f.pre-$MGMT_MARK"
    mv -f "$tmp" "$f"
    # Load it now when ufw is running (when it is not, "ufw enable" below loads it). A failed reload puts the old file back.
    if ufw status 2>/dev/null | grep -q '^Status: active'; then
      if ! ufw reload; then
        cp -p "$f.pre-$MGMT_MARK" "$f"
        ufw reload >/dev/null 2>&1 || true
        die "ufw reload failed with the remote WinBox rules in $f; the previous $f was put back. See the ufw output above (journalctl -u ufw)."
      fi
    fi
    note "remote WinBox forwarding rules written to $f"
  fi
}

# ---------------------------------------------------------------------------
backups() {
  step "backups (6-hourly dumps, monthly restore drill, weekly Drive upload)"
  local f BK_CAL
  # The old daily cron is replaced by the dump timer (which also makes the daily file tar).
  if [ -e /etc/cron.daily/$APP-backup ]; then run rm -f /etc/cron.daily/$APP-backup; note "removed the old daily cron job"; fi
  if [ "$DRY" = 1 ]; then
    printf '    [dry-run] install %s-backup.{service,timer} %s-dump.{service,timer} %s-drill.{service,timer}\n' "$APP" "$APP" "$APP"
    printf '    [dry-run] enable --now %s-dump.timer (and %s-drill.timer with PostgreSQL); weekly Drive timer only after: sudo %s-backup setup\n' "$APP" "$APP" "$APP"
    BK_MSG="Google Drive weekly backup: run  sudo $APP-backup setup"
    return 0
  fi
  for f in backup dump drill; do install -m 0644 "$ASSETS/$APP-$f.service" /etc/systemd/system/$APP-$f.service; done
  install -m 0644 "$ASSETS/$APP-dump.timer" /etc/systemd/system/$APP-dump.timer
  install -m 0644 "$ASSETS/$APP-drill.timer" /etc/systemd/system/$APP-drill.timer
  BK_CAL='Sun *-*-* 02:30:00 Africa/Nairobi'
  if ! systemd-analyze calendar "$BK_CAL" >/dev/null 2>&1; then BK_CAL='Sat *-*-* 23:30:00 UTC'; fi   # same moment, older systemd
  sed "s|^OnCalendar=.*|OnCalendar=$BK_CAL|" "$ASSETS/$APP-backup.timer" >/etc/systemd/system/$APP-backup.timer
  chmod 644 /etc/systemd/system/$APP-backup.timer
  systemctl daemon-reload
  systemctl enable --now $APP-dump.timer >/dev/null
  if [ -n "$(envget DATABASE_URL)" ]; then systemctl enable --now $APP-drill.timer >/dev/null; fi
  BK_MSG="Google Drive weekly backup: run  sudo $APP-backup setup"
  if grep -qs '^\[ftn-backup\]' /root/.config/rclone/rclone.conf; then
    systemctl enable --now $APP-backup.timer >/dev/null 2>&1 || true   # re-arm after a reinstall; harmless if already on
    BK_MSG="Google Drive weekly backup: on   (status: sudo $APP-backup status)"
  fi
}

# ---------------------------------------------------------------------------
# app_cli ARGS...: run the program's own command line as the service user with
# the few settings the database commands need.
app_cli() {
  runuser -u $APP -- env DATABASE_URL="$(envget DATABASE_URL)" DATA_DIR="$DATA" "$BIN" "$@"
}

# On an upgrade with PostgreSQL: a verified dump first (kept outside the
# rotation), then the migrations, before the service is restarted on the new
# program. A failure stops the installer with the old data untouched.
pre_upgrade_database() {
  local dump keep
  if [ "$UPGRADE" != 1 ] || [ -z "$(envget DATABASE_URL)" ]; then return 0; fi
  note "taking a database dump before the upgrade"
  "$SBIN/$APP-backup" dump || die "the pre-upgrade database dump failed, so nothing was changed in the database (see: journalctl -u $APP-dump)"
  dump=$(ls -1t "$BACKUPS"/db/*.dump 2>/dev/null | head -1 || true)
  [ -n "$dump" ] || die "no database dump found in $BACKUPS/db after the pre-upgrade dump"
  keep="$BACKUPS/pre-upgrade-$(date +%F-%H%M%S).dump"
  ( umask 077; cp -a "$dump" "$keep" )
  note "pre-upgrade dump kept at $keep"
  note "running the database migrations"
  app_cli migrate || die "the database migration failed. Restore the dump above (deploy/BACKUP.md) and report the error."
}

# A one-time admin setup code (the wrapper stops the service while it runs and starts it again), for signing in until the authenticator is set up.
# Guarded: a program without the -setup-code option (before the sign-in rebuild)
# must not make the installer fail.
make_setup_code() {
  SETUP_OUT=""
  local out rc=0
  out=$("$SBIN/$APP-admin-reset" --setup-code 2>&1) || rc=$?
  if [ "$rc" = 0 ]; then SETUP_OUT=$out
  else
    SETUP_OUT=""
    warn "no admin setup code was made ($(printf '%s' "$out" | tail -1)). If this program is older than the new sign-in, update it; then run: sudo $APP-admin-reset --setup-code"
  fi
}

start_and_check() {
  step "start, first dump, health check"
  if [ "$DRY" = 1 ]; then
    printf '    [dry-run] upgrade only: %s-backup dump, keep it as %s/pre-upgrade-<time>.dump, then: %s migrate\n' "$APP" "$BACKUPS" "$APP"
    printf '    [dry-run] systemctl enable + restart %s; wait for /healthz\n' "$APP"
    printf '    [dry-run] %s db-check (row counts and the report of flats sharing a MAC address)\n' "$APP"
    printf '    [dry-run] %s-admin-reset --setup-code (one-time admin setup code, printed in the final banner)\n' "$APP"
    printf '    [dry-run] %s-backup dump; %s-backup drill; %s-doctor\n' "$APP" "$APP" "$APP"
    return 0
  fi
  pre_upgrade_database
  systemctl daemon-reload
  systemctl enable $APP >/dev/null
  systemctl restart $APP
  local i hz=""
  for i in $(seq 1 60); do
    hz=$(curl -fsS --max-time 3 http://127.0.0.1:8080/healthz 2>/dev/null || true)
    [ -n "$hz" ] && break
    systemctl is-active --quiet $APP || break
    sleep 2
  done
  [ -n "$hz" ] || { journalctl -u $APP -n 30 --no-pager; die "the service did not answer /healthz (settings in $ENVF)"; }
  HEALTH=$hz
  if [ -n "$(envget DATABASE_URL)" ]; then
    local chk
    chk=$(app_cli db-check 2>&1 || true)
    DB_COUNTS=$(printf '%s\n' "$chk" | sed -n '1,/^applied payments/p' | tr -s ' ')
    MAC_REPORT=$(printf '%s\n' "$chk" | sed -n '/^MAC conflicts:/,$p')
  fi
  make_setup_code
  "$SBIN/$APP-backup" dump || warn "the first database dump failed (see: journalctl -u $APP-dump)"
  if [ -n "$(envget DATABASE_URL)" ]; then "$SBIN/$APP-backup" drill >/dev/null 2>&1 || warn "the first restore drill failed: sudo $APP-backup drill"; fi
}

doctor_and_banner() {
  step "doctor"
  if [ "$DRY" = 1 ]; then printf '    [dry-run] %s/%s-doctor\n' "$SBIN" "$APP"; DOC_RC=0
  else
    "$SBIN/$APP-doctor" && DOC_RC=0 || DOC_RC=$?
  fi
  local backend="JSON file"
  if [ "$DRY" = 1 ]; then
    printf '\n    [dry-run] finished: nothing was changed.\n'
    return 0
  fi
  local pwtext="your existing admin password"
  if [ "$UPGRADE" != 1 ]; then pwtext="the password $(envget ADMIN_PASS)"; fi
  if [ -n "$(envget DATABASE_URL)" ]; then backend="PostgreSQL (local socket)"; fi
  cat <<EOF

────────────────────────────────────────────────────────────────────
 Fast Tech Net billing is running ($PROG_VERSION)

 Portal (public):   $BASE/portal
 Admin (public):    $BASE/admin    (password + authenticator code)
 Storage:           $backend
 Settings:          Admin > Settings (M-Pesa, WhatsApp, Email; no restart needed)
                    $ENVF   (restart-only values, then: systemctl restart $APP)
 Locked out?        sudo $APP-admin-reset   (--generate, --setup-code, --reset-totp)
 Logs:              journalctl -u $APP -f
 Health:            ${HEALTH:-}
 WireGuard:         $WG_IF (routers) udp/$WG_PORT, server key $WG_PUB
EOF
  if [ "$MGMT_ON" = 1 ]; then echo " Remote WinBox: sudo $APP-mgmt add <your-device>"; fi
  if [ -n "$MAC_REPORT" ]; then
    printf '\n ATTENTION, flats that share a MAC address (not provisioned until fixed in Admin):\n%s\n' "$(printf '%s\n' "$MAC_REPORT" | sed 's/^/   /')"
  elif printf '%s' "${DB_COUNTS:-}" | grep -q 'applied payments'; then
    echo " MAC conflicts:     none"
  elif [ -n "$(envget DATABASE_URL)" ]; then
    warn "db-check gave no answer, so the MAC-conflict report is missing: run  sudo -u $APP $BIN db-check" 
  fi
  if [ -n "${DB_COUNTS:-}" ]; then printf ' Database:\n%s\n' "$(printf '%s\n' "$DB_COUNTS" | sed 's/^/                    /')"; fi
  if [ -n "$SETUP_OUT" ]; then
    cat <<EOF

 FIRST SIGN-IN (shown once; run  sudo $APP-admin-reset --setup-code  for a new one):
   1. Open $BASE/admin
   2. Sign in as $(envget ADMIN_USER) with $pwtext
      and the setup code below.
   3. Scan the QR code with an authenticator app and save the recovery codes offline.

$(printf '%s\n' "$SETUP_OUT" | sed 's/^/   /')
EOF
  fi
  if [ -n "$REMOVED_NOTE" ]; then printf '\n %s\n' "$REMOVED_NOTE"; fi
  cat <<EOF

 Next: in the admin, Routers -> Add router. It gives you one command to
 paste into the router's MikroTik terminal; the router then joins the tunnel.
EOF
  case "$BASE" in https://*) ;; *) echo " NOTE: join commands need HTTPS. Re-run this installer with DOMAIN=your.domain first." ;; esac
  if grep -q '^ROUTER_URL=' "$ENVF"; then
    echo " NOTE: ROUTER_URL in $ENVF is no longer used. Add that router in the admin (Routers, Add router) and paste its line on it."
  fi
  echo " ${BK_MSG:-}"
  echo " Health check any time:  sudo $APP-doctor"
  if [ "$DOC_RC" != 0 ]; then echo " The doctor above found problems (FAIL lines): fix them before relying on this server."; fi
  echo "────────────────────────────────────────────────────────────────────"
}

main() {
  cd /
  # Everything below was already read; never let a command swallow the rest of a piped script.
  exec </dev/null
  PROG_VERSION=""; BUNDLE=""; BIN_SRC=""; ASSETS=""; BK_MSG=""; HEALTH=""; DB_COUNTS=""; DOC_RC=0
  MAC_REPORT=""; SETUP_OUT=""; REMOVED_NOTE=""; UPGRADE=0; MGMT_ON=0
  WG_PUB=""; PUBIP=""; BASE=""; MEM_MB=0; GOARCH=amd64
  preflight
  packages
  system_setup
  caddy_rclone
  acquire_program
  users_and_tools
  wireguard_routers
  remove_legacy_admin_vpn
  settings
  wireguard_mgmt
  postgres_setup
  app_and_caddy
  firewall
  backups
  start_and_check
  doctor_and_banner
}

main "$@"
exit 0
