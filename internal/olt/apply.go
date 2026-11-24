package olt

import (
	"context"
	"fmt"
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
}

func (s *Service) mayConfigure(st store.OLTState) error {
	if !st.Settings.Provisioning || !st.Settings.DialectChecked {
		return ErrProvisioningOff
	}
	return nil
}

// readyAdds are the found ONUs the plan authorises: verdict ready (their serial is in the inventory, given to a house
// that this OLT and PON serve).
func readyAdds(st store.OLTState, retryAfter time.Duration, now time.Time) []DesiredONU {
	var out []DesiredONU
	for _, f := range st.Found {
		if f.Verdict != store.OLTFoundReady || f.PON <= 0 {
			continue
		}
		if !f.TriedAt.IsZero() && now.Sub(f.TriedAt) < retryAfter {
			continue
		}
		out = append(out, DesiredONU{PON: f.PON, Serial: f.Serial, EquipmentID: f.EquipmentID, Label: f.Detail})
	}
	return out
}

// computePlan reads the running configuration inside a session and builds the plan.
func (s *Service) computePlan(ctx context.Context, o *opened) (Plan, *Running, error) {
	text, err := o.sess.Show(ctx, s.o.Dialect.RunningCmd)
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
		want, notes, derr = BuildDesired(d, e, st, run, readyAdds(st, 0, s.now()), false)
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
	return p, p.Hash(), err
}

// Apply runs the plan the owner reviewed (its hash). It saves the running configuration first, sends the lines, saves,
// re-reads and compares, and writes an audit entry with the (redacted) transcript. The first refused line stops it.
func (s *Service) Apply(ctx context.Context, id, wantHash, by string) (Result, error) {
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
		res, err = s.applyPlan(ctx, o, p, "before a configuration change", by, s.verifyPlan(id, false))
		return err
	})
	return res, err
}

// applyPlan is the one place the server writes to an OLT: backup, send, save, verify, audit. The caller has checked
// permission.
func (s *Service) applyPlan(ctx context.Context, o *opened, p Plan, why, by string, verify verifier) (Result, error) {
	var res Result
	cmds := p.Commands()
	if len(cmds) == 0 {
		return res, nil
	}
	b, _, err := s.backupIn(ctx, o, why, by)
	if err != nil {
		return res, fmt.Errorf("the configuration could not be saved first, nothing was changed: %w", err)
	}
	res.Backup = b.ID
	before := len(o.sess.Transcript)
	cerr := o.sess.Config(ctx, cmds)
	res.Sent = cmds
	if cerr == nil {
		cerr = o.sess.Save(ctx)
	}
	res.Entries = append([]Entry(nil), o.sess.Transcript[before:]...)
	if cerr != nil {
		s.auditApply(o.e, by, p, res, "FAILED: "+errText(cerr))
		if s.o.Alert != nil {
			s.o.Alert("OLT "+store.OLTLabel(o.e)+": configuration stopped", clip(errText(cerr), 400)+" The earlier lines may have been applied; the configuration was saved first.")
		}
		return res, cerr
	}
	// The ONUs it authorised are placed on this OLT before the check, so that the check looks at their services too.
	s.afterApply(o, p)
	// Verify: read it back; nothing the plan wanted may be missing.
	text, err := o.sess.Show(ctx, s.o.Dialect.RunningCmd)
	if err == nil {
		run := ParseRunning(text)
		s.o.Store.View(func(d *store.Data) { res.Remaining = verify(d, run) })
		res.Verified = len(res.Remaining) == 0
	}
	s.auditApply(o.e, by, p, res, "ok")
	return res, nil
}

// verifier lists what is still missing after an apply, from the re-read configuration.
type verifier func(d *store.Data, run *Running) []string

// verifyPlan rebuilds the plan from the re-read configuration: an empty result means the OLT is as wanted.
func (s *Service) verifyPlan(id string, onusOnly bool) verifier {
	return func(d *store.Data, run *Running) []string {
		want, _, err := BuildDesired(d, d.Equipment[id], d.OLTStateOf(id), run, nil, onusOnly)
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
	s.o.Store.View(func(d *store.Data) { ready = readyAdds(d.OLTStateOf(o.e.ID), 30*time.Minute, s.now()) })
	if len(ready) > 8 {
		ready = ready[:8]
	}
	if len(ready) == 0 {
		return
	}
	text, err := o.sess.Show(ctx, s.o.Dialect.RunningCmd)
	if err != nil {
		return
	}
	run := ParseRunning(text)
	if !run.HasBlock("profile dba name "+DefaultDBA) || !run.HasBlock("profile onu name "+DefaultONUProfile) {
		s.markFound(o.e.ID, ready, store.OLTFoundHeld, "the base configuration (profiles) is not on the OLT yet: open Plan and apply it first")
		return
	}
	var want Desired
	s.o.Store.View(func(d *store.Data) {
		want, _, _ = BuildDesired(d, d.Equipment[o.e.ID], d.OLTStateOf(o.e.ID), run, ready, true)
	})
	p := s.o.Dialect.BuildPlan(want, run)
	if len(p.Adds) == 0 {
		return
	}
	res, err := s.applyPlan(ctx, o, p, "before automatic ONU add", "system", s.verifyPlan(o.e.ID, true))
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
	text, err := o.sess.Show(ctx, s.o.Dialect.RunningCmd)
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
		res, err = s.applyPlan(ctx, o, p, "before removing an ONU", by, verifyRemoved(serial))
		return err
	})
	return res, err
}
