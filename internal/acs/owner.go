package acs

import (
	"errors"
	"time"

	"ispbill/internal/store"
)

// Owner decisions about a box: confirm that a Wi-Fi change works, approve or block a quarantined check-in, cancel a
// queued task, show the admin password. The web screens and the API call these; the role and step-up checks are theirs.

var (
	ErrNothingToConfirm = errors.New("there is no Wi-Fi change waiting for a confirmation on this house's ONU")
	ErrNotQuarantined   = errors.New("that ONU is not waiting for an approval")
	ErrNoAdminPassword  = errors.New("no admin password was set on this ONU")
	ErrTaskGone         = errors.New("that task is not queued any more")
	ErrTwoPlaces        = errors.New("check-ins for this ONU came from more than one place while it was held back, so one of them is not the real box: block the place shown (the latest) if it is not the box, or wait for its next check-in, then approve")
)

// newestWiFiTask is the newest Wi-Fi task of an ONU.
func newestWiFiTask(d *store.Data, onuID string) (store.ONUTask, bool) {
	for _, t := range d.ONUTasksOf(onuID) {
		if t.Kind == store.TaskWiFi {
			return t, true
		}
	}
	return store.ONUTask{}, false
}

// ConfirmWiFi: the owner saw a phone join the new network. The task becomes Confirmed and the passphrase name that
// took is remembered for this box and for its model (OUI, product class, firmware): it is tried first next time.
func (s *Service) ConfirmWiFi(by, ip, accountID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUOfAccount(accountID)
		if !ok {
			return ErrNoONU
		}
		t, ok := newestWiFiTask(d, o.ID)
		if !ok || t.Status != store.TaskAccepted {
			return ErrNothingToConfirm
		}
		now := s.now()
		t.Status, t.ConfirmedAt, t.ConfirmedBy = store.TaskConfirmed, now, by
		d.ONUTasks[t.ID] = t
		if t.PassLeaf != "" {
			o.Profile.PassLeaf = t.PassLeaf
			var bad []string
			for _, b := range o.Profile.BadLeaves {
				if b != t.PassLeaf {
					bad = append(bad, b)
				}
			}
			o.Profile.BadLeaves = bad
			d.ONUs[o.ID] = o
			works := readWorks(d)
			k := modelKey(o.OUI, o.ProductClass, o.SWVersion)
			w := works[k]
			if w.PassLeaf != t.PassLeaf {
				w.Count = 0 // another name: the confirmations of the old one do not carry over
			}
			w.OUI, w.Class, w.Firmware, w.Profile, w.PassLeaf = o.OUI, o.ProductClass, o.SWVersion, pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass).Name, t.PassLeaf
			w.Count++
			w.Confirmed, w.By = now, by
			works[k] = w
			writeWorks(d, works, now)
		}
		s.audit(d, o, by, ip, "Wi-Fi change confirmed working on the ONU (a phone joined the new network)")
		return nil
	})
}

// RejectWiFi: the owner says the box accepted the change but the network did not change. The passphrase name it took
// is not trusted for this box any more, and the change is queued again (it tries the next name).
func (s *Service) RejectWiFi(by, ip, accountID string) (store.ONUTask, error) {
	var t store.ONUTask
	var onuID string
	err := s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUOfAccount(accountID)
		if !ok {
			return ErrNoONU
		}
		old, ok := newestWiFiTask(d, o.ID)
		if !ok || (old.Status != store.TaskAccepted && old.Status != store.TaskConfirmed) {
			return ErrNothingToConfirm
		}
		old.Status, old.Fault = store.TaskFailed, "the owner reports that the Wi-Fi did not change on the ONU"
		d.ONUTasks[old.ID] = old
		if old.PassLeaf != "" {
			if o.Profile.PassLeaf == old.PassLeaf {
				o.Profile.PassLeaf = ""
			}
			if !contains(o.Profile.BadLeaves, old.PassLeaf) {
				o.Profile.BadLeaves = append(o.Profile.BadLeaves, old.PassLeaf)
			}
			d.ONUs[o.ID] = o
			unlearnWork(d, o, old.PassLeaf, s.now())
		}
		o = d.ONUs[o.ID]
		t = queue(d, o, store.TaskWiFi, by, s.now())
		onuID = o.ID
		s.audit(d, o, by, ip, "the Wi-Fi change did not take on the ONU (owner); trying another way")
		return nil
	})
	if err != nil {
		return store.ONUTask{}, err
	}
	s.kickAsync(onuID)
	return t, nil
}

// CancelTask cancels a queued task (for instance the factory reset of a returned box).
func (s *Service) CancelTask(by, ip, taskID string) error {
	return s.st.Update(func(d *store.Data) error {
		t, ok := d.ONUTasks[taskID]
		if !ok || t.Status != store.TaskPending {
			return ErrTaskGone
		}
		t.Status, t.Fault, t.FinishedAt = store.TaskFailed, "cancelled by "+by, s.now()
		d.ONUTasks[t.ID] = t
		if o, ok := d.ONUs[t.ONUID]; ok {
			s.audit(d, o, by, ip, "ONU task cancelled: "+t.Kind)
		}
		return nil
	})
}

// ApproveONU accepts the place a quarantined check-in came from: the ONU is bound to that router, address and MAC, and
// gets its own ACS login again.
func (s *Service) ApproveONU(by, ip, onuID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return ErrONUGone
		}
		q := o.Sec.Quarantine
		if !q.Active {
			return ErrNotQuarantined
		}
		if q.Places > 1 {
			return ErrTwoPlaces
		}
		if q.SiteID != "" && o.AccountID != "" && d.Accounts[o.AccountID].SiteID != q.SiteID {
			return ErrSite
		}
		if q.SiteID != "" {
			o.SiteID = q.SiteID
		}
		o.MgmtIP, o.Sec.MgmtMAC = q.IP, q.MAC
		o.Sec.Quarantine = store.ONUQuarantine{}
		o.Sec.FlapCount, o.Sec.FlapSince = 0, time.Time{}
		// A fresh own login is written at the next verified session. The current one is kept until then: a box that
		// was held back while it used its own login (an address the lease snapshot still gave to another MAC) still
		// logs in with it, and dropping it would lock that box out for good. Whoever else may know it is still bound
		// by the place check, and the rewrite ends it.
		o.Sec.ACSCred, o.Sec.ACSTries = store.CredNone, 0
		d.ONUs[o.ID] = o
		s.audit(d, o, by, ip, "ONU "+o.Serial+": the check-in from "+q.IP+" was approved; the ONU is bound to its new place")
		return nil
	})
}

// BlockONU refuses the place a quarantined check-in came from: the serial is turned away from that MAC (or address
// when the router could not name the MAC), and the alert is closed.
func (s *Service) BlockONU(by, ip, onuID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return ErrONUGone
		}
		q := o.Sec.Quarantine
		if !q.Active {
			return ErrNotQuarantined
		}
		add := func(k string) {
			if !contains(o.Sec.BlockedFrom, k) {
				o.Sec.BlockedFrom = append(o.Sec.BlockedFrom, k)
			}
		}
		if q.MAC != "" {
			add("mac:" + q.MAC)
		}
		add("ip:" + q.SiteID + "/" + q.IP)
		o.Sec.Quarantine = store.ONUQuarantine{}
		d.ONUs[o.ID] = o
		s.audit(d, o, by, ip, "ONU "+o.Serial+": the check-in from "+q.IP+" was blocked")
		return nil
	})
}

// UnblockONU lets a serial back in from every place that was blocked for it.
func (s *Service) UnblockONU(by, ip, onuID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return ErrONUGone
		}
		if len(o.Sec.BlockedFrom) == 0 {
			return store.ErrNoChange
		}
		o.Sec.BlockedFrom = nil
		d.ONUs[o.ID] = o
		s.audit(d, o, by, ip, "ONU "+o.Serial+": blocked places were cleared")
		return nil
	})
}

// AdminPassword opens the password this ACS set on the ONU's own web admin. The caller makes the owner step up first.
func (s *Service) AdminPassword(onuID string) (string, error) {
	var enc string
	s.st.View(func(d *store.Data) { enc = d.ONUs[onuID].Sec.AdminPassEnc })
	if enc == "" {
		return "", ErrNoAdminPassword
	}
	p, err := s.openAdmin(enc)
	if err != nil {
		return "", errors.New("the admin password cannot be opened (DATA_KEY changed?)")
	}
	return p, nil
}

// ---- upkeep ----

// expireTasks expires queued tasks that waited longer than their kind allows. It returns how many.
func expireTasks(d *store.Data, now time.Time) int {
	n := 0
	for _, t := range d.ONUTasks {
		if t.Kind == store.TaskFactoryReset {
			if o, ok := d.ONUs[t.ONUID]; ok && o.State.Returned && o.AccountID == "" {
				continue // a returned box keeps its reset until it checks in: the old house's Wi-Fi must not travel with it
			}
		}
		if t.Status == store.TaskPending && now.Sub(t.CreatedAt) > store.TaskTTL(t.Kind) {
			t.Status, t.Fault, t.FinishedAt = store.TaskExpired, "the ONU did not check in in time, so this was not sent", now
			d.ONUTasks[t.ID] = t
			n++
		}
	}
	return n
}

// Upkeep expires old queued tasks and forgets boxes that are not at any house, not in the register and not returned,
// and have not been heard from for a week (unknown or fake records would otherwise pile up and fill a router's quota).
// A box linked to a house is never forgotten.
func (s *Service) Upkeep() {
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		changed := expireTasks(d, now) > 0
		for id, o := range d.ONUs {
			if o.AccountID != "" {
				continue
			}
			// A returned box (its reset is still owed) and a box in the register are kept: forgetting one that holds
			// its own ACS login would lock it out for good (nobody knows that login any more), and a forgotten returned
			// box would never be reset. Fake serials are never in the register, so they still go.
			if o.State.Returned {
				continue
			}
			if _, inReg := d.ONTBySerial(o.Serial); inReg {
				continue
			}
			seen := o.LastInform
			if seen.IsZero() {
				seen = o.FirstSeen
			}
			if !seen.IsZero() && now.Sub(seen) > store.ONUStaleAfter {
				for _, t := range d.ONUTasksOf(id) {
					delete(d.ONUTasks, t.ID)
				}
				delete(d.ONUs, id)
				changed = true
			}
		}
		if !changed {
			return store.ErrNoChange
		}
		return nil
	})
}
