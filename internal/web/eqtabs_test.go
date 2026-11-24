package web_test

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// eqRig is the OLT story plus one ONU on PON 1, one switch and one splitter.
func eqRig(t *testing.T) *app {
	t.Helper()
	a := newONUApp(t)
	a.makeOLT(t, nil)
	oid := a.oltID(t)
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEGTAB1"}, "mac": {tONUMAC}, "model": {"F601"}, "placement": {"1"}, "olt": {oid}, "pon_port": {"1"}})
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"switch"}, "serial": {"SWTAB001"}, "mac": {"AA:BB:CC:00:5E:01"}, "model": {"CRS326"}})
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"splitter"}, "serial": {"SPTAB001"}, "model": {"1x8"}})
	return a
}

// mainOf is the page body: the add-customer dialog in the footer lists the ONU stock on every page.
func mainOf(h string) string {
	i := strings.Index(h, "<main")
	j := strings.Index(h, "</main>")
	if i < 0 || j < i {
		return h
	}
	return h[i:j]
}

func navOf(h string) string {
	i := strings.Index(h, `aria-label="Main"`)
	j := strings.Index(h, "</aside>")
	if i < 0 || j < i {
		return ""
	}
	return h[i:j]
}

func TestNavShowsEquipmentNotRouters(t *testing.T) {
	a := eqRig(t)
	for _, p := range []string{"/admin", "/admin/equipment", "/admin/routers", "/admin/equipment?tab=olt", "/admin/onus"} {
		nav := navOf(a.adminGet(t, p))
		if nav == "" {
			t.Fatalf("%s: no nav", p)
		}
		if !strings.Contains(nav, `href="/admin/equipment"`) {
			t.Errorf("%s: nav lacks Equipment", p)
		}
		for _, gone := range []string{`href="/admin/routers"`, `href="/admin/onus"`, ">Routers</a>", ">ONUs</a>"} {
			if strings.Contains(nav, gone) {
				t.Errorf("%s: nav still has %q", p, gone)
			}
		}
	}
	// Equipment is the highlighted item on every page under it.
	cur := regexp.MustCompile(`<a class="nav" href="/admin/equipment" aria-current="page">`)
	for _, p := range []string{"/admin/equipment", "/admin/equipment?tab=other", "/admin/routers", "/admin/routers/view?id=" + a.site, "/admin/onus", "/admin/olts/view?id=" + a.oltID(t)} {
		if !cur.MatchString(a.adminGet(t, p)) {
			t.Errorf("%s: Equipment is not the highlighted nav item", p)
		}
	}
	if strings.Contains(a.adminGet(t, "/admin"), `href="/admin/equipment" aria-current`) {
		t.Error("Equipment highlighted on the overview")
	}
}

func TestEquipmentTabsRenderTheirLists(t *testing.T) {
	a := eqRig(t)
	h := a.adminGet(t, "/admin/equipment")
	for _, want := range []string{`aria-label="Equipment"`, "tab=routers", "tab=olt", "tab=onu", "tab=other", "tab=tree", "Add router", "Apartments", "OLTs", "Last seen"} {
		if !strings.Contains(h, want) {
			t.Errorf("default tab lacks %q", want)
		}
	}
	if !regexp.MustCompile(`href="/admin/equipment\?tab=routers" aria-current="page"`).MatchString(h) {
		t.Error("Routers is not the default tab")
	}
	if !strings.Contains(h, "/admin/routers/view?id="+a.site) {
		t.Error("router row does not open the router page")
	}
	olt := mainOf(a.adminGet(t, "/admin/equipment?tab=olt"))
	for _, want := range []string{"Block A OLT", "V1600GS-R", tOLTMAC, "ether3", "4 ports", "Block A", "0 / 1", "Add OLT", "/admin/olts/new"} {
		if !strings.Contains(olt, want) {
			t.Errorf("OLT tab lacks %q", want)
		}
	}
	if strings.Contains(olt, "ZTEGTAB1") || strings.Contains(olt, "SWTAB001") || strings.Contains(olt, "Olt-Secret-99") {
		t.Error("OLT tab shows foreign items or the password")
	}
	onu := mainOf(a.adminGet(t, "/admin/equipment?tab=onu"))
	for _, want := range []string{"ZTEGTAB1", tONUMAC, "F601", "Block A OLT", "PON 1", "TR-069 last check-in", "Add ONU", "Import a list (CSV)", `name="router"`, `name="olt"`, `name="status"`, `name="q"`, "in stock", "deployed"} {
		if !strings.Contains(onu, want) {
			t.Errorf("ONU tab lacks %q", want)
		}
	}
	if strings.Contains(onu, "SWTAB001") || strings.Contains(onu, "VS0001") {
		t.Error("ONU tab shows other kinds")
	}
	oth := mainOf(a.adminGet(t, "/admin/equipment?tab=other"))
	for _, want := range []string{"SWTAB001", "SPTAB001", "CRS326", "Add item"} {
		if !strings.Contains(oth, want) {
			t.Errorf("Other tab lacks %q", want)
		}
	}
	tree := a.adminGet(t, "/admin/equipment?tab=tree")
	if !strings.Contains(tree, "Apartments served") || !strings.Contains(tree, "Block A OLT") {
		t.Error("tree view lost")
	}
}

func TestEquipmentOtherExcludesOLTAndONU(t *testing.T) {
	a := eqRig(t)
	oth := mainOf(a.adminGet(t, "/admin/equipment?tab=other"))
	for _, no := range []string{"ZTEGTAB1", "VS0001", "Block A OLT", tOLTMAC, tONUMAC, ">ont<", ">olt<"} {
		if strings.Contains(oth, no) {
			t.Errorf("Other tab lists %q", no)
		}
	}
	// the kind filter of Other cannot be turned into olt or ont
	if h := mainOf(a.adminGet(t, "/admin/equipment?tab=other&kind=ont")); strings.Contains(h, "ZTEGTAB1") {
		t.Error("kind=ont leaked into Other")
	}
	// the add form of Other offers no OLT or ONT
	i := strings.Index(oth, `<select name="kind">`)
	if i < 0 {
		t.Fatal("no kind select")
	}
	sel := oth[i : i+strings.Index(oth[i:], "</select>")]
	if strings.Contains(sel, "olt") || strings.Contains(sel, "ont") || !strings.Contains(sel, "switch") {
		t.Errorf("Other kind list: %s", sel)
	}
}

func TestEquipmentTabCounts(t *testing.T) {
	a := eqRig(t)
	h := a.adminGet(t, "/admin/equipment")
	nav := h[strings.Index(h, `<nav class="tabs" aria-label="Equipment">`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	count := func(tab string) string {
		m := regexp.MustCompile(`tab=` + tab + `"[^>]*>[^<]*<span class="n">(\d+)</span>`).FindStringSubmatch(nav)
		if m == nil {
			t.Fatalf("no count on tab %s in %s", tab, nav)
		}
		return m[1]
	}
	for tab, want := range map[string]string{"routers": "1", "olt": "1", "onu": "1", "other": "2"} {
		if got := count(tab); got != want {
			t.Errorf("tab %s count = %s, want %s", tab, got, want)
		}
	}
	if regexp.MustCompile(`tab=tree"[^>]*>[^<]*<span`).MatchString(nav) {
		t.Error("the tree view has a count")
	}
	// one more switch moves only Other
	a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"switch"}, "serial": {"SWTAB002"}})
	h = a.adminGet(t, "/admin/equipment?tab=olt")
	if !regexp.MustCompile(`tab=other"[^>]*>Other<span class="n">3</span>`).MatchString(h) {
		t.Error("Other did not count the new switch")
	}
}

func TestOldRouterAndEquipmentURLsStillWork(t *testing.T) {
	a := eqRig(t)
	for _, p := range []string{"/admin/routers", "/admin/routers?tab=online", "/admin/routers?tab=offline", "/admin/routers?add=1", "/admin/equipment?view=list", "/admin/equipment?kind=ont", "/admin/equipment?status=in_stock"} {
		h := a.adminGet(t, p)
		if !strings.Contains(h, `aria-label="Equipment"`) {
			t.Errorf("%s does not render under Equipment", p)
		}
	}
	h := a.adminGet(t, "/admin/routers")
	if !regexp.MustCompile(`tab=routers" aria-current="page"`).MatchString(h) || !strings.Contains(h, "Block A") {
		t.Error("/admin/routers: Routers tab not highlighted or router missing")
	}
	// the old list lands on the tab of its kind
	if h := a.adminGet(t, "/admin/equipment?kind=ont"); !strings.Contains(h, "ZTEGTAB1") || !regexp.MustCompile(`tab=onu" aria-current="page"`).MatchString(h) {
		t.Error("?kind=ont is not the ONU tab")
	}
	if h := a.adminGet(t, "/admin/equipment?kind=switch"); !strings.Contains(h, "SWTAB001") || strings.Contains(h, "SPTAB001") {
		t.Error("?kind=switch")
	}
	if r := a.admin("GET", "/admin/fleet", nil); r.StatusCode != 302 || r.Header.Get("Location") != "/admin/routers" {
		t.Errorf("/admin/fleet: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := a.admin("GET", "/admin/sites", nil); r.StatusCode != 302 {
		t.Errorf("/admin/sites: %d", r.StatusCode)
	}
}

func TestRouterPageBreadcrumbIsEquipmentRouters(t *testing.T) {
	a := eqRig(t)
	h := a.adminGet(t, "/admin/routers/view?id="+a.site)
	i := strings.Index(h, `aria-label="Breadcrumb"`)
	crumb := h[i : i+strings.Index(h[i:], "</nav>")]
	for _, want := range []string{"<span>Equipment</span>", `href="/admin/equipment?tab=routers">Routers</a>`, `aria-current="page">Block A`} {
		if !strings.Contains(crumb, want) {
			t.Errorf("breadcrumb lacks %q: %s", want, crumb)
		}
	}
	for _, p := range []string{"/admin/olts/view?id=" + a.oltID(t), "/admin/olts/new"} {
		c := a.adminGet(t, p)
		j := strings.Index(c, `aria-label="Breadcrumb"`)
		cr := c[j : j+strings.Index(c[j:], "</nav>")]
		if !strings.Contains(cr, "<span>Equipment</span>") || !strings.Contains(cr, `href="/admin/equipment?tab=olt">OLT</a>`) {
			t.Errorf("%s breadcrumb: %s", p, cr)
		}
	}
}

func TestEquipmentONUFilters(t *testing.T) {
	a := eqRig(t)
	if h := a.adminGet(t, "/admin/equipment?tab=onu&q=ztegtab1"); !strings.Contains(h, "ZTEGTAB1") {
		t.Error("search by serial")
	}
	if h := a.adminGet(t, "/admin/equipment?tab=onu&q=nomatch"); strings.Contains(h, "ZTEGTAB1</b>") || !strings.Contains(h, "No ONU matches") {
		t.Error("search with no match")
	}
	if h := a.adminGet(t, "/admin/equipment?tab=onu&olt="+a.oltID(t)); !strings.Contains(h, "ZTEGTAB1</b>") {
		t.Error("OLT filter")
	}
	if h := a.adminGet(t, "/admin/equipment?tab=onu&router=nosuch"); strings.Contains(h, "ZTEGTAB1</b>") {
		t.Error("router filter")
	}
	if h := a.adminGet(t, "/admin/equipment?tab=onu&status=faulty"); strings.Contains(h, "ZTEGTAB1</b>") {
		t.Error("status filter")
	}
	// adding an ONU from its tab lands back on it, and errors too
	loc := a.adminPost(t, "/admin/equipment/save", url.Values{"kind": {"ont"}, "serial": {"ZTEGTAB1"}, "next": {"/admin/equipment?tab=onu"}})
	if !strings.Contains(loc, "/admin/equipment?tab=onu") || !strings.Contains(loc, "Error") {
		t.Errorf("duplicate serial: %s", loc)
	}
}
