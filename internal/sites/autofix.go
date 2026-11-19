package sites

import (
	"context"
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

// SweepSite is the scheduled work of one building: the customer ports, then
// the automatic fixes. The billing sweeper calls it once per interval.
func (m *Manager) SweepSite(ctx context.Context, siteID string) {
	m.EnsureCustomerPort(ctx, siteID, false)
	m.AutoMaintain(ctx, siteID)
}

// AutoMaintain runs the automatic fixes that are due for a building and
// reports what it did.
func (m *Manager) AutoMaintain(ctx context.Context, siteID string) []AutoFixOutcome {
	e := m.cport.entry(siteID)
	e.run.Lock()
	defer e.run.Unlock()
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || !site.Runnable() {
		return nil
	}
	c, err := m.Checks(ctx, siteID, false)
	if err != nil || !c.Router.Reachable || c.Layout != LayoutV5 {
		return nil
	}
	var out []AutoFixOutcome
	for _, it := range c.Items {
		if it.Status != StatusWarn && it.Status != StatusFail {
			continue
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
			it.Auto = &AutoInfo{State: "pending", Text: "The server fixes this by itself."}
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
			it.Auto = &AutoInfo{State: "fixed", At: &at, Text: "Fixed by the server at " + eatText(at)}
		}
	}
	c.Items = items
	return c
}
