package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// defaultSupport is shown when neither the building nor the server config has a support phone.
const defaultSupport = "0100133133"

// Payment prompts cost the payer a PIN screen, so they are limited per
// address and per account (the limiters live here: server.go is not ours).
var (
	// Every flat of a building reaches us from the router's one address, so the
	// per-address limits are generous; the per-account and per-phone limits do the work.
	// An address that is not a recognised building is a stranger on the internet: much tighter.
	payIPRL       = newLimiter(60, 10*time.Minute)
	payFindRL     = newLimiter(120, time.Minute)
	payIPRLOpen   = newLimiter(8, 10*time.Minute)
	payFindRLOpen = newLimiter(15, time.Minute)
	payAcctRL     = newLimiter(4, 10*time.Minute) // keyed by address and account, so one tenant cannot lock the building out
)

func payFindLimiter(p payPage) *limiter {
	if p.Detected {
		return payFindRL
	}
	return payFindRLOpen
}

func payStartLimiter(p payPage) *limiter {
	if p.Detected {
		return payIPRL
	}
	return payIPRLOpen
}

// limitKey keys a limiter by client: an IPv6 address by its /64 (a household
// has a whole /64, and rotating inside it must not dodge the limit).
func limitKey(ip string) string {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip
}

var (
	paySecretOnce sync.Once
	paySecret     []byte
)

// payToken is a short-lived signed reference to an account, so the page after a
// flat-label lookup does not have to carry (or show) the account code.
func payToken(accountID string, now time.Time) string {
	paySecretOnce.Do(func() { paySecret = make([]byte, 32); rand.Read(paySecret) })
	body := base64.RawURLEncoding.EncodeToString([]byte(accountID + "|" + now.Add(10*time.Minute).UTC().Format(time.RFC3339)))
	m := hmac.New(sha256.New, paySecret)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func payTokenAccount(tok string, now time.Time) (string, bool) {
	paySecretOnce.Do(func() { paySecret = make([]byte, 32); rand.Read(paySecret) })
	body, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return "", false
	}
	m := hmac.New(sha256.New, paySecret)
	m.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", false
	}
	id, exp, ok := strings.Cut(string(raw), "|")
	t, err := time.Parse(time.RFC3339, exp)
	if !ok || err != nil || now.After(t) {
		return "", false
	}
	return id, true
}

const payCSRFCookie = "ftn_pay_csrf"

// payCSRF returns this visitor's token for the page's forms, setting the cookie
// on first use. The public forms are protected by the usual double submit.
func (s *Server) payCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(payCSRFCookie); err == nil && len(c.Value) >= 16 {
		return c.Value
	}
	b := make([]byte, 18)
	rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: payCSRFCookie, Value: v, Path: "/pay", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	return v
}

func payCSRFOK(r *http.Request) bool {
	c, err := r.Cookie(payCSRFCookie)
	f := r.FormValue("_pcsrf")
	return err == nil && len(c.Value) >= 16 && subtle.ConstantTimeCompare([]byte(c.Value), []byte(f)) == 1
}

func (s *Server) payRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /pay", s.payHome)
	mux.HandleFunc("POST /pay/find", s.payFind)
	mux.HandleFunc("POST /pay/start", s.payStart)
	mux.HandleFunc("POST /pay/voucher", s.payVoucher)
}

// payPage is everything the public payment page shows. It never carries a
// phone number, tenant name or MAC: only the flat label of the account the
// visitor already named by its code.
type payPage struct {
	Brand, Support, SiteName, Site string
	Error                          string
	Detected                       bool   // the building was recognised from the visitor's address
	Code                           string // shown only when the visitor typed the code
	Tok, CSRF                      string
	ByCode                         bool
	Found                          bool
	Flat                           string
	State                          string
	ExpiresAt                      time.Time
	Groups                         []packageGroup
	Blocked                        map[string]bool
	BlockedUntil                   string
	Selected, Phone                string
	Mpesa, Test                    bool
	// Choices: set when a typed flat number is used in more than one apartment of the router.
	// The visitor picks theirs (ApartmentID) and continues.
	Choices []payChoice
	Typed   string
}

type payChoice struct{ ID, Name string }

// payWrongRouter says, in words, that the visitor's device is on another router than the account's.
// "" = fine, or the visitor's router is not known. No payment is taken until the visitor is where the account is.
func (s *Server) payWrongRouter(p payPage, a store.Account) string {
	if !p.Detected || p.Site == "" || p.Site == a.SiteID {
		return ""
	}
	var here, there string
	s.st.View(func(d *store.Data) {
		here = d.Sites[p.Site].Name
		var names []string
		for _, id := range d.SiteApartmentIDs(p.Site) {
			names = append(names, d.Apartments[id].Name)
		}
		if len(names) > 0 {
			here += " (" + strings.Join(names, ", ") + ")"
		}
		there = d.Apartments[a.ApartmentID].Name
		if there == "" {
			there = d.Sites[a.SiteID].Name
		}
	})
	return "Your device is connected at " + here + ", not at " + there + ". Call " + p.Support + "."
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
	p := s.payBase(r)
	p.CSRF = s.payCSRF(w, r)
	s.render(w, "pay", p)
}

// payAccount resolves what the visitor typed to an open account: its account
// code anywhere, or, when the building was recognised, the flat label in it.
func (s *Server) payAccount(site, text, apartmentID string) (a store.Account, byCode, ok bool, ambiguous []store.Account) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 40 {
		return store.Account{}, false, false, nil
	}
	if a, ok := s.svc.AccountByCode(strings.ToUpper(text)); ok {
		return a, true, true, nil
	}
	if site == "" {
		return store.Account{}, false, false, nil
	}
	a, ok, ambiguous = s.resolveFlat(site, text, apartmentID)
	return a, false, ok, ambiguous
}

func (s *Server) payFail(w http.ResponseWriter, r *http.Request, code int, p payPage, msg string) {
	p.Error = msg
	p.CSRF = s.payCSRF(w, r)
	w.WriteHeader(code)
	s.render(w, "pay", p)
}

const payExpired = "This page expired. Please try again."

func (s *Server) payFind(w http.ResponseWriter, r *http.Request) {
	p := s.payBase(r)
	if !payCSRFOK(r) {
		s.payFail(w, r, http.StatusForbidden, p, payExpired)
		return
	}
	if !payFindLimiter(p).allow(limitKey(realIP(r))) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a minute and try again.")
		return
	}
	a, byCode, ok, amb := s.payAccount(p.Site, r.FormValue("code"), r.FormValue("apartment_id"))
	if len(amb) > 0 {
		// The same label is used in more than one apartment of this router: ask which one, never guess.
		p.Typed = strings.TrimSpace(r.FormValue("code"))
		for _, x := range amb {
			p.Choices = append(p.Choices, payChoice{ID: x.ApartmentID, Name: s.apartmentName(x.ApartmentID)})
		}
		s.payFail(w, r, http.StatusConflict, p, "More than one flat on this network has that number. Choose your apartment, or enter your account number.")
		return
	}
	if !ok {
		s.payFail(w, r, http.StatusNotFound, p, "We could not find that account. Check the account number, or call "+p.Support+".")
		return
	}
	s.payChoose(w, r, p, a, byCode, http.StatusOK, "", "")
}

// payChoose renders the package step for a found account. A visitor who
// typed the account code sees it back with the state; one who typed a flat
// label sees only the label, and the page carries a signed token instead of
// the code, so a flat label cannot be turned into an account code.
func (s *Server) payChoose(w http.ResponseWriter, r *http.Request, p payPage, a store.Account, byCode bool, status int, errMsg, selected string) {
	if msg := s.payWrongRouter(p, a); msg != "" {
		p.Error, p.Found = msg, false
		p.CSRF = s.payCSRF(w, r)
		w.WriteHeader(http.StatusConflict)
		s.render(w, "pay", p)
		return
	}
	p.Site, p.Found, p.Flat, p.Error, p.ByCode = a.SiteID, true, a.Flat, errMsg, byCode
	p.Tok = payToken(a.ID, s.svc.Now())
	p.CSRF = s.payCSRF(w, r)
	if byCode {
		p.Code, p.ExpiresAt, p.State = a.Code, a.ExpiresAt, billing.AccountState(a, s.svc.Now())
	}
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
	if p.Phone == "" && byCode {
		// Prefilled (and editable) only for a visitor who typed the account code: a flat label is easy to guess, and
		// must not show a stranger a neighbour's number.
		p.Phone = localPhone(a.Phone)
	}
	s.render(w, "pay", p)
}

// payFormAccount finds the open account a pay form is about: the signed token
// of the package step, or the account code. Only open accounts can pay: a
// closed or deleted one is refused.
func (s *Server) payFormAccount(r *http.Request) (a store.Account, byCode, ok bool) {
	if id, tok := payTokenAccount(r.FormValue("tok"), s.svc.Now()); tok {
		a, ok = s.svc.GetAccount(id)
	} else if c := strings.ToUpper(strings.TrimSpace(r.FormValue("code"))); c != "" && len(c) <= 40 {
		a, ok = s.svc.AccountByCode(c)
		byCode = true
	}
	if ok {
		if open, found := s.svc.AccountByCode(a.Code); !found || open.ID != a.ID {
			ok = false
		}
	}
	return a, byCode, ok
}

// payStart creates the order and sends the M-Pesa prompt to the phone.
func (s *Server) payStart(w http.ResponseWriter, r *http.Request) {
	p := s.payBase(r)
	if !payCSRFOK(r) {
		s.payFail(w, r, http.StatusForbidden, p, payExpired)
		return
	}
	ip := limitKey(realIP(r))
	if !payStartLimiter(p).allow(ip) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	a, byCode, ok := s.payFormAccount(r)
	if !ok {
		s.payFail(w, r, http.StatusNotFound, p, "We could not find that account. Please start again, or call "+p.Support+".")
		return
	}
	if s.payWrongRouter(p, a) != "" { // before any order or prompt
		s.payChoose(w, r, p, a, byCode, http.StatusConflict, "", "")
		return
	}
	// The account exists now, so it is safe to start counting against it. The key
	// includes the address: one tenant's retries do not lock the building out.
	if !payAcctRL.allow(ip + "|" + a.ID) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	method, pkgID := r.FormValue("method"), r.FormValue("package")
	again := func(code int, msg string) {
		s.payChoose(w, r, p, a, byCode, code, msg, pkgID)
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
	if !s.stkRL.allow("pay-phone:" + phone) { // a phone gets a few prompts, however many accounts or addresses ask
		again(http.StatusTooManyRequests, "Too many payment requests for this phone. Wait a few minutes and try again.")
		return
	}
	o, err := s.svc.CreateOrder(billing.OrderInput{AccountID: a.ID, PackageID: pkgID, Phone: phone, Method: method})
	if err != nil {
		again(http.StatusBadRequest, err.Error())
		return
	}
	if err := prov.Start(r.Context(), o); err != nil {
		s.log.Warn("payment start failed", "order", o.ID, "method", method, "err", err)
		_ = s.svc.FailOrder(r.Context(), o.ID, "the payment prompt could not be sent")
		again(http.StatusBadGateway, "We could not send the payment prompt. Please try again in a minute, or call "+p.Support+".")
		return
	}
	http.Redirect(w, r, "/portal/wait?order="+o.ID, http.StatusSeeOther)
}

// payVoucher redeems a prepaid voucher for the account the visitor named. A
// voucher made for one building is refused at every other building, and the
// answer for a wrong, used or foreign voucher is the same.
func (s *Server) payVoucher(w http.ResponseWriter, r *http.Request) {
	p := s.payBase(r)
	if !payCSRFOK(r) {
		s.payFail(w, r, http.StatusForbidden, p, payExpired)
		return
	}
	ip := limitKey(realIP(r))
	if !payStartLimiter(p).allow(ip) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	a, byCode, ok := s.payFormAccount(r)
	if !ok {
		s.payFail(w, r, http.StatusNotFound, p, "We could not find that account. Please start again, or call "+p.Support+".")
		return
	}
	if s.payWrongRouter(p, a) != "" { // before any order or prompt
		s.payChoose(w, r, p, a, byCode, http.StatusConflict, "", "")
		return
	}
	if !payAcctRL.allow("voucher|" + ip + "|" + a.ID) {
		s.payFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	again := func(code int, msg string) { s.payChoose(w, r, p, a, byCode, code, msg, "") }
	v, err := s.svc.LookupVoucher(r.FormValue("voucher"))
	if err != nil || (v.SiteID != "" && v.SiteID != a.SiteID) {
		again(http.StatusBadRequest, "That voucher is invalid or already used.")
		return
	}
	o, err := s.svc.CreateOrder(billing.OrderInput{AccountID: a.ID, PackageID: v.PackageID, Method: "voucher", VoucherCode: v.Code})
	if err != nil {
		again(http.StatusBadRequest, err.Error())
		return
	}
	if err := s.providers["voucher"].Start(r.Context(), o); err != nil {
		again(http.StatusBadRequest, "That voucher is invalid or already used.")
		return
	}
	http.Redirect(w, r, "/portal/success?order="+o.ID, http.StatusSeeOther)
}

// localPhone shows a stored 2547XXXXXXXX number the way people type it (07XX XXX XXX, no spaces).
func localPhone(p string) string {
	if len(p) == 12 && strings.HasPrefix(p, "254") {
		return "0" + p[3:]
	}
	return p
}
