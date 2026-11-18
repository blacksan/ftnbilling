package web

import (
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

type apiPkg struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SpeedMbps int    `json:"speed_mbps"`
	PriceKES  int    `json:"price_kes"`
}

type apiRouterState struct {
	InSync            bool   `json:"in_sync"`
	SuspendedOnRouter bool   `json:"suspended_on_router"`
	LeaseStatus       string `json:"lease_status"`
	LastSeen          any    `json:"last_seen"`
	PendingTask       bool   `json:"pending_task"`
	LastError         string `json:"last_error"`
}

type apiAccount struct {
	ID          string         `json:"id"`
	Code        string         `json:"code"`
	SiteID      string         `json:"site_id"`
	SiteName    string         `json:"site_name"`
	Flat        string         `json:"flat"`
	Floor       string         `json:"floor"`
	MAC         string         `json:"mac"`
	MACConflict bool           `json:"mac_conflict"`
	IP          string         `json:"ip"`
	Tenant      string         `json:"tenant"`
	Phone       string         `json:"phone"`
	WhatsApp    string         `json:"whatsapp"`
	Notes       string         `json:"notes"`
	State       string         `json:"state"`
	Suspended   bool           `json:"suspended"`
	Package     *apiPkg        `json:"package"`
	ExpiresAt   any            `json:"expires_at"`
	Router      apiRouterState `json:"router"`
	CustomerID  string         `json:"customer_id"`
	CreatedAt   string         `json:"created_at"`
}

func leaseStatus(v string) string {
	switch v {
	case "bound", "waiting", "none":
		return v
	}
	return "none"
}

// accountsJSON builds the API shape of accounts. It takes one read of the
// store, then asks the billing service for each router state (which reads
// again, so it must not run inside the first read).
func (s *Server) accountsJSON(accts []store.Account) []apiAccount {
	now := s.svc.Now()
	siteNames := map[string]string{}
	pkgs := map[string]*apiPkg{}
	s.st.View(func(d *store.Data) {
		for _, a := range accts {
			if _, ok := siteNames[a.SiteID]; !ok {
				siteNames[a.SiteID] = d.Sites[a.SiteID].Name
			}
			if a.PackageID != "" {
				if p, ok := d.EffectivePackage(a.SiteID, a.PackageID); ok {
					pkgs[a.SiteID+"|"+a.PackageID] = &apiPkg{p.ID, p.Name, p.SpeedMbps, p.PriceKES}
				}
			}
		}
	})
	out := make([]apiAccount, 0, len(accts))
	for _, a := range accts {
		rs := s.svc.AccountRouterState(a.ID)
		out = append(out, apiAccount{
			ID: a.ID, Code: a.Code, SiteID: a.SiteID, SiteName: siteNames[a.SiteID], Flat: a.Flat, Floor: a.Floor,
			MAC: a.MAC, MACConflict: a.MACConflict, IP: a.IP, Tenant: a.Tenant, Phone: a.Phone, WhatsApp: a.WhatsApp, Notes: a.Notes,
			State: billing.AccountState(a, now), Suspended: a.Suspended, Package: pkgs[a.SiteID+"|"+a.PackageID], ExpiresAt: tp(a.ExpiresAt),
			Router: apiRouterState{InSync: rs.InSync, SuspendedOnRouter: rs.SuspendedOnRouter, LeaseStatus: leaseStatus(rs.LeaseStatus),
				LastSeen: tp(rs.LastSeen), PendingTask: rs.PendingTask, LastError: rs.LastError},
			CustomerID: a.CustomerID, CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

func (s *Server) oneAccountJSON(id string) (apiAccount, bool) {
	a, ok := s.svc.GetAccount(id)
	if !ok {
		return apiAccount{}, false
	}
	return s.accountsJSON([]store.Account{a})[0], true
}

func hexOnlyLower(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *Server) siteExists(id string) bool {
	ok := false
	s.st.View(func(d *store.Data) { st, f := d.Sites[id]; ok = f && !st.Removed })
	return ok
}

// apiAccounts lists accounts: ?site=&state=&q=&cursor=&limit=.
func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pg, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	site, state, text := q.Get("site"), q.Get("state"), strings.ToLower(strings.TrimSpace(q.Get("q")))
	switch state {
	case "", "active", "expired", "suspended", "new", "mac_conflict":
	default:
		validationErr(w, map[string]string{"state": "must be active, expired, suspended, new or mac_conflict"})
		return
	}
	hexq := hexOnlyLower(text)
	now := s.svc.Now()
	var list []store.Account
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if site != "" && a.SiteID != site {
				continue
			}
			switch state {
			case "":
			case "mac_conflict":
				if !a.MACConflict {
					continue
				}
			default:
				if billing.AccountState(a, now) != state {
					continue
				}
			}
			if text != "" {
				hit := strings.Contains(strings.ToLower(a.Flat), text) || strings.Contains(strings.ToLower(a.Code), text) ||
					strings.Contains(strings.ToLower(a.Tenant), text) || strings.Contains(strings.ToLower(a.Phone), text)
				if !hit && len(hexq) >= 4 && strings.Contains(hexOnlyLower(a.MAC), hexq) {
					hit = true
				}
				if !hit {
					continue
				}
			}
			list = append(list, a)
		}
	})
	rows, next := slicePage(list, pg)
	writeJSON(w, http.StatusOK, map[string]any{"items": s.accountsJSON(rows), "next_cursor": next})
}

type apiCustomer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
}

type apiEvent struct {
	Time  string `json:"time"`
	Msg   string `json:"msg"`
	Actor string `json:"actor"`
}

func (s *Server) apiAccount(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	id := r.PathValue("id")
	a, ok := s.svc.GetAccount(id)
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such account.", nil)
		return
	}
	base := s.accountsJSON([]store.Account{a})[0]
	// Always an object: with no customer record linked, the id is empty and
	// the name and phone are the account's tenant and phone.
	cust := &apiCustomer{"", a.Tenant, a.Phone, ""}
	s.st.View(func(d *store.Data) {
		if cu, ok := d.Customers[a.CustomerID]; ok && a.CustomerID != "" {
			cust = &apiCustomer{cu.ID, cu.Name, cu.Phone, cu.Email}
		}
	})
	pays, _ := s.svc.FilterPayments(billing.PaymentFilter{AccountID: id})
	if len(pays) > 20 {
		pays = pays[:20]
	}
	events := []apiEvent{}
	if evs, err := s.st.Events(r.Context(), store.EventQuery{AccountID: id, Limit: 20}); err == nil {
		for _, e := range evs {
			events = append(events, apiEvent{e.Time.UTC().Format(time.RFC3339), e.Msg, e.Actor})
		}
	}
	var monthBytes float64
	if col := s.mgr.Collector(a.SiteID); col != nil {
		fr := col.BuildFlat(a.Code, "month", s.svc.Now())
		monthBytes = (fr.Row.DownGB + fr.Row.UpGB) * 1e9
	}
	writeJSON(w, http.StatusOK, struct {
		apiAccount
		Customer *apiCustomer   `json:"customer"`
		Payments []apiPayment   `json:"payments"`
		Events   []apiEvent     `json:"events"`
		Usage    map[string]any `json:"usage"`
	}{base, cust, s.paymentsJSON(pays), events, map[string]any{"month_bytes": int64(monthBytes)}})
}

// ---- writes ------------------------------------------------------------------------------

func (s *Server) accountOr404(w http.ResponseWriter, id string) (store.Account, bool) {
	a, ok := s.svc.GetAccount(id)
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such account.", nil)
	}
	return a, ok
}

func (s *Server) respondAccount(w http.ResponseWriter, status int, id string) {
	if a, ok := s.oneAccountJSON(id); ok {
		writeJSON(w, status, a)
		return
	}
	apiErr(w, http.StatusNotFound, codeNotFound, "No such account.", nil)
}

func (s *Server) apiAccountCreate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID string `json:"site_id"`
		Flat   string `json:"flat"`
		MAC    string `json:"mac"`
		Tenant string `json:"tenant"`
		Phone  string `json:"phone"`
		Notes  string `json:"notes"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	fields := map[string]string{}
	if strings.TrimSpace(req.SiteID) == "" {
		fields["site_id"] = "choose a router"
	} else if !s.siteExists(req.SiteID) {
		fields["site_id"] = "no such router"
	}
	if strings.TrimSpace(req.Flat) == "" {
		fields["flat"] = "a flat label is required"
	}
	mac, merr := billing.NormalizeMAC(req.MAC)
	if merr != nil {
		fields["mac"] = "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	a, err := s.svc.CreateAccount(billing.AccountInput{SiteID: req.SiteID, Flat: req.Flat, MAC: mac, Tenant: req.Tenant, Phone: req.Phone, Notes: req.Notes})
	if err != nil {
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "account "+a.Code+" ("+a.Flat+") created")
	s.respondAccount(w, http.StatusCreated, a.ID)
}

func (s *Server) apiAccountPatch(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		Flat   *string `json:"flat"`
		Tenant *string `json:"tenant"`
		Phone  *string `json:"phone"`
		Notes  *string `json:"notes"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	flat, tenant, phone, notes := a.Flat, a.Tenant, a.Phone, a.Notes
	if req.Flat != nil {
		flat = *req.Flat
	}
	if req.Tenant != nil {
		tenant = *req.Tenant
	}
	if req.Phone != nil {
		phone = *req.Phone
	}
	if req.Notes != nil {
		notes = *req.Notes
	}
	if strings.TrimSpace(flat) == "" {
		validationErr(w, map[string]string{"flat": "a flat label is required"})
		return
	}
	if err := s.svc.UpdateAccount(a.ID, flat, tenant, phone, notes); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "account "+a.Code+" edited")
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiAccountMAC(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		MAC string `json:"mac"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	mac, err := billing.NormalizeMAC(req.MAC)
	if err != nil {
		validationErr(w, map[string]string{"mac": "must be 12 hex digits, like AA:BB:CC:DD:EE:FF"})
		return
	}
	if err := s.svc.ChangeMAC(r.Context(), a.ID, mac); err != nil {
		s.billingErr(w, err, mac)
		return
	}
	c.audit(s, "account "+a.Code+" MAC set to "+mac)
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiAccountSuspend(suspend bool) apiFunc {
	return func(w http.ResponseWriter, r *http.Request, c *apiCtx) {
		a, ok := s.accountOr404(w, r.PathValue("id"))
		if !ok {
			return
		}
		if err := s.svc.SetSuspended(r.Context(), a.ID, suspend); err != nil {
			s.billingErr(w, err, "")
			return
		}
		verb := "unsuspended"
		if suspend {
			verb = "suspended"
		}
		c.audit(s, "account "+a.Code+" "+verb)
		s.respondAccount(w, http.StatusOK, a.ID)
	}
}

func (s *Server) apiAccountPackage(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		PackageID string `json:"package_id"`
		KeepTime  bool   `json:"keep_time"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if req.PackageID == "" {
		validationErr(w, map[string]string{"package_id": "choose a package"})
		return
	}
	if err := s.svc.ChangePackage(r.Context(), a.ID, req.PackageID, req.KeepTime); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "account "+a.Code+" package changed")
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiAccountExtend(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		PackageID string `json:"package_id"`
		Note      string `json:"note"`
		Force     bool   `json:"force"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if req.PackageID == "" {
		validationErr(w, map[string]string{"package_id": "choose a package"})
		return
	}
	if err := s.svc.ExtendAccount(r.Context(), a.ID, req.PackageID, req.Note, req.Force); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "account "+a.Code+" extended")
	s.respondAccount(w, http.StatusOK, a.ID)
}

func (s *Server) apiAccountReconcile(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	a, ok := s.accountOr404(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.svc.QueueReconcile(a.ID, "reconcile requested in the app"); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "account "+a.Code+" reconcile queued")
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}
