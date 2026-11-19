package routeros

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlotsCapReadsAndWrites(t *testing.T) {
	var cur, maxR, curW, maxW int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, m := &cur, &maxR
		if r.Method != http.MethodGet {
			c, m = &curW, &maxW
		}
		n := atomic.AddInt32(c, 1)
		for {
			o := atomic.LoadInt32(m)
			if n <= o || atomic.CompareAndSwapInt32(m, o, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		atomic.AddInt32(c, -1)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.Rows(context.Background(), "/ip/arp", nil) }()
		go func() { defer wg.Done(); c.RemoveRow(context.Background(), "/ip/arp", "*1") }()
	}
	wg.Wait()
	if atomic.LoadInt32(&maxR) != 2 || atomic.LoadInt32(&maxW) != 1 {
		t.Fatalf("max in flight reads=%d writes=%d, want 2 and 1", maxR, maxW)
	}
	// A second REST for the same router shares the slots.
	if NewREST(srv.URL, "u", "p").ep != c.ep {
		t.Fatal("endpoint not shared per base URL")
	}
}

func TestDeadlineStartsAfterSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p").WithTimeout(200 * time.Millisecond)
	// 4 reads, 2 slots: the last two wait ~120 ms for a slot yet still get their own 200 ms.
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { _, err := c.Rows(context.Background(), "/x", nil); errs <- err }()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("queued read failed: %v", err)
		}
	}
}

func TestReadRetriedOnceOnTimeout(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		w.Write([]byte(`[{"name":"a"}]`))
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p").WithTimeout(100 * time.Millisecond)
	rows, err := c.Rows(context.Background(), "/x", nil)
	if err != nil || len(rows) != 1 || atomic.LoadInt32(&n) != 2 {
		t.Fatalf("rows=%v err=%v calls=%d", rows, err, n)
	}
	// Still failing: exactly one retry, then the error.
	atomic.StoreInt32(&n, -100)
	n2 := int32(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n2, 1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv2.Close()
	if _, err := NewREST(srv2.URL, "u", "p").WithTimeout(100*time.Millisecond).Rows(context.Background(), "/x", nil); err == nil || atomic.LoadInt32(&n2) != 2 {
		t.Fatalf("err=%v calls=%d, want error after 2 calls", err, n2)
	}
}

func TestSentWriteNotRetried(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p").WithTimeout(100 * time.Millisecond)
	if _, err := c.AddRow(context.Background(), "/x", map[string]string{"a": "b"}); err == nil {
		t.Fatal("want timeout")
	}
	if atomic.LoadInt32(&n) != 1 {
		t.Fatalf("write sent %d times, want 1", n)
	}
}

func TestNotSentWriteRetriedOnce(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused: never sent
	c := NewREST(url, "u", "p")
	start := time.Now()
	err := c.RemoveRow(context.Background(), "/x", "*1")
	if err == nil || !NotSent(err) {
		t.Fatalf("err=%v, want a not-sent error", err)
	}
	if time.Since(start) < retryDelay {
		t.Fatal("expected a retry after the delay")
	}
	if NotSent(errors.New("HTTP 400")) || NotSent(context.DeadlineExceeded) {
		t.Fatal("NotSent too eager")
	}
}

func TestRowsPropsSendsProplist(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get(".proplist") + "|" + r.URL.Query().Get("mac-address")
		w.Write([]byte(`[{".id":"*1","mac-address":"AA"}]`))
	}))
	defer srv.Close()
	c := NewREST(srv.URL, "u", "p")
	if _, err := c.RowsProps(context.Background(), "/ip/arp", map[string]string{"mac-address": "AA"}, []string{"mac-address", "address"}); err != nil {
		t.Fatal(err)
	}
	if got != ".id,mac-address,address|AA" {
		t.Fatalf("got %q", got)
	}
	c.Rows(context.Background(), "/ip/arp", nil)
	if got != "|" {
		t.Fatalf("Rows must not send .proplist, got %q", got)
	}
	if rows, err := NoRouter().RowsProps(context.Background(), "/x", nil, []string{"a"}); rows != nil || !errors.Is(err, ErrNoRouter) {
		t.Fatal("NoRouter")
	}
	if _, err := NewMock().RowsProps(context.Background(), "/x", nil, []string{"a"}); err != nil {
		t.Fatal(err)
	}
}
