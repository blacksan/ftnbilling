package web_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/routeros"
)

type fakeJobs struct {
	mu      sync.Mutex
	running map[string]bool
	state   map[string]string
}

func (j *fakeJobs) Running(id string) bool { j.mu.Lock(); defer j.mu.Unlock(); return j.running[id] }
func (j *fakeJobs) State(id string) string { j.mu.Lock(); defer j.mu.Unlock(); return j.state[id] }

func liveRig(t *testing.T) (*apiRig, *routeros.Mock) {
	t.Helper()
	r := newAPIRig(t)
	mk := r.mock(r.A.ID)
	mk.SeedLive()
	return r, mk
}

func TestAPILiveAndInterfaces(t *testing.T) {
	r, _ := liveRig(t)
	a := r.acct(t, r.A, "Flat 3B", "AA:BB:CC:DD:EE:01")
	_ = a
	for _, tok := range []string{r.view.Access, r.oper.Access, r.owner.Access} {
		res := r.call(tok, "GET", "/routers/"+r.A.ID+"/live", nil, nil)
		if res.Status != 200 {
			t.Fatalf("%d %s", res.Status, res.Raw)
		}
	}
	res := r.call(r.view.Access, "GET", "/routers/"+r.A.ID+"/live", nil, nil)
	j := res.JSON(t)
	if j["router_id"] != r.A.ID || j["reachable"] != true {
		t.Fatalf("%s", res.Raw)
	}
	sys := j["system"].(map[string]any)
	if sys["model"] != "RB5009UG+S+IN" || sys["version"] != "7.25.1 (stable)" || sys["cpu_load_pct"].(float64) != 7 || sys["temp_c"].(float64) != 41 {
		t.Fatalf("system %v", sys)
	}
	fl := j["flats"].(map[string]any)
	if fl["total"].(float64) != 1 {
		t.Fatalf("flats %v", fl)
	}
	for _, k := range []string{"wan", "tunnel", "throughput", "revenue", "equipment", "provision"} {
		if _, ok := j[k].(map[string]any); !ok {
			t.Errorf("missing %s", k)
		}
	}
	if j["setup_state"] != "" {
		t.Fatalf("setup_state %v", j["setup_state"])
	}
	if th := j["throughput"].(map[string]any); th["step_s"].(float64) != 300 || len(th["t"].([]any)) != 36 {
		t.Fatalf("throughput %v", th)
	}
	// aliases: router_name is not in this answer, but the old /sites path does not exist for new routes
	if x := r.call(r.view.Access, "GET", "/sites/"+r.A.ID+"/live", nil, nil); x.Status != 404 {
		t.Fatalf("/sites twin must not exist: %d", x.Status)
	}
	if x := r.call(r.view.Access, "GET", "/routers/nope/live", nil, nil); x.Status != 404 || x.errCode(t) != "not_found" {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	if x := r.call("", "GET", "/routers/"+r.A.ID+"/live", nil, nil); x.Status != 401 {
		t.Fatalf("%d", x.Status)
	}

	// router B has no seeded data: 200 with reachable:false, not an error
	x := r.call(r.view.Access, "GET", "/routers/"+r.B.ID+"/live", nil, nil)
	if x.Status != 200 || x.JSON(t)["reachable"] != false {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}

	ifs := r.call(r.view.Access, "GET", "/routers/"+r.A.ID+"/interfaces", nil, nil)
	ij := ifs.JSON(t)
	items := ij["items"].([]any)
	if ifs.Status != 200 || len(items) != 5 || ij["reachable"] != true || ij["generated_at"] == "" {
		t.Fatalf("%d %s", ifs.Status, ifs.Raw)
	}
	roles := map[string]string{}
	for _, it := range items {
		m := it.(map[string]any)
		roles[m["name"].(string)] = m["role"].(string)
		if m["rx_bps"] != nil {
			t.Errorf("first read must have null rates: %v", m)
		}
	}
	if roles["ether1"] != "wan" || roles["sfp-sfpplus1"] != "olt" || roles["br-hotspot"] != "customer" || roles["wg-ftn"] != "tunnel" || roles["ether5"] != "other" {
		t.Fatalf("%v", roles)
	}
}

var secretKey = regexp.MustCompile(`(?i)"[a-z_-]*(password|private|secret|token)[a-z_-]*"\s*:`)

func TestAPILiveResponsesCarryNoSecrets(t *testing.T) {
	r, mk := liveRig(t)
	mk.SeedRow("/interface", routeros.Row{"name": "wg-x", "type": "wg", "private-key": "PRIVKEY-123", "password": "PW-HUNTER2"})
	mk.SeedRow("/ip/dhcp-server/lease", routeros.Row{"address": "10.5.50.99", "mac-address": "AA:AA:AA:00:00:09", "status": "bound", "password": "PW-HUNTER2"})
	r.acct(t, r.A, "Flat 3B", "AA:BB:CC:DD:EE:01")
	for _, p := range []string{"live", "interfaces", "clients"} {
		res := r.call(r.owner.Access, "GET", "/routers/"+r.A.ID+"/"+p, nil, nil)
		if res.Status != 200 {
			t.Fatalf("%s: %d", p, res.Status)
		}
		s := string(res.Raw)
		if secretKey.MatchString(s) || strings.Contains(s, "PRIVKEY-123") || strings.Contains(s, "PW-HUNTER2") || strings.Contains(s, "hs_pass") {
			t.Errorf("%s leaks: %s", p, s)
		}
	}
	// the account's hotspot password never shows either
	res := r.call(r.owner.Access, "GET", "/routers/"+r.A.ID+"/clients", nil, nil)
	if strings.Contains(string(res.Raw), "hspass") {
		t.Fatal("hs pass")
	}
}

func TestAPIClientsSearchAndFilters(t *testing.T) {
	r, mk := liveRig(t)
	a1 := r.acct(t, r.A, "Flat 3B", "AA:BB:CC:DD:EE:01")
	r.acct(t, r.A, "Flat 10A", "AA:BB:CC:DD:EE:02")
	for _, q := range mk.Table("/queue/simple") { // the seeded queue belongs to the first flat
		if q["name"] == "ftn-K7Q2MX" {
			mk.SetRow(context.Background(), "/queue/simple", q[routeros.RowID], map[string]string{"name": "ftn-" + a1.Code})
		}
	}
	get := func(q string) map[string]any {
		t.Helper()
		res := r.call(r.view.Access, "GET", "/routers/"+r.A.ID+"/clients"+q, nil, nil)
		if res.Status != 200 {
			t.Fatalf("%s: %d %s", q, res.Status, res.Raw)
		}
		return res.JSON(t)
	}
	all := get("")
	if c := all["counts"].(map[string]any); c["flats"].(float64) != 2 || c["unregistered"].(float64) != 1 || len(all["items"].([]any)) != 3 {
		t.Fatalf("%s", r.call(r.view.Access, "GET", "/routers/"+r.A.ID+"/clients", nil, nil).Raw)
	}
	n := func(q string) int { return len(get(q)["items"].([]any)) }
	for q, want := range map[string]int{
		"?q=3b": 1, "?q=flat": 2, "?q=10.5.50.61": 1, "?q=ee:02": 1, "?q=brians": 1, "?q=apple": 1, "?q=zzzz": 0,
		"?kind=flats": 2, "?kind=unregistered": 1, "?kind=flats&q=10a": 1, "?online=true": 2, "?online=false": 1,
	} {
		if got := n(q); got != want {
			t.Errorf("%s: %d, want %d", q, got, want)
		}
	}
	// the flat on the lease shows the router's detail
	for _, it := range all["items"].([]any) {
		m := it.(map[string]any)
		if m["flat"] == "Flat 3B" && (m["host_name"] != "Brians-iPhone" || m["queue_limit"] != "20M/20M" || m["port"] != "sfp-sfpplus1" || m["kind"] != "flat") {
			t.Fatalf("%v", m)
		}
	}
	for _, bad := range []string{"?kind=nope", "?online=maybe"} {
		if x := r.call(r.view.Access, "GET", "/routers/"+r.A.ID+"/clients"+bad, nil, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
			t.Fatalf("%s %d %s", bad, x.Status, x.Raw)
		}
	}
}

func TestAPIRebootGuards(t *testing.T) {
	r, mk := liveRig(t)
	jobs := &fakeJobs{running: map[string]bool{}, state: map[string]string{}}
	r.mgr.SetJobs(jobs)
	path := "/routers/" + r.A.ID + "/actions/reboot"
	reboots := func() int {
		n := 0
		for _, c := range mk.Calls() {
			if c.Path == "/system/reboot" {
				n++
			}
		}
		return n
	}
	// role: owner only
	for _, tok := range []string{r.view.Access, r.oper.Access} {
		if x := r.call(tok, "POST", path, map[string]any{"confirm_name": "Alpha"}, nil); x.Status != 403 || x.errCode(t) != "forbidden_role" {
			t.Fatalf("%d %s", x.Status, x.Raw)
		}
	}
	// the typed name must match
	for _, body := range []any{map[string]any{"confirm_name": "Beta"}, map[string]any{"confirm_name": ""}, map[string]any{}} {
		if x := r.call(r.owner.Access, "POST", path, body, nil); x.Status != 400 || x.errCode(t) != "validation_failed" {
			t.Fatalf("%d %s", x.Status, x.Raw)
		}
	}
	if x := r.call(r.owner.Access, "POST", path, nil, nil); x.Status != 400 {
		t.Fatalf("%d", x.Status)
	}
	if reboots() != 0 {
		t.Fatal("a refused call rebooted the router")
	}
	// refused while a provisioning job runs; a refused attempt does not use the 10-minute slot
	jobs.running[r.A.ID] = true
	if x := r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "Alpha"}, nil); x.Status != 409 || x.errCode(t) != "bad_state" {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	jobs.running[r.A.ID] = false
	// the router itself says no: 409 router_refused, slot not used
	mk.SetPathFail("/system/reboot", &routeros.APIError{Status: 403, Message: "Forbidden", Detail: "not enough permissions (9)", Method: "POST", Path: "/system/reboot"})
	if x := r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "alpha"}, nil); x.Status != 409 || x.errCode(t) != "router_refused" {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	mk.SetPathFail("/system/reboot", nil)
	// name match ignores case; success is 202 {queued:true}
	x := r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "  aLpHa "}, nil)
	if x.Status != 202 || x.JSON(t)["queued"] != true || reboots() != 1 {
		t.Fatalf("%d %s reboots=%d", x.Status, x.Raw, reboots())
	}
	// 1 per 10 minutes per router
	x = r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "Alpha"}, nil)
	if x.Status != 429 || x.errCode(t) != "rate_limited" || x.Header.Get("Retry-After") == "" || reboots() != 1 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	// another router is not held back
	r.mock(r.B.ID)
	if y := r.call(r.owner.Access, "POST", "/routers/"+r.B.ID+"/actions/reboot", map[string]any{"confirm_name": "Beta"}, nil); y.Status != 202 {
		t.Fatalf("%d %s", y.Status, y.Raw)
	}
	// after 10 minutes it works again
	r.svc.SetClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	if x = r.call(r.owner.Access, "POST", path, map[string]any{"confirm_name": "Alpha"}, nil); x.Status != 202 || reboots() != 2 {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	// unknown router
	if x = r.call(r.owner.Access, "POST", "/routers/nope/actions/reboot", map[string]any{"confirm_name": "x"}, nil); x.Status != 404 {
		t.Fatalf("%d", x.Status)
	}
}

func TestAPIRecheckAndResync(t *testing.T) {
	r, _ := liveRig(t)
	if x := r.call(r.view.Access, "POST", "/routers/"+r.A.ID+"/actions/recheck", nil, nil); x.Status != 403 {
		t.Fatalf("%d", x.Status)
	}
	if x := r.call(r.view.Access, "POST", "/routers/"+r.A.ID+"/actions/resync", nil, nil); x.Status != 403 {
		t.Fatalf("%d", x.Status)
	}
	x := r.call(r.oper.Access, "POST", "/routers/"+r.A.ID+"/actions/recheck", nil, nil)
	j := x.JSON(t)
	if x.Status != 200 || j["site_id"] != r.A.ID || j["router_id"] != r.A.ID || len(j["items"].([]any)) == 0 {
		t.Fatalf("%d %.300s", x.Status, x.Raw)
	}
	x = r.call(r.oper.Access, "POST", "/routers/"+r.A.ID+"/actions/resync", nil, nil)
	if x.Status != 202 || x.JSON(t)["queued"] != true {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
	r.svc.Wait()
	if x = r.call(r.oper.Access, "POST", "/routers/nope/actions/resync", nil, nil); x.Status != 404 {
		t.Fatalf("%d", x.Status)
	}
	_ = context.Background
}
