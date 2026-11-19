package billing

import (
	"context"
	"time"

	"ispbill/internal/store"
)

// QueueReconcile queues a router sync for one account, as if something about
// it had changed (the API's "reconcile" call and the app's refresh button), and
// starts the building's queue in the background. It does not wait for the
// router: the task queue talks to it, with its usual backoff.
func (s *Service) QueueReconcile(accountID, reason string) error {
	var site string
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		s.enqueueLocked(d, a, reason)
		site = a.SiteID
		return nil
	})
	if err != nil {
		return err
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s.RunSiteTasks(ctx, site)
	}()
	return nil
}

// RefreshHostsAsync pulls the building's unregistered devices in the
// background (the API's refresh call). The result shows in the device list.
func (s *Service) RefreshHostsAsync(siteID string) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.RefreshHosts(ctx, siteID); err != nil {
			s.log.Warn("refresh hosts", "site", siteID, "err", err)
		}
	}()
}
