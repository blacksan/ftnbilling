package sites

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// Safe fixes apply themselves. On every sweep of a joined v5 router the
// server runs each check fix of kind auto (never confirm, never a fix that
// needs hands on the router) through applyFix: the same allowlisted writer, the
// same re-read before writing, the REST budget of the checks, an audit event
// with the actor "system". One attempt per item per interval; after a failure
// the wait doubles (1 min to 15 min). The checklist says "fixed by the server
// at ..." or why it could not, and keeps a "Try now" button only after a
// failed automatic attempt. A router without the v5 layout is never touched.

const (
	autoBackoffBase = time.Minute
	autoBackoffMax  = 15 * time.Minute
)

func autoBackoff(n int) time.Duration {
	d := autoBackoffBase
	for i := 1; i < n && d < autoBackoffMax; i++ {
		d *= 2
	}
	if d > autoBackoffMax {
		d = autoBackoffMax
	}
	return d
}

// autoState is the memory of the automatic attempts for one check of one building.
type autoState struct {
	fixedAt time.Time
	lastTry time.Time
	lastErr string
	fails   int
	next    time.Time
}

// AutoInfo is what the checklist says about an automatically fixed (or failed) item.
type AutoInfo struct {
	State string     `json:"state"` // fixed | failed | pending
	At    *time.Time `json:"at,omitempty"`
	Error string     `json:"error,omitempty"`
	Text  string     `json:"text"`
}

func eatText(t time.Time) string { return t.In(billing.EAT).Format("2 Jan 15:04") }

// Sweep cadence per router: every 5 minutes while the last pass was clean (nothing written, nothing
// failed), every minute after a change or a failure. The billing sweeper calls SweepSite every minute;
// the reconcile of the flats keeps its own cadence.
const (
	sweepCleanEvery = 5 * time.Minute
	sweepBusyEvery  = time.Minute
)

// SweepSite is the scheduled work of one building: the customer ports, then
// the automatic fixes. A router with automatic fixes paused is left alone.
func (m *Manager) SweepSite(ctx context.Context, siteID string) {
	e := m.cport.entry(siteID)
	now := m.now()
	e.mu.Lock()
	wait := now.Before(e.nextSweep)
	e.mu.Unlock()
	if wait {
		return
	}
	if m.AutoFixPause(siteID).Paused {
		e.mu.Lock()
		e.nextSweep = now.Add(sweepBusyEvery)
		e.mu.Unlock()
		return
	}
	// Look at the router before taking its writer lock: a router that does not answer must not make every
	// save, fix and port change wait behind the sweep's reads (the cached look is reused by the passes below).
	if c, err := m.Checks(ctx, siteID, false); err != nil || !c.Router.Reachable {
		e.mu.Lock()
		e.nextSweep = now.Add(sweepBusyEvery)
		e.mu.Unlock()
		return
	}
	_, busy := m.ensureCustomerPort(ctx, siteID, false)
	out := m.AutoMaintain(ctx, siteID)
	if len(out) > 0 {
		busy = true
	}
	next := sweepCleanEvery
	if busy {
		next = sweepBusyEvery
	}
	e.mu.Lock()
	e.nextSweep = now.Add(next)
	e.mu.Unlock()
}

// ---- pausing the automatic fixes ------------------------------------------------------------------

// PauseState says whether the automatic fixes of a router are paused, and until when (zero Until =
// until somebody resumes them).
type PauseState struct {
	Paused bool       `json:"paused"`
	Until  *time.Time `json:"until,omitempty"`
}

func pauseKey(siteID string) string { return "site." + siteID + ".autofix_pause" }

// AutoFixPause reports the router's pause. A pause with an end time lifts itself.
func (m *Manager) AutoFixPause(siteID string) PauseState {
	var v string
	m.st.View(func(d *store.Data) { v = d.Settings[pauseKey(siteID)].Value })
	switch v {
	case "":
		return PauseState{}
	case "forever":
		return PauseState{Paused: true}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil || !m.now().Before(t) {
		return PauseState{}
	}
	return PauseState{Paused: true, Until: &t}
}

// ErrPauseInPast: the end time of a pause is not in the future.
var ErrPauseInPast = errors.New("the end time of the pause must be in the future")

// SetAutoFixPause pauses (until the time, or until resumed when until is zero) or resumes the
// automatic fixes of a router. Only an owner may; the web and API layers check the role. The change
// is recorded in the router's activity with the actor.
func (m *Manager) SetAutoFixPause(siteID string, paused bool, until time.Time, actor string) error {
	if paused && !until.IsZero() && !until.After(m.now()) {
		return ErrPauseInPast
	}
	now := m.now()
	return m.st.Update(func(d *store.Data) error {
		site, ok := d.Sites[siteID]
		if !ok || site.Removed {
			return billing.ErrNoSite
		}
		k := pauseKey(siteID)
		var msg string
		switch {
		case !paused:
			if _, had := d.Settings[k]; !had {
				return store.ErrNoChange
			}
			delete(d.Settings, k)
			msg = "automatic fixes resumed"
		case until.IsZero():
			d.Settings[k] = store.Setting{Key: k, Value: "forever", UpdatedAt: now}
			msg = "automatic fixes paused until they are resumed"
		default:
			d.Settings[k] = store.Setting{Key: k, Value: until.UTC().Format(time.RFC3339), UpdatedAt: now}
			msg = "automatic fixes paused until " + eatText(until)
		}
		d.AddEvent(store.Event{Time: now, SiteID: siteID, Kind: "audit", Actor: actor, Msg: msg})
		return nil
	})
}

// AutoMaintain runs the automatic fixes that are due for a building and
// reports what it did.
func (m *Manager) AutoMaintain(ctx context.Context, siteID string) []AutoFixOutcome {
	e := m.cport.entry(siteID)
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || !site.Runnable() || m.AutoFixPause(siteID).Paused {
		return nil
	}
	// The cached checks (at most a minute old), read BEFORE the writer lock is taken: a router that does not
	// answer must not hold the lock for the length of the read.
	c, err := m.Checks(ctx, siteID, false)
	if err != nil || !c.Router.Reachable || c.Layout != LayoutV5 {
		return nil
	}
	e.run.Lock()
	defer e.run.Unlock()
	var out []AutoFixOutcome
	for _, it := range c.Items {
		if it.Status != StatusWarn && it.Status != StatusFail {
			continue
		}
		if m.AutoFixPause(siteID).Paused {
			break // paused while this pass runs: no more fixes
		}
		if _, has := fixers[it.ID]; !has || it.Fix.Kind != FixAuto || it.Fix.Physical {
			continue
		}
		now := m.now()
		e.mu.Lock()
		st := e.auto[it.ID]
		if st == nil {
			st = &autoState{}
			e.auto[it.ID] = st
		}
		wait := now.Before(st.next)
		e.mu.Unlock()
		if wait {
			continue
		}
		res, err := m.applyFix(ctx, siteID, it.ID, false, ActorSystem, false)
		o := AutoFixOutcome{ID: it.ID, Applied: res.Applied}
		msg := ""
		switch {
		case err != nil:
			msg = writeErrMsg(err)
		case res.Item.Status != StatusOK && res.Item.Status != StatusInfo:
			msg = "it is still not fixed after the change: " + res.Item.Detail
		}
		o.Err = msg
		e.mu.Lock()
		st.lastTry = now
		if msg == "" {
			st.fixedAt, st.lastErr, st.fails, st.next = now, "", 0, time.Time{}
		} else {
			st.lastErr = msg
			st.fails++
			st.next = now.Add(autoBackoff(st.fails))
		}
		e.mu.Unlock()
		out = append(out, o)
	}
	return out
}

// decorate gives the checklist its final form: no blank evidence lines, and
// for the items the server fixes by itself, what it did or why it could not.
// The cached checks are never changed.
func (m *Manager) decorate(siteID string, c Checks) Checks {
	items := make([]CheckItem, len(c.Items))
	copy(items, c.Items)
	var site store.Site
	m.st.View(func(d *store.Data) { site = d.Sites[siteID] })
	auto := c.Layout == LayoutV5 && site.Runnable()
	pause := m.AutoFixPause(siteID)
	c.AutoFixes = &pause
	e := m.cport.entry(siteID)
	for i := range items {
		it := &items[i]
		ev := make([]string, 0, len(it.Evidence))
		for _, l := range it.Evidence {
			if l = strings.TrimSpace(l); l != "" {
				ev = append(ev, l)
			}
		}
		it.Evidence = ev
		if !auto || it.Fix.Kind != FixAuto || it.Fix.Physical {
			continue
		}
		if _, has := fixers[it.ID]; !has {
			continue
		}
		if pause.Paused {
			// the button is back: nothing is applied by itself while the router is paused
			it.Auto = &AutoInfo{State: "paused", Text: "Automatic fixes are paused for this router."}
			continue
		}
		it.Fix.Automatic = true
		e.mu.Lock()
		var st autoState
		if p := e.auto[it.ID]; p != nil {
			st = *p
		}
		e.mu.Unlock()
		open := it.Status == StatusWarn || it.Status == StatusFail
		switch {
		case open && st.lastErr != "":
			at := st.lastTry
			it.Fix.TryNow = true
			it.Auto = &AutoInfo{State: "failed", At: &at, Error: st.lastErr,
				Text: fmt.Sprintf("The server tried at %s and could not fix it: %s. It will try again at %s.", eatText(at), st.lastErr, eatText(st.next))}
		case open:
			it.Auto = &AutoInfo{State: "pending", Text: "The server fixes this by itself." + keepsOn(it.ID)}
		}
	}
	for i := range items {
		it := &items[i]
		e.mu.Lock()
		var st autoState
		if p := e.auto[it.ID]; p != nil {
			st = *p
		}
		e.mu.Unlock()
		if it.Auto == nil && !st.fixedAt.IsZero() && (it.Status == StatusOK || it.Status == StatusInfo) {
			at := st.fixedAt
			it.Auto = &AutoInfo{State: "fixed", At: &at, Text: "Fixed by the server at " + eatText(at) + "." + keepsOn(it.ID)}
		}
		if it.Auto == nil && auto && !pause.Paused && keepsOn(it.ID) != "" && (it.Status == StatusOK || it.Status == StatusInfo) {
			it.Auto = &AutoInfo{State: "keeps", Text: strings.TrimSpace(keepsOn(it.ID))}
		}
	}
	c.Items = items
	return c
}

// keepsOn is the sentence for the items whose fix puts back what an operator may have changed on
// purpose (customer DNS, the customer DHCP server, traffic reporting): the server keeps them on until
// the router's automatic fixes are paused.
func keepsOn(id string) string {
	switch id {
	case CheckDNS, CheckDHCP, CheckTrafficFlow:
		return " The server keeps this on."
	}
	return ""
}
