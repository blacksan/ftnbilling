package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"ispbill/internal/acs"
	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// Equipment is one place in the main navigation with four tabs (Routers, OLT, ONU, Other) and the hierarchy tree as a
// fifth view. Every tab is a plain link (/admin/equipment?tab=...), so it can be bookmarked and works without JS.
// The old addresses (/admin/routers, /admin/onus, /admin/equipment?view=list...) still work and light up Equipment.

// Tab keys, in display order.
const (
	eqTabRouters = "routers"
	eqTabOLT     = "olt"
	eqTabONU     = "onu"
	eqTabOther   = "other"
	eqTabTree    = "tree"
)

var eqTabKeys = []string{eqTabRouters, eqTabOLT, eqTabONU, eqTabOther, eqTabTree}

var eqTabLabels = map[string]string{eqTabRouters: "Routers", eqTabOLT: "OLT", eqTabONU: "ONU", eqTabOther: "Other", eqTabTree: "Tree"}

// eqHref is the address of a tab.
func eqHref(tab string) string { return "/admin/equipment?tab=" + tab }

// eqTabForKind says which tab lists a register kind: ont is ONU, olt is OLT, everything else is Other.
func eqTabForKind(kind string) string {
	switch kind {
	case store.KindONT:
		return eqTabONU
	case store.KindOLT:
		return eqTabOLT
	}
	return eqTabOther
}

// eqKindOf is the register kind a tab's add form creates ("" for tabs with a choice).
func eqIsOther(kind string) bool { return kind != store.KindONT && kind != store.KindOLT }

type eqTabLink struct {
	Key, Label, Href string
	N                int
	Count            bool // false for the tree view, which has no count
}

// eqCounts counts what each tab lists in the scope (the same predicate the lists use).
func (s *Server) eqCounts(sc scope, routers int) map[string]int {
	c := map[string]int{eqTabRouters: routers}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.SiteID != "" && !sc.Has(e.SiteID) {
				continue
			}
			c[eqTabForKind(e.Kind)]++
		}
	})
	return c
}

func (s *Server) eqTabLinks(r *http.Request) []eqTabLink {
	c := s.eqCounts(s.scopeOf(r), len(s.scopeOf(r).Sites))
	var out []eqTabLink
	for _, k := range eqTabKeys {
		out = append(out, eqTabLink{Key: k, Label: eqTabLabels[k], Href: eqHref(k), N: c[k], Count: k != eqTabTree})
	}
	return out
}

// eqPage is s.page for a page that lives under Equipment: the tab bar, the breadcrumb (Equipment > tab) and the nav.
func (s *Server) eqPage(r *http.Request, active, tab string) adminPage {
	p := s.page(r, active)
	p.Data["EqTabs"], p.Data["EqTab"] = s.eqTabLinks(r), tab
	p.eqCrumb(tab)
	return p
}

// eqCrumb makes the breadcrumb read Equipment / <tab>; a Detail added later reads Equipment / <tab> / <name>.
func (p *adminPage) eqCrumb(tab string) {
	p.Group, p.Label, p.Href = "Equipment", eqTabLabels[tab], eqHref(tab)
}

// eqTabOf is the tab a request asks for. The old Equipment list (?view=list, ?kind=, ?status=, ?q=) maps to the tab of
// its kind; no hint at all is the Routers tab.
func eqTabOf(r *http.Request) string {
	q := r.URL.Query()
	if t := q.Get("tab"); t != "" {
		for _, k := range eqTabKeys {
			if k == t {
				return t
			}
		}
	}
	if k := q.Get("kind"); k != "" {
		return eqTabForKind(k)
	}
	if q.Get("view") == "list" || q.Get("status") != "" || q.Get("q") != "" {
		return eqTabONU
	}
	return eqTabRouters
}

func (s *Server) adminEquipment(w http.ResponseWriter, r *http.Request) {
	switch eqTabOf(r) {
	case eqTabOLT:
		s.eqOLTs(w, r)
	case eqTabONU:
		s.eqONUs(w, r)
	case eqTabOther:
		s.eqOther(w, r)
	case eqTabTree:
		s.eqTree(w, r)
	default:
		s.adminSites(w, r)
	}
}

func (s *Server) eqTree(w http.ResponseWriter, r *http.Request) {
	p := s.eqPage(r, "equipment", eqTabTree)
	p.H1a, p.H1b, p.Lead = "Equipment", "tree.", "Router, OLT, PON port and ONU: what hangs off what."
	p.Data["Tree"] = s.treeFor(s.scopeOf(r))
	s.render(w, "admin_equipment_tree", p)
}

// ---- OLT tab ------------------------------------------------------------------------------------

type eqOLTRow struct {
	E          store.Equipment
	Label      string
	Router     string
	Apartments []string // served by its PON ports
	ONUs       int
	Online     int
	Warnings   int
}

func (s *Server) eqOLTs(w http.ResponseWriter, r *http.Request) {
	p := s.eqPage(r, "equipment", eqTabOLT)
	p.H1a, p.H1b, p.Lead = "Your", "OLTs.", "Each OLT, the router port it hangs off, the apartments its PON ports serve and how many ONUs are online."
	if p.CanWrite {
		p.Acts = []heroAct{{Label: "Add OLT", Href: "/admin/olts/new"}}
	}
	sc := s.scopeOf(r)
	qv := r.URL.Query()
	status, rtr, q := qv.Get("status"), qv.Get("router"), strings.TrimSpace(qv.Get("q"))
	info := map[string]billing.TreeOLT{}
	tree := s.treeFor(sc)
	for _, tr := range tree.Routers {
		for _, o := range tr.OLTs {
			info[o.Equip.ID] = o
		}
	}
	for _, o := range tree.UnplacedOLTs {
		info[o.Equip.ID] = o
	}
	names := p.Data["SiteNames"].(map[string]string)
	var rows []eqOLTRow
	counts := map[string]int{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind != store.KindOLT || (e.SiteID != "" && !sc.Has(e.SiteID)) {
				continue
			}
			if rtr != "" && e.SiteID != rtr {
				continue
			}
			if q != "" && !(store.ContainsFold(e.Name, q) || store.ContainsFold(e.Model, q) || store.ContainsFold(e.Serial, q) ||
				store.ContainsFold(strings.ReplaceAll(e.MAC, ":", ""), strings.ReplaceAll(q, ":", "")) || store.ContainsFold(e.MAC, q)) {
				continue
			}
			counts[e.Status]++
			if status != "" && e.Status != status {
				continue
			}
			row := eqOLTRow{E: e, Label: store.OLTLabel(e), Router: names[e.SiteID]}
			if t, ok := info[e.ID]; ok {
				row.ONUs, row.Online, row.Warnings = t.ONUCount, t.Online, t.Warnings
			}
			seen := map[string]bool{}
			for _, n := range d.PONPortNumbers(e) {
				for _, id := range e.PONApartmentIDs(n) {
					if a, ok := d.Apartments[id]; ok && !seen[id] {
						seen[id] = true
						row.Apartments = append(row.Apartments, a.Name)
					}
				}
			}
			sort.Strings(row.Apartments)
			rows = append(rows, row)
		}
	})
	sort.Slice(rows, func(i, j int) bool { return strings.ToLower(rows[i].Label) < strings.ToLower(rows[j].Label) })
	p.Data["Rows"], p.Data["Counts"], p.Data["Status"], p.Data["Router"], p.Data["Q"] = rows, counts, status, rtr, q
	p.Data["Statuses"], p.Data["Routers"] = billing.OLTStatuses, sc.Sites
	s.render(w, "admin_equipment_olts", p)
}

// ---- ONU tab ------------------------------------------------------------------------------------

type eqONURow struct {
	E         store.Equipment
	Router    string
	OLTLabel  string
	OLTID     string
	House     string // unit number
	Code      string
	Customer  string
	Apartment string
	TR069     bool
	CheckIn   time.Time
	Held      bool
	Online    bool
}

func (s *Server) eqONUs(w http.ResponseWriter, r *http.Request) {
	p := s.eqPage(r, "equipment", eqTabONU)
	p.H1a, p.H1b, p.Lead = "Your", "ONUs.", "The ONU register: serial, WAN MAC, where each one hangs, which house has it and when it last checked in over TR-069."
	if p.CanWrite {
		p.Acts = []heroAct{{Label: "Add ONU", Href: "#add"}, {Label: "TR-069 check-ins", Href: "/admin/onus", Ghost: true}}
	} else {
		p.Acts = []heroAct{{Label: "TR-069 check-ins", Href: "/admin/onus", Ghost: true}}
	}
	sc := s.scopeOf(r)
	qv := r.URL.Query()
	status, rtr, olt, q := qv.Get("status"), qv.Get("router"), qv.Get("olt"), strings.TrimSpace(qv.Get("q"))
	info := map[string]billing.TreeONU{}
	tree := s.treeFor(sc)
	add := func(o billing.TreeOLT) {
		for _, pon := range o.PONs {
			for _, n := range pon.ONUs {
				info[n.Equip.ID] = n
			}
		}
	}
	for _, tr := range tree.Routers {
		for _, o := range tr.OLTs {
			add(o)
		}
	}
	for _, o := range tree.UnplacedOLTs {
		add(o)
	}
	for _, n := range tree.UnplacedONUs {
		info[n.Equip.ID] = n
	}
	names := p.Data["SiteNames"].(map[string]string)
	var rows []eqONURow
	counts := map[string]int{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind != store.KindONT || (e.SiteID != "" && !sc.Has(e.SiteID)) {
				continue
			}
			site := e.SiteID
			if site == "" && e.OLTID != "" {
				site = d.Equipment[e.OLTID].SiteID
			}
			if rtr != "" && site != rtr {
				continue
			}
			if olt != "" && e.OLTID != olt {
				continue
			}
			row := eqONURow{E: e, Router: names[site], OLTID: e.OLTID}
			if o, ok := d.Equipment[e.OLTID]; ok && e.OLTID != "" {
				row.OLTLabel = store.OLTLabel(o)
			}
			if n, ok := info[e.ID]; ok {
				row.House, row.Code, row.Customer, row.Apartment = n.House, n.Code, n.Customer, n.Apartment
				row.TR069, row.CheckIn, row.Held, row.Online = n.TR069, n.TR069Inform, n.TR069Held, n.Online
			}
			if q != "" && !(store.ContainsFold(e.Serial, q) || store.ContainsFold(e.GPONSerial, q) || store.ContainsFold(e.TR069Serial, q) || store.ContainsFold(e.MAC, q) ||
				store.ContainsFold(strings.ReplaceAll(e.MAC, ":", ""), strings.ReplaceAll(q, ":", "")) || store.ContainsFold(e.Model, q) ||
				store.ContainsFold(row.House, q) || store.ContainsFold(row.Code, q) || store.ContainsFold(row.Customer, q) || store.ContainsFold(row.OLTLabel, q)) {
				continue
			}
			counts[e.Status]++
			if status != "" && e.Status != status {
				continue
			}
			rows = append(rows, row)
		}
	})
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].E.Serial != rows[j].E.Serial {
			return rows[i].E.Serial < rows[j].E.Serial
		}
		return rows[i].E.ID < rows[j].E.ID
	})
	p.Data["Rows"], p.Data["Counts"], p.Data["Status"], p.Data["Router"], p.Data["OLT"], p.Data["Q"] = rows, counts, status, rtr, olt, q
	p.Data["Statuses"], p.Data["Routers"], p.Data["OLTs"] = billing.EquipmentStatuses, sc.Sites, s.oltChoices(sc)
	p.Data["Flash2"] = qv.Get("skipped")
	if s.acs != nil {
		var unk []acs.UnknownONU
		for _, u := range s.acs.UnknownONUs("") {
			if (u.SiteID == "" || sc.Has(u.SiteID)) && (rtr == "" || u.SiteID == rtr) {
				unk = append(unk, u)
			}
		}
		p.Data["Unknown"] = unk
	}
	p.Data["AddBack"] = "/admin/equipment?tab=onu"
	// "Add to inventory" from an unknown ONU opens this page with the form prefilled.
	p.Data["AddSerial"], p.Data["AddModel"] = prefillText(qv.Get("add_serial")), prefillText(qv.Get("add_model"))
	s.render(w, "admin_equipment_onus", p)
}

// ---- Other tab ----------------------------------------------------------------------------------

func (s *Server) eqOther(w http.ResponseWriter, r *http.Request) {
	p := s.eqPage(r, "equipment", eqTabOther)
	p.H1a, p.H1b, p.Lead = "Other", "equipment.", "Switches, access points, splitters, UPS and every other item of the register that is not an OLT or an ONU."
	if p.CanWrite {
		p.Acts = []heroAct{{Label: "Add item", Href: "#add"}}
	}
	sc := s.scopeOf(r)
	qv := r.URL.Query()
	kind, status, q := qv.Get("kind"), qv.Get("status"), strings.TrimSpace(qv.Get("q"))
	if kind != "" && !eqIsOther(kind) {
		kind = ""
	}
	names := p.Data["SiteNames"].(map[string]string)
	var rows []eqRow
	counts := map[string]int{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if !eqIsOther(e.Kind) || (e.SiteID != "" && !sc.Has(e.SiteID)) || (kind != "" && e.Kind != kind) {
				continue
			}
			a, _ := d.AnyAccount(e.AccountID)
			if q != "" && !(store.ContainsFold(e.Serial, q) || store.ContainsFold(e.MAC, q) || store.ContainsFold(e.Model, q) || store.ContainsFold(e.Name, q) ||
				store.ContainsFold(a.Flat, q) || store.ContainsFold(a.Code, q) || store.ContainsFold(strings.ReplaceAll(e.MAC, ":", ""), strings.ReplaceAll(q, ":", ""))) {
				continue
			}
			counts[e.Status]++
			if status != "" && e.Status != status {
				continue
			}
			rows = append(rows, eqRow{e, a, names[e.SiteID]})
		}
	})
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].E, rows[j].E
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Serial < b.Serial
	})
	var kinds []string
	for _, k := range billing.EquipmentKinds {
		if eqIsOther(k) {
			kinds = append(kinds, k)
		}
	}
	p.Data["Rows"], p.Data["Counts"], p.Data["Kind"], p.Data["Status"], p.Data["Q"] = rows, counts, kind, status, q
	p.Data["Kinds"], p.Data["Statuses"] = kinds, billing.EquipmentStatuses
	p.Data["Flash2"] = qv.Get("skipped")
	s.render(w, "admin_equipment_other", p)
}

// prefillText keeps a value taken from the address bar short and printable before it goes into a form field.
func prefillText(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}
