package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// IPoE provisioning. The accounts table is the truth; the reconciler makes a
// building's router match it through the RouterOS REST row API:
//
//   - one static DHCP lease per flat (MAC -> the flat's fixed IP) on ftn-dhcp,
//   - one simple queue "ftn-<code>" per flat at the package speed,
//   - the "ftn-suspended" address-list holds the IP of every flat that has no
//     paid time .
//
// It only ever changes rows whose comment starts "ftn:acct:". A row that is
// not ours but clashes with a flat (same MAC, IP or queue name) is reported
// and left alone. Un-suspends are written first, so a payment restores the
// flat before anything else happens. A second pass over an unchanged router
// writes nothing.

// Names the join script and the reconciler share.
const (
	AcctPrefix     = "ftn:acct:"
	DHCPServerName = "ftn-dhcp"

	// The v5 layout marker: the bootstrap script creates the customer bridge
	// with this name and comment, and nothing else does. A router without it
	// was not built by the v5 script, so nothing is written to it.
	CustomerBridgeName    = "br-customers"
	CustomerBridgeComment = "ftn: customer bridge"
	SuspendedList         = "ftn-suspended"
	UnregList             = "ftn-unreg"
	unregComment          = "ftn: unregistered range"

	pathBridge = "/interface/bridge"
	pathLease  = "/ip/dhcp-server/lease"
	pathDHCP   = "/ip/dhcp-server"
	pathQueue  = "/queue/simple"
	pathList   = "/ip/firewall/address-list"
	pathARP    = "/ip/arp"

	// walledRate is the queue of a flat that has no package yet.
	walledRate = "2M/2M"

	// staticLeaseTime is the lease time of a flat's static lease.
	staticLeaseTime = "1h"
)

// AcctComment is the comment every row of a flat carries on the router. The
// join script renders the same text, so the first reconcile pass finds nothing
// to change.
func AcctComment(code, flat string) string {
	return AcctPrefix + code + " " + strings.Join(strings.Fields(flat), " ")
}

// QueueName is the simple queue of a flat.
func QueueName(code string) string { return "ftn-" + code }

// QueueLimit is the queue's max-limit for a package: "up/down", both the
// package speed. A flat with no package gets a small walled rate.
func QueueLimit(p store.Package) string {
	if p.SpeedMbps <= 0 {
		return walledRate
	}
	return p.RateLimit()
}

// codeOfComment returns the account code of one of our comments, or "".
func codeOfComment(c string) string {
	rest, ok := strings.CutPrefix(c, AcctPrefix)
	if !ok {
		return ""
	}
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	return ""
}

func isOurs(r routeros.Row) bool { return strings.HasPrefix(r["comment"], AcctPrefix) }

func isDynamic(r routeros.Row) bool { return r["dynamic"] == "true" || r["dynamic"] == "yes" }

// ProvisionStatus is what the reconciler knows about one building: when it
// last compared the router with the accounts table, how far apart they were,
// and what could not be done. It is the "provision" object of the API's Site.
type ProvisionStatus struct {
	LastRunAt  time.Time `json:"last_run_at"`
	InSync     bool      `json:"in_sync"`
	Drift      int       `json:"drift"`       // rows still different after the pass, plus flats that could not be provisioned
	PendingOps int       `json:"pending_ops"` // router writes still queued
	LastError  string    `json:"last_error"`
	// ForeignConflicts describes rows on the router that are not ours but
	// clash with an account (same MAC or IP), and flats skipped because their
	// MAC is flagged; they are reported, never touched.
	ForeignConflicts []string `json:"foreign_conflicts"`
	// Additive fields (the first four keys above are the frozen contract).
	Mode     string `json:"mode,omitempty"`     // always ipoe
	Note     string `json:"note,omitempty"`     // e.g. router not set up
	Repaired int    `json:"repaired,omitempty"` // rows changed by the last pass
}

// AccountRouterState is what the router side looks like for one account. It
// is the "router" object of the API's Account.
type AccountRouterState struct {
	InSync            bool      `json:"in_sync"`
	SuspendedOnRouter bool      `json:"suspended_on_router"`
	LeaseStatus       string    `json:"lease_status"` // bound | waiting | none
	LastSeen          time.Time `json:"last_seen"`
	PendingTask       bool      `json:"pending_task"`
	LastError         string    `json:"last_error"`
}

// provisionBlock says, in words, why an account must not be provisioned on its
// router: it has no apartment, its apartment is gone, or its router does not
// serve its apartment. "" means nothing stands in the way. Call it inside View.
func provisionBlock(d *store.Data, a store.Account) string {
	if a.ApartmentID == "" {
		return "no apartment: choose the building this customer lives in, then it is provisioned"
	}
	apt, ok := d.Apartments[a.ApartmentID]
	if !ok {
		return "its apartment no longer exists: choose the building this customer lives in"
	}
	if !d.ApartmentServedBy(a.ApartmentID, a.SiteID) {
		return fmt.Sprintf("not provisioned: router %s does not serve %s. Link the router to the apartment, or move the customer to a router of %s", d.Sites[a.SiteID].Name, apt.Name, apt.Name)
	}
	return ""
}

// AccountProblem is provisionBlock for the pages and the API ("" = none).
func (s *Service) AccountProblem(accountID string) (why string) {
	s.st.View(func(d *store.Data) {
		if a, ok := d.Accounts[accountID]; ok {
			why = provisionBlock(d, a)
		}
	})
	return
}

// ---- status -------------------------------------------------------------

// provState is what the last reconcile pass of a building found.
type provState struct {
	mu      sync.Mutex
	status  ProvisionStatus
	accts   map[string]acctSnap // by account code
	seen    map[string]time.Time
	started time.Time       // first pass, the baseline of "not seen for 24 h"
	foreign map[string]bool // addresses of the router's own static leases, from the last pass
	sawOn   bool            // ftn-dhcp has been seen enabled by this server
	run     sync.Mutex      // one pass at a time per building
}

type acctSnap struct {
	leaseStatus string // bound | waiting | none
	lastSeen    time.Time
	suspended   bool // on the router's suspended list
	clash       bool // a foreign row or a flag keeps this flat from being provisioned
	inSync      bool
	err         string
}

func (s *Service) prov(siteID string) *provState {
	st := s.state(siteID)
	st.provOnce.Do(func() { st.prov = &provState{accts: map[string]acctSnap{}, seen: map[string]time.Time{}} })
	return st.prov
}

// ProvisionStatus reports the reconciler state of a building.
func (s *Service) ProvisionStatus(siteID string) ProvisionStatus {
	ps := s.prov(siteID)
	ps.mu.Lock()
	out := ps.status
	ps.mu.Unlock()
	if out.ForeignConflicts == nil {
		out.ForeignConflicts = []string{}
	}
	out.Mode = store.AccessIPoE
	s.st.View(func(d *store.Data) {
		for _, t := range d.Tasks {
			if t.SiteID == siteID {
				out.PendingOps++
			}
		}
	})
	if out.PendingOps > 0 {
		out.InSync = false
	}
	return out
}

// AccountRouterState reports the router-side state of an account.
func (s *Service) AccountRouterState(accountID string) AccountRouterState {
	st := AccountRouterState{InSync: true, LeaseStatus: "none"}
	var site store.Site
	var code string
	s.st.View(func(d *store.Data) {
		a, ok := d.Accounts[accountID]
		if ok {
			site, code = d.Sites[a.SiteID], a.Code
		}
		for _, t := range d.Tasks {
			if t.AccountID == accountID {
				st.PendingTask, st.InSync, st.LastError = true, false, t.LastErr
				break
			}
		}
	})
	ps := s.prov(site.ID)
	ps.mu.Lock()
	snap, ok := ps.accts[code]
	ps.mu.Unlock()
	if !ok {
		st.InSync = false // not looked at yet
		return st
	}
	st.LeaseStatus, st.LastSeen, st.SuspendedOnRouter = snap.leaseStatus, snap.lastSeen, snap.suspended
	if snap.err != "" && st.LastError == "" {
		st.LastError = snap.err
	}
	st.InSync = st.InSync && snap.inSync
	return st
}

// ---- the pass -----------------------------------------------------------

type provOp struct {
	phase      int // 1 un-suspend, 2 removals, 3 add/change, 4 suspend
	kind       string
	path, id   string
	code, desc string
	body       map[string]string
}

const (
	phaseUnsuspend = 1
	phaseRemove    = 2
	phaseWrite     = 3
	phaseSuspend   = 4
)

// ProvisionRun is what one reconcile pass did.
type ProvisionRun struct {
	Ops       int // rows changed (or that would be changed in a dry run)
	Failed    int
	Conflicts []string
	Waiting   bool // the router has no ftn-dhcp yet: nothing was written
}

type wantAcct struct {
	a        store.Account
	pkg      store.Package
	entitled bool
	comment  string
}

// ErrNotSetUp is returned by ReconcileSite when the router is reachable but does
// not have the v5 layout. Nothing was written, and the caller keeps its queued
// tasks for when the router has been rebuilt.
var ErrNotSetUp = errors.New("router not set up: it does not have the v5 layout, nothing was written")

// HasV5Layout reports whether the bridge rows hold the v5 marker.
func HasV5Layout(bridges []routeros.Row) bool {
	for _, r := range bridges {
		if r["name"] == CustomerBridgeName && r["comment"] == CustomerBridgeComment {
			return true
		}
	}
	return false
}

// ReconcileSite compares the building's router with the accounts table and
// changes the router where they differ. It
// is safe to call from anywhere: passes of one building run one at a time.
func (s *Service) ReconcileSite(ctx context.Context, siteID string) (ProvisionRun, error) {
	var run ProvisionRun
	site, ok := s.site(siteID)
	if !ok || !site.Runnable() {
		return run, nil
	}
	ps := s.prov(siteID)
	ps.run.Lock()
	defer ps.run.Unlock()
	run, err := s.reconcileSite(ctx, site, ps)
	if err != nil {
		ps.mu.Lock()
		ps.status.LastRunAt, ps.status.LastError, ps.status.InSync = s.now(), err.Error(), false
		ps.mu.Unlock()
	}
	return run, err
}

func (s *Service) reconcileSite(ctx context.Context, site store.Site, ps *provState) (ProvisionRun, error) {
	var run ProvisionRun
	now := s.now()
	siteID := site.ID
	pl, err := s.PlanFor(site)
	if err != nil {
		return run, err
	}
	// Every flat needs an address before anything can be written.
	if _, err := s.EnsureIPs(siteID); err != nil && !errors.Is(err, store.ErrNoChange) {
		return run, fmt.Errorf("allocating addresses: %w", err)
	}
	var wants []wantAcct
	var skipped []string
	blocked := map[string]string{}       // codes whose router does not serve their apartment, with the words for it
	flagged := map[string]bool{}         // codes we must not touch
	flaggedEntitled := map[string]bool{} // of those, the flats that have paid time (their walled rows must go)
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID != siteID {
				continue
			}
			switch {
			case provisionBlock(d, a) != "":
				// The one rule of apartments: an account is only ever written on its own
				// router, and that router must serve the account's apartment. Nothing is
				// written for it, and its existing rows are left as they are.
				why := provisionBlock(d, a)
				blocked[a.Code] = why
				flagged[a.Code] = true
				flaggedEntitled[a.Code] = AccountState(a, now) == StateActive
				skipped = append(skipped, fmt.Sprintf("%s (%s): %s", a.Flat, a.Code, why))
			case a.MACConflict:
				flagged[a.Code] = true
				flaggedEntitled[a.Code] = AccountState(a, now) == StateActive
				skipped = append(skipped, fmt.Sprintf("%s (%s): MAC %s is flagged as a duplicate, not provisioned until fixed", a.Flat, a.Code, a.MAC))
			case a.MAC == "" || a.IP == "":
				flagged[a.Code] = true
				flaggedEntitled[a.Code] = AccountState(a, now) == StateActive
				skipped = append(skipped, fmt.Sprintf("%s (%s): no MAC or no address yet, not provisioned", a.Flat, a.Code))
			default:
				wants = append(wants, wantAcct{a: a, pkg: d.Packages[a.PackageID], entitled: AccountState(a, now) == StateActive, comment: AcctComment(a.Code, a.Flat)})
			}
		}
	})
	rt := s.router(siteID)
	leases, err := rt.Rows(ctx, pathLease, nil)
	if err != nil {
		return run, fmt.Errorf("reading DHCP leases: %w", err)
	}
	queues, err := rt.Rows(ctx, pathQueue, nil)
	if err != nil {
		return run, fmt.Errorf("reading queues: %w", err)
	}
	suspended, err := rt.Rows(ctx, pathList, map[string]string{"list": SuspendedList})
	if err != nil {
		return run, fmt.Errorf("reading the suspended list: %w", err)
	}
	unreg, err := rt.Rows(ctx, pathList, map[string]string{"list": UnregList})
	if err != nil {
		return run, fmt.Errorf("reading the unregistered list: %w", err)
	}
	servers, err := rt.Rows(ctx, pathDHCP, nil)
	if err != nil {
		return run, fmt.Errorf("reading DHCP servers: %w", err)
	}
	bridges, err := rt.Rows(ctx, pathBridge, nil)
	if err != nil {
		return run, fmt.Errorf("reading bridges: %w", err)
	}
	arp, arpErr := rt.Rows(ctx, pathARP, nil) // best effort: only feeds the device list
	if arpErr != nil {
		arp = nil
	}

	// Device list and swap detection never depend on the writes below.
	s.collectDevices(site, pl, wants, leases, arp, ps)

	var ftn routeros.Row
	for _, r := range servers {
		if r["name"] == DHCPServerName {
			ftn = r
		}
	}

	snap := map[string]acctSnap{}
	ifc := ""
	if ftn != nil {
		ifc = ftn["interface"]
	}
	pd := planOps(site, wants, flagged, leases, queues, suspended, snap, now, planExtra{unreg: unreg, arp: arp, ifc: ifc, flaggedEntitled: flaggedEntitled, blocked: blockedSet(blocked)})
	ps.mu.Lock()
	ps.foreign = map[string]bool{}
	for _, ip := range pd.foreignIPs {
		ps.foreign[ip] = true
	}
	for code, sn := range snap { // the router's own "last seen" survives a restart of this server
		if sn.lastSeen.After(ps.seen[code]) {
			ps.seen[code] = sn.lastSeen
		}
	}
	ps.mu.Unlock()
	for code, why := range blocked {
		snap[code] = acctSnap{leaseStatus: "none", clash: true, err: why}
	}
	run.Conflicts = append(pd.conflicts, skipped...)
	run.Ops = len(pd.ops)

	// The one gate: a router without the v5 layout (the customer bridge the
	// script makes, and its ftn-dhcp server) has not been set up by the v5
	// script, so nothing is written. The caller gets ErrNotSetUp and keeps its
	// tasks queued.
	note := ""
	if ftn == nil || !HasV5Layout(bridges) {
		run.Waiting, note = true, "router not set up: rebuild required (reset the router and paste a new line), nothing was written"
		pd.ops = nil
	} else {
		ps.mu.Lock()
		ps.sawOn = true
		ps.mu.Unlock()
	}

	var firstErr error
	failed := map[string]bool{}
	sort.SliceStable(pd.ops, func(i, j int) bool { return pd.ops[i].phase < pd.ops[j].phase })
	for _, op := range pd.ops {
		if ctx.Err() != nil {
			firstErr = ctx.Err()
			break
		}
		var e error
		switch op.kind {
		case "add":
			_, e = rt.AddRow(ctx, op.path, op.body)
		case "set":
			e = rt.SetRow(ctx, op.path, op.id, op.body)
		case "rm":
			e = rt.RemoveRow(ctx, op.path, op.id)
		}
		if e != nil {
			run.Failed++
			failed[op.code] = true
			if firstErr == nil {
				firstErr = fmt.Errorf("%s %s: %w", op.kind, op.desc, e)
			}
			var ae *routeros.APIError
			if !errors.As(e, &ae) {
				break // the router is unreachable: stop and retry later
			}
		}
	}
	// What the router holds now: a flat is in sync when its rows were all
	// written (or already right) and nothing clashes.
	for code, sn := range snap {
		sn.inSync = !sn.clash && !failed[code] && !run.Waiting
		if failed[code] {
			sn.err = "a router write failed"
		}
		snap[code] = sn
	}
	if len(pd.ops) > 0 {
		s.log.Info("ipoe reconcile", "site", siteID, "changes", len(pd.ops), "failed", run.Failed)
	}

	ps.mu.Lock()
	if ps.started.IsZero() {
		ps.started = now
	}
	ps.accts = snap
	st := ProvisionStatus{LastRunAt: now, Mode: store.AccessIPoE, Note: note, ForeignConflicts: append([]string{}, run.Conflicts...),
		Repaired: len(pd.ops) - run.Failed}
	st.Drift = run.Failed + len(run.Conflicts)
	if run.Waiting {
		st.Drift = run.Ops + len(run.Conflicts)
		st.Repaired = 0
	}
	if firstErr != nil {
		st.LastError = firstErr.Error()
	}
	st.InSync = firstErr == nil && !run.Waiting && st.Drift == 0
	ps.status = st
	ps.mu.Unlock()
	if firstErr == nil && run.Waiting {
		firstErr = ErrNotSetUp
	}
	return run, firstErr
}

// planExtra is the rest of what planOps reads.
type planExtra struct {
	unreg, arp      []routeros.Row
	ifc             string          // the customer interface (ftn-dhcp's)
	flaggedEntitled map[string]bool // flagged flats that have paid time
	// blocked: accounts that must not be provisioned on this router (provisionBlock). They stay
	// flagged, and the lease, queue and ARP row we wrote for them earlier are removed, so a
	// blocked account never keeps its last router state.
	blocked map[string]bool
}

// opSet collects what a pass wants to do.
type opSet struct {
	foreignIPs []string // addresses held by the router's own static leases
	ops        []provOp
	conflicts  []string
}

// planOps compares the three tables with the wanted state. It is pure: it
// reads rows, it writes nothing.
func planOps(site store.Site, wants []wantAcct, flagged map[string]bool, leases, queues, suspended []routeros.Row, snap map[string]acctSnap, now time.Time, x planExtra) opSet {
	var out opSet
	byCode := map[string]*wantAcct{}
	for i := range wants {
		byCode[wants[i].a.Code] = &wants[i]
	}
	add := func(o provOp) { out.ops = append(out.ops, o) }
	conflict := func(f string, a ...any) { out.conflicts = append(out.conflicts, fmt.Sprintf(f, a...)) }

	// Group our rows by account code; keep foreign ones for clash checks.
	ours := func(rows []routeros.Row) (map[string][]routeros.Row, []routeros.Row, []routeros.Row) {
		m := map[string][]routeros.Row{}
		var foreign, dyn []routeros.Row
		for _, r := range rows {
			switch {
			case isOurs(r):
				m[codeOfComment(r["comment"])] = append(m[codeOfComment(r["comment"])], r)
			case isDynamic(r):
				dyn = append(dyn, r)
			case strings.HasPrefix(r["comment"], "ftn:"):
				// made by the join script (not a flat's row): not ours to judge, not a clash
			default:
				foreign = append(foreign, r)
			}
		}
		return m, foreign, dyn
	}
	ourLeases, foreignLeases, dynLeases := ours(leases)
	ourQueues, foreignQueues, _ := ours(queues)
	ourList, foreignList, _ := ours(suspended)

	// --- leases
	evicted := map[string]bool{} // row ids removed to make room for another flat
	removed := map[string]bool{}
	rm := func(path string, r routeros.Row, code, why string) {
		if removed[path+r[routeros.RowID]] {
			return
		}
		removed[path+r[routeros.RowID]] = true
		add(provOp{phase: phaseRemove, kind: "rm", path: path, id: r[routeros.RowID], code: code, desc: why})
	}
	// An address or MAC that another of our rows holds today, for a flat that
	// wants something else, is freed first.
	wantAddr, wantMAC := map[string]string{}, map[string]string{}
	for _, w := range wants {
		wantAddr[w.a.IP], wantMAC[strings.ToUpper(w.a.MAC)] = w.a.Code, w.a.Code
	}
	for code, rows := range ourLeases {
		for _, r := range rows {
			if owner, ok := wantAddr[r["address"]]; ok && owner != code {
				evicted[r[routeros.RowID]] = true
			}
			if owner, ok := wantMAC[strings.ToUpper(r["mac-address"])]; ok && owner != code {
				evicted[r[routeros.RowID]] = true
			}
		}
	}
	for _, r := range foreignLeases {
		if r["address"] != "" {
			out.foreignIPs = append(out.foreignIPs, r["address"])
		}
	}
	for _, w := range wants {
		a := w.a
		sn := acctSnap{leaseStatus: "none"}
		// Foreign static lease on our MAC or address: report, do not touch.
		clash := false
		for _, r := range foreignLeases {
			if strings.EqualFold(r["mac-address"], a.MAC) || r["address"] == a.IP {
				conflict("%s (%s): the router has its own static lease %s %s (id %s) that is not ours; left alone, no lease written", a.Flat, a.Code, r["mac-address"], r["address"], r[routeros.RowID])
				clash = true
			}
		}
		// A dynamic lease on our MAC or address would block the static one.
		for _, r := range dynLeases {
			if r["server"] != DHCPServerName {
				continue // another DHCP server's leases are not ours to remove
			}
			if strings.EqualFold(r["mac-address"], a.MAC) || (r["address"] == a.IP && !strings.EqualFold(r["mac-address"], a.MAC)) {
				if !clash {
					rm(pathLease, r, a.Code, "dynamic lease "+r["mac-address"]+" "+r["address"])
				}
			}
		}
		// A flat's static lease lasts an hour; unregistered devices renew every minute (the script's server default),
		// so a newly registered flat moves to its fixed address within about a minute.
		want := map[string]string{"mac-address": a.MAC, "address": a.IP, "server": DHCPServerName, "lease-time": staticLeaseTime, "comment": w.comment}
		var mine []routeros.Row
		for _, r := range ourLeases[a.Code] {
			if evicted[r[routeros.RowID]] {
				rm(pathLease, r, a.Code, "lease "+r["address"]+" (address or MAC wanted by another flat)")
				continue
			}
			mine = append(mine, r)
		}
		// keep the row that already has the right MAC, else the first
		sort.SliceStable(mine, func(i, j int) bool {
			return strings.EqualFold(mine[i]["mac-address"], a.MAC) && !strings.EqualFold(mine[j]["mac-address"], a.MAC)
		})
		for i, r := range mine {
			if i > 0 {
				rm(pathLease, r, a.Code, "duplicate lease row "+r["address"])
			}
		}
		if clash {
			sn.clash = true
			snap[a.Code] = sn
			continue
		}
		if len(mine) == 0 {
			add(provOp{phase: phaseWrite, kind: "add", path: pathLease, code: a.Code, body: want, desc: fmt.Sprintf("lease %s %s (%s)", a.MAC, a.IP, a.Flat)})
			sn.leaseStatus = "waiting"
		} else {
			r := mine[0]
			diff := rowDiff(r, want)
			if r["disabled"] == "true" || r["disabled"] == "yes" {
				diff["disabled"] = "no"
			}
			if len(diff) > 0 {
				add(provOp{phase: phaseWrite, kind: "set", path: pathLease, id: r[routeros.RowID], code: a.Code, body: diff, desc: fmt.Sprintf("lease %s %s (%s)", a.MAC, a.IP, a.Flat)})
			}
			sn.leaseStatus = "waiting"
			if r["status"] == "bound" {
				sn.leaseStatus = "bound"
				sn.lastSeen = now
			} else if d := routeros.ParseRouterDuration(r["last-seen"]); d > 0 && r["last-seen"] != "never" {
				sn.lastSeen = now.Add(-d)
			}
		}
		snap[a.Code] = sn
	}
	// leases of flats that no longer exist here
	for code, rows := range ourLeases {
		if byCode[code] != nil || (flagged[code] && !x.blocked[code]) {
			continue
		}
		for _, r := range rows {
			rm(pathLease, r, code, "stray lease "+r["address"])
		}
	}

	// --- queues
	for _, w := range wants {
		a := w.a
		sn := snap[a.Code]
		name := QueueName(a.Code)
		clash := false
		for _, r := range foreignQueues {
			if r["name"] == name || strings.TrimSuffix(r["target"], "/32") == a.IP {
				conflict("%s (%s): the router has its own queue %q (target %s) that is not ours; left alone, no queue written", a.Flat, a.Code, r["name"], r["target"])
				clash = true
			}
		}
		if clash {
			sn.clash = true
			snap[a.Code] = sn
			continue
		}
		want := map[string]string{"name": name, "target": a.IP + "/32", "max-limit": QueueLimit(w.pkg), "comment": w.comment}
		mine := ourQueues[a.Code]
		for i, r := range mine {
			if i > 0 {
				rm(pathQueue, r, a.Code, "duplicate queue "+r["name"])
			}
		}
		if len(mine) == 0 {
			add(provOp{phase: phaseWrite, kind: "add", path: pathQueue, code: a.Code, body: want, desc: fmt.Sprintf("queue %s (%s)", name, a.Flat)})
		} else {
			diff := rowDiff(mine[0], want)
			if mine[0]["disabled"] == "true" || mine[0]["disabled"] == "yes" {
				diff["disabled"] = "no"
			}
			if len(diff) > 0 {
				add(provOp{phase: phaseWrite, kind: "set", path: pathQueue, id: mine[0][routeros.RowID], code: a.Code, body: diff, desc: fmt.Sprintf("queue %s (%s)", name, a.Flat)})
			}
		}
		snap[a.Code] = sn
	}
	for code, rows := range ourQueues {
		if byCode[code] != nil || (flagged[code] && !x.blocked[code]) {
			continue
		}
		for _, r := range rows {
			rm(pathQueue, r, code, "stray queue "+r["name"])
		}
	}

	// --- suspended list: un-suspends first, suspends last
	for _, w := range wants {
		a := w.a
		sn := snap[a.Code]
		mine := ourList[a.Code]
		wantIn := !w.entitled
		var keep *routeros.Row
		for i, r := range mine {
			if wantIn && keep == nil && r["address"] == a.IP {
				keep = &mine[i]
				if r["disabled"] == "true" || r["disabled"] == "yes" { // a disabled entry walls nobody: turn it on
					add(provOp{phase: phaseSuspend, kind: "set", path: pathList, id: r[routeros.RowID], code: a.Code, body: map[string]string{"disabled": "no"}, desc: "enable suspended entry " + a.IP + " (" + a.Flat + ")"})
				}
				continue
			}
			add(provOp{phase: phaseUnsuspend, kind: "rm", path: pathList, id: r[routeros.RowID], code: a.Code, desc: "suspended entry " + r["address"] + " (" + a.Flat + ")"})
		}
		for _, r := range foreignList {
			if r["address"] == a.IP {
				conflict("%s (%s): %s is on %s with a comment that is not ours; left alone", a.Flat, a.Code, a.IP, SuspendedList)
				sn.clash = true
			}
		}
		if wantIn && keep == nil {
			add(provOp{phase: phaseSuspend, kind: "add", path: pathList, code: a.Code,
				body: map[string]string{"list": SuspendedList, "address": a.IP, "comment": w.comment}, desc: fmt.Sprintf("suspend %s (%s)", a.IP, a.Flat)})
		}
		sn.suspended = keep != nil
		snap[a.Code] = sn
	}
	for code, rows := range ourList {
		if byCode[code] != nil || (flagged[code] && !x.flaggedEntitled[code]) {
			continue // a flagged flat that has paid time is un-walled; one that has not stays as it is
		}
		for _, r := range rows {
			add(provOp{phase: phaseUnsuspend, kind: "rm", path: pathList, id: r[routeros.RowID], code: code, desc: "stray suspended entry " + r["address"]})
		}
	}

	// --- the unregistered range is always walled: the script makes the entry; a disabled one is turned on
	for _, r := range x.unreg {
		if r["comment"] == unregComment && (r["disabled"] == "true" || r["disabled"] == "yes") {
			add(provOp{phase: phaseSuspend, kind: "set", path: pathList, id: r[routeros.RowID], body: map[string]string{"disabled": "no"}, desc: "wall the unregistered range"})
		}
	}

	// --- static ARP: with the interface on reply-only, only these (and the
	// DHCP server's own entries) let an address talk, so nobody can squat a
	// flat's address
	if x.ifc != "" {
		ourARP, foreignARP, _ := ours(x.arp)
		for _, w := range wants {
			a := w.a
			if snap[a.Code].clash {
				continue
			}
			want := map[string]string{"address": a.IP, "mac-address": a.MAC, "interface": x.ifc, "comment": w.comment}
			mine := ourARP[a.Code]
			foreignHere := false
			for _, r := range foreignARP {
				if r["address"] == a.IP && !strings.EqualFold(r["mac-address"], a.MAC) && r["dynamic"] != "true" && r["dynamic"] != "yes" {
					conflict("%s (%s): the router has its own static ARP entry for %s (%s) that is not ours; left alone", a.Flat, a.Code, a.IP, r["mac-address"])
					foreignHere = true
				}
			}
			if foreignHere {
				continue
			}
			for i, r := range mine {
				if i > 0 {
					rm(pathARP, r, a.Code, "duplicate ARP row "+r["address"])
				}
			}
			if len(mine) == 0 {
				add(provOp{phase: phaseWrite, kind: "add", path: pathARP, code: a.Code, body: want, desc: fmt.Sprintf("ARP %s %s (%s)", a.IP, a.MAC, a.Flat)})
			} else if diff := rowDiff(mine[0], want); len(diff) > 0 {
				add(provOp{phase: phaseWrite, kind: "set", path: pathARP, id: mine[0][routeros.RowID], code: a.Code, body: diff, desc: fmt.Sprintf("ARP %s %s (%s)", a.IP, a.MAC, a.Flat)})
			}
		}
		for code, rows := range ourARP {
			if byCode[code] != nil || (flagged[code] && !x.blocked[code]) {
				continue
			}
			for _, r := range rows {
				rm(pathARP, r, code, "stray ARP row "+r["address"])
			}
		}
	}
	return out
}

// rowDiff returns the wanted properties that differ from the row.
func rowDiff(r routeros.Row, want map[string]string) map[string]string {
	diff := map[string]string{}
	for k, v := range want {
		cur, has := r[k]
		if has && sameProp(k, cur, v) {
			continue
		}
		if !has && v == "" {
			continue
		}
		diff[k] = v
	}
	return diff
}

func sameProp(key, cur, want string) bool {
	if key == "target" {
		return strings.TrimSuffix(cur, "/32") == strings.TrimSuffix(want, "/32")
	}
	return routeros.SameValue(key, cur, want)
}

// RouterRows reads rows of a building's router (read only; the setup tests and the bench helper use it).
func (s *Service) RouterRows(ctx context.Context, siteID, path string, query map[string]string) ([]routeros.Row, error) {
	return s.router(siteID).Rows(ctx, path, query)
}

func blockedSet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for code := range m {
		out[code] = true
	}
	return out
}

// OrderOnline reports whether a paid order's flat is switched back on at its router: no queued router task for the
// account, no suspended entry, and the building was compared with the router after the payment. The pay page's
// "You're back online" waits for this.
func (s *Service) OrderOnline(o store.Order) bool {
	if o.Status != store.OrderPaid {
		return false
	}
	st := s.AccountRouterState(o.AccountID)
	if st.PendingTask || st.SuspendedOnRouter {
		return false
	}
	return !s.ProvisionStatus(o.SiteID).LastRunAt.Before(o.PaidAt)
}
