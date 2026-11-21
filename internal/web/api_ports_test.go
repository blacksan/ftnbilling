package web_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func portsRig(t *testing.T) (*apiRig, *routeros.Mock, string) {
	t.Helper()
	r := newAPIRig(t)
	mk := r.mock(r.A.ID)
	for _, n := range []string{"ether1", "ether2", "ether3", "ether4", "ether5", "sfp-sfpplus1"} {
		mk.SeedRow("/interface", routeros.Row{"name": n, "type": "ether", "running": "true", "disabled": "false", "mac-address": "78:9A:18:00:00:01"})
	}
	mk.SeedRow("/interface", routeros.Row{"name": "br-customers", "type": "bridge"})
	mk.SeedRow("/interface/bridge", routeros.Row{"name": "br-lan", "comment": "ftn: admin LAN bridge"})
	mk.SeedRow("/interface/bridge", routeros.Row{"name": "br-customers", "comment": "ftn: customer bridge"})
	mk.SeedRow("/interface/bridge/port", routeros.Row{"bridge": "br-lan", "interface": "ether2", "comment": "ftn: LAN port"})
	mk.SeedRow("/interface/bridge/port", routeros.Row{"bridge": "br-customers", "interface": "sfp-sfpplus1", "horizon": "1", "comment": "ftn: customer port"})
	mk.ResetWrites()
	return r, mk, "/routers/" + r.A.ID + "/ports"
}

func portOf(t *testing.T, res reply, name string) map[string]any {
	t.Helper()
	for _, it := range res.JSON(t)["items"].([]any) {
		if p := it.(map[string]any); p["name"] == name {
			return p
		}
	}
	t.Fatalf("no port %s in %s", name, res.Raw)
	return nil
}

func TestAPIPortsListAndRoles(t *testing.T) {
	r, _, path := portsRig(t)
	if res := r.call(r.view.Access, "GET", path, nil, nil); res.Status != 200 {
		t.Fatalf("%d %s", res.Status, res.Raw)
	}
	res := r.call(r.oper.Access, "GET", path, nil, nil)
	want := map[string]string{"ether1": "wan", "ether2": "lan", "sfp-sfpplus1": "olt", "ether3": "unused"}
	for n, role := range want {
		if p := portOf(t, res, n); p["role"] != role {
			t.Errorf("%s: %v", n, p)
		}
	}
	if w := portOf(t, res, "ether1"); w["locked"] != true || w["can_change"] != false {
		t.Fatalf("the WAN row: %v", w)
	}
	if res.JSON(t)["roles"] == nil {
		t.Fatal("no role labels")
	}
}

func TestAPIPortChangeRules(t *testing.T) {
	r, mk, path := portsRig(t)
	patch := func(tok, port string, body map[string]any) reply {
		return r.call(tok, "PATCH", path+"/"+port, body, nil)
	}
	code := func(res reply) string {
		e, _ := res.JSON(t)["error"].(map[string]any)
		s, _ := e["code"].(string)
		return s
	}
	// the viewer may not
	if res := patch(r.view.Access, "ether3", map[string]any{"disabled": true}); res.Status != 403 {
		t.Fatalf("viewer: %d %s", res.Status, res.Raw)
	}
	// the WAN is refused for everybody, and nothing is written
	for _, tok := range []string{r.oper.Access, r.owner.Access} {
		res := patch(tok, "ether1", map[string]any{"disabled": true})
		if res.Status != 409 || code(res) != "port_locked" {
			t.Fatalf("WAN: %d %s", res.Status, res.Raw)
		}
	}
	if res := patch(r.owner.Access, "ether3", map[string]any{"role": "wan"}); res.Status != 409 || code(res) != "port_locked" {
		t.Fatalf("wan role: %d %s", res.Status, res.Raw)
	}
	if mk.Writes() != 0 {
		t.Fatalf("the router was written: %d", mk.Writes())
	}
	// the last admin LAN port
	if res := patch(r.owner.Access, "ether2", map[string]any{"role": "unused", "confirm_name": "Alpha"}); res.Status != 409 || code(res) != "last_lan_port" {
		t.Fatalf("last LAN: %d %s", res.Status, res.Raw)
	}
	// operator: customer and unused only
	if res := patch(r.oper.Access, "ether3", map[string]any{"role": "customer"}); res.Status != 200 {
		t.Fatalf("operator customer: %d %s", res.Status, res.Raw)
	}
	for name, body := range map[string]map[string]any{"lan": {"role": "lan"}, "olt": {"role": "olt", "confirm_name": "Alpha"}} {
		if res := patch(r.oper.Access, "ether4", body); res.Status != 403 {
			t.Errorf("operator %s: %d %s", name, res.Status, res.Raw)
		}
	}
	if res := patch(r.oper.Access, "sfp-sfpplus1", map[string]any{"disabled": true, "confirm_name": "Alpha"}); res.Status != 403 {
		t.Fatalf("operator disables the OLT: %d %s", res.Status, res.Raw)
	}
	// the OLT uplink is never switched off from here, with or without the name
	for _, body := range []map[string]any{{"disabled": true}, {"disabled": true, "confirm_name": "Alpha"}} {
		if res := patch(r.owner.Access, "sfp-sfpplus1", body); res.Status != 409 || code(res) != "olt_disable_refused" || !strings.Contains(string(res.Raw), "Give the OLT role to another port first") {
			t.Fatalf("OLT disable: %d %s", res.Status, res.Raw)
		}
	}
	if res := patch(r.owner.Access, "ether4", map[string]any{"role": "lan"}); res.Status != 200 {
		t.Fatalf("owner lan: %d %s", res.Status, res.Raw)
	}
	if res := patch(r.owner.Access, "ether9", map[string]any{"role": "customer"}); res.Status != 404 {
		t.Fatalf("no such port: %d %s", res.Status, res.Raw)
	}
	if res := patch(r.owner.Access, "ether5", map[string]any{}); res.Status != 400 {
		t.Fatalf("empty change: %d %s", res.Status, res.Raw)
	}
	// the change shows in the list and in the router's activity with the actor
	list := r.call(r.owner.Access, "GET", path, nil, nil)
	if portOf(t, list, "ether3")["role"] != "customer" || portOf(t, list, "ether4")["role"] != "lan" {
		t.Fatalf("%s", list.Raw)
	}
	found := false
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if e.SiteID == r.A.ID && e.Actor == "olga" && strings.Contains(e.Msg, "port ether3: role Unused -> Customer port") {
				found = true
			}
		}
	})
	if !found {
		t.Fatal("the role change is not in the router's activity with its actor")
	}
}

func TestAdminPortsSessionEndpoints(t *testing.T) {
	m, site, _ := uiRig(t)
	mk := m.mock(site)
	for _, n := range []string{"ether1", "ether2", "ether3", "sfp-sfpplus1"} {
		mk.SeedRow("/interface", routeros.Row{"name": n, "type": "ether", "running": "true", "disabled": "false"})
	}
	var out struct {
		Items []struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"items"`
	}
	m.getJSON(t, "/admin/routers/ports.json?id="+site, &out)
	if len(out.Items) != 4 {
		t.Fatalf("%+v", out)
	}
	code, j := m.postJSON(t, "/admin/routers/ports/update", url.Values{"id": {site}, "port": {"ether1"}, "disabled": {"1"}})
	if code != 409 || !strings.Contains(strings.ToLower(jsonText(j)), "port_locked") {
		t.Fatalf("WAN over the session endpoint: %d %v", code, j)
	}
	code, j = m.postJSON(t, "/admin/routers/ports/update", url.Values{"id": {site}, "port": {"ether3"}, "disabled": {"1"}})
	if code != 200 || j["ok"] != true {
		t.Fatalf("%d %v", code, j)
	}
}

func jsonText(m map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			b.WriteString(x + " ")
		case map[string]any:
			for k, e := range x {
				b.WriteString(k + " ")
				walk(e)
			}
		}
	}
	walk(m)
	return b.String()
}

func TestAPIRouterJSONCarriesTheCustomerAndDisabledPortLists(t *testing.T) {
	r, _, path := portsRig(t)
	id := "/routers/" + r.A.ID
	if res := r.call(r.oper.Access, "PATCH", path+"/ether4", map[string]any{"disabled": true}, nil); res.Status != 200 {
		t.Fatalf("%d %s", res.Status, res.Raw)
	}
	res := r.call(r.view.Access, "GET", id, nil, nil)
	j := res.JSON(t)
	if r2, ok := j["router"].(map[string]any); ok {
		j = r2
	}
	for _, k := range []string{"customer_ports", "disabled_ports", "bench_port"} {
		if _, ok := j[k]; !ok {
			t.Fatalf("no %s in %s", k, res.Raw)
		}
	}
	if d, _ := j["disabled_ports"].([]any); len(d) != 1 || d[0] != "ether4" {
		t.Fatalf("disabled_ports: %v", j["disabled_ports"])
	}
	if c, ok := j["customer_ports"].([]any); !ok || c == nil {
		t.Fatalf("customer_ports must be an array: %v", j["customer_ports"])
	}
}

func TestAPIRouterLANPortsChangeNeedsTheNameAndAConnectedPort(t *testing.T) {
	r, mk, _ := portsRig(t)
	id := "/routers/" + r.A.ID
	// no typed name
	res := r.call(r.owner.Access, "PATCH", id, map[string]any{"lan_ports": "ether5"}, nil)
	if res.Status != 422 || !strings.Contains(string(res.Raw), "confirm_name") {
		t.Fatalf("no name: %d %s", res.Status, res.Raw)
	}
	// the name, but no cable on the new port
	for _, row := range mk.Table("/interface") {
		if row["name"] == "ether5" {
			_ = mk.SetRow(t.Context(), "/interface", row[".id"], map[string]string{"running": "false"})
		}
	}
	res = r.call(r.owner.Access, "PATCH", id, map[string]any{"lan_ports": "ether5", "confirm_name": "Alpha"}, nil)
	if res.Status != 409 || !strings.Contains(string(res.Raw), "last_lan_port") {
		t.Fatalf("cable-less: %d %s", res.Status, res.Raw)
	}
}

func TestAPIRouterPatchKeepsPortsTabChanges(t *testing.T) {
	r, _, path := portsRig(t)
	id := "/routers/" + r.A.ID
	if res := r.call(r.owner.Access, "PATCH", path+"/ether4", map[string]any{"role": "lan"}, nil); res.Status != 200 {
		t.Fatalf("%d %s", res.Status, res.Raw)
	}
	if res := r.call(r.owner.Access, "PATCH", id, map[string]any{"support_phone": "0700000001"}, nil); res.Status != 200 {
		t.Fatalf("%d %s", res.Status, res.Raw)
	}
	j := r.call(r.view.Access, "GET", id, nil, nil).JSON(t)
	if r2, ok := j["router"].(map[string]any); ok {
		j = r2
	}
	if j["lan_ports"] != "ether2,ether4" {
		t.Fatalf("lan_ports: %v", j["lan_ports"])
	}
}

// Ports are off until the owner enables them: the list says so, Enable switches the port on at the router and
// records it, the admin port is marked, and the OLT can be enabled but not disabled.
func TestAPIPortEnableFlow(t *testing.T) {
	r, mk, path := portsRig(t)
	if err := r.st.Update(func(d *store.Data) error {
		s := d.Sites[r.A.ID]
		s.PortsPolicy, s.EnabledPorts = "off", ""
		d.Sites[r.A.ID] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"ether3", "sfp-sfpplus1"} {
		for _, row := range mk.Table("/interface") {
			if row["name"] == n {
				mk.SetRow(context.Background(), "/interface", row[".id"], map[string]string{"disabled": "true"})
			}
		}
	}
	list := r.call(r.oper.Access, "GET", path, nil, nil)
	if p := portOf(t, list, "ether3"); p["disabled"] != true || p["owner_enabled"] != false {
		t.Fatalf("a port nobody enabled: %v", p)
	}
	if p := portOf(t, list, "ether2"); p["note"] != "Admin — router management and the billing site only" || p["owner_enabled"] != true || p["disabled"] != false {
		t.Fatalf("admin port: %v", p)
	}
	if p := portOf(t, list, "ether1"); p["locked"] != true {
		t.Fatalf("uplink: %v", p)
	}
	res := r.call(r.oper.Access, "PATCH", path+"/ether3", map[string]any{"disabled": false}, nil)
	if res.Status != 200 {
		t.Fatalf("enable: %d %s", res.Status, res.Raw)
	}
	if p := res.JSON(t)["port"].(map[string]any); p["disabled"] != false || p["owner_enabled"] != true {
		t.Fatalf("after enable: %v", p)
	}
	for _, row := range mk.Table("/interface") {
		if row["name"] == "ether3" && row["disabled"] == "yes" || row["name"] == "ether3" && row["disabled"] == "true" {
			t.Fatalf("not enabled at the router: %v", row)
		}
	}
	var enabled string
	r.st.View(func(d *store.Data) { enabled = d.Sites[r.A.ID].EnabledPorts })
	if enabled != "ether3" {
		t.Fatalf("recorded: %q", enabled)
	}
	// the OLT: enable is allowed (owner), disable is refused with the new text
	if res := r.call(r.owner.Access, "PATCH", path+"/sfp-sfpplus1", map[string]any{"disabled": false}, nil); res.Status != 200 {
		t.Fatalf("enable the OLT: %d %s", res.Status, res.Raw)
	}
	res = r.call(r.owner.Access, "PATCH", path+"/sfp-sfpplus1", map[string]any{"disabled": true, "confirm_name": "Alpha"}, nil)
	if res.Status != 409 || !strings.Contains(string(res.Raw), "OLT port: can't be switched off here. Give the OLT role to another port first.") {
		t.Fatalf("disable the OLT: %d %s", res.Status, res.Raw)
	}
	// the uplink is never changed
	if res := r.call(r.owner.Access, "PATCH", path+"/ether1", map[string]any{"disabled": false}, nil); res.Status != 409 {
		t.Fatalf("uplink: %d %s", res.Status, res.Raw)
	}
}
