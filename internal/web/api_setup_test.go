package web_test

import (
	"strings"
	"testing"

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
		{"GET", "/routers/" + id + "/script", nil, F, F, OK},
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

func TestAPIScriptDownloadIsOwnerOnlyAndNotCached(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	x := r.call(r.owner.Access, "GET", "/routers/"+id+"/script", nil, nil)
	if x.Status != 200 || !strings.Contains(string(x.Raw), "ftn: ") || !strings.Contains(x.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("%d %.200s", x.Status, x.Raw)
	}
	if !strings.Contains(string(x.Raw), "/system/reset-configuration") && !strings.Contains(string(x.Raw), "ftn-setup.rsc") {
		t.Fatalf("not the bootstrap script: %.200s", x.Raw)
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
