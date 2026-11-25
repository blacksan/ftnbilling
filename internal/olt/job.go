package olt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ispbill/internal/store"
)

// Three OLT actions take far longer than one HTTP request may last: "Secure this OLT" (backup, new passwords, a second
// session to test them, saves, SNMP, the SSH access list), "Apply" of a reviewed plan (backup, the lines, a save of up to
// Dialect.SaveTimeout, a read back) and "Check now" (which may secure a factory OLT). Each runs in the background,
// detached from the request that started it, through one mechanism: at most ONE job of any kind per OLT (the slot), a
// stage text for the page, and a record of the outcome that feeds the progress display. The session lock still
// serialises a job with every other OLT operation. The results that matter are stored as before (OLTAdopt notes, audit,
// alerts, backups); the job record here only feeds the display and holds no secret.

// Job budgets. A job's context bounds the waiting and the reading; once a configuration change has begun it runs to its
// end (or its undo) on its own detached budget (applyWriteBudget, aclStepBudget, aclRestoreBudget).
const (
	// secureJobTimeout: about twenty commands, two sessions and two saves of up to Dialect.SaveTimeout each, with room.
	secureJobTimeout = 15 * time.Minute
	// applyJobTimeout: the backup, the wait for the session lock, and the budget below.
	applyJobTimeout = 15 * time.Minute
	// applyWriteBudget: the configuration lines, the 120 s save and the read back.
	applyWriteBudget = 10 * time.Minute
	// checkJobTimeout: a check may end in the secure of a factory OLT.
	checkJobTimeout = secureJobTimeout
)

// JobKind is what a background job does.
type JobKind string

const (
	JobSecure JobKind = "secure"
	JobApply  JobKind = "apply"
	JobCheck  JobKind = "check"
)

// Label is the word the page shows while the job runs.
func (k JobKind) Label() string {
	switch k {
	case JobSecure:
		return "Securing"
	case JobApply:
		return "Applying"
	case JobCheck:
		return "Checking"
	}
	return "Working"
}

// ErrJobRunning: a background job (of any kind) is already in progress for this OLT. The error returned says which.
var ErrJobRunning = errors.New("a job is already running for this OLT")

// ErrSecureRunning is the old name of ErrJobRunning.
var ErrSecureRunning = ErrJobRunning

type runningErr struct {
	kind JobKind
	pass bool // the server's own adoption pass holds the slot (no job record, no progress on the page)
}

func (e runningErr) Error() string {
	if e.pass {
		return "The server's own check of the OLT management port is working on this OLT; try again in a few minutes"
	}
	return e.kind.Label() + " is already running for this OLT"
}
func (e runningErr) Is(t error) bool { return t == ErrJobRunning }

// ByPass: the refusal err comes from the server's own adoption pass, which the page shows no progress of.
func ByPass(err error) bool {
	var r runningErr
	return errors.As(err, &r) && r.pass
}

// RunningKind says which job blocked a start (ok false when err is not a "running" refusal).
func RunningKind(err error) (JobKind, bool) {
	var r runningErr
	if errors.As(err, &r) {
		return r.kind, true
	}
	return "", false
}

type job struct {
	kind     JobKind
	by       string
	started  time.Time
	finished time.Time
	stage    string
	trail    []string // every stage the job went through, in order (for tests and the log)
	running  bool
	err      string
	sec      SecureResult
	app      Result
}

// jobOut is what a job's work returns besides its error.
type jobOut struct {
	sec SecureResult
	app Result
}

// JobStatus is the progress or the outcome of the latest background job of an OLT. It holds no secret.
type JobStatus struct {
	Known    bool // a job ran (or runs) in this server process
	Running  bool
	Kind     JobKind
	Started  time.Time
	Finished time.Time
	By       string
	Stage    string
	OK       bool   // finished without an error
	Error    string // why it failed
	Secure   SecureResult
	Apply    Result // Sent and Remaining are redacted; the transcript is not kept here
	// Interrupted: no job runs, but one did not finish: InterruptedKind says which. A Secure leaves a sealed new login pair
	// (the server tries it at its next check of the OLT); an Apply leaves a mark (the plan is to be read again, nothing is
	// applied twice because the plan is built from what the OLT has now).
	Interrupted     bool
	InterruptedKind JobKind
}

// claim takes the OLT's one job slot, or says which job holds it.
func (s *Service) claim(id, by string, kind JobKind) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.busyLocked(id); err != nil {
		return nil, err
	}
	j := &job{kind: kind, by: by, started: s.now(), stage: "Starting", trail: []string{"Starting"}, running: true}
	if s.jobs == nil {
		s.jobs = map[string]*job{}
	}
	s.jobs[id] = j
	return j, nil
}

// busy: a job (or the timer's adoption pass) holds the OLT's slot now.
func (s *Service) busy(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyLocked(id)
}

func (s *Service) busyLocked(id string) error {
	if j := s.jobs[id]; j != nil && j.running {
		return runningErr{kind: j.kind}
	}
	if s.held[id] {
		return runningErr{kind: JobCheck, pass: true}
	}
	return nil
}

// hold takes the OLT's slot for an adoption pass that is not itself a job (the 5-minute timer, or the pass of a Check job
// for the OLTs of the same router other than its own): false when a job holds it, and the pass leaves that OLT alone.
// The pass has no job record, so it never touches the page's stage text, and while it holds the slot no Secure, Apply or
// Check can start: two logins changes can never interleave. release gives it back.
func (s *Service) hold(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busyLocked(id) != nil {
		return false
	}
	if s.held == nil {
		s.held = map[string]bool{}
	}
	s.held[id] = true
	return true
}

func (s *Service) release(id string) {
	s.mu.Lock()
	delete(s.held, id)
	s.mu.Unlock()
}

// launch runs work in the background under its own budget and records the outcome. The slot j was claimed by the caller.
func (s *Service) launch(id string, j *job, budget time.Duration, work func(ctx context.Context) (jobOut, error)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		var out jobOut
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%s stopped unexpectedly: %v", j.kind.Label(), r)
				}
			}()
			out, err = work(ctx)
		}()
		if j.kind == JobApply {
			s.clearJobMark(id, false) // before the job is reported finished, so the page never shows both
		}
		s.recordLast(id, j, out, err)
		s.mu.Lock()
		j.running, j.finished, j.sec, j.app = false, s.now(), out.sec, out.app
		if err != nil {
			j.err = errText(err)
			if errors.Is(err, context.DeadlineExceeded) {
				j.err = j.kind.Label() + " did not finish within " + budget.String() + " (" + j.stage + "); the OLT took too long to answer"
			}
			j.stage = "Failed"
		} else {
			j.stage = "Done"
		}
		s.mu.Unlock()
	}()
}

// recordLast stores the outcome of a finished job on the OLT's state (redacted, no secret), so that it is shown until the
// next job ends, whatever happened to the request that started it.
func (s *Service) recordLast(id string, j *job, out jobOut, err error) {
	l := store.OLTLastJob{Kind: string(j.kind), By: j.by, Started: j.started, Finished: s.now(), OK: err == nil,
		Sent: len(out.app.Sent), Verified: out.app.Verified, Remaining: len(out.app.Remaining), Backup: out.app.Backup,
		Failed: out.app.Failed, Skipped: out.app.Skipped}
	if err != nil {
		l.Error = errText(err)
		if errors.Is(err, context.DeadlineExceeded) {
			l.Error = j.kind.Label() + " did not finish within its time; the OLT took too long to answer"
		}
	}
	for _, en := range out.app.Entries {
		if en.Err != "" {
			l.Refused, l.RefusedOut = en.Cmd, clip(Redact(firstNonEmpty(en.Out, en.Err)), 400)
			break
		}
	}
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) { st.Settings.Last = l })
}

// ---- Secure ----------------------------------------------------------------------------------------------------------------

// StartSecure checks what can be checked at once and then runs Secure in the background. It returns at once.
func (s *Service) StartSecure(id, by string) error {
	_, st, ok := s.Load(id)
	if !ok {
		return ErrNotFound
	}
	if st.Settings.Adopt.SerialOK != "ok" {
		return ErrNotIdentified
	}
	if s.o.Box == nil {
		return ErrNoKey
	}
	j, err := s.claim(id, by, JobSecure)
	if err != nil {
		return err
	}
	s.launch(id, j, secureJobTimeout, func(ctx context.Context) (jobOut, error) {
		res, err := s.Secure(ctx, id, by)
		return jobOut{sec: res}, err
	})
	return nil
}

// ---- Apply -----------------------------------------------------------------------------------------------------------------

// StartApply runs a reviewed plan in the background. What can be decided at once is decided here, in the caller's
// request, BEFORE anything starts: provisioning on, no other job running, and the plan the OLT has now is the plan the
// owner reviewed (its hash). The caller has checked the owner's password. The job then rebuilds the plan inside its own
// session and compares the hash again, so nothing that changed in between is sent.
func (s *Service) StartApply(ctx context.Context, id, wantHash, by string) error {
	_, st, ok := s.Load(id)
	if !ok {
		return ErrNotFound
	}
	if err := s.mayConfigure(st); err != nil {
		return err
	}
	if wantHash == "" {
		return ErrPlanChanged
	}
	if err := s.busy(id); err != nil {
		return err
	}
	_, hash, err := s.Plan(ctx, id) // reads the OLT; also forgets the mark of an earlier interrupted apply
	if err != nil {
		return err
	}
	if hash != wantHash {
		return ErrPlanChanged
	}
	j, err := s.claim(id, by, JobApply)
	if err != nil {
		return err
	}
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Settings.Job = store.OLTJobMark{Kind: string(JobApply), By: by, At: s.now(), Plan: hash}
	})
	s.launch(id, j, applyJobTimeout, func(ctx context.Context) (jobOut, error) {
		res, err := s.apply(ctx, id, wantHash, by, func(text string) { s.stage(id, text) })
		return jobOut{app: res}, err
	})
	return nil
}

// StartRemove runs a reviewed ONU removal plan in the background, as an Apply (same slot, same durable mark, same checks:
// provisioning on, no job running, the removal plan the OLT has now is the one the owner reviewed). The job builds the
// removal plan again inside its own session and compares the hash again before it sends anything.
func (s *Service) StartRemove(ctx context.Context, id, serial, wantHash, by string) error {
	_, st, ok := s.Load(id)
	if !ok {
		return ErrNotFound
	}
	if err := s.mayConfigure(st); err != nil {
		return err
	}
	if wantHash == "" {
		return ErrPlanChanged
	}
	if err := s.busy(id); err != nil {
		return err
	}
	_, hash, err := s.PlanRemove(ctx, id, serial)
	if err != nil {
		return err
	}
	if hash != wantHash {
		return ErrPlanChanged
	}
	j, err := s.claim(id, by, JobApply)
	if err != nil {
		return err
	}
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Settings.Job = store.OLTJobMark{Kind: string(JobApply), By: by, At: s.now(), Plan: hash}
	})
	s.launch(id, j, applyJobTimeout, func(ctx context.Context) (jobOut, error) {
		res, err := s.applyRemove(ctx, id, serial, wantHash, by, func(text string) { s.stage(id, text) })
		return jobOut{app: res}, err
	})
	return nil
}

// clearJobMark forgets the durable mark of an apply (it ended, or the plan was read again). With idle, only when no job
// holds the slot, decided inside the store update: a job claims the slot before it writes its mark, so a mark written by a
// job that started meanwhile is never cleared here.
func (s *Service) clearJobMark(id string, idle bool) {
	if _, st, ok := s.Load(id); !ok || st.Settings.Job.Kind == "" {
		return
	}
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		if !idle || s.busy(id) == nil {
			st.Settings.Job = store.OLTJobMark{}
		}
	})
}

// ---- Check now -------------------------------------------------------------------------------------------------------------

// StartCheck is "Check now": the pass over the router's OLT management port, which adopts the OLT and may secure a
// factory one (minutes). What is known at once (no router, no port, adoption off) is answered here.
func (s *Service) StartCheck(id, by string) error {
	if err := s.busy(id); err != nil {
		return err
	}
	siteID, err := s.checkSite(id)
	if err != nil {
		return err
	}
	if s.o.PortSeen == nil {
		return ErrAdoptOff
	}
	j, err := s.claim(id, by, JobCheck)
	if err != nil {
		return err
	}
	s.launch(id, j, checkJobTimeout, func(ctx context.Context) (jobOut, error) {
		return jobOut{}, s.adoptSite(ctx, siteID, by, id)
	})
	return nil
}

// ---- stage and status ------------------------------------------------------------------------------------------------------

// stage notes what a running job is doing (no effect when none was started through the job mechanism).
func (s *Service) stage(id, text string) {
	s.mu.Lock()
	if j := s.jobs[id]; j != nil && j.running {
		j.stage = text
		j.trail = append(j.trail, text)
	}
	s.mu.Unlock()
}

// JobStatus reports the latest background job of an OLT.
func (s *Service) JobStatus(id string) JobStatus {
	var v JobStatus
	s.mu.Lock()
	if j := s.jobs[id]; j != nil {
		v = JobStatus{Known: true, Running: j.running, Kind: j.kind, Started: j.started, Finished: j.finished, By: j.by, Stage: j.stage,
			OK: !j.running && j.err == "", Error: j.err, Secure: j.sec, Apply: j.app}
	}
	s.mu.Unlock()
	if !v.Running {
		if _, st, ok := s.Load(id); ok {
			switch {
			case st.Settings.Adopt.PendingPass != "":
				v.Interrupted, v.InterruptedKind = true, JobSecure
			case st.Settings.Job.Kind != "":
				v.Interrupted, v.InterruptedKind = true, JobKind(st.Settings.Job.Kind)
			}
		}
	}
	return v
}
