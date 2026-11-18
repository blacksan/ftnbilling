package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Strict makes Update panic when a callback returns ErrNoChange after it
// changed something. Tests set it; production leaves it off.
var Strict bool

// DefaultCommitTimeout bounds one Commit.
const DefaultCommitTimeout = 5 * time.Second

// reloadEvery is how often a degraded store retries loading from its backend.
var reloadEvery = 5 * time.Second

// Store guards Data with a mutex. Update runs a callback against the live
// data, works out what changed by comparing with a shadow copy of the last
// committed state, and has the Backend make that change durable. If the
// callback fails or the commit fails, the change is rolled back in memory.
type Store struct {
	mu            sync.RWMutex
	b             Backend
	data          Data
	shadow        Data
	degraded      error
	commitTimeout time.Duration
	now           func() time.Time

	stop     chan struct{}
	retrying bool
	wg       sync.WaitGroup
	closed   bool
}

// OpenWith loads the backend's data and returns a Store on top of it.
func OpenWith(b Backend) (*Store, error) {
	d, err := b.Load(context.Background())
	if err != nil {
		return nil, err
	}
	s := &Store{b: b, commitTimeout: DefaultCommitTimeout, now: time.Now, stop: make(chan struct{})}
	s.install(d)
	return s, nil
}

// install makes d the live data and the shadow. Caller holds the write lock
// (or owns the Store exclusively).
func (s *Store) install(d *Data) {
	d.init()
	normalizeData(d)
	s.data = *d
	s.shadow = cloneData(&s.data)
}

// SetCommitTimeout overrides the per-commit time limit.
func (s *Store) SetCommitTimeout(d time.Duration) {
	s.mu.Lock()
	s.commitTimeout = d
	s.mu.Unlock()
}

// Kind reports the backend: "json" or "postgres".
func (s *Store) Kind() string { return s.b.Kind() }

// Healthy returns nil, or the reason the store cannot accept writes.
func (s *Store) Healthy() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.degraded
}

// View runs fn under a read lock. fn must not retain or modify d's maps.
func (s *Store) View(fn func(d *Data)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.data)
}

func (s *Store) runFn(fn func(d *Data) error) (err error, pv any, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			pv, panicked = r, true
		}
	}()
	err = fn(&s.data)
	return
}

// Update runs fn under the write lock and makes the result durable. If fn
// returns an error or panics, everything it changed is rolled back. Return
// ErrNoChange (before changing anything) to skip the commit. A failed commit
// is rolled back too and returned as ErrPersist.
func (s *Store) Update(fn func(d *Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.degraded != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, s.degraded)
	}
	ferr, pv, panicked := s.runFn(fn)
	if !panicked && ferr != nil && errors.Is(ferr, ErrNoChange) {
		if Strict {
			if cs := diffData(&s.data, &s.shadow, s.now()); !cs.Empty() {
				rollbackData(&s.data, &s.shadow, cs)
				panic("store: Update returned ErrNoChange after changing data:" + cs.Summary())
			}
		}
		return nil
	}
	cs := diffData(&s.data, &s.shadow, s.now())
	if panicked {
		rollbackData(&s.data, &s.shadow, cs)
		panic(pv)
	}
	if ferr != nil {
		rollbackData(&s.data, &s.shadow, cs)
		return ferr
	}
	if cs.Empty() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.commitTimeout)
	defer cancel()
	if err := s.b.Commit(ctx, &s.data, cs); err != nil {
		rollbackData(&s.data, &s.shadow, cs)
		if errors.Is(err, ErrAmbiguous) {
			s.reloadLocked()
		}
		return fmt.Errorf("%w: %w", ErrPersist, err)
	}
	applyChangeSet(&s.shadow, &s.data, cs)
	return nil
}

// reloadLocked replaces memory with what the backend holds. When that fails
// the store turns degraded and keeps retrying in the background.
func (s *Store) reloadLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*s.commitTimeout)
	defer cancel()
	d, err := s.b.Load(ctx)
	if err != nil {
		s.degraded = err
		s.startRetryLocked()
		return
	}
	s.install(d)
	s.degraded = nil
}

func (s *Store) startRetryLocked() {
	if s.retrying || s.closed {
		return
	}
	s.retrying = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(reloadEvery)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
			s.mu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 3*s.commitTimeout)
			d, err := s.b.Load(ctx)
			cancel()
			if err == nil {
				s.install(d)
				s.degraded = nil
				s.retrying = false
				s.mu.Unlock()
				return
			}
			s.degraded = err
			s.mu.Unlock()
		}
	}()
}

// Events returns events from the full history, newest first. Backends that
// keep every event answer from storage; the JSON backend only has the newest
// ones in memory.
func (s *Store) Events(ctx context.Context, q EventQuery) ([]Event, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if r, ok := s.b.(EventReader); ok {
		return r.Events(ctx, q)
	}
	var out []Event
	s.View(func(d *Data) {
		for i := len(d.Events) - 1; i >= 0 && len(out) < q.Limit; i-- {
			e := d.Events[i]
			if q.BeforeSeq > 0 && e.Seq >= q.BeforeSeq {
				continue
			}
			if (q.SiteID != "" && e.SiteID != q.SiteID) || (q.AccountID != "" && e.AccountID != q.AccountID) ||
				(q.CustomerID != "" && e.CustomerID != q.CustomerID) || (q.EquipmentID != "" && e.EquipmentID != q.EquipmentID) ||
				(q.Kind != "" && e.Kind != q.Kind) {
				continue
			}
			out = append(out, e)
		}
	})
	return out, nil
}

// Close stops background work and closes the backend.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stop)
	s.mu.Unlock()
	s.wg.Wait()
	return s.b.Close()
}

// Backend returns the backend (for tools such as db-check).
func (s *Store) Backend() Backend { return s.b }

// Snapshot returns a deep copy of everything in memory (tests, export).
func (s *Store) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneData(&s.data)
}

// ---- lookups and sorted lists that need the new tables --------------------

// AnyAccount finds an account, open or closed.
func (d *Data) AnyAccount(id string) (Account, bool) { return lookupAccount(d, id) }

// ClosedAccountList returns closed accounts, newest closure first.
func (d *Data) ClosedAccountList() []Account {
	out := make([]Account, 0, len(d.ClosedAccounts))
	for _, a := range d.ClosedAccounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClosedAt.After(out[j].ClosedAt) })
	return out
}

// OpenTenancy returns the current tenancy of an account.
func (d *Data) OpenTenancy(accountID string) (Tenancy, bool) {
	for _, t := range d.Tenancies {
		if t.AccountID == accountID && t.MoveOut.IsZero() {
			return t, true
		}
	}
	return Tenancy{}, false
}

// SetReloadInterval changes how often a degraded store retries (tests). It
// returns a function that restores the old value.
func SetReloadInterval(d time.Duration) (restore func()) {
	old := reloadEvery
	reloadEvery = d
	return func() { reloadEvery = old }
}
