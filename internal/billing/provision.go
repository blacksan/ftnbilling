package billing

import (
	"time"

	"ispbill/internal/store"
)

// This file holds the frozen interface of the IPoE provisioning work (W2).
// The types and signatures are fixed so the API (W1) and the app can compile
// against them; W2 replaces the bodies.

// ProvisionStatus is what the reconciler knows about one building: when it
// last compared the router with the accounts table, how far apart they were,
// and what could not be done. It is the "provision" object of the API's Site.
type ProvisionStatus struct {
	LastRunAt  time.Time `json:"last_run_at"`
	InSync     bool      `json:"in_sync"`
	Drift      int       `json:"drift"`       // rows that differ between the accounts table and the router
	PendingOps int       `json:"pending_ops"` // router writes still queued
	LastError  string    `json:"last_error"`
	// ForeignConflicts describes rows on the router that are not ours but
	// clash with an account (same MAC or IP); they are reported, never touched.
	ForeignConflicts []string `json:"foreign_conflicts"`
}

// AccountRouterState is what the router side looks like for one account. It
// is the "router" object of the API's Account.
type AccountRouterState struct {
	InSync            bool      `json:"in_sync"`
	SuspendedOnRouter bool      `json:"suspended_on_router"`
	LeaseStatus       string    `json:"lease_status"` // bound | waiting | none
	LastSeen          time.Time `json:"last_seen"`
	PendingTask       bool      `json:"pending_task"`
	LastError         string    `json:"last_error"`
}

// ProvisionStatus reports the reconciler state of a building.
// STUB (W0): nothing is reconciled yet, so every building reports in sync.
func (s *Service) ProvisionStatus(siteID string) ProvisionStatus {
	return ProvisionStatus{InSync: true, ForeignConflicts: []string{}}
}

// AccountRouterState reports the router-side state of an account.
// STUB (W0): the only real input is the task queue (a queued task means not
// in sync); everything else is filled in by W2.
func (s *Service) AccountRouterState(accountID string) AccountRouterState {
	st := AccountRouterState{InSync: true, LeaseStatus: "none"}
	s.st.View(func(d *store.Data) {
		for _, t := range d.Tasks {
			if t.AccountID == accountID {
				st.PendingTask, st.InSync, st.LastError = true, false, t.LastErr
				break
			}
		}
	})
	return st
}
