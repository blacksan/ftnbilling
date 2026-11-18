package web

import (
	"net"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// PortalCtx is the hotspot context carried through every portal step.
type PortalCtx struct{ MAC, IP, Dst string }

// ctxFrom reads and sanitises mac, ip and dst from query or form.
func ctxFrom(r *http.Request) PortalCtx {
	c := PortalCtx{IP: strings.TrimSpace(r.FormValue("ip")), Dst: strings.TrimSpace(r.FormValue("dst"))}
	if m, err := billing.NormalizeMAC(r.FormValue("mac")); err == nil {
		c.MAC = m
	}
	if net.ParseIP(c.IP) == nil {
		c.IP = ""
	}
	if !isHTTPURL(c.Dst) {
		c.Dst = ""
	}
	return c
}

type portalPage struct {
	Brand, Support string
	Ctx            PortalCtx
	Known          bool // MAC belongs to an account
	Acct           store.Account
	State          string
	Pkg            store.Package // current package, if any
	Active         bool
	Suspended      bool
	Groups         []packageGroup
	Mpesa, Test    bool
	Error, Notice  string
	Phone          string
	Selected       string
	LoginFailed    bool
	Blocked        map[string]bool // slower than the active plan: not purchasable yet
	BlockedUntil   string
}

func (s *Server) portalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /portal", s.portalHome)
	mux.HandleFunc("GET /brand.woff2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Write(brandFont)
	})
	mux.HandleFunc("GET /brand/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, err := logoFS.ReadFile("static/" + r.PathValue("name"))
		if err != nil || !strings.HasPrefix(r.PathValue("name"), "logo-") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(faviconSVG)
	})
	mux.HandleFunc("POST /portal/pay", s.portalPay)
	mux.HandleFunc("POST /portal/voucher", s.portalVoucher)
	mux.HandleFunc("POST /portal/connect", s.portalConnect)
	mux.HandleFunc("GET /portal/wait", s.portalWait)
	mux.HandleFunc("GET /portal/success", s.portalSuccess)
	mux.HandleFunc("GET /portal/status", s.portalHome)
	mux.HandleFunc("GET /api/orders/{id}", s.apiOrder)
}

// pctx is ctxFrom plus, on a public server, a check that the mac/ip really
// is a device on the building hotspot. Unverified requests lose their MAC and
// get the neutral "open this page from the WiFi login" page.
func (s *Server) pctx(r *http.Request) PortalCtx {
	pc := ctxFrom(r)
	if s.guard != nil && pc.MAC != "" && !s.guard.ok(r.Context(), pc.MAC, pc.IP) {
		s.log.Warn("portal: mac/ip not on the hotspot, ignored", "mac", pc.MAC, "ip", pc.IP, "remote", r.RemoteAddr)
		return PortalCtx{Dst: pc.Dst}
	}
	return pc
}

// view builds the portal page for the MAC in the request. Unknown MACs are
// recorded for the admin ("Unregistered devices") and get the contact page.
func (s *Server) view(r *http.Request, errMsg, phone, selected string) portalPage {
	p := portalPage{
		Brand: s.cfg.BrandName, Support: s.cfg.SupportPhone,
		Mpesa: s.providers["mpesa"] != nil && s.providers["mpesa"].Enabled(),
		Test:  s.providers["test"] != nil && s.providers["test"].Enabled(),
		Error: errMsg, Phone: phone, Selected: selected,
	}
	p.Ctx = s.pctx(r)
	if p.Ctx.MAC == "" {
		return p
	}
	a, ok := s.svc.AccountByMAC(p.Ctx.MAC)
	if !ok {
		s.svc.NoteDevice(p.Ctx.MAC, p.Ctx.IP, "portal")
		return p
	}
	p.Known, p.Acct = true, a
	p.State = billing.AccountState(a, s.svc.Now())
	p.Active = p.State == billing.StateActive
	p.Suspended = a.Suspended
	s.st.View(func(d *store.Data) {
		p.Pkg = d.Packages[a.PackageID]
		p.Groups = packageGroups(d.PackageList())
	})
	if p.Active {
		p.Blocked = map[string]bool{}
		for _, g := range p.Groups {
			for _, pk := range g.Packages {
				if until, bl := s.svc.LowerPlanBlocked(a, pk); bl {
					p.Blocked[pk.ID] = true
					p.BlockedUntil = until.In(billing.EAT).Format("Mon 2 Jan 2006")
				}
			}
		}
		if p.Blocked[p.Selected] {
			p.Selected = ""
		}
	}
	if p.Selected == "" {
		for _, g := range p.Groups {
			for _, pk := range g.Packages {
				if p.Selected == "" && !p.Blocked[pk.ID] {
					p.Selected = pk.ID
				}
			}
		}
	}
	return p
}

func (s *Server) portalHome(w http.ResponseWriter, r *http.Request) {
	p := s.view(r, "", "", "")
	if p.Known && p.Active {
		if !s.svc.AutoLogin(r.Context(), p.Acct, p.Ctx.IP) && p.Ctx.IP != "" {
			p.LoginFailed = true
		}
	}
	s.render(w, "portal", p)
}

// need returns the account behind the request's MAC, or renders the right page.
func (s *Server) need(w http.ResponseWriter, r *http.Request) (store.Account, PortalCtx, bool) {
	pc := s.pctx(r)
	if pc.MAC == "" {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "portal", s.view(r, "", "", ""))
		return store.Account{}, pc, false
	}
	a, ok := s.svc.AccountByMAC(pc.MAC)
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "portal", s.view(r, "", "", ""))
		return a, pc, false
	}
	if a.Suspended {
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "portal", s.view(r, "", "", ""))
		return a, pc, false
	}
	return a, pc, true
}

func (s *Server) portalPay(w http.ResponseWriter, r *http.Request) {
	a, pc, ok := s.need(w, r)
	if !ok {
		return
	}
	method := r.FormValue("method")
	pkgID := r.FormValue("package")
	prov := s.providers[method]
	fail := func(msg string) {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "portal", s.view(r, msg, r.FormValue("phone"), pkgID))
	}
	if prov == nil || !prov.Enabled() || method == "voucher" {
		fail("That payment method is not available.")
		return
	}
	phone := ""
	if raw := strings.TrimSpace(r.FormValue("phone")); raw != "" || method == "mpesa" {
		var err error
		if phone, err = billing.NormalizePhone(raw); err != nil {
			fail(err.Error())
			return
		}
	}
	o, err := s.svc.CreateOrder(billing.OrderInput{
		AccountID: a.ID, PackageID: pkgID, Phone: phone, Method: method, MAC: pc.MAC, IP: pc.IP, Dst: pc.Dst,
	})
	if err != nil {
		fail(err.Error())
		return
	}
	if err := prov.Start(r.Context(), o); err != nil {
		s.log.Warn("payment start failed", "order", o.ID, "method", method, "err", err)
		_ = s.svc.FailOrder(r.Context(), o.ID, err.Error())
		fail("Could not start the payment: " + err.Error())
		return
	}
	http.Redirect(w, r, "/portal/wait?order="+o.ID, http.StatusSeeOther)
}

func (s *Server) portalVoucher(w http.ResponseWriter, r *http.Request) {
	a, pc, ok := s.need(w, r)
	if !ok {
		return
	}
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		s.render(w, "portal", s.view(r, msg, "", ""))
	}
	if !s.rl.allow("v:" + pc.MAC) {
		fail(http.StatusTooManyRequests, "Too many attempts. Wait a minute and try again.")
		return
	}
	v, err := s.svc.LookupVoucher(r.FormValue("voucher"))
	if err != nil {
		fail(http.StatusBadRequest, "That voucher is invalid or already used.")
		return
	}
	o, err := s.svc.CreateOrder(billing.OrderInput{
		AccountID: a.ID, PackageID: v.PackageID, Method: "voucher", VoucherCode: v.Code, MAC: pc.MAC, IP: pc.IP, Dst: pc.Dst,
	})
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if err := s.providers["voucher"].Start(r.Context(), o); err != nil {
		fail(http.StatusBadRequest, "That voucher is invalid or already used.")
		return
	}
	http.Redirect(w, r, "/portal/success?order="+o.ID, http.StatusSeeOther)
}

// portalConnect retries the automatic login ("Connect now" button).
func (s *Server) portalConnect(w http.ResponseWriter, r *http.Request) {
	a, pc, ok := s.need(w, r)
	if !ok {
		return
	}
	if !s.rl.allow("c:" + pc.MAC) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "portal", s.view(r, "Too many attempts. Wait a minute and try again.", "", ""))
		return
	}
	s.svc.RunTasks(r.Context())
	cur, _ := s.svc.GetAccount(a.ID)
	p := s.view(r, "", "", "")
	if s.svc.AutoLogin(r.Context(), cur, pc.IP) {
		p.LoginFailed = false
	} else if billing.AccountState(cur, s.svc.Now()) == billing.StateActive {
		p.LoginFailed = true
	}
	s.render(w, "portal", p)
}

func (s *Server) portalWait(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.FormValue("order"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if o.Status == store.OrderPaid {
		http.Redirect(w, r, "/portal/success?order="+o.ID, http.StatusSeeOther)
		return
	}
	s.render(w, "wait", map[string]any{"Brand": s.cfg.BrandName, "Order": o, "Test": o.Method == "test"})
}

func (s *Server) apiOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"` + o.Status + `","reason":` + jsonStr(o.Reason) + `}`))
}

func (s *Server) portalSuccess(w http.ResponseWriter, r *http.Request) {
	o, ok := s.svc.GetOrder(r.FormValue("order"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if o.Status != store.OrderPaid {
		http.Redirect(w, r, "/portal/wait?order="+o.ID, http.StatusSeeOther)
		return
	}
	a, _ := s.svc.GetAccount(o.AccountID)
	var pkg store.Package
	s.st.View(func(d *store.Data) { pkg = d.Packages[a.PackageID] })
	// The logged-in flag is set shortly after payment; re-check now so the page is right.
	logged := o.LoggedIn
	if !logged && o.IP != "" && strings.EqualFold(o.MAC, a.MAC) {
		logged = s.svc.AutoLogin(r.Context(), a, o.IP)
	}
	s.render(w, "success", map[string]any{
		"Brand": s.cfg.BrandName, "Acct": a, "Pkg": pkg, "LoggedIn": logged, "Dst": o.Dst,
		"Support": s.cfg.SupportPhone, "MAC": o.MAC, "IP": o.IP, "Expires": a.ExpiresAt,
	})
}

func jsonStr(s string) string {
	b, _ := jsonMarshal(s)
	return string(b)
}

var _ = time.Second
