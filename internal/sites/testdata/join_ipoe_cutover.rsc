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
# Running it again is safe: the rollback script and the record of the old servers are kept as the first run made them.
:local fails 0
:local abort ""
:local report ""
:local armed false
:local switched false
:local force false
:local rbExists false
:local confirmed false
:do {

:put "ftn cutover 1/7: checking the router (nothing is changed yet)"
:local ver [/system/resource/get version]
:put ("   RouterOS " . $ver)
:local major [:tonum [:pick $ver 0 [:find $ver "."]]]
:if ($major < 7) do={ :set abort "ftn: RouterOS 7 or newer is required"; :error $abort }
# The rollback timer needs the scheduler. With device-mode blocking it, fail now, before anything is changed.
# BENCH-CHECK: [/system/device-mode/get scheduler] returns a bool or "yes"/"no"; both are accepted.
:if (!([:tostr [/system/device-mode/get scheduler]] ~ "^(true|yes)\$")) do={ :set abort "ftn: device-mode does not allow the scheduler, so the rollback timer cannot be made. Nothing was changed. On the router run: /system/device-mode/update scheduler=yes (press the reset button within 5 minutes when asked; the router reboots), then run this cutover again"; :error $abort }
:if ([/ip/dns/get allow-remote-requests] = false) do={ :set abort "ftn: the router's DNS does not answer the flats (/ip/dns allow-remote-requests is off). Turn it on first, because the DHCP server hands out the router as the resolver"; :error $abort }
:set rbExists ([:len [/system/script/find where name="ftn-rollback"]] > 0)
:local hs0 [/ip/hotspot/find where name="hotspot1"]
:if ([:len $hs0] = 0) do={ :set hs0 [/ip/hotspot/find] }
:if ([:len $hs0] = 0) do={ :set abort "ftn: no hotspot server found on this router, so the interface the flats are on is unknown"; :error $abort }
:local hsname [/ip/hotspot/get ($hs0->0) name]
:local ifc [/ip/hotspot/get ($hs0->0) interface]
:put ("   hotspot " . $hsname . " on interface " . $ifc)
:if ([:len [/ip/address/find where interface=$ifc and address="10.5.50.1/24"]] = 0) do={ :set abort ("ftn: interface " . $ifc . " has no address 10.5.50.1/24; check the building's customer subnet on the server"); :error $abort }
# The servers on the customer interface. The first run takes the enabled ones (the ones this script switches
# off); a later run takes them in any state and records nothing new.
:local hs
:local olddhcp
:if ($rbExists) do={
  :set hs [/ip/hotspot/find where interface=$ifc]
  :set olddhcp [/ip/dhcp-server/find where interface=$ifc and name!="ftn-dhcp"]
} else={
  :set hs [/ip/hotspot/find where interface=$ifc and disabled=no]
  :set olddhcp [/ip/dhcp-server/find where interface=$ifc and disabled=no and name!="ftn-dhcp"]
}
:set confirmed ([:len [/ip/dhcp-server/find where name="ftn-dhcp" and disabled=no]] > 0 && [:len [/system/scheduler/find where name="ftn-rollback-timer"]] = 0)
:if ($confirmed) do={ :put "   ftn-dhcp is already serving and the cutover was confirmed: no timer is armed and nothing is rolled back on an error" }
:local v6 false
:do { :if ([/ipv6/settings/get disable-ipv6] = false) do={ :set v6 true } } on-error={}
:if ($v6) do={ :put "   IPv6 is on: the flats' IPv6 forwarding is blocked, because IPoE billing sees IPv4 only" }
:foreach q in=[/queue/simple/find where dynamic=no] do={
  :if ([:pick [/queue/simple/get $q name] 0 4] != "ftn-") do={
    :if ([:tostr [/queue/simple/get $q target]] ~ "0.0.0.0/0") do={ :put ("   WARNING: queue " . [/queue/simple/get $q name] . " targets 0.0.0.0/0 and may cap or hide the flats' own queues") }
  }
}
# Devices that are not flats (an OLT's management address, an access point on a fixed address) have an ARP entry
# but no lease. With the interface on reply-only they would stop working, so they are listed, and a forced run keeps them.
:local known "AA:BB:CC:DD:EE:01,00:E0:FC:12:34:56"
:local keepArp ({})
:local keepLease ({})
:foreach a in=[/ip/arp/find where interface=$ifc] do={
  :local m [/ip/arp/get $a mac-address]
  :if ([:typeof $m] = "str" && [:len $m] > 0 && [:typeof [:find $known $m]] = "nil" && [:len [/ip/dhcp-server/lease/find where mac-address=$m]] = 0 && [:pick [/ip/arp/get $a comment] 0 4] != "ftn:") do={
    :set keepArp ($keepArp, ($m . "|" . [/ip/arp/get $a address]))
  }
}
:foreach i in=$olddhcp do={
  :local sn [/ip/dhcp-server/get $i name]
  :foreach l in=[/ip/dhcp-server/lease/find where server=$sn and dynamic=no] do={
    :local m [/ip/dhcp-server/lease/get $l mac-address]
    :if ([:typeof [:find $known $m]] = "nil" && [:pick [/ip/dhcp-server/lease/get $l comment] 0 4] != "ftn:") do={ :set keepLease ($keepLease, ($m . "|" . [/ip/dhcp-server/lease/get $l address])) }
  }
}
:if ([:len $keepArp] + [:len $keepLease] > 0) do={
  :foreach e in=$keepArp do={ :put ("   not a flat: device " . $e) }
  :foreach e in=$keepLease do={ :put ("   not a flat: static lease " . $e) }
  :if (!$force) do={ :set abort "ftn: the devices above are not flats and would lose their connection (the flats' interface goes to reply-only ARP). Nothing was changed. Check them, or make the cutover command with force from the billing admin to keep them as static entries"; :error $abort }
  :put "   forced: they are copied as static entries (comment ftn: keep)"
}

:put "ftn cutover 2/7: backup and export (kept if they already exist)"
:if ([:len [/file/find where name="ftn-pre-ipoe.backup"]] = 0) do={ /system/backup/save name=ftn-pre-ipoe } else={ :put "   ftn-pre-ipoe.backup already exists, kept" }
:if ([:len [/file/find where name="ftn-pre-ipoe.rsc"]] = 0) do={ /export file=ftn-pre-ipoe } else={ :put "   ftn-pre-ipoe.rsc already exists, kept" }

:put "ftn cutover 3/7: safety net: back to the hotspot after 15 minutes unless confirmed"
:onerror e in={
  :if (!$confirmed) do={
    :if (!$rbExists) do={
      /system/script add name=ftn-rollback policy=read,write,policy,test source=":log warning \"ftn: IPoE rollback running\"\n:do { /ip dhcp-server disable [find where name=\"ftn-dhcp\"] } on-error={}\n:do { /ip firewall address-list disable [find where list~\"^ftn-(unreg|suspended)\\\$\"] } on-error={}\n:do { /ip hotspot enable [find where name=\"hotspot1\"] } on-error={}\n:do { /ip firewall filter remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ip firewall nat remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ipv6 firewall filter remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ip arp remove [find where comment~\"^ftn:acct:\"] } on-error={}\n:do { /ip arp remove [find where comment=\"ftn: keep\"] } on-error={}\n:do { /ip dhcp-server lease remove [find where comment=\"ftn: keep\"] } on-error={}\n:do { /queue simple remove [find where comment~\"^ftn:acct:\"] } on-error={}\n:do { /system scheduler remove [find where name=\"ftn-rollback-timer\"] } on-error={}\n" comment="ftn: IPoE rollback"
    }
    # What the switch will change is written into the rollback script BEFORE it is changed, on every run:
    # a first run that stopped after creating the script (rbExists is then true) must not leave the lines out.
    # Each line is added only if it is not in the script yet.
    # Names read from the router are spliced into a script that runs with the policy policy: refuse any
    # that is not a plain name (a user with only write access can rename objects).
    :local okname "^[A-Za-z0-9][A-Za-z0-9._-]*\$"
    :if (!($ifc ~ $okname)) do={ :error ("ftn: interface name is not a plain name: " . $ifc) }
    :foreach h in=$hs do={ :if (!([/ip/hotspot/get $h name] ~ $okname)) do={ :error "ftn: a hotspot server name is not a plain name" } }
    :foreach i in=$olddhcp do={ :if (!([/ip/dhcp-server/get $i name] ~ $okname)) do={ :error "ftn: a DHCP server name is not a plain name" } }
    :local rb [/system/script/get [find where name="ftn-rollback"] source]
    :local lines ({})
    :foreach h in=$hs do={ :set lines ($lines, (":do { /ip hotspot enable [find where name=\"" . [/ip/hotspot/get $h name] . "\"] } on-error={}\n")) }
    :foreach i in=$olddhcp do={ :set lines ($lines, (":do { /ip dhcp-server enable [find where name=\"" . [/ip/dhcp-server/get $i name] . "\"] } on-error={}\n")) }
    :foreach l in=$lines do={ :if ([:typeof [:find $rb $l]] = "nil") do={ :set rb ($rb . $l) } }
    # The customer interface's ARP mode. A mode already reply-only (an earlier run got that far) is restored as
    # "enabled": a recorded line always exists, so the rollback never leaves the interface reply-only.
    :foreach x in=[/interface/bridge/find where name=$ifc] do={
      :local cur [/interface/bridge/get $x arp]
      :local pre (":do { /interface bridge set [find where name=\"" . $ifc . "\"] arp=")
      :if ([:typeof [:find $rb $pre]] = "nil") do={
        :if ($cur = "reply-only") do={ :set cur "enabled" }
        :set rb ($rb . $pre . $cur . " } on-error={}\n")
      }
    }
    :foreach x in=[/interface/vlan/find where name=$ifc] do={
      :local cur [/interface/vlan/get $x arp]
      :local pre (":do { /interface vlan set [find where name=\"" . $ifc . "\"] arp=")
      :if ([:typeof [:find $rb $pre]] = "nil") do={
        :if ($cur = "reply-only") do={ :set cur "enabled" }
        :set rb ($rb . $pre . $cur . " } on-error={}\n")
      }
    }
    :foreach x in=[/interface/ethernet/find where name=$ifc] do={
      :local cur [/interface/ethernet/get $x arp]
      :local pre (":do { /interface ethernet set [find where name=\"" . $ifc . "\"] arp=")
      :if ([:typeof [:find $rb $pre]] = "nil") do={
        :if ($cur = "reply-only") do={ :set cur "enabled" }
        :set rb ($rb . $pre . $cur . " } on-error={}\n")
      }
    }
    /system/script set [find where name="ftn-rollback"] source=$rb
    /system/script remove [find where name="ftn-rollback-tick"]
    /system/script add name=ftn-rollback-tick policy=read,write,policy,test source=":local id [/system scheduler find where name=\"ftn-rollback-timer\"]\n:if ([:len \$id] = 0) do={ :error \"no timer\" }\n:local c [/system scheduler get \$id comment]\n:local n [:tonum [:pick \$c 20 [:len \$c]]]\n:set n (\$n + 1)\n:if (\$n >= 15) do={ /system script run ftn-rollback } else={ /system scheduler set \$id comment=(\"ftn: rollback timer \" . \$n) }\n" comment="ftn: IPoE rollback timer tick"
    /system/scheduler remove [find where name="ftn-rollback-timer"]
    /system/scheduler add name=ftn-rollback-timer interval=1m on-event=ftn-rollback-tick policy=read,write,policy,test comment="ftn: rollback timer 0"
    :set armed true
  }
} do={ :set report ($report . "3 rollback timer: " . $e . "\n"); :set abort ("ftn: section 3 rollback timer failed: " . $e); :error $abort }

:put "ftn cutover 4/7: unregistered pool, DHCP server, 2 flat(s) (nothing is live yet)"
:onerror e in={
  :if ([:len [/ip/pool/find where name="ftn-unreg"]] = 0) do={
    /ip/pool add name=ftn-unreg ranges=10.5.50.10-10.5.50.59 comment="ftn: unregistered pool"
  } else={
    /ip/pool set [find where name="ftn-unreg"] ranges=10.5.50.10-10.5.50.59
  }
  :if ([:len [/ip/dhcp-server/find where name="ftn-dhcp"]] = 0) do={
    /ip/dhcp-server add name=ftn-dhcp interface=$ifc address-pool=ftn-unreg lease-time=10m authoritative=yes add-arp=yes disabled=yes comment="ftn: IPoE DHCP"
  } else={
    /ip/dhcp-server set [find where name="ftn-dhcp"] interface=$ifc address-pool=ftn-unreg lease-time=10m authoritative=yes add-arp=yes
  }
  :if ([:len [/ip/dhcp-server/network/find where address="10.5.50.0/24"]] = 0) do={
    /ip/dhcp-server/network add address=10.5.50.0/24 gateway=10.5.50.1 dns-server=10.5.50.1 comment="ftn: IPoE DHCP network"
  }
  /ip/dhcp-server/lease remove [find where comment~"^ftn:acct:"]
  /queue/simple remove [find where comment~"^ftn:acct:"]
  /ip/firewall/address-list remove [find where list="ftn-suspended" and comment~"^ftn:acct:"]
  /ip/firewall/address-list remove [find where list="ftn-unreg" and comment="ftn: unregistered range"]
  # Disabled until the switch: the old DHCP pool overlaps this range, so walling it earlier would wall the flats that are online now.
  /ip/firewall/address-list add list=ftn-unreg address=10.5.50.10-10.5.50.59 disabled=yes comment="ftn: unregistered range"
  :do { /ip/dhcp-server/lease add address=10.5.50.60 mac-address=AA:BB:CC:DD:EE:01 server=ftn-dhcp comment="ftn:acct:K7Q2MX Flat 3B" } on-error={ :set fails ($fails + 1); :put "   lease for Flat 3B failed" }
  # Disabled: a queue on the hotspot's own addresses would throttle it before the switch.
  :do { /queue/simple add name=ftn-K7Q2MX target=10.5.50.60/32 max-limit=20M/20M disabled=yes comment="ftn:acct:K7Q2MX Flat 3B" } on-error={ :set fails ($fails + 1); :put "   queue for Flat 3B failed" }
  :do { /ip/dhcp-server/lease add address=10.5.50.61 mac-address=00:E0:FC:12:34:56 server=ftn-dhcp comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   lease for Flat \"10A\" \$x failed" }
  # Disabled: a queue on the hotspot's own addresses would throttle it before the switch.
  :do { /queue/simple add name=ftn-P4R8TN target=10.5.50.61/32 max-limit=10M/10M disabled=yes comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   queue for Flat \"10A\" \$x failed" }
  :do { /ip/firewall/address-list add list=ftn-suspended address=10.5.50.61 disabled=yes comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   suspended entry for Flat \"10A\" \$x failed" }
} do={ :set report ($report . "4 DHCP server, leases, queues: " . $e . "\n"); :set abort ("ftn: section 4 DHCP server, leases, queues failed: " . $e); :error $abort }

:put "ftn cutover 5/7: walled garden (DNS and the payment page only) and queue-friendly firewall"
:onerror e in={
  /ip/firewall/filter remove [find where comment~"^ftn: ipoe"]
  /ip/firewall/nat remove [find where comment~"^ftn: ipoe"]
  :do { /ipv6/firewall/filter remove [find where comment~"^ftn: ipoe"] } on-error={}
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=drop src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled input drop ftn-suspended" } else={ /ip/firewall/filter add chain=input action=drop src-address-list=ftn-suspended comment="ftn: ipoe walled input drop ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=icmp src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled icmp ftn-suspended" } else={ /ip/firewall/filter add chain=input action=accept protocol=icmp src-address-list=ftn-suspended comment="ftn: ipoe walled icmp ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled dhcp ftn-suspended" } else={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 src-address-list=ftn-suspended comment="ftn: ipoe walled dhcp ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=drop src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled input drop ftn-unreg" } else={ /ip/firewall/filter add chain=input action=drop src-address-list=ftn-unreg comment="ftn: ipoe walled input drop ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=icmp src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled icmp ftn-unreg" } else={ /ip/firewall/filter add chain=input action=accept protocol=icmp src-address-list=ftn-unreg comment="ftn: ipoe walled icmp ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled dhcp ftn-unreg" } else={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=67 src-address-list=ftn-unreg comment="ftn: ipoe walled dhcp ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe dns tcp" } else={ /ip/firewall/filter add chain=input action=accept protocol=tcp dst-port=53 in-interface=$ifc comment="ftn: ipoe dns tcp" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe dns udp" } else={ /ip/firewall/filter add chain=input action=accept protocol=udp dst-port=53 in-interface=$ifc comment="ftn: ipoe dns udp" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=drop protocol=tcp in-interface=$ifc dst-port=8081 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe block rest" } else={ /ip/firewall/filter add chain=input action=drop protocol=tcp in-interface=$ifc dst-port=8081 comment="ftn: ipoe block rest" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=drop in-interface=$ifc out-interface=$ifc place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe isolate flats" } else={ /ip/firewall/filter add chain=forward action=drop in-interface=$ifc out-interface=$ifc comment="ftn: ipoe isolate flats" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related dst-address=10.5.50.0/24 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe customer est/rel in (above FastTrack, so queues apply)" } else={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related dst-address=10.5.50.0/24 comment="ftn: ipoe customer est/rel in (above FastTrack, so queues apply)" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe customer est/rel out (above FastTrack, so queues apply)" } else={ /ip/firewall/filter add chain=forward action=accept connection-state=established,related src-address=10.5.50.0/24 comment="ftn: ipoe customer est/rel out (above FastTrack, so queues apply)" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-suspended comment="ftn: ipoe walled reject ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=icmp-admin-prohibited src-address-list=ftn-unreg comment="ftn: ipoe walled reject ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject tcp ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-suspended comment="ftn: ipoe walled reject tcp ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled reject tcp ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=reject reject-with=tcp-reset protocol=tcp src-address-list=ftn-unreg comment="ftn: ipoe walled reject tcp ftn-unreg" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-suspended place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled payment page ftn-suspended" } else={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-suspended comment="ftn: ipoe walled payment page ftn-suspended" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-unreg place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe walled payment page ftn-unreg" } else={ /ip/firewall/filter add chain=forward action=accept protocol=tcp dst-port=80,443 dst-address=203.0.113.10 src-address-list=ftn-unreg comment="ftn: ipoe walled payment page ftn-unreg" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-suspended to-addresses=203.0.113.10 to-ports=80 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled http to the payment page ftn-suspended" } else={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-suspended to-addresses=203.0.113.10 to-ports=80 comment="ftn: ipoe walled http to the payment page ftn-suspended" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-unreg to-addresses=203.0.113.10 to-ports=80 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled http to the payment page ftn-unreg" } else={ /ip/firewall/nat add chain=dstnat action=dst-nat protocol=tcp dst-port=80 dst-address=!203.0.113.10 src-address-list=ftn-unreg to-addresses=203.0.113.10 to-ports=80 comment="ftn: ipoe walled http to the payment page ftn-unreg" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list=ftn-suspended to-ports=53 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled dns redirect tcp ftn-suspended" } else={ /ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list=ftn-suspended to-ports=53 comment="ftn: ipoe walled dns redirect tcp ftn-suspended" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list=ftn-suspended to-ports=53 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled dns redirect udp ftn-suspended" } else={ /ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list=ftn-suspended to-ports=53 comment="ftn: ipoe walled dns redirect udp ftn-suspended" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list=ftn-unreg to-ports=53 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled dns redirect tcp ftn-unreg" } else={ /ip/firewall/nat add chain=dstnat action=redirect protocol=tcp dst-port=53 src-address-list=ftn-unreg to-ports=53 comment="ftn: ipoe walled dns redirect tcp ftn-unreg" }
  :if ([:len [/ip/firewall/nat find where dynamic=no]] > 0) do={ /ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list=ftn-unreg to-ports=53 place-before=([/ip/firewall/nat find where dynamic=no]->0) comment="ftn: ipoe walled dns redirect udp ftn-unreg" } else={ /ip/firewall/nat add chain=dstnat action=redirect protocol=udp dst-port=53 src-address-list=ftn-unreg to-ports=53 comment="ftn: ipoe walled dns redirect udp ftn-unreg" }
  :if ($v6) do={ :if ([:len [/ipv6/firewall/filter/find where dynamic=no]] > 0) do={ /ipv6/firewall/filter add chain=forward action=drop in-interface=$ifc place-before=([/ipv6/firewall/filter/find where dynamic=no]->0) comment="ftn: ipoe v6 drop" } else={ /ipv6/firewall/filter add chain=forward action=drop in-interface=$ifc comment="ftn: ipoe v6 drop" } }
  :if ([:len [/ip/firewall/nat/find where comment="ftn: ipoe customer masquerade"]] = 0) do={
    /ip/firewall/nat add chain=srcnat action=masquerade src-address=10.5.50.0/24 out-interface-list=WAN comment="ftn: ipoe customer masquerade"
  }
} do={ :set report ($report . "5 firewall: " . $e . "\n"); :set abort ("ftn: section 5 firewall failed: " . $e); :error $abort }

:put "ftn cutover 6/7: switching the flats over"
:onerror e in={
  # Marked first: from here on a failure means the flats may be half switched.
  :set switched true
  :foreach i in=$hs do={ /ip/hotspot disable $i }
  :foreach i in=$olddhcp do={ /ip/dhcp-server disable $i }
  /ip/dhcp-server enable [find where name="ftn-dhcp"]
  # Static ARP for every flat, and for the devices kept above. Only now: under the hotspot they would be live too early.
  /ip/arp remove [find where comment~"^ftn:acct:"]
  /ip/arp remove [find where comment="ftn: keep"]
  /ip/dhcp-server/lease remove [find where comment="ftn: keep"]
  :do { /ip/arp add address=10.5.50.60 mac-address=AA:BB:CC:DD:EE:01 interface=$ifc comment="ftn:acct:K7Q2MX Flat 3B" } on-error={ :set fails ($fails + 1); :put "   ARP entry for Flat 3B failed" }
  :do { /ip/arp add address=10.5.50.61 mac-address=00:E0:FC:12:34:56 interface=$ifc comment="ftn:acct:P4R8TN Flat \"10A\" \$x" } on-error={ :set fails ($fails + 1); :put "   ARP entry for Flat \"10A\" \$x failed" }
  :foreach e in=$keepArp do={ :local p [:find $e "|"]; :do { /ip/arp add address=[:pick $e ($p + 1) [:len $e]] mac-address=[:pick $e 0 $p] interface=$ifc comment="ftn: keep" } on-error={ :set fails ($fails + 1); :put ("   keeping " . $e . " failed") } }
  :foreach e in=$keepLease do={ :local p [:find $e "|"]; :do { /ip/dhcp-server/lease add address=[:pick $e ($p + 1) [:len $e]] mac-address=[:pick $e 0 $p] server=ftn-dhcp comment="ftn: keep" } on-error={ :set fails ($fails + 1); :put ("   keeping " . $e . " failed") } }
  # The flats' queues go live now, above any other queue (the first matching simple queue wins).
  :local fq ""
  :foreach q in=[/queue/simple/find where dynamic=no] do={ :if ([:len $fq] = 0 && [:pick [/queue/simple/get $q name] 0 4] != "ftn-") do={ :set fq [:tostr $q] } }
  /queue/simple enable [find where comment~"^ftn:acct:"]
  :if ([:len $fq] > 0) do={ :do { /queue/simple/move [/queue/simple/find where name~"^ftn-"] destination=$fq } on-error={ :put "   WARNING: could not move the flats' queues above the other queues" } }
  /ip/firewall/address-list enable [find where list="ftn-unreg" and comment="ftn: unregistered range"]
  /ip/firewall/address-list enable [find where list="ftn-suspended" and comment~"^ftn:acct:"]
  # Only addresses with a static ARP entry (ours, or the DHCP server's) can talk: nobody can squat a flat's address.
  # The old setting was written into the rollback script in section 3, before anything changed.
  :foreach x in=[/interface/bridge/find where name=$ifc] do={ /interface/bridge set $x arp=reply-only }
  :foreach x in=[/interface/vlan/find where name=$ifc] do={ /interface/vlan set $x arp=reply-only }
  :foreach x in=[/interface/ethernet/find where name=$ifc] do={ /interface/ethernet set $x arp=reply-only }
  # Once more, in case something re-ordered the queues meanwhile.
  :if ([:len $fq] > 0) do={ :do { /queue/simple/move [/queue/simple/find where name~"^ftn-"] destination=$fq } on-error={} }
} do={ :set report ($report . "6 switch: " . $e . "\n"); :set abort ("ftn: section 6 switch failed: " . $e); :error $abort }

:put "ftn cutover 7/7: checking the internet and the billing server"
:local r1 0
:local r2 0
:do { :set r1 [/ping 1.1.1.1 count=3] } on-error={}
:do { :set r2 [/ping 10.99.0.1 count=3] } on-error={}
:if ($r1 = 0) do={ :put "   WARNING: no answer from 1.1.1.1"; :set fails ($fails + 1) } else={ :put ("   internet: " . $r1 . " of 3 replies") }
:if ($r2 = 0) do={ :put "   WARNING: no answer from the billing server tunnel address 10.99.0.1"; :set fails ($fails + 1) } else={ :put ("   billing server: " . $r2 . " of 3 replies") }
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " problem(s), see the messages above") }
:put "ftn: the flats are now on IPoE. Check one flat, then press Confirm cutover in the billing admin within 15 minutes."
:put "ftn: otherwise the router goes back to the hotspot by itself."
:put "ftn: a flat that was moved out of the .10-.59 range gets its new address when its ONT renews. Reboot those ONTs."
} on-error={
  :put "ftn: the cutover stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  :if ($armed) do={
    # Safe before the switch too: the rollback script only undoes what this script did. The timer is left alone;
    # the rollback script removes it itself, and if it fails the timer is still the safety net.
    :put "   undoing the changes now (running ftn-rollback)"
    :do { /system/script/run ftn-rollback } on-error={ :put "   WARNING: ftn-rollback failed. Run /system/script/run ftn-rollback by hand; the timer will also try in a few minutes" }
    :if ($switched) do={ :put "   the flats were switched and are going back to the hotspot" } else={ :put "   the flats were not switched" }
  } else={
    :if ($confirmed) do={ :put "   the building was already cut over and confirmed: nothing was rolled back" } else={ :put "   nothing was changed: the hotspot is still serving the flats" }
  }
  /file remove [find where name="ftn-cutover.rsc"]
  :error "ftn: cutover stopped; the script file was removed from the router"
}
/file remove [find where name="ftn-cutover.rsc"]
