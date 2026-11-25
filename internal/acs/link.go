package acs

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"ispbill/internal/store"
)

// Linking an unknown check-in to an inventory item.
//
// Some firmwares report a TR-069 serial that is neither the serial on the sticker nor the GPON serial (V-SOL V2804N-Z:
// "12345" + the label MAC). Such a box is listed as unknown although it is in the inventory. The owner can link the
// reported serial to the item (Equipment.TR069Serial); from then on the box is the registered ONU. Nothing is ever
// linked automatically: serials and MACs are spoofable, so the suggestions only help the owner choose.

var (
	ErrNotUnknown = errors.New("that serial is not on the unknown ONU list (it may have been added or expired): reload the list")
	ErrNoItem     = errors.New("that inventory item does not exist")
	ErrItemUnfit  = errors.New("only an ONT that is not retired and has no TR-069 serial yet can be linked")
	// ErrItemInUse: a box already checks in as the item under its serial or GPON serial, so the unknown check-in is not
	// that box (it would be a second one claiming the item and its house).
	ErrItemInUse = errors.New("that inventory item's ONU already checks in under its serial: the unknown ONU is another box")
)

const maxSuggestions = 5

// Suggestion is an inventory item an unknown check-in may be, with the reasons why.
type Suggestion struct {
	EquipmentID string   `json:"equipment_id"`
	Label       string   `json:"label"`
	Serial      string   `json:"serial,omitempty"`
	GPONSerial  string   `json:"gpon_serial,omitempty"`
	MAC         string   `json:"mac,omitempty"`
	Model       string   `json:"model,omitempty"`
	Why         []string `json:"why"`
	// Strength: 3 = the serial holds the item's MAC, 2 = the management MAC is near its WAN MAC, 1 = the model matches.
	Strength int `json:"strength"`
}

// isHex12 says whether s is 12 upper-case hex digits.
func isHex12(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func macFromHex(h string) string {
	var b strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

func normModel(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// modelLike: one model name holds the other (at least 3 characters, so "V" matches nothing).
func modelLike(a, b string) bool {
	a, b = normModel(a), normModel(b)
	if len(a) < 3 || len(b) < 3 {
		return false
	}
	return a == b || strings.Contains(a, b) || strings.Contains(b, a)
}

// itemHasRecord: a device record already checks in as the item (under its serial or GPON serial).
func itemHasRecord(d *store.Data, e store.Equipment) (store.ONU, bool) {
	if l := d.ONUsOfSerial(e.Serial, e.GPONSerial, e.TR069Serial); len(l) > 0 {
		return l[0], true
	}
	return store.ONU{}, false
}

// Suggest lists the inventory items (ONTs that are not retired, have no TR-069 serial and whose box does not already check
// in) the unknown check-in may be, strongest first. A suggestion is a hint for the owner, never a decision.
func Suggest(d *store.Data, u UnknownONU) []Suggestion {
	out := []Suggestion{}
	serial := strings.ToUpper(strings.Join(strings.Fields(u.Serial), ""))
	for _, e := range d.Equipment {
		if e.Kind != store.KindONT || e.Status == store.EqRetired || e.TR069Serial != "" {
			continue
		}
		if _, ok := itemHasRecord(d, e); ok {
			continue // its box already checks in: the unknown one is another box (a copy embedding its MAC, say)
		}
		var why []string
		strength := 0
		raise := func(n int) {
			if n > strength {
				strength = n
			}
		}
		if _, ok := store.CanonMAC(e.MAC); ok {
			// The firmware's serial holds the box's MAC (the label's, or the WAN's): any 12 hex digits in it that are
			// the item's MAC or next to it (same OUI, last 24 bits within macNearBy) count.
			for i := 0; i+12 <= len(serial); i++ {
				w := serial[i : i+12]
				if isHex12(w) && macNear(macFromHex(w), e.MAC) {
					why = append(why, fmt.Sprintf("MAC %s in the serial (inventory MAC %s)", macFromHex(w), macText(e.MAC)))
					raise(3)
					break
				}
			}
			if u.LeaseMAC != "" && macNear(u.LeaseMAC, e.MAC) {
				why = append(why, fmt.Sprintf("management MAC %s near WAN MAC %s", macText(u.LeaseMAC), macText(e.MAC)))
				raise(2)
			}
		}
		if e.Model != "" && (modelLike(u.Model, e.Model)) {
			why = append(why, fmt.Sprintf("model %s matches %s", nonEmpty(u.Model, "?"), e.Model))
			raise(1)
		}
		if c, ok := store.CanonMAC(e.MAC); ok && len(u.OUI) == 6 && strings.EqualFold(u.OUI, strings.ReplaceAll(c[:8], ":", "")) {
			why = append(why, "OUI "+strings.ToUpper(u.OUI)+" matches the item's MAC")
			raise(1)
		}
		if len(why) == 0 {
			continue
		}
		label := e.Serial
		if label == "" {
			label = e.GPONSerial
		}
		if e.Model != "" {
			label = strings.TrimSpace(e.Model + " " + label)
		}
		out = append(out, Suggestion{EquipmentID: e.ID, Label: label, Serial: e.Serial, GPONSerial: e.GPONSerial, MAC: e.MAC, Model: e.Model, Why: why, Strength: strength})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strength != out[j].Strength {
			return out[i].Strength > out[j].Strength
		}
		return out[i].EquipmentID < out[j].EquipmentID
	})
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	return out
}

// LinkUnknown is the owner's decision: the serial an unknown ONU reported becomes the TR-069 serial of the inventory
// item. The next Inform from that box is the registered ONU (its management MAC is still checked against the item's WAN
// MAC) and the entry leaves the unknown list. The caller has checked that the user is an owner.
func (s *Service) LinkUnknown(by, ip, equipmentID, serial string) (store.Equipment, error) {
	n := store.NormSerial(serial)
	if n == "" {
		return store.Equipment{}, ErrNotUnknown
	}
	var out store.Equipment
	err := s.st.Update(func(d *store.Data) error {
		now := s.now()
		var u UnknownONU
		found := false
		for _, x := range liveUnknown(d, readUnknown(d), now) {
			if store.NormSerial(x.Serial) == n {
				u, found = x, true
				break
			}
		}
		if !found {
			return ErrNotUnknown
		}
		e, ok := d.Equipment[equipmentID]
		if !ok {
			return ErrNoItem
		}
		if e.Kind != store.KindONT || e.Status == store.EqRetired || e.TR069Serial != "" {
			return ErrItemUnfit
		}
		for _, o := range d.Equipment {
			if o.ID == e.ID || o.Kind != store.KindONT {
				continue
			}
			if store.NormSerial(o.Serial) == n || store.NormSerial(o.GPONSerial) == n || store.NormSerial(o.TR069Serial) == n {
				return fmt.Errorf("an ONT with serial number %s is already in the register", u.Serial)
			}
		}
		if n == store.NormSerial(e.Serial) || n == store.NormSerial(e.GPONSerial) {
			return ErrItemUnfit
		}
		if o, ok := itemHasRecord(d, e); ok {
			return fmt.Errorf("%w (as %s)", ErrItemInUse, o.Serial)
		}
		e.TR069Serial, e.UpdatedAt = n, now
		d.Equipment[e.ID] = e
		d.ClearMACTrust(e.Serial, e.GPONSerial, e.TR069Serial)
		ev := store.EquipmentEvent{ID: newONUID(), EquipmentID: e.ID, Kind: store.EqEvEdited, FromStatus: e.Status, ToStatus: e.Status, SiteID: e.SiteID,
			AccountID: e.AccountID, Note: "TR-069 serial " + n + " linked", Time: now}
		d.EquipmentEvents[ev.ID] = ev
		d.AddEvent(store.Event{Time: now, SiteID: nonEmpty(e.SiteID, u.SiteID), Kind: "audit", AccountID: e.AccountID, Actor: by, IP: ip,
			Msg: fmt.Sprintf("Unknown ONU %s linked to inventory item %s (%s): its TR-069 serial is now %s", u.Serial, e.ID, nonEmpty(e.Serial, e.GPONSerial), n)})
		out = e
		return nil
	})
	return out, err
}
