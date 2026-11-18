package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP sends email: STARTTLS on port 587 (or whatever the server offers), or
// implicit TLS on port 465.
type SMTP struct {
	Host, User, Pass, From string
	Port                   int
	Implicit               bool        // TLS from the first byte (always true on port 465)
	TLS                    *tls.Config // tests only; nil uses the system roots
	Timeout                time.Duration
}

func (s *SMTP) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 20 * time.Second
}

// Message builds the RFC 5322 text of an email.
func (s *SMTP) message(m Message, to []string) []byte {
	enc := func(v string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(v) }
	var b strings.Builder
	b.WriteString("From: " + enc(s.From) + "\r\n")
	b.WriteString("To: " + enc(strings.Join(to, ", ")) + "\r\n")
	b.WriteString("Subject: " + mimeQ(enc(m.Subject)) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: <" + m.ID + "@ispbill>\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	body := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")
	b.WriteString(body + "\r\n")
	return []byte(b.String())
}

func mimeQ(s string) string {
	for _, r := range s {
		if r > 126 {
			return "=?utf-8?q?" + strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"), "?", "=3F") + "?="
		}
	}
	return s
}

// Send delivers m to m.To (comma-separated addresses).
func (s *SMTP) Send(ctx context.Context, m Message) (string, error) {
	var to []string
	for _, a := range strings.FieldsFunc(m.To, func(r rune) bool { return r == ',' || r == ';' }) {
		addr, err := mail.ParseAddress(strings.TrimSpace(a))
		if err != nil {
			return "", Permanent("bad recipient %q", a)
		}
		to = append(to, addr.Address)
	}
	if len(to) == 0 {
		return "", Permanent("no recipient")
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return "", Permanent("SMTP_FROM is not an address: %v", err)
	}
	port := s.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(port))
	d := net.Dialer{Timeout: s.timeout()}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("smtp connect: %w", err)
	}
	tcfg := s.TLS
	if tcfg == nil {
		tcfg = &tls.Config{}
	}
	tcfg = tcfg.Clone()
	if tcfg.ServerName == "" {
		tcfg.ServerName = s.Host
	}
	implicit := s.Implicit || port == 465
	if implicit {
		conn = tls.Client(conn, tcfg)
	}
	// The whole exchange ends by the caller's deadline when it has one (the
	// web server's write timeout is 30 s), and never later than twice the
	// connection timeout. A cancelled context closes the connection, which
	// also ends a TLS handshake or a read that is waiting.
	dl := time.Now().Add(2 * s.timeout())
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	_ = conn.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return "", classifySMTP(err)
	}
	defer c.Close()
	if !implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tcfg); err != nil {
				return "", fmt.Errorf("smtp starttls: %w", err)
			}
		} else if s.User != "" && !isLoopback(s.Host) {
			return "", Permanent("the SMTP server does not offer STARTTLS; refusing to send the password in clear text")
		}
	}
	if s.User != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(plainAuth{user: s.User, pass: s.Pass}); err != nil {
				return "", classifySMTP(err)
			}
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return "", classifySMTP(err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return "", classifySMTP(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return "", classifySMTP(err)
	}
	if _, err := w.Write(s.message(m, to)); err != nil {
		return "", classifySMTP(err)
	}
	if err := w.Close(); err != nil {
		return "", classifySMTP(err)
	}
	_ = c.Quit()
	return "", nil
}

func isLoopback(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// plainAuth is AUTH PLAIN without net/smtp's host check (we check TLS ourselves).
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// classifySMTP: 535/534/530 are credential problems, other 5xx are permanent,
// 4xx and network errors are retried.
func classifySMTP(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) {
		switch {
		case te.Code == 535 || te.Code == 534 || te.Code == 530:
			return AuthError{fmt.Errorf("smtp: %w", err)}
		case te.Code >= 500:
			return PermanentError{fmt.Errorf("smtp: %w", err)}
		}
	}
	return fmt.Errorf("smtp: %w", err)
}
