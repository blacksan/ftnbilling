//go:build !nopg

package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// B1: when the Postgres writer lock is lost run() must END (so the process exits non-zero and systemd restarts
// it). It used to close the web server and then wait for ever for loops that only stop when the context is
// cancelled.
func TestRunReturnsWhenTheWriterLockIsLost(t *testing.T) {
	u := schemaURL(t)
	t.Setenv("DATABASE_URL", u)
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("DATA_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	t.Setenv("ADMIN_PASS", "a-long-test-password-1")
	t.Setenv("ROUTER_MOCK", "true")
	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	done := make(chan error, 1)
	go func() { done <- run(slog.New(slog.NewTextHandler(testWriter{t}, nil))) }()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// wait for the app to take the lock, then kill the session that holds it
	deadline := time.Now().Add(20 * time.Second)
	killed := 0
	for time.Now().Before(deadline) && killed == 0 {
		select {
		case err := <-done:
			t.Fatalf("run ended before the lock was lost: %v", err)
		default:
		}
		_ = conn.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype='advisory' AND granted AND pid <> pg_backend_pid() AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&killed)
		time.Sleep(200 * time.Millisecond)
	}
	if killed == 0 {
		t.Fatal("the app never took the writer lock")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil after a lost writer lock: the process must exit non-zero")
		}
	case <-time.After(40 * time.Second):
		t.Fatal("run() did not return after the writer lock was lost: the process would hang and systemd would never restart it")
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
