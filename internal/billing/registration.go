package billing

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"
	"unicode"

	"ispbill/internal/store"
)

// Self-registration: a visitor of the captive page asks for an account. The request waits as a pending
// store.Registration until the owner approves it (the account is made with the device's MAC) or rejects it.
// Nothing here touches a router: the device stays walled because it has no account.

const (
	// MinPendingPerRouter is the least number of waiting registrations a router may hold. The real cap grows with
	// the building: max(MinPendingPerRouter, 2 x the houses the router serves), see PendingCap.
	MinPendingPerRouter = 25
	// MaxPendingUnassigned bounds the waiting registrations that name no router (two routers behind one public
	// address, so the form asked for a building name instead).
	MaxPendingUnassigned = 25
	// registrationKeep is how long a decided or abandoned registration stays before it is dropped. The privacy
	// notice on the form says so.
	registrationKeep = 30 * 24 * time.Hour
	// approvingStale is how long an approval may hold its claim: after that the registration is pending again (the
	// server crashed or was stopped between the claim and the end of the approval).
	approvingStale = 5 * time.Minute
	// candidateWindow is how close to the registration a device must first have been seen to be preselected.
	candidateWindow = 10 * time.Minute
	maxCandidates   = 40
)

// PendingCap is how many registrations may wait for a router: at least MinPendingPerRouter, and twice the houses
// (open accounts) it serves, so a large building can register all its tenants while a flood still has a ceiling.
func PendingCap(houses int) int {
	if c := 2 * houses; c > MinPendingPerRouter {
		return c
	}
	return MinPendingPerRouter
}

var (
	// ErrRegDecided: the registration was approved or rejected already.
	ErrRegDecided = errors.New("this registration was already decided")
	// ErrRegNeedsMAC: approving needs the device's MAC.
	ErrRegNeedsMAC = errors.New("choose the device this person is using (its MAC address), then approve")
	// ErrRegUnassigned: the visitor named a building but no router; the owner picks the router and apartment first.
	ErrRegUnassigned = errors.New("this registration names no router yet: choose the router and the apartment first")
	// ErrRegRateLimited: the web layer's admit hook refused (too many tries for this browser or number).
	ErrRegRateLimited = errors.New("too many attempts")
)

// RegError is a refusal with a message that is safe to show to the visitor of the captive page.
type RegError struct{ Msg string }

func (e *RegError) Error() string { return e.Msg }

func regErr(msg string) error { return &RegError{msg} }

// RegistrationInput is what the captive form sends. MAC is never taken from the browser: the web layer leaves it
// empty unless the server itself identified the device (it cannot today, see web/captive.go).
//
// SiteID may be empty when the server could not tell the router (two routers behind one public address): then
// Building is the free-text name the visitor typed and the owner assigns the registration to a router later.
// Admit, when set, is called with the normalised phone once the fields have passed every check and before anything
// is stored, so a refused form (a missing tick, a taken house number) never counts against a rate limit. It
// returns false to refuse (ErrRegRateLimited).
type RegistrationInput struct {
	SiteID, ApartmentID, Unit, Name, Phone, Email, MAC string
	DeviceName, Building                               string
	Consent                                            bool
	Admit                                              func(phone string) bool
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
	devName := cleanText(in.DeviceName, 60)
	if hasControl(in.DeviceName) || len([]rune(strings.TrimSpace(in.DeviceName))) > 60 {
		return store.Registration{}, regErr("Please use plain text and up to 60 characters for the device name.")
	}
	building := ""
	if in.SiteID == "" {
		if hasControl(in.Building) {
			return store.Registration{}, regErr("Please use plain text in the form.")
		}
		building = cleanText(in.Building, 80)
		if len([]rune(building)) < 2 || len([]rune(strings.TrimSpace(in.Building))) > 80 {
			return store.Registration{}, regErr("Enter the name of your apartment (2 to 80 characters).")
		}
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
		siteID := ""
		if in.SiteID != "" {
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
			siteID = site.ID
			if mac != "" {
				if _, taken := d.AccountByMAC(mac); taken {
					return regErr("This device is already registered. Contact support if your internet is paused.")
				}
			}
		}
		// The form is valid. From here on a refusal is about volume, not about a typing mistake.
		if in.Admit != nil && !in.Admit(phone) {
			return ErrRegRateLimited
		}
		pruneRegistrationsLocked(d, now)
		pending := 0
		for _, r := range d.Registrations {
			if !regPendingAt(r, now) {
				continue
			}
			if r.SiteID == siteID {
				pending++
			}
			sameHouse := siteID != "" && r.ApartmentID == in.ApartmentID && store.UnitKey(r.Unit) == store.UnitKey(unit)
			if siteID == "" && r.SiteID == "" && strings.EqualFold(r.Building, building) && store.UnitKey(r.Unit) == store.UnitKey(unit) {
				sameHouse = true
			}
			if r.Phone == phone || sameHouse {
				return regErr("We already have your registration. The owner will review it; you do not need to send it again.")
			}
		}
		limit := MaxPendingUnassigned
		if siteID != "" {
			limit = PendingCap(housesOfSiteLocked(d, siteID))
		}
		if pending >= limit {
			return regErr("Too many registrations are waiting for this building. Please contact support.")
		}
		out = store.Registration{
			ID: RandomID(), SiteID: siteID, ApartmentID: in.ApartmentID, Unit: unit, Name: name, Phone: phone, Email: email,
			MAC: mac, DeviceName: devName, Building: building, Status: store.RegPending, ConsentAt: now, CreatedAt: now,
		}
		if siteID == "" {
			out.ApartmentID = ""
		}
		d.Registrations[out.ID] = out
		return nil
	})
	return out, err
}

// housesOfSiteLocked counts the houses (open accounts) a router serves.
func housesOfSiteLocked(d *store.Data, siteID string) int {
	n := 0
	for _, a := range d.Accounts {
		if a.SiteID == siteID {
			n++
		}
	}
	return n
}

// regPendingAt: waiting for the owner. An approval whose claim is older than approvingStale counts as waiting
// again (its server stopped half way).
func regPendingAt(r store.Registration, now time.Time) bool {
	return r.Status == store.RegPending || (r.Status == store.RegApproving && now.Sub(r.DecidedAt) > approvingStale)
}

// pruneRegistrationsLocked drops every registration older than registrationKeep whatever its state (decided
// rows hold no personal data; undecided ones are the data the privacy notice promises to delete), the waiting
// requests of a router that was removed, and puts stale approvals back to pending. It reports whether it changed
// anything.
func pruneRegistrationsLocked(d *store.Data, now time.Time) bool {
	changed := false
	for id, r := range d.Registrations {
		switch {
		case now.Sub(r.CreatedAt) > registrationKeep:
			delete(d.Registrations, id)
			changed = true
		case regPendingAt(r, now) && r.SiteID != "" && (d.Sites[r.SiteID].ID == "" || d.Sites[r.SiteID].Removed):
			delete(d.Registrations, id) // its router is gone: nobody can approve it
			changed = true
		case r.Status == store.RegApproving && regPendingAt(r, now):
			r.Status, r.DecidedAt, r.DecidedBy = store.RegPending, time.Time{}, ""
			d.Registrations[id] = r
			changed = true
		}
	}
	return changed
}

// MaintainRegistrations is the periodic housekeeping of the registrations (called from the sweep): stale
// approvals return to pending and old or orphaned rows are deleted, with or without new submissions.
func (s *Service) MaintainRegistrations() {
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		if !pruneRegistrationsLocked(d, now) {
			return store.ErrNoChange
		}
		return nil
	})
}

// RegCandidate is one unregistered device the owner may pick for a registration.
type RegCandidate struct {
	UnregDevice
	// NameMatch: the device's DHCP host name looks like the device name the visitor gave.
	NameMatch bool
	// Likely: the only device first seen within 10 minutes of the registration (the one preselected).
	Likely bool
}

// RegistrationView is a pending registration with what the owner needs to decide.
type RegistrationView struct {
	store.Registration
	SiteName, ApartmentName string
	// Candidates are the router's unregistered devices, the ones first seen closest to the registration first.
	// GuessMAC (on the embedded row) is the preselected one, set only when exactly one device was first seen
	// within 10 minutes of the registration.
	Candidates []RegCandidate
	// Unassigned: the visitor named a building, not a router (see Registration.Building).
	Unassigned bool
}

// PendingRegistrations lists the waiting registrations, oldest first. has filters routers (nil = all); a
// registration that names no router yet is always listed, because whoever can approve must assign it.
func (s *Service) PendingRegistrations(has func(siteID string) bool) []RegistrationView {
	now := s.now()
	var out []RegistrationView
	s.st.View(func(d *store.Data) {
		var rows []store.Registration
		for _, r := range d.Registrations {
			if regPendingAt(r, now) && (r.SiteID == "" || has == nil || has(r.SiteID)) {
				rows = append(rows, r)
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
				return rows[i].CreatedAt.Before(rows[j].CreatedAt)
			}
			return rows[i].ID < rows[j].ID
		})
		for _, r := range rows {
			out = append(out, RegistrationView{Registration: r, SiteName: d.Sites[r.SiteID].Name, ApartmentName: d.Apartments[r.ApartmentID].Name, Unassigned: r.SiteID == ""})
		}
	})
	cache := map[string][]UnregDevice{}
	for i := range out {
		sid := out[i].SiteID
		if sid == "" {
			continue
		}
		if _, ok := cache[sid]; !ok {
			cache[sid] = s.Unregistered(sid, false)
		}
		out[i].Candidates, out[i].GuessMAC = registrationCandidates(cache[sid], out[i].Registration)
	}
	return out
}

// registrationCandidates orders the router's unknown devices by how close their first sighting is to the
// registration, marks the ones whose host name matches the visitor's device name, and says which one to
// preselect: the only device first seen within candidateWindow of the registration.
func registrationCandidates(devs []UnregDevice, r store.Registration) ([]RegCandidate, string) {
	out := make([]RegCandidate, 0, len(devs))
	for _, d := range devs {
		out = append(out, RegCandidate{UnregDevice: d, NameMatch: deviceNameMatches(r.DeviceName, d.HostName)})
	}
	gap := func(c RegCandidate) time.Duration {
		g := c.FirstSeen.Sub(r.CreatedAt)
		if g < 0 {
			g = -g
		}
		return g
	}
	sort.SliceStable(out, func(i, j int) bool { return gap(out[i]) < gap(out[j]) })
	if len(out) > maxCandidates {
		keep := out[:maxCandidates]
		for _, c := range out[maxCandidates:] {
			if c.NameMatch { // never drop a device that matches the visitor's name
				keep = append(keep, c)
			}
		}
		out = keep
	}
	near := -1
	for i, c := range out {
		if gap(c) <= candidateWindow {
			if near >= 0 {
				return out, ""
			}
			near = i
		}
	}
	if near < 0 {
		return out, ""
	}
	out[near].Likely = true
	return out, out[near].MAC
}

// deviceNameMatches: does the DHCP host name look like the name the visitor read from their device? Both are
// reduced to lower-case letters and digits ("Wanjiru's iPhone" and "Wanjirus-iPhone" are alike), then compared as a whole
// or by a shared word of four or more characters.
func deviceNameMatches(given, host string) bool {
	a, b := alnumWords(given), alnumWords(host)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	ja, jb := strings.Join(a, ""), strings.Join(b, "")
	if len(ja) >= 4 && len(jb) >= 4 && (ja == jb || strings.Contains(ja, jb) || strings.Contains(jb, ja)) {
		return true
	}
	for _, x := range a {
		if len(x) < 4 {
			continue
		}
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func alnumWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// PendingRegistrationCount counts the waiting registrations (has filters routers; nil = all; unassigned ones always count).
func (s *Service) PendingRegistrationCount(has func(siteID string) bool) int {
	now := s.now()
	n := 0
	s.st.View(func(d *store.Data) {
		for _, r := range d.Registrations {
			if regPendingAt(r, now) && (r.SiteID == "" || has == nil || has(r.SiteID)) {
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

// AssignRegistration gives a registration that names only a building a router and an apartment, so it can be
// approved. The apartment must be one the router serves and its house number must be free.
func (s *Service) AssignRegistration(id, siteID, apartmentID, actor string) error {
	now := s.now()
	return s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok {
			return ErrNotFound
		}
		if !regPendingAt(r, now) {
			return ErrRegDecided
		}
		site, ok := d.Sites[siteID]
		if !ok || site.Removed || site.Disabled {
			return errors.New("choose one of your routers")
		}
		if apartmentID == "" || !d.ApartmentServedBy(apartmentID, site.ID) {
			return errors.New("choose an apartment this router serves")
		}
		if err := unitTakenLocked(d, apartmentID, r.Unit, ""); err != nil {
			return errors.New("that house number already has an account in this apartment")
		}
		r.SiteID, r.ApartmentID, r.Building, r.Status, r.DecidedAt = site.ID, apartmentID, "", store.RegPending, time.Time{}
		d.Registrations[id] = r
		d.AddEvent(store.Event{Time: now, SiteID: site.ID, Kind: "info", Msg: "registration assigned to this router", Actor: actor})
		return nil
	})
}

// claimRegistration takes the approval claim: pending (or an approving claim gone stale) becomes approving, with
// DecidedAt as the time of the claim.
func (s *Service) claimRegistration(id string) error {
	now := s.now()
	return s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok {
			return ErrNotFound
		}
		if !regPendingAt(r, now) {
			return ErrRegDecided
		}
		r.Status, r.DecidedAt = store.RegApproving, now
		d.Registrations[id] = r
		return nil
	})
}

func (s *Service) releaseRegistration(id string) {
	_ = s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok || r.Status != store.RegApproving {
			return store.ErrNoChange
		}
		r.Status, r.DecidedAt = store.RegPending, time.Time{}
		d.Registrations[id] = r
		return nil
	})
}

// customerForPhone is the existing customer to link when a returning tenant registers again: the one
// non-anonymised customer with this normalised phone, provided none of its open accounts is in another
// apartment (then it is somebody else's number, or a person who lives elsewhere: a new record is made).
func (s *Service) customerForPhone(phone, apartmentID string) (store.Customer, bool) {
	var found []store.Customer
	s.st.View(func(d *store.Data) {
		for _, c := range d.Customers {
			if c.AnonymizedAt.IsZero() && c.Phone == phone {
				found = append(found, c)
			}
		}
		if len(found) != 1 {
			return
		}
		for _, a := range d.Accounts {
			if a.CustomerID == found[0].ID && a.ApartmentID != apartmentID {
				found = nil
				return
			}
		}
	})
	if len(found) != 1 {
		return store.Customer{}, false
	}
	return found[0], true
}

// ApproveRegistration makes the account (the apartment, house number and the device's MAC), a customer record
// with the visitor's details linked to it (the existing customer with the same phone number, when there is one
// that lives in no other apartment), and closes the registration. The rules of any account apply: the MAC may not
// belong to another apartment's customer, nor to the router itself. The device leaves the wall on the router's
// next pass. mac may be empty when the registration already carries one.
func (s *Service) ApproveRegistration(ctx context.Context, id, mac, actor string) (store.Account, error) {
	reg, ok := s.GetRegistration(id)
	if !ok {
		return store.Account{}, ErrNotFound
	}
	if !regPendingAt(reg, s.now()) {
		return store.Account{}, ErrRegDecided
	}
	if reg.SiteID == "" {
		return store.Account{}, ErrRegUnassigned
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
	if err := s.claimRegistration(id); err != nil {
		return store.Account{}, err
	}
	a, err := s.CreateFromDevice(ctx, DeviceAccountInput{SiteID: reg.SiteID, ApartmentID: reg.ApartmentID, Unit: reg.Unit, MAC: m, Tenant: reg.Name, Phone: reg.Phone, ConfirmNoAddress: true})
	if err != nil {
		s.releaseRegistration(id)
		return store.Account{}, err
	}
	cust, found := s.customerForPhone(reg.Phone, reg.ApartmentID)
	if !found {
		c, cerr := s.SaveCustomer(CustomerInput{Name: reg.Name, Phone: reg.Phone, Email: reg.Email, Note: "Registered on the captive page"})
		if cerr != nil {
			s.log.Warn("registration: could not save the customer", "account", a.Code, "err", cerr)
		}
		cust, found = c, cerr == nil
	}
	if found {
		if lerr := s.LinkCustomer(a.ID, cust.ID, time.Time{}); lerr != nil {
			s.log.Warn("registration: could not link the customer", "account", a.Code, "err", lerr)
		}
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

// erasedLocked is a decided registration: no personal data is kept.
func erasedLocked(r store.Registration, status string, now time.Time, actor string) store.Registration {
	return store.Registration{ID: r.ID, SiteID: r.SiteID, ApartmentID: r.ApartmentID, Unit: r.Unit, Status: status,
		CreatedAt: r.CreatedAt, ConsentAt: r.ConsentAt, DecidedAt: now, DecidedBy: actor}
}

// RejectRegistration closes a pending registration and removes the visitor's details from it.
func (s *Service) RejectRegistration(id, actor string) error {
	now := s.now()
	return s.st.Update(func(d *store.Data) error {
		r, ok := d.Registrations[id]
		if !ok {
			return ErrNotFound
		}
		if !regPendingAt(r, now) {
			return ErrRegDecided
		}
		d.Registrations[id] = erasedLocked(r, store.RegRejected, now, actor)
		d.AddEvent(store.Event{Time: now, SiteID: r.SiteID, Kind: "info", Msg: "registration rejected", Actor: actor})
		return nil
	})
}

// UnassignedSite is the siteID RejectAllPending takes for the registrations that name no router.
const UnassignedSite = ""

// RejectAllPending rejects every waiting registration of one router (siteID UnassignedSite: the ones naming no
// router) and erases their details, the owner's answer to a flood of junk requests. It returns how many it
// rejected. Claimed approvals that are still fresh are left alone.
func (s *Service) RejectAllPending(siteID, actor string) (int, error) {
	now := s.now()
	n := 0
	err := s.st.Update(func(d *store.Data) error {
		for id, r := range d.Registrations {
			if r.SiteID == siteID && regPendingAt(r, now) {
				d.Registrations[id] = erasedLocked(r, store.RegRejected, now, actor)
				n++
			}
		}
		if n == 0 {
			return store.ErrNoChange
		}
		d.AddEvent(store.Event{Time: now, SiteID: siteID, Kind: "info", Msg: fmt.Sprintf("%d waiting registrations rejected", n), Actor: actor})
		return nil
	})
	return n, err
}
