package olt

import (
	"context"
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"ispbill/internal/store"
)

// The login access list of the V-SOL OLT (bench, V1.0.5):
//
//	login-access-list <enable|disable|clear>
//	login-access-list <permit|deny> <telnet|web|snmp|ssh> A.B.C.D A.B.C.D        (address and MASK; any = 0.0.0.0 0.0.0.0)
//	login-access-list ipv6 <permit|deny> <telnet|web|snmp|ssh> X:X::X:X/M
//	no login-access-list ...   show login-access-list
//
// The factory configuration denies telnet and snmp from everywhere and has the list enabled.

// ACLRule is one rule of the list.
type ACLRule struct {
	V6            bool
	Action, Proto string // permit|deny, telnet|web|snmp|ssh
	Addr, Mask    string // IPv6: Addr is the prefix ("::/0"), Mask is empty
}

// String is the rule as the running configuration prints it, without the leading "login-access-list ".
func (r ACLRule) String() string {
	s := r.Action + " " + r.Proto + " " + r.Addr
	if r.Mask != "" {
		s += " " + r.Mask
	}
	if r.V6 {
		s = "ipv6 " + s
	}
	return s
}

// ACLState is what the OLT shows: its rules and whether the list is on (nil = not shown).
type ACLState struct {
	Rules   []ACLRule
	Enabled *bool
}

var (
	aclProtos = map[string]bool{"telnet": true, "web": true, "snmp": true, "ssh": true}
	enabledRE = regexp.MustCompile(`(?i)\b(enabled?|disabled?)\b`)
)

func parseACLRule(line string) (ACLRule, bool) {
	var r ACLRule
	var v4 []string
	for _, t := range strings.Fields(strings.ToLower(line)) {
		t = strings.Trim(t, ",;")
		switch {
		case t == "no":
			return r, false
		case t == "ipv6":
			r.V6 = true
		case t == "permit" || t == "deny":
			if r.Action == "" {
				r.Action = t
			}
		case aclProtos[t]:
			if r.Proto == "" {
				r.Proto = t
			}
		case strings.Contains(t, ":"):
			r.V6, r.Addr = true, t
		default:
			if a, err := netip.ParseAddr(t); err == nil && a.Is4() {
				v4 = append(v4, t)
			}
		}
	}
	if r.Action == "" || r.Proto == "" {
		return r, false
	}
	if r.V6 {
		return r, r.Addr != ""
	}
	if len(v4) == 0 {
		return r, false
	}
	r.Addr = v4[0]
	if len(v4) > 1 {
		r.Mask = v4[1]
	}
	return r, true
}

// ParseACL reads the list from the running configuration (exact lines) and from `show login-access-list` (layout not
// captured: any line that holds permit/deny, a protocol and an address is a rule). The state (on/off) is the running
// configuration's `login-access-list enable|disable`; of the show output only a "disabled" counts (nil = not known).
// Rules found in both are counted once. Unreadable text gives an empty state, never an error.
func ParseACL(running, show string) ACLState {
	var st ACLState
	seen := map[string]bool{}
	add := func(r ACLRule) {
		if !seen[r.String()] {
			seen[r.String()] = true
			st.Rules = append(st.Rules, r)
		}
	}
	for _, l := range strings.Split(running, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "login-access-list") {
			continue
		}
		f := strings.Fields(l)
		if len(f) == 2 && (f[1] == "enable" || f[1] == "disable") {
			v := f[1] == "enable"
			st.Enabled = &v
			continue
		}
		if r, ok := parseACLRule(l); ok {
			add(r)
		}
	}
	for _, l := range strings.Split(show, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "%") {
			continue
		}
		if r, ok := parseACLRule(l); ok {
			add(r)
			continue
		}
		// Only "disabled" is taken from the show output (its layout is not known): a word "enabled" in it could belong to
		// something else, and a list wrongly taken as on would leave SSH unrestricted while the step reports it restricted.
		// "Disabled" read wrongly only makes the step send `login-access-list enable`, which the readback then checks.
		if st.Enabled == nil {
			if m := enabledRE.FindString(l); m != "" && strings.HasPrefix(strings.ToLower(m), "disable") {
				v := false
				st.Enabled = &v
			}
		}
	}
	return st
}

// Has reports whether the exact rule is in the list.
func (a ACLState) Has(r ACLRule) bool {
	for _, x := range a.Rules {
		if x == r {
			return true
		}
	}
	return false
}

// HasRule reports whether any rule with this action and protocol is in the list (any address).
func (a ACLState) HasRule(action, proto string) bool {
	for _, x := range a.Rules {
		if x.Action == action && x.Proto == proto {
			return true
		}
	}
	return false
}

// RuleStrings are the rules for display ("deny telnet 0.0.0.0 0.0.0.0").
func (a ACLState) RuleStrings() []string {
	out := []string{}
	for _, r := range a.Rules {
		out = append(out, r.String())
	}
	return out
}

func (p ACLPlan) rule() ACLRule {
	return ACLRule{Action: "permit", Proto: "ssh", Addr: p.Source, Mask: "255.255.255.255"}
}

// readACL reads the running configuration and `show login-access-list`. The running configuration must be readable; the
// show command's layout is not known, so its failure is ignored. It also returns the running configuration.
func (s *Service) readACL(ctx context.Context, o *opened) (ACLState, string, error) {
	run, err := s.readRunning(ctx, o.sess)
	if err != nil {
		return ACLState{}, "", err
	}
	show, _ := o.sess.ShowAt(ctx, ModeConfig, ShowACLCmd)
	return ParseACL(run, show), run, nil
}

// mixedRE is the V1600GS-R's refusal of a permit beside a deny (or the reverse) for one protocol:
// "add failed, protocol has permit rule,please detele it first!".
var mixedRE = regexp.MustCompile(`(?i)has (permit|deny) rule`)

// ipv6AddrRE: the OLT has an IPv6 address configured (an interface line `ipv6 address X/M`).
var ipv6AddrRE = regexp.MustCompile(`(?im)^\s*ipv6 address\s+[0-9a-f:]+`)

// ACLNoteDone is the note of a restriction in force (the page shows it as is).
const aclLimited = "SSH limited to the router (the OLT allows SSH only from listed addresses): "

// secureACL limits SSH to the router's OLT-management address. On the V1600GS-R a protocol's rules are either all permit
// (an allowlist: only the listed sources may log in) or all deny, and the OLT refuses to add the other kind ("add failed,
// protocol has permit rule"). So the step sends only `login-access-list permit ssh <router> 255.255.255.255`, and switches
// the list on last (only when it was off); a NEW SSH session (it arrives from the router's address, like every session of
// the server) must then log in while the old session stays open; only then is the configuration saved. If it cannot, the
// list is switched off again (if this step switched it on) and the permit removed again through the old session, nothing
// is saved and the note says SSH was left open. If even that fails (the old session broke), nothing was saved, so a power
// cycle of the OLT undoes the rule: the owners are alerted. An OLT whose list already denies SSH by rule (deny-list mode)
// is left as it is: a permit cannot be added beside a deny, and the server does not remove the owner's rules. Telnet,
// snmp and web rules are never touched. No IPv6 line is sent (a deny is not possible beside the permit); when the OLT has
// an IPv6 address, that SSH over IPv6 is not limited is a finding.
//
// saved reports that this step ran the save (which also saves whatever earlier steps of the same session changed). stuck
// reports that its rules could not all be undone: the running configuration then holds unverified rules, and nothing more
// may be saved in this session (a power cycle is the way back).
//
// The step runs on its own time budget, detached from the caller's: once a line is sent, the verification and the undo
// must not be cut short by the web request's deadline.
func (s *Service) secureACL(pctx context.Context, o *opened, user, login, enable, serial, by string, res *SecureResult) (saved, stuck bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(pctx), aclStepBudget)
	defer cancel()
	dl := s.o.Dialect
	label := store.OLTLabel(o.e)
	left := func(why string) {
		res.ACLNote = "Couldn't restrict SSH; left open. " + why
		s.auditNote(o.e.ID, by, "OLT "+label+": SSH was not restricted (left open): "+why)
	}
	if s.o.SSHSource == nil {
		res.ACLNote = "SSH is not restricted: the server does not know which address the OLT sees its sessions from. The OLT is only reachable through the router's OLT management port."
		return false, stuck
	}
	var src string
	var serr error
	s.o.Store.View(func(d *store.Data) {
		e, ok := d.Equipment[o.e.ID]
		if !ok {
			serr = ErrNotFound
			return
		}
		src, serr = s.o.SSHSource(d, e, d.OLTStateOf(e.ID).Settings)
	})
	if serr != nil {
		left("The router's address toward the OLT is not known (" + errText(serr) + "). Nothing was changed.")
		return false, stuck
	}
	plan, err := dl.SecureACLPlan(src)
	if err != nil {
		left("The router's address toward the OLT cannot be used (" + errText(err) + "). Nothing was changed.")
		return false, stuck
	}
	before, _, err := s.readACL(ctx, o)
	if err != nil {
		left("The access list could not be read (" + errText(err) + "). Nothing was changed.")
		return false, stuck
	}
	if before.Enabled == nil {
		// Without it the step could add rules to a list that is off and report SSH restricted.
		left("The OLT's configuration does not show whether the access list is on (no `login-access-list enable` or `disable` line). Nothing was changed.")
		return false, stuck
	}
	if before.HasRule("deny", "ssh") {
		left("The OLT's access list already denies SSH by rule (" + strings.Join(sshRules(before, "deny"), "; ") + "). This OLT keeps a service's rules either all permit or all deny, so the server cannot add its permit for " + plan.Source + " beside them, and it does not remove the owner's rules. Nothing was changed.")
		return false, stuck
	}
	turnOn := !*before.Enabled
	if turnOn && (before.HasRule("deny", "web") || before.HasRule("permit", "web")) {
		// Switching the list on would bring the owner's web rules into force, and the new SSH session cannot check those.
		left("The access list is off and holds rules for the web interface; switching it on could lock the web interface out. Nothing was changed.")
		return false, stuck
	}
	pr := plan.rule()
	var send []string
	if !before.Has(pr) {
		send = append(send, plan.Permit)
	}
	if turnOn {
		send = append(send, plan.Enable)
	}
	// restore undoes the first n lines of send, in reverse order: the list goes off first (if this step switched it on),
	// then the permit comes off. It runs on a fresh budget of its own.
	restore := func(n int, why string) {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(pctx), aclRestoreBudget)
		defer rcancel()
		var bad []string
		for k := n - 1; k >= 0; k-- {
			l := "no " + send[k]
			if send[k] == plan.Enable {
				l = plan.Disable
			}
			if err := o.sess.RunRestore(rctx, []string{l}); err != nil {
				var ce *CmdError
				if errors.As(err, &ce) && notExistRE.MatchString(ce.Output) {
					continue
				}
				bad = append(bad, l)
			}
		}
		if len(bad) > 0 {
			stuck = true
			left(why + " The rules could NOT all be removed again (" + strings.Join(bad, "; ") + "), and the server may be locked out of the OLT's SSH. Nothing was saved: power-cycle the OLT to undo the rules (do not save its configuration from the web interface first).")
			if s.o.Alert != nil {
				s.o.Alert("OLT "+label+": power-cycle the OLT to undo the SSH access list",
					clip("Secure this OLT added SSH access-list rules that could not be verified or removed again ("+strings.Join(bad, "; ")+"). The server may be unable to log in to the OLT. Nothing was saved: power-cycle the OLT to undo them, and do not save its configuration from the web interface before that.", 900))
			}
			return
		}
		extra := ""
		if turnOn && n == len(send) {
			extra = " The access list was switched off again, as it was before."
		}
		left(why + " The rules were removed again and nothing was saved." + extra)
	}
	if len(send) > 0 {
		if err := o.sess.RunSecure(ctx, send); err != nil {
			// Undo the lines before the refused one (a refused line changed nothing); when it is not known where it stopped
			// (the session broke), all of them.
			n := len(send)
			var ce *CmdError
			if errors.As(err, &ce) {
				for k, l := range send {
					if l == ce.Cmd {
						n = k
						break
					}
				}
				if mixedRE.MatchString(ce.Output) {
					restore(n, "The OLT refused the permit because the list already holds the other kind of SSH rule ("+errText(err)+"); this OLT keeps a service's rules either all permit or all deny.")
					return false, stuck
				}
			}
			restore(n, "The OLT refused a line ("+errText(err)+").")
			return false, stuck
		}
	}
	// A new session, while the old one stays open.
	if _, err := s.verifyNew(ctx, o, user, login, enable, serial); err != nil {
		if len(send) == 0 {
			left("The rule for " + plan.Source + " is already on the OLT, but a new session could not log in (" + errText(err) + "). Nothing was changed.")
			return false, stuck
		}
		restore(len(send), "A new SSH session from "+plan.Source+" could not log in once the rule was on ("+errText(err)+").")
		return false, stuck
	}
	// Read back: the permit must be there, no SSH deny beside it, and the list on.
	after, run, err := s.readACL(ctx, o)
	if err != nil || !after.Has(pr) || after.HasRule("deny", "ssh") || after.Enabled == nil || !*after.Enabled {
		why := "The rule (and the list switched on) could not be read back from the OLT."
		if err != nil {
			why += " (" + errText(err) + ")"
		}
		if len(send) == 0 {
			left(why + " Nothing was changed.")
			return false, stuck
		}
		restore(len(send), why)
		return false, stuck
	}
	if len(send) > 0 {
		if err := o.sess.Save(ctx); err != nil {
			// The running rules are undone. If the save did complete despite the error, what it saved is the restriction a
			// new session was just verified with.
			restore(len(send), "The OLT would not save its configuration ("+errText(err)+").")
			return false, stuck
		}
		saved = true
	}
	res.ACLDone, res.ACLRules = true, after.RuleStrings()
	if len(send) == 0 {
		res.ACLNote = aclLimited + plan.Source + " was already the listed address; a new session from it logged in."
	} else {
		res.ACLNote = aclLimited + plan.Source + " (the router's address toward the OLT); a new session from it logged in before the configuration was saved."
	}
	if others := otherPermits(after, pr); len(others) > 0 {
		res.ACLNote += " SSH is also allowed from rules the server did not add: " + strings.Join(others, "; ") + "."
	}
	var finding []string
	if !after.HasRule("deny", "web") {
		finding = append(finding, "The OLT's web interface (HTTPS) is reachable from the management network: the access list has no rule for web. The server does not change web rules.")
	}
	if ipv6AddrRE.MatchString(run) {
		finding = append(finding, "IPv6 SSH is not restricted: the OLT has an IPv6 address, and the list's SSH rule is for IPv4 only (a deny cannot be added beside a permit on this OLT).")
	}
	res.ACLFinding = strings.Join(finding, " ")
	s.auditNote(o.e.ID, by, "OLT "+label+": SSH limited to "+plan.Source+" (permit-only list), verified with a new session, configuration saved")
	return saved, false
}

// sshRules are the list's SSH rules of one action, for a note.
func sshRules(a ACLState, action string) []string {
	var out []string
	for _, r := range a.Rules {
		if r.Proto == "ssh" && r.Action == action {
			out = append(out, r.String())
		}
	}
	return out
}

// otherPermits are the SSH permits other than the server's own.
func otherPermits(a ACLState, own ACLRule) []string {
	var out []string
	for _, r := range a.Rules {
		if r.Proto == "ssh" && r.Action == "permit" && r != own {
			out = append(out, r.String())
		}
	}
	return out
}

// The SSH restriction's own time budgets (see secureACL).
var (
	aclStepBudget    = 5 * time.Minute // the lines, a verification session and a save (up to Dialect.SaveTimeout)
	aclRestoreBudget = time.Minute
)

var notExistRE = regexp.MustCompile(`(?i)not exist|not found|no such|does not exist`)
