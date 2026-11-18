package routeros

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRESTOneAndCall(t *testing.T) {
	var method, path string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = nil
		if len(b) > 0 {
			json.Unmarshal(b, &body)
		}
		switch r.URL.Path {
		case "/rest/system/device-mode":
			w.Write([]byte(`{"mode":"home","scheduler":"false","fetch":"true","flagged":false,"n":3}`))
		case "/rest/ip/dns":
			w.Write([]byte(`[{"allow-remote-requests":"yes"}]`))
		case "/rest/ip/dns/set":
			w.Write([]byte(`[]`))
		case "/rest/empty":
			w.Write([]byte(`[]`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":404,"message":"Not Found","detail":"no such command"}`))
		}
	}))
	defer srv.Close()
	var c Inspector = NewREST(srv.URL, "u", "p")
	ctx := context.Background()
	r, err := c.One(ctx, "/system/device-mode")
	if err != nil || r["mode"] != "home" || r["scheduler"] != "false" || r["flagged"] != "false" || r["n"] != "3" || method != "GET" {
		t.Fatalf("%v %v", r, err)
	}
	if r, err = c.One(ctx, "ip/dns"); err != nil || r["allow-remote-requests"] != "yes" {
		t.Fatalf("a one-row table: %v %v", r, err)
	}
	if r, err = c.One(ctx, "/empty"); err != nil || len(r) != 0 {
		t.Fatalf("%v %v", r, err)
	}
	var ae *APIError
	if _, err = c.One(ctx, "/nope"); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("%v", err)
	}
	if err = c.Call(ctx, "/ip/dns/set", map[string]string{"allow-remote-requests": "yes"}); err != nil ||
		method != "POST" || path != "/rest/ip/dns/set" || body["allow-remote-requests"] != "yes" {
		t.Fatalf("%v %s %s %v", err, method, path, body)
	}
}

func TestMockInspector(t *testing.T) {
	var _ Inspector = NewMock()
	var _ Inspector = NoRouter().(Inspector)
	m := NewMock()
	ctx := context.Background()
	if _, err := m.One(ctx, "/ip/dns"); err == nil {
		t.Fatal("an unseeded singleton is a 404")
	}
	m.SeedOne("/ip/dns", Row{"allow-remote-requests": "no"})
	if err := m.Call(ctx, "/ip/dns/set", map[string]string{"allow-remote-requests": "yes"}); err != nil {
		t.Fatal(err)
	}
	r, _ := m.One(ctx, "/ip/dns")
	if r["allow-remote-requests"] != "yes" || len(m.Calls()) != 1 || m.Writes() != 1 {
		t.Fatalf("%v %v", r, m.Calls())
	}
	m.SetPathFail("/ip/dns", errors.New("boom"))
	if _, err := m.One(ctx, "/ip/dns"); err == nil {
		t.Fatal("path failure")
	}
	if _, err := m.Rows(ctx, "/ip/dns", nil); err == nil {
		t.Fatal("path failure on rows")
	}
	m.SetPathFail("/ip/dns", nil)
	if _, err := m.One(ctx, "/ip/dns"); err != nil {
		t.Fatal(err)
	}
	if _, err := NoRouter().(Inspector).One(ctx, "/x"); !errors.Is(err, ErrNoRouter) {
		t.Fatal(err)
	}
}

func TestMockAddRowHonoursPlaceBefore(t *testing.T) {
	m := NewMock()
	ctx := context.Background()
	a := m.SeedRow("/ip/firewall/filter", Row{"comment": "a"})
	m.SeedRow("/ip/firewall/filter", Row{"comment": "b"})
	if _, err := m.AddRow(ctx, "/ip/firewall/filter", map[string]string{"comment": "x", "place-before": a}); err != nil {
		t.Fatal(err)
	}
	rows := m.Table("/ip/firewall/filter")
	if len(rows) != 3 || rows[0]["comment"] != "x" || rows[0]["place-before"] != "" {
		t.Fatalf("%v", rows)
	}
	if _, err := m.AddRow(ctx, "/ip/firewall/filter", map[string]string{"comment": "y", "place-before": "*FFFF"}); err == nil {
		t.Fatal("an unknown place-before is an error")
	}
}
