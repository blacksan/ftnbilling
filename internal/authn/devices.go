package authn

import (
	"sort"
	"time"

	"ispbill/internal/store"
)

// DeviceInfo is what the app says about itself at login.
type DeviceInfo struct {
	InstallID, Name, Model, Platform, AppVersion string
}

// Tokens is what login and refresh hand to the app. The secrets in it exist
// nowhere else: the store keeps hashes.
type Tokens struct {
	Access         string
	AccessExpires  time.Time
	Refresh        string
	RefreshExpires time.Time
	DeviceID       string
}

// refreshHistory is how many older refresh hashes a device remembers.
const refreshHistory = 5

func olderHas(list []string, h string) bool {
	found := false
	for _, o := range list { // no early exit: the time does not tell which matched
		if hashEq(h, o) {
			found = true
		}
	}
	return found
}

func (s *Service) issueLocked(dev *store.APIDevice, now time.Time) Tokens {
	acc := newToken(accessPrefix, dev.ID)
	ref := newToken(refreshPrefix, dev.ID)
	dev.AccessHash, dev.AccessExpiresAt = HashToken(acc), now.Add(AccessLife)
	dev.RefreshHash = HashToken(ref)
	dev.RefreshExpiresAt = now.Add(RefreshLife)
	if dev.RefreshExpiresAt.After(dev.AbsoluteExpiresAt) {
		dev.RefreshExpiresAt = dev.AbsoluteExpiresAt
	}
	return Tokens{Access: acc, AccessExpires: dev.AccessExpiresAt, Refresh: ref, RefreshExpires: dev.RefreshExpiresAt, DeviceID: dev.ID}
}

// LoginDevice signs a phone in. A second login from the same install of the
// same user replaces that device's tokens instead of adding a row.
func (s *Service) LoginDevice(userID string, info DeviceInfo, ip string) (Tokens, store.APIDevice, error) {
	var tok Tokens
	var dev store.APIDevice
	err := s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok || u.Disabled {
			return ErrNoUser
		}
		s.pruneLocked(d)
		now := s.now()
		found := false
		if info.InstallID != "" {
			for _, x := range d.APIDevices {
				if x.UserID == userID && x.InstallID == info.InstallID && x.RevokedAt.IsZero() {
					dev, found = x, true
					break
				}
			}
		}
		if !found {
			dev = store.APIDevice{ID: RandomID(), UserID: userID, InstallID: clip(info.InstallID, 100), CreatedAt: now}
		}
		dev.Name, dev.Model = clip(info.Name, 80), clip(info.Model, 80)
		dev.Platform, dev.AppVersion = clip(info.Platform, 20), clip(info.AppVersion, 40)
		dev.AbsoluteExpiresAt = now.Add(RefreshAbsolute)
		dev.RefreshPrevHash, dev.RefreshOlder, dev.RefreshRotatedAt = "", nil, now
		dev.LastIP, dev.LastSeenAt = ip, now
		tok = s.issueLocked(&dev, now)
		d.APIDevices[dev.ID] = dev
		s.Audit(d, u.Username, ip, "app sign-in: "+clip(dev.Name, 60)+" ("+dev.ID+")", "", "")
		return nil
	})
	return tok, dev, err
}

// Refresh exchanges a refresh token for a new pair. Every use rotates the
// refresh token. Presenting the previous one again is reuse: within the grace
// period (a lost response) it is answered with a fresh pair; later it means a
// copy of the token exists, and the device is revoked.
func (s *Service) Refresh(refreshToken, ip string) (Tokens, store.APIDevice, store.AdminUser, error) {
	id, ok := splitToken(refreshPrefix, refreshToken)
	if !ok {
		return Tokens{}, store.APIDevice{}, store.AdminUser{}, ErrTokenRevoked
	}
	h := HashToken(refreshToken)
	var tok Tokens
	var dev store.APIDevice
	var user store.AdminUser
	var result error
	err := s.st.Update(func(d *store.Data) error {
		x, found := d.APIDevices[id]
		if !found || !x.RevokedAt.IsZero() {
			result = ErrTokenRevoked
			return store.ErrNoChange
		}
		u, found := d.AdminUsers[x.UserID]
		now := s.now()
		if !found || u.Disabled || !now.Before(x.RefreshExpiresAt) || !now.Before(x.AbsoluteExpiresAt) {
			result = ErrTokenRevoked
			return store.ErrNoChange
		}
		switch {
		case hashEq(h, x.RefreshHash):
			if x.RefreshPrevHash != "" {
				x.RefreshOlder = append(x.RefreshOlder, x.RefreshPrevHash)
				if n := len(x.RefreshOlder); n > refreshHistory {
					x.RefreshOlder = append([]string(nil), x.RefreshOlder[n-refreshHistory:]...)
				}
			}
			x.RefreshPrevHash, x.RefreshRotatedAt = x.RefreshHash, now
		case hashEq(h, x.RefreshPrevHash) && now.Sub(x.RefreshRotatedAt) <= RefreshGrace:
			// A retry after a lost response: new pair, same previous token.
		case hashEq(h, x.RefreshPrevHash):
			s.revokeDeviceLocked(d, id, x, now, "refresh token used twice")
			s.Audit(d, u.Username, ip, "app device "+id+" revoked: an old refresh token was used again", "", "")
			result = ErrTokenRevoked
			return nil
		case olderHas(x.RefreshOlder, h):
			// A token from before the previous one: only a copy can still hold it.
			s.revokeDeviceLocked(d, id, x, now, "refresh token used twice")
			s.Audit(d, u.Username, ip, "app device "+id+" revoked: an old refresh token was used again", "", "")
			result = ErrTokenRevoked
			return nil
		default:
			// Not a token this device ever had: a guess. Revoking would let
			// anyone who knows a device id sign that phone out.
			result = ErrTokenRevoked
			return store.ErrNoChange
		}
		x.LastIP, x.LastSeenAt = ip, now
		tok = s.issueLocked(&x, now)
		d.APIDevices[id] = x
		dev, user = x, cloneUser(u)
		return nil
	})
	if err != nil && err != store.ErrNoChange {
		return Tokens{}, store.APIDevice{}, store.AdminUser{}, err
	}
	if result != nil {
		return Tokens{}, store.APIDevice{}, store.AdminUser{}, result
	}
	return tok, dev, user, nil
}

// AuthAccess checks an access token. It reads only; call TouchDevice after.
func (s *Service) AuthAccess(token string) (dev store.APIDevice, user store.AdminUser, err error) {
	id, ok := splitToken(accessPrefix, token)
	if !ok {
		return dev, user, ErrTokenRevoked
	}
	h := HashToken(token)
	err = ErrTokenRevoked
	s.st.View(func(d *store.Data) {
		x, found := d.APIDevices[id]
		if !found || !hashEq(h, x.AccessHash) || !x.RevokedAt.IsZero() {
			return
		}
		u, found := d.AdminUsers[x.UserID]
		now := s.now()
		if !found || u.Disabled || !now.Before(x.AbsoluteExpiresAt) {
			return
		}
		dev, user = x, cloneUser(u)
		if !now.Before(x.AccessExpiresAt) {
			err = ErrTokenExpired
			return
		}
		err = nil
	})
	return
}

// TouchDevice records activity, at most once a minute per device.
func (s *Service) TouchDevice(id, ip string) {
	now := s.now()
	s.mu.Lock()
	if t, ok := s.touch[id]; ok && now.Sub(t) < touchEvery {
		s.mu.Unlock()
		return
	}
	s.touch[id] = now
	s.mu.Unlock()
	_ = s.st.Update(func(d *store.Data) error {
		x, ok := d.APIDevices[id]
		if !ok {
			return store.ErrNoChange
		}
		x.LastSeenAt, x.LastIP = now, ip
		d.APIDevices[id] = x
		return nil
	})
}

func (s *Service) revokeDeviceLocked(d *store.Data, id string, x store.APIDevice, now time.Time, reason string) {
	x.RevokedAt, x.RevokedReason = now, reason
	x.AccessHash, x.RefreshHash, x.RefreshPrevHash = "", "", ""
	d.APIDevices[id] = x
}

// RevokeDevice signs a phone out for good.
func (s *Service) RevokeDevice(id, reason, actor, ip string) error {
	return s.st.Update(func(d *store.Data) error {
		x, ok := d.APIDevices[id]
		if !ok || !x.RevokedAt.IsZero() {
			return store.ErrNoChange
		}
		s.revokeDeviceLocked(d, id, x, s.now(), reason)
		s.Audit(d, actor, ip, "app device "+id+" ("+clip(x.Name, 60)+") revoked: "+reason, "", "")
		return nil
	})
}

// Device returns one device.
func (s *Service) Device(id string) (x store.APIDevice, ok bool) {
	s.st.View(func(d *store.Data) { x, ok = d.APIDevices[id] })
	return
}

// Devices lists a user's phones (all users when userID is ""), newest first,
// including revoked ones that were not pruned yet.
func (s *Service) Devices(userID string) []store.APIDevice {
	var out []store.APIDevice
	s.st.View(func(d *store.Data) {
		for _, x := range d.APIDevices {
			if userID == "" || x.UserID == userID {
				out = append(out, x)
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}
