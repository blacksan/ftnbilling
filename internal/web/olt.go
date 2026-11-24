package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// The equipment hierarchy screens: the Equipment tree (router > OLT > PON > ONU), the OLT form, and the pieces that
// other pages show (router tab, apartment page, account card). Nothing here writes to a router or an OLT.

func (s *Server) oltRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/olts/new", s.adminOLTNew)
	get("/admin/olts/view", s.adminOLTView)
	post("/admin/olts/save", s.adminOLTSave)
	post("/admin/olts/reveal", s.adminOLTReveal)
	s.oltRoutes2(get, post)
}

// treeFor builds the equipment tree for the routers the admin may see. Equipment with no router shows only in the
// all-routers view.
func (s *Server) treeFor(sc scope) billing.EquipmentTree {
	return s.svc.Tree(func(id string) bool {
		if id == "" {
			return sc.All
		}
		return sc.Has(id)
	})
}

func (s *Server) adminOLTNew(w http.ResponseWriter, r *http.Request) {
	site := r.URL.Query().Get("site")
	if site == "" {
		site = s.scopeOf(r).ID
	}
	s.oltForm(w, r, store.Equipment{Kind: store.KindOLT, SiteID: site, PONPorts: 4, Status: store.EqInstalled}, false)
}

func (s *Server) adminOLTView(w http.ResponseWriter, r *http.Request) {
	var e store.Equipment
	var ok bool
	s.st.View(func(d *store.Data) { e, ok = d.Equipment[r.URL.Query().Get("id")] })
	if !ok || e.Kind != store.KindOLT || (e.SiteID != "" && !s.scopeAllows(r, e.SiteID)) {
		http.NotFound(w, r)
		return
	}
	s.oltForm(w, r, e, true)
}

// ponRow is one PON port row of the OLT form: the port, the apartments of the router and which ones it serves.
type ponRow struct {
	Port int
	Apts []ponApt
	ONUs []store.Equipment
}
type ponApt struct {
	ID, Name string
	Serves   bool
}

// oltForm renders the OLT form. With rejected set (a save that was refused) it shows that error over the values the
// operator typed, with status 422; the password is never put back.
func (s *Server) oltForm(w http.ResponseWriter, r *http.Request, e store.Equipment, existing bool, rejected ...string) {
	p := s.eqPage(r, "equipment", eqTabOLT)
	if len(rejected) > 0 {
		p.Flash, p.FlashBad = "Error: "+rejected[0], true
	}
	sc := s.scopeOf(r)
	var rows []ponRow
	var apts []store.Apartment
	var onus int
	var hist []eqEvRow
	s.st.View(func(d *store.Data) {
		for _, id := range d.SiteApartmentIDs(e.SiteID) {
			apts = append(apts, d.Apartments[id])
		}
		n := e.PONPorts
		if n < 4 {
			n = 4
		}
		for _, k := range d.PONPortNumbers(e) {
			if k > n {
				n = k
			}
		}
		for port := 1; port <= n && port <= billing.MaxPONPorts; port++ {
			row := ponRow{Port: port, ONUs: d.ONUsOfOLT(e.ID, port)}
			serves := map[string]bool{}
			for _, id := range e.PONApartmentIDs(port) {
				serves[id] = true
			}
			for _, a := range apts {
				row.Apts = append(row.Apts, ponApt{a.ID, a.Name, serves[a.ID]})
			}
			onus += len(row.ONUs)
			rows = append(rows, row)
		}
		if existing {
			for _, ev := range d.EquipmentEvents {
				if ev.EquipmentID == e.ID {
					hist = append(hist, eqEvRow{Ev: ev})
				}
			}
		}
	})
	for i := 1; i < len(hist); i++ { // newest first
		for j := i; j > 0 && hist[j].Ev.Time.After(hist[j-1].Ev.Time); j-- {
			hist[j], hist[j-1] = hist[j-1], hist[j]
		}
	}
	title := "Add an OLT"
	if existing {
		title = store.OLTLabel(e)
	}
	p.Detail = title
	p.H1a, p.H1b = "OLT", title
	if !existing {
		p.H1a, p.H1b = "Add an", "OLT"
	}
	p.Data["E"], p.Data["Existing"], p.Data["PONRows"], p.Data["Hist"] = e, existing, rows, hist
	p.Data["HasPass"], p.Data["Statuses"], p.Data["Sites"] = billing.HasOLTPassword(e), billing.OLTStatuses, sc.Sites
	p.Data["RouterName"] = p.Data["SiteNames"].(map[string]string)[e.SiteID]
	p.Data["HasApts"] = len(apts) > 0
	p.Data["ONUCount"] = onus
	if len(rejected) > 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	s.render(w, "admin_olt", p)
}

func (s *Server) adminOLTSave(w http.ResponseWriter, r *http.Request) {
	atoi := func(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }
	id := r.PostFormValue("id")
	in := billing.OLTInput{ID: id, Name: r.PostFormValue("name"), Model: r.PostFormValue("model"), Serial: r.PostFormValue("serial"),
		MAC: r.PostFormValue("mac"), Firmware: r.PostFormValue("firmware"), Notes: r.PostFormValue("notes"), SiteID: r.PostFormValue("site"),
		RouterPort: r.PostFormValue("router_port"), PONPorts: atoi(r.PostFormValue("pon_ports")), MgmtIP: r.PostFormValue("mgmt_ip"),
		MgmtUser: r.PostFormValue("mgmt_user"), MgmtPass: r.PostFormValue("mgmt_pass"), ClearMgmtPass: r.PostFormValue("clear_pass") == "1",
		Status: r.PostFormValue("status")}
	if in.SiteID != "" && !s.scopeAllows(r, in.SiteID) {
		s.back(w, r, "/admin/equipment?tab=olt", "Error: choose one of your routers")
		return
	}
	// The PON mapping is replaced only when the form carried it (the add form for a router with no apartments does not).
	if r.PostFormValue("pon_form") == "1" {
		in.PONApartments = map[int][]string{}
		for k, v := range r.PostForm {
			if n, ok := strings.CutPrefix(k, "pon_apt_"); ok {
				if port := atoi(n); port > 0 {
					in.PONApartments[port] = v
				}
			}
		}
	}
	e, err := s.svc.SaveOLT(in)
	if err != nil {
		// A refused save keeps what was typed (never the password).
		var cur store.Equipment
		var exists bool
		if id != "" {
			s.st.View(func(d *store.Data) { cur, exists = d.Equipment[id] })
			if !exists || cur.Kind != store.KindOLT {
				s.back(w, r, "/admin/equipment?tab=olt", "Error: "+err.Error())
				return
			}
		} else {
			cur = store.Equipment{Kind: store.KindOLT}
		}
		cur.Name, cur.Model, cur.Serial, cur.MAC, cur.Firmware, cur.Notes = in.Name, in.Model, in.Serial, in.MAC, in.Firmware, in.Notes
		cur.SiteID, cur.RouterPort, cur.PONPorts, cur.MgmtIP, cur.MgmtUser = in.SiteID, in.RouterPort, in.PONPorts, in.MgmtIP, in.MgmtUser
		if in.Status != "" {
			cur.Status = in.Status
		}
		if in.PONApartments != nil {
			cur.PONApartments = map[string][]string{}
			for n, ids := range in.PONApartments {
				if len(ids) > 0 {
					cur.PONApartments[store.PONKey(n)] = ids
				}
			}
		}
		s.oltForm(w, r, cur, id != "", err.Error())
		return
	}
	s.back(w, r, "/admin/olts/view?id="+url.QueryEscape(e.ID), "Saved")
}

// adminOLTReveal shows the stored login password once, to an owner who typed their own password again. It is never
// part of any other page, the API, a log or the audit text.
func (s *Server) adminOLTReveal(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := "/admin/olts/view?id=" + url.QueryEscape(id)
	if u, _ := userOf(r); u.Role != store.RoleOwner {
		s.back(w, r, to, "Error: only an owner can see this password")
		return
	}
	// A few reveals per owner in ten minutes, wrong passwords included.
	u, _ := userOf(r)
	if !s.revealRL.allow("olt-reveal:" + u.ID) {
		s.back(w, r, to, "Error: too many password reveals in a short time; wait a few minutes and try again")
		return
	}
	if !s.reauth(w, r, to) {
		return
	}
	pw, err := s.svc.RevealOLTPassword(id)
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.audit(r, "OLT login password shown")
	w.Header().Set("Cache-Control", "no-store")
	s.notice(w, r, http.StatusOK, "OLT login password", "Password "+pw, to, "Back")
}

// ---- pieces of other pages ------------------------------------------------------------------------------

// routerEquipmentData fills the router page's Equipment tab: the apartments it serves and its OLTs.
func (s *Server) routerEquipmentData(d map[string]any, siteID string) {
	t := s.svc.Tree(func(id string) bool { return id == siteID })
	for _, r := range t.Routers {
		if r.Site.ID == siteID {
			d["EqRouter"] = r
		}
	}
}

// apartmentEquipmentData fills the apartment page's "Served by" card.
func (s *Server) apartmentEquipmentData(d map[string]any, apartmentID string) {
	d["ServedBy"] = s.svc.ApartmentServedBy(apartmentID)
}

// placementOf maps equipment id -> placement for the items of an account page.
func (s *Server) placements(items []store.Equipment) map[string]billing.ONUPlacement {
	out := map[string]billing.ONUPlacement{}
	for _, e := range items {
		if e.Kind == store.KindONT {
			out[e.ID] = s.svc.PlacementOf(e.ID)
		}
	}
	return out
}

// oltChoices lists the OLTs for the ONU form (and the add form), with their PON counts.
func (s *Server) oltChoices(sc scope) []store.Equipment {
	var out []store.Equipment
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindOLT && e.Status != store.EqRetired && (e.SiteID == "" || sc.Has(e.SiteID)) {
				out = append(out, e)
			}
		}
	})
	sortOLTs(out)
	return out
}

func sortOLTs(l []store.Equipment) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && strings.ToLower(store.OLTLabel(l[j])) < strings.ToLower(store.OLTLabel(l[j-1])); j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// ageText is "3 min ago" for a time, or "never".
func (s *Server) ageText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return shortAge(s.svc.Now().Sub(t)) + " ago"
}
