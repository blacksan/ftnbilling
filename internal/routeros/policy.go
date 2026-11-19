package routeros

import (
	"net/netip"
	"regexp"
	"strconv"
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

// Kind says how an accepted command is run.
type Kind int

const (
	// KindExec: send Decision.Exec through /rest/execute.
	KindExec Kind = iota
	// KindPing: call the router's ping endpoint with Target and Count.
	KindPing
	// KindTraceroute: call the router's traceroute endpoint with Target.
	KindTraceroute
)

// Decision is the policy's answer for one command line.
type Decision struct {
	Verdict Verdict
	Reason  string
	// Redacted is the command with secret values masked, printable ASCII
	// only, at most 300 characters: what the audit log records.
	Redacted string
	// Kind, Exec, Target and Count describe how to run an accepted command.
	// Exec is rebuilt from the parsed command (never the raw input), with the
	// flags that make the output safe to redact forced on.
	Kind   Kind
	Exec   string
	Target string
	Count  int
}

// MaxCommandLen is the longest command line the console accepts.
const MaxCommandLen = 500

// The console grammar is an allowlist. One submission is exactly one plain
// command:
//
//	/menu/path verb [word | name=value | name="quoted value"]... [where ...]
//
// Path and verb words are lower-case [a-z0-9-]. Any scripting syntax is
// refused anywhere in the line, quotes included. Verbs outside the lists below
// are refused, not sent to the router to find out.

// readVerbs run without confirmation; changeVerbs need the building name and
// the current password.
var readVerbs = map[string]bool{"print": true, "get": true, "monitor": true, "export": true, "ping": true, "traceroute": true}
var changeVerbs = map[string]bool{"add": true, "set": true, "remove": true, "enable": true, "disable": true,
	"reset-counters": true, "reboot": true, "shutdown": true}

// deniedPaths are menus that are never run. A pattern matches when the first
// words of the command's path are abbreviations (RouterOS accepts unique
// prefixes) of the pattern's words. printOK menus still allow print, whose
// output is redacted.
var deniedPaths = []struct {
	words   []string
	printOK bool
	why     string
}{
	{[]string{"user"}, false, "router users and groups are not managed here"},
	{[]string{"password"}, false, "passwords are not changed here"},
	{[]string{"system", "reset-configuration"}, false, "resetting the router is not allowed here"},
	{[]string{"system", "script"}, false, "scripts are not allowed here"},
	{[]string{"system", "scheduler"}, false, "the scheduler is not allowed here"},
	{[]string{"system", "backup"}, false, "backups are not allowed here"},
	{[]string{"system", "package"}, false, "packages are not allowed here"},
	{[]string{"system", "routerboard"}, false, "the router board is not changed here"},
	{[]string{"tool", "fetch"}, false, "fetching files is not allowed here"},
	{[]string{"import"}, false, "importing scripts is not allowed here"},
	{[]string{"file"}, true, "files can be listed but not changed here"},
	{[]string{"ip", "service"}, false, "router services are not changed here"},
	{[]string{"certificate"}, false, "certificates are not managed here"},
	{[]string{"interface", "wireguard"}, true, "WireGuard interfaces and peers can be listed but not changed here"},
	{[]string{"ppp", "secret"}, false, "PPP secrets are not managed here"},
	{[]string{"ip", "hotspot", "user"}, true, "hotspot users can be listed but not changed here"},
	{[]string{"interface", "wireless", "security-profiles"}, false, "wireless security profiles are not managed here"},
	{[]string{"interface", "wifi", "security"}, false, "Wi-Fi security settings are not managed here"},
	{[]string{"interface", "wifi", "security-profiles"}, false, "Wi-Fi security settings are not managed here"},
	{[]string{"snmp", "community"}, false, "SNMP communities are not managed here"},
	{[]string{"radius"}, false, "RADIUS servers are not managed here"},
}

// secretWords make a read command refuse: a name containing one is a secret
// (asking for it, or filtering on it, would leak it a guess at a time).
var secretWords = []string{"key", "secret", "password", "passphrase", "pre-shared", "preshared", "psk", "private", "auth"}

// scriptKeys are argument names that hold something the router runs later.
var scriptKeys = regexp.MustCompile(`(?i)(script|^on-|source|command|contents|^run-)`)

var (
	wordRe  = regexp.MustCompile(`^[a-z0-9-]+$`)
	headRe  = regexp.MustCompile(`^/?[a-z0-9-]+(/[a-z0-9-]+)*/?$`)
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9!][A-Za-z0-9_.!-]*$`)
	bareRe  = regexp.MustCompile(`^[A-Za-z0-9*!][A-Za-z0-9_.:/*@+,%~<>=!&|-]*$`)
	quoteRe = regexp.MustCompile(`^([A-Za-z0-9!][A-Za-z0-9_.!-]*)(=|~|!=|<=|>=|<|>)"([^"]*)"$`)
)

// Classify applies the console policy to one command line. It is a pure
// function: the answer depends only on cmd.
func Classify(cmd string) Decision {
	d := Decision{Redacted: auditText(cmd)}
	d, ok := classify(d, cmd)
	if !ok && d.Verdict != Deny {
		d.Verdict = Deny
	}
	if d.Verdict == Deny {
		d.Exec, d.Target, d.Count = "", "", 0
	}
	return d
}

func deny(d Decision, why string) (Decision, bool) { d.Verdict, d.Reason = Deny, why; return d, false }

func classify(d Decision, cmd string) (Decision, bool) {
	cmd = strings.Trim(cmd, " ")
	switch {
	case cmd == "":
		return deny(d, "type a command")
	case len(cmd) > MaxCommandLen:
		return deny(d, "the command is longer than 500 characters")
	}
	for _, r := range cmd {
		if r < 0x20 || r > 0x7e {
			return deny(d, "one line of plain ASCII only: no tabs, line breaks or special characters")
		}
		if strings.ContainsRune(";[]{}$`\\#", r) {
			return deny(d, "scripting characters ("+string(r)+") are not allowed, not even inside quotes")
		}
	}
	toks, err := tokenize(cmd)
	if err != "" {
		return deny(d, err)
	}
	// Leading words: path and verb. They end at the first token that is not a
	// plain word (an argument such as name=value, an address or a quoted text).
	var words []string
	i := 0
	for ; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, ":") {
			return deny(d, "scripting commands (starting with :) are not allowed")
		}
		if !headRe.MatchString(t) {
			break
		}
		for _, p := range strings.Split(t, "/") {
			if p != "" {
				words = append(words, p)
			}
		}
	}
	args := toks[i:]
	// Denied menus first, so the reason names them whatever the verb is.
	var denyPath bool
	var printOK bool
	var why string
	for _, p := range deniedPaths {
		if prefixMatch(words, p.words) {
			denyPath, printOK, why = true, p.printOK, p.why
			break
		}
	}
	vi := -1
	for k, w := range words {
		if readVerbs[w] || changeVerbs[w] {
			vi = k
			break
		}
	}
	if denyPath && (!printOK || vi < 0 || words[vi] != "print") {
		return deny(d, why)
	}
	if vi < 0 {
		return deny(d, "no known command: use a menu path and a verb such as print, get, add, set, remove, enable or disable")
	}
	path, verb := words[:vi], words[vi]
	// Words after the verb are its first arguments.
	args = append(append([]string{}, words[vi+1:]...), args...)
	// Every argument must be a plain word, name=value, or name="quoted value".
	for _, a := range args {
		if why := checkArg(a); why != "" {
			return deny(d, why)
		}
	}
	base := "/" + strings.Join(path, " ")
	join := func(parts ...string) string {
		out := strings.TrimRight(base, " ")
		for _, p := range parts {
			if p == "" {
				continue
			}
			if out == "/" {
				out += p
			} else {
				out += " " + p
			}
		}
		return out
	}

	switch verb {
	case "ping":
		if len(path) > 1 || len(path) == 1 && path[0] != "tool" {
			return deny(d, "use /ping or /tool ping")
		}
		return pingLike(d, args, KindPing)
	case "traceroute":
		if len(path) != 1 || path[0] != "tool" {
			return deny(d, "use /tool traceroute")
		}
		return pingLike(d, args, KindTraceroute)
	case "print", "get", "monitor", "export":
		// Names that hold secrets are never read or filtered on.
		for _, w := range path {
			if hasSecretWord(w) {
				return deny(d, "this menu holds secrets and is not read here")
			}
		}
		for _, a := range args {
			if hasSecretWord(argName(a)) || hasListSecret(a) {
				return deny(d, "properties that hold secrets are not read or filtered on here")
			}
		}
	}

	switch verb {
	case "print":
		var flags, rest []string
		var where bool
		for _, a := range args {
			if where {
				rest = append(rest, a)
				continue
			}
			switch {
			case a == "where":
				where = true
				rest = append(rest, a)
			case a == "terse" || a == "without-paging":
				// forced below
			case a == "stats" || a == "count-only":
				flags = append(flags, a)
			case strings.HasPrefix(a, "proplist=") || strings.HasPrefix(a, "from="):
				flags = append(flags, a)
			default:
				return deny(d, "print takes only where, stats, count-only, proplist= and from= (not "+truncate(argName(a), 40)+")")
			}
		}
		parts := []string{"print"}
		if !contains(flags, "count-only") {
			parts = append(parts, "terse")
		}
		parts = append(parts, "without-paging")
		parts = append(parts, flags...)
		parts = append(parts, rest...)
		d.Verdict, d.Exec = Allow, join(parts...)
		return d, true
	case "get":
		if len(args) == 0 || len(args) > 2 {
			return deny(d, "get takes the item and one property, for example: /ip address get 0 address")
		}
		for _, a := range args {
			if !isPlain(a) && !strings.HasPrefix(a, "value-name=") || strings.ContainsAny(a, "~<>!&|") {
				return deny(d, "get takes the item and one property")
			}
		}
		d.Verdict, d.Exec = Allow, join(append([]string{"get"}, args...)...)
		return d, true
	case "monitor":
		if !contains(args, "once") {
			return deny(d, "monitor needs the word once (it never ends otherwise)")
		}
		for _, a := range args {
			if a != "once" && !isPlain(a) {
				return deny(d, "monitor takes only the item name and once")
			}
		}
		d.Verdict, d.Exec = Allow, join(append([]string{"monitor"}, args...)...)
		return d, true
	case "export":
		var flags []string
		for _, a := range args {
			switch a {
			case "compact", "terse", "verbose":
				flags = append(flags, a)
			case "hide-sensitive":
			case "show-sensitive":
				return deny(d, "show-sensitive is not allowed")
			default:
				return deny(d, "export takes only compact, terse or verbose (no files)")
			}
		}
		d.Verdict, d.Exec = Allow, join(append([]string{"export", "hide-sensitive"}, flags...)...)
		return d, true
	}

	// Changes.
	for _, a := range args {
		if n := argName(a); scriptKeys.MatchString(n) && strings.Contains(a, "=") {
			return deny(d, "settings that hold scripts are not allowed here ("+truncate(n, 40)+")")
		}
	}
	if verb == "reboot" || verb == "shutdown" {
		// a reboot or shutdown from the console would skip the guards of the reboot button (a running set-up, a pending rollback)
		return deny(d, "/system "+verb+" is not run from the console: use the router's Reboot button, which checks that no set-up is running")
	}
	d.Verdict = Confirm
	d.Reason = "this changes the router"
	d.Exec = join(append([]string{verb}, args...)...)
	return d, true
}

// pingLike reads the arguments of ping and traceroute: one target and
// optionally count=. They run through the router's JSON endpoints, never as a
// script.
func pingLike(d Decision, args []string, k Kind) (Decision, bool) {
	target, count := "", 0
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "count="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "count="))
			if err != nil || n < 1 || n > 10 {
				return deny(d, "count= must be a number from 1 to 10")
			}
			count = n
		case strings.Contains(a, "=") || target != "":
			return deny(d, "give one target and optionally count=")
		default:
			target = a
		}
	}
	if !ValidTarget(target) {
		return deny(d, "the target must be an IP address or a host name")
	}
	if k == KindPing && count == 0 {
		count = 4
	}
	if k == KindTraceroute {
		count = 0
	}
	d.Verdict, d.Kind, d.Target, d.Count = Allow, k, target, count
	return d, true
}

var hostRe = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// ValidTarget accepts a plain IPv4 or IPv6 address (no zone, so no %) or a
// strict host name. Nothing else is ever handed to a router or to exec.
func ValidTarget(t string) bool {
	if t == "" || len(t) > 253 || strings.HasPrefix(t, "-") {
		return false
	}
	if a, err := netip.ParseAddr(t); err == nil {
		return a.Zone() == "" && !strings.Contains(t, "%")
	}
	return hostRe.MatchString(t)
}

// tokenize splits on spaces outside double quotes. A quote may only open after
// an operator (name="text") or at the start of a token, and must close.
func tokenize(cmd string) ([]string, string) {
	var toks []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '"':
			inQ = !inQ
			cur.WriteByte(c)
		case c == ' ' && !inQ:
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inQ {
		return nil, "a quote is not closed"
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks, ""
}

// checkArg returns why an argument is refused, or "".
func checkArg(a string) string {
	if strings.Contains(a, `"`) {
		m := quoteRe.FindStringSubmatch(a)
		if m == nil {
			return "quotes are allowed only as name=\"text\""
		}
		if strings.HasPrefix(m[3], ":") || strings.Contains(m[3], " :") {
			return "scripting commands (starting with :) are not allowed, not even inside quotes"
		}
		return ""
	}
	if !bareRe.MatchString(a) {
		return "this argument is not a plain word or name=value: " + truncate(auditText(a), 40)
	}
	return ""
}

func argName(a string) string {
	if q := quoteRe.FindStringSubmatch(a); q != nil {
		return strings.ToLower(q[1])
	}
	if i := strings.IndexAny(a, "=~<>"); i >= 0 {
		a = a[:i]
	}
	return strings.ToLower(a)
}

// hasListSecret checks the values of proplist= and value-name=, which name
// properties rather than hold data.
func hasListSecret(a string) bool {
	for _, p := range []string{"proplist=", "value-name="} {
		if strings.HasPrefix(a, p) {
			return hasSecretWord(strings.ToLower(strings.TrimPrefix(a, p)))
		}
	}
	return false
}

func hasSecretWord(s string) bool {
	s = strings.ToLower(s)
	for _, w := range secretWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func isPlain(a string) bool { return !strings.ContainsAny(a, `="~<>`) }

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// prefixMatch reports whether the first words are abbreviations of pat.
func prefixMatch(words, pat []string) bool {
	if len(words) < len(pat) {
		return false
	}
	for i, p := range pat {
		if words[i] == "" || !strings.HasPrefix(p, words[i]) {
			return false
		}
	}
	return true
}

// auditText makes a command safe to log: printable ASCII only (anything else
// becomes ?), secrets masked, at most 300 characters.
func auditText(cmd string) string {
	b := make([]rune, 0, len(cmd))
	for _, r := range cmd {
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		b = append(b, r)
	}
	return truncate(RedactOutput(string(b)), 300)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// secretRe matches a name that holds a secret and its value: key=value,
// key: value and "key":"value" forms.
var secretRe = regexp.MustCompile(`(?i)("?[A-Za-z0-9_.-]*(?:pre-?shared-key|private-key|authentication-key|password|secret|passphrase|psk|key)[A-Za-z0-9_.-]*"?\s*[=:]\s*)("(?:[^"\\]|\\.)*"|[^\s,;]+)`)

// RedactOutput masks the value of any key whose name contains pre-shared-key,
// private-key, authentication-key, password, secret, passphrase, psk or key,
// in key=value, key: value and JSON forms. It is applied to router output (the
// console forces terse output, which is key=value only) and to commands before
// they are logged.
func RedactOutput(s string) string {
	return secretRe.ReplaceAllString(s, `${1}[hidden]`)
}
