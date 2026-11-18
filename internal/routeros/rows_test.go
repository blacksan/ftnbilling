package routeros

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMockRowsCRUD(t *testing.T) {
	m := NewMock()
	ctx := context.Background()
	id, err := m.AddRow(ctx, "/ip/dhcp-server/lease/", map[string]string{"mac-address": "AA:BB:CC:DD:EE:01", "address": "10.5.0.60", "comment": "ftn:acct:X"})
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	m.AddRow(ctx, "ip/dhcp-server/lease", map[string]string{"mac-address": "AA:BB:CC:DD:EE:02"})
	rows, _ := m.Rows(ctx, "/ip/dhcp-server/lease", map[string]string{"mac-address": "aa:bb:cc:dd:ee:01"})
	if len(rows) != 1 || rows[0][RowID] != id || rows[0]["address"] != "10.5.0.60" {
		t.Fatalf("%v", rows)
	}
	if all, _ := m.Rows(ctx, "/ip/dhcp-server/lease", nil); len(all) != 2 {
		t.Fatalf("%v", all)
	}
	if other, _ := m.Rows(ctx, "/queue/simple", nil); len(other) != 0 {
		t.Fatal("tables must be separate")
	}
	if err := m.SetRow(ctx, "/ip/dhcp-server/lease", id, map[string]string{"address": "10.5.0.61"}); err != nil {
		t.Fatal(err)
	}
	if got := m.Table("/ip/dhcp-server/lease")[0]; got["address"] != "10.5.0.61" || got["comment"] != "ftn:acct:X" {
		t.Fatalf("set must change only the given keys: %v", got)
	}
	var ae *APIError
	if err := m.SetRow(ctx, "/ip/dhcp-server/lease", "*FF", nil); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("%v", err)
	}
	if err := m.RemoveRow(ctx, "/ip/dhcp-server/lease", id); err != nil || len(m.Table("/ip/dhcp-server/lease")) != 1 {
		t.Fatal(err)
	}
	if err := m.RemoveRow(ctx, "/ip/dhcp-server/lease", id); err != nil {
		t.Fatalf("removing a missing row: %v", err)
	}
	m.SetFail(errors.New("down"))
	if _, err := m.Rows(ctx, "/x", nil); err == nil {
		t.Fatal("SetFail must fail row calls")
	}
	if _, err := NoRouter().Rows(ctx, "/x", nil); !errors.Is(err, ErrNoRouter) {
		t.Fatal(err)
	}
}

func TestRESTRows(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]any{{".id": "*1", "address": "10.0.0.2", "dynamic": false}})
		case http.MethodPut:
			json.NewEncoder(w).Encode(map[string]any{".id": "*2"})
		case http.MethodDelete:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":404,"message":"Not Found"}`))
		}
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	ctx := context.Background()
	rows, err := c.Rows(ctx, "/ip/arp", map[string]string{"interface": "br1"})
	if err != nil || len(rows) != 1 || rows[0][RowID] != "*1" || rows[0]["dynamic"] != "false" {
		t.Fatal(rows, err)
	}
	if id, err := c.AddRow(ctx, "/ip/arp", map[string]string{"address": "1.1.1.1"}); err != nil || id != "*2" {
		t.Fatal(id, err)
	}
	if err := c.SetRow(ctx, "/ip/arp", "*2", map[string]string{"comment": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveRow(ctx, "/ip/arp", "*2"); err != nil {
		t.Fatalf("404 on delete must be fine: %v", err)
	}
	want := []string{"GET /rest/ip/arp?interface=br1", "PUT /rest/ip/arp?", "PATCH /rest/ip/arp/*2?", "DELETE /rest/ip/arp/*2?"}
	for i, w := range want {
		if i >= len(seen) || seen[i] != w {
			t.Fatalf("requests %v, want %v", seen, want)
		}
	}
}
