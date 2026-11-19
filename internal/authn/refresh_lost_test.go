package authn_test

import (
	"errors"
	"testing"
	"time"

	"ispbill/internal/authn"
)

// A refresh whose answer is lost (the phone left coverage) must not sign the phone out when it
// retries later with the token it still has: nothing was done with the newer pair, so the
// previous token is still the phone's own, once.
func TestLostRefreshResponseDoesNotRevokePhone(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	t0, dev, err := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i1", Name: "phone"}, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	c.add(20 * time.Minute)
	if _, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1"); err != nil { // server rotates; the answer never reaches the phone
		t.Fatal(err)
	}
	c.add(90 * time.Second) // the phone is back online and retries with the token it still has
	t2, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1")
	if err != nil {
		t.Fatalf("the retry of a lost refresh was refused: %v", err)
	}
	if d, _ := s.Device(dev.ID); !d.RevokedAt.IsZero() {
		t.Fatalf("device revoked (%q): the user must sign in again with password and authenticator", d.RevokedReason)
	}
	// ... but only once: after the phone has used the pair it got, the old token is reuse again.
	c.add(time.Second)
	s.TouchDevice(dev.ID, "1.1.1.1")
	c.add(5 * time.Minute)
	if _, _, _, err := s.Refresh(t0.Refresh, "6.6.6.6"); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("a token used before the phone confirmed its newest pair: %v", err)
	}
	if _, _, _, err := s.Refresh(t2.Refresh, "1.1.1.1"); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("the device survived a reuse: %v", err)
	}
}

// Past the lost-response window an unconfirmed previous token is theft again.
func TestLostRefreshWindowEnds(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	t0, dev, _ := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i1", Name: "phone"}, "1.1.1.1")
	c.add(20 * time.Minute)
	if _, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	c.add(authn.RefreshLostGrace + time.Minute)
	if _, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1"); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("an old token after the window: %v", err)
	}
	if d, _ := s.Device(dev.ID); d.RevokedAt.IsZero() {
		t.Fatal("device not revoked")
	}
}
