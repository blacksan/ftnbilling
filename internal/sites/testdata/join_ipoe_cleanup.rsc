# Fast Tech Net: IPoE cleanup (stage: cleanup) for Block A (sab12cd), generated 2026-11-18 09:30 UTC
# Run this a week after the cutover, once the flats have been fine on IPoE.
# It deletes only what the cutover disabled: the hotspot server and its profile,
# the old DHCP server and pool, the hotspot's own firewall rules, and the
# rollback script. The backup files ftn-pre-ipoe.* stay on the router.
# It refuses to run unless the cutover was confirmed (no rollback timer) and
# the ftn-dhcp server is enabled.
:local abort ""
:do {

:put "ftn cleanup 1/3: checking that IPoE is running"
:if ([:len [/ip/dhcp-server/find where name="ftn-dhcp" and disabled=no]] = 0) do={ :set abort "ftn: ftn-dhcp is not enabled, nothing was deleted"; :error $abort }
:if ([:len [/system/scheduler/find where name="ftn-rollback-timer"]] > 0) do={ :set abort "ftn: the cutover was not confirmed yet (rollback timer still there), nothing was deleted"; :error $abort }
:local ifc [/ip/dhcp-server/get [find where name="ftn-dhcp"] interface]

:put "ftn cleanup 2/3: deleting the disabled hotspot objects"
:do { /ip/hotspot/remove [find where name="hotspot1" and disabled=yes] } on-error={ :put "   hotspot server: left in place" }
:do { /ip/hotspot/profile/remove [find where name="ispbill-hsprof"] } on-error={ :put "   hotspot profile: left in place (still in use?)" }
:do { /ip/dhcp-server/remove [find where interface=$ifc and disabled=yes and name!="ftn-dhcp"] } on-error={ :put "   old DHCP server: left in place" }
:do { /ip/pool/remove [find where name="pool-hotspot"] } on-error={ :put "   pool-hotspot: left in place (still in use?)" }
:do { /ip/hotspot/walled-garden/ip/remove [find where comment="ftn: portal"] } on-error={}
# Flat isolation and REST-port blocking were "ftn: hs" rules; keep them as "ftn: ipoe" rules first.
:if ([:len [/ip/firewall/filter/find where comment="ftn: ipoe isolate flats"]] = 0) do={
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=forward action=drop in-interface=$ifc out-interface=$ifc place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe isolate flats" } else={ /ip/firewall/filter add chain=forward action=drop in-interface=$ifc out-interface=$ifc comment="ftn: ipoe isolate flats" }
}
:if ([:len [/ip/firewall/filter/find where comment="ftn: ipoe block rest"]] = 0) do={
  :if ([:len [/ip/firewall/filter find where dynamic=no]] > 0) do={ /ip/firewall/filter add chain=input action=drop protocol=tcp in-interface=$ifc dst-port=8081 place-before=([/ip/firewall/filter find where dynamic=no]->0) comment="ftn: ipoe block rest" } else={ /ip/firewall/filter add chain=input action=drop protocol=tcp in-interface=$ifc dst-port=8081 comment="ftn: ipoe block rest" }
}
/ip/firewall/filter remove [find where comment~"^ftn: hs"]

:put "ftn cleanup 3/3: deleting the rollback script"
/system/script remove [find where name="ftn-rollback"]
/system/script remove [find where name="ftn-rollback-tick"]
:put "ftn: cleanup done. The flats are served by ftn-dhcp."
} on-error={
  :put "ftn: the cleanup stopped early, see below"
  :if ([:len $abort] > 0) do={ :put $abort }
  /file remove [find where name="ftn-cleanup.rsc"]
  :error "ftn: cleanup stopped; the script file was removed from the router"
}
/file remove [find where name="ftn-cleanup.rsc"]
