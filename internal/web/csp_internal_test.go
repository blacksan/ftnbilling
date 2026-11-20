package web

import "testing"

func TestAdminCSPIgnoresABadWebFigDomain(t *testing.T) {
	s := &Server{}
	s.cfg.WebFigDomain = "evil.example; script-src *"
	if got := s.adminCSP(); got != cspAdmin {
		t.Fatalf("a bad WEBFIG_DOMAIN changed the header: %q", got)
	}
	s.cfg.WebFigDomain = ""
	if got := s.adminCSP(); got != cspAdmin {
		t.Fatalf("WebFig off changed the header: %q", got)
	}
}
