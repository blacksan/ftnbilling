package sites

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// OLT management port (role "oltmgmt").
//
// The owner picks a spare router port (not in any bridge) and cables it to the OLT's factory management port (GE0/2,
// VLAN 1, 192.168.8.200/24). The server then gives that port the router's own address in the OLT's factory subnet and
// a narrow set of firewall and NAT rules over REST:
//
//	server 10.99.0.1 -> wg-ftn -> router tunnel address :22000+h -> dst-nat -> <OLT address>:22
//
// h is the last octet of the OLT's address (22200 for 192.168.8.200). Only the server (source address, no other) may
// start that connection; the router masquerades it toward the OLT; everything else to or from the port is dropped
// (no LAN, VLAN 99, WAN, unregistered pool or admin access, and no internet for the OLT).
//
// What the server writes, and nothing else (oltmWriter enforces the exact shapes):
//
//	/ip/address               one row on the port ("ftn: oltm address")
//	/ip/firewall/filter       the rows "ftn: oltm ..." (forward accepts and drops, an input drop)
//	/ip/firewall/nat          the rows "ftn: oltm ..." (dst-nat, masquerade)
//
// Never routes, WireGuard, /user, /ip/service or /system. Removing the role removes exactly those rows.
const (
	// DefaultOLTMgmtNet is the router's address on the OLT management port: the OLT's factory subnet.
	DefaultOLTMgmtNet = "192.168.8.1/24"
	// DefaultOLTFactoryIP is where a factory V-SOL OLT answers.
	DefaultOLTFactoryIP = "192.168.8.200"

	oltmAddrComment  = "ftn: oltm address"
	oltmNatComment   = "ftn: oltm dst-nat"
	oltmMasqComment  = "ftn: oltm masquerade"
	oltmFwdComment   = "ftn: oltm server to olt"
	oltmReplyComment = "ftn: oltm replies"
	oltmDropIn       = "ftn: oltm drop in"
	oltmDropOut      = "ftn: oltm drop out"
	oltmInputDrop    = "ftn: oltm input drop"
)

// OLTAdoptForwardPort is the TCP port of the router's tunnel address that reaches the OLT at ip (22000 + last octet).
func OLTAdoptForwardPort(ip string) (int, error) {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is4() {
		return 0, fmt.Errorf("the OLT address %q is not an IPv4 address", ip)
	}
	return oltSSHBase + int(a.As4()[3]), nil
}

// OLTMgmtOLTIP is the address the OLT is reached at: the one on its inventory record, else the factory address.
func OLTMgmtOLTIP(mgmtIP string) string {
	if strings.TrimSpace(mgmtIP) != "" {
		return strings.TrimSpace(mgmtIP)
	}
	return DefaultOLTFactoryIP
}

// OLTMgmtNetOf is the router's address on the OLT management port with its default.
func OLTMgmtNetOf(s store.Site) string {
	if strings.TrimSpace(s.OLTMgmtNet) != "" {
		return strings.TrimSpace(s.OLTMgmtNet)
	}
	return DefaultOLTMgmtNet
}

// OLTAdoptPlan is the addressing and the rules of one router's OLT management port.
type OLTAdoptPlan struct {
	Port     string
	CIDR     string // the router's address on the port, 192.168.8.1/24
	RouterIP string
	OLTIP    string
	ServerIP string // the only source that may reach the OLT
	TunnelIP string // the router's tunnel address: the server dials it
	FwdPort  int    // 22000 + last octet of OLTIP
}

// PlanOLTAdopt computes the plan for a router. oltIP is the OLT's address ("" = the factory one).
func PlanOLTAdopt(eff store.Site, oltIP, serverIP string) (OLTAdoptPlan, error) {
	var p OLTAdoptPlan
	port := strings.TrimSpace(eff.OLTMgmtPort)
	if port == "" || !ValidIface(port) {
		return p, errors.New("the router has no OLT management port")
	}
	pfx, err := netip.ParsePrefix(OLTMgmtNetOf(eff))
	if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 16 || pfx.Bits() > 29 {
		return p, fmt.Errorf("the router address %q must be an IPv4 address with its prefix, like 192.168.8.1/24 (a /16 to /29 network)", OLTMgmtNetOf(eff))
	}
	oltIP = OLTMgmtOLTIP(oltIP)
	oa, err := netip.ParseAddr(oltIP)
	if err != nil || !oa.Is4() {
		return p, fmt.Errorf("the OLT address %q is not an IPv4 address", oltIP)
	}
	net := pfx.Masked()
	if !net.Contains(oa) || oa == net.Addr() || oa == lastAddr(net) || oa == pfx.Addr() {
		return p, fmt.Errorf("the OLT address %s must be a host address inside %s other than the router's %s", oa, net, pfx.Addr())
	}
	// The forwarded port is 22000 + the last octet; the OLT management VLAN (oltmgmt.go) uses 22002-22006 (OLTs at .2 to
	// .6 of its /29) on the same tunnel address, so an OLT here must not end in .0 to .6.
	if oa.As4()[3] <= 6 {
		return p, fmt.Errorf("the OLT address %s must not end in .0 to .6 (those forwarded ports belong to the OLT management VLAN): use .7 to .254, like the factory %s", oa, DefaultOLTFactoryIP)
	}
	if _, err := netip.ParseAddr(serverIP); err != nil {
		return p, fmt.Errorf("the server tunnel address %q is not an address", serverIP)
	}
	if _, err := netip.ParseAddr(eff.TunnelIP); err != nil {
		return p, errors.New("the router has no tunnel address yet")
	}
	// The subnet may not overlap one the router already uses for something else.
	for _, other := range []string{eff.CustomerSubnet, eff.LANSubnet, eff.WANAddress} {
		if other == "" {
			continue
		}
		if op, err := netip.ParsePrefix(strings.TrimSpace(other)); err == nil && op.Masked().Overlaps(net) {
			return p, fmt.Errorf("the OLT management network %s overlaps %s, which the router already uses", net, op.Masked())
		}
	}
	fp, err := OLTAdoptForwardPort(oltIP)
	if err != nil {
		return p, err
	}
	return OLTAdoptPlan{Port: port, CIDR: pfx.String(), RouterIP: pfx.Addr().String(), OLTIP: oltIP, ServerIP: serverIP, TunnelIP: eff.TunnelIP, FwdPort: fp}, nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= (1 << host) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// oltmRule is one wanted router row: the table, the REST body (without place-before) and where it goes.
type oltmRule struct {
	Path string
	Body map[string]string
}

// addressRow is the one /ip/address row.
func (p OLTAdoptPlan) addressRow() oltmRule {
	return oltmRule{"/ip/address", map[string]string{"address": p.CIDR, "interface": p.Port, "comment": oltmAddrComment}}
}

// Rules are the wanted firewall and NAT rows, in the order they are added (the forward rules go above the router's
// drops, the input drop at the top of the input chain, the NAT rows at the end).
func (p OLTAdoptPlan) Rules() (forward, input, nat []oltmRule) {
	f := func(b map[string]string, c string) oltmRule {
		b["comment"] = c
		return oltmRule{"/ip/firewall/filter", b}
	}
	nat = []oltmRule{
		{"/ip/firewall/nat", map[string]string{"chain": "dstnat", "action": "dst-nat", "protocol": "tcp", "in-interface": WGInterface, "src-address": p.ServerIP,
			"dst-address": p.TunnelIP, "dst-port": strconv.Itoa(p.FwdPort), "to-addresses": p.OLTIP, "to-ports": "22", "comment": oltmNatComment}},
		{"/ip/firewall/nat", map[string]string{"chain": "srcnat", "action": "masquerade", "protocol": "tcp", "out-interface": p.Port, "dst-address": p.OLTIP,
			"dst-port": "22", "comment": oltmMasqComment}},
	}
	forward = []oltmRule{
		f(map[string]string{"chain": "forward", "action": "accept", "protocol": "tcp", "in-interface": WGInterface, "out-interface": p.Port,
			"src-address": p.ServerIP, "dst-address": p.OLTIP, "dst-port": "22"}, oltmFwdComment),
		f(map[string]string{"chain": "forward", "action": "accept", "connection-state": "established,related", "in-interface": p.Port, "out-interface": WGInterface,
			"src-address": p.OLTIP, "dst-address": p.ServerIP}, oltmReplyComment),
		f(map[string]string{"chain": "forward", "action": "drop", "in-interface": p.Port}, oltmDropIn),
		f(map[string]string{"chain": "forward", "action": "drop", "out-interface": p.Port}, oltmDropOut),
	}
	input = []oltmRule{f(map[string]string{"chain": "input", "action": "drop", "in-interface": p.Port}, oltmInputDrop)}
	return forward, input, nat
}

// oltmComments are the comments of every row this file writes.
var oltmComments = map[string]bool{oltmAddrComment: true, oltmNatComment: true, oltmMasqComment: true, oltmFwdComment: true, oltmReplyComment: true,
	oltmDropIn: true, oltmDropOut: true, oltmInputDrop: true}

// ---- the allowlisted writer ------------------------------------------------------------------------------------

// oltmWriter writes only the rows of one OLTAdoptPlan, exactly: an add must equal one of the plan's rows (plus
// place-before), a remove may only touch a row that carries one of the "ftn: oltm ..." comments. It never sets a row.
type oltmWriter struct {
	rt   rowWriter
	want map[string][]map[string]string // path -> wanted bodies
	stop func() bool
	log  []string
}

func newOLTMWriter(rt rowWriter, p *OLTAdoptPlan) *oltmWriter {
	w := &oltmWriter{rt: rt, want: map[string][]map[string]string{}}
	if p != nil {
		add := func(r oltmRule) { w.want[r.Path] = append(w.want[r.Path], r.Body) }
		add(p.addressRow())
		fwd, in, nat := p.Rules()
		for _, l := range [][]oltmRule{fwd, in, nat} {
			for _, r := range l {
				add(r)
			}
		}
	}
	return w
}

func sameBody(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (w *oltmWriter) add(ctx context.Context, path string, body map[string]string, before string) (string, error) {
	if w.stop != nil && w.stop() {
		return "", ErrAutoFixesPaused
	}
	ok := false
	for _, b := range w.want[path] {
		ok = ok || sameBody(b, body)
	}
	if !ok {
		return "", forbid("a row that is not part of the OLT management port rules: " + path)
	}
	send := make(map[string]string, len(body)+1)
	for k, v := range body {
		send[k] = v
	}
	if before != "" {
		send["place-before"] = before
	}
	id, err := w.rt.AddRow(ctx, path, send)
	if err == nil {
		w.log = append(w.log, "added "+path+" "+body["comment"])
	}
	return id, err
}

func (w *oltmWriter) remove(ctx context.Context, path string, row routeros.Row) error {
	if w.stop != nil && w.stop() {
		return ErrAutoFixesPaused
	}
	switch path {
	case "/ip/address", "/ip/firewall/filter", "/ip/firewall/nat":
	default:
		return forbid(path)
	}
	if !oltmComments[row["comment"]] {
		return forbid("a row the server did not make for the OLT management port")
	}
	if path == "/ip/address" && row["comment"] != oltmAddrComment {
		return forbid("an address row that is not the OLT management address")
	}
	if err := w.rt.RemoveRow(ctx, path, row[routeros.RowID]); err != nil {
		return err
	}
	w.log = append(w.log, "removed "+path+" "+row["comment"])
	return nil
}

// ---- applying -------------------------------------------------------------------------------------------------

// ErrOLTMgmtPort: the OLT management port cannot be set up (the message says why).
type ErrOLTMgmtPort struct{ Msg string }

func (e *ErrOLTMgmtPort) Error() string { return e.Msg }

func oltmErr(format string, a ...any) error { return &ErrOLTMgmtPort{Msg: fmt.Sprintf(format, a...)} }

// oltmRows reads what the apply and the remove need.
type oltmRows struct {
	addr, filter, nat, bridge, ifaces []routeros.Row
}

func (m *Manager) readOLTMgmt(ctx context.Context, rt routeros.Client) (oltmRows, error) {
	var r oltmRows
	var err error
	rd := func(path string, props []string) []routeros.Row {
		if err != nil {
			return nil
		}
		var rows []routeros.Row
		rows, err = rt.RowsProps(ctx, path, nil, props)
		return rows
	}
	r.addr = rd("/ip/address", []string{"address", "network", "interface", "comment", "disabled", "dynamic"})
	r.filter = rd("/ip/firewall/filter", []string{"chain", "action", "comment", "dynamic", "in-interface", "out-interface", "src-address", "dst-address", "dst-port", "protocol", "connection-state", "disabled"})
	r.nat = rd("/ip/firewall/nat", []string{"chain", "action", "comment", "in-interface", "out-interface", "src-address", "dst-address", "dst-port", "to-addresses", "to-ports", "protocol", "disabled"})
	r.bridge = rd("/interface/bridge/port", []string{"interface", "bridge"})
	r.ifaces = rd("/interface", []string{"name", "type", "disabled", "running"})
	if err != nil {
		return r, fmt.Errorf("%w: %v", ErrRouterUnreachable, shortErr(err))
	}
	return r, nil
}

// EnsureOLTMgmt makes the router match the OLT management port of the site: with a port set, its address and rules
// (added when missing, replaced when they differ); with none, whatever "ftn: oltm ..." rows are left are removed. It is
// safe to call every few minutes.
func (m *Manager) EnsureOLTMgmt(ctx context.Context, siteID, by string) error {
	e := m.cport.entry(siteID)
	if err := e.run.LockCtx(ctx); err != nil {
		return oltmErr("%s", ErrRouterBusy.Error())
	}
	defer e.run.Unlock()
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return billing.ErrNoSite
	}
	eff := m.Effective(site)
	return m.ensureOLTMgmtLocked(ctx, eff, m.adoptOLTIP(eff), by)
}

// ensureOLTMgmtLocked is EnsureOLTMgmt for a caller that holds the router's run lock.
func (m *Manager) ensureOLTMgmtLocked(ctx context.Context, eff store.Site, oltIP, by string) error {
	rt := m.Router(eff.ID)
	rows, err := m.readOLTMgmt(ctx, rt)
	if err != nil {
		return err
	}
	if eff.OLTMgmtPort == "" {
		return m.removeOLTMgmtRows(ctx, rt, rows, nil, eff, by)
	}
	plan, err := PlanOLTAdopt(eff, oltIP, ServerTunnelIP(m.pfx))
	if err != nil {
		return oltmErr("%s", err.Error())
	}
	if err := checkOLTMgmtPort(eff, plan, rows); err != nil {
		return err
	}
	w := newOLTMWriter(rt, &plan)
	w.stop = m.oltmStop(eff.ID, by)
	ctx2 := ctx
	var added int
	// What exists, by comment.
	have := map[string]routeros.Row{}
	for _, l := range [][]routeros.Row{rows.addr, rows.filter, rows.nat} {
		for _, r := range l {
			if oltmComments[r["comment"]] {
				have[r["comment"]] = r
			}
		}
	}
	// A row of an old plan (another port, address or OLT) that does not match is removed first.
	fwd, in, nat := plan.Rules()
	wanted := map[string]oltmRule{oltmAddrComment: plan.addressRow()}
	for _, l := range [][]oltmRule{fwd, in, nat} {
		for _, r := range l {
			wanted[r.Body["comment"]] = r
		}
	}
	var stale []routeros.Row
	for c, r := range have {
		if !rowHas(r, wanted[c].Body) || yes(r["disabled"]) {
			stale = append(stale, r)
			delete(have, c)
		}
	}
	for _, r := range stale {
		if err := w.remove(ctx2, pathOfComment(r["comment"]), r); err != nil {
			return writeFail(err)
		}
	}
	// Re-read the firewall order for the place-before anchors after removals.
	frows, nrows := rows.filter, rows.nat
	if len(stale) > 0 {
		if fr, err := rt.RowsProps(ctx, "/ip/firewall/filter", nil, []string{"chain", "comment", "dynamic"}); err == nil {
			frows = fr
		}
		if nr, err := rt.RowsProps(ctx, "/ip/firewall/nat", nil, []string{"chain", "comment", "dynamic"}); err == nil {
			nrows = nr
		}
	}
	_ = nrows
	// The address first.
	if _, ok := have[oltmAddrComment]; !ok {
		if _, err := w.add(ctx2, "/ip/address", plan.addressRow().Body, ""); err != nil {
			return writeFail(err)
		}
		added++
	}
	fwdAnchor := anchorForward(frows)
	inAnchor := anchorInput(frows)
	for _, r := range fwd {
		if _, ok := have[r.Body["comment"]]; ok {
			continue
		}
		if _, err := w.add(ctx2, r.Path, r.Body, fwdAnchor); err != nil {
			return writeFail(err)
		}
		added++
	}
	for _, r := range in {
		if _, ok := have[r.Body["comment"]]; ok {
			continue
		}
		if _, err := w.add(ctx2, r.Path, r.Body, inAnchor); err != nil {
			return writeFail(err)
		}
		added++
	}
	for _, r := range nat {
		if _, ok := have[r.Body["comment"]]; ok {
			continue
		}
		if _, err := w.add(ctx2, r.Path, r.Body, ""); err != nil {
			return writeFail(err)
		}
		added++
	}
	if added > 0 || len(stale) > 0 {
		m.audit(eff.ID, by, fmt.Sprintf("OLT management port %s set up (router address %s, server -> %s:22 through port %d): %d rows written, %d replaced", plan.Port, plan.CIDR, plan.OLTIP, plan.FwdPort, added, len(stale)))
	}
	return nil
}

// oltmStop: the server's own healing stops when the owner paused the automatic fixes; an owner's own action does not.
func (m *Manager) oltmStop(siteID, by string) func() bool {
	if by != "" && by != ActorSystem {
		return nil
	}
	return func() bool { return m.AutoFixPause(siteID).Paused }
}

func (m *Manager) audit(siteID, by, msg string) {
	if by == "" {
		by = ActorSystem
	}
	_ = m.st.Update(func(d *store.Data) error {
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Actor: by, Msg: msg})
		return nil
	})
}

func pathOfComment(c string) string {
	switch c {
	case oltmAddrComment:
		return "/ip/address"
	case oltmNatComment, oltmMasqComment:
		return "/ip/firewall/nat"
	}
	return "/ip/firewall/filter"
}

func rowHas(r routeros.Row, want map[string]string) bool {
	for k, v := range want {
		if r[k] != v {
			return false
		}
	}
	return true
}

func writeFail(err error) error {
	var fe *RouterRefusedError
	if errors.As(classifyFixErr(err), &fe) || errors.Is(err, ErrFixNotAllowed) {
		return classifyFixErr(err)
	}
	return fmt.Errorf("%w: %v", ErrRouterUnreachable, shortErr(err))
}

// anchorForward is the id of the row the new forward rules go above: "ftn: wg no forward in", else the first static
// forward rule; "" appends.
func anchorForward(rows []routeros.Row) string {
	first := ""
	for _, r := range rows {
		if r["chain"] != "forward" || yes(r["dynamic"]) || r[routeros.RowID] == "" {
			continue
		}
		if r["comment"] == "ftn: wg no forward in" {
			return r[routeros.RowID]
		}
		if first == "" {
			first = r[routeros.RowID]
		}
	}
	return first
}

func anchorInput(rows []routeros.Row) string {
	if id, ok := firstStaticInput(rows); ok {
		return id
	}
	return ""
}

// checkOLTMgmtPort refuses a port that cannot safely be the OLT management port.
func checkOLTMgmtPort(eff store.Site, p OLTAdoptPlan, r oltmRows) error {
	ifc := map[string]routeros.Row{}
	for _, x := range r.ifaces {
		ifc[x["name"]] = x
	}
	row, ok := ifc[p.Port]
	if !ok || !physicalType(row["type"]) {
		return oltmErr("the router has no physical port called %s", p.Port)
	}
	if p.Port == eff.WANInterface || p.Port == eff.OLTInterface || containsStr(SplitPorts(eff.LANPorts), p.Port) || containsStr(SplitPorts(eff.CustomerPorts), p.Port) {
		return oltmErr("%s already has another role", p.Port)
	}
	for _, b := range r.bridge {
		if b["interface"] == p.Port {
			return oltmErr("%s is a member of the bridge %s. The OLT management port must be in no bridge.", p.Port, b["bridge"])
		}
	}
	net := netip.MustParsePrefix(p.CIDR).Masked()
	for _, a := range r.addr {
		if oltmComments[a["comment"]] {
			continue
		}
		if a["interface"] == p.Port {
			return oltmErr("%s already has the address %s, which the server did not give it", p.Port, a["address"])
		}
		if ap, err := netip.ParsePrefix(a["address"]); err == nil && ap.Masked().Overlaps(net) {
			return oltmErr("the OLT management network %s overlaps the address %s on %s", net, a["address"], a["interface"])
		}
	}
	return nil
}

// removeOLTMgmtRows removes every "ftn: oltm ..." row of the router: exactly what EnsureOLTMgmt added.
func (m *Manager) removeOLTMgmtRows(ctx context.Context, rt routeros.Client, rows oltmRows, plan *OLTAdoptPlan, eff store.Site, by string) error {
	w := newOLTMWriter(rt, plan)
	w.stop = m.oltmStop(eff.ID, by)
	n := 0
	// NAT and filter first (the rules that carry traffic), the address last.
	for _, set := range []struct {
		path string
		rows []routeros.Row
	}{{"/ip/firewall/nat", rows.nat}, {"/ip/firewall/filter", rows.filter}, {"/ip/address", rows.addr}} {
		for _, r := range set.rows {
			if !oltmComments[r["comment"]] || pathOfComment(r["comment"]) != set.path {
				continue
			}
			if err := w.remove(ctx, set.path, r); err != nil {
				return writeFail(err)
			}
			n++
		}
	}
	if n > 0 {
		m.audit(eff.ID, by, fmt.Sprintf("OLT management port rules removed from the router (%d rows)", n))
	}
	return nil
}

// clearOLTMgmtLocked removes the OLT management rows from a router (the role was taken away); the caller holds the run lock.
func (m *Manager) clearOLTMgmtLocked(ctx context.Context, eff store.Site, by string) error {
	rt := m.Router(eff.ID)
	rows, err := m.readOLTMgmt(ctx, rt)
	if err != nil {
		return err
	}
	return m.removeOLTMgmtRows(ctx, rt, rows, nil, eff, by)
}

// ---- seeing the OLT on the port ----------------------------------------------------------------------------------

// OLTPortSeen is what the router shows on its OLT management port.
type OLTPortSeen struct {
	Port string
	// Rows are the devices that have an ARP entry on the port (the MAC is upper case with colons).
	Rows []OLTSeenMAC
}

// OLTSeenMAC is one device on the OLT management port.
type OLTSeenMAC struct{ MAC, IP, Status string }

// OLTMgmtSeen reads the devices on the site's OLT management port. A site with no such port gives Port "" and no
// error. Read only.
func (m *Manager) OLTMgmtSeen(ctx context.Context, siteID string) (OLTPortSeen, error) {
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return OLTPortSeen{}, billing.ErrNoSite
	}
	out := OLTPortSeen{Port: site.OLTMgmtPort}
	if site.OLTMgmtPort == "" {
		return out, nil
	}
	rt := m.Router(siteID)
	rows, err := rt.RowsProps(ctx, "/ip/arp", map[string]string{"interface": site.OLTMgmtPort}, []string{"address", "mac-address", "interface", "status", "complete", "invalid"})
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrRouterUnreachable, shortErr(err))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r["interface"] != site.OLTMgmtPort || yes(r["invalid"]) {
			continue
		}
		mac, ok := store.CanonMAC(r["mac-address"])
		if !ok || seen[mac] {
			continue
		}
		seen[mac] = true
		out.Rows = append(out.Rows, OLTSeenMAC{MAC: mac, IP: r["address"], Status: r["status"]})
	}
	sort.Slice(out.Rows, func(i, j int) bool { return out.Rows[i].MAC < out.Rows[j].MAC })
	return out, nil
}

// OLTAdoptReach is the address the server dials for an OLT behind a router with an OLT management port: the router's
// tunnel address and the forwarded port. ok is false when the site has no such port (the older routes apply).
func (m *Manager) OLTAdoptReach(s store.Site, mgmtIP string) (addr string, ok bool, err error) {
	if s.OLTMgmtPort == "" {
		return "", false, nil
	}
	p, err := PlanOLTAdopt(m.Effective(s), mgmtIP, ServerTunnelIP(m.pfx))
	if err != nil {
		return "", true, err
	}
	return net.JoinHostPort(p.TunnelIP, strconv.Itoa(p.FwdPort)), true, nil
}

// adoptOLTIP is the address the OLT behind the site's OLT management port is expected at: the management address of an
// OLT of the site that lies inside the port's subnet, else the factory address. (An OLT with an address on the older
// management VLAN keeps using that path.)
func (m *Manager) adoptOLTIP(eff store.Site) string {
	pfx, err := netip.ParsePrefix(OLTMgmtNetOf(eff))
	if err != nil {
		return DefaultOLTFactoryIP
	}
	ip := ""
	m.st.View(func(d *store.Data) {
		var ids []string
		for _, o := range d.OLTsOfSite(eff.ID) {
			if o.Status == store.EqRetired || o.MgmtIP == "" {
				continue
			}
			if a, err := netip.ParseAddr(o.MgmtIP); err == nil && pfx.Contains(a) {
				ids = append(ids, o.MgmtIP)
			}
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			ip = ids[0]
		}
	})
	return OLTMgmtOLTIP(ip)
}

// SetOLTMgmtNet stores the router's address on the OLT management port ("" = the default 192.168.8.1/24) and, when the
// port has the role, applies it. The address must be an IPv4 network of /16 to /29 that no other network of the router uses.
func (m *Manager) SetOLTMgmtNet(ctx context.Context, siteID, cidr, by string) error {
	cidr = strings.TrimSpace(cidr)
	if cidr == DefaultOLTMgmtNet {
		cidr = ""
	}
	e := m.cport.entry(siteID)
	if err := e.run.LockCtx(ctx); err != nil {
		return oltmErr("%s", ErrRouterBusy.Error())
	}
	defer e.run.Unlock()
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return billing.ErrNoSite
	}
	next := site
	next.OLTMgmtNet = cidr
	probe := m.Effective(next)
	if probe.OLTMgmtPort == "" {
		probe.OLTMgmtPort = "ether1" // only to check the address
	}
	if _, err := PlanOLTAdopt(probe, m.adoptOLTIP(probe), ServerTunnelIP(m.pfx)); err != nil {
		return oltmErr("%s", err.Error())
	}
	if err := m.st.Update(func(d *store.Data) error {
		s := d.Sites[siteID]
		s.OLTMgmtNet = cidr
		d.Sites[siteID] = s
		return nil
	}); err != nil {
		return err
	}
	m.audit(siteID, by, "OLT management port address set to "+OLTMgmtNetOf(next))
	if site.OLTMgmtPort == "" {
		return nil
	}
	m.st.View(func(d *store.Data) { site = d.Sites[siteID] })
	eff := m.Effective(site)
	return m.ensureOLTMgmtLocked(ctx, eff, m.adoptOLTIP(eff), by)
}
