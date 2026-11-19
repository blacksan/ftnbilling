//go:build !nopg

package main

import (
	"context"
	"strings"
	"testing"

	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
	"ispbill/internal/store/storetest"
)

// The same story on Postgres (needs TEST_DATABASE_URL, like the other Postgres tests).
func TestWipeDataPostgres(t *testing.T) {
	u := schemaURL(t)
	dir := t.TempDir()
	base := cliEnv(dir)
	env := func(k string) string {
		switch k {
		case "DATABASE_URL":
			return u
		case "LISTEN_ADDR":
			return freeAddr(t)
		}
		return base(k)
	}
	if _, err := cli(t, env, "migrate"); err != nil {
		t.Fatal(err)
	}
	wipeScenario(t, dir, env, func() *store.Store {
		b, err := pgstore.Open(context.Background(), u, storetest.PGOptions(""))
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenWith(b)
		if err != nil {
			t.Fatal(err)
		}
		return st
	})
}

// A running service holds the single-writer lock: the real wipe cannot start.
func TestWipeDataPostgresRefusedWhileLocked(t *testing.T) {
	u := schemaURL(t)
	base := cliEnv(t.TempDir())
	env := func(k string) string {
		switch k {
		case "DATABASE_URL":
			return u
		case "LISTEN_ADDR":
			return freeAddr(t)
		}
		return base(k)
	}
	if _, err := cli(t, env, "migrate"); err != nil {
		t.Fatal(err)
	}
	b, err := pgstore.Open(context.Background(), u, storetest.PGOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := cli(t, env, "wipe-data", "-confirm", "WIPE"); err == nil || !strings.Contains(err.Error(), "another ispbill process") {
		t.Fatalf("want the lock refusal, got %v", err)
	}
	// The dry run takes no lock.
	if _, err := cli(t, env, "wipe-data"); err != nil {
		t.Fatal(err)
	}
}
