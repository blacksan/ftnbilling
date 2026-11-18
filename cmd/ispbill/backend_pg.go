//go:build !nopg

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
)

func openPostgres(ctx context.Context, cfg config.Config, log *slog.Logger) (*store.Store, error) {
	b, err := pgstore.Open(ctx, cfg.DatabaseURL, pgstore.Options{Wait: 60 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := autoImport(ctx, b, cfg, log); err != nil {
		b.Close()
		return nil, err
	}
	st, err := store.OpenWith(b)
	if err != nil {
		b.Close()
		return nil, err
	}
	st.SetCommitTimeout(cfg.DBCommitTimeout)
	log.Info("database: Postgres", "schema_version", pgstore.SchemaVersion())
	return st, nil
}

// autoImport moves DATA_DIR/ispbill.json into an empty database, once.
func autoImport(ctx context.Context, b *pgstore.Backend, cfg config.Config, log *slog.Logger) error {
	path := filepath.Join(cfg.DataDir, "ispbill.json")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if _, done, err := b.Meta(ctx, "imported_at"); err != nil || done {
		return err
	}
	empty, err := b.IsEmpty(ctx)
	if err != nil || !empty {
		if err == nil {
			log.Warn("ispbill.json exists but the database already has data; not importing (use `ispbill import-json -force` to replace it)")
		}
		return err
	}
	d, from, err := store.ReadJSON(path)
	if err != nil {
		return fmt.Errorf("cannot read %s for import: %w", path, err)
	}
	log.Info("importing ispbill.json into Postgres", "format", from)
	counts, err := b.Import(ctx, d, pgstore.ImportOptions{From: path})
	if err != nil {
		var ve *pgstore.ValidationError
		if errors.As(err, &ve) {
			return fmt.Errorf("%w\nFix these in the JSON file (or ask for help) and start again; nothing was changed", err)
		}
		return fmt.Errorf("import failed (nothing was kept): %w", err)
	}
	to, err := renameImported(path, time.Now())
	if err != nil {
		return fmt.Errorf("imported, but could not rename %s: %w", path, err)
	}
	log.Info("import finished", "rows", counts.Rows, "applied_kes", counts.AppliedKES, "file_moved_to", filepath.Base(to))
	return nil
}

// openPostgresShared opens the database without the single-writer lock and
// without migrating, for the small admin commands that may run while the
// service is up (each change is one transaction on a settings row).
func openPostgresShared(ctx context.Context, cfg config.Config) (*store.Store, error) {
	b, err := pgstore.Open(ctx, cfg.DatabaseURL, pgstore.Options{NoLock: true, NoMigrate: true, Wait: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	st, err := store.OpenWith(b)
	if err != nil {
		b.Close()
		return nil, err
	}
	return st, nil
}
