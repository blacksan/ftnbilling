package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Old DHCP leases and the evidence window (release plan A1, review B2).
//
// A client with a lease of length T asks its server again at T/2. When the old
// server is switched off it gets no answer and starts over at 7T/8 or T, so
// customers can take most of a lease time to show up on ftn-dhcp. Before the
// switch the job therefore shortens the lease time of the old DHCP servers on
// the customer interface, waits until every lease that was running has been
// renewed (so every client holds a short lease), and sizes the evidence window
// from the longest original lease. The original lease times are kept in a
// setting and put back when the job is live, failed or rolled back (the kit has
// no undo row for them: a 1 minute lease on a re-enabled old server is only
// chatty, never wrong).

const (
	leaseShortDefault = "1m"
	leaseGraceDefault = time.Minute
	leaseCapDefault   = 65 * time.Minute
	// the kit gives the switch 15 minutes: the window never goes past 12 of them
	evidenceCap = 12 * time.Minute
)

func (t ProvTiming) leaseShort() string {
	if t.LeaseShort == "" {
		return leaseShortDefault
	}
	return t.LeaseShort
}
func (t ProvTiming) leaseGrace() time.Duration {
	if t.LeaseGrace <= 0 {
		return leaseGraceDefault
	}
	return t.LeaseGrace
}
func (t ProvTiming) leaseSweep() time.Duration {
	if t.LeaseSweep <= 0 {
		return time.Minute
	}
	return t.LeaseSweep
}
func (t ProvTiming) countdown() time.Duration {
	if t.Countdown <= 0 {
		return KitArmTicks * 30 * time.Second
	}
	return t.Countdown
}
func (t ProvTiming) leaseCap() time.Duration {
	if t.LeaseCap <= 0 {
		return leaseCapDefault
	}
	return t.LeaseCap
}

// evidenceWindow is how long the job waits for the flats to come back: at least
// the configured window, and half the longest old lease plus two graces, but never
// more than the kit's countdown allows.
func (t ProvTiming) evidenceWindow(longest time.Duration) time.Duration {
	w := longest/2 + 2*t.leaseGrace()
	if w < t.Evidence {
		w = t.Evidence
	}
	if w > evidenceCap && t.Evidence <= evidenceCap {
		w = evidenceCap
	}
	return w
}

// parseROSDuration reads a RouterOS time: 1w2d3h4m5s, 10m, 30s, 00:10:00, 1d00:00:00 or plain seconds.
func parseROSDuration(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if strings.Contains(v, ":") {
		var d time.Duration
		if i := strings.IndexAny(v, "wd"); i >= 0 {
			pre, ok := parseROSDuration(v[:i+1])
			if !ok {
				return 0, false
			}
			d += pre
			v = v[i+1:]
		}
		parts := strings.Split(v, ":")
		if len(parts) != 3 {
			return 0, false
		}
		var n [3]float64
		for i, p := range parts {
			f, err := strconv.ParseFloat(p, 64)
			if err != nil || f < 0 {
				return 0, false
			}
			n[i] = f
		}
		return d + time.Duration((n[0]*3600+n[1]*60+n[2])*float64(time.Second)), true
	}
	var total time.Duration
	num := ""
	seen := false
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9' || c == '.':
			num += string(c)
		default:
			unit, ok := map[rune]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}[c]
			if !ok || num == "" {
				return 0, false
			}
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			total += time.Duration(f * float64(unit))
			num, seen = "", true
		}
	}
	if num != "" {
		if seen {
			return 0, false
		}
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, false
		}
		total = time.Duration(f * float64(time.Second))
	}
	return total, true
}

func leaseKey(siteID string) string { return "site." + siteID + ".prov_lease" }

// leaseRec is what is kept of an old DHCP server before its lease time is shortened.
type leaseRec struct {
	Lease string `json:"lease"`
	Iface string `json:"iface"` // the customer interface it served: the writer's scope when the time is put back
}

func (m *Manager) loadLeaseOrig(siteID string) map[string]leaseRec {
	var raw string
	m.st.View(func(d *store.Data) { raw = d.Settings[leaseKey(siteID)].Value })
	out := map[string]leaseRec{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (m *Manager) saveLeaseOrig(siteID string, v map[string]leaseRec) {
	raw := ""
	if len(v) > 0 {
		b, _ := json.Marshal(v)
		raw = string(b)
	}
	_ = m.st.Update(func(d *store.Data) error {
		k := leaseKey(siteID)
		if d.Settings[k].Value == raw {
			return store.ErrNoChange
		}
		d.Settings[k] = store.Setting{Key: k, Value: raw, UpdatedAt: m.now()}
		return nil
	})
}

// stepLeases shortens the old DHCP servers' lease time and waits for the running leases to be renewed.
func (j *jobCtl) stepLeases() (string, error) {
	_, dh, err := j.existingServers()
	if err != nil {
		return "", routerErr(err, "reading the old DHCP servers")
	}
	if len(dh) == 0 {
		return "no old DHCP server is running on the customer network", nil
	}
	short := j.tm.leaseShort()
	shortD, _ := parseROSDuration(short)
	orig := j.m.loadLeaseOrig(j.siteID)
	recs := map[string]leaseRec{}
	for k, v := range orig {
		recs[k] = v
	}
	names := map[string]bool{}
	var longest time.Duration
	for _, r := range dh {
		names[r["name"]] = true
		cur := r["lease-time"]
		if _, kept := recs[r["name"]]; !kept && cur != short {
			if cur == "" {
				cur = "10m" // the RouterOS default
			}
			recs[r["name"]] = leaseRec{Lease: cur, Iface: r["interface"]}
		}
		if d, ok := parseROSDuration(recs[r["name"]].Lease); ok && d > longest {
			longest = d
		}
	}
	// a lease longer than the job is willing to wait is the operator's to shorten: nothing is changed yet
	if longest > j.tm.leaseCap() {
		return "", fail(JobNeedsAction, "the old DHCP lease time is %s, longer than the %s this set-up can wait for. Shorten the old DHCP lease time to 60 minutes or less, wait for it to take effect, then run the set-up again",
			longest.Round(time.Second), j.tm.leaseCap().Round(time.Second))
	}
	j.m.saveLeaseOrig(j.siteID, recs)
	j.job.LongestLeaseSec = int(longest.Seconds())
	j.save()
	// what is running now: the longest time any bound lease has left
	var remaining time.Duration
	leases, err := j.rows("/ip/dhcp-server/lease")
	if err != nil {
		return "", routerErr(err, "reading the leases")
	}
	for _, l := range leases {
		if !names[l["server"]] || !boundLease(l) {
			continue
		}
		if d, ok := parseROSDuration(l["expires-after"]); ok && d > remaining {
			remaining = d
		}
	}
	for _, r := range dh {
		if d, ok := parseROSDuration(r["lease-time"]); ok && d <= shortD && r["lease-time"] != "" {
			continue
		}
		if err := j.w.set(j.ctx, "/ip/dhcp-server", r, map[string]string{"lease-time": short}); err != nil {
			return "", routerErr(err, "shortening the lease time of "+r["name"])
		}
	}
	if remaining <= 0 {
		return fmt.Sprintf("lease time of %d old server(s) set to %s; no lease is running", len(dh), short), nil
	}
	wait := remaining + j.tm.leaseGrace()
	if cap := longest + j.tm.leaseGrace(); wait > cap {
		wait = cap
	}
	if wait > j.tm.leaseCap() {
		wait = j.tm.leaseCap()
	}
	end := time.Now().Add(wait)
	for {
		left := time.Until(end)
		if left <= 0 {
			break
		}
		j.step(StepLeases, StepRunning, fmt.Sprintf("waiting for the running leases to renew: %s left", left.Round(time.Second)))
		pause := j.tm.Poll
		if pause > left {
			pause = left
		}
		select {
		case <-j.ctx.Done():
			return "", j.ctx.Err()
		case <-time.After(pause):
		}
	}
	return fmt.Sprintf("lease time of %d old server(s) set to %s, waited %s for the running leases to renew", len(dh), short, wait.Round(time.Second)), nil
}

// restoreLeases puts the old servers' lease times back. The writer's scope is taken from what was saved
// (the interface each server served), not from the job: a rollback request or the startup sweep has no
// customer interface of its own. The setting stays until every server is back.
func (j *jobCtl) restoreLeases(ctx context.Context) bool {
	orig := j.m.loadLeaseOrig(j.siteID)
	if len(orig) == 0 {
		return true
	}
	if j.w == nil {
		j.newWriter()
	}
	rows, err := j.rt.Rows(ctx, "/ip/dhcp-server", nil)
	if err != nil {
		return false
	}
	ok := true
	for _, r := range rows {
		rec, has := orig[r["name"]]
		if !has || r["lease-time"] == rec.Lease {
			continue
		}
		if rec.Iface != "" && r["interface"] != rec.Iface {
			continue // not the server we shortened any more
		}
		w := *j.w
		w.sc.iface = r["interface"]
		if err := w.set(ctx, "/ip/dhcp-server", r, map[string]string{"lease-time": rec.Lease}); err != nil {
			ok = false
			j.log("could not put the lease time of %s back to %s: %v", r["name"], rec.Lease, err)
		}
	}
	if ok {
		j.m.saveLeaseOrig(j.siteID, nil)
	}
	return ok
}

// ---- the sweep ----------------------------------------------------------------------------------

// leaseSweeper puts lease times back when no job is around to do it (a rollback request, a restart,
// a shutdown, an unreachable router, a rollback that took too long to be seen). It retries with a backoff.
func (m *Manager) leaseSweeper(ctx context.Context) {
	defer m.wg.Done()
	for {
		m.SweepLeases(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.timing().leaseSweep()):
		}
	}
}

// SweepLeases runs one pass: every building with saved lease times and no job running gets them restored.
func (m *Manager) SweepLeases(ctx context.Context) {
	var ids []string
	m.st.View(func(d *store.Data) {
		for id, s := range d.Sites {
			if !s.Removed && d.Settings[leaseKey(id)].Value != "" {
				ids = append(ids, id)
			}
		}
	})
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		m.sweepLeases(ctx, id)
	}
}

func (m *Manager) sweepLeases(ctx context.Context, id string) {
	m.prov.mu.Lock()
	if m.prov.runs[id] != nil || time.Now().Before(m.prov.leaseNext[id]) {
		m.prov.mu.Unlock()
		return
	}
	m.prov.mu.Unlock()
	var site store.Site
	m.st.View(func(d *store.Data) { site = d.Sites[id] })
	rt := m.Router(id)
	j := &jobCtl{m: m, ctx: ctx, siteID: id, o: ProvOptions{Trigger: "lease-sweep", Actor: ActorSystem}, tm: m.timing(), rt: rt, release: func() {}}
	j.site = m.Effective(site)
	j.insp, _ = rt.(routeros.Inspector)
	j.plan, _ = PlanSubnet(j.site.CustomerSubnet)
	j.newWriter()
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ok := j.restoreLeases(c)
	m.prov.mu.Lock()
	if m.prov.leaseNext == nil {
		m.prov.leaseNext, m.prov.leaseTry = map[string]time.Time{}, map[string]int{}
	}
	if ok {
		delete(m.prov.leaseNext, id)
		delete(m.prov.leaseTry, id)
	} else {
		n := m.prov.leaseTry[id] + 1
		m.prov.leaseTry[id] = n
		back := 30 * time.Second << min(n, 5)
		m.prov.leaseNext[id] = time.Now().Add(back)
	}
	m.prov.mu.Unlock()
	if ok {
		_ = m.st.Update(func(d *store.Data) error {
			d.AddEvent(store.Event{Time: m.now(), SiteID: id, Kind: "audit", Msg: "router setup: the old DHCP lease times were put back"})
			return nil
		})
	}
}
