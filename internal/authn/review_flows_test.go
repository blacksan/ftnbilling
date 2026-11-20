package authn_test

import (
	"testing"
	"time"

	"ispbill/internal/authn"
)

// Review: the phone DID receive and use the new pair, but its first use fell inside TouchDevice's 60 s throttle
// (it had made a call just before refreshing). LastSeenAt never moves past the rotation, so a copy of the previous
// refresh token is accepted for 24 h instead of revoking the device.
func TestReview_UsedPairStillUnconfirmedOldTokenAccepted(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	t0, dev, err := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i1", Name: "phone"}, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	c.add(14*time.Minute + 50*time.Second)
	s.TouchDevice(dev.ID, "1.1.1.1") // an API call with the old access token
	c.add(10 * time.Second)
	if _, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1"); err != nil { // the phone gets pair 2
		t.Fatal(err)
	}
	c.add(5 * time.Second)
	s.TouchDevice(dev.ID, "1.1.1.1")                // the phone uses pair 2 (throttled: no write)
	c.add(2 * time.Hour)                            // app in the background
	_, _, _, err = s.Refresh(t0.Refresh, "6.6.6.6") // a copy of the old refresh token
	d, _ := s.Device(dev.ID)
	t.Logf("old token after the new pair was used: err=%v revoked=%v", err, !d.RevokedAt.IsZero())
	if err == nil {
		t.Errorf("a copy of the previous refresh token was answered 2 h after the phone used the new pair")
	}
}
