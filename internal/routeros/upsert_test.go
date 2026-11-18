package routeros

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeROS mimics RouterOS 7.20: PATCH returns a bare 500 whenever the body
// contains "name"; GET returns normalised values.
type fakeROS struct {
	mu       sync.Mutex
	rows     map[string]map[string]any
	calls    []string
	patch500 bool
}

func (f *fakeROS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == "GET":
		out := []map[string]any{}
		for _, row := range f.rows {
			if row["name"] == r.URL.Query().Get("name") {
				out = append(out, row)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "PUT":
		row := map[string]any{".id": "*1"}
		for k, v := range body {
			row[k] = v
		}
		f.rows["*1"] = row
		_ = json.NewEncoder(w).Encode(row)
	case r.Method == "PATCH":
		if _, ok := body["name"]; ok || f.patch500 {
			http.Error(w, "", 500)
			return
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		for k, v := range body {
			f.rows[id][k] = v
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/set"):
		for k, v := range body {
			if k != ".id" {
				f.rows[body[".id"]][k] = v
			}
		}
	}
}

func TestUpsertSkipsUnchangedAndNeverPatchesName(t *testing.T) {
	f := &fakeROS{rows: map[string]map[string]any{
		"*1": {".id": "*1", "name": "ispbill-1-hour", "rate-limit": "20000k/20000k", "shared-users": "1"},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	if err := c.EnsureProfile(context.Background(), Profile{Name: "ispbill-1-hour", RateLimit: "20M/20M", SharedUsers: 1}); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "PATCH") || strings.HasPrefix(call, "POST") {
			t.Fatalf("unchanged profile should not be written: %v", f.calls)
		}
	}
	if err := c.EnsureProfile(context.Background(), Profile{Name: "ispbill-1-hour", RateLimit: "40M/40M", SharedUsers: 1}); err != nil {
		t.Fatal(err)
	}
	if got := f.rows["*1"]["rate-limit"]; got != "40M/40M" {
		t.Fatalf("rate-limit = %v", got)
	}
}

func TestUpsertFallsBackToSet(t *testing.T) {
	f := &fakeROS{patch500: true, rows: map[string]map[string]any{
		"*1": {".id": "*1", "name": "ABC123", "profile": "ispbill-1-day"},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	if err := c.EnsureUser(context.Background(), User{Name: "ABC123", Password: "x", Profile: "ispbill-1-week", MAC: "aa:bb:cc:dd:ee:ff"}); err != nil {
		t.Fatal(err)
	}
	if f.rows["*1"]["profile"] != "ispbill-1-week" {
		t.Fatalf("fallback set not applied: %v", f.rows["*1"])
	}
}

func TestUpsertCreates(t *testing.T) {
	f := &fakeROS{rows: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	if err := c.EnsureUser(context.Background(), User{Name: "4RBF64", Password: "x", Profile: "ispbill-1-hour"}); err != nil {
		t.Fatal(err)
	}
	if f.rows["*1"]["name"] != "4RBF64" {
		t.Fatal("not created")
	}
}
