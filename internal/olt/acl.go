package olt

import (
	"context"
	"errors"
	"net/netip"
	"regexp"
	"strings"

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
// captured: any line that holds permit/deny, a protocol and an address is a rule; a lone "enabled"/"disabled" is the state).
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
		if st.Enabled == nil {
			if m := enabledRE.FindString(l); m != "" {
				v := strings.HasPrefix(strings.ToLower(m), "enable")
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

func (p ACLPlan) rules() (permit, deny4, deny6 ACLRule) {
	return ACLRule{Action: "permit", Proto: "ssh", Addr: p.Source, Mask: "255.255.255.255"},
		ACLRule{Action: "deny", Proto: "ssh", Addr: "0.0.0.0", Mask: "0.0.0.0"},
		ACLRule{V6: true, Action: "deny", Proto: "ssh", Addr: "::/0"}
}

// readACL reads the running configuration and `show login-access-list`. The running configuration must be readable; the
// show command's layout is not known, so its failure is ignored.
func (s *Service) readACL(ctx context.Context, o *opened) (ACLState, error) {
	run, err := s.readRunning(ctx, o.sess)
	if err != nil {
		return ACLState{}, err
	}
	show, _ := o.sess.ShowAt(ctx, ModeConfig, ShowACLCmd)
	return ParseACL(run, show), nil
}

// secureACL restricts SSH to the router's OLT-management address. The OLT's rule order is not known, so: the permit goes in
// first, then the two denies; a NEW SSH session (it arrives from the router's address, like every session of the server)
// must then log in while the old session stays open; only then is the configuration saved. If it cannot, the denies and the
// permit are removed again through the old session, nothing is saved and the note says SSH was left open. Lines already
// present are skipped. Telnet, snmp and web rules are never touched.
func (s *Service) secureACL(ctx context.Context, o *opened, user, login, enable, serial, by string, res *SecureResult) {
	dl := s.o.Dialect
	label := store.OLTLabel(o.e)
	left := func(why string) {
		res.ACLNote = "Couldn't restrict SSH; left open. " + why
		s.auditNote(o.e.ID, by, "OLT "+label+": SSH was not restricted (left open): "+why)
	}
	if s.o.SSHSource == nil {
		res.ACLNote = "SSH is not restricted: the server does not know which address the OLT sees its sessions from. The OLT is only reachable through the router's OLT management port."
		return
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
		return
	}
	plan, err := dl.SecureACLPlan(src)
	if err != nil {
		left("The router's address toward the OLT cannot be used (" + errText(err) + "). Nothing was changed.")
		return
	}
	before, err := s.readACL(ctx, o)
	if err != nil {
		left("The access list could not be read (" + errText(err) + "). Nothing was changed.")
		return
	}
	pr, d4, d6 := plan.rules()
	var send, undo []string
	if !before.Has(pr) {
		send, undo = append(send, plan.Permit), append([]string{"no " + plan.Permit}, undo...)
	}
	if !before.Has(d4) {
		send, undo = append(send, plan.Deny4), append([]string{"no " + plan.Deny4}, undo...)
	}
	if !before.Has(d6) {
		send, undo = append(send, plan.Deny6), append([]string{"no " + plan.Deny6}, undo...)
	}
	// The denies come off before the permit (undo is in reverse order of sending).
	turnedOn := false
	if before.Enabled != nil && !*before.Enabled {
		send, turnedOn = append(send, plan.Enable), true
	}
	restore := func(why string) {
		var bad []string
		for _, l := range undo {
			if err := o.sess.RunSecure(ctx, []string{l}); err != nil {
				var ce *CmdError
				if errors.As(err, &ce) && notExistRE.MatchString(ce.Output) {
					continue
				}
				bad = append(bad, l)
			}
		}
		extra := ""
		if turnedOn {
			extra = " The access list itself was switched on and stays on."
		}
		if len(bad) > 0 {
			left(why + " The rules could NOT all be removed again (" + strings.Join(bad, "; ") + "): remove them on the OLT. Nothing was saved." + extra)
			return
		}
		left(why + " The rules were removed again and nothing was saved." + extra)
	}
	if len(send) > 0 {
		if err := o.sess.RunSecure(ctx, send); err != nil {
			restore("The OLT refused a line (" + errText(err) + ").")
			return
		}
	}
	// A new session, while the old one stays open.
	if _, err := s.verifyNew(ctx, o, user, login, enable, serial); err != nil {
		if len(send) == 0 {
			left("The rules for " + plan.Source + " are already on the OLT, but a new session could not log in (" + errText(err) + "). Nothing was changed.")
			return
		}
		restore("A new SSH session from " + plan.Source + " could not log in once the rules were on (" + errText(err) + ").")
		return
	}
	// Read back: the rules must be there.
	after, err := s.readACL(ctx, o)
	if err != nil || !after.Has(pr) || !after.Has(d4) || !after.Has(d6) {
		why := "The rules could not be read back from the OLT."
		if err != nil {
			why += " (" + errText(err) + ")"
		}
		if len(send) == 0 {
			left(why + " Nothing was changed.")
			return
		}
		restore(why)
		return
	}
	if len(send) > 0 {
		if err := o.sess.Save(ctx); err != nil {
			restore("The OLT would not save its configuration (" + errText(err) + ").")
			return
		}
	}
	res.ACLDone, res.ACLRules = true, after.RuleStrings()
	if len(send) == 0 {
		res.ACLNote = "SSH was already restricted to " + plan.Source + "; a new session from it logged in."
	} else {
		res.ACLNote = "SSH is restricted to " + plan.Source + " (the router's address toward the OLT) and denied from everywhere else; a new session from it logged in before the configuration was saved."
	}
	if !after.HasRule("deny", "web") {
		res.ACLFinding = "The OLT's web interface (HTTPS) is reachable from the management network: the access list has no rule for web. The server does not change web rules."
	}
	s.auditNote(o.e.ID, by, "OLT "+label+": SSH restricted to "+plan.Source+", verified with a new session, configuration saved")
}

var notExistRE = regexp.MustCompile(`(?i)not exist|not found|no such|does not exist`)
