package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func (s *Server) deviceRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/devices", s.adminDevices)
	post("/admin/devices/create", s.adminDeviceCreate)
	post("/admin/devices/attach", s.adminDeviceAttach)
	post("/admin/devices/dismiss", s.adminDeviceDismiss)
	post("/admin/devices/move", s.adminDeviceMove)
	post("/admin/registrations/approve", s.adminRegApprove)
	post("/admin/registrations/reject", s.adminRegReject)
	post("/admin/registrations/reject-all", s.adminRegRejectAll)
	post("/admin/registrations/assign", s.adminRegAssign)
	get("/admin/sites/devices.json", s.adminRouterDevices)
}

// adminRouterDevices lists the devices a router sees that no flat owns, for the
// "pick from the router" list in Add customer. It reads the router first so
// the list is current; if that fails the last known devices are still listed.
func (s *Server) adminRouterDevices(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("router")
	if id == "" {
		id = r.URL.Query().Get("site")
	}
	st, ok := s.siteByID(id)
	if !ok || !s.scopeOf(r).Has(st.ID) {
		http.NotFound(w, r)
		return
	}
	out := map[string]any{}
	ctx, cancel := context.WithTimeout(r.Context(), routeros.DefaultTimeout)
	defer cancel()
	if err := s.svc.RefreshHosts(ctx, st.ID); err != nil {
		out["error"] = err.Error()
	}
	type item struct {
		MAC      string `json:"mac"`
		Vendor   string `json:"vendor"`
		HostName string `json:"host_name"`
		IP       string `json:"ip"`
		Online   bool   `json:"online"`
		Seen     string `json:"seen"`
	}
	items := []item{}
	for _, u := range s.svc.Unregistered(st.ID, false) {
		if len(items) >= 40 {
			break
		}
		items = append(items, item{MAC: u.MAC, Vendor: u.Vendor, HostName: u.HostName, IP: u.IP, Online: u.Online, Seen: shortAge(s.svc.Now().Sub(u.LastSeen)) + " ago"})
	}
	out["items"] = items
	writeJSON(w, http.StatusOK, out)
}

// pickItem is one flat in a building's account picker.
type pickItem struct {
	ID, Code, Flat string
	Stale          bool // MAC not seen for a day, or flagged: where a new device probably belongs
}

// devRow is one device with its picker.
type devRow struct {
	billing.UnregDevice
	Pick []pickItem
}

func (s *Server) adminDevices(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "devices")
	sc := s.scopeOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), routeros.DefaultTimeout)
	defer cancel()
	if !sc.All { // refreshing asks one router; "all routers" shows what is already known
		if err := s.svc.RefreshHosts(ctx, sc.ID); err != nil {
			p.Data["HostErr"] = err.Error()
		}
	}
	onlineOnly := r.URL.Query().Get("online") == "1"
	var list []billing.UnregDevice
	if sc.All {
		for _, u := range s.svc.Unregistered("", onlineOnly) {
			if sc.Has(u.SiteID) {
				list = append(list, u)
			}
		}
	} else {
		list = s.svc.Unregistered(sc.ID, onlineOnly)
	}
	picks := map[string][]pickItem{}
	stale := map[string]map[string]bool{}
	var pkgs []store.Package
	s.st.View(func(d *store.Data) {
		pkgs = d.PackageList()
		for _, a := range d.AccountList() {
			picks[a.SiteID] = append(picks[a.SiteID], pickItem{ID: a.ID, Code: a.Code, Flat: a.Flat})
		}
	})
	rows := make([]devRow, 0, len(list))
	for _, u := range list {
		if _, ok := stale[u.SiteID]; !ok {
			stale[u.SiteID] = s.svc.StaleAccountIDs(u.SiteID)
		}
		pk := append([]pickItem(nil), picks[u.SiteID]...)
		for i := range pk {
			pk[i].Stale = stale[u.SiteID][pk[i].ID]
		}
		sort.SliceStable(pk, func(i, j int) bool { return pk[i].Stale && !pk[j].Stale })
		rows = append(rows, devRow{UnregDevice: u, Pick: pk})
	}
	now := s.svc.Now().In(billing.EAT)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, billing.EAT)
	var onlineN, today, suggested int
	for _, x := range rows {
		if x.Online {
			onlineN++
		}
		if !x.LastSeen.Before(dayStart) {
			today++
		}
		if x.Suggested != nil {
			suggested++
		}
	}
	regs := s.svc.PendingRegistrations(sc.Has)
	p.Data["Regs"], p.Data["RegGroups"], p.Data["AssignChoices"] = regs, regGroups(regs), s.assignChoices(sc)
	p.Data["Rows"], p.Data["Pkgs"], p.Data["OnlineOnly"], p.Data["Count"] = rows, pkgs, onlineOnly, len(rows)
	p.Data["OnlineN"], p.Data["Today"], p.Data["Suggested"] = onlineN, today, suggested
	s.render(w, "admin_devices", p)
}

// resolveFlat turns what was typed (an account code, or a unit label in any
// case) into an open account of the router. A code is exact. A label that more
// than one open account of the router carries (the same unit in two apartments)
// is never guessed: the accounts come back in ambiguous and ok is false, unless
// apartmentID narrows it to one.
func (s *Server) resolveFlat(siteID, text, apartmentID string) (acct store.Account, ok bool, ambiguous []store.Account) {
	text = strings.TrimSpace(text)
	// The datalist shows "Flat 3B (K7Q2MX)"; accept that form too.
	if i := strings.LastIndex(text, "("); i >= 0 && strings.HasSuffix(text, ")") {
		text = strings.TrimSpace(text[i+1 : len(text)-1])
	}
	if text == "" {
		return store.Account{}, false, nil
	}
	var hits []store.Account
	s.st.View(func(d *store.Data) {
		key := store.UnitKey(text)
		for _, a := range d.AccountList() {
			if a.SiteID != siteID {
				continue
			}
			if strings.EqualFold(a.Code, text) {
				hits = []store.Account{a}
				return
			}
			if apartmentID != "" && a.ApartmentID != apartmentID {
				continue
			}
			if store.UnitKey(a.Unit) == key || store.UnitKey(a.Flat) == key {
				hits = append(hits, a)
			}
		}
	})
	switch len(hits) {
	case 0:
		return store.Account{}, false, nil
	case 1:
		return hits[0], true, nil
	}
	return store.Account{}, false, hits
}

// apartmentName is an apartment's name for a page ("" when it is gone).
func (s *Server) apartmentName(id string) (n string) {
	s.st.View(func(d *store.Data) { n = d.Apartments[id].Name })
	return n
}

func (s *Server) adminDeviceCreate(w http.ResponseWriter, r *http.Request) {
	in := customerFormOf(r, s.siteFor(r))
	a, rs, err := s.createCustomer(r.Context(), in)
	if err != nil && a.ID == "" {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), routeros.DefaultTimeout)
	defer cancel()
	msg := "Account created: " + a.Flat + " (" + a.Code + "). " + routerStateText(s.routerStateJSON(ctx, a, rs))
	if err != nil {
		msg += " " + err.Error()
	}
	s.back(w, r, "/admin/accounts/view?code="+a.Code, msg)
}

// adminDeviceAttach gives an existing flat this device's MAC (an ONT swap, or
// the first MAC of a flat that was created without one). A MAC owned by
// another flat is refused, and the error names that flat and its building.
func (s *Server) adminDeviceAttach(w http.ResponseWriter, r *http.Request) {
	site := s.siteFor(r)
	a, ok, amb := s.resolveFlat(site, r.PostFormValue("acct"), r.PostFormValue("apartment_id"))
	if len(amb) > 0 {
		s.back(w, r, "/admin/devices", "Error: more than one house of this router has that number ("+s.ambiguousText(amb)+"). Type the account code instead")
		return
	}
	if id := r.PostFormValue("account"); !ok && id != "" { // account id, as older forms sent it
		a, ok = s.svc.GetAccount(id)
	}
	if !ok {
		s.back(w, r, "/admin/devices", "Error: choose a house of this router (type its name or code)")
		return
	}
	if _, err := s.svc.AttachDevice(r.Context(), site, r.PostFormValue("mac"), a.ID); err != nil {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	s.back(w, r, s.accountURL(a.ID), "Device attached: "+a.Flat+" now uses this MAC (the router is updating)")
}

func (s *Server) adminDeviceDismiss(w http.ResponseWriter, r *http.Request) {
	s.back(w, r, "/admin/devices", msgOr(s.svc.DismissDevice(s.siteFor(r), r.PostFormValue("mac")), "Dismissed (it comes back if it opens the portal again)"))
}

// adminDeviceMove is "Move customer here": the device belongs to a customer of
// another building, and the operator moves that customer to this router's
// building after a confirm. The old router's lease and queue go, the new one's
// are written, and the account history says so.
func (s *Server) adminDeviceMove(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "1" {
		s.back(w, r, "/admin/devices", "Error: tick the box to confirm the move")
		return
	}
	a, err := s.svc.MoveCustomerHere(r.Context(), s.siteFor(r), r.PostFormValue("mac"), r.PostFormValue("apartment_id"))
	if err != nil {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	s.back(w, r, s.accountURL(a.ID), "Moved "+a.Tenant+" to this apartment. The router is updating.")
}

// regGroup is the waiting registrations of one router (SiteID "" = the ones that name no router yet).
type regGroup struct {
	SiteID, SiteName string
	N                int
}

func regGroups(regs []billing.RegistrationView) []regGroup {
	var out []regGroup
	idx := map[string]int{}
	for _, g := range regs {
		i, ok := idx[g.SiteID]
		if !ok {
			i = len(out)
			idx[g.SiteID] = i
			name := g.SiteName
			if g.SiteID == "" {
				name = "no router yet"
			}
			out = append(out, regGroup{SiteID: g.SiteID, SiteName: name})
		}
		out[i].N++
	}
	return out
}

// assignChoice is one router and apartment a registration that names only a building can be given.
type assignChoice struct{ Value, Label string }

func (s *Server) assignChoices(sc scope) []assignChoice {
	var out []assignChoice
	s.st.View(func(d *store.Data) {
		for _, st := range sc.Sites {
			for _, id := range d.SiteApartmentIDs(st.ID) {
				out = append(out, assignChoice{Value: st.ID + "|" + id, Label: st.Name + " / " + d.Apartments[id].Name})
			}
		}
	})
	return out
}

// regScopeAllows: the registration's router is one the user may act on; one that names no router yet is for
// whoever can act on any router.
func (s *Server) regScopeAllows(r *http.Request, siteID string) bool {
	if siteID == "" {
		return len(s.scopeOf(r).Sites) > 0
	}
	return s.scopeAllows(r, siteID)
}

// adminRegRejectAll rejects every waiting registration of one router (site "none": the ones naming no router).
func (s *Server) adminRegRejectAll(w http.ResponseWriter, r *http.Request) {
	site := r.PostFormValue("site")
	if site == "none" {
		site = billing.UnassignedSite
	}
	if !s.regScopeAllows(r, site) {
		http.NotFound(w, r)
		return
	}
	n, err := s.svc.RejectAllPending(site, s.actorOf(r))
	if err != nil && n == 0 {
		s.back(w, r, "/admin/devices", "Nothing was waiting.")
		return
	}
	s.back(w, r, "/admin/devices", fmt.Sprintf("Rejected %d waiting registrations. Their details were erased.", n))
}

// adminRegAssign gives a registration that named only a building its router and apartment.
func (s *Server) adminRegAssign(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	reg, ok := s.svc.GetRegistration(id)
	if !ok || reg.SiteID != "" || !s.regScopeAllows(r, "") {
		http.NotFound(w, r)
		return
	}
	siteID, aptID, _ := strings.Cut(r.PostFormValue("target"), "|")
	if !s.scopeAllows(r, siteID) {
		s.back(w, r, "/admin/devices", "Error: choose one of your routers and apartments")
		return
	}
	s.back(w, r, "/admin/devices", msgOr(s.svc.AssignRegistration(id, siteID, aptID, s.actorOf(r)), "Assigned. Pick the device and approve."))
}

// adminRegApprove makes the account of a pending registration with the MAC the owner picked (or typed).
func (s *Server) adminRegApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	reg, ok := s.svc.GetRegistration(id)
	if !ok || !s.scopeAllows(r, reg.SiteID) {
		http.NotFound(w, r)
		return
	}
	mac := strings.TrimSpace(r.PostFormValue("mac_typed"))
	if mac == "" {
		mac = r.PostFormValue("mac")
	}
	a, err := s.svc.ApproveRegistration(r.Context(), id, mac, s.actorOf(r))
	if err != nil {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	s.back(w, r, s.accountURL(a.ID), "Approved. The account is made; the device leaves the wall when the router's next pass has run.")
}

func (s *Server) adminRegReject(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	reg, ok := s.svc.GetRegistration(id)
	if !ok || !s.regScopeAllows(r, reg.SiteID) {
		http.NotFound(w, r)
		return
	}
	s.back(w, r, "/admin/devices", msgOr(s.svc.RejectRegistration(id, s.actorOf(r)), "Rejected. The details were erased."))
}

// scopeAllows: the router is one of the live routers (the web admin has one role set for all of them).
func (s *Server) scopeAllows(r *http.Request, siteID string) bool {
	for _, st := range s.scopeOf(r).Sites {
		if st.ID == siteID {
			return true
		}
	}
	return false
}

func (s *Server) actorOf(r *http.Request) string {
	if u, ok := userOf(r); ok {
		return u.Username
	}
	return ""
}

// ambiguousText lists "apartment, unit (code)" for the accounts a typed label matched.
func (s *Server) ambiguousText(list []store.Account) string {
	var parts []string
	for _, a := range list {
		parts = append(parts, s.apartmentName(a.ApartmentID)+", house number "+a.Unit+" ("+a.Code+")")
	}
	return strings.Join(parts, "; ")
}
