# Fast Tech Net: join script (stage: join) for Block A (sab12cd), generated 2026-11-18 09:30 UTC
# This only connects the router to the billing server. No customer is affected:
# the hotspot, the DHCP server and the flats' addresses are left exactly as they are.
# Single use. It holds this router's WireGuard private key and REST password,
# so keep it private. It removes itself from the router when it finishes, and
# also when it stops on an error (the whole body is wrapped for that).
# Every object it creates has a comment that starts with "ftn:".
:local fails 0
:local abort ""
:do {

:put "ftn join 0/5: checking RouterOS"
:local ver [/system/resource/get version]
:put ("   RouterOS " . $ver)
:local major [:tonum [:pick $ver 0 [:find $ver "."]]]
:if ($major < 7) do={ :set abort "ftn: RouterOS 7 or newer is required"; :error $abort }

:put "ftn join 1/5: WireGuard tunnel to the billing server"
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
} on-error={ :set abort "ftn: section 1 (WireGuard) failed"; :error $abort }

:put "ftn join 2/5: firewall (the tunnel can only reach the REST API)"
:do {
  /ip/firewall/filter remove [find where comment~"^ftn: wg"]
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg no forward out" } else={ /ip/firewall/filter add chain=forward action=drop out-interface=wg-ftn comment="ftn: wg no forward out" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg no forward in" } else={ /ip/firewall/filter add chain=forward action=drop in-interface=wg-ftn comment="ftn: wg no forward in" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg drop other input" } else={ /ip/firewall/filter add chain=input action=drop in-interface=wg-ftn comment="ftn: wg drop other input" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg icmp" } else={ /ip/firewall/filter add chain=input action=accept protocol=icmp in-interface=wg-ftn src-address=10.99.0.1 comment="ftn: wg icmp" }
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8081 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: wg rest" } else={ /ip/firewall/filter add chain=input action=accept protocol=tcp in-interface=wg-ftn src-address=10.99.0.1 dst-port=8081 comment="ftn: wg rest" }
} on-error={ :set abort "ftn: section 2 (firewall) failed"; :error $abort }

:put "ftn join 3/5: REST API for the billing server"
:do {
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
} on-error={ :set abort "ftn: section 3 (REST API) failed"; :error $abort }

:put "ftn join 4/5: traffic flow to the billing server"
:do {
  :local ifc ""
  :local hs [/ip/hotspot/find where name="hotspot1"]
  :if ([:len $hs] = 0) do={ :set hs [/ip/hotspot/find] }
  :if ([:len $hs] > 0) do={ :set ifc [/ip/hotspot/get ($hs->0) interface] }
  :if ([:len $ifc] > 0) do={
    /ip/traffic-flow set enabled=yes interfaces=$ifc
    /ip/traffic-flow/target remove [find where comment="ftn: netflow"]
    /ip/traffic-flow/target add dst-address=10.99.0.1 port=2055 version=5 comment="ftn: netflow"
    :put ("   traffic flow on " . $ifc)
  } else={
    :put "   no hotspot interface found: set /ip/traffic-flow interfaces by hand if you want the per-service split"
  }
  :put "   note: per-flat totals do not depend on traffic flow; they come from the flats' queues"
} on-error={ :set fails ($fails + 1); :put "ftn: section 4 (traffic flow) failed, continuing" }

:put "ftn join 5/5: checking the tunnel"
:do { /ping 10.99.0.1 count=3 } on-error={ :put "   no answer from the billing server yet; the tunnel can take a few seconds to come up" }
:if ($fails > 0) do={ :put ("ftn: finished with " . $fails . " section(s) failed, see the messages above") } else={ :put "ftn: all sections done. The flats were not touched." }
} on-error={
  :put "ftn: the join script stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  /file remove [find where name="ftn-join.rsc"]
  :error "ftn: join stopped; the script file was removed from the router. Fix the cause, then make a new join command"
}
/file remove [find where name="ftn-join.rsc"]
