//go:build !nopg

package main

import (
	"context"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/store"
	"ispbill/internal/store/pgstore"
)

// wipePostgres counts, or with apply deletes, the business data. Applying takes
// the single-writer lock, so it fails while a service still runs on the database.
func wipePostgres(ctx context.Context, cfg config.Config, apply bool) (store.WipeReport, error) {
	b, err := pgstore.Open(ctx, cfg.DatabaseURL, pgstore.Options{NoLock: !apply, NoMigrate: true, Wait: 10 * time.Second})
	if err != nil {
		return store.WipeReport{}, err
	}
	defer b.Close()
	if apply {
		return b.Wipe(ctx)
	}
	return b.WipePreview(ctx)
}
