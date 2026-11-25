package olt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func waitSecure(t *testing.T, a *aenv) SecureStatus {
	t.Helper()
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if v := a.svc.SecureStatus(oltID); v.Known && !v.Running {
			return v
		}
	}
	t.Fatal("the background Secure did not finish")
	return SecureStatus{}
}

func TestStartSecureRunsDetachedAndReports(t *testing.T) {
	a := newAdoptEnv(t)
	a.adopt(t)
	_, old, _ := a.opened(t)
	if err := a.svc.StartSecure(oltID, "olga"); err != nil {
		t.Fatal(err)
	}
	v := a.svc.SecureStatus(oltID)
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
	a.svc.jobs[oltID] = &secJob{running: true, stage: "x", started: time.Now()}
	a.svc.mu.Unlock()
	if err := a.svc.StartSecure(oltID, "olga"); !errors.Is(err, ErrSecureRunning) {
		t.Fatalf("got %v", err)
	}
	a.svc.stage(oltID, "Testing the new login in a new session")
	if v := a.svc.SecureStatus(oltID); !v.Running || v.Stage != "Testing the new login in a new session" {
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
	if v := a.svc.SecureStatus(oltID); v.Known || v.Running || !v.Interrupted {
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
