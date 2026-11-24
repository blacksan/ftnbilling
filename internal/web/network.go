package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The Network page: every apartment (building) on a map (MapLibre GL JS, OpenFreeMap tiles: no key, no account),
// with the equipment installed there. It is
// separate from Equipment (the registers): Equipment is the list of things, Network is where they are.
//
//	/admin/network            the map whenever an apartment has a GPS point, else the list view
//	/admin/network?view=list  the same data as cards, with "View on Google Maps" links
//	/admin/network/map.json   the data both views and static/netmap.js read (any signed-in role, like the routers list)
//
// The old pages stay: Live now (/admin/live) and the server drawing (/admin/map, "Topology") are tabs of this page.

// Marker states, worst first. The Android app uses the same words (NetworkMarkerState).
const (
	netOK      = "ok"      // every router up, every ONU online
	netPartial = "partial" // some ONU offline, or a router paused/not joined
	netDown    = "down"    // routers serve it and none answers
	netUnknown = "unknown" // no router serves it
)

type netRouter struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // up | down | paused | waiting
	URL   string `json:"url"`
}

type netONU struct {
	ID        string `json:"id"`
	Serial    string `json:"serial"`
	Model     string `json:"model,omitempty"`
	House     string `json:"house,omitempty"`
	Customer  string `json:"customer,omitempty"`
	Online    bool   `json:"online"`
	Status    string `json:"status"` // online | offline
	OLT       string `json:"olt,omitempty"`
	PON       int    `json:"pon,omitempty"`
	URL       string `json:"url"`
	AccountID string `json:"account_id,omitempty"`
	Account   string `json:"account_url,omitempty"`
}

type netOLT struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Router     string `json:"router,omitempty"`
	PONs       []int  `json:"pons"`
	ONUsOnline int    `json:"onus_online"`
	ONUsTotal  int    `json:"onus_total"`
	URL        string `json:"url"`
}

type netOther struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Status string `json:"status"`
	House  string `json:"house,omitempty"`
	URL    string `json:"url"`
}

type netHouses struct {
	Active int `json:"active"`
	Total  int `json:"total"`
	Online int `json:"online"`
}

// netApt is one apartment on the Network page.
type netApt struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Address    string      `json:"address"`
	HasGPS     bool        `json:"has_gps"`
	Lat        *float64    `json:"lat"`
	Lng        *float64    `json:"lng"`
	State      string      `json:"state"`
	URL        string      `json:"url"`
	EditURL    string      `json:"edit_url"`
	MapsURL    string      `json:"maps_url,omitempty"`
	Houses     netHouses   `json:"houses"`
	Routers    []netRouter `json:"routers"`
	OLTs       []netOLT    `json:"olts"`
	ONUs       []netONU    `json:"onus"`
	Other      []netOther  `json:"other"`
	ONUsOnline int         `json:"onus_online"`
	ONUsTotal  int         `json:"onus_total"`
	Alerts     int         `json:"open_alerts"`
}

type netMapData struct {
	Apartments []netApt `json:"apartments"`
	WithGPS    int      `json:"with_gps"`
	NoGPS      int      `json:"without_gps"`
	Down       int      `json:"down"`
	Partial    int      `json:"partial"`
	OK         int      `json:"ok"`
}

// googleMapsLink is the one-click "View on Google Maps" address for a point.
func googleMapsLink(lat, lng float64) string {
	return "https://www.google.com/maps/search/?api=1&query=" + store.FormatGPS(lat, lng)
}

// netMarkerState says how an apartment's marker looks. routers is every router that serves it (state up, down,
// paused or waiting), and the ONU counts are the ones installed in its houses.
//
//	down     the apartment has routers and none of them is up (at least one is down)
//	partial  a router is paused, not joined or down while another is up, or some ONU is offline
//	ok       every router is up and every ONU is online
//	unknown  no router serves it
func netMarkerState(routers []netRouter, onusOnline, onusTotal int) string {
	if len(routers) == 0 {
		return netUnknown
	}
	up, down := 0, 0
	for _, r := range routers {
		switch r.State {
		case "up":
			up++
		case "down":
			down++
		}
	}
	switch {
	case up == 0 && down > 0:
		return netDown
	case up != len(routers), onusOnline < onusTotal:
		return netPartial
	}
	return netOK
}

// netMapStyleURL is the OpenFreeMap style the map draws (no key, no account). Positron is the calm light grey one: the
// apartment markers (teal, slate, red) are the only saturated colours on it, which Liberty's coloured land use and
// buildings would compete with. Its sprite, glyphs and tiles are all on the same host the CSP allows (see cspNetwork).
const netMapStyleURL = "https://tiles.openfreemap.org/styles/positron"

// networkData builds the page and the JSON for the routers a request may see.
func (s *Server) networkData(ctx context.Context, sc scope) netMapData {
	out := netMapData{Apartments: []netApt{}}
	states := s.siteStates(ctx, sc.Sites)
	tree := s.treeFor(sc)
	sums := s.svc.Apartments()

	byApt := map[string]*netApt{}
	var order []string
	for _, sm := range sums {
		served := false
		for _, st := range sm.Routers {
			if sc.Has(st.ID) {
				served = true
			}
		}
		if !sc.All && !served {
			continue
		}
		a := &netApt{ID: sm.ID, Name: sm.Name, Address: sm.Address, URL: aptURL(sm.ID),
			EditURL: "/admin/apartments/edit?id=" + url.QueryEscape(sm.ID),
			Houses:  netHouses{Active: sm.Active, Total: sm.Total, Online: sm.Online},
			Routers: []netRouter{}, OLTs: []netOLT{}, ONUs: []netONU{}, Other: []netOther{}}
		if sm.HasGPS() {
			lat, lng := *sm.Lat, *sm.Lng // copies: never point into the store
			a.HasGPS, a.Lat, a.Lng = true, &lat, &lng
			a.MapsURL = googleMapsLink(lat, lng)
		}
		for _, st := range sm.Routers {
			if !sc.Has(st.ID) {
				continue
			}
			a.Routers = append(a.Routers, netRouter{ID: st.ID, Name: st.Name, State: states[st.ID], URL: "/admin/routers/view?id=" + url.QueryEscape(st.ID)})
			if s.ops.Alerts != nil {
				a.Alerts += s.ops.Alerts.OpenCount(st.ID)
			}
		}
		byApt[sm.ID] = a
		order = append(order, sm.ID)
	}

	addONU := func(a *netApt, n billing.TreeONU, olt store.Equipment, port int) {
		o := netONU{ID: n.Equip.ID, Serial: n.Equip.Serial, Model: n.Equip.Model, House: n.House, Customer: n.Customer, Online: n.Online,
			Status: "offline", PON: port, URL: "/admin/equipment/view?id=" + url.QueryEscape(n.Equip.ID)}
		if o.Serial == "" {
			o.Serial = n.Equip.GPONSerial
		}
		if n.Online {
			o.Status = "online"
			a.ONUsOnline++
		}
		if olt.ID != "" {
			o.OLT = store.OLTLabel(olt)
		}
		if n.AccountID != "" {
			o.AccountID = n.AccountID
		}
		if n.Code != "" {
			o.Account = "/admin/accounts/view?code=" + url.QueryEscape(n.Code)
		}
		a.ONUs = append(a.ONUs, o)
		a.ONUsTotal++
	}
	for _, tr := range tree.Routers {
		for _, to := range tr.OLTs {
			per := map[string]*netOLT{}
			get := func(a *netApt) *netOLT {
				if o := per[a.ID]; o != nil {
					return o
				}
				o := &netOLT{ID: to.Equip.ID, Name: to.Label, Router: tr.Site.Name, PONs: []int{}, URL: "/admin/olts/view?id=" + url.QueryEscape(to.Equip.ID)}
				per[a.ID] = o
				return o
			}
			for _, p := range to.PONs {
				for _, ta := range p.Apartments { // a PON port serves the apartment even before an ONU is there
					if a := byApt[ta.ID]; a != nil {
						o := get(a)
						o.PONs = appendUniqueInt(o.PONs, p.Port)
					}
				}
				for _, n := range p.ONUs {
					if a := byApt[n.ApartmentID]; a != nil && n.ApartmentID != "" {
						o := get(a)
						o.PONs = appendUniqueInt(o.PONs, p.Port)
						o.ONUsTotal++
						if n.Online {
							o.ONUsOnline++
						}
						addONU(a, n, to.Equip, p.Port)
					}
				}
			}
			for id, o := range per {
				sort.Ints(o.PONs)
				byApt[id].OLTs = append(byApt[id].OLTs, *o)
			}
		}
	}
	for _, n := range tree.UnplacedONUs { // installed in a house, OLT not recorded yet
		if a := byApt[n.ApartmentID]; a != nil && n.ApartmentID != "" {
			addONU(a, n, store.Equipment{}, 0)
		}
	}

	// Other equipment (switches, splitters...) is placed through the house it is installed at.
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindONT || e.Kind == store.KindOLT || e.AccountID == "" {
				continue
			}
			acc, ok := d.AnyAccount(e.AccountID)
			if !ok {
				continue
			}
			if a := byApt[acc.ApartmentID]; a != nil {
				house := acc.Unit
				if house == "" {
					house = acc.Flat
				}
				label := strings.TrimSpace(strings.Join(nonEmpty(e.Model, e.Serial), " "))
				if label == "" {
					label = e.Kind
				}
				a.Other = append(a.Other, netOther{ID: e.ID, Kind: e.Kind, Label: label, Status: e.Status, House: house, URL: "/admin/equipment/view?id=" + url.QueryEscape(e.ID)})
			}
		}
	})

	for _, id := range order {
		a := byApt[id]
		sort.SliceStable(a.ONUs, func(i, j int) bool { return store.NaturalLess(a.ONUs[i].House, a.ONUs[j].House) })
		sort.SliceStable(a.Other, func(i, j int) bool { return a.Other[i].Label < a.Other[j].Label })
		a.State = netMarkerState(a.Routers, a.ONUsOnline, a.ONUsTotal)
		if a.HasGPS {
			out.WithGPS++
			switch a.State {
			case netDown:
				out.Down++
			case netPartial:
				out.Partial++
			case netOK:
				out.OK++
			}
		} else {
			out.NoGPS++
		}
		out.Apartments = append(out.Apartments, *a)
	}
	return out
}

func appendUniqueInt(l []int, n int) []int {
	for _, x := range l {
		if x == n {
			return l
		}
	}
	return append(l, n)
}

func nonEmpty(v ...string) []string {
	var out []string
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func (s *Server) networkRoutes(get func(string, http.HandlerFunc)) {
	get("/admin/network", s.adminNetwork)
	get("/admin/network/map.json", s.adminNetworkJSON)
}

func (s *Server) adminNetworkJSON(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), routeros.DefaultTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, s.networkData(ctx, s.scopeOf(r)))
}

// netTab is a tab of the Network page.
type netTab struct {
	Key, Label, Href string
	Current          bool
}

func (s *Server) adminNetwork(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "network")
	ctx, cancel := context.WithTimeout(r.Context(), routeros.DefaultTimeout)
	defer cancel()
	d := s.networkData(ctx, s.scopeOf(r))
	view := "map"
	if r.URL.Query().Get("view") == "list" {
		view = "list"
	}
	p.Data["Net"] = d
	p.Data["View"] = view
	// The map shows whenever some apartment has a GPS point; without one the page explains and shows the lists.
	p.Data["MapStyle"] = netMapStyleURL
	p.Data["MapOn"] = view == "map" && d.WithGPS > 0
	p.Data["NetTabs"] = []netTab{
		{"map", "Map", "/admin/network", view == "map"},
		{"list", "List", "/admin/network?view=list", view == "list"},
		{"topology", "Topology", "/admin/map", false},
		{"live", "Live now", "/admin/live", false},
	}
	var nogps []netApt
	for _, a := range d.Apartments {
		if !a.HasGPS {
			nogps = append(nogps, a)
		}
	}
	p.Data["NoGPS"] = nogps
	s.render(w, "admin_network", p)
}

// apiNetworkMap is GET /api/v1/network/map: the same data as /admin/network/map.json for the Android app (every
// building, like the other API lists).
func (s *Server) apiNetworkMap(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	ctx, cancel := context.WithTimeout(r.Context(), routeros.DefaultTimeout)
	defer cancel()
	d := s.networkData(ctx, scope{All: true, Sites: s.liveSites("")})
	writeJSON(w, http.StatusOK, d)
}
