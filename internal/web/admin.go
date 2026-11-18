package web

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

const csrfCookie = "ispbill_csrf"

func (s *Server) adminRoutes(mux *http.ServeMux) {
	get := func(p string, h http.HandlerFunc) { mux.Handle("GET "+p, s.admin(h)) }
	post := func(p string, h http.HandlerFunc) { mux.Handle("POST "+p, s.admin(h)) }
	get("/admin", s.adminDashboard)
	// Log out: always 401, so the browser forgets the cached password.
	mux.HandleFunc("GET /admin/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="ispbill admin", charset="UTF-8"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width"><body style="font:16px system-ui;background:#0d1117;color:#e6e9ef;display:grid;place-items:center;height:90vh"><div>Logged out. <a style="color:#8b83ff" href="/admin">Log in again</a></div>`)
	})
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
	get("/admin/devices", s.adminDevices)
	post("/admin/devices/create", s.adminDeviceCreate)
	post("/admin/devices/attach", s.adminDeviceAttach)
	post("/admin/devices/dismiss", s.adminDeviceDismiss)
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
	get("/admin/settings", s.adminSettings)
	s.metricsRoutes(get, post)
	s.opsRoutes(get, post)
}

// admin enforces basic auth and, for POST, the CSRF double-submit check.
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.fromTunnel(r) {
			// The admin is never served to the router tunnels.
			http.NotFound(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		userOK := safeEq(u, s.cfg.AdminUser)
		passOK := safeEq(p, s.cfg.AdminPass)
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="ispbill admin", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		c, err := r.Cookie(csrfCookie)
		token := ""
		if err == nil {
			token = c.Value
		}
		if r.Method == http.MethodPost {
			if token == "" || !safeEq(r.PostFormValue("_csrf"), token) {
				http.Error(w, "CSRF check failed", http.StatusForbidden)
				return
			}
		} else if token == "" {
			b := make([]byte, 16)
			rand.Read(b)
			token = hex.EncodeToString(b)
			http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: token, Path: "/admin",
				HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		r = r.WithContext(context.WithValue(r.Context(), csrfKey{}, token))
		h(w, r)
	})
}

type csrfKey struct{}

func csrfOf(r *http.Request) string { t, _ := r.Context().Value(csrfKey{}).(string); return t }

type adminPage struct {
	Brand, Active, CSRF, Flash string
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
}

func (s *Server) page(r *http.Request, active string) adminPage {
	p := adminPage{Brand: s.cfg.BrandName, Active: active, CSRF: csrfOf(r), Flash: r.URL.Query().Get("msg"), Data: map[string]any{}}
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
	s.st.View(func(d *store.Data) {
		for _, dv := range d.Devices {
			if sc.Has(dv.SiteID) {
				p.UnregCount++
			}
		}
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

// ---- dashboard ---------------------------------------------------------------

func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	if sc := s.scopeOf(r); sc.All && sc.Multi() && s.hasOps() {
		http.Redirect(w, r, "/admin/fleet", http.StatusSeeOther) // many buildings: start at the fleet overview
		return
	}
	p := s.page(r, "dashboard")
	now := s.svc.Now()
	nowE := now.In(billing.EAT)
	dayStart := time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, billing.EAT)
	monthStart := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, billing.EAT)
	pol := s.svc.Policy()
	sc := s.scopeOf(r)
	var active, soon, expired, newAcc, susp, today, month, pending, total int
	var events []store.Event
	var tasks []store.Task
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if !sc.Has(a.SiteID) {
				continue
			}
			total++
			switch billing.AccountState(a, now) {
			case billing.StateActive:
				active++
				if a.ExpiresAt.Sub(now) <= time.Duration(pol.ReminderDays)*24*time.Hour {
					soon++
				}
			case billing.StateExpired:
				expired++
			case billing.StateSuspended:
				susp++
			default:
				newAcc++
			}
		}
		for _, o := range d.Orders {
			if o.Status == store.OrderPending && sc.Has(o.SiteID) {
				pending++
			}
		}
		for _, pay := range d.Payments {
			if countsAsRevenue(pay) && sc.Has(pay.SiteID) {
				if !pay.PaidAt.Before(dayStart) {
					today += pay.AmountKES
				}
				if !pay.PaidAt.Before(monthStart) {
					month += pay.AmountKES
				}
			}
		}
		ev := d.Events
		for i := len(ev) - 1; i >= 0 && len(events) < 8; i-- {
			if sc.Has(ev[i].SiteID) {
				events = append(events, ev[i])
			}
		}
		for _, t := range d.TaskList() {
			if sc.Has(t.SiteID) {
				tasks = append(tasks, t)
			}
		}
	})
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	routerOK, routerMsg := s.routerSummary(ctx, sc)
	p.Data = map[string]any{"Total": total, "Active": active, "Soon": soon, "Expired": expired, "New": newAcc, "Suspended": susp,
		"Today": today, "Month": month, "Pending": pending, "Events": events, "Tasks": tasks,
		"RouterOK": routerOK, "RouterErr": routerMsg, "TestMode": s.cfg.TestMode, "Mpesa": s.cfg.MpesaEnabled(),
		"SiteNames": p.Data["SiteNames"], "ShowSite": p.Data["ShowSite"]}
	s.render(w, "admin_dashboard", p)
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
		return false, "no building is connected yet"
	}
	if up == n {
		return true, strconv.Itoa(n) + " of " + strconv.Itoa(n) + " buildings reachable"
	}
	return false, strconv.Itoa(up) + " of " + strconv.Itoa(n) + " buildings reachable. " + firstErr
}

type acctRow struct {
	A     store.Account
	Pkg   store.Package
	State string
}

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "accounts")
	now := s.svc.Now()
	var rows []acctRow
	sc := s.scopeOf(r)
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if !sc.Has(a.SiteID) {
				continue
			}
			rows = append(rows, acctRow{a, d.Packages[a.PackageID], billing.AccountState(a, now)})
		}
	})
	p.Data["Rows"] = rows
	p.Data["FormMAC"] = r.URL.Query().Get("mac")
	p.Data["FormFlat"] = r.URL.Query().Get("flat")
	s.render(w, "admin_accounts", p)
}

func (s *Server) adminAccountCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.CreateAccount(billing.AccountInput{
		Flat: r.PostFormValue("flat"), MAC: r.PostFormValue("mac"), Tenant: r.PostFormValue("tenant"),
		Phone: r.PostFormValue("phone"), Notes: r.PostFormValue("notes"), SiteID: s.siteFor(r),
		WhatsApp: r.PostFormValue("whatsapp"), WASamePhone: r.PostFormValue("wa_same") == "1",
	})
	if err != nil {
		s.back(w, r, "/admin/accounts?mac="+url.QueryEscape(r.PostFormValue("mac"))+"&flat="+url.QueryEscape(r.PostFormValue("flat")), "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/accounts/view?code="+a.Code, "Account created: "+a.Flat+" ("+a.Code+")")
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
	s.back(w, r, s.accountURL(id), "Moved. Its ONT MAC must now be on the new building's network. Payments already made stay with the old building in the ledger.")
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

// ---- unregistered devices ---------------------------------------------------------

func (s *Server) adminDevices(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "devices")
	sc := s.scopeOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	if !sc.All { // refreshing asks one router; "all buildings" shows what is already known
		if err := s.svc.RefreshHosts(ctx, sc.ID); err != nil {
			p.Data["HostErr"] = err.Error()
		}
	}
	var devs []store.Device
	var accts []store.Account
	s.st.View(func(d *store.Data) {
		for _, dv := range d.DeviceList() {
			if sc.Has(dv.SiteID) {
				devs = append(devs, dv)
			}
		}
		for _, a := range d.AccountList() {
			if sc.Has(a.SiteID) {
				accts = append(accts, a)
			}
		}
	})
	p.UnregCount = len(devs)
	p.Data["Devices"], p.Data["Accounts"] = devs, accts
	s.render(w, "admin_devices", p)
}

func (s *Server) adminDeviceCreate(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.CreateAccount(billing.AccountInput{Flat: r.PostFormValue("flat"), MAC: r.PostFormValue("mac"), Tenant: r.PostFormValue("tenant"), SiteID: s.siteFor(r)})
	if err != nil {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/accounts/view?code="+a.Code, "Account created: "+a.Flat+" ("+a.Code+")")
}

func (s *Server) adminDeviceAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("account")
	err := s.svc.ChangeMAC(r.Context(), id, r.PostFormValue("mac"))
	if err != nil {
		s.back(w, r, "/admin/devices", "Error: "+err.Error())
		return
	}
	s.back(w, r, s.accountURL(id), "Device attached: the account now uses this MAC")
}

func (s *Server) adminDeviceDismiss(w http.ResponseWriter, r *http.Request) {
	s.back(w, r, "/admin/devices", msgOr(s.svc.DismissDevice(s.siteFor(r), r.PostFormValue("mac")), "Dismissed (it comes back if it opens the portal again)"))
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
	st := r.URL.Query().Get("status")
	p.Data["Rows"] = s.payRows(s.scopeOf(r), st, 500)
	p.Data["Status"] = st
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
	cw := csv.NewWriter(w)
	cw.Write([]string{"key", "paid_at_eat", "recorded_at_eat", "method", "status", "amount_kes", "account_code", "flat", "building", "package", "payer_phone", "payer_name", "bill_ref", "order_id", "note"})
	f := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(billing.EAT).Format("2006-01-02 15:04:05")
	}
	sc := s.scopeOf(r)
	names := map[string]string{}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
	}
	for _, row := range s.payRows(sc, "", 0) {
		p := row.P
		cw.Write([]string{csvSafe(p.Key), f(p.PaidAt), f(p.CreatedAt), p.Method, p.Status, strconv.Itoa(p.AmountKES), row.A.Code,
			csvSafe(row.A.Flat), csvSafe(names[p.SiteID]), csvSafe(row.Pkg.Name), csvSafe(p.Phone), csvSafe(p.Name), csvSafe(p.BillRef), p.OrderID, csvSafe(p.Note)})
	}
	cw.Flush()
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
	s.st.View(func(d *store.Data) { p.Data["Pkgs"] = d.PackageList() })
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

// ---- settings / info ---------------------------------------------------------------

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "settings")
	pol := s.svc.Policy()
	base := s.cfg.PublicBaseURL
	if base == "" {
		base = "https://YOUR-PUBLIC-URL"
	}
	tok := ""
	if s.cfg.MpesaCallbackToken != "" {
		tok = "?t=TOKEN"
	}
	p.Data = map[string]any{
		"Policy": pol, "Cfg": s.cfg, "Mpesa": s.cfg.MpesaEnabled(),
		"CBStk": base + "/pay/cb/stk" + tok, "CBVal": base + "/pay/cb/c2b/validate" + tok, "CBConf": base + "/pay/cb/c2b/confirm" + tok,
		"Support":    s.cfg.SupportPhone,
		"WGEndpoint": s.cfg.WGEndpoint, "WGSubnet": s.cfg.WGSubnet, "JoinTTL": s.cfg.JoinTokenTTL.String(),
		"Stray": s.mgr.Stray(),
	}
	if env, ok := s.envSite(); ok {
		p.Data["Env"] = s.mgr.Effective(env)
	}
	s.render(w, "admin_settings", p)
}

func (s *Server) adminDeletePackage(w http.ResponseWriter, r *http.Request) {
	s.back(w, r, "/admin/packages", msgOr(s.svc.DeletePackage(r.Context(), r.PostFormValue("id")), "Package deleted"))
}
