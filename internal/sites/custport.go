package sites

import (
	"context"
	"errors"
	"fmt"
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

// CustomerPortStatus is what the router page shows.
type CustomerPortStatus struct {
	Port    string `json:"port"`
	State   string `json:"state"`
	Text    string `json:"text"`    // "ether5: customer port ✓", "ether5: applying…", or the error in words
	Message string `json:"message"` // the reason, for an error
	// OLTMessage is why the OLT uplink port could not be applied ("" when it is fine).
	OLTMessage string     `json:"olt_message,omitempty"`
	At         *time.Time `json:"at,omitempty"`
}

func portText(port, state, msg string) string {
	switch state {
	case PortOK:
		return port + ": customer port ✓"
	case PortApplying:
		if port == "" {
			return "removing the old customer port…"
		}
		return port + ": applying…"
	case PortError:
		return port + ": " + msg
	case PortRebuild:
		return rebuildNeededMsg
	}
	return ""
}

// cpEntry is the per-building state of the customer port work.
type cpEntry struct {
	run   sync.Mutex // one pass or automatic fix at a time per building
	mu    sync.Mutex // guards the rest
	st    CustomerPortStatus
	fails int
	next  time.Time
	last  string // the last event text written (an unchanged error is recorded once)
	auto  map[string]*autoState
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
		e = &cpEntry{auto: map[string]*autoState{}}
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

// CustomerPortStatus returns the last known state of the building's extra customer port.
func (m *Manager) CustomerPortStatus(siteID string) CustomerPortStatus {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return CustomerPortStatus{State: PortOff}
	}
	port := site.BenchPort
	e := m.cport.entry(siteID)
	e.mu.Lock()
	st := e.st
	e.mu.Unlock()
	if port == "" {
		if st.State == PortApplying && st.Port == "" {
			return st // the old port is being taken out
		}
		return CustomerPortStatus{State: PortOff, OLTMessage: st.OLTMessage}
	}
	if st.Port != port || st.State == "" {
		st = CustomerPortStatus{Port: port, State: PortApplying, OLTMessage: st.OLTMessage}
		st.Text = portText(port, PortApplying, "")
	}
	return st
}

// oltApplyError is why the OLT uplink port could not be applied ("" if nothing is wrong).
func (m *Manager) oltApplyError(siteID string) string {
	e := m.cport.entry(siteID)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.OLTMessage
}

// QueueCustomerPort applies the customer and OLT ports of a router right away,
// in the background (a save never waits for the router). Without a v5 router
// the pass writes nothing.
func (m *Manager) QueueCustomerPort(siteID string) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || !site.Runnable() {
		return
	}
	e := m.cport.entry(siteID)
	port := m.Effective(site).BenchPort
	e.mu.Lock()
	e.st = CustomerPortStatus{Port: port, State: PortApplying, OLTMessage: e.st.OLTMessage}
	e.st.Text = portText(port, PortApplying, "")
	if port == "" {
		e.st.State = PortApplying
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
	state, msg, oltMsg string
	applied            []string
	failed             bool // retry with backoff
}

// EnsureCustomerPort runs one pass for a building: it reads the router and
// makes the extra customer port and the OLT port match the fields. force
// ignores the backoff (a save). The sweep calls it with force false.
func (m *Manager) EnsureCustomerPort(ctx context.Context, siteID string, force bool) CustomerPortStatus {
	e := m.cport.entry(siteID)
	e.run.Lock()
	defer e.run.Unlock()
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || !site.Runnable() {
		return m.CustomerPortStatus(siteID)
	}
	eff := m.Effective(site)
	now := m.now()
	e.mu.Lock()
	if !force && now.Before(e.next) {
		e.mu.Unlock()
		return m.CustomerPortStatus(siteID)
	}
	e.mu.Unlock()

	res := m.cpPass(ctx, siteID, eff)

	e.mu.Lock()
	at := now
	e.st = CustomerPortStatus{Port: eff.BenchPort, State: res.state, Message: res.msg, OLTMessage: res.oltMsg, At: &at}
	if eff.BenchPort == "" && res.state != PortError && res.state != PortRebuild {
		e.st.State = PortOff
	}
	e.st.Text = portText(eff.BenchPort, e.st.State, res.msg)
	if res.failed || res.state == PortError || res.oltMsg != "" {
		e.fails++
		e.next = now.Add(cpBackoff(e.fails))
	} else {
		e.fails, e.next = 0, time.Time{}
	}
	ev := ""
	switch {
	case len(res.applied) > 0:
		ev = "customer port: " + strings.Join(res.applied, "; ")
	case res.state == PortError && res.msg != "":
		ev = "customer port " + eff.BenchPort + " not applied: " + res.msg
	case res.oltMsg != "":
		ev = "OLT port " + eff.OLTInterface + " not applied: " + res.oltMsg
	}
	if ev != "" && ev == e.last && len(res.applied) == 0 {
		ev = "" // the same trouble again: recorded once
	}
	if ev != "" {
		e.last = ev
	} else if res.state == PortOK && res.oltMsg == "" {
		e.last = ""
	}
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
	return st
}

var cpTables = []string{"/interface/bridge/port", "/interface", "/interface/vlan", "/interface/list/member", "/ip/address", "/ip/dhcp-server", "/ip/dhcp-client"}

// cpPass reads the router and writes what the customer ports need.
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
	for _, p := range plan.ensure {
		w.custPorts[p] = true
	}
	w.vlanName, w.oltPort = plan.vlanName, eff.OLTInterface
	var res cpResult
	fail := func(err error) cpResult {
		res.applied = append(res.applied, w.applied...)
		res.state, res.failed = PortError, true
		res.msg = writeErrMsg(err)
		return res
	}

	// 1. the server's own membership of ports that are no longer wanted
	for _, r := range plan.staleRows {
		if err := w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
			return fail(err)
		}
		w.note("took %s out of %s", r["interface"], CustomerBridge)
	}
	// 2. the OLT uplink
	if plan.oltProblem == "" && plan.vlanName != "" {
		if err := ensureCustomerVLAN(ctx, w, sn, eff, plan); err != nil {
			var pe *planError
			if errors.As(err, &pe) {
				plan.oltProblem = pe.msg
			} else {
				return fail(err)
			}
		}
		for _, r := range plan.oltBridgeRows { // a member of br-customers made by the server: out of the bridge
			if err := w.removeRow(ctx, "/interface/bridge/port", r); err != nil {
				return fail(err)
			}
			w.note("took %s out of %s (it carries VLAN %d now)", r["interface"], CustomerBridge, eff.VLANID)
		}
	}
	// 3. memberships
	for _, p := range plan.ensure {
		if p == plan.benchPort && plan.benchProblem != "" {
			continue
		}
		if (p == eff.OLTInterface || p == plan.vlanName) && plan.oltProblem != "" {
			continue
		}
		if err := ensureMember(ctx, w, sn, p); err != nil {
			return fail(err)
		}
	}
	res.applied = append(res.applied, w.applied...)
	res.oltMsg = plan.oltProblem
	switch {
	case plan.benchPort == "":
		res.state = PortOff
	case plan.benchProblem != "":
		res.state, res.msg = PortError, plan.benchProblem
	default:
		res.state = PortOK
	}
	return res
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
	benchPort, vlanName string
	benchProblem        string
	oltProblem          string
	ensure              []string // ports that must be members of br-customers
	stale               map[string]bool
	staleRows           []routeros.Row
	oltBridgeRows       []routeros.Row
}

func serverMade(c string) bool { return c == CustPortComment || c == oldBenchComment }

// portProblems says, in words, why a port may not be put into (or kept in)
// br-customers by the server. role is "bench" or "olt".
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
	if role == "bench" && iface == site.OLTInterface {
		out = append(out, iface+" is the OLT uplink port")
	}
	if role == "olt" && iface == site.BenchPort {
		out = append(out, iface+" is the extra customer port")
	}
	if wanIfaces(sn, site)[iface] {
		out = append(out, iface+" is the WAN port")
	}
	for _, p := range SplitPorts(site.LANPorts) {
		if p == iface {
			out = append(out, iface+" is an admin LAN port")
		}
	}
	for _, r := range sn.tbl("/interface/list/member") {
		if r["list"] == "LAN" && r["interface"] == iface && !rowDisabled(r) {
			out = append(out, iface+" is in the LAN interface list")
		}
	}
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["interface"] != iface || r["bridge"] == CustomerBridge {
			continue
		}
		if r["bridge"] == LANBridge {
			out = append(out, fmt.Sprintf("%s is a port of the admin LAN bridge %s; the server never moves the admin LAN port", iface, LANBridge))
		} else {
			out = append(out, fmt.Sprintf("%s is a port of bridge %s; the server does not move a port out of a bridge it did not make for customers", iface, r["bridge"]))
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
	if role == "bench" {
		for _, r := range sn.tbl("/interface/vlan") {
			if r["interface"] == iface {
				out = append(out, "a VLAN interface ("+r["name"]+") sits on "+iface)
				break
			}
		}
	}
	return out
}

func planCustomerPorts(sn *snapshot, site store.Site) customerPlan {
	p := customerPlan{benchPort: site.BenchPort, stale: map[string]bool{}}
	if site.VLANID > 0 {
		p.vlanName = customerVLANName(site.VLANID)
	}
	if p.benchPort != "" {
		if pr := portProblems(sn, site, p.benchPort, "bench"); len(pr) > 0 {
			p.benchProblem = strings.Join(pr, "; ")
		}
		p.ensure = append(p.ensure, p.benchPort)
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
	protected := map[string]bool{site.OLTInterface: true, site.WANInterface: true}
	for _, e := range p.ensure {
		protected[e] = true
	}
	for _, r := range sn.tbl("/interface/vlan") {
		protected[r["name"]] = true
	}
	for _, l := range SplitPorts(site.LANPorts) {
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
	}
	return p
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
	w := &fixWriter{rt: rt, insp: insp, ifaces: map[string]bool{}, custPorts: map[string]bool{}, stale: map[string]bool{}, serverIP: ServerTunnelIP(m.pfx)}
	for _, n := range []string{eff.OLTInterface, CustomerBridge} {
		if n != "" {
			w.ifaces[n] = true
		}
	}
	return w
}

// PortChoices lists the router's real Ethernet, SFP and combo ports for the
// pickers of the OLT uplink port and the customer port; typed entry stays
// possible. It reads over REST within the usual budget; an unreachable router
// gives an empty list.
func (m *Manager) PortChoices(ctx context.Context, siteID string) []string {
	iv, err := m.LiveInterfaces(ctx, siteID)
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range iv.Items {
		if it.Type == "vlan" || it.Type == "bridge" || it.Type == "wg" || it.Type == "wireguard" {
			continue
		}
		if it.Type == "ether" || IsPortName(it.Name) {
			out = append(out, it.Name)
		}
	}
	return out
}
