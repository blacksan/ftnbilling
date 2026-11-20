package pgstore_test

import (
	"context"
	"testing"
	"time"

	"ispbill/internal/store/pgstore"
	"ispbill/internal/store/storetest"
)

// The single-writer lock lives on one idle connection. A Postgres restart (security update of the
// postgresql package) or any dropped connection releases it; the backend must notice, report it
// through LockLost/Ping, and refuse commits, so the service exits and systemd restarts it.
func TestAdvisoryLockLossIsDetected(t *testing.T) {
	url := storetest.RequirePostgres(t)
	schema := storetest.NewSchema(t, url)
	ctx := context.Background()
	a, err := open(t, url, schema, func(o *pgstore.Options) { o.LockCheck = 100 * time.Millisecond })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.LockLost():
		t.Fatal("lock reported lost while held")
	case <-time.After(300 * time.Millisecond):
	}
	// kill the session that holds the advisory lock (what a PG restart or a network blip does)
	var n int
	err = a.Pool().QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND granted AND pid <> pg_backend_pid() AND objid = ((hashtext('ispbill:'||$1)::bigint) & 4294967295)::oid`, a.Schema()).Scan(&n)
	if err != nil || n != 1 {
		t.Fatalf("terminate lock holder: n=%d err=%v", n, err)
	}
	select {
	case <-a.LockLost():
	case <-time.After(5 * time.Second):
		t.Fatal("the lost writer lock was not noticed")
	}
	if a.LockErr() == nil || a.Ping(ctx) == nil {
		t.Fatal("a backend that lost its lock still reports healthy")
	}
}
