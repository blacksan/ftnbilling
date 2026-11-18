package routeros

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		cmd  string
		want Verdict
	}{
		// read-only: allowed
		{"/ip/address/print", Allow},
		{"ip address print where interface=ether1", Allow},
		{"/system/resource/print", Allow},
		{"/interface/print stats", Allow},
		{"/ip/hotspot/user print", Allow}, // "user" inside a path is not the /user menu
		{"/log/print", Allow},
		{"export", Allow},
		{"/ip/dhcp-server/lease/print where mac-address=AA:BB:CC:DD:EE:01", Allow},
		{":put hello", Allow},
		{":put [/system/identity/print]", Allow},
		{"/tool/traceroute 8.8.8.8 count=1", Allow},
		{"/ping 8.8.8.8 count=4", Allow},
		{"/interface/monitor-traffic ether1 once", Confirm}, // unknown verb: confirm
		{"/interface/ethernet/monitor ether1 once", Allow},
		{`/ip/firewall/filter/print where comment~"user; reboot"`, Allow}, // quoted text is not a command
		// confirm: changes something
		{"/ip/address/add address=1.2.3.4/24 interface=ether1", Confirm},
		{"/ip/hotspot/user/remove [find name=x]", Confirm},
		{"/interface/disable ether5", Confirm},
		{"/system/reboot", Confirm},
		{"/system/shutdown", Confirm},
		{"/ip/hotspot/active/remove [find]", Confirm},
		{":foreach i in=[/ip/address/find] do={:put $i}", Confirm},
		{"/ip/address/print file=out", Confirm},
		{"/ping 8.8.8.8 count=2; /ip address add address=1.1.1.1", Confirm},
		// deny
		{"/system/reset-configuration", Deny},
		{"/system reset-configuration no-defaults=yes", Deny},
		{"/sys res", Deny},
		{"/system/script/add name=x source=:put", Deny},
		{"/system/script/run x", Deny},
		{"/system/scheduler/add name=x", Deny},
		{"/system/backup/save", Deny},
		{"/system/package/disable ipv6", Deny},
		{"/system/routerboard/upgrade", Deny},
		{"/user/add name=x password=y group=full", Deny},
		{"/user print", Deny},
		{"user active print", Deny},
		{"/password", Deny},
		{"/ip/service/set ssh disabled=yes", Deny},
		{"/ip/service/print", Deny},
		{"/certificate/print", Deny},
		{"/tool/fetch url=http://x/y", Deny},
		{"/import file-name=x.rsc", Deny},
		{"/file/remove x", Deny},
		{"/file/edit x contents=y", Deny},
		{"/interface/wireguard/set wg0 private-key=x", Deny},
		{"/interface/wireguard/peers/remove 0", Deny},
		{"/interface/wireguard/print", Allow},
		{":execute {/system reboot}", Deny},
		{"/export show-sensitive", Deny},
		{"/ip/address/print show-sensitive", Deny},
		// nesting and separators cannot hide a denied command
		{":put [/user print]", Deny},
		{":foreach i in=[/user/find] do={/user remove $i}", Deny},
		{"/ip/address/print; /system/script/add name=x", Deny},
		{"/ip/address/print;/user/add name=a", Deny},
		{"{ /certificate print }", Deny},
		{"/system/identity/print;{/tool/fetch url=x}", Deny},
		// never-ending or malformed
		{"/ping 8.8.8.8", Deny},
		{"/tool/traceroute 8.8.8.8", Deny},
		{"/interface/ethernet/monitor ether1", Deny},
		{"/log/print follow", Deny},
		{"", Deny},
		{"   ", Deny},
		{"/ip/address/print\n/system/reboot", Deny},
		{"/ip/address/print\x00", Deny},
		{strings.Repeat("a", 501), Deny},
	}
	for _, c := range cases {
		got := Classify(c.cmd)
		if got.Verdict != c.want {
			t.Errorf("%q: got %s (%s), want %s", c.cmd, got.Verdict, got.Reason, c.want)
		}
		if len(got.Redacted) > 300 {
			t.Errorf("%q: redacted command is %d characters", c.cmd, len(got.Redacted))
		}
	}
}

func TestRedaction(t *testing.T) {
	in := "name=wg0 private-key=\"AAAAsecretBBBB\" preshared-key=xyz password=hunter2 secret=\"a b\" passphrase=p\n" +
		`{"password":"json-pass","private-key":"jk"} private-key: kv-secret`
	out := RedactOutput(in)
	for _, leak := range []string{"AAAAsecretBBBB", "xyz", "hunter2", "a b", "json-pass", "jk\"", "kv-secret"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "name=wg0") {
		t.Fatalf("harmless text must stay: %s", out)
	}
	d := Classify("/ip/hotspot/user/add name=a password=topsecret")
	if strings.Contains(d.Redacted, "topsecret") || d.Verdict != Confirm {
		t.Fatalf("%+v", d)
	}
}

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
