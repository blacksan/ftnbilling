// Package fake is a small fake V-SOL OLT command line, served over SSH and Telnet, for tests. It emulates prompts,
// enable, configuration modes, pagination, an error line, the auto-find list and "onu add", and it records every line
// it receives. Its output formats are the dialect's best guess, not the real OLT's.
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
	Version                string
	// AutoFind is what "show onu auto-find" lists: {pon, serial}.
	AutoFind [][2]string
	// PageLines paginates "show running-config" with --More-- every n lines (0 = off).
	PageLines int
	// NoPagingCmd makes "terminal length 0" an error (the usual case on an unknown firmware).
	NoPagingCmd bool
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

type block struct {
	header string
	lines  []string
}

// New starts the fake on loopback ports.
func New() (*OLT, error) {
	o := &OLT{User: "admin", Pass: "olt-pass", Host: "gpon-olt", Version: "V-SOL V1600GS-R fake 1.0", Secret: "s3cr3t-hash"}
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

// cli is the command loop. telnet only changes nothing here; both transports speak the same.
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
	enabled, config := false, false
	var stack []string // contexts below config: interface gpon 0/1, vlan 98, profile ...
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
	prompt := func() string {
		switch {
		case config && len(stack) > 0:
			return o.Host + "(config-" + strings.ReplaceAll(strings.Fields(stack[len(stack)-1])[0], "/", "-") + ")# "
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
		switch {
		case line == "":
		case o.FailOn != "" && strings.Contains(line, o.FailOn):
			out = "% Invalid input detected at '^' marker.\n"
		case strings.Contains(line, "badcmd"):
			out = "% Unknown command.\n"
		case fields[0] == "enable":
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
				out = "% Unknown command.\n"
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
				out = "% Unknown command.\n"
			}
		case line == "show version":
			out = o.Version + "\nuptime 1 day\n"
		case line == "show running-config":
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
		case line == "show onu auto-find":
			var b strings.Builder
			b.WriteString("OnuIndex        Sn              State\n-----------------------------------\n")
			o.mu.Lock()
			for _, f := range o.AutoFind {
				fmt.Fprintf(&b, "GPON0/%s:--     %s    unknown\n", f[0], f[1])
			}
			o.mu.Unlock()
			out = b.String()
		case line == "show onu state":
			var b strings.Builder
			b.WriteString("OnuIndex   Admin State  OMCC State  Phase State\n")
			pon := ""
			if len(stack) > 0 {
				pon = strings.TrimPrefix(strings.Fields(stack[len(stack)-1])[len(strings.Fields(stack[len(stack)-1]))-1], "0/")
			}
			o.mu.Lock()
			for _, bl := range o.blocks {
				if bl.header == "interface gpon 0/"+pon {
					for _, l := range bl.lines {
						f := strings.Fields(l)
						if len(f) >= 7 && f[0] == "onu" && f[1] == "add" {
							fmt.Fprintf(&b, "GPON0/%s:%s   enable       enable      working\n", pon, f[2])
						}
					}
				}
			}
			o.mu.Unlock()
			out = b.String()
		case line == "show onu optical-info":
			out = "% Unknown command.\n"
		case line == "write" || line == "write memory":
			out = "Saving... OK\n"
		case config && (fields[0] == "interface" || fields[0] == "vlan" || fields[0] == "profile"):
			o.mu.Lock()
			stack = append(stack, line)
			curBlock()
			if fields[0] == "vlan" || (fields[0] == "interface" && len(fields) > 1 && fields[1] == "vlan") {
				// a vlan line is a one-line block when nothing is put under it
			}
			o.mu.Unlock()
		case config:
			o.mu.Lock()
			b := curBlock()
			dup := false
			for _, l := range b.lines {
				if l == line {
					dup = true
				}
			}
			if !dup && fields[0] != "commit" {
				b.lines = append(b.lines, line)
			}
			o.mu.Unlock()
		default:
			out = "% Unknown command.\n"
		}
		w(out)
		w(prompt())
		_ = time.Now
	}
}
