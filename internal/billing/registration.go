package billing

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"ispbill/internal/store"
)

// Self-registration: a visitor of the captive page asks for an account. The request waits as a pending
// store.Registration until the owner approves it (the account is made with the device's MAC) or rejects it.
// Nothing here touches a router: the device stays walled because it has no account.

const (
	// MaxPendingPerRouter bounds the waiting registrations of one router (a flood of forms cannot fill the table).
	MaxPendingPerRouter = 25
	// registrationKeep is how long a decided or abandoned registration stays before it is dropped.
	registrationKeep = 30 * 24 * time.Hour
)

var (
	// ErrRegDecided: the registration was approved or rejected already.
	ErrRegDecided = errors.New("this registration was already decided")
	// ErrRegNeedsMAC: approving needs the device's MAC.
	ErrRegNeedsMAC = errors.New("choose the device this person is using (its MAC address), then approve")
)

// RegError is a refusal with a message that is safe to show to the visitor of the captive page.
type RegError struct{ Msg string }

func (e *RegError) Error() string { return e.Msg }

func regErr(msg string) error { return &RegError{msg} }

// RegistrationInput is what the captive form sends. MAC is never taken from the browser: the web layer leaves it
// empty unless the server itself identified the device (it cannot today, see web/captive.go).
type RegistrationInput struct {
	SiteID, ApartmentID, Unit, Name, Phone, Email, MAC string
	Consent                                            bool
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// SubmitRegistration records a pending registration after checking every field.
func (s *Service) SubmitRegistration(in RegistrationInput) (store.Registration, error) {
	if !in.Consent {
		return store.Registration{}, regErr("Please tick the box to agree to the privacy notice.")
	}
	if hasControl(in.Name) || hasControl(in.Unit) || hasControl(in.Email) {
		return store.Registration{}, regErr("Please use plain text in the form.")
	}
	name := cleanText(in.Name, 80)
	if len([]rune(name)) < 2 || len([]rune(strings.TrimSpace(in.Name))) > 80 {
		return store.Registration{}, regErr("Enter your full name (2 to 80 letters).")
	}
	phone, err := NormalizePhone(in.Phone)
	if err != nil {
		return store.Registration{}, regErr("Enter a Kenyan phone number like 0712 345 678.")
	}
	unit := cleanText(in.Unit, 40)
	if unit == "" || len([]rune(strings.TrimSpace(in.Unit))) > 40 {
		return store.Registration{}, regErr("Enter your house number (up to 40 characters).")
	}
	email := strings.TrimSpace(in.Email)
	if email != "" {
		a, err := mail.ParseAddress(email)
		if err != nil || a.Name != "" || len(email) > 120 || !strings.Contains(a.Address, "@") {
			return store.Registration{}, regErr("That email address does not look right. Leave it empty if you have none.")
		}
		email = a.Address
	}
	mac := ""
	if in.MAC != "" {
		if m, err := NormalizeMAC(in.MAC); err == nil && !s.isRouterMAC(m) {
			mac = m
		}
	}
	now := s.now()
	var out store.Registration
	err = s.st.Update(func(d *store.Data) error {
		site, ok := d.Sites[in.SiteID]
		if !ok || site.Removed || site.Disabled {
			return regErr("We could not tell which building you are in. Please contact support.")
		}
		if in.ApartmentID == "" || !d.ApartmentServedBy(in.ApartmentID, site.ID) {
			return regErr("Choose your apartment from the list.")
		}
		if err := unitTakenLocked(d, in.ApartmentID, unit, ""); err != nil {
			return regErr("That house number already has an account. If it is yours, contact support.")
		}
		if mac != "" {
			if _, taken := d.AccountByMAC(mac); taken {
				return regErr("This device is already registered. Contact support if your internet is paused.")
			}
		}
		pending := 0
		for _, r := range d.Registrations {
			if r.Status != store.RegPending {
				continue
			}
			if r.SiteID == site.ID {
				pending++
			}
			if r.Phone == phone || (r.ApartmentID == in.ApartmentID && store.UnitKey(r.Unit) == store.UnitKey(unit)) {
				return regErr("We already have your registration. The owner will review it; you do not need to send it again.")
			}
		}
		if pending >= MaxPendingPerRouter {
			return regErr("Too many registrations are waiting for this building. Please contact support.")
		}
		for id, r := range d.Registrations { // drop old decided rows and abandoned requests
			if now.Sub(r.CreatedAt) > registrationKeep {
				delete(d.Registrations, id)
			}
		}
		out = store.Registration{
			ID: RandomID(), SiteID: site.ID, ApartmentID: in.ApartmentID, Unit: unit, Name: name, Phone: phone, Email: email,
			MAC: mac, GuessMAC: guessMACLocked(d, site.ID, now, mac), Status: store.RegPending, ConsentAt: now, CreatedAt: now,
		}
		d.Registrations[out.ID] = out
		return nil
	})
	return out, err
}

// guessMACLocked is the one online unregistered device of the router, when there is exactly one (and the form
// did not name a device). A hint for the owner, nothing more.
func guessMACLocked(d *store.Data, siteID string, now time.Time, have string) string {
	if have != "" {
		return ""
	}
	var found []string
	for _, dv := range d.Devices {
		if dv.SiteID == siteID && dv.Source != SourceDismissed && dv.Online && now.Sub(dv.LastSeen) <= OnlineWindow {
			if len(d.OpenAccountsByMAC(dv.MAC)) == 0 {
				found = append(found, dv.MAC)
			}
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// RegistrationView is a pending registration with what the owner needs to decide.
type RegistrationView struct {
	store.Registration
	SiteName, ApartmentName string
	// Candidates are the router's unregistered devices (online first), the ones the owner can pick from.
	Candidates []UnregDevice
}

// PendingRegistrations lists the waiting registrations, oldest first; has filters routers (nil = all).
func (s *Service) PendingRegistrations(has func(siteID string) bool) []RegistrationView {
	var out []RegistrationView
	s.st.View(func(d *store.Data) {
		for _, r := range d.PendingRegistrations("") {
			if has != nil && !has(r.SiteID) {
				continue
			}
			out = append(out, RegistrationView{Registration: r, SiteName: d.Sites[r.SiteID].Name, ApartmentName: d.Apartments[r.ApartmentID].Name})
		}
	})
	cache := map[string][]UnregDevice{}
	for i := range out {
		sid := out[i].SiteID
		if _, ok := cache[sid]; !ok {
			cache[sid] = s.Unregistered(sid, false)
		}
		out[i].Candidates = cache[sid]
	}
	return out
}

// PendingRegistrationCount counts the waiting registrations (has filters routers; nil = all).
func (s *Service) PendingRegistrationCount(has func(siteID string) bool) int {
	n := 0
	s.st.View(func(d *store.Data) {
		for _, r := range d.Registrations {
			if r.Status == store.RegPending && (has == nil || has(r.SiteID)) {
				n++
			}
		}
	})
	return n
}

// GetRegistration returns one registration.
func (s *Service) GetRegistration(id string) (store.Registration, bool) {
	var r store.Registration
	var ok bool
	s.st.View(func(d *store.Data) { r, ok = d.Registrations[id] })
	return r, ok
}

// ApproveRegistration makes the account (the apartment, house number and the device's MAC), a customer record
// with the visitor's details linked to it, and closes the registration. The rules of any account apply: the MAC
// may not belong to another apartment's customer, nor to the router itself. The device leaves the wall on the
// router's next pass. mac may be empty when the registration already carries one.
func (s *Service) ApproveRegistration(ctx context.Context, id, mac, actor string) (store.Account, error) {
	reg, ok := s.GetRegistration(id)
	if !ok {
		return store.Account{}, ErrNotFound
	}
	if reg.Status != store.RegPending {
		return store.Account{}, ErrRegDecided
	}
	if strings.TrimSpace(mac) == "" {
		mac = reg.MAC
	}
	if strings.TrimSpace(mac) == "" {
		return store.Account{}, ErrRegNeedsMAC
	}
	m, err := NormalizeMAC(mac)
	if err != nil {
		return store.Account{}, err
	}
	// claim: a second click (or a second owner) must not make a second account
	if err := s.setRegStatus(id, store.RegPending, "approving", ""); err != nil {
		return store.Account{}, err
	}
	a, err := s.CreateFromDevice(ctx, DeviceAccountInput{SiteID: reg.SiteID, ApartmentID: reg.ApartmentID, Unit: reg.Unit, MAC: m, Tenant: reg.Name, Phone: reg.Phone})
	if err != nil {
		_ = s.setRegStatus(id, "approving", store.RegPending, "")
		return store.Account{}, err
	}
	if c, cerr := s.SaveCustomer(CustomerInput{Name: reg.Name, Phone: reg.Phone, Email: reg.Email, Note: "Registered on the captive page"}); cerr == nil {
		if lerr := s.LinkCustomer(a.ID, c.ID, time.Time{}); lerr != nil {
			s.log.Warn("registration: could not link the customer", "account", a.Code, "err", lerr)
		}
	} else {
		s.log.Warn("registration: could not save the customer", "account", a.Code, "err", cerr)
	}
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok {
			return store.ErrNoChange
		}
		// The details now live on the customer record: the registration keeps no personal data.
		d.Registrations[id] = store.Registration{ID: r.ID, SiteID: r.SiteID, ApartmentID: r.ApartmentID, Unit: r.Unit, MAC: m, Status: store.RegApproved,
			CreatedAt: r.CreatedAt, ConsentAt: r.ConsentAt, DecidedAt: now, DecidedBy: actor, AccountID: a.ID}
		d.AddEvent(store.Event{Time: now, SiteID: r.SiteID, Kind: "info", AccountID: a.ID, Msg: "registration approved (self-registered on the captive page)", Actor: actor})
		return nil
	})
	got, _ := s.GetAccount(a.ID)
	return got, nil
}

// RejectRegistration closes a pending registration and removes the visitor's details from it.
func (s *Service) RejectRegistration(id, actor string) error {
	now := s.now()
	return s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok {
			return ErrNotFound
		}
		if r.Status != store.RegPending {
			return ErrRegDecided
		}
		d.Registrations[id] = store.Registration{ID: r.ID, SiteID: r.SiteID, ApartmentID: r.ApartmentID, Unit: r.Unit, Status: store.RegRejected,
			CreatedAt: r.CreatedAt, ConsentAt: r.ConsentAt, DecidedAt: now, DecidedBy: actor}
		d.AddEvent(store.Event{Time: now, SiteID: r.SiteID, Kind: "info", Msg: "registration rejected", Actor: actor})
		return nil
	})
}

func (s *Service) setRegStatus(id, from, to, by string) error {
	return s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok {
			return ErrNotFound
		}
		if r.Status != from {
			return ErrRegDecided
		}
		r.Status = to
		d.Registrations[id] = r
		return nil
	})
}
