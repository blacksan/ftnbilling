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
//
// OtherBuilding is true when the owner lives in a different apartment than the
// account the MAC was meant for: a MAC tied to a customer in one building must
// never be provisioned in another, so the API answers mac_other_building (it
// still matches ErrDuplicateMAC).
type DuplicateMACError struct {
	AccountID     string
	Code          string
	Flat          string
	SiteID        string
	SiteName      string
	CustomerName  string
	ApartmentID   string
	ApartmentName string
	Unit          string
	OtherBuilding bool
}

func (e *DuplicateMACError) Error() string {
	where := e.SiteName
	if e.ApartmentName != "" {
		where = e.ApartmentName
	}
	if e.OtherBuilding {
		return fmt.Sprintf("that MAC address already belongs to %s in another building (%s, unit %s, code %s): move the customer here first, or use another device", e.who(), where, e.Flat, e.Code)
	}
	return fmt.Sprintf("%s (%s in %s, code %s)", ErrDuplicateMAC.Error(), e.Flat, where, e.Code)
}

// CustomerOrCustomer is the owner's name, or "a customer" when there is none.
func (e *DuplicateMACError) CustomerOrCustomer() string { return e.who() }

func (e *DuplicateMACError) who() string {
	if e.CustomerName != "" {
		return e.CustomerName
	}
	return "a customer"
}

// Unwrap makes errors.Is(err, ErrDuplicateMAC) true.
func (e *DuplicateMACError) Unwrap() error { return ErrDuplicateMAC }

// duplicateMAC builds the error for the account that owns the MAC. Call it
// inside View/Update with d. forApartment is the apartment of the account the
// MAC was meant for ("" = unknown): another apartment means another building.
func duplicateMAC(d *store.Data, owner store.Account, forApartment string) error {
	e := &DuplicateMACError{AccountID: owner.ID, Code: owner.Code, Flat: owner.Unit, SiteID: owner.SiteID, SiteName: d.Sites[owner.SiteID].Name,
		CustomerName: accountCustomerName(d, owner), ApartmentID: owner.ApartmentID, ApartmentName: d.Apartments[owner.ApartmentID].Name, Unit: owner.Unit}
	if e.Flat == "" {
		e.Flat = owner.Flat
	}
	e.OtherBuilding = forApartment != "" && owner.ApartmentID != "" && owner.ApartmentID != forApartment
	return e
}

// accountCustomerName is the tenant's name for display.
func accountCustomerName(d *store.Data, a store.Account) string {
	if c, ok := d.Customers[a.CustomerID]; ok && c.Name != "" {
		return c.Name
	}
	return a.Tenant
}

// ErrMACOtherBuilding matches (errors.Is) a DuplicateMACError whose owner is in another apartment.
var ErrMACOtherBuilding = errors.New("that MAC address belongs to a customer in another building")

// Is lets errors.Is(err, ErrMACOtherBuilding) tell the cross-building case.
func (e *DuplicateMACError) Is(target error) bool {
	return target == ErrMACOtherBuilding && e.OtherBuilding
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
	return &Service{st: st, routers: rs, log: log, now: time.Now, policy: DefaultPolicy()}
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
	// ApartmentID is the building the customer lives in (the API and the web
	// form always send it). SiteID is the router; empty = the apartment's only
	// router. A call with only SiteID (older callers) uses the router's only
	// apartment, or makes one named after the router.
	ApartmentID string
	SiteID      string
	// Unit is the flat or door inside the apartment; Flat is its older name.
	Unit                            string
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

// CreateAccount makes a new account (admin only). The MAC must be unique, the
// unit must be free in the apartment, and the router must serve the apartment.
func (s *Service) CreateAccount(in AccountInput) (store.Account, error) {
	unit := cleanText(in.Unit, 40)
	if unit == "" {
		unit = cleanText(in.Flat, 40)
	}
	if unit == "" {
		return store.Account{}, errors.New("the unit (flat or door number) is required, e.g. B4")
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
		aptID, siteID, err := s.resolveLocked(d, in.ApartmentID, in.SiteID)
		if err != nil {
			return err
		}
		if other, ok := d.AccountByMAC(mac); ok {
			return duplicateMAC(d, other, aptID)
		}
		if err := unitTakenLocked(d, aptID, unit, ""); err != nil {
			return err
		}
		a = store.Account{
			ID: RandomID(), SiteID: siteID, ApartmentID: aptID, Code: s.uniqueCode(d), Flat: unit, Unit: unit, MAC: mac,
			Tenant: cleanText(in.Tenant, 60), Phone: phone, Notes: cleanText(in.Notes, 300), WhatsApp: wa,
			CreatedAt: s.now(),
		}
		// A flat gets its fixed address now, and the router a task.
		pl, err := s.PlanFor(d.Sites[siteID])
		if err != nil {
			return err
		}
		if a.IP, err = FreeIPLocked(d, siteID, pl, s.foreignIPs(siteID)); err != nil {
			return err
		}
		d.Accounts[a.ID] = a
		delete(d.Devices, store.DeviceKey(a.SiteID, mac))
		s.enqueueLocked(d, a, "account created")
		return nil
	})
	if err == nil {
		s.log.Info("account created", "site", a.SiteID, "apartment", a.ApartmentID, "code", a.Code, "unit", a.Unit, "mac", a.MAC)
	}
	return a, err
}

// UpdateAccount edits the unit, tenant, phone and notes. Code and MAC are not
// touched (use ChangeMAC for an ONT swap); the apartment and router change
// with MoveToApartment.
// ValidateAccountEdit checks the fields UpdateAccount would refuse for their own form (no store access), so a
// caller that does something irreversible first (a move) can refuse the whole request up front.
func (s *Service) ValidateAccountEdit(flat, phone string) error {
	if cleanText(flat, 40) == "" {
		return errors.New("the unit (flat or door number) is required")
	}
	_, err := normPhone(phone)
	return err
}

func (s *Service) UpdateAccount(id, flat, tenant, phone, notes string) error {
	unit := cleanText(flat, 40)
	if unit == "" {
		return errors.New("the unit (flat or door number) is required")
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
		if a.ApartmentID != "" {
			if err := unitTakenLocked(d, a.ApartmentID, unit, id); err != nil {
				return err
			}
		}
		a.Flat, a.Unit, a.Tenant, a.Phone, a.Notes = unit, unit, cleanText(tenant, 60), ph, cleanText(notes, 300)
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
			return duplicateMAC(d, other, a.ApartmentID)
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

// NoteDevice records a MAC that has no account on this router. It is a no-op
// for the ONTs of this router's own customers.
func (s *Service) NoteDevice(siteID, mac, ip, source string) {
	if mac == "" || siteID == "" {
		return
	}
	_ = s.st.Update(func(d *store.Data) error {
		if own := d.OpenAccountsByMAC(mac); len(own) > 0 && own[0].SiteID == siteID {
			return store.ErrNoChange // already a customer here; on another router it is listed as "belongs to"
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

// AdminRunCap is how long an admin action waits for the router before it answers: the change is saved, the
// router work stays queued and the retry loop finishes it (as for payments, which cap at runSiteShort).
const AdminRunCap = 4 * time.Second

// runAccountSite runs the router tasks of the building that holds the account, for at most AdminRunCap.
func (s *Service) runAccountSite(ctx context.Context, accountID string) {
	c, cancel := context.WithTimeout(ctx, AdminRunCap)
	defer cancel()
	s.runAccountSiteNoCap(c, accountID)
}

// runAccountSiteNoCap is runAccountSite for callers that already set their own deadline.
func (s *Service) runAccountSiteNoCap(ctx context.Context, accountID string) {
	if a, ok := s.GetAccount(accountID); ok {
		s.RunSiteTasks(ctx, a.SiteID)
	}
}

// runSitesShort runs the router tasks of several buildings at the same time, for at most AdminRunCap: a move
// must not make the new router wait behind a dead old one.
func (s *Service) runSitesShort(ctx context.Context, siteIDs ...string) {
	c, cancel := context.WithTimeout(ctx, AdminRunCap)
	defer cancel()
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, id := range siteIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.RunSiteTasks(c, id)
		}()
	}
	wg.Wait()
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
