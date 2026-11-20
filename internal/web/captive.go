package web

import (
	"errors"
	"html/template"
	"net/http"
	"strings"
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
// nothing of the device: not its 10.x address, not its MAC. realIP(r) therefore identifies the BUILDING (siteByAddr,
// the address of the router's WireGuard peer; it answers only when exactly one building has it) and never the
// device. Making the device visible would take a router change (the walled HTTP through the tunnel without
// masquerade, and the customer subnet in the peer's AllowedIPs, impossible when two buildings reuse a subnet),
// or one conntrack read per page view: both break the rules this server works under, so neither is done.
//
// So a registration is bound to the building, never to a MAC taken from the request: no form field, header or query
// can name a MAC. The owner attaches the device when approving, from the router's list of unregistered devices,
// helped by a hint (the one online unregistered device the router had when the form arrived, if exactly one).

const (
	regPath     = "/pay/register" // under /pay: the double-submit cookie of the portal is scoped to that path
	regDonePath = "/pay/register/done"
)

var (
	// Every flat of a building reaches us from the router's one address: the per-address limit is generous when the
	// building is recognised, tight for a stranger on the internet. The per-phone and per-MAC limits do the real work.
	regIPRL      = newLimiter(40, 10*time.Minute)
	regIPRLOpen  = newLimiter(4, 10*time.Minute)
	regPhoneRL   = newLimiter(3, time.Hour)
	regMACRL     = newLimiter(3, time.Hour)
	regGetRL     = newLimiter(240, time.Minute)
	regGetRLOpen = newLimiter(20, time.Minute)
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
	CSRF, Error              string
	Apartments               []payChoice
	// what the visitor typed (never a MAC or an address)
	Name, Phone, Unit, Email, ApartmentID string
	Consent                               bool
}

// captiveIdentify says which router the visitor is in.
func (s *Server) captiveIdentify(r *http.Request) (store.Site, bool) { return s.siteByAddr(realIP(r)) }

// captiveMACOf is the one place a device's MAC could enter a registration: it answers from what the SERVER knows
// (never from a form field, header or query), and answers "" today because the server cannot see the device (see
// above). If the walled HTTP ever reaches the server with the device's own address from a known router, this is
// where that address is looked up in the router's lease and ARP data the sweep already holds (no router read per
// page view).
var captiveMACOf = func(s *Server, r *http.Request, site store.Site) string { return "" }

func (s *Server) regBase(r *http.Request) regPage {
	p := regPage{Brand: s.cfg.BrandName}
	site, ok := s.captiveIdentify(r)
	if ok {
		p.Detected, p.SiteName = true, site.Name
		s.st.View(func(d *store.Data) {
			for _, id := range d.SiteApartmentIDs(site.ID) {
				p.Apartments = append(p.Apartments, payChoice{ID: id, Name: d.Apartments[id].Name})
			}
		})
		p.Contacts = s.contacts(site.ID)
	} else {
		p.Contacts = s.contacts("")
	}
	p.Support = p.Contacts.Words()
	return p
}

func (s *Server) registerForm(w http.ResponseWriter, r *http.Request) {
	p := s.regBase(r)
	lim := regGetRLOpen
	if p.Detected {
		lim = regGetRL
	}
	if !lim.allow(limitKey(realIP(r))) {
		http.Error(w, "Too many requests. Wait a minute and try again.", http.StatusTooManyRequests)
		return
	}
	p.CSRF = s.payCSRF(w, r)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "register", p)
}

func (s *Server) regFail(w http.ResponseWriter, r *http.Request, code int, p regPage, msg string) {
	p.Error, p.CSRF = msg, s.payCSRF(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	s.render(w, "register", p)
}

func (s *Server) registerSubmit(w http.ResponseWriter, r *http.Request) {
	p := s.regBase(r)
	f := func(k string, max int) string {
		v := strings.TrimSpace(r.PostFormValue(k))
		if r := []rune(v); len(r) > max {
			v = string(r[:max]) // never kept longer than the field allows
		}
		return v
	}
	p.Name, p.Phone, p.Unit, p.Email, p.ApartmentID = f("name", 200), f("phone", 40), f("unit", 100), f("email", 200), f("apartment_id", 40)
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
	if r.PostFormValue("website") != "" { // the hidden field only a script fills in: say thanks, keep nothing
		http.Redirect(w, r, regDonePath, http.StatusSeeOther)
		return
	}
	site, ok := s.captiveIdentify(r)
	if !ok {
		s.regFail(w, r, http.StatusConflict, p, "We could not tell which network you are on, so we cannot take your details here. Please contact support.")
		return
	}
	in := billing.RegistrationInput{SiteID: site.ID, ApartmentID: p.ApartmentID, Unit: p.Unit, Name: p.Name, Phone: p.Phone, Email: p.Email, Consent: p.Consent,
		MAC: captiveMACOf(s, r, site)}
	// A phone number is limited whatever the address: one number cannot be used to flood the owner's list.
	if ph, err := billing.NormalizePhone(p.Phone); err == nil && !regPhoneRL.allow(ph) {
		s.regFail(w, r, http.StatusTooManyRequests, p, "Too many attempts with this number. Please try again later.")
		return
	}
	if in.MAC != "" && !regMACRL.allow(in.MAC) {
		s.regFail(w, r, http.StatusTooManyRequests, p, "Too many attempts from this device. Please try again later.")
		return
	}
	if _, err := s.svc.SubmitRegistration(in); err != nil {
		var re *billing.RegError
		if errors.As(err, &re) {
			s.regFail(w, r, http.StatusUnprocessableEntity, p, re.Msg)
			return
		}
		s.log.Error("registration failed", "site", site.ID, "err", err) // no name, phone or address in the log
		s.regFail(w, r, http.StatusInternalServerError, p, "Something went wrong. Please try again, or contact support.")
		return
	}
	s.log.Info("registration received", "site", site.ID) // counts only: no personal data in logs
	http.Redirect(w, r, regDonePath, http.StatusSeeOther)
}

func (s *Server) registerDone(w http.ResponseWriter, r *http.Request) {
	p := s.regBase(r)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "register_done", p)
}
