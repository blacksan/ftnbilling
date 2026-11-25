package olt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/olt/fake"
)

func newFake(t *testing.T) *fake.OLT {
	t.Helper()
	f, err := fake.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func target(f *fake.OLT, proto string) Target {
	addr := f.SSHAddr
	if proto == "telnet" {
		addr = f.TelnetAddr
	}
	return Target{Addr: addr, Proto: proto, User: f.User, Pass: f.Pass, Enable: f.EnablePass, Protect: 98}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestSessionBothTransports(t *testing.T) {
	for _, proto := range []string{"ssh", "telnet"} {
		t.Run(proto, func(t *testing.T) {
			f := newFake(t)
			f.EnablePass = "en-pass"
			f.PageLines = 3
			f.SetRunning("vlan 100\ninterface gpon 0/1\n  onu add 1 profile hgu sn VSOL00000001\n  onu 1 tcont 1 dba dba_1\nip route 0.0.0.0 0.0.0.0 10.7.1.1\n")
			s, err := Open(ctx(t), VSOL(), target(f, proto), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if proto == "ssh" && s.NewHostKey != f.HostKeyFP {
				t.Fatalf("host key not captured: %q vs %q", s.NewHostKey, f.HostKeyFP)
			}
			s.DisablePaging(ctx(t))
			// The bench: "show version" is not valid at the # prompt, it exists in configuration mode.
			if _, err := s.Show(ctx(t), "show version"); err == nil {
				t.Fatal("show version worked at the # prompt: the fake does not follow the bench")
			}
			v, err := s.ShowAt(ctx(t), ModeConfig, "show version")
			if err != nil || !strings.Contains(v, "V1600GS-R") {
				t.Fatalf("version %q %v", v, err)
			}
			run, err := s.ShowAt(ctx(t), ModeConfig, "show running-config")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(run, "More") || !strings.Contains(run, "onu 1 tcont 1 dba dba_1") || !strings.Contains(run, "ip route") {
				t.Fatalf("pagination left damage or lost lines:\n%s", run)
			}
			if s.mode != ModePriv {
				t.Fatalf("mode %v", s.mode)
			}
		})
	}
}

func TestEnableNeedsRightPassword(t *testing.T) {
	f := newFake(t)
	f.EnablePass = "right" // with an enable password the fake starts at the > prompt
	tg := target(f, "ssh")
	tg.Enable = "wrong"
	s, err := Open(ctx(t), VSOL(), tg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ShowAt(ctx(t), ModeConfig, "show version"); !errors.Is(err, ErrEnable) {
		t.Fatalf("want ErrEnable, got %v", err)
	}
}

func TestWrongLogin(t *testing.T) {
	f := newFake(t)
	for _, proto := range []string{"ssh", "telnet"} {
		tg := target(f, proto)
		tg.Pass = "nope"
		if _, err := Open(ctx(t), VSOL(), tg, nil); err == nil {
			t.Fatalf("%s: wrong password logged in", proto)
		}
	}
}

func TestHostKeyPinned(t *testing.T) {
	f := newFake(t)
	tg := target(f, "ssh")
	tg.PinnedKey = f.HostKeyFP
	s, err := Open(ctx(t), VSOL(), tg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	tg.PinnedKey = "SHA256:somethingelse"
	if _, err := Open(ctx(t), VSOL(), tg, nil); !errors.Is(err, ErrHostKey) {
		t.Fatalf("want ErrHostKey, got %v", err)
	}
}

func TestDenylistBlocksBeforeSending(t *testing.T) {
	f := newFake(t)
	s, err := Open(ctx(t), VSOL(), target(f, "telnet"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bad := []string{
		"erase flash", "era startup-config", "factory-reset", "default interface", "reboot", "reload", "rebo", "format flash:",
		"delete startup-config", "write erase", "copy tftp flash", "upgrade firmware", "username admin password x", "password new",
		"enable password x", "shutdown", "no ip route 0.0.0.0/0 10.7.1.1", "no interface vlan 98", "no ipaddress",
		// Abbreviations the CLI accepts, other spellings, "do", and words further along the line.
		"reb", "reboo", "REBOOT", "  Reload  ", "rel", "res", "er", "fo", "form flash", "fac", "def", "dele startup-config", "cop tftp",
		"wr er", "write era", "w e", "write tftp 1.2.3.4", "do reboot", "do  wr er", "system reboot", "onu reset 3", "onu reb 3",
		"shu", "usern admin", "no usern admin", "no ip addr", "no ip rou 0.0.0.0/0 10.7.1.1", "no int vlan 98", "no in vl 98",
		"no vlan 90-100", "no vlan 5,98", "no vl 98", "clear config",
		// A line break or control character inside a value.
		"onu add 1 profile p sn ABCD12345678\nreboot", "vlan 5\rreload", "vlan 5\x00", "vlan\t5",
	}
	for _, c := range bad {
		if err := s.Config(ctx(t), []string{"vlan 5", c}); !errors.Is(err, ErrDenied) {
			t.Errorf("%q was not denied: %v", c, err)
		}
	}
	// A denied line anywhere in the batch stops it before the first line goes out.
	for _, l := range f.Lines() {
		if strings.HasPrefix(l, "vlan 5") {
			t.Fatalf("a batch with a denied line sent %q", l)
		}
	}
	// Management VLAN protection: removals under interface vlan 98, and "no vlan 98".
	if err := s.Config(ctx(t), []string{"interface vlan 98", "no ip address 10.7.1.2/29"}); !errors.Is(err, ErrDenied) {
		t.Errorf("removal under the management interface: %v", err)
	}
	if err := s.Config(ctx(t), []string{"no vlan 98"}); !errors.Is(err, ErrDenied) {
		t.Errorf("no vlan 98: %v", err)
	}
	// Good lines still pass.
	if err := s.Config(ctx(t), []string{"vlan 99", "exit", "no onu 3"}); err != nil {
		t.Errorf("ordinary lines refused: %v", err)
	}
	// Every line the dialect itself builds passes the denylist, in the context it is sent in.
	d := VSOL()
	sx := &Session{d: d}
	acs := ACS{URL: "http://10.99.0.1:7547/", User: "ftn-a1b2c3", Pass: "Pa55word99", InformSeconds: 300}
	var gen []string
	gen = append(gen, d.VLANLines(100)...)
	gen = append(gen, d.UplinkLines("0/1", []int{99, 98}, 100)...)
	gen = append(gen, d.MgmtIfaceLines(98, "10.7.1.2", "255.255.255.248", "10.7.1.1")...)
	gen = append(gen, d.DBALines(d.DefaultDBA, DefaultMaximum)...)
	gen = append(gen, d.LineProfileLines(d.DefaultLine, d.DefaultDBA, 100, 99)...)
	gen = append(gen, d.SrvProfileLines(d.DefaultSrv)...)
	gen = append(gen, d.PriProfileLines(d.DefaultPri, 100, 99, &acs)...)
	gen = append(gen, d.PONEnter(1)...)
	gen = append(gen, d.ONUAddLine(1, d.DefaultONUProfile, "ABCD12345678"), d.ONURemoveLine(1), d.ONUBindLine(1, "line", "line_hgu"),
		d.ONUBindLine(1, "srv", "srv_hgu"), d.ONUBindLine(1, "pri", "ftn_pri"), d.ONUDescLine(1, "Maple Gardens A1"))
	gen = append(gen, d.ONUWANLines(1, 2, 99, acs)...)
	gen = append(gen, d.PONExit()...)
	gen = append(gen, d.AutoLearnOffLines(1)...)
	gen = append(gen, d.ExitCmd, d.SaveCmd, d.NoPaging, d.VersionCmd, d.RunningCmd)
	gen = append(gen, d.PONReadQuery(1).Cmds...)
	gen = append(gen, d.VersionQuery().Cmds...)
	cs := cstate{}
	for _, l := range gen {
		if err := sx.checkIn(l, cs); err != nil {
			t.Errorf("a dialect line is denied: %q: %v", Redact(l), err)
		}
		cs = sx.trackIn(l, cs)
	}
	if err := sx.checkIn(d.EnableCmd, cstate{}); err != nil {
		t.Errorf("enable at the user prompt: %v", err)
	}
}

// The "service" exception of a sub-mode needs the OLT's prompt to agree: if a line left the profile by itself, the
// tracking still says "sub-mode" but the prompt does not, and the top-level word is refused.
func TestSubModeExceptionFollowsThePrompt(t *testing.T) {
	s := &Session{d: VSOL(), mode: ModeConfig, cs: cstate{sub: true}}
	for prompt, ok := range map[string]bool{
		"gpon-olt(config)#":              false,
		"gpon-olt(config-line-profile)#": true,
		"gpon-olt(config-pon-0/1)#":      true,
		"gpon-olt#":                      false,
	} {
		s.prompt = prompt
		err := s.check("service internet gemport 1 tag 100")
		if (err == nil) != ok {
			t.Errorf("prompt %q: %v", prompt, err)
		}
	}
}

// Every command of the bench grammar that must never be sent, in the shapes the CLI accepts them.
func TestDenylistBenchGrammar(t *testing.T) {
	d := VSOL()
	s := &Session{d: d, Protect: 98}
	for _, c := range []string{
		"start-shell", "start-sh", "erase startup-config", "erase", "copy running-config tftp", "debug gponapp", "gponapp debug onu_reg on",
		"gponapp show all", "onu rapid-deployment debug on", "set license key AAAA onu-limit 8", "set cust super_onu KEY", "no cust super_onu UBOOTKEY",
		"show cust super_onu UBOOTKEY",
		"onu 1 pri factory_reset", "onu 1 pri factory", "onu 3 pri mac_clean", "onu 1 reboot", "onu 1 reb", "onu-schedule-reboot enable", "onu-schedule-reboot onu 1 at day 1 hour 3 minute 0",
		"no onu-schedule-reboot onu 1", "onu all deactivate", "onu 1 deactivate", "onu 1,2 disable", "onu 1 dis", "onu 2 standy", "onu 1-3 deactivate",
		"onu auto-delete enable", "onu auto-delete timeout 5", "no onu auto-delete", "remove pre-config-onu all", "remove pre-config-onu 1",
		"rogue-onu-detect enable auto-shutdown enable", "rogue-onu-detect disable", "upgrade onu_max 10", "upgrade select pon 1 onu all",
		"auto-upgrade onu equipment_id X version 1 image f", "no auto-upgrade onu equipment_id X", "auto-upgrade clean completed onu",
		"onu 1 pri mac_clean", "no onu all", "no onu all desc", "onu all desc x", "onu all profile line name x", "onu all gemport 1 tcont 1", "onu al reboot",
		"no onu all profile", "onu all iphost 1 dhcp",
		"terminal monitor", "terminal width 100", "terminal length 25", "terminal", "term len 0", "ssh 10.0.0.1", "telnet 10.0.0.1", "watchfrr x", "output file",
		// the config-mode top-level words
		"reboot", "exec ls", "download x", "upload x", "upgrade x", "software x", "password x", "user manager", "line vty", "sshd", "ssh", "telnet", "tacacs",
		"login-access-list deny ssh 0.0.0.0 0.0.0.0", "login-access-list permit ssh 10.0.0.1 255.255.255.255", "login-access-list ipv6 deny ssh ::/0", "login-access-list enable", "login-access-list disable", "login-access-list clear", "set x", "schedule x", "clean x", "clear x", "save", "time x", "timezone x", "dst x", "ntp x", "remote x",
		"router ospf", "route-map x", "frr x", "zebra x", "bfd", "mpls x", "evpn", "vrf x", "pseudowire x", "nexthop-group x", "allow-external-route-update",
		"key chain x", "monitor session 1", "service integrated-vtysh-config", "snmp x", "snmp-server community public ro", "web x", "ip route 10.0.0.0/8 1.2.3.4",
		"ip address 1.2.3.4/24 extra", "ip ssh", "interface wan", "interface wan 1", "int wan", "hostname x", "domainname x", "del onu application 1", "del onu preset-profile 1",
		"remove x", "enable password x", "enable", "en",
		// Undoing a denied command, taking VLANs off ports, port mode changes, several ONUs at once.
		"no login-access-list deny telnet 0.0.0.0 0.0.0.0", "no login-access-list enable", "no login-access-list permit ssh 10.0.0.1 255.255.255.255", "no login-access-list deny ssh 0.0.0.0 0.0.0.0", "no login-access-list ipv6 deny ssh ::/0", "no snmp-server community private", "no sshd", "no ssh",
		"no service integrated-vtysh-config", "no telnet", "no tacacs", "no hostname", "no ntp x", "no web", "no time", "no line vty",
		"no vlan 100", "no vlan 99", "no vlan 98", "no vl 100", "no switchport hybrid vlan 98", "no swi hybrid vlan 99", "switchport mode access",
		"switchport access vlan 5", "switchport trunk vlan 5", "no onu 1-64", "no onu 1,2", "no onu 1-64 profile", "no shutdown",
	} {
		cs := cstate{config: true}
		if err := s.checkIn(c, cs); !errors.Is(err, ErrDenied) {
			t.Errorf("%q was not denied: %v", c, err)
		}
	}
	// "service" is a top-level word that is denied, and an ordinary line of a line profile.
	if err := s.checkIn("service internet gemport 1 tag 100", cstate{config: true}); !errors.Is(err, ErrDenied) {
		t.Errorf("service at the top level: %v", err)
	}
	if err := s.checkIn("service internet gemport 1 tag 100", cstate{config: true, sub: true}); err != nil {
		t.Errorf("service in a profile: %v", err)
	}
	if err := s.checkIn("serv internet gemport 1 tag 100", cstate{config: true, sub: true}); !errors.Is(err, ErrDenied) {
		t.Errorf("an abbreviated service in a profile: %v", err)
	}
	// Lines that must keep working.
	for _, c := range []string{
		"terminal length 0", "configure terminal", "interface gpon 0/1", "interface gigabitEthernet 0/1", "interface vlan 98", "profile line name x", "exit", "end", "write",
		"show version", "show running-config", "show onu info", "show onu state", "show onu optical-info", "show onu distance", "show onu auto-find", "show onu auto-find detail-info",
		"show running-config onu 1", "onu add 3 profile default sn VSOL00127E0F", "no onu 3", "onu 3 desc A1", "onu 3 profile line name line_hgu", "onu 3 profile pri name ftn_pri",
		"onu 1,2 desc x", "ip address 10.7.1.2/29", "ip route 0.0.0.0/0 10.7.1.1", "no onu auto-learn", "plug_and_play disable",
		"onu 3 pri wan_adv add route", "onu 3 pri wan_adv commit", "onu 3 pri tr069_mng enable acs_server url http://10.99.0.1:7547/ username u password p certificate disable inform enable inform_interval 300 reverse_connection username u password p",
		"onu add 1 profile default sn VSOL00127e0f", "onu 3 pri onu_mode status enable mode hgu",
	} {
		if err := s.checkIn(c, cstate{config: c != "show version"}); err != nil {
			t.Errorf("%q was refused: %v", c, err)
		}
	}
}

func TestWriteRefusesLineBreaks(t *testing.T) {
	f := newFake(t)
	f.EnablePass = "x"
	tg := target(f, "telnet")
	tg.Enable = "x\rreboot"
	if s, err := Open(ctx(t), VSOL(), tg, nil); err == nil {
		defer s.Close()
		if _, err := s.ShowAt(ctx(t), ModeConfig, "show version"); err == nil {
			t.Fatal("an enable password with a line break was sent")
		}
	}
	for _, l := range f.Lines() {
		if strings.Contains(strings.ToLower(l), "reboot") {
			t.Fatalf("the OLT received %q", l)
		}
	}
}

func TestErrorAbortsBatch(t *testing.T) {
	f := newFake(t)
	f.FailOn = "desc b"
	s, err := Open(ctx(t), VSOL(), target(f, "ssh"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Config(ctx(t), []string{"interface gpon 0/1", "onu 1 desc a", "onu 1 desc b", "onu 1 desc cc", "exit"})
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Cmd != "onu 1 desc b" || !strings.Contains(ce.Output, "Invalid") {
		t.Fatalf("want a CmdError for the failing line, got %v", err)
	}
	for _, l := range f.Lines() {
		if strings.Contains(l, "desc cc") {
			t.Fatalf("a line after the failure was sent: %q", l)
		}
	}
	// The session recovers: back at a usable prompt.
	if _, err := s.ShowAt(ctx(t), ModeConfig, "show version"); err != nil {
		t.Fatalf("session not usable after the abort: %v", err)
	}
}

func TestRedact(t *testing.T) {
	in := "onu 1 pri tr069_mng enable acs_server url http://10.99.0.1:7547/ username ftn-abc password Pa55w0rd certificate disable inform enable inform_interval 300 reverse_connection username ftn-abc password Pa55w0rd\nusername admin password s3cr3t\nsnmp community public\nenable password 7 abcdef\nradius-server key rad1us\ntacacs-server key 7 tac4cs\nvlan 5"
	out := Redact(in)
	for _, w := range []string{"Pa55w0rd", "ftn-abc", "s3cr3t", "public", "abcdef", "rad1us", "tac4cs"} {
		if strings.Contains(out, w) {
			t.Fatalf("%q survived: %s", w, out)
		}
	}
	if !strings.Contains(out, "vlan 5") || !strings.Contains(out, "http://10.99.0.1:7547/") {
		t.Fatal("redaction ate an ordinary line")
	}
}

// The server's own notes go through Redact too (errText): its prose must stay readable, its secrets must not.
func TestRedactKeepsTheServersProse(t *testing.T) {
	for _, note := range []string{
		"the OLT would not save its configuration (olt: the OLT did not answer in time); the old password was restored and nothing was saved",
		"the new password did not work in a new session",
		"Secured: the login and enable password were replaced by generated ones",
		"SSH host key of OLT Alpha pinned on first use",
		"OLT Alpha: SSH host key changed",
		"the generated one is stored and can be revealed; the old login password was restored.",
		"there was no known enable password to restore",
	} {
		if got := Redact(note); got != note {
			t.Errorf("prose changed:\n %s\n %s", note, got)
		}
	}
	for in, secret := range map[string]string{
		"password Pa55w0rdXyz":                 "Pa55w0rdXyz",
		"user login-password admin Gen3rated1": "Gen3rated1",
		"password: hunter2":                    "hunter2",
		"community private":                    "private",
	} {
		if strings.Contains(Redact(in), secret) {
			t.Errorf("%q survived in %q", secret, Redact(in))
		}
	}
}
