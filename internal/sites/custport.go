package sites

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The customer port (the "bench port" of older versions) and the OLT uplink
// port are provisioned from the server. The server keeps them in step with the
// router's fields: when they are saved, and again on every sweep (self-heal),
// it makes sure that
//
//	the extra customer port is a member of br-customers (horizon=1, comment "ftn: customer port"),
//	the OLT port is a member of br-customers (no VLAN), or carries the customer VLAN
//	interface, which is the member (VLAN set),
//
// and takes the server's own membership of a port that was dropped or changed
// away. It never moves a port that is the WAN, an admin LAN port, or a member
// of any other bridge; it says so in words instead. Only routers with the v5
// layout marker are touched. All writes go through fixWriter.

// Comments of the rows the server makes.
const (
	CustPortComment  = "ftn: customer port"
	oldBenchComment  = "ftn: bench port" // written by earlier scripts; rewritten on the next pass
	CustVLANComment  = "ftn: customer VLAN"
	LANPortComment   = "ftn: LAN port" // the server's (and the script's) br-lan membership rows
	rebuildNeededMsg = "Rebuild required: reset the router and paste a new line"
)

// States of the customer port.
const (
	PortOff      = "off"      // no customer port is set
	PortOK       = "ok"       // applied
	PortApplying = "applying" // a pass is queued or running
	PortError    = "error"    // refused or failed; Message says why in words
	PortRebuild  = "rebuild"  // the router has no v5 layout: nothing is written
)

// PortApply is the apply state of one port: ok | applying | error.
type PortApply struct {
	Port    string `json:"port"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// CustomerPortStatus is what the router page shows for the customer ports.
// Port is the first customer port (the deprecated bench_port alias).
type CustomerPortStatus struct {
	Port    string   `json:"port"`
	Ports   []string `json:"ports"`
	State   string   `json:"state"`
	Text    string   `json:"text"`    // "ether5: customer port ✓", "ether5: applying…", or the error in words
	Message string   `json:"message"` // the reasons, for an error
	// OLTMessage is why the OLT uplink port could not be applied ("" when it is fine);
	// LANMessage the same for the admin LAN ports.
	OLTMessage string      `json:"olt_message,omitempty"`
	LANMessage string      `json:"lan_message,omitempty"`
	Items      []PortApply `json:"items,omitempty"` // every port the pass looked at
	At         *time.Time  `json:"at,omitempty"`
}

func portText(ports []string, state, msg string) string {
	list := strings.Join(ports, ", ")
	switch state {
	case PortOK:
		if len(ports) > 1 {
			return list + ": customer ports ✓"
		}
		return list + ": customer port ✓"
	case PortApplying:
		if len(ports) == 0 {
			return "removing the old customer port…"
		}
		return list + ": applying…"
	case PortError:
		return list + ": " + msg
	case PortRebuild:
		return rebuildNeededMsg
	}
	return ""
}

// cpEntry is the per-building state of the port work.
type cpEntry struct {
	run   sync.Mutex // one pass, automatic fix or manual port action at a time per building
	mu    sync.Mutex // guards the rest
	st    CustomerPortStatus
	fails int
	next  time.Time
	last  string // the last event text written (an unchanged error is recorded once)
	auto  map[string]*autoState
	items map[string]PortApply // apply state of every port of the last pass
	// problem is the last trouble the pass reported (state, message, OLT message). The sweep counts a pass as busy
	// only when it wrote something or the trouble is new or different; an unchanged problem keeps the slow cadence.
	problem string
	// nextSweep: SweepSite does nothing before this time (5 minutes after a clean pass, 1 minute after a change or failure).
	nextSweep time.Time
}

type cpState struct {
	mu sync.Mutex
	m  map[string]*cpEntry
	wg sync.WaitGroup
}

func (c *cpState) entry(id string) *cpEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*cpEntry{}
	}
	e := c.m[id]
	if e == nil {
		e = &cpEntry{auto: map[string]*autoState{}, items: map[string]PortApply{}}
		c.m[id] = e
	}
	return e
}

// cpBackoff is the wait after the n-th failed pass: 5 s doubling to 5 min,
// like the other router writes.
func cpBackoff(n int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < n && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CustomerPortStatus returns the last known state of the building's extra customer ports.
func (m *Manager) CustomerPortStatus(siteID string) CustomerPortStatus {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return CustomerPortStatus{State: PortOff, Ports: []string{}}
	}
	ports := SplitPorts(site.CustomerPorts)
	e := m.cport.entry(siteID)
	e.mu.Lock()
	st := e.st
	e.mu.Unlock()
	first := ""
	if len(ports) > 0 {
		first = ports[0]
	}
	if len(ports) == 0 {
		if st.State == PortApplying && len(st.Ports) == 0 {
			return st // the old port is being taken out
		}
		return CustomerPortStatus{State: PortOff, Ports: []string{}, OLTMessage: st.OLTMessage, LANMessage: st.LANMessage, Items: st.Items}
	}
	if !sameList(st.Ports, ports) || st.State == "" {
		st = CustomerPortStatus{Port: first, Ports: ports, State: PortApplying, OLTMessage: st.OLTMessage, LANMessage: st.LANMessage, Items: st.Items}
		st.Text = portText(ports, PortApplying, "")
	}
	return st
}

// PortApplyStates is the apply state of every port the last pass looked at.
func (m *Manager) PortApplyStates(siteID string) map[string]PortApply {
	e := m.cport.entry(siteID)
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]PortApply, len(e.items))
	for k, v := range e.items {
		out[k] = v
	}
	return out
}

// oltApplyError is why the OLT uplink port could not be applied ("" if nothing is wrong).
func (m *Manager) oltApplyError(siteID string) string {
	e := m.cport.entry(siteID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.OLTMessage
}

// QueueCustomerPort applies the ports of a router (customer ports, admin LAN
// ports, the OLT uplink, ports kept switched off) right away, in the
// background (a save never waits for the router). Without a v5 router the pass
// writes nothing.
func (m *Manager) QueueCustomerPort(siteID string) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || !site.Runnable() {
		return
	}
	e := m.cport.entry(siteID)
	eff := m.Effective(site)
	ports := SplitPorts(eff.CustomerPorts)
	e.mu.Lock()
	e.st = CustomerPortStatus{Ports: ports, State: PortApplying, OLTMessage: e.st.OLTMessage, LANMessage: e.st.LANMessage}
	if len(ports) > 0 {
		e.st.Port = ports[0]
	}
	e.st.Text = portText(ports, PortApplying, "")
	for _, p := range append(append(append([]string{}, ports...), SplitPorts(eff.LANPorts)...), eff.OLTInterface) {
		e.items[p] = PortApply{Port: p, State: PortApplying}
	}
	e.next = time.Time{}
	e.mu.Unlock()
	m.cport.wg.Add(1)
	go func() {
		defer m.cport.wg.Done()
		m.mu.RLock()
		parent := m.ctx
		m.mu.RUnlock()
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, time.Minute)
		defer cancel()
		m.EnsureCustomerPort(ctx, siteID, true)
	}()
}

// cpResult is the outcome of one pass.
type cpResult struct {
	state, msg, oltMsg, lanMsg string
	applied                    []string
	failed                     bool // retry with backoff
	items                      []PortApply
}

// EnsureCustomerPort runs one pass for a building: it reads the router and
// makes the customer ports, the admin LAN ports, the OLT port and the switched
// off ports match the fields. force ignores the backoff (a save). The sweep
// calls it with force false.
func (m *Manager) EnsureCustomerPort(ctx context.Context, siteID string, force bool) CustomerPortStatus {
	st, _ := m.ensureCustomerPort(ctx, siteID, force)
	return st
}

// ensureCustomerPort is EnsureCustomerPort that also says whether the pass is "busy": it wrote something, or it
// found trouble that is new or different from the last pass. An unchanged problem is not busy.
// A background pass (not force) leaves a router with paused automatic fixes alone; a save is an explicit action and runs.
func (m *Manager) ensureCustomerPort(ctx context.Context, siteID string, force bool) (CustomerPortStatus, bool) {
	e := m.cport.entry(siteID)
	e.run.Lock()
	defer e.run.Unlock()
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || !site.Runnable() || (!force && m.AutoFixPause(siteID).Paused) {
		return m.CustomerPortStatus(siteID), false
	}
	eff := m.Effective(site)
	now := m.now()
	e.mu.Lock()
	if !force && now.Before(e.next) {
		e.mu.Unlock()
		return m.CustomerPortStatus(siteID), false
	}
	e.mu.Unlock()

	res := m.cpPass(ctx, siteID, eff)

	ports := SplitPorts(eff.CustomerPorts)
	first := ""
	if len(ports) > 0 {
		first = ports[0]
	}
	e.mu.Lock()
	at := now
	e.st = CustomerPortStatus{Port: first, Ports: ports, State: res.state, Message: res.msg, OLTMessage: res.oltMsg, LANMessage: res.lanMsg, Items: res.items, At: &at}
	if len(ports) == 0 && res.state != PortError && res.state != PortRebuild {
		e.st.State = PortOff
	}
	e.st.Text = portText(ports, e.st.State, res.msg)
	if res.state != PortRebuild && !res.failed || len(res.items) > 0 {
		e.items = map[string]PortApply{}
		for _, it := range res.items {
			e.items[it.Port] = it
		}
	}
	if res.failed || res.state == PortError || res.oltMsg != "" || res.lanMsg != "" {
		e.fails++
		e.next = now.Add(cpBackoff(e.fails))
	} else {
		e.fails, e.next = 0, time.Time{}
	}
	ev := ""
	switch {
	case len(res.applied) > 0:
		ev = "ports: " + strings.Join(res.applied, "; ")
	case res.state == PortError && res.msg != "":
		ev = "customer port " + strings.Join(ports, ",") + " not applied: " + res.msg
	case res.oltMsg != "":
		ev = "OLT port " + eff.OLTInterface + " not applied: " + res.oltMsg
	case res.lanMsg != "":
		ev = "admin LAN port not applied: " + res.lanMsg
	}
	if ev != "" && ev == e.last && len(res.applied) == 0 {
		ev = "" // the same trouble again: recorded once
	}
	if ev != "" {
		e.last = ev
	} else if res.state == PortOK && res.oltMsg == "" && res.lanMsg == "" {
		e.last = ""
	}
	problem := ""
	if res.failed || res.state == PortError || res.oltMsg != "" {
		problem = string(res.state) + "|" + res.msg + "|" + res.oltMsg
	}
	changedProblem := problem != e.problem
	e.problem = problem
	st := e.st
	e.mu.Unlock()
	if ev != "" {
		_ = m.st.Update(func(d *store.Data) error {
			d.AddEvent(store.Event{Time: now, SiteID: siteID, Kind: "audit", Actor: ActorSystem, Msg: ev})
			return nil
		})
	}
	if len(res.applied) > 0 {
		m.chk.drop(siteID)
	}
	return st, len(res.applied) > 0 || (problem != "" && changedProblem)
}

var cpTables = []string{"/interface/bridge/port", "/interface", "/interface/vlan", "/interface/list/member", "/ip/address", "/ip/dhcp-server", "/ip/dhcp-client"}

// cpPass reads the router and writes what the ports need. Order: what is no
// longer wanted leaves only when nothing it was replaced by is blocked; the
// new admin LAN members go in before any old one comes out; then the OLT
// uplink, the customer ports and the switched off ports.
func (m *Manager) cpPass(ctx context.Context, siteID string, eff store.Site) cpResult {
	rt := m.Router(siteID)
	bridges, err := rt.Rows(ctx, "/interface/bridge", nil)
	if err != nil {
		return cpResult{state: PortError, msg: unreachableMsg(err), failed: true}
	}
	if !billing.HasV5Layout(bridges) {
		return cpResult{state: PortRebuild, msg: rebuildNeededMsg}
	}
	sn := readSnapshot(ctx, rt, nil, cpTables)
	for _, p := range cpTables {
		if err := sn.errs[p]; err != nil {
			return cpResult{state: PortError, msg: unreachableMsg(err), failed: true}
		}
	}
	w := m.newFixWriter(rt, eff)
	plan := planCustomerPorts(sn, eff)
	for iface := range plan.stale {
		w.stale[iface] = true
	}
	for iface := range plan.staleLAN {
		w.staleLAN[iface] = true
	}
	for _, p := range plan.ensure {
		w.custPorts[p] = true
	}
	for _, p := range plan.lanOK {
		w.lanPorts[p] = true
	}
	for _, r := range plan.disable {
		w.offPorts[r["name"]] = true
	}
	w.vlanName, w.oltPort = plan.vlanName, eff.OLTInterface
	var res cpResult
	fail := func(err error) cpResult {
		res.applied = append(res.applied, w.applied...)
		res.state, res.failed = PortError, true
		res.msg = writeErrMsg(err)
		return res
	}
	// 0. a port that changes role between the admin LAN and the customer bridge leaves its old
	// bridge first (a port can be a member of one bridge only); only the server's own row, only
	// when the port has no problem in its new role, and an admin LAN member only when another one stays.
	for _, r := range plan.moveRows {
		if r["bridge"] == LANBridge {
			if !plan.lanPresent {
				continue
			}
			w.lanSafe = true
		}
		if err := w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
			return fail(err)
		}
		w.lanSafe = false
		w.note("took %s out of %s (it moves)", r["interface"], r["bridge"])
	}
	// 1. new: the admin LAN members
	lanHave := plan.lanPresent
	for _, p := range plan.lanOK {
		added, err := ensureLANMember(ctx, w, sn, p)
		if err != nil {
			return fail(err)
		}
		if added || plan.lanMember[p] {
			lanHave = true
		}
	}
	// 2. new: the OLT uplink (the customer VLAN is added or moved onto the OLT port)
	if plan.oltProblem == "" && plan.vlanName != "" {
		if err := ensureCustomerVLAN(ctx, w, sn, eff, plan); err != nil {
			var pe *planError
			if !errors.As(err, &pe) {
				return fail(err)
			}
			plan.oltProblem = pe.msg
		}
	}
	// 3. new: the memberships of br-customers
	for _, p := range plan.ensure {
		if plan.custProblem[p] != "" {
			continue
		}
		if (p == eff.OLTInterface || p == plan.vlanName) && plan.oltProblem != "" {
			continue
		}
		if err := ensureMember(ctx, w, sn, p); err != nil {
			return fail(err)
		}
	}
	// 4. old: only when everything is right, and only the server's own rows
	if plan.oltProblem == "" && len(plan.custProblem) == 0 && len(plan.lanProblem) == 0 {
		for _, r := range plan.oltBridgeRows { // VLAN set: the OLT port itself leaves the bridge
			w.stale[r["interface"]] = true
			if err := w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
				return fail(err)
			}
			w.note("took %s out of %s (it carries VLAN %d now)", r["interface"], CustomerBridge, eff.VLANID)
		}
		for _, r := range plan.staleRows {
			w.stale[r["interface"]] = true
			if err := w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
				return fail(err)
			}
			w.note("took %s out of %s", r["interface"], CustomerBridge)
		}
		if lanHave {
			w.lanSafe = true
			for _, r := range plan.staleLANRows {
				if err := w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
					return fail(err)
				}
				w.note("took %s out of %s", r["interface"], LANBridge)
			}
		}
		for _, v := range plan.leftoverVLANs {
			w.oldVLANs[v["name"]] = true
			if err := w.removeRow(ctx, "/interface/vlan", v); err != nil {
				return fail(err)
			}
			w.note("removed the old customer VLAN %s", v["name"])
		}
	}
	// 5. ports the server keeps switched off
	for _, r := range plan.disable {
		if err := w.setRow(ctx, "/interface", r, map[string]string{"disabled": "yes"}); err != nil {
			return fail(err)
		}
		w.note("switched off %s", r["name"])
	}
	res.applied = append(res.applied, w.applied...)
	res.oltMsg = plan.oltProblem
	res.lanMsg = joinProblems(plan.lanProblem)
	var custMsgs []string
	for _, p := range plan.custPorts {
		if msg := plan.custProblem[p]; msg != "" {
			custMsgs = append(custMsgs, p+": "+msg)
			res.items = append(res.items, PortApply{Port: p, State: PortError, Message: msg})
		} else {
			res.items = append(res.items, PortApply{Port: p, State: PortOK})
		}
	}
	for _, p := range plan.lanPorts {
		if msg := plan.lanProblem[p]; msg != "" {
			res.items = append(res.items, PortApply{Port: p, State: PortError, Message: msg})
		} else {
			res.items = append(res.items, PortApply{Port: p, State: PortOK})
		}
	}
	if plan.oltProblem != "" {
		res.items = append(res.items, PortApply{Port: eff.OLTInterface, State: PortError, Message: plan.oltProblem})
	} else {
		res.items = append(res.items, PortApply{Port: eff.OLTInterface, State: PortOK})
	}
	switch {
	case len(plan.custPorts) == 0:
		res.state = PortOff
	case len(custMsgs) > 0:
		res.state, res.msg = PortError, strings.Join(custMsgs, "; ")
	default:
		res.state = PortOK
	}
	return res
}

func joinProblems(m map[string]string) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+": "+m[k])
	}
	return strings.Join(out, "; ")
}

type planError struct{ msg string }

func (e *planError) Error() string { return e.msg }

func unreachableMsg(err error) string {
	return "the router did not answer (" + shortErr(err) + "); the server will try again"
}

// writeErrMsg puts a failed write in words.
func writeErrMsg(err error) string {
	err = classifyFixErr(err)
	var rr *RouterRefusedError
	switch {
	case errors.As(err, &rr):
		return err.Error()
	case errors.Is(err, ErrRouterUnreachable):
		return "the router did not answer; the server will try again"
	}
	return err.Error()
}

// customerPlan is what a pass finds to do.
type customerPlan struct {
	custPorts     []string
	custProblem   map[string]string // per customer port: why it cannot be applied
	lanPorts      []string
	lanProblem    map[string]string
	lanOK         []string        // admin LAN ports without a problem
	lanMember     map[string]bool // admin LAN ports already members of br-lan
	lanPresent    bool            // some wanted admin LAN port is a member of br-lan
	vlanName      string
	oltProblem    string
	ensure        []string // ports that must be members of br-customers
	stale         map[string]bool
	staleRows     []routeros.Row
	staleLAN      map[string]bool
	staleLANRows  []routeros.Row
	oltBridgeRows []routeros.Row
	moveRows      []routeros.Row // server rows of ports that change role between br-customers and br-lan
	leftoverVLANs []routeros.Row // customer VLAN interfaces the server made that are no longer wanted
	disable       []routeros.Row // /interface rows the server keeps switched off and that are on
}

func serverMade(c string) bool { return c == CustPortComment || c == oldBenchComment }

// portProblems says, in words, why a port may not be put into (or kept in)
// br-customers or br-lan by the server. role is "bench" (a customer port),
// "olt" or "lan".
func portProblems(sn *snapshot, site store.Site, iface, role string) []string {
	var out []string
	exists := false
	for _, r := range sn.tbl("/interface") {
		if r["name"] == iface {
			exists = true
		}
	}
	if !exists {
		out = append(out, "the router has no port called "+iface)
	}
	custs := SplitPorts(site.CustomerPorts)
	lans := SplitPorts(site.LANPorts)
	if role != "olt" && iface == site.OLTInterface {
		out = append(out, iface+" is the OLT uplink port")
	}
	if role == "olt" && containsStr(custs, iface) {
		out = append(out, iface+" is a customer port")
	}
	if role == "lan" && containsStr(custs, iface) {
		out = append(out, iface+" is a customer port")
	}
	if wanIfaces(sn, site)[iface] {
		out = append(out, iface+" is the WAN port")
	}
	if role != "lan" && containsStr(lans, iface) {
		out = append(out, iface+" is an admin LAN port")
	}
	if role != "lan" {
		for _, r := range sn.tbl("/interface/list/member") {
			if r["list"] == "LAN" && r["interface"] == iface && !rowDisabled(r) {
				out = append(out, iface+" is in the LAN interface list")
			}
		}
	}
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["interface"] != iface {
			continue
		}
		switch {
		case r["bridge"] == CustomerBridge && role != "lan":
		case r["bridge"] == CustomerBridge && serverMade(r["comment"]):
		case r["bridge"] == LANBridge && role == "lan":
		case r["bridge"] == LANBridge && r["comment"] == LANPortComment && !containsStr(lans, iface):
			// the server's own old admin LAN membership: it is taken out when the port moves
		case r["bridge"] == LANBridge:
			out = append(out, fmt.Sprintf("%s is a port of the admin LAN bridge %s; the server never moves the admin LAN port", iface, LANBridge))
		default:
			out = append(out, fmt.Sprintf("%s is a port of bridge %s; the server does not move a port out of a bridge it did not make", iface, r["bridge"]))
		}
	}
	for _, c := range []struct{ path, what string }{{"/ip/address", "an IP address"}, {"/ip/dhcp-server", "a DHCP server"}, {"/ip/dhcp-client", "a DHCP client"}} {
		for _, r := range sn.tbl(c.path) {
			if r["interface"] == iface {
				out = append(out, iface+" itself has "+c.what)
				break
			}
		}
	}
	// A VLAN interface the server did not make on a port that would join br-customers without a VLAN
	// is reported in words (the port in the bridge would swallow its traffic); the server's own old
	// customer VLAN is a leftover, removed once the replacement is in place.
	if role == "bench" || (role == "olt" && site.VLANID == 0) {
		for _, r := range sn.tbl("/interface/vlan") {
			if r["interface"] == iface && r["comment"] != CustVLANComment {
				out = append(out, "a VLAN interface ("+r["name"]+") sits on "+iface+"; it is left as it is, and the server will not put "+iface+" in the customer bridge. The old customer port membership stays until this is fixed")
				break
			}
		}
	}
	return out
}

func planCustomerPorts(sn *snapshot, site store.Site) customerPlan {
	p := customerPlan{custPorts: SplitPorts(site.CustomerPorts), lanPorts: SplitPorts(site.LANPorts),
		custProblem: map[string]string{}, lanProblem: map[string]string{}, lanMember: map[string]bool{},
		stale: map[string]bool{}, staleLAN: map[string]bool{}}
	if site.VLANID > 0 {
		p.vlanName = customerVLANName(site.VLANID)
	}
	for _, c := range p.custPorts {
		if pr := portProblems(sn, site, c, "bench"); len(pr) > 0 {
			p.custProblem[c] = strings.Join(pr, "; ")
		}
		p.ensure = append(p.ensure, c)
	}
	for _, l := range p.lanPorts {
		if pr := portProblems(sn, site, l, "lan"); len(pr) > 0 {
			p.lanProblem[l] = strings.Join(pr, "; ")
			continue
		}
		p.lanOK = append(p.lanOK, l)
	}
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["bridge"] == LANBridge && !rowDisabled(r) && containsStr(p.lanPorts, r["interface"]) {
			p.lanMember[r["interface"]] = true
			p.lanPresent = true
		}
	}
	if pr := portProblems(sn, site, site.OLTInterface, "olt"); len(pr) > 0 {
		p.oltProblem = strings.Join(pr, "; ")
	}
	if p.vlanName != "" {
		p.ensure = append(p.ensure, p.vlanName)
		for _, r := range sn.tbl("/interface/bridge/port") {
			if r["interface"] == site.OLTInterface && r["bridge"] == CustomerBridge && serverMade(r["comment"]) {
				p.oltBridgeRows = append(p.oltBridgeRows, r)
			}
		}
	} else {
		p.ensure = append(p.ensure, site.OLTInterface)
	}
	leftover := map[string]bool{}
	for _, r := range sn.tbl("/interface/vlan") {
		if r["comment"] == CustVLANComment && r["name"] != p.vlanName {
			leftover[r["name"]] = true
			p.leftoverVLANs = append(p.leftoverVLANs, r)
		}
	}
	protected := map[string]bool{site.OLTInterface: true, site.WANInterface: true}
	for _, e := range p.ensure {
		protected[e] = true
	}
	for _, r := range sn.tbl("/interface/vlan") {
		if !leftover[r["name"]] {
			protected[r["name"]] = true
		}
	}
	for _, l := range p.lanPorts {
		protected[l] = true
	}
	for iface := range wanIfaces(sn, site) {
		protected[iface] = true
	}
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["bridge"] == CustomerBridge && serverMade(r["comment"]) && r["interface"] != "" && !protected[r["interface"]] {
			p.stale[r["interface"]] = true
			p.staleRows = append(p.staleRows, r)
		}
		// moving to the admin LAN: out of br-customers first
		if r["bridge"] == CustomerBridge && serverMade(r["comment"]) && containsStr(p.lanOK, r["interface"]) && r["interface"] != site.OLTInterface {
			p.stale[r["interface"]] = true
			p.moveRows = append(p.moveRows, r)
		}
		if r["bridge"] == LANBridge && r["comment"] == LANPortComment && r["interface"] != "" && !containsStr(p.lanPorts, r["interface"]) &&
			r["interface"] != site.WANInterface {
			p.staleLAN[r["interface"]] = true
			if containsStr(p.custPorts, r["interface"]) && p.custProblem[r["interface"]] == "" {
				p.moveRows = append(p.moveRows, r) // moving to br-customers: out of br-lan first
			} else {
				p.staleLANRows = append(p.staleLANRows, r)
			}
		}
	}
	off := SplitPorts(site.DisabledPorts)
	for _, r := range sn.tbl("/interface") {
		if containsStr(off, r["name"]) && !rowDisabled(r) && r["name"] != site.WANInterface && physicalType(r["type"]) && !wanIfaces(sn, site)[r["name"]] {
			p.disable = append(p.disable, r)
		}
	}
	return p
}

// physicalType: the interface types of real ports (an SFP or combo port is "ether").
func physicalType(t string) bool {
	switch t {
	case "ether", "wlan", "wifi":
		return true
	}
	return false
}

// ensureLANMember puts a port in br-lan with the server's comment (new members go in before old ones come out).
func ensureLANMember(ctx context.Context, w *fixWriter, sn *snapshot, iface string) (bool, error) {
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["interface"] != iface || r["bridge"] != LANBridge {
			continue
		}
		if rowDisabled(r) {
			if err := w.setRow(ctx, "/interface/bridge/port", r, map[string]string{"disabled": "no"}); err != nil {
				return false, err
			}
			w.note("%s is an admin LAN port on %s", iface, LANBridge)
			return true, nil
		}
		return false, nil
	}
	body := map[string]string{"bridge": LANBridge, "interface": iface, "comment": LANPortComment}
	if err := w.addRow(ctx, "/interface/bridge/port", body); err != nil {
		return false, err
	}
	w.note("put %s on %s as an admin LAN port", iface, LANBridge)
	return true, nil
}

// ensureMember puts a port in br-customers with horizon=1 and the server's comment.
func ensureMember(ctx context.Context, w *fixWriter, sn *snapshot, iface string) error {
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["interface"] != iface || r["bridge"] != CustomerBridge {
			continue
		}
		if !rowDisabled(r) && r["horizon"] == "1" && r["comment"] == CustPortComment {
			return nil
		}
		diff := map[string]string{"horizon": "1", "comment": CustPortComment}
		if rowDisabled(r) {
			diff["disabled"] = "no"
		}
		if err := w.setRow(ctx, "/interface/bridge/port", r, diff); err != nil {
			return err
		}
		w.note("%s is a customer port on %s", iface, CustomerBridge)
		return nil
	}
	body := map[string]string{"bridge": CustomerBridge, "interface": iface, "horizon": "1", "comment": CustPortComment}
	if err := w.addRow(ctx, "/interface/bridge/port", body); err != nil {
		return err
	}
	w.note("put %s on %s as a customer port", iface, CustomerBridge)
	return nil
}

// ensureCustomerVLAN keeps the customer VLAN interface on the OLT port.
func ensureCustomerVLAN(ctx context.Context, w *fixWriter, sn *snapshot, site store.Site, plan customerPlan) error {
	for _, r := range sn.tbl("/interface/vlan") {
		if r["name"] != plan.vlanName {
			continue
		}
		if r["comment"] != CustVLANComment {
			return &planError{"a VLAN interface called " + plan.vlanName + " exists that the server did not make"}
		}
		if r["vlan-id"] != fmt.Sprint(site.VLANID) {
			return &planError{"the VLAN id of " + plan.vlanName + " on the router differs; a new line (rebuild) is needed to change it"}
		}
		if r["interface"] == site.OLTInterface {
			return nil
		}
		if err := w.setRow(ctx, "/interface/vlan", r, map[string]string{"interface": site.OLTInterface}); err != nil {
			return err
		}
		w.note("moved VLAN %d (%s) to the OLT port %s", site.VLANID, plan.vlanName, site.OLTInterface)
		return nil
	}
	body := map[string]string{"name": plan.vlanName, "vlan-id": fmt.Sprint(site.VLANID), "interface": site.OLTInterface, "comment": CustVLANComment}
	if err := w.addRow(ctx, "/interface/vlan", body); err != nil {
		return err
	}
	w.note("made VLAN %d (%s) on the OLT port %s", site.VLANID, plan.vlanName, site.OLTInterface)
	return nil
}

// newFixWriter makes the allowlisted writer for a building's router.
func (m *Manager) newFixWriter(rt routeros.Client, eff store.Site) *fixWriter {
	insp, _ := rt.(routeros.Inspector)
	w := &fixWriter{rt: rt, insp: insp, ifaces: map[string]bool{}, custPorts: map[string]bool{}, stale: map[string]bool{}, lanPorts: map[string]bool{}, staleLAN: map[string]bool{}, offPorts: map[string]bool{}, oldVLANs: map[string]bool{}, wan: eff.WANInterface, serverIP: ServerTunnelIP(m.pfx)}
	for _, n := range []string{eff.OLTInterface, CustomerBridge} {
		if n != "" {
			w.ifaces[n] = true
		}
	}
	return w
}

// PortChoices lists the interfaces the OLT uplink port and the customer port may be: every interface the
// router lists that is not a bridge, WireGuard, VLAN, PPPoE or the WAN (so a bonding/LACP uplink works).
// Physical ports (Ethernet, SFP, combo) come first, then the rest. It reads over REST within the usual
// budget; an unreachable router gives an empty list.
func (m *Manager) PortChoices(ctx context.Context, siteID string) []string {
	iv, err := m.LiveInterfaces(ctx, siteID)
	if err != nil {
		return nil
	}
	var site store.Site
	m.st.View(func(d *store.Data) { site = d.Sites[siteID] })
	wan := m.Effective(site).WANInterface
	var phys, other []string
	for _, it := range iv.Items {
		t := strings.ToLower(it.Type)
		if it.Name == wan || it.Name == "" {
			continue
		}
		switch {
		case t == "bridge" || t == "vlan" || t == "wg" || t == "wireguard" || strings.HasPrefix(t, "pppoe"):
			continue
		case t == "ether" || IsPortName(it.Name):
			phys = append(phys, it.Name)
		default:
			other = append(other, it.Name)
		}
	}
	return append(phys, other...)
}
