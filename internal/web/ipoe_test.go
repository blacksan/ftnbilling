package web_test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	page := m.adminGet(t, "/admin/sites/view?id="+s.ID)
	for _, want := range []string{"Access mode", "/admin/sites/adopt", "Cut over command", "Join only command", "Cleanup command", "in sync"} {
		if !strings.Contains(page, want) {
			t.Errorf("site page lacks %q", want)
		}
	}
	// A command is minted for a stage; the join command itself is not changed.
	r := m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"cutover"}})
	b := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, "ftn-cutover.rsc") || !strings.Contains(b, "?stage=cutover") || !strings.Contains(b, "stage: cutover") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	if r = m.admin("POST", "/admin/sites/script", url.Values{"id": {s.ID}, "stage": {"wipe"}}); r.StatusCode != 404 {
		t.Fatalf("unknown stage: %d", r.StatusCode)
	}
	// The cutover command refuses on a building that is not IPoE.
	if err := m.mgr.SetAccessMode(s.ID, store.AccessHotspot); err != nil {
		t.Fatal(err)
	}
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
	page = m.adminGet(t, "/admin/sites/view?id="+s.ID)
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
	tok, _, _ := m.mgr.MintJoin(s.ID)
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
	tok, _, err := m.mgr.MintJoin(s.ID)
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

func TestPayPageFindAndStart(t *testing.T) {
	m, s, _ := ipoeApp(t, nil)
	a, err := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01", Tenant: "Jane Wanjiru", Phone: "0712345678"})
	if err != nil {
		t.Fatal(err)
	}
	code, page := m.portal(t, "/pay")
	if code != 200 || !strings.Contains(page, "Account number") || !strings.Contains(page, "0700 111 222") {
		t.Fatalf("%d %s", code, page)
	}
	// An unknown code and a known one look different only in what they reveal after a match.
	r, _ := m.cl.PostForm(m.ts.URL+"/pay/find", url.Values{"code": {"NOPE99"}})
	if r.StatusCode != 404 || strings.Contains(body(t, r), "Flat 3B") {
		t.Fatal("unknown account")
	}
	r, _ = m.cl.PostForm(m.ts.URL+"/pay/find", url.Values{"code": {strings.ToLower(a.Code)}})
	b := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(b, "Flat 3B") || !strings.Contains(b, `name="package"`) || !strings.Contains(b, "Riverside") {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	for _, leak := range []string{"Jane", "0712345678", "AA:BB:CC:DD:EE:01", "10.5.50"} {
		if strings.Contains(b, leak) {
			t.Errorf("pay page leaks %q", leak)
		}
	}
	// Pay (test provider): the order is created without a MAC and the flat is un-walled by the reconciler.
	r, _ = m.cl.PostForm(m.ts.URL+"/pay/start", url.Values{"code": {a.Code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"0712345678"}})
	if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/portal/wait?order=") {
		t.Fatalf("%d %s", r.StatusCode, body(t, r))
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
	// Bad phone, bad package: friendly errors, no order.
	r, _ = m.cl.PostForm(m.ts.URL+"/pay/start", url.Values{"code": {a.Code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"123"}})
	if r.StatusCode != 400 || !strings.Contains(body(t, r), "Flat 3B") {
		t.Fatalf("bad phone: %d", r.StatusCode)
	}
	r, _ = m.cl.PostForm(m.ts.URL+"/pay/start", url.Values{"code": {"NOPE99"}, "package": {"x"}, "method": {"test"}, "phone": {"0712345678"}})
	if r.StatusCode != 404 {
		t.Fatalf("unknown code start: %d", r.StatusCode)
	}
	// Old hotspot entry points go to /pay for an IPoE building.
	r, _ = m.cl.Get(m.ts.URL + "/portal?site=" + s.ID)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/pay" {
		t.Fatalf("portal redirect: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// And its hotspot files are not served.
	r, _ = m.cl.Get(m.ts.URL + "/routeros/hotspot/" + s.ID + "/login.html")
	if r.StatusCode != 404 {
		t.Fatalf("hotspot file for an IPoE building: %d", r.StatusCode)
	}
}

func TestPayStartIsRateLimited(t *testing.T) {
	m, s, _ := ipoeApp(t, nil)
	a, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	var last int
	for i := 0; i < 6; i++ {
		r, _ := m.cl.PostForm(m.ts.URL+"/pay/start", url.Values{"code": {a.Code}, "package": {m.pkgID("1 Hour")}, "method": {"test"}, "phone": {"bad"}})
		r.Body.Close()
		last = r.StatusCode
	}
	if last != 429 {
		t.Fatalf("expected the account to be rate limited, got %d", last)
	}
}

func TestPayDetectsBuildingFromAddress(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "wg0.dump")
	m := newMultiOps(t, func(c *config.Config) { c.WGDumpPath = dump }, nil)
	s := m.addSite(t, "Riverside")
	other := m.addSite(t, "Hillcrest")
	m.mgr.SetAccessMode(s.ID, store.AccessIPoE)
	var key string
	m.st.View(func(d *store.Data) { key = d.Sites[s.ID].WGPublicKey })
	// The test client connects from 127.0.0.1: pretend that is the router's WAN address.
	os.WriteFile(dump, []byte(fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.5/32\t%d\t1\t1\t25\n", key, time.Now().Unix())), 0o600)
	a, _ := m.svc.CreateAccount(billing.AccountInput{SiteID: s.ID, Flat: "Flat 3B", MAC: "AA:BB:CC:DD:EE:01"})
	_, page := m.portal(t, "/pay")
	if !strings.Contains(page, "Riverside") || !strings.Contains(page, "Your internet is paused") {
		t.Fatalf("building not detected: %s", page)
	}
	// With the building known, the flat label works as well as the code.
	r, _ := m.cl.PostForm(m.ts.URL+"/pay/find", url.Values{"code": {"flat 3b"}})
	if b := body(t, r); r.StatusCode != 200 || !strings.Contains(b, "Account "+a.Code) {
		t.Fatalf("%d %s", r.StatusCode, b)
	}
	// A second building behind the same address: ambiguous, so the page does not guess.
	var key2 string
	m.st.View(func(d *store.Data) { key2 = d.Sites[other.ID].WGPublicKey })
	os.WriteFile(dump, []byte(fmt.Sprintf("%s\t127.0.0.1:51820\t10.99.0.5/32\t%d\t1\t1\t25\n%s\t127.0.0.1:51821\t10.99.0.6/32\t%d\t1\t1\t25\n", key, time.Now().Unix(), key2, time.Now().Unix())), 0o600)
	_, page = m.portal(t, "/pay")
	if strings.Contains(page, "Riverside") {
		t.Fatal("a building without a matching peer must not be detected")
	}
	r, _ = m.cl.PostForm(m.ts.URL+"/pay/find", url.Values{"code": {"flat 3b"}})
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("a flat label alone must not resolve without a building: %d", r.StatusCode)
	}
}

func newReq(u string) (*http.Request, error) {
	req, err := http.NewRequest("GET", u, nil)
	if req != nil {
		req.Header.Set("User-Agent", "MikroTik/7.20 (fetch)")
	}
	return req, err
}
