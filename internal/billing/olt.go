package billing

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"ispbill/internal/store"
)

// The OLT is equipment of kind "olt": a router (Site) has OLTs, an OLT has PON ports, a PON port serves apartments
// and has ONUs (equipment of kind "ont"), an ONU belongs to a house. Everything here is a record in the register:
// nothing is written to a router or an OLT.

// ErrOLTMAC: a customer account cannot be given the MAC of an OLT.
var ErrOLTMAC = errors.New("That MAC belongs to an OLT")

// OLTMACError says which OLT. errors.Is(err, ErrOLTMAC) is true for it.
type OLTMACError struct{ Name, ID string }

func (e *OLTMACError) Error() string {
	if e.ID == "" {
		return "That MAC is a possible OLT seen on a router's OLT management port"
	}
	return "That MAC belongs to OLT " + e.Name
}
func (e *OLTMACError) Is(target error) bool { return target == ErrOLTMAC }

// oltMACLocked returns an *OLTMACError when mac is the MAC of a live OLT, else nil.
func oltMACLocked(d *store.Data, mac string) error {
	if o, ok := d.OLTByMAC(mac); ok {
		return &OLTMACError{Name: store.OLTLabel(o), ID: o.ID}
	}
	for _, dv := range d.Devices {
		if dv.Source == store.DeviceSourceOLTMgmt && strings.EqualFold(dv.MAC, mac) {
			return &OLTMACError{Name: "possible OLT"}
		}
	}
	return nil
}

// ErrConfirmNoAddress: an unknown device that never asked for an address is made a customer only when the
// operator confirmed it.
var ErrConfirmNoAddress = errors.New("This device has never asked for an address. Is it really a customer's ONU?")

// OLT statuses as the forms name them: stock, deployed (installed), faulty, returned.
var OLTStatuses = []string{store.EqInStock, store.EqInstalled, store.EqFaulty, store.EqReturned}

// MaxPONPorts bounds the PON port count of an OLT (a V-SOL V1600GS-R has 4, big chassis 16 per card).
const MaxPONPorts = 64

// OLTInput is the add/edit form of an OLT.
type OLTInput struct {
	ID, Name, Model, Serial, MAC, Firmware, Notes string
	SiteID, RouterPort                            string
	PONPorts                                      int
	// PONApartments maps a PON port number to the apartments it serves. nil keeps what is stored; a non-nil map
	// (even an empty one) replaces it.
	PONApartments map[int][]string
	MgmtIP        string
	MgmtUser      string
	// MgmtPass is the login password; "" keeps the stored one. ClearMgmtPass removes it.
	MgmtPass      string
	ClearMgmtPass bool
	// Status is one of OLTStatuses; "" keeps the current one (a new OLT: deployed when it has a router, else in stock).
	Status string
}

var mgmtAAD = []byte("olt.mgmt")

// errOLTNoKey is ErrNoDataKey in the OLT form's words.
var errOLTNoKey = &noKeyError{}

type noKeyError struct{}

func (*noKeyError) Error() string {
	return "the OLT password cannot be stored: set DATA_KEY on the server (openssl rand -base64 32), or leave the password empty"
}
func (*noKeyError) Is(t error) bool { return t == ErrNoDataKey }

func parseMgmtIP(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() {
		return "", errors.New("the management address must be an IP address like 192.168.1.10")
	}
	return a.Unmap().String(), nil
}

// oltMACTakenLocked: the MAC is on another live item or is a customer's ONT MAC.
func oltMACTakenLocked(d *store.Data, mac, exceptID string) error {
	for _, o := range d.Equipment {
		if o.ID != exceptID && o.Status != store.EqRetired && strings.EqualFold(o.MAC, mac) {
			return fmt.Errorf("%w (%s %s)", ErrDuplicateEquipmentMAC, o.Kind, firstNonEmpty(o.Name, o.Serial))
		}
	}
	if a, ok := d.AccountByMAC(mac); ok {
		return fmt.Errorf("that MAC is already the ONT of house %s (%s)", a.Unit, a.Code)
	}
	return nil
}

// SaveOLT adds an OLT (ID empty) or edits one. Validation: a name and a valid MAC that no other equipment item and no
// customer account uses; a router that exists, a router port not used by another OLT of that router; a PON port count;
// PON ports mapped only to apartments the router serves. The password is sealed (DATA_KEY) and never returned.
func (s *Service) SaveOLT(in OLTInput) (store.Equipment, error) {
	name := cleanText(in.Name, 60)
	if name == "" {
		return store.Equipment{}, errors.New("give the OLT a name, e.g. Block A OLT")
	}
	if strings.TrimSpace(in.MAC) == "" {
		return store.Equipment{}, errors.New("the OLT's MAC address is required")
	}
	mac, err := NormalizeMAC(in.MAC)
	if err != nil {
		return store.Equipment{}, err
	}
	if s.isRouterMAC(mac) {
		return store.Equipment{}, ErrRouterOwnMAC
	}
	if in.PONPorts < 0 || in.PONPorts > MaxPONPorts {
		return store.Equipment{}, fmt.Errorf("the number of PON ports must be between 0 and %d", MaxPONPorts)
	}
	ip, err := parseMgmtIP(in.MgmtIP)
	if err != nil {
		return store.Equipment{}, err
	}
	if in.Status != "" {
		ok := false
		for _, st := range OLTStatuses {
			ok = ok || st == in.Status
		}
		if !ok {
			return store.Equipment{}, errors.New("choose in stock, deployed, faulty or returned")
		}
	}
	var sealed string
	if in.MgmtPass != "" {
		if s.box == nil {
			return store.Equipment{}, errOLTNoKey
		}
		if len(in.MgmtPass) > 128 {
			return store.Equipment{}, errors.New("the password is too long")
		}
		if sealed, err = s.box.SealAAD(in.MgmtPass, mgmtAAD); err != nil {
			return store.Equipment{}, errors.New("could not store the password")
		}
	}
	serial := strings.ToUpper(cleanText(in.Serial, 60))
	port := cleanText(in.RouterPort, 30)
	var out store.Equipment
	err = s.st.Update(func(d *store.Data) error {
		now := s.now()
		e, exists := d.Equipment[in.ID]
		if in.ID != "" && (!exists || e.Kind != store.KindOLT) {
			return ErrNotFound
		}
		if serialTaken(d, store.KindOLT, serial, in.ID) {
			return fmt.Errorf("an olt with serial number %s is already in the register", serial)
		}
		if e.Status != store.EqRetired || in.Status != "" { // a retired OLT may keep a MAC another item now uses, until it is brought back
			if err := oltMACTakenLocked(d, mac, in.ID); err != nil {
				return err
			}
		}
		status := in.Status
		if status == "" {
			status = e.Status
		}
		if !exists {
			e = store.Equipment{ID: RandomID(), Kind: store.KindOLT, CreatedAt: now}
			if status == "" {
				status = store.EqInStock
				if in.SiteID != "" {
					status = store.EqInstalled
				}
			}
		}
		siteID := in.SiteID
		if status == store.EqInstalled && siteID == "" {
			return errors.New("a deployed OLT needs its router")
		}
		if status == store.EqInStock || status == store.EqReturned || status == store.EqRetired {
			siteID, port = "", "" // in the store room: not on a router
		}
		if siteID != "" {
			if st, ok := d.Sites[siteID]; !ok || st.Removed {
				return ErrNoSite
			}
			if port != "" {
				for _, o := range d.OLTsOfSite(siteID) {
					if o.ID != in.ID && strings.EqualFold(o.RouterPort, port) {
						return fmt.Errorf("router port %s already has OLT %s", port, store.OLTLabel(o))
					}
				}
			}
		}
		// The PON map follows the router: in stock it is empty, and when the router changes the apartments the new
		// router does not serve are dropped (the form still carries the old router's rows), not refused.
		pons := e.PONApartments
		switch {
		case siteID == "":
			pons = nil
		case in.PONApartments != nil:
			m := in.PONApartments
			if exists && siteID != e.SiteID {
				m = prunePONMap(d, siteID, m)
			}
			if pons, err = checkPONMap(d, siteID, in.PONPorts, m); err != nil {
				return err
			}
		case siteID != e.SiteID && len(pons) > 0: // the router changed under an existing mapping
			if pons, err = checkPONMap(d, siteID, in.PONPorts, prunePONMap(d, siteID, intKeys(pons))); err != nil {
				return err
			}
		}
		// A smaller port count must not strand ONUs on a port that no longer exists.
		if in.PONPorts > 0 {
			for _, o := range d.ONUsOfOLT(e.ID, 0) {
				if o.PONPort > in.PONPorts {
					return fmt.Errorf("ONU %s is on PON port %d: move it before reducing the OLT to %d ports", o.Serial, o.PONPort, in.PONPorts)
				}
			}
		}
		from, oldSite := e.Status, e.SiteID
		e.Name, e.Model, e.Serial, e.MAC, e.Firmware = name, cleanText(in.Model, 60), serial, mac, cleanText(in.Firmware, 40)
		e.Notes, e.SiteID, e.RouterPort, e.PONPorts, e.PONApartments = cleanText(in.Notes, 300), siteID, port, in.PONPorts, pons
		e.MgmtIP, e.MgmtUser = ip, cleanText(in.MgmtUser, 60)
		switch {
		case in.ClearMgmtPass:
			e.MgmtPass = ""
		case sealed != "":
			e.MgmtPass = sealed
		}
		if status == store.EqInstalled && e.Status != store.EqInstalled {
			e.InstalledAt = now
		}
		e.Status, e.UpdatedAt = status, now
		d.Equipment[e.ID] = e
		switch {
		case !exists:
			addEqEvent(d, e, store.EqEvReceived, "", e.Status, "OLT added to the register", now)
		case from != e.Status:
			kind := store.EqEvEdited
			switch e.Status {
			case store.EqFaulty:
				kind = store.EqEvFaulty
			case store.EqReturned:
				kind = store.EqEvReturned
			case store.EqInstalled:
				kind = store.EqEvAssigned
			case store.EqInStock:
				kind = store.EqEvUnassigned
			}
			addEqEvent(d, e, kind, from, e.Status, "", now)
		case oldSite != e.SiteID:
			addEqEvent(d, e, store.EqEvMoved, e.Status, e.Status, "router changed", now)
		default:
			addEqEvent(d, e, store.EqEvEdited, e.Status, e.Status, "OLT details edited", now) // never the password
		}
		out = e
		return nil
	})
	if err == nil {
		out.MgmtPass = "" // the caller never needs the sealed value
	}
	return out, err
}

// prunePONMap drops the apartments that do not exist or that the router does not serve.
func prunePONMap(d *store.Data, siteID string, in map[int][]string) map[int][]string {
	out := map[int][]string{}
	for n, ids := range in {
		for _, id := range ids {
			if _, ok := d.Apartments[id]; ok && d.ApartmentServedBy(id, siteID) {
				out[n] = append(out[n], id)
			}
		}
	}
	return out
}

func intKeys(m map[string][]string) map[int][]string {
	out := map[int][]string{}
	for k, v := range m {
		n := 0
		fmt.Sscanf(k, "%d", &n)
		out[n] = v
	}
	return out
}

// checkPONMap validates a PON mapping and returns it in store form: ports 1..max, apartments that exist and that the
// OLT's router serves (a PON port cannot serve an apartment the router does not). With no router nothing can be
// mapped.
func checkPONMap(d *store.Data, siteID string, ports int, in map[int][]string) (map[string][]string, error) {
	out := map[string][]string{}
	nports := make([]int, 0, len(in))
	for n := range in {
		nports = append(nports, n)
	}
	sort.Ints(nports)
	for _, n := range nports {
		ids := in[n]
		if len(ids) == 0 {
			continue
		}
		if n < 1 || (ports > 0 && n > ports) || n > MaxPONPorts {
			return nil, fmt.Errorf("PON port %d does not exist on this OLT (it has %d)", n, ports)
		}
		if siteID == "" {
			return nil, errors.New("choose the OLT's router before mapping PON ports to apartments")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			a, ok := d.Apartments[id]
			if !ok {
				return nil, errors.New("an apartment on PON port " + fmt.Sprint(n) + " does not exist")
			}
			if !d.ApartmentServedBy(id, siteID) {
				return nil, fmt.Errorf("PON port %d cannot serve %s: the router %s does not serve it", n, a.Name, d.Sites[siteID].Name)
			}
			out[store.PONKey(n)] = append(out[store.PONKey(n)], id)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// HasOLTPassword says whether a login password is stored (the password itself is never returned).
func HasOLTPassword(e store.Equipment) bool { return e.MgmtPass != "" }

// RevealOLTPassword opens the stored password. The caller must have re-checked the owner's password first (web:
// reauth). Every reveal is an audit event; the password is never logged.
func (s *Service) RevealOLTPassword(id string) (string, error) {
	if s.box == nil {
		return "", errOLTNoKey
	}
	var plain string
	err := s.st.Update(func(d *store.Data) error {
		e, ok := d.Equipment[id]
		if !ok || e.Kind != store.KindOLT {
			return ErrNotFound
		}
		if e.MgmtPass == "" {
			return errors.New("no password is stored for this OLT")
		}
		p, err := s.box.OpenAAD(e.MgmtPass, mgmtAAD)
		if err != nil {
			return errors.New("the stored password cannot be opened with this server's data key")
		}
		d.AddEvent(store.Event{Time: s.now(), SiteID: e.SiteID, EquipmentID: e.ID, Kind: "audit", Msg: "OLT login password revealed: " + store.OLTLabel(e)})
		plain = p
		return nil
	})
	return plain, err
}

// ---- ONU placement ------------------------------------------------------------------------------------------

// checkPlacementLocked validates an ONU's OLT and PON port. An OLT that is retired, faulty or in stock takes no new
// ONU (current is the OLT the ONU is already under: keeping it is not a new placement).
func checkPlacementLocked(d *store.Data, oltID string, pon int, current string) error {
	if oltID == "" {
		if pon != 0 {
			return errors.New("choose the OLT before the PON port")
		}
		return nil
	}
	o, ok := d.Equipment[oltID]
	if !ok || o.Kind != store.KindOLT {
		return errors.New("choose an OLT from the register")
	}
	if oltID != current && (o.Status == store.EqRetired || o.Status == store.EqFaulty || o.Status == store.EqInStock) {
		return fmt.Errorf("%s is %s: put the ONU under an OLT that is in service", store.OLTLabel(o), strings.ReplaceAll(o.Status, "_", " "))
	}
	if pon < 0 || pon > MaxPONPorts || (o.PONPorts > 0 && pon > o.PONPorts) {
		return fmt.Errorf("PON port %d does not exist on %s (it has %d)", pon, store.OLTLabel(o), o.PONPorts)
	}
	return nil
}

// ONUWarnings are the soft problems of an ONU's place: its house is in an apartment its PON port does not serve, or
// its OLT is on another router than the house. They warn; they never block.
func ONUWarnings(d *store.Data, e store.Equipment) []string {
	if e.Kind != store.KindONT || e.OLTID == "" || e.AccountID == "" {
		return nil
	}
	o, ok := d.Equipment[e.OLTID]
	if !ok {
		return nil
	}
	a, ok := d.AnyAccount(e.AccountID)
	if !ok {
		return nil
	}
	var out []string
	if serves, known := o.PONServes(e.PONPort, a.ApartmentID); known && !serves && a.ApartmentID != "" {
		where := "PON port " + fmt.Sprint(e.PONPort)
		if e.PONPort == 0 {
			where = "no PON port"
		}
		names := ponApartmentNames(d, o, e.PONPort)
		out = append(out, fmt.Sprintf("House %s is in %s, but %s of %s serves %s.", a.Unit, d.Apartments[a.ApartmentID].Name, where, store.OLTLabel(o), names))
	}
	if o.SiteID != "" && a.SiteID != "" && o.SiteID != a.SiteID {
		out = append(out, fmt.Sprintf("House %s is served by router %s, but %s is on router %s.", a.Unit, d.Sites[a.SiteID].Name, store.OLTLabel(o), d.Sites[o.SiteID].Name))
	}
	return out
}

func ponApartmentNames(d *store.Data, o store.Equipment, port int) string {
	var ids []string
	if port > 0 {
		ids = o.PONApartmentIDs(port)
	} else {
		for _, v := range o.PONApartments {
			ids = append(ids, v...)
		}
	}
	var names []string
	for _, id := range ids {
		names = append(names, d.Apartments[id].Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ONUApartmentIDs lists the apartments the PON port of an in-register ONU serves (the ones to preselect when a
// customer is created from it). With no PON port it is every apartment of its OLT; with no mapping, none.
func ONUApartmentIDs(d *store.Data, e store.Equipment) []string {
	o, ok := d.Equipment[e.OLTID]
	if !ok || e.OLTID == "" {
		return nil
	}
	var ids []string
	if e.PONPort > 0 {
		ids = append(ids, o.PONApartmentIDs(e.PONPort)...)
	} else {
		seen := map[string]bool{}
		for _, v := range o.PONApartments {
			for _, id := range v {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
		sort.Strings(ids)
	}
	return ids
}

// ONUPlacement is what the register says about where an ONU is.
type ONUPlacement struct {
	OLTID, OLTName, Router, RouterID string
	PONPort                          int
	ApartmentIDs                     []string
	Warnings                         []string
}

// PlacementOf describes an ONU's OLT, PON port and the apartments served.
func (s *Service) PlacementOf(equipID string) (p ONUPlacement) {
	s.st.View(func(d *store.Data) {
		e, ok := d.Equipment[equipID]
		if !ok {
			return
		}
		p = placementLocked(d, e)
	})
	return p
}

func placementLocked(d *store.Data, e store.Equipment) (p ONUPlacement) {
	if o, ok := d.Equipment[e.OLTID]; ok && e.OLTID != "" {
		p = ONUPlacement{OLTID: o.ID, OLTName: store.OLTLabel(o), RouterID: o.SiteID, Router: d.Sites[o.SiteID].Name, PONPort: e.PONPort,
			ApartmentIDs: ONUApartmentIDs(d, e)}
	}
	p.Warnings = ONUWarnings(d, e)
	return p
}
