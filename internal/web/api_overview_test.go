package web_test

import (
	"testing"
)

func TestAPIOverview(t *testing.T) {
	r, _ := liveRig(t)
	r.acct(t, r.A, "Flat 3B", "AA:BB:CC:DD:EE:01")
	r.acct(t, r.B, "Flat 9", "AA:BB:CC:DD:EE:09")
	x := r.call(r.view.Access, "GET", "/overview", nil, nil)
	if x.Status != 200 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	j := x.JSON(t)
	for _, k := range []string{"greeting_period", "setup", "revenue", "active", "online_now", "routers", "today", "next", "renewals_48h", "busiest_routers", "health", "expiries_per_day"} {
		if _, ok := j[k]; !ok {
			t.Errorf("missing %s: %s", k, x.Raw)
		}
	}
	if rt := j["routers"].(map[string]any); rt["total"].(float64) != 2 {
		t.Fatalf("%v", rt)
	}
	if len(j["expiries_per_day"].([]any)) != 14 || len(j["revenue"].(map[string]any)["spark_7d"].([]any)) != 7 {
		t.Fatalf("%s", x.Raw)
	}
	if st := j["setup"].(map[string]any); st["total"].(float64) != 6 || len(st["steps"].([]any)) != 6 {
		t.Fatalf("%v", st)
	}
	// ?router= narrows it (and ?site= is the same)
	for _, q := range []string{"?router=" + r.A.ID, "?site=" + r.A.ID} {
		y := r.call(r.view.Access, "GET", "/overview"+q, nil, nil)
		if y.Status != 200 || y.JSON(t)["routers"].(map[string]any)["total"].(float64) != 1 {
			t.Fatalf("%s: %d %s", q, y.Status, y.Raw)
		}
	}
	if y := r.call(r.view.Access, "GET", "/overview?router=nope", nil, nil); y.Status != 404 {
		t.Fatalf("%d", y.Status)
	}
	if y := r.call("", "GET", "/overview", nil, nil); y.Status != 401 {
		t.Fatalf("%d", y.Status)
	}
}
