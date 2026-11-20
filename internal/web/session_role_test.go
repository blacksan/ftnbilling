package web

import (
	"net/http"
	"testing"

	"ispbill/internal/store"
)

// Operators change customer and unused ports (ChangePort keeps the admin LAN and the OLT for owners);
// the rest of the router tools stay with owners.
func TestMinRoleLetsOperatorsPostThePortsUpdate(t *testing.T) {
	s := &Server{}
	for _, p := range []string{"/admin/sites/ports/update", "/admin/routers/ports/update"} {
		if got := s.minRole(http.MethodPost, p); got != store.RoleOperator {
			t.Errorf("%s: %s", p, got)
		}
	}
	for _, p := range []string{"/admin/routers/update", "/admin/sites/reboot", "/admin/routers/remove"} {
		if got := s.minRole(http.MethodPost, p); got != store.RoleOwner {
			t.Errorf("%s: %s", p, got)
		}
	}
	if got := s.minRole(http.MethodGet, "/admin/routers/ports.json"); got != store.RoleViewer {
		t.Errorf("ports.json: %s", got)
	}
}
