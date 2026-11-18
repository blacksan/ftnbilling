package sites

import (
	"fmt"
	"strings"
)

// The cutover kit: two fixed scripts and a 30 s scheduler that the join script
// (run by the router's admin) installs once. The billing server never creates
// scripts or schedulers; it only adds and removes address-list marker rows
// (which needs only the "write" policy), and the kit reacts to them. The kit
// text is a constant: it contains nothing the server or a customer supplies,
// apart from the hotspot server's name (checked by ValidIface) in the
// rollback script's first lines.
//
// Marker protocol (all rows use address 127.0.0.1; only list and comment matter):
//
//	list=ftn-cmd   comment=backup   save ftn-pre-ipoe.backup and .rsc (each only if absent), then the row is removed
//	list=ftn-armed comment=<n>      countdown: <n> is the tick count (empty or 0 to start); the tick adds 1;
//	                                at KitArmTicks (15 min) the rollback runs. comment=now runs it on the next tick.
//	list=ftn-undo  comment=...      what the rollback restores, one row each:
//	                                  hotspot|<server name>            enable that hotspot server
//	                                  dhcp|<server name>               enable that DHCP server
//	                                  arp|<kind>|<interface>|<mode>    set the interface's arp mode back
//	                                                                   (kind bridge|vlan|ethernet)
//
// Write each ftn-undo row BEFORE the change it undoes (a run that dies in between
// then still rolls back correctly), and for an interface whose arp mode is already
// reply-only record "enabled".
//
// After a rollback the ftn-armed and ftn-undo rows are removed. "Confirm" is
// the server removing those same rows itself.
const (
	KitVersion     = "v1"
	KitComment     = "ftn: kit " + KitVersion
	KitTickScript  = "ftn-kit-tick"
	KitRollback    = "ftn-kit-rollback"
	KitTimer       = "ftn-kit-timer"
	KitInterval    = "30s"
	KitArmTicks    = 30 // 30 ticks of 30 s: 15 minutes
	KitListCmd     = "ftn-cmd"
	KitListArmed   = "ftn-armed"
	KitListUndo    = "ftn-undo"
	KitCmdBackup   = "backup"
	KitArmedNow    = "now"
	KitBackupFile  = "ftn-pre-ipoe" // .backup and .rsc
	kitScriptPolic = "read,write,policy,test"
	kitTickPolicy  = "ftp,read,write,policy,test,sensitive" // BENCH-CHECK: backup save and export may need ftp/sensitive
)

// KitUndoHotspot, KitUndoDHCP and KitUndoARP are the comments of ftn-undo rows.
func KitUndoHotspot(name string) string { return "hotspot|" + name }
func KitUndoDHCP(name string) string    { return "dhcp|" + name }
func KitUndoARP(kind, iface, oldMode string) string {
	return "arp|" + kind + "|" + iface + "|" + oldMode
}

// kitTickSource is the 30 s tick. Both markers are acted on from here only.
func kitTickSource() string {
	return fmt.Sprintf(`:local bk [/ip firewall address-list find where list="%[1]s" and comment="%[2]s"]
:if ([:len $bk] > 0) do={
  :do {
    :if ([:len [/file find where name="%[3]s.backup"]] = 0) do={ /system backup save name=%[3]s }
    :if ([:len [/file find where name="%[3]s.rsc"]] = 0) do={ /export file=%[3]s }
  } on-error={ :log warning "ftn: kit backup failed" }
  /ip firewall address-list remove [find where list="%[1]s" and comment="%[2]s"]
}
:local a [/ip firewall address-list find where list="%[4]s"]
:if ([:len $a] > 0) do={
  :local c [/ip firewall address-list get ($a->0) comment]
  :if ($c = "%[5]s") do={
    /system script run %[6]s
  } else={
    :local n 0
    :do { :set n [:tonum $c] } on-error={}
    :if ([:typeof $n] != "num") do={ :set n 0 }
    :set n ($n + 1)
    :if ($n >= %[7]d) do={
      /system script run %[6]s
    } else={
      /ip firewall address-list set ($a->0) comment=[:tostr $n]
    }
  }
}
`, KitListCmd, KitCmdBackup, KitBackupFile, KitListArmed, KitArmedNow, KitRollback, KitArmTicks)
}

// kitRollbackSource is rollbackBase (minus the legacy timer removal) followed
// by the undo rows. Row comments are parsed with string functions only and
// used as plain values in find expressions: nothing from a comment is ever
// evaluated as code.
func kitRollbackSource(hotspot string) string {
	var base []string
	for _, l := range strings.Split(strings.TrimRight(rollbackBase(hotspot), "\n"), "\n") {
		if strings.Contains(l, "/system scheduler remove") {
			continue
		}
		base = append(base, l)
	}
	return strings.Join(base, "\n") + fmt.Sprintf(`
:foreach r in=[/ip firewall address-list find where list="%[1]s"] do={
  :do {
    :local c [/ip firewall address-list get $r comment]
    :local p1 [:find $c "|"]
    :local k [:pick $c 0 $p1]
    :local v [:pick $c ($p1 + 1) [:len $c]]
    :if ($k = "hotspot") do={ :do { /ip hotspot enable [find where name=$v] } on-error={} }
    :if ($k = "dhcp") do={ :do { /ip dhcp-server enable [find where name=$v] } on-error={} }
    :if ($k = "arp") do={
      :local p2 [:find $v "|"]
      :local kind [:pick $v 0 $p2]
      :local w [:pick $v ($p2 + 1) [:len $v]]
      :local p3 [:find $w "|"]
      :local ifn [:pick $w 0 $p3]
      :local mode [:pick $w ($p3 + 1) [:len $w]]
      :if ($mode ~ "^(disabled|enabled|proxy-arp|local-proxy-arp|reply-only)$") do={
        :if ($kind = "bridge") do={ :do { /interface bridge set [find where name=$ifn] arp=$mode } on-error={} }
        :if ($kind = "vlan") do={ :do { /interface vlan set [find where name=$ifn] arp=$mode } on-error={} }
        :if ($kind = "ethernet") do={ :do { /interface ethernet set [find where name=$ifn] arp=$mode } on-error={} }
      }
    }
  } on-error={ :log warning "ftn: kit rollback could not read an undo row" }
}
:do { /ip firewall address-list remove [find where list="%[2]s"] } on-error={}
:do { /ip firewall address-list remove [find where list="%[1]s"] } on-error={}
:log warning "ftn: kit rollback done"
`, KitListUndo, KitListArmed)
}
