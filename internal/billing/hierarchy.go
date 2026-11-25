package billing

import (
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// The equipment tree: Router > { Apartments (logical) | Equipment (physical): OLT > PON port > ONUs }. Built from the
// register and the account list; it reads the live "last seen" the sweeps keep, and writes nothing.

// TreeApartment is an apartment a router serves (or a PON port serves).
type TreeApartment struct{ ID, Name string }

// TreeONU is one ONU in the tree.
type TreeONU struct {
	Equip store.Equipment
	// House: the account the ONU is installed at (zero when not installed).
	AccountID, Code, House, ApartmentID, Apartment, Customer string
	Online                                                   bool
	LastSeen                                                 time.Time
	// TR069: the TR-069 record linked to the house or serial.
	TR069         bool
	TR069Inform   time.Time
	TR069Held     bool // quarantined
	Warnings      []string
	PlacementNote string
}

// TreePON is one PON port of an OLT.
type TreePON struct {
	Port       int // 0 = the ONUs of the OLT that name no PON port
	Apartments []TreeApartment
	ONUs       []TreeONU
	Online     int
}

// TreeOLT is one OLT with its PON ports.
type TreeOLT struct {
	Equip       store.Equipment
	Label       string
	HasPassword bool
	PONs        []TreePON
	ONUCount    int
	Online      int
	Warnings    int
}

// TreeRouter is one router with what it serves and what hangs off it.
type TreeRouter struct {
	Site       store.Site
	Apartments []TreeApartment
	OLTs       []TreeOLT
	ONUCount   int
	Online     int
}

// EquipmentTree is the whole hierarchy.
type EquipmentTree struct {
	Routers []TreeRouter
	// UnplacedOLTs are OLTs with no router (in stock, returned).
	UnplacedOLTs []TreeOLT
	// UnplacedONUs are ONUs that name no OLT (in stock, or installed with the OLT not recorded yet).
	UnplacedONUs []TreeONU
}

// Tree builds the equipment tree for the routers siteOK allows (nil = all). Removed routers are left out.
func (s *Service) Tree(siteOK func(siteID string) bool) EquipmentTree {
	var t EquipmentTree
	type seenInfo struct {
		last   time.Time
		online bool
	}
	seen := map[string]seenInfo{}
	var accts []string
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindONT && e.AccountID != "" {
				accts = append(accts, e.AccountID)
			}
		}
	})
	for _, id := range accts { // outside the store read: AccountDevice takes its own locks
		if ad, ok := s.AccountDevice(id); ok {
			seen[id] = seenInfo{ad.LastSeen, ad.Online}
		}
	}
	s.st.View(func(d *store.Data) {
		onu := func(e store.Equipment) TreeONU {
			n := TreeONU{Equip: e, Warnings: ONUWarnings(d, e)}
			if a, ok := d.AnyAccount(e.AccountID); ok && e.AccountID != "" {
				n.AccountID, n.Code, n.House, n.ApartmentID, n.Apartment = a.ID, a.Code, a.Unit, a.ApartmentID, d.Apartments[a.ApartmentID].Name
				if n.House == "" {
					n.House = a.Flat
				}
				n.Customer = accountCustomerName(d, a)
				if si, ok := seen[a.ID]; ok {
					n.Online, n.LastSeen = si.online, si.last
				}
				if o, ok := d.ONUOfAccount(a.ID); ok {
					n.TR069, n.TR069Inform, n.TR069Held = true, o.LastInform, o.Sec.Quarantine.Active
				}
			}
			if !n.TR069 {
				for _, o := range d.ONUsOfSerial(e.Serial, e.GPONSerial, e.TR069Serial) {
					n.TR069, n.TR069Inform, n.TR069Held = true, o.LastInform, o.Sec.Quarantine.Active
					break
				}
			}
			return n
		}
		olt := func(o store.Equipment) TreeOLT {
			to := TreeOLT{Equip: o, Label: store.OLTLabel(o), HasPassword: o.MgmtPass != ""}
			byPON := map[int]*TreePON{}
			get := func(n int) *TreePON {
				p := byPON[n]
				if p == nil {
					p = &TreePON{Port: n}
					for _, id := range o.PONApartmentIDs(n) {
						p.Apartments = append(p.Apartments, TreeApartment{id, d.Apartments[id].Name})
					}
					byPON[n] = p
				}
				return p
			}
			for _, n := range d.PONPortNumbers(o) {
				get(n)
			}
			for _, e := range d.ONUsOfOLT(o.ID, 0) {
				p := get(e.PONPort)
				n := onu(e)
				p.ONUs = append(p.ONUs, n)
				if n.Online {
					p.Online++
				}
				to.ONUCount++
				if n.Online {
					to.Online++
				}
				to.Warnings += len(n.Warnings)
			}
			ports := make([]int, 0, len(byPON))
			for n := range byPON {
				ports = append(ports, n)
			}
			sort.Ints(ports)
			for _, n := range ports {
				to.PONs = append(to.PONs, *byPON[n])
			}
			return to
		}
		for _, st := range d.SiteList() {
			if st.Removed || (siteOK != nil && !siteOK(st.ID)) {
				continue
			}
			tr := TreeRouter{Site: st}
			for _, id := range d.SiteApartmentIDs(st.ID) {
				tr.Apartments = append(tr.Apartments, TreeApartment{id, d.Apartments[id].Name})
			}
			for _, o := range d.OLTsOfSite(st.ID) {
				to := olt(o)
				tr.OLTs = append(tr.OLTs, to)
				tr.ONUCount += to.ONUCount
				tr.Online += to.Online
			}
			t.Routers = append(t.Routers, tr)
		}
		if siteOK == nil || siteOK("") {
			for _, o := range d.UnplacedOLTs() {
				t.UnplacedOLTs = append(t.UnplacedOLTs, olt(o))
			}
			for _, e := range d.UnplacedONUs() {
				t.UnplacedONUs = append(t.UnplacedONUs, onu(e))
			}
		}
	})
	return t
}

// ServedBy is the "Apartment page" view: which OLT and PON ports serve an apartment, and their ONUs in it.
type ServedBy struct {
	OLT  store.Equipment
	Port int
	ONUs []TreeONU
}

// ApartmentServedBy lists the OLT PON ports that serve an apartment, with the ONUs of its houses.
func (s *Service) ApartmentServedBy(apartmentID string) []ServedBy {
	tree := s.Tree(nil)
	var out []ServedBy
	for _, r := range tree.Routers {
		for _, o := range r.OLTs {
			for _, p := range o.PONs {
				serves := false
				for _, a := range p.Apartments {
					serves = serves || a.ID == apartmentID
				}
				if !serves {
					continue
				}
				sb := ServedBy{OLT: o.Equip, Port: p.Port}
				for _, n := range p.ONUs {
					if n.ApartmentID == apartmentID {
						sb.ONUs = append(sb.ONUs, n)
					}
				}
				out = append(out, sb)
			}
		}
	}
	return out
}

// PONServesText lists the apartment names of a PON port ("MG One, Block B").
func (p TreePON) PONServesText() string {
	var n []string
	for _, a := range p.Apartments {
		n = append(n, a.Name)
	}
	return strings.Join(n, ", ")
}
