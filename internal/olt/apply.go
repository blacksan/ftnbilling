package olt

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Result of an apply.
type Result struct {
	Backup    string // id of the configuration saved before the change
	Sent      []string
	Verified  bool
	Remaining []string // lines the re-read configuration still lacks
	Entries   []Entry
	// Failed are the steps the OLT refused a line of ("title: why"); Skipped the steps not tried because an earlier step
	// failed; Done the steps that went through (only set when a step failed).
	Failed, Skipped, Done []string
	// SavedPartly: a step failed, but steps that do not depend on it went through and the configuration was saved.
	SavedPartly bool
}

func (s *Service) mayConfigure(st store.OLTState) error {
	if !st.Settings.Provisioning || !st.Settings.DialectChecked {
		return ErrProvisioningOff
	}
	return nil
}

// readyAdds are the found ONUs the plan authorises: verdict ready (their serial is in the inventory, given to a house
// that this OLT and PON serve). The verdict stored by the last poll is not trusted on its own: the inventory may have
// changed since (the ONU retired, given to another house, moved to another OLT), so each one is classified again
// against the data now, and only a serial that is still ready, for the same inventory item, is added.
func readyAdds(d *store.Data, olt store.Equipment, st store.OLTState, retryAfter time.Duration, now time.Time) []DesiredONU {
	var out []DesiredONU
	for _, f := range st.Found {
		if f.Verdict != store.OLTFoundReady || f.PON <= 0 || !serialOK(f.Serial) {
			continue
		}
		if !f.TriedAt.IsZero() && now.Sub(f.TriedAt) < retryAfter {
			continue
		}
		v, label, eid := Classify(d, olt, Found{PON: f.PON, Serial: f.Serial})
		if v != VerdictReady || eid != f.EquipmentID {
			continue
		}
		out = append(out, DesiredONU{PON: f.PON, Serial: f.Serial, EquipmentID: eid, Label: label})
	}
	return out
}

// serialOK: a GPON serial as the auto-find parser reads it (4 letters, 8 hex digits). Nothing else is ever put into
// an "onu add" line.
func serialOK(s string) bool { return gponSerialRE.MatchString(s) }

var gponSerialRE = regexp.MustCompile(`^[A-Z]{4}[0-9A-F]{8}$`)

// computePlan reads the running configuration inside a session and builds the plan.
func (s *Service) computePlan(ctx context.Context, o *opened) (Plan, *Running, error) {
	text, err := s.readRunning(ctx, o.sess)
	if err != nil {
		return Plan{}, nil, err
	}
	run := ParseRunning(text)
	var want Desired
	var notes []string
	var derr error
	s.o.Store.View(func(d *store.Data) {
		e := d.Equipment[o.e.ID]
		st := d.OLTStateOf(o.e.ID)
		want, notes, derr = BuildDesired(d, e, st, run, readyAdds(d, e, st, 0, s.now()), false, s.o.Dialect, s.acsInfo())
	})
	if derr != nil {
		return Plan{}, nil, derr
	}
	p := s.o.Dialect.BuildPlan(want, run)
	p.Notes = append(notes, p.Notes...)
	return p, run, nil
}

// Plan shows what the server would change on the OLT. It only reads, so it works with provisioning off: that is how an
// owner checks the dialect before turning provisioning on.
func (s *Service) Plan(ctx context.Context, id string) (Plan, string, error) {
	var p Plan
	err := s.withSession(ctx, id, true, func(o *opened) error {
		var err error
		p, _, err = s.computePlan(ctx, o)
		return err
	})
	if err == nil {
		// Reading the plan again is the answer to "unfinished": the plan is built from what the OLT has now.
		s.clearStaleMark(id)
	}
	return p, p.Hash(), err
}

// clearStaleMark forgets the mark of an interrupted apply when no job runs.
func (s *Service) clearStaleMark(id string) { s.clearJobMark(id, true) }

// Apply runs the plan the owner reviewed (its hash). It saves the running configuration first, sends the lines, saves,
// re-reads and compares, and writes an audit entry with the (redacted) transcript. The first refused line stops it.
func (s *Service) Apply(ctx context.Context, id, wantHash, by string) (Result, error) {
	return s.apply(ctx, id, wantHash, by, nil)
}

// apply is Apply with a stage report (nil = none): the background job tells the page what it is doing.
func (s *Service) apply(ctx context.Context, id, wantHash, by string, stage func(string)) (Result, error) {
	var res Result
	err := s.withSession(ctx, id, true, func(o *opened) error {
		if err := s.mayConfigure(o.st); err != nil {
			return err
		}
		p, _, err := s.computePlan(ctx, o)
		if err != nil {
			return err
		}
		if wantHash == "" || p.Hash() != wantHash {
			return ErrPlanChanged
		}
		res, err = s.applyPlan(ctx, o, p, "before a configuration change", by, s.verifyPlan(id, false), stage)
		return err
	})
	return res, err
}

// applyPlan is the one place the server writes to an OLT: backup, send, save, verify, audit. The caller has checked
// permission.
func (s *Service) applyPlan(ctx context.Context, o *opened, p Plan, why, by string, verify verifier, stage func(string)) (Result, error) {
	var res Result
	cmds := p.Commands()
	if len(cmds) == 0 {
		return res, nil
	}
	if stage == nil {
		stage = func(string) {}
	}
	stage("Saving a copy of the configuration")
	b, _, err := s.backupIn(ctx, o, why, by)
	if err != nil {
		return res, fmt.Errorf("the configuration could not be saved first, nothing was changed: %w", err)
	}
	res.Backup = b.ID
	// From the first configuration line on, the batch runs to its end (or its first refused line) even when the web
	// request that started it is cancelled or times out: stopping half way would leave a half-made ONU or VLAN in the
	// running configuration, unsaved. The OLT's own per-command timeouts still apply.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyWriteBudget)
	defer cancel()
	before := len(o.sess.Transcript)
	// Every line of every step passes the denylist before the first one is sent.
	if err := o.sess.CheckAll(cmds); err != nil {
		return res, err
	}
	stage(fmt.Sprintf("Sending %d lines", len(cmds)))
	// Each step is its own batch. A refused line stops its step and every later step, except the Independent ones (the
	// per-ONU lines of ONUs already on the OLT): ONU 1 gets its TR-069 lines even when a new Pri profile is refused.
	var cerr error
	okAfterFail := false
	donePON := map[int]bool{}
	for i, st := range p.Steps {
		if cerr != nil && !st.Independent {
			res.Skipped = append(res.Skipped, st.Title)
			continue
		}
		n, err := o.sess.ConfigN(wctx, st.Lines)
		res.Sent = append(res.Sent, redactAll(st.Lines[:n])...)
		if err != nil {
			res.Failed = append(res.Failed, st.Title+": "+errText(err))
			if cerr == nil {
				cerr = err
			}
			if ctx.Err() != nil || wctx.Err() != nil || !isRefusal(err) {
				// The session itself failed (not a refused line): nothing more is tried.
				for _, rest := range p.Steps[i+1:] {
					res.Skipped = append(res.Skipped, rest.Title)
				}
				break
			}
			continue
		}
		res.Done = append(res.Done, st.Title)
		if cerr != nil {
			okAfterFail = true
		}
		if st.NewONUs > 0 {
			donePON[st.NewONUs] = true
		}
	}
	if cerr == nil || okAfterFail {
		stage("Saving on the OLT")
		if serr := o.sess.Save(wctx); serr != nil {
			if cerr == nil {
				cerr = serr
			} else {
				res.Failed = append(res.Failed, "save: "+errText(serr))
			}
		} else if cerr != nil {
			res.SavedPartly = true
		}
	}
	res.Entries = append([]Entry(nil), o.sess.Transcript[before:]...)
	if cerr != nil {
		err := stepError(cerr, res)
		s.auditApply(o.e, by, p, res, "FAILED: "+errText(err))
		if s.o.Alert != nil {
			s.o.Alert("OLT "+store.OLTLabel(o.e)+": configuration stopped", clip(errText(err), 400)+" The earlier lines may have been applied; the configuration was saved first.")
		}
		// The ONUs a step that went through authorised are still recorded.
		s.afterApply(o, addsOf(p, donePON))
		return res, err
	}
	res.Failed, res.Skipped, res.Done = nil, nil, nil
	// The ONUs it authorised are placed on this OLT before the check, so that the check looks at their services too.
	s.afterApply(o, p)
	// Verify: read it back; nothing the plan wanted may be missing.
	stage("Reading back")
	text, err := s.readRunning(wctx, o.sess)
	if err == nil {
		run := ParseRunning(text)
		s.o.Store.View(func(d *store.Data) { res.Remaining = redactAll(verify(d, run)) })
		res.Verified = len(res.Remaining) == 0
	}
	s.auditApply(o.e, by, p, res, "ok")
	return res, nil
}

// isRefusal: the OLT answered the line with an error (the session is fine), as opposed to a broken or timed-out session.
func isRefusal(err error) bool {
	var ce *CmdError
	return errors.As(err, &ce)
}

// stepError is the error of an apply where a step failed: which step, what was still done (and saved), what was skipped.
func stepError(first error, res Result) error {
	if len(res.Failed) == 0 {
		return first
	}
	msg := "the OLT refused a line in step " + res.Failed[0]
	if len(res.Failed) > 1 {
		msg += "; also failed: " + strings.Join(res.Failed[1:], "; ")
	}
	if len(res.Done) > 0 {
		msg += "; went through: " + strings.Join(res.Done, ", ")
		if res.SavedPartly {
			msg += " (saved)"
		} else {
			msg += " (not saved)"
		}
	}
	if len(res.Skipped) > 0 {
		msg += "; skipped: " + strings.Join(res.Skipped, ", ")
	}
	return &stepErr{msg: msg, err: first}
}

type stepErr struct {
	msg string
	err error
}

func (e *stepErr) Error() string { return e.msg }
func (e *stepErr) Unwrap() error { return e.err }

// addsOf is the plan's adds whose step went through (by PON).
func addsOf(p Plan, done map[int]bool) Plan {
	var q Plan
	for _, a := range p.Adds {
		if done[a.PON] {
			q.Adds = append(q.Adds, a)
		}
	}
	return q
}

// verifier lists what is still missing after an apply, from the re-read configuration.
type verifier func(d *store.Data, run *Running) []string

// verifyPlan rebuilds the plan from the re-read configuration: an empty result means the OLT is as wanted.
func (s *Service) verifyPlan(id string, onusOnly bool) verifier {
	return func(d *store.Data, run *Running) []string {
		want, _, err := BuildDesired(d, d.Equipment[id], d.OLTStateOf(id), run, nil, onusOnly, s.o.Dialect, s.acsInfo())
		if err != nil {
			return []string{err.Error()}
		}
		return s.o.Dialect.BuildPlan(want, run).Commands()
	}
}

// verifyRemoved checks that a serial is gone from the configuration.
func verifyRemoved(serial string) verifier {
	return func(d *store.Data, run *Running) []string {
		if _, _, ok := run.ONUBySerial(serial); ok {
			return []string{"ONU " + NormSerial(serial) + " is still authorised"}
		}
		return nil
	}
}

func (s *Service) auditApply(e store.Equipment, by string, p Plan, res Result, outcome string) {
	var b strings.Builder
	fmt.Fprintf(&b, "OLT %s configured (plan %s, %d lines): %s. ", store.OLTLabel(e), p.Hash(), len(res.Sent), outcome)
	if !res.Verified && outcome == "ok" {
		fmt.Fprintf(&b, "Verify: %d lines still missing. ", len(res.Remaining))
	}
	for _, en := range res.Entries {
		b.WriteString("> " + en.Cmd)
		if en.Err != "" {
			b.WriteString(" [" + en.Err + "] " + clip(en.Out, 200))
		}
		b.WriteString("; ")
	}
	_ = s.upd(e.ID, func(d *store.Data, e store.Equipment, st *store.OLTState) { s.audit(d, e, by, b.String()) })
}

// afterApply records the ONUs a plan authorised: their place in the inventory and their found-list verdict.
func (s *Service) afterApply(o *opened, p Plan) {
	if len(p.Adds) == 0 {
		return
	}
	_ = s.upd(o.e.ID, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		for _, a := range p.Adds {
			if eq, ok := d.Equipment[a.EquipmentID]; ok && eq.Kind == store.KindONT {
				eq.OLTID, eq.PONPort, eq.UpdatedAt = e.ID, a.PON, s.now()
				d.Equipment[eq.ID] = eq
			}
			for i := range st.Found {
				if st.Found[i].PON == a.PON && st.Found[i].Serial == a.Serial {
					st.Found[i].Verdict = store.OLTFoundAdded
					st.Found[i].Detail = fmt.Sprintf("authorised as ONU %d", a.ID)
				}
			}
			s.audit(d, e, "system", fmt.Sprintf("ONU %s authorised on PON %d of %s as ONU %d (%s)", a.Serial, a.PON, store.OLTLabel(e), a.ID, a.Label))
		}
	})
}

// autoProvision authorises the found ONUs that are ready, inside the poll's session. It changes nothing but ONUs: the
// base configuration (VLANs, profiles) must already be there, else the ONUs wait.
func (s *Service) autoProvision(ctx context.Context, o *opened) {
	var ready []DesiredONU
	s.o.Store.View(func(d *store.Data) {
		ready = readyAdds(d, d.Equipment[o.e.ID], d.OLTStateOf(o.e.ID), 30*time.Minute, s.now())
	})
	if len(ready) > 8 {
		ready = ready[:8]
	}
	if len(ready) == 0 {
		return
	}
	text, err := s.readRunning(ctx, o.sess)
	if err != nil {
		return
	}
	run := ParseRunning(text)
	dl := s.o.Dialect
	if !run.HasBlock("profile line name "+dl.DefaultLine) || !run.HasBlock("profile srv name "+dl.DefaultSrv) || !run.HasBlock("profile pri name "+dl.DefaultPri) {
		s.markFound(o.e.ID, ready, store.OLTFoundHeld, "the base configuration (profiles) is not on the OLT yet: open Plan and apply it first")
		return
	}
	var want Desired
	s.o.Store.View(func(d *store.Data) {
		want, _, _ = BuildDesired(d, d.Equipment[o.e.ID], d.OLTStateOf(o.e.ID), run, ready, true, s.o.Dialect, s.acsInfo())
	})
	p := s.o.Dialect.BuildPlan(want, run)
	if len(p.Adds) == 0 {
		return
	}
	res, err := s.applyPlan(ctx, o, p, "before automatic ONU add", "system", s.verifyPlan(o.e.ID, true), nil)
	if err != nil {
		s.markFound(o.e.ID, ready, store.OLTFoundFailed, errText(err))
		return
	}
	if !res.Verified && s.o.Alert != nil {
		s.o.Alert("OLT "+store.OLTLabel(o.e)+": check the ONUs", fmt.Sprintf("%d ONU(s) were authorised but the OLT's configuration does not show every line.", len(p.Adds)))
	}
	if s.o.Alert != nil {
		var l []string
		for _, a := range p.Adds {
			l = append(l, fmt.Sprintf("%s on PON %d as ONU %d (%s)", a.Serial, a.PON, a.ID, a.Label))
		}
		s.o.Alert("OLT "+store.OLTLabel(o.e)+": ONU authorised", strings.Join(l, "; "))
	}
}

func (s *Service) markFound(id string, ons []DesiredONU, verdict, detail string) {
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		for _, a := range ons {
			for i := range st.Found {
				if st.Found[i].PON == a.PON && st.Found[i].Serial == a.Serial {
					st.Found[i].Verdict, st.Found[i].Detail, st.Found[i].TriedAt = verdict, clip(detail, 200), s.now()
				}
			}
		}
	})
}

// ---- removal: only on an explicit owner action ---------------------------------------------------------------------------

// PlanRemove shows the command that would remove an ONU (by its GPON serial) from the OLT. It never runs by itself.
func (s *Service) PlanRemove(ctx context.Context, id, serial string) (Plan, string, error) {
	var p Plan
	err := s.withSession(ctx, id, true, func(o *opened) error {
		var err error
		p, err = s.removePlan(ctx, o, serial)
		return err
	})
	return p, p.Hash(), err
}

func (s *Service) removePlan(ctx context.Context, o *opened, serial string) (Plan, error) {
	text, err := s.readRunning(ctx, o.sess)
	if err != nil {
		return Plan{}, err
	}
	run := ParseRunning(text)
	pon, ro, ok := run.ONUBySerial(serial)
	if !ok {
		return Plan{}, fmt.Errorf("ONU %s is not authorised on this OLT", NormSerial(serial))
	}
	d := s.o.Dialect
	return Plan{
		Steps: []Step{{Title: fmt.Sprintf("Remove ONU %d (%s) from PON %d", ro.ID, NormSerial(serial), pon),
			Lines: append(append(d.PONEnter(pon), d.ONURemoveLine(ro.ID)), d.PONExit()...)}},
		Notes: []string{"The ONU stops working at once. This is never done automatically."},
	}, nil
}

// ApplyRemove runs a reviewed removal plan.
func (s *Service) ApplyRemove(ctx context.Context, id, serial, wantHash, by string) (Result, error) {
	return s.applyRemove(ctx, id, serial, wantHash, by, nil)
}

// applyRemove is ApplyRemove with a stage report (nil = none).
func (s *Service) applyRemove(ctx context.Context, id, serial, wantHash, by string, stage func(string)) (Result, error) {
	var res Result
	err := s.withSession(ctx, id, true, func(o *opened) error {
		if err := s.mayConfigure(o.st); err != nil {
			return err
		}
		p, err := s.removePlan(ctx, o, serial)
		if err != nil {
			return err
		}
		if wantHash == "" || p.Hash() != wantHash {
			return ErrPlanChanged
		}
		res, err = s.applyPlan(ctx, o, p, "before removing an ONU", by, verifyRemoved(serial), stage)
		return err
	})
	return res, err
}

// readRunning reads the running configuration in the mode the dialect needs.
func (s *Service) readRunning(ctx context.Context, sess *Session) (string, error) {
	return sess.ShowAt(ctx, s.o.Dialect.RunningMode, s.o.Dialect.RunningCmd)
}

// acsInfo is the ACS address and the shared first-contact login the ONUs are given (nil when there is no ACS or it has
// no login yet).
func (s *Service) acsInfo() *ACS {
	if s.o.ACS == nil {
		return nil
	}
	return s.o.ACS()
}
