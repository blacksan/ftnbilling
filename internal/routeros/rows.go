package routeros

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The generic row API on the REST client: GET / PUT / PATCH / DELETE per call,
// with a PATCH-then-/set fallback (RouterOS 7.20 answers some PATCH bodies with a bare 500).

func rowsPath(path string) string { return "/" + strings.Trim(strings.TrimSpace(path), "/") }

// Rows lists the rows of path whose properties equal query.
func (c *REST) Rows(ctx context.Context, path string, query map[string]string) ([]Row, error) {
	return c.RowsProps(ctx, path, query, nil)
}

// RowsProps is Rows asking the router for only the listed properties
// (.proplist), which cuts big tables (leases, ARP, hosts) on a slow tunnel.
// The row's .id is always included. nil/empty props returns every property.
func (c *REST) RowsProps(ctx context.Context, path string, query map[string]string, props []string) ([]Row, error) {
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	if len(props) > 0 {
		q.Set(".proplist", strings.Join(append([]string{RowID}, props...), ","))
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

// SetRow changes properties of one row. RouterOS 7.20 answers some PATCH
// bodies with a bare HTTP 500, so a failed PATCH is retried once with the
// console-style POST <path>/set. A 404 (the row is gone) is returned as is: a
// retry cannot help and the caller should list the table again.
func (c *REST) SetRow(ctx context.Context, path, id string, diff map[string]string) error {
	if len(diff) == 0 {
		return nil
	}
	p := rowsPath(path)
	err := c.do(ctx, http.MethodPatch, p+"/"+idPath(id), nil, diff, nil)
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return err
	}
	set := map[string]string{RowID: id}
	for k, v := range diff {
		set[k] = v
	}
	if err2 := c.do(ctx, http.MethodPost, p+"/set", nil, set, nil); err2 != nil {
		return fmt.Errorf("%v; fallback set: %w", err, err2)
	}
	return nil
}

// SameValue compares a router's value for a property with the wanted one,
// tolerating RouterOS's normalisation of MACs (upper case) and rate limits
// (20M -> 20000k, "20M/20M" pairs).
func SameValue(key, cur, want string) bool {
	if sameValue(key, cur, want) {
		return true
	}
	if key == "max-limit" || key == "rate-limit" {
		return normRate(cur) == normRate(want)
	}
	return false
}

// RemoveRow deletes one row (DELETE); a row that is already gone is fine.
// If RouterOS refuses the DELETE for any reason other than "gone", the
// console-style POST <path>/remove is tried once, as SetRow does with /set.
func (c *REST) RemoveRow(ctx context.Context, path, id string) error {
	p := rowsPath(path)
	err := c.do(ctx, http.MethodDelete, p+"/"+idPath(id), nil, nil, nil)
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	if !errors.As(err, &ae) {
		return err // the request may not have reached the router; the caller retries
	}
	err2 := c.do(ctx, http.MethodPost, p+"/remove", nil, map[string]string{RowID: id}, nil)
	if err2 == nil {
		return nil
	}
	if errors.As(err2, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("%v; fallback remove: %w", err, err2)
}

// rowIDRe is a RouterOS internal item id: "*" followed by hex digits.
var rowIDRe = regexp.MustCompile(`^\*[0-9A-Fa-f]+$`)

// idPath is a row id as a URL path segment. RouterOS 7.25 does not decode
// "%2A", so "*2" escaped by url.PathEscape is answered with "missing or
// invalid resource identifier". A real id ("*" and hex) is safe as it is;
// anything else is escaped.
func idPath(id string) string {
	if rowIDRe.MatchString(id) {
		return id
	}
	return url.PathEscape(id)
}
