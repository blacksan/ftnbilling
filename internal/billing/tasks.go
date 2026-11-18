package billing

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Router actions are queued in the store, so a payment never depends on the
// router being reachable at that moment: tasks are retried with backoff until
// they succeed and they survive a restart.

const (
	backoffBase = 5 * time.Second
	backoffMax  = 5 * time.Minute
)

func backoff(attempts int) time.Duration {
	d := backoffBase
	for i := 1; i < attempts && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// enqueueLocked adds or refreshes the reconcile task for an account (inside an
// Update). Merging resets the retry timer so a new change is tried at once.
// Tasks merge per (account, building): moving an account queues one task for
// the old router and one for the new.
func (s *Service) enqueueLocked(d *store.Data, a store.Account, kick bool, reason string) {
	now := s.now()
	for id, t := range d.Tasks {
		if t.AccountID == a.ID && t.SiteID == a.SiteID {
			t.Kick = t.Kick || kick
			t.Reason, t.NextTry, t.Attempts, t.Rev = reason, now, 0, t.Rev+1
			d.Tasks[id] = t
			return
		}
	}
	t := store.Task{ID: RandomID(), SiteID: a.SiteID, AccountID: a.ID, Code: a.Code, Kick: kick, Reason: reason, NextTry: now, CreatedAt: now, Rev: 1}
	d.Tasks[t.ID] = t
}

// PendingTasks lists queued router actions (oldest first), all buildings.
func (s *Service) PendingTasks() (out []store.Task) {
	s.st.View(func(d *store.Data) { out = d.TaskList() })
	return
}

func (s *Service) runSiteShort(ctx context.Context, siteID string) {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	s.RunSiteTasks(c, siteID)
}

// RunTasks attempts every due task in every building. Each building with due
// work runs in its own goroutine, so one dead router cannot stall the others.
// It returns how many tasks succeeded.
func (s *Service) RunTasks(ctx context.Context) int {
	now := s.now()
	bySite := map[string]bool{}
	s.st.View(func(d *store.Data) {
		for _, t := range d.Tasks {
			if !t.NextTry.After(now) {
				bySite[t.SiteID] = true
			}
		}
	})
	var wg sync.WaitGroup
	var total atomic.Int64
	for id := range bySite {
		wg.Add(1)
		go func() {
			defer wg.Done()
			total.Add(int64(s.RunSiteTasks(ctx, id)))
		}()
	}
	wg.Wait()
	return int(total.Load())
}

// RunSiteTasks attempts every due task of one building. Tasks of a paused,
// removed or not yet joined building stay queued and use up no attempts.
func (s *Service) RunSiteTasks(ctx context.Context, siteID string) int {
	if st, ok := s.site(siteID); !ok || !st.Runnable() {
		return 0
	}
	state := s.state(siteID)
	state.taskMu.Lock()
	defer state.taskMu.Unlock()
	now := s.now()
	var due []store.Task
	s.st.View(func(d *store.Data) {
		for _, t := range d.TaskList() {
			if t.SiteID == siteID && !t.NextTry.After(now) {
				due = append(due, t)
			}
		}
	})
	ok := 0
	// An IPoE building is reconciled as a whole: one pass over the router
	// answers every due task (a hundred queued tasks after an outage cost one
	// pass, not a hundred).
	ipoe := false
	if st, found := s.site(siteID); found && IPoE(st) {
		ipoe = true
	}
	var batchErr error
	batchDone := false
	for _, t := range due {
		if ctx.Err() != nil {
			break
		}
		var err error
		if ipoe {
			if !batchDone {
				_, batchErr = s.ReconcileSite(ctx, siteID)
				batchDone = true
			}
			err = batchErr
		} else {
			err = s.reconcile(ctx, t)
		}
		_ = s.st.Update(func(d *store.Data) error {
			cur, found := d.Tasks[t.ID]
			if !found {
				return store.ErrNoChange
			}
			if err == nil {
				if cur.Rev == t.Rev {
					delete(d.Tasks, t.ID)
				} else { // changed while we worked: run again at once
					cur.NextTry = now
					d.Tasks[t.ID] = cur
				}
				return nil
			}
			cur.Attempts++
			cur.LastErr = err.Error()
			cur.NextTry = s.now().Add(backoff(cur.Attempts))
			d.Tasks[t.ID] = cur
			return nil
		})
		if err == nil {
			ok++
		} else {
			s.log.Warn("router task failed; will retry", "site", siteID, "code", t.Code, "attempt", t.Attempts+1, "err", err)
		}
	}
	return ok
}

// reconcile makes the task's building router match the account's current state.
// If the account is gone, or now lives in another building, the router user is
// removed from this building's router.
func (s *Service) reconcile(ctx context.Context, t store.Task) error {
	if st, ok := s.site(t.SiteID); ok && IPoE(st) {
		_, err := s.ReconcileSite(ctx, t.SiteID) // IPoE: the table diff covers the task
		return err
	}
	var a store.Account
	var pkg store.Package
	var found bool
	s.st.View(func(d *store.Data) {
		a, found = d.Accounts[t.AccountID]
		pkg = d.Packages[a.PackageID]
	})
	rt := s.router(t.SiteID)
	if !found || a.SiteID != t.SiteID {
		if err := rt.KickUser(ctx, t.Code); err != nil {
			return err
		}
		return rt.RemoveUser(ctx, t.Code)
	}
	want := wantUser(a, s.now())
	if want {
		if !s.state(t.SiteID).profilesOK.Load() {
			if err := s.SyncProfiles(ctx, t.SiteID); err != nil {
				return fmt.Errorf("profile sync: %w", err)
			}
		}
		if err := rt.EnsureUser(ctx, routeros.User{
			Name: a.Code, Password: a.HSPass, Profile: pkg.Profile(), MAC: a.MAC, Comment: "ispbill:" + a.Flat,
		}); err != nil {
			return err
		}
		if t.Kick {
			if err := rt.KickUser(ctx, a.Code); err != nil {
				return err
			}
		}
	} else {
		if err := rt.KickUser(ctx, a.Code); err != nil {
			return err
		}
		if err := rt.RemoveUser(ctx, a.Code); err != nil {
			return err
		}
	}
	return s.st.Update(func(d *store.Data) error {
		cur, ok := d.Accounts[a.ID]
		if !ok || cur.RouterActive == want {
			return store.ErrNoChange
		}
		cur.RouterActive = want
		d.Accounts[a.ID] = cur
		return nil
	})
}

// ---- sweeper ------------------------------------------------------------

// Sweep records state changes made by the clock (a plan that just expired,
// reminder events), then, per building and in parallel, re-syncs profiles
// after a failure and runs the task queue.
func (s *Service) Sweep(ctx context.Context) {
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		changed := false
		queued := map[string]bool{}
		for _, t := range d.Tasks {
			queued[t.SiteID+"|"+t.AccountID] = true
		}
		remind := time.Duration(s.policy.ReminderDays) * 24 * time.Hour
		for _, a := range d.AccountList() {
			// IPoE: the router holds no per-user session; the per-site diff
			// below turns a plan that just ended into a suspended-list entry.
			if !IPoE(d.Sites[a.SiteID]) && wantUser(a, now) != a.RouterActive && !queued[a.SiteID+"|"+a.ID] {
				s.enqueueLocked(d, a, false, "state changed by clock")
				changed = true
			}
			if a.Suspended || a.ExpiresAt.IsZero() {
				continue
			}
			pkg := d.Packages[a.PackageID]
			long := time.Duration(pkg.DurationMin)*time.Minute > remind
			if s.policy.ReminderDays > 0 && long && a.ExpiresAt.After(now) && a.ExpiresAt.Sub(now) <= remind && !a.RemindedFor.Equal(a.ExpiresAt) {
				d.AddEvent(store.Event{Time: now, SiteID: a.SiteID, Kind: "reminder_due", AccountID: a.ID,
					Msg: fmt.Sprintf("%s (%s): plan ends %s, send the %d-day reminder", a.Flat, a.Code, a.ExpiresAt.In(EAT).Format("2 Jan 15:04"), s.policy.ReminderDays)})
				a.RemindedFor = a.ExpiresAt
				d.Accounts[a.ID] = a
				changed = true
			}
			if s.policy.ReminderOnExpiry && !a.ExpiresAt.After(now) && !a.ExpiredFor.Equal(a.ExpiresAt) {
				d.AddEvent(store.Event{Time: now, SiteID: a.SiteID, Kind: "expired", AccountID: a.ID,
					Msg: fmt.Sprintf("%s (%s): plan expired %s, send the expiry reminder", a.Flat, a.Code, a.ExpiresAt.In(EAT).Format("2 Jan 15:04"))})
				a.ExpiredFor = a.ExpiresAt
				d.Accounts[a.ID] = a
				changed = true
			}
		}
		if !changed {
			return store.ErrNoChange
		}
		return nil
	})
	var wg sync.WaitGroup
	for _, id := range s.runnableSites() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st, ok := s.site(id); ok && IPoE(st) {
				// Full per-site diff every sweep; due tasks ride on the same pass.
				if s.hasDueTasks(id) {
					s.RunSiteTasks(ctx, id)
				} else if _, err := s.ReconcileSite(ctx, id); err != nil {
					s.log.Warn("ipoe reconcile failed", "site", id, "err", err)
				}
				return
			}
			if !s.state(id).profilesOK.Load() {
				if err := s.SyncProfiles(ctx, id); err != nil {
					s.log.Warn("profile sync retry failed", "site", id, "err", err)
				}
			}
			s.RunSiteTasks(ctx, id)
			// A hotspot building has no reconcile pass: read its devices here
			// (best effort, read only) so the unregistered list stays current.
			if st, ok := s.site(id); ok && (st.Mode == store.SiteModeEnv || !st.JoinedAt.IsZero()) {
				dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				if err := s.refreshDevices(dctx, st); err != nil {
					s.log.Debug("device refresh failed", "site", id, "err", err)
				}
			}
		}()
	}
	wg.Wait()
}

// hasDueTasks reports whether a building has queued router tasks that may run now.
func (s *Service) hasDueTasks(siteID string) (due bool) {
	now := s.now()
	s.st.View(func(d *store.Data) {
		for _, t := range d.Tasks {
			if t.SiteID == siteID && !t.NextTry.After(now) {
				due = true
			}
		}
	})
	return
}

// RunSweeper retries router tasks every few seconds and sweeps every interval.
func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	s.Sweep(ctx)
	tasks := time.NewTicker(10 * time.Second)
	sweep := time.NewTicker(every)
	defer tasks.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tasks.C:
			s.RunTasks(ctx)
		case <-sweep.C:
			s.Sweep(ctx)
		}
	}
}

// ResyncAll forgets what the router was last told, so the next sweep rebuilds
// every hotspot user. Called at start-up: the router may have been reset or
// restored from a backup while this app was stopped.
func (s *Service) ResyncAll() {
	_ = s.st.Update(func(d *store.Data) error {
		for id, a := range d.Accounts {
			if IPoE(d.Sites[a.SiteID]) {
				continue // IPoE rows are compared with the router by the diff, not forgotten
			}
			if a.RouterActive {
				a.RouterActive = false
				d.Accounts[id] = a
			}
		}
		return nil
	})
}
