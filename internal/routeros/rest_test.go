package routeros

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRESTUpsertEscapingAndErrors(t *testing.T) {
	var calls []string
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "ispbill" || p != "pw" {
			http.Error(w, "", 401)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			m := map[string]string{}
			json.Unmarshal(b, &m)
			bodies = append(bodies, m)
		}
		switch {
		case r.Method == "GET" && r.URL.Query().Get("name") == "EXISTS":
			w.Write([]byte(`[{".id":"*1A","name":"EXISTS"}]`))
		case r.Method == "GET":
			w.Write([]byte(`[]`))
		case r.URL.Path == "/rest/ip/hotspot/active/login":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"invalid user name or password"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "ispbill", "pw")
	ctx := context.Background()

	if err := c.EnsureUser(ctx, User{Name: "NEW", Password: "123456", Profile: "ispbill-x", MAC: "AA:BB:CC:DD:EE:01", Comment: "ispbill:1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureUser(ctx, User{Name: "EXISTS", Password: "1", Profile: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveUser(ctx, "EXISTS"); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureProfile(ctx, Profile{Name: "ispbill-x", RateLimit: "5M/5M"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"GET /rest/ip/hotspot/user?name=NEW", "PUT /rest/ip/hotspot/user?",
		"PATCH /rest/ip/hotspot/user/*1A?", "DELETE /rest/ip/hotspot/user/*1A?",
		"PUT /rest/ip/hotspot/user/profile?",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}
	if bodies[0]["mac-address"] != "AA:BB:CC:DD:EE:01" || bodies[0]["profile"] != "ispbill-x" {
		t.Errorf("create body %v", bodies[0])
	}

	err := c.Login(ctx, "U", "P", "AA:BB:CC:DD:EE:01", "10.5.50.20")
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 400 || ae.Detail != "invalid user name or password" {
		t.Fatalf("error not surfaced: %#v", err)
	}
	if bad := NewREST(srv.URL, "ispbill", "wrong"); bad.Ping(ctx) == nil {
		t.Fatal("expected 401")
	}
}

func TestEscapedID(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`[{".id":"*1A"}]`))
			return
		}
		raw = r.URL.EscapedPath()
	}))
	defer srv.Close()
	NewREST(srv.URL, "u", "p").KickUser(context.Background(), "X")
	if raw != "/rest/ip/hotspot/active/%2A1A" {
		t.Fatalf("path %q", raw)
	}
}

func TestRESTTelemetryParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/ip/hotspot/active":
			w.Write([]byte(`[{".id":"*A","user":"K7Q2MX","address":"10.5.50.20","mac-address":"AA:BB:CC:DD:EE:01","bytes-in":"1000","bytes-out":"90000","uptime":"1h2m3s"}]`))
		case "/rest/queue/simple":
			w.Write([]byte(`[{"name":"<hotspot-K7Q2MX>","target":"10.5.50.20/32","rate":"120000/8500000","max-limit":"20000000/20000000"}]`))
		case "/rest/interface":
			w.Write([]byte(`[{"name":"ether1","type":"ether","rx-byte":"500","tx-byte":"70","running":"true","disabled":"false"}]`))
		case "/rest/ip/hotspot/host":
			w.Write([]byte(`[{"mac-address":"AA:BB:CC:DD:EE:02","address":"10.5.50.9","authorized":"false","bypassed":"false"},{"mac-address":"AA:BB:CC:DD:EE:01","address":"10.5.50.20","authorized":"true"}]`))
		case "/rest/ip/dns/cache":
			w.Write([]byte(`[{"name":"rr1.googlevideo.com","type":"A","data":"142.250.0.10"}]`))
		case "/rest/ping":
			var in map[string]string
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &in)
			if r.Method != "POST" || in["address"] != "8.8.8.8" || in["count"] != "5" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`[{"seq":"0","host":"8.8.8.8","time":"11ms500us"},{"seq":"1","host":"8.8.8.8","time":"13ms"},{"seq":"2","host":"8.8.8.8"},{"seq":"3","host":"8.8.8.8","time":"12ms"},{"seq":"4","host":"8.8.8.8","time":"12ms","sent":"5","received":"4","packet-loss":"20","avg-rtt":"12ms125us"}]`))
		case "/rest/system/resource":
			w.Write([]byte(`{"cpu-load":"7","free-memory":"500","total-memory":"1000","uptime":"1d"}`))
		case "/rest/system/health":
			w.Write([]byte(`[{"name":"temperature","value":"46","type":"C"}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	ctx := context.Background()
	ss, err := c.ActiveSessions(ctx)
	if err != nil || len(ss) != 1 || ss[0].BytesIn != 1000 || ss[0].BytesOut != 90000 || ss[0].Uptime.Seconds() != 3723 || ss[0].MAC != "AA:BB:CC:DD:EE:01" {
		t.Fatalf("%+v %v", ss, err)
	}
	qs, _ := c.SimpleQueues(ctx)
	if len(qs) != 1 || qs[0].RateA != 120000 || qs[0].RateB != 8500000 || qs[0].MaxB != 20000000 || qs[0].Target != "10.5.50.20/32" {
		t.Fatalf("%+v", qs)
	}
	ifs, _ := c.Interfaces(ctx)
	if len(ifs) != 1 || ifs[0].RxBytes != 500 || !ifs[0].Running || ifs[0].Disabled {
		t.Fatalf("%+v", ifs)
	}
	hs, _ := c.Hosts(ctx)
	if len(hs) != 2 || hs[0].Authorized || !hs[1].Authorized {
		t.Fatalf("%+v", hs)
	}
	dn, _ := c.DNSCache(ctx)
	if len(dn) != 1 || dn[0].Data != "142.250.0.10" {
		t.Fatalf("%+v", dn)
	}
	pr, err := c.PingHost(ctx, "8.8.8.8", 5)
	if err != nil || pr.Sent != 5 || pr.Received != 4 || pr.LossPct != 20 || pr.AvgRTT.Microseconds() != 12125 {
		t.Fatalf("%+v %v", pr, err)
	}
	si, err := c.System(ctx)
	if err != nil || si.CPULoad != 7 || !si.HasTemp || si.TempC != 46 {
		t.Fatalf("%+v %v", si, err)
	}
	if ParseRouterDuration("1w2d3h") == 0 || ParseRouterDuration("00:01:02").Seconds() != 62 {
		t.Fatal("duration parser")
	}
}
