package acs

import (
	"context"
	"strconv"
	"time"

	"ispbill/internal/store"
)

// program is what the server does in one session with one ONU, after the Inform and the trust decision (trust.go):
//
//  1. First contact, factory reset, boot or a refresh request: discover the Wi-Fi instances and WAN MACs.
//  2. A box that is not quarantined and not unverified is provisioned: its own ACS login, its connection-request
//     login, the periodic Inform.
//  3. A verified box linked to a house gets the house's Wi-Fi (admin lock, and the Wi-Fi itself) when it is new to the
//     house, or when it has just come back from a factory reset.
//  4. The queued tasks run.
//
// A boot (1 BOOT) only refreshes what the ONU reports; it never writes the Wi-Fi configuration (the owner of the
// box may have changed it by hand).
func (s *Service) program(ctx context.Context, ss *session, in Inform, onuID string, v verdict, ai authInfo) {
	o, ok := s.loadONU(onuID)
	if !ok {
		return
	}
	bootstrap := in.HasEvent("0")
	boot := in.HasEvent("1")
	if boot && !bootstrap && !v.reset {
		in = s.withKey(ctx, ss, o, in)
	}
	reset := bootstrap || v.reset || s.keyShowsReset(o, in)
	s.noteKey(onuID, in) // the key this Inform reports
	needDiscover := !o.Profile.Discovered || reset || boot || (len(o.WiFi) == 0 && !o.State.NoWiFi) || hasTask(s, onuID, store.TaskRefresh)
	if needDiscover && o.Root != "" {
		if err := s.discover(ctx, ss, onuID); err != nil {
			s.log.Info("ACS: discovery stopped", "onu", onuID, "err", err.Error())
			if ctx.Err() != nil {
				return
			}
		}
	}
	if o.Root != "" {
		if err := s.provision(ctx, ss, onuID, reset); err != nil && ctx.Err() != nil {
			return
		}
	}
	if v.trust == trustFull {
		if o.Root != "" {
			if err := s.adminLock(ctx, ss, onuID, reset); err != nil && ctx.Err() != nil {
				return
			}
		}
		s.queueHomeWiFi(onuID, reset)
	}
	s.runTasks(ctx, ss, onuID, v.trust, ai.kind == authShared)
}

func (s *Service) loadONU(id string) (o store.ONU, ok bool) {
	s.st.View(func(d *store.Data) { o, ok = d.ONUs[id]; o = store.CloneONU(o) })
	return
}

func hasTask(s *Service, onuID, kind string) bool {
	found := false
	s.st.View(func(d *store.Data) {
		for _, t := range d.ONUTasksOf(onuID) {
			if t.Kind == kind && t.Active() {
				found = true
			}
		}
	})
	return found
}

// ---- reset detection ----

// keyShowsReset: the box reports a ParameterKey other than the ones this ACS wrote (or none), and it has been seen to
// report them before: it forgot what was written, which is what a factory reset does. The 0 BOOTSTRAP event says the same
// for boxes that send it; some send only 1 BOOT after a reset.
func (s *Service) keyShowsReset(o store.ONU, in Inform) bool {
	if o.Root == "" || o.State.LastKey == "" || !o.State.KeyWorks {
		return false
	}
	rep, has := in.Param(o.Root + "ManagementServer.ParameterKey")
	if !has {
		return false
	}
	return rep == "" || (rep != o.State.LastKey && rep != o.State.PrevKey)
}

// withKey: a box whose Inform leaves out the ParameterKey (it should be there; some firmware omits it) is asked for it
// once on a boot, the moment a reset that keeps the login (and sends no 0 BOOTSTRAP) shows. One read; nothing on a
// fault.
func (s *Service) withKey(ctx context.Context, ss *session, o store.ONU, in Inform) Inform {
	name := o.Root + "ManagementServer.ParameterKey"
	if o.Root == "" || o.State.LastKey == "" {
		return in
	}
	if _, has := in.Param(name); has {
		return in
	}
	vals, f, err := ss.gpv(ctx, []string{name})
	if err != nil || f != nil {
		return in
	}
	for _, v := range vals {
		if v.Name == name {
			in.Params = append(append([]ParamValue(nil), in.Params...), v)
		}
	}
	return in
}

// noteKey learns that a box reports the key we wrote (so a later change of it can be trusted as a reset).
func (s *Service) noteKey(onuID string, in Inform) {
	_ = s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok || o.Root == "" || o.State.LastKey == "" || o.State.KeyWorks {
			return store.ErrNoChange
		}
		if rep, has := in.Param(o.Root + "ManagementServer.ParameterKey"); has && rep == o.State.LastKey {
			o.State.KeyWorks = true
			d.ONUs[o.ID] = o
			return nil
		}
		return store.ErrNoChange
	})
}

// spvKeyed is SetParameterValues with a fresh ParameterKey, remembered when the box accepts the change.
func (s *Service) spvKeyed(ctx context.Context, ss *session, onuID string, ps []ParamValue) (int, *Fault, error) {
	key := "ftn-" + randHex(4)
	st, f, err := ss.spv(ctx, ps, key)
	if err != nil || f != nil {
		return st, f, err
	}
	_ = s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		o.State.PrevKey, o.State.LastKey = o.State.LastKey, key
		d.ONUs[o.ID] = o
		return nil
	})
	return st, nil, nil
}

// setLoose writes a list of parameters the box may not all have: a name it refuses (9005, 9008, or a 9003 that names
// it) is dropped and the rest is tried again; a refusal that names nothing is sorted out by writing one at a time.
// It returns the names written and the names refused (with the fault code).
func (s *Service) setLoose(ctx context.Context, ss *session, onuID string, ps []ParamValue) (applied []string, refused map[string]int, err error) {
	refused = map[string]int{}
	left := append([]ParamValue(nil), ps...)
	for try := 0; try < 6 && len(left) > 0; try++ {
		_, f, err := s.spvKeyed(ctx, ss, onuID, left)
		if err != nil {
			return applied, refused, err
		}
		if f == nil {
			for _, p := range left {
				applied = append(applied, p.Name)
			}
			return applied, refused, nil
		}
		named := map[string]int{}
		for _, pf := range f.Params {
			if pf.Name != "" {
				named[pf.Name] = pf.Code
			}
		}
		if len(named) == 0 {
			// Nothing named: one at a time.
			for _, p := range left {
				_, f1, err := s.spvKeyed(ctx, ss, onuID, []ParamValue{p})
				if err != nil {
					return applied, refused, err
				}
				if f1 != nil {
					refused[p.Name] = f1.Code
				} else {
					applied = append(applied, p.Name)
				}
			}
			return applied, refused, nil
		}
		var next []ParamValue
		for _, p := range left {
			if c, bad := named[p.Name]; bad {
				refused[p.Name] = c
			} else {
				next = append(next, p)
			}
		}
		if len(next) == len(left) {
			return applied, refused, nil // it named something that is not ours: give up on the rest
		}
		left = next
	}
	return applied, refused, nil
}

// ---- provisioning: the box's own ACS login, its connection-request login, the periodic Inform ----

func (s *Service) provision(ctx context.Context, ss *session, onuID string, reset bool) error {
	o, ok := s.loadONU(onuID)
	if !ok {
		return nil
	}
	ms := o.Root + "ManagementServer."
	// 1. The box's own ACS login (a refusal flags it as "shared login only").
	if o.Sec.ACSCred == store.CredNone {
		user := "ftn-" + o.ID
		pass := randAlnum(24)
		enc, err := s.sealDev(pass)
		if err != nil {
			return err
		}
		// One Set, never split: a box that took the user name but refused the password would log in with a login
		// that neither side knows, and nothing could reach it until a factory reset.
		_, f, err := s.spvKeyed(ctx, ss, onuID, []ParamValue{{Name: ms + "Username", Value: user}, {Name: ms + "Password", Value: pass}})
		if err != nil {
			return err
		}
		state := store.CredShared
		if f == nil {
			state = store.CredPending
		}
		_ = s.st.Update(func(d *store.Data) error {
			cur, ok := d.ONUs[onuID]
			if !ok {
				return store.ErrNoChange
			}
			cur.Sec.ACSCred, cur.Sec.ACSTries = state, 0
			if state == store.CredPending {
				cur.Sec.ACSUser, cur.Sec.ACSPassEnc = user, enc
			} else {
				cur.Sec.ACSPassEnc = ""
				if cur.Sec.ACSPrevPassEnc == "" {
					cur.Sec.ACSUser = "" // with a previous login kept, its user name stays so the original box is still known
				}
				s.log.Info("ACS: the ONU keeps the shared ACS login", "onu", onuID, "fault", f.Code)
				s.audit(d, cur, "system", "", "ONU "+cur.Serial+" refused its own ACS login: it logs in with the shared login only (weaker)")
			}
			d.ONUs[cur.ID] = cur
			return nil
		})
	}
	// 2. The connection-request login and the periodic Inform.
	o, _ = s.loadONU(onuID)
	crUser, crPass, have := s.openCR(o)
	needCR := !o.State.CRReadOnly && (!have || !o.CRApplied || o.State.CRRefused || reset)
	every := strconv.Itoa(int(s.informEvery() / time.Second))
	var ps []ParamValue
	if needCR {
		if !have {
			crUser, crPass = "cr"+randHex(3), randAlnum(16)
			eu, ep, err := s.sealCR(crUser, crPass)
			if err != nil {
				return err
			}
			if err := s.st.Update(func(d *store.Data) error {
				cur, ok := d.ONUs[onuID]
				if !ok {
					return store.ErrNoChange
				}
				cur.ConnReqUserEnc, cur.ConnReqPassEnc, cur.CRApplied = eu, ep, false
				d.ONUs[onuID] = cur
				return nil
			}); err != nil {
				return err
			}
		}
		ps = append(ps, ParamValue{Name: ms + "ConnectionRequestUsername", Value: crUser}, ParamValue{Name: ms + "ConnectionRequestPassword", Value: crPass})
	}
	if reset || needCR || !o.CRApplied {
		ps = append(ps, ParamValue{Name: ms + "PeriodicInformEnable", Value: "1", Type: "xsd:boolean"},
			ParamValue{Name: ms + "PeriodicInformInterval", Value: every, Type: "xsd:unsignedInt"})
	}
	if len(ps) == 0 {
		return nil
	}
	applied, refused, err := s.setLoose(ctx, ss, onuID, ps)
	if err != nil {
		return err
	}
	wrote := map[string]bool{}
	for _, n := range applied {
		wrote[n] = true
	}
	_, userRefused := refused[ms+"ConnectionRequestUsername"]
	_, passRefused := refused[ms+"ConnectionRequestPassword"]
	return s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		switch {
		case wrote[ms+"ConnectionRequestUsername"] && wrote[ms+"ConnectionRequestPassword"]:
			cur.CRApplied, cur.State.CRRefused, cur.State.CRReadOnly = true, false, false
		case needCR && (userRefused || passRefused):
			// A non-writable login: the box keeps what it has and the ACS goes on with that (no retry at every Inform).
			cur.State.CRReadOnly, cur.State.CRRefused = true, false
			cur.CRApplied = true
			s.log.Info("ACS: the ONU does not take a connection-request login", "onu", onuID)
		}
		d.ONUs[onuID] = cur
		return nil
	})
}

// ---- the ONU's own web admin ----

// adminCandidates are the places a web-admin password may live. None is bench-verified: the ACS tries them in order and
// remembers when none takes (the box is then marked "unsupported" and nothing is claimed).
func adminCandidates(root string) []struct{ parent, leaf string } {
	if root == store.RootTR181 {
		return []struct{ parent, leaf string }{{"Device.Users.User.", "Password"}}
	}
	return []struct{ parent, leaf string }{
		{root + "UserInterface.X_HW_WebUserInfo.", "Password"}, // Huawei spelling (BENCH-CHECK)
		{root + "User.", "Password"},                           // TR-098 Amendment 2 user table
	}
}

// adminLock sets a random password on the ONU's own web admin, best effort, so a tenant cannot open it (and, from
// there, bridge the management VLAN to a LAN port). It cannot read the password back: the state stays "unverified".
func (s *Service) adminLock(ctx context.Context, ss *session, onuID string, reset bool) error {
	o, ok := s.loadONU(onuID)
	if !ok {
		return nil
	}
	if o.Sec.AdminPass == store.AdminUnsupported && !reset {
		return nil
	}
	if o.Sec.AdminPass == store.AdminUnverified && !reset {
		return nil
	}
	pass := ""
	if o.Sec.AdminPassEnc != "" {
		pass, _ = s.openAdmin(o.Sec.AdminPassEnc)
	}
	if pass == "" {
		pass = randAlnum(16)
	}
	done := false
	for _, c := range adminCandidates(o.Root) {
		insts, err := s.listInst(ctx, ss, c.parent, "")
		if err != nil {
			return err
		}
		if len(insts) == 0 {
			continue
		}
		var ps []ParamValue
		for _, in := range insts {
			ps = append(ps, ParamValue{Name: in + c.leaf, Value: pass})
		}
		applied, _, err := s.setLoose(ctx, ss, onuID, ps)
		if err != nil {
			return err
		}
		if len(applied) > 0 {
			done = true
			break
		}
	}
	enc := ""
	if done {
		var err error
		if enc, err = s.sealAdmin(pass); err != nil {
			return err
		}
	}
	return s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		if done {
			cur.Sec.AdminPassEnc, cur.Sec.AdminPass = enc, store.AdminUnverified
		} else {
			cur.Sec.AdminPass = store.AdminUnsupported
		}
		d.ONUs[onuID] = cur
		return nil
	})
}

// ---- the house's Wi-Fi ----

// queueHomeWiFi queues the house's Wi-Fi for a verified box that is linked to it: when the box has just been reset, and
// when it has not yet received this version of the house's choice (a replacement box on its first verified session).
// A box the house's choice was already applied to is left alone: its tenant may have changed the name by hand.
func (s *Service) queueHomeWiFi(onuID string, reset bool) {
	_ = s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok || o.AccountID == "" || o.State.Returned {
			return store.ErrNoChange
		}
		w := d.Accounts[o.AccountID].HomeWiFi
		if !w.Set {
			return store.ErrNoChange
		}
		have := o.State.AppliedFor == o.AccountID && o.State.AppliedAt.Equal(w.UpdatedAt)
		if have && !reset {
			return store.ErrNoChange
		}
		for _, t := range d.ONUTasksOf(o.ID) {
			if t.Kind == store.TaskWiFi && t.Active() {
				return store.ErrNoChange
			}
		}
		queue(d, o, store.TaskWiFi, "system", s.now())
		if reset {
			s.audit(d, o, "system", "", "ONU "+o.Serial+" started again from factory settings: the house's Wi-Fi name and password are being restored")
		} else {
			s.audit(d, o, "system", "", "ONU "+o.Serial+" is new to this house: the house's Wi-Fi name and password are being set")
		}
		return nil
	})
}

// ---- tasks ----

func (s *Service) nextTask(onuID string, skip map[string]bool) (store.ONUTask, bool) {
	var best store.ONUTask
	found := false
	s.st.View(func(d *store.Data) {
		for _, t := range d.ONUTasks {
			if t.ONUID != onuID || t.Status != store.TaskPending || skip[t.ID] {
				continue
			}
			if !found || t.CreatedAt.Before(best.CreatedAt) || (t.CreatedAt.Equal(best.CreatedAt) && t.ID < best.ID) {
				best, found = t, true
			}
		}
	})
	return best, found
}

func (s *Service) setTask(id string, f func(*store.ONUTask)) {
	_ = s.st.Update(func(d *store.Data) error {
		t, ok := d.ONUTasks[id]
		if !ok {
			return store.ErrNoChange
		}
		f(&t)
		d.ONUTasks[id] = t
		return nil
	})
}

func (s *Service) finish(id, fault string) {
	now := s.now()
	s.setTask(id, func(t *store.ONUTask) {
		t.FinishedAt = now
		if fault == "" {
			t.Status, t.Fault = store.TaskDone, ""
		} else {
			t.Status, t.Fault = store.TaskFailed, fault
		}
	})
}

// finishWiFi ends a Wi-Fi task: accepted by the box (never "done": only the owner confirms that it works), or failed.
func (s *Service) finishWiFi(id string, r wifiResult) {
	now := s.now()
	s.setTask(id, func(t *store.ONUTask) {
		t.FinishedAt = now
		if r.fault != "" {
			t.Status, t.Fault = store.TaskFailed, r.fault
			return
		}
		t.Status, t.Fault, t.AfterReboot, t.PassLeaf = store.TaskAccepted, r.note, r.afterReboot, r.leaf
	})
}

func (s *Service) runTasks(ctx context.Context, ss *session, onuID string, trust trustLevel, shared bool) {
	skipped := map[string]bool{}
	for ctx.Err() == nil {
		t, ok := s.nextTask(onuID, skipped)
		if !ok {
			return
		}
		if t.Kind == store.TaskWiFi && trust != trustFull {
			// The password goes only to a box whose identity was verified in this session (its own login, or the
			// shared login from the address the router leased to its MAC): a spoofed serial gets nothing.
			skipped[t.ID] = true
			s.log.Info("ACS: Wi-Fi change waits: the ONU is not verified yet", "onu", onuID)
			continue
		}
		if t.Kind == store.TaskFactoryReset && shared {
			// The owed reset of a returned box goes to the box itself: on the shared login anyone who knows its serial
			// could take it (and the old house's Wi-Fi would stay on the real box). It waits for the box's own login,
			// unless the box refuses one.
			if o, ok := s.loadONU(onuID); ok && o.Sec.ACSCred != store.CredShared {
				skipped[t.ID] = true
				continue
			}
		}
		now := s.now()
		s.setTask(t.ID, func(x *store.ONUTask) { x.Status, x.StartedAt, x.Attempts = store.TaskRunning, now, x.Attempts+1 })
		var fault string
		var err error
		switch t.Kind {
		case store.TaskWiFi:
			var r wifiResult
			r, err = s.doWiFi(ctx, ss, onuID, t)
			if err == nil {
				s.finishWiFi(t.ID, r)
			}
		case store.TaskReboot:
			fault, err = s.doSimple(ctx, ss, func(id string) []byte { return rebootRPC(ss.ns, id, t.ID) }, "RebootResponse")
		case store.TaskFactoryReset:
			fault, err = s.doSimple(ctx, ss, func(id string) []byte { return factoryResetRPC(ss.ns, id) }, "FactoryResetResponse")
			if err == nil && fault == "" {
				s.forgetKeys(onuID)
			}
		case store.TaskRefresh:
			err = s.refreshSSIDs(ctx, ss, onuID)
		default:
			fault = "unknown task"
		}
		if err != nil {
			if ctx.Err() != nil {
				return // the session died: the task is picked up again at the next Inform
			}
			s.finish(t.ID, "the ONU did not answer properly ("+err.Error()+")")
		} else if t.Kind != store.TaskWiFi {
			s.finish(t.ID, fault)
		}
		if t.Kind == store.TaskReboot || t.Kind == store.TaskFactoryReset {
			return // the ONU restarts: nothing more in this session
		}
	}
}

// forgetKeys: after a factory reset the box has no key; what it reports next is not a surprise.
func (s *Service) forgetKeys(onuID string) {
	_ = s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		o.State.LastKey, o.State.PrevKey, o.State.KeyWorks = "", "", false
		o.Sec.ACSCred = store.CredNone
		o.State.AppliedFor, o.State.AppliedAt = "", time.Time{}
		d.ONUs[onuID] = o
		return nil
	})
}

func (s *Service) doSimple(ctx context.Context, ss *session, build func(id string) []byte, want string) (string, error) {
	f, err := ss.simple(ctx, build, want)
	if err != nil {
		return "", err
	}
	if f != nil {
		return f.SafeText(), nil
	}
	return "", nil
}

// refreshSSIDs reads the Wi-Fi names the ONU has now.
func (s *Service) refreshSSIDs(ctx context.Context, ss *session, onuID string) error {
	o, ok := s.loadONU(onuID)
	if !ok {
		return nil
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	for _, w := range o.WiFi {
		vals, f, err := ss.gpv(ctx, []string{w.Path + prof.SSID, w.Path + prof.Enable})
		if err != nil {
			return err
		}
		if f != nil {
			continue
		}
		ssid, en := valueOf(vals, w.Path+prof.SSID), isOn(valueOf(vals, w.Path+prof.Enable))
		path := w.Path
		_ = s.st.Update(func(d *store.Data) error {
			cur, ok := d.ONUs[onuID]
			if !ok {
				return store.ErrNoChange
			}
			for i := range cur.WiFi {
				if cur.WiFi[i].Path == path {
					cur.WiFi[i].SSID, cur.WiFi[i].Enabled = ssid, en
				}
			}
			cur.State.WiFiChanged = ssidDiffers(cur, homeWiFi(d, cur))
			d.ONUs[onuID] = cur
			return nil
		})
	}
	return nil
}
