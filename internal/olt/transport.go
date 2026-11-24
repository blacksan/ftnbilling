package olt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target is where and how to log in. Addr is the address the server dials (for a real OLT: the router's tunnel
// address and the forwarded port, see internal/sites/oltmgmt.go).
type Target struct {
	Addr      string
	Proto     string // ssh or telnet
	User      string
	Pass      string
	Enable    string
	PinnedKey string // SHA256:... fingerprint; "" = pin on first use
	Protect   int    // the management VLAN, never removed
}

// DialFunc opens the TCP connection (tests dial a fake OLT directly).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Open connects, logs in and returns a session at the privileged prompt's neighbourhood (user or privileged mode).
func Open(ctx context.Context, d *Dialect, t Target, dial DialFunc) (*Session, error) {
	if dial == nil {
		nd := net.Dialer{Timeout: 10 * time.Second}
		dial = nd.DialContext
	}
	conn, err := dial(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, fmt.Errorf("olt: cannot reach the OLT: %w", err)
	}
	var s *Session
	switch strings.ToLower(t.Proto) {
	case "telnet":
		s = newSession(d, newTelnet(conn), "\r\n")
	default:
		s, err = openSSH(ctx, d, t, conn)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	s.Protect = t.Protect
	if err := s.login(ctx, t.User, t.Pass, t.Enable); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func openSSH(ctx context.Context, d *Dialect, t Target, conn net.Conn) (*Session, error) {
	sup, ins := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	var newKey string
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(t.Pass),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				a := make([]string, len(qs))
				for i := range a {
					a[i] = t.Pass
				}
				return a, nil
			}),
		},
		Timeout: 15 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if t.PinnedKey != "" && t.PinnedKey != fp {
				return ErrHostKey
			}
			if t.PinnedKey == "" {
				newKey = fp
			}
			return nil
		},
	}
	// An OLT's SSH server is often old. The modern algorithms come first; the legacy ones are only a fallback, on a
	// private path (the tunnel and the OLT management VLAN).
	cfg.Config.KeyExchanges = append(sup.KeyExchanges, ins.KeyExchanges...)
	cfg.Config.Ciphers = append(sup.Ciphers, ins.Ciphers...)
	cfg.Config.MACs = append(sup.MACs, ins.MACs...)
	cfg.HostKeyAlgorithms = append(sup.HostKeys, ins.HostKeys...)

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, cfg)
	if err != nil {
		if errors.Is(err, ErrHostKey) || strings.Contains(err.Error(), ErrHostKey.Error()) {
			return nil, ErrHostKey
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, ErrLoginFailed
		}
		return nil, fmt.Errorf("olt: SSH: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	cl := ssh.NewClient(c, chans, reqs)
	sess, err := cl.NewSession()
	if err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("olt: SSH session: %w", err)
	}
	if err := sess.RequestPty("vt100", 200, 200, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("olt: SSH terminal: %w", err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		_ = cl.Close()
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		_ = cl.Close()
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		_ = cl.Close()
		return nil, fmt.Errorf("olt: SSH shell: %w", err)
	}
	rw := &sshRW{r: out, w: in, closeFn: func() { _ = sess.Close(); _ = cl.Close() }}
	s := newSession(d, rw, "\r")
	s.NewHostKey = newKey
	return s, nil
}

type sshRW struct {
	r       io.Reader
	w       io.WriteCloser
	closeFn func()
}

func (x *sshRW) Read(p []byte) (int, error)  { return x.r.Read(p) }
func (x *sshRW) Write(p []byte) (int, error) { return x.w.Write(p) }
func (x *sshRW) Close() error                { x.closeFn(); return nil }

// ---- Telnet ----------------------------------------------------------------------------------------------------------

// A minimal Telnet client: it strips option negotiation from the stream, agrees to the server echoing and to
// suppress-go-ahead, and refuses every other option.
const (
	tIAC  = 255
	tDONT = 254
	tDO   = 253
	tWONT = 252
	tWILL = 251
	tSB   = 250
	tSE   = 240

	optEcho = 1
	optSGA  = 3
)

type telnetConn struct {
	c     net.Conn
	mu    sync.Mutex // writes (the reader answers negotiation)
	state int
	cmd   byte
}

func newTelnet(c net.Conn) *telnetConn { return &telnetConn{c: c} }

func (t *telnetConn) Read(p []byte) (int, error) {
	buf := make([]byte, len(p))
	for {
		n, err := t.c.Read(buf)
		out := 0
		for _, b := range buf[:n] {
			switch t.state {
			case 0:
				if b == tIAC {
					t.state = 1
				} else {
					p[out] = b
					out++
				}
			case 1: // after IAC
				switch b {
				case tIAC:
					p[out] = tIAC
					out++
					t.state = 0
				case tDO, tDONT, tWILL, tWONT:
					t.cmd, t.state = b, 2
				case tSB:
					t.state = 3
				default:
					t.state = 0
				}
			case 2: // option byte
				t.answer(t.cmd, b)
				t.state = 0
			case 3: // inside a sub-negotiation
				if b == tIAC {
					t.state = 4
				}
			case 4:
				if b == tSE {
					t.state = 0
				} else {
					t.state = 3
				}
			}
		}
		if out > 0 || err != nil {
			return out, err
		}
	}
}

func (t *telnetConn) answer(cmd, opt byte) {
	var reply byte
	switch cmd {
	case tWILL:
		reply = tDONT
		if opt == optEcho || opt == optSGA {
			reply = tDO
		}
	case tDO:
		reply = tWONT
		if opt == optSGA {
			reply = tWILL
		}
	default:
		return // DONT and WONT need no answer
	}
	t.mu.Lock()
	_, _ = t.c.Write([]byte{tIAC, reply, opt})
	t.mu.Unlock()
}

func (t *telnetConn) Write(p []byte) (int, error) {
	esc := make([]byte, 0, len(p))
	for _, b := range p {
		esc = append(esc, b)
		if b == tIAC {
			esc = append(esc, tIAC)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.c.Write(esc); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *telnetConn) Close() error { return t.c.Close() }
