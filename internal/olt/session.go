package olt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Errors of the driver.
var (
	ErrDenied      = errors.New("olt: command refused by the denylist")
	ErrHostKey     = errors.New("olt: the SSH host key is not the one pinned for this OLT")
	ErrTimeout     = errors.New("olt: the OLT did not answer in time")
	ErrNoPrompt    = errors.New("olt: no CLI prompt")
	ErrLoginFailed = errors.New("olt: the OLT refused the login")
	ErrEnable      = errors.New("olt: the OLT refused the enable password")
)

// DeniedError says which line the denylist refused and why.
type DeniedError struct{ Line, Why string }

func (e *DeniedError) Error() string {
	return "olt: refused by the denylist (" + e.Why + "): " + Redact(e.Line)
}
func (e *DeniedError) Is(t error) bool { return t == ErrDenied }

// CmdError is a command the OLT answered with an error: the batch stopped there.
type CmdError struct {
	Cmd    string
	Output string
}

func (e *CmdError) Error() string {
	return "the OLT refused: " + Redact(e.Cmd) + ": " + Redact(firstLines(e.Output, 3))
}

func firstLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[:n]
	}
	return strings.Join(l, " / ")
}

// Entry is one line of a transcript: what was sent and what came back, both redacted.
type Entry struct {
	Cmd string
	Out string
	Err string
}

// Session is one logged-in CLI session with an OLT. It is not safe for concurrent use: one command at a time.
type Session struct {
	d    *Dialect
	rw   io.ReadWriteCloser
	eol  string
	in   chan []byte
	rerr error
	mu   sync.Mutex
	raw  []byte
	// answered is len(raw) when the last pagination marker was answered.
	answered int
	mode     Mode
	prompt   string
	enable   string
	Timeout  time.Duration // per command (default 20 s)
	// Protect is the management VLAN: nothing may be removed under it.
	Protect int
	inMgmt  bool

	Transcript []Entry
	// NewHostKey is the fingerprint seen when none was pinned (the caller pins it).
	NewHostKey string
	closer     func()
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]|\x1b[=>]`)

// clean applies backspaces, drops CR, NUL and terminal escapes.
func clean(b []byte) string {
	b = ansiRE.ReplaceAll(b, nil)
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch c {
		case '\r', 0:
		case 8, 127:
			if n := len(out); n > 0 && out[n-1] != '\n' {
				out = out[:n-1]
			}
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

func newSession(d *Dialect, rw io.ReadWriteCloser, eol string) *Session {
	s := &Session{d: d, rw: rw, eol: eol, in: make(chan []byte, 64), Timeout: 20 * time.Second, mode: ModeUser}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				c := make([]byte, n)
				copy(c, buf[:n])
				s.in <- c
			}
			if err != nil {
				s.mu.Lock()
				s.rerr = err
				s.mu.Unlock()
				close(s.in)
				return
			}
		}
	}()
	return s
}

// Close ends the session.
func (s *Session) Close() {
	_ = s.rw.Close()
	if s.closer != nil {
		s.closer()
	}
}

func (s *Session) write(line string) error {
	_, err := io.WriteString(s.rw, line+s.eol)
	return err
}

// Reading ---------------------------------------------------------------------------------------------------------

type stopAt int

const (
	stopPrompt stopAt = iota
	stopLogin         // also stop at a user or password prompt
)

// readUntil reads until the prompt (or, with stopLogin, a login prompt), answers pagination, and returns the cleaned
// text before the final line and the final line itself.
func (s *Session) readUntil(ctx context.Context, timeout time.Duration, st stopAt) (out, last string, err error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		text := clean(s.raw)
		lastNL := strings.LastIndexByte(text, '\n')
		line := text[lastNL+1:]
		tl := strings.TrimRight(line, " \t")
		if tl != "" {
			switch {
			case s.d.MoreRE.MatchString(line) && len(s.raw) > s.answered:
				s.answered = len(s.raw)
				if err := s.write(" "); err != nil { // a space turns the page; no end of line
					return "", "", err
				}
				goto wait
			case s.d.PromptRE.MatchString(tl):
				return s.tidy(text[:lastNL+1]), tl, nil
			case st == stopLogin && (s.d.UserRE.MatchString(tl) || s.d.PasswordRE.MatchString(tl)):
				return s.tidy(text[:lastNL+1]), tl, nil
			}
		}
	wait:
		select {
		case c, ok := <-s.in:
			if !ok {
				s.mu.Lock()
				e := s.rerr
				s.mu.Unlock()
				if e == nil || errors.Is(e, io.EOF) {
					e = errors.New("olt: the connection closed")
				}
				return clean(s.raw), "", e
			}
			s.raw = append(s.raw, c...)
		case <-deadline.C:
			return clean(s.raw), "", ErrTimeout
		case <-ctx.Done():
			return clean(s.raw), "", ctx.Err()
		}
	}
}

// tidy removes pagination markers that were left in the text.
func (s *Session) tidy(t string) string {
	t = s.d.MoreRE.ReplaceAllString(t, "")
	return t
}

func (s *Session) setPrompt(p string) {
	s.prompt = p
	switch {
	case strings.HasSuffix(p, ">"):
		s.mode = ModeUser
	case strings.Contains(p, "("):
		s.mode = ModeConfig
	default:
		s.mode = ModePriv
	}
}

// Login: reaching the first prompt -------------------------------------------------------------------------------------

func (s *Session) login(ctx context.Context, user, pass, enable string) error {
	s.enable = enable
	s.raw, s.answered = nil, 0
	// The OLT may greet first or wait for a Return.
	deadline := time.Now().Add(15 * time.Second)
	nudged, sentUser, sentPass := false, false, false
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return ErrNoPrompt
		}
		sub := wait
		if !nudged && sub > 3*time.Second {
			sub = 3 * time.Second
		}
		_, last, err := s.readUntil(ctx, sub, stopLogin)
		switch {
		case err == nil && s.d.PromptRE.MatchString(last):
			s.setPrompt(last)
			s.settle(ctx)
			return nil
		case err == nil && s.d.UserRE.MatchString(last):
			if sentUser {
				return ErrLoginFailed
			}
			sentUser = true
			s.raw = nil
			if err := s.write(user); err != nil {
				return err
			}
		case err == nil && s.d.PasswordRE.MatchString(last):
			if sentPass {
				return ErrLoginFailed
			}
			sentPass = true
			s.raw = nil
			if err := s.write(pass); err != nil {
				return err
			}
		case errors.Is(err, ErrTimeout) && !nudged:
			nudged = true
			if err := s.write(""); err != nil {
				return err
			}
		default:
			if err == nil {
				err = ErrNoPrompt
			}
			if sentPass {
				return ErrLoginFailed
			}
			return err
		}
	}
}

// settle swallows what the OLT sends after the login prompt (a second prompt, a banner), so that the first command's
// answer is not taken from stale text.
func (s *Session) settle(ctx context.Context) {
	for {
		select {
		case c, ok := <-s.in:
			if !ok {
				return
			}
			s.raw = append(s.raw, c...)
		case <-time.After(150 * time.Millisecond):
			text := clean(s.raw)
			if i := strings.LastIndexByte(strings.TrimRight(text, " "), '\n'); i >= 0 || text != "" {
				l := strings.TrimRight(text[strings.LastIndexByte(text, '\n')+1:], " \t")
				if s.d.PromptRE.MatchString(l) {
					s.setPrompt(l)
				}
			}
			s.raw, s.answered = nil, 0
			return
		case <-ctx.Done():
			return
		}
	}
}

// Modes -------------------------------------------------------------------------------------------------------------

// raise moves the session to at least want (User -> Priv -> Config).
func (s *Session) raise(ctx context.Context, want Mode) error {
	for s.mode < want {
		switch s.mode {
		case ModeUser:
			if err := s.enableMode(ctx); err != nil {
				return err
			}
		case ModePriv:
			if _, err := s.exec(ctx, s.d.ConfigCmd, true); err != nil {
				return err
			}
			if s.mode != ModeConfig {
				return errors.New("olt: could not enter configuration mode")
			}
		}
	}
	return nil
}

func (s *Session) enableMode(ctx context.Context) error {
	if err := s.write(s.d.EnableCmd); err != nil {
		return err
	}
	s.raw, s.answered = nil, 0
	for tries := 0; tries < 2; tries++ {
		_, last, err := s.readUntil(ctx, s.Timeout, stopLogin)
		if err != nil {
			return err
		}
		if s.d.PasswordRE.MatchString(last) {
			s.raw = nil
			if err := s.write(s.enable); err != nil { // an empty line when none is set
				return err
			}
			continue
		}
		s.setPrompt(last)
		if s.mode == ModeUser {
			return ErrEnable
		}
		return nil
	}
	return ErrEnable
}

// lower leaves configuration mode.
func (s *Session) lower(ctx context.Context) {
	for i := 0; i < 3 && s.mode == ModeConfig; i++ {
		if _, err := s.exec(ctx, s.d.EndCmd, true); err != nil {
			return
		}
	}
	s.inMgmt = false
}

// Commands -------------------------------------------------------------------------------------------------------------

// check applies the denylist and the management-path protection to a line about to be sent.
func (s *Session) check(line string) error { return s.checkIn(line, s.inMgmt) }

func (s *Session) checkIn(line string, inMgmt bool) error {
	n := Norm(line)
	for _, r := range s.d.Deny {
		if r.RE.MatchString(n) {
			return &DeniedError{Line: line, Why: r.Why}
		}
	}
	if s.Protect > 0 {
		f := strings.Fields(n)
		if len(f) == 3 && f[0] == "no" && f[1] == "vlan" && f[2] == fmt.Sprint(s.Protect) {
			return &DeniedError{Line: line, Why: "removal of the management VLAN"}
		}
		if inMgmt && len(f) > 0 && f[0] == "no" {
			return &DeniedError{Line: line, Why: "a removal inside the management interface"}
		}
	}
	return nil
}

func (s *Session) track(line string) { s.inMgmt = s.trackIn(line, s.inMgmt) }

// trackIn is the "inside the management interface" state after a line.
func (s *Session) trackIn(line string, inMgmt bool) bool {
	f := strings.Fields(Norm(line))
	if len(f) == 0 {
		return inMgmt
	}
	switch {
	case len(f) >= 3 && f[0] == "interface" && f[1] == "vlan":
		return s.Protect > 0 && f[2] == fmt.Sprint(s.Protect)
	case f[0] == "interface", f[0] == "exit", f[0] == "end", f[0] == "vlan", f[0] == "profile":
		return false
	}
	return inMgmt
}

// exec sends one line (already checked unless trusted) and returns what the OLT printed before its next prompt.
func (s *Session) exec(ctx context.Context, line string, trusted bool) (string, error) {
	if !trusted {
		if err := s.check(line); err != nil {
			return "", err
		}
	}
	s.raw, s.answered = nil, 0
	if err := s.write(line); err != nil {
		return "", err
	}
	out, last, err := s.readUntil(ctx, s.Timeout, stopPrompt)
	if err != nil {
		s.Transcript = append(s.Transcript, Entry{Cmd: Redact(line), Err: err.Error()})
		return out, err
	}
	s.setPrompt(last)
	s.track(line)
	// The first line is the OLT echoing what was typed.
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		if Norm(out[:i]) == Norm(line) || strings.HasSuffix(Norm(out[:i]), Norm(line)) {
			out = out[i+1:]
		}
	} else if Norm(out) == Norm(line) {
		out = ""
	}
	out = strings.TrimSpace(out)
	s.Transcript = append(s.Transcript, Entry{Cmd: Redact(line), Out: Redact(clip(out, 600))})
	return out, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// failed reports the first output line the dialect calls an error.
func failed(re *regexp.Regexp, out string) bool {
	for _, l := range strings.Split(out, "\n") {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// Show runs one read-only command at the privileged prompt and returns its output.
func (s *Session) Show(ctx context.Context, cmd string) (string, error) {
	if err := s.raise(ctx, ModePriv); err != nil {
		return "", err
	}
	return s.show(ctx, cmd)
}

func (s *Session) show(ctx context.Context, cmd string) (string, error) {
	saved := s.Timeout
	if strings.Contains(cmd, "running") || strings.Contains(cmd, "auto-find") {
		s.Timeout = 90 * time.Second
	}
	out, err := s.exec(ctx, cmd, false)
	s.Timeout = saved
	if err != nil {
		return out, err
	}
	if failed(s.d.QueryErrRE, out) {
		return out, &CmdError{Cmd: cmd, Output: out}
	}
	return out, nil
}

// Query runs a Query (a read-only sequence that may sit in configuration mode) and returns the output of its show
// commands. It always leaves configuration mode afterwards.
func (s *Session) Query(ctx context.Context, q Query) (string, error) {
	if err := s.raise(ctx, q.Mode); err != nil {
		return "", err
	}
	var b bytes.Buffer
	var firstErr error
	for _, c := range q.Cmds {
		out, err := s.show(ctx, c)
		if err != nil {
			firstErr = err
			break
		}
		if strings.HasPrefix(Norm(c), "show") {
			b.WriteString(out + "\n")
		}
	}
	s.lower(ctx)
	return b.String(), firstErr
}

// DisablePaging tries the dialect's no-paging command once; a failure is ignored.
func (s *Session) DisablePaging(ctx context.Context) {
	if s.d.NoPaging == "" {
		return
	}
	if err := s.raise(ctx, ModePriv); err != nil {
		return
	}
	_, _ = s.exec(ctx, s.d.NoPaging, false)
}

// Config runs configuration lines in order. The first line the OLT refuses (or the denylist stops) ends the batch: the
// returned error is a *CmdError or *DeniedError naming it, and results holds every line sent so far. Nothing is saved
// here: call Save when the batch worked.
func (s *Session) Config(ctx context.Context, lines []string) error {
	// Check every line before any is sent: a denied line must stop the batch before it changes anything.
	in := s.inMgmt
	for _, l := range lines {
		if err := s.checkIn(l, in); err != nil {
			return err
		}
		in = s.trackIn(l, in)
	}
	if err := s.raise(ctx, ModeConfig); err != nil {
		return err
	}
	for _, l := range lines {
		out, err := s.exec(ctx, l, false)
		if err != nil {
			s.lower(ctx)
			return err
		}
		if failed(s.d.ErrorRE, out) {
			s.Transcript[len(s.Transcript)-1].Err = "refused"
			s.lower(ctx)
			return &CmdError{Cmd: l, Output: out}
		}
	}
	s.lower(ctx)
	return nil
}

// Save writes the running configuration to the OLT's flash.
func (s *Session) Save(ctx context.Context) error {
	if err := s.raise(ctx, ModePriv); err != nil {
		return err
	}
	out, err := s.exec(ctx, s.d.SaveCmd, false)
	if err != nil {
		return err
	}
	if failed(s.d.ErrorRE, out) {
		return &CmdError{Cmd: s.d.SaveCmd, Output: out}
	}
	return nil
}

// Redact hides what looks like a secret in text that is shown or stored.
func Redact(s string) string { return redactRE.ReplaceAllString(s, "${1}<redacted>") }

var redactRE = regexp.MustCompile(`(?im)\b((?:enable\s+)?(?:password|passwd|secret|community|auth-key|priv-key|key-string|md5|sha|encrypted|psk)\b\s*(?:\d\s+)?(?:=\s*|:\s*)?)\S+`)
