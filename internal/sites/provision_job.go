package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/netplan"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The router provisioning job (release plan section 2): one state machine, two
// kinds. A "fresh" router (no live devices) is set up for IPoE directly. A
// "migrate" router has customers on its hotspot: the job arms the kit's rollback
// (marker rows, kit.go), stages everything disabled, switches, and then decides
// on machine evidence alone (amendment A1) whether to keep the switch or to
// make the router roll itself back. There is no manual confirm.
//
//	waiting_router -> checking -> fixing -> needs_action
//	  -> staging -> (fresh)   building -> verifying -> live
//	  -> staging -> (migrate) ready -> arming -> switching -> verifying -> live
//	                                                          verifying -> rolling_back -> rolled_back
//	any -> failed
//
// Every step is an idempotent "ensure" (find by name or ftn: comment, add or set
// the difference), so running the job again on a finished router writes nothing.
// All router writes go through provWriter (prov_writer.go).

// Job states.
const (
	JobWaiting     = "waiting_router"
	JobChecking    = "checking"
	JobFixing      = "fixing"
	JobNeedsAction = "needs_action"
	JobStaging     = "staging"
	JobReady       = "ready"
	JobArming      = "arming"
	JobBuilding    = "building"
	JobSwitching   = "switching"
	JobVerifying   = "verifying"
	JobLive        = "live"
	JobRollingBack = "rolling_back"
	JobRolledBack  = "rolled_back"
	JobFailed      = "failed"
)

// Job kinds.
const (
	KindFresh   = "fresh"
	KindMigrate = "migrate"
)

// Step keys, in order.
const (
	StepPreflight = "preflight"
	StepLeases    = "leases"
	StepBackup    = "backup"
	StepArm       = "arm"
	StepNetwork   = "network"
	StepDHCP      = "dhcp"
	StepFirewall  = "firewall"
	StepSwitch    = "switch"
	StepTelemetry = "telemetry"
	StepVerify    = "verify"
	StepReconcile = "reconcile"
)

var stepTitles = map[string]string{
	StepPreflight: "Check the router", StepLeases: "Wait for old leases", StepBackup: "Back up the router", StepArm: "Arm the safety rollback",
	StepNetwork: "Customer network", StepDHCP: "DHCP, queues and lists", StepFirewall: "Firewall and DNS",
	StepSwitch: "Switch the customers over", StepTelemetry: "Traffic reporting", StepVerify: "Verify",
	StepReconcile: "Sync the customers",
}

var stepOrder = []string{StepPreflight, StepLeases, StepBackup, StepArm, StepNetwork, StepDHCP, StepFirewall, StepSwitch, StepTelemetry, StepVerify, StepReconcile}

// Step statuses.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepOK      = "ok"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// ProvStep is one line of the job's progress.
type ProvStep struct {
	N      int       `json:"n"`
	Key    string    `json:"key"`
	Title  string    `json:"title"`
	Status string    `json:"status"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// ProvEvidence is what the A1 rule looks at.
type ProvEvidence struct {
	TunnelUp      bool     `json:"tunnel_up"`
	InternetOK    bool     `json:"internet_ok"`
	CustomerPath  bool     `json:"customer_path_ok"` // a ping from the customer gateway address got through (forwarding and masquerade)
	DNSOK         bool     `json:"dns_ok"`           // customers can ask the router for DNS
	ConnSeen      bool     `json:"connection_seen"`  // a required flat has a replied connection to the internet
	OLTOK         bool     `json:"olt_ok"`
	Unknown       []string `json:"unknown,omitempty"` // reads that timed out or failed in the last poll: keep polling, not a verdict
	FlatsRequired int      `json:"flats_required"`
	FlatsBound    int      `json:"flats_bound"`
	Missing       []string `json:"missing,omitempty"`
}

// ProvJob is the persisted job (setting site.<id>.prov_job).
type ProvJob struct {
	Kind        string       `json:"kind,omitempty"`
	State       string       `json:"state"`
	Trigger     string       `json:"trigger,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	SwitchedAt  time.Time    `json:"switched_at,omitzero"`
	DeadlineAt  time.Time    `json:"deadline_at,omitzero"`
	Steps       []ProvStep   `json:"steps"`
	LastError   string       `json:"last_error,omitempty"`
	Blockers    []string     `json:"blockers,omitempty"`
	LiveDevices int          `json:"live_devices"`
	KeepUnknown bool         `json:"keep_unknown,omitempty"`
	StartNow    bool         `json:"start_now,omitempty"` // the operator (or the join continuation) started the migration
	Armed       bool         `json:"armed,omitempty"`     // the kit's markers were written: this job must never be resumed
	Online      []string     `json:"online,omitempty"`    // codes of flats seen online, "code@RFC3339"
	Evidence    ProvEvidence `json:"evidence"`
	// LongestLeaseSec is the longest lease time of the old DHCP servers; it sizes the evidence window.
	LongestLeaseSec int `json:"longest_lease_s,omitempty"`
}

func provKey(siteID string) string    { return "site." + siteID + ".prov_job" }
func servingKey(siteID string) string { return "site." + siteID + ".serving_customers" }

// ProvTiming holds the waits of the job; tests shorten them.
type ProvTiming struct {
	Poll         time.Duration // between two reads while waiting
	WaitRouter   time.Duration // how long the job waits for the router to answer
	Backup       time.Duration // the kit's backup file
	Arm          time.Duration // the kit timer's first tick
	Evidence     time.Duration // A1: flats bound within this long of the switch
	OnlineWindow time.Duration // A1: "seen online" means this recently before the switch
	JoinWindow   time.Duration // auto-start of a migration: the join paste is younger than this
	Rollback     time.Duration // how long to wait for the router to roll back
	LeaseShort   string        // lease time given to the old DHCP servers before the switch ("" = 1m)
	LeaseGrace   time.Duration // slack on the lease waits and the evidence window ("" = 1 minute)
	LeaseCap     time.Duration // the longest the job waits for old leases (0 = 65 minutes)
	LeaseSweep   time.Duration // how often lease times are put back when no job is around (0 = 1 minute)
	Countdown    time.Duration // the kit's countdown after the switch (0 = 15 minutes)
}

// DefaultProvTiming is the production timing.
func DefaultProvTiming() ProvTiming {
	return ProvTiming{Poll: 3 * time.Second, WaitRouter: 2 * time.Minute, Backup: 90 * time.Second, Arm: 45 * time.Second,
		Evidence: 5 * time.Minute, OnlineWindow: 10 * time.Minute, JoinWindow: 30 * time.Minute, Rollback: 3 * time.Minute}
}

// Provisioner is what the job needs from the billing service.
type Provisioner interface {
	// HoldSite takes the building's reconcile lock until release is called.
	HoldSite(siteID string) (release func())
	ReconcileSite(ctx context.Context, siteID string) (billing.ProvisionRun, error)
	// SetHotspotBypass turns A1's hotspot bypass rows on or off.
	SetHotspotBypass(siteID string, on bool)
}

type provRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	state  string // state the run is in, for SetupBusy
}

type provMgr struct {
	mu       sync.Mutex
	runs     map[string]*provRun
	p        Provisioner
	announce func(siteID, text, dedupe string)
	tm       ProvTiming
	tmSet    bool
	// lease restore backoff, per building
	leaseNext map[string]time.Time
	leaseTry  map[string]int
}

// SetProvisioner connects the billing service (reconcile lock, reconcile, bypass rows).
func (m *Manager) SetProvisioner(p Provisioner) {
	m.prov.mu.Lock()
	m.prov.p = p
	m.prov.mu.Unlock()
}

// SetAnnouncer sets the function that tells the operators about a job's outcome
// (alerts.Engine.Announce: WhatsApp and email).
func (m *Manager) SetAnnouncer(f func(siteID, text, dedupe string)) {
	m.prov.mu.Lock()
	m.prov.announce = f
	m.prov.mu.Unlock()
}

// SetProvTiming overrides the waits (tests).
func (m *Manager) SetProvTiming(t ProvTiming) {
	m.prov.mu.Lock()
	m.prov.tm, m.prov.tmSet = t, true
	m.prov.mu.Unlock()
}

func (m *Manager) timing() ProvTiming {
	m.prov.mu.Lock()
	defer m.prov.mu.Unlock()
	if !m.prov.tmSet {
		return DefaultProvTiming()
	}
	return m.prov.tm
}

func (m *Manager) provisioner() Provisioner {
	m.prov.mu.Lock()
	defer m.prov.mu.Unlock()
	return m.prov.p
}

func (m *Manager) announce(siteID, text, dedupe string) {
	m.prov.mu.Lock()
	f := m.prov.announce
	m.prov.mu.Unlock()
	if f != nil {
		f(siteID, text, dedupe)
	}
}

// ---- persistence --------------------------------------------------------------------------

// ProvJobOf returns the persisted job of a router.
func (m *Manager) ProvJobOf(siteID string) (ProvJob, bool) {
	var raw string
	m.st.View(func(d *store.Data) { raw = d.Settings[provKey(siteID)].Value })
	if raw == "" {
		return ProvJob{}, false
	}
	var j ProvJob
	if json.Unmarshal([]byte(raw), &j) != nil {
		return ProvJob{}, false
	}
	return j, true
}

func (m *Manager) saveJob(siteID string, j ProvJob) {
	j.UpdatedAt = m.now()
	raw, _ := json.Marshal(j)
	_ = m.st.Update(func(d *store.Data) error {
		k := provKey(siteID)
		d.Settings[k] = store.Setting{Key: k, Value: string(raw), UpdatedAt: j.UpdatedAt}
		return nil
	})
}

// ServingCustomers is the router's intent: it already serves customers.
func (m *Manager) ServingCustomers(siteID string) (on bool) {
	m.st.View(func(d *store.Data) { on = d.Settings[servingKey(siteID)].Value == "yes" })
	return
}

// SetServingCustomers records the intent "already serving customers".
func (m *Manager) SetServingCustomers(siteID string, on bool) {
	v := "no"
	if on {
		v = "yes"
	}
	_ = m.st.Update(func(d *store.Data) error {
		k := servingKey(siteID)
		if d.Settings[k].Value == v {
			return store.ErrNoChange
		}
		d.Settings[k] = store.Setting{Key: k, Value: v, UpdatedAt: m.now()}
		return nil
	})
}

// ---- running ------------------------------------------------------------------------------

// Errors of the job API.
var (
	// ErrJobRunning: a job is already working on this router.
	ErrJobRunning = errors.New("a setup job is already running for this router")
	// ErrNotJoined: the router has not joined yet, there is nothing to set up.
	ErrNotJoined = errors.New("the router has not joined yet: paste the join command on it first")
)

// SetupBusy reports whether a setup job is running for the router (checking,
// fixing, staging, building, arming, switching, verifying or rolling back).
// Reboots and other disruptive actions must be refused while it is true.
func (m *Manager) SetupBusy(siteID string) bool {
	m.prov.mu.Lock()
	defer m.prov.mu.Unlock()
	_, ok := m.prov.runs[siteID]
	return ok
}

// BeginReboot takes the router's run slot for a reboot: while it is held no setup job can start (StartProvision
// and RollbackNow see a running job), and it fails when a job is already running. Call end when the reboot call is over.
func (m *Manager) BeginReboot(siteID string) (end func(), ok bool) {
	m.prov.mu.Lock()
	defer m.prov.mu.Unlock()
	if m.prov.runs == nil {
		m.prov.runs = map[string]*provRun{}
	}
	if m.prov.runs[siteID] != nil {
		return nil, false
	}
	run := &provRun{cancel: func() {}, done: make(chan struct{}), state: "rebooting"}
	m.prov.runs[siteID] = run
	var once sync.Once
	return func() {
		once.Do(func() {
			m.prov.mu.Lock()
			if m.prov.runs[siteID] == run {
				delete(m.prov.runs, siteID)
			}
			m.prov.mu.Unlock()
			close(run.done)
		})
	}, true
}

// ProvOptions says how a job starts.
type ProvOptions struct {
	Trigger     string // join | run | migrate | restart
	StartNow    bool   // a migration may start without waiting for the button
	KeepUnknown bool
	Actor       string
}

// StartProvision starts the job for a router in the background.
func (m *Manager) StartProvision(siteID string, o ProvOptions) error {
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return billing.ErrNoSite
	}
	if site.Mode != store.SiteModeWG || site.WGPublicKey == "" || site.Disabled {
		return ErrNotJoined
	}
	m.prov.mu.Lock()
	if m.prov.runs == nil {
		m.prov.runs = map[string]*provRun{}
	}
	if _, busy := m.prov.runs[siteID]; busy {
		m.prov.mu.Unlock()
		return ErrJobRunning
	}
	base := m.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	run := &provRun{cancel: cancel, done: make(chan struct{})}
	m.prov.runs[siteID] = run
	m.prov.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.prov.mu.Lock()
			delete(m.prov.runs, siteID)
			m.prov.mu.Unlock()
			close(run.done)
			cancel()
		}()
		m.runProvision(ctx, siteID, o)
	}()
	return nil
}

// abortProvision stops a running job (it writes nothing more) and waits for it.
func (m *Manager) abortProvision(siteID string) {
	m.prov.mu.Lock()
	run := m.prov.runs[siteID]
	m.prov.mu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}
}

// WaitProvision blocks until no job runs for the router (tests, shutdown).
func (m *Manager) WaitProvision(siteID string) {
	for m.SetupBusy(siteID) {
		time.Sleep(5 * time.Millisecond)
	}
}

// jobCtl is one run of the pipeline.
type jobCtl struct {
	m      *Manager
	ctx    context.Context
	siteID string
	o      ProvOptions
	site   store.Site // effective
	plan   netplan.Plan
	rt     routeros.Client
	insp   routeros.Inspector
	w      *provWriter
	job    ProvJob
	tm     ProvTiming
	// release drops the reconcile lock (idempotent).
	release func()
	cust    custInfo
	checks  Checks
	// flats staged for a migration
	flats []flatRow
	// armDeadline is real time: when the kit's countdown after the switch runs out
	armDeadline time.Time
	// hold-related
	hold bool
}

type flatRow struct {
	Code, Flat, MAC, IP, Limit string
	Suspended                  bool
	Entitled                   bool
}

// failure ends a run with a state and a message.
type failure struct {
	state string
	msg   string
	// unreachable: the router could not be reached, so nothing more can be written to it.
	unreachable bool
}

func (f *failure) Error() string { return f.msg }

func (j *jobCtl) save() { j.m.saveJob(j.siteID, j.job) }

func (j *jobCtl) setState(s string) {
	j.job.State = s
	j.m.prov.mu.Lock()
	if r := j.m.prov.runs[j.siteID]; r != nil {
		r.state = s
	}
	j.m.prov.mu.Unlock()
	j.save()
}

func (j *jobCtl) log(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	_ = j.m.st.Update(func(d *store.Data) error {
		d.AddEvent(store.Event{Time: j.m.now(), SiteID: j.siteID, Kind: "audit", Msg: "router setup: " + msg})
		return nil
	})
}

func (j *jobCtl) step(key, status, detail string) {
	for i := range j.job.Steps {
		if j.job.Steps[i].Key == key {
			j.job.Steps[i].Status, j.job.Steps[i].Detail, j.job.Steps[i].At = status, detail, j.m.now()
			j.save()
			return
		}
	}
}

func newSteps(kind string) []ProvStep {
	var out []ProvStep
	for i, k := range stepOrder {
		out = append(out, ProvStep{N: i + 1, Key: k, Title: stepTitles[k], Status: StepPending})
	}
	return out
}

// runProvision is the whole job.
func (m *Manager) runProvision(ctx context.Context, siteID string, o ProvOptions) {
	var site store.Site
	m.st.View(func(d *store.Data) { site = d.Sites[siteID] })
	j := &jobCtl{m: m, ctx: ctx, siteID: siteID, o: o, tm: m.timing(), rt: m.Router(siteID)}
	j.site = m.Effective(site)
	j.insp, _ = j.rt.(routeros.Inspector)
	j.release = func() {}
	j.job = ProvJob{State: JobWaiting, Trigger: o.Trigger, StartedAt: m.now(), Steps: newSteps(""), KeepUnknown: o.KeepUnknown, StartNow: o.StartNow}
	j.save()
	defer func() { j.release() }()
	err := j.pipeline()
	if err == nil || errors.Is(err, errParked) {
		return
	}
	j.finishFailed(err)
}

// errParked ends a run on purpose (needs_action, ready, waiting_router, rolled_back, live).
var errParked = errors.New("parked")

func (j *jobCtl) park(state, msg string) error {
	j.job.LastError = msg
	j.setState(state)
	return errParked
}

func fail(state, format string, a ...any) *failure {
	return &failure{state: state, msg: fmt.Sprintf(format, a...)}
}

// routerErr classifies an error of a router call: a router that answered with an error is
// "refused" (it can still be told to roll back), anything else is "unreachable".
func routerErr(err error, what string) *failure {
	var ae *routeros.APIError
	if errors.As(err, &ae) {
		msg := ae.Message
		if ae.Detail != "" {
			msg += " (" + ae.Detail + ")"
		}
		return &failure{state: JobFailed, msg: fmt.Sprintf("%s: the router refused: %s", what, msg)}
	}
	if errors.Is(err, ErrProvNotAllowed) {
		return &failure{state: JobFailed, msg: fmt.Sprintf("%s: %v", what, err)}
	}
	return &failure{state: JobFailed, msg: fmt.Sprintf("%s: the router does not answer: %v", what, shortErr(err)), unreachable: true}
}

// finishFailed ends a run that failed. If the kit is armed, the router is told
// to roll back (the one write allowed after a failure) unless it cannot be reached.
func (j *jobCtl) finishFailed(err error) {
	f, ok := err.(*failure)
	if !ok {
		if errors.Is(err, context.Canceled) {
			j.job.LastError = "stopped"
			j.setState(JobFailed)
			return
		}
		f = &failure{state: JobFailed, msg: err.Error()}
	}
	if j.ctx.Err() != nil { // aborted: the one who aborted decides what happens next
		j.job.LastError = "stopped"
		j.setState(JobFailed)
		return
	}
	j.job.LastError = f.msg
	j.log("%s", f.msg)
	if j.job.Armed && !f.unreachable {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if e := j.setNow(c); errors.Is(e, ErrNothingToRollBack) {
			j.job.LastError += " (nothing had been changed on the router)"
			j.setState(JobFailed)
			j.m.announce(j.siteID, "Setting up router "+j.site.Name+" stopped: "+j.job.LastError, j.dedupe("failed"))
			return
		} else if e != nil {
			j.job.LastError += " (and the rollback marker could not be set: " + shortErr(e) + ")"
			j.setState(JobFailed)
			j.m.announce(j.siteID, "Setting up router "+j.site.Name+" failed and the router could not be told to roll back: "+f.msg+". It rolls back by itself within 15 minutes.", j.dedupe("failed"))
			return
		}
		j.job.State = JobRollingBack
		j.save()
		j.rollbackTail("setup failed: " + f.msg)
		return
	}
	if j.job.Armed && f.unreachable {
		j.job.LastError += ". The router could not be reached; its own 15 minute timer rolls it back."
	}
	if !j.job.Armed && !f.unreachable {
		rc, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		j.restoreLeases(rc)
		rcancel()
	}
	st := f.state
	if st == "" {
		st = JobFailed
	}
	if st == JobFailed || st == JobNeedsAction {
		for i := range j.job.Steps {
			if j.job.Steps[i].Status == StepRunning {
				j.job.Steps[i].Status, j.job.Steps[i].Detail = StepFailed, f.msg
			}
		}
	}
	j.setState(st)
	if st == JobFailed {
		j.m.announce(j.siteID, "Setting up router "+j.site.Name+" stopped: "+j.job.LastError, j.dedupe("failed"))
	}
}

func (j *jobCtl) dedupe(what string) string {
	return fmt.Sprintf("prov:%s:%s:%d", j.siteID, what, j.job.StartedAt.Unix())
}

// ---- the pipeline -------------------------------------------------------------------------

func (j *jobCtl) pipeline() error {
	m := j.m
	// waiting for the router
	if err := j.waitRouter(); err != nil {
		return err
	}
	// checking and fixing
	j.setState(JobChecking)
	j.step(StepPreflight, StepRunning, "reading the router")
	c, err := m.Checks(j.ctx, j.siteID, true)
	if err != nil {
		return fail(JobFailed, "reading the router: %v", err)
	}
	if !c.Router.Reachable {
		return j.park(JobWaiting, "the router does not answer: check the tunnel")
	}
	j.setState(JobFixing)
	if outs, err := m.ApplyAutoFixes(j.ctx, j.siteID); err == nil {
		for _, o := range outs {
			if len(o.Applied) > 0 {
				j.log("fix %s: %s", o.ID, strings.Join(o.Applied, "; "))
			}
		}
	}
	if c, err = m.Checks(j.ctx, j.siteID, true); err != nil || !c.Router.Reachable {
		return j.park(JobWaiting, "the router does not answer: check the tunnel")
	}
	j.checks = c
	j.job.Evidence = ProvEvidence{}
	plan, perr := PlanSubnet(j.site.CustomerSubnet)
	if perr != nil {
		return j.park(JobNeedsAction, perr.Error())
	}
	j.plan = plan

	// classification
	rows, cerr := j.classify()
	if cerr != nil {
		return routerErr(cerr, "reading the customer devices")
	}
	j.job.LiveDevices = rows.live
	kind := KindFresh
	switch {
	case rows.serving: // ftn-dhcp already serves: this is a rerun, every step is a no-op
	case j.o.StartNow && j.o.Trigger == "migrate":
		kind = KindMigrate
	case rows.live > 0 && !m.ServingCustomers(j.siteID):
		j.job.Kind = KindMigrate
		return j.park(JobNeedsAction, fmt.Sprintf("%d live devices found on the router: run it as a migration", rows.live))
	case m.ServingCustomers(j.siteID):
		kind = KindMigrate // intent "serving": always the careful path, even when nobody is online right now
	}
	j.job.Kind = kind

	// blockers
	var blockers []string
	if kind == KindMigrate {
		blockers = c.Blockers(j.o.KeepUnknown, false)
	} else {
		blockers = freshBlockers(c)
	}
	j.job.Blockers = blockers
	if len(blockers) > 0 {
		j.step(StepPreflight, StepFailed, "needs attention: "+strings.Join(blockers, ", "))
		return j.park(JobNeedsAction, "these checks need attention first: "+strings.Join(blockers, ", "))
	}
	if kind == KindMigrate {
		if bad := j.conflictedPaying(); len(bad) > 0 {
			j.step(StepPreflight, StepFailed, "paying flats with a MAC conflict: "+strings.Join(bad, ", "))
			return j.park(JobNeedsAction, "these paying flats have a MAC address conflict and would be left without internet by the switch. Fix their MACs first: "+strings.Join(bad, ", "))
		}
	}
	j.job.LastError = ""
	j.job.Blockers = nil
	j.setState(JobStaging)
	if err := j.ensureAddresses(); err != nil {
		return err
	}
	if kind == KindMigrate {
		if err := j.noteOnline(); err != nil {
			return err
		}
	}
	j.step(StepPreflight, StepOK, fmt.Sprintf("%s: %d live device(s), checks green", kind, rows.live))

	if kind == KindMigrate {
		start := j.o.StartNow
		if !start && j.o.Trigger == "join" && m.autoStartWindow(j.siteID) {
			start = true
		}
		if !start {
			j.job.StartNow = false
			return j.park(JobReady, "")
		}
		j.job.StartNow = true
	}
	return j.build(kind, rows)
}

// freshBlockers are the failing checks that stop a fresh set-up. The kit, the
// scheduler, DNS, the customer interface and the customer-DNS rule are made by
// the job itself, and nothing is online, so the flat-related guards do not apply.
func freshBlockers(c Checks) []string {
	skip := map[string]bool{CheckKit: true, CheckDMScheduler: true, CheckDNS: true, CheckCustDNS: true, CheckCustomerIf: true,
		CheckUnmanaged: true, CheckUnseen: true}
	var out []string
	for _, id := range c.Blockers(false, false) {
		if !skip[id] {
			out = append(out, id)
		}
	}
	return out
}

// autoStartWindow: the join paste is younger than 30 minutes (release plan A1).
func (m *Manager) autoStartWindow(siteID string) bool {
	var used time.Time
	m.st.View(func(d *store.Data) { used = d.Sites[siteID].Join.UsedAt })
	return !used.IsZero() && m.now().Sub(used) <= m.timing().JoinWindow
}

func (j *jobCtl) waitRouter() error {
	deadline := time.Now().Add(j.tm.WaitRouter)
	for {
		pc, cancel := context.WithTimeout(j.ctx, 5*time.Second)
		err := j.rt.Ping(pc)
		cancel()
		if err == nil {
			return nil
		}
		j.job.LastError = "waiting for the router: " + shortErr(err)
		j.setState(JobWaiting)
		if j.ctx.Err() != nil {
			return j.ctx.Err()
		}
		if time.Now().After(deadline) {
			return j.park(JobWaiting, "the router does not answer: check the tunnel and the join command")
		}
		select {
		case <-j.ctx.Done():
			return j.ctx.Err()
		case <-time.After(j.tm.Poll):
		}
	}
}

// ensureAddresses gives every flat its fixed address (the script and the reconciler agree on it).
func (j *jobCtl) ensureAddresses() error {
	err := j.m.st.Update(func(d *store.Data) error {
		_, err := billing.EnsureIPsLocked(d, j.siteID, j.plan, j.m.foreignIPsOf(j.siteID))
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNoChange) {
		return fail(JobFailed, "giving the flats their addresses: %v", err)
	}
	return nil
}

// ---- classification -----------------------------------------------------------------------

type classification struct {
	live    int
	serving bool // ftn-dhcp is enabled on the router already
	cust    custInfo
}

var classifyTables = []string{"/ip/address", "/ip/hotspot", "/ip/dhcp-server", "/interface/bridge", "/ip/dhcp-server/lease", "/ip/arp", "/interface/bridge/host"}

func boundLease(r routeros.Row) bool {
	return r["status"] == "bound"
}

func arpOnlineRow(r routeros.Row) bool {
	switch r["status"] {
	case "":
		return r["complete"] != "false" && r["invalid"] != "true" && r["disabled"] != "true"
	case "reachable", "delay", "probe", "permanent":
		return true
	}
	return false
}

// classify counts the live devices: hotspot sessions, bound leases of the old
// DHCP servers on the customer interface, and complete ARP entries in the
// customer network, never counting what was learned on the bench port.
func (j *jobCtl) classify() (classification, error) {
	var out classification
	sn := readSnapshot(j.ctx, j.rt, nil, classifyTables)
	for _, p := range []string{"/ip/address", "/ip/dhcp-server"} {
		if err := sn.errs[p]; err != nil {
			return out, err
		}
	}
	out.cust = detectCustomer(sn, j.plan)
	j.cust = out.cust
	for _, r := range sn.tbl("/ip/dhcp-server") {
		if r["name"] == billing.DHCPServerName && !rowDisabled(r) {
			out.serving = true
		}
	}
	bench := map[string]bool{}
	if j.site.BenchPort != "" {
		for _, r := range sn.tbl("/interface/bridge/host") {
			if r["on-interface"] == j.site.BenchPort {
				if mac, ok := store.CanonMAC(r["mac-address"]); ok {
					bench[mac] = true
				}
			}
		}
	}
	macs := map[string]bool{}
	add := func(raw string) {
		if mac, ok := store.CanonMAC(raw); ok && !bench[mac] {
			macs[mac] = true
		}
	}
	if sess, err := j.rt.ActiveSessions(j.ctx); err == nil {
		for _, s := range sess {
			add(s.MAC)
		}
	}
	if !out.serving && out.cust.found {
		oldServers := map[string]bool{}
		for _, r := range sn.tbl("/ip/dhcp-server") {
			if r["interface"] == out.cust.iface && r["name"] != billing.DHCPServerName {
				oldServers[r["name"]] = true
			}
		}
		for _, r := range sn.tbl("/ip/dhcp-server/lease") {
			if boundLease(r) && r["server"] != billing.DHCPServerName && (r["server"] == "" || oldServers[r["server"]]) {
				add(r["mac-address"])
			}
		}
		for _, r := range sn.tbl("/ip/arp") {
			if !strings.HasPrefix(r["comment"], ftnPrefix) && arpOnlineRow(r) && j.plan.InNetwork(r["address"]) && r["address"] != j.plan.Gateway {
				add(r["mac-address"])
			}
		}
	}
	out.live = len(macs)
	return out, nil
}

// conflictedPaying lists the flats that have paid time but whose MAC is flagged: the switch does not provision them.
func (j *jobCtl) conflictedPaying() []string {
	now := j.m.now()
	var out []string
	j.m.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == j.siteID && a.MACConflict && billing.AccountState(a, now) == billing.StateActive {
				out = append(out, a.Flat)
			}
		}
	})
	sort.Strings(out)
	return out
}

// flatsOf builds the staged rows of the building's flats.
func (j *jobCtl) flatsOf() []flatRow {
	now := j.m.now()
	var out []flatRow
	j.m.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID != j.siteID || a.MACConflict || a.MAC == "" || a.IP == "" || !j.plan.InStatic(a.IP) {
				continue
			}
			ent := billing.AccountState(a, now) == billing.StateActive
			out = append(out, flatRow{Code: a.Code, Flat: a.Flat, MAC: a.MAC, IP: a.IP, Limit: billing.QueueLimit(d.Packages[a.PackageID]),
				Suspended: !ent, Entitled: ent})
		}
	})
	sort.Slice(out, func(a, b int) bool { return out[a].Code < out[b].Code })
	return out
}

// onlineMACs reads who is on the router now (any of: hotspot host or session,
// bound lease, reachable ARP entry in the customer network).
func (j *jobCtl) onlineMACs(ctx context.Context) (map[string]bool, error) {
	macs := map[string]bool{}
	add := func(raw string) {
		if mac, ok := store.CanonMAC(raw); ok {
			macs[mac] = true
		}
	}
	var firstErr error
	got := 0
	if sess, err := j.rt.ActiveSessions(ctx); err == nil {
		got++
		for _, s := range sess {
			add(s.MAC)
		}
	} else {
		firstErr = err
	}
	if hosts, err := j.rt.Hosts(ctx); err == nil {
		got++
		for _, h := range hosts {
			if h.Authorized || h.Bypassed {
				add(h.MAC)
			}
		}
	}
	if rows, err := j.rt.Rows(ctx, "/ip/dhcp-server/lease", nil); err == nil {
		got++
		for _, r := range rows {
			if boundLease(r) && r["server"] != billing.DHCPServerName {
				add(r["mac-address"])
			}
		}
	} else if firstErr == nil {
		firstErr = err
	}
	if rows, err := j.rt.Rows(ctx, "/ip/arp", nil); err == nil {
		got++
		for _, r := range rows {
			if !strings.HasPrefix(r["comment"], ftnPrefix) && arpOnlineRow(r) && j.plan.InNetwork(r["address"]) {
				add(r["mac-address"])
			}
		}
	}
	if got == 0 {
		return nil, firstErr
	}
	return macs, nil
}

// noteOnline records which entitled flats are online right now (A1: the ones seen
// in the 10 minutes before the switch must come back bound).
func (j *jobCtl) noteOnline() error {
	macs, err := j.onlineMACs(j.ctx)
	if err != nil {
		return routerErr(err, "reading who is online")
	}
	now := j.m.now()
	seen := map[string]time.Time{}
	for _, e := range j.job.Online {
		if code, ts, ok := strings.Cut(e, "@"); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				seen[code] = t
			}
		}
	}
	for _, f := range j.flatsOf() {
		if mac, ok := store.CanonMAC(f.MAC); ok && f.Entitled && macs[mac] {
			seen[f.Code] = now
		}
	}
	j.job.Online = j.job.Online[:0]
	for code, t := range seen {
		j.job.Online = append(j.job.Online, code+"@"+t.UTC().Format(time.RFC3339))
	}
	sort.Strings(j.job.Online)
	j.save()
	return nil
}

// requiredFlats are the entitled flats that were seen online in the 10 minutes before at.
func (j *jobCtl) requiredFlats(at time.Time) []flatRow {
	seen := map[string]time.Time{}
	for _, e := range j.job.Online {
		if code, ts, ok := strings.Cut(e, "@"); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				seen[code] = t
			}
		}
	}
	var out []flatRow
	for _, f := range j.flats {
		if t, ok := seen[f.Code]; ok && f.Entitled && at.Sub(t) <= j.tm.OnlineWindow {
			out = append(out, f)
		}
	}
	return out
}

// ---- helpers on the router ----------------------------------------------------------------

func (j *jobCtl) rows(path string) ([]routeros.Row, error) {
	return j.rt.Rows(j.ctx, path, nil)
}

func yn(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// diffRow returns the wanted properties that differ from the row (booleans compare by meaning).
func diffRow(r routeros.Row, want map[string]string) map[string]string {
	diff := map[string]string{}
	for k, v := range want {
		if k == "place-before" {
			continue
		}
		cur, has := r[k]
		if has && sameProvValue(k, cur, v) {
			continue
		}
		if !has && v == "" {
			continue
		}
		diff[k] = v
	}
	return diff
}

func sameProvValue(k, cur, want string) bool {
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

// ensure finds the row for which match is true and sets the difference, or adds
// it. It returns the row id and whether it wrote.
func (j *jobCtl) ensure(path string, rows []routeros.Row, match func(routeros.Row) bool, want map[string]string) (string, bool, error) {
	for _, r := range rows {
		if match(r) {
			if diff := diffRow(r, want); len(diff) > 0 {
				if err := j.w.set(j.ctx, path, r, diff); err != nil {
					return "", false, routerErr(err, path)
				}
				return r[routeros.RowID], true, nil
			}
			return r[routeros.RowID], false, nil
		}
	}
	id, err := j.w.addID(j.ctx, path, want)
	if err != nil {
		return "", false, routerErr(err, path)
	}
	return id, true, nil
}

func byComment(c string) func(routeros.Row) bool {
	return func(r routeros.Row) bool { return r["comment"] == c }
}

func byName(n string) func(routeros.Row) bool {
	return func(r routeros.Row) bool { return r["name"] == n }
}

func (j *jobCtl) newWriter() {
	sc := provScope{iface: j.cust.iface, gateway: j.plan.GatewayIP, network: j.plan.Network, wanList: j.site.WANList, ports: map[string]bool{}}
	sc.bridge = HotspotBridge
	if j.site.VLANID > 0 {
		sc.vlan = fmt.Sprintf("vlan%d-customers", j.site.VLANID)
		sc.ports[sc.vlan] = true
	} else {
		sc.ports[j.site.OLTInterface] = true
	}
	if j.site.BenchPort != "" {
		sc.ports[j.site.BenchPort] = true
	}
	j.w = &provWriter{rt: j.rt, insp: j.insp, sc: sc, audit: func(s string) { j.log("%s", s) }}
}

func (j *jobCtl) setCustomer(iface string) {
	j.cust.iface = iface
	j.cust.found = true
	j.w.sc.iface = iface
}

// ProvJobs is the view of the job manager that other parts (the reboot guard,
// the live view) use. It satisfies sites.ProvisionJobs where that is declared.
type ProvJobs struct{ m *Manager }

// ProvJobsView returns the job view of the manager (the default ProvisionJobs hook).
func (m *Manager) ProvJobsView() *ProvJobs { return &ProvJobs{m} }

// Running reports whether a setup job is working on the router (reboot refused while true).
func (p *ProvJobs) Running(siteID string) bool { return p.m.SetupBusy(siteID) }

// State is the job's state name ("live", "needs_action", ...; "" when none ran).
func (p *ProvJobs) State(siteID string) string {
	j, ok := p.m.ProvJobOf(siteID)
	if !ok {
		return ""
	}
	return j.State
}
