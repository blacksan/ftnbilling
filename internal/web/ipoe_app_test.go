package web_test

import (
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// ipoeApp is a multi-building app with one router that is set up (ftn-dhcp is on).
func ipoeApp(t *testing.T, mut func(*config.Config)) (*multi, store.Site, *routeros.Mock) {
	t.Helper()
	m := newMultiOps(t, mut, nil)
	s := m.addSite(t, "Riverside")
	rt := m.mock(s.ID)
	rt.SeedRow("/ip/dhcp-server", routeros.Row{"name": "ftn-dhcp", "disabled": "false"})
	rt.SeedRow("/interface/bridge", routeros.Row{"name": "br-customers", "comment": "ftn: customer bridge"})
	m.selectSite(t, s.ID)
	return m, s, rt
}
