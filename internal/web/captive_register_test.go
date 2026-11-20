package web_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/web"
)

// regForm is a valid registration of the captive harness's building.
func (c *captive) regForm(t *testing.T, name, phone, unit string) url.Values {
	t.Helper()
	apt := ""
	c.st.View(func(d *store.Data) {
		for _, id := range d.SiteApartmentIDs(c.site.ID) {
			apt = id
		}
	})
	return url.Values{"name": {name}, "phone": {phone}, "apartment_id": {apt}, "unit": {unit}, "email": {""}, "consent": {"1"}}
}

func (c *captive) register(t *testing.T, form url.Values) (int, string, *http.Response) {
	t.Helper()
	return c.pay(t, "/pay/register", form)
}

func (c *captive) regs() (out []store.Registration) {
	c.st.View(func(d *store.Data) {
		for _, r := range d.Registrations {
			out = append(out, r)
		}
	})
	return
}

func (c *captive) unknownDevice(mac, ip string) {
	c.rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": mac, "address": ip, "active-address": ip, "host-name": "phone",
		"status": "bound", "dynamic": "true", "server": "ftn-dhcp"})
}

// The form lists the apartments of the visitor's own router and nobody else's, with the privacy notice and the contacts.
func TestRegisterPageShowsThisBuildingsApartmentsAndTheNotice(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptiveCfg(t, true, func(cfg *config.Config) { cfg.SupportEmail, cfg.SupportWhatsApp = "support@ftw.co.ke", "254100133133" })
	other, err := c.mgr.CreateSite(sites.SiteInput{Name: "Hidden Heights"})
	if err != nil {
		t.Fatal(err)
	}
	apartmentFor(t, c.st, other.ID, "Hidden Heights Block")
	html := body(t, c.visit("GET", "/pay/register", "isp.ftw.co.ke", nil))
	for _, want := range []string{"Register for internet", "Riverside Court", `name="name"`, `name="phone"`, `name="unit"`, `name="apartment_id"`, `name="email"`,
		`name="consent"`, "Kenya Data Protection Act", "How we use your details", `name="website"`, `name="_pcsrf"`} {
		if !strings.Contains(html, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	if strings.Contains(html, "Hidden Heights") {
		t.Error("an apartment of another router is offered")
	}
	if regexp.MustCompile(`<script[^>]*>`).MatchString(regexp.MustCompile(`<script[^>]* src=[^>]*>`).ReplaceAllString(html, "")) {
		t.Error("the page carries inline script")
	}
	// a stranger gets no form (the building is not known) but still the contacts
	req, _ := http.NewRequest("GET", c.ts.URL+"/pay/register", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.50")
	r, _ := c.cl.Do(req)
	h := body(t, r)
	if strings.Contains(h, `name="apartment_id"`) || strings.Contains(h, "Riverside Court") || !strings.Contains(h, "could not tell which network") || !strings.Contains(h, "wa.me/254100133133") {
		t.Errorf("stranger page: %s", h)
	}
}

func TestRegisterCreatesAPendingRegistrationBoundToTheBuildingNeverToAPostedMAC(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptive(t, true)
	c.unknownDevice("00:E0:4C:68:01:02", "10.5.50.20")
	c.reconcile(t)
	form := c.regForm(t, "  Njeri   Mwangi ", "0722 111 222", "b7")
	form.Set("mac", "AA:BB:CC:00:00:99") // nothing the browser says can name the device
	form.Set("site", "someone-elses")
	form.Set("site_id", "someone-elses")
	form.Set("email", "njeri@example.com")
	code, html, resp := c.register(t, form)
	if code != http.StatusSeeOther || resp.Header.Get("Location") != "/pay/register/done" {
		t.Fatalf("%d %s %s", code, resp.Header.Get("Location"), html)
	}
	regs := c.regs()
	if len(regs) != 1 {
		t.Fatalf("registrations: %+v", regs)
	}
	r := regs[0]
	if r.Status != store.RegPending || r.SiteID != c.site.ID || r.MAC != "" || r.Name != "Njeri Mwangi" || r.Phone != "254722111222" || r.Unit != "b7" || r.Email != "njeri@example.com" || r.ConsentAt.IsZero() {
		t.Fatalf("%+v", r)
	}
	if r.GuessMAC != "00:E0:4C:68:01:02" {
		t.Fatalf("the one online unknown device should be the owner's hint: %q", r.GuessMAC)
	}
	done := body(t, c.visit("GET", "/pay/register/done", "isp.ftw.co.ke", nil))
	if !strings.Contains(done, "We received your registration") || !strings.Contains(done, "offline until then") {
		t.Errorf("done page: %s", done)
	}
	// the device is still walled: no account, still listed as new
	if got := c.svc.Unregistered(c.site.ID, false); len(got) != 1 || got[0].MAC != "00:E0:4C:68:01:02" {
		t.Fatalf("unregistered: %+v", got)
	}
}

func TestRegisterValidationShowsTheMessageAndKeepsTheTypedValues(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptive(t, true)
	cases := []struct {
		mod  func(url.Values)
		want string
	}{
		{func(v url.Values) { v.Del("consent") }, "privacy notice"},
		{func(v url.Values) { v.Set("phone", "12345") }, "Kenyan phone"},
		{func(v url.Values) { v.Set("name", "A") }, "full name"},
		{func(v url.Values) { v.Set("unit", "") }, "house number"},
		{func(v url.Values) { v.Set("email", "nope") }, "email address"},
		{func(v url.Values) { v.Set("apartment_id", "other") }, "Choose your apartment"},
		{func(v url.Values) { v.Set("name", strings.Repeat("x", 500)) }, "full name"},
	}
	for i, tc := range cases {
		f := c.regForm(t, "Valid Name", "0733"+strings.Repeat(string(rune('1'+i)), 6), "V"+string(rune('1'+i)))
		tc.mod(f)
		code, html, _ := c.register(t, f)
		if code != http.StatusUnprocessableEntity || !strings.Contains(html, tc.want) {
			t.Errorf("case %d: %d, want %q in %s", i, code, tc.want, html)
		}
	}
	if n := len(c.regs()); n != 0 {
		t.Fatalf("refused forms were kept: %d", n)
	}
	// the typed values come back escaped
	f := c.regForm(t, `<script>alert(1)</script>`, "0744555666", "Q1")
	f.Del("consent")
	_, html, _ := c.register(t, f)
	if strings.Contains(html, "<script>alert(1)") || !strings.Contains(html, "&lt;script&gt;alert(1)") {
		t.Errorf("not escaped: %s", html)
	}
	// a taken house number
	code, html, _ := c.register(t, c.regForm(t, "Taken Flat", "0755666777", "Flat 4B"))
	if code != http.StatusUnprocessableEntity || !strings.Contains(html, "already has an account") {
		t.Errorf("taken: %d %s", code, html)
	}
}

func TestRegisterNeedsTheCSRFTokenAndIgnoresTheHoneypotFiller(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptive(t, true)
	// no token, wrong token, no cookie
	f := c.regForm(t, "No Token", "0722333444", "T1")
	r := c.visit("POST", "/pay/register", "isp.ftw.co.ke", f) // cookie added by visit, but no _pcsrf field
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("without the token: %d", r.StatusCode)
	}
	f.Set("_pcsrf", "wrong-wrong-wrong-wrong")
	r = c.visit("POST", "/pay/register", "isp.ftw.co.ke", f)
	r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("wrong token: %d", r.StatusCode)
	}
	// another origin
	f.Set("_pcsrf", payCSRFValue)
	req, _ := http.NewRequest("POST", c.ts.URL+"/pay/register", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", capWANIP)
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: "ftn_pay_csrf", Value: payCSRFValue})
	r2, _ := c.cl.Do(req)
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: %d", r2.StatusCode)
	}
	// the honeypot: looks like success, keeps nothing
	h := c.regForm(t, "Bot Botson", "0722999000", "BOT1")
	h.Set("website", "http://spam.example")
	code, _, resp := c.register(t, h)
	if code != http.StatusSeeOther || resp.Header.Get("Location") != "/pay/register/done" {
		t.Errorf("honeypot: %d %s", code, resp.Header.Get("Location"))
	}
	if n := len(c.regs()); n != 0 {
		t.Fatalf("registrations kept: %d", n)
	}
}

func TestRegisterRateLimits(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptive(t, true)
	// per phone: three tries an hour
	for i := 0; i < 4; i++ {
		f := c.regForm(t, "Same Phone", "0711000999", "P"+string(rune('1'+i)))
		f.Del("consent")
		code, _, _ := c.register(t, f)
		want := http.StatusUnprocessableEntity
		if i == 3 {
			want = http.StatusTooManyRequests
		}
		if code != want {
			t.Errorf("phone try %d: %d, want %d", i, code, want)
		}
	}
	// per address, a stranger on the internet: four tries
	form := func(i int) url.Values {
		f := c.regForm(t, "Stranger", "072200"+strings.Repeat(string(rune('0'+i)), 4), "S1")
		f.Set("_pcsrf", payCSRFValue)
		return f
	}
	post := func(i int) int {
		req, _ := http.NewRequest("POST", c.ts.URL+"/pay/register", strings.NewReader(form(i).Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "192.0.2.77")
		req.AddCookie(&http.Cookie{Name: "ftn_pay_csrf", Value: payCSRFValue})
		r, _ := c.cl.Do(req)
		r.Body.Close()
		return r.StatusCode
	}
	for i := 1; i <= 4; i++ {
		if code := post(i); code == http.StatusTooManyRequests {
			t.Fatalf("stranger try %d limited too early", i)
		}
	}
	if code := post(5); code != http.StatusTooManyRequests {
		t.Errorf("stranger 5th try: %d", code)
	}
	// per address, a recognised building: 40 in ten minutes (the building's flats share one address)
	web.ResetRegLimits()
	for i := 0; i < 40; i++ {
		f := c.regForm(t, "Many", "07330"+strings.Repeat(string(rune('0'+i%10)), 1)+string(rune('0'+i/10))+"000", "M"+string(rune('A'+i%26)))
		f.Del("consent")
		if code, _, _ := c.register(t, f); code == http.StatusTooManyRequests {
			t.Fatalf("building limited at try %d", i)
		}
	}
	f := c.regForm(t, "Many", "0733999111", "MZ")
	f.Del("consent")
	if code, _, _ := c.register(t, f); code != http.StatusTooManyRequests {
		t.Errorf("41st try from the building's address: %d", code)
	}
	if n := len(c.regs()); n != 0 {
		t.Fatalf("limited or refused forms were kept: %d", n)
	}
}

// The seam where an identified device would enter: its MAC is kept on the registration and limited per MAC.
func TestRegisterWithAnIdentifiedDeviceBindsItsMACAndLimitsPerMAC(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptive(t, true)
	defer web.SetCaptiveMAC("00:E0:4C:68:01:02")()
	code, _, _ := c.register(t, c.regForm(t, "Device Owner", "0722123456", "D1"))
	if code != http.StatusSeeOther {
		t.Fatalf("%d", code)
	}
	if rs := c.regs(); len(rs) != 1 || rs[0].MAC != "00:E0:4C:68:01:02" || rs[0].GuessMAC != "" {
		t.Fatalf("%+v", rs)
	}
	for i := 0; i < 2; i++ { // two more tries are allowed; the fourth is limited
		f := c.regForm(t, "Device Owner", "0722123456", "D1")
		f.Del("consent")
		c.register(t, f)
	}
	f := c.regForm(t, "Device Owner", "0722654321", "D2")
	f.Del("consent")
	if code, _, _ := c.register(t, f); code != http.StatusTooManyRequests {
		t.Errorf("per-MAC limit: %d", code)
	}
}

// pending -> approve from the admin page: the account, the customer and the device off the unknown list; reject erases.
func TestRegistrationsAreDecidedInNewDevicesAndCounted(t *testing.T) {
	web.ResetRegLimits()
	c := newCaptive(t, true)
	c.unknownDevice("00:E0:4C:68:01:02", "10.5.50.20")
	c.reconcile(t)
	c.register(t, c.regForm(t, "Wanjiku Ngugi", "0722000555", "C3"))
	c.register(t, c.regForm(t, "Reject Me", "0722000666", "C4"))
	adm := func(path string, v url.Values) *http.Response { return c.br.Do(c.cl, c.ts.URL, "POST", path, v) }
	page := body(t, c.br.Do(c.cl, c.ts.URL, "GET", "/admin/devices", nil))
	for _, want := range []string{"Waiting for approval", "Wanjiku Ngugi", "254722000555", "house number C3", "00:E0:4C:68:01:02", "Approve", "Reject"} {
		if !strings.Contains(page, want) {
			t.Errorf("New devices lacks %q", want)
		}
	}
	// the overview counts them
	ov := body(t, c.br.Do(c.cl, c.ts.URL, "GET", "/admin", nil))
	if !strings.Contains(ov, "2 people have registered and wait for approval") {
		t.Errorf("overview does not count the registrations")
	}
	var wanjiku, reject store.Registration
	for _, r := range c.regs() {
		if r.Unit == "C3" {
			wanjiku = r
		} else {
			reject = r
		}
	}
	// approving without a device is refused and changes nothing
	resp := adm("/admin/registrations/approve", url.Values{"id": {wanjiku.ID}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "Error") {
		t.Errorf("approve without a MAC: %s", resp.Header.Get("Location"))
	}
	resp = adm("/admin/registrations/approve", url.Values{"id": {wanjiku.ID}, "mac": {"00:E0:4C:68:01:02"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(resp.Header.Get("Location"), "Error") {
		t.Fatalf("approve: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var acct store.Account
	c.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.Unit == "C3" {
				acct = a
			}
		}
	})
	if acct.ID == "" || acct.MAC != "00:E0:4C:68:01:02" || acct.Tenant != "Wanjiku Ngugi" || acct.Phone != "254722000555" || acct.CustomerID == "" {
		t.Fatalf("account: %+v", acct)
	}
	// the wall lifts on the next pass: a static lease for the MAC at its own address, and it is no longer "new"
	c.reconcile(t)
	if got := c.svc.Unregistered(c.site.ID, false); len(got) != 0 {
		t.Fatalf("still listed as new: %+v", got)
	}
	static := false
	for _, r := range c.rt.Table("/ip/dhcp-server/lease") {
		if strings.EqualFold(r["mac-address"], acct.MAC) && r["address"] == acct.IP && r["dynamic"] != "true" {
			static = true
		}
	}
	if !static {
		t.Error("no static lease after the next pass")
	}
	// reject
	resp = adm("/admin/registrations/reject", url.Values{"id": {reject.ID}})
	resp.Body.Close()
	got, _ := c.svc.GetRegistration(reject.ID)
	if got.Status != store.RegRejected || got.Name != "" || got.Phone != "" {
		t.Fatalf("rejected: %+v", got)
	}
	if n := c.svc.PendingRegistrationCount(nil); n != 0 {
		t.Fatalf("pending: %d", n)
	}
	// a decided registration cannot be decided again
	resp = adm("/admin/registrations/reject", url.Values{"id": {reject.ID}})
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "Error") {
		t.Errorf("rejecting twice: %s", resp.Header.Get("Location"))
	}
	// a viewer cannot decide: a plain GET of the page is all they have
	if resp := c.visit("POST", "/admin/registrations/approve", "isp.ftw.co.ke", url.Values{"id": {wanjiku.ID}}); resp.StatusCode == http.StatusSeeOther && !strings.Contains(resp.Header.Get("Location"), "login") {
		t.Errorf("anonymous approve: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// The contacts on every captive page: the three items, as links, and each hidden when its setting is empty.
func TestCaptivePagesShowTheSupportContactsAndHideEmptyOnes(t *testing.T) {
	web.ResetRegLimits()
	full := func(cfg *config.Config) {
		cfg.SupportPhone, cfg.SupportEmail, cfg.SupportWhatsApp = "0100133133", "support@ftw.co.ke", "254100133133"
	}
	c := newCaptiveCfg(t, true, full)
	// the building's own phone wins over the server's (the harness gives Riverside Court 0722 000 111)
	pages := func(c *captive) map[string]string {
		out := map[string]string{}
		out["pay"] = body(t, c.visit("GET", "/pay", "isp.ftw.co.ke", nil))
		out["register"] = body(t, c.visit("GET", "/pay/register", "isp.ftw.co.ke", nil))
		out["done"] = body(t, c.visit("GET", "/pay/register/done", "isp.ftw.co.ke", nil))
		_, out["pay-found"], _ = c.pay(t, "/pay/find", url.Values{"code": {c.acct.Code}})
		_, _, resp := c.pay(t, "/pay/start", url.Values{"code": {c.acct.Code}, "package": {c.pkg("1 Day")}, "method": {"mpesa"}, "phone": {"0712345678"}})
		if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/portal/wait") {
			out["wait"] = body(t, c.visit("GET", loc, "isp.ftw.co.ke", nil))
			c.callback(t, "QWE123", priceOf(c, "1 Day"))
			out["success"] = body(t, c.visit("GET", strings.Replace(loc, "wait", "success", 1), "isp.ftw.co.ke", nil))
		}
		return out
	}
	got := pages(c)
	for _, k := range []string{"pay", "register", "done", "pay-found", "wait", "success"} {
		if got[k] == "" {
			t.Fatalf("page %s was not fetched", k)
		}
	}
	for name, h := range got {
		for _, want := range []string{`href="tel:0722000111"`, `href="https://wa.me/254100133133"`, `href="mailto:support@ftw.co.ke"`, "WhatsApp us"} {
			if !strings.Contains(h, want) {
				t.Errorf("%s page lacks %s", name, want)
			}
		}
	}
	// each item hides when its setting is empty
	none := newCaptiveCfg(t, true, func(cfg *config.Config) { cfg.SupportPhone, cfg.SupportEmail, cfg.SupportWhatsApp = "", "", "" })
	none.mgr.Reload(none.site.ID)
	none.st.Update(func(d *store.Data) error {
		s := d.Sites[none.site.ID]
		s.SupportPhone = ""
		d.Sites[none.site.ID] = s
		return nil
	})
	for name, h := range pages(none) {
		if strings.Contains(h, "tel:") || strings.Contains(h, "wa.me") || strings.Contains(h, "mailto:") || strings.Contains(h, "WhatsApp us") || strings.Contains(h, "Need help?") {
			t.Errorf("%s page shows a contact that is not set", name)
		}
	}
	onlyMail := newCaptiveCfg(t, true, func(cfg *config.Config) {
		cfg.SupportPhone, cfg.SupportEmail, cfg.SupportWhatsApp = "", "help@example.com", ""
	})
	onlyMail.st.Update(func(d *store.Data) error {
		s := d.Sites[onlyMail.site.ID]
		s.SupportPhone = ""
		d.Sites[onlyMail.site.ID] = s
		return nil
	})
	h := body(t, onlyMail.visit("GET", "/pay", "isp.ftw.co.ke", nil))
	if !strings.Contains(h, `mailto:help@example.com`) || strings.Contains(h, "tel:") || strings.Contains(h, "wa.me") {
		t.Errorf("only the email should show: %s", h)
	}
}
