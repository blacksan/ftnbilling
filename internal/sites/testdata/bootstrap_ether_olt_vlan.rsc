# Fast Tech Net: bootstrap script for Block A (s1), generated 2026-11-18 09:00 UTC
# Builds this router from nothing: WAN, admin LAN, customer network, DHCP,
# firewall, services, the WireGuard tunnel to the billing server and the API
# user. Single use. It holds this router's WireGuard private key and API
# password, so keep it private. It removes itself from the router when it
# finishes, and also when it stops on an error (the whole body is wrapped).
# Every object it creates has a comment that starts with "ftn:". It runs only
# on a blank router: if it finds "ftn:" objects of an earlier run, or anything
# that is not ours, it refuses and changes nothing; a rebuild always goes
# through the paste line, which resets the router first. It never prints a secret.
# Ports are off by default: only the uplink and the admin port are up when it finishes; the admin port has no
# internet except the server's own names (tcp 80/443). The router is never open: the firewall hold (section 2) is in place before the
# WAN is up, the WAN client is created switched off and is switched on only
# after the whole rule set is in (section 6).
:local fails 0
:local abort ""
:local report ""
:local ver ""
:local mgmt ""
:local result "fail"
:do {

:put "ftn 0/12: waiting for the ports, checking the router"
:onerror e in={
  :set ver [/system/resource/get version]
  :put ("   RouterOS " . $ver)
  :local major [:tonum [:pick $ver 0 [:find $ver "."]]]
  :if ($major < 7) do={ :error "RouterOS 7 or newer is required" }
  :local need {"ether1";"ether3";"ether2";"ether5"}
  :local waited 0
  :local ready false
  :local missing ""
  :while (!$ready && $waited <= 60) do={
    :set ready true
    :set missing ""
    :foreach n in=$need do={
      :if ([:len [/interface/find where name=$n]] = 0) do={ :set ready false; :set missing ($missing . " " . $n) }
    }
    :if (!$ready) do={ :delay 2s; :set waited ($waited + 2) }
  }
  :if (!$ready) do={ :error ("these ports do not exist on the router:" . $missing . ". Fix the port names in the billing admin") }
  # The script runs on a blank router only. It never rebuilds in place: that would tear down the admin LAN under
  # the operator's own session. A rebuild goes through the paste line, which resets the router first.
  :local ours ""
  :if ([:len [/interface/bridge/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " bridge;") }
  :if ([:len [/ip/address/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " address;") }
  :if ([:len [/ip/dhcp-server/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " dhcp-server;") }
  :if ([:len [/ip/dhcp-client/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " dhcp-client;") }
  :if ([:len [/ip/firewall/filter/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " firewall rule;") }
  :if ([:len [/ip/firewall/nat/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " nat rule;") }
  :if ([:len [/interface/wireguard/find where comment~"^ftn:"]] > 0) do={ :set ours ($ours . " wireguard;") }
  :if ([:len $ours] > 0) do={ :error ("this router has already been set up by a Fast Tech Net script:" . $ours . " Nothing was changed. To rebuild it, run /system/reset-configuration no-defaults=yes keep-users=yes on the router (or press the router reset button), then paste a new line from the billing admin (running this script again on a built router is refused)") }
  # Foreign settings: not ours either, nothing is replaced. (After the reset of the paste line the router is empty.)
  :local foreign ""
  :foreach i in=[/ip/address/find where dynamic=no] do={ :if (!([/ip/address/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " address " . [/ip/address/get $i address] . ";") } }
  :foreach i in=[/interface/bridge/find] do={ :if (!([/interface/bridge/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " bridge " . [/interface/bridge/get $i name] . ";") } }
  :foreach i in=[/ip/dhcp-server/find] do={ :if (!([/ip/dhcp-server/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " dhcp-server " . [/ip/dhcp-server/get $i name] . ";") } }
  :foreach i in=[/ip/firewall/filter/find where dynamic=no] do={ :if (!([/ip/firewall/filter/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " firewall rule;") } }
  :foreach i in=[/ip/firewall/nat/find where dynamic=no] do={ :if (!([/ip/firewall/nat/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " nat rule;") } }
  :if ([:len $foreign] > 0) do={ :error ("this router already has settings that are not ours:" . $foreign . " Nothing was changed. To rebuild, run /system/reset-configuration no-defaults=yes keep-users=yes on the router (or press the router reset button), then paste a new line from the billing admin") }
} do={ :set report ($report . "0 wait and guard: " . $e . "\n"); :set abort ("ftn: section 0 wait and guard failed: " . $e); :error $abort }

:put "ftn 1/12: name, clock, trust anchors"
:onerror e in={
  /system/identity set name="FTN-Block-A"
  /system/clock set time-zone-name=Africa/Nairobi
  /system/ntp/client set enabled=yes servers=162.159.200.1,162.159.200.123
  # Upstream DNS now, not in section 7: the failure report must be able to find the server whatever stops.
  /ip/dns set servers=1.1.1.1,8.8.8.8
  :do { /certificate/settings set builtin-trust-anchors=trusted } on-error={ :put "   (this RouterOS has no built-in trust anchors setting)" }
} do={ :set report ($report . "1 system: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 1 system failed, continuing: " . $e) }

:put "ftn 2/12: firewall hold, then WAN ether1 (dhcp)"
:onerror e in={
  # First of all, before any WAN client exists: a hold that drops new and invalid traffic from the WAN, both to
  # the router (input) and through it (forward). Established and related traffic is not touched, so the
  # router's own outbound flows (the report, NTP) come back. The WAN client below is created switched off and
  # is switched on only after section 6 has put the whole rule set in.
  # IPv6 is switched off before anything faces the WAN, so it is never reachable from there.
  :do { /ipv6/settings set disable-ipv6=yes } on-error={ :put "   (no IPv6 on this router)" }
  :if ([:len [/interface/list/find where name="WAN"]] = 0) do={ /interface/list add name=WAN comment="ftn: WAN list" }
  :if ([:len [/interface/list/find where name="LAN"]] = 0) do={ /interface/list add name=LAN comment="ftn: LAN list" }
  /interface/list/member remove [find where list="WAN" and interface="ether1"]
  /interface/list/member add list=WAN interface=ether1 comment="ftn: WAN member"
  /ip/firewall/filter remove [find where comment="ftn: hold"]
  :local fst [/ip/firewall/filter/find]
  :if ([:len $fst] > 0) do={
    /ip/firewall/filter add chain=input action=drop connection-state=new,invalid in-interface-list=WAN place-before=[:pick $fst 0] comment="ftn: hold"
    /ip/firewall/filter add chain=forward action=drop connection-state=new,invalid in-interface-list=WAN place-before=[:pick $fst 0] comment="ftn: hold"
  } else={
    /ip/firewall/filter add chain=input action=drop connection-state=new,invalid in-interface-list=WAN comment="ftn: hold"
    /ip/firewall/filter add chain=forward action=drop connection-state=new,invalid in-interface-list=WAN comment="ftn: hold"
  }
  # Ports are off by default: every physical ethernet and SFP port except the uplink and the admin port is switched
  # off here, before the WAN exists, so a cable plugged into a spare port gets nothing. The OLT and customer ports
  # start off too; the owner enables them in the Ports tab. Nothing later in this script needs them to be up
  # (bridge ports, the customer VLAN and addresses are made on disabled ports without trouble, and the report goes
  # out over the WAN). If the script stops early they stay off.
  :local keepUp {"ether1";"ether2"}
  :foreach i in=[/interface/find where type=ether] do={
    :local n [/interface/get $i name]
    :if ([:typeof [:find $keepUp $n]] = "nil") do={ /interface/set $i disabled=yes }
  }
  /ip/dhcp-client remove [find where comment~"^ftn:"]
  /ip/dhcp-client remove [find where interface="ether1"]
  /interface/pppoe-client remove [find where comment~"^ftn:"]
  /ip/address remove [find where comment="ftn: WAN address"]
  /ip/route remove [find where comment="ftn: WAN default route"]
  /ip/dhcp-client add interface=ether1 use-peer-dns=no add-default-route=yes disabled=yes comment="ftn: WAN dhcp"
} do={ :set report ($report . "2 hold and WAN: " . $e . "\n"); :set abort ("ftn: section 2 hold and WAN failed: " . $e); :error $abort }

:put "ftn 3/12: admin LAN 192.168.88.0/24 on ether2"
:onerror e in={
  /ip/dhcp-server remove [find where name="ftn-lan-dhcp"]
  /ip/dhcp-server/network remove [find where comment="ftn: LAN DHCP network"]
  /ip/pool remove [find where name="ftn-lan-pool"]
  /ip/address remove [find where comment="ftn: LAN gateway"]
  /interface/bridge/port remove [find where comment="ftn: LAN port"]
  /interface/list/member remove [find where list="LAN" and interface="br-lan"]
  /interface/bridge add name=br-lan comment="ftn: admin LAN bridge"
  /interface/bridge/port remove [find where interface="ether2"]
  /interface/bridge/port add bridge=br-lan interface=ether2 comment="ftn: LAN port"
  /ip/address add address=192.168.88.1/24 interface=br-lan comment="ftn: LAN gateway"
  /ip/pool add name=ftn-lan-pool ranges=192.168.88.10-192.168.88.254 comment="ftn: LAN pool"
  /ip/dhcp-server add name=ftn-lan-dhcp interface=br-lan address-pool=ftn-lan-pool lease-time=1h disabled=no comment="ftn: LAN DHCP"
  /ip/dhcp-server/network add address=192.168.88.0/24 gateway=192.168.88.1 dns-server=192.168.88.1 comment="ftn: LAN DHCP network"
  /interface/list/member add list=LAN interface=br-lan comment="ftn: LAN member"
} do={ :set report ($report . "3 admin LAN: " . $e . "\n"); :set abort ("ftn: section 3 admin LAN failed: " . $e); :error $abort }

:put "ftn 4/12: customer network 10.5.50.0/24"
:onerror e in={
  /ip/dhcp-server remove [find where name="ftn-dhcp"]
  /ip/address remove [find where comment="ftn: customer gateway"]
  /interface/bridge/port remove [find where comment~"^ftn: (customer|bench) port"]
  /interface/vlan remove [find where name="vlan100-customers"]
  /interface/bridge/port remove [find where interface="ether3"]
  /interface/vlan add name=vlan100-customers vlan-id=100 interface=ether3 comment="ftn: customer VLAN"
  /interface/bridge add name=br-customers arp=reply-only comment="ftn: customer bridge"
  /interface/bridge/port add bridge=br-customers interface=vlan100-customers horizon=1 comment="ftn: customer port"
  /interface/bridge/port remove [find where interface="ether5"]
  /interface/bridge/port add bridge=br-customers interface=ether5 horizon=1 comment="ftn: customer port"
  /ip/address add address=10.5.50.1/24 interface=br-customers comment="ftn: customer gateway"
} do={ :set report ($report . "4 customer network: " . $e . "\n"); :set abort ("ftn: section 4 customer network failed: " . $e); :error $abort }

:put "ftn 5/12: DHCP for the customers (unregistered .10-.59, flats get fixed addresses from the server)"
:onerror e in={
  /ip/dhcp-server/network remove [find where comment="ftn: customer DHCP network"]
  /ip/pool remove [find where name="ftn-unreg"]
  /ip/firewall/address-list remove [find where list="ftn-unreg" and comment="ftn: unregistered range"]
  /ip/pool add name=ftn-unreg ranges=10.5.50.10-10.5.50.59 comment="ftn: unregistered pool"
  /ip/dhcp-server add name=ftn-dhcp interface=br-customers address-pool=ftn-unreg lease-time=1m authoritative=yes add-arp=yes disabled=no comment="ftn: IPoE DHCP"
  /ip/dhcp-server/network add address=10.5.50.0/24 gateway=10.5.50.1 dns-server=10.5.50.1 comment="ftn: customer DHCP network"
  /ip/firewall/address-list add list=ftn-unreg address=10.5.50.10-10.5.50.59 comment="ftn: unregistered range"
} do={ :set report ($report . "5 DHCP: " . $e . "\n"); :set abort ("ftn: section 5 DHCP failed: " . $e); :error $abort }

:put "ftn 6/12: firewall (nothing is opened on the WAN), then the WAN is switched on"
:onerror e in={
  # The hold of section 2 is already at the top of the filter. The full rule set goes in behind it; the WAN
  # client is switched on after the last rule is in; the hold is removed last of all. If anything fails on the
  # way the hold stays, so the router stays closed.
  # The tunnel interface exists before any rule names it (RouterOS refuses a rule for an unknown interface).
  # Its address, peer and route are added in section 9.
  /interface/wireguard remove [find where name="wg-ftn"]
  /interface/wireguard add name=wg-ftn private-key="PRIVATEKEYPRIVATEKEYPRIVATEKEYPRIVATEKEY0=" mtu=1380 comment="ftn: tunnel to the billing server"
  # The admin port reaches the router and the server's own names (tcp 80/443) only. RouterOS resolves the FQDN
  # entries itself and keeps them current. The WAN is not up yet, so an entry that cannot be resolved now is not an
  # error: the list is healed by the server's sweep, and the drop rule below holds either way.
  /ip/firewall/address-list remove [find where list="ftn-admin-allow"]
  :do { /ip/firewall/address-list add list=ftn-admin-allow address=203.0.113.10 comment="ftn: admin allow 203.0.113.10" } on-error={ :put "   (could not add 203.0.113.10 to the admin allow list now; the server adds it later)" }
  :do { /ip/firewall/address-list add list=ftn-admin-allow address=billing.example.co.ke comment="ftn: admin allow billing.example.co.ke" } on-error={ :put "   (could not add billing.example.co.ke to the admin allow list now; the server adds it later)" }
  /ip/firewall/filter remove [find where comment~"^ftn:" and comment!="ftn: hold"]
  /ip/firewall/nat remove [find where comment~"^ftn:"]
  /ip/firewall/filter add chain=forward action=accept in-interface=br-lan out-interface-list=WAN protocol=tcp dst-port=80,443 dst-address-list=ftn-admin-allow connection-state=new,established,related comment="ftn: admin allow billing site"
  /ip/firewall/filter add chain=forward action=drop in-interface=br-lan out-interface-list=WAN comment="ftn: admin no internet"
  /ip/firewall/filter add chain=forward action=drop src-address=192.168.88.0/24 out-interface-list=WAN comment="ftn: admin net no internet"
  /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-unreg comment="ftn: ipoe walled payment page ftn-unreg"
  /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-suspended comment="ftn: ipoe walled payment page ftn-suspended"
  /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-unreg comment="ftn: ipoe walled reject tcp ftn-unreg"
  /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-suspended comment="ftn: ipoe walled reject tcp ftn-suspended"
  /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-unreg comment="ftn: ipoe walled reject ftn-unreg"
  /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-suspended comment="ftn: ipoe walled reject ftn-suspended"
  /ip/firewall/filter add chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24 comment="ftn: ipoe customer est/rel out (above FastTrack, so queues apply)"
  /ip/firewall/filter add chain=forward action=accept connection-state=established,related dst-address=10.5.50.0/24 comment="ftn: ipoe customer est/rel in (above FastTrack, so queues apply)"
  /ip/firewall/filter add chain=forward action=drop in-interface=br-customers out-interface=br-customers comment="ftn: ipoe isolate flats"
  /ip/firewall/filter add chain=forward action=accept connection-state=established,related comment="ftn: forward est/rel"
  /ip/firewall/filter add chain=forward action=drop connection-state=invalid comment="ftn: forward drop invalid"
  /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn comment="ftn: wg no forward in"
  /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn comment="ftn: wg no forward out"
  /ip/firewall/filter add chain=forward action=accept in-interface-list=LAN out-interface=br-customers comment="ftn: lan to customers"
  /ip/firewall/filter add chain=forward action=accept in-interface=br-customers out-interface-list=WAN comment="ftn: customers to wan"
  /ip/firewall/filter add chain=forward action=drop comment="ftn: forward drop rest"
  /ip/firewall/filter add chain=input action=accept connection-state=established,related comment="ftn: input est/rel"
  /ip/firewall/filter add chain=input action=drop connection-state=invalid comment="ftn: input drop invalid"
  /ip/firewall/filter add chain=input action=drop protocol=udp dst-port=53 in-interface-list=WAN comment="ftn: no WAN DNS"
  /ip/firewall/filter add chain=input action=drop protocol=tcp dst-port=53 in-interface-list=WAN comment="ftn: no WAN DNS"
  /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8729 comment="ftn: wg rest"
  /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 comment="ftn: wg icmp"
  /ip/firewall/filter add chain=input action=accept protocol=tcp dst-port=8291 src-address=10.98.0.0/24 in-interface=wg-ftn comment="ftn: mgmt winbox"
  /ip/firewall/filter add chain=input action=accept protocol=icmp src-address=10.98.0.0/24 in-interface=wg-ftn comment="ftn: mgmt icmp"
  /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn comment="ftn: wg drop other input"
  /ip/firewall/filter add chain=input action=accept in-interface-list=LAN comment="ftn: lan input"
  /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=53 in-interface=br-customers comment="ftn: customer dns"
  /ip/firewall/filter add chain=input action=accept protocol=tcp dst-port=53 in-interface=br-customers comment="ftn: customer dns"
  /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 in-interface=br-customers comment="ftn: customer dhcp"
  /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=br-customers comment="ftn: customer icmp"
  /ip/firewall/filter add chain=input action=drop protocol=tcp in-interface=br-customers dst-port=8729 comment="ftn: ipoe block rest"
  /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 src-address-list=ftn-unreg comment="ftn: ipoe walled dhcp ftn-unreg"
  /ip/firewall/filter add chain=input action=accept protocol=icmp src-address-list=ftn-unreg comment="ftn: ipoe walled icmp ftn-unreg"
  /ip/firewall/filter add chain=input action=drop src-address-list=ftn-unreg comment="ftn: ipoe walled input drop ftn-unreg"
  /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 src-address-list=ftn-suspended comment="ftn: ipoe walled dhcp ftn-suspended"
  /ip/firewall/filter add chain=input action=accept protocol=icmp src-address-list=ftn-suspended comment="ftn: ipoe walled icmp ftn-suspended"
  /ip/firewall/filter add chain=input action=drop src-address-list=ftn-suspended comment="ftn: ipoe walled input drop ftn-suspended"
  /ip/firewall/filter add chain=input action=drop comment="ftn: input drop rest"
  /ip/firewall/nat add chain=srcnat action=masquerade out-interface-list=WAN src-address=10.5.50.0/24 comment="ftn: customer masquerade"
  /ip/firewall/nat add chain=srcnat action=masquerade out-interface-list=WAN src-address=192.168.88.0/24 comment="ftn: lan masquerade"
  /ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list=ftn-unreg to-ports=53 comment="ftn: ipoe walled dns redirect udp ftn-unreg"
  /ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list=ftn-unreg to-ports=53 comment="ftn: ipoe walled dns redirect tcp ftn-unreg"
  /ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list=ftn-suspended to-ports=53 comment="ftn: ipoe walled dns redirect udp ftn-suspended"
  /ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list=ftn-suspended to-ports=53 comment="ftn: ipoe walled dns redirect tcp ftn-suspended"
  /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-unreg to-addresses=203.0.113.10 to-ports=80 comment="ftn: ipoe walled http to the payment page ftn-unreg"
  /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-suspended to-addresses=203.0.113.10 to-ports=80 comment="ftn: ipoe walled http to the payment page ftn-suspended"

  /ip/dhcp-client enable [find where comment="ftn: WAN dhcp"]
  /ip/firewall/filter remove [find where comment="ftn: hold"]
} do={ :set report ($report . "6 firewall: " . $e . "\n"); :set abort ("ftn: section 6 firewall failed: " . $e); :error $abort }

:put "ftn 7/12: DNS and IPv6"
:onerror e in={
  /ip/dns set servers=1.1.1.1,8.8.8.8 allow-remote-requests=yes
  :do {
    /ipv6/firewall/filter remove [find where comment~"^ftn:"]
    /ipv6/firewall/filter add chain=forward action=drop in-interface=br-customers comment="ftn: ipv6 drop"
    # Belt and braces (IPv6 is switched off above): if the uplink ever hands out IPv6, nothing but the admin LAN reaches the router over it.
    /ipv6/firewall/filter add chain=input action=accept connection-state=established,related comment="ftn: ipv6 input est/rel"
    /ipv6/firewall/filter add chain=input action=accept protocol=icmpv6 comment="ftn: ipv6 input icmpv6"
    /ipv6/firewall/filter add chain=input action=drop in-interface-list=!LAN comment="ftn: ipv6 input drop non-LAN"
  } on-error={ :put "   (no IPv6 firewall on this router)" }
} do={ :set report ($report . "7 DNS and IPv6: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 7 DNS and IPv6 failed, continuing: " . $e) }

:put "ftn 8/12: services"
:onerror e in={
  /ip/service set telnet disabled=yes
  /ip/service set ftp disabled=yes
  /ip/service set api disabled=yes
  /ip/service set api-ssl disabled=yes
  # WebFig (www, and www-ssl if it is ever switched on) answers the admin LAN and the billing server's tunnel address only.
  /ip/service set www-ssl address=192.168.88.0/24,10.99.0.1/32 disabled=yes
  /ip/service set www port=8729 address=192.168.88.0/24,10.99.0.1/32 disabled=no
  /ip/service set ssh address=192.168.88.0/24 disabled=no
  # Remote WinBox: the LAN, plus the admin devices of the server's wg-mgmt interface (through the tunnel).
  # MAC-WinBox below stays LAN-only.
  /ip/service set winbox address=192.168.88.0/24,10.98.0.0/24 disabled=no
  :if ([:len [/interface/list/member/find where list="LAN"]] > 0) do={
    /tool/mac-server set allowed-interface-list=LAN
    /tool/mac-server/mac-winbox set allowed-interface-list=LAN
    /ip/neighbor/discovery-settings set discover-interface-list=LAN
  }
  /tool/bandwidth-server set enabled=no
} do={ :set report ($report . "8 services: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 8 services failed, continuing: " . $e) }

:put "ftn 9/12: WireGuard tunnel to the billing server"
:onerror e in={
  :if ([:len [/interface/wireguard/find where name="wg-ftn"]] = 0) do={ :error "the tunnel interface from section 6 is missing" }
  /ip/address remove [find where comment="ftn: tunnel address"]
  /ip/address add address=10.99.0.2 network=10.99.0.1 interface=wg-ftn comment="ftn: tunnel address"
  /interface/wireguard/peers remove [find where comment="ftn: billing server"]
  /interface/wireguard/peers add interface=wg-ftn public-key="SERVERPUBKEYSERVERPUBKEYSERVERPUBKEYSERV0=" endpoint-address=203.0.113.10 endpoint-port=51820 allowed-address=10.99.0.1/32,10.98.0.0/24 persistent-keepalive=25s comment="ftn: billing server"
  # Replies to the admin devices go back into the tunnel (the tunnel address only has a route to the server).
  /ip/route remove [find where comment="ftn: mgmt route"]
  /ip/route add dst-address=10.98.0.0/24 gateway=wg-ftn comment="ftn: mgmt route"
  # Tells the server (in the report, not a secret) that this router has remote WinBox for this subnet.
  :set mgmt "10.98.0.0/24"
} do={ :set report ($report . "9 WireGuard: " . $e . "\n"); :set abort ("ftn: section 9 WireGuard failed: " . $e); :error $abort }

:put "ftn 10/12: API user for the billing server"
:onerror e in={
  /user remove [find where name="ispbill"]
  /user remove [find where name="ispbill"]
  /user/group remove [find where name="ispbill"]
  /user/group remove [find where name="ispbill"]
  /user/group add name=ispbill policy=read,write,test,sensitive,api,rest-api,reboot comment="ftn: billing API"
  /user add name=ispbill group=ispbill password="RESTPASSWORD0123456789abcdefghij" address=10.99.0.1/32 comment="ftn: billing API"
} do={ :set report ($report . "10 REST user: " . $e . "\n"); :set abort ("ftn: section 10 REST user failed: " . $e); :error $abort }

:put "ftn 11/12: traffic reporting"
:onerror e in={
  # Traffic-flow targets take no comment on RouterOS 7.25; the server's tunnel address marks ours.
  /ip/traffic-flow/target remove [find where dst-address=10.99.0.1]
  /ip/traffic-flow set enabled=yes interfaces=br-customers
  /ip/traffic-flow/target add dst-address=10.99.0.1 port=2055 version=5
} do={ :set report ($report . "11 traffic-flow: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 11 traffic-flow failed, continuing: " . $e) }

:put "ftn 12/12: checking the internet and the billing server"
:local w 0
:while ([:len [/ip/route/find where dst-address="0.0.0.0/0" and active=yes]] = 0 && $w < 30) do={ :delay 1s; :set w ($w + 1) }
:local nt 0
:do { :while (([/system/ntp/client/get status] != "synchronized") && $nt < 30) do={ :delay 1s; :set nt ($nt + 1) } } on-error={}
:if ([:len [/ip/route/find where dst-address="0.0.0.0/0" and active=yes]] = 0) do={ :set report ($report . "12 internet: the router has no default route (check the WAN settings)\n"); :set fails ($fails + 1); :put "   no default route yet" }

# A WAN address that overlaps the admin LAN or the customer network would break routing. Report it (the router is
# left as it is). BENCH-CHECK: "in" with an ip-prefix on 7.25.1; if it is not understood the check is skipped.
:do {
  :local wi [/ip/address/find where dynamic=yes and interface="ether1"]
  :if ([:len $wi] > 0) do={
    :local wa [/ip/address/get [:pick $wi 0] address]
    :local wn [/ip/address/get [:pick $wi 0] network]
    :if (($wn in 192.168.88.0/24) || (192.168.88.1 in $wa) || ($wn in 10.5.50.0/24) || (10.5.50.1 in $wa)) do={
      :set report ($report . "12 WAN address overlap: the WAN address " . $wa . " overlaps the admin LAN 192.168.88.0/24 or the customer network 10.5.50.0/24. Change that subnet in the billing admin and rebuild the router\n")
      :set fails ($fails + 1)
      :put "   the WAN address overlaps a network of this router"
    }
  }
} on-error={ :put "   (could not check the WAN address against the router networks)" }
:local pr 0
:do { :set pr [/ping 10.99.0.1 count=3] } on-error={}
:if ($pr = 0) do={ :put "   no answer from the billing server yet; the tunnel can take a few seconds to come up" } else={ :put ("   billing server: " . $pr . " of 3 replies") }
:log info "ftn: bootstrap finished"
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " section(s) failed, see the messages above") } else={ :put "ftn: all sections done"; :set result "ok" }
:if ([:len $report] > 0) do={ :put "ftn: problems:"; :put $report }
:if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="https://billing.example.co.ke/j/TOKEN0123456789TOKEN0123456789TOKEN01234/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . " mgmt " . $mgmt . "\n" . $report) output=none check-certificate=yes } on-error={ :put "   could not send the join report to the server" }
} on-error={
  :put "ftn: the bootstrap script stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  # The hold is still in the filter (it is removed only at the very end of section 6). Switch the WAN on behind it so
  # the failure report can reach the server: the hold lets the router's own traffic and its answers through.
  :do {
  /ip/dhcp-client enable [find where comment="ftn: WAN dhcp"]
  } on-error={ :put "   could not switch the WAN on" }
  :local hw 0
  :while ([:len [/ip/route/find where dst-address="0.0.0.0/0" and active=yes]] = 0 && $hw < 30) do={ :delay 1s; :set hw ($hw + 1) }
  # A reset router's clock can be weeks off, and HTTPS fails until NTP corrects it.
  :local nw 0
  :do { :while (([/system/ntp/client/get status] != "synchronized") && $nw < 30) do={ :delay 1s; :set nw ($nw + 1) } } on-error={}
  :if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="https://billing.example.co.ke/j/TOKEN0123456789TOKEN0123456789TOKEN01234/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . " mgmt " . $mgmt . "\n" . $report) output=none check-certificate=yes } on-error={ :put "   could not send the join report to the server" }
  /file remove [find where name="ftn-setup.rsc"]
  /file remove [find where name="ftn-reset.rsc"]
  :error "ftn: bootstrap stopped; the script files were removed from the router. Fix the cause, then make a new line"
}
/file remove [find where name="ftn-setup.rsc"]
/file remove [find where name="ftn-reset.rsc"]
