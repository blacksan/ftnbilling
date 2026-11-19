package sites

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Named fixes for the setup checklist. This file is the only place that
// writes to the router for the checks, and only through fixWriter, which is
// an allowlist: the server may change
//
//	/ip/dns                      one property (allow-remote-requests)
//	/ip/traffic-flow             enabled and interfaces
//	/ip/traffic-flow/target      rows commented "ftn: netflow"
//	/interface/bridge/port       rows of the building's OLT and bench ports
//	/ip/firewall/filter, /nat    the customer DNS accepts, and the walled-garden rules
//	/ip/dhcp-server              enabling ftn-dhcp
//	/file                        removing ftn-setup.rsc and ftn-reset.rsc
//
// and nothing else: never /user, /ip/service, /system/*, /interface/wireguard,
// routes, the WAN or any other firewall rule. Each fix re-reads what it depends
// on right before writing and refuses when the precondition no longer holds.

// Errors of ApplyFix.
var (
	// ErrUnknownCheck: no check has that id.
	ErrUnknownCheck = errors.New("there is no such check")
	// ErrNoFix: the check has no fix the server can run (see its manual command).
	ErrNoFix = errors.New("this check cannot be fixed by the server")
	// ErrConfirmRequired: the fix needs confirm=true; the message says what it will do.
	ErrConfirmRequired = errors.New("this fix needs confirmation")
	// ErrFixForbidden: the role may not run fixes.
	ErrFixForbidden = errors.New("your role may not change the router")
	// ErrNeedsOwner: only an owner may run this fix.
	ErrNeedsOwner = errors.New("only an owner may run this fix")
	// ErrPreconditionChanged: the router changed since the checklist was read,
	// and the fix is no longer safe (or no longer needed); nothing was written.
	ErrPreconditionChanged = errors.New("the router changed since the check; nothing was changed")
	// ErrFixNotAllowed: a fix tried to write outside the allowlist (a bug).
	ErrFixNotAllowed = errors.New("write refused: not on the server's allowlist")
)

// RouterRefusedError is a router that answered a fix's write with an error
// (HTTP 4xx/5xx from the REST API). Msg is the router's own text.
type RouterRefusedError struct {
	Msg string
	Err error
}

func (e *RouterRefusedError) Error() string { return "the router refused the change: " + e.Msg }
func (e *RouterRefusedError) Unwrap() error { return e.Err }

// FixResult is the answer of ApplyFix: the re-read item and what was done.
type FixResult struct {
	Item    CheckItem `json:"item"`
	Applied []string  `json:"applied"`
}

// ActorSystem is the actor of the fixes the server runs by itself.
const ActorSystem = "system"

// ---- the allowlisted writer ----------------------------------------------------------------

// rowWriter is the only part of the router client a fix may use to write:
// row operations. Command POSTs go through routeros.Inspector (call).
type rowWriter interface {
	AddRow(ctx context.Context, path string, body map[string]string) (string, error)
	SetRow(ctx context.Context, path, id string, diff map[string]string) error
	RemoveRow(ctx context.Context, path, id string) error
}

type fixWriter struct {
	rt        rowWriter
	insp      routeros.Inspector
	ifaces    map[string]bool // the OLT and bench ports of this building
	custIface string          // the customer interface, for the DNS accept rules
	walled    map[string]bool // comments of the walled-garden rules a fix may write (set by fixWalled)
	applied   []string
}

func (w *fixWriter) note(format string, a ...any) {
	w.applied = append(w.applied, fmt.Sprintf(format, a...))
}

func forbid(what string) error { return fmt.Errorf("%w: %s", ErrFixNotAllowed, what) }

// call sends a command POST. Only two are allowed.
func (w *fixWriter) call(ctx context.Context, path string, body map[string]string) error {
	switch path {
	case "/ip/dns/set":
		if len(body) != 1 || body["allow-remote-requests"] == "" {
			return forbid("/ip/dns/set may only change allow-remote-requests")
		}
	case "/ip/traffic-flow/set":
		for k := range body {
			if k != "enabled" && k != "interfaces" {
				return forbid("/ip/traffic-flow/set may only change enabled and interfaces")
			}
		}
	default:
		return forbid(path)
	}
	if w.insp == nil {
		return routeros.ErrNoInspector
	}
	return w.insp.Call(ctx, path, body)
}

func (w *fixWriter) allowedRow(path string, row routeros.Row) error {
	switch path {
	case "/interface/bridge/port":
		if !w.ifaces[row["interface"]] || row["interface"] == "" {
			return forbid("bridge port " + row["interface"] + " is not this building's OLT or bench port")
		}
	case "/ip/traffic-flow/target":
		if row["comment"] != netflowComment {
			return forbid("a traffic-flow target the server did not make")
		}
	case "/ip/dhcp-server":
		if row["name"] != billing.DHCPServerName {
			return forbid("a DHCP server the server did not make")
		}
	case "/ip/firewall/filter", "/ip/firewall/nat":
		if !w.walled[row["comment"]] && !strings.HasPrefix(row["comment"], "ftn: ipoe") {
			return forbid("a firewall rule that is not part of the walled garden")
		}
	case "/file":
		if row["name"] != SetupFile && row["name"] != ResetFile {
			return forbid("a file the server did not make")
		}
	default:
		return forbid(path)
	}
	return nil
}

func (w *fixWriter) addRow(ctx context.Context, path string, body map[string]string) error {
	switch path {
	case "/interface/bridge/port":
		if !w.ifaces[body["interface"]] {
			return forbid("bridge port " + body["interface"] + " is not this building's OLT or bench port")
		}
	case "/ip/traffic-flow/target":
		if body["comment"] != netflowComment {
			return forbid("a traffic-flow target without the ftn: netflow comment")
		}
	case "/ip/firewall/filter", "/ip/firewall/nat":
		if w.walled[body["comment"]] {
			for k := range body {
				if !ruleProp(k) {
					return forbid("firewall rule property " + k)
				}
			}
			break
		}
		if path == "/ip/firewall/nat" {
			return forbid("a NAT rule that is not part of the walled garden")
		}
		// Otherwise only the permanent "let customers ask the router for DNS" accepts.
		if body["comment"] != custDNSComment || body["chain"] != "input" || body["action"] != "accept" ||
			body["dst-port"] != "53" || (body["protocol"] != "udp" && body["protocol"] != "tcp") ||
			w.custIface == "" || body["in-interface"] != w.custIface {
			return forbid("a firewall rule that is not an \"" + custDNSComment + "\" accept for the customer interface")
		}
		for k := range body {
			switch k {
			case "chain", "action", "dst-port", "protocol", "in-interface", "comment", "place-before":
			default:
				return forbid("firewall rule property " + k)
			}
		}
	default:
		return forbid(path)
	}
	_, err := w.rt.AddRow(ctx, path, body)
	return err
}

// addRowID is addRow for the walled-garden rules: it returns the new row's id
// (the next rule goes above it).
func (w *fixWriter) addRowID(ctx context.Context, path string, body map[string]string) (string, error) {
	if !w.walled[body["comment"]] || (path != "/ip/firewall/filter" && path != "/ip/firewall/nat") {
		return "", forbid("a rule that is not part of the walled garden")
	}
	for k := range body {
		if !ruleProp(k) {
			return "", forbid("firewall rule property " + k)
		}
	}
	return w.rt.AddRow(ctx, path, body)
}

// ruleProp: the properties a walled-garden rule uses.
func ruleProp(k string) bool {
	switch k {
	case "chain", "action", "protocol", "dst-port", "dst-address", "src-address", "src-address-list", "in-interface", "out-interface",
		"in-interface-list", "out-interface-list", "connection-state", "reject-with", "to-addresses", "to-ports", "comment", "place-before":
		return true
	}
	return false
}

// setRow changes one existing row; row is the row as just read.
func (w *fixWriter) setRow(ctx context.Context, path string, row routeros.Row, diff map[string]string) error {
	if err := w.allowedRow(path, row); err != nil {
		return err
	}
	switch path {
	case "/interface/bridge/port":
		for k := range diff {
			if k != "bridge" {
				return forbid("a bridge port may only change its bridge")
			}
		}
	case "/ip/dhcp-server":
		for k := range diff {
			if k != "disabled" {
				return forbid("a DHCP server may only be switched on")
			}
		}
	case "/ip/firewall/filter", "/ip/firewall/nat":
		for k := range diff {
			if !ruleProp(k) {
				return forbid("firewall rule property " + k)
			}
		}
	}
	return w.rt.SetRow(ctx, path, row[routeros.RowID], diff)
}

// removeRow deletes one existing row; row is the row as just read.
func (w *fixWriter) removeRow(ctx context.Context, path string, row routeros.Row) error {
	switch path {
	case "/interface/bridge/port", "/ip/traffic-flow/target", "/ip/firewall/filter", "/ip/firewall/nat", "/file":
		if err := w.allowedRow(path, row); err != nil {
			return err
		}
	default:
		return forbid(path)
	}
	return w.rt.RemoveRow(ctx, path, row[routeros.RowID])
}

// ---- the registry ----------------------------------------------------------------------------

type fixEnv struct {
	m       *Manager
	site    store.Site // effective
	w       *fixWriter
	rd      rowReader // reads before writing
	confirm bool
	role    string
	partial bool // an automatic run: do only what needs no confirmation
}

type fixFunc func(ctx context.Context, fe *fixEnv) error

// fixers is the allowlist of named fixes, by check id.
var fixers = map[string]fixFunc{
	CheckDNS:         fixDNS,
	CheckCustDNS:     fixCustDNS,
	CheckOLT:         fixOLT,
	CheckBench:       fixBench,
	CheckTrafficFlow: fixTrafficFlow,
	CheckWalled:      fixWalled,
	CheckDHCP:        fixDHCP,
	CheckBootFile:    fixBootFile,
}

// freshRead re-reads what a fix depends on and fails (router unreachable)
// when any of it cannot be read.
func (fe *fixEnv) freshRead(ctx context.Context, singles, tables []string) (*snapshot, error) {
	sn := readSnapshot(ctx, fe.rd, singles, tables)
	for _, p := range append(append([]string(nil), singles...), tables...) {
		if err := sn.errs[p]; err != nil {
			return nil, fmt.Errorf("%w: reading %s: %v", ErrRouterUnreachable, p, shortErr(err))
		}
	}
	return sn, nil
}

func changed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrPreconditionChanged, fmt.Sprintf(format, a...))
}

var dnsTables = []string{"/ip/firewall/filter", "/ip/firewall/raw", "/interface/list/member", "/ip/route", "/ip/dhcp-client", "/ip/address", "/ip/dhcp-server"}

// readTolerant reads tables whose failure is acceptable (the packages may be absent).
func (fe *fixEnv) readTolerant(ctx context.Context, singles, tables []string, into *snapshot) {
	sn := readSnapshot(ctx, fe.rd, singles, tables)
	for k, v := range sn.one {
		into.one[k] = v
	}
	for k, v := range sn.rows {
		into.rows[k] = v
	}
	for k, v := range sn.errs {
		into.errs[k] = v
	}
}

// customer finds the customer interface in a snapshot.
func (fe *fixEnv) customer(sn *snapshot) custInfo {
	plan, err := PlanSubnet(fe.site.CustomerSubnet)
	if err != nil {
		return custInfo{}
	}
	return detectCustomer(sn, plan)
}

func fixDNS(ctx context.Context, fe *fixEnv) error {
	sn, err := fe.freshRead(ctx, []string{"/ip/dns"}, dnsTables)
	if err != nil {
		return err
	}
	fe.readTolerant(ctx, []string{"/ipv6/settings"}, []string{"/ipv6/firewall/filter", "/interface/pppoe-client", "/interface"}, sn)
	if yes(sn.single("/ip/dns")["allow-remote-requests"]) {
		return nil // somebody already did it
	}
	if ok, why := dnsSafe(sn, fe.site, fe.customer(sn).safeSet()); !ok {
		return changed("it is no longer shown to be safe to turn on DNS (%s)", why)
	}
	if err := fe.w.call(ctx, "/ip/dns/set", map[string]string{"allow-remote-requests": "yes"}); err != nil {
		return err
	}
	fe.w.note("DNS for customers turned on (/ip/dns allow-remote-requests=yes)")
	return nil
}

func fixCustDNS(ctx context.Context, fe *fixEnv) error {
	sn, err := fe.freshRead(ctx, nil, []string{"/ip/firewall/filter", "/interface/list/member", "/ip/address", "/ip/dhcp-server", "/interface/bridge"})
	if err != nil {
		return err
	}
	cust := fe.customer(sn)
	if !cust.found {
		return changed("the customer interface is not found")
	}
	bad, why := custDNSProblems(sn, cust)
	if len(bad) == 0 {
		return nil
	}
	for _, r := range sn.tbl("/ip/firewall/filter") {
		if r["comment"] == custDNSComment {
			return changed("a %q rule exists but does not take effect (%s)", custDNSComment, why)
		}
	}
	fe.w.custIface = cust.iface
	before, hasBefore := firstStaticInput(sn.tbl("/ip/firewall/filter"))
	for _, proto := range bad {
		body := map[string]string{"chain": "input", "in-interface": cust.iface, "protocol": proto, "dst-port": "53", "action": "accept", "comment": custDNSComment}
		if hasBefore {
			body["place-before"] = before
		}
		if err := fe.w.addRow(ctx, "/ip/firewall/filter", body); err != nil {
			return err
		}
		fe.w.note("firewall: accept %s DNS from %s", proto, cust.iface)
	}
	return nil
}

var oltTables = []string{"/interface/bridge/port", "/interface/vlan", "/interface/list/member", "/ip/address",
	"/ip/dhcp-server", "/ip/dhcp-client", "/interface/bridge/host", "/interface/bridge"}

func fixOLT(ctx context.Context, fe *fixEnv) error {
	sn, err := fe.freshRead(ctx, nil, oltTables)
	if err != nil {
		return err
	}
	a := assessOLT(sn, fe.site)
	switch {
	case !a.conflict:
		return nil // already fixed
	case len(a.hard) > 0:
		return changed("%s", strings.Join(a.hard, "; "))
	case len(a.unknown) > 0:
		return changed("%s", strings.Join(a.unknown, "; "))
	}
	// A port that sits in some other bridge than the customer bridge, or has
	// devices talking through it, is only taken out with an owner's confirm.
	if other, learned := otherBridges(a, fe.customer(sn)), a.learned; len(other) > 0 || len(learned) > 0 {
		if !fe.confirm {
			return fmt.Errorf("%w: %s is in bridge %s, devices learned: %s", ErrConfirmRequired, a.iface, orNone(other), orNone(learned))
		}
		if fe.role != store.RoleOwner {
			return ErrNeedsOwner
		}
	}
	for _, r := range a.rows {
		if err := fe.w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
			return err
		}
		fe.w.note("took %s out of bridge %s", a.iface, r["bridge"])
	}
	return nil
}

var benchTables = []string{"/interface/bridge/port", "/interface/bridge", "/interface", "/interface/vlan", "/interface/list/member",
	"/ip/address", "/ip/dhcp-server", "/ip/dhcp-client", "/interface/bridge/host"}

func fixBench(ctx context.Context, fe *fixEnv) error {
	if fe.site.BenchPort == "" {
		return changed("the building has no bench port")
	}
	sn, err := fe.freshRead(ctx, nil, benchTables)
	if err != nil {
		return err
	}
	plan, perr := PlanSubnet(fe.site.CustomerSubnet)
	if perr != nil {
		return changed("the customer network is not valid")
	}
	cust := detectCustomer(sn, plan)
	if !cust.found || !cust.isBridge {
		return changed("the customer bridge is not found")
	}
	b := assessBench(sn, fe.site, cust)
	switch {
	case b.noIface:
		return changed("the router has no interface %s", fe.site.BenchPort)
	case b.inCustomer:
		return nil
	case len(b.hard) > 0:
		return changed("%s", strings.Join(b.hard, "; "))
	case len(b.unknown) > 0:
		return changed("%s", strings.Join(b.unknown, "; "))
	case len(b.learned) > 0:
		if !fe.confirm {
			return fmt.Errorf("%w: devices are connected to %s (%s)", ErrConfirmRequired, b.iface, strings.Join(b.learned, ", "))
		}
		if fe.role != store.RoleOwner {
			return ErrNeedsOwner
		}
	}
	if b.inOther != "" {
		if len(b.rows) != 1 {
			return changed("the bench port has %d bridge rows", len(b.rows))
		}
		if err := fe.w.setRow(ctx, "/interface/bridge/port", b.rows[0], map[string]string{"bridge": cust.iface}); err != nil {
			return err
		}
		fe.w.note("moved %s from bridge %s to %s", b.iface, b.inOther, cust.iface)
		return nil
	}
	if err := fe.w.addRow(ctx, "/interface/bridge/port", map[string]string{"bridge": cust.iface, "interface": b.iface, "horizon": "1", "comment": "ftn: bench port"}); err != nil {
		return err
	}
	fe.w.note("put %s on bridge %s", b.iface, cust.iface)
	return nil
}

func fixTrafficFlow(ctx context.Context, fe *fixEnv) error {
	sn, err := fe.freshRead(ctx, []string{"/ip/traffic-flow"}, []string{"/ip/traffic-flow/target", "/ip/address", "/ip/dhcp-server", "/interface/bridge"})
	if err != nil {
		return err
	}
	plan, perr := PlanSubnet(fe.site.CustomerSubnet)
	if perr != nil {
		return changed("the customer network is not valid")
	}
	cust := detectCustomer(sn, plan)
	if !cust.found {
		return changed("the customer interface is not found")
	}
	want, ok := fe.m.flowWant(cust.iface)
	if !ok {
		return changed("the server's traffic-flow listener is switched off")
	}
	problems, targetID := flowProblems(sn, want)
	if len(problems) == 0 {
		return nil
	}
	tf := sn.single("/ip/traffic-flow")
	if !yes(tf["enabled"]) || !sameSet(splitList(tf["interfaces"]), []string{want.iface}) {
		if err := fe.w.call(ctx, "/ip/traffic-flow/set", map[string]string{"enabled": "yes", "interfaces": want.iface}); err != nil {
			return err
		}
		fe.w.note("traffic-flow on, watching %s", want.iface)
	}
	body := map[string]string{"dst-address": want.dst, "port": want.port, "version": "5"}
	if targetID != "" {
		var row routeros.Row
		for _, r := range sn.tbl("/ip/traffic-flow/target") {
			if r[routeros.RowID] == targetID {
				row = r
			}
		}
		body["disabled"] = "no"
		if err := fe.w.setRow(ctx, "/ip/traffic-flow/target", row, body); err != nil {
			return err
		}
		fe.w.note("traffic-flow target set to %s:%s", want.dst, want.port)
		return nil
	}
	body["comment"] = netflowComment
	if err := fe.w.addRow(ctx, "/ip/traffic-flow/target", body); err != nil {
		return err
	}
	fe.w.note("traffic-flow target added: %s:%s", want.dst, want.port)
	return nil
}

// ---- entry points -----------------------------------------------------------------------------

func roleRank(role string) int {
	switch role {
	case store.RoleOwner, ActorSystem:
		return 2
	case store.RoleOperator:
		return 1
	}
	return 0
}

// ApplyFix runs the named fix of a check on the building's router. The
// checklist is read fresh first; the fix then re-reads what it needs right
// before writing and refuses (ErrPreconditionChanged) if it is no longer
// safe. actorRole is the admin's role (owner or operator); a fix whose item
// says role owner needs an owner, and a confirm-kind fix needs confirm=true.
// Errors: ErrUnknownCheck, ErrNoFix, ErrConfirmRequired, ErrFixForbidden,
// ErrNeedsOwner, ErrPreconditionChanged, ErrRouterUnreachable, a
// *RouterRefusedError for a router that answered with an error, and
// billing.ErrNoSite. The result holds the re-read item and what was done.
func (m *Manager) ApplyFix(ctx context.Context, siteID, checkID string, confirm bool, actorRole string) (FixResult, error) {
	return m.applyFix(ctx, siteID, checkID, confirm, actorRole, false)
}

func (m *Manager) applyFix(ctx context.Context, siteID, checkID string, confirm bool, actor string, partial bool) (FixResult, error) {
	res := FixResult{Applied: []string{}}
	if roleRank(actor) < 1 {
		return res, ErrFixForbidden
	}
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return res, billing.ErrNoSite
	}
	c, err := m.Checks(ctx, siteID, true)
	if err != nil {
		return res, err
	}
	item, ok := c.Item(checkID)
	if !ok {
		return res, ErrUnknownCheck
	}
	res.Item = item
	if !c.Router.Reachable {
		return res, ErrRouterUnreachable
	}
	if item.Status == StatusOK || item.Status == StatusInfo {
		return res, nil // nothing to fix
	}
	fn, hasFixer := fixers[checkID]
	switch item.Fix.Kind {
	case FixAuto, FixConfirm:
	default:
		if item.Fix.Command != "" {
			return res, fmt.Errorf("%w: run this on the router: %s", ErrNoFix, item.Fix.Command)
		}
		return res, ErrNoFix
	}
	if !hasFixer {
		return res, ErrNoFix
	}
	if item.Fix.Role == store.RoleOwner && roleRank(actor) < 2 {
		return res, ErrNeedsOwner
	}
	if actor == ActorSystem && item.Fix.Kind != FixAuto && !partial {
		return res, fmt.Errorf("%w: the server does not run this by itself", ErrConfirmRequired)
	}
	if item.Fix.Kind == FixConfirm && !confirm && !partial {
		msg := item.Fix.ConfirmText
		if msg == "" {
			msg = "confirm to go on"
		}
		return res, fmt.Errorf("%w: %s", ErrConfirmRequired, msg)
	}
	eff := m.Effective(site)
	rt := m.Router(siteID)
	insp, _ := rt.(routeros.Inspector)
	w := &fixWriter{rt: rt, insp: insp, ifaces: map[string]bool{}}
	for _, n := range []string{eff.OLTInterface, eff.BenchPort, CustomerBridge} {
		if n != "" {
			w.ifaces[n] = true
		}
	}
	role := actor
	if actor == ActorSystem {
		role = store.RoleOperator
	}
	fe := &fixEnv{m: m, site: eff, w: w, rd: rt, confirm: confirm, role: role, partial: partial}
	werr := fn(ctx, fe)
	res.Applied = append(res.Applied, w.applied...)
	if len(w.applied) > 0 || werr != nil {
		msg := fmt.Sprintf("setup fix %s by %s: %s", checkID, actor, strings.Join(w.applied, "; "))
		if werr != nil {
			msg = fmt.Sprintf("setup fix %s by %s failed: %v", checkID, actor, werr)
			if len(w.applied) > 0 {
				msg += " (done before the failure: " + strings.Join(w.applied, "; ") + ")"
			}
		}
		_ = m.st.Update(func(d *store.Data) error {
			d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: msg})
			return nil
		})
	}
	m.chk.drop(siteID)
	if werr != nil {
		return res, classifyFixErr(werr)
	}
	if c2, err := m.Checks(ctx, siteID, true); err == nil {
		if it, ok := c2.Item(checkID); ok {
			res.Item = it
		}
	}
	return res, nil
}

// classifyFixErr turns a router error into RouterRefusedError (the router
// said no) or ErrRouterUnreachable (no answer); the package's own errors
// pass through.
func classifyFixErr(err error) error {
	for _, own := range []error{ErrUnknownCheck, ErrNoFix, ErrConfirmRequired, ErrFixForbidden, ErrNeedsOwner,
		ErrPreconditionChanged, ErrFixNotAllowed, ErrRouterUnreachable, routeros.ErrNoInspector, billing.ErrNoSite} {
		if errors.Is(err, own) {
			return err
		}
	}
	var ae *routeros.APIError
	if errors.As(err, &ae) {
		msg := ae.Message
		if ae.Detail != "" {
			msg += " (" + ae.Detail + ")"
		}
		return &RouterRefusedError{Msg: msg, Err: err}
	}
	return fmt.Errorf("%w: %v", ErrRouterUnreachable, err)
}

// AutoFixOutcome is what the automatic run did for one check.
type AutoFixOutcome struct {
	ID      string   `json:"id"`
	Applied []string `json:"applied"`
	Err     string   `json:"err,omitempty"`
}

// ApplyAutoFixes runs once every fix of kind auto that the checklist asks
// for each audited as
// by "system". Fixes that need confirmation are not run. W5c / the join hook
// call it after a successful join; it is safe to call again.
func (m *Manager) ApplyAutoFixes(ctx context.Context, siteID string) ([]AutoFixOutcome, error) {
	c, err := m.Checks(ctx, siteID, true)
	if err != nil {
		return nil, err
	}
	if !c.Router.Reachable {
		return nil, ErrRouterUnreachable
	}
	var out []AutoFixOutcome
	for _, it := range c.Items {
		if it.Status != StatusWarn && it.Status != StatusFail {
			continue
		}
		if _, ok := fixers[it.ID]; !ok {
			continue
		}
		if it.Fix.Kind != FixAuto {
			continue
		}
		res, err := m.applyFix(ctx, siteID, it.ID, false, ActorSystem, false)
		o := AutoFixOutcome{ID: it.ID, Applied: res.Applied}
		if err != nil {
			o.Err = err.Error()
		}
		out = append(out, o)
	}
	return out, nil
}
