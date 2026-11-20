package web_test

import (
	"net/url"
	"strings"
	"testing"
)

// D8: /api/v1/routers… serves what /api/v1/sites… serves, ?router= means
// ?site=, router_id means site_id, and answers carry router_id/router_name.
func TestAPIRouterAliases(t *testing.T) {
	r := newAPIRig(t)
	a := r.acct(t, r.A, "RA-1", "AA:BB:CC:00:90:01")
	_ = r.acct(t, r.B, "RB-1", "AA:BB:CC:00:90:02")
	tok := r.view.Access

	old := r.call(tok, "GET", "/sites/"+r.A.ID, nil, nil)
	alias := r.call(tok, "GET", "/routers/"+r.A.ID, nil, nil)
	if old.Status != 200 || alias.Status != 200 || string(old.Raw) != string(alias.Raw) {
		t.Fatalf("site vs router: %d %s / %d %s", old.Status, old.Raw, alias.Status, alias.Raw)
	}
	if l := r.call(tok, "GET", "/routers", nil, nil).JSON(t)["items"].([]any); len(l) != 2 {
		t.Fatalf("routers list: %d", len(l))
	}
	for _, p := range []string{"/routers/" + r.A.ID + "/qoe", "/routers/" + r.A.ID + "/onts"} {
		if x := r.call(tok, "GET", p, nil, nil); x.Status != 200 {
			t.Errorf("%s: %d %s", p, x.Status, x.Raw)
		}
	}
	if x := r.call(tok, "GET", "/routers/nope", nil, nil); x.Status != 404 || x.errCode(t) != "not_found" {
		t.Errorf("unknown router: %d %s", x.Status, x.Raw)
	}
	if x := r.call("", "GET", "/routers", nil, nil); x.Status != 401 {
		t.Errorf("no token: %d", x.Status)
	}

	// ?router= filters like ?site=.
	bySite := r.call(tok, "GET", "/accounts?site="+r.B.ID, nil, nil).JSON(t)["items"].([]any)
	byRouter := r.call(tok, "GET", "/accounts?router="+r.B.ID, nil, nil).JSON(t)["items"].([]any)
	if len(bySite) != 1 || len(byRouter) != 1 {
		t.Fatalf("filters: %d %d", len(bySite), len(byRouter))
	}

	// Answers carry both spellings.
	acc := r.call(tok, "GET", "/accounts/"+a.ID, nil, nil).JSON(t)
	if acc["site_id"] != r.A.ID || acc["router_id"] != r.A.ID || acc["site_name"] != "Alpha" || acc["router_name"] != "Alpha" {
		t.Errorf("account aliases: %v", acc)
	}
	it := byRouter[0].(map[string]any)
	if it["router_id"] != it["site_id"] || it["router_name"] != it["site_name"] || it["router_id"] == nil {
		t.Errorf("list item aliases: %v", it)
	}

	// router_id in a body means site_id.
	x := r.call(r.oper.Access, "POST", "/accounts", map[string]any{"apartment_id": aptOf(t, r.st, r.A.ID), "router_id": r.A.ID, "flat": "RA-2", "mac": "aa-bb-cc-00-90-03"}, nil)
	if x.Status != 201 {
		t.Fatalf("create with router_id: %d %s", x.Status, x.Raw)
	}
	if j := x.JSON(t); j["site_id"] != r.A.ID || j["router_id"] != r.A.ID {
		t.Errorf("created: %v", j)
	}
}

func TestAdminRouterPaths(t *testing.T) {
	m := newMulti(t)
	for from, to := range map[string]string{"/admin/sites/view?id=abc": "/admin/routers/view?id=abc", "/admin/sites": "/admin/routers"} {
		r := m.admin("GET", from, nil)
		r.Body.Close()
		if r.StatusCode != 302 || r.Header.Get("Location") != to {
			t.Errorf("GET %s: %d -> %q", from, r.StatusCode, r.Header.Get("Location"))
		}
	}
	// POST works on both.
	for _, base := range []string{"/admin/sites", "/admin/routers"} {
		loc := m.adminPost(t, base+"/create", url.Values{"name": {"Via " + base}})
		if !strings.Contains(loc, "/admin/routers/view?id=") {
			t.Errorf("create via %s: %s", base, loc)
		}
	}
	if h := m.adminGet(t, "/admin/routers/new"); !strings.Contains(h, "<form") {
		t.Error("new page at the new address")
	}
}
