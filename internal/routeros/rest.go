package routeros

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIError is a RouterOS REST error ({"error":400,"message":..,"detail":..}).
type APIError struct {
	Status  int
	Message string
	Detail  string
	Method  string
	Path    string
	Body    string // raw response body (truncated), for the log
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("routeros: HTTP %d: %s", e.Status, e.Message)
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}
	if e.Method != "" {
		s += " [" + e.Method + " " + e.Path + "]"
	}
	if e.Body != "" && e.Detail == "" {
		s += " body=" + e.Body
	}
	return s
}

// REST implements Client over RouterOS v7 /rest with basic auth.
type REST struct {
	base, user, pass string
	hc               *http.Client
}

func NewREST(baseURL, user, pass string) *REST {
	return &REST{
		base: strings.TrimRight(baseURL, "/"),
		user: user,
		pass: pass,
		hc:   &http.Client{}, // per-call deadlines come from do()
	}
}

const defaultTimeout = 8 * time.Second

func (c *REST) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.doT(ctx, defaultTimeout, method, path, q, body, out)
}

// doT is do() with an explicit deadline (the ping call needs longer).
func (c *REST) doT(ctx context.Context, timeout time.Duration, method, path string, q url.Values, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := c.base + "/rest" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		raw := strings.TrimSpace(string(b))
		if len(raw) > 400 {
			raw = raw[:400]
		}
		ae := &APIError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode),
			Method: method, Path: path, Body: raw}
		var e struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			ae.Message, ae.Detail = e.Message, e.Detail
		} else if t := strings.TrimSpace(string(b)); t != "" && len(t) < 200 {
			ae.Message = t
		}
		return ae
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *REST) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/system/resource", nil, nil, nil)
}

type idRow struct {
	ID string `json:".id"`
}

// findIDs returns the .id of every row at path whose property key equals val.
func (c *REST) findIDs(ctx context.Context, path, key, val string) ([]string, error) {
	var rows []idRow
	if err := c.do(ctx, http.MethodGet, path, url.Values{key: {val}}, nil, &rows); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.ID != "" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// upsert creates the row if no row with name exists. If it exists, it
// compares the current values and changes only the fields that differ
// (never "name"), and does nothing when nothing differs. RouterOS 7.20
// answers some PATCH bodies with a bare HTTP 500, so a failed PATCH is
// retried once with the console-style POST <path>/set.
func (c *REST) upsert(ctx context.Context, path, name string, body map[string]string) error {
	var rows []map[string]any
	if err := c.do(ctx, http.MethodGet, path, url.Values{"name": {name}}, nil, &rows); err != nil {
		return err
	}
	var row map[string]any
	for _, r := range rows {
		if fmt.Sprint(r["name"]) == name {
			row = r
			break
		}
	}
	if row == nil {
		return c.do(ctx, http.MethodPut, path, nil, body, nil)
	}
	id := fmt.Sprint(row[".id"])
	diff := map[string]string{}
	for k, v := range body {
		if k == "name" {
			continue
		}
		if cur, ok := row[k]; ok && sameValue(k, fmt.Sprint(cur), v) {
			continue
		}
		if _, ok := row[k]; !ok && v == "" {
			continue // unset on router and empty in ours
		}
		diff[k] = v
	}
	if len(diff) == 0 {
		return nil
	}
	err := c.do(ctx, http.MethodPatch, path+"/"+url.PathEscape(id), nil, diff, nil)
	if err == nil {
		return nil
	}
	set := map[string]string{".id": id}
	for k, v := range diff {
		set[k] = v
	}
	if err2 := c.do(ctx, http.MethodPost, path+"/set", nil, set, nil); err2 != nil {
		return fmt.Errorf("%v; fallback set: %w", err, err2)
	}
	return nil
}

// sameValue compares a router value with ours, tolerating RouterOS's
// normalisation of MACs (upper case) and rate limits (20M -> 20000k).
func sameValue(key, cur, want string) bool {
	if strings.EqualFold(cur, want) {
		return true
	}
	if key == "rate-limit" {
		return normRate(cur) == normRate(want)
	}
	return false
}

func normRate(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == ' ' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strconv.FormatInt(rateBits(p), 10))
	}
	return strings.Join(out, "/")
}

func rateBits(p string) int64 {
	p = strings.TrimSpace(p)
	mult := int64(1)
	switch {
	case strings.HasSuffix(p, "k"), strings.HasSuffix(p, "K"):
		mult, p = 1000, p[:len(p)-1]
	case strings.HasSuffix(p, "M"):
		mult, p = 1000000, p[:len(p)-1]
	case strings.HasSuffix(p, "G"):
		mult, p = 1000000000, p[:len(p)-1]
	}
	n, err := strconv.ParseInt(p, 10, 64)
	if err != nil {
		return -1
	}
	return n * mult
}

func (c *REST) EnsureProfile(ctx context.Context, p Profile) error {
	shared := p.SharedUsers
	if shared <= 0 {
		shared = 1
	}
	return c.upsert(ctx, "/ip/hotspot/user/profile", p.Name, map[string]string{
		"name":         p.Name,
		"rate-limit":   p.RateLimit,
		"shared-users": strconv.Itoa(shared),
	})
}

func (c *REST) EnsureUser(ctx context.Context, u User) error {
	body := map[string]string{
		"name":     u.Name,
		"password": u.Password,
		"profile":  u.Profile,
		"comment":  u.Comment,
	}
	if u.MAC != "" {
		body["mac-address"] = u.MAC
	}
	return c.upsert(ctx, "/ip/hotspot/user", u.Name, body)
}

func (c *REST) RemoveUser(ctx context.Context, name string) error {
	ids, err := c.findIDs(ctx, "/ip/hotspot/user", "name", name)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.do(ctx, http.MethodDelete, "/ip/hotspot/user/"+url.PathEscape(id), nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *REST) RemoveProfile(ctx context.Context, name string) error {
	ids, err := c.findIDs(ctx, "/ip/hotspot/user/profile", "name", name)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.do(ctx, http.MethodDelete, "/ip/hotspot/user/profile/"+url.PathEscape(id), nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *REST) KickUser(ctx context.Context, name string) error {
	ids, err := c.findIDs(ctx, "/ip/hotspot/active", "user", name)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.do(ctx, http.MethodDelete, "/ip/hotspot/active/"+url.PathEscape(id), nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *REST) Login(ctx context.Context, user, password, mac, ip string) error {
	body := map[string]string{"user": user, "password": password}
	if mac != "" {
		body["mac-address"] = mac
	}
	if ip != "" {
		body["ip"] = ip
	}
	return c.do(ctx, http.MethodPost, "/ip/hotspot/active/login", nil, body, nil)
}
