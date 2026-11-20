package routeros

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The metrics ping (POST /ping) has its own slot and does not take the router's only write slot, so a
// row write (customer activation after a payment, a Ports-tab change) has to wait behind it, and its
// context (8 s for the short task run) counts the wait.
func TestPingDoesNotHoldTheWriteSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ping") {
			time.Sleep(1500 * time.Millisecond) // count=5 takes ~5 s on a real router; scaled down
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	go c.PingHost(context.Background(), "8.8.8.8", 5)
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.SetRow(ctx, "/interface", "*1", map[string]string{"disabled": "yes"})
	_ = start
	if err != nil {
		t.Fatalf("a row write could not get the write slot while a ping was running: %v", err)
	}
}
