package payments_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ispbill/internal/payments"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

func TestSpoolReplayIdempotentAndOrdered(t *testing.T) {
	sp := payments.NewSpool(filepath.Join(t.TempDir(), "spool"))
	for _, b := range []string{"a", "b", "c"} {
		if err := sp.Put("c2b", []byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	if sp.Pending() != 3 {
		t.Fatalf("pending %d", sp.Pending())
	}
	var seen []string
	// the database is down for the second body: replay stops, keeps b and c
	n, err := sp.Replay(func(k string, b []byte) error {
		if string(b) == "b" {
			return store.ErrUnavailable
		}
		seen = append(seen, string(b))
		return nil
	})
	if n != 1 || !errors.Is(err, store.ErrUnavailable) || sp.Pending() != 2 {
		t.Fatalf("n=%d err=%v pending=%d", n, err, sp.Pending())
	}
	n, err = sp.Replay(func(k string, b []byte) error { seen = append(seen, string(b)); return nil })
	if n != 2 || err != nil || sp.Pending() != 0 {
		t.Fatalf("n=%d err=%v pending=%d", n, err, sp.Pending())
	}
	if len(seen) != 3 || seen[0] != "a" || seen[1] != "b" || seen[2] != "c" {
		t.Errorf("order %v", seen)
	}
}

// Names start with the time, so oldest-first holds across kinds (a plain name
// sort used to group by kind first), and spool files written by older versions
// (<kind>-<nano>-<seq>.json) are still replayed, in time order with the new ones.
func TestSpoolReplayOldestFirstAcrossKindsAndOldNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	sp := payments.NewSpool(dir)
	os.MkdirAll(dir, 0o700)
	old := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old("stk-1000000000000000001-1.json", "old-stk") // oldest
	old("c2b-1000000000000000002-2.json", "old-c2b") // newer, sorts before "stk" by name
	for _, b := range []struct{ kind, body string }{{"stk", "n1"}, {"c2b", "n2"}, {"stk", "n3"}, {"c2b", "n4"}} {
		if err := sp.Put(b.kind, []byte(b.body)); err != nil {
			t.Fatal(err)
		}
	}
	var seen, kinds []string
	n, err := sp.Replay(func(k string, b []byte) error { seen, kinds = append(seen, string(b)), append(kinds, k); return nil })
	want := []string{"old-stk", "old-c2b", "n1", "n2", "n3", "n4"}
	wantKinds := []string{"stk", "c2b", "stk", "c2b", "stk", "c2b"}
	if err != nil || n != 6 || strings.Join(seen, ",") != strings.Join(want, ",") || strings.Join(kinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("n=%d err=%v seen=%v kinds=%v", n, err, seen, kinds)
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(names) != 0 {
		t.Fatalf("left behind: %v", names)
	}
	sp.Put("c2b", []byte("x"))
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(names) != 1 || !strings.HasSuffix(names[0], "-c2b.json") {
		t.Fatalf("new name format: %v", names)
	}
}

// A slow handler (a router push) must not block callbacks being spooled.
func TestSpoolPutDoesNotWaitForReplayHandler(t *testing.T) {
	sp := payments.NewSpool(filepath.Join(t.TempDir(), "spool"))
	sp.Put("c2b", []byte("first"))
	inHandler, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		sp.Replay(func(k string, b []byte) error { close(inHandler); <-release; return nil })
		close(done)
	}()
	<-inHandler
	put := make(chan error, 1)
	go func() { put <- sp.Put("stk", []byte("second")) }()
	select {
	case err := <-put:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Put blocked behind a running Replay handler")
	}
	// A second Replay while one runs does not run handlers concurrently.
	if n, err := sp.Replay(func(string, []byte) error { t.Error("concurrent replay ran a handler"); return nil }); n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	close(release)
	<-done
	if sp.Pending() != 1 {
		t.Fatalf("pending %d, want the second body", sp.Pending())
	}
}

func TestSpoolSetsAsideUnappliableBody(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	sp := payments.NewSpool(dir)
	sp.Put("stk", []byte("x"))
	sp.Put("stk", []byte("y"))
	n, err := sp.Replay(func(k string, b []byte) error {
		if string(b) == "x" {
			return errors.New("garbage")
		}
		return nil
	})
	if n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	bad, _ := filepath.Glob(filepath.Join(dir, "*.bad"))
	if len(bad) != 1 || sp.Pending() != 0 {
		t.Errorf("bad=%v pending=%d", bad, sp.Pending())
	}
	if fi, _ := os.Stat(bad[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestC2BSpooledOnPersistFailureThenReplayed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	sp := payments.NewSpool(filepath.Join(t.TempDir(), "spool"))
	r.mp.SetSpool(sp)
	before := r.expiry()

	r.h.Fault.FailNextCommit(storetest.ErrInjected)
	res := r.mp.HandleC2BConfirmation(ctx, c2b("SPOOL1", r.acct.Code, "50.00"))
	if res.ResultCode != "0" {
		t.Fatalf("daraja must still be answered Accepted: %+v", res)
	}
	if sp.Pending() != 1 {
		t.Fatalf("expected the callback spooled, pending=%d", sp.Pending())
	}
	if !r.expiry().Equal(before) {
		t.Fatal("the failed commit must leave the account untouched")
	}
	n, err := r.mp.Replay(ctx)
	if err != nil || n != 1 || sp.Pending() != 0 {
		t.Fatalf("replay n=%d err=%v pending=%d", n, err, sp.Pending())
	}
	if !r.expiry().After(before) {
		t.Error("replay should have extended the account")
	}
	// a second delivery of the same receipt changes nothing
	exp := r.expiry()
	r.mp.HandleC2BConfirmation(ctx, c2b("SPOOL1", r.acct.Code, "50.00"))
	if !r.expiry().Equal(exp) {
		t.Error("duplicate receipt extended the account twice")
	}
}

func TestSTKSpooledOnPersistFailure(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	sp := payments.NewSpool(filepath.Join(t.TempDir(), "spool"))
	r.mp.SetSpool(sp)
	o := r.order(t, "1 Day")
	if err := r.mp.Start(ctx, o); err != nil {
		t.Fatal(err)
	}
	r.h.Fault.FailNextCommit(storetest.ErrInjected)
	if err := r.mp.HandleSTKCallback(ctx, stkCB("ws_CO_123", 0, "STK1", o.AmountKES)); err != nil {
		t.Fatalf("must acknowledge: %v", err)
	}
	if sp.Pending() != 1 {
		t.Fatalf("pending=%d", sp.Pending())
	}
	if n, err := r.mp.Replay(ctx); err != nil || n != 1 {
		t.Fatalf("replay n=%d err=%v", n, err)
	}
}
