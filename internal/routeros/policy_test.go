package routeros

import (
	"strings"
	"testing"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		cmd  string
		want Verdict
		exec string // expected command sent to the router, for accepted commands
	}{
		// read-only, no confirmation; print always gets terse and without-paging
		{"/ip/address/print", Allow, "/ip address print terse without-paging"},
		{"ip address print where interface=ether1", Allow, "/ip address print terse without-paging where interface=ether1"},
		{"/system/resource/print", Allow, "/system resource print terse without-paging"},
		{"/interface/print stats", Allow, "/interface print terse without-paging stats"},
		{"/interface/print terse", Allow, "/interface print terse without-paging"},
		{"/ip  address   print", Allow, "/ip address print terse without-paging"},
		{"/ip/hotspot/user/print", Allow, "/ip hotspot user print terse without-paging"},
		{"/interface/wireguard/print", Allow, "/interface wireguard print terse without-paging"},
		{"/file/print", Allow, "/file print terse without-paging"},
		{"/log/print", Allow, "/log print terse without-paging"},
		{"print", Allow, ""},
		{"/ip/address/print count-only", Allow, "/ip address print without-paging count-only"},
		{"/ip/dhcp-server/lease/print where mac-address=AA:BB:CC:DD:EE:01", Allow, "/ip dhcp-server lease print terse without-paging where mac-address=AA:BB:CC:DD:EE:01"},
		{`/ip/firewall/filter/print where comment~"user reboot"`, Allow, ""},
		{"/system/identity/get name", Allow, "/system identity get name"},
		{"/ip/address/get 0 address", Allow, "/ip address get 0 address"},
		{"/interface/ethernet/monitor ether1 once", Allow, "/interface ethernet monitor ether1 once"},
		{"/export", Allow, "/export hide-sensitive"},
		{"/ip/firewall/export compact", Allow, "/ip firewall export hide-sensitive compact"},
		{"/ping 8.8.8.8 count=4", Allow, ""},
		{"/ping 8.8.8.8", Allow, ""},
		{"/tool/ping example.co.ke count=2", Allow, ""},
		{"/tool/traceroute 8.8.8.8 count=1", Allow, ""},
		{"/tool traceroute 2001:db8::1", Allow, ""},
		// confirm: changes something
		{"/ip/address/add address=1.2.3.4/24 interface=ether1", Confirm, "/ip address add address=1.2.3.4/24 interface=ether1"},
		{`/ip/firewall/filter/add chain=input comment="allow it"`, Confirm, ""},
		{"/ip/address/set 0 comment=x", Confirm, ""},
		{"/ip/address/remove 0", Confirm, ""},
		{"/interface/disable ether5", Confirm, "/interface disable ether5"},
		{"/interface/enable ether5", Confirm, ""},
		{"/interface/reset-counters ether5", Confirm, ""},
		{"/ip/hotspot/active/remove 0", Confirm, ""},
		{"/system/reboot", Confirm, "/system reboot"},
		{"/system/shutdown", Confirm, ""},
		// denied paths, every verb
		{"/system/reset-configuration", Deny, ""},
		{"/system reset-configuration no-defaults=yes", Deny, ""},
		{"/system  reset-configuration", Deny, ""},
		{"/system/reset-configuration/run", Deny, ""},
		{"/sys res", Deny, ""},
		{"/sys reset", Deny, ""},
		{"/SYSTEM reset-configuration", Deny, ""},
		{"/System/Reset-Configuration", Deny, ""},
		{"/system\treset-configuration", Deny, ""},
		{"/system reset-configuration", Deny, ""},
		{"/ｓｙｓｔｅｍ reset-configuration", Deny, ""},
		{"/system reset‑configuration", Deny, ""},
		{"/system/script/add name=x source=:put", Deny, ""},
		{"/system/script/print", Deny, ""},
		{"/system/script/run x", Deny, ""},
		{"/system/scheduler/add name=x", Deny, ""},
		{"/system/scheduler/print", Deny, ""},
		{"/system/backup/save", Deny, ""},
		{"/system/package/disable ipv6", Deny, ""},
		{"/system/package/print", Deny, ""},
		{"/system/routerboard/upgrade", Deny, ""},
		{"/system/routerboard/print", Deny, ""},
		{"/user/add name=x password=y group=full", Deny, ""},
		{"/user print", Deny, ""},
		{"user active print", Deny, ""},
		{"/us print", Deny, ""},
		{"/password", Deny, ""},
		{"/ip/service/set ssh disabled=yes", Deny, ""},
		{"/ip/service/print", Deny, ""},
		{"/ip serv print", Deny, ""},
		{"/certificate/print", Deny, ""},
		{"/tool/fetch url=http://x/y", Deny, ""},
		{"/tool fet url=http://x/y", Deny, ""},
		{"/import file-name=x.rsc", Deny, ""},
		{"/file/remove x", Deny, ""},
		{"/file/edit x contents=y", Deny, ""},
		{"/file/set x contents=y", Deny, ""},
		{"/interface/wireguard/set wg0 private-key=x", Deny, ""},
		{"/interface/wireguard/add name=w", Deny, ""},
		{"/interface/wireguard/peers/remove 0", Deny, ""},
		{"/interface wireguard peers set 0 preshared-key=x", Deny, ""},
		{"/ppp/secret/print", Deny, ""},
		{"/ppp/secret/add name=a password=b", Deny, ""},
		{"/ip/hotspot/user/add name=a password=b", Deny, ""},
		{"/ip/hotspot/user/set 0 password=b", Deny, ""},
		{"/ip/hotspot/user/remove 0", Deny, ""},
		{"/interface/wireless/security-profiles/print", Deny, ""},
		{"/interface/wifi/security/print", Deny, ""},
		{"/snmp/community/print", Deny, ""},
		{"/radius/print", Deny, ""},
		// scripting syntax anywhere, quotes included
		{`:put "$[/system reset-configuration]"`, Deny, ""},
		{`/ip/address/print where comment="$[/system reset-configuration]"`, Deny, ""},
		{`/ip/address/add comment="a;b"`, Deny, ""},
		{`/ip/address/add comment="[x]"`, Deny, ""},
		{`/ip/address/add comment="{x}"`, Deny, ""},
		{`/ip/address/add comment="a\"b"`, Deny, ""},
		{"/ip/address/add comment=\"a`b\"", Deny, ""},
		{`/ip/address/add comment="a # b"`, Deny, ""},
		{`/ip/address/add comment=":put x"`, Deny, ""},
		{`/ip/address/add comment="hello :put x"`, Deny, ""},
		{":parse", Deny, ""},
		{`:global f [:parse ":put 1"]; $f`, Deny, ""},
		{"$f", Deny, ""},
		{"/system/identity/print; /system/reboot", Deny, ""},
		{"/ip/address/print;/user/add name=a", Deny, ""},
		{"/ip/address/print where [/user find]", Deny, ""},
		{"{ /certificate print }", Deny, ""},
		{":put [/interface wireguard get wg0 private-key]", Deny, ""},
		{":put hello", Deny, ""},
		{":foreach i in=[/ip/address/find] do={:put $i}", Deny, ""},
		{":execute {/system reboot}", Deny, ""},
		{":resolve example.com", Deny, ""},
		{"/ip/address/print\n/system/reboot", Deny, ""},
		{"/ip/address/print\r/system/reboot", Deny, ""},
		{"/ip/address/print\x00", Deny, ""},
		{"/ip/address/print\t", Deny, ""},
		{"/ip/address/print $x", Deny, ""},
		{"/ip/address/print where name=$x", Deny, ""},
		{"/ip/address/print # comment", Deny, ""},
		{"/ip/address/print \\x", Deny, ""},
		{"/ip/address/print where comment=\"a", Deny, ""},
		{"/ip/address/print where comment=\"a\"\"", Deny, ""},
		{"/ip/address/print where comment=ｘ", Deny, ""},
		{"/ip/address/print where comment=\"é\"", Deny, ""},
		{"/ip/address/print :put", Deny, ""},
		{"/ip/address/print /user", Deny, ""},
		{"/ip/address/print (1)", Deny, ""},
		// secrets are never read or filtered on
		{"/interface/wireguard/get wg0 private-key", Deny, ""},
		{"/interface/wireguard/get wg0 value-name=private-key", Deny, ""},
		{"/ip/hotspot/user/get 0 password", Deny, ""},
		{"/ip/hotspot/user/print where password=abc", Deny, ""},
		{"/ip/hotspot/user/print proplist=name,password", Deny, ""},
		{"/ip/ipsec/identity/print where secret=x", Deny, ""},
		{"/interface/wifi/print where security.passphrase=x", Deny, ""},
		{"/ip/ipsec/identity/print where auth-method=pre-shared-key", Deny, ""},
		{"/interface/get ether1 name private-key", Deny, ""},
		{"/certificate/get 0 private-key", Deny, ""},
		// other refusals
		{"/export show-sensitive", Deny, ""},
		{"/ip/address/export show-sensitive", Deny, ""},
		{"/export file=x", Deny, ""},
		{"/ip/address/print show-sensitive", Deny, ""},
		{"/ip/address/print file=out", Deny, ""},
		{"/ip/address/print as-value", Deny, ""},
		{"/ip/address/print detail", Deny, ""},
		{"/log/print follow", Deny, ""},
		{"/log/print interval=1", Deny, ""},
		{"/interface/monitor-traffic ether1 once", Deny, ""},
		{"/interface/ethernet/monitor ether1", Deny, ""},
		{"/ping 8.8.8.8 count=99", Deny, ""},
		{"/ping 8.8.8.8 interval=1", Deny, ""},
		{"/ping -c 4", Deny, ""},
		{"/ping fe80::1%x", Deny, ""},
		{"/ping a b", Deny, ""},
		{"/traceroute 8.8.8.8", Deny, ""},
		{"/tool/ping", Deny, ""},
		{"/ip/address/run x", Deny, ""},
		{"/ip/address/find", Deny, ""},
		{"/ip/address/move 0 destination=1", Deny, ""},
		{"/tool/netwatch/add host=1.1.1.1 up-script=reboot", Deny, ""},
		{"/tool/netwatch/add host=1.1.1.1 on-event=x", Deny, ""},
		{"/ip/address/get 0 address name", Deny, ""},
		{"/ip/address/get", Deny, ""},
		{"", Deny, ""},
		{"   ", Deny, ""},
		{"/", Deny, ""},
		{strings.Repeat("a", 501), Deny, ""},
	}
	for _, c := range cases {
		got := Classify(c.cmd)
		want := c.want
		if got.Verdict != want {
			t.Errorf("%q: got %s (%s), want %s", c.cmd, got.Verdict, got.Reason, want)
			continue
		}
		if len(got.Redacted) > 300 {
			t.Errorf("%q: redacted command is %d characters", c.cmd, len(got.Redacted))
		}
		for _, r := range got.Redacted {
			if r < 0x20 || r > 0x7e {
				t.Errorf("%q: audit text holds a non-printable character", c.cmd)
				break
			}
		}
		if got.Verdict == Deny && (got.Exec != "" || got.Target != "") {
			t.Errorf("%q: a denied command carries something to run", c.cmd)
		}
		if got.Verdict != Deny && got.Kind == KindExec && got.Exec == "" {
			t.Errorf("%q: nothing to run", c.cmd)
		}
		if c.exec != "" && got.Exec != c.exec {
			t.Errorf("%q: exec %q, want %q", c.cmd, got.Exec, c.exec)
		}
	}
}

func TestClassifyPingTraceroute(t *testing.T) {
	d := Classify("/ping 8.8.8.8 count=3")
	if d.Kind != KindPing || d.Target != "8.8.8.8" || d.Count != 3 {
		t.Fatalf("%+v", d)
	}
	if d = Classify("/ping example.com"); d.Count != 4 || d.Target != "example.com" {
		t.Fatalf("%+v", d)
	}
	if d = Classify("/tool traceroute 1.1.1.1"); d.Kind != KindTraceroute || d.Target != "1.1.1.1" {
		t.Fatalf("%+v", d)
	}
}

func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"8.8.8.8", "2001:db8::1", "example.com", "a-b.example.co.ke", "router1"} {
		if !ValidTarget(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-c", "-4", "fe80::1%eth0", "fe80::1%x;/user/add", "8.8.8.8; /x", "a b", "a;b", "$(id)", "`id`",
		"-example.com", "example-.com", "a..b", "exa mple", "example.com/../x", "ex\nample", "ｅxample.com", "http://x", strings.Repeat("a", 64) + ".com"} {
		if ValidTarget(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRedaction(t *testing.T) {
	in := "name=wg0 private-key=\"AAAAsecretBBBB\" preshared-key=xyz password=hunter2 secret=\"a b\" passphrase=p\n" +
		`{"password":"json-pass","private-key":"jk"} private-key: kv-secret` +
		"\n0 name=u wpa2-pre-shared-key=aaa security.passphrase=bbb authentication-key=ccc psk=ddd auth-key=eee key=fff\n"
	out := RedactOutput(in)
	for _, leak := range []string{"AAAAsecretBBBB", "xyz", "hunter2", "a b", "json-pass", "jk\"", "kv-secret", "aaa", "bbb", "ccc", "ddd", "eee", "fff"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "name=wg0") || !strings.Contains(out, "name=u") {
		t.Fatalf("harmless text must stay: %s", out)
	}
	d := Classify("/ip/firewall/filter/add comment=\"x\" password=topsecret")
	if strings.Contains(d.Redacted, "topsecret") || d.Verdict != Confirm {
		t.Fatalf("%+v", d)
	}
	// The audit text of a refused command is also redacted and printable.
	d = Classify("/user/add name=a password=hunter2\n\x1b[31m")
	if strings.Contains(d.Redacted, "hunter2") || strings.ContainsAny(d.Redacted, "\n\x1b") {
		t.Fatalf("%q", d.Redacted)
	}
}
