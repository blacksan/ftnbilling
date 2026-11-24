package web

import "testing"

func TestNetMarkerState(t *testing.T) {
	r := func(states ...string) []netRouter {
		var l []netRouter
		for _, s := range states {
			l = append(l, netRouter{State: s})
		}
		return l
	}
	for _, c := range []struct {
		name          string
		routers       []netRouter
		online, total int
		want          string
	}{
		{"no routers", nil, 0, 0, netUnknown},
		{"all good", r("up"), 3, 3, netOK},
		{"all good, no ONUs", r("up", "up"), 0, 0, netOK},
		{"some ONU offline", r("up"), 2, 3, netPartial},
		{"router down", r("down"), 3, 3, netDown},
		{"router down, ONUs offline too", r("down"), 0, 3, netDown},
		{"all routers down", r("down", "down"), 0, 0, netDown},
		{"one of two down", r("up", "down"), 3, 3, netPartial},
		{"paused", r("paused"), 0, 0, netPartial},
		{"not joined yet", r("waiting"), 0, 0, netPartial},
		{"down and paused", r("down", "paused"), 0, 0, netDown},
	} {
		if got := netMarkerState(c.routers, c.online, c.total); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNetworkCSPOnlyWithKeyAndKeepsWebFigFormAction(t *testing.T) {
	s := &Server{}
	if got := s.networkCSP(); got != cspAdmin {
		t.Fatalf("no key must give the admin policy: %s", got)
	}
	s.cfg.GoogleMapsAPIKey = "AIzaSyTestKeyTestKeyTestKeyTestKey12345"
	s.cfg.WebFigDomain = "tech.example.co.ke"
	got := s.networkCSP()
	if want := "form-action 'self' https://tech.example.co.ke;"; !contains(got, want) {
		t.Errorf("WebFig form-action lost: %s", got)
	}
	s.cfg.WebFigDomain = "evil.example; script-src *"
	if got := s.networkCSP(); contains(got, "evil") {
		t.Errorf("bad WEBFIG_DOMAIN in the header: %s", got)
	}
	if got := googleMapsLink(-1.2864, 36.8172); got != "https://www.google.com/maps/search/?api=1&query=-1.2864,36.8172" {
		t.Errorf("link: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
