package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrVoucherUsed  = errors.New("voucher is invalid or already used")
	ErrNoPackage    = errors.New("no package matches that amount")
	ErrSuspended    = errors.New("this account is suspended")
	ErrDuplicateMAC = errors.New("that MAC address already belongs to another account")
	ErrDuplicateKey = errors.New("that transaction code is already recorded")
	ErrBadState     = errors.New("that payment is not waiting for a decision")
)

// Account states shown to people.
const (
	StateNew       = "new" // no package bought yet
	StateActive    = "active"
	StateExpired   = "expired"
	StateSuspended = "suspended"
)

// Policy is the lifecycle configuration (see the admin settings page).
type Policy struct {
	ReminderDays     int  // "reminder due" event this many days before expiry (packages longer than this)
	ReminderOnExpiry bool // also record an event when a package expires
}

// DefaultPolicy: reminders 3 days before expiry and on expiry; no reconnection
// fee; reconnection is automatic when a payment arrives.
func DefaultPolicy() Policy { return Policy{ReminderDays: 3, ReminderOnExpiry: true} }

// Service implements the billing rules on top of the store and the router.
type Service struct {
	st     *store.Store
	rt     routeros.Client
	log    *slog.Logger
	now    func() time.Time
	policy Policy

	profilesOK atomic.Bool
	taskMu     sync.Mutex
}

func NewService(st *store.Store, rt routeros.Client, log *slog.Logger) *Service {
	return &Service{st: st, rt: rt, log: log, now: time.Now, policy: DefaultPolicy()}
}

// SetClock overrides the clock (tests).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// SetPolicy overrides the lifecycle policy.
func (s *Service) SetPolicy(p Policy) { s.policy = p }

// Policy returns the lifecycle policy.
func (s *Service) Policy() Policy { return s.policy }

// Now returns the service's current time.
func (s *Service) Now() time.Time { return s.now() }

// AccountState tells what an account is right now.
func AccountState(a store.Account, now time.Time) string {
	switch {
	case a.Suspended:
		return StateSuspended
	case a.ExpiresAt.IsZero() || a.PackageID == "":
		return StateNew
	case a.ExpiresAt.After(now):
		return StateActive
	}
	return StateExpired
}

// wantUser: should the router hold a hotspot user for this account?
func wantUser(a store.Account, now time.Time) bool {
	return AccountState(a, now) == StateActive
}

// ---- accounts -----------------------------------------------------------

// AccountInput is what the admin types when creating or editing a flat.
type AccountInput struct {
	Flat, MAC, Tenant, Phone, Notes string
}

func cleanText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func (s *Service) uniqueCode(d *store.Data) string {
	for {
		c := RandomCode(6)
		if _, taken := d.AccountByCode(c); !taken {
			return c
		}
	}
}

func flatTaken(d *store.Data, flat, exceptID string) bool {
	for _, a := range d.Accounts {
		if a.ID != exceptID && strings.EqualFold(a.Flat, flat) {
			return true
		}
	}
	return false
}

func normPhone(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return NormalizePhone(raw)
}

// CreateAccount makes a new flat account (admin only). The MAC must be unique.
func (s *Service) CreateAccount(in AccountInput) (store.Account, error) {
	flat := cleanText(in.Flat, 40)
	if flat == "" {
		return store.Account{}, errors.New("flat label is required, e.g. Flat 3B")
	}
	mac, err := NormalizeMAC(in.MAC)
	if err != nil {
		return store.Account{}, err
	}
	phone, err := normPhone(in.Phone)
	if err != nil {
		return store.Account{}, err
	}
	var a store.Account
	err = s.st.Update(func(d *store.Data) error {
		if other, ok := d.AccountByMAC(mac); ok {
			return fmt.Errorf("%w (%s)", ErrDuplicateMAC, other.Flat)
		}
		if flatTaken(d, flat, "") {
			return fmt.Errorf("an account for %q already exists", flat)
		}
		a = store.Account{
			ID: RandomID(), Code: s.uniqueCode(d), Flat: flat, MAC: mac,
			Tenant: cleanText(in.Tenant, 60), Phone: phone, Notes: cleanText(in.Notes, 300),
			HSPass: RandomSecret(16), CreatedAt: s.now(),
		}
		d.Accounts[a.ID] = a
		delete(d.Devices, mac)
		return nil
	})
	if err == nil {
		s.log.Info("account created", "code", a.Code, "flat", a.Flat, "mac", a.MAC)
	}
	return a, err
}

// UpdateAccount edits the label, tenant, phone and notes. Code and MAC are not
// touched (use ChangeMAC for an ONT swap).
func (s *Service) UpdateAccount(id, flat, tenant, phone, notes string) error {
	flat = cleanText(flat, 40)
	if flat == "" {
		return errors.New("flat label is required")
	}
	ph, err := normPhone(phone)
	if err != nil {
		return err
	}
	return s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		if flatTaken(d, flat, id) {
			return fmt.Errorf("an account for %q already exists", flat)
		}
		a.Flat, a.Tenant, a.Phone, a.Notes = flat, cleanText(tenant, 60), ph, cleanText(notes, 300)
		d.Accounts[id] = a
		return nil
	})
}

// ChangeMAC moves an account to a new ONT: the router user's mac-address is
// updated and any live session on the old ONT is kicked (retried until done).
func (s *Service) ChangeMAC(ctx context.Context, id, rawMAC string) error {
	mac, err := NormalizeMAC(rawMAC)
	if err != nil {
		return err
	}
	err = s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		if other, ok := d.AccountByMAC(mac); ok && other.ID != id {
			return fmt.Errorf("%w (%s)", ErrDuplicateMAC, other.Flat)
		}
		if strings.EqualFold(a.MAC, mac) {
			return store.ErrNoChange
		}
		d.AddEvent(store.Event{Time: s.now(), Kind: "info", AccountID: id, Msg: fmt.Sprintf("%s: ONT MAC changed %s -> %s", a.Flat, a.MAC, mac)})
		a.MAC = mac
		d.Accounts[id] = a
		delete(d.Devices, mac)
		s.enqueueLocked(d, a, true, "MAC changed")
		return nil
	})
	if err == nil {
		s.RunTasks(ctx)
	}
	return err
}

// SetSuspended suspends or unsuspends an account. Suspending removes the
// router user and kicks the session; unsuspending restores an unexpired plan.
func (s *Service) SetSuspended(ctx context.Context, id string, suspended bool) error {
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		if a.Suspended == suspended {
			return store.ErrNoChange
		}
		a.Suspended = suspended
		d.Accounts[id] = a
		s.enqueueLocked(d, a, suspended, "suspend toggled")
		return nil
	})
	if err == nil {
		s.RunTasks(ctx)
	}
	return err
}

func (s *Service) GetAccount(id string) (a store.Account, ok bool) {
	s.st.View(func(d *store.Data) { a, ok = d.Accounts[id] })
	return
}

func (s *Service) AccountByCode(code string) (a store.Account, ok bool) {
	s.st.View(func(d *store.Data) { a, ok = d.AccountByCode(code) })
	return
}

func (s *Service) AccountByMAC(mac string) (a store.Account, ok bool) {
	s.st.View(func(d *store.Data) { a, ok = d.AccountByMAC(mac) })
	return
}

// ---- unregistered devices ----------------------------------------------

// NoteDevice records a MAC that has no account. It is a no-op for known MACs.
func (s *Service) NoteDevice(mac, ip, source string) {
	if mac == "" {
		return
	}
	_ = s.st.Update(func(d *store.Data) error {
		if _, ok := d.AccountByMAC(mac); ok {
			return store.ErrNoChange
		}
		now := s.now()
		dev, ok := d.Devices[mac]
		if !ok {
			dev = store.Device{MAC: mac, FirstSeen: now}
		}
		dev.LastSeen, dev.Hits = now, dev.Hits+1
		if ip != "" {
			dev.IP = ip
		}
		if source == "portal" || dev.Source == "" {
			dev.Source = source
		}
		d.Devices[mac] = dev
		if len(d.Devices) > 500 { // bounded: drop the stalest
			var oldest string
			for m, v := range d.Devices {
				if oldest == "" || v.LastSeen.Before(d.Devices[oldest].LastSeen) {
					oldest = m
				}
			}
			delete(d.Devices, oldest)
		}
		return nil
	})
}

// HotspotHosts returns the router's current hotspot host table.
func (s *Service) HotspotHosts(ctx context.Context) ([]routeros.Host, error) {
	return s.rt.Hosts(ctx)
}

// RefreshHosts pulls unauthorised hotspot hosts from the router into the list.
func (s *Service) RefreshHosts(ctx context.Context) error {
	hosts, err := s.rt.Hosts(ctx)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if h.Authorized || h.Bypassed {
			continue
		}
		mac, err := NormalizeMAC(h.MAC)
		if err != nil {
			continue
		}
		s.NoteDevice(mac, h.Address, "hotspot-host")
	}
	return nil
}

// DismissDevice removes a MAC from the unregistered list.
func (s *Service) DismissDevice(mac string) error {
	return s.st.Update(func(d *store.Data) error {
		if _, ok := d.Devices[mac]; !ok {
			return store.ErrNoChange
		}
		delete(d.Devices, mac)
		return nil
	})
}

// ---- packages -----------------------------------------------------------

// PackageForAmount finds the package whose price equals amount (enabled ones first).
func (s *Service) PackageForAmount(amount int) (p store.Package, ok bool) {
	s.st.View(func(d *store.Data) {
		for _, c := range d.PackageList() {
			if c.PriceKES == amount && c.PriceKES > 0 && (!ok || (p.Disabled && !c.Disabled)) {
				p, ok = c, true
			}
		}
	})
	return
}

// Ping reports whether the router answers.
func (s *Service) Ping(ctx context.Context) error { return s.rt.Ping(ctx) }

// ---- login --------------------------------------------------------------

// AutoLogin logs the device in through REST when the account is active. If the
// account already has a session from the same IP it counts as logged in; a
// session from another IP is replaced (shared-users is 1).
func (s *Service) AutoLogin(ctx context.Context, a store.Account, ip string) bool {
	if ip == "" || !wantUser(a, s.now()) {
		return false
	}
	if sessions, err := s.rt.ActiveSessions(ctx); err == nil {
		for _, se := range sessions {
			if se.User != a.Code {
				continue
			}
			if se.Address == ip {
				return true
			}
			if err := s.rt.KickUser(ctx, a.Code); err != nil {
				s.log.Warn("kick stale session failed", "code", a.Code, "err", err)
			}
		}
	}
	if err := s.rt.Login(ctx, a.Code, a.HSPass, a.MAC, ip); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already") {
			return true
		}
		s.log.Warn("router login failed", "code", a.Code, "err", err)
		return false
	}
	return true
}

// Kick ends the account's live session without touching the subscription.
func (s *Service) Kick(ctx context.Context, code string) error {
	return s.rt.KickUser(ctx, strings.ToUpper(code))
}

// DescribeMinutes renders 90 as "1 h 30 min", 1440 as "1 day".
func DescribeMinutes(m int) string {
	switch {
	case m%43200 == 0 && m >= 43200:
		return plural(m/43200, "month")
	case m%10080 == 0 && m >= 10080:
		return plural(m/10080, "week")
	case m%1440 == 0 && m >= 1440:
		return plural(m/1440, "day")
	case m%60 == 0 && m >= 60:
		return plural(m/60, "hour")
	case m > 60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d min", m)
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
