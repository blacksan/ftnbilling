package olt

import (
	"fmt"
	"net/netip"
	"strings"

	"ispbill/internal/store"
)

// Defaults of the generated profiles.
const (
	DefaultDBA        = "ftn_dba"
	DefaultONUProfile = "ftn_hgu"
	DefaultAssured    = 10000   // kbit/s
	DefaultMaximum    = 1000000 // kbit/s
)

// GPONSerialOf is the serial the OLT knows an inventory ONU by: its GPON serial, else its serial.
func GPONSerialOf(e store.Equipment) string {
	if e.GPONSerial != "" {
		return NormSerial(e.GPONSerial)
	}
	return NormSerial(e.Serial)
}

// BuildDesired computes the desired configuration of an OLT from the server's data. add lists ONUs to authorise (their
// serial, PON port and inventory item); every inventory ONU that is placed on this OLT and already authorised on it
// gets its services checked. baseOnly leaves the ONUs out; onusOnly leaves the base configuration out.
func BuildDesired(d *store.Data, olt store.Equipment, st store.OLTState, run *Running, add []DesiredONU, onusOnly bool) (Desired, []string, error) {
	var notes []string
	site, ok := d.Sites[olt.SiteID]
	if !ok || site.Removed {
		return Desired{}, nil, fmt.Errorf("the OLT is not on a router")
	}
	s := st.Settings
	w := Desired{DBA: DefaultDBA, DBAAssured: DefaultAssured, DBAMax: DefaultMaximum, ONUProfile: DefaultONUProfile}
	w.InternetVLAN = s.InternetVLAN
	if w.InternetVLAN == 0 {
		w.InternetVLAN = site.VLANID
	}
	w.InternetUntagged = s.InternetVLAN == 0 && site.VLANID == 0
	if w.InternetVLAN == 0 {
		w.InternetVLAN, w.InternetUntagged = 100, true
	}
	if site.ONUMgmtN > 0 {
		w.TR069VLAN = site.MgmtVLANOrDefault()
	}
	w.MgmtVLAN = s.MgmtVLANOrDefault()
	w.Uplink = s.UplinkOrDefault()
	// The OLT's management address: its register address when it is inside the router's OLT subnet.
	if site.ONUMgmtN > 0 && olt.MgmtIP != "" {
		sub := netip.MustParsePrefix(fmt.Sprintf("10.7.%d.0/29", site.ONUMgmtN))
		if a, err := netip.ParseAddr(olt.MgmtIP); err == nil && sub.Contains(a) {
			w.MgmtIP, w.MgmtMask, w.MgmtGateway = olt.MgmtIP, "255.255.255.248", fmt.Sprintf("10.7.%d.1", site.ONUMgmtN)
		} else {
			notes = append(notes, fmt.Sprintf("The OLT's address %s is not in the router's OLT network %s, so the plan leaves its management address alone.", olt.MgmtIP, sub))
		}
	}
	w.OnlyONUs = onusOnly
	// Placed ONUs already on the OLT keep their services.
	if run != nil {
		for _, e := range d.Equipment {
			if e.Kind != store.KindONT || e.Status == store.EqRetired || e.OLTID != olt.ID || e.PONPort <= 0 {
				continue
			}
			serial := GPONSerialOf(e)
			if serial == "" {
				continue
			}
			if pon, ro, ok := run.ONUBySerial(serial); ok && pon == e.PONPort {
				w.ONUs = append(w.ONUs, DesiredONU{PON: pon, ID: ro.ID, Serial: serial, EquipmentID: e.ID, Label: houseLabel(d, e)})
			}
		}
	}
	for _, a := range add {
		a.Add = true
		w.ONUs = append(w.ONUs, a)
	}
	return w, notes, nil
}

func houseLabel(d *store.Data, e store.Equipment) string {
	if e.AccountID == "" {
		return ""
	}
	a, ok := d.Accounts[e.AccountID]
	if !ok {
		return ""
	}
	l := a.Unit
	if ap, ok := d.Apartments[a.ApartmentID]; ok && ap.Name != "" {
		l = ap.Name + " " + a.Unit
	}
	return strings.TrimSpace(l)
}

// Verdicts for a found ONU.
const (
	VerdictReady   = store.OLTFoundReady
	VerdictUnknown = store.OLTFoundUnknown
	VerdictHeld    = store.OLTFoundHeld
)

// Classify decides what to do with a serial the OLT found on a PON port. It never adds anything itself.
func Classify(d *store.Data, olt store.Equipment, f Found) (verdict, detail, equipmentID string) {
	e, ok := d.ONTBySerial(f.Serial)
	if !ok {
		for _, x := range d.Equipment {
			if x.Kind == store.KindONT && x.Status == store.EqRetired && (GPONSerialOf(x) == f.Serial || NormSerial(x.Serial) == f.Serial) {
				return VerdictUnknown, "this serial is in the inventory as retired", x.ID
			}
		}
		return VerdictUnknown, "not in the inventory", ""
	}
	// Matched by the TR-069 serial only? Then the OLT serial is the same string; fine.
	if e.OLTID != "" && e.OLTID != olt.ID {
		name := "another OLT"
		if o, ok := d.Equipment[e.OLTID]; ok {
			name = store.OLTLabel(o)
		}
		return VerdictHeld, "the inventory has this ONU on " + name, e.ID
	}
	if e.OLTID == olt.ID && e.PONPort > 0 && f.PON > 0 && e.PONPort != f.PON {
		return VerdictHeld, fmt.Sprintf("the inventory has this ONU on PON %d, it was found on PON %d", e.PONPort, f.PON), e.ID
	}
	if e.AccountID == "" {
		return VerdictHeld, "in the inventory but not given to a house", e.ID
	}
	acct, ok := d.Accounts[e.AccountID]
	if !ok {
		return VerdictHeld, "its house is closed", e.ID
	}
	if acct.SiteID != olt.SiteID {
		return VerdictHeld, "its house is served by another router", e.ID
	}
	apt := acct.ApartmentID
	if apt == "" {
		return VerdictHeld, "its house has no apartment (building) set", e.ID
	}
	if f.PON > 0 {
		serves, known := olt.PONServes(f.PON, apt)
		switch {
		case known && serves:
			return VerdictReady, houseLabel(d, e), e.ID
		case known:
			return VerdictHeld, fmt.Sprintf("PON %d of this OLT does not serve this house's apartment (wrong PON port or wrong OLT)", f.PON), e.ID
		}
	}
	serves, known := olt.PONServes(0, apt)
	switch {
	case !known:
		return VerdictHeld, "this OLT has no PON map: set which apartments each PON port serves", e.ID
	case !serves:
		return VerdictHeld, "this OLT does not serve this house's apartment", e.ID
	}
	if f.PON == 0 {
		return VerdictHeld, "the OLT did not say which PON port it was found on", e.ID
	}
	return VerdictHeld, fmt.Sprintf("PON %d has no map; this house's apartment is served by another PON port", f.PON), e.ID
}
