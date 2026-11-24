package olt

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The grammar conformance check: every line the dialect can emit must be a line of the bench OLT's own grammar (its
// `find` output, testdata/bench-find.txt) in the CLI mode it is sent in, or one of the few lines that `find` was not
// asked about but the bench running configuration shows in that mode (fromRunning below, each with where it was seen).

// grammar is the bench grammar: mode -> syntax lines.
type grammar map[string][]gline

type gline struct {
	src string
	re  *regexp.Regexp
}

func loadGrammar(t *testing.T) grammar {
	t.Helper()
	b, err := os.ReadFile("testdata/bench-find.txt")
	if err != nil {
		t.Fatal(err)
	}
	g := grammar{}
	lineRE := regexp.MustCompile(`^\s+\(([^)]+)\)\s\s(.+?)\s*$`)
	for _, l := range strings.Split(string(b), "\n") {
		m := lineRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		re, ok := syntaxRE(m[2])
		if !ok {
			continue // a line this small converter cannot read (the wifi country lists); none is needed
		}
		g[m[1]] = append(g[m[1]], gline{m[2], re})
	}
	return g
}

// syntaxRE turns one grammar line ("onu add (1-64) profile NAME <sn|loid|loid+pw> WORD [WORD]") into a regular
// expression over a command line with a leading space.
func syntaxRE(s string) (*regexp.Regexp, bool) {
	p := &gparser{s: s}
	body, ok := p.seq()
	if !ok || p.i != len(p.s) {
		return nil, false
	}
	re, err := regexp.Compile(`(?i)^` + body + `\s*$`)
	return re, err == nil
}

type gparser struct {
	s string
	i int
}

func (p *gparser) skip() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

// seq reads items up to a closing bracket or '|'.
func (p *gparser) seq() (string, bool) {
	var b strings.Builder
	for {
		p.skip()
		if p.i >= len(p.s) || strings.IndexByte("]>}|", p.s[p.i]) >= 0 {
			return b.String(), true
		}
		it, ok := p.item()
		if !ok {
			return "", false
		}
		b.WriteString(it)
	}
}

func (p *gparser) alt(close byte) (string, bool) {
	var alts []string
	for {
		s, ok := p.seq()
		if !ok {
			return "", false
		}
		alts = append(alts, s)
		if p.i >= len(p.s) {
			return "", false
		}
		c := p.s[p.i]
		p.i++
		if c == '|' {
			continue
		}
		if c != close {
			return "", false
		}
		return "(?:" + strings.Join(alts, "|") + ")", true
	}
}

var placeholderRE = regexp.MustCompile(`^[A-Z][A-Z0-9_.:/+\-]*$|^[A-Z0-9_.:/\-]*[A-Z][A-Z0-9_.:/\-]*$`)

func (p *gparser) item() (string, bool) {
	switch c := p.s[p.i]; c {
	case '[':
		p.i++
		a, ok := p.alt(']')
		return a + "?", ok
	case '<':
		p.i++
		return p.alt('>')
	case '{':
		p.i++
		return p.alt('}')
	case '(':
		j := strings.IndexByte(p.s[p.i:], ')')
		if j < 0 || !regexp.MustCompile(`^\(-?\d+--?\d+\)$`).MatchString(p.s[p.i:p.i+j+1]) {
			return "", false
		}
		p.i += j + 1
		return `\s+-?\d+`, true
	}
	j := p.i
	for j < len(p.s) && strings.IndexByte(" []<>{}|()", p.s[j]) < 0 {
		j++
	}
	w := p.s[p.i:j]
	if w == "" {
		return "", false
	}
	p.i = j
	if placeholderRE.MatchString(w) && strings.ToLower(w) != w {
		return `\s+\S+`, true // WORD, NAME, A.B.C.D/M, VLAN_LIST, S/P ...
	}
	return `\s+` + regexp.QuoteMeta(w), true
}

// fromRunning are lines that `find` was not asked about: each is in the bench running configuration, in the same mode.
var fromRunning = map[string][]*regexp.Regexp{
	"config": {
		regexp.MustCompile(`^interface gpon 0/\d+$`),            // "interface gpon 0/1"
		regexp.MustCompile(`^interface gigabitEthernet 0/\d+$`), // "interface gigabitEthernet 0/1"
	},
	"vlan_interface": {regexp.MustCompile(`^ip address \d+\.\d+\.\d+\.\d+/\d+$`)}, // "ip address 192.168.8.200/24"
	"DBA profile":    {regexp.MustCompile(`^type 4 maximum \d+$`)},                // "type 4 maximum 1000000"
	"Line profile": {
		regexp.MustCompile(`^gemport \d+ tcont \d+ gemport_name \S+$`), // "gemport 1 tcont 1 gemport_name gem_1"
		regexp.MustCompile(`^service-port \d+ description \S+$`),       // "service-port 1 description internet"
		regexp.MustCompile(`^commit$`),
	},
	"Srv profile": {regexp.MustCompile(`^commit$`)},
}

// unverified are lines in neither: each is a TODO(bench) the review lists. The test fails when one is added silently.
var unverified = map[string][]*regexp.Regexp{
	"Pri profile": {regexp.MustCompile(`^commit$`)}, // srv, line and onu profiles show "commit" in the running config; pri has none there yet
}

// cliMode is the grammar mode a line runs in, after the lines before it.
type cliMode struct{ stack []string }

func (m *cliMode) cur() string {
	if len(m.stack) == 0 {
		return "config"
	}
	return m.stack[len(m.stack)-1]
}

func (m *cliMode) after(line string) {
	f := strings.Fields(line)
	switch {
	case len(f) == 0:
	case f[0] == "exit":
		if len(m.stack) > 0 {
			m.stack = m.stack[:len(m.stack)-1]
		}
	case f[0] == "interface" && len(f) > 1 && f[1] == "gpon":
		m.stack = append(m.stack, "slot_pon")
	case f[0] == "interface" && len(f) > 1 && f[1] == "gigabitEthernet":
		m.stack = append(m.stack, "port interface config")
	case f[0] == "interface" && len(f) > 1 && f[1] == "vlan":
		m.stack = append(m.stack, "vlan_interface")
	case f[0] == "vlan" && len(f) == 2:
		m.stack = append(m.stack, "vlan_cfg")
	case f[0] == "profile" && len(f) > 1:
		m.stack = append(m.stack, map[string]string{"dba": "DBA profile", "line": "Line profile", "srv": "Srv profile", "pri": "Pri profile", "onu": "Onu profile"}[f[1]])
	}
}

func (g grammar) match(mode, line string) string {
	for _, x := range g[mode] {
		if x.re.MatchString(" " + line) {
			return x.src
		}
	}
	return ""
}

func anyMatch(res []*regexp.Regexp, line string) bool {
	for _, re := range res {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// checkLines asserts every line of a batch (sent from configuration mode) is in the grammar of its mode.
func checkLines(t *testing.T, g grammar, what string, lines []string) {
	t.Helper()
	var m cliMode
	for _, l := range lines {
		mode := m.cur()
		switch {
		case l == "exit" && mode != "config":
		case g.match(mode, l) != "":
			t.Logf("%s: (%s) %s  <=  %s", what, mode, Redact(l), g.match(mode, l))
		case anyMatch(fromRunning[mode], l):
		case anyMatch(unverified[mode], l):
			t.Logf("%s: unverified on the bench (TODO): (%s) %s", what, mode, Redact(l))
		default:
			t.Errorf("%s: not in the bench grammar: (%s) %s", what, mode, Redact(l))
		}
		m.after(l)
	}
	if len(m.stack) != 0 {
		t.Errorf("%s: the batch does not return to configuration mode (still in %v)", what, m.stack)
	}
}

func TestGrammarConverter(t *testing.T) {
	re, ok := syntaxRE("onu add (1-64) profile NAME <sn|loid|loid+pw> WORD [WORD]")
	if !ok {
		t.Fatal("not converted")
	}
	for l, want := range map[string]bool{
		" onu add 3 profile default sn VSOL00127E0F":  true,
		" onu add 3 profile default sn":               false,
		" onu add 3 profile default serial X":         false,
		" onu add 3 profile default loid+pw a b":      true,
		" onu add 3 profile default sn A B C":         false,
		" onu add x profile default sn VSOL00127E0F":  false,
		" onu add 3 profile default sn VSOL00127E0F ": true,
	} {
		if re.MatchString(l) != want {
			t.Errorf("%q: want %v", l, want)
		}
	}
	g := loadGrammar(t)
	for _, mode := range []string{"config", "slot_pon", "Pri profile", "Line profile", "Srv profile", "port interface config", "vlan_cfg"} {
		if len(g[mode]) == 0 {
			t.Errorf("no grammar lines for mode %q", mode)
		}
	}
}

// Every line every builder emits, for a factory OLT and for the bench OLT, is in the bench grammar.
func TestDialectLinesInBenchGrammar(t *testing.T) {
	g := loadGrammar(t)
	d := VSOL()
	acs := &ACS{URL: "https://acs.example.net:7547/cwmp", User: "ftn-a1b2c3", Pass: "Pa55word99ZZ", InformSeconds: 300}
	want := Desired{InternetVLAN: 100, TR069VLAN: 99, MgmtVLAN: 98, MgmtIP: "10.7.3.2", MgmtMask: "255.255.255.248", MgmtGateway: "10.7.3.1",
		Uplink: "0/1", DBA: d.DefaultDBA, DBAMax: DefaultMaximum, LineProfile: d.DefaultLine, SrvProfile: d.DefaultSrv, PriProfile: d.DefaultPri,
		ONUProfile: d.DefaultONUProfile, ACS: acs, Provisioning: true,
		ONUs: []DesiredONU{{PON: 1, Serial: "VSOL00ABCDEF", Label: "Block A 3", Add: true}}}
	// A factory OLT: everything is made.
	factory := ParseRunning("vlan 1\nexit\ninterface gpon 0/1\nonu auto-learn\nplug_and_play enable vlan 1\nexit\n")
	p := d.BuildPlan(want, factory)
	for _, st := range p.Steps {
		checkLines(t, g, st.Title, st.Lines)
	}
	cmds := strings.Join(p.Commands(), "\n")
	for _, s := range []string{"profile dba name", "profile line name", "profile srv name", "profile pri name", "interface vlan 98", "ip route 0.0.0.0/0", "no onu auto-learn", "onu add 1 profile"} {
		if !strings.Contains(cmds, s) {
			t.Errorf("the factory plan lacks %q, so the check did not cover it", s)
		}
	}
	// Untagged internet VLAN on the uplink.
	want2 := want
	want2.InternetUntagged = true
	checkLines(t, g, "uplink untagged", d.UplinkLines("0/1", []int{99}, 100))
	// The bench OLT: ONU 1 exists with an internet WAN and gets a TR-069 WAN per ONU.
	run := benchRunning(t)
	want2.ONUs = []DesiredONU{{PON: 1, ID: 1, Serial: "VSOL00127E0F"}}
	p2 := d.BuildPlan(want2, run)
	if !strings.Contains(strings.Join(p2.Commands(), "\n"), "onu 1 pri wan_adv index 2 route mode tr069") {
		t.Fatalf("the bench plan does not add the TR-069 WAN to ONU 1:\n%s", Redact(strings.Join(p2.Commands(), "\n")))
	}
	for _, st := range p2.Steps {
		checkLines(t, g, "bench: "+st.Title, st.Lines)
	}
	// The other builders.
	checkLines(t, g, "remove", append(append(d.PONEnter(1), d.ONURemoveLine(5)), d.PONExit()...))
	checkLines(t, g, "adopt", d.AdoptSnippet(98, "10.7.3.2", "255.255.255.248", "10.7.3.1", "0/1")[2:len(d.AdoptSnippet(98, "10.7.3.2", "255.255.255.248", "10.7.3.1", "0/1"))-2])
	q := d.PONReadQuery(1)
	checkLines(t, g, "PON read", q.Cmds)
}

// The SSH restriction and the SNMP removal use only lines of the bench grammar (`find login-access-list`, V1.0.5), and
// the running configuration of the bench shows the factory lines in the same form.
func TestGrammarSecureACLAndSNMP(t *testing.T) {
	g := loadGrammar(t)
	d := VSOL()
	plan, err := d.SecureACLPlan("192.168.8.1")
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{plan.Permit, plan.Deny4, plan.Deny6, plan.Enable, "no " + plan.Deny4, "no " + plan.Deny6, "no " + plan.Permit,
		"no snmp-server community public", ShowACLCmd, ShowSNMPCmd}
	for _, l := range lines {
		if g.match("config", l) == "" {
			t.Errorf("not in the bench grammar: %q", l)
		}
		if strings.HasPrefix(l, "show ") {
			continue
		}
		if !d.SecureLineOK(l, false) {
			t.Errorf("not an exact secure shape: %q", l)
		}
	}
	// The factory lines of the bench running configuration parse as rules and are the shapes of the fixture.
	raw, err := os.ReadFile("testdata/bench-running-config.txt")
	if err != nil {
		t.Fatal(err)
	}
	st := ParseACL(string(raw), "")
	got := strings.Join(st.RuleStrings(), "|")
	if got != "deny telnet 0.0.0.0 0.0.0.0|ipv6 deny telnet ::/0|deny snmp 0.0.0.0 0.0.0.0|ipv6 deny snmp ::/0" || st.Enabled == nil || !*st.Enabled {
		t.Fatalf("the bench running configuration parsed as %q enabled=%v", got, st.Enabled)
	}
	// The bench capture of `find login-access-list`.
	for _, l := range []string{"login-access-list enable", "login-access-list disable", "login-access-list clear", "login-access-list deny telnet 0.0.0.0 0.0.0.0",
		"login-access-list ipv6 deny telnet ::/0", "no login-access-list deny snmp 0.0.0.0 0.0.0.0", "no login-access-list ipv6 deny ssh ::/0"} {
		if g.match("config", l) == "" {
			t.Errorf("not in the bench grammar: %q", l)
		}
	}
}
