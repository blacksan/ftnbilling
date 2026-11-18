//go:build nopg

package main

import (
	"context"
	"errors"
	"log/slog"

	"ispbill/internal/config"
	"ispbill/internal/store"
)

// The router image is built with -tags nopg: no Postgres driver is linked.
func openPostgres(context.Context, config.Config, *slog.Logger) (*store.Store, error) {
	return nil, errors.New("DATABASE_URL is set, but this build has no Postgres support (built with -tags nopg)")
}
