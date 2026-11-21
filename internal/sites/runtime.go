package sites

import (
	"context"
	"sync"
	"time"

	"ispbill/internal/metrics"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Runtime is everything that runs for one joined, enabled building.
type Runtime struct {
	Site   store.Site // with defaults applied; secrets included, never log it
	Client routeros.Client
	Col    *metrics.Collector

	cancel func()
	done   chan struct{}
}

// run drives the collector (which saves its file on the way out) and, for a
// building that has not been seen answering yet, the join watcher.
func (m *Manager) run(ctx context.Context, rt *Runtime) {
	defer close(rt.done)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); rt.Col.Run(ctx) }()
	if rt.Site.Mode == store.SiteModeWG && rt.Site.JoinedAt.IsZero() {
		wg.Add(1)
		go func() { defer wg.Done(); m.watchJoin(ctx, rt) }()
	}
	wg.Wait()
}

// watchJoin pings the router until it answers once, then records JoinedAt: the
// admin page shows the building as joined from then on.
func (m *Manager) watchJoin(ctx context.Context, rt *Runtime) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		pc, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := rt.Client.Ping(pc)
		cancel()
		if err == nil {
			id := rt.Site.ID
			now := m.now()
			_ = m.st.Update(func(d *store.Data) error {
				s, ok := d.Sites[id]
				if !ok || !s.JoinedAt.IsZero() || s.WGPublicKey != rt.Site.WGPublicKey {
					return store.ErrNoChange
				}
				s.JoinedAt = now
				d.Sites[id] = s
				return nil
			})
			m.log.Info("router joined: it answered", "site", id)
			m.autoFixAfterJoin(ctx, id)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// autoFixAfterJoin runs the non-confirm automatic setup fixes once, a little
// after the router first answered (the join script may still be finishing).
// It never blocks the runtime: it runs in its own goroutine and stops with ctx.
func (m *Manager) autoFixAfterJoin(ctx context.Context, siteID string) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if m.autoFixDelay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.autoFixDelay):
			}
		}
		c, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		out, err := m.ApplyAutoFixes(c, siteID)
		if err != nil {
			m.log.Warn("post-join setup fixes skipped", "site", siteID, "err", err)
		}
		for _, o := range out {
			m.log.Info("post-join setup fix", "site", siteID, "check", o.ID, "applied", o.Applied, "err", o.Err)
		}
		m.afterJoin(ctx, siteID)
	}()
}
