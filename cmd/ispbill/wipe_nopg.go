//go:build nopg

package main

import (
	"context"
	"errors"

	"ispbill/internal/config"
	"ispbill/internal/store"
)

func wipePostgres(context.Context, config.Config, bool) (store.WipeReport, error) {
	return store.WipeReport{}, errors.New("DATABASE_URL is set, but this build has no Postgres support (built with -tags nopg)")
}
