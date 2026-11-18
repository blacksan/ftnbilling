package web_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"ispbill/internal/web"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/routeros"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// ipoeApp is a multi-building app with one IPoE building (already "cut over").
func ipoeApp(t *testing.T, mut func(*config.Config)) (*multi, store.Site, *routeros.Mock) {
	t.Helper()
	m := newMultiOps(t, mut, nil)
	s := m.addSite(t, "Riverside")
	if err := m.mgr.SetAccessMode(s.ID, store.AccessIPoE); err != nil {
		t.Fatal(err)
	}
	rt := m.mock(s.ID)
	rt.SeedRow("/ip/dhcp-server", routeros.Row{"name": "ftn-dhcp", "disabled": "false"})
	m.selectSite(t, s.ID)
	return m, s, rt
}

func lease(rt *routeros.Mock, mac, ip, host string) {
	rt.SeedRow("/ip/dhcp-server/lease", routeros.Row{"mac-address": mac, "address": ip, "active-address": ip, "host-name": host, "status": "bound", "dynamic": "true", "server": "ftn-dhcp"})
}

func TestDevicesPageOnlineFirstAttachCreateDismiss(t *testing.T) {
	m, s, rt := ipoeApp(t, nil)
	f1, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 4A", MAC: "AA:BB:CC:DD:EE:02"}); err != nil {
		t.Fatal(err)
	}
	lease(rt, "00:15:EB:11:22:33", "10.5.50.20", "ont-new")
	lease(rt, "AA:BB:CC:DD:EE:01", "10.5.50.60", "") // registered: never listed
	page := m.adminGet(t, "/admin/devices")
	for _, want := range []string{"00:15:EB:11:22:33", "ont-new", "10.5.50.20", "online", "/admin/devices/attach", "/admin/devices/create", "Flat 3B", "Flat 4A"} {
		if !strings.Contains(page, want) {
			t.Errorf("devices page lacks %q", want)
		}
	}
	if strings.Contains(page, "AA:BB:CC:DD:EE:01</b>") {
		t.Error("a registered MAC is listed")
	}
	// The nav badge and the dashboard count the online unregistered device.
	if dash := m.adminGet(t, "/admin"); !strings.Contains(dash, `<span class="badge">1</span>`) {
		t.Error("badge")
	}
	// Attach by typing the flat name; the device becomes the flat's MAC and the account page opens.
	loc := m.adminPost(t, "/admin/devices/attach", url.Values{"site": {s.ID}, "mac": {"00:15:EB:11:22:33"}, "acct": {"flat 3b"}})
	if !strings.Contains(loc, "/admin/accounts/view?code="+f1.Code) || !strings.Contains(loc, "Device attached") {
		t.Fatal(loc)
	}
	got, _ := m.svc.GetAccount(f1.ID)
	if got.MAC != "00:15:EB:11:22:33" {
		t.Fatal(got.MAC)
	}
	// Attaching a MAC that belongs to another flat is refused and names it.
	loc = m.adminPost(t, "/admin/devices/attach", url.Values{"site": {s.ID}, "mac": {"AA:BB:CC:DD:EE:02"}, "acct": {f1.Code}})
	if !strings.Contains(loc, "Error") || !strings.Contains(loc, "Flat 4A") {
		t.Fatalf("conflict must name the owner: %s", loc)
	}
	// Unknown flat.
	if loc = m.adminPost(t, "/admin/devices/attach", url.Values{"site": {s.ID}, "mac": {"00:15:EB:11:22:44"}, "acct": {"Flat 99"}}); !strings.Contains(loc, "Error") {
		t.Fatal(loc)
	}
	// Create an account from a device, with a plan.
	lease(rt, "00:15:EB:AA:AA:AA", "10.5.50.21", "")
	m.adminGet(t, "/admin/devices")
	loc = m.adminPost(t, "/admin/devices/create", url.Values{"site": {s.ID}, "mac": {"00:15:EB:AA:AA:AA"}, "flat": {"Flat 9"}, "package": {m.pkgID("1 Hour")}})
	if !strings.Contains(loc, "Account created") {
		t.Fatal(loc)
	}
	a9 := m.account(t, "Flat 9")
	if a9.MAC != "00:15:EB:AA:AA:AA" || a9.PackageID != m.pkgID("1 Hour") || a9.IP == "" {
		t.Fatalf("%+v", a9)
	}
	// Dismiss hides a device; it stays hidden on refresh.
	lease(rt, "00:15:EB:BB:BB:BB", "10.5.50.22", "printer")
	if !strings.Contains(m.adminGet(t, "/admin/devices"), "00:15:EB:BB:BB:BB") {
		t.Fatal("not listed")
	}
	m.adminPost(t, "/admin/devices/dismiss", url.Values{"site": {s.ID}, "mac": {"00:15:EB:BB:BB:BB"}})
	if strings.Contains(m.adminGet(t, "/admin/devices"), "00:15:EB:BB:BB:BB") {
		t.Fatal("dismissed device came back")
	}
}

func TestSiteViewIPoEActions(t *testing.T) {
	m, s, rt := ipoeApp(t, nil)
	if _, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"}); err != nil {
		t.Fatal(err)
	}
	page := m.adminGet(t, "/admin/routers/view?id="+s.ID)
	for _, want := range []string{"Access mode", "/admin/sites/adopt", "Cut over command", "Join only command", "Cleanup command", "in sync"} {
		if !strings.Contains(page, want) {
			t.Errorf("site page lacks %q", want)
		}
	}
	// A command is minted for a stage; the join command itself is not changed.
	// A paying flat the router has not seen blocks the command until the admin forces it.
	m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 5", MAC: "AA:BB:CC:DD:EE:05"})
	paid := m.account(t, "Flat 5")
	day := m.pkgID("1 Day")
	m.st.Update(func(d *store.Data) error {
		a := d.Accounts[paid.ID]
		a.ExpiresAt, a.PackageID = m.svc.Now().Add(48*time.Hour), day
		d.Accounts[paid.ID] = a
		return nil
	})
	r := m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"cutover"}})
	if b := body(t, r); r.StatusCode != 200 || !strings.Contains(b, "have not been seen") || !strings.Contains(b, "Flat 5") || strings.Contains(b, "ftn-cutover.rsc") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	r = m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"cutover"}, "force": {"1"}})
	b := body(t, r)
	if !strings.Contains(b, "&amp;force=1") && !strings.Contains(b, "&force=1") {
		t.Fatalf("a forced command carries force=1: %s", b)
	}
	lease(rt, "AA:BB:CC:DD:EE:05", "10.5.50.70", "")
	r = m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"cutover"}})
	b = body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, "ftn-cutover.rsc") || !strings.Contains(b, "?stage=cutover") || !strings.Contains(b, "stage: cutover") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if r = m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"wipe"}}); r.StatusCode != 404 {
		t.Fatalf("unknown stage: %d", r.StatusCode)
	}
	// While the router serves the flats from ftn-dhcp the mode cannot go back to hotspot.
	if loc := m.adminPost(t, "/admin/sites/mode", url.Values{"id": {s.ID}, "mode": {"hotspot"}}); !strings.Contains(loc, "Error") || !strings.Contains(loc, "ftn-dhcp") {
		t.Fatal(loc)
	}
	for _, r := range rt.Table("/ip/dhcp-server") {
		rt.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "true"})
	}
	if err := m.mgr.SetAccessMode(s.ID, store.AccessHotspot); err != nil {
		t.Fatal(err)
	}
	for _, r := range rt.Table("/ip/dhcp-server") {
		rt.SetRow(context.Background(), "/ip/dhcp-server", r[".id"], map[string]string{"disabled": "false"})
	}
	// The cutover command refuses on a building that is not IPoE.
	if loc := m.adminPost(t, "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"cutover"}}); !strings.Contains(loc, "Error") {
		t.Fatal(loc)
	}
	// Mode switch audits and reconciles at once.
	loc := m.adminPost(t, "/admin/sites/mode", url.Values{"id": {s.ID}, "mode": {"ipoe_observe"}})
	if !strings.Contains(loc, "ipoe_observe") || strings.Contains(loc, "Error") {
		t.Fatal(loc)
	}
	if loc = m.adminPost(t, "/admin/sites/mode", url.Values{"id": {s.ID}, "mode": {"nonsense"}}); !strings.Contains(loc, "Error") {
		t.Fatal(loc)
	}
	// Adopt leases.
	lease(rt, "AA:BB:CC:DD:EE:01", "10.5.50.77", "")
	if loc = m.adminPost(t, "/admin/sites/adopt", url.Values{"id": {s.ID}}); !strings.Contains(loc, "Adopted") {
		t.Fatal(loc)
	}
	// Confirm cutover is refused until the router runs ftn-dhcp, and removes the timer when it does.
	rt.SeedRow("/system/scheduler", routeros.Row{"name": sites.RollbackSchedule})
	page = m.adminGet(t, "/admin/routers/view?id="+s.ID)
	if !strings.Contains(page, "Cutover not confirmed") {
		t.Fatal("pending cutover is not shown")
	}
	if loc = m.adminPost(t, "/admin/sites/confirm", url.Values{"id": {s.ID}}); strings.Contains(loc, "Error") {
		t.Fatal(loc) // ftn-dhcp is enabled in ipoeApp
	}
	if len(rt.Table("/system/scheduler")) != 0 {
		t.Fatal("rollback timer must be removed")
	}
}

func TestJoinURLStageWithoutPublicIPKeepsToken(t *testing.T) {
	m, s, _ := ipoeApp(t, nil) // no PUBLIC_IP: the cutover cannot be made
	m.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.TunnelIP = "10.99.0.5"
		d.Sites[s.ID] = x
		return nil
	})
	tok, _, _ := m.mgr.MintJoinStage(s.ID, "cutover")
	for i := 0; i < 2; i++ { // still 409, never 404: the token was not used up
		req, _ := newReq(m.ts.URL + "/j/" + tok + "?stage=cutover")
		resp, _ := m.cl.Do(req)
		if b := body(t, resp); resp.StatusCode != 409 || !strings.Contains(b, "PUBLIC_IP") {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
}

func TestJoinURLStageAndRefusal(t *testing.T) {
	m, s, _ := ipoeApp(t, func(c *config.Config) { c.PublicIP = "203.0.113.10" })
	if _, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"}); err != nil {
		t.Fatal(err)
	}
	// addSite gave the building a key, so it counts as joined.
	m.st.Update(func(d *store.Data) error {
		x := d.Sites[s.ID]
		x.TunnelIP = "10.99.0.5"
		d.Sites[s.ID] = x
		return nil
	})
	tok, _, err := m.mgr.MintJoinStage(s.ID, "cutover")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		req, _ := newReq(m.ts.URL + path)
		resp, err := m.cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body(t, resp)
	}
	// The user agent check applies to every stage.
	req, _ := newReq(m.ts.URL + "/j/" + tok + "?stage=cutover")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if resp, _ := m.cl.Do(req); resp.StatusCode != 404 {
		t.Fatalf("browser UA: %d", resp.StatusCode)
	}
	code, b := get("/j/" + tok + "?stage=cutover")
	if code != 200 || !strings.Contains(b, "stage: cutover") || !strings.Contains(b, "ftn-rollback") {
		t.Fatalf("%d %s", code, b)
	}
	if code, _ = get("/j/" + tok + "?stage=cutover"); code != 404 {
		t.Fatalf("token reuse: %d", code)
	}
}

// payClient is a visitor of /pay: it keeps the CSRF cookie and posts the token.
type payClient struct {
	t    *testing.T
	m    *multi
	cl   *http.Client
	csrf string
}

var csrfRe = regexp.MustCompile(`name="_pcsrf" value="([^"]+)"`)

func newPayClient(t *testing.T, m *multi) (*payClient, string) {
	jar, _ := cookiejar.New(nil)
	c := &payClient{t: t, m: m, cl: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	r, err := c.cl.Get(m.ts.URL + "/pay")
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, r)
	if mm := csrfRe.FindStringSubmatch(page); mm != nil {
		c.csrf = mm[1]
	}
	return c, page
}

func (c *payClient) post(path string, v url.Values, withCSRF bool) (*http.Response, string) {
	if withCSRF {
		v.Set("_pcsrf", c.csrf)
	}
	r, err := c.cl.PostForm(c.m.ts.URL+path, v)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	return r, string(b)
}

func TestPayPageFindAndStart(t *testing.T) {
	m, s, _ := ipoeApp(t, nil)
	a, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01", Tenant: "Jane Wanjiru", Phone: "0712345678"})
	if err != nil {
		t.Fatal(err)
	}
	c, page := newPayClient(t, m)
	if !strings.Contains(page, "Account number") || !strings.Contains(page, "0700 111 222") || c.csrf == "" {
		t.Fatalf("%s", page)
	}
	// No token, no go.
	if r, _ := c.post("/pay/find", url.Values{"code": {a.Code}}, false); r.StatusCode != 403 {
		t.Fatalf("missing csrf: %d", r.StatusCode)
	}
	if r, _ := c.post("/pay/start", url.Values{"code": {a.Code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0712345678"}}, false); r.StatusCode != 403 {
		t.Fatalf("missing csrf on start: %d", r.StatusCode)
	}
	r, b := c.post("/pay/find", url.Values{"code": {"NOPE99"}}, true)
	if r.StatusCode != 404 || strings.Contains(b, "Flat 3B") {
		t.Fatal("unknown account")
	}
	r, b = c.post("/pay/find", url.Values{"code": {strings.ToLower(a.Code)}}, true)
	if r.StatusCode != 200 || !strings.Contains(b, "Flat 3B") || !strings.Contains(b, "Account "+a.Code) || !strings.Contains(b, `name="package"`) || !strings.Contains(b, "Riverside") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	for _, leak := range []string{"Jane", "0712345678", "AA:BB:CC:DD:EE:01", "10.5.50"} {
		if strings.Contains(b, leak) {
			t.Errorf("pay page leaks %q", leak)
		}
	}
	tok := regexp.MustCompile(`name="tok" value="([^"]+)"`).FindStringSubmatch(b)
	if tok == nil {
		t.Fatal("no signed token")
	}
	// Pay with the signed token (test provider): the flat is un-walled by the reconciler.
	r, b = c.post("/pay/start", url.Values{"tok": {tok[1]}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0712345678"}}, true)
	if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/portal/wait?order=") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := m.svc.GetAccount(a.ID); billing.AccountState(got, m.svc.Now()) == billing.StateActive {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, _ := m.svc.GetAccount(a.ID); billing.AccountState(got, m.svc.Now()) != billing.StateActive {
		t.Fatal("not paid")
	}
	// A forged or expired token is refused.
	if r, _ = c.post("/pay/start", url.Values{"tok": {tok[1] + "x"}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0712345678"}}, true); r.StatusCode != 404 {
		t.Fatalf("forged token: %d", r.StatusCode)
	}
	// Bad phone: friendly error, no order.
	r, b = c.post("/pay/start", url.Values{"code": {a.Code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"123"}}, true)
	if r.StatusCode != 400 || !strings.Contains(b, "Flat 3B") {
		t.Fatalf("bad phone: %d", r.StatusCode)
	}
	if r, _ = c.post("/pay/start", url.Values{"code": {"NOPE99"}, "package": {"x"}, "method": {"test"}, "phone": {"0712345678"}}, true); r.StatusCode != 404 {
		t.Fatalf("unknown code start: %d", r.StatusCode)
	}
	// Old hotspot entry points go to /pay for an IPoE building, and its hotspot files are not served.
	rr, _ := m.cl.Get(m.ts.URL + "/portal?site=" + s.ID)
	if rr.StatusCode != 303 || rr.Header.Get("Location") != "/pay" {
		t.Fatalf("portal redirect: %d %s", rr.StatusCode, rr.Header.Get("Location"))
	}
	rr, _ = m.cl.Get(m.ts.URL + "/routeros/hotspot/" + s.ID + "/login.html")
	if rr.StatusCode != 404 {
		t.Fatalf("hotspot file for an IPoE building: %d", rr.StatusCode)
	}
}

// ipoeAppDetected is ipoeApp where the test client counts as inside the building (its address is the WireGuard peer's endpoint).
func ipoeAppDetected(t *testing.T) (*multi, store.Site, *routeros.Mock) {
	dump := filepath.Join(t.TempDir(), "wg0.dump")
	m, s, rt := ipoeApp(t, func(c *config.Config) { c.WGDumpPath = dump })
	var key string
	m.st.View(func(d *store.Data) { key = d.Sites[s.ID].WGPublicKey })
	os.WriteFile(dump, []byte(fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.5/32\t%d\t1\t1\t25\n", key, time.Now().Unix())), 0o600)
	return m, s, rt
}

// A visitor from an address that is not a known building gets much tighter limits.
func TestPayStrangersAreLimitedTighter(t *testing.T) {
	m, s, _ := ipoeApp(t, nil)
	a, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	c, page := newPayClient(t, m)
	if strings.Contains(page, "paused") {
		t.Fatal("not detected")
	}
	n429 := 0
	for i := 0; i < 12; i++ {
		r, _ := c.post("/pay/start", url.Values{"code": {a.Code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"bad"}}, true)
		if r.StatusCode == 429 {
			n429++
		}
	}
	if n429 == 0 {
		t.Fatal("a stranger was never limited")
	}
}

func TestPayLimitsAndGenericErrors(t *testing.T) {
	m, s, _ := ipoeAppDetected(t)
	a, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	b2, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 4A", MAC: "AA:BB:CC:DD:EE:02"})
	c, _ := newPayClient(t, m)
	start := func(code, phone string) int {
		r, _ := c.post("/pay/start", url.Values{"code": {code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {phone}}, true)
		return r.StatusCode
	}
	// Guesses at codes that do not exist do not use up any account's allowance.
	for i := 0; i < 3; i++ {
		start("NOPE99", "0712345678")
	}
	// The same account from the same address is limited after a few tries (bad phone: no order is made) ...
	var last int
	for i := 0; i < 5; i++ {
		last = start(a.Code, "bad")
	}
	if last != 429 {
		t.Fatalf("expected the account to be limited from this address, got %d", last)
	}
	// ... but that does not lock the building out: another flat from the same address still works.
	if got := start(b2.Code, "bad"); got != 400 {
		t.Fatalf("another flat must not be blocked by one flat's retries: %d", got)
	}
	// One phone gets only a few prompts, whatever account asks.
	c2, _ := newPayClient(t, m)
	for _, code := range []string{a.Code, b2.Code} {
		_ = code
	}
	n429 := 0
	for i := 0; i < 6; i++ {
		r, _ := c2.post("/pay/start", url.Values{"code": {[]string{a.Code, b2.Code}[i%2]}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0799000111"}}, true)
		if r.StatusCode == 429 {
			n429++
		}
	}
	if n429 == 0 {
		t.Fatal("the phone limit never applied")
	}
}

func TestPayFlatLookupDoesNotRevealTheAccountCode(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "wg0.dump")
	m := newMultiOps(t, func(c *config.Config) { c.WGDumpPath = dump }, nil)
	s := m.addSite(t, "Riverside")
	m.mgr.SetAccessMode(s.ID, store.AccessIPoE)
	var key string
	m.st.View(func(d *store.Data) { key = d.Sites[s.ID].WGPublicKey })
	os.WriteFile(dump, []byte(fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.5/32\t%d\t1\t1\t25\n", key, time.Now().Unix())), 0o600)
	a, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	c, page := newPayClient(t, m)
	if !strings.Contains(page, "Riverside") || !strings.Contains(page, "Your internet is paused") {
		t.Fatalf("building not detected: %s", page)
	}
	r, b := c.post("/pay/find", url.Values{"code": {"flat 3b"}}, true)
	if r.StatusCode != 200 || !strings.Contains(b, "Flat 3B") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	for _, leak := range []string{a.Code, "Account ", "Active", "Expired", "No package yet"} {
		if strings.Contains(b, leak) {
			t.Errorf("a flat-label lookup reveals %q", leak)
		}
	}
	if !strings.Contains(b, `name="tok"`) || strings.Contains(b, `name="code"`) {
		t.Fatal("the page carries a signed token, not the code")
	}
}

func TestPayDetectsBuildingFromAddress(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "wg0.dump")
	m := newMultiOps(t, func(c *config.Config) { c.WGDumpPath = dump }, nil)
	s := m.addSite(t, "Riverside")
	other := m.addSite(t, "Hillcrest")
	m.mgr.SetAccessMode(s.ID, store.AccessIPoE)
	var key, key2 string
	m.st.View(func(d *store.Data) { key, key2 = d.Sites[s.ID].WGPublicKey, d.Sites[other.ID].WGPublicKey })
	// The test client connects from 127.0.0.1: pretend that is the router's WAN address.
	os.WriteFile(dump, []byte(fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.5/32\t%d\t1\t1\t25\n", key, time.Now().Unix())), 0o600)
	m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	_, page := m.portal(t, "/pay")
	if !strings.Contains(page, "Riverside") {
		t.Fatalf("building not detected: %s", page)
	}
	// A second building behind the same address: ambiguous, so the page does not guess and a flat label alone finds nothing.
	os.WriteFile(dump, []byte(fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.5/32\t%d\t1\t1\t25\n%s\t127.0.0.1:51821\t10.99.0.6/32\t%d\t1\t1\t25\n", key, time.Now().Unix(), key2, time.Now().Unix())), 0o600)
	c, page := newPayClient(t, m)
	if strings.Contains(page, "Riverside") {
		t.Fatal("two buildings behind one address must not be guessed")
	}
	if r, _ := c.post("/pay/find", url.Values{"code": {"flat 3b"}}, true); r.StatusCode != 404 {
		t.Fatalf("a flat label alone must not resolve without a building: %d", r.StatusCode)
	}
}

func TestPayLimitKeyUsesTheIPv6Prefix(t *testing.T) {
	if web.LimitKey("2001:db8:1:2:aaaa:bbbb:cccc:dddd") != web.LimitKey("2001:db8:1:2:1111:2222:3333:4444") {
		t.Fatal("addresses of one /64 share a key")
	}
	if web.LimitKey("2001:db8:1:3::1") == web.LimitKey("2001:db8:1:2::1") || web.LimitKey("10.0.0.1") != "10.0.0.1" {
		t.Fatal("other prefixes and IPv4 are keyed as they are")
	}
}

func newReq(u string) (*http.Request, error) {
	req, err := http.NewRequest("GET", u, nil)
	if req != nil {
		req.Header.Set("User-Agent", "MikroTik/7.20 (fetch)")
	}
	return req, err
}

func TestModeOverrideAndPreflightLists(t *testing.T) {
	m, s, rt := ipoeApp(t, nil)
	if _, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"}); err != nil {
		t.Fatal(err)
	}
	// A device with no lease shows up in the command-time preflight with the force button.
	rt.SeedRow("/ip/arp", routeros.Row{"mac-address": "00:19:3E:00:00:01", "address": "10.5.50.5", "interface": "br-hotspot"})
	r := m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"cutover"}})
	b := body(t, r)
	if !strings.Contains(b, "00:19:3E:00:00:01") || !strings.Contains(b, "Cut over anyway") || strings.Contains(b, "ftn-cutover.rsc") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// Unreachable router: the switch back to hotspot is refused unless overridden.
	rt.SetFail(errors.New("timeout"))
	if loc := m.adminPost(t, "/admin/sites/mode", url.Values{"id": {s.ID}, "mode": {"hotspot"}}); !strings.Contains(loc, "Error") || !strings.Contains(loc, "cannot be reached") {
		t.Fatal(loc)
	}
	if loc := m.adminPost(t, "/admin/sites/mode", url.Values{"id": {s.ID}, "mode": {"hotspot"}, "override": {"1"}}); strings.Contains(loc, "Error: the router") {
		t.Fatal(loc)
	}
}
