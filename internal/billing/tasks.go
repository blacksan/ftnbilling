package billing

import (
	"context"
	"fmt"
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
func (s *Service) enqueueLocked(d *store.Data, a store.Account, kick bool, reason string) {
	now := s.now()
	for id, t := range d.Tasks {
		if t.AccountID == a.ID {
			t.Kick = t.Kick || kick
			t.Reason, t.NextTry, t.Attempts, t.Rev = reason, now, 0, t.Rev+1
			d.Tasks[id] = t
			return
		}
	}
	t := store.Task{ID: RandomID(), AccountID: a.ID, Code: a.Code, Kick: kick, Reason: reason, NextTry: now, CreatedAt: now, Rev: 1}
	d.Tasks[t.ID] = t
}

// PendingTasks lists queued router actions (oldest first).
func (s *Service) PendingTasks() (out []store.Task) {
	s.st.View(func(d *store.Data) { out = d.TaskList() })
	return
}

func (s *Service) runTasksShort(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	s.RunTasks(c)
}

// RunTasks attempts every task that is due. It returns how many succeeded.
func (s *Service) RunTasks(ctx context.Context) int {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	now := s.now()
	var due []store.Task
	s.st.View(func(d *store.Data) {
		for _, t := range d.TaskList() {
			if !t.NextTry.After(now) {
				due = append(due, t)
			}
		}
	})
	ok := 0
	for _, t := range due {
		if ctx.Err() != nil {
			break
		}
		err := s.reconcile(ctx, t)
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
			s.log.Warn("router task failed; will retry", "code", t.Code, "attempt", t.Attempts+1, "err", err)
		}
	}
	return ok
}

// reconcile makes the router match the account's current state.
func (s *Service) reconcile(ctx context.Context, t store.Task) error {
	var a store.Account
	var pkg store.Package
	var found bool
	s.st.View(func(d *store.Data) {
		a, found = d.Accounts[t.AccountID]
		pkg = d.Packages[a.PackageID]
	})
	if !found {
		// Account deleted: make sure its router user and session are gone.
		if err := s.rt.KickUser(ctx, t.Code); err != nil {
			return err
		}
		return s.rt.RemoveUser(ctx, t.Code)
	}
	want := wantUser(a, s.now())
	if want {
		if !s.profilesOK.Load() {
			if err := s.SyncProfiles(ctx); err != nil {
				return fmt.Errorf("profile sync: %w", err)
			}
		}
		if err := s.rt.EnsureUser(ctx, routeros.User{
			Name: a.Code, Password: a.HSPass, Profile: pkg.Profile(), MAC: a.MAC, Comment: "ispbill:" + a.Flat,
		}); err != nil {
			return err
		}
		if t.Kick {
			if err := s.rt.KickUser(ctx, a.Code); err != nil {
				return err
			}
		}
	} else {
		if err := s.rt.KickUser(ctx, a.Code); err != nil {
			return err
		}
		if err := s.rt.RemoveUser(ctx, a.Code); err != nil {
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

// Sweep re-syncs profiles after a failure, queues router work for accounts
// whose state changed by the clock (a plan that just expired), records
// reminder events, and runs the task queue.
func (s *Service) Sweep(ctx context.Context) {
	if !s.profilesOK.Load() {
		if err := s.SyncProfiles(ctx); err != nil {
			s.log.Warn("profile sync retry failed", "err", err)
		}
	}
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		changed := false
		queued := map[string]bool{}
		for _, t := range d.Tasks {
			queued[t.AccountID] = true
		}
		remind := time.Duration(s.policy.ReminderDays) * 24 * time.Hour
		for _, a := range d.AccountList() {
			if wantUser(a, now) != a.RouterActive && !queued[a.ID] {
				s.enqueueLocked(d, a, false, "state changed by clock")
				changed = true
			}
			if a.Suspended || a.ExpiresAt.IsZero() {
				continue
			}
			pkg := d.Packages[a.PackageID]
			long := time.Duration(pkg.DurationMin)*time.Minute > remind
			if s.policy.ReminderDays > 0 && long && a.ExpiresAt.After(now) && a.ExpiresAt.Sub(now) <= remind && !a.RemindedFor.Equal(a.ExpiresAt) {
				d.AddEvent(store.Event{Time: now, Kind: "reminder_due", AccountID: a.ID,
					Msg: fmt.Sprintf("%s (%s): plan ends %s, send the %d-day reminder", a.Flat, a.Code, a.ExpiresAt.In(EAT).Format("2 Jan 15:04"), s.policy.ReminderDays)})
				a.RemindedFor = a.ExpiresAt
				d.Accounts[a.ID] = a
				changed = true
			}
			if s.policy.ReminderOnExpiry && !a.ExpiresAt.After(now) && !a.ExpiredFor.Equal(a.ExpiresAt) {
				d.AddEvent(store.Event{Time: now, Kind: "expired", AccountID: a.ID,
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
	s.RunTasks(ctx)
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
			if a.RouterActive {
				a.RouterActive = false
				d.Accounts[id] = a
			}
		}
		return nil
	})
}
