package authn

import (
	"sort"
	"time"

	"ispbill/internal/store"
)

func (s *Service) newSession(d *store.Data, userID, ip, ua string, mfa bool, created time.Time) (value string, sess store.AdminSession) {
	now := s.now()
	value = NewSessionValue()
	life := PreMFALife
	if mfa {
		life = SessionAbsolute
	}
	if created.IsZero() {
		created = now
	}
	sess = store.AdminSession{ID: HashToken(value), UserID: userID, CSRF: NewCSRFToken(), MFA: mfa,
		IP: ip, UserAgent: clip(ua, 200), CreatedAt: created, LastSeenAt: now, ExpiresAt: now.Add(life)}
	d.AdminSessions[sess.ID] = sess
	return
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CreateSession starts a browser session after the password was accepted
// (mfa=false) or after both factors (mfa=true). The cookie value is returned
// once; the store keeps only its hash.
func (s *Service) CreateSession(userID, ip, ua string, mfa bool) (value string, sess store.AdminSession, err error) {
	err = s.st.Update(func(d *store.Data) error {
		if u, ok := d.AdminUsers[userID]; !ok || u.Disabled {
			return ErrNoUser
		}
		s.pruneLocked(d)
		s.capLocked(d, userID)
		value, sess = s.newSession(d, userID, ip, ua, mfa, time.Time{})
		return nil
	})
	return
}

// Rotate replaces a session by a new one with a new id and CSRF token (after
// login and after the second factor), ending the old one at once.
func (s *Service) Rotate(oldID string, mfa bool) (value string, sess store.AdminSession, err error) {
	err = s.st.Update(func(d *store.Data) error {
		old, ok := d.AdminSessions[oldID]
		if !ok {
			return ErrTokenRevoked
		}
		delete(d.AdminSessions, oldID)
		value, sess = s.newSession(d, old.UserID, old.IP, old.UserAgent, mfa, old.CreatedAt)
		return nil
	})
	return
}

// Session finds the session for a cookie value and checks its lifetime: idle
// time, absolute time, and that its user still exists and is enabled.
func (s *Service) Session(value string) (sess store.AdminSession, user store.AdminUser, ok bool) {
	if value == "" || len(value) > 100 {
		return
	}
	id := HashToken(value)
	now := s.now()
	s.st.View(func(d *store.Data) {
		x, f := d.AdminSessions[id]
		if !f || !now.Before(x.ExpiresAt) || now.Sub(x.LastSeenAt) >= SessionIdle {
			return
		}
		u, f := d.AdminUsers[x.UserID]
		if !f || u.Disabled {
			return
		}
		sess, user, ok = x, cloneUser(u), true
	})
	return
}

// Touch records activity, at most once a minute per session.
func (s *Service) Touch(sess store.AdminSession) {
	now := s.now()
	s.mu.Lock()
	if t, ok := s.touch[sess.ID]; ok && now.Sub(t) < touchEvery {
		s.mu.Unlock()
		return
	}
	if len(s.touch) > 4000 {
		s.touch = map[string]time.Time{}
	}
	s.touch[sess.ID] = now
	s.mu.Unlock()
	_ = s.st.Update(func(d *store.Data) error {
		x, ok := d.AdminSessions[sess.ID]
		if !ok {
			return store.ErrNoChange
		}
		x.LastSeenAt = now
		d.AdminSessions[sess.ID] = x
		return nil
	})
}

// DeleteSession ends one session.
func (s *Service) DeleteSession(id string) {
	_ = s.st.Update(func(d *store.Data) error {
		if _, ok := d.AdminSessions[id]; !ok {
			return store.ErrNoChange
		}
		delete(d.AdminSessions, id)
		return nil
	})
}

// UserSessions lists a user's live sessions, newest first.
func (s *Service) UserSessions(userID string) []store.AdminSession {
	var out []store.AdminSession
	now := s.now()
	s.st.View(func(d *store.Data) {
		for _, x := range d.AdminSessions {
			if x.UserID == userID && now.Before(x.ExpiresAt) && x.MFA {
				out = append(out, x)
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	return out
}

// RevokeSessions ends every session and every app sign-in of a user except
// keepSession. It returns how many sessions ended.
func (s *Service) RevokeSessions(userID, keepSession, reason, actor, ip string) (n int, err error) {
	err = s.st.Update(func(d *store.Data) error {
		n = s.revokeAllLocked(d, userID, keepSession, reason)
		if n == 0 {
			return store.ErrNoChange
		}
		s.Audit(d, actor, ip, "all sign-ins ended ("+reason+")", "", "")
		return nil
	})
	if err == store.ErrNoChange {
		err = nil
	}
	return
}

// revokeAllLocked deletes the user's sessions (except keep) and revokes their
// app devices. Call it inside an Update.
func (s *Service) revokeAllLocked(d *store.Data, userID, keep, reason string) int {
	n := 0
	for id, x := range d.AdminSessions {
		if x.UserID == userID && id != keep {
			delete(d.AdminSessions, id)
			n++
		}
	}
	now := s.now()
	for id, x := range d.APIDevices {
		if x.UserID == userID && x.RevokedAt.IsZero() {
			s.revokeDeviceLocked(d, id, x, now, reason)
			n++
		}
	}
	return n
}

// pruneLocked drops expired sessions and long-revoked devices.
func (s *Service) pruneLocked(d *store.Data) {
	now := s.now()
	for id, x := range d.AdminSessions {
		if !now.Before(x.ExpiresAt) || now.Sub(x.LastSeenAt) >= SessionIdle {
			delete(d.AdminSessions, id)
		}
	}
	for id, x := range d.APIDevices {
		if !x.RevokedAt.IsZero() && now.Sub(x.RevokedAt) > revokedKeep {
			delete(d.APIDevices, id)
		}
	}
}

// capLocked keeps one user from filling the table: past the cap the oldest go.
func (s *Service) capLocked(d *store.Data, userID string) {
	var mine []store.AdminSession
	for _, x := range d.AdminSessions {
		if x.UserID == userID {
			mine = append(mine, x)
		}
	}
	if len(mine) < maxSessionsPerUse {
		return
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].LastSeenAt.Before(mine[j].LastSeenAt) })
	for _, x := range mine[:len(mine)-maxSessionsPerUse+1] {
		delete(d.AdminSessions, x.ID)
	}
}

// Prune removes expired rows (called now and then by the server).
func (s *Service) Prune() {
	_ = s.st.Update(func(d *store.Data) error {
		before := len(d.AdminSessions) + len(d.APIDevices)
		s.pruneLocked(d)
		if len(d.AdminSessions)+len(d.APIDevices) == before {
			return store.ErrNoChange
		}
		return nil
	})
}
