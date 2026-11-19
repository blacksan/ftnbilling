package web_test

import (
	"net/url"
	"strings"
	"testing"

	"ispbill/internal/routeros"
	"ispbill/internal/sites"
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
		{"POST", "/routers/" + id + "/setup/run", nil, F, OK, OK},
		{"POST", "/routers/" + id + "/setup/migrate", map[string]any{}, F, OK, OK},
		{"POST", "/routers/" + id + "/setup/rollback", nil, F, F, OK},
		{"POST", "/routers", map[string]any{}, F, F, OK},
		{"PATCH", "/routers/" + id, map[string]any{}, F, F, OK},
		{"GET", "/routers/" + id + "/checks", nil, OK, OK, OK},
		{"POST", "/routers/" + id + "/join-command", nil, F, F, OK},
		{"GET", "/routers/" + id + "/cutover", nil, OK, OK, OK},
		{"POST", "/routers/" + id + "/cutover/rollback", nil, F, F, OK},
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
	if !r.mgr.ServingCustomers(str(rt, "id")) {
		t.Fatal("serving intent not stored")
	}
	if x := r.call(r.owner.Access, "POST", "/routers", map[string]any{"name": " "}, nil); x.Status != 422 && x.Status != 400 {
		t.Fatalf("empty name: %d %s", x.Status, x.Raw)
	}
}

func TestAPISetupViewRunAndGuards(t *testing.T) {
	r := newAPIRig(t)
	c := r.call(r.owner.Access, "POST", "/routers", map[string]any{"name": "Delta"}, nil)
	id := str(c.JSON(t)["router"].(map[string]any), "id")
	v := r.call(r.view.Access, "GET", "/routers/"+id+"/setup", nil, nil)
	if v.Status != 200 {
		t.Fatalf("%d %s", v.Status, v.Raw)
	}
	j := v.JSON(t)
	for _, k := range []string{"router_id", "state", "kind", "serving_customers", "auto_start", "join", "next_action", "steps", "checks", "blockers", "live_devices", "evidence", "last_error"} {
		if _, ok := j[k]; !ok {
			t.Errorf("setup lacks %q: %s", k, v.Raw)
		}
	}
	if j["join"].(map[string]any)["state"] != "waiting" || j["next_action"].(map[string]any)["kind"] != "paste_join" {
		t.Fatalf("a new router waits for its join line: %s", v.Raw)
	}
	if got := r.call(r.oper.Access, "POST", "/routers/"+id+"/setup/run", nil, nil); got.Status != 409 || got.errCode(t) != "bad_state" {
		t.Fatalf("run before the join: %d %s", got.Status, got.Raw)
	}
	if got := r.call(r.oper.Access, "POST", "/routers/"+id+"/setup/migrate", map[string]any{"confirm_name": "Delta"}, nil); got.Status != 409 {
		t.Fatalf("migrate before the join: %d %s", got.Status, got.Raw)
	}
	if got := r.call(r.oper.Access, "GET", "/routers/nope/setup", nil, nil); got.Status != 404 {
		t.Fatalf("%d", got.Status)
	}
	// edit
	p := r.call(r.owner.Access, "PATCH", "/routers/"+id, map[string]any{"name": "Delta 2", "serving_customers": true}, nil)
	if p.Status != 200 || str(p.JSON(t), "name") != "Delta 2" || p.JSON(t)["serving_customers"] != true {
		t.Fatalf("%d %s", p.Status, p.Raw)
	}
}

func TestAPICreateAccountWithAPackageAndActivation(t *testing.T) {
	r := newAPIRig(t)
	pk := r.pkgID(t, "1 Week")
	ok := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"site_id": r.A.ID, "flat": "Flat 9", "mac": "AA:BB:CC:00:60:09", "package_id": pk, "activate": true, "note": "cash"}, nil)
	if ok.Status != 201 {
		t.Fatalf("%d %s", ok.Status, ok.Raw)
	}
	var got store.Account
	r.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.Flat == "Flat 9" {
				got = a
			}
		}
	})
	if got.PackageID != pk || got.ExpiresAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	bad := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"site_id": r.A.ID, "flat": "Flat 10", "mac": "AA:BB:CC:00:60:10", "activate": true}, nil)
	if bad.Status != 422 && bad.Status != 400 {
		t.Fatalf("activate without a package: %d %s", bad.Status, bad.Raw)
	}
	dup := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"site_id": r.A.ID, "flat": "Flat 11", "mac": "AA:BB:CC:00:60:09", "package_id": pk}, nil)
	if dup.Status != 409 || dup.errCode(t) != "mac_in_use" {
		t.Fatalf("%d %s", dup.Status, dup.Raw)
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
	post := r.admin("POST", "/admin/routers/setup/run", url.Values{"id": {r.A.ID}})
	if b := body(t, post); post.StatusCode != 202 || !strings.Contains(b, `"router_id"`) {
		t.Fatalf("%d %s", post.StatusCode, b)
	}
	fresh, err := r.mgr.CreateRouter(sites.SiteInput{Name: "Epsilon"}, false)
	if err != nil {
		t.Fatal(err)
	}
	post = r.admin("POST", "/admin/routers/setup/run", url.Values{"id": {fresh.ID}})
	if b := body(t, post); post.StatusCode != 409 || !strings.Contains(b, `"error"`) {
		t.Fatalf("a router that never joined: %d %s", post.StatusCode, b)
	}
	if g := r.admin("GET", "/admin/routers/setup.json?id=nope", nil); g.StatusCode != 404 {
		t.Fatalf("%d", g.StatusCode)
	}
}

func TestAPIRollbackNeedsTheNameAndSomethingToRollBack(t *testing.T) {
	r := newAPIRig(t)
	id := r.A.ID
	for _, path := range []string{"/routers/" + id + "/setup/rollback", "/routers/" + id + "/cutover/rollback"} {
		if x := r.call(r.owner.Access, "POST", path, map[string]any{}, nil); x.Status != 422 && x.Status != 400 {
			t.Fatalf("%s without the name: %d %s", path, x.Status, x.Raw)
		}
		if x := r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "Beta"}, nil); x.Status != 422 && x.Status != 400 {
			t.Fatalf("%s with another name: %d %s", path, x.Status, x.Raw)
		}
		// a live router holds no markers: there is nothing to roll back, and nothing may be written
		r.mock(id).ResetWrites()
		x := r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "Alpha"}, nil)
		if x.Status != 409 || x.errCode(t) != "bad_state" {
			t.Fatalf("%s: %d %s", path, x.Status, x.Raw)
		}
		if r.mock(id).Writes() != 0 {
			t.Fatal("the rollback request wrote to a router with nothing armed")
		}
	}
	for _, row := range r.mock(id).Table("/ip/firewall/address-list") {
		if row["list"] == "ftn-armed" {
			t.Fatalf("a marker was created: %v", row)
		}
	}
	// with a pending change the request is accepted and the marker says now
	r.mock(id).SeedRow("/ip/firewall/address-list", routeros.Row{"list": "ftn-armed", "address": "127.0.0.2", "comment": "5"})
	r.mock(id).SeedRow("/ip/firewall/address-list", routeros.Row{"list": "ftn-undo", "address": "127.0.1.1", "comment": "hotspot|hotspot1"})
	x := r.call(r.owner.Access, "POST", "/routers/"+id+"/setup/rollback", map[string]any{"confirm_name": "Alpha"}, nil)
	if x.Status != 202 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	found := false
	for _, row := range r.mock(id).Table("/ip/firewall/address-list") {
		if row["list"] == "ftn-armed" && row["comment"] == "now" {
			found = true
		}
	}
	if !found {
		t.Fatal("the armed marker must say now")
	}
}
