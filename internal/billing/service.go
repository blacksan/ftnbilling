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

	"ispbill/internal/store"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrVoucherUsed  = errors.New("voucher is invalid or already used")
	ErrNoPackage    = errors.New("no package matches that amount")
	ErrSuspended    = errors.New("this account is suspended")
	ErrDuplicateMAC = store.ErrDuplicateMAC
	// ErrDuplicateEquipmentMAC: another live (not retired) equipment item has that MAC.
	ErrDuplicateEquipmentMAC = store.ErrDuplicateEquipmentMAC
	ErrDuplicateKey          = errors.New("that transaction code is already recorded")
	ErrBadState              = errors.New("that payment is not waiting for a decision")
	ErrVoucherSite           = errors.New("this voucher is for another building")
	ErrNoSite                = errors.New("choose a building")
)

// DuplicateMACError says which open account already owns a MAC. It matches
// ErrDuplicateMAC (and store.ErrDuplicateMAC) with errors.Is; use errors.As to
// get the owner, e.g. for the API's mac_in_use details. A MAC is unique across
// all buildings among open accounts.
type DuplicateMACError struct {
	AccountID string
	Code      string
	Flat      string
	SiteID    string
	SiteName  string
}

func (e *DuplicateMACError) Error() string {
	return fmt.Sprintf("%s (%s in %s, code %s)", ErrDuplicateMAC.Error(), e.Flat, e.SiteName, e.Code)
}

// Unwrap makes errors.Is(err, ErrDuplicateMAC) true.
func (e *DuplicateMACError) Unwrap() error { return ErrDuplicateMAC }

// duplicateMAC builds the error for the account that owns the MAC. Call it
// inside View/Update with d.
func duplicateMAC(d *store.Data, owner store.Account) error {
	return &DuplicateMACError{AccountID: owner.ID, Code: owner.Code, Flat: owner.Flat, SiteID: owner.SiteID, SiteName: d.Sites[owner.SiteID].Name}
}

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
	st      *store.Store
	routers RouterSource
	log     *slog.Logger
	now     func() time.Time
	policy  Policy
	box     *Secretbox // national-ID encryption; nil = feature off

	sites sync.Map                                                 // siteID -> *siteState
	hook  func()                                                   // called after a payment was applied (tenant messages)
	sweep atomic.Pointer[func(ctx context.Context, siteID string)] // per-building work after each sweep pass (set once at start)
	bg    sync.WaitGroup
}

func NewService(st *store.Store, rs RouterSource, log *slog.Logger) *Service {
	svc := &Service{st: st, routers: rs, log: log, now: time.Now, policy: DefaultPolicy()}
	if h, ok := rs.(interface {
		SetForeignIPs(func(siteID string) map[string]bool)
	}); ok {
		h.SetForeignIPs(svc.foreignIPs) // address allocation avoids the router's own static leases
	}
	return svc
}

// SetSweepHook registers work the sweeper does for every runnable building
// after its reconcile pass (the sites manager's customer-port self-heal and
// automatic fixes). The hook keeps its own timing and backoff.
func (s *Service) SetSweepHook(f func(ctx context.Context, siteID string)) { s.sweep.Store(&f) }

// SetClock overrides the clock (tests).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// SetCreditHook registers a function called (without locks held) after a
// payment was applied, so the tenant message scanners can run at once.
func (s *Service) SetCreditHook(f func()) { s.hook = f }

func (s *Service) credited() {
	if s.hook != nil {
		s.hook()
	}
}

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

// ---- accounts -----------------------------------------------------------

// AccountInput is what the admin types when creating or editing a flat.
type AccountInput struct {
	SiteID                          string // required: the building
	Flat, MAC, Tenant, Phone, Notes string
	// WhatsApp is the tenant's WhatsApp number; WASamePhone copies Phone instead.
	WhatsApp    string
	WASamePhone bool
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
		if _, taken := d.AccountByCode(c); !taken && !closedCodeTaken(d, c) {
			return c
		}
	}
}

func closedCodeTaken(d *store.Data, code string) bool {
	for _, a := range d.ClosedAccounts {
		if a.Code == code {
			return true
		}
	}
	return false
}

// flatTaken: flat labels only need to be unique inside one building.
func flatTaken(d *store.Data, siteID, flat, exceptID string) bool {
	for _, a := range d.Accounts {
		if a.SiteID == siteID && a.ID != exceptID && strings.EqualFold(a.Flat, flat) {
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

// waNumber picks the WhatsApp number: the phone when same is set, else the
// typed number (empty = none).
func waNumber(raw string, same bool, phone string) (string, error) {
	if same {
		return phone, nil
	}
	n, err := normPhone(raw)
	if err != nil {
		return "", errors.New("WhatsApp number: " + err.Error())
	}
	return n, nil
}

// SetWhatsApp sets the tenant's WhatsApp number and opt-out. samePhone copies
// the account's phone number.
func (s *Service) SetWhatsApp(id, raw string, samePhone, optOut bool) error {
	return s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[id]
		if !ok {
			return ErrNotFound
		}
		wa, err := waNumber(raw, samePhone, a.Phone)
		if err != nil {
			return err
		}
		if !d.SetWhatsApp(id, wa, optOut) { // also updates the linked customer
			return store.ErrNoChange
		}
		return nil
	})
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
	wa, err := waNumber(in.WhatsApp, in.WASamePhone, phone)
	if err != nil {
		return store.Account{}, err
	}
	var a store.Account
	err = s.st.Update(func(d *store.Data) error {
		if st, ok := d.Sites[in.SiteID]; !ok || st.Removed {
			return ErrNoSite
		}
		if other, ok := d.AccountByMAC(mac); ok {
			return duplicateMAC(d, other)
		}
		if flatTaken(d, in.SiteID, flat, "") {
			return fmt.Errorf("an account for %q already exists in this building", flat)
		}
		a = store.Account{
			ID: RandomID(), SiteID: in.SiteID, Code: s.uniqueCode(d), Flat: flat, MAC: mac,
			Tenant: cleanText(in.Tenant, 60), Phone: phone, Notes: cleanText(in.Notes, 300), WhatsApp: wa,
			CreatedAt: s.now(),
		}
		// A flat gets its fixed address now, and the router a task.
		pl, err := s.PlanFor(d.Sites[in.SiteID])
		if err != nil {
			return err
		}
		if a.IP, err = FreeIPLocked(d, in.SiteID, pl, s.foreignIPs(in.SiteID)); err != nil {
			return err
		}
		d.Accounts[a.ID] = a
		delete(d.Devices, store.DeviceKey(a.SiteID, mac))
		s.enqueueLocked(d, a, "account created")
		return nil
	})
	if err == nil {
		s.log.Info("account created", "site", a.SiteID, "code", a.Code, "flat", a.Flat, "mac", a.MAC)
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
		if flatTaken(d, a.SiteID, flat, id) {
			return fmt.Errorf("an account for %q already exists in this building", flat)
		}
		a.Flat, a.Tenant, a.Phone, a.Notes = flat, cleanText(tenant, 60), ph, cleanText(notes, 300)
		d.Accounts[id] = a
		return nil
	})
}

// ChangeMAC moves an account to a new ONT: the flat's lease on the router is
// rewritten for the new MAC (a queued task, retried until done).
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
			return duplicateMAC(d, other)
		}
		if strings.EqualFold(a.MAC, mac) && !a.MACConflict {
			return store.ErrNoChange
		}
		// Writing a MAC always puts the account back under the uniqueness
		// rule (clears a migration's MACConflict flag). The same MAC on a
		// flagged account is a re-check: it clears the flag if the MAC is
		// free (checked above) and was refused otherwise.
		msg := fmt.Sprintf("%s: ONT MAC changed %s -> %s", a.Flat, a.MAC, mac)
		if strings.EqualFold(a.MAC, mac) {
			msg = fmt.Sprintf("%s: ONT MAC %s checked, no other flat uses it", a.Flat, mac)
		}
		d.AddEvent(store.Event{Time: s.now(), SiteID: a.SiteID, Kind: "info", AccountID: id, Msg: msg})
		a.MAC, a.MACConflict = mac, false
		d.Accounts[id] = a
		delete(d.Devices, store.DeviceKey(a.SiteID, mac))
		s.enqueueLocked(d, a, "MAC changed")
		return nil
	})
	if err == nil {
		s.runAccountSite(ctx, id)
	}
	return err
}

// SetSuspended suspends or unsuspends an account. Suspending puts the flat's
// address on the router's suspended list; unsuspending restores an unexpired plan.
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
		s.enqueueLocked(d, a, "suspend toggled")
		return nil
	})
	if err == nil {
		s.runAccountSite(ctx, id)
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

// AccountByMAC finds the open account that owns a MAC in any building.
func (s *Service) AccountByMAC(mac string) (a store.Account, ok bool) {
	s.st.View(func(d *store.Data) { a, ok = d.AccountByMAC(mac) })
	return
}

// ---- unregistered devices ----------------------------------------------

// NoteDevice records a MAC that has no account. It is a no-op for known MACs.
func (s *Service) NoteDevice(siteID, mac, ip, source string) {
	if mac == "" || siteID == "" {
		return
	}
	_ = s.st.Update(func(d *store.Data) error {
		if len(d.OpenAccountsByMAC(mac)) > 0 {
			return store.ErrNoChange // already a customer (here or in another building)
		}
		if st, ok := d.Sites[siteID]; !ok || st.Removed {
			return store.ErrNoChange
		}
		now := s.now()
		key := store.DeviceKey(siteID, mac)
		dev, ok := d.Devices[key]
		if !ok {
			dev = store.Device{SiteID: siteID, MAC: mac, FirstSeen: now}
		}
		dev.LastSeen, dev.Hits = now, dev.Hits+1
		dev.Vendor = Vendor(mac)
		if ip != "" {
			dev.IP = ip
		}
		if source == "portal" || dev.Source == "" {
			dev.Source = source
		}
		d.Devices[key] = dev
		for len(d.Devices) > 500 { // bounded: drop the stalest
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

// DismissDevice removes a MAC from the unregistered list. A device the
// reconciler keeps seeing (source dhcp or arp) is kept as a dismissed marker
// so it does not come back every minute; one that opens the portal again does.
func (s *Service) DismissDevice(siteID, mac string) error {
	if m, err := NormalizeMAC(mac); err == nil {
		mac = m
	}
	return s.st.Update(func(d *store.Data) error {
		key := store.DeviceKey(siteID, mac)
		dev, ok := d.Devices[key]
		if !ok {
			return store.ErrNoChange
		}
		if dev.Source == "dhcp" || dev.Source == "arp" {
			dev.Source, dev.Online = SourceDismissed, false
			d.Devices[key] = dev
			return nil
		}
		delete(d.Devices, key)
		return nil
	})
}

// ---- packages -----------------------------------------------------------

// PackageForAmount finds the package whose price at the building equals
// amount (enabled ones first). The returned package carries the building's
// effective price.
func (s *Service) PackageForAmount(siteID string, amount int) (p store.Package, ok bool) {
	s.st.View(func(d *store.Data) {
		for _, c := range d.EffectivePackages(siteID) {
			if c.PriceKES == amount && c.PriceKES > 0 && (!ok || (p.Disabled && !c.Disabled)) {
				p, ok = c, true
			}
		}
	})
	return
}

// Ping reports whether the building's router answers.
func (s *Service) Ping(ctx context.Context, siteID string) error { return s.router(siteID).Ping(ctx) }

// ---- login --------------------------------------------------------------

// runAccountSite runs the router tasks of the building that holds the account.
func (s *Service) runAccountSite(ctx context.Context, accountID string) {
	if a, ok := s.GetAccount(accountID); ok {
		s.RunSiteTasks(ctx, a.SiteID)
	}
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
