package olt

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func waitSecure(t *testing.T, a *aenv) JobStatus { t.Helper(); return waitJob(t, a.svc) }

func waitJob(t *testing.T, svc *Service) JobStatus {
	t.Helper()
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if v := svc.JobStatus(oltID); v.Known && !v.Running {
			return v
		}
	}
	t.Fatal("the background job did not finish")
	return JobStatus{}
}

func TestStartSecureRunsDetachedAndReports(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	_, old, _ := a.opened(t)
	if err := a.svc.StartSecure(oltID, "olga"); err != nil {
		t.Fatal(err)
	}
	v := a.svc.JobStatus(oltID)
	if !v.Known || v.Started.IsZero() || v.By != "olga" {
		t.Fatalf("status right after the start: %+v", v)
	}
	v = waitSecure(t, a)
	if !v.OK || v.Error != "" || v.Stage != "Done" {
		t.Fatalf("finished: %+v", v)
	}
	_, nw, _ := a.opened(t)
	if nw == old {
		t.Fatal("the password did not change")
	}
	if !a.adoptState().Secured || v.Interrupted {
		t.Fatalf("%+v", a.adoptState())
	}
	a.noLeak(t, nw)
}

func TestStartSecureRefusesWhileRunningAndBeforeIdentified(t *testing.T) {
	a := newAdoptEnv(t)
	if err := a.svc.StartSecure(oltID, "olga"); !errors.Is(err, ErrNotIdentified) {
		t.Fatalf("got %v", err)
	}
	a.adopt(t)
	a.svc.mu.Lock()
	a.svc.jobs[oltID] = &job{kind: JobSecure, running: true, stage: "x", started: time.Now()}
	a.svc.mu.Unlock()
	if err := a.svc.StartSecure(oltID, "olga"); !errors.Is(err, ErrSecureRunning) {
		t.Fatalf("got %v", err)
	}
	a.svc.stage(oltID, "Testing the new login in a new session")
	if v := a.svc.JobStatus(oltID); !v.Running || v.Stage != "Testing the new login in a new session" {
		t.Fatalf("%+v", v)
	}
}

func TestSecureStatusReportsAnInterruptedSecure(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	// The server stopped half way through a Secure: a sealed pair is stored, no job exists in the new process.
	sl, _ := a.svc.o.Box.SealAAD("PendingLoginPass1234567890abcd", mgmtAAD)
	if err := a.svc.setAdopt(oltID, func(ad *store.OLTAdopt, e *store.Equipment, d *store.Data) { ad.PendingPass = sl }); err != nil {
		t.Fatal(err)
	}
	if v := a.svc.JobStatus(oltID); v.Known || v.Running || !v.Interrupted {
		t.Fatalf("%+v", v)
	}
}

// The bench: Secure failed at the save because `write` took longer than one command may. The save has its own budget.
func TestSecureSurvivesASlowChattySave(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	a.svc.o.Timeout = time.Second
	a.f.WriteBuilding, a.f.WriteDelay = true, 2*time.Second
	if err := a.svc.StartSecure(oltID, "olga"); err != nil {
		t.Fatal(err)
	}
	if v := waitSecure(t, a); !v.OK {
		t.Fatalf("%+v", v)
	}
	if !a.adoptState().Secured {
		t.Fatalf("%+v", a.adoptState())
	}
}

// The bench note read "the old password <redacted> restored": the stored note of a failed Secure must be readable.
func TestSecureFailureNoteIsReadable(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	a.f.FailWrite = true
	if err := a.svc.StartSecure(oltID, "olga"); err != nil {
		t.Fatal(err)
	}
	v := waitSecure(t, a)
	for _, txt := range []string{v.Error, a.adoptState().Error} {
		if !strings.Contains(txt, "the old password was restored and nothing was saved") || strings.Contains(txt, "<redacted> restored") {
			t.Errorf("note: %q", txt)
		}
	}
}

// ---- Apply and Check now as background jobs ------------------------------------------------------------------------------

// slowSave makes the OLT's save take d (chatty, as the bench's does), with a command timeout shorter than that.
func (e *env) slowSave(d time.Duration) {
	e.svc.o.Timeout = time.Second
	e.f.WriteBuilding, e.f.WriteDelay = true, d
}

// stages collects the distinct stage texts of the running job until it ends.
func stages(t *testing.T, svc *Service) ([]string, JobStatus) {
	t.Helper()
	var seen []string
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		v := svc.JobStatus(oltID)
		if v.Known && v.Stage != "" && (len(seen) == 0 || seen[len(seen)-1] != v.Stage) {
			seen = append(seen, v.Stage)
		}
		if v.Known && !v.Running {
			return seen, v
		}
	}
	t.Fatal("the background job did not finish")
	return nil, JobStatus{}
}

func TestStartApplyRunsInTheBackgroundWithStages(t *testing.T) {
	e := newEnv(t)
	e.withACS()
	e.f.SetRunning("")
	e.enable(t)
	e.slowSave(2500 * time.Millisecond)
	p, h, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := e.svc.StartApply(context.Background(), oltID, h, "olga"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("StartApply waited for the write (%v)", d)
	}
	v := e.svc.JobStatus(oltID)
	if !v.Known || !v.Running || v.Kind != JobApply || v.By != "olga" || v.Started.IsZero() {
		t.Fatalf("status right after the start: %+v", v)
	}
	if m := e.state().Settings.Job; m.Kind != "apply" || m.Plan != h || m.By != "olga" {
		t.Fatalf("no durable mark while it runs: %+v", m)
	}
	_, v = stages(t, e.svc)
	e.svc.mu.Lock()
	seen := append([]string(nil), e.svc.jobs[oltID].trail...)
	e.svc.mu.Unlock()
	if !v.OK || v.Stage != "Done" || !v.Apply.Verified || len(v.Apply.Sent) != len(p.Commands()) || v.Apply.Backup == "" {
		t.Fatalf("finished: %+v", v)
	}
	joined := strings.Join(seen, "|")
	for _, want := range []string{"Saving a copy of the configuration", "Sending " + strconv.Itoa(len(p.Commands())) + " lines", "Saving on the OLT", "Reading back"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stage %q not shown; saw %q", want, joined)
		}
	}
	if e.state().Settings.Job.Kind != "" || v.Interrupted {
		t.Fatalf("the mark stays after the job: %+v", e.state().Settings.Job)
	}
	if !strings.Contains(strings.Join(e.f.Lines(), "\n"), "password "+testACSPass) {
		t.Fatal("the OLT was not given the login")
	}
	if !strings.Contains(e.auditText(), "configured (plan "+h) || !strings.Contains(e.auditText(), ": ok.") {
		t.Fatalf("audit: %s", e.auditText())
	}
	// No secret in the status the page and the API are built from, nor in the audit and alerts.
	e.noSecretsAnywhere(t, fmt.Sprintf("%+v", v))
	if p2, _, _ := e.svc.Plan(context.Background(), oltID); !p2.Empty() {
		t.Fatal("the plan is not empty after the apply")
	}
}

// What the request decides stays in the request: nothing starts for a stale hash, an unset hash or provisioning off.
func TestStartApplyChecksBeforeStarting(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning("")
	if err := e.svc.StartApply(context.Background(), oltID, "x", "olga"); !errors.Is(err, ErrProvisioningOff) {
		t.Fatalf("provisioning off: %v", err)
	}
	e.enable(t)
	e.f.Reset()
	for _, h := range []string{"", "deadbeef"} {
		if err := e.svc.StartApply(context.Background(), oltID, h, "olga"); !errors.Is(err, ErrPlanChanged) {
			t.Fatalf("hash %q: %v", h, err)
		}
	}
	if e.svc.JobStatus(oltID).Known || e.state().Settings.Job.Kind != "" {
		t.Fatal("a job or mark exists after a refused start")
	}
	for _, l := range e.f.Lines() {
		if writeLine(l) {
			t.Fatalf("a refused start sent %q", l)
		}
	}
}

// One job of any kind per OLT: while an Apply runs, a second Apply, a Secure and a Check now are all refused and say
// which job holds the slot; nothing else is started.
func TestOneJobPerOLTAcrossKinds(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	a.f.SetRunning("")
	a.enable(t)
	a.slowSave(2 * time.Second)
	_, h, err := a.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.svc.StartApply(context.Background(), oltID, h, "olga"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"apply":  a.svc.StartApply(context.Background(), oltID, h, "olga"),
		"secure": a.svc.StartSecure(oltID, "olga"),
		"check":  a.svc.StartCheck(oltID, "olga"),
	} {
		k, ok := RunningKind(err)
		if !errors.Is(err, ErrJobRunning) || !ok || k != JobApply || !strings.Contains(err.Error(), "Applying is already running") {
			t.Errorf("%s while applying: %v", name, err)
		}
	}
	if v := waitJob(t, a.svc); !v.OK || v.Kind != JobApply {
		t.Fatalf("%+v", v)
	}
	// The slot is free again; and a Check now holds it against the others.
	if err := a.svc.StartCheck(oltID, "olga"); err != nil {
		t.Fatal(err)
	}
	if v := a.svc.JobStatus(oltID); v.Kind != JobCheck || !v.Running {
		t.Fatalf("%+v", v)
	}
	if k, _ := RunningKind(a.svc.StartSecure(oltID, "olga")); k != JobCheck {
		t.Fatalf("secure during check: %v", k)
	}
	if err := a.svc.StartApply(context.Background(), oltID, h, "olga"); !errors.Is(err, ErrJobRunning) {
		t.Fatalf("apply during check: %v", err)
	}
	if v := waitJob(t, a.svc); !v.OK || v.Kind != JobCheck {
		t.Fatalf("%+v", v)
	}
}

// "Check now" on a factory OLT secures it (a save of seconds, longer than one command may take) without holding the request.
func TestStartCheckSecuresAFactoryOLTInTheBackground(t *testing.T) {
	a := newAdoptEnv(t)
	a.slowSave(2 * time.Second)
	t0 := time.Now()
	if err := a.svc.StartCheck(oltID, "olga"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("StartCheck waited (%v)", d)
	}
	_, v := stages(t, a.svc)
	a.svc.mu.Lock()
	seen := append([]string(nil), a.svc.jobs[oltID].trail...)
	a.svc.mu.Unlock()
	if !v.OK || v.Kind != JobCheck || v.Stage != "Done" {
		t.Fatalf("%+v (stages %v)", v, seen)
	}
	if !strings.Contains(strings.Join(seen, "|"), "Saving the OLT's configuration") {
		t.Errorf("the secure stages are not shown while checking: %v", seen)
	}
	st := a.adoptState()
	if !st.Secured || !st.Adopted {
		t.Fatalf("%+v", st)
	}
	_, nw, _ := a.opened(t)
	a.noLeak(t, nw)
}

// What is known at once is answered at once and starts no job (and the finding is recorded as before).
func TestStartCheckAnswersAtOnceWithoutAPort(t *testing.T) {
	a := newAdoptEnv(t)
	if err := a.st.Update(func(d *store.Data) error {
		s := d.Sites[siteID]
		s.OLTMgmtPort = ""
		d.Sites[siteID] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.svc.StartCheck(oltID, "olga"); !errors.Is(err, ErrNoPort) {
		t.Fatalf("%v", err)
	}
	if a.svc.JobStatus(oltID).Known || a.adoptState().Stage != store.AdoptNoPort {
		t.Fatalf("%+v", a.adoptState())
	}
}

// A failed Apply is a finished job with a redacted reason; the slot is free again.
func TestApplyJobFailureIsReportedAndFreesTheSlot(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning("")
	e.enable(t)
	_, h, _ := e.svc.Plan(context.Background(), oltID)
	e.f.FailOn = "profile srv name"
	if err := e.svc.StartApply(context.Background(), oltID, h, "olga"); err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, e.svc)
	if v.OK || v.Stage != "Failed" || v.Error == "" || v.Running {
		t.Fatalf("%+v", v)
	}
	if !strings.Contains(e.auditText(), "FAILED") || !strings.Contains(e.alertText(), "configuration stopped") {
		t.Fatal("no audit or alert for a failed apply")
	}
	if e.state().Settings.Job.Kind != "" {
		t.Fatal("the mark stays after a failed job")
	}
	if err := e.svc.busy(oltID); err != nil {
		t.Fatalf("slot still held: %v", err)
	}
}

// The server stopped in the middle of an Apply: no job in the new process, the durable mark says so; reading the plan again
// clears it. Plan idempotence: what the first run put on the OLT is not sent again, and a finished plan is empty.
func TestApplyInterruptedByRestartIsReportedAndNothingIsAppliedTwice(t *testing.T) {
	e := newEnv(t)
	e.f.SetRunning("")
	e.enable(t)
	p1, h, _ := e.svc.Plan(context.Background(), oltID)
	// The first run is cut half way: a late line is refused, so the lines before it are on the OLT (not saved).
	e.f.FailOn = "interface vlan 98"
	res, err := e.svc.Apply(context.Background(), oltID, h, "olga")
	if err == nil || len(res.Sent) == 0 || len(res.Sent) >= len(p1.Commands()) {
		t.Fatalf("not cut half way: sent %d of %d, err %v", len(res.Sent), len(p1.Commands()), err)
	}
	// ... and the process died before it could clear its mark.
	if err := e.svc.upd(oltID, func(d *store.Data, _ store.Equipment, st *store.OLTState) {
		st.Settings.Job = store.OLTJobMark{Kind: "apply", By: "olga", At: time.Now(), Plan: h}
	}); err != nil {
		t.Fatal(err)
	}
	e.f.FailOn = ""
	e.svc = New(e.svc.o) // the restart: a new process has no job record
	v := e.svc.JobStatus(oltID)
	if v.Known || v.Running || !v.Interrupted || v.InterruptedKind != JobApply {
		t.Fatalf("%+v", v)
	}
	p2, h2, err := e.svc.Plan(context.Background(), oltID)
	if err != nil {
		t.Fatal(err)
	}
	if v := e.svc.JobStatus(oltID); v.Interrupted {
		t.Fatal("reading the plan again does not clear the report")
	}
	if len(p2.Commands()) == 0 || len(p2.Commands()) >= len(p1.Commands()) {
		t.Fatalf("the new plan is not just the rest: %d of %d lines", len(p2.Commands()), len(p1.Commands()))
	}
	have := map[string]bool{}
	for _, l := range p2.Commands() {
		have[l] = true
	}
	// A line that creates something and was sent the first time is not in the new plan (exit/end style lines repeat).
	for _, l := range res.Sent {
		if strings.HasPrefix(l, "vlan ") && have[l] {
			t.Errorf("%q would be sent twice", l)
		}
	}
	res2, err := e.svc.Apply(context.Background(), oltID, h2, "olga")
	if err != nil || !res2.Verified {
		t.Fatalf("second run: %+v %v", res2, err)
	}
	if p3, _, _ := e.svc.Plan(context.Background(), oltID); !p3.Empty() {
		t.Fatal("not empty after the second run")
	}
}
