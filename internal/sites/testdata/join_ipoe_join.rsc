# Fast Tech Net: join script (stage: join) for Block A (sab12cd), generated 2026-11-18 09:30 UTC
# This only connects the router to the billing server. No customer is affected:
# the hotspot, the DHCP server and the flats' addresses are left exactly as they are.
# Single use. It holds this router's WireGuard private key and REST password,
# so keep it private. It removes itself from the router when it finishes, and
# also when it stops on an error (the whole body is wrapped for that).
# Every object it creates has a comment that starts with "ftn:".
:local fails 0
:local abort ""
:local report ""
:local ver ""
:local result "fail"
:do {

:put "ftn join 0/5: checking RouterOS"
:set ver [/system/resource/get version]
:put ("   RouterOS " . $ver)
:local major [:tonum [:pick $ver 0 [:find $ver "."]]]
:if ($major < 7) do={ :set abort "ftn: RouterOS 7 or newer is required"; :set report ($report . $abort . "\n"); :error $abort }

:put "ftn join 1/5: WireGuard tunnel to the billing server"
:onerror e in={
  :if ([:len [/interface/wireguard/find where name="wg-ftn"]] = 0) do={
    /interface/wireguard add name=wg-ftn private-key="mFtRkR0kq3B0hH8xYQ1p0Zk9m3m8wq2Fv5s8y8q1m0A=" mtu=1380 comment="ftn: tunnel to the billing server"
  } else={
    /interface/wireguard set [find where name="wg-ftn"] private-key="mFtRkR0kq3B0hH8xYQ1p0Zk9m3m8wq2Fv5s8y8q1m0A=" mtu=1380
  }
  /ip/address remove [find where comment="ftn: tunnel address"]
  /ip/address add address=10.99.0.2 network=10.99.0.1 interface=wg-ftn comment="ftn: tunnel address"
  /interface/wireguard/peers remove [find where comment="ftn: billing server"]
  /interface/wireguard/peers add interface=wg-ftn public-key="kJ3b0m2Zs8Xc1Vn5Bq7Lw9Rt4Yu6Ip0Oa2Sd8Fg1Hk=" endpoint-address=203.0.113.10 endpoint-port=51820 allowed-address=10.99.0.1/32 persistent-keepalive=25s comment="ftn: billing server"
} do={ :set report ($report . "1 WireGuard: " . $e . "\n"); :set abort ("ftn: section 1 WireGuard failed: " . $e); :error $abort }

:put "ftn join 2/5: firewall (the tunnel can only reach the REST API)"
:onerror e in={
  /ip/firewall/filter remove [find where comment~"^ftn: wg"]
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg no forward out" } else={ /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn comment="ftn: wg no forward out" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg no forward in" } else={ /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn comment="ftn: wg no forward in" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg drop other input" } else={ /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn comment="ftn: wg drop other input" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg icmp" } else={ /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 comment="ftn: wg icmp" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8081 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg rest" } else={ /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8081 comment="ftn: wg rest" }
} do={ :set report ($report . "2 firewall: " . $e . "\n"); :set abort ("ftn: section 2 firewall failed: " . $e); :error $abort }

:put "ftn join 3/5: REST API for the billing server"
:onerror e in={
  :if ([:len [/user/group/find where name="ftn-api"]] = 0) do={
    /user/group add name=ftn-api policy=read,write,test,sensitive,api,rest-api comment="ftn: billing API"
  } else={
    /user/group set [find where name="ftn-api"] policy=read,write,test,sensitive,api,rest-api
  }
  :if ([:len [/user/find where name="ftn-api"]] = 0) do={
    /user/add name=ftn-api group=ftn-api password="Zx3Kp9Qw7Lm2Vb5Nc8Td1Rf4Gh6Jy0Ua" address=10.99.0.1/32 comment="ftn: billing API"
  } else={
    /user/set [find where name="ftn-api"] group=ftn-api password="Zx3Kp9Qw7Lm2Vb5Nc8Td1Rf4Gh6Jy0Ua" address=10.99.0.1/32
  }
  /ip/service set www port=8081 disabled=no address=10.99.0.1/32
  :put "   WebFig (the www service) now answers only the billing server; WinBox, SSH and the API are not affected"
} do={ :set report ($report . "3 REST API: " . $e . "\n"); :set abort ("ftn: section 3 REST API failed: " . $e); :error $abort }

:put "ftn join 4/5: cutover kit (backup and rollback timer for the server-run cutover)"
:if ([:tostr [/system/device-mode/get scheduler]] ~ "^(true|yes)$") do={
:onerror e in={
  /system/scheduler remove [find where name="ftn-kit-timer"]
  /system/script remove [find where name="ftn-kit-tick"]
  /system/script remove [find where name="ftn-kit-rollback"]
  /system/script add name=ftn-kit-rollback policy=read,write,policy,test source=":log warning \"ftn: IPoE rollback running\"\n:do { /ip dhcp-server disable [find where name=\"ftn-dhcp\"] } on-error={}\n:do { /ip hotspot enable [find where name=\"hotspot1\"] } on-error={}\n:do { /ip firewall filter remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ip firewall nat remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ipv6 firewall filter remove [find where comment~\"^ftn: ipoe\"] } on-error={}\n:do { /ip arp remove [find where comment~\"^ftn:acct:\"] } on-error={}\n:do { /ip arp remove [find where comment=\"ftn: keep\"] } on-error={}\n:do { /ip dhcp-server lease remove [find where comment=\"ftn: keep\"] } on-error={}\n:do { /queue simple remove [find where comment~\"^ftn:acct:\"] } on-error={}\n:foreach r in=[/ip firewall address-list find where list=\"ftn-undo\"] do={\n  :do {\n    :local c [/ip firewall address-list get \$r comment]\n    :local p1 [:find \$c \"|\"]\n    :local k [:pick \$c 0 \$p1]\n    :local v [:pick \$c (\$p1 + 1) [:len \$c]]\n    :if (\$k = \"hotspot\") do={ :do { /ip hotspot enable [find where name=\$v] } on-error={} }\n    :if (\$k = \"dhcp\") do={ :do { /ip dhcp-server enable [find where name=\$v] } on-error={} }\n    :if (\$k = \"arp\") do={\n      :local p2 [:find \$v \"|\"]\n      :local kind [:pick \$v 0 \$p2]\n      :local w [:pick \$v (\$p2 + 1) [:len \$v]]\n      :local p3 [:find \$w \"|\"]\n      :local ifn [:pick \$w 0 \$p3]\n      :local mode [:pick \$w (\$p3 + 1) [:len \$w]]\n      :if (\$mode ~ \"^(disabled|enabled|proxy-arp|local-proxy-arp|reply-only)\$\") do={\n        :if (\$kind = \"bridge\") do={ :do { /interface bridge set [find where name=\$ifn] arp=\$mode } on-error={} }\n        :if (\$kind = \"vlan\") do={ :do { /interface vlan set [find where name=\$ifn] arp=\$mode } on-error={} }\n        :if (\$kind = \"ethernet\") do={ :do { /interface ethernet set [find where name=\$ifn] arp=\$mode } on-error={} }\n      }\n    }\n  } on-error={ :log warning \"ftn: kit rollback could not read an undo row\" }\n}\n:do { /ip firewall address-list remove [find where list=\"ftn-armed\"] } on-error={}\n:do { /ip firewall address-list remove [find where list=\"ftn-undo\"] } on-error={}\n:log warning \"ftn: kit rollback done\"\n" comment="ftn: kit v1"
  /system/script add name=ftn-kit-tick policy=ftp,read,write,policy,test,sensitive source=":local bk [/ip firewall address-list find where list=\"ftn-cmd\" and comment=\"backup\"]\n:if ([:len \$bk] > 0) do={\n  :do {\n    :if ([:len [/file find where name=\"ftn-pre-ipoe.backup\"]] = 0) do={ /system backup save name=ftn-pre-ipoe }\n    :if ([:len [/file find where name=\"ftn-pre-ipoe.rsc\"]] = 0) do={ /export file=ftn-pre-ipoe }\n  } on-error={ :log warning \"ftn: kit backup failed\" }\n  /ip firewall address-list remove [find where list=\"ftn-cmd\" and comment=\"backup\"]\n}\n:local a [/ip firewall address-list find where list=\"ftn-armed\"]\n:if ([:len \$a] > 0) do={\n  :local c [/ip firewall address-list get (\$a->0) comment]\n  :if (\$c = \"now\") do={\n    /system script run ftn-kit-rollback\n  } else={\n    :local n 0\n    :do { :set n [:tonum \$c] } on-error={}\n    :if ([:typeof \$n] != \"num\") do={ :set n 0 }\n    :set n (\$n + 1)\n    :if (\$n >= 30) do={\n      /system script run ftn-kit-rollback\n    } else={\n      /ip firewall address-list set (\$a->0) comment=[:tostr \$n]\n    }\n  }\n}\n" comment="ftn: kit v1"
  /system/scheduler add name=ftn-kit-timer interval=30s on-event=ftn-kit-tick policy=ftp,read,write,policy,test,sensitive comment="ftn: kit v1"
  :put "   kit installed: the server can now run the cutover safety net itself"
} do={ :set report ($report . "4 cutover kit: " . $e . "\n"); :set fails ($fails + 1); :put ("ftn: section 4 cutover kit failed, continuing: " . $e) }
} else={
  :set fails ($fails + 1)
  :set report ($report . "4 cutover kit: skipped, the scheduler is off in device-mode\n")
  :put "   cutover kit NOT installed: device-mode has the scheduler off. Run /system/device-mode/update scheduler=yes, press the button, then paste the join command again"
}

:put "ftn join 5/5: checking the tunnel"
:do { /ping 10.99.0.1 count=3 } on-error={ :put "   no answer from the billing server yet; the tunnel can take a few seconds to come up" }
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " section(s) failed, see the messages above") } else={ :put "ftn: all sections done. The flats were not touched."; :set result "ok" }
:if ([:len $report] > 0) do={ :put "ftn: problems:"; :put $report }
:if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="https://billing.example.co.ke/j/TOKEN-abc_123/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . "\n" . $report) output=none check-certificate=yes } on-error={ :put "   could not send the join report to the server" }
} on-error={
  :put "ftn: the join script stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  :if ([:len $report] > 1500) do={ :set report [:pick $report 0 1500] }; :do { /tool/fetch url="https://billing.example.co.ke/j/TOKEN-abc_123/report" http-method=post http-header-field="Content-Type: text/plain" http-data=("result " . $result . " ros " . $ver . "\n" . $report) output=none check-certificate=yes } on-error={ :put "   could not send the join report to the server" }
  /file remove [find where name="ftn-join.rsc"]
  :error "ftn: join stopped; the script file was removed from the router. Fix the cause, then make a new join command"
}
/file remove [find where name="ftn-join.rsc"]
