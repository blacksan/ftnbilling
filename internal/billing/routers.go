package billing

import (
	"context"
	"sync"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// RouterSource gives the router client of a building. Unknown, unjoined or
// paused buildings get a client whose calls fail with routeros.ErrNoRouter.
type RouterSource interface {
	Router(siteID string) routeros.Client
}

type single struct{ rt routeros.Client }

func (s single) Router(string) routeros.Client { return s.rt }

// SingleRouter serves one client for every site (tests, single router setups).
func SingleRouter(rt routeros.Client) RouterSource { return single{rt} }

// MockRouters is a test RouterSource: one fake router per site ID.
type MockRouters map[string]*routeros.Mock

func (m MockRouters) Router(siteID string) routeros.Client {
	if rt, ok := m[siteID]; ok && rt != nil {
		return rt
	}
	return routeros.NoRouter()
}

// siteState is the per-building state of the service: a lock that serialises
// task runs and the last reconcile pass.
type siteState struct {
	taskMu   ctxMutex
	provOnce sync.Once
	prov     *provState
}

func (s *Service) state(siteID string) *siteState {
	if v, ok := s.sites.Load(siteID); ok {
		return v.(*siteState)
	}
	v, _ := s.sites.LoadOrStore(siteID, &siteState{})
	return v.(*siteState)
}

// router returns the building's router client.
func (s *Service) router(siteID string) routeros.Client { return s.routers.Router(siteID) }

// site returns a copy of the site record.
func (s *Service) site(siteID string) (st store.Site, ok bool) {
	s.st.View(func(d *store.Data) { st, ok = d.Sites[siteID] })
	return
}

// runnableSites lists the IDs of the sites whose router may be worked on.
func (s *Service) runnableSites() (ids []string) {
	s.st.View(func(d *store.Data) {
		for id, st := range d.Sites {
			if st.Runnable() {
				ids = append(ids, id)
			}
		}
	})
	return
}

// Wait blocks until background work started by the service has finished. Used at shutdown and in tests.
func (s *Service) Wait() { s.bg.Wait() }

// ctxMutex is a mutex whose wait ends with a context. The zero value is ready to use.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *ctxMutex) init() { m.once.Do(func() { m.ch = make(chan struct{}, 1) }) }

// LockCtx takes the lock, or gives up when ctx ends first.
func (m *ctxMutex) LockCtx(ctx context.Context) bool {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return true
	default:
	}
	select {
	case m.ch <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Unlock releases a lock taken by LockCtx.
func (m *ctxMutex) Unlock() { <-m.ch }
