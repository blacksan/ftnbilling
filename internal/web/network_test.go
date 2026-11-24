package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

const testMapsKey = "AIzaSyTestKeyTestKeyTestKeyTestKey12345"

// netRig is the OLT story (Block A with an OLT on PON 1, an ONU in house A1) plus a second apartment with no GPS.
// The first apartment gets a GPS point; key sets the Google Maps key.
func netRig(t *testing.T, key string) *app {
	t.Helper()
	a := newAppCfg(t, false, func(c *config.Config) {
		c.ACSListen, c.ACSCRPorts, c.ACSInformInterval, c.ACSMaxPerRouter = "10.99.0.1:7547", []int{7547, 30005}, 300, 200
		c.WGSubnet = "10.99.0.0/24"
		c.GoogleMapsAPIKey = key
	})
	a.makeOLT(t, nil)
	oid := a.oltID(t)
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTENET01"}, "mac": {tONUMAC}, "model": {"F601"}, "placement": {"1"}, "olt": {oid}, "pon_port": {"1"}})
	var eq string
	a.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Serial == "ZTENET01" {
				eq = e.ID
			}
		}
	})
	a.adminPost(t, "/admin/accounts/create", url.Values{"flat": {"A1"}, "tenant": {"Jane Wanjiru"}, "equipment_id": {eq}, "apartment_id": {a.aptID(t)}})
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"switch"}, "serial": {"SWNET001"}, "model": {"CRS326"}})
	id := a.aptID(t)
	if err := a.st.Update(func(d *store.Data) error {
		ap := d.Apartments[id]
		lat, lng := -1.2864, 36.8172
		ap.Lat, ap.Lng, ap.Address = &lat, &lng, "Ngong Road, Nairobi"
		d.Apartments[id] = ap
		// the switch is installed at the same house
		for k, e := range d.Equipment {
			if e.Serial == "SWNET001" {
				for _, ac := range d.Accounts {
					if ac.Flat == "A1" {
						e.AccountID = ac.ID
						d.Equipment[k] = e
					}
				}
			}
		}
		nid := store.NewApartmentID()
		d.Apartments[nid] = store.Apartment{ID: nid, Name: `No GPS <b>Court</b>`, CreatedAt: time.Now()}
		d.ApartmentSites[store.ApartmentSiteKey(nid, a.site)] = store.ApartmentSite{ApartmentID: nid, SiteID: a.site, CreatedAt: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

type netJSON struct {
	MapsKeySet bool `json:"maps_key_set"`
	WithGPS    int  `json:"with_gps"`
	NoGPS      int  `json:"without_gps"`
	Apartments []struct {
		Name, State string
		HasGPS      bool     `json:"has_gps"`
		Lat         *float64 `json:"lat"`
		Lng         *float64 `json:"lng"`
		Houses      struct{ Active, Total, Online int }
		Routers     []struct{ Name, State, URL string }
		OLTs        []struct {
			Name, URL  string
			PONs       []int `json:"pons"`
			ONUsOnline int   `json:"onus_online"`
			ONUsTotal  int   `json:"onus_total"`
		}
		ONUs []struct {
			Serial, House, Customer, Status, URL string
			PON                                  int
		}
		Other      []struct{ Kind, Label, URL string }
		Alerts     int `json:"open_alerts"`
		ONUsTotal  int `json:"onus_total"`
		ONUsOnline int `json:"onus_online"`
	}
}

func getNetJSON(t *testing.T, a *app) (netJSON, string) {
	t.Helper()
	raw := a.adminGet(t, "/admin/network/map.json")
	var j netJSON
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, raw)
	}
	return j, raw
}

func TestNetworkMapJSONContent(t *testing.T) {
	a := netRig(t, testMapsKey)
	raw := a.adminGet(t, "/admin/network/map.json")
	var generic struct {
		Apartments []map[string]any `json:"apartments"`
	}
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatal(err)
	}
	var withGPS, without map[string]any
	for _, m := range generic.Apartments {
		if m["has_gps"] == true {
			withGPS = m
		} else {
			without = m
		}
	}
	if withGPS == nil || without == nil {
		t.Fatalf("want one apartment with and one without GPS: %s", raw)
	}
	if withGPS["name"] != "Block A" || withGPS["lat"].(float64) != -1.2864 || withGPS["lng"].(float64) != 36.8172 || withGPS["address"] != "Ngong Road, Nairobi" {
		t.Errorf("apartment: %v", withGPS)
	}
	if withGPS["maps_url"] != "https://www.google.com/maps/search/?api=1&query=-1.2864,36.8172" {
		t.Errorf("maps_url: %v", withGPS["maps_url"])
	}
	if without["lat"] != nil || without["lng"] != nil || without["maps_url"] != nil {
		t.Errorf("no-GPS apartment has a point: %v", without)
	}
	for _, want := range []string{`"houses":{"active":`, `"routers":[{`, `"olts":[{`, `"Block A OLT"`, `"pons":[1]`, `"onus":[{`, `"serial":"ZTENET01"`, `"customer":"Jane Wanjiru"`, `"account_id":"`, `"house":"A1"`,
		`"other":[{`, `"CRS326`, `"open_alerts":0`, `/admin/olts/view?id=`, `/admin/routers/view?id=`, `/admin/equipment/view?id=`} {
		if !strings.Contains(raw, want) {
			t.Errorf("map.json lacks %q:\n%s", want, raw)
		}
	}
	j, _ := getNetJSON(t, a)
	if j.WithGPS != 1 || j.NoGPS != 1 || !j.MapsKeySet {
		t.Errorf("counts: %+v", j)
	}
	for _, ap := range j.Apartments {
		if ap.Name == "Block A" {
			if len(ap.Routers) != 1 || ap.Routers[0].State != "up" || len(ap.OLTs) != 1 || ap.OLTs[0].ONUsTotal != 1 || len(ap.ONUs) != 1 || ap.ONUs[0].PON != 1 || len(ap.Other) != 1 {
				t.Errorf("equipment: %+v", ap)
			}
		}
	}
	// a stored name is data: it comes back as a JSON string, never as markup the page would run
	if !strings.Contains(raw, `No GPS \u003cb\u003eCourt\u003c/b\u003e`) {
		t.Errorf("name not JSON-escaped:\n%s", raw)
	}
}

func TestNetworkMarkerStatePausedRouterIsPartial(t *testing.T) {
	a := netRig(t, testMapsKey)
	if err := a.st.Update(func(d *store.Data) error {
		st := d.Sites[a.site]
		st.Disabled = true
		d.Sites[a.site] = st
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	j, raw := getNetJSON(t, a)
	for _, ap := range j.Apartments {
		if len(ap.Routers) != 1 || ap.Routers[0].State != "paused" || ap.State != "partial" {
			t.Fatalf("paused router: %+v\n%s", ap, raw)
		}
	}
	if h := a.adminGet(t, "/admin/network?view=list"); !strings.Contains(h, "Some offline") {
		t.Error("list view does not say some are offline")
	}
}

func TestNetworkMapJSONRoles(t *testing.T) {
	a := netRig(t, testMapsKey)
	// signed out: not served
	r, err := a.cl.Get(a.ts.URL + "/admin/network/map.json")
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode == 200 {
		t.Fatal("map.json served without a session")
	}
	r.Body.Close()
	// every signed-in role reads it, like the routers list
	for _, role := range []string{store.RoleViewer, store.RoleOperator} {
		u := seedUser(t, a.srv, a.st, "net-"+role, role, longPW)
		b := signIn(t, a.ts, a.srv, a.st, u)
		for _, p := range []string{"/admin/network/map.json", "/admin/network", "/admin/network?view=list"} {
			req, _ := http.NewRequest("GET", a.ts.URL+p, nil)
			req.AddCookie(b.Cookie)
			resp, err := a.cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body(t, resp)
			if resp.StatusCode != 200 {
				t.Errorf("%s as %s: %d", p, role, resp.StatusCode)
			}
		}
	}
}

// The Network page, and only it, gets the Google Maps policy, and only when a key is set.
func TestNetworkCSPRelaxedOnlyOnNetworkPage(t *testing.T) {
	a := netRig(t, testMapsKey)
	csp := func(p string) (string, http.Header) {
		r := a.admin("GET", p, nil)
		r.Body.Close()
		return r.Header.Get("Content-Security-Policy"), r.Header
	}
	net, h := csp("/admin/network")
	for _, want := range []string{"script-src 'self' https://maps.googleapis.com", "img-src 'self' data: blob: https://*.googleapis.com https://*.gstatic.com https://*.google.com",
		"connect-src 'self' https://maps.googleapis.com https://*.googleapis.com", "style-src 'self' 'unsafe-inline' https://fonts.googleapis.com", "font-src 'self' https://fonts.gstatic.com",
		"frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(net, want) {
			t.Errorf("network CSP lacks %q: %s", want, net)
		}
	}
	if strings.Contains(net, "script-src 'self' 'unsafe-inline'") || strings.Contains(net, "unsafe-eval") || strings.Contains(net, "'unsafe-inline' https://maps") {
		t.Errorf("network CSP allows inline script: %s", net)
	}
	if rp := h.Get("Referrer-Policy"); rp != "strict-origin-when-cross-origin" {
		t.Errorf("a referrer-restricted key needs the origin: Referrer-Policy %q", rp)
	}
	// everything else keeps the strict policy and referrer policy
	for _, p := range []string{"/admin", "/admin/network/map.json", "/admin/equipment", "/admin/apartments", "/admin/live", "/admin/map", "/admin/settings/maps", "/admin/network/", "/admin/networks"} {
		got, hh := csp(p)
		if strings.Contains(got, "google") || strings.Contains(got, "gstatic") {
			t.Errorf("%s: CSP mentions Google: %s", p, got)
		}
		if p != "/admin/network/" && p != "/admin/networks" && !strings.Contains(got, "script-src 'self';") {
			t.Errorf("%s: CSP changed: %s", p, got)
		}
		if hh.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: Referrer-Policy %q", p, hh.Get("Referrer-Policy"))
		}
	}
	// the login page and the API stay strict
	for _, p := range []string{"/admin/login", "/portal"} {
		r, _ := a.cl.Get(a.ts.URL + p)
		r.Body.Close()
		if c := r.Header.Get("Content-Security-Policy"); strings.Contains(c, "google") {
			t.Errorf("%s: %s", p, c)
		}
	}
}

func TestNetworkCSPUnchangedWithoutKey(t *testing.T) {
	a := netRig(t, "")
	r := a.admin("GET", "/admin/network", nil)
	r.Body.Close()
	c := r.Header.Get("Content-Security-Policy")
	if strings.Contains(c, "google") || !strings.Contains(c, "script-src 'self';") {
		t.Errorf("no key, but the CSP changed: %s", c)
	}
	if r.Header.Get("Referrer-Policy") != "same-origin" {
		t.Error("referrer policy changed without a key")
	}
}

func TestNetworkPageWithKeyLoadsMapFromExternalScript(t *testing.T) {
	a := netRig(t, testMapsKey)
	h := a.adminGet(t, "/admin/network")
	m := mainOf(h)
	for _, want := range []string{`id="netwrap"`, `data-json="/admin/network/map.json"`, `data-key="` + testMapsKey + `"`, `/static/netmap.js`, `id="net-nogps"`, "No location yet"} {
		if !strings.Contains(h, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(h, "maps.googleapis.com") {
		t.Error("the Maps script must be added by netmap.js, not written in the page")
	}
	if strings.Contains(m, "<script>") || strings.Contains(strings.ToLower(m), " onclick=") {
		t.Error("inline script on the Network page")
	}
	// the map script is ours and never builds HTML from data
	js, err := os.ReadFile("static/netmap.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(src, bad) {
			t.Errorf("netmap.js uses %s", bad)
		}
	}
	for _, want := range []string{"textContent", "maps.googleapis.com/maps/api/js?key=", "callback=", "importLibrary"} {
		if !strings.Contains(src, want) {
			t.Errorf("netmap.js lacks %s", want)
		}
	}
}

func TestNetworkNoKeyFallbackRendersListWithGoogleMapsLinks(t *testing.T) {
	a := netRig(t, "")
	h := a.adminGet(t, "/admin/network")
	m := mainOf(h)
	for _, want := range []string{"The map is not switched on", "Maps JavaScript API", "/admin/settings/maps",
		`href="https://www.google.com/maps/search/?api=1&amp;query=-1.2864,36.8172"`, "View on Google Maps", "Open apartment", "Block A", "Ngong Road, Nairobi",
		" active of 1", "Block A OLT", "ONUs online", "ZTENET01", "Jane Wanjiru", "CRS326"} {
		if !strings.Contains(m, want) {
			t.Errorf("fallback list lacks %q", want)
		}
	}
	if strings.Contains(m, `id="netwrap"`) || strings.Contains(h, "netmap.js") {
		t.Error("the map markup is there without a key")
	}
	if strings.Contains(h, testMapsKey) {
		t.Error("key on a page without one")
	}
}

func TestNetworkListsApartmentsWithoutGPSWithAddLink(t *testing.T) {
	a := netRig(t, testMapsKey)
	h := mainOf(a.adminGet(t, "/admin/network"))
	i := strings.Index(h, `id="net-nogps"`)
	if i < 0 {
		t.Fatal("no 'No location yet' panel")
	}
	panel := h[i:]
	if !strings.Contains(panel, "No location yet") || !strings.Contains(panel, "No GPS &lt;b&gt;Court&lt;/b&gt;") || strings.Contains(panel, "<b>Court</b>") {
		t.Errorf("name missing or not escaped: %s", panel)
	}
	if !strings.Contains(panel, "/admin/apartments/edit?id=") || !strings.Contains(panel, "Add GPS") {
		t.Errorf("no link to add GPS: %s", panel)
	}
	if strings.Contains(panel, ">Block A<") {
		t.Error("an apartment with GPS is listed as having no location")
	}
}

func TestNetworkNavSeparateFromEquipmentAndOldPagesReachable(t *testing.T) {
	a := netRig(t, "")
	nav := navOf(a.adminGet(t, "/admin/network"))
	if !strings.Contains(nav, `<a class="nav" href="/admin/network" aria-current="page">`) {
		t.Errorf("Network not highlighted: %s", nav)
	}
	if !strings.Contains(nav, `href="/admin/equipment"`) || strings.Contains(nav, `<a class="nav" href="/admin/equipment" aria-current="page">`) {
		t.Error("Equipment must stay its own, un-highlighted item")
	}
	h := a.adminGet(t, "/admin/network")
	for _, want := range []string{`href="/admin/network?view=list"`, `href="/admin/map"`, `href="/admin/live"`} {
		if !strings.Contains(h, want) {
			t.Errorf("tab or link %s missing", want)
		}
	}
	for _, p := range []string{"/admin/live", "/admin/map"} {
		a.adminGet(t, p)
	}
	if !strings.Contains(a.adminGet(t, "/admin/equipment"), "Equipment") {
		t.Error("equipment")
	}
}

func TestSettingsMapsPageAndRegistry(t *testing.T) {
	a := netRig(t, "")
	h := a.adminGet(t, "/admin/settings/maps")
	for _, want := range []string{"Maps JavaScript API", "Websites"} {
		if !strings.Contains(h, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	def, ok := settings.Lookup("maps.google_key")
	if !ok || def.Env != "GOOGLE_MAPS_API_KEY" || def.Secret {
		t.Fatalf("registry: %+v", def)
	}
	for _, bad := range []string{"not a key!", "short", strings.Repeat("A", 90), "AIza\nxxxxxxxxxxxxxxxxxxxxxxxx"} {
		if _, err := def.Norm(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if v, err := def.Norm("  " + testMapsKey + " "); err != nil || v != testMapsKey {
		t.Errorf("good key: %q %v", v, err)
	}
	if v, err := def.Norm(""); err != nil || v != "" {
		t.Errorf("empty key: %q %v", v, err)
	}
}

func TestAPINetworkMapForAndroid(t *testing.T) {
	r := newAPIRig(t)
	if err := r.st.Update(func(d *store.Data) error {
		for _, id := range d.SiteApartmentIDs(r.A.ID) {
			ap := d.Apartments[id]
			lat, lng := -1.30, 36.80
			ap.Lat, ap.Lng = &lat, &lng
			d.Apartments[id] = ap
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if res := r.call("", "GET", "/network/map", nil, nil); res.Status != 401 {
		t.Fatalf("no token: %d", res.Status)
	}
	for _, tok := range []string{r.view.Access, r.oper.Access, r.owner.Access} {
		res := r.call(tok, "GET", "/network/map", nil, nil)
		if res.Status != 200 {
			t.Fatalf("%d %s", res.Status, res.Raw)
		}
		raw := string(res.Raw)
		for _, want := range []string{`"apartments":[`, `"has_gps":true`, `"has_gps":false`, `"maps_key_set":false`, `"with_gps":1`, `"maps_url":"https://www.google.com/maps/search/?api=1\u0026query=-1.3,36.8"`} {
			if !strings.Contains(raw, want) {
				t.Errorf("API map lacks %q: %s", want, raw)
			}
		}
	}
}
