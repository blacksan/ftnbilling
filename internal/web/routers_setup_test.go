package web_test

import (
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"ispbill/internal/store"
)

// The router form carries the WAN mode and its fields, and the PPPoE password
// is write-only: the page shows a "set" state and never the password.
func TestRouterFormWANFieldsAndWriteOnlyPPPoEPassword(t *testing.T) {
	m, site, _ := uiRig(t)
	const secret = "sekret-pppoe-pass-77"
	form := url.Values{"id": {site}, "name": {"Alpha Court"}, "wanmode": {"pppoe"}, "wan": {"ether1"}, "pppoeuser": {"isp-user"}, "pppoepass": {secret},
		"lanports": {"ether2"}, "lansubnet": {"192.168.88.0/24"}, "olt": {"sfp-sfpplus1"}, "vlan": {"100"}, "subnet": {"10.5.50.0/24"}, "bench": {"ether5"}}
	// the VLAN changes on a router that is set up: refused until the operator confirms
	if loc := m.adminPost(t, "/admin/routers/update", form); !strings.Contains(loc, "Customers on the OLT lose service") {
		t.Fatalf("an unconfirmed VLAN change was accepted: %s", loc)
	}
	form.Set("confirm", "1")
	if loc := m.adminPost(t, "/admin/routers/update", form); !strings.Contains(loc, "Saved") {
		t.Fatal(loc)
	}
	var st store.Site
	m.st.View(func(d *store.Data) { st = d.Sites[site] })
	if st.WANMode != "pppoe" || st.PPPoEUser != "isp-user" || st.PPPoEPassEnc == "" || st.LANPorts != "ether2" || st.VLANID != 100 || st.BenchPort != "ether5" {
		t.Fatalf("not saved: %+v", st)
	}
	h := m.adminGet(t, "/admin/routers/view?id="+site+"&tab=profile")
	for _, want := range []string{`name="wanmode"`, `name="wan"`, `name="wanaddr"`, `name="wangw"`, `name="wandns"`, `name="pppoeuser"`, `name="pppoepass"`,
		`name="lanports"`, `name="lansubnet"`, `name="olt"`, `name="vlan"`, `name="subnet"`, `name="bench"`, "data-pppoe-set", `value="isp-user"`} {
		if !strings.Contains(h, want) {
			t.Errorf("router form lacks %q", want)
		}
	}
	if strings.Contains(h, secret) || strings.Contains(h, st.PPPoEPassEnc) {
		t.Fatal("the PPPoE password (or its sealed form) is rendered")
	}
	// The Add router dialog has the same fields.
	list := m.adminGet(t, "/admin/routers")
	for _, want := range []string{`name="wanmode"`, `name="lanports"`, `name="bench"`, `name="olt"`} {
		if !strings.Contains(list, want) {
			t.Errorf("Add router lacks %q", want)
		}
	}
	if strings.Contains(list, `name="serving"`) {
		t.Error("the migration question is still in Add router")
	}
}

// The setup tab shows the four derived steps, the one bootstrap line with its
// copy button, and an owner-only reset that needs the typed name.
func TestRouterSetupTabStepsLineAndReset(t *testing.T) {
	m, site, _ := uiRig(t)
	h := m.adminGet(t, "/admin/routers/view?id="+site+"&tab=setup")
	for _, key := range []string{"script_fetched", "tunnel_up", "server_login", "checks_green"} {
		if !strings.Contains(h, `data-step-key="`+key+`"`) {
			t.Errorf("setup tab lacks the step %s", key)
		}
	}
	for _, want := range []string{"data-setup-view", "data-reset-router", `name="confirm_name"`} {
		if !strings.Contains(h, want) {
			t.Errorf("setup tab lacks %q", want)
		}
	}
	// Making the line shows it once with a copy button; it is the only command.
	resp := m.admin("POST", "/admin/routers/join", url.Values{"id": {site}})
	page := body(t, resp)
	if !strings.Contains(page, `data-copy-from="#joincmd"`) || !regexp.MustCompile(`/j/[A-Za-z0-9_-]{43}`).MatchString(page) {
		t.Fatalf("the bootstrap line and its copy button are missing:\n%s", page)
	}
	if strings.Count(page, `data-copy-from="#`) != 2 || strings.Contains(page, "data-copy=") {
		t.Error("the page offers a router command besides the bootstrap line (and its no-certificate-check variant)")
	}
	// Reset: a wrong name is refused, the right one forgets the router's keys.
	code, j := m.postJSON(t, "/admin/routers/setup/reset", url.Values{"id": {site}, "confirm_name": {"nope"}})
	if code != 422 || j["error"] == nil {
		t.Fatalf("wrong name: %d %v", code, j)
	}
	var before store.Site
	m.st.View(func(d *store.Data) { before = d.Sites[site] })
	if before.WGPublicKey == "" {
		t.Fatal("a wrong name must change nothing")
	}
	code, j = m.postJSON(t, "/admin/routers/setup/reset", url.Values{"id": {site}, "confirm_name": {"Alpha Court"}})
	if code != 200 || j["state"] != "not_started" {
		t.Fatalf("reset: %d %v", code, j)
	}
	var after store.Site
	m.st.View(func(d *store.Data) { after = d.Sites[site] })
	if after.WGPublicKey != "" || after.RESTPass != "" || !after.JoinedAt.IsZero() {
		t.Fatalf("reset left keys behind: %+v", after)
	}
	// The list row shows the setup view's state.
	if l := m.adminGet(t, "/admin/routers"); !strings.Contains(l, "Waiting for the line") {
		t.Error("the router list does not show the setup state")
	}
}

// No template or script refers to an endpoint v5 removed, and nothing renders a
// router command to copy by hand.
func TestNoRemovedEndpointsInUI(t *testing.T) {
	js, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	removed := regexp.MustCompile(`(?i)setup/(run|migrate|rollback)|/routers/(confirm|cutover|adopt|mode)|/sites/(confirm|cutover|adopt|mode)|hotspot|Roll back now|Start migration|Run set-up|Copy command`)
	if m := removed.FindString(string(js)); m != "" {
		t.Errorf("app.js still has %q", m)
	}
	ents, _ := os.ReadDir("templates")
	for _, e := range ents {
		f, _ := os.Open("templates/" + e.Name())
		b, _ := io.ReadAll(f)
		f.Close()
		if m := removed.FindString(string(b)); m != "" {
			t.Errorf("%s still has %q", e.Name(), m)
		}
	}
}

// The owner can pause the server's automatic fixes of a router from the Setup tab; it is shown and recorded.
func TestRouterPageAutoFixPause(t *testing.T) {
	m, site, _ := uiRig(t)
	loc := m.adminPost(t, "/admin/routers/auto-fixes", url.Values{"id": {site}, "paused": {"1"}})
	if !strings.Contains(loc, "paused") {
		t.Fatal(loc)
	}
	h := m.adminGet(t, "/admin/routers/view?id="+site+"&tab=setup")
	if !strings.Contains(h, "data-autofix-state") || !strings.Contains(h, "Resume automatic fixes") {
		t.Fatal("the paused state is not on the page")
	}
	var found bool
	m.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == site && strings.Contains(e.Msg, "automatic fixes paused") && e.Actor != "" {
				found = true
			}
		}
	})
	if !found {
		t.Fatal("not recorded in the router's activity")
	}
	if loc := m.adminPost(t, "/admin/routers/auto-fixes", url.Values{"id": {site}, "paused": {"0"}}); !strings.Contains(loc, "resumed") {
		t.Fatal(loc)
	}
	if h := m.adminGet(t, "/admin/routers/view?id="+site+"&tab=setup"); !strings.Contains(h, "Pause automatic fixes") {
		t.Fatal("not resumed")
	}
}
