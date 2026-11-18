package web

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHostGuard(t *testing.T) {
	calls := 0
	tbl := map[string]string{"AA:BB:CC:DD:EE:01": "10.5.50.21"}
	var fail error
	g := &hostGuard{maxAge: time.Minute, minWait: 0, fetch: func(context.Context) (map[string]string, error) {
		calls++
		if fail != nil {
			return nil, fail
		}
		return tbl, nil
	}}
	ctx := context.Background()
	if !g.ok(ctx, "AA:BB:CC:DD:EE:01", "10.5.50.21") {
		t.Fatal("known host rejected")
	}
	if g.ok(ctx, "AA:BB:CC:DD:EE:01", "10.5.50.99") {
		t.Fatal("wrong ip accepted")
	}
	if g.ok(ctx, "AA:BB:CC:DD:EE:02", "") {
		t.Fatal("unknown mac accepted")
	}
	n := calls
	if !g.ok(ctx, "AA:BB:CC:DD:EE:01", "") || calls != n {
		t.Fatal("cached hit should not refetch")
	}
	fail = errors.New("router down")
	g.hosts = nil
	if g.ok(ctx, "AA:BB:CC:DD:EE:01", "") {
		t.Fatal("must fail closed when router is unreachable")
	}
}
