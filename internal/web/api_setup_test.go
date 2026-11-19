package web_test

import (
	"strings"
	"testing"
	"time"

	"ispbill/internal/store"
)

func TestAPISetupRoles(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	type tc struct {
		method, path string
		body         any
		viewer, oper int
		owner        int
	}
	const F, OK = 403, 0
	for _, c := range []tc{
		{"GET", "/routers/" + id + "/setup", nil, OK, OK, OK},
		{"POST", "/routers", map[string]any{}, F, F, OK},
		{"PATCH", "/routers/" + id, map[string]any{}, F, F, OK},
		{"GET", "/routers/" + id + "/checks", nil, OK, OK, OK},
		{"POST", "/routers/" + id + "/join-command", nil, F, F, OK},
		{"POST", "/routers/" + id + "/actions/reset", map[string]any{"confirm_name": "nope"}, F, F, OK},
		{"POST", "/routers/" + id + "/script", map[string]any{"confirm_name": "nope"}, F, F, OK},
	} {
		for who, want := range map[string]int{r.view.Access: c.viewer, r.oper.Access: c.oper, r.owner.Access: c.owner} {
			got := r.call(who, c.method, c.path, c.body, nil)
			role := map[string]string{r.view.Access: "viewer", r.oper.Access: "operator", r.owner.Access: "owner"}[who]
			if (want == F) != (got.Status == 403) {
				t.Errorf("%s %s %s: got %d %s", role, c.method, c.path, got.Status, got.Raw)
			}
		}
	}
}

func TestAPICreateRouterReturnsTheJoinLineAndIsNeverReplayed(t *testing.T) {
	r := newAPIRig(t)
	h := map[string]string{"Idempotency-Key": "router-create-1"}
	body := map[string]any{"name": "Gamma", "serving_customers": true, "customer_subnet": "10.7.50.0/24"}
	a := r.call(r.owner.Access, "POST", "/routers", body, h)
	if a.Status != 201 {
		t.Fatalf("%d %s", a.Status, a.Raw)
	}
	if cc := a.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control %q", cc)
	}
	j := a.JSON(t)
	rt := j["router"].(map[string]any)
	join := j["join"].(map[string]any)
	if rt["name"] != "Gamma" || rt["id"] == "" {
		t.Fatalf("%v", rt)
	}
	for _, k := range []string{"command", "command_nocheck", "expires_at"} {
		if str(join, k) == "" {
			t.Fatalf("join lacks %s: %v", k, join)
		}
	}
	if !strings.Contains(str(join, "command"), "https://billing.example.co.ke") || strings.Contains(str(join, "command_nocheck"), "check-certificate=yes") {
		t.Fatalf("%v", join)
	}
	// the same key again makes a second router with a fresh token: a one-time secret is never replayed
	body["name"] = "Gamma 2"
	b := r.call(r.owner.Access, "POST", "/routers", body, h)
	if b.Status != 201 || b.Header.Get("Idempotent-Replayed") != "" || str(b.JSON(t)["join"].(map[string]any), "command") == str(join, "command") {
		t.Fatalf("the second call was replayed: %d %s", b.Status, b.Raw)
	}
	if x := r.call(r.owner.Access, "POST", "/routers", map[string]any{"name": " "}, nil); x.Status != 422 && x.Status != 400 {
		t.Fatalf("empty name: %d %s", x.Status, x.Raw)
	}
}

func TestAPISetupViewAndGuards(t *testing.T) {
	r := newAPIRig(t)
	c := r.call(r.owner.Access, "POST", "/routers", map[string]any{"name": "Delta"}, nil)
	id := str(c.JSON(t)["router"].(map[string]any), "id")
	v := r.call(r.view.Access, "GET", "/routers/"+id+"/setup", nil, nil)
	if v.Status != 200 {
		t.Fatalf("%d %s", v.Status, v.Raw)
	}
	j := v.JSON(t)
	for _, k := range []string{"router_id", "state", "join", "next_action", "steps", "checks", "blockers", "last_error"} {
		if _, ok := j[k]; !ok {
			t.Errorf("setup lacks %q: %s", k, v.Raw)
		}
	}
	if j["state"] != "not_started" || j["join"].(map[string]any)["state"] != "waiting" || j["next_action"].(map[string]any)["kind"] != "paste_line" {
		t.Fatalf("a new router waits for its line: %s", v.Raw)
	}
}

func TestAPIRemovedEndpointsAnswer410(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	for _, c := range [][2]string{
		{"POST", "/routers/" + id + "/setup/run"}, {"POST", "/routers/" + id + "/setup/migrate"}, {"POST", "/routers/" + id + "/setup/rollback"},
		{"PUT", "/sites/" + id + "/mode"}, {"GET", "/sites/" + id + "/cutover"}, {"POST", "/sites/" + id + "/cutover/start"},
		{"POST", "/sites/" + id + "/cutover/confirm"}, {"POST", "/sites/" + id + "/cutover/rollback"},
	} {
		x := r.call(r.owner.Access, c[0], c[1], map[string]any{}, nil)
		if x.Status != 410 || x.errCode(t) != "gone" {
			t.Errorf("%s %s: %d %s", c[0], c[1], x.Status, x.Raw)
		}
	}
}

func TestAPICreateAndPatchTheLayout(t *testing.T) {
	r := newAPIRig(t)
	c := r.call(r.owner.Access, "POST", "/routers", map[string]any{"name": "Layout", "wan_mode": "static", "wan_address": "198.51.100.5/24", "wan_gateway": "198.51.100.1",
		"lan_ports": "ether2,ether3", "lan_subnet": "192.168.77.0/24", "vlan_id": 100, "bench_port": "ether5"}, nil)
	if c.Status != 201 {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
	id := str(c.JSON(t)["router"].(map[string]any), "id")
	var st store.Site
	r.st.View(func(d *store.Data) { st = d.Sites[id] })
	if st.WANMode != "static" || st.LANPorts != "ether2,ether3" || st.LANSubnet != "192.168.77.0/24" || st.VLANID != 100 || st.AccessMode != store.AccessIPoE {
		t.Fatalf("%+v", st)
	}
	// field errors name the field
	for name, body := range map[string]map[string]any{
		"static without address": {"wan_mode": "static"},
		"lan overlaps customers": {"lan_subnet": "10.5.50.0/24"},
		"unknown wan mode":       {"wan_mode": "carrier-pigeon"},
		"lan port is the wan":    {"lan_ports": "ether1"},
	} {
		body["name"] = "Bad " + name
		x := r.call(r.owner.Access, "POST", "/routers", body, nil)
		if x.Status != 400 {
			t.Errorf("%s: %d %s", name, x.Status, x.Raw)
		}
	}
	// a PPPoE password needs DATA_KEY on the server; it is never echoed
	p := r.call(r.owner.Access, "PATCH", "/routers/"+id, map[string]any{"wan_mode": "dhcp", "support_phone": "0700000000"}, nil)
	if p.Status != 200 || strings.Contains(string(p.Raw), "pppoe_pass\":") {
		t.Fatalf("%d %s", p.Status, p.Raw)
	}
}

func TestAPIResetNeedsTheNameAndGivesANewLine(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	if x := r.call(r.owner.Access, "POST", "/routers/"+id+"/actions/reset", map[string]any{}, nil); x.Status != 400 {
		t.Fatalf("no name: %d %s", x.Status, x.Raw)
	}
	if x := r.call(r.owner.Access, "POST", "/routers/"+id+"/actions/reset", map[string]any{"confirm_name": "Beta"}, nil); x.Status != 400 {
		t.Fatalf("other name: %d %s", x.Status, x.Raw)
	}
	x := r.call(r.owner.Access, "POST", "/routers/"+id+"/actions/reset", map[string]any{"confirm_name": "Alpha"}, nil)
	if x.Status != 200 || !strings.Contains(string(x.Raw), "/import ftn-reset.rsc") || !strings.Contains(x.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	var st store.Site
	r.st.View(func(d *store.Data) { st = d.Sites[id] })
	if st.WGPublicKey != "" || !st.JoinedAt.IsZero() {
		t.Fatalf("not reset: %+v", st)
	}
}

func TestAPIScriptDownloadIsPostOwnerOnlyConfirmedAndNotCached(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	// the old GET is gone: a download changes keys, so it is never a plain link
	if x := r.call(r.owner.Access, "GET", "/routers/"+id+"/script", nil, nil); x.Status == 200 {
		t.Fatalf("GET still serves the script: %d", x.Status)
	}
	// make the router a not-yet-joined one first
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[id]
		s.JoinedAt = time.Time{}
		d.Sites[id] = s
		return nil
	})
	for _, bad := range []any{nil, map[string]any{}, map[string]any{"confirm_name": "nope"}} {
		x := r.call(r.owner.Access, "POST", "/routers/"+id+"/script", bad, nil)
		if x.Status != 422 && x.Status != 400 {
			t.Fatalf("without the router name: %d %.200s", x.Status, x.Raw)
		}
	}
	x := r.call(r.owner.Access, "POST", "/routers/"+id+"/script", map[string]any{"confirm_name": r.A.Name}, nil)
	if x.Status != 200 || !strings.Contains(string(x.Raw), "ftn: ") || !strings.Contains(x.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("%d %.200s", x.Status, x.Raw)
	}
	if !strings.Contains(string(x.Raw), "/system/reset-configuration") && !strings.Contains(string(x.Raw), "ftn-setup.rsc") {
		t.Fatalf("not the bootstrap script: %.200s", x.Raw)
	}
}

// A router that is joined with its tunnel up keeps its keys: the download is refused with 409 unless force is set.
func TestAPIScriptDownloadRefusesALiveRouterUnlessForced(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	var before store.Site
	r.st.Update(func(d *store.Data) error {
		s := d.Sites[id]
		s.JoinedAt = time.Now()
		d.Sites[id] = s
		before = s
		return nil
	})
	body := map[string]any{"confirm_name": r.A.Name}
	x := r.call(r.owner.Access, "POST", "/routers/"+id+"/script", body, nil)
	if x.Status != 409 {
		t.Fatalf("live router: %d %.200s", x.Status, x.Raw)
	}
	var after store.Site
	r.st.View(func(d *store.Data) { after = d.Sites[id] })
	if after.WGPublicKey != before.WGPublicKey || after.RESTPass != before.RESTPass {
		t.Fatal("a refused download still rotated the router's keys")
	}
	body["force"] = true
	x = r.call(r.owner.Access, "POST", "/routers/"+id+"/script", body, nil)
	if x.Status != 200 || !strings.Contains(string(x.Raw), "ftn-setup.rsc") {
		t.Fatalf("forced: %d %.200s", x.Status, x.Raw)
	}
	// the operator never gets it, forced or not
	if x := r.call(r.oper.Access, "POST", "/routers/"+id+"/script", body, nil); x.Status != 403 {
		t.Fatalf("operator: %d", x.Status)
	}
}

func TestSessionSetupJSON(t *testing.T) {
	r := newAPIRig(t)
	resp := r.admin("GET", "/admin/routers/setup.json?id="+r.A.ID, nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "json") || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header.Get("Location"))
	}
	if b := body(t, resp); !strings.Contains(b, `"router_id":"`+r.A.ID+`"`) {
		t.Fatalf("%s", b)
	}
	if g := r.admin("GET", "/admin/routers/setup.json?id=nope", nil); g.StatusCode != 404 {
		t.Fatalf("%d", g.StatusCode)
	}
}

func TestAPIPortFieldsAreValidatedAndReportedPerField(t *testing.T) {
	r := newAPIRig(t)
	c := r.call(r.owner.Access, "POST", "/routers", map[string]any{"name": "Ports", "wan_interface": "ether1", "lan_ports": "ether2", "olt_interface": "ether3", "bench_port": "ether5"}, nil)
	if c.Status != 201 {
		t.Fatalf("%d %s", c.Status, c.Raw)
	}
	rt := c.JSON(t)["router"].(map[string]any)
	id := str(rt, "id")
	if rt["bench_port"] != "ether5" || rt["olt_interface"] != "ether3" || rt["olt_cap_mbps"].(float64) != 1000 {
		t.Fatalf("%v", rt)
	}
	cp, ok := rt["customer_port"].(map[string]any)
	if !ok || cp["state"] == nil {
		t.Fatalf("no customer_port status: %v", rt)
	}
	for name, tc := range map[string]struct {
		body  map[string]any
		field string
	}{
		"bench is the WAN":  {map[string]any{"bench_port": "ether1"}, "bench_port"},
		"bench is a LAN":    {map[string]any{"bench_port": "ether2"}, "bench_port"},
		"bench is the OLT":  {map[string]any{"bench_port": "ether3"}, "bench_port"},
		"olt is the WAN":    {map[string]any{"olt_interface": "ether1"}, "olt_interface"},
		"olt is a LAN port": {map[string]any{"olt_interface": "ether2"}, "olt_interface"},
		"olt is the bench":  {map[string]any{"olt_interface": "ether5"}, "olt_interface"},
	} {
		x := r.call(r.owner.Access, "PATCH", "/routers/"+id, tc.body, nil)
		if x.Status != 400 && x.Status != 422 {
			t.Errorf("%s: %d %s", name, x.Status, x.Raw)
			continue
		}
		f, _ := x.JSON(t)["error"].(map[string]any)["details"].(map[string]any)["fields"].(map[string]any)
		if f[tc.field] == nil {
			t.Errorf("%s: no fields.%s in %s", name, tc.field, x.Raw)
		}
	}
	var st store.Site
	r.st.View(func(d *store.Data) { st = d.Sites[id] })
	if st.CustomerPorts != "ether5" || st.OLTInterface != "ether3" {
		t.Fatalf("a refused save changed the router: %+v", st)
	}
}
