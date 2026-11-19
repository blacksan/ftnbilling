package web_test

import (
	"context"
	"encoding/json"
	"ispbill/internal/routeros"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// Wiring tests: the web admin (W7) uses the real services of W5c (set-up job,
// CreateCustomer, CreateRouter) and W9 (live views, guarded reboot, overview).

func routerState(t *testing.T, j map[string]any) map[string]any {
	t.Helper()
	rs, ok := j["router"].(map[string]any)
	if !ok {
		t.Fatalf("no router state in %v", j)
	}
	return rs
}

func TestWireAddCustomerReportsTheRealRouterState(t *testing.T) {
	m, s, rt := ipoeApp(t, nil)
	pkg := m.pkgID("Monthly 20 Mbps")
	// A joined router that answers: the flat is put on it during the request.
	code, j := m.postJSON(t, "/admin/accounts/create", url.Values{"site": {s.ID}, "flat": {"Flat W1"}, "tenant": {"Achieng"}, "package": {pkg}, "mac": {"aa-bb-cc-00-00-71"}, "activate": {"1"}})
	if code != 201 || j["ok"] != true {
		t.Fatalf("create: %d %v", code, j)
	}
	rs := routerState(t, j)
	if rs["state"] != "ok" || rs["done"] != true || rs["text"] != "Provisioned on router" {
		t.Fatalf("a reachable router should report provisioned: %v", rs)
	}
	var st map[string]any
	m.getJSON(t, "/admin/accounts/status.json?id="+j["id"].(string), &st)
	if st["state"] != "ok" || st["in_sync"] != true {
		t.Fatalf("status.json disagrees: %v", st)
	}
	var queues int
	for _, q := range rt.Table("/queue/simple") {
		if strings.Contains(q["name"], j["code"].(string)) {
			queues++
		}
	}
	if queues != 1 {
		t.Fatalf("the flat's queue is not on the router: %d", queues)
	}
	// Activate needs a plan; a bad plan leaves nothing behind.
	if code, j := m.postJSON(t, "/admin/accounts/create", url.Values{"site": {s.ID}, "flat": {"Flat W2"}, "mac": {"AA:BB:CC:00:00:72"}, "activate": {"1"}}); code != 422 || j["ok"] != false {
		t.Fatalf("activate without plan: %d %v", code, j)
	}
	if code, j := m.postJSON(t, "/admin/accounts/create", url.Values{"site": {s.ID}, "flat": {"Flat W3"}, "mac": {"AA:BB:CC:00:00:73"}, "package": {"nope"}}); code != 422 || j["ok"] != false {
		t.Fatalf("unknown plan: %d %v", code, j)
	}
	if n := countFlat(m, "Flat W3"); n != 0 {
		t.Fatalf("a refused create left a flat behind")
	}

	// A router that has not joined: queued, with the reason.
	fresh, err := m.mgr.CreateRouter(sites.SiteInput{Name: "Unjoined"})
	if err != nil {
		t.Fatal(err)
	}
	code, j = m.postJSON(t, "/admin/accounts/create", url.Values{"site": {fresh.ID}, "flat": {"Flat Q1"}, "package": {pkg}, "mac": {"AA:BB:CC:00:00:74"}})
	if code != 201 {
		t.Fatalf("%d %v", code, j)
	}
	rs = routerState(t, j)
	if rs["state"] != "wait" || rs["done"] != true || !strings.Contains(rs["text"].(string), "Queued") {
		t.Fatalf("an unjoined router should report queued: %v", rs)
	}
	// The same MAC is refused, naming the owner.
	code, j = m.postJSON(t, "/admin/accounts/create", url.Values{"site": {s.ID}, "flat": {"Flat W4"}, "mac": {"AA:BB:CC:00:00:71"}})
	if code != 422 || j["ok"] != false || !strings.Contains(j["error"].(string), "Flat W1") {
		t.Fatalf("duplicate: %d %v", code, j)
	}
	// Without JavaScript the flash carries the same words.
	loc := m.adminPost(t, "/admin/accounts/create", url.Values{"site": {s.ID}, "flat": {"Flat W5"}, "mac": {"AA:BB:CC:00:00:75"}, "package": {pkg}})
	if !strings.Contains(loc, "Account created: Flat W5") || !strings.Contains(loc, "Provisioned on router") {
		t.Fatalf("no-JS create: %s", loc)
	}
}

func countFlat(m *multi, flat string) (n int) {
	m.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.Flat == flat {
				n++
			}
		}
	})
	return
}

func TestWireAddRouterMakesTheBootstrapLine(t *testing.T) {
	r := newAPIRig(t)
	code, j := r.postJSON(t, "/admin/routers/create", url.Values{"name": {"Web New"}, "wanmode": {"static"}, "wanaddr": {"198.51.100.5/24"}, "wangw": {"198.51.100.1"}})
	if code != 200 || j["ok"] != true {
		t.Fatalf("%d %v", code, j)
	}
	id := j["id"].(string)
	cmd := j["command"].(string)
	if !strings.Contains(cmd, "/reset") || !strings.Contains(cmd, "/import ftn-reset.rsc") || strings.Contains(cmd, "hotspot") {
		t.Errorf("the line: %s", cmd)
	}
	var st store.Site
	r.st.View(func(d *store.Data) { st = d.Sites[id] })
	if st.AccessMode != store.AccessIPoE || st.WANMode != "static" || st.WANAddress != "198.51.100.5/24" {
		t.Errorf("%+v", st)
	}
	// a bad layout is refused
	if code, _ := r.postJSON(t, "/admin/routers/create", url.Values{"name": {"Bad"}, "wanmode": {"static"}}); code != 422 {
		t.Errorf("static WAN without an address: %d", code)
	}
	join := r.admin("POST", "/admin/routers/join", url.Values{"id": {id}})
	if b := body(t, join); join.StatusCode != 200 || !strings.Contains(b, "/import ftn-reset.rsc") {
		t.Errorf("the Setup tab line: %d", join.StatusCode)
	}
}

func TestWireSetupJSONShape(t *testing.T) {
	r, _ := liveRig(t)
	var j map[string]any
	r.getJSON(t, "/admin/routers/setup.json?id="+r.A.ID, &j)
	for _, k := range []string{"state", "steps", "next_action", "join", "checks", "blockers", "last_error"} {
		if _, ok := j[k]; !ok {
			t.Errorf("setup.json lacks %q", k)
		}
	}
	if steps, _ := j["steps"].([]any); len(steps) != 4 {
		t.Errorf("steps: %v", j["steps"])
	}
	for _, p := range []string{"run", "migrate", "rollback"} {
		if code, _ := r.postJSON(t, "/admin/routers/setup/"+p, url.Values{"id": {r.A.ID}}); code != 410 {
			t.Errorf("setup/%s: %d, want 410", p, code)
		}
	}
}

func TestWireClientsInterfacesAndOverviewTabs(t *testing.T) {
	r, mk := liveRig(t)
	a1 := r.acct(t, r.A, "Flat 3B", "AA:BB:CC:DD:EE:01")
	a2 := r.acct(t, r.A, "Flat 10A", "AA:BB:CC:DD:EE:02")
	for _, q := range mk.Table("/queue/simple") { // the seeded queue belongs to the first flat
		if q["name"] == "ftn-K7Q2MX" {
			mk.SetRow(context.Background(), "/queue/simple", q[routeros.RowID], map[string]string{"name": "ftn-" + a1.Code})
		}
	}
	// "Flat 3B" is also the Add customer placeholder, so look for the flat's own link
	has := func(h string, a store.Account) bool { return strings.Contains(h, "/admin/accounts/view?code="+a.Code) }
	view := func(tab, q string) string {
		p := "/admin/routers/view?id=" + r.A.ID + "&tab=" + tab
		if q != "" {
			p += "&q=" + url.QueryEscape(q)
		}
		return r.adminGet(t, p)
	}
	all := view("clients", "")
	for _, want := range []string{"data-clients-q", "20M/20M", "2 flats"} {
		if !strings.Contains(all, want) {
			t.Errorf("clients tab lacks %q", want)
		}
	}
	if !has(all, a1) || !has(all, a2) {
		t.Errorf("clients tab lacks a flat")
	}
	one := view("clients", "10a")
	if !has(one, a2) || has(one, a1) || strings.Contains(one, "20M/20M") {
		t.Errorf("search by flat did not narrow the list")
	}
	if byIP := view("clients", "10.5.50.60"); !has(byIP, a1) || has(byIP, a2) {
		t.Errorf("search by IP")
	}
	if byMAC := view("clients", "ee:02"); !has(byMAC, a2) || has(byMAC, a1) {
		t.Errorf("search by MAC")
	}
	if none := view("clients", "zzzz"); !strings.Contains(none, "Nothing matches that search") {
		t.Errorf("empty search state")
	}
	ifs := view("interfaces", "")
	for _, want := range []string{"ether1", "sfp-sfpplus1", "wg-ftn", "br-hotspot", ">wan<", ">olt<", ">tunnel<"} {
		if !strings.Contains(ifs, want) {
			t.Errorf("interfaces tab lacks %q", want)
		}
	}
	ov := view("overview", "")
	for _, want := range []string{"RouterOS 7.25.1", "7%", "Flats online", `id="rt-live"`} {
		if !strings.Contains(ov, want) {
			t.Errorf("overview tab lacks %q", want)
		}
	}
	// a router that cannot be read says so instead of showing zeros as facts
	b := r.adminGet(t, "/admin/routers/view?id="+r.B.ID+"&tab=overview")
	if !strings.Contains(b, "could not be read") {
		t.Errorf("an unreadable router is not flagged")
	}
}

func TestWireRebootThroughTheWebUsesTheSameGuards(t *testing.T) {
	r, mk := liveRig(t)
	reboots := func() int {
		n := 0
		for _, c := range mk.Calls() {
			if c.Path == "/system/reboot" {
				n++
			}
		}
		return n
	}
	danger := r.adminGet(t, "/admin/routers/view?id="+r.A.ID+"&tab=danger")
	if !strings.Contains(danger, `action="/admin/routers/reboot"`) || strings.Contains(danger, "Hook: arrives") || strings.Contains(danger, "disabled>Reboot") {
		t.Fatal("the Reboot button is not wired")
	}
	reboot := func(name string) (int, map[string]any) {
		return r.postJSON(t, "/admin/routers/reboot", url.Values{"id": {r.A.ID}, "confirm_name": {name}})
	}
	if code, j := reboot("Beta"); code != 422 || j["ok"] != false {
		t.Fatalf("wrong name: %d %v", code, j)
	}
	if reboots() != 0 {
		t.Fatal("a refused request rebooted the router")
	}
	if code, j := reboot("alpha"); code != 202 || j["ok"] != true || reboots() != 1 {
		t.Fatalf("reboot: %d %v (%d)", code, j, reboots())
	}
	// the limiter is shared with the API: the API is now refused, and so is the web
	if x := r.call(r.owner.Access, "POST", "/routers/"+r.A.ID+"/actions/reboot", map[string]any{"confirm_name": "Alpha"}, nil); x.Status != 429 {
		t.Fatalf("API after web: %d %s", x.Status, x.Raw)
	}
	if code, _ := reboot("Alpha"); code != 429 || reboots() != 1 {
		t.Fatalf("second web reboot: %d (%d)", code, reboots())
	}
	// and the other way round
	r.mock(r.B.ID)
	if x := r.call(r.owner.Access, "POST", "/routers/"+r.B.ID+"/actions/reboot", map[string]any{"confirm_name": "Beta"}, nil); x.Status != 202 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	if code, _ := r.postJSON(t, "/admin/routers/reboot", url.Values{"id": {r.B.ID}, "confirm_name": {"Beta"}}); code != 429 {
		t.Fatalf("web after API: %d", code)
	}
	// owner only: an operator's POST is refused before anything happens
	opBr := signIn(t, r.ts, r.srv, r.st, r.operW)
	req, _ := newJSONReq(r.ts.URL+"/admin/routers/reboot", url.Values{"id": {r.A.ID}, "confirm_name": {"Alpha"}, "_csrf": {opBr.CSRF}})
	req.AddCookie(opBr.Cookie)
	resp, err := r.cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || reboots() != 1 {
		t.Fatalf("operator: %d", resp.StatusCode)
	}
}

func TestWireOverviewLinksAllResolve(t *testing.T) {
	r := newAPIRig(t)
	r.acct(t, r.A, "Flat 1", "AA:BB:CC:DD:EE:01")
	// the overview as the API serves it, and every href in it
	res := r.call(r.view.Access, "GET", "/overview", nil, nil)
	if res.Status != 200 {
		t.Fatalf("%d %s", res.Status, res.Raw)
	}
	var ov struct {
		Setup struct{ Steps []struct{ Href string } }
		Next  []struct{ Href string }
	}
	if err := json.Unmarshal(res.Raw, &ov); err != nil {
		t.Fatal(err)
	}
	hrefs := map[string]bool{
		// the ones that only show when there is something to do
		"/admin/plans": true, "/admin/routers": true, "/admin/subscribers": true, "/admin/devices": true, "/admin/payments?tab=unmatched": true,
		"/admin/routers?tab=offline": true, "/admin/subscribers?tab=active": true, "/admin/subscribers?tab=mac_conflict": true, "/admin/subscribers?tab=new": true,
		"/admin/settings/mpesa": true, "/admin/settings/whatsapp": true,
	}
	for _, s := range ov.Setup.Steps {
		hrefs[s.Href] = true
	}
	for _, n := range ov.Next {
		hrefs[n.Href] = true
	}
	for h := range hrefs {
		resp := r.admin("GET", h, nil)
		for i := 0; i < 3 && resp.StatusCode == http.StatusFound; i++ {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			resp = r.admin("GET", loc, nil)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", h, resp.StatusCode)
		}
	}
	// the redirects keep what they were asked for
	for from, to := range map[string]string{"/admin/plans": "/admin/packages", "/admin/subscribers?tab=mac_conflict": "/admin/accounts?tab=conflict"} {
		resp := r.admin("GET", from, nil)
		resp.Body.Close()
		if resp.StatusCode != 302 || resp.Header.Get("Location") != to {
			t.Errorf("%s -> %d %q, want %q", from, resp.StatusCode, resp.Header.Get("Location"), to)
		}
	}
	if h := r.adminGet(t, "/admin/payments?tab=unmatched"); !strings.Contains(h, `aria-current="page"`) {
		t.Error("the unmatched payments tab is not selected")
	}
}

func TestWireOverviewPageShowsTheAPINumbers(t *testing.T) {
	r, _ := liveRig(t)
	a := r.acct(t, r.A, "Flat 3B", "AA:BB:CC:DD:EE:01")
	if err := r.svc.ExtendAccount(context.Background(), a.ID, r.pkgID(t, "Monthly 20 Mbps"), "wire test", false); err != nil {
		t.Fatal(err)
	}
	res := r.call(r.view.Access, "GET", "/overview", nil, nil)
	var ov struct {
		Active  int `json:"active"`
		Setup   struct{ Done, Total int }
		Routers struct{ Total, Online int }
	}
	if err := json.Unmarshal(res.Raw, &ov); err != nil {
		t.Fatal(err)
	}
	page := r.adminGet(t, "/admin")
	for _, want := range []string{"Get set up", "What's next"} {
		if !strings.Contains(page, want) {
			t.Errorf("overview page lacks %q", want)
		}
	}
	if ov.Active != 1 || !strings.Contains(page, `aria-label="Setup progress"`) {
		t.Errorf("active %d", ov.Active)
	}
}

func TestWireOutageNoticeReturnsToRouters(t *testing.T) {
	m, site, _ := uiRig(t)
	resp := m.admin("POST", "/admin/routers/outage-notice", url.Values{"site": {site}})
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != 303 || !strings.HasPrefix(loc, "/admin/routers?") || strings.Contains(loc, "fleet") {
		t.Fatalf("%d %q", resp.StatusCode, loc)
	}
}

func TestWireResetNeedsTheTypedName(t *testing.T) {
	r, _ := liveRig(t)
	if code, j := r.postJSON(t, "/admin/routers/setup/reset", url.Values{"id": {r.A.ID}}); code != 422 || j["error"] == nil {
		t.Fatalf("reset without a name: %d %v", code, j)
	}
	if code, j := r.postJSON(t, "/admin/routers/setup/reset", url.Values{"id": {r.A.ID}, "confirm_name": {"Alpha"}}); code != 200 {
		t.Fatalf("reset with the name: %d %v", code, j)
	}
}

func TestFixECreateCustomerKeepsNotesAndWhatsApp(t *testing.T) {
	m, s, _ := ipoeApp(t, nil)
	pkg := m.pkgID("Monthly 20 Mbps")
	code, j := m.postJSON(t, "/admin/accounts/create", url.Values{"site": {s.ID}, "flat": {"Flat N1"}, "package": {pkg}, "mac": {"AA:BB:CC:00:00:91"}, "activate": {"1"}, "notes": {"gate code 4411"}, "whatsapp": {"0722000999"}})
	if code != 201 {
		t.Fatalf("%d %v", code, j)
	}
	var notes, wa string
	m.st.View(func(d *store.Data) { a := d.Accounts[j["id"].(string)]; notes, wa = a.Notes, a.WhatsApp })
	if notes != "gate code 4411" || wa == "" {
		t.Fatalf("notes %q whatsapp %q", notes, wa)
	}
}

func TestFixEMapGeoLinkSurvivesTemplateEscaping(t *testing.T) {
	m, site, _ := uiRig(t)
	m.st.Update(func(d *store.Data) error {
		x := d.Sites[site]
		x.Lat, x.Lng = -1.2864, 36.8172
		d.Sites[site] = x
		return nil
	})
	if h := m.adminGet(t, "/admin/map"); !strings.Contains(h, `href="geo:-1.286400,36.817200"`) {
		t.Error("geo link missing or blanked on the map page")
	}
}
