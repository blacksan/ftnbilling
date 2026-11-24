package web

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

func (s *Server) apiPackages(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	site := r.URL.Query().Get("site")
	if site != "" && !s.siteExists(site) {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	items := []map[string]any{}
	s.st.View(func(d *store.Data) {
		for _, p := range d.EffectivePackages(site) {
			if p.Deleted {
				continue
			}
			items = append(items, map[string]any{"id": p.ID, "name": p.Name, "speed_mbps": p.SpeedMbps, "duration_min": p.DurationMin,
				"price_kes": p.PriceKES, "disabled": p.Disabled})
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		validationErr(w, map[string]string{"q": "type something to search for"})
		return
	}
	if !s.searchRL.allow("u:" + c.user.ID) {
		rateLimited(w, time.Minute, false)
		return
	}
	res := s.svc.Search(q, func(string) bool { return true }, c.user.Role == store.RoleOwner)
	s.auditNationalIDHits(c.user, c.ip, res)
	custs := []apiCustomer{}
	for _, cu := range res.Customers {
		custs = append(custs, apiCustomer{cu.ID, cu.Name, cu.Phone, cu.Email})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.accountsJSON(res.Accounts), "customers": custs, "payments": s.paymentsJSON(res.Payments)})
}

// apiAudit pages through the audit events (owner only), newest first. The
// cursor is the sequence number of the last event of the previous page.
func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			validationErr(w, map[string]string{"limit": "must be a whole number from 1 to 200"})
			return
		}
		limit = min(n, 200)
	}
	var before int64
	if v := r.URL.Query().Get("cursor"); v != "" {
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "s"), 10, 64)
		if err != nil || !strings.HasPrefix(v, "s") || n < 1 {
			validationErr(w, map[string]string{"cursor": "is not a cursor from this server"})
			return
		}
		before = n
	}
	evs, err := s.st.Events(r.Context(), store.EventQuery{Kind: "audit", BeforeSeq: before, Limit: limit})
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, codeDBDown, "The audit log could not be read.", nil)
		return
	}
	items := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		items = append(items, map[string]any{"seq": e.Seq, "time": tp(e.Time), "actor": e.Actor, "ip": e.IP, "msg": e.Msg, "account_id": e.AccountID, "site_id": e.SiteID})
	}
	var next any
	if len(evs) == limit {
		next = "s" + strconv.FormatInt(evs[len(evs)-1].Seq, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// ---- unregistered devices (addendum H) ---------------------------------------------------------

// deviceOnline: the router saw the device recently. The flag is written by the
// reconciler on every pass, so it is trusted only while its time is fresh (2 minutes, as the contract says).
func deviceOnline(dv store.Device, now time.Time) bool {
	return dv.Online && now.Sub(dv.LastSeen) <= billing.OnlineWindow // same rule as the web page
}

type apiSuggested struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	Flat string `json:"flat"`
}

// suggestAccount picks the account in the building this device most likely
// belongs to: an open account with no MAC yet, else one whose MAC has not been
// seen for over 24 hours (never seen counts, once the account is a day old).
// The first in flat order wins. Nil when nothing fits.
func (s *Server) suggestAccounts(siteID string, now time.Time) (noMAC, stale *apiSuggested) {
	var accts []store.Account
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == siteID {
				accts = append(accts, a)
			}
		}
	})
	for _, a := range accts {
		if a.MAC == "" {
			if noMAC == nil {
				noMAC = &apiSuggested{a.ID, a.Code, a.Flat}
			}
			continue
		}
		if stale != nil || a.MACConflict {
			continue
		}
		seen := s.svc.AccountRouterState(a.ID).LastSeen
		old := now.Sub(seen) > 24*time.Hour
		if seen.IsZero() {
			old = now.Sub(a.CreatedAt) > 24*time.Hour
		}
		if old {
			stale = &apiSuggested{a.ID, a.Code, a.Flat}
		}
	}
	return
}

func (s *Server) apiUnregistered(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	site := r.URL.Query().Get("site")
	onlyOnline := false
	switch v := r.URL.Query().Get("online"); v {
	case "", "false", "0":
	case "true", "1":
		onlyOnline = true
	default:
		validationErr(w, map[string]string{"online": "must be true or false"})
		return
	}
	now := s.svc.Now()
	var devs []store.Device
	names := map[string]string{}
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			names[st.ID] = st.Name
		}
		for _, dv := range d.DeviceList() {
			// Dismissed devices stay in the store as markers (so a later sighting
			// doesn't revive them); they are never listed.
			if dv.Source == billing.SourceDismissed {
				continue
			}
			if _, live := names[dv.SiteID]; live && (site == "" || dv.SiteID == site) {
				devs = append(devs, dv)
			}
		}
	})
	type sug struct{ noMAC, stale *apiSuggested }
	sugs := map[string]sug{}
	items := []map[string]any{}
	sort.SliceStable(devs, func(i, j int) bool {
		oi, oj := deviceOnline(devs[i], now), deviceOnline(devs[j], now)
		if oi != oj {
			return oi
		}
		return devs[i].LastSeen.After(devs[j].LastSeen)
	})
	for _, dv := range devs {
		on := deviceOnline(dv, now)
		if onlyOnline && !on {
			continue
		}
		// A MAC of an account of this very router is no unknown device; one of
		// an account served by another building is listed with belongs_to.
		bt, own := s.svc.DeviceOwnership(dv.SiteID, dv.MAC)
		if own {
			continue
		}
		cls := s.svc.ClassifyMAC(dv.SiteID, dv.MAC)
		if cls.Class == billing.ClassRouter {
			continue // a router's own interface is never a device
		}
		sg, ok := sugs[dv.SiteID]
		if !ok {
			a, b := s.suggestAccounts(dv.SiteID, now)
			sg = sug{a, b}
			sugs[dv.SiteID] = sg
		}
		var suggested any
		switch {
		case sg.noMAC != nil:
			suggested = sg.noMAC
		case sg.stale != nil:
			suggested = sg.stale
		}
		var belongs any // null unless the MAC belongs to a customer of another building
		if bt != nil {
			suggested = nil
			choices := []map[string]string{}
			for _, ap := range bt.ApartmentChoices {
				choices = append(choices, map[string]string{"id": ap.ID, "name": ap.Name})
			}
			belongs = map[string]any{"account_id": bt.AccountID, "code": bt.Code, "customer": bt.Customer,
				"apartment": map[string]any{"id": bt.ApartmentID, "name": bt.Apartment}, "unit": bt.Unit,
				"site_id": bt.SiteID, "site_name": bt.SiteName, "same_apartment": bt.SameApartment, "move_to_choices": choices}
		}
		items = append(items, map[string]any{"site_id": dv.SiteID, "site_name": names[dv.SiteID], "mac": dv.MAC, "vendor": dv.Vendor,
			"host_name": dv.HostName, "ip": dv.IP, "source": dv.Source, "port": dv.Port, "address_note": billing.UnregDevice{Device: dv}.AddrNote(), "online": on, "first_seen": tp(dv.FirstSeen), "last_seen": tp(dv.LastSeen),
			"hits": dv.Hits, "suggested_account": suggested, "belongs_to": belongs,
			"class": apiDeviceClass(cls)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiUnregRefresh(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	s.svc.RefreshHostsAsync(st.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

// macField normalises a request MAC, answering with validation_failed if it is not one.
func macField(w http.ResponseWriter, raw string) (string, bool) {
	mac, err := billing.NormalizeMAC(raw)
	if err != nil {
		validationErr(w, map[string]string{"mac": "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"})
		return "", false
	}
	return mac, true
}

func (s *Server) apiUnregAttach(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID    string `json:"site_id"`
		MAC       string `json:"mac"`
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	mac, ok := macField(w, req.MAC)
	if !ok {
		return
	}
	if !s.siteExists(req.SiteID) {
		validationErr(w, map[string]string{"site_id": "no such router"})
		return
	}
	a, ok := s.accountOr404(w, req.AccountID)
	if !ok {
		return
	}
	if a.SiteID != req.SiteID {
		validationErr(w, map[string]string{"account_id": "that account is in another router"})
		return
	}
	// The same rule as PUT /accounts/{id}/mac: one MAC, one open account, anywhere.
	if err := s.svc.ChangeMAC(r.Context(), a.ID, mac); err != nil {
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "device "+mac+" attached to account "+a.Code)
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiUnregCreate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID      string `json:"site_id"`
		MAC         string `json:"mac"`
		Flat        string `json:"flat"` // older name of unit
		Unit        string `json:"unit"`
		ApartmentID string `json:"apartment_id"`
		Tenant      string `json:"tenant"`
		Phone       string `json:"phone"`
		PackageID   string `json:"package_id"`
		Activate    bool   `json:"activate"`     // grant the package's first period free; default false, like POST /accounts
		EquipmentID string `json:"equipment_id"` // an in-stock ONU of the register (the device is that ONU)
		// ConfirmNoAddress: yes, this device that never asked for an address is the customer's ONU.
		ConfirmNoAddress bool `json:"confirm_no_address"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	fields := map[string]string{}
	if !s.siteExists(req.SiteID) {
		fields["site_id"] = "no such router"
	}
	if strings.TrimSpace(req.Unit) == "" {
		req.Unit = req.Flat
	}
	if strings.TrimSpace(req.Unit) == "" {
		fields["unit"] = "a house number is required"
	}
	if strings.TrimSpace(req.ApartmentID) == "" {
		fields["apartment_id"] = "an apartment is required"
	} else if !s.apartmentExists(req.ApartmentID) {
		fields["apartment_id"] = "no such apartment"
	}
	mac, merr := billing.NormalizeMAC(req.MAC)
	if merr != nil {
		fields["mac"] = "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"
	}
	if req.PackageID != "" {
		known := false
		s.st.View(func(d *store.Data) { p, ok := d.Packages[req.PackageID]; known = ok && !p.Deleted })
		if !known {
			fields["package_id"] = "no such package"
		}
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	// The same door as POST /accounts: a package is set as the plan and the tenant pays for it; free time only
	// when "activate" says so. A package that cannot be set refuses the request before anything is made.
	a, _, err := s.svc.CreateCustomer(r.Context(), billing.AddCustomerInput{ApartmentID: req.ApartmentID, SiteID: req.SiteID, Unit: req.Unit, Flat: req.Unit,
		MAC: mac, Tenant: req.Tenant, Phone: req.Phone, PackageID: req.PackageID, Activate: req.Activate,
		EquipmentID: req.EquipmentID, ConfirmNoAddress: req.ConfirmNoAddress})
	if err != nil {
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "account "+a.Code+" ("+a.Flat+") created from device "+mac)
	s.respondAccount(w, http.StatusCreated, a.ID)
}

func (s *Server) apiUnregDismiss(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID string `json:"site_id"`
		MAC    string `json:"mac"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	mac, ok := macField(w, req.MAC)
	if !ok {
		return
	}
	if !s.siteExists(req.SiteID) {
		validationErr(w, map[string]string{"site_id": "no such router"})
		return
	}
	if err := s.svc.DismissDevice(req.SiteID, mac); err != nil {
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "device "+mac+" dismissed")
	w.WriteHeader(http.StatusNoContent)
}

// apiDeviceClass is a New devices row's place in the equipment hierarchy: kind is "olt" (no Create customer or
// Attach: dismiss only), "onu" (an ONU of the register; create the customer with equipment_id) or "unknown"
// (never_asked: the device only showed up at layer 2, creating a customer needs confirm_no_address).
func apiDeviceClass(c billing.DeviceClass) map[string]any {
	ids := c.ApartmentIDs
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{"kind": c.Class, "label": c.Label, "equipment_id": c.EquipmentID, "creatable": c.Creatable,
		"attachable": c.Class != billing.ClassOLT && c.Class != billing.ClassPossibleOLT, "never_asked": c.NeverAsked, "apartment_ids": ids}
}
