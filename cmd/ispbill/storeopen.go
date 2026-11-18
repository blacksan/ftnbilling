package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
)

// openStore opens the database selected by the configuration: Postgres when
// DATABASE_URL is set, the JSON file in DATA_DIR otherwise.
func openStore(ctx context.Context, cfg config.Config, log *slog.Logger) (*store.Store, error) {
	if cfg.DatabaseURL == "" {
		// A file renamed by an earlier import means the data now lives in
		// Postgres. Starting on a fresh JSON file would fork the business data.
		if old, _ := filepath.Glob(filepath.Join(cfg.DataDir, "ispbill.json.imported-*")); len(old) > 0 {
			return nil, fmt.Errorf("DATABASE_URL is not set, but %s shows this data was moved into Postgres. "+
				"Set DATABASE_URL again (starting on a new JSON file would split your records). "+
				"To go back to the JSON file on purpose, restore it from that file and rename it to ispbill.json first", filepath.Base(old[0]))
		}
		log.Info("database: JSON file", "dir", cfg.DataDir)
		return store.Open(cfg.DataDir)
	}
	return openPostgres(ctx, cfg, log)
}

// importedName is where an imported JSON file is moved.
func importedName(path string, now time.Time) string {
	name := path + ".imported-" + now.Format("20060102")
	if _, err := os.Stat(name); err == nil {
		name = path + ".imported-" + now.Format("20060102-150405")
	}
	return name
}

func renameImported(path string, now time.Time) (string, error) {
	to := importedName(path, now)
	if err := os.Rename(path, to); err != nil {
		return "", err
	}
	_ = os.Chmod(to, 0o600)
	return to, nil
}
