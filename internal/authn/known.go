package authn

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Known browsers and phones.
//
// After a full sign-in the browser gets a long-lived random cookie (the phone
// gets the same value in its login answer). The server keeps only its hash, in
// a settings row, tied to the user. A sign-in attempt that presents a known
// value for the user it names is exempt from the guesses-from-strangers limits
// (global and per account) and has its own slot for the password hash, so a
// crowd of strangers hammering the owner's name can never keep the owner out.
// The value is worth nothing without the password and the authenticator code.
//
// No schema change: the rows live in the existing settings table (key prefix
// "admin.kb.") and so in both backends.

const (
	knownPrefix = "admin.kb."
	// KnownLife is how long an unused known-browser value stays good; use renews it.
	KnownLife = 180 * 24 * time.Hour
	// maxKnownPerUser caps the values one user keeps; the oldest are dropped.
	maxKnownPerUser = 10
	knownRenewEvery = 24 * time.Hour
)

func knownKey(value string) string {
	h := sha256.Sum256([]byte(value))
	return knownPrefix + hex.EncodeToString(h[:])
}

// IssueKnown registers a new known browser/phone for the user and returns the
// value to hand to the client (stored only as a hash).
func (s *Service) IssueKnown(userID string) (string, error) {
	value := NewSessionValue()
	err := s.st.Update(func(d *store.Data) error {
		if u, ok := d.AdminUsers[userID]; !ok || u.Disabled {
			return ErrNoUser
		}
		now := s.now()
		type row struct {
			key string
			at  time.Time
		}
		var mine []row
		for k, v := range d.Settings {
			if !strings.HasPrefix(k, knownPrefix) {
				continue
			}
			if now.Sub(v.UpdatedAt) > KnownLife {
				delete(d.Settings, k)
			} else if v.Value == userID {
				mine = append(mine, row{k, v.UpdatedAt})
			}
		}
		sort.Slice(mine, func(i, j int) bool { return mine[i].at.Before(mine[j].at) })
		for len(mine) >= maxKnownPerUser {
			delete(d.Settings, mine[0].key)
			mine = mine[1:]
		}
		k := knownKey(value)
		d.Settings[k] = store.Setting{Key: k, Value: userID, UpdatedAt: now}
		return nil
	})
	return value, err
}

// KnownUser reports which user a known-browser value belongs to ("" = none).
func (s *Service) KnownUser(value string) (userID string) {
	if len(value) < 32 || len(value) > 100 {
		return ""
	}
	k := knownKey(value)
	s.st.View(func(d *store.Data) {
		if v, ok := d.Settings[k]; ok && s.now().Sub(v.UpdatedAt) <= KnownLife {
			userID = v.Value
		}
	})
	return userID
}

// KnownID is a stable, non-secret label for a known value (to count failures per browser).
func KnownID(value string) string { return knownKey(value)[len(knownPrefix) : len(knownPrefix)+16] }

// RenewKnown pushes the end of a value's life forward (at most once a day).
func (s *Service) RenewKnown(value string) {
	k := knownKey(value)
	_ = s.st.Update(func(d *store.Data) error {
		v, ok := d.Settings[k]
		if !ok || s.now().Sub(v.UpdatedAt) < knownRenewEvery {
			return store.ErrNoChange
		}
		v.UpdatedAt = s.now()
		d.Settings[k] = v
		return nil
	})
}

// revokeKnownLocked forgets every known browser of the user. Call it inside an Update.
func (s *Service) revokeKnownLocked(d *store.Data, userID string) int {
	n := 0
	for k, v := range d.Settings {
		if strings.HasPrefix(k, knownPrefix) && v.Value == userID {
			delete(d.Settings, k)
			n++
		}
	}
	return n
}
