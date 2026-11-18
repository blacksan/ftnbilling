# Fast Tech Net: IPoE cutover (stage: cutover) for Block A (sab12cd), generated 2026-11-18 09:30 UTC
# Switches the flats from the hotspot to server-managed IPoE: one static lease and
# one queue per flat, and a walled garden for unregistered and unpaid addresses.
# Nothing is deleted: the hotspot server and the old DHCP server are only disabled.
# It never touches the WAN interface, the routes, the router's users, or any
# service other than www (which the join stage handled).
# If "Confirm cutover" is not pressed in the billing admin within 15 minutes, the
# router switches back to the hotspot by itself (script ftn-rollback). To go back
# at once: /system/script/run ftn-rollback
# Every object it creates has a comment that starts with "ftn:".
:local fails 0
:local abort ""
:local switched false
:do {

:put "ftn cutover 0/7: checking RouterOS"
:local ver [/system/resource/get version]
:put ("   RouterOS " . $ver)
:local major [:tonum [:pick $ver 0 [:find $ver "."]]]
:if ($major < 7) do={ :set abort "ftn: RouterOS 7 or newer is required"; :error $abort }

:put "ftn cutover 1/7: backup and export (ftn-pre-ipoe.backup, ftn-pre-ipoe.rsc)"
/system/backup/save name=ftn-pre-ipoe
/export file=ftn-pre-ipoe

:put "ftn cutover 2/7: safety net: back to the hotspot after 15 minutes unless confirmed"
:do {
  /system/script remove [find where name="ftn-rollback"]
  /system/script remove [find where name="ftn-rollback-tick"]
  /system/script add name=ftn-rollback policy=read,write,policy,test source=":log warning \"ftn: IPoE rollback running\"\n:do { /ip dhcp-server disable [find where name=\"ftn-dhcp\"] } on-error={}\n:do { /ip hotspot enable [find where name=\"hotspot1\"] } on-error={}\n:do { /ip firewall filter remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ip firewall nat remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /system scheduler remove [find where name=\"ftn-rollback-timer\"] } on-error={}\n" comment="ftn: IPoE rollback"
  /system/script add name=ftn-rollback-tick policy=read,write,policy,test source=":local id [/system scheduler find where name=\"ftn-rollback-timer\"]\n:if ([:len \$id] = 0) do={ :error \"no timer\" }\n:local c [/system scheduler get \$id comment]\n:local n [:tonum [:pick \$c 20 [:len \$c]]]\n:set n (\$n + 1)\n:if (\$n >= 15) do={ /system script run ftn-rollback } else={ /system scheduler set \$id comment=(\"ftn: rollback timer \" . \$n) }\n" comment="ftn: IPoE rollback timer tick"
  /system/scheduler remove [find where name="ftn-rollback-timer"]
  /system/scheduler add name=ftn-rollback-timer interval=1m on-event=ftn-rollback-tick policy=read,write,policy,test comment="ftn: rollback timer 0"
} on-error={ :set abort "ftn: section 2 (rollback timer) failed, nothing was changed"; :error $abort }

:put "ftn cutover 3/7: finding the customer interface"
:local hs [/ip/hotspot/find where name="hotspot1"]
:if ([:len $hs] = 0) do={ :set hs [/ip/hotspot/find] }
:if ([:len $hs] = 0) do={ :set abort "ftn: no hotspot server found on this router, so the interface the flats are on is unknown"; :error $abort }
:local hsname [/ip/hotspot/get ($hs->0) name]
:local ifc [/ip/hotspot/get ($hs->0) interface]
:put ("   hotspot " . $hsname . " on interface " . $ifc)
:if ([:len [/ip/address/find where interface=$ifc and address="10.5.50.1/24"]] = 0) do={ :set abort ("ftn: interface " . $ifc . " has no address 10.5.50.1/24; check the building's customer subnet on the server"); :error $abort }
:local olddhcp [/ip/dhcp-server/find where interface=$ifc and disabled=no and name!="ftn-dhcp"]
:local rb [/system/script/get ftn-rollback source]
:set rb ($rb . ":do { /ip hotspot enable [find where name=\"" . $hsname . "\"] } on-error={}\n")
:foreach i in=$olddhcp do={ :set rb ($rb . ":do { /ip dhcp-server enable [find where name=\"" . [/ip/dhcp-server/get $i name] . "\"] } on-error={}\n") }
/system/script set ftn-rollback source=$rb

:put "ftn cutover 4/7: unregistered pool, DHCP server, 2 flat(s)"
:do {
  :if ([:len [/ip/pool/find where name="ftn-unreg"]] = 0) do={
    /ip/pool add name=ftn-unreg ranges=10.5.50.10-10.5.50.59 comment="ftn: unregistered pool"
  } else={
    /ip/pool set [find where name="ftn-unreg"] ranges=10.5.50.10-10.5.50.59
  }
  :if ([:len [/ip/dhcp-server/find where name="ftn-dhcp"]] = 0) do={
    /ip/dhcp-server add name=ftn-dhcp interface=$ifc address-pool=ftn-unreg lease-time=10m authoritative=yes disabled=yes comment="ftn: IPoE DHCP"
  } else={
    /ip/dhcp-server set [find where name="ftn-dhcp"] interface=$ifc address-pool=ftn-unreg lease-time=10m authoritative=yes
  }
  :if ([:len [/ip/dhcp-server/network/find where address="10.5.50.0/24"]] = 0) do={
    /ip/dhcp-server/network add address=10.5.50.0/24 gateway=10.5.50.1 dns-server=10.5.50.1 comment="ftn: IPoE DHCP network"
  }
  /ip/dhcp-server/lease remove [find where comment~"^ftn:acct:"]
  /queue/simple remove [find where comment~"^ftn:acct:"]
  /ip/firewall/address-list remove [find where list="ftn-suspended" and comment~"^ftn:acct:"]
  /ip/firewall/address-list remove [find where list="ftn-unreg" and comment="ftn: unregistered range"]
  /ip/firewall/address-list add list=ftn-unreg address=10.5.50.10-10.5.50.59 comment="ftn: unregistered range"
  :do { /ip/dhcp-server/lease add address=10.5.50.60 mac-address=AA:BB:CC:DD:EE:01 server=ftn-dhcp comment="ftn:acct:K7Q2MX Flat 3B" } on-error={ :set fails ($fails + 1); :put "   lease for Flat 3B failed" }
  :do { /queue/simple add name=ftn-K7Q2MX target=10.5.50.60/32 max-limit=20M/20M comment="ftn:acct:K7Q2MX Flat 3B" } on-error={ :set fails ($fails + 1); :put "   queue for Flat 3B failed" }
  :do { /ip/dhcp-server/lease add address=10.5.50.61 mac-address=00:E0:FC:12:34:56 server=ftn-dhcp comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   lease for Flat \"10A\" \$x failed" }
  :do { /queue/simple add name=ftn-P4R8TN target=10.5.50.61/32 max-limit=10M/10M comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   queue for Flat \"10A\" \$x failed" }
  :do { /ip/firewall/address-list add list=ftn-suspended address=10.5.50.61 disabled=yes comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   suspended entry for Flat \"10A\" \$x failed" }
} on-error={ :set abort "ftn: section 4 (DHCP server, leases, queues) failed"; :error $abort }

:put "ftn cutover 5/7: walled garden (DNS and the payment page only) and queue-friendly firewall"
:do {
  /ip/firewall/filter remove [find where comment~"^ftn: ipoe"]
  /ip/firewall/nat remove [find where comment~"^ftn: ipoe"]
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe dns tcp" } else={ /ip/firewall/filter add chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc comment="ftn: ipoe dns tcp" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe dns udp" } else={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc comment="ftn: ipoe dns udp" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related dst-address=10.5.50.0/24 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe customer est/rel in (above FastTrack, so queues apply)" } else={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related dst-address=10.5.50.0/24 comment="ftn: ipoe customer est/rel in (above FastTrack, so queues apply)" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe customer est/rel out (above FastTrack, so queues apply)" } else={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24 comment="ftn: ipoe customer est/rel out (above FastTrack, so queues apply)" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-suspended comment="ftn: ipoe walled reject ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-unreg comment="ftn: ipoe walled reject ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject tcp ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-suspended comment="ftn: ipoe walled reject tcp ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject tcp ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-unreg comment="ftn: ipoe walled reject tcp ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled payment page ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-suspended comment="ftn: ipoe walled payment page ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled payment page ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-unreg comment="ftn: ipoe walled payment page ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=53 src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled dns tcp ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=53 src-address-list=ftn-suspended comment="ftn: ipoe walled dns tcp ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=53 src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled dns tcp ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=53 src-address-list=ftn-unreg comment="ftn: ipoe walled dns tcp ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=udp dst-port=53 src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled dns udp ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=accept protocol=udp dst-port=53 src-address-list=ftn-suspended comment="ftn: ipoe walled dns udp ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=udp dst-port=53 src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled dns udp ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=accept protocol=udp dst-port=53 src-address-list=ftn-unreg comment="ftn: ipoe walled dns udp ftn-unreg" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-suspended to-addresses=203.0.113.10 to-ports=80 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled http to the payment page ftn-suspended" } else={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-suspended to-addresses=203.0.113.10 to-ports=80 comment="ftn: ipoe walled http to the payment page ftn-suspended" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-unreg to-addresses=203.0.113.10 to-ports=80 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled http to the payment page ftn-unreg" } else={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-unreg to-addresses=203.0.113.10 to-ports=80 comment="ftn: ipoe walled http to the payment page ftn-unreg" }
  :if ([:len [/ip/firewall/nat/find where comment="ftn: ipoe customer masquerade"]] = 0) do={
    /ip/firewall/nat add chain=srcnat action=masquerade src-address=10.5.50.0/24 out-interface-list=WAN comment="ftn: ipoe customer masquerade"
  }
} on-error={ :set abort "ftn: section 5 (firewall) failed"; :error $abort }

:put "ftn cutover 6/7: switching the flats over"
:do {
  :foreach i in=$hs do={ /ip/hotspot disable $i }
  :foreach i in=$olddhcp do={ /ip/dhcp-server disable $i }
  /ip/dhcp-server enable [find where name="ftn-dhcp"]
  /ip/firewall/address-list enable [find where list="ftn-suspended" and comment~"^ftn:acct:"]
  :set switched true
} on-error={ :set abort "ftn: section 6 (switch) failed; the safety net will switch back"; :error $abort }

:put "ftn cutover 7/7: checking the internet and the billing server"
:do { /ping 1.1.1.1 count=3 } on-error={ :put "   WARNING: no answer from 1.1.1.1" }
:do { /ping 10.99.0.1 count=3 } on-error={ :put "   WARNING: no answer from the billing server tunnel address 10.99.0.1" }
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " problem(s), see the messages above") }
:put "ftn: the flats are now on IPoE. Check one flat, then press Confirm cutover in the billing admin within 15 minutes."
:put "ftn: otherwise the router goes back to the hotspot by itself."
} on-error={
  :put "ftn: the cutover stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  :if ($switched) do={
    :put "   the flats were switched: the safety net returns to the hotspot, or run /system/script/run ftn-rollback now"
  } else={
    :do { /system/scheduler remove [find where name="ftn-rollback-timer"] } on-error={}
    :put "   nothing was switched: the hotspot is still serving the flats"
  }
  /file remove [find where name="ftn-cutover.rsc"]
  :error "ftn: cutover stopped; the script file was removed from the router"
}
/file remove [find where name="ftn-cutover.rsc"]
