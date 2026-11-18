// Package storetest opens a store for tests on either backend. By default it
// is the JSON file; with TEST_STORE=postgres and TEST_DATABASE_URL it is a
// fresh Postgres schema per test, dropped at cleanup.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
)

// Postgres reports whether the Postgres backend was asked for.
func Postgres() bool { return os.Getenv("TEST_STORE") == "postgres" }

// URL is the admin database URL for Postgres tests ("" = none). It fails the
// test when Postgres was requested but no URL is given, so a CI mistake never
// silently skips the Postgres legs.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if Postgres() && u == "" {
		t.Fatal("TEST_STORE=postgres needs TEST_DATABASE_URL")
	}
	return u
}

// RequirePostgres skips the test unless a database is available.
func RequirePostgres(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set: Postgres test skipped")
	}
	return u
}

// NewSchema creates an empty schema in the test database and drops it when the
// test ends. It returns the schema name.
func NewSchema(t testing.TB, url string) string {
	t.Helper()
	var b [6]byte
	rand.Read(b[:])
	schema := "t_" + hex.EncodeToString(b[:])
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("cannot connect to the test database: %v", err)
	}
	if _, err := c.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		c.Close(ctx)
		t.Fatal(err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, url)
		if err != nil {
			return
		}
		c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		c.Close(ctx)
	})
	return schema
}

// PGOptions are the pgstore options tests use: a huge order window so old
// fixture dates still load.
func PGOptions(schema string) pgstore.Options {
	return pgstore.Options{Schema: schema, OrderWindow: 100 * 365 * 24 * time.Hour, AppName: "ispbill-test"}
}

// Fault injects failures between the Store and its backend.
type Fault struct {
	mu        sync.Mutex
	failNext  error // returned by the next Commit, nothing written
	ambiguous int   // 1 = next Commit returns ErrAmbiguous without committing; 2 = commits, then returns ErrAmbiguous
	loadErr   error
	commits   int
}

// FailNextCommit makes the next commit fail with err; nothing is written.
func (f *Fault) FailNextCommit(err error) { f.mu.Lock(); f.failNext = err; f.mu.Unlock() }

// AmbiguousNextCommit makes the next commit report an unknown outcome. When
// committed is true the change really is written first.
func (f *Fault) AmbiguousNextCommit(committed bool) {
	f.mu.Lock()
	f.ambiguous = 1
	if committed {
		f.ambiguous = 2
	}
	f.mu.Unlock()
}

// FailLoads makes every Load fail with err until it is called with nil.
func (f *Fault) FailLoads(err error) { f.mu.Lock(); f.loadErr = err; f.mu.Unlock() }

// Commits is how many commits reached the backend.
func (f *Fault) Commits() int { f.mu.Lock(); defer f.mu.Unlock(); return f.commits }

type faultBackend struct {
	store.Backend
	f *Fault
}

func (w faultBackend) Load(ctx context.Context) (*store.Data, error) {
	w.f.mu.Lock()
	err := w.f.loadErr
	w.f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return w.Backend.Load(ctx)
}

func (w faultBackend) Commit(ctx context.Context, d *store.Data, cs *store.ChangeSet) error {
	w.f.mu.Lock()
	fail, amb := w.f.failNext, w.f.ambiguous
	w.f.failNext, w.f.ambiguous = nil, 0
	w.f.commits++
	w.f.mu.Unlock()
	if fail != nil {
		return fail
	}
	switch amb {
	case 1:
		return fmt.Errorf("injected: %w", store.ErrAmbiguous)
	case 2:
		if err := w.Backend.Commit(ctx, d, cs); err != nil {
			return err
		}
		return fmt.Errorf("injected: %w", store.ErrAmbiguous)
	}
	return w.Backend.Commit(ctx, d, cs)
}

type faultReader struct{ faultBackend }

func (w faultReader) Events(ctx context.Context, q store.EventQuery) ([]store.Event, error) {
	return w.Backend.(store.EventReader).Events(ctx, q)
}

// Handle is a store under test.
type Handle struct {
	Store *store.Store
	// Kind is "json" or "postgres".
	Kind string
	// Dir is the data directory (JSON file location).
	Dir   string
	Fault *Fault

	t      testing.TB
	url    string
	schema string
}

// New opens an empty store on the selected backend.
func New(t testing.TB) *Handle {
	t.Helper()
	return NewIn(t, t.TempDir())
}

// NewIn is New with a given data directory (JSON backend).
func NewIn(t testing.TB, dir string) *Handle {
	t.Helper()
	store.Strict = true // rigs catch an ErrNoChange that follows a change
	h := &Handle{t: t, Dir: dir, Fault: &Fault{}, Kind: "json"}
	if Postgres() {
		h.Kind = "postgres"
		h.url = URL(t)
		h.schema = NewSchema(t, h.url)
	}
	h.Store = h.open()
	t.Cleanup(func() { h.Store.Close() })
	return h
}

func (h *Handle) open() *store.Store {
	h.t.Helper()
	var b store.Backend
	var err error
	if h.Kind == "postgres" {
		b, err = pgstore.Open(context.Background(), h.url, PGOptions(h.schema))
	} else {
		b, err = store.NewJSONBackend(h.Dir)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var wrapped store.Backend = faultBackend{b, h.Fault}
	if _, ok := b.(store.EventReader); ok {
		wrapped = faultReader{faultBackend{b, h.Fault}}
	}
	s, err := store.OpenWith(wrapped)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// Reopen closes the store and opens a new one on the same data, like a restart.
func (h *Handle) Reopen() *store.Store {
	h.t.Helper()
	if err := h.Store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.Store = h.open()
	return h.Store
}

// Schema is the Postgres schema of this handle ("" for JSON).
func (h *Handle) Schema() string { return h.schema }

// ErrInjected is a convenient error for FailNextCommit.
var ErrInjected = errors.New("injected commit failure")

// Open is NewIn in the shape of store.Open, for test rigs: the store on the
// selected backend, closed when the test ends.
func Open(t testing.TB, dir string) (*store.Store, error) { return NewIn(t, dir).Store, nil }
