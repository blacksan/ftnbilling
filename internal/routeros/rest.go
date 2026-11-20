package routeros

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
//
// Budget for a slow tunnel: one transport and one set of slots per router base
// URL (shared by every REST value built for it): 2 reads, 1 write and 1 ping or traceroute in
// flight, at most 3 connections, keep-alive. The per-call deadline starts only
// once a slot is held, so queueing does not eat the router's time.
type REST struct {
	base, user, pass string
	ep               *endpoint
	timeout          time.Duration
}

// DefaultTimeout is the per-call deadline (after the slot is taken).
const DefaultTimeout = 10 * time.Second

const (
	readSlots  = 2
	writeSlots = 1
	retryDelay = 300 * time.Millisecond
)

// endpoint is the shared state of one router: its transport and slots.
type endpoint struct {
	hc     *http.Client
	reads  chan struct{}
	writes chan struct{}
	tools  chan struct{} // ping and traceroute: they change nothing, so they never take the write slot
}

var (
	endpointsMu sync.Mutex
	endpoints   = map[string]*endpoint{}
)

func endpointFor(base string) *endpoint {
	endpointsMu.Lock()
	defer endpointsMu.Unlock()
	if e, ok := endpoints[base]; ok {
		return e
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxConnsPerHost:       readSlots + writeSlots + 1, // + the ping/traceroute slot
		MaxIdleConnsPerHost:   readSlots + writeSlots + 1,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // the per-call deadline bounds it
	}
	e := &endpoint{
		hc:     &http.Client{Transport: tr},
		reads:  make(chan struct{}, readSlots),
		writes: make(chan struct{}, writeSlots),
		tools:  make(chan struct{}, 1),
	}
	endpoints[base] = e
	return e
}

func NewREST(baseURL, user, pass string) *REST {
	base := strings.TrimRight(baseURL, "/")
	return &REST{base: base, user: user, pass: pass, ep: endpointFor(base), timeout: DefaultTimeout}
}

// WithTimeout sets the per-call deadline (ROUTER_READ_TIMEOUT); d <= 0 keeps the default.
func (c *REST) WithTimeout(d time.Duration) *REST {
	if d > 0 {
		c.timeout = d
	}
	return c
}

// NotSent reports that the request never left: the connection was never made
// (dial failure, refused, no route, DNS). A timeout while connecting is the
// same; only a timeout after the request was written means the router may
// have acted on it.
func NotSent(err error) bool {
	if err == nil {
		return false
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	m := err.Error()
	return strings.Contains(m, "dial tcp") || strings.Contains(m, "connection refused") || strings.Contains(m, "no route to host") ||
		strings.Contains(m, "network is unreachable") || strings.Contains(m, "no such host")
}

func (c *REST) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.doT(ctx, c.timeout, method, path, q, body, out)
}

// transient: a read worth one more try (timeout or reset, request may or may not have arrived).
func transient(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return false
	}
	if NotSent(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "connection reset")
}

// doT is do() with an explicit per-call deadline (the ping call needs longer).
// It takes a read or write slot (GET is a read, anything else a write), then
// starts the deadline. Reads are retried once after 300 ms on timeout or
// reset; writes only when the request was never sent.
func (c *REST) doT(ctx context.Context, timeout time.Duration, method, path string, q url.Values, body, out any) error {
	return c.doSlot(ctx, nil, timeout, method, path, q, body, out)
}

// doTool is doT for ping and traceroute: a POST that only measures. It has its own slot, so a running ping
// (5 s or more) does not hold the router's single write slot, and writes do not wait behind it.
func (c *REST) doTool(ctx context.Context, timeout time.Duration, method, path string, q url.Values, body, out any) error {
	return c.doSlot(ctx, c.ep.tools, timeout, method, path, q, body, out)
}

// doSlot: slots nil = the read slots for GET and the write slots for everything else.
func (c *REST) doSlot(ctx context.Context, slots chan struct{}, timeout time.Duration, method, path string, q url.Values, body, out any) error {
	read := method == http.MethodGet
	err := c.once(ctx, slots, read, timeout, method, path, q, body, out)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if (read && transient(err)) || (!read && NotSent(err)) {
		select {
		case <-time.After(retryDelay):
		case <-ctx.Done():
			return err
		}
		return c.once(ctx, slots, read, timeout, method, path, q, body, out)
	}
	return err
}

func (c *REST) once(ctx context.Context, slots chan struct{}, read bool, timeout time.Duration, method, path string, q url.Values, body, out any) error {
	if slots == nil {
		slots = c.ep.writes
		if read {
			slots = c.ep.reads
		}
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slots }()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.send(ctx, method, path, q, body, out)
}

func (c *REST) send(ctx context.Context, method, path string, q url.Values, body, out any) error {
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
	resp, err := c.ep.hc.Do(req)
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
