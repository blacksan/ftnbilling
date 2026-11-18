//go:build nopg

package main

import (
	"errors"
	"io"
)

func runDBCommand(string, []string, func(string) string, io.Writer) error {
	return errors.New("this build has no Postgres support (built with -tags nopg)")
}
