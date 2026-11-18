package billing

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/store"
)

// EquipmentInput is the add/edit form of an equipment item.
type EquipmentInput struct {
	ID, Kind, Serial, MAC, Model, Firmware, SiteID, Notes string
}

var equipKinds = map[string]bool{store.KindONT: true, store.KindRouter: true, store.KindOLT: true, store.KindSplitter: true, store.KindSwitch: true, store.KindOther: true}

// EquipmentKinds lists the kinds in display order.
var EquipmentKinds = []string{store.KindONT, store.KindRouter, store.KindOLT, store.KindSplitter, store.KindSwitch, store.KindOther}

// EquipmentStatuses lists the statuses in display order.
var EquipmentStatuses = []string{store.EqInStock, store.EqInstalled, store.EqFaulty, store.EqReturned, store.EqRetired}

func addEqEvent(d *store.Data, e store.Equipment, kind, from, to, note string, at time.Time) {
	cust := ""
	if a, ok := d.AnyAccount(e.AccountID); ok {
		cust = a.CustomerID
	}
	ev := store.EquipmentEvent{ID: RandomID(), EquipmentID: e.ID, Kind: kind, FromStatus: from, ToStatus: to, SiteID: e.SiteID,
		AccountID: e.AccountID, CustomerID: cust, Note: cleanText(note, 200), Time: at}
	d.EquipmentEvents[ev.ID] = ev
}

func serialTaken(d *store.Data, kind, serial, exceptID string) bool {
	for _, e := range d.Equipment {
		if e.ID != exceptID && e.Kind == kind && strings.EqualFold(e.Serial, serial) && serial != "" {
			return true
		}
	}
	return false
}

// SaveEquipment adds a new item (ID empty) or edits one. A new item with a
// building and no account (a router, an OLT) is installed at the building;
// anything else starts in stock.
func (s *Service) SaveEquipment(in EquipmentInput) (store.Equipment, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if !equipKinds[kind] {
		return store.Equipment{}, errors.New("choose what kind of equipment this is")
	}
	serial := strings.ToUpper(cleanText(in.Serial, 60))
	mac := ""
	if strings.TrimSpace(in.MAC) != "" {
		m, err := NormalizeMAC(in.MAC)
		if err != nil {
			return store.Equipment{}, err
		}
		mac = m
	}
	var out store.Equipment
	err := s.st.Update(func(d *store.Data) error {
		if in.SiteID != "" {
			if st, ok := d.Sites[in.SiteID]; !ok || st.Removed {
				return ErrNoSite
			}
		}
		if serialTaken(d, kind, serial, in.ID) {
			return fmt.Errorf("a %s with serial number %s is already in the register", kind, serial)
		}
		now := s.now()
		e, exists := d.Equipment[in.ID]
		if in.ID != "" && !exists {
			return ErrNotFound
		}
		if !exists {
			e = store.Equipment{ID: RandomID(), Status: store.EqInStock, CreatedAt: now}
			if in.SiteID != "" && kind != store.KindONT {
				e.Status, e.InstalledAt = store.EqInstalled, now
			}
		}
		oldSite := e.SiteID
		e.Kind, e.Serial, e.MAC = kind, serial, mac
		e.Model, e.Firmware, e.Notes = cleanText(in.Model, 60), cleanText(in.Firmware, 40), cleanText(in.Notes, 300)
		if e.AccountID == "" {
			e.SiteID = in.SiteID
		}
		e.UpdatedAt = now
		d.Equipment[e.ID] = e
		switch {
		case !exists:
			addEqEvent(d, e, store.EqEvReceived, "", e.Status, "added to the register", now)
		case oldSite != e.SiteID:
			addEqEvent(d, e, store.EqEvMoved, e.Status, e.Status, "building changed", now)
		default:
			addEqEvent(d, e, store.EqEvEdited, e.Status, e.Status, "details edited", now)
		}
		out = e
		return nil
	})
	return out, err
}

// changeMACLocked is ChangeMAC's body inside an Update.
func (s *Service) changeMACLocked(d *store.Data, id, mac string) error {
	a, ok := d.Accounts[id]
	if !ok {
		return ErrNotFound
	}
	if other, ok := d.AccountByMAC(a.SiteID, mac); ok && other.ID != id {
		return fmt.Errorf("%w (%s)", ErrDuplicateMAC, other.Flat)
	}
	if strings.EqualFold(a.MAC, mac) {
		return nil
	}
	d.AddEvent(store.Event{Time: s.now(), SiteID: a.SiteID, Kind: "info", AccountID: id, Msg: fmt.Sprintf("%s: ONT MAC changed %s -> %s", a.Flat, a.MAC, mac)})
	a.MAC = mac
	d.Accounts[id] = a
	delete(d.Devices, store.DeviceKey(a.SiteID, mac))
	s.enqueueLocked(d, a, true, "MAC changed")
	return nil
}

// AssignEquipment installs an item at a flat (accountID) or at a building
// (siteID, no account). With useAsMAC, an ONT that has a MAC becomes the
// account's ONT MAC (like ChangeMAC).
func (s *Service) AssignEquipment(ctx context.Context, id, accountID, siteID string, useAsMAC bool) error {
	macChanged := ""
	err := s.st.Update(func(d *store.Data) error {
		e, ok := d.Equipment[id]
		if !ok {
			return ErrNotFound
		}
		if e.Status == store.EqInstalled || e.Status == store.EqRetired {
			return fmt.Errorf("this item is %s; return it first", strings.ReplaceAll(e.Status, "_", " "))
		}
		now := s.now()
		from := e.Status
		switch {
		case accountID != "":
			a, ok := d.Accounts[accountID]
			if !ok {
				return ErrNotFound
			}
			if e.Kind == store.KindSplitter {
				return errors.New("a splitter belongs to a building: set it as the flat's splitter in the location card")
			}
			e.AccountID, e.SiteID = a.ID, a.SiteID
			if useAsMAC && e.Kind == store.KindONT && e.MAC != "" {
				if err := s.changeMACLocked(d, a.ID, e.MAC); err != nil {
					return err
				}
				macChanged = a.ID
			}
		case siteID != "":
			if st, ok := d.Sites[siteID]; !ok || st.Removed {
				return ErrNoSite
			}
			e.AccountID, e.SiteID = "", siteID
		default:
			return errors.New("choose a flat or a building")
		}
		e.Status, e.InstalledAt, e.UpdatedAt = store.EqInstalled, now, now
		d.Equipment[id] = e
		addEqEvent(d, e, store.EqEvAssigned, from, e.Status, "", now)
		return nil
	})
	if err == nil && macChanged != "" {
		s.runAccountSite(ctx, macChanged)
	}
	return err
}

// returnLocked takes an item off its flat and gives it a new status.
func returnLocked(d *store.Data, key string, e store.Equipment, status, note string, now time.Time) {
	from := e.Status
	kind := store.EqEvReturned
	switch {
	case status == store.EqFaulty:
		kind = store.EqEvFaulty
	case status == store.EqInStock && from == store.EqFaulty:
		kind = store.EqEvRepaired
	case status == store.EqInStock:
		kind = store.EqEvUnassigned
	}
	addEqEvent(d, e, kind, from, status, note, now) // before the account link is cleared
	e.AccountID, e.Status, e.UpdatedAt = "", status, now
	if status != store.EqInstalled {
		e.SiteID = ""
	}
	d.Equipment[key] = e
	for k, a := range d.Accounts {
		if a.SplitterID == e.ID && status != store.EqInstalled {
			a.SplitterID, a.SplitterPort = "", 0
			d.Accounts[k] = a
		}
	}
}

// ReturnEquipment takes an item out of service: back in stock, returned by the
// customer, faulty, or retired.
func (s *Service) ReturnEquipment(id, status, note string) error {
	switch status {
	case store.EqInStock, store.EqReturned, store.EqFaulty, store.EqRetired:
	default:
		return errors.New("choose in stock, returned, faulty or retired")
	}
	return s.st.Update(func(d *store.Data) error {
		e, ok := d.Equipment[id]
		if !ok {
			return ErrNotFound
		}
		if e.Status == status && e.AccountID == "" {
			return store.ErrNoChange
		}
		returnLocked(d, id, e, status, note, s.now())
		return nil
	})
}

// EquipmentNote adds a free-text line to an item's history.
func (s *Service) EquipmentNote(id, text string) error {
	text = cleanText(text, 300)
	if text == "" {
		return errors.New("write a note first")
	}
	return s.st.Update(func(d *store.Data) error {
		e, ok := d.Equipment[id]
		if !ok {
			return ErrNotFound
		}
		addEqEvent(d, e, store.EqEvNote, e.Status, e.Status, text, s.now())
		return nil
	})
}

// SetLocation records where a flat is inside the building: floor, ODB box and
// the splitter (an equipment item of kind splitter) and port.
func (s *Service) SetLocation(accountID, floor, odb, splitterID string, port int) error {
	if port < 0 || port > 128 {
		return errors.New("splitter port must be between 0 and 128")
	}
	return s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		if splitterID != "" {
			sp, ok := d.Equipment[splitterID]
			if !ok || sp.Kind != store.KindSplitter {
				return errors.New("choose a splitter from the equipment register")
			}
		}
		floor, odb = cleanText(floor, 20), cleanText(odb, 40)
		if a.Floor == floor && a.ODB == odb && a.SplitterID == splitterID && a.SplitterPort == port {
			return store.ErrNoChange
		}
		a.Floor, a.ODB, a.SplitterID, a.SplitterPort = floor, odb, splitterID, port
		d.Accounts[accountID] = a
		return nil
	})
}

// ImportResult says what a CSV import did.
type ImportResult struct {
	Added   int
	Skipped []string // plain-words reasons
}

// ImportEquipment adds items from pasted CSV lines "kind,serial,mac,model".
// A line whose serial already exists is skipped, not an error.
func (s *Service) ImportEquipment(text string) (ImportResult, error) {
	var res ImportResult
	r := csv.NewReader(strings.NewReader(strings.TrimSpace(text)))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil {
		return res, fmt.Errorf("could not read the pasted lines: %w", err)
	}
	if len(rows) > 500 {
		return res, errors.New("import at most 500 lines at a time")
	}
	for i, row := range rows {
		get := func(n int) string {
			if n < len(row) {
				return strings.TrimSpace(row[n])
			}
			return ""
		}
		if i == 0 && strings.EqualFold(get(0), "kind") {
			continue // header line
		}
		if _, err := s.SaveEquipment(EquipmentInput{Kind: get(0), Serial: get(1), MAC: get(2), Model: get(3)}); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		res.Added++
	}
	return res, nil
}
