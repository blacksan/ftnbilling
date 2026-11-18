package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

func (s *Server) deviceRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/devices", s.adminDevices)
	post("/admin/devices/create", s.adminDeviceCreate)
	post("/admin/devices/attach", s.adminDeviceAttach)
	post("/admin/devices/dismiss", s.adminDeviceDismiss)
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
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
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
	p.Data["Rows"], p.Data["Pkgs"], p.Data["OnlineOnly"], p.Data["Count"] = rows, pkgs, onlineOnly, len(rows)
	s.render(w, "admin_devices", p)
}

// resolveFlat turns what the admin typed in the picker (an account code or a
// flat label, any case) into an account of the building.
func (s *Server) resolveFlat(siteID, text string) (store.Account, bool) {
	text = strings.TrimSpace(text)
	// The datalist shows "Flat 3B (K7Q2MX)"; accept that form too.
	if i := strings.LastIndex(text, "("); i >= 0 && strings.HasSuffix(text, ")") {
		text = strings.TrimSpace(text[i+1 : len(text)-1])
	}
	if text == "" {
		return store.Account{}, false
	}
	var out store.Account
	found := false
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID != siteID {
				continue
			}
			if strings.EqualFold(a.Code, text) || strings.EqualFold(a.Flat, text) {
				out, found = a, true
				if strings.EqualFold(a.Code, text) {
					return
				}
			}
		}
	})
	return out, found
}

func (s *Server) adminDeviceCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.CreateFromDevice(r.Context(), billing.DeviceAccountInput{
		SiteID: s.siteFor(r), MAC: r.PostFormValue("mac"), Flat: r.PostFormValue("flat"),
		Tenant: r.PostFormValue("tenant"), Phone: r.PostFormValue("phone"), PackageID: r.PostFormValue("package"),
	})
	if err != nil {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/accounts/view?code="+a.Code, "Account created: "+a.Flat+" ("+a.Code+")")
}

// adminDeviceAttach gives an existing flat this device's MAC (an ONT swap, or
// the first MAC of a flat that was created without one). A MAC owned by
// another flat is refused, and the error names that flat and its building.
func (s *Server) adminDeviceAttach(w http.ResponseWriter, r *http.Request) {
	site := s.siteFor(r)
	a, ok := s.resolveFlat(site, r.PostFormValue("acct"))
	if id := r.PostFormValue("account"); !ok && id != "" { // account id, as older forms sent it
		a, ok = s.svc.GetAccount(id)
	}
	if !ok {
		s.back(w, r, "/admin/devices", "Error: choose a flat of this router (type its name or code)")
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
