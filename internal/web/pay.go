package web

import (
	"net"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// defaultSupport is shown when neither the building nor the server config has a support phone.
const defaultSupport = "0100133133"

// Payment prompts cost the payer a PIN screen, so they are limited per
// address and per account (the limiters live here: server.go is not ours).
var (
	payIPRL   = newLimiter(8, 10*time.Minute)
	payAcctRL = newLimiter(4, 10*time.Minute)
)

func (s *Server) payRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /pay", s.payHome)
	mux.HandleFunc("POST /pay/find", s.payFind)
	mux.HandleFunc("POST /pay/start", s.payStart)
}

// payPage is everything the public payment page shows. It never carries a
// phone number, tenant name or MAC: only the flat label of the account the
// visitor already named by its code.
type payPage struct {
	Brand, Support, SiteName, Site string
	Error                          string
	Detected                       bool // the building was recognised from the visitor's address
	Code                           string
	Found                          bool
	Flat                           string
	State                          string
	ExpiresAt                      time.Time
	Groups                         []packageGroup
	Blocked                        map[string]bool
	BlockedUntil                   string
	Selected, Phone                string
	Mpesa, Test                    bool
}

// siteByAddr finds the building a visitor is in. A walled flat reaches us
// through its router's WAN address, which is the address the building's
// WireGuard peer connects from. It answers only when exactly one building
// matches (two buildings behind one NAT address cannot be told apart).
func (s *Server) siteByAddr(ip string) (store.Site, bool) {
	if ip == "" {
		return store.Site{}, false
	}
	var cands []store.Site
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if !st.Removed && !st.Disabled && st.WGPublicKey != "" {
				cands = append(cands, st)
			}
		}
	})
	var hit []store.Site
	for _, st := range cands {
		ps, found, _ := s.mgr.PeerStatus(st.ID)
		if !found || ps.Endpoint == "" {
			continue
		}
		host, _, err := net.SplitHostPort(ps.Endpoint)
		if err != nil {
			host = ps.Endpoint
		}
		if host == ip {
			hit = append(hit, st)
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return store.Site{}, false
}

func (s *Server) payBase(r *http.Request) payPage {
	p := payPage{Brand: s.cfg.BrandName, Support: s.cfg.SupportPhone,
		Mpesa: s.providers["mpesa"] != nil && s.providers["mpesa"].Enabled(),
		Test:  s.providers["test"] != nil && s.providers["test"].Enabled()}
	if st, ok := s.siteByAddr(realIP(r)); ok {
		p.Site, p.SiteName, p.Detected = st.ID, st.Name, true
		if st.SupportPhone != "" {
			p.Support = st.SupportPhone
		}
	}
	if p.Support == "" {
		p.Support = defaultSupport
	}
	return p
}

func (s *Server) payHome(w http.ResponseWriter, r *http.Request) {
	s.render(w, "pay", s.payBase(r))
}

// payAccount resolves what the visitor typed to an open account: its account
// code anywhere, or, when the building was recognised, the flat label in it.
func (s *Server) payAccount(site, text string) (store.Account, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 40 {
		return store.Account{}, false
	}
	if a, ok := s.svc.AccountByCode(strings.ToUpper(text)); ok {
		return a, true
	}
	if site == "" {
		return store.Account{}, false
	}
	return s.resolveFlat(site, text)
}

func (s *Server) payFail(w http.ResponseWriter, r *http.Request, code int, p payPage, msg string) {
	p.Error = msg
	w.WriteHeader(code)
	s.render(w, "pay", p)
}

func (s *Server) payFind(w http.ResponseWriter, r *http.Request) {
	p := s.payBase(r)
	if !s.rl.allow("pf:" + realIP(r)) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a minute and try again.")
		return
	}
	text := r.FormValue("code")
	a, ok := s.payAccount(p.Site, text)
	if !ok {
		s.payFail(w, r, http.StatusNotFound, p, "We could not find that account. Check the account number, or call "+p.Support+".")
		return
	}
	s.payChoose(w, r, p, a, http.StatusOK, "", "")
}

// payChoose renders the package step for a found account.
func (s *Server) payChoose(w http.ResponseWriter, r *http.Request, p payPage, a store.Account, status int, errMsg, selected string) {
	p.Site, p.Code, p.Found, p.Flat, p.Error = a.SiteID, a.Code, true, a.Flat, errMsg
	p.ExpiresAt = a.ExpiresAt
	p.State = billing.AccountState(a, s.svc.Now())
	s.st.View(func(d *store.Data) {
		st := d.Sites[a.SiteID]
		p.SiteName = st.Name
		if st.SupportPhone != "" {
			p.Support = st.SupportPhone
		}
		p.Groups = packageGroups(d.EffectivePackages(a.SiteID))
	})
	if a.Suspended {
		p.Found = false
		p.Error = "This account is suspended. Please call " + p.Support + "."
		w.WriteHeader(http.StatusForbidden)
		s.render(w, "pay", p)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	p.Blocked = map[string]bool{}
	for _, g := range p.Groups {
		for _, pk := range g.Packages {
			if until, bl := s.svc.LowerPlanBlocked(a, pk); bl {
				p.Blocked[pk.ID] = true
				p.BlockedUntil = until.In(billing.EAT).Format("Mon 2 Jan 2006")
			}
		}
	}
	p.Selected = selected
	if p.Selected == "" || p.Blocked[p.Selected] {
		p.Selected = ""
		for _, g := range p.Groups {
			for _, pk := range g.Packages {
				if p.Selected == "" && !p.Blocked[pk.ID] {
					p.Selected = pk.ID
				}
			}
		}
	}
	p.Phone = r.FormValue("phone")
	s.render(w, "pay", p)
}

// payStart creates the order and sends the M-Pesa prompt to the phone.
func (s *Server) payStart(w http.ResponseWriter, r *http.Request) {
	p := s.payBase(r)
	ip := realIP(r)
	if !payIPRL.allow(ip) || !payAcctRL.allow(strings.ToUpper(strings.TrimSpace(r.FormValue("code")))) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	a, ok := s.svc.AccountByCode(strings.ToUpper(strings.TrimSpace(r.FormValue("code"))))
	if !ok {
		s.payFail(w, r, http.StatusNotFound, p, "We could not find that account. Call "+p.Support+".")
		return
	}
	method, pkgID := r.FormValue("method"), r.FormValue("package")
	again := func(code int, msg string) {
		s.payChoose(w, r, p, a, code, msg, pkgID)
	}
	prov := s.providers[method]
	if prov == nil || !prov.Enabled() || method == "voucher" {
		again(http.StatusBadRequest, "That payment method is not available.")
		return
	}
	phone, err := billing.NormalizePhone(strings.TrimSpace(r.FormValue("phone")))
	if err != nil {
		again(http.StatusBadRequest, err.Error())
		return
	}
	o, err := s.svc.CreateOrder(billing.OrderInput{AccountID: a.ID, PackageID: pkgID, Phone: phone, Method: method})
	if err != nil {
		again(http.StatusBadRequest, err.Error())
		return
	}
	if err := prov.Start(r.Context(), o); err != nil {
		s.log.Warn("payment start failed", "order", o.ID, "method", method, "err", err)
		_ = s.svc.FailOrder(r.Context(), o.ID, err.Error())
		again(http.StatusBadGateway, "Could not start the payment: "+err.Error())
		return
	}
	http.Redirect(w, r, "/portal/wait?order="+o.ID, http.StatusSeeOther)
}
