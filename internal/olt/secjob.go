package olt

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// "Secure this OLT" takes minutes (backup, new passwords, a second session to test them, saves, SNMP, the SSH access
// list), far longer than one HTTP request may last. StartSecure runs it in the background, detached from the request;
// SecureStatus reports it. One job per OLT; the session lock still serialises it with every other OLT operation. The
// result is stored as before (OLTAdopt notes, audit); the job record here only feeds the progress display.

// secureJobTimeout bounds one background Secure.
const secureJobTimeout = 10 * time.Minute

// ErrSecureRunning: a Secure of this OLT is already in progress.
var ErrSecureRunning = errors.New("Secure this OLT is already running for this OLT")

type secJob struct {
	by       string
	started  time.Time
	finished time.Time
	stage    string
	running  bool
	err      string
	res      SecureResult
}

// SecureStatus is the progress or the outcome of a background Secure. It holds no secret.
type SecureStatus struct {
	Known    bool // a job ran (or runs) in this server process
	Running  bool
	Started  time.Time
	Finished time.Time
	By       string
	Stage    string
	OK       bool   // finished without an error
	Error    string // why it failed
	Result   SecureResult
	// Interrupted: no job runs, but a sealed new login pair is still stored from a Secure that did not finish (the
	// server stopped, or the OLT was left half way). The server tries that pair at its next check of the OLT.
	Interrupted bool
}

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
	s.mu.Lock()
	if j := s.jobs[id]; j != nil && j.running {
		s.mu.Unlock()
		return ErrSecureRunning
	}
	j := &secJob{by: by, started: s.now(), stage: "Starting", running: true}
	if s.jobs == nil {
		s.jobs = map[string]*secJob{}
	}
	s.jobs[id] = j
	s.mu.Unlock()
	go s.runSecureJob(id, by, j)
	return nil
}

func (s *Service) runSecureJob(id, by string, j *secJob) {
	ctx, cancel := context.WithTimeout(context.Background(), secureJobTimeout)
	defer cancel()
	var res SecureResult
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("Secure this OLT stopped unexpectedly: %v", r)
			}
		}()
		res, err = s.Secure(ctx, id, by)
	}()
	s.mu.Lock()
	j.running, j.finished, j.res = false, s.now(), res
	if err != nil {
		j.err = errText(err)
		j.stage = "Failed"
	} else {
		j.stage = "Done"
	}
	s.mu.Unlock()
}

// stage notes what a running Secure is doing (no effect when none was started through StartSecure).
func (s *Service) stage(id, text string) {
	s.mu.Lock()
	if j := s.jobs[id]; j != nil && j.running {
		j.stage = text
	}
	s.mu.Unlock()
}

// SecureStatus reports the background Secure of an OLT.
func (s *Service) SecureStatus(id string) SecureStatus {
	var v SecureStatus
	s.mu.Lock()
	if j := s.jobs[id]; j != nil {
		v = SecureStatus{Known: true, Running: j.running, Started: j.started, Finished: j.finished, By: j.by, Stage: j.stage,
			OK: !j.running && j.err == "", Error: j.err, Result: j.res}
	}
	s.mu.Unlock()
	if !v.Running {
		if _, st, ok := s.Load(id); ok && st.Settings.Adopt.PendingPass != "" {
			v.Interrupted = true
		}
	}
	return v
}
