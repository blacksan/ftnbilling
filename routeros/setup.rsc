# LEGACY: the one-router setup. New buildings join with the one-time command
# from Admin > Buildings > Add building (see deploy/README.md).
# ============================================================================
# ispbill - RouterOS 7 setup for MikroTik RB5009UG+S+IN (arm64)  -- TESTING SETUP
#
# HOW TO USE: do NOT blindly /import this whole file on a production router.
# Work through it section by section: paste each section into a terminal,
# check the output, then continue. Anything marked  >>> CHANGE <<<  is a
# placeholder you must replace first. Written from RouterOS 7 documentation;
# it has NOT been run on a real RB5009 by the author, so expect to adjust.
#
# Layout after setup:
#   container ispbill  172.17.0.2/24 on veth-ispbill (bridge br-containers)
#   router             172.17.0.1/24 on br-containers (REST API on tcp/8081)
#   customer hotspot   vlan100-customers (on sfp-sfpplus1, from the OLT) and the
#                      optional bench port ether5, both in bridge br-hotspot
#                      10.5.50.1/24, DHCP .10-.254, hotspot server hotspot1
#
# ============================================================================
# PORT NAMES USED BELOW (literal values; each is marked >>> CHANGE IF NEEDED <<<)
#   sfp-sfpplus1 = port the V1600GS-R OLT is cabled to (10G DAC)
#   100          = service VLAN the OLT delivers customers on
#   ether5       = optional bench-test port
# Search this file for "CHANGE IF NEEDED" if your layout differs. No variables
# are used, so every section can be pasted on its own.
# ============================================================================

# ---------------------------------------------------------------------------
# OLT SIDE (configured on the V-SOL V1600GS-R, NOT on this router)
# ---------------------------------------------------------------------------
# The OLT is a pure Layer 2 device. All billing and control is on this router.
#  - Uplink port (10G SFP+): carries the service VLAN 100 TAGGED.
#  - Each ONT service/WAN port: untagged on the ONT, translated/mapped to VLAN
#    100 on the OLT (so the router only ever sees VLAN 100).
#  - Each ONT runs in ROUTE mode (NAT + Wi-Fi) with its WAN set to DHCP. The
#    router therefore sees ONE client per flat: the ONT's WAN MAC/IP. One
#    captive-page login unlocks every device inside that flat.
#  - Enable ONT-to-ONT isolation on the OLT (port/user isolation, "no
#    inter-ONU forwarding") so flats cannot see each other at Layer 2.
#  - Do not run DHCP, PPPoE or other L3 services on the OLT.


# ---------------------------------------------------------------------------
# 0. PREREQUISITES (manual, not executed)
# ---------------------------------------------------------------------------
# a) The "container" extra package must be installed. Download "extra
#    packages" for your exact RouterOS version from mikrotik.com, upload
#    container-<version>-arm64.npk to Files, reboot. Check: /system/package/print
#
# b) Container mode must be enabled in device-mode. This needs PHYSICAL access:
#      /system/device-mode/update container=yes hotspot=yes fetch=yes
#    (7.20 also gates hotspot and /tool/fetch behind device-mode; enable all
#    three in one go so you only press the button once.) Then, within the time the router tells you (about 5 minutes), press the
#    reset button briefly (or power-cycle the router, depending on version).
#    Without this step /container commands will not work. Verify with:
#      /system/device-mode/print
#
# c) A USB stick formatted ext4 and mounted as usb1. On 7.20:
#      /disk/format usb1 file-system=ext4 label=usb1
#    The RB5009 can flip the slot name between usb1 and usb2 across reboots,
#    which breaks root-dir. Pin the mount point to the filesystem label:
#      /disk/settings/set default-mount-point-template="[fs-label]"
#      /disk/set [find] mount-point-template="[fs-label]"
#    Check: /disk/print   (containers on internal flash are not recommended)
#
# c2) The RB5009 has no clock battery. Set the time zone and NTP so the
#     clock is right once WAN is up (expiry times depend on it):
#      /system/clock/set time-zone-name=Africa/Nairobi
#      /system/ntp/client/set enabled=yes servers=time.google.com,pool.ntp.org
#     Before WAN exists, set it by hand after each reboot:
#      /system/clock/set date=2026-11-17 time=20:00:00
#
# c3) If you started from the default config, sfp-sfpplus1 (and ether5 if
#     you use it as the bench port) are members of the default "bridge".
#     Remove them before section 4 or the bridge port add fails:
#      /interface/bridge/port/remove [find interface=sfp-sfpplus1]
#      /interface/bridge/port/remove [find interface=ether5]
#
# d) Upload dist/ispbill-arm64.tar to usb1 (Files, WinBox drag-drop, or scp).
#    Upload the hotspot-ispbill folder (routeros/hotspot) to the router root.


# ---------------------------------------------------------------------------
# 1. CONTAINER NETWORK: bridge + veth, router address, NAT
# ---------------------------------------------------------------------------
/interface/veth add name=veth-ispbill address=172.17.0.2/24 gateway=172.17.0.1 comment="ispbill container"
/interface/bridge add name=br-containers comment="ispbill containers"
/ip/address add address=172.17.0.1/24 interface=br-containers comment="ispbill container gateway"
/interface/bridge/port add bridge=br-containers interface=veth-ispbill

# Let the container reach the internet (Daraja later, DNS now).
# >>> CHECK <<< "WAN" must be your WAN interface list: /interface/list/print
/ip/firewall/nat add chain=srcnat action=masquerade src-address=172.17.0.0/24 out-interface-list=WAN comment="ispbill container internet"

# DNS: the container uses the router as resolver.
/ip/dns set allow-remote-requests=yes

# Input rules so the container may talk to the router (DNS + REST API).
# They go to the top so they sit before the default "drop everything else" rule.
/ip/firewall/filter add chain=input action=accept src-address=172.17.0.0/24 protocol=udp dst-port=53 place-before=0 comment="ispbill: container DNS"
/ip/firewall/filter add chain=input action=accept src-address=172.17.0.0/24 protocol=tcp dst-port=53 place-before=0 comment="ispbill: container DNS tcp"
/ip/firewall/filter add chain=input action=accept src-address=172.17.0.2 protocol=tcp dst-port=8081 place-before=0 comment="ispbill: container -> REST API"


# ---------------------------------------------------------------------------
# 2. REST API USER + SERVICE (inside the router only)
# ---------------------------------------------------------------------------
/user/group add name=ispbill-api policy=read,write,test,sensitive,api,rest-api comment="ispbill REST API"

# >>> CHANGE <<< replace CHANGE_ME_ROUTER_PASS (same value goes in the env list below)
/user add name=ispbill group=ispbill-api password="CHANGE_ME_ROUTER_PASS" address=172.17.0.0/24 comment="ispbill REST user"

# REST is served by the www service. Port 80 is left alone (the hotspot uses
# it), so www moves to 8081 and only accepts the container network.
/ip/service set www port=8081 address=172.17.0.0/24 disabled=no

# Hotspot clients must never reach the REST port, whatever the service says.
/ip/firewall/filter add chain=input action=drop in-interface=br-containers protocol=tcp dst-port=8081 src-address=!172.17.0.2 place-before=0 comment="ispbill: REST only from the container"
# (the br-hotspot drop is added in section 5 once the bridge exists)


# ---------------------------------------------------------------------------
# 3. CONTAINER CONFIG, ENV LIST, MOUNT, CONTAINER
# ---------------------------------------------------------------------------
/container/config set registry-url=https://registry-1.docker.io tmpdir=usb1/pull

# Written for RouterOS 7.20+ (envs use list=, the container uses envlists=).
# On RouterOS older than 7.20 use name= and envlist= instead.
# NOTE: property names of /container/envs and /container/mounts changed between
# RouterOS 7.x releases (newer releases group env entries with a "list=" property
# instead of "name=", and mounts may use "list=" too). Check what your version
# accepts BEFORE pasting this section:
#   /container/envs/add ?
#   /container/mounts/add ?
# and adjust the lines below (for example name=ispbill -> list=ispbill, and the
# container's envlist= / mounts= properties). The same caution applies to
# "/file add type=directory": if it errors, create the folder usb1/ispbill-data
# by hand (WinBox: Files > New Folder).
#
# >>> CHANGE <<< every CHANGE_ME_* value. ADMIN_PASS must not be "admin".
/container/envs
add list=ispbill key=TEST_MODE value=true comment="TESTING: test payment button on. Set false for real use."
add list=ispbill key=BRAND_NAME value="Fast Tech Net"
add list=ispbill key=ROUTER_URL value="http://172.17.0.1:8081"
add list=ispbill key=ROUTER_USER value=ispbill
add list=ispbill key=ROUTER_PASS value="CHANGE_ME_ROUTER_PASS"
add list=ispbill key=HOTSPOT_SERVER value=hotspot1
add list=ispbill key=ADMIN_USER value=admin
add list=ispbill key=ADMIN_PASS value="CHANGE_ME_ADMIN_PASS"
add list=ispbill key=SUPPORT_PHONE value="07XXXXXXXX"
# Metrics page (optional; these are the defaults). Check the interface names.
add list=ispbill key=WAN_INTERFACE value=ether1
add list=ispbill key=OLT_INTERFACE value=sfp-sfpplus1
add list=ispbill key=WAN_CAPACITY_MBPS value=500
add list=ispbill key=FUP_TB value=15
add list=ispbill key=NETFLOW_ADDR value=":2055"
# Leave the M-Pesa variables out until Daraja is ready (see README). Then add:
#   MPESA_ENV=sandbox  MPESA_CONSUMER_KEY  MPESA_CONSUMER_SECRET
#   MPESA_SHORTCODE    MPESA_PASSKEY       PUBLIC_BASE_URL
#   MPESA_CALLBACK_TOKEN (optional random string)

# The data directory must exist on the USB stick. If this errors, create the
# folder "ispbill-data" on usb1 by hand (WinBox: Files > New Folder).
/file add name=usb1/ispbill-data type=directory
# (On releases where mounts also use list=, write list=ispbill-data instead.)
/container/mounts add name=ispbill-data src=usb1/ispbill-data dst=/data

# Import the image tarball and create the container.
/container add file=usb1/ispbill-arm64.tar interface=veth-ispbill envlists=ispbill mounts=ispbill-data root-dir=usb1/ispbill-root hostname=ispbill dns=172.17.0.1 logging=yes start-on-boot=yes comment="ispbill"

# Wait until /container/print shows status=stopped (extracted), then start it:
#   /container/print
#   /container/start [find comment=ispbill]
# Logs (logging=yes sends stdout to the router log):
#   /log/print where topics~"container"


# ---------------------------------------------------------------------------
# 4. CUSTOMER NETWORK: VLAN from the OLT + optional bench port -> br-hotspot
# ---------------------------------------------------------------------------
# Make sure the OLT port is not already in another bridge:
#   /interface/bridge/port/print
# >>> CHANGE IF NEEDED <<<  interface=sfp-sfpplus1 (OLT port) and vlan-id=100 (service VLAN)
/interface/vlan add name=vlan100-customers interface=sfp-sfpplus1 vlan-id=100 comment="ispbill customers from OLT"
/interface/bridge add name=br-hotspot comment="ispbill customers"
# horizon=1 on every customer-facing port: ports with the same horizon never
# forward to each other, so the bench port and the OLT VLAN stay separated.
# (A bridge never sends a frame back out the port it came in on, so flats that
# arrive over the single OLT port cannot talk to each other through this router;
# isolation between ONTs themselves is the OLT's job, see the notes at the top.)
/interface/bridge/port add bridge=br-hotspot interface=vlan100-customers horizon=1 comment="ispbill: OLT customers"

# OPTIONAL bench-test port: lets you test with a laptop or a phone behind a
# plain access point, without the OLT. Remove ether5 from any other bridge first:
#   /interface/bridge/port/remove [find interface=ether5]
# Uncomment to use:
# >>> CHANGE IF NEEDED <<<  interface=ether5
#/interface/bridge/port add bridge=br-hotspot interface=ether5 horizon=1 comment="ispbill: bench test port"

/ip/address add address=10.5.50.1/24 interface=br-hotspot comment="ispbill hotspot"
/ip/pool add name=pool-hotspot ranges=10.5.50.10-10.5.50.254
# Long lease: one lease per flat (the ONT WAN), so it should not flap.
/ip/dhcp-server add name=dhcp-hotspot interface=br-hotspot address-pool=pool-hotspot lease-time=1d disabled=no
/ip/dhcp-server/network add address=10.5.50.0/24 gateway=10.5.50.1 dns-server=10.5.50.1
# Internet for customers (skip if your existing WAN masquerade already covers all sources):
/ip/firewall/nat add chain=srcnat action=masquerade src-address=10.5.50.0/24 out-interface-list=WAN comment="ispbill customers internet"

# ---------------------------------------------------------------------------
# 5. HOTSPOT (custom login page redirects to the container)
# ---------------------------------------------------------------------------
# Login is tied to the flat's MAC: ispbill creates each hotspot user with
# mac-address=<ONT WAN MAC>. mac-cookie keeps the flat logged in without
# re-entering anything (the "MAC" is the ONT, so this is per flat).
# The folder "hotspot-ispbill" must already be uploaded to the router root.
/ip/hotspot/profile add name=ispbill-hsprof hotspot-address=10.5.50.1 html-directory=hotspot-ispbill login-by=http-pap,http-chap,mac-cookie http-cookie-lifetime=1d
/ip/hotspot add name=hotspot1 interface=br-hotspot address-pool=pool-hotspot profile=ispbill-hsprof disabled=no

# Let unauthenticated clients reach the portal in the container.
/ip/hotspot/walled-garden/ip add action=accept dst-address=172.17.0.2 comment="ispbill portal"
# Do NOT whitelist captive-portal check hosts (connectivitycheck.gstatic.com,
# captive.apple.com...): phones must fail those checks so they pop up the sign-in page.

# Keep hotspot clients away from the router's REST service and the rest of the
# container network (portal port 8080 stays reachable).
/ip/firewall/filter add chain=input action=drop in-interface=br-hotspot protocol=tcp dst-port=8081 place-before=0 comment="ispbill: hotspot clients -> REST blocked"
/ip/firewall/filter add chain=forward action=accept in-interface=br-hotspot dst-address=172.17.0.2 protocol=tcp dst-port=8080 place-before=0 comment="ispbill: hotspot clients -> portal"
/ip/firewall/filter add chain=forward action=drop in-interface=br-hotspot dst-address=172.17.0.0/24 place-before=1 comment="ispbill: hotspot clients -> rest of container net blocked"

# No customer-to-customer traffic through the router either (also catches proxy-ARP tricks):
/ip/firewall/filter add chain=forward action=drop in-interface=br-hotspot out-interface=br-hotspot place-before=2 comment="ispbill: flats isolated from each other"

# OPTIONAL: stop hotspot clients reaching your private LANs (adjust ranges):
#/ip/firewall/filter add chain=forward action=drop in-interface=br-hotspot dst-address=192.168.0.0/16 place-before=3 comment="ispbill: hotspot isolation"


# ---------------------------------------------------------------------------
# 5b. TRAFFIC FLOW (feeds the "Services" charts on /admin/metrics; optional)
# ---------------------------------------------------------------------------
# The router exports NetFlow v5 about traffic on br-hotspot to the container
# (UDP 2055). Per-flat byte counts do NOT depend on this (they come from the
# hotspot sessions); only the per-service split does.
/ip/traffic-flow set enabled=yes interfaces=br-hotspot cache-entries=16k active-flow-timeout=1m inactive-flow-timeout=15s
/ip/traffic-flow/target add dst-address=172.17.0.2 port=2055 version=5 comment="ispbill metrics"
# The router sends from 172.17.0.1 into the container network; if you have an
# input/forward drop rule covering br-containers, allow UDP 2055 to 172.17.0.2:
#/ip/firewall/filter add chain=forward action=accept protocol=udp dst-address=172.17.0.2 dst-port=2055 comment="ispbill netflow" place-before=0
#
# FASTTRACK NOTE: fasttracked connections BYPASS traffic-flow, so they would be
# missing from the Services charts. If you have the default "fasttrack-connection"
# rule in /ip/firewall/filter, either disable it or exclude the hotspot network:
#   /ip/firewall/filter print where action=fasttrack-connection
#   /ip/firewall/filter set [find action=fasttrack-connection] src-address=!10.5.50.0/24 dst-address=!10.5.50.0/24
# (Hotspot traffic is mostly not fasttracked anyway, but check.)


# ---------------------------------------------------------------------------
# 6. LATER, WHEN M-PESA GOES LIVE (left commented on purpose)
# ---------------------------------------------------------------------------
# Daraja must reach the container from the internet over HTTPS. Easiest and
# safest is a tunnel/reverse proxy that terminates TLS in front of the router
# and forwards to 172.17.0.2:8080. A direct dst-nat needs a public IP and a TLS
# terminator (the app serves plain HTTP). Example, only if you have one:
#
#/ip/firewall/nat add chain=dstnat action=dst-nat in-interface-list=WAN protocol=tcp dst-port=443 to-addresses=172.17.0.2 to-ports=8080 comment="ispbill daraja callbacks (HTTP!)"
#/ip/firewall/filter add chain=forward action=accept in-interface-list=WAN dst-address=172.17.0.2 protocol=tcp dst-port=8080 comment="ispbill daraja callbacks" place-before=0
#
# Restrict that to Safaricom's published callback source IPs where possible, and
# set MPESA_CALLBACK_TOKEN so forged callbacks are refused. Only /pay/cb/* must
# be exposed, never /admin: put the admin UI behind the tunnel's path rules.
