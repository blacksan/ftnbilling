# Fast Tech Net: bootstrap script for Block A (s1), generated 2026-11-18 09:00 UTC
# Builds this router from nothing: WAN, admin LAN, customer network, DHCP,
# firewall, services, the WireGuard tunnel to the billing server and the API
# user. Single use. It holds this router's WireGuard private key and API
# password, so keep it private. It removes itself from the router when it
# finishes, and also when it stops on an error (the whole body is wrapped).
# Every object it creates has a comment that starts with "ftn:"; running it
# again replaces them. It never prints a secret.
:local fails 0
:local abort ""
:local report ""
:local ver ""
:local result "fail"
:do {

:put "ftn 0/12: waiting for the ports, checking the router"
:onerror e in={
  :set ver [/system/resource/get version]
  :put ("   RouterOS " . $ver)
  :local major [:tonum [:pick $ver 0 [:find $ver "."]]]
  :if ($major < 7) do={ :error "RouterOS 7 or newer is required" }
  :local need {"ether1";"sfp-sfpplus1";"ether2"}
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
  # Foreign settings: this script may only replace what it made itself. (After the reset of the paste line the
  # router is empty and nothing is found.)
  :local foreign ""
  :foreach i in=[/ip/address/find where dynamic=no] do={ :if (!([/ip/address/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " address " . [/ip/address/get $i address] . ";") } }
  :foreach i in=[/interface/bridge/find] do={ :if (!([/interface/bridge/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " bridge " . [/interface/bridge/get $i name] . ";") } }
  :foreach i in=[/ip/dhcp-server/find] do={ :if (!([/ip/dhcp-server/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " dhcp-server " . [/ip/dhcp-server/get $i name] . ";") } }
  :foreach i in=[/ip/firewall/filter/find where dynamic=no] do={ :if (!([/ip/firewall/filter/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " firewall rule;") } }
  :foreach i in=[/ip/firewall/nat/find where dynamic=no] do={ :if (!([/ip/firewall/nat/get $i comment] ~ "^ftn:")) do={ :set foreign ($foreign . " nat rule;") } }
  :if ([:len $foreign] > 0) do={ :error ("this router already has settings that are not ours:" . $foreign . " Nothing was changed. Use the paste line from the billing admin (it erases the router first)") }
} do={ :set report ($report . "0 wait and guard: " . $e . "\n"); :set abort ("ftn: section 0 wait and guard failed: " . $e); :error $abort }

:put "ftn 1/12: name, clock, trust anchors"
:onerror e in={
  /system/identity set name="FTN-Block-A"
  /system/clock set time-zone-name=Africa/Nairobi
  /system/ntp/client set enabled=yes servers=162.159.200.1,162.159.200.123
  :do { /certificate/settings set builtin-trust-anchors=trusted } on-error={ :put "   (this RouterOS has no built-in trust anchors setting)" }
} do={ :set report ($report . "1 system: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 1 system failed, continuing: " . $e) }

:put "ftn 2/12: WAN ether1 (dhcp)"
:onerror e in={
  :if ([:len [/interface/list/find where name="WAN"]] = 0) do={ /interface/list add name=WAN comment="ftn: WAN list" }
  :if ([:len [/interface/list/find where name="LAN"]] = 0) do={ /interface/list add name=LAN comment="ftn: LAN list" }
  /ip/dhcp-client remove [find where comment~"^ftn:"]
  /ip/dhcp-client remove [find where interface="ether1"]
  /interface/pppoe-client remove [find where comment~"^ftn:"]
  /ip/address remove [find where comment="ftn: WAN address"]
  /ip/route remove [find where comment="ftn: WAN default route"]
  /interface/list/member remove [find where list="WAN" and interface="ether1"]
  /ip/dhcp-client add interface=ether1 use-peer-dns=no add-default-route=yes disabled=no comment="ftn: WAN dhcp"
  /interface/list/member add list=WAN interface=ether1 comment="ftn: WAN member"
} do={ :set report ($report . "2 lists and WAN: " . $e . "\n"); :set abort ("ftn: section 2 lists and WAN failed: " . $e); :error $abort }

:put "ftn 3/12: admin LAN 192.168.88.0/24 on ether2"
:onerror e in={
  /ip/dhcp-server remove [find where name="ftn-lan-dhcp"]
  /ip/dhcp-server/network remove [find where comment="ftn: LAN DHCP network"]
  /ip/pool remove [find where name="ftn-lan-pool"]
  /ip/address remove [find where comment="ftn: LAN gateway"]
  /interface/bridge/port remove [find where comment="ftn: LAN port"]
  /interface/list/member remove [find where list="LAN" and interface="br-lan"]
  /interface/bridge remove [find where name="br-lan"]
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
  /interface/bridge remove [find where name="br-customers"]
  /interface/bridge/port remove [find where interface="sfp-sfpplus1"]
  /interface/bridge add name=br-customers arp=reply-only comment="ftn: customer bridge"
  /interface/bridge/port add bridge=br-customers interface=sfp-sfpplus1 horizon=1 comment="ftn: customer port"
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

:put "ftn 6/12: firewall (nothing is opened on the WAN)"
:onerror e in={
  # The router is never unfiltered: a hold rule that drops new and invalid
  # input from the WAN goes to the top first, the old rules are replaced
  # behind it, and the hold is removed only after every new rule is in. If a
  # rule fails the hold stays, so the router stays closed.
  /ip/firewall/filter remove [find where comment="ftn: hold"]
  :if ([:len [/ip/firewall/filter/find]] > 0) do={
    /ip/firewall/filter add chain=input action=drop connection-state=new,invalid in-interface-list=WAN place-before=0 comment="ftn: hold"
  } else={
    /ip/firewall/filter add chain=input action=drop connection-state=new,invalid in-interface-list=WAN comment="ftn: hold"
  }
  /ip/firewall/filter remove [find where comment~"^ftn:" and comment!="ftn: hold"]
  /ip/firewall/nat remove [find where comment~"^ftn:"]
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
  /ip/firewall/filter add chain=forward action=accept in-interface-list=LAN out-interface-list=WAN comment="ftn: lan to wan"
  /ip/firewall/filter add chain=forward action=accept in-interface-list=LAN out-interface=br-customers comment="ftn: lan to customers"
  /ip/firewall/filter add chain=forward action=accept in-interface=br-customers out-interface-list=WAN comment="ftn: customers to wan"
  /ip/firewall/filter add chain=forward action=drop comment="ftn: forward drop rest"
  /ip/firewall/filter add chain=input action=accept connection-state=established,related comment="ftn: input est/rel"
  /ip/firewall/filter add chain=input action=drop connection-state=invalid comment="ftn: input drop invalid"
  /ip/firewall/filter add chain=input action=drop protocol=udp dst-port=53 in-interface-list=WAN comment="ftn: no WAN DNS"
  /ip/firewall/filter add chain=input action=drop protocol=tcp dst-port=53 in-interface-list=WAN comment="ftn: no WAN DNS"
  /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8729 comment="ftn: wg rest"
  /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 comment="ftn: wg icmp"
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
  /ip/firewall/filter remove [find where comment="ftn: hold"]
} do={ :set report ($report . "6 firewall: " . $e . "\n"); :set abort ("ftn: section 6 firewall failed: " . $e); :error $abort }

:put "ftn 7/12: DNS and IPv6"
:onerror e in={
  /ip/dns set servers=1.1.1.1,8.8.8.8 allow-remote-requests=yes
  /ipv6/settings set disable-ipv6=yes
  :do {
    /ipv6/firewall/filter remove [find where comment~"^ftn:"]
    /ipv6/firewall/filter add chain=forward action=drop in-interface=br-customers comment="ftn: ipv6 drop"
  } on-error={ :put "   (no IPv6 firewall on this router)" }
} do={ :set report ($report . "7 DNS and IPv6: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 7 DNS and IPv6 failed, continuing: " . $e) }

:put "ftn 8/12: services"
:onerror e in={
  /ip/service set telnet disabled=yes
  /ip/service set ftp disabled=yes
  /ip/service set api disabled=yes
  /ip/service set api-ssl disabled=yes
  /ip/service set www-ssl disabled=yes
  /ip/service set www port=8729 address=10.99.0.1/32 disabled=no
  /ip/service set ssh address=192.168.88.0/24 disabled=no
  /ip/service set winbox address=192.168.88.0/24 disabled=no
  :if ([:len [/interface/list/member/find where list="LAN"]] > 0) do={
    /tool/mac-server set allowed-interface-list=LAN
    /tool/mac-server/mac-winbox set allowed-interface-list=LAN
    /ip/neighbor/discovery-settings set discover-interface-list=LAN
  }
  /tool/bandwidth-server set enabled=no
} do={ :set report ($report . "8 services: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 8 services failed, continuing: " . $e) }

:put "ftn 9/12: WireGuard tunnel to the billing server"
:onerror e in={
  /interface/wireguard remove [find where name="wg-ftn"]
  /interface/wireguard add name=wg-ftn private-key="PRIVATEKEYPRIVATEKEYPRIVATEKEYPRIVATEKEY0=" mtu=1380 comment="ftn: tunnel to the billing server"
  /ip/address remove [find where comment="ftn: tunnel address"]
  /ip/address add address=10.99.0.2 network=10.99.0.1 interface=wg-ftn comment="ftn: tunnel address"
  /interface/wireguard/peers remove [find where comment="ftn: billing server"]
  /interface/wireguard/peers add interface=wg-ftn public-key="SERVERPUBKEYSERVERPUBKEYSERVERPUBKEYSERV0=" endpoint-address=203.0.113.10 endpoint-port=51820 allowed-address=10.99.0.1/32 persistent-keepalive=25s comment="ftn: billing server"
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
  /ip/traffic-flow/target remove [find where comment="ftn: netflow"]
  /ip/traffic-flow set enabled=yes interfaces=br-customers
  /ip/traffic-flow/target add dst-address=10.99.0.1 port=2055 version=5 comment="ftn: netflow"
} do={ :set report ($report . "11 traffic-flow: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 11 traffic-flow failed, continuing: " . $e) }

:put "ftn 12/12: checking the internet and the billing server"
:local w 0
:while ([:len [/ip/route/find where dst-address="0.0.0.0/0" and active=yes]] = 0 && $w < 30) do={ :delay 1s; :set w ($w + 1) }
:if ([:len [/ip/route/find where dst-address="0.0.0.0/0" and active=yes]] = 0) do={ :set report ($report . "12 internet: the router has no default route (check the WAN settings)\n"); :set fails ($fails + 1); :put "   no default route yet" }
:local pr 0
:do { :set pr [/ping 10.99.0.1 count=3] } on-error={}
:if ($pr = 0) do={ :put "   no answer from the billing server yet; the tunnel can take a few seconds to come up" } else={ :put ("   billing server: " . $pr . " of 3 replies") }
:log info "ftn: bootstrap finished"
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " section(s) failed, see the messages above") } else={ :put "ftn: all sections done"; :set result "ok" }
:if ([:len $report] > 0) do={ :put "ftn: problems:"; :put $report }
:if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="https://billing.example.co.ke/j/TOKEN0123456789TOKEN0123456789TOKEN01234/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . "\n" . $report) output=none check-certificate=yes } on-error={ :put "   could not send the join report to the server" }
} on-error={
  :put "ftn: the bootstrap script stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  :if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="https://billing.example.co.ke/j/TOKEN0123456789TOKEN0123456789TOKEN01234/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . "\n" . $report) output=none check-certificate=yes } on-error={ :put "   could not send the join report to the server" }
  /file remove [find where name="ftn-setup.rsc"]
  /file remove [find where name="ftn-reset.rsc"]
  :error "ftn: bootstrap stopped; the script files were removed from the router. Fix the cause, then make a new line"
}
/file remove [find where name="ftn-setup.rsc"]
/file remove [find where name="ftn-reset.rsc"]
