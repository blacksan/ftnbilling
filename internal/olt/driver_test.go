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
			v, err := s.Show(ctx(t), "show version")
			if err != nil || !strings.Contains(v, "V1600GS-R") {
				t.Fatalf("version %q %v", v, err)
			}
			run, err := s.Show(ctx(t), "show running-config")
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
	f.EnablePass = "right"
	tg := target(f, "ssh")
	tg.Enable = "wrong"
	s, err := Open(ctx(t), VSOL(), tg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Show(ctx(t), "show version"); !errors.Is(err, ErrEnable) {
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
		"enable password x", "shutdown", "no ip route 0.0.0.0 0.0.0.0 10.7.1.1", "no interface vlan 98", "no ipaddress",
		// Abbreviations the CLI accepts, other spellings, "do", and words further along the line.
		"reb", "reboo", "REBOOT", "  Reload  ", "rel", "res", "er", "fo", "form flash", "fac", "def", "dele startup-config", "cop tftp",
		"wr er", "write era", "w e", "write tftp 1.2.3.4", "do reboot", "do  wr er", "system reboot", "onu reset 3", "onu reb 3",
		"shu", "usern admin", "no usern admin", "no ip addr", "no ip rou 0.0.0.0 0.0.0.0 10.7.1.1", "no int vlan 98", "no in vl 98",
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
	if err := s.Config(ctx(t), []string{"interface vlan 98", "no ipaddress 10.7.1.2 255.255.255.248"}); !errors.Is(err, ErrDenied) {
		t.Errorf("removal under the management interface: %v", err)
	}
	if err := s.Config(ctx(t), []string{"no vlan 98"}); !errors.Is(err, ErrDenied) {
		t.Errorf("no vlan 98: %v", err)
	}
	// Good lines still pass.
	if err := s.Config(ctx(t), []string{"vlan 99", "exit", "no onu 3"}); err != nil {
		t.Errorf("ordinary lines refused: %v", err)
	}
	// Every line the dialect itself builds passes the denylist.
	d := VSOL()
	var gen []string
	gen = append(gen, d.VLANLines(100)...)
	gen = append(gen, d.UplinkLines("0/1", []int{99, 98}, 100)...)
	gen = append(gen, d.MgmtIfaceLines(98, "10.7.1.2", "255.255.255.248", "10.7.1.1")...)
	gen = append(gen, d.DBALines(DefaultDBA, DefaultAssured, DefaultMaximum)...)
	gen = append(gen, d.ONUProfileLines(DefaultONUProfile)...)
	gen = append(gen, d.PONEnter(1)...)
	gen = append(gen, d.ONUAddLine(1, DefaultONUProfile, "ABCD12345678"), d.ONURemoveLine(1))
	gen = append(gen, d.ONUServiceLines(1, DefaultDBA, 100, 99)...)
	gen = append(gen, d.PONExit()...)
	gen = append(gen, d.EnableCmd, d.ConfigCmd, d.EndCmd, d.ExitCmd, d.SaveCmd, d.NoPaging, d.VersionCmd, d.RunningCmd, d.AutoFindCmd)
	gen = append(gen, d.StateQuery(1).Cmds...)
	gen = append(gen, d.OpticalQuery(1).Cmds...)
	for _, l := range gen {
		if err := s.checkIn(l, false); err != nil {
			t.Errorf("a dialect line is denied: %q: %v", l, err)
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
		if _, err := s.Show(ctx(t), "show version"); err == nil {
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
	f.FailOn = "tcont 2"
	s, err := Open(ctx(t), VSOL(), target(f, "ssh"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Config(ctx(t), []string{"interface gpon 0/1", "onu 1 tcont 1 dba d", "onu 1 tcont 2 dba d", "onu 1 gemport 1 tcont 1", "exit"})
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Cmd != "onu 1 tcont 2 dba d" || !strings.Contains(ce.Output, "Invalid") {
		t.Fatalf("want a CmdError for the failing line, got %v", err)
	}
	for _, l := range f.Lines() {
		if strings.Contains(l, "gemport") {
			t.Fatalf("a line after the failure was sent: %q", l)
		}
	}
	// The session recovers: back at a usable prompt.
	if _, err := s.Show(ctx(t), "show version"); err != nil {
		t.Fatalf("session not usable after the abort: %v", err)
	}
}

func TestRedact(t *testing.T) {
	in := "username admin password s3cr3t\nsnmp community public\nenable password 7 abcdef\nradius-server key rad1us\ntacacs-server key 7 tac4cs\nvlan 5"
	out := Redact(in)
	for _, w := range []string{"s3cr3t", "public", "abcdef", "rad1us", "tac4cs"} {
		if strings.Contains(out, w) {
			t.Fatalf("%q survived: %s", w, out)
		}
	}
	if !strings.Contains(out, "vlan 5") {
		t.Fatal("redaction ate an ordinary line")
	}
}
