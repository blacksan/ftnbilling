package olt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
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
	// asked is len(raw) when readUntil last stopped at a question (the same question is not returned twice).
	asked int
	// SaveTimeout overrides Dialect.SaveTimeout (tests).
	SaveTimeout time.Duration
	mode        Mode
	prompt      string
	enable      string
	Timeout     time.Duration // per command (default 20 s)
	// Protect is the management VLAN: nothing may be removed under it.
	Protect int
	cs      cstate // where the lines sent so far have left the CLI (inside the management interface, inside a sub-mode)

	Transcript []Entry
	// NewHostKey is the fingerprint seen when none was pinned (the caller pins it).
	NewHostKey string
	closer     func()
	// lastDbg is the last command's raw output with its control characters made visible (diagnostics only).
	lastDbg string
}

// clean replays terminal output (see render): cursor movement, carriage returns, backspaces and escapes.
func clean(b []byte) string { return render(b) }

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
	// Every write is one line, including the login name and the passwords: a line break inside one would be a second
	// command that no check has seen.
	if strings.ContainsAny(line, "\r\n\x00") {
		return errors.New("olt: refusing to send a line with a line break inside it")
	}
	_, err := io.WriteString(s.rw, line+s.eol)
	return err
}

// Reading ---------------------------------------------------------------------------------------------------------

type stopAt int

const (
	stopPrompt   stopAt = iota
	stopLogin           // also stop at a user or password prompt
	stopQuestion        // also stop at a yes/no question (Dialect.QuestionRE)
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
				blankMore(s.raw, s.d.MoreRE)
				s.answered = len(s.raw)
				if err := s.write(" "); err != nil { // a space turns the page; no end of line
					return "", "", err
				}
				goto wait
			case s.d.PromptRE.MatchString(tl):
				return s.tidy(text[:lastNL+1]), tl, nil
			case st == stopLogin && (s.d.UserRE.MatchString(tl) || s.d.PasswordRE.MatchString(tl)):
				return s.tidy(text[:lastNL+1]), tl, nil
			case st == stopQuestion && s.d.QuestionRE != nil && s.d.QuestionRE.MatchString(tl) && len(s.raw) > s.asked:
				s.asked = len(s.raw)
				return s.tidy(text[:lastNL+1]), tl, nil
			case st == stopQuestion && s.prompt != "" && len(tl) > len(strings.TrimRight(s.prompt, " \t")) && strings.HasSuffix(tl, strings.TrimRight(s.prompt, " \t")):
				// The save's own prompt drawn on the same line as its last output ("[OK]gpon-olt(config)#").
				p := strings.TrimRight(s.prompt, " \t")
				return s.tidy(text[:lastNL+1] + tl[:len(tl)-len(p)] + "\n"), p, nil
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

// blankMore overwrites the pagination marker on the last line of raw with blanks of the same length, so the marker
// cannot survive in the rendered text when the OLT redraws the line with a carriage return and a shorter line (the
// marker's tail would show through) and every column position stays as the OLT counted it.
func blankMore(raw []byte, re *regexp.Regexp) {
	from := bytes.LastIndexByte(raw, '\n') + 1
	locs := re.FindAllIndex(raw[from:], -1)
	if len(locs) == 0 {
		return
	}
	l := locs[len(locs)-1]
	for k := from + l[0]; k < from+l[1]; k++ {
		raw[k] = ' '
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
	s.cs = cstate{}
}

// Commands -------------------------------------------------------------------------------------------------------------

// cstate is what the denylist needs to know about where a line will run: inside the management interface, inside a
// sub-mode (an interface, a VLAN or a profile; a few words mean something else there), and in configuration mode at all.
type cstate struct{ mgmt, sub, config bool }

// check applies the denylist and the management-path protection to a line about to be sent.
func (s *Session) check(line string) error {
	cs := s.cs
	cs.config = s.mode == ModeConfig
	// The sub-mode exceptions ("service" inside a line profile) also need the OLT's own prompt to say it is in a
	// sub-mode: a line that left the sub-mode by itself (or a prompt the tracking did not expect) must not let a top-level
	// word through.
	cs.sub = cs.sub && promptInSubMode(s.prompt)
	return s.checkIn(line, cs)
}

// promptInSubMode reports whether a configuration prompt names a sub-mode: "gpon-olt(config-pon-0/1)#", not
// "gpon-olt(config)#".
func promptInSubMode(p string) bool {
	i, j := strings.IndexByte(p, '('), strings.LastIndexByte(p, ')')
	if i < 0 || j <= i {
		return false
	}
	in := strings.ToLower(p[i+1 : j])
	return in != "config" && in != "cfg"
}

func (s *Session) checkIn(line string, cs cstate) error {
	// One line is one command: a line break (or any other control character) inside it would let a value smuggle a
	// second command past the checks below.
	for _, c := range line {
		if c < 0x20 || c == 0x7f {
			return &DeniedError{Line: line, Why: "a control character or line break inside the line"}
		}
	}
	n := Norm(line)
	f := strings.Fields(n)
	// "do <command>" runs a privileged command from configuration mode: check the command itself.
	for len(f) > 1 && f[0] == "do" {
		f = f[1:]
	}
	n = strings.Join(f, " ")
	for _, r := range s.d.Deny {
		if r.RE.MatchString(n) {
			return &DeniedError{Line: line, Why: r.Why}
		}
	}
	if len(f) == 0 {
		return nil
	}
	allowed := false
	for _, re := range s.d.AllowLines {
		if re.MatchString(n) {
			allowed = true
		}
	}
	if !allowed {
		skipVerb := cs.sub && inList(f[0], s.d.SubVerbsOK)
		if !skipVerb && prefixOfAny(f[0], s.d.DenyVerbs, 1) {
			return &DeniedError{Line: line, Why: "a denied command (or an abbreviation of one)"}
		}
		if cs.config && isPrefix(f[0], "enable", 2) {
			return &DeniedError{Line: line, Why: "enable in configuration mode changes the enable password"}
		}
		for _, w := range f {
			if prefixOfAny(w, s.d.DenyAnywhere, 3) {
				return &DeniedError{Line: line, Why: "a denied word (or an abbreviation of one)"}
			}
		}
	}
	// terminal: only the paging length may be set.
	if isPrefix(f[0], "terminal", 2) && n != Norm(s.d.NoPaging) {
		return &DeniedError{Line: line, Why: "terminal settings other than the paging length"}
	}
	if isPrefix(f[0], "write", 1) && len(f) > 1 && !inList(f[1], s.d.WriteArgsOK) {
		return &DeniedError{Line: line, Why: "write to elsewhere"}
	}
	if isPrefix(f[0], "interface", 2) && len(f) > 1 && isPrefix(f[1], "wan", 1) {
		return &DeniedError{Line: line, Why: "the OLT's own WAN interface"}
	}
	// "onu all ..." and "no onu all": every ONU at once. Neither is ever sent.
	if t := onuTarget(f); t != "" && isPrefix(t, "all", 2) {
		return &DeniedError{Line: line, Why: "an action on all ONUs at once"}
	}
	// "onu <list> deactivate|disable|standy" takes ONUs out of service.
	if f[0] == "onu" && len(f) > 2 && prefixOfAny(f[2], []string{"deactivate", "disable", "standy"}, 3) {
		return &DeniedError{Line: line, Why: "taking an ONU out of service"}
	}
	// The plan only ever uses hybrid ports and adds VLANs to them: a port mode change or a VLAN taken off a port (the
	// uplink carries the management VLAN) is never sent.
	if isPrefix(f[0], "switchport", 3) && len(f) > 1 && !isPrefix(f[1], "hybrid", 1) {
		return &DeniedError{Line: line, Why: "a port mode change"}
	}
	if f[0] == "no" && len(f) > 1 {
		switch {
		case prefixOfAny(f[1], s.d.DenyAfterNo, 1):
			return &DeniedError{Line: line, Why: "user, password, address or route removal"}
		case f[1] == "ip" && len(f) > 2 && prefixOfAny(f[2], []string{"address", "route", "default-gateway", "gateway"}, 1):
			return &DeniedError{Line: line, Why: "removal of the management address or route"}
		case isPrefix(f[1], "interface", 2) && len(f) > 2 && prefixOfAny(f[2], []string{"vlan", "vlanif"}, 1):
			return &DeniedError{Line: line, Why: "removal of a VLAN interface"}
		// "no login-access-list ...", "no snmp-server ...", "no sshd", "no service ...": undoing a denied top-level
		// command is as denied as the command.
		case prefixOfAny(f[1], s.d.DenyVerbs, 1):
			return &DeniedError{Line: line, Why: "removal of a system, login or access setting"}
		// A plan never removes a VLAN or takes one off a port.
		case isPrefix(f[1], "vlan", 2), isPrefix(f[1], "switchport", 3):
			return &DeniedError{Line: line, Why: "removal of a VLAN"}
		// "no onu 1-64", "no onu 1,2,3": one ONU at a time only.
		case f[1] == "onu" && len(f) > 2 && onuListRE.MatchString(f[2]):
			return &DeniedError{Line: line, Why: "removal of several ONUs at once"}
		}
	}
	if s.Protect > 0 {
		if f[0] == "no" && len(f) >= 3 && isPrefix(f[1], "vlan", 2) && vlanListHas(f[2], s.Protect) {
			return &DeniedError{Line: line, Why: "removal of the management VLAN"}
		}
		if cs.mgmt && f[0] == "no" {
			return &DeniedError{Line: line, Why: "a removal inside the management interface"}
		}
	}
	return nil
}

// onuListRE is an ONU list of more than one ONU ("1-64", "1,2", "3,5-9").
var onuListRE = regexp.MustCompile(`^\d+([,-]\d*)+$`)

// onuTarget is the ONU list word of "onu <list> ..." or "no onu <list> ..." ("" for other lines).
func onuTarget(f []string) string {
	switch {
	case len(f) > 1 && f[0] == "onu":
		return f[1]
	case len(f) > 2 && f[0] == "no" && f[1] == "onu":
		return f[2]
	}
	return ""
}

// isPrefix reports whether w (at least min letters) is the start of word: how the CLI reads an abbreviation.
func isPrefix(w, word string, min int) bool {
	return len(w) >= min && strings.HasPrefix(word, w)
}

func prefixOfAny(w string, words []string, min int) bool {
	for _, x := range words {
		if isPrefix(w, x, min) {
			return true
		}
	}
	return false
}

func inList(w string, l []string) bool {
	for _, x := range l {
		if w == x {
			return true
		}
	}
	return false
}

// vlanListHas reports whether a VLAN argument ("98", "90-100", "5,98") names v.
func vlanListHas(arg string, v int) bool {
	for _, part := range strings.Split(arg, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if a <= v && v <= b {
			return true
		}
	}
	return false
}

func (s *Session) track(line string) { s.cs = s.trackIn(line, s.cs) }

// trackIn is the state after a line.
func (s *Session) trackIn(line string, cs cstate) cstate {
	f := strings.Fields(Norm(line))
	if len(f) == 0 {
		return cs
	}
	switch {
	case len(f) >= 3 && isPrefix(f[0], "interface", 2) && isPrefix(f[1], "vlan", 1):
		return cstate{mgmt: s.Protect > 0 && f[2] == fmt.Sprint(s.Protect), sub: true}
	case isPrefix(f[0], "interface", 2), f[0] == "profile", f[0] == "vlan" && len(f) == 2:
		return cstate{sub: true}
	case f[0] == "exit", f[0] == "end", f[0] == "vlan":
		return cstate{}
	}
	return cs
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
	s.lastDbg = debugCopy(s.raw, 4000)
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
	return s.ShowAt(ctx, ModePriv, cmd)
}

// ShowAt runs one read-only command in the given mode (a version or a running configuration may only exist in
// configuration mode on some firmware) and leaves configuration mode again.
func (s *Session) ShowAt(ctx context.Context, mode Mode, cmd string) (string, error) {
	if err := s.raise(ctx, mode); err != nil {
		return "", err
	}
	out, err := s.show(ctx, cmd)
	if mode == ModeConfig {
		s.lower(ctx)
	}
	return out, err
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

// CmdOut is what one command of a Query printed.
type CmdOut struct {
	Cmd string
	Out string
	Err error  // a refusal by the OLT (*CmdError); other errors end the Query
	Dbg string // the raw output with control characters made visible, for diagnosing a layout
}

// QueryEach runs a Query (a read-only sequence that may sit in configuration mode) and returns each command's own
// output. A command the OLT refuses does not stop the rest; a lost connection or a timeout does. It always leaves
// configuration mode afterwards.
func (s *Session) QueryEach(ctx context.Context, q Query) ([]CmdOut, error) {
	if err := s.raise(ctx, q.Mode); err != nil {
		return nil, err
	}
	var res []CmdOut
	var hard error
	for _, c := range q.Cmds {
		s.lastDbg = ""
		out, err := s.show(ctx, c)
		var ce *CmdError
		if err != nil && !errors.As(err, &ce) {
			hard = err
			break
		}
		res = append(res, CmdOut{Cmd: c, Out: out, Err: err, Dbg: s.lastDbg})
	}
	s.lower(ctx)
	return res, hard
}

// Query runs a Query and returns the output of its show commands, joined. The first refusal ends it.
func (s *Session) Query(ctx context.Context, q Query) (string, error) {
	outs, err := s.QueryEach(ctx, q)
	var b bytes.Buffer
	for _, o := range outs {
		if o.Err != nil {
			return b.String(), o.Err
		}
		if strings.HasPrefix(Norm(o.Cmd), "show") {
			b.WriteString(o.Out + "\n")
		}
	}
	return b.String(), err
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
	_, err := s.ConfigN(ctx, lines)
	return err
}

// ConfigN is Config that also says how many of the lines were sent to the OLT (the refused one included).
// CheckAll applies the denylist to a batch of lines (as ConfigN would before sending), without sending anything.
func (s *Session) CheckAll(lines []string) error {
	in := s.cs
	in.config = true
	for _, l := range lines {
		in.config = true
		if err := s.checkIn(l, in); err != nil {
			return err
		}
		in = s.trackIn(l, in)
	}
	return nil
}

func (s *Session) ConfigN(ctx context.Context, lines []string) (int, error) {
	// Check every line before any is sent: a denied line must stop the batch before it changes anything.
	in := s.cs
	in.config = true
	for _, l := range lines {
		in.config = true
		if err := s.checkIn(l, in); err != nil {
			return 0, err
		}
		in = s.trackIn(l, in)
	}
	if err := s.raise(ctx, ModeConfig); err != nil {
		return 0, err
	}
	for i, l := range lines {
		out, err := s.exec(ctx, l, false)
		if err != nil {
			s.lower(ctx)
			return i + 1, err
		}
		if failed(s.d.ErrorRE, out) {
			s.Transcript[len(s.Transcript)-1].Err = "refused"
			s.lower(ctx)
			return i + 1, &CmdError{Cmd: l, Output: out}
		}
	}
	s.lower(ctx)
	return len(lines), nil
}

// Save writes the running configuration to the OLT's flash.
func (s *Session) Save(ctx context.Context) error {
	if err := s.raise(ctx, ModePriv); err != nil {
		return err
	}
	// Start from a quiet line: output still arriving from an earlier command (the first save's late "[OK]" and its prompt)
	// would otherwise be read as this save's answer, or leave the reading one prompt behind.
	s.resync(ctx)
	cmd := s.d.SaveCmd
	if err := s.check(cmd); err != nil {
		return err
	}
	to := s.SaveTimeout
	if to <= 0 {
		to = s.d.SaveTimeout
	}
	if to < s.Timeout {
		to = s.Timeout
	}
	s.raw, s.answered, s.asked = nil, 0, 0
	if err := s.write(cmd); err != nil {
		return err
	}
	deadline := time.Now().Add(to)
	confirmed := false
	var out, last string
	for {
		var err error
		out, last, err = s.readUntil(ctx, time.Until(deadline), stopQuestion)
		if err != nil {
			s.Transcript = append(s.Transcript, Entry{Cmd: Redact(cmd), Err: err.Error()})
			if errors.Is(err, ErrTimeout) {
				// What the OLT showed last, so that the next failure can be read from the page.
				return fmt.Errorf("the save did not finish within %s: %w; the OLT showed last: %q", to, err, outputTail(out, 200))
			}
			return err
		}
		if s.d.PromptRE.MatchString(last) {
			break
		}
		// A yes/no question: only the save's own confirmation is answered, once.
		if !confirmed && s.d.SaveConfirmRE != nil && s.d.SaveConfirmRE.MatchString(last) {
			confirmed = true
			if err := s.write(s.d.SaveConfirmAnswer); err != nil {
				return err
			}
			continue
		}
		_ = s.write("n")
		_, pl, _ := s.readUntil(ctx, s.Timeout, stopPrompt)
		if pl != "" {
			s.setPrompt(pl)
		}
		q := Redact(clip(last, 200))
		s.Transcript = append(s.Transcript, Entry{Cmd: Redact(cmd), Out: q, Err: "unexpected question, answered n"})
		return &CmdError{Cmd: cmd, Output: "the OLT asked \"" + q + "\", which is not the save confirmation; it was answered n"}
	}
	// Some OLTs print the prompt before the flash write ends and then "[OK]" and the prompt again: wait for a short quiet
	// moment and take the last prompt.
	if more, plast, ok := s.saveSettle(ctx); ok {
		out, last = out+last+"\n"+more, plast
	}
	s.setPrompt(last)
	s.track(cmd)
	if i := strings.IndexByte(out, '\n'); i >= 0 && (Norm(out[:i]) == Norm(cmd) || strings.HasSuffix(Norm(out[:i]), Norm(cmd))) {
		out = out[i+1:]
	}
	out = strings.TrimSpace(out)
	s.Transcript = append(s.Transcript, Entry{Cmd: Redact(cmd), Out: Redact(clip(out, 600))})
	if failed(s.d.ErrorRE, out) {
		return &CmdError{Cmd: cmd, Output: out}
	}
	return nil
}

// settleQuiet is how long the session waits after a save's prompt for more output (a late "[OK]" and a second prompt).
var settleQuiet = 700 * time.Millisecond

// resyncQuiet is how long resync waits for output still in flight.
var resyncQuiet = 200 * time.Millisecond

// saveSettle reads what arrives within settleQuiet after a prompt. ok: more output came and it ends at a prompt again (more is
// the text before that prompt).
func (s *Session) saveSettle(ctx context.Context) (more, last string, ok bool) {
	s.raw, s.answered = nil, 0
	out, l, err := s.readUntil(ctx, settleQuiet, stopQuestion)
	if err != nil || l == "" || !s.d.PromptRE.MatchString(l) {
		return "", "", false
	}
	return out, l, true
}

// resync drains what is still arriving and makes sure the OLT is at a prompt: an empty line, read to the prompt (a short
// wait; a failure is left to the command that follows).
func (s *Session) resync(ctx context.Context) {
	s.raw, s.answered = nil, 0
	_, _, _ = s.readUntil(ctx, resyncQuiet, stopPrompt) // whatever is in flight
	s.raw, s.answered = nil, 0
	if s.write("") != nil {
		return
	}
	if _, last, err := s.readUntil(ctx, 5*time.Second, stopPrompt); err == nil && last != "" {
		// Late output may hold more than one prompt: read until it is quiet.
		for {
			s.raw, s.answered = nil, 0
			_, l, err := s.readUntil(ctx, resyncQuiet, stopPrompt)
			if err != nil || l == "" {
				break
			}
			last = l
		}
		s.setPrompt(last)
	}
	s.raw, s.answered = nil, 0
}

// outputTail is the last n characters of rendered output, on one line and redacted, for an error message.
func outputTail(out string, n int) string {
	t := strings.Join(strings.Fields(out), " ")
	if r := []rune(t); len(r) > n {
		t = "..." + string(r[len(r)-n:])
	}
	return Redact(t)
}

// Redact hides what looks like a secret in text that is shown or stored.
//
// The server's own notes use the same words in prose ("the old password was restored", "the SSH host key of the OLT"),
// so a value that is a plain English word of those notes, or only punctuation, is left as it is: no generated or typed
// secret is one of them, and a note stays readable.
func Redact(s string) string {
	s = redactWith(userPassRE, s)
	return redactWith(redactRE, s)
}

func redactWith(re *regexp.Regexp, s string) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		val := s[l[3]:l[1]] // the value after the key word (group 1 is the key word and what follows it)
		if proseWord(val) {
			continue
		}
		b.WriteString(s[last:l[3]])
		b.WriteString("<redacted>")
		last = l[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// proseWords are the words that follow a key word in the server's own notes and errors. A secret is never one of them.
var proseWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but nor so then than as at by for from in into of on onto to with without
		is are was were be been being has have had do does did done not no yes it its this that these those there here
		now still again only also just yet already all any each every both either neither
		works worked work working failed fails fail kept stored changed restored replaced reset set sent saved removed
		added tried tested checked verified matched mismatch changes change pinned unknown missing wrong ok
		could would should will can may might must cannot isn't wasn't doesn't didn't`) {
		proseWords[w] = true
	}
}

func proseWord(v string) bool {
	w := strings.ToLower(strings.TrimRight(v, ".,;:!?)]\"'"))
	if w == "" {
		return true // only punctuation ("the password." / "password)")
	}
	return proseWords[w]
}

// userPassRE: "user login-password NAME PASSWORD" and "user enable-password NAME PASSWORD" (the name is not the secret, the
// word after it is). Applied before redactRE, which would otherwise take the user name for the password.
var userPassRE = regexp.MustCompile(`(?i)\b((?:login|enable)-password\s+\S+\s+)\S+`)

var redactRE = regexp.MustCompile(`(?im)\b((?:enable\s+)?(?:username|password|passwd|pwd|secret|community|auth-key|priv-key|key-string|key|md5|sha|encrypted|cipher|psk)\b\s*(?:\d\s+)?(?:=\s*|:\s*)?)\S+`)

// ---- the secure action ---------------------------------------------------------------------------------------------

// ErrSecureShape: RunSecure was given a line that is not one of the exact shapes of the secure action.
var ErrSecureShape = errors.New("olt: not a line of the secure action")

// RunSecure sends the lines of the "Secure this OLT" action: the user manager passwords and the removal of the default
// SNMP communities. They are the only user, password or SNMP lines the driver ever sends, and every one must have one of
// the exact shapes in Dialect.SecureShapes (checked for the whole batch before the first is sent). The lines go through
// the normal command path otherwise (control characters, redacted transcript, error detection), and the session is back
// out of configuration mode afterwards. The first line the OLT refuses ends the batch (*CmdError, redacted).
func (s *Session) RunSecure(ctx context.Context, lines []string) error {
	return s.runSecure(ctx, lines, false)
}

// RunRestore is RunSecure for the lines that put an old login or enable password back after a failed change: the shapes
// of RunSecure plus the old-password shapes (Dialect.RestoreShapes).
func (s *Session) RunRestore(ctx context.Context, lines []string) error {
	return s.runSecure(ctx, lines, true)
}

func (s *Session) runSecure(ctx context.Context, lines []string, restore bool) error {
	for _, l := range lines {
		if !s.d.SecureLineOK(l, restore) {
			return ErrSecureShape
		}
	}
	for _, l := range lines {
		for _, c := range l {
			if c < 0x20 || c == 0x7f {
				return ErrSecureShape
			}
		}
	}
	if err := s.raise(ctx, ModeConfig); err != nil {
		return err
	}
	var rerr error
	for _, l := range lines {
		out, err := s.exec(ctx, l, true)
		if err != nil {
			rerr = err
			break
		}
		if failed(s.d.ErrorRE, out) {
			s.Transcript[len(s.Transcript)-1].Err = "refused"
			rerr = &CmdError{Cmd: l, Output: out}
			break
		}
	}
	if rerr != nil {
		// Leave whatever sub-mode the batch was in (a refused line inside `user manager`).
		for i := 0; i < 2 && s.mode == ModeConfig && promptInSubMode(s.prompt); i++ {
			if _, err := s.exec(ctx, s.d.ExitCmd, true); err != nil {
				break
			}
		}
	}
	s.lower(ctx)
	return rerr
}

// VerifyEnable exercises the enable password: a session that starts at the user prompt must get to the privileged one with
// it. checked is false when the session started privileged (the bench OLT's SSH does): then the enable password cannot be
// tested from this login and nothing is claimed about it.
func (s *Session) VerifyEnable(ctx context.Context) (checked bool, err error) {
	if s.mode != ModeUser {
		return false, nil
	}
	return true, s.enableMode(ctx)
}
