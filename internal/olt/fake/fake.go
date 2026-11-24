// Package fake is a small fake V-SOL OLT command line, served over SSH and Telnet, for tests. It follows the grammar
// the bench OLT (V1600GS-R, V1.0.5) printed: SSH lands at "gpon-olt#" (privileged), "show version" and "show
// running-config" work in configuration mode only, "show onu ..." and "onu ..." only inside "interface gpon 0/N"
// (prompt "gpon-olt(config-pon-0/1)#"), profiles are entered with "profile <kind> name X" and left with "commit" and
// "exit", and "onu add" prints the serial back in lower-case hex. It records every line it receives. The output of
// the state and distance tables is a guess (the bench did not show them).
package fake

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// OLT is a fake OLT. Create it with New, then use Addr for the SSH or Telnet listener.
type OLT struct {
	User, Pass, EnablePass string
	Host                   string // prompt name, default gpon-olt
	Version                string // the text "show version" prints (default: the bench's)
	// AutoFind is what "show onu auto-find" lists: {pon, serial}.
	AutoFind [][2]string
	// PageLines paginates "show running-config" with --More-- every n lines (0 = off).
	PageLines int
	// NoPagingCmd makes "terminal length 0" an error (the usual case on an unknown firmware).
	NoPagingCmd bool
	// NoOptical makes "show onu optical-info" an error.
	NoOptical bool
	// FailOn: a command containing this text answers with a "% Invalid input" line.
	FailOn string
	// MaxPON bounds interface gpon 0/N.
	Secret string // appears in the running config as "password <Secret>"

	SSHAddr, TelnetAddr string
	HostKeyFP           string

	mu     sync.Mutex
	lines  []string
	blocks []*block
	closeF []func()
}

// BenchVersion is what "show version" printed on the bench OLT.
const BenchVersion = `  Olt Serial Number:           V2609170360
  Olt Device Model:            V1600GS-R
  Hardware Version:            V7.1.1
  Software Version:            V1.0.5
  Software Created Time:       Tue, 14 Jul 2026 14:35:36 
  Copyright 2020-2026
`

type block struct {
	header string
	lines  []string
}

// New starts the fake on loopback ports.
func New() (*OLT, error) {
	o := &OLT{User: "admin", Pass: "olt-pass", Host: "gpon-olt", Version: BenchVersion, Secret: "s3cr3t-hash"}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	_ = pub
	o.HostKeyFP = ssh.FingerprintSHA256(signer.PublicKey())
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if c.User() == o.User && string(p) == o.Pass {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		sl.Close()
		return nil, err
	}
	o.SSHAddr, o.TelnetAddr = sl.Addr().String(), tl.Addr().String()
	o.closeF = []func(){func() { sl.Close() }, func() { tl.Close() }}
	go func() {
		for {
			c, err := sl.Accept()
			if err != nil {
				return
			}
			go o.serveSSH(c, cfg)
		}
	}()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go o.serveTelnet(c)
		}
	}()
	return o, nil
}

// Close stops the listeners.
func (o *OLT) Close() {
	for _, f := range o.closeF {
		f()
	}
}

// Lines returns every command line received so far (login answers are not included).
func (o *OLT) Lines() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.lines...)
}

// Reset forgets the recorded lines.
func (o *OLT) Reset() { o.mu.Lock(); o.lines = nil; o.mu.Unlock() }

// SetRunning replaces the running configuration with a text (blocks start at lines that begin in column 0 and are
// followed by indented lines; the text is only used for "show running-config").
func (o *OLT) SetRunning(text string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.blocks = nil
	var cur *block
	for _, l := range strings.Split(strings.Trim(text, "\n"), "\n") {
		if strings.HasPrefix(l, " ") && cur != nil {
			cur.lines = append(cur.lines, strings.TrimSpace(l))
			continue
		}
		cur = &block{header: strings.TrimSpace(l)}
		o.blocks = append(o.blocks, cur)
	}
}

// Running is the running configuration as "show running-config" prints it.
func (o *OLT) Running() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.running()
}

func (o *OLT) running() string {
	var b strings.Builder
	if o.Secret != "" {
		b.WriteString("username admin password " + o.Secret + "\n")
	}
	for _, bl := range o.blocks {
		b.WriteString(bl.header + "\n")
		for _, l := range bl.lines {
			b.WriteString("  " + l + "\n")
		}
		if len(bl.lines) > 0 {
			b.WriteString("exit\n")
		}
	}
	return b.String()
}

func (o *OLT) serveSSH(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for r := range creqs {
				ok := r.Type == "pty-req" || r.Type == "shell"
				if r.WantReply {
					_ = r.Reply(ok, nil)
				}
				if r.Type == "shell" {
					go func() { o.cli(ch, false); ch.Close() }()
				}
			}
		}()
	}
}

func (o *OLT) serveTelnet(c net.Conn) {
	defer c.Close()
	// Negotiation the client must strip: WILL ECHO, WILL SGA, DO TTYPE.
	_, _ = c.Write([]byte{255, 251, 1, 255, 251, 3, 255, 253, 24})
	br := bufio.NewReader(c)
	rw := struct {
		io.Reader
		io.Writer
	}{&telnetIn{br: br, w: c}, c}
	fmt.Fprint(c, "\r\nUsername: ")
	u := readLine(rw.Reader)
	fmt.Fprint(c, "\r\nPassword: ")
	p := readLine(rw.Reader)
	if b, err := br.Peek(1); err == nil && b[0] == '\n' {
		_, _ = br.ReadByte() // the LF of the CR LF that ended the password
	}
	if u != o.User || p != o.Pass {
		fmt.Fprint(c, "\r\nLogin incorrect\r\n")
		return
	}
	fmt.Fprint(c, "\r\n")
	o.cli(rw, true)
}

// telnetIn strips IAC sequences from what the client sends.
type telnetIn struct {
	br *bufio.Reader
	w  io.Writer
}

func (t *telnetIn) Read(p []byte) (int, error) {
	for {
		b, err := t.br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 255 {
			p[0] = b
			return 1, nil
		}
		c, _ := t.br.ReadByte()
		switch c {
		case 251, 252, 253, 254:
			_, _ = t.br.ReadByte()
		case 250:
			for {
				x, err := t.br.ReadByte()
				if err != nil {
					return 0, err
				}
				if x == 255 {
					if y, _ := t.br.ReadByte(); y == 240 {
						break
					}
				}
			}
		case 255:
			p[0] = 255
			return 1, nil
		}
	}
}

func readLine(r io.Reader) string {
	var sb strings.Builder
	one := make([]byte, 1)
	for {
		if _, err := r.Read(one); err != nil {
			return sb.String()
		}
		switch one[0] {
		case '\r', '\n':
			if sb.Len() > 0 || one[0] == '\r' {
				return sb.String()
			}
		default:
			sb.WriteByte(one[0])
		}
	}
}

// cli is the command loop. Both transports speak the same.
func (o *OLT) cli(rw io.ReadWriter, telnet bool) {
	in := make(chan byte, 4096)
	go func() {
		one := make([]byte, 1)
		for {
			if _, err := rw.Read(one); err != nil {
				close(in)
				return
			}
			in <- one[0]
		}
	}()
	// SSH lands privileged on the bench; with an enable password set the fake starts at the user prompt instead.
	enabled, config := o.EnablePass == "", false
	var stack []string // contexts below config: interface gpon 0/1, vlan 98, profile line name x ...
	curBlock := func() *block {
		hdr := ""
		if len(stack) > 0 {
			hdr = stack[len(stack)-1]
		}
		for _, b := range o.blocks {
			if b.header == hdr {
				return b
			}
		}
		b := &block{header: hdr}
		o.blocks = append(o.blocks, b)
		return b
	}
	// ponOf is the PON port number of the current context (0 = not inside a PON interface).
	ponOf := func() int {
		if len(stack) == 0 {
			return 0
		}
		f := strings.Fields(stack[len(stack)-1])
		if len(f) == 3 && f[0] == "interface" && f[1] == "gpon" {
			var n int
			fmt.Sscanf(f[2], "0/%d", &n)
			return n
		}
		return 0
	}
	prompt := func() string {
		switch {
		case config && len(stack) > 0:
			f := strings.Fields(stack[len(stack)-1])
			name := f[0]
			switch {
			case ponOf() > 0:
				name = "pon-" + f[2]
			case f[0] == "profile":
				name = f[1] + "-profile"
			case len(f) > 1:
				name = f[0] + "-" + strings.ReplaceAll(f[1], "/", "-")
			}
			return o.Host + "(config-" + name + ")# "
		case config:
			return o.Host + "(config)# "
		case enabled:
			return o.Host + "# "
		}
		return o.Host + "> "
	}
	w := func(s string) { _, _ = io.WriteString(rw, strings.ReplaceAll(s, "\n", "\r\n")) }
	w(prompt())
	skipLF := false
	for {
		var sb strings.Builder
		for {
			b, ok := <-in
			if !ok {
				return
			}
			if b == '\n' && skipLF {
				skipLF = false
				continue
			}
			skipLF = false
			if b == '\r' || b == '\n' {
				skipLF = b == '\r'
				break
			}
			sb.WriteByte(b)
		}
		line := strings.TrimSpace(sb.String())
		w(line + "\n") // the OLT echoes what was typed
		o.mu.Lock()
		o.lines = append(o.lines, line)
		o.mu.Unlock()
		fields := strings.Fields(strings.ToLower(line))
		out := ""
		pon := ponOf()
		unknown := "% Unknown command.\n"
		switch {
		case line == "":
		case o.FailOn != "" && strings.Contains(line, o.FailOn):
			out = "% Invalid input detected at '^' marker.\n"
		case strings.Contains(line, "badcmd"):
			out = unknown
		case fields[0] == "enable":
			if config {
				out = unknown
				break
			}
			if o.EnablePass != "" {
				w("Password: ")
				p := ""
				for {
					b, ok := <-in
					if !ok {
						return
					}
					if b == '\n' && skipLF {
						skipLF = false
						continue
					}
					skipLF = false
					if b == '\r' || b == '\n' {
						skipLF = b == '\r'
						break
					}
					p += string(b)
				}
				w("\n")
				if p != o.EnablePass {
					out = "% Access denied\n"
					break
				}
			}
			enabled = true
		case fields[0] == "configure":
			if !enabled {
				out = unknown
			} else {
				config = true
			}
		case fields[0] == "end":
			config, stack = false, nil
		case fields[0] == "exit" || fields[0] == "quit":
			switch {
			case len(stack) > 0:
				stack = stack[:len(stack)-1]
			case config:
				config = false
			case enabled:
				enabled = false
			default:
				return
			}
		case line == "terminal length 0":
			if o.NoPagingCmd {
				out = unknown
			}
		case line == "show":
			out = "% Command incomplete: show\n"
		case line == "show version":
			if !config {
				out = unknown // bench: not valid at the # prompt
			} else {
				out = o.Version
			}
		case line == "show running-config":
			if !config {
				out = unknown
				break
			}
			out = o.running()
			if o.PageLines > 0 {
				lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
				for i := 0; i < len(lines); i += o.PageLines {
					j := i + o.PageLines
					if j > len(lines) {
						j = len(lines)
					}
					w(strings.Join(lines[i:j], "\n") + "\n")
					if j < len(lines) {
						w(" --More-- ")
						if _, ok := <-in; !ok {
							return
						}
						w("\b\b\b\b\b\b\b\b\b\b          \b\b\b\b\b\b\b\b\b\b")
					}
				}
				out = ""
			}
		case strings.HasPrefix(line, "show onu ") && pon == 0:
			out = unknown // the "show onu" tables exist inside a PON interface only
		case line == "show onu auto-find":
			var b strings.Builder
			o.mu.Lock()
			for _, f := range o.AutoFind {
				if f[0] == fmt.Sprint(pon) {
					fmt.Fprintf(&b, "GPON0/%s:--     %s    unknown\n", f[0], f[1])
				}
			}
			o.mu.Unlock()
			out = b.String() // nothing pending prints nothing
		case line == "show onu info":
			var b strings.Builder
			o.mu.Lock()
			for _, on := range o.onus(pon) {
				fmt.Fprintf(&b, "GPON0/%d:%d  V414                 %-22s sn      %s\n", pon, on.id, on.profile, on.serial)
			}
			o.mu.Unlock()
			out = b.String()
		case line == "show onu state":
			var b strings.Builder
			b.WriteString("OnuIndex   Admin State  OMCC State  Phase State\n")
			o.mu.Lock()
			for _, on := range o.onus(pon) {
				fmt.Fprintf(&b, "GPON0/%d:%d   enable       enable      working\n", pon, on.id)
			}
			o.mu.Unlock()
			out = b.String()
		case line == "show onu distance":
			var b strings.Builder
			o.mu.Lock()
			for _, on := range o.onus(pon) {
				fmt.Fprintf(&b, "GPON0/%d:%d   %d m\n", pon, on.id, 120+on.id)
			}
			o.mu.Unlock()
			out = b.String()
		case line == "show onu optical-info":
			if o.NoOptical {
				out = unknown
				break
			}
			var b strings.Builder
			o.mu.Lock()
			for _, on := range o.onus(pon) {
				fmt.Fprintf(&b, "ONU ID                     : %d\nAlarm                      : enable\nRx optical level(ONU)      : -8.94\n"+
					"Lower rx optical threshold : ont internal policy\nTx optical level           : 2.55 \nONU response time          : 0\n"+
					"Power feed voltage         : 3.30(V) \nLaser bias current         : 8.160(mA) \nTemperature                : 24.465(C) \n\n", on.id)
			}
			o.mu.Unlock()
			out = b.String()
		case line == "write" || line == "write memory":
			out = "Saving... OK\n"
		case config && (fields[0] == "interface" || fields[0] == "vlan" && len(fields) == 2 || fields[0] == "profile"):
			o.mu.Lock()
			hdr := line
			if fields[0] == "profile" {
				hdr = o.profileHeader(fields)
			}
			stack = append(stack, hdr)
			curBlock()
			o.mu.Unlock()
		case config && len(stack) > 0 && pon > 0:
			out = o.ponLine(pon, line, fields, curBlock())
		case config:
			o.mu.Lock()
			b := curBlock()
			dup := false
			for _, l := range b.lines {
				if l == line {
					dup = true
				}
			}
			if !dup {
				b.lines = append(b.lines, line)
			}
			o.mu.Unlock()
		default:
			out = unknown
		}
		w(out)
		w(prompt())
		_ = time.Now
	}
}

type fakeONU struct {
	id      int
	profile string
	serial  string
}

// onus lists the "onu add" lines of a PON interface (the caller holds the lock).
func (o *OLT) onus(pon int) []fakeONU {
	var res []fakeONU
	for _, bl := range o.blocks {
		if bl.header != fmt.Sprintf("interface gpon 0/%d", pon) {
			continue
		}
		for _, l := range bl.lines {
			f := strings.Fields(l)
			if len(f) >= 7 && f[0] == "onu" && f[1] == "add" {
				var id int
				fmt.Sscan(f[2], &id)
				res = append(res, fakeONU{id, f[4], f[6]})
			}
		}
	}
	return res
}

// profileHeader gives a profile the header the bench config shows: "profile dba id 10 name dba_1g".
func (o *OLT) profileHeader(f []string) string {
	kind, name := "", ""
	if len(f) > 1 {
		kind = f[1]
	}
	for i, x := range f {
		if x == "name" && i+1 < len(f) {
			name = f[i+1]
		}
	}
	for _, b := range o.blocks {
		bf := strings.Fields(b.header)
		if len(bf) >= 5 && bf[0] == "profile" && bf[1] == kind && bf[len(bf)-1] == name {
			return b.header
		}
	}
	n := 1
	for _, b := range o.blocks {
		if bf := strings.Fields(b.header); len(bf) > 1 && bf[0] == "profile" && bf[1] == kind {
			n++
		}
	}
	return fmt.Sprintf("profile %s id %d name %s", kind, n, name)
}

// hasProfile reports whether a profile of that kind and name exists (the caller holds the lock).
func (o *OLT) hasProfile(kind, name string) bool {
	for _, b := range o.blocks {
		bf := strings.Fields(b.header)
		if len(bf) >= 5 && bf[0] == "profile" && bf[1] == kind && bf[len(bf)-1] == name {
			return true
		}
	}
	return false
}

// ponLine handles a configuration line inside a PON interface.
func (o *OLT) ponLine(pon int, line string, f []string, b *block) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	add := func(l string) {
		for _, x := range b.lines {
			if x == l {
				return
			}
		}
		b.lines = append(b.lines, l)
	}
	switch {
	case line == "no onu auto-learn":
		var keep []string
		for _, l := range b.lines {
			if l != "onu auto-learn" {
				keep = append(keep, l)
			}
		}
		b.lines = keep
	case len(f) >= 7 && f[0] == "onu" && f[1] == "add" && f[5] == "sn":
		for _, on := range o.onus(pon) {
			if fmt.Sprint(on.id) == f[2] || strings.EqualFold(on.serial, f[6]) {
				return "% ONU already exist.\n"
			}
		}
		orig := strings.Fields(line)
		sn := orig[6]
		if len(sn) > 4 { // the bench OLT prints the hex digits in lower case
			sn = sn[:4] + strings.ToLower(sn[4:])
		}
		add(fmt.Sprintf("onu add %s profile %s sn %s", f[2], orig[4], sn))
	case len(f) == 3 && f[0] == "no" && f[1] == "onu":
		var keep []string
		for _, l := range b.lines {
			lf := strings.Fields(l)
			if len(lf) > 1 && lf[0] == "onu" && (lf[1] == f[2] || lf[1] == "add" && len(lf) > 2 && lf[2] == f[2]) {
				continue
			}
			keep = append(keep, l)
		}
		b.lines = keep
	case len(f) == 6 && f[0] == "onu" && f[2] == "profile" && f[4] == "name":
		if !o.hasProfile(f[3], f[5]) {
			return "% Profile not exist.\n"
		}
		add(line)
	case line == "plug_and_play disable":
		var keep []string
		for _, l := range b.lines {
			if !strings.HasPrefix(l, "plug_and_play") {
				keep = append(keep, l)
			}
		}
		b.lines = keep
	default:
		add(line)
	}
	return ""
}
