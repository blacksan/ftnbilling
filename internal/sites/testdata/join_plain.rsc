# Fast Tech Net: join script for Block A (sab12cd), generated 2026-11-18 09:30 UTC
# Single use. It holds this router's WireGuard private key and REST password,
# so keep it private. It removes itself from the router when it finishes.
# Every object it creates has a comment that starts with "ftn:".
:local fails 0

:put "ftn 0/9: checking RouterOS"
:local ver [/system/resource/get version]
:put ("   RouterOS " . $ver)
:local major [:tonum [:pick $ver 0 [:find $ver "."]]]
:if ($major < 7) do={ :error "ftn: RouterOS 7 or newer is required" }

:put "ftn 1/9: WireGuard tunnel to the billing server"
:do {
  :if ([:len [/interface/wireguard/find where name="wg-ftn"]] = 0) do={
    /interface/wireguard add name=wg-ftn private-key="mFtRkR0kq3B0hH8xYQ1p0Zk9m3m8wq2Fv5s8y8q1m0A=" mtu=1380 comment="ftn: tunnel to the billing server"
  } else={
    /interface/wireguard set [find where name="wg-ftn"] private-key="mFtRkR0kq3B0hH8xYQ1p0Zk9m3m8wq2Fv5s8y8q1m0A=" mtu=1380
  }
  /ip/address remove [find where comment="ftn: tunnel address"]
  /ip/address add address=10.99.0.2 network=10.99.0.1 interface=wg-ftn comment="ftn: tunnel address"
  /interface/wireguard/peers remove [find where comment="ftn: billing server"]
  /interface/wireguard/peers add interface=wg-ftn public-key="kJ3b0m2Zs8Xc1Vn5Bq7Lw9Rt4Yu6Ip0Oa2Sd8Fg1Hk=" endpoint-address=203.0.113.10 endpoint-port=51820 allowed-address=10.99.0.1/32 persistent-keepalive=25s comment="ftn: billing server"
} on-error={ :error "ftn: section 1 (WireGuard) failed" }

:put "ftn 2/9: firewall (the tunnel can only reach the REST API)"
:do {
  /ip/firewall/filter remove [find where comment~"^ftn: wg"]
  :if ([:len [/ip/firewall/filter find]] > 0) do={ /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn place-before=([/ip/firewall/filter find]->0) comment="ftn: wg no forward out" } else={ /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn comment="ftn: wg no forward out" }
  :if ([:len [/ip/firewall/filter find]] > 0) do={ /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn place-before=([/ip/firewall/filter find]->0) comment="ftn: wg no forward in" } else={ /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn comment="ftn: wg no forward in" }
  :if ([:len [/ip/firewall/filter find]] > 0) do={ /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn place-before=([/ip/firewall/filter find]->0) comment="ftn: wg drop other input" } else={ /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn comment="ftn: wg drop other input" }
  :if ([:len [/ip/firewall/filter find]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 place-before=([/ip/firewall/filter find]->0) comment="ftn: wg icmp" } else={ /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 comment="ftn: wg icmp" }
  :if ([:len [/ip/firewall/filter find]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8081 place-before=([/ip/firewall/filter find]->0) comment="ftn: wg rest" } else={ /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8081 comment="ftn: wg rest" }
} on-error={ :error "ftn: section 2 (firewall) failed" }

:put "ftn 3/9: REST API for the billing server"
:do {
  :if ([:len [/user/group/find where name="ftn-api"]] = 0) do={
    /user/group add name=ftn-api policy=read,write,test,sensitive,rest-api comment="ftn: billing API"
  } else={
    /user/group set [find where name="ftn-api"] policy=read,write,test,sensitive,rest-api
  }
  :if ([:len [/user/find where name="ftn-api"]] = 0) do={
    /user/add name=ftn-api group=ftn-api password="Zx3Kp9Qw7Lm2Vb5Nc8Td1Rf4Gh6Jy0Ua" address=10.99.0.1/32 comment="ftn: billing API"
  } else={
    /user/set [find where name="ftn-api"] group=ftn-api password="Zx3Kp9Qw7Lm2Vb5Nc8Td1Rf4Gh6Jy0Ua" address=10.99.0.1/32
  }
  /ip/service set www port=8081 disabled=no address=10.99.0.1/32
} on-error={ :error "ftn: section 3 (REST API) failed" }

:put "ftn 4/9 and 5/9: customer network and hotspot are left as they are (Provision is off)"

:put "ftn 6/9: hotspot login pages and walled garden"
:do {
  :local dir [/ip/hotspot/profile/get [find where name="ispbill-hsprof"] html-directory]
  :foreach f in={ "login.html";"alogin.html";"status.html";"logout.html";"rlogin.html";"error.html" } do={
    /tool/fetch url=("https://billing.example.co.ke/routeros/hotspot/sab12cd/" . $f) dst-path=($dir . "/" . $f) check-certificate=yes
  }
  /ip/hotspot/walled-garden/ip remove [find where comment="ftn: portal"]
  /ip/hotspot/walled-garden/ip add action=accept dst-address=203.0.113.10 comment="ftn: portal"
} on-error={ :set fails ($fails + 1); :put "ftn: section 6 (login pages) failed, continuing" }

:put "ftn 7/9: traffic flow to the billing server"
:do {
  /ip/traffic-flow set enabled=yes interfaces=br-hotspot
  /ip/traffic-flow/target remove [find where comment="ispbill metrics"]
  /ip/traffic-flow/target remove [find where comment="ftn: netflow"]
  /ip/traffic-flow/target add dst-address=10.99.0.1 port=2055 version=5 comment="ftn: netflow"
  :put "   note: per-flat totals do not depend on traffic flow; the per-service split can be incomplete while FastTrack is on"
} on-error={ :set fails ($fails + 1); :put "ftn: section 7 (traffic flow) failed, continuing" }

:put "ftn 9/9: checking the tunnel"
:do { /ping 10.99.0.1 count=3 } on-error={ :put "   no answer from the billing server yet; the tunnel can take a few seconds to come up" }
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " section(s) failed, see the messages above") } else={ :put "ftn: all sections done" }
/file remove [find where name="ftn-join.rsc"]
