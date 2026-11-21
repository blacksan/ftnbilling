package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// ---- support contacts ---------------------------------------------------------------------

// supportContacts are the contacts shown at the foot of every captive page. An empty one is not shown.
type supportContacts struct {
	Phone     string       // as written, for the label
	PhoneHref template.URL // tel:...
	Email     string
	WAHref    template.URL // https://wa.me/<international digits>
}

// Words is how prose names the help line: the number, or "support" when none is set.
func (c supportContacts) Words() string {
	if c.Phone != "" {
		return c.Phone
	}
	return "support"
}

func telHref(p string) template.URL {
	var b strings.Builder
	for i, r := range p {
		if (r >= '0' && r <= '9') || (r == '+' && i == 0) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return template.URL("tel:" + b.String())
}

func waDigits(n string) string {
	var b strings.Builder
	for _, r := range n {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// contacts builds the contacts of a building: its own support phone when it has one, else the server's.
func (s *Server) contacts(siteID string) supportContacts {
	live := s.live()
	c := supportContacts{Phone: strings.TrimSpace(live.SupportPhone), Email: strings.TrimSpace(live.SupportEmail)}
	if siteID != "" {
		s.st.View(func(d *store.Data) {
			if sp := strings.TrimSpace(d.Sites[siteID].SupportPhone); sp != "" {
				c.Phone = sp
			}
		})
	}
	c.PhoneHref = telHref(c.Phone)
	if c.PhoneHref == "" {
		c.Phone = ""
	}
	if !strings.Contains(c.Email, "@") || strings.ContainsAny(c.Email, " \t\r\n<>\"") {
		c.Email = ""
	}
	if d := waDigits(live.SupportWhatsApp); len(d) >= 8 {
		c.WAHref = template.URL("https://wa.me/" + d)
	}
	return c
}

// ---- self-registration --------------------------------------------------------------------
//
// WHO IS THE VISITOR? A walled device's HTTP is dst-nat'ed by its router to this server's public address and
// leaves through the router's WAN with the router's masquerade, so the server sees the router's WAN address and
// nothing of the device: not its 10.x address, not its MAC. realIP(r) therefore identifies the BUILDING (sitesByAddr,
// the address of the router's WireGuard peer) and never the device. Making the device visible would take a router
// change (the walled HTTP through the tunnel without masquerade, and the customer subnet in the peer's AllowedIPs,
// impossible when two buildings reuse a subnet), or one conntrack read per page view: both break the rules this
// server works under, so neither is done.
//
// IS THE "RECOGNISED ROUTER ADDRESS" TRUSTWORTHY? realIP is the TCP peer, or the last X-Forwarded-For entry when
// the peer is the local Caddy (which appends the real peer), so a client cannot choose it. But it is only an
// address: (1) every person who reaches the server from that same public address is "recognised", which on a
// carrier-grade NAT includes strangers on the same ISP address; (2) when two routers sit behind one public
// address, nothing tells their visitors apart. For (2) the form never reveals an apartment name: it asks for
// a free-text "apartment or building name" and the registration is attached to NO router (SiteID empty,
// Registration.Building set) until the owner assigns it. For (1) the per-browser, per-phone and pending-cap limits
// below, and the owner's "Reject all", bound the damage; only an SMS or WhatsApp code would remove it.
//
// So a registration is bound to the building, never to a MAC taken from the request: no form field, header or query
// can name a MAC. The owner attaches the device when approving, from the router's list of unregistered devices,
// helped by the visitor's device name and by when each device was first seen.

const (
	regPath     = "/pay/register" // under /pay: the double-submit cookie of the portal is scoped to that path
	regDonePath = "/pay/register/done"
	// regBrowserCookie is the signed, HttpOnly token that tells one browser from another behind the router's one address.
	regBrowserCookie = "ftn_reg"
	// regHoneypot is a field name no autofill uses.
	regHoneypot = "ftn_hp_url"
)

var (
	// Every flat of a building reaches us from the router's one address, so the per-address limit is generous for a
	// recognised router (one tenant cannot use it up for the building) and tight for a stranger. The per-browser
	// and per-phone limits below do the per-person limiting.
	regIPRL      = newLimiter(200, 10*time.Minute)
	regIPRLOpen  = newLimiter(4, 10*time.Minute)
	regPreRL     = newLimiter(300, 10*time.Minute) // before any lookup: caps the work one address can ask for
	regMACRL     = newLimiter(3, time.Hour)
	regGetRL     = newLimiter(240, time.Minute)
	regGetRLOpen = newLimiter(20, time.Minute)
	// Counted only once the form has validated (see billing.RegistrationInput.Admit).
	regBrowserRL      = newLimiter(5, time.Hour)  // per browser token
	regPhoneBrowserRL = newLimiter(3, time.Hour)  // per phone AND browser token: retries of one person
	regPhoneRL        = newLimiter(10, time.Hour) // per phone, whatever the browser: a number cannot be locked out by one browser
	// Tokens are handed out per address, so a script that drops its cookie every time is still bounded.
	regTokenRL     = newLimiter(100, 10*time.Minute)
	regTokenRLOpen = newLimiter(6, 10*time.Minute)

	regHoneypotHits atomic.Int64
)

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+regPath, s.registerForm)
	mux.HandleFunc("POST "+regPath, s.registerSubmit)
	mux.HandleFunc("GET "+regDonePath, s.registerDone)
}

type regPage struct {
	Brand, SiteName, Support string
	Contacts                 supportContacts
	Detected                 bool
	// Shared: the address matches more than one router. The form then asks for a building name and shows no
	// apartment names.
	Shared      bool
	CSRF, Error string
	Apartments  []payChoice
	// what the visitor typed (never a MAC or an address)
	Name, Phone, Unit, Email, ApartmentID, Building, DeviceName string
	Consent                                                     bool
	site                                                        store.Site // set when exactly one router matched
}

// captiveSites says which routers the visitor may be in.
func (s *Server) captiveSites(r *http.Request) []store.Site { return s.sitesByAddr(realIP(r)) }

// captiveMACOf is the one place a device's MAC could enter a registration: it answers from what the SERVER knows
// (never from a form field, header or query), and answers "" today because the server cannot see the device (see
// above). If the walled HTTP ever reaches the server with the device's own address from a known router, this is
// where that address is looked up in the router's lease and ARP data the sweep already holds (no router read per
// page view).
var captiveMACOf = func(s *Server, r *http.Request, site store.Site) string { return "" }

func (s *Server) regBase(r *http.Request) regPage {
	p := regPage{Brand: s.cfg.BrandName}
	sites := s.captiveSites(r)
	switch len(sites) {
	case 0:
		p.Contacts = s.contacts("")
	case 1:
		site := sites[0]
		p.Detected, p.SiteName, p.site = true, site.Name, site
		s.st.View(func(d *store.Data) {
			for _, id := range d.SiteApartmentIDs(site.ID) {
				p.Apartments = append(p.Apartments, payChoice{ID: id, Name: d.Apartments[id].Name})
			}
		})
		p.Contacts = s.contacts(site.ID)
	default:
		// Two routers behind one address: reveal no apartment name, ask for free text.
		p.Detected, p.Shared = true, true
		p.Contacts = s.contacts("")
	}
	p.Support = p.Contacts.Words()
	return p
}

// regBrowserToken makes the signed value of a browser token: id.signature.
func regBrowserToken(id string) string {
	paySecretOnce.Do(func() { paySecret = make([]byte, 32); rand.Read(paySecret) })
	m := hmac.New(sha256.New, paySecret)
	m.Write([]byte("reg-browser|" + id))
	return id + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// regBrowserID returns the id of the browser token the request carries, when its signature is ours.
func regBrowserID(r *http.Request) (string, bool) {
	c, err := r.Cookie(regBrowserCookie)
	if err != nil || len(c.Value) > 80 {
		return "", false
	}
	id, _, ok := strings.Cut(c.Value, ".")
	if !ok || id == "" || subtle.ConstantTimeCompare([]byte(regBrowserToken(id)), []byte(c.Value)) != 1 {
		return "", false
	}
	return id, true
}

// regIssueBrowser gives the browser a token unless it has a valid one. ok is false when this address has asked
// for too many new tokens.
func (s *Server) regIssueBrowser(w http.ResponseWriter, r *http.Request, detected bool) (id string, ok bool) {
	if id, ok := regBrowserID(r); ok {
		return id, true
	}
	lim := regTokenRLOpen
	if detected {
		lim = regTokenRL
	}
	if !lim.allow(limitKey(realIP(r))) {
		return "", false
	}
	b := make([]byte, 12)
	rand.Read(b)
	id = base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: regBrowserCookie, Value: regBrowserToken(id), Path: "/pay", MaxAge: 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: payHTTPS(r)})
	return id, true
}

func (s *Server) registerForm(w http.ResponseWriter, r *http.Request) {
	// Before any lookup: one address cannot make the server read the tunnel state without limit.
	if !regGetRL.allow(limitKey(realIP(r))) {
		http.Error(w, "Too many requests. Wait a minute and try again.", http.StatusTooManyRequests)
		return
	}
	p := s.regBase(r)
	if !p.Detected && !regGetRLOpen.allow(limitKey(realIP(r))) {
		http.Error(w, "Too many requests. Wait a minute and try again.", http.StatusTooManyRequests)
		return
	}
	if _, ok := s.regIssueBrowser(w, r, p.Detected); !ok {
		http.Error(w, "Too many requests. Wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
	p.CSRF = s.payCSRF(w, r)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "register", p)
}

func (s *Server) regFail(w http.ResponseWriter, r *http.Request, code int, p regPage, msg string) {
	p.Error, p.CSRF = msg, s.payCSRF(w, r)
	s.regIssueBrowser(w, r, p.Detected) // a stale token is replaced so a retry works
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	s.render(w, "register", p)
}

func (s *Server) registerSubmit(w http.ResponseWriter, r *http.Request) {
	if !regPreRL.allow(limitKey(realIP(r))) { // before the router lookup
		http.Error(w, "Too many requests. Wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
	p := s.regBase(r)
	f := func(k string, max int) string {
		v := strings.TrimSpace(r.PostFormValue(k))
		if r := []rune(v); len(r) > max {
			v = string(r[:max]) // never kept longer than the field allows
		}
		return v
	}
	p.Name, p.Phone, p.Unit, p.Email, p.ApartmentID = f("name", 200), f("phone", 40), f("unit", 100), f("email", 200), f("apartment_id", 40)
	p.Building, p.DeviceName = f("apartment_name", 200), f("device_name", 120)
	p.Consent = r.PostFormValue("consent") == "1"
	if !payCSRFOK(r) {
		s.regFail(w, r, http.StatusForbidden, p, payExpired)
		return
	}
	lim := regIPRLOpen
	if p.Detected {
		lim = regIPRL
	}
	if !lim.allow(limitKey(realIP(r))) {
		s.regFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	if r.PostFormValue(regHoneypot) != "" { // the hidden field only a script fills in: say thanks, keep nothing, count it
		n := regHoneypotHits.Add(1)
		s.log.Info("registration honeypot filled", "count", n) // a counter only: nothing about the sender
		http.Redirect(w, r, regDonePath, http.StatusSeeOther)
		return
	}
	if !p.Detected {
		s.regFail(w, r, http.StatusConflict, p, "We could not tell which network you are on, so we cannot take your details here. Please contact support.")
		return
	}
	browser, ok := regBrowserID(r)
	if !ok { // no token (a script, or a page from before a restart): open the form again
		s.regFail(w, r, http.StatusForbidden, p, payExpired)
		return
	}
	in := billing.RegistrationInput{SiteID: p.site.ID, ApartmentID: p.ApartmentID, Unit: p.Unit, Name: p.Name, Phone: p.Phone, Email: p.Email,
		DeviceName: p.DeviceName, Consent: p.Consent, MAC: captiveMACOf(s, r, p.site)}
	if p.Shared {
		in.SiteID, in.ApartmentID, in.Building = "", "", p.Building
	}
	// Counted once the form is valid (so a typing mistake never uses a try): this browser, this phone from this
	// browser, and this phone from anywhere. The first two do the per-person limiting behind the router's one address.
	// A device the server itself identified (no path does today) is limited too.
	in.Admit = func(phone string) bool {
		return regBrowserRL.allow(browser) && regPhoneBrowserRL.allow(phone+"|"+browser) && regPhoneRL.allow(phone) &&
			(in.MAC == "" || regMACRL.allow(in.MAC))
	}
	if _, err := s.svc.SubmitRegistration(in); err != nil {
		var re *billing.RegError
		switch {
		case errors.Is(err, billing.ErrRegRateLimited):
			s.regFail(w, r, http.StatusTooManyRequests, p, "Too many attempts. Please try again later, or contact support.")
		case errors.As(err, &re):
			s.regFail(w, r, http.StatusUnprocessableEntity, p, re.Msg)
		default:
			s.log.Error("registration failed", "site", p.site.ID, "err", err) // no name, phone or address in the log
			s.regFail(w, r, http.StatusInternalServerError, p, "Something went wrong. Please try again, or contact support.")
		}
		return
	}
	s.log.Info("registration received", "site", p.site.ID, "shared_address", p.Shared) // counts only: no personal data in logs
	http.Redirect(w, r, regDonePath, http.StatusSeeOther)
}

func (s *Server) registerDone(w http.ResponseWriter, r *http.Request) {
	p := s.regBase(r)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "register_done", p)
}
