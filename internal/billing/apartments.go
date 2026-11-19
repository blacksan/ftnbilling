package billing

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Apartments are the buildings customers live in. Routers serve apartments
// many to many; an account belongs to one apartment, has a unit inside it and
// is provisioned only on its own router, which must serve its apartment.

var (
	ErrNoApartment      = errors.New("choose an apartment")
	ErrApartmentName    = errors.New("the apartment needs a name")
	ErrChooseRouter     = errors.New("choose a router: this apartment is served by more than one")
	ErrApartmentNoRoute = errors.New("this apartment has no router yet: link a router to it first")
	// ErrDeviceChanged: the account's MAC is no longer the one the operator was looking at.
	ErrDeviceChanged   = errors.New("this customer's device changed while you were moving them: check the device again")
	ErrRouterNotLinked = errors.New("that router does not serve this apartment: link it to the apartment first")
)

// UnitTakenError says which open account of the apartment already has a unit.
type UnitTakenError struct{ Unit, Apartment, Customer, Code string }

func (e *UnitTakenError) Error() string {
	if e.Customer != "" {
		return fmt.Sprintf("unit %s in %s is already used by %s (%s)", e.Unit, e.Apartment, e.Customer, e.Code)
	}
	return fmt.Sprintf("unit %s in %s is already used by account %s", e.Unit, e.Apartment, e.Code)
}

// Unwrap makes errors.Is(err, store.ErrDuplicateUnit) true.
func (e *UnitTakenError) Unwrap() error { return store.ErrDuplicateUnit }

// RouterBlockedError is returned when an unlink is refused because open
// accounts of the apartment are still served by the router.
type RouterBlockedError struct {
	Apartment, Router string
	Units             []string
}

func (e *RouterBlockedError) Error() string {
	u := e.Units
	more := ""
	if len(u) > 6 {
		u, more = u[:6], fmt.Sprintf(" and %d more", len(e.Units)-6)
	}
	return fmt.Sprintf("%s still serves %d open account(s) of %s (%s%s). Move them to another router first", e.Router, len(e.Units), e.Apartment, strings.Join(u, ", "), more)
}

// ContactInput is the caretaker or landlord section of the apartment form.
type ContactInput struct{ Name, Phone, Phone2, Email, Notes string }

func (c ContactInput) empty() bool {
	return c == ContactInput{} || (strings.TrimSpace(c.Name+c.Phone+c.Phone2+c.Email+c.Notes) == "")
}

// ApartmentInput is the apartment create and edit form.
type ApartmentInput struct {
	Name, Address, Notes string
	Caretaker, Landlord  ContactInput
}

// NormalizeContactPhone writes a Kenyan mobile number as +254XXXXXXXXX with the
// same rules as the M-Pesa phone helper (NormalizePhone). Empty stays empty.
func NormalizeContactPhone(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	n, err := NormalizePhone(raw)
	if err != nil {
		return "", err
	}
	return "+" + n, nil
}

// WhatsAppLink is the wa.me link of a +254 number ("" when there is none).
func WhatsAppLink(phone string) string {
	p := strings.TrimPrefix(strings.TrimSpace(phone), "+")
	if p == "" {
		return ""
	}
	return "https://wa.me/" + p
}

func cleanContact(key, who string, in ContactInput) (store.ApartmentContact, error) {
	if in.empty() {
		return store.ApartmentContact{}, nil
	}
	name := cleanText(in.Name, 80)
	if name == "" {
		return store.ApartmentContact{}, &FieldError{key + ".name", who + ": the name is required when any other detail is given"}
	}
	p1, err := NormalizeContactPhone(in.Phone)
	if err != nil {
		return store.ApartmentContact{}, &FieldError{key + ".phone", who + " phone: " + err.Error()}
	}
	p2, err := NormalizeContactPhone(in.Phone2)
	if err != nil {
		return store.ApartmentContact{}, &FieldError{key + ".phone2", who + " alternative phone: " + err.Error()}
	}
	email := strings.TrimSpace(in.Email)
	if email != "" {
		a, err := mail.ParseAddress(email)
		if err != nil || a.Address != email || len(email) > 120 {
			return store.ApartmentContact{}, &FieldError{key + ".email", who + ": that email address does not look right"}
		}
	}
	return store.ApartmentContact{Name: name, Phone: p1, Phone2: p2, Email: email, Notes: cleanText(in.Notes, 300)}, nil
}

func (in ApartmentInput) clean() (store.Apartment, error) {
	name := cleanText(in.Name, 80)
	if name == "" {
		return store.Apartment{}, &FieldError{"name", ErrApartmentName.Error()}
	}
	ct, err := cleanContact("caretaker", "Caretaker", in.Caretaker)
	if err != nil {
		return store.Apartment{}, err
	}
	ll, err := cleanContact("landlord", "Landlord / agent", in.Landlord)
	if err != nil {
		return store.Apartment{}, err
	}
	return store.Apartment{Name: name, Address: cleanText(in.Address, 200), Notes: cleanText(in.Notes, 500), Caretaker: ct, Landlord: ll}, nil
}

func apartmentNameTaken(d *store.Data, name, exceptID string) bool {
	if o, ok := d.ApartmentByName(name); ok && o.ID != exceptID {
		return true
	}
	return false
}

// ErrApartmentExists: another apartment already has that name (any letter case).
var ErrApartmentExists = errors.New("an apartment with that name already exists")

// FieldError is a validation problem on one input field (for the API's
// validation_failed answer). Field is "name" or "caretaker.phone" and so on.
type FieldError struct{ Field, Msg string }

func (e *FieldError) Error() string { return e.Msg }

// CreateApartment makes an apartment and links the given routers to it.
func (s *Service) CreateApartment(in ApartmentInput, siteIDs []string) (store.Apartment, error) {
	a, err := in.clean()
	if err != nil {
		return a, err
	}
	err = s.st.Update(func(d *store.Data) error {
		if apartmentNameTaken(d, a.Name, "") {
			return ErrApartmentExists
		}
		a.ID, a.CreatedAt = store.NewApartmentID(), s.now()
		for _, ok := d.Apartments[a.ID]; ok; _, ok = d.Apartments[a.ID] {
			a.ID = store.NewApartmentID()
		}
		d.Apartments[a.ID] = a
		for _, sid := range siteIDs {
			if err := linkLocked(d, a.ID, sid, s.now()); err != nil {
				return err
			}
		}
		d.AddEvent(store.Event{Time: s.now(), Kind: "info", Msg: "apartment created: " + a.Name})
		return nil
	})
	return a, err
}

// UpdateApartment edits the name, address, notes and contacts (not the routers).
func (s *Service) UpdateApartment(id string, in ApartmentInput) error {
	a, err := in.clean()
	if err != nil {
		return err
	}
	return s.st.Update(func(d *store.Data) error {
		cur, ok := d.Apartments[id]
		if !ok {
			return ErrNotFound
		}
		if apartmentNameTaken(d, a.Name, id) {
			return ErrApartmentExists
		}
		a.ID, a.CreatedAt = cur.ID, cur.CreatedAt
		if a == cur {
			return store.ErrNoChange
		}
		d.Apartments[id] = a
		if cur.Name != a.Name {
			d.AddEvent(store.Event{Time: s.now(), Kind: "info", Msg: fmt.Sprintf("apartment renamed: %s -> %s", cur.Name, a.Name)})
		}
		return nil
	})
}

// DeleteApartment removes an apartment that has no accounts (open or closed).
// Its router links go with it.
func (s *Service) DeleteApartment(id string) error {
	return s.st.Update(func(d *store.Data) error {
		a, ok := d.Apartments[id]
		if !ok {
			return ErrNotFound
		}
		n := 0
		for _, x := range d.Accounts {
			if x.ApartmentID == id {
				n++
			}
		}
		for _, x := range d.ClosedAccounts {
			if x.ApartmentID == id {
				n++
			}
		}
		if n > 0 {
			return fmt.Errorf("%s still has %d account(s), open or closed. Move the open ones to another apartment first; an apartment with history cannot be deleted", a.Name, n)
		}
		for k, l := range d.ApartmentSites {
			if l.ApartmentID == id {
				delete(d.ApartmentSites, k)
			}
		}
		delete(d.Apartments, id)
		d.AddEvent(store.Event{Time: s.now(), Kind: "info", Msg: "apartment deleted: " + a.Name})
		return nil
	})
}

func linkLocked(d *store.Data, aptID, siteID string, now time.Time) error {
	st, ok := d.Sites[siteID]
	if !ok || st.Removed {
		return ErrNoSite
	}
	if _, ok := d.Apartments[aptID]; !ok {
		return ErrNotFound
	}
	k := store.ApartmentSiteKey(aptID, siteID)
	if _, ok := d.ApartmentSites[k]; ok {
		return store.ErrNoChange
	}
	d.ApartmentSites[k] = store.ApartmentSite{ApartmentID: aptID, SiteID: siteID, CreatedAt: now}
	return nil
}

// LinkRouter makes a router serve an apartment. A router can serve several
// apartments, and an apartment can have several routers.
func (s *Service) LinkRouter(aptID, siteID string) error {
	return s.st.Update(func(d *store.Data) error {
		if err := linkLocked(d, aptID, siteID, s.now()); err != nil {
			return err
		}
		d.AddEvent(store.Event{Time: s.now(), SiteID: siteID, Kind: "info",
			Msg: fmt.Sprintf("router %s now serves apartment %s", d.Sites[siteID].Name, d.Apartments[aptID].Name)})
		return nil
	})
}

// UnlinkRouter stops a router serving an apartment. It is refused while the
// router still serves open accounts of that apartment: move them first.
func (s *Service) UnlinkRouter(aptID, siteID string) error {
	return s.st.Update(func(d *store.Data) error {
		apt, ok := d.Apartments[aptID]
		if !ok {
			return ErrNotFound
		}
		k := store.ApartmentSiteKey(aptID, siteID)
		if _, ok := d.ApartmentSites[k]; !ok {
			return store.ErrNoChange
		}
		var units []string
		for _, a := range d.OpenAccountsOfApartment(aptID) {
			if a.SiteID == siteID {
				units = append(units, a.Unit)
			}
		}
		if len(units) > 0 {
			return &RouterBlockedError{Apartment: apt.Name, Router: d.Sites[siteID].Name, Units: units}
		}
		delete(d.ApartmentSites, k)
		d.AddEvent(store.Event{Time: s.now(), SiteID: siteID, Kind: "info",
			Msg: fmt.Sprintf("router %s no longer serves apartment %s", d.Sites[siteID].Name, apt.Name)})
		return nil
	})
}

// SetApartmentRouters makes the apartment's routers exactly siteIDs (links
// added first, then removed ones checked). All or nothing.
func (s *Service) SetApartmentRouters(aptID string, siteIDs []string) error {
	return s.st.Update(func(d *store.Data) error {
		apt, ok := d.Apartments[aptID]
		if !ok {
			return ErrNotFound
		}
		want := map[string]bool{}
		for _, id := range siteIDs {
			want[id] = true
		}
		changed := false
		for id := range want {
			if err := linkLocked(d, aptID, id, s.now()); err == nil {
				changed = true
			} else if !errors.Is(err, store.ErrNoChange) {
				return err
			}
		}
		for _, l := range d.ApartmentSites {
			if l.ApartmentID != aptID || want[l.SiteID] {
				continue
			}
			var units []string
			for _, a := range d.OpenAccountsOfApartment(aptID) {
				if a.SiteID == l.SiteID {
					units = append(units, a.Unit)
				}
			}
			if len(units) > 0 {
				return &RouterBlockedError{Apartment: apt.Name, Router: d.Sites[l.SiteID].Name, Units: units}
			}
			delete(d.ApartmentSites, store.ApartmentSiteKey(aptID, l.SiteID))
			changed = true
		}
		if !changed {
			return store.ErrNoChange
		}
		return nil
	})
}

// ---- reading ------------------------------------------------------------

// ApartmentSummary is one row of the apartments list.
type ApartmentSummary struct {
	store.Apartment
	Routers []store.Site
	Total   int // open accounts
	Active  int // open accounts with paid time
	Online  int // open accounts whose router lease is bound right now
}

// ApartmentDetail is the apartment page: the summary, its accounts and routers.
type ApartmentDetail struct {
	ApartmentSummary
	Accounts []store.Account
	// RouterOf is the router name of each account, by account id.
	RouterOf map[string]string
	// Unlinked are the routers that do not serve the apartment (to link one).
	Unlinked []store.Site
	// AccountRouters counts the open accounts of this apartment per router id.
	AccountRouters map[string]int
}

// Apartments lists every apartment with its counts, by name.
func (s *Service) Apartments() []ApartmentSummary {
	var out []ApartmentSummary
	now := s.now()
	var online map[string]bool
	s.st.View(func(d *store.Data) {
		online = s.onlineSetLocked(d)
		for _, a := range d.ApartmentList() {
			out = append(out, summarize(d, a, now, online))
		}
	})
	return out
}

// ApartmentByID reads one apartment page.
func (s *Service) ApartmentByID(id string) (ApartmentDetail, bool) {
	var out ApartmentDetail
	ok := false
	now := s.now()
	s.st.View(func(d *store.Data) {
		a, found := d.Apartments[id]
		if !found {
			return
		}
		ok = true
		online := s.onlineSetLocked(d)
		out.ApartmentSummary = summarize(d, a, now, online)
		out.Accounts = d.OpenAccountsOfApartment(id)
		out.RouterOf = map[string]string{}
		out.AccountRouters = map[string]int{}
		for _, x := range out.Accounts {
			out.RouterOf[x.ID] = d.Sites[x.SiteID].Name
			out.AccountRouters[x.SiteID]++
		}
		for _, st := range d.SiteList() {
			if !d.ApartmentServedBy(id, st.ID) {
				out.Unlinked = append(out.Unlinked, st)
			}
		}
	})
	return out, ok
}

// onlineSetLocked is the set of account ids whose lease is bound on the last
// reconcile pass of their router.
func (s *Service) onlineSetLocked(d *store.Data) map[string]bool {
	out := map[string]bool{}
	for _, a := range d.Accounts {
		ps := s.prov(a.SiteID)
		ps.mu.Lock()
		sn, ok := ps.accts[a.Code]
		ps.mu.Unlock()
		if ok && sn.leaseStatus == "bound" && !sn.clash {
			out[a.ID] = true
		}
	}
	return out
}

func summarize(d *store.Data, a store.Apartment, now time.Time, online map[string]bool) ApartmentSummary {
	sum := ApartmentSummary{Apartment: a}
	for _, id := range d.ApartmentSiteIDs(a.ID) {
		sum.Routers = append(sum.Routers, d.Sites[id])
	}
	for _, x := range d.Accounts {
		if x.ApartmentID != a.ID {
			continue
		}
		sum.Total++
		if AccountState(x, now) == StateActive {
			sum.Active++
		}
		if online[x.ID] {
			sum.Online++
		}
	}
	return sum
}

// ApartmentsOfRouter lists the apartments a router serves.
func (s *Service) ApartmentsOfRouter(siteID string) (out []store.Apartment) {
	s.st.View(func(d *store.Data) {
		for _, id := range d.SiteApartmentIDs(siteID) {
			out = append(out, d.Apartments[id])
		}
	})
	return
}

// ---- resolving the apartment and router of an account --------------------

// resolveLocked works out the apartment and router of a new or moved account.
//
//   - apartmentID "" is the legacy call that only names a router: the router's
//     only apartment is used, one is made (named after the router) when it
//     serves none, and it is an error when it serves several;
//   - siteID "" picks the apartment's only router, and is an error when there
//     are none or several;
//   - the router must serve the apartment.
func (s *Service) resolveLocked(d *store.Data, aptID, siteID string) (string, string, error) {
	if aptID == "" {
		if siteID == "" {
			return "", "", ErrNoApartment
		}
		st, ok := d.Sites[siteID]
		if !ok || st.Removed {
			return "", "", ErrNoSite
		}
		ids := d.SiteApartmentIDs(siteID)
		switch len(ids) {
		case 1:
			aptID = ids[0]
		case 0:
			aptID = store.MigratedApartmentID(siteID)
			if _, taken := d.Apartments[aptID]; taken {
				return "", "", ErrNoApartment
			}
			name := cleanText(st.Name, 80)
			if apartmentNameTaken(d, name, "") {
				name += " (" + siteID + ")"
			}
			d.Apartments[aptID] = store.Apartment{ID: aptID, Name: name, Address: st.Address, CreatedAt: s.now()}
			if err := linkLocked(d, aptID, siteID, s.now()); err != nil {
				return "", "", err
			}
		default:
			return "", "", ErrNoApartment
		}
	}
	apt, ok := d.Apartments[aptID]
	if !ok {
		return "", "", ErrNoApartment
	}
	if siteID == "" {
		ids := d.ApartmentSiteIDs(aptID)
		switch len(ids) {
		case 0:
			return "", "", fmt.Errorf("%w (%s)", ErrApartmentNoRoute, apt.Name)
		case 1:
			siteID = ids[0]
		default:
			return "", "", ErrChooseRouter
		}
	}
	if st, ok := d.Sites[siteID]; !ok || st.Removed {
		return "", "", ErrNoSite
	}
	if !d.ApartmentServedBy(aptID, siteID) {
		return "", "", fmt.Errorf("%w (%s is not linked to %s)", ErrRouterNotLinked, d.Sites[siteID].Name, apt.Name)
	}
	return aptID, siteID, nil
}

// unitTakenLocked returns an error when another open account of the apartment has the unit.
func unitTakenLocked(d *store.Data, aptID, unit, exceptID string) error {
	k := store.UnitKey(unit)
	for _, a := range d.Accounts {
		if a.ApartmentID == aptID && a.ID != exceptID && store.UnitKey(a.Unit) == k {
			return &UnitTakenError{Unit: a.Unit, Apartment: d.Apartments[aptID].Name, Customer: accountCustomerName(d, a), Code: a.Code}
		}
	}
	return nil
}

// ---- moving a customer ---------------------------------------------------

// MoveInput moves an account to another apartment and/or router.
type MoveInput struct {
	AccountID   string
	ApartmentID string // "" = keep
	SiteID      string // "" = the apartment's only router; error if several
	Unit        string // "" = keep
	ExpectMAC   string // when set, the account must still have this MAC inside the update, or ErrDeviceChanged
}

// MoveToApartment changes an account's apartment (and router, picked from the
// new apartment's routers) and, when the router changes, takes the address
// from the new router's range. The old router's lease, queue and ARP row are
// removed and the new router gets them, both by the reconciler (one task per
// router), and the move is recorded in the account's history. The customer,
// package and paid time stay.
func (s *Service) MoveToApartment(ctx context.Context, in MoveInput) (store.Account, error) {
	var from, to string
	var out store.Account
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[in.AccountID]
		if !ok {
			return ErrNotFound
		}
		if in.ExpectMAC != "" && !strings.EqualFold(a.MAC, in.ExpectMAC) {
			return ErrDeviceChanged
		}
		aptID := in.ApartmentID
		if aptID == "" {
			aptID = a.ApartmentID
		}
		siteID := in.SiteID
		if siteID == "" && aptID == a.ApartmentID && d.ApartmentServedBy(aptID, a.SiteID) {
			siteID = a.SiteID // staying in the apartment: keep the router unless told otherwise
		}
		aptID, siteID, err := s.resolveLocked(d, aptID, siteID)
		if err != nil {
			return err
		}
		unit := a.Unit
		if u := cleanText(in.Unit, 40); u != "" {
			unit = u
		}
		if aptID == a.ApartmentID && siteID == a.SiteID && unit == a.Unit {
			return store.ErrNoChange
		}
		if err := unitTakenLocked(d, aptID, unit, a.ID); err != nil {
			return err
		}
		old, oldApt := d.Sites[a.SiteID], d.Apartments[a.ApartmentID]
		from, to = a.SiteID, siteID
		if siteID != a.SiteID {
			s.enqueueLocked(d, a, "account moved away") // the old router drops it
			a.SiteID, a.IP = siteID, ""
			pl, err := s.PlanFor(d.Sites[siteID])
			if err != nil {
				return err
			}
			if a.IP, err = FreeIPLocked(d, siteID, pl, s.foreignIPs(siteID)); err != nil {
				return err
			}
			for k, o := range d.Orders {
				if o.AccountID == a.ID && o.Status == store.OrderPending {
					o.Status, o.Reason = store.OrderFailed, "account moved to another building"
					d.Orders[k] = o
				}
			}
		}
		was := a.Unit
		a.ApartmentID, a.Unit, a.Flat = aptID, unit, unit
		d.Accounts[a.ID] = a
		delete(d.Devices, store.DeviceKey(a.SiteID, a.MAC))
		s.enqueueLocked(d, a, "account moved in")
		d.AddEvent(store.Event{Time: s.now(), SiteID: a.SiteID, Kind: "info", AccountID: a.ID,
			Msg: fmt.Sprintf("%s (%s): moved from %s unit %s on %s to %s unit %s on %s", unit, a.Code, nameOr(oldApt.Name, "no apartment"), was, nameOr(old.Name, "?"),
				d.Apartments[aptID].Name, unit, d.Sites[siteID].Name)})
		out = a
		return nil
	})
	if err == nil {
		s.runSitesShort(ctx, to, from) // both at once: the new router never waits for the old one
	}
	return out, err
}

// SortedUnits lists the units of the accounts, for messages.
func SortedUnits(as []store.Account) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Unit)
	}
	sort.Slice(out, func(i, j int) bool { return store.NaturalLess(out[i], out[j]) })
	return out
}
