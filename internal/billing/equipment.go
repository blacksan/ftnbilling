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
	// GPONSerial is the OLT-side serial of an ONT when it differs from Serial (the TR-069 serial).
	GPONSerial string
	// TR069Serial is the serial the box reports in its CWMP DeviceId when it is neither Serial nor GPONSerial. nil leaves
	// the stored value as it is (callers that only change another field); a pointer to "" clears it.
	TR069Serial *string
	// SetPlacement applies OLTID and PONPort to an ONT (optional OLT and PON port); without it they are left as they are.
	SetPlacement bool
	OLTID        string
	PONPort      int
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
	if kind == store.KindOLT {
		return s.saveOLTGeneric(in) // an OLT has one set of rules, wherever it is saved from
	}
	serial := strings.ToUpper(cleanText(in.Serial, 60))
	gpon := strings.ToUpper(strings.Join(strings.Fields(cleanText(in.GPONSerial, 60)), ""))
	if gpon == strings.Join(strings.Fields(serial), "") {
		gpon = "" // the same serial twice says nothing
	}
	if gpon != "" && kind != store.KindONT {
		return store.Equipment{}, errors.New("only an ONT has a GPON serial")
	}
	tr069 := ""
	if in.TR069Serial != nil {
		tr069 = store.NormSerial(cleanText(*in.TR069Serial, 60))
		if tr069 == store.NormSerial(serial) || tr069 == store.NormSerial(gpon) {
			tr069 = "" // the same serial twice says nothing
		}
		if tr069 != "" && kind != store.KindONT {
			return store.Equipment{}, errors.New("only an ONT has a TR-069 serial")
		}
	}
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
		if kind == store.KindONT {
			// A serial (either kind) identifies one ONU: it cannot be on two inventory items. Retired items count
			// too, like serialTaken and the Postgres unique index on gpon_serial (JSON and Postgres refuse alike).
			for _, o := range d.Equipment {
				if o.ID == in.ID || o.Kind != store.KindONT {
					continue
				}
				for _, mine := range []string{serial, gpon, tr069} {
					if n := store.NormSerial(mine); n != "" && (store.NormSerial(o.Serial) == n || store.NormSerial(o.GPONSerial) == n || store.NormSerial(o.TR069Serial) == n) {
						return fmt.Errorf("an ONT with serial number %s is already in the register", mine)
					}
				}
			}
		}
		now := s.now()
		e, exists := d.Equipment[in.ID]
		if in.ID != "" && !exists {
			return ErrNotFound
		}
		if in.TR069Serial == nil {
			tr069 = e.TR069Serial // not part of this edit: kept
			if tr069 != "" && kind != store.KindONT {
				tr069 = ""
			}
		}
		if !exists {
			e = store.Equipment{ID: RandomID(), Status: store.EqInStock, CreatedAt: now}
			if in.SiteID != "" && kind != store.KindONT {
				e.Status, e.InstalledAt = store.EqInstalled, now
			}
		}
		if exists && e.Kind == store.KindOLT && kind != store.KindOLT {
			for _, o := range d.Equipment {
				if o.OLTID == e.ID {
					return errors.New("ONUs are placed under this OLT: move them to another OLT before changing what kind of equipment it is")
				}
			}
		}
		if kind == store.KindOLT && mac != "" && e.Status != store.EqRetired {
			if a, ok := d.AccountByMAC(mac); ok {
				return fmt.Errorf("that MAC is already the ONT of house %s (%s)", a.Unit, a.Code)
			}
		}
		if in.SetPlacement && kind == store.KindONT {
			if err := checkPlacementLocked(d, in.OLTID, in.PONPort, e.OLTID); err != nil {
				return err
			}
			e.OLTID, e.PONPort = in.OLTID, in.PONPort
		}
		if kind != store.KindONT {
			e.OLTID, e.PONPort = "", 0
		}
		if mac != "" && e.Status != store.EqRetired {
			for _, o := range d.Equipment {
				if o.ID != e.ID && o.Status != store.EqRetired && strings.EqualFold(o.MAC, mac) {
					return fmt.Errorf("%w (%s %s)", ErrDuplicateEquipmentMAC, o.Kind, o.Serial)
				}
			}
		}
		oldSite := e.SiteID
		if exists && e.Kind == store.KindONT && (!strings.EqualFold(e.MAC, mac) || store.NormSerial(e.TR069Serial) != tr069) {
			// a trusted MAC was trusted against the old WAN MAC, or for the box the old TR-069 serial named
			d.ClearMACTrust(e.Serial, e.GPONSerial, e.TR069Serial, serial, gpon, tr069)
		}
		e.Kind, e.Serial, e.MAC, e.GPONSerial, e.TR069Serial = kind, serial, mac, gpon, tr069
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
			addEqEvent(d, e, store.EqEvMoved, e.Status, e.Status, "router changed", now)
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
	if err := oltMACLocked(d, mac); err != nil {
		return err
	}
	if other, ok := d.AccountByMAC(mac); ok && other.ID != id {
		return duplicateMAC(d, other, a.ApartmentID)
	}
	if strings.EqualFold(a.MAC, mac) && !a.MACConflict {
		return nil
	}
	msg := fmt.Sprintf("%s: ONT MAC changed %s -> %s", a.Flat, a.MAC, mac)
	if strings.EqualFold(a.MAC, mac) {
		msg = fmt.Sprintf("%s: ONT MAC %s checked, no other house uses it", a.Flat, mac)
	}
	d.AddEvent(store.Event{Time: s.now(), SiteID: a.SiteID, Kind: "info", AccountID: id, Msg: msg})
	a.MAC, a.MACConflict = mac, false // back under the uniqueness rule
	d.Accounts[id] = a
	delete(d.Devices, store.DeviceKey(a.SiteID, mac))
	s.enqueueLocked(d, a, "MAC changed")
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
				return errors.New("a splitter is shared by several houses: set it as the house's splitter in the location card")
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
			return errors.New("choose a house or a router")
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
	if e.Kind == store.KindONT && e.AccountID != "" && from == store.EqInstalled {
		// The box leaves its house: its ONU row is unlinked now and a factory reset waits for its next check-in, so the
		// old tenant's Wi-Fi name and password do not go to whoever gets the box next.
		for _, o := range d.ONUsOfSerial(e.Serial, e.GPONSerial, e.TR069Serial) {
			if o.AccountID == e.AccountID {
				d.ReleaseONU(o.ID, now, "system")
			}
		}
	}
	if e.Kind == store.KindONT && status != store.EqInstalled {
		d.ClearMACTrust(e.Serial, e.GPONSerial, e.TR069Serial) // out of service (also when it was not at a house): no trusted MAC survives
	}
	e.AccountID, e.Status, e.UpdatedAt = "", status, now
	if status != store.EqInstalled && !(e.Kind == store.KindOLT && status == store.EqFaulty) {
		e.SiteID = "" // a faulty OLT stays under its router (its ONUs still hang off it)
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
		kind := get(0)
		if strings.EqualFold(kind, "onu") { // the team says ONU; the register's key is "ont"
			kind = store.KindONT
		}
		in := EquipmentInput{Kind: kind, Serial: get(1), MAC: get(2), Model: get(3), GPONSerial: get(4)}
		if !equipKinds[strings.ToLower(kind)] {
			// "serial,mac,model[,gpon serial]": the ONU shorthand
			if i == 0 && strings.EqualFold(get(0), "serial") {
				continue
			}
			in = EquipmentInput{Kind: store.KindONT, Serial: get(0), MAC: get(1), Model: get(2), GPONSerial: get(3)}
		}
		if _, err := s.SaveEquipment(in); err != nil {
			res.Skipped = append(res.Skipped, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		res.Added++
	}
	return res, nil
}

// saveOLTGeneric is SaveEquipment for kind olt (import, API, the generic form): a new OLT is refused (it needs a
// name and a router, which only the OLT form and /olts carry), and an edit of an OLT goes through SaveOLT, so the
// same rules apply: name, router when deployed, MAC uniqueness against equipment, accounts and routers.
func (s *Service) saveOLTGeneric(in EquipmentInput) (store.Equipment, error) {
	var cur store.Equipment
	found := false
	s.st.View(func(d *store.Data) { cur, found = d.Equipment[in.ID] })
	if in.ID == "" || !found || cur.Kind != store.KindOLT {
		return store.Equipment{}, errors.New("add an OLT with the OLT form (Equipment, Add OLT): it needs a name, a router and its MAC")
	}
	return s.SaveOLT(OLTInput{ID: cur.ID, Name: cur.Name, Model: in.Model, Serial: in.Serial, MAC: in.MAC, Firmware: in.Firmware, Notes: in.Notes,
		SiteID: in.SiteID, RouterPort: cur.RouterPort, PONPorts: cur.PONPorts, MgmtIP: cur.MgmtIP, MgmtUser: cur.MgmtUser})
}
