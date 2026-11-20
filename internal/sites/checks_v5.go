package sites

import (
	"context"
	"fmt"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The checks and fixes that belong to the bootstrap script's own objects: the
// walled garden, the customer DHCP server, the way out to the internet and the
// script files.

// walledIface is the customer interface the walled rules are written for.
func (e *evalCtx) walledIface() string {
	if e.cust.found {
		return e.cust.iface
	}
	return CustomerBridge
}

// ---- walled garden -------------------------------------------------------------------

// walledProblems compares the table rows with the walled-garden rule set.
type walledDiff struct {
	missing, wrong, stale []string
	order                 bool // some group is out of order
}

func (d walledDiff) ok() bool {
	return len(d.missing) == 0 && len(d.wrong) == 0 && len(d.stale) == 0 && !d.order
}

func (d walledDiff) text() string {
	var parts []string
	if len(d.missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d rule(s) missing", len(d.missing)))
	}
	if len(d.wrong) > 0 {
		parts = append(parts, fmt.Sprintf("%d rule(s) changed", len(d.wrong)))
	}
	if len(d.stale) > 0 {
		parts = append(parts, fmt.Sprintf("%d old rule(s) left over", len(d.stale)))
	}
	if d.order {
		parts = append(parts, "the rules are out of order")
	}
	return strings.Join(parts, ", ")
}

type ruleGroup struct {
	path  string
	want  []ipoeRule
	chain string // "forward", "input" or "" (NAT: the whole table)
}

func groupsOf(w walled) []ruleGroup {
	return []ruleGroup{
		{"/ip/firewall/filter", w.Forward, "forward"},
		{"/ip/firewall/filter", w.Input, "input"},
		{"/ip/firewall/nat", w.NAT, ""},
	}
}

func walledComments(w walled) map[string]bool {
	m := map[string]bool{}
	for _, r := range w.All() {
		m[r.Comment] = true
	}
	return m
}

func staleWalled(rows []routeros.Row, known map[string]bool) []routeros.Row {
	var out []routeros.Row
	for _, r := range rows {
		c := r["comment"]
		if strings.HasPrefix(c, "ftn: ipoe") && !known[c] && !strings.HasPrefix(c, "ftn: ipoe v6") {
			out = append(out, r)
		}
	}
	return out
}

func diffWalled(sn *snapshot, w walled, iface string) walledDiff {
	var d walledDiff
	known := walledComments(w)
	for _, g := range groupsOf(w) {
		rows := sn.tbl(g.path)
		next := 0
		for _, r := range g.want {
			var hit []routeros.Row
			pos := -1
			for i, row := range rows {
				if row["comment"] == r.Comment {
					hit = append(hit, row)
					if pos < 0 {
						pos = i
					}
				}
			}
			switch {
			case len(hit) == 0:
				d.missing = append(d.missing, r.Comment)
			case len(hit) > 1:
				d.wrong = append(d.wrong, r.Comment+" (twice)")
			default:
				if diff := diffRow(hit[0], r.Body(iface)); len(diff) > 0 {
					d.wrong = append(d.wrong, r.Comment)
				}
				if pos < next {
					d.order = true
				} else {
					next = pos + 1
				}
			}
		}
		for _, r := range staleWalled(rows, known) {
			d.stale = append(d.stale, r["comment"])
		}
	}
	// the same stale row is seen once per group that reads its table
	d.stale = uniq(d.stale)
	return d
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) walledSet(site store.Site) (walled, error) {
	plan, err := PlanSubnet(site.CustomerSubnet)
	if err != nil {
		return walled{}, err
	}
	if m.cfg.PublicIP == "" {
		return walled{}, fmt.Errorf("PUBLIC_IP is not set on the server")
	}
	return walledRules(plan, m.cfg.PublicIP, site.RESTPort), nil
}

func evalWalled(e *evalCtx, it *CheckItem) {
	w, err := e.m.walledSet(e.site)
	if err != nil {
		it.Status = StatusUnknown
		it.Detail = "The walled-garden rules cannot be worked out: " + err.Error() + "."
		return
	}
	d := diffWalled(e.sn, w, e.walledIface())
	if d.ok() {
		okItem(it, "Unregistered devices and houses with no paid time can only reach DNS and the payment page.")
		return
	}
	it.Status = StatusFail
	it.Evidence = append(it.Evidence, d.missing...)
	it.Evidence = append(it.Evidence, d.wrong...)
	it.Evidence = append(it.Evidence, d.stale...)
	it.Detail = "The firewall rules that wall unregistered devices and unpaid houses are not as the server writes them (" + d.text() + "), so someone may be online without paying."
	it.Fix = CheckFix{Kind: FixAuto, Label: "Repair the walled-garden rules", Role: store.RoleOperator}
}

// evalWalledDNS warns about the one thing the walled garden cannot close on
// RouterOS: a walled flat (unpaid, suspended or unregistered) asks the router
// for DNS, and the router answers for every name. A DNS tunnel (dnstt, iodine)
// through those answers gives slow free internet. RouterOS has no per-client DNS
// policy: /ip/dns/static FWD entries add names, they do not remove the others,
// and a layer-7 regexp on DNS payloads is CPU-heavy and cannot be tested here,
// so the server leaves DNS as it is and says so.
func evalWalledDNS(e *evalCtx, it *CheckItem) {
	v := e.sn.single("/ip/dns")["allow-remote-requests"]
	if !yes(v) {
		okItem(it, "The router does not answer DNS for customers at all (see the DNS check).")
		return
	}
	it.Evidence = append(it.Evidence, "allow-remote-requests: "+v)
	it.Status = StatusWarn
	it.Detail = "A walled house (unpaid, suspended or not registered) can still look up any name through the router, which is what makes the payment page work. " +
		"Someone who knows how could tunnel traffic through those lookups (dnstt, iodine) for slow free internet. RouterOS cannot limit DNS to a few names per customer, " +
		"so nothing is changed on the router. If a walled house shows heavy DNS traffic, suspend it and ask the tenant to pay."
}

// ---- customer DHCP server ------------------------------------------------------------

func evalDHCP(e *evalCtx, it *CheckItem) {
	for _, r := range e.sn.tbl("/ip/dhcp-server") {
		if r["name"] != billing.DHCPServerName {
			continue
		}
		it.Evidence = append(it.Evidence, fmt.Sprintf("%s on %s", r["name"], r["interface"]))
		if rowDisabled(r) {
			it.Status = StatusFail
			it.Detail = "The customer DHCP server (" + billing.DHCPServerName + ") exists but is switched off, so no house gets an address."
			it.Fix = CheckFix{Kind: FixAuto, Label: "Turn the customer DHCP server on", Role: store.RoleOperator}
			return
		}
		okItem(it, "The customer DHCP server ("+billing.DHCPServerName+") is on.")
		return
	}
	it.Status = StatusFail
	it.Detail = "The router has no customer DHCP server (" + billing.DHCPServerName + "). The set-up line builds it: paste a new line on the router."
	it.Fix = CheckFix{Kind: FixNone}
}

// ---- the way out ----------------------------------------------------------------------

func evalWAN(e *evalCtx, it *CheckItem) {
	for _, r := range e.sn.tbl("/ip/route") {
		if r["dst-address"] == "0.0.0.0/0" && yes(r["active"]) && !rowDisabled(r) {
			okItem(it, "The router has a default route"+viaOf(r)+".", "default route "+viaOf(r))
			return
		}
	}
	it.Status = StatusFail
	it.Detail = "The router has no active default route, so it cannot reach the internet. Check the WAN cable and the WAN settings (" + e.site.WANMode + " on " + e.site.WANInterface + ") of this router in the billing admin."
	it.Fix = CheckFix{Kind: FixNone}
}

func viaOf(r routeros.Row) string {
	if g := firstOf(r["gateway"], r["immediate-gw"]); g != "" {
		return " via " + g
	}
	return ""
}

// ---- script files ---------------------------------------------------------------------

func bootFiles(rows []routeros.Row) []routeros.Row {
	var out []routeros.Row
	for _, r := range rows {
		if n := r["name"]; n == SetupFile || n == ResetFile {
			out = append(out, r)
		}
	}
	return out
}

func evalBootFile(e *evalCtx, it *CheckItem) {
	files := bootFiles(e.sn.tbl("/file"))
	if len(files) == 0 {
		okItem(it, "No set-up script is left on the router.")
		return
	}
	for _, f := range files {
		it.Evidence = append(it.Evidence, "file "+f["name"])
	}
	if !e.m.scriptFinished(e.site.ID) {
		it.Status = StatusInfo
		it.Detail = "The set-up script is still on the router; it removes itself when it finishes."
		return
	}
	it.Status = StatusWarn
	it.Detail = "A set-up script (it holds the router's keys) is still on the router although it has finished."
	it.Fix = CheckFix{Kind: FixAuto, Label: "Remove the script file", Role: store.RoleOperator}
}

// scriptFinished: the script's report has arrived, or it was fetched so long
// ago that it cannot still be running.
func (m *Manager) scriptFinished(siteID string) bool {
	var used, rep bool
	var at, usedAt = m.now(), m.now()
	m.st.View(func(d *store.Data) {
		s := d.Sites[siteID]
		usedAt = s.Join.UsedAt
		used = !s.Join.UsedAt.IsZero()
	})
	if !used {
		return true // nothing was fetched: a stale file of an older attempt
	}
	if r, ok := m.JoinReport(siteID); ok {
		rep, at = true, r.At
	}
	return (rep && !at.Before(usedAt)) || m.now().Sub(usedAt) > ReportWindow
}

// ---- fixes ------------------------------------------------------------------------------

func fixDHCP(ctx context.Context, fe *fixEnv) error {
	sn, err := fe.freshRead(ctx, nil, []string{"/ip/dhcp-server"})
	if err != nil {
		return err
	}
	for _, r := range sn.tbl("/ip/dhcp-server") {
		if r["name"] != billing.DHCPServerName {
			continue
		}
		if !rowDisabled(r) {
			return nil
		}
		if err := fe.w.setRow(ctx, "/ip/dhcp-server", r, map[string]string{"disabled": "no"}); err != nil {
			return err
		}
		fe.w.note("turned the customer DHCP server (%s) on", billing.DHCPServerName)
		return nil
	}
	return changed("the router has no %s", billing.DHCPServerName)
}

func fixBootFile(ctx context.Context, fe *fixEnv) error {
	sn, err := fe.freshRead(ctx, nil, []string{"/file"})
	if err != nil {
		return err
	}
	if !fe.m.scriptFinished(fe.site.ID) {
		return changed("the set-up script may still be running")
	}
	for _, r := range bootFiles(sn.tbl("/file")) {
		if err := fe.w.removeRow(ctx, "/file", r); err != nil {
			return err
		}
		fe.w.note("removed the file %s", r["name"])
	}
	return nil
}

// fixWalled repairs the walled-garden rules. For each group (the forward
// walled rules, the input walled rules, the NAT rules) it first sets rules whose
// properties changed and adds missing ones, each just above the rule that
// should follow it, so the walls never have a gap; only when the group is out
// of order does it take the group's rules out and put them back. Old
// "ftn: ipoe" rules that are not in the set are removed last.
func fixWalled(ctx context.Context, fe *fixEnv) error {
	w, err := fe.m.walledSet(fe.site)
	if err != nil {
		return changed("%v", err)
	}
	sn, err := fe.freshRead(ctx, nil, []string{"/ip/firewall/filter", "/ip/firewall/nat", "/ip/address", "/ip/dhcp-server"})
	if err != nil {
		return err
	}
	iface := CustomerBridge
	if c := fe.customer(sn); c.found {
		iface = c.iface
	}
	fe.w.walled = walledComments(w)
	if diffWalled(sn, w, iface).ok() {
		return nil
	}
	for _, g := range groupsOf(w) {
		rows, err := fe.rd.Rows(ctx, g.path, nil)
		if err != nil {
			return classifyFixErr(err)
		}
		if err := ensureGroup(ctx, fe.w, fe.rd, g, iface, rows); err != nil {
			return err
		}
	}
	for _, path := range []string{"/ip/firewall/filter", "/ip/firewall/nat"} {
		rows, err := fe.rd.Rows(ctx, path, nil)
		if err != nil {
			return classifyFixErr(err)
		}
		for _, r := range staleWalled(rows, fe.w.walled) {
			if err := fe.w.removeRow(ctx, path, r); err != nil {
				return err
			}
			fe.w.note("removed the old rule %q", r["comment"])
		}
	}
	return nil
}

// anchorOf is the row a group's rules go above: the first rule of the rest of
// the chain the script writes (forward est/rel, input drop rest), else the
// first static row of the chain, else none (the end of the table).
func anchorOf(g ruleGroup, rows []routeros.Row) string {
	name := map[string]string{"forward": "ftn: forward est/rel", "input": "ftn: input drop rest"}[g.chain]
	if name != "" {
		for _, r := range rows {
			if r["comment"] == name && r[routeros.RowID] != "" {
				return r[routeros.RowID]
			}
		}
	}
	if g.chain != "" {
		for _, r := range rows {
			if r["chain"] == g.chain && !yes(r["dynamic"]) && r[routeros.RowID] != "" {
				return r[routeros.RowID]
			}
		}
	}
	return ""
}

func ensureGroup(ctx context.Context, wr *fixWriter, rd rowReader, g ruleGroup, iface string, rows []routeros.Row) error {
	known := map[string]bool{}
	for _, r := range g.want {
		known[r.Comment] = true
	}
	have := map[string]routeros.Row{}
	var dup []routeros.Row
	orderOK, next := true, 0
	for i, r := range rows {
		c := r["comment"]
		if !known[c] {
			continue
		}
		if _, d := have[c]; d {
			dup = append(dup, r)
			continue
		}
		have[c] = r
		_ = i
	}
	// order: positions of the kept rows must follow the wanted order
	pos := map[string]int{}
	n := 0
	for _, r := range rows {
		if c := r["comment"]; known[c] && pos[c] == 0 && have[c][routeros.RowID] == r[routeros.RowID] {
			n++
			pos[c] = n
		}
	}
	for _, r := range g.want {
		if p := pos[r.Comment]; p > 0 {
			if p < next {
				orderOK = false
			} else {
				next = p
			}
		}
	}
	if !orderOK {
		// Out of order: put a complete new copy of the group in the right order directly above the old rules
		// first, and only then take the old rows out (by their ids), so a walled flat never meets a gap.
		var old []routeros.Row
		for _, r := range rows {
			if known[r["comment"]] && r[routeros.RowID] != "" {
				old = append(old, r)
			}
		}
		if len(old) == 0 {
			return nil
		}
		nextID := old[0][routeros.RowID]
		for i := len(g.want) - 1; i >= 0; i-- {
			body := g.want[i].Body(iface)
			body["place-before"] = nextID
			id, err := wr.addRowID(ctx, g.path, body)
			if err != nil {
				return err
			}
			if id != "" {
				nextID = id
			}
		}
		for _, r := range old {
			if err := wr.removeRow(ctx, g.path, r); err != nil {
				return err
			}
		}
		wr.note("put the walled-garden %s rules back in order (the new set went in before the old one came out)", g.chain)
		return nil
	}
	nextID := anchorOf(g, rows)
	for i := len(g.want) - 1; i >= 0; i-- {
		r := g.want[i]
		body := r.Body(iface)
		if row, ok := have[r.Comment]; ok {
			if diff := diffRow(row, body); len(diff) > 0 {
				if err := wr.setRow(ctx, g.path, row, diff); err != nil {
					return err
				}
				wr.note("repaired the rule %q", r.Comment)
			}
			nextID = row[routeros.RowID]
			continue
		}
		if nextID != "" {
			body["place-before"] = nextID
		}
		id, err := wr.addRowID(ctx, g.path, body)
		if err != nil {
			return err
		}
		wr.note("added the rule %q", r.Comment)
		if id != "" {
			nextID = id
		}
	}
	for _, r := range dup {
		if err := wr.removeRow(ctx, g.path, r); err != nil {
			return err
		}
		wr.note("removed a duplicate of %q", r["comment"])
	}
	return nil
}

// diffRow returns the wanted properties that differ from the row (booleans compare by meaning).
func diffRow(r routeros.Row, want map[string]string) map[string]string {
	diff := map[string]string{}
	for k, v := range want {
		if k == "place-before" {
			continue
		}
		cur, has := r[k]
		if has && sameValue(k, cur, v) {
			continue
		}
		if !has && v == "" {
			continue
		}
		diff[k] = v
	}
	return diff
}

func sameValue(k, cur, want string) bool {
	if routeros.SameValue(k, cur, want) {
		return true
	}
	b := func(s string) string {
		switch strings.ToLower(s) {
		case "yes", "true":
			return "yes"
		case "no", "false":
			return "no"
		}
		return s
	}
	return b(cur) == b(want)
}
