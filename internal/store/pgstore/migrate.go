package pgstore

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration file %q must start with a number and an underscore", name)
		}
		raw, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		out = append(out, migration{version: v, name: strings.TrimSuffix(name, ".sql"), sql: string(raw), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// SchemaVersion is the newest migration this binary knows.
func SchemaVersion() int {
	ms, _ := loadMigrations()
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].version
}

const lockSQL = "SELECT pg_advisory_xact_lock(hashtext('ispbill-migrate:'||current_schema()))"

// Migrate applies every pending migration, each in its own transaction under
// an advisory lock. It is forward-only: it refuses to run when an applied
// migration was edited (checksum) or when the database is newer than this
// binary.
func (b *Backend) Migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, lockSQL); err != nil {
		tx.Rollback(ctx)
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY, name text NOT NULL, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, m := range ms {
		if err := b.applyOne(ctx, m, ms[len(ms)-1].version); err != nil {
			return err
		}
	}
	// The database must not be newer than the binary.
	var newest int
	if err := b.pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&newest); err != nil {
		return err
	}
	if newest > ms[len(ms)-1].version {
		return fmt.Errorf("the database schema (version %d) is newer than this program understands (version %d): upgrade ispbill, do not downgrade it", newest, ms[len(ms)-1].version)
	}
	return nil
}

func (b *Backend) applyOne(ctx context.Context, m migration, newestKnown int) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, lockSQL); err != nil {
		return err
	}
	// A database newer than the binary must not be touched at all.
	var newest int
	if err := tx.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&newest); err != nil {
		return err
	}
	if newest > newestKnown {
		return fmt.Errorf("the database schema (version %d) is newer than this program understands (version %d): upgrade ispbill, do not downgrade it", newest, newestKnown)
	}
	var sum string
	err = tx.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version = $1", m.version).Scan(&sum)
	switch {
	case err == nil:
		if sum != m.checksum {
			return fmt.Errorf("migration %s was changed after it was applied (checksum mismatch); never edit a released migration, add a new one", m.name)
		}
		return nil
	case err != pgx.ErrNoRows:
		return err
	}
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s failed: %w", m.name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES ($1, $2, $3, $4)",
		m.version, m.name, m.checksum, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
