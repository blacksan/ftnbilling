package payments_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
