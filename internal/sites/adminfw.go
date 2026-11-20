package sites

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The admin LAN has no internet. Someone who plugs into the admin port can manage the router (WinBox,
// WebFig, SSH, DNS and ping all arrive through the input chain, which is untouched) and can open the
// billing site and the WebFig site of the server, and nothing else: in the forward chain, at the very top
// (above every established/related accept and above the walled-garden rules), the router carries
//
//  1. ftn: admin allow billing site   accept br-lan -> WAN, tcp 80,443, to the address list ftn-admin-allow
//     (new, established, related)
//  2. ftn: admin no internet          drop   br-lan -> WAN
//  3. ftn: admin net no internet      drop   src <admin LAN subnet> -> WAN (belt and braces)
//
// The address list holds the server's own names (DOMAIN = the host of PUBLIC_BASE_URL, WEBFIG_DOMAIN when
// set) as FQDN entries, which RouterOS resolves and keeps current by itself, and the server's public IP as
// a fallback. The masquerade of the admin LAN ("ftn: lan masquerade") stays: it only ever sees what rule 1
// lets through, and without it that traffic could not come back. Customer and router-own traffic, the
// WireGuard tunnel and remote WinBox do not match any of these rules.
const (
	AdminAllowList      = "ftn-admin-allow"
	adminAllowPrefix    = "ftn: admin allow "
	AdminAllowComment   = "ftn: admin allow billing site"
	AdminDropComment    = "ftn: admin no internet"
	AdminNetDropComment = "ftn: admin net no internet"
)

// AdminAllowEntry is one address of the admin allow list.
type AdminAllowEntry struct{ Address, Comment string }

// adminAllowEntries are the names (and the IP) the admin LAN may reach, from the server's configuration.
func (m *Manager) adminAllowEntries() []AdminAllowEntry {
	return AdminAllowFor(m.cfg.PublicBaseURL, m.cfg.WebFigDomain, m.cfg.PublicIP)
}

// AdminAllowFor is the allow list for a server: the host of its public base URL (DOMAIN), the WebFig domain
// when there is one, and the public IP as a fallback.
func AdminAllowFor(publicBase, webfigDomain, publicIP string) []AdminAllowEntry {
	var hosts []string
	if u, err := url.Parse(publicBase); err == nil && u.Hostname() != "" {
		hosts = append(hosts, strings.ToLower(u.Hostname()))
	}
	if webfigDomain != "" {
		hosts = append(hosts, strings.ToLower(webfigDomain))
	}
	if publicIP != "" {
		hosts = append(hosts, publicIP)
	}
	return AdminAllowEntries(hosts)
}

// AdminAllowEntries dedupes hosts into list entries, each with its own comment.
func AdminAllowEntries(hosts []string) []AdminAllowEntry {
	seen := map[string]bool{}
	var out []AdminAllowEntry
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] || strings.ContainsAny(h, "\"$\\ ,;") {
			continue
		}
		seen[h] = true
		out = append(out, AdminAllowEntry{Address: h, Comment: adminAllowPrefix + h})
	}
	return out
}

// adminRuleSet is the three forward rules, top to bottom. lanNet is the admin LAN network.
func adminRuleSet(lanNet string) []ipoeRule {
	f := func(args, comment string) ipoeRule { return newIpoeRule("filter", args, comment) }
	return []ipoeRule{
		f("chain=forward action=accept in-interface="+LANBridge+" out-interface-list=WAN protocol=tcp dst-port=80,443 dst-address-list="+AdminAllowList+" connection-state=new,established,related", AdminAllowComment),
		f("chain=forward action=drop in-interface="+LANBridge+" out-interface-list=WAN", AdminDropComment),
		f("chain=forward action=drop src-address="+lanNet+" out-interface-list=WAN", AdminNetDropComment),
	}
}

func adminComments(rules []ipoeRule) map[string]bool {
	m := map[string]bool{}
	for _, r := range rules {
		m[r.Comment] = true
	}
	return m
}

// setProp: properties whose value is a comma list the router may reorder.
func setProp(k string) bool { return k == "connection-state" || k == "dst-port" }

func sameAdminValue(k, cur, want string) bool {
	if setProp(k) {
		a, b := strings.Split(cur, ","), strings.Split(want, ",")
		sort.Strings(a)
		sort.Strings(b)
		return strings.Join(a, ",") == strings.Join(b, ",")
	}
	return sameValue(k, cur, want)
}

// adminRuleDiff is the properties of r that differ from the wanted rule.
func adminRuleDiff(row routeros.Row, want ipoeRule) map[string]string {
	diff := map[string]string{}
	for k, v := range want.Body("") {
		cur, has := row[k]
		if has && sameAdminValue(k, cur, v) {
			continue
		}
		diff[k] = v
	}
	if rowDisabled(row) {
		diff["disabled"] = "no"
	}
	return diff
}

// bypassesAdmin: an active static forward rule that accepts traffic and sits above the admin rules.
func bypassesAdmin(r routeros.Row, ours map[string]bool) bool {
	if r["chain"] != "forward" || yes(r["dynamic"]) || rowDisabled(r) || ours[r["comment"]] {
		return false
	}
	switch r["action"] {
	case "accept", "fasttrack-connection", "passthrough", "jump":
		return true
	}
	return false
}

type adminDiff struct {
	missing, wrong, stale []string
	order                 bool
	problems              []string
}

func (d adminDiff) ok() bool {
	return len(d.missing) == 0 && len(d.wrong) == 0 && len(d.stale) == 0 && !d.order
}

func (d adminDiff) text() string {
	var p []string
	if len(d.missing) > 0 {
		p = append(p, fmt.Sprintf("%d missing", len(d.missing)))
	}
	if len(d.wrong) > 0 {
		p = append(p, fmt.Sprintf("%d changed", len(d.wrong)))
	}
	if len(d.stale) > 0 {
		p = append(p, fmt.Sprintf("%d old allow entr(ies)", len(d.stale)))
	}
	if d.order {
		p = append(p, "not at the top of the forward chain")
	}
	return strings.Join(p, ", ")
}

// adminAllowRows are the static rows of the allow list as read (dynamic rows are the resolved addresses).
func adminAllowRows(rows []routeros.Row) []routeros.Row {
	var out []routeros.Row
	for _, r := range rows {
		if r["list"] == AdminAllowList && !yes(r["dynamic"]) {
			out = append(out, r)
		}
	}
	return out
}

func diffAdmin(sn *snapshot, rules []ipoeRule, entries []AdminAllowEntry) adminDiff {
	var d adminDiff
	rows := sn.tbl("/ip/firewall/filter")
	ours := adminComments(rules)
	next := -1
	firstOurs := -1
	for _, want := range rules {
		var hits []int
		for i, r := range rows {
			if r["comment"] == want.Comment {
				hits = append(hits, i)
			}
		}
		switch {
		case len(hits) == 0:
			d.missing = append(d.missing, want.Comment)
			continue
		case len(hits) > 1:
			d.wrong = append(d.wrong, want.Comment+" (twice)")
		}
		if len(adminRuleDiff(rows[hits[0]], want)) > 0 {
			d.wrong = append(d.wrong, want.Comment)
		}
		if hits[0] < next {
			d.order = true
		}
		next = hits[0]
		if firstOurs < 0 {
			firstOurs = hits[0]
		}
	}
	if firstOurs >= 0 {
		for i := 0; i < firstOurs; i++ {
			if bypassesAdmin(rows[i], ours) {
				d.order = true
				d.problems = append(d.problems, "an accept rule sits above the admin rules")
				break
			}
		}
	}
	have := map[string]routeros.Row{}
	for _, r := range adminAllowRows(sn.tbl("/ip/firewall/address-list")) {
		if !strings.HasPrefix(r["comment"], adminAllowPrefix) {
			continue
		}
		have[r["comment"]] = r
	}
	want := map[string]bool{}
	for _, e := range entries {
		want[e.Comment] = true
		r, ok := have[e.Comment]
		switch {
		case !ok:
			d.missing = append(d.missing, e.Comment)
		case !strings.EqualFold(r["address"], e.Address) || rowDisabled(r):
			d.wrong = append(d.wrong, e.Comment)
		}
	}
	for c := range have {
		if !want[c] {
			d.stale = append(d.stale, c)
		}
	}
	// a static row of the list that is not one of ours (no comment) is left alone
	sort.Strings(d.stale)
	return d
}

func evalAdminNet(e *evalCtx, it *CheckItem) {
	lan, err := PlanLAN(e.site.LANSubnet)
	if err != nil {
		it.Status = StatusUnknown
		it.Detail = "The admin LAN subnet is not valid: " + err.Error() + "."
		return
	}
	hasLAN := false
	for _, b := range e.sn.tbl("/interface/bridge") {
		if b["name"] == LANBridge {
			hasLAN = true
		}
	}
	if !hasLAN { // a rule that names a bridge the router does not have is refused by RouterOS
		it.Status = StatusInfo
		it.Detail = "The router has no admin LAN bridge (" + LANBridge + "), so there is no admin port to keep off the internet."
		return
	}
	d := diffAdmin(e.sn, adminRuleSet(lan.Network), e.m.adminAllowEntries())
	if d.ok() {
		okItem(it, "The admin port reaches the router and the billing site only; the rest of the internet is blocked.")
		return
	}
	it.Status = StatusFail
	it.Evidence = append(it.Evidence, d.missing...)
	it.Evidence = append(it.Evidence, d.wrong...)
	it.Evidence = append(it.Evidence, d.stale...)
	it.Evidence = append(it.Evidence, d.problems...)
	it.Detail = "The firewall rules that keep the admin port off the internet are not as the server writes them (" + d.text() + "), so someone plugged into the admin port may get free internet."
	it.Fix = CheckFix{Kind: FixAuto, Label: "Repair the admin port rules", Role: store.RoleOperator}
}

// fixAdminNet puts the allow list and the three rules right. The list goes first (so the accept is never
// ahead of its names). Rules whose properties changed are set in place; when a rule is missing, doubled or
// not at the top, a complete new copy goes in above the first static forward rule and only then do the old
// rows come out, so the admin port is never open in between.
func fixAdminNet(ctx context.Context, fe *fixEnv) error {
	lan, err := PlanLAN(fe.site.LANSubnet)
	if err != nil {
		return changed("%v", err)
	}
	rules := adminRuleSet(lan.Network)
	entries := fe.m.adminAllowEntries()
	sn, err := fe.freshRead(ctx, nil, []string{"/ip/firewall/filter", "/ip/firewall/address-list", "/interface/bridge"})
	if err != nil {
		return err
	}
	hasLAN := false
	for _, b := range sn.tbl("/interface/bridge") {
		if b["name"] == LANBridge {
			hasLAN = true
		}
	}
	if !hasLAN { // RouterOS refuses a rule that names an interface that does not exist
		return changed("the router has no %s", LANBridge)
	}
	fe.w.adminRules = adminComments(rules)
	fe.w.adminList = true
	if diffAdmin(sn, rules, entries).ok() {
		return nil
	}
	// 1. the list
	have := map[string]routeros.Row{}
	for _, r := range adminAllowRows(sn.tbl("/ip/firewall/address-list")) {
		if strings.HasPrefix(r["comment"], adminAllowPrefix) {
			have[r["comment"]] = r
		}
	}
	want := map[string]bool{}
	for _, en := range entries {
		want[en.Comment] = true
		r, ok := have[en.Comment]
		switch {
		case !ok:
			if err := fe.w.addRow(ctx, "/ip/firewall/address-list", map[string]string{"list": AdminAllowList, "address": en.Address, "comment": en.Comment}); err != nil {
				return err
			}
			fe.w.note("added %s to the admin allow list", en.Address)
		case !strings.EqualFold(r["address"], en.Address) || rowDisabled(r):
			if err := fe.w.setRow(ctx, "/ip/firewall/address-list", r, map[string]string{"address": en.Address, "disabled": "no"}); err != nil {
				return err
			}
			fe.w.note("corrected %s in the admin allow list", en.Address)
		}
	}
	for c, r := range have {
		if !want[c] {
			if err := fe.w.removeRow(ctx, "/ip/firewall/address-list", r); err != nil {
				return err
			}
			fe.w.note("removed %s from the admin allow list", r["address"])
		}
	}
	// 2. the rules
	rows := sn.tbl("/ip/firewall/filter")
	d := diffAdmin(sn, rules, nil)
	var old []routeros.Row
	for _, r := range rows {
		if fe.w.adminRules[r["comment"]] {
			old = append(old, r)
		}
	}
	rebuild := d.order || len(d.missing) > 0 || len(old) != len(rules)
	if !rebuild {
		for _, want := range rules {
			for _, r := range old {
				if r["comment"] != want.Comment {
					continue
				}
				if diff := adminRuleDiff(r, want); len(diff) > 0 {
					if err := fe.w.setRow(ctx, "/ip/firewall/filter", r, diff); err != nil {
						return err
					}
					fe.w.note("repaired the rule %q", want.Comment)
				}
			}
		}
		return nil
	}
	anchor := ""
	for _, r := range rows {
		if r["chain"] == "forward" && !yes(r["dynamic"]) && r[routeros.RowID] != "" && !fe.w.adminRules[r["comment"]] {
			anchor = r[routeros.RowID]
			break
		}
	}
	next := anchor
	for i := len(rules) - 1; i >= 0; i-- {
		body := rules[i].Body("")
		if next != "" {
			body["place-before"] = next
		}
		id, err := fe.w.addRowID(ctx, "/ip/firewall/filter", body)
		if err != nil {
			return err
		}
		if id != "" {
			next = id
		}
	}
	for _, r := range old {
		if err := fe.w.removeRow(ctx, "/ip/firewall/filter", r); err != nil {
			return err
		}
	}
	fe.w.note("put the admin port rules at the top of the forward chain")
	return nil
}
