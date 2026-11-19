package sites

import (
	"fmt"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// ForgetLive drops the cached live views of a router.
func (m *Manager) forgetLive(siteID string) {
	m.live.mu.Lock()
	defer m.live.mu.Unlock()
	for _, v := range []string{viewLive, viewInterfaces, viewClients} {
		delete(m.live.ent, siteID+"|"+v)
	}
	delete(m.live.prev, siteID)
}

// ResetRouter takes a router back to "not set up" on the server's side: the
// next paste of a new line builds it again. The typed name must match. It
// clears the tunnel key, the API password, the join state and report, the
// obsolete per-router settings, the router's queued tasks and discovered
// devices, the cached checks and live views and the billing service's
// in-memory reconcile state; it rewrites the WireGuard peers, stops the
// runtime and queues every flat again for when the router is back. Accounts
// and the tunnel address are kept. It does not touch the router itself: the
// paste line erases it.
func (m *Manager) ResetRouter(siteID, confirmName, actor string) error {
	err := m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		if strings.TrimSpace(confirmName) != s.Name {
			return ErrNameMismatch
		}
		s.WGPublicKey, s.RESTPass, s.JoinedAt, s.Join = "", "", time.Time{}, store.JoinToken{}
		d.Sites[siteID] = s
		pre := "site." + siteID + "."
		for k := range d.Settings {
			if strings.HasPrefix(k, pre) {
				delete(d.Settings, k)
			}
		}
		for k, t := range d.Tasks {
			if t.SiteID == siteID {
				delete(d.Tasks, k)
			}
		}
		for k, dev := range d.Devices {
			if dev.SiteID == siteID {
				delete(d.Devices, k)
			}
		}
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: fmt.Sprintf("router reset by %s: it must be set up again with a new line", actor)})
		return nil
	})
	if err != nil {
		return err
	}
	m.refused.clear(siteID)
	m.chk.drop(siteID)
	m.forgetLive(siteID)
	m.Reload(siteID) // not joined any more: the runtime stops
	if err := m.ApplyPeers(); err != nil {
		m.log.Warn("could not write the WireGuard peers file", "site", siteID, "err", err)
	}
	if p := m.provisioner(); p != nil {
		p.ForgetSite(siteID)
		p.EnqueueSite(siteID, "router reset")
	}
	return nil
}
