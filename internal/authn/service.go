package authn

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

// Errors callers map to responses. None of them says more than it must.
var (
	ErrNoDataKey        = errors.New("DATA_KEY is not set: two-factor secrets cannot be stored")
	ErrNoUser           = errors.New("no such user")
	ErrBadCode          = errors.New("that code is not right")
	ErrTOTPBackoff      = errors.New("too many wrong codes; wait before trying again")
	ErrTOTPLocked       = errors.New("too many wrong codes; only a recovery code works now")
	ErrAlreadyEnrolled  = errors.New("two-factor sign-in is already set up")
	ErrNotEnrolled      = errors.New("two-factor sign-in is not set up")
	ErrTokenExpired     = errors.New("the access token has expired")
	ErrTokenRevoked     = errors.New("the token is no longer valid")
	ErrLastOwner        = errors.New("there must be at least one active owner")
	ErrBadRole          = errors.New("role must be owner, operator or viewer")
	ErrUsernameTaken    = store.ErrDuplicateUsername
	errSetupCodeSetting = "admin.setup."
)

// Lifetimes (the plan's numbers).
const (
	SessionIdle     = 30 * time.Minute
	SessionAbsolute = 12 * time.Hour
	// PreMFALife is how long a session that has only passed the password lasts.
	PreMFALife      = 15 * time.Minute
	AccessLife      = 15 * time.Minute
	RefreshLife     = 30 * 24 * time.Hour
	RefreshAbsolute = 90 * 24 * time.Hour
	RefreshGrace    = 60 * time.Second
	// RefreshLostGrace is how long the previous refresh token still works, once, when the phone never used
	// the answer to the last refresh (a response lost to a coverage gap). After the phone uses the new pair
	// the previous token is dead and reusing it revokes the device.
	RefreshLostGrace  = 2 * time.Hour
	SetupCodeLife     = 14 * 24 * time.Hour
	touchEvery        = 60 * time.Second
	revokedKeep       = 30 * 24 * time.Hour
	maxSessionsPerUse = 20

	// MaxHalfSessions is how many password-only sessions one user may have open.
	MaxHalfSessions = 3
)

// Service holds the sign-in state of the admin pages and the app.
type Service struct {
	st     *store.Store
	box    *secretbox.Box // nil = no DATA_KEY: TOTP cannot be enrolled
	now    func() time.Time
	issuer string
	// audit writes an audit event. The server uses d.AddEvent; the command
	// line tools send it to stderr instead (see SetAudit).
	audit func(d *store.Data, e store.Event)

	// onAlert tells the owners about a security event (see SetAlert).
	onAlert func(msg string)

	mu    sync.Mutex
	touch map[string]time.Time // session or device id -> last write of last_seen
}

// New builds the service. dataKey may be nil (everything but TOTP works).
func New(st *store.Store, dataKey []byte, issuer string) (*Service, error) {
	s := &Service{st: st, now: time.Now, issuer: issuer, touch: map[string]time.Time{},
		audit: func(d *store.Data, e store.Event) { d.AddEvent(e) }}
	if s.issuer == "" {
		s.issuer = "Fast Tech Net"
	}
	if len(dataKey) > 0 {
		b, err := secretbox.NewLabelled(dataKey, "ispbill/totp", "ispbill/totp-idx")
		if err != nil {
			return nil, err
		}
		s.box = b
	}
	return s, nil
}

// SetClock overrides the clock (tests).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// Now is the service clock.
func (s *Service) Now() time.Time { return s.now() }

// SetAudit replaces the audit writer (the CLI cannot add event rows while the
// service runs on the same database: it would collide with its event numbers).
func (s *Service) SetAudit(fn func(d *store.Data, e store.Event)) { s.audit = fn }

// SetAlert installs the function that carries a security alert to the owners
// (the server wires it to the WhatsApp/e-mail outbox).
func (s *Service) SetAlert(fn func(msg string)) { s.onAlert = fn }

// Alert sends msg through the alert hook, if there is one.
func (s *Service) Alert(msg string) {
	if s.onAlert != nil {
		s.onAlert(msg)
	}
}

// HasDataKey reports whether TOTP secrets can be stored.
func (s *Service) HasDataKey() bool { return s.box != nil }

// Issuer is the name authenticator apps show.
func (s *Service) Issuer() string { return s.issuer }

// Audit records who did what, from where. Call it inside an Update.
func (s *Service) Audit(d *store.Data, actor, ip, msg, siteID, accountID string) {
	s.audit(d, store.Event{Time: s.now(), Kind: "audit", Actor: actor, IP: ip, Msg: msg, SiteID: siteID, AccountID: accountID})
}

// AuditNow records an audit event of its own (a failed login, a block).
func (s *Service) AuditNow(actor, ip, msg string) {
	_ = s.st.Update(func(d *store.Data) error {
		s.Audit(d, actor, ip, msg, "", "")
		return nil
	})
}

// ---- users --------------------------------------------------------------------------

// ValidRole reports whether r is a role name.
func ValidRole(r string) bool {
	return r == store.RoleOwner || r == store.RoleOperator || r == store.RoleViewer
}

// RoleRank orders roles: viewer < operator < owner.
func RoleRank(r string) int {
	switch r {
	case store.RoleOwner:
		return 3
	case store.RoleOperator:
		return 2
	case store.RoleViewer:
		return 1
	}
	return 0
}

func cloneUser(u store.AdminUser) store.AdminUser {
	u.RecoveryCodes = append([]string(nil), u.RecoveryCodes...)
	return u
}

// UserByName finds a user by name, ignoring letter case.
func (s *Service) UserByName(name string) (u store.AdminUser, ok bool) {
	name = strings.TrimSpace(name)
	s.st.View(func(d *store.Data) {
		for _, x := range d.AdminUsers {
			if strings.EqualFold(x.Username, name) {
				u, ok = cloneUser(x), true
				return
			}
		}
	})
	return
}

// UserByID finds a user by id.
func (s *Service) UserByID(id string) (u store.AdminUser, ok bool) {
	s.st.View(func(d *store.Data) {
		if x, f := d.AdminUsers[id]; f {
			u, ok = cloneUser(x), true
		}
	})
	return
}

// Users lists everyone, owners first and then by name.
func (s *Service) Users() []store.AdminUser {
	var out []store.AdminUser
	s.st.View(func(d *store.Data) {
		for _, x := range d.AdminUsers {
			out = append(out, cloneUser(x))
		}
	})
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := RoleRank(out[i].Role), RoleRank(out[j].Role); ri != rj {
			return ri > rj
		}
		return strings.ToLower(out[i].Username) < strings.ToLower(out[j].Username)
	})
	return out
}

// Owner returns the first owner (oldest). The command line tools act on it.
func (s *Service) Owner() (u store.AdminUser, ok bool) {
	for _, x := range s.Users() {
		if x.Role == store.RoleOwner {
			if !ok || x.CreatedAt.Before(u.CreatedAt) {
				u, ok = x, true
			}
		}
	}
	return
}

// SeedOwner creates the first owner when there are no users yet (first boot of
// a server that had a single admin). It does nothing when anyone exists.
func (s *Service) SeedOwner(username, passHash string) (created bool, err error) {
	err = s.st.Update(func(d *store.Data) error {
		if len(d.AdminUsers) > 0 {
			return store.ErrNoChange
		}
		now := s.now()
		u := store.AdminUser{ID: RandomID(), Username: strings.TrimSpace(username), Role: store.RoleOwner,
			PassHash: passHash, PassChangedAt: now, CreatedAt: now}
		d.AdminUsers[u.ID] = u
		s.Audit(d, "system", "", "admin owner "+u.Username+" created from the existing admin password", "", "")
		created = true
		return nil
	})
	return
}

// CreateUser adds a user (owner action). passHash is a PBKDF2 hash.
func (s *Service) CreateUser(username, role, passHash, actor, ip string) (store.AdminUser, error) {
	username = strings.TrimSpace(username)
	if !validUsername(username) {
		return store.AdminUser{}, errors.New("the user name must be 3 to 40 letters, digits, dots, dashes or underscores")
	}
	if !ValidRole(role) {
		return store.AdminUser{}, ErrBadRole
	}
	var u store.AdminUser
	err := s.st.Update(func(d *store.Data) error {
		now := s.now()
		u = store.AdminUser{ID: RandomID(), Username: username, Role: role, PassHash: passHash, PassChangedAt: now, CreatedAt: now}
		d.AdminUsers[u.ID] = u
		s.Audit(d, actor, ip, "user "+username+" created as "+role, "", "")
		return nil
	})
	return u, err
}

func validUsername(n string) bool {
	if len(n) < 3 || len(n) > 40 {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// SetPassword stores a new password hash and ends every other sign-in of the
// user (keepSession is the browser session that made the change, "" = none).
func (s *Service) SetPassword(userID, passHash, actor, ip, keepSession string) error {
	return s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		now := s.now()
		u.PassHash, u.PassChangedAt, u.UpdatedAt = passHash, now, now
		d.AdminUsers[userID] = u
		// A setup code issued for the old password must not outlive it.
		delete(d.Settings, errSetupCodeSetting+userID)
		s.revokeAllLocked(d, userID, keepSession, "password changed")
		s.Audit(d, actor, ip, "password changed for "+u.Username+"; other sign-ins ended", "", "")
		return nil
	})
}

// SetRole changes a user's role. The last active owner cannot be demoted.
func (s *Service) SetRole(userID, role, actor, ip string) error {
	if !ValidRole(role) {
		return ErrBadRole
	}
	return s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		if u.Role == role {
			return store.ErrNoChange
		}
		if u.Role == store.RoleOwner && activeOwners(d, userID) == 0 {
			return ErrLastOwner
		}
		u.Role, u.UpdatedAt = role, s.now()
		d.AdminUsers[userID] = u
		s.revokeAllLocked(d, userID, "", "role changed")
		s.Audit(d, actor, ip, "role of "+u.Username+" set to "+role, "", "")
		return nil
	})
}

// SetDisabled blocks or unblocks a user; blocking ends all their sign-ins.
func (s *Service) SetDisabled(userID string, disabled bool, actor, ip string) error {
	return s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		if u.Disabled == disabled {
			return store.ErrNoChange
		}
		if disabled && u.Role == store.RoleOwner && activeOwners(d, userID) == 0 {
			return ErrLastOwner
		}
		u.Disabled, u.UpdatedAt = disabled, s.now()
		d.AdminUsers[userID] = u
		verb := "enabled"
		if disabled {
			verb = "disabled"
			s.revokeAllLocked(d, userID, "", "user disabled")
		}
		s.Audit(d, actor, ip, "user "+u.Username+" "+verb, "", "")
		return nil
	})
}

// activeOwners counts enabled owners other than except.
func activeOwners(d *store.Data, except string) int {
	n := 0
	for id, u := range d.AdminUsers {
		if id != except && u.Role == store.RoleOwner && !u.Disabled {
			n++
		}
	}
	return n
}

// ---- TOTP enrolment and checks -------------------------------------------------------

func totpAAD(userID string) []byte { return []byte("totp:" + userID) }

func (s *Service) openSecret(u store.AdminUser) ([]byte, error) {
	if s.box == nil {
		return nil, ErrNoDataKey
	}
	if u.TOTPSecretEnc == "" {
		return nil, ErrNotEnrolled
	}
	p, err := s.box.OpenAAD(u.TOTPSecretEnc, totpAAD(u.ID))
	if err != nil {
		return nil, err
	}
	return DecodeSecret(p)
}

// Enrolled reports whether the user finished two-factor set-up.
func Enrolled(u store.AdminUser) bool { return !u.TOTPConfirmedAt.IsZero() }

// BeginEnrol returns the user's pending secret, making one when there is none
// (so reloading the page shows the same QR code).
func (s *Service) BeginEnrol(userID string) ([]byte, error) {
	if s.box == nil {
		return nil, ErrNoDataKey
	}
	var secret []byte
	err := s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		if Enrolled(u) {
			return ErrAlreadyEnrolled
		}
		if u.TOTPSecretEnc != "" {
			sec, err := s.openSecret(u)
			if err == nil {
				secret = sec
				return store.ErrNoChange
			}
		}
		sec, err := NewTOTPSecret()
		if err != nil {
			return err
		}
		enc, err := s.box.SealAAD(EncodeSecret(sec), totpAAD(userID))
		if err != nil {
			return err
		}
		u.TOTPSecretEnc, u.UpdatedAt = enc, s.now()
		d.AdminUsers[userID] = u
		secret = sec
		return nil
	})
	if errors.Is(err, store.ErrNoChange) {
		err = nil
	}
	return secret, err
}

// ConfirmEnrol finishes set-up when code matches the pending secret. It returns
// the recovery codes, which are shown once and stored only as hashes.
func (s *Service) ConfirmEnrol(userID, code, actor, ip string) (recovery []string, err error) {
	err = s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		if Enrolled(u) {
			return ErrAlreadyEnrolled
		}
		sec, err := s.openSecret(u)
		if err != nil {
			return err
		}
		step, ok := MatchTOTP(sec, code, s.now(), 0)
		if !ok {
			return ErrBadCode
		}
		codes, hashes, err := NewRecoveryCodes(RecoveryCodeCount)
		if err != nil {
			return err
		}
		now := s.now()
		u.TOTPConfirmedAt, u.TOTPLastStep, u.RecoveryCodes, u.UpdatedAt = now, step, hashes, now
		d.AdminUsers[userID] = u
		delete(d.Settings, errSetupCodeSetting+userID)
		s.Audit(d, actor, ip, "two-factor sign-in set up for "+u.Username, "", "")
		recovery = codes
		return nil
	})
	return
}

// Wrong authenticator codes are counted per user in the database. From
// totpBackoffAfter wrong codes in a row the user's codes are refused for a
// while (doubling, up to an hour); from totpLockAfter only a recovery code
// signs in, and a good recovery code starts the count again.
const (
	totpBackoffAfter = 5
	totpLockAfter    = 10
	totpDecay        = 24 * time.Hour // a count below the lock fades after this long without a wrong code
)

// TOTPBackoff is how long a user with n wrong codes in a row must wait since the last one.
func TOTPBackoff(n int) time.Duration {
	if n < totpBackoffAfter {
		return 0
	}
	d := 30 * time.Second << uint(n-totpBackoffAfter)
	if d > time.Hour || d <= 0 {
		d = time.Hour
	}
	return d
}

// TOTPState reports the user's failure state: how many wrong codes, whether only a
// recovery code works now, and how long until codes are tried again.
func (s *Service) TOTPState(u store.AdminUser) (fails int, recoveryOnly bool, wait time.Duration) {
	fails = u.TOTPFails
	if fails >= totpLockAfter {
		return fails, true, 0
	}
	if fails > 0 && s.now().Sub(u.TOTPFailAt) > totpDecay {
		return 0, false, 0
	}
	if w := TOTPBackoff(fails) - s.now().Sub(u.TOTPFailAt); w > 0 {
		return fails, false, w
	}
	return fails, false, 0
}

// VerifyTOTP checks a code for an enrolled user. A code works once: the step
// it matched is stored, and the same or an older step is refused afterwards.
func (s *Service) VerifyTOTP(userID, code string) error { return s.VerifyTOTPFrom(userID, code, "") }

// VerifyTOTPFrom is VerifyTOTP that also names the client address for the audit trail.
// It returns ErrBadCode for a wrong code, ErrTOTPBackoff while the user waits
// and ErrTOTPLocked when only a recovery code is accepted.
func (s *Service) VerifyTOTPFrom(userID, code, ip string) error {
	var bad error
	var alert string
	err := s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok || !Enrolled(u) {
			bad = ErrBadCode
			return store.ErrNoChange
		}
		fails, recoveryOnly, wait := s.TOTPState(u)
		if recoveryOnly {
			bad = ErrTOTPLocked
			return store.ErrNoChange
		}
		if wait > 0 {
			bad = ErrTOTPBackoff
			return store.ErrNoChange
		}
		sec, err := s.openSecret(u)
		if err != nil {
			return err
		}
		step, ok := MatchTOTP(sec, code, s.now(), u.TOTPLastStep)
		if !ok {
			fails++
			u.TOTPFails, u.TOTPFailAt = fails, s.now()
			d.AdminUsers[userID] = u
			bad = ErrBadCode
			if fails == totpBackoffAfter || fails == totpLockAfter {
				alert = itoa(fails) + " wrong authenticator codes in a row for " + u.Username
				if fails == totpLockAfter {
					alert += "; only a recovery code signs in now"
				}
				s.Audit(d, u.Username, ip, alert, "", "")
			}
			return nil
		}
		u.TOTPLastStep, u.TOTPFails, u.TOTPFailAt = step, 0, time.Time{}
		d.AdminUsers[userID] = u
		return nil
	})
	if errors.Is(err, store.ErrNoChange) {
		err = nil
	}
	if err != nil {
		return err
	}
	if alert != "" && s.onAlert != nil {
		s.onAlert(alert + " (from " + ip + ")")
	}
	return bad
}

// UseRecoveryCode spends one recovery code (single use).
func (s *Service) UseRecoveryCode(userID, code, ip string) error {
	var bad bool
	err := s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok || !Enrolled(u) {
			bad = true
			return store.ErrNoChange
		}
		i := matchRecovery(u.RecoveryCodes, code)
		if i < 0 {
			bad = true
			return store.ErrNoChange
		}
		rest := make([]string, 0, len(u.RecoveryCodes)-1)
		rest = append(rest, u.RecoveryCodes[:i]...)
		rest = append(rest, u.RecoveryCodes[i+1:]...)
		u.RecoveryCodes, u.UpdatedAt = rest, s.now()
		u.TOTPFails, u.TOTPFailAt = 0, time.Time{}
		d.AdminUsers[userID] = u
		s.Audit(d, u.Username, ip, "recovery code used by "+u.Username+" ("+itoa(len(rest))+" left)", "", "")
		return nil
	})
	if errors.Is(err, store.ErrNoChange) {
		err = nil
	}
	if err != nil {
		return err
	}
	if bad {
		return ErrBadCode
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// RegenerateRecoveryCodes replaces the unused codes with a fresh set.
func (s *Service) RegenerateRecoveryCodes(userID, actor, ip string) ([]string, error) {
	var codes []string
	err := s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok || !Enrolled(u) {
			return ErrNotEnrolled
		}
		c, h, err := NewRecoveryCodes(RecoveryCodeCount)
		if err != nil {
			return err
		}
		u.RecoveryCodes, u.UpdatedAt = h, s.now()
		d.AdminUsers[userID] = u
		s.Audit(d, actor, ip, "recovery codes regenerated for "+u.Username, "", "")
		codes = c
		return nil
	})
	return codes, err
}

// ResetTOTP removes the user's second factor, ends all their sign-ins and (for
// the command line) leaves them to enrol again at the next login with a setup
// code. keepSession is the caller's own browser session ("" = none).
func (s *Service) ResetTOTP(userID, actor, ip, keepSession string) error {
	return s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		u.TOTPSecretEnc, u.TOTPConfirmedAt, u.TOTPLastStep, u.RecoveryCodes, u.UpdatedAt = "", time.Time{}, 0, nil, s.now()
		u.TOTPFails, u.TOTPFailAt = 0, time.Time{}
		d.AdminUsers[userID] = u
		s.revokeAllLocked(d, userID, keepSession, "two-factor reset")
		s.Audit(d, actor, ip, "two-factor sign-in reset for "+u.Username, "", "")
		return nil
	})
}

// ---- setup codes ---------------------------------------------------------------------

// IssueSetupCode makes (and returns, once) a setup code for a user who has not
// enrolled. It replaces any earlier code.
func (s *Service) IssueSetupCode(userID, actor, ip string) (string, error) {
	var code string
	err := s.st.Update(func(d *store.Data) error {
		u, ok := d.AdminUsers[userID]
		if !ok {
			return ErrNoUser
		}
		if Enrolled(u) {
			return ErrAlreadyEnrolled
		}
		raw, err := randomFrom(recAlphabet, 16)
		if err != nil {
			return err
		}
		code = raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16]
		d.Settings[errSetupCodeSetting+userID] = store.Setting{Key: errSetupCodeSetting + userID, Value: HashRecoveryCode(raw), UpdatedAt: s.now()}
		s.Audit(d, actor, ip, "setup code issued for "+u.Username, "", "")
		return nil
	})
	return code, err
}

// CheckSetupCode reports whether code is the user's current, unexpired setup
// code. Call it with a user id of "" to burn the same time for an unknown name.
func (s *Service) CheckSetupCode(userID, code string) bool {
	var row store.Setting
	var have bool
	if userID != "" {
		s.st.View(func(d *store.Data) { row, have = d.Settings[errSetupCodeSetting+userID] })
	}
	h := HashRecoveryCode(code)
	ok := hashEq(h, row.Value)
	return have && ok && s.now().Sub(row.UpdatedAt) < SetupCodeLife
}

// HasSetupCode reports whether a (possibly expired) setup code is stored.
func (s *Service) HasSetupCode(userID string) (has bool) {
	s.st.View(func(d *store.Data) { _, has = d.Settings[errSetupCodeSetting+userID] })
	return
}
