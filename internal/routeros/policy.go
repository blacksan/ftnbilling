package routeros

import (
	"regexp"
	"strings"
)

// Verdict is what the console policy says about a command line.
type Verdict int

const (
	// Allow: read-only, run it straight away.
	Allow Verdict = iota
	// Confirm: it changes something; the admin must type the building name and
	// the current password first.
	Confirm
	// Deny: never run from the web console.
	Deny
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Confirm:
		return "confirm"
	}
	return "deny"
}

// Decision is the policy's answer for one command line.
type Decision struct {
	Verdict Verdict
	Reason  string
	// Redacted is the command with secret values masked, at most 300
	// characters: what the audit log records.
	Redacted string
}

// MaxCommandLen is the longest command line the console accepts.
const MaxCommandLen = 500

// denied are word-prefix patterns that are never run. A pattern matches when
// its words are prefixes-of (abbreviations of) consecutive words of a segment.
// anchored patterns must start the segment; others may start anywhere.
var denied = []struct {
	words    []string
	anchored bool
	why      string
}{
	{[]string{"system", "reset-configuration"}, false, "resetting the router is not allowed here"},
	{[]string{"system", "script"}, false, "scripts are not allowed here"},
	{[]string{"system", "scheduler"}, false, "the scheduler is not allowed here"},
	{[]string{"system", "backup"}, false, "backups are not allowed here"},
	{[]string{"system", "package"}, false, "packages are not allowed here"},
	{[]string{"system", "routerboard", "upgrade"}, false, "firmware upgrades are not allowed here"},
	{[]string{"user"}, true, "router users and groups are not managed here"},
	{[]string{"password"}, true, "passwords are not changed here"},
	{[]string{"ip", "service"}, false, "router services are not changed here"},
	{[]string{"certificate"}, false, "certificates are not managed here"},
	{[]string{"tool", "fetch"}, false, "fetching files is not allowed here"},
	{[]string{"import"}, true, "importing scripts is not allowed here"},
	{[]string{"file", "remove"}, false, "removing files is not allowed here"},
	{[]string{"file", "edit"}, false, "editing files is not allowed here"},
}

var readVerbs = map[string]bool{"print": true, "get": true, "find": true, "export": true, "monitor": true, "ping": true, "traceroute": true}
var writeVerbs = map[string]bool{"add": true, "set": true, "remove": true, "enable": true, "disable": true, "edit": true, "move": true,
	"unset": true, "reset": true, "reboot": true, "shutdown": true, "run": true, "comment": true, "export-file": true, "flush": true,
	"login": true, "logout": true, "release": true, "renew": true, "make-static": true, "scan": true, "upgrade": true}

// argStarts end the "path and verb" part of a segment.
var argStarts = map[string]bool{"where": true, "as-value": true, "without-paging": true, "terse": true, "detail": true, "brief": true,
	"count-only": true, "interval": true, "once": true, "proplist": true, "from": true, "value-name": true, "do": true, "in": true}

// Classify applies the console policy to one command line. It is a pure
// function: the answer depends only on cmd.
func Classify(cmd string) Decision {
	d := Decision{Redacted: truncate(RedactOutput(cmd), 300)}
	cmd = strings.TrimSpace(cmd)
	switch {
	case cmd == "":
		return deny(d, "type a command")
	case len(cmd) > MaxCommandLen:
		return deny(d, "the command is longer than 500 characters")
	}
	for _, r := range cmd {
		if r < 0x20 || r == 0x7f {
			return deny(d, "one line only, no control characters")
		}
	}
	lower := strings.ToLower(cmd)
	if strings.Contains(lower, "show-sensitive") {
		return deny(d, "show-sensitive is not allowed")
	}
	if strings.Contains(lower, ":execute") {
		return deny(d, ":execute is not allowed")
	}
	segs := splitSegments(cmd)
	if len(segs) == 0 {
		return deny(d, "type a command")
	}
	worst := Allow
	reason := ""
	for _, words := range segs {
		v, why := classifySegment(words)
		if v == Deny {
			return deny(d, why)
		}
		if v > worst {
			worst, reason = v, why
		}
	}
	d.Verdict, d.Reason = worst, reason
	if worst == Confirm && reason == "" {
		d.Reason = "this changes the router"
	}
	return d
}

func deny(d Decision, why string) Decision { d.Verdict, d.Reason = Deny, why; return d }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// splitSegments cuts the line at ; { } [ ] outside double quotes, turns
// slashes outside quotes into spaces, lower-cases, and returns the words of
// every non-empty segment. A quoted string is one word that starts with a
// double quote, so text inside it is never mistaken for a command.
func splitSegments(cmd string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	inQ, esc := false, false
	flushWord := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	flushSeg := func() {
		flushWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	for _, r := range cmd {
		if inQ {
			cur.WriteRune(r)
			switch {
			case esc:
				esc = false
			case r == '\\':
				esc = true
			case r == '"':
				inQ = false
			}
			continue
		}
		switch {
		case r == '"':
			if cur.Len() > 0 && !strings.HasSuffix(cur.String(), "=") {
				flushWord()
			}
			cur.WriteRune(r)
			inQ = true
		case r == ';' || r == '{' || r == '}' || r == '[' || r == ']':
			flushSeg()
		case r == '/' || r == ' ' || r == '\t':
			flushWord()
		default:
			cur.WriteRune(r)
		}
	}
	flushSeg()
	for i, ws := range segs {
		for j, w := range ws {
			if !strings.Contains(w, "\"") {
				segs[i][j] = strings.ToLower(w)
			}
		}
	}
	return segs
}

// abbrev reports whether w is a (non-empty) abbreviation of full: RouterOS
// accepts unique prefixes of command words.
func abbrev(w, full string) bool {
	return w != "" && !strings.HasPrefix(w, "\"") && strings.HasPrefix(full, w)
}

func matchAt(words []string, at int, pat []string) bool {
	if at+len(pat) > len(words) {
		return false
	}
	for i, p := range pat {
		if !abbrev(words[at+i], p) {
			return false
		}
	}
	return true
}

func classifySegment(words []string) (Verdict, string) {
	for _, p := range denied {
		if p.anchored {
			if matchAt(words, 0, p.words) {
				return Deny, p.why
			}
			continue
		}
		for at := range words {
			if matchAt(words, at, p.words) {
				return Deny, p.why
			}
		}
	}
	// "interface wireguard ..." may be read, never changed (it carries keys).
	for at := range words {
		if matchAt(words, at, []string{"interface", "wireguard"}) {
			if v := verbOf(words); !readVerbs[v] {
				return Deny, "WireGuard interfaces and peers are not changed here"
			}
		}
	}
	// Streaming commands never end through /rest/execute.
	for _, w := range words {
		if strings.Contains(w, "follow") {
			return Deny, "follow never ends: use print without follow"
		}
	}
	first := words[0]
	if strings.HasPrefix(first, ":") {
		switch first {
		case ":put", ":resolve":
			return Allow, ""
		}
		return Confirm, "scripting commands can change the router"
	}
	verb := verbOf(words)
	switch {
	case verb == "monitor":
		if !hasWord(words, "once") {
			return Deny, "monitor needs the word once (it never ends otherwise)"
		}
	case verb == "ping" || verb == "traceroute":
		if !hasArg(words, "count=") {
			return Deny, verb + " needs count=, for example count=4 (it never ends otherwise)"
		}
	}
	if readVerbs[verb] {
		if hasArg(words, "file=") || hasArg(words, "file-name=") {
			return Confirm, "this writes a file on the router"
		}
		return Allow, ""
	}
	if writeVerbs[verb] {
		return Confirm, "this changes the router"
	}
	return Confirm, "this command is not known to be read-only"
}

// verbOf finds the verb of a segment: the first known verb among the words
// before the first argument.
func verbOf(words []string) string {
	for _, w := range words {
		if strings.Contains(w, "=") || argStarts[w] {
			break
		}
		if readVerbs[w] || writeVerbs[w] {
			return w
		}
	}
	return ""
}

func hasWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

func hasArg(words []string, prefix string) bool {
	for _, x := range words {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}
	return false
}

var secretRe = regexp.MustCompile(`(?i)(private-key|preshared-key|password|secret|passphrase)((?:"?\s*[=:]\s*)|(?:\s*=\s*))("[^"]*"|[^\s,;]+)`)

// RedactOutput masks the value of private-key=, preshared-key=, password=,
// secret= and passphrase= (also in the "key: value" and JSON "key":"value"
// forms) in router output and in commands before they are logged.
func RedactOutput(s string) string {
	return secretRe.ReplaceAllString(s, `${1}${2}[hidden]`)
}
