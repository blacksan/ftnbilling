package sites

import (
	"context"
	"errors"
	"sync"

	"ispbill/internal/billing"
)

// Provisioner is what the sites manager needs from the billing service after a
// router joins and when it is reset.
type Provisioner interface {
	// ReconcileSite writes the building's flats to the router.
	ReconcileSite(ctx context.Context, siteID string) (billing.ProvisionRun, error)
	// EnsureIPs gives every flat of the building its fixed address.
	EnsureIPs(siteID string) (int, error)
	// EnqueueSite queues a router task for every flat of the building.
	EnqueueSite(siteID, reason string)
	// ForgetSite drops the in-memory reconcile state of a building.
	ForgetSite(siteID string)
}

// provMgr holds the hooks set after construction and the reboot slot.
type provMgr struct {
	mu       sync.Mutex
	runs     map[string]struct{} // routers with a disruptive action in flight (a reboot)
	p        Provisioner
	announce func(siteID, text, dedupe string)
}

// SetProvisioner connects the billing service.
func (m *Manager) SetProvisioner(p Provisioner) {
	m.prov.mu.Lock()
	m.prov.p = p
	m.prov.mu.Unlock()
}

// SetAnnouncer sets the function that tells the operators about an outcome
// (alerts.Engine.Announce: WhatsApp and email).
func (m *Manager) SetAnnouncer(f func(siteID, text, dedupe string)) {
	m.prov.mu.Lock()
	m.prov.announce = f
	m.prov.mu.Unlock()
}

func (m *Manager) provisioner() Provisioner {
	m.prov.mu.Lock()
	defer m.prov.mu.Unlock()
	return m.prov.p
}

func (m *Manager) announce(siteID, text, dedupe string) {
	m.prov.mu.Lock()
	f := m.prov.announce
	m.prov.mu.Unlock()
	if f != nil {
		f(siteID, text, dedupe)
	}
}

// ErrBusy: a reboot is already in flight for this router.
var ErrBusy = errors.New("another action is running on this router")

// BeginReboot takes the router's slot for a reboot, so two reboots (or a reset
// and a reboot) cannot overlap. It fails when the slot is taken. Call end when
// the reboot call is over.
func (m *Manager) BeginReboot(siteID string) (end func(), ok bool) {
	m.prov.mu.Lock()
	defer m.prov.mu.Unlock()
	if m.prov.runs == nil {
		m.prov.runs = map[string]struct{}{}
	}
	if _, busy := m.prov.runs[siteID]; busy {
		return nil, false
	}
	m.prov.runs[siteID] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.prov.mu.Lock()
			delete(m.prov.runs, siteID)
			m.prov.mu.Unlock()
		})
	}, true
}
