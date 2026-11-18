package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/metrics"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

func (s *Server) adminRoutes(mux *http.ServeMux) {
	// D8: /admin/routers… is the page address; /admin/sites… keeps working
	// (GET redirects to the new address, POST is served on both).
	get := func(p string, h http.HandlerFunc) {
		if np, ok := routerAdminPath(p); ok {
			mux.Handle("GET "+np, s.admin(h))
			mux.Handle("GET "+p, s.admin(redirectToRouters))
			return
		}
		mux.Handle("GET "+p, s.admin(h))
	}
	post := func(p string, h http.HandlerFunc) {
		mux.Handle("POST "+p, s.admin(h))
		if np, ok := routerAdminPath(p); ok {
			mux.Handle("POST "+np, s.admin(h))
		}
	}
	get("/admin", s.adminDashboard)
	get("/admin/live", s.adminLive)
	get("/admin/live.json", s.adminLiveJSON)
	get("/admin/map", s.adminMap)
	get("/admin/search.json", s.adminSearchJSON)
	get("/admin/audit", s.adminAudit)
	get("/admin/accounts/status.json", s.adminAccountStatus)
	get("/admin/site/select", s.adminSiteSelect)
	post("/admin/accounts/move", s.adminAccountMove)
	s.siteRoutes(get, post)
	get("/admin/accounts", s.adminAccounts)
	get("/admin/accounts/view", s.adminAccount)
	post("/admin/accounts/create", s.adminAccountCreate)
	post("/admin/accounts/update", s.adminAccountUpdate)
	post("/admin/accounts/mac", s.adminAccountMAC)
	post("/admin/accounts/suspend", s.adminAccountSuspend)
	post("/admin/accounts/extend", s.adminAccountExtend)
	post("/admin/accounts/kick", s.adminAccountKick)
	post("/admin/accounts/package", s.adminAccountPackage)
	post("/admin/accounts/endplan", s.adminAccountEndPlan)
	post("/admin/accounts/delete", s.adminAccountDelete)
	get("/admin/payments", s.adminPayments)
	get("/admin/payments.csv", s.adminPaymentsCSV)
	get("/admin/payments/record", s.adminRecordForm)
	post("/admin/payments/record", s.adminRecordPost)
	get("/admin/unmatched", s.adminUnmatched)
	post("/admin/unmatched/assign", s.adminUnmatchedAssign)
	post("/admin/unmatched/resolve", s.adminUnmatchedResolve)
	get("/admin/orders", s.adminOrders)
	post("/admin/orders/markpaid", s.adminMarkPaid)
	get("/admin/packages", s.adminPackages)
	post("/admin/packages/save", s.adminSavePackage)
	post("/admin/packages/delete", s.adminDeletePackage)
	get("/admin/vouchers", s.adminVouchers)
	post("/admin/vouchers/generate", s.adminGenVouchers)
	get("/admin/vouchers/print", s.adminPrintVouchers)
	s.securityRoutes(get, post)
	s.settingsRoutes(get, post)
	s.crmRoutes(get, post)
	s.metricsRoutes(get, post)
	s.opsRoutes(get, post)
}

type adminPage struct {
	Brand, Active, CSRF, Flash string
	User, Role                 string       // who is signed in
	Sites                      []store.Site // live buildings, for the switcher
	Site                       string       // selected building id, "" = all
	SiteName                   string
	AllSites, Multi            bool
	Next                       string // where the switcher returns to
	Now                        string // banner clock, EAT
	Refresh                    int    // seconds between automatic reloads, 0 = none
	HasOps                     bool   // the fleet monitor is running
	OpenAlerts, FailedMsgs     int    // sidebar badges
	FlashBad                   bool
	UnregCount, UnmatchedCount int
	Data                       map[string]any

	// Shell and hero (see nav.go): what the sidebar, breadcrumb and page
	// header show. Pages override Detail, Lead and Acts when they know more.
	Group, Label, Href string
	Detail             string
	H1a, H1b, Lead     string
	Acts               []heroAct
	CanWrite           bool            // owner or operator: the add and edit buttons show
	Pkgs               []store.Package // enabled plans, for the Add customer form
	Initial            string          // avatar letter
	Version            string
}

func (s *Server) page(r *http.Request, active string) adminPage {
	p := adminPage{Brand: s.cfg.BrandName, Active: active, CSRF: csrfOf(r), Flash: r.URL.Query().Get("msg"), Data: map[string]any{}}
	if u, ok := userOf(r); ok {
		p.User, p.Role = u.Username, u.Role
	}
	p.FlashBad = strings.HasPrefix(p.Flash, "Error")
	p.Now = s.svc.Now().In(billing.EAT).Format("Mon 02 Jan, 15:04")
	sc := s.scopeOf(r)
	p.Sites, p.Site, p.AllSites, p.Multi = sc.Sites, sc.ID, sc.All, sc.Multi()
	p.Next = r.URL.RequestURI()
	names := map[string]string{}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
		if st.ID == sc.ID {
			p.SiteName = st.Name
		}
	}
	p.Data["SiteNames"], p.Data["ShowSite"], p.Data["Scope"] = names, sc.All && sc.Multi(), sc
	p.HasOps = s.hasOps()
	if s.ops.Alerts != nil {
		p.OpenAlerts = s.ops.Alerts.OpenCount(sc.ID)
	}
	if s.ops.Box != nil {
		p.FailedMsgs = s.ops.Box.Counts()["failed"]
	}
	p.UnregCount = s.svc.UnregisteredOnline(sc.Has) // online, unregistered devices only
	p.CanWrite = p.Role == store.RoleOwner || p.Role == store.RoleOperator
	p.Initial = initialOf(p.User)
	if p.CanWrite {
		p.Data["RouterDefaults"] = sites.ApplyDefaults(s.cfg, store.Site{Mode: store.SiteModeWG})
		p.Data["JoinTTL"] = int(s.cfg.JoinTokenTTL.Minutes())
	}
	s.st.View(func(d *store.Data) {
		for _, pk := range d.EffectivePackages(sc.ID) {
			if !pk.Disabled && !pk.Deleted {
				p.Pkgs = append(p.Pkgs, pk)
			}
		}
	})
	sort.SliceStable(p.Pkgs, func(i, j int) bool { return p.Pkgs[i].PriceKES < p.Pkgs[j].PriceKES })
	p.setHero(active, s.svc.Now())
	s.st.View(func(d *store.Data) {
		for _, pay := range d.Payments {
			if pay.Status == store.PayUnmatched && (pay.SiteID == "" || sc.Has(pay.SiteID)) {
				p.UnmatchedCount++
			}
		}
	})
	return p
}

// siteFor resolves the building a create form targets: the form's own site
// field, else the selected building. Empty when neither is set.
func (s *Server) siteFor(r *http.Request) string {
	if v := strings.TrimSpace(r.PostFormValue("site")); v != "" {
		return v
	}
	return s.scopeOf(r).ID
}

func (s *Server) back(w http.ResponseWriter, r *http.Request, to, msg string) {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	http.Redirect(w, r, to+sep+"msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func msgOr(err error, ok string) string {
	if err != nil {
		return "Error: " + err.Error()
	}
	return ok
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// countsAsRevenue: money that really arrived (vouchers and admin grants are free time).
func countsAsRevenue(p store.Payment) bool {
	return p.Status == store.PayApplied && p.Method != "voucher" && p.Method != "admin"
}

// ---- accounts ----------------------------------------------------------------

// routerSummary pings the selected building, or every runnable one.
func (s *Server) routerSummary(ctx context.Context, sc scope) (bool, string) {
	if !sc.All {
		err := s.svc.Ping(ctx, sc.ID)
		return err == nil, errString(err)
	}
	up, n := 0, 0
	var firstErr string
	for _, st := range sc.Sites {
		if !st.Runnable() {
			continue
		}
		n++
		if err := s.svc.Ping(ctx, st.ID); err != nil {
			if firstErr == "" {
				firstErr = st.Name + ": " + err.Error()
			}
			continue
		}
		up++
	}
	if n == 0 {
		return false, "no router is connected yet"
	}
	if up == n {
		return true, strconv.Itoa(n) + " of " + strconv.Itoa(n) + " routers reachable"
	}
	return false, strconv.Itoa(up) + " of " + strconv.Itoa(n) + " routers reachable. " + firstErr
}

type acctRow struct {
	A         store.Account
	Pkg       store.Package
	State     string
	Router    string
	Seen      string
	Sync      string
	SyncCls   string
	Online    bool
	New       bool // created in the last 7 days
	Expiring7 bool
}

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "accounts")
	now := s.svc.Now()
	sc := s.scopeOf(r)
	live := map[string]bool{}
	for _, row := range s.liveRows(sc).Rows {
		if row.Kind == "flat" {
			live[row.SiteID+"/"+row.Code] = true
		}
	}
	names := map[string]string{}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
	}
	tab := r.URL.Query().Get("tab")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var all []acctRow
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if !sc.Has(a.SiteID) {
				continue
			}
			row := acctRow{A: a, Pkg: d.Packages[a.PackageID], State: billing.AccountState(a, now), Router: names[a.SiteID], Online: live[a.SiteID+"/"+a.Code]}
			row.New = now.Sub(a.CreatedAt) < 7*24*time.Hour
			row.Expiring7 = row.State == billing.StateActive && a.ExpiresAt.Sub(now) <= 7*24*time.Hour
			all = append(all, row)
		}
	})
	var atRisk int
	for i := range all {
		rs := s.svc.AccountRouterState(all[i].A.ID)
		switch {
		case rs.PendingTask:
			all[i].Sync, all[i].SyncCls = "pending", "pending"
		case !rs.InSync:
			all[i].Sync, all[i].SyncCls = "drift", "drift"
		default:
			all[i].Sync, all[i].SyncCls = "in sync", "synced"
		}
		switch {
		case all[i].Online:
			all[i].Seen = "online now"
		case !rs.LastSeen.IsZero():
			all[i].Seen = shortAge(now.Sub(rs.LastSeen)) + " ago"
			if all[i].State == billing.StateActive && now.Sub(rs.LastSeen) > 7*24*time.Hour {
				atRisk++
			}
		}
	}
	cnt := map[string]int{"all": len(all)}
	var online, expiring, fresh int
	var rows []acctRow
	for _, x := range all {
		cnt[x.State]++
		if x.A.MACConflict {
			cnt["conflict"]++
		}
		if x.New {
			cnt["new7"]++
			fresh++
		}
		if x.Online {
			online++
		}
		if x.Expiring7 {
			expiring++
		}
		ok := true
		switch tab {
		case "active", "expired", "suspended":
			ok = x.State == tab
		case "new":
			ok = x.State == billing.StateNew
		case "conflict":
			ok = x.A.MACConflict
		}
		if ok && q != "" {
			ok = store.ContainsFold(x.A.Flat, q) || store.ContainsFold(x.A.Tenant, q) || strings.EqualFold(x.A.Code, q) || store.ContainsFold(x.A.MAC, q) || strings.Contains(x.A.Phone, q)
		}
		if ok {
			rows = append(rows, x)
		}
	}
	if tab == "" {
		tab = "all"
	}
	p.Data["Rows"], p.Data["Tab"], p.Data["Cnt"], p.Data["Q"] = rows, tab, cnt, q
	p.Data["Total"], p.Data["Online"], p.Data["Expiring"], p.Data["Fresh"], p.Data["AtRisk"] = len(all), online, expiring, fresh, atRisk
	p.Data["FormMAC"] = r.URL.Query().Get("mac")
	p.Data["FormFlat"] = r.URL.Query().Get("flat")
	p.Data["FormSite"] = r.URL.Query().Get("site")
	s.render(w, "admin_accounts", p)
}

// customerForm is everything the Add customer form can say.
type customerForm struct {
	SiteID, Flat, Tenant, Phone, WhatsApp, MAC, PackageID, Notes string
	WASame, Activate                                             bool
}

func customerFormOf(r *http.Request, site string) customerForm {
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	return customerForm{SiteID: site, Flat: f("flat"), Tenant: f("tenant"), Phone: f("phone"), WhatsApp: f("whatsapp"), MAC: f("mac"),
		PackageID: f("package"), Notes: f("notes"), WASame: r.PostFormValue("wa_same") == "1", Activate: r.PostFormValue("activate") == "1"}
}

// createCustomer is the single place the web admin creates a customer account.
// W5c: replace the body with one call to billing.CreateCustomer(ctx,
// CustomerInput{SiteID, Flat, Tenant, Phone, MAC, PackageID, Activate, Note});
// the form, the JSON answer and the status poll stay as they are.
func (s *Server) createCustomer(ctx context.Context, in customerForm) (store.Account, error) {
	if in.SiteID == "" {
		return store.Account{}, errors.New("choose a router")
	}
	if in.Activate && in.PackageID == "" {
		return store.Account{}, errors.New("choose a plan to activate")
	}
	var a store.Account
	var err error
	if in.PackageID != "" {
		a, err = s.svc.CreateFromDevice(ctx, billing.DeviceAccountInput{SiteID: in.SiteID, MAC: in.MAC, Flat: in.Flat, Tenant: in.Tenant, Phone: in.Phone, PackageID: in.PackageID})
		if err == nil && (in.Notes != "" || in.WhatsApp != "" || in.WASame) {
			_ = s.svc.UpdateAccount(a.ID, a.Flat, a.Tenant, a.Phone, in.Notes)
			_ = s.svc.SetWhatsApp(a.ID, in.WhatsApp, in.WASame, false)
		}
	} else {
		a, err = s.svc.CreateAccount(billing.AccountInput{Flat: in.Flat, MAC: in.MAC, Tenant: in.Tenant, Phone: in.Phone, Notes: in.Notes, SiteID: in.SiteID,
			WhatsApp: in.WhatsApp, WASamePhone: in.WASame})
	}
	if err != nil {
		return store.Account{}, err
	}
	if in.Activate {
		if err := s.svc.ExtendAccount(ctx, a.ID, in.PackageID, "activated when the account was added", false); err != nil {
			return a, fmt.Errorf("the account was created but could not be activated: %w", err)
		}
		a, _ = s.svc.GetAccount(a.ID)
	}
	return a, nil
}

func (s *Server) adminAccountCreate(w http.ResponseWriter, r *http.Request) {
	in := customerFormOf(r, s.siteFor(r))
	a, err := s.createCustomer(r.Context(), in)
	if wantsJSON(r) {
		if err != nil && a.ID == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		out := map[string]any{"ok": true, "id": a.ID, "code": a.Code, "flat": a.Flat, "url": s.accountURL(a.ID)}
		if err != nil {
			out["warning"] = err.Error()
		}
		writeJSON(w, http.StatusCreated, out)
		return
	}
	if err != nil && a.ID == "" {
		s.back(w, r, "/admin/accounts?mac="+url.QueryEscape(in.MAC)+"&flat="+url.QueryEscape(in.Flat), "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/accounts/view?code="+a.Code, "Account created: "+a.Flat+" ("+a.Code+")")
}

// adminAccountStatus is what the Add customer dialog polls after a create: has
// the router caught up with the new account?
func (s *Server) adminAccountStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := s.svc.GetAccount(r.URL.Query().Get("id"))
	if !ok || !s.scopeOf(r).Has(a.SiteID) {
		http.NotFound(w, r)
		return
	}
	st, _ := s.siteByID(a.SiteID)
	rs := s.svc.AccountRouterState(a.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	out := map[string]any{"in_sync": rs.InSync, "pending_task": rs.PendingTask, "lease_status": rs.LeaseStatus, "last_error": rs.LastError}
	switch {
	case !st.Runnable():
		out["state"], out["done"], out["text"] = "wait", true, "Saved. Queued: the router has not joined yet, it applies when it does"
	case rs.PendingTask && !s.siteOnline(ctx, a.SiteID):
		out["state"], out["done"], out["text"] = "wait", true, "Queued: the router is offline, this applies automatically"
	case rs.PendingTask || !rs.InSync:
		out["state"], out["done"], out["text"] = "work", false, "Applying on the router"
		if rs.LastError != "" {
			out["text"] = "Applying on the router (" + rs.LastError + ")"
		}
	default:
		out["state"], out["done"], out["text"] = "ok", true, "Provisioned on router"
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminAccount(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "accounts")
	a, ok := s.svc.AccountByCode(r.URL.Query().Get("code"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var pays []store.Payment
	var pkg store.Package
	pkgMap := map[string]store.Package{}
	var pkgs []store.Package
	s.st.View(func(d *store.Data) {
		for _, pay := range d.PaymentList() {
			if pay.AccountID == a.ID {
				pays = append(pays, pay)
			}
		}
		pkg = d.Packages[a.PackageID]
		for k, v := range d.Packages {
			pkgMap[k] = v
		}
		pkgs = d.EffectivePackages(a.SiteID)
		for _, ep := range pkgs {
			pkgMap[ep.ID] = ep
		}
		if ep, ok := d.EffectivePackage(a.SiteID, a.PackageID); ok {
			pkg = ep
		}
	})
	tab := r.URL.Query().Get("tab")
	if tab != "payments" && tab != "usage" && tab != "events" {
		tab = "overview"
	}
	p.Data["Tab"] = tab
	p.Detail, p.H1a, p.H1b, p.Lead = a.Flat, "Flat", a.Flat+".", ""
	p.Label, p.Href, p.Group = "Subscribers", "/admin/accounts", "Customers"
	var evs []store.Event
	s.st.View(func(d *store.Data) {
		for i := len(d.Events) - 1; i >= 0 && len(evs) < 60; i-- {
			if d.Events[i].AccountID == a.ID {
				evs = append(evs, d.Events[i])
			}
		}
	})
	p.Data["Events"] = evs
	if col := s.mgr.Collector(a.SiteID); col != nil && tab == "usage" {
		fr := col.BuildFlat(a.Code, metrics.PeriodMonth, s.svc.Now())
		if fr.Found {
			p.Data["Usage"] = fr.Row
		}
	}
	p.Data["RS"] = s.svc.AccountRouterState(a.ID)
	p.Data["A"], p.Data["Pkg"], p.Data["State"] = a, pkg, billing.AccountState(a, s.svc.Now())
	p.Data["Payments"], p.Data["PkgMap"], p.Data["Pkgs"] = pays, pkgMap, pkgs
	p.Data["AcctSite"] = p.Data["SiteNames"].(map[string]string)[a.SiteID]
	var others []store.Site
	for _, st := range p.Sites {
		if st.ID != a.SiteID {
			others = append(others, st)
		}
	}
	p.Data["MoveTo"] = others
	s.accountCards(p.Data, a, s.scopeOf(r))
	s.render(w, "admin_account", p)
}

func (s *Server) accountURL(id string) string {
	if a, ok := s.svc.GetAccount(id); ok {
		return "/admin/accounts/view?code=" + a.Code
	}
	return "/admin/accounts"
}

func (s *Server) adminAccountMove(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := r.PostFormValue("to")
	a, _ := s.svc.GetAccount(id)
	if strings.ToUpper(strings.TrimSpace(r.PostFormValue("confirm"))) != a.Code {
		s.back(w, r, s.accountURL(id), "Error: type the account code to confirm the move")
		return
	}
	if err := s.svc.MoveAccount(r.Context(), id, to); err != nil {
		s.back(w, r, s.accountURL(id), "Error: "+err.Error())
		return
	}
	s.back(w, r, s.accountURL(id), "Moved. Its ONT MAC must now be on the new router's network. Payments already made stay with the old router in the ledger.")
}

func (s *Server) adminAccountUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.UpdateAccount(id, r.PostFormValue("flat"), r.PostFormValue("tenant"), r.PostFormValue("phone"), r.PostFormValue("notes"))
	if err == nil {
		err = s.svc.SetWhatsApp(id, r.PostFormValue("whatsapp"), r.PostFormValue("wa_same") == "1", r.PostFormValue("wa_optout") == "1")
	}
	s.back(w, r, s.accountURL(id), msgOr(err, "Details saved"))
}

func (s *Server) adminAccountMAC(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.ChangeMAC(r.Context(), id, r.PostFormValue("mac"))
	s.back(w, r, s.accountURL(id), msgOr(err, "ONT MAC updated. The old session is being ended."))
}

func (s *Server) adminAccountSuspend(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	on := r.PostFormValue("suspend") == "1"
	err := s.svc.SetSuspended(r.Context(), id, on)
	msg := "Account unsuspended"
	if on {
		msg = "Account suspended"
	}
	s.back(w, r, s.accountURL(id), msgOr(err, msg))
}

func (s *Server) adminAccountExtend(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.ExtendAccount(r.Context(), id, r.PostFormValue("package"), r.PostFormValue("note"), r.PostFormValue("force") == "1")
	s.back(w, r, s.accountURL(id), msgOr(err, "Package added (free grant, logged in the payments list)"))
}

func (s *Server) adminAccountPackage(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	keep := r.PostFormValue("mode") != "fresh"
	err := s.svc.ChangePackage(r.Context(), id, r.PostFormValue("package"), keep)
	msg := "Package changed; expiry kept. The session restarts at the new speed."
	if !keep {
		msg = "Package changed and started fresh from now. The session restarts at the new speed."
	}
	s.back(w, r, s.accountURL(id), msgOr(err, msg))
}

func (s *Server) adminAccountEndPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	s.back(w, r, s.accountURL(id), msgOr(s.svc.EndPlan(r.Context(), id), "Plan ended. The flat is offline until it buys again."))
}

func (s *Server) adminAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	a, _ := s.svc.GetAccount(id)
	if err := s.svc.DeleteAccount(r.Context(), id, strings.ToUpper(strings.TrimSpace(r.PostFormValue("confirm")))); err != nil {
		s.back(w, r, s.accountURL(id), "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/accounts", "Account deleted: "+a.Flat+" ("+a.Code+"). Its payments stay in the ledger.")
}

func (s *Server) adminAccountKick(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	a, _ := s.svc.GetAccount(id)
	s.back(w, r, s.accountURL(id), msgOr(s.svc.Kick(r.Context(), a.SiteID, a.Code), "Session ended (the flat logs in again by itself if still active)"))
}

// ---- payments ledger ----------------------------------------------------------------

type payRow struct {
	P   store.Payment
	A   store.Account
	Pkg store.Package
}

func (s *Server) payRows(sc scope, status string, limit int) []payRow {
	var out []payRow
	s.st.View(func(d *store.Data) {
		for _, p := range d.PaymentList() {
			if status != "" && p.Status != status {
				continue
			}
			// An unmatched payment may not know its building yet; it shows everywhere.
			if !sc.Has(p.SiteID) && !(p.SiteID == "" && p.Status == store.PayUnmatched) {
				continue
			}
			out = append(out, payRow{p, d.Accounts[p.AccountID], d.Packages[p.PackageID]})
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	})
	return out
}

func (s *Server) adminPayments(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "payments")
	f, form := s.payFilter(r)
	rows, tot := s.svc.FilterPayments(f)
	const limit = 500
	shown := rows
	if len(shown) > limit {
		shown = shown[:limit]
	}
	var out []payRow
	s.st.View(func(d *store.Data) {
		for _, pay := range shown {
			a, _ := d.AnyAccount(pay.AccountID)
			out = append(out, payRow{pay, a, d.Packages[pay.PackageID]})
		}
	})
	// Ledger strip: money in today, this week, this month, and the tab counts.
	now := s.svc.Now()
	nowE := now.In(billing.EAT)
	dayStart := time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, billing.EAT)
	monthStart := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, billing.EAT)
	var pToday, pWeek, pMonth, pMpesa int
	pc := map[string]int{}
	sc := s.scopeOf(r)
	s.st.View(func(d *store.Data) {
		for _, pay := range d.Payments {
			if !sc.Has(pay.SiteID) && !(pay.SiteID == "" && pay.Status == store.PayUnmatched) {
				continue
			}
			pc["all"]++
			switch {
			case pay.Status == store.PayUnmatched:
				pc["unmatched"]++
			case pay.Status == store.PayRefunded:
				pc["refunded"]++
			}
			switch pay.Method {
			case "mpesa", "cash", "voucher":
				pc[pay.Method]++
			}
			if !countsAsRevenue(pay) {
				continue
			}
			if !pay.PaidAt.Before(dayStart) {
				pToday += pay.AmountKES
			}
			if !pay.PaidAt.Before(dayStart.AddDate(0, 0, -6)) {
				pWeek += pay.AmountKES
			}
			if !pay.PaidAt.Before(monthStart) {
				pMonth += pay.AmountKES
				if pay.Method == "mpesa" {
					pMpesa += pay.AmountKES
				}
			}
		}
	})
	share := 0
	if pMonth > 0 {
		share = 100 * pMpesa / pMonth
	}
	ptab := "all"
	switch {
	case form.Status == "unmatched" || form.Status == "refunded":
		ptab = form.Status
	case form.Method == "mpesa" || form.Method == "cash" || form.Method == "voucher":
		ptab = form.Method
	}
	p.Data["PToday"], p.Data["PWeek"], p.Data["PMonth"], p.Data["PShare"], p.Data["PCnt"], p.Data["PTab"] = pToday, pWeek, pMonth, share, pc, ptab
	p.Data["Rows"], p.Data["Status"], p.Data["F"], p.Data["Tot"] = out, form.Status, form, tot
	p.Data["Methods"], p.Data["Total"], p.Data["Limit"] = s.paymentMethods(), len(rows), limit
	p.Data["Query"] = r.URL.RawQuery
	s.render(w, "admin_payments", p)
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func (s *Server) adminPaymentsCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="payments-ledger.csv"`)
	f, _ := s.payFilter(r)
	rows, _ := s.svc.FilterPayments(f)
	names := map[string]string{}
	for _, st := range s.scopeOf(r).Sites {
		names[st.ID] = st.Name
	}
	s.writePaymentsCSV(w, rows, names)
}

func (s *Server) adminRecordForm(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "record")
	sc := s.scopeOf(r)
	var accts []store.Account
	var pkgs []store.Package
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if sc.Has(a.SiteID) {
				accts = append(accts, a)
			}
		}
		pkgs = d.EffectivePackages(sc.ID)
	})
	amount, _ := strconv.Atoi(r.URL.Query().Get("amount"))
	selected := ""
	if amount > 0 {
		site := sc.ID
		if ac, ok := s.svc.AccountByCode(r.URL.Query().Get("account")); ok {
			site = ac.SiteID
		}
		if pk, ok := s.svc.PackageForAmount(site, amount); ok {
			selected = pk.ID
		}
	}
	p.Data["Accounts"], p.Data["Pkgs"] = accts, pkgs
	p.Data["Code"], p.Data["Amount"], p.Data["Selected"] = r.URL.Query().Get("account"), amount, selected
	p.Data["Trans"], p.Data["Phone"] = r.URL.Query().Get("trans"), r.URL.Query().Get("phone")
	s.render(w, "admin_record", p)
}

func (s *Server) adminRecordPost(w http.ResponseWriter, r *http.Request) {
	a, ok := s.svc.AccountByCode(r.PostFormValue("account"))
	if !ok {
		s.back(w, r, "/admin/payments/record", "Error: choose an account")
		return
	}
	amount, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("amount")))
	var paid time.Time
	if v := strings.TrimSpace(r.PostFormValue("date")); v != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", v, billing.EAT)
		if err != nil {
			t, err = time.ParseInLocation("2006-01-02", v, billing.EAT)
		}
		if err != nil {
			s.back(w, r, "/admin/payments/record", "Error: the date is not valid")
			return
		}
		paid = t
	}
	pay, err := s.svc.RecordManual(r.Context(), billing.ManualInput{
		AccountID: a.ID, TransCode: r.PostFormValue("trans"), AmountKES: amount, Phone: r.PostFormValue("phone"),
		PackageID: r.PostFormValue("package"), Force: r.PostFormValue("force") == "1", PaidAt: paid, Note: r.PostFormValue("note"),
	})
	if errors.Is(err, billing.ErrLowerPlan) {
		s.back(w, r, "/admin/unmatched", "This flat is active on a faster plan, so the payment was put in the Unmatched list instead of lowering the speed. Assign it there, or record it again with \"apply even if lower speed\" ticked.")
		return
	}
	if err != nil {
		q := "?account=" + url.QueryEscape(a.Code) + "&amount=" + strconv.Itoa(amount) + "&trans=" + url.QueryEscape(r.PostFormValue("trans")) + "&phone=" + url.QueryEscape(r.PostFormValue("phone"))
		s.back(w, r, "/admin/payments/record"+q, "Error: "+err.Error())
		return
	}
	s.back(w, r, s.accountURL(a.ID), "Recorded "+pay.Key+": KES "+strconv.Itoa(pay.AmountKES)+" applied to "+a.Flat)
}

func (s *Server) adminUnmatched(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "unmatched")
	sc := s.scopeOf(r)
	var accts []store.Account
	var pkgs []store.Package
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if sc.Has(a.SiteID) {
				accts = append(accts, a)
			}
		}
		pkgs = d.PackageList()
	})
	var open, done []payRow
	for _, row := range s.payRows(sc, "", 0) {
		switch row.P.Status {
		case store.PayUnmatched:
			open = append(open, row)
		case store.PayRefunded, store.PayIgnored:
			done = append(done, row)
		}
	}
	// A payment also counts as handled once assigned; show those with "applied" only in the ledger.
	p.Data["Open"], p.Data["Done"], p.Data["Accounts"], p.Data["Pkgs"] = open, done, accts, pkgs
	s.render(w, "admin_unmatched", p)
}

func (s *Server) adminUnmatchedAssign(w http.ResponseWriter, r *http.Request) {
	err := s.svc.AssignPayment(r.Context(), r.PostFormValue("key"), r.PostFormValue("account"), r.PostFormValue("package"), r.PostFormValue("note"))
	s.back(w, r, "/admin/unmatched", msgOr(err, "Payment assigned and applied"))
}

func (s *Server) adminUnmatchedResolve(w http.ResponseWriter, r *http.Request) {
	err := s.svc.ResolvePayment(r.PostFormValue("key"), r.PostFormValue("status"), r.PostFormValue("note"))
	s.back(w, r, "/admin/unmatched", msgOr(err, "Marked "+r.PostFormValue("status")))
}

// ---- orders, packages, vouchers ----------------------------------------------------------

func (s *Server) adminOrders(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "orders")
	sc := s.scopeOf(r)
	s.st.View(func(d *store.Data) {
		var o []store.Order
		for _, x := range d.OrderList() {
			if sc.Has(x.SiteID) {
				o = append(o, x)
			}
		}
		if len(o) > 300 {
			o = o[:300]
		}
		p.Data["Orders"] = o
		p.Data["PkgMap"] = d.Packages
		p.Data["AcctMap"] = d.Accounts
	})
	s.render(w, "admin_orders", p)
}

func (s *Server) adminMarkPaid(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.CompleteOrder(r.Context(), id, "MANUAL-"+strings.ToUpper(id))
	s.back(w, r, "/admin/orders", msgOr(err, "Order marked paid"))
}

func (s *Server) adminPackages(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "packages")
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "all"
	}
	cnt := map[string]int{}
	subs := map[string]int{}
	var shown []store.Package
	var active, disabled, subTotal int
	now := s.svc.Now()
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.PackageID != "" && billing.AccountState(a, now) == "active" {
				subs[a.PackageID]++
				subTotal++
			}
		}
		for _, pk := range d.PackageList() {
			if pk.Deleted {
				continue
			}
			quick := pk.DurationMin < 28*24*60
			kinds := []string{"all"}
			if pk.Disabled {
				disabled++
				kinds = append(kinds, "disabled")
			} else {
				active++
			}
			if quick {
				kinds = append(kinds, "quick")
			} else {
				kinds = append(kinds, "monthly")
			}
			match := false
			for _, k := range kinds {
				cnt[k]++
				match = match || k == tab
			}
			if match {
				shown = append(shown, pk)
			}
		}
	})
	p.Data["Pkgs"], p.Data["Tab"], p.Data["Cnt"] = shown, tab, cnt
	p.Data["PActive"], p.Data["PDisabled"], p.Data["PSubs"], p.Data["PSubMap"] = active, disabled, subTotal, subs
	s.render(w, "admin_packages", p)
}

func (s *Server) adminSavePackage(w http.ResponseWriter, r *http.Request) {
	atoi := func(k string) int { n, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue(k))); return n }
	pkg := store.Package{
		ID: r.PostFormValue("id"), Name: strings.TrimSpace(r.PostFormValue("name")),
		DurationMin: atoi("duration"), SpeedMbps: atoi("speed"), PriceKES: atoi("price"),
		Disabled: r.PostFormValue("disabled") == "1",
	}
	_, err := s.svc.SavePackage(r.Context(), pkg)
	s.back(w, r, "/admin/packages", msgOr(err, "Package saved"))
}

func (s *Server) adminVouchers(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "vouchers")
	sc := s.scopeOf(r)
	s.st.View(func(d *store.Data) {
		var v []store.Voucher
		for _, x := range d.VoucherList() {
			// A voucher for "any building" shows in every view.
			if x.SiteID == "" || sc.Has(x.SiteID) {
				v = append(v, x)
			}
		}
		if len(v) > 500 {
			v = v[:500]
		}
		p.Data["Vouchers"] = v
		p.Data["Pkgs"] = d.PackageList()
		p.Data["PkgMap"] = d.Packages
	})
	s.render(w, "admin_vouchers", p)
}

func (s *Server) adminGenVouchers(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PostFormValue("count"))
	batch, _, err := s.svc.GenerateVouchers(r.PostFormValue("package"), strings.TrimSpace(r.PostFormValue("site")), n)
	if err != nil {
		s.back(w, r, "/admin/vouchers", "Error: "+err.Error())
		return
	}
	http.Redirect(w, r, "/admin/vouchers/print?batch="+batch, http.StatusSeeOther)
}

func (s *Server) adminPrintVouchers(w http.ResponseWriter, r *http.Request) {
	batch := r.URL.Query().Get("batch")
	var list []store.Voucher
	pkgs := map[string]store.Package{}
	s.st.View(func(d *store.Data) {
		for _, v := range d.VoucherList() {
			if v.Batch == batch && !v.Used {
				list = append(list, v)
			}
		}
		for k, v := range d.Packages {
			pkgs[k] = v
		}
	})
	s.render(w, "admin_print", map[string]any{"Brand": s.cfg.BrandName, "Vouchers": list, "Pkgs": pkgs, "Batch": batch, "Support": s.cfg.SupportPhone})
}

func (s *Server) adminDeletePackage(w http.ResponseWriter, r *http.Request) {
	s.back(w, r, "/admin/packages", msgOr(s.svc.DeletePackage(r.Context(), r.PostFormValue("id")), "Package deleted"))
}
