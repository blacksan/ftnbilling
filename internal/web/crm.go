package web

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

func (s *Server) crmRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/search", s.adminSearch)
	get("/admin/customers", s.adminCustomersGone)
	get("/admin/customers/view", s.adminCustomer)
	post("/admin/customers/save", s.adminCustomerSave)
	post("/admin/customers/link", s.adminCustomerLink)
	post("/admin/customers/moveout", s.adminCustomerMoveOut)
	post("/admin/customers/note", s.adminCustomerNote)
	post("/admin/customers/natid", s.adminCustomerNatID)
	post("/admin/customers/natid/reveal", s.adminCustomerReveal)
	post("/admin/customers/anonymize", s.adminCustomerAnonymize)
	post("/admin/accounts/customer", s.adminAccountCustomer)
	post("/admin/accounts/location", s.adminAccountLocation)
	post("/admin/accounts/note", s.adminAccountNote)
	get("/admin/equipment", s.adminEquipment)
	get("/admin/equipment/view", s.adminEquipmentView)
	post("/admin/equipment/save", s.adminEquipmentSave)
	post("/admin/equipment/import", s.adminEquipmentImport)
	post("/admin/equipment/assign", s.adminEquipmentAssign)
	post("/admin/equipment/return", s.adminEquipmentReturn)
	post("/admin/equipment/note", s.adminEquipmentNote)
	post("/admin/sites/profile", s.adminSiteProfile)
	// Router contacts were replaced by the apartment's caretaker and landlord.
	post("/admin/sites/contacts/save", s.adminContactGone)
	post("/admin/sites/contacts/delete", s.adminContactGone)
}

// parseDay reads an EAT calendar day (2026-11-17); empty is the zero time.
func parseDay(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation("2006-01-02", v, billing.EAT)
}

func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(billing.EAT).Format("2006-01-02")
}

// maskCode shows the first four characters of a voucher code and hides the rest.
func maskCode(c string) string {
	if len(c) <= 4 {
		return strings.Repeat("*", len(c))
	}
	return c[:4] + strings.Repeat("*", len(c)-4)
}

func lastFour(s string) string {
	if s == "" {
		return ""
	}
	return "ID ending " + s
}

// ---- search --------------------------------------------------------------------------

func (s *Server) adminSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sc := s.scopeOf(r)
	p := s.page(r, "search")
	p.Data["Q"] = q
	if q == "" {
		s.render(w, "admin_search", p)
		return
	}
	res, ok := s.runSearch(w, r, q, sc.Has)
	if !ok {
		return
	}
	if kind, id, ok := res.Single(); ok {
		to := ""
		switch kind {
		case "customer":
			to = "/admin/customers/view?id=" + url.QueryEscape(id)
		case "account":
			to = "/admin/accounts/view?code=" + url.QueryEscape(id)
		case "equipment":
			to = "/admin/equipment/view?id=" + url.QueryEscape(id)
		case "payment":
			to = "/admin/payments?q=" + url.QueryEscape(id)
		case "apartment":
			to = aptURL(id)
		case "building":
			to = "/admin/routers/view?id=" + url.QueryEscape(id)
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	siteOf := map[string]string{}
	acctOf := map[string]store.Account{}
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			acctOf[a.ID] = a
		}
	})
	for _, st := range sc.Sites {
		siteOf[st.ID] = st.Name
	}
	p.Data["R"], p.Data["SiteOf"], p.Data["AcctOf"] = res, siteOf, acctOf
	s.render(w, "admin_search", p)
}

// ---- customers ------------------------------------------------------------------------

// adminCustomersGone: the Contacts list was removed (people are reached from
// their flat on Subscribers); old links land there.
func (s *Server) adminCustomersGone(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/accounts", http.StatusSeeOther)
}

func (s *Server) adminCustomer(w http.ResponseWriter, r *http.Request) {
	s.customerPage(w, r, "")
}

func (s *Server) customerPage(w http.ResponseWriter, r *http.Request, revealed string) {
	id := r.URL.Query().Get("id")
	if id == "" {
		id = r.PostFormValue("id")
	}
	prof, ok := s.svc.Customer(r.Context(), id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p := s.page(r, "accounts")
	sc := s.scopeOf(r)
	var free []store.Account
	siteOf := map[string]string{}
	for _, st := range sc.Sites {
		siteOf[st.ID] = st.Name
	}
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if sc.Has(a.SiteID) && a.CustomerID != id {
				free = append(free, a)
			}
		}
	})
	p.Data["P"], p.Data["Free"], p.Data["SiteOf"] = prof, free, siteOf
	p.Data["NatID"], p.Data["Revealed"] = s.svc.NationalIDEnabled(), revealed
	p.Data["Anon"] = !prof.Customer.AnonymizedAt.IsZero()
	p.Data["SeeID4"] = p.Role == store.RoleOwner
	p.Data["SiteNames"] = siteOf
	s.render(w, "admin_customer", p)
}

func (s *Server) adminCustomerSave(w http.ResponseWriter, r *http.Request) {
	in := billing.CustomerInput{ID: r.PostFormValue("id"), Name: r.PostFormValue("name"), Phone: r.PostFormValue("phone"),
		Phone2: r.PostFormValue("phone2"), WhatsApp: r.PostFormValue("whatsapp"), Email: r.PostFormValue("email"),
		Note: r.PostFormValue("notes"), WASamePhone: r.PostFormValue("wa_same") == "1", WAOptOut: r.PostFormValue("wa_optout") == "1"}
	c, err := s.svc.SaveCustomer(in)
	if err != nil {
		to := "/admin/accounts"
		if in.ID != "" {
			to = "/admin/customers/view?id=" + url.QueryEscape(in.ID)
		}
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/customers/view?id="+url.QueryEscape(c.ID), "Saved "+c.Name)
}

func (s *Server) adminCustomerLink(w http.ResponseWriter, r *http.Request) {
	cid := r.PostFormValue("id")
	at, err := parseDay(r.PostFormValue("move_in"))
	if err == nil {
		err = s.svc.LinkCustomer(r.PostFormValue("account"), cid, at)
	}
	s.back(w, r, "/admin/customers/view?id="+url.QueryEscape(cid), msgOr(err, "Linked to the house"))
}

func (s *Server) adminCustomerMoveOut(w http.ResponseWriter, r *http.Request) {
	at, err := parseDay(r.PostFormValue("move_out"))
	if err == nil {
		err = s.svc.MoveOut(r.PostFormValue("account"), at)
	}
	to := r.PostFormValue("next")
	if to == "" || !strings.HasPrefix(to, "/admin/") {
		to = "/admin/accounts"
	}
	s.back(w, r, to, msgOr(err, "Moved out. The house is vacant."))
}

func (s *Server) adminCustomerNote(w http.ResponseWriter, r *http.Request) {
	cid := r.PostFormValue("id")
	err := s.svc.AddNote(cid, "", "", "", r.PostFormValue("text"))
	s.back(w, r, "/admin/customers/view?id="+url.QueryEscape(cid), msgOr(err, "Note added"))
}

func (s *Server) adminCustomerNatID(w http.ResponseWriter, r *http.Request) {
	cid := r.PostFormValue("id")
	err := s.svc.SetNationalID(cid, r.PostFormValue("national_id"))
	s.back(w, r, "/admin/customers/view?id="+url.QueryEscape(cid), msgOr(err, "National ID saved (encrypted)"))
}

// adminCustomerReveal shows the ID once, in the response to this POST. The
// reveal is written to the audit log by the service before it is returned.
func (s *Server) adminCustomerReveal(w http.ResponseWriter, r *http.Request) {
	plain, err := s.svc.RevealNationalID(r.PostFormValue("id"))
	if err != nil {
		s.back(w, r, "/admin/customers/view?id="+url.QueryEscape(r.PostFormValue("id")), "Error: "+err.Error())
		return
	}
	q := r.URL.Query()
	q.Set("id", r.PostFormValue("id"))
	r.URL.RawQuery = q.Encode()
	s.customerPage(w, r, plain)
}

func (s *Server) adminCustomerAnonymize(w http.ResponseWriter, r *http.Request) {
	cid := r.PostFormValue("id")
	err := s.svc.AnonymizeCustomer(cid, r.PostFormValue("confirm"))
	s.back(w, r, "/admin/customers/view?id="+url.QueryEscape(cid), msgOr(err, "Customer anonymised. The payment ledger is unchanged."))
}

// adminAccountCustomer links an account to an existing customer, or makes a
// customer from the details typed on the account page and links that.
func (s *Server) adminAccountCustomer(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	back := s.accountURL(id)
	at, err := parseDay(r.PostFormValue("move_in"))
	if err != nil {
		s.back(w, r, back, "Error: the move-in date must look like 2026-11-17")
		return
	}
	cid := r.PostFormValue("customer")
	if cid == "" {
		c, err := s.svc.SaveCustomer(billing.CustomerInput{Name: r.PostFormValue("name"), Phone: r.PostFormValue("phone"),
			WhatsApp: r.PostFormValue("whatsapp"), WASamePhone: r.PostFormValue("wa_same") == "1"})
		if err != nil {
			s.back(w, r, back, "Error: "+err.Error())
			return
		}
		cid = c.ID
	}
	s.back(w, r, back, msgOr(s.svc.LinkCustomer(id, cid, at), "Customer linked"))
}

func (s *Server) adminAccountLocation(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	port, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port")))
	err := s.svc.SetLocation(id, r.PostFormValue("floor"), r.PostFormValue("odb"), r.PostFormValue("splitter"), port)
	s.back(w, r, s.accountURL(id), msgOr(err, "Location saved"))
}

func (s *Server) adminAccountNote(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	a, _ := s.svc.GetAccount(id)
	err := s.svc.AddNote(a.CustomerID, id, a.SiteID, "", r.PostFormValue("text"))
	s.back(w, r, s.accountURL(id), msgOr(err, "Note added"))
}

// accountCards adds the customer, location and equipment cards to the
// account page data.
func (s *Server) accountCards(d map[string]any, a store.Account, sc scope) {
	var cust store.Customer
	var custs []store.Customer
	var eq, stock, splitters []store.Equipment
	var notes []store.Note
	s.st.View(func(dd *store.Data) {
		cust = dd.Customers[a.CustomerID]
		for _, c := range dd.Customers {
			if c.AnonymizedAt.IsZero() {
				custs = append(custs, c)
			}
		}
		for _, e := range dd.Equipment {
			switch {
			case e.AccountID == a.ID:
				eq = append(eq, e)
			case e.Kind == store.KindSplitter && e.SiteID == a.SiteID:
				splitters = append(splitters, e)
			case e.AccountID == "" && e.Status == store.EqInStock && e.Kind != store.KindSplitter:
				stock = append(stock, e)
			}
		}
		for _, n := range dd.Notes {
			if n.AccountID == a.ID {
				notes = append(notes, n)
			}
		}
	})
	sort.Slice(custs, func(i, j int) bool { return strings.ToLower(custs[i].Name) < strings.ToLower(custs[j].Name) })
	sort.Slice(eq, func(i, j int) bool { return eq[i].Kind < eq[j].Kind })
	sort.Slice(stock, func(i, j int) bool { return stock[i].Kind+stock[i].Serial < stock[j].Kind+stock[j].Serial })
	sort.Slice(notes, func(i, j int) bool { return notes[i].Time.After(notes[j].Time) })
	d["Cust"], d["Custs"], d["Equip"], d["Stock"], d["Splitters"], d["Notes"] = cust, custs, eq, stock, splitters, notes
	if dev, ok := s.svc.AccountDevice(a.ID); ok {
		d["AcctDev"] = dev
	}
}

// ---- equipment ------------------------------------------------------------------------

type eqRow struct {
	E    store.Equipment
	A    store.Account
	Site string
}

func (s *Server) adminEquipment(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "equipment")
	sc := s.scopeOf(r)
	qv := r.URL.Query()
	kind, status, q := qv.Get("kind"), qv.Get("status"), strings.TrimSpace(qv.Get("q"))
	names := map[string]string{}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
	}
	var rows []eqRow
	counts := map[string]int{}
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.SiteID != "" && !sc.Has(e.SiteID) {
				continue
			}
			if (kind != "" && e.Kind != kind) || (status != "" && e.Status != status) {
				continue
			}
			a, _ := d.AnyAccount(e.AccountID)
			if q != "" && !(store.ContainsFold(e.Serial, q) || store.ContainsFold(e.MAC, q) || store.ContainsFold(e.Model, q) ||
				store.ContainsFold(a.Flat, q) || store.ContainsFold(a.Code, q) || store.ContainsFold(strings.ReplaceAll(e.MAC, ":", ""), strings.ReplaceAll(q, ":", ""))) {
				continue
			}
			counts[e.Status]++
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
	p.Data["Rows"], p.Data["Counts"], p.Data["Kind"], p.Data["Status"], p.Data["Q"] = rows, counts, kind, status, q
	p.Data["Kinds"], p.Data["Statuses"] = billing.EquipmentKinds, billing.EquipmentStatuses
	p.Data["Flash2"] = qv.Get("skipped")
	s.render(w, "admin_equipment", p)
}

type eqEvRow struct {
	Ev store.EquipmentEvent
	A  store.Account
}

func (s *Server) adminEquipmentView(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p := s.page(r, "equipment")
	sc := s.scopeOf(r)
	var e store.Equipment
	var ok bool
	var hist []eqEvRow
	var acct store.Account
	var cust store.Customer
	var accts []store.Account
	s.st.View(func(d *store.Data) {
		e, ok = d.Equipment[id]
		if !ok {
			return
		}
		acct, _ = d.AnyAccount(e.AccountID)
		cust = d.Customers[acct.CustomerID]
		for _, ev := range d.EquipmentEvents {
			if ev.EquipmentID == id {
				a, _ := d.AnyAccount(ev.AccountID)
				hist = append(hist, eqEvRow{ev, a})
			}
		}
		for _, a := range d.AccountList() {
			if e.SiteID == "" || a.SiteID == e.SiteID || sc.Has(a.SiteID) {
				accts = append(accts, a)
			}
		}
	})
	if !ok {
		http.NotFound(w, r)
		return
	}
	sort.Slice(hist, func(i, j int) bool { return hist[i].Ev.Time.After(hist[j].Ev.Time) })
	p.Data["E"], p.Data["Acct"], p.Data["Cust"], p.Data["Hist"], p.Data["Accts"] = e, acct, cust, hist, accts
	p.Data["Kinds"], p.Data["Statuses"] = billing.EquipmentKinds, billing.EquipmentStatuses
	p.Data["Sites"] = sc.Sites
	p.Data["SiteName"] = p.Data["SiteNames"].(map[string]string)[e.SiteID]
	s.render(w, "admin_equipment_view", p)
}

func (s *Server) adminEquipmentSave(w http.ResponseWriter, r *http.Request) {
	in := billing.EquipmentInput{ID: r.PostFormValue("id"), Kind: r.PostFormValue("kind"), Serial: r.PostFormValue("serial"),
		MAC: r.PostFormValue("mac"), Model: r.PostFormValue("model"), Firmware: r.PostFormValue("firmware"), GPONSerial: r.PostFormValue("gpon_serial"),
		SiteID: r.PostFormValue("site"), Notes: r.PostFormValue("notes")}
	if in.ID == "" && in.SiteID == "" {
		in.SiteID = s.scopeOf(r).ID
	}
	e, err := s.svc.SaveEquipment(in)
	if err != nil {
		to := "/admin/equipment"
		if in.ID != "" {
			to = "/admin/equipment/view?id=" + url.QueryEscape(in.ID)
		}
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/equipment/view?id="+url.QueryEscape(e.ID), "Saved")
}

func (s *Server) adminEquipmentImport(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.ImportEquipment(r.PostFormValue("lines"))
	if err != nil {
		s.back(w, r, nextOr(r, "/admin/equipment"), "Error: "+err.Error())
		return
	}
	msg := "Imported " + strconv.Itoa(res.Added) + " items"
	if len(res.Skipped) > 0 {
		msg += ". Skipped: " + strings.Join(res.Skipped, "; ")
	}
	s.back(w, r, nextOr(r, "/admin/equipment"), msg)
}

func (s *Server) adminEquipmentAssign(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.AssignEquipment(r.Context(), id, r.PostFormValue("account"), r.PostFormValue("site"), r.PostFormValue("use_mac") == "1")
	to := r.PostFormValue("next")
	if !strings.HasPrefix(to, "/admin/") {
		to = "/admin/equipment/view?id=" + url.QueryEscape(id)
	}
	s.back(w, r, to, msgOr(err, "Installed"))
}

func (s *Server) adminEquipmentReturn(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.ReturnEquipment(id, r.PostFormValue("status"), r.PostFormValue("note"))
	to := r.PostFormValue("next")
	if !strings.HasPrefix(to, "/admin/") {
		to = "/admin/equipment/view?id=" + url.QueryEscape(id)
	}
	s.back(w, r, to, msgOr(err, "Updated"))
}

func (s *Server) adminEquipmentNote(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.EquipmentNote(id, r.PostFormValue("text"))
	s.back(w, r, "/admin/equipment/view?id="+url.QueryEscape(id), msgOr(err, "Note added"))
}

// ---- building profile ---------------------------------------------------------------

func (s *Server) adminSiteProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := "/admin/sites/view?id=" + url.QueryEscape(id)
	lat, e1 := parseCoord(r.PostFormValue("lat"))
	lng, e2 := parseCoord(r.PostFormValue("lng"))
	gl, e3 := parseDay(r.PostFormValue("go_live"))
	if e1 != nil || e2 != nil || e3 != nil {
		s.back(w, r, to, "Error: latitude and longitude are numbers like -1.2864, and the go-live date looks like 2026-11-17")
		return
	}
	err := s.svc.SaveSiteProfile(billing.SiteProfileInput{SiteID: id, Address: r.PostFormValue("address"), Lat: lat, Lng: lng, GoLive: gl})
	s.back(w, r, to, msgOr(err, "Router profile saved"))
}

func parseCoord(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	return strconv.ParseFloat(v, 64)
}

// adminContactGone answers the removed router contacts forms with 410 Gone.
func (s *Server) adminContactGone(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Router contacts were removed. The caretaker and landlord are now part of each apartment: open Apartments.", http.StatusGone)
}

// ---- payment filters -----------------------------------------------------------------

type payFilterForm struct {
	From, To, Method, Status, Q string
}

func (s *Server) payFilter(r *http.Request) (billing.PaymentFilter, payFilterForm) {
	qv := r.URL.Query()
	form := payFilterForm{From: qv.Get("from"), To: qv.Get("to"), Method: qv.Get("method"), Status: qv.Get("status"), Q: strings.TrimSpace(qv.Get("q"))}
	f := billing.PaymentFilter{Site: s.scopeOf(r).Has, Method: form.Method, Status: form.Status, Q: form.Q}
	f.From, _ = parseDay(form.From)
	f.To, _ = parseDay(form.To)
	return f, form
}

func (s *Server) paymentMethods() []string {
	seen := map[string]bool{}
	s.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			seen[p.Method] = true
		}
	})
	var out []string
	for m := range seen {
		if m != "" {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// ---- settings: backup status -----------------------------------------------------------

// BackupStatus is DATA/backup-status.json, written by the backup tool after
// each dump, restore drill and Drive upload. Every field is optional.
type BackupStatus struct {
	DumpAt     time.Time `json:"dump_at"`
	DumpFile   string    `json:"dump_file"`
	DumpBytes  int64     `json:"dump_bytes"`
	DrillAt    time.Time `json:"drill_at"`
	DrillOK    *bool     `json:"drill_ok"`
	DrillMsg   string    `json:"drill_msg"`
	UploadAt   time.Time `json:"upload_at"`
	UploadOK   *bool     `json:"upload_ok"`
	Error      string    `json:"error"`
	Stale      bool      `json:"-"`
	Present    bool      `json:"-"`
	ReadFailed bool      `json:"-"`
}

func (s *Server) backupStatus() BackupStatus {
	var b BackupStatus
	raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "backup-status.json"))
	if err != nil {
		return b
	}
	b.Present = true
	if err := json.Unmarshal(raw, &b); err != nil {
		b = BackupStatus{Present: true, ReadFailed: true}
		return b
	}
	b.Stale = b.DumpAt.IsZero() || s.svc.Now().Sub(b.DumpAt) > 36*time.Hour
	return b
}

// csvPayments writes the filtered ledger.
func (s *Server) writePaymentsCSV(w http.ResponseWriter, rows []store.Payment, names map[string]string) {
	cw := csv.NewWriter(w)
	cw.Write([]string{"key", "paid_at_eat", "recorded_at_eat", "method", "status", "amount_kes", "account_code", "house_number", "router", "package", "payer_phone", "payer_name", "bill_ref", "order_id", "customer", "note"})
	f := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(billing.EAT).Format("2006-01-02 15:04:05")
	}
	type ctx struct {
		a    store.Account
		pkg  string
		cust string
	}
	look := map[string]ctx{}
	s.st.View(func(d *store.Data) {
		for _, p := range d.Payments {
			a, _ := d.AnyAccount(p.AccountID)
			look[p.Key] = ctx{a, d.Packages[p.PackageID].Name, d.Customers[p.CustomerID].Name}
		}
	})
	for _, p := range rows {
		c := look[p.Key]
		cw.Write([]string{csvSafe(p.Key), f(p.PaidAt), f(p.CreatedAt), p.Method, p.Status, strconv.Itoa(p.AmountKES), c.a.Code,
			csvSafe(c.a.Flat), csvSafe(names[p.SiteID]), csvSafe(c.pkg), csvSafe(p.Phone), csvSafe(p.Name), csvSafe(p.BillRef), p.OrderID, csvSafe(c.cust), csvSafe(p.Note)})
	}
	cw.Flush()
}
