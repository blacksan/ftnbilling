package billing

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/store"
)

// CustomerInput is the add/edit form of a customer.
type CustomerInput struct {
	ID                                         string // empty = new
	Name, Phone, Phone2, WhatsApp, Email, Note string
	WASamePhone, WAOptOut                      bool
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// syncAccountLocked copies the customer's contact details to an account (the
// copy messaging reads).
func syncAccountLocked(d *store.Data, accountID string, c store.Customer) {
	a, ok := d.Accounts[accountID]
	if !ok {
		return
	}
	a.Tenant, a.Phone, a.WhatsApp, a.WAOptOut = c.Name, c.Phone, c.WhatsApp, c.WAOptOut
	d.Accounts[accountID] = a
}

func closeTenancyLocked(d *store.Data, accountID string, at time.Time) {
	for k, t := range d.Tenancies {
		if t.AccountID == accountID && t.MoveOut.IsZero() {
			t.MoveOut = later(at, t.MoveIn)
			d.Tenancies[k] = t
		}
	}
}

func custEvent(d *store.Data, now time.Time, customerID, accountID, siteID, msg string) {
	d.AddEvent(store.Event{Time: now, SiteID: siteID, Kind: "info", AccountID: accountID, CustomerID: customerID, Msg: msg})
}

// SaveCustomer adds or edits a customer. Its name, phone, WhatsApp number and
// opt-out are copied to the accounts it currently occupies.
func (s *Service) SaveCustomer(in CustomerInput) (store.Customer, error) {
	name := cleanText(in.Name, 80)
	if name == "" {
		return store.Customer{}, errors.New("the customer's name is required")
	}
	phone, err := normPhone(in.Phone)
	if err != nil {
		return store.Customer{}, err
	}
	phone2, err := normPhone(in.Phone2)
	if err != nil {
		return store.Customer{}, errors.New("second phone: " + err.Error())
	}
	wa, err := waNumber(in.WhatsApp, in.WASamePhone, phone)
	if err != nil {
		return store.Customer{}, err
	}
	email := cleanText(in.Email, 120)
	if email != "" && !strings.Contains(email, "@") {
		return store.Customer{}, errors.New("that email address is not valid")
	}
	var out store.Customer
	err = s.st.Update(func(d *store.Data) error {
		now := s.now()
		c, ok := d.Customers[in.ID]
		if in.ID != "" && !ok {
			return ErrNotFound
		}
		if ok && !c.AnonymizedAt.IsZero() {
			return errors.New("this customer was anonymised and cannot be edited")
		}
		if !ok {
			c = store.Customer{ID: RandomID(), CreatedAt: now}
		}
		c.Name, c.Phone, c.Phone2, c.WhatsApp, c.WAOptOut = name, phone, phone2, wa, in.WAOptOut
		c.Email, c.Notes, c.UpdatedAt = email, cleanText(in.Note, 500), now
		d.Customers[c.ID] = c
		for _, a := range d.Accounts {
			if a.CustomerID == c.ID {
				syncAccountLocked(d, a.ID, c)
			}
		}
		if !ok {
			custEvent(d, now, c.ID, "", "", "customer added: "+c.Name)
		}
		out = c
		return nil
	})
	return out, err
}

// LinkCustomer makes the customer the current occupant of the account from
// moveIn on. The previous occupant's tenancy ends at that moment.
func (s *Service) LinkCustomer(accountID, customerID string, moveIn time.Time) error {
	return s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		c, ok := d.Customers[customerID]
		if !ok {
			return ErrNotFound
		}
		if !c.AnonymizedAt.IsZero() {
			return errors.New("an anonymised customer cannot be linked to a house")
		}
		if cur, has := d.OpenTenancy(accountID); has && cur.CustomerID == customerID {
			return store.ErrNoChange
		}
		now := s.now()
		if moveIn.IsZero() {
			moveIn = now
		}
		if moveIn.After(now.Add(24 * time.Hour)) {
			return errors.New("the move-in date cannot be in the future")
		}
		closeTenancyLocked(d, accountID, moveIn)
		t := store.Tenancy{ID: RandomID(), CustomerID: customerID, AccountID: accountID, MoveIn: moveIn}
		d.Tenancies[t.ID] = t
		a.CustomerID = customerID
		d.Accounts[accountID] = a
		syncAccountLocked(d, accountID, c)
		custEvent(d, now, customerID, accountID, a.SiteID, fmt.Sprintf("%s linked to %s", c.Name, a.Flat))
		return nil
	})
}

// MoveOut ends the current tenancy of an account. The flat becomes vacant and
// the old tenant's details are removed from the account, so no message goes to
// them any more.
func (s *Service) MoveOut(accountID string, at time.Time) error {
	return s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		t, has := d.OpenTenancy(accountID)
		if !has && a.CustomerID == "" {
			return store.ErrNoChange
		}
		now := s.now()
		if at.IsZero() {
			at = now
		}
		closeTenancyLocked(d, accountID, at)
		who := d.Customers[a.CustomerID].Name
		a.CustomerID, a.Tenant, a.Phone, a.WhatsApp, a.WAOptOut = "", "", "", "", false
		d.Accounts[accountID] = a
		custEvent(d, now, t.CustomerID, accountID, a.SiteID, fmt.Sprintf("%s moved out of %s", nameOr(who, "tenant"), a.Flat))
		return nil
	})
}

// AddNote files a free-text note under a customer, an account, a building or an item.
func (s *Service) AddNote(customerID, accountID, siteID, equipmentID, text string) error {
	text = cleanText(text, 1000)
	if text == "" {
		return errors.New("write a note first")
	}
	if customerID == "" && accountID == "" && siteID == "" && equipmentID == "" {
		return errors.New("a note must belong to something")
	}
	return s.st.Update(func(d *store.Data) error {
		n := store.Note{ID: RandomID(), CustomerID: customerID, AccountID: accountID, SiteID: siteID, EquipmentID: equipmentID, Text: text, Time: s.now()}
		d.Notes[n.ID] = n
		return nil
	})
}

// SetDataKey turns on the national-ID feature (32 random bytes from DATA_KEY).
func (s *Service) SetDataKey(key []byte) error {
	if len(key) == 0 {
		s.box = nil
		return nil
	}
	b, err := NewSecretbox(key)
	if err != nil {
		return err
	}
	s.box = b
	return nil
}

// NationalIDEnabled tells whether DATA_KEY is configured.
func (s *Service) NationalIDEnabled() bool { return s.box != nil }

// SetNationalID stores a customer's ID encrypted. An empty id removes it.
func (s *Service) SetNationalID(customerID, id string) error {
	if s.box == nil {
		return ErrNoDataKey
	}
	var enc, idx, last4 string
	if strings.TrimSpace(id) != "" {
		n, err := NormalizeNationalID(id)
		if err != nil {
			return err
		}
		if enc, err = s.box.Seal(n); err != nil {
			return err
		}
		idx, last4 = s.box.Index(n), n[len(n)-4:]
	}
	return s.st.Update(func(d *store.Data) error {
		c, ok := d.Customers[customerID]
		if !ok {
			return ErrNotFound
		}
		if !c.AnonymizedAt.IsZero() {
			return errors.New("this customer was anonymised")
		}
		c.NationalIDEnc, c.NationalIDHMAC, c.NationalIDLast4, c.UpdatedAt = enc, idx, last4, s.now()
		d.Customers[customerID] = c
		msg := "national ID set"
		if enc == "" {
			msg = "national ID removed"
		}
		d.AddEvent(store.Event{Time: s.now(), Kind: "audit", CustomerID: customerID, Msg: msg}) // never the number itself
		return nil
	})
}

// RevealNationalID decrypts a customer's ID. Every reveal is written to the
// audit log (as an event) in the same update, before the value is returned.
func (s *Service) RevealNationalID(customerID string) (string, error) {
	if s.box == nil {
		return "", ErrNoDataKey
	}
	var plain string
	err := s.st.Update(func(d *store.Data) error {
		c, ok := d.Customers[customerID]
		if !ok {
			return ErrNotFound
		}
		if c.NationalIDEnc == "" {
			return errors.New("no national ID is stored for this customer")
		}
		p, err := s.box.Open(c.NationalIDEnc)
		if err != nil {
			return err
		}
		d.AddEvent(store.Event{Time: s.now(), Kind: "audit", CustomerID: customerID, Msg: "national ID revealed by admin"})
		plain = p
		return nil
	})
	if err != nil {
		return "", err
	}
	return plain, nil
}

// FindByNationalID finds the customers whose stored ID equals id exactly.
func (s *Service) FindByNationalID(id string) []store.Customer {
	if s.box == nil {
		return nil
	}
	n, err := NormalizeNationalID(id)
	if err != nil {
		return nil
	}
	idx := s.box.Index(n)
	var out []store.Customer
	s.st.View(func(d *store.Data) {
		for _, c := range d.Customers {
			if c.NationalIDHMAC == idx {
				out = append(out, c)
			}
		}
	})
	return out
}

// AnonymizeCustomer scrubs a person's data (name, contacts, ID, notes) while
// keeping tenancies and the payment ledger. confirm must equal the name.
func (s *Service) AnonymizeCustomer(id, confirm string) error {
	return s.st.Update(func(d *store.Data) error {
		c, ok := d.Customers[id]
		if !ok {
			return ErrNotFound
		}
		if !c.AnonymizedAt.IsZero() {
			return store.ErrNoChange
		}
		if strings.TrimSpace(confirm) != c.Name {
			return fmt.Errorf("type the customer's name (%s) to confirm", c.Name)
		}
		now := s.now()
		for _, a := range d.Accounts {
			if a.CustomerID == id { // the flat becomes vacant for messaging
				a.CustomerID, a.Tenant, a.Phone, a.WhatsApp, a.WAOptOut = "", "", "", "", false
				d.Accounts[a.ID] = a
				closeTenancyLocked(d, a.ID, now)
			}
		}
		for k, n := range d.Notes {
			if n.CustomerID == id {
				delete(d.Notes, k)
			}
		}
		d.Customers[id] = store.Customer{ID: id, Name: "Anonymised customer", AnonymizedAt: now, CreatedAt: c.CreatedAt, UpdatedAt: now}
		custEvent(d, now, id, "", "", "customer anonymised")
		return nil
	})
}

// ---- building profile -------------------------------------------------------

// SiteProfileInput is the building profile form.
type SiteProfileInput struct {
	SiteID, Address string
	Lat, Lng        float64
	GoLive          time.Time
}

// SaveSiteProfile sets a building's address, GPS position and go-live date.
func (s *Service) SaveSiteProfile(in SiteProfileInput) error {
	if in.Lat < -90 || in.Lat > 90 || in.Lng < -180 || in.Lng > 180 {
		return errors.New("the GPS position is outside the valid range (latitude -90 to 90, longitude -180 to 180)")
	}
	if (in.Lat == 0) != (in.Lng == 0) {
		return errors.New("give both latitude and longitude, or neither")
	}
	return s.st.Update(func(d *store.Data) error {
		st, ok := d.Sites[in.SiteID]
		if !ok {
			return ErrNoSite
		}
		st.Address, st.Lat, st.Lng, st.GoLive = cleanText(in.Address, 200), in.Lat, in.Lng, in.GoLive
		d.Sites[in.SiteID] = st
		return nil
	})
}
