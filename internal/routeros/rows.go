package routeros

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The generic row API on the REST client. It is the thin W0 version: a plain
// GET / PUT / PATCH / DELETE per call. W2 extends it (PATCH-then-/set fallback,
// batching) behind the same signatures.

func rowsPath(path string) string { return "/" + strings.Trim(strings.TrimSpace(path), "/") }

// Rows lists the rows of path whose properties equal query.
func (c *REST) Rows(ctx context.Context, path string, query map[string]string) ([]Row, error) {
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	var raw []map[string]any
	if err := c.do(ctx, http.MethodGet, rowsPath(path), q, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(raw))
	for _, r := range raw {
		row := make(Row, len(r))
		for k, v := range r {
			row[k] = fmt.Sprint(v)
		}
		out = append(out, row)
	}
	return out, nil
}

// AddRow creates a row (PUT) and returns its .id.
func (c *REST) AddRow(ctx context.Context, path string, body map[string]string) (string, error) {
	var out map[string]any
	if err := c.do(ctx, http.MethodPut, rowsPath(path), nil, body, &out); err != nil {
		return "", err
	}
	return fmt.Sprint(out[RowID]), nil
}

// SetRow changes properties of one row (PATCH).
func (c *REST) SetRow(ctx context.Context, path, id string, diff map[string]string) error {
	return c.do(ctx, http.MethodPatch, rowsPath(path)+"/"+url.PathEscape(id), nil, diff, nil)
}

// RemoveRow deletes one row (DELETE); a row that is already gone is fine.
func (c *REST) RemoveRow(ctx context.Context, path, id string) error {
	err := c.do(ctx, http.MethodDelete, rowsPath(path)+"/"+url.PathEscape(id), nil, nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}
