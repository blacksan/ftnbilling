package routeros

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRESTAuthAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "ispbill" || p != "pw" {
			http.Error(w, "", 401)
			return
		}
		if r.Method == "PUT" && r.URL.Path == "/rest/ip/dhcp-server/lease" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"failure: already have such entry"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	c := NewREST(srv.URL, "ispbill", "pw")
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := c.AddRow(ctx, "/ip/dhcp-server/lease", map[string]string{"address": "10.5.50.60"})
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 400 || ae.Detail != "failure: already have such entry" {
		t.Fatalf("error not surfaced: %#v", err)
	}
	if bad := NewREST(srv.URL, "ispbill", "wrong"); bad.Ping(ctx) == nil {
		t.Fatal("expected 401")
	}
}

func TestEscapedID(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.EscapedPath()
	}))
	defer srv.Close()
	if err := NewREST(srv.URL, "u", "p").RemoveRow(context.Background(), "/ip/dhcp-server/lease", "*1A"); err != nil {
		t.Fatal(err)
	}
	if raw != "/rest/ip/dhcp-server/lease/%2A1A" {
		t.Fatalf("path %q", raw)
	}
}

func TestRESTTelemetryParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/queue/simple":
			w.Write([]byte(`[{"name":"ftn-K7Q2MX","target":"10.5.50.20/32","rate":"120000/8500000","max-limit":"20000000/20000000"}]`))
		case "/rest/interface":
			w.Write([]byte(`[{"name":"ether1","type":"ether","rx-byte":"500","tx-byte":"70","running":"true","disabled":"false"}]`))
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
	qs, _ := c.SimpleQueues(ctx)
	if len(qs) != 1 || qs[0].RateA != 120000 || qs[0].RateB != 8500000 || qs[0].MaxB != 20000000 || qs[0].Target != "10.5.50.20/32" {
		t.Fatalf("%+v", qs)
	}
	ifs, _ := c.Interfaces(ctx)
	if len(ifs) != 1 || ifs[0].RxBytes != 500 || !ifs[0].Running || ifs[0].Disabled {
		t.Fatalf("%+v", ifs)
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
