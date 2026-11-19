package routeros

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRESTSetRowFallsBackToSet(t *testing.T) {
	var seen []string
	var setBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/rest/queue/simple/*9":
			w.WriteHeader(500) // the bare 500 RouterOS 7.20 answers some PATCH bodies with
		case r.Method == http.MethodPatch:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":404,"message":"Not Found"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/rest/queue/simple/set":
			json.NewDecoder(r.Body).Decode(&setBody)
		}
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	ctx := context.Background()
	if err := c.SetRow(ctx, "/queue/simple", "*9", map[string]string{"max-limit": "5M/5M"}); err != nil {
		t.Fatal(err)
	}
	if setBody[RowID] != "*9" || setBody["max-limit"] != "5M/5M" {
		t.Fatalf("set body %v", setBody)
	}
	// A vanished row is a 404, not a reason to try /set.
	var ae *APIError
	if err := c.SetRow(ctx, "/queue/simple", "*1", map[string]string{"a": "b"}); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("%v", err)
	}
	if got := seen[len(seen)-1]; got != "PATCH /rest/queue/simple/*1" {
		t.Fatalf("no fallback expected after 404: %v", seen)
	}
	if !SameValue("max-limit", "10M/10M", "10000k/10000k") || !SameValue("mac-address", "AA:BB", "aa:bb") || SameValue("address", "10.0.0.1", "10.0.0.2") {
		t.Fatal("SameValue")
	}
}

// RouterOS 7.25.1 answers DELETE /rest/.../%2A2 with 400 "missing or invalid
// resource identifier": it does not decode %2A. Item ids must go on the wire as "*2".
func TestRESTRowIDsAreSentLiterally(t *testing.T) {
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.Method+" "+r.RequestURI)
		if strings.Contains(r.RequestURI, "%2A") || strings.Contains(r.RequestURI, "%2a") {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":400,"message":"Bad Request","detail":"missing or invalid resource identifier"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	ctx := context.Background()
	if err := c.RemoveRow(ctx, "/ip/firewall/address-list", "*2"); err != nil {
		t.Fatalf("remove: %v (requests %v)", err, uris)
	}
	if err := c.SetRow(ctx, "/queue/simple", "*1A", map[string]string{"max-limit": "5M/5M"}); err != nil {
		t.Fatalf("set: %v (requests %v)", err, uris)
	}
	if uris[0] != "DELETE /rest/ip/firewall/address-list/*2" || uris[1] != "PATCH /rest/queue/simple/*1A" {
		t.Fatalf("ids must be sent as they are, got %v", uris)
	}
	if got := idPath("../x"); got == "../x" {
		t.Fatalf("a value that is not a RouterOS id must be escaped, got %q", got)
	}
}

// A DELETE RouterOS refuses (not "gone") is retried once as POST <path>/remove.
func TestRESTRemoveRowFallsBackToRemove(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodDelete {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":400,"message":"Bad Request"}`))
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body[".id"] != "*7" {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if err := NewREST(srv.URL, "u", "p").RemoveRow(context.Background(), "/ip/firewall/address-list", "*7"); err != nil {
		t.Fatalf("fallback remove must succeed: %v (%v)", err, seen)
	}
	if len(seen) != 2 || seen[1] != "POST /rest/ip/firewall/address-list/remove" {
		t.Fatalf("requests %v", seen)
	}
}
