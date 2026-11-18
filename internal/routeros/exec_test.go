package routeros

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRESTExecuteTracerouteLeases(t *testing.T) {
	var gotBody, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody, gotQuery = r.URL.Path, string(b), r.URL.RawQuery
		switch r.URL.Path {
		case "/rest/execute":
			w.Write([]byte(`{"ret":"name: gw1"}`))
		case "/rest/tool/traceroute":
			w.Write([]byte(`[{".section":"0","address":"10.0.0.1","loss":"0%","sent":"1","last":"1ms"},
				{".section":"1","address":"","status":"timeout"},
				{".section":"0","address":"10.0.0.1","loss":"0%","sent":"1","last":"2ms","avg":"2ms"},
				{".section":"1","address":"8.8.8.8","loss":"0%","sent":"1","last":"9ms"}]`))
		case "/rest/ip/dhcp-server/lease":
			w.Write([]byte(`[{"mac-address":"aa:bb:cc:dd:ee:01","address":"10.5.50.20","active-address":"10.5.50.21","host-name":"ont","status":"bound","server":"dhcp1"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	ctx := context.Background()

	out, err := c.Execute(ctx, "/system/identity/print")
	if err != nil || out != "name: gw1" || gotPath != "/rest/execute" || !strings.Contains(gotBody, `"script":"/system/identity/print"`) || !strings.Contains(gotBody, `"as-string":""`) {
		t.Fatalf("%q %v %s %s", out, err, gotPath, gotBody)
	}
	hops, err := c.Traceroute(ctx, "8.8.8.8", 20)
	if err != nil || len(hops) != 2 || hops[0].Address != "10.0.0.1" || hops[0].Avg != "2ms" || hops[1].Address != "8.8.8.8" || hops[0].N != 1 {
		t.Fatalf("%+v %v", hops, err)
	}
	if !strings.Contains(gotBody, `"count":"1"`) || !strings.Contains(gotBody, `"max-hops":"20"`) || !strings.Contains(gotBody, `"address":"8.8.8.8"`) {
		t.Fatalf("traceroute body %s", gotBody)
	}
	ls, err := c.Leases(ctx, "aa:bb:cc:dd:ee:01")
	if err != nil || len(ls) != 1 || ls[0].Address != "10.5.50.21" || ls[0].MAC != "AA:BB:CC:DD:EE:01" || ls[0].HostName != "ont" {
		t.Fatalf("%+v %v", ls, err)
	}
	if gotQuery != "mac-address=AA%3ABB%3ACC%3ADD%3AEE%3A01" {
		t.Fatalf("lease query %q", gotQuery)
	}
}

// When the router refuses as-string, a bare print falls back to a GET.
func TestExecuteFallsBackToGETForPrint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/execute":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"unknown parameter as-string"}`))
		case r.Method == "GET" && r.URL.Path == "/rest/ip/address":
			w.Write([]byte(`[{".id":"*1","address":"10.0.0.1/24","interface":"ether1"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	out, err := c.Execute(context.Background(), "/ip/address/print")
	if err != nil || !strings.Contains(out, "address=10.0.0.1/24") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := c.Execute(context.Background(), "/ip/address/add address=1.1.1.1/32"); err == nil {
		t.Fatal("a non-print command must not fall back")
	}
}

func TestParseExecuteResultShapes(t *testing.T) {
	for in, want := range map[string]string{`{"ret":"a"}`: "a", `[{"ret":"a"},{"ret":"b"}]`: "a\nb", `"plain"`: `"plain"`} {
		if got := parseExecuteResult([]byte(in)); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
}
