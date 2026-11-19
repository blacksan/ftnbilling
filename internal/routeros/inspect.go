package routeros

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Inspector is the optional extra a client may offer for the setup checks:
// reading a singleton (a path that answers one object, not a table) and
// sending a command POST (/ip/dns/set, /ip/traffic-flow/set). REST and Mock
// implement it; the sites package type-asserts it, like Executor.
type Inspector interface {
	// One reads a singleton such as /system/device-mode, /ip/dns,
	// /ip/traffic-flow or /ipv6/settings. If the router answers a table, the
	// first row is returned. Values are strings ("true"/"false" or "yes"/"no").
	One(ctx context.Context, path string) (Row, error)
	// Call POSTs a body to a command path ("/ip/dns/set") and ignores the answer.
	Call(ctx context.Context, path string, body map[string]string) error
}

// ErrNoInspector is returned by callers that need an Inspector when the
// building's client does not offer one.
var ErrNoInspector = errors.New("this router client cannot read single-object settings")

// One implements Inspector.
func (c *REST) One(ctx context.Context, path string) (Row, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, rowsPath(path), nil, nil, &raw); err != nil {
		return nil, err
	}
	toRow := func(m map[string]any) Row {
		row := make(Row, len(m))
		for k, v := range m {
			row[k] = fmt.Sprint(v)
		}
		return row
	}
	var one map[string]any
	if json.Unmarshal(raw, &one) == nil {
		return toRow(one), nil
	}
	var many []map[string]any
	if json.Unmarshal(raw, &many) == nil {
		if len(many) == 0 {
			return Row{}, nil
		}
		return toRow(many[0]), nil
	}
	return nil, fmt.Errorf("routeros: %s: unexpected answer", path)
}

// Call implements Inspector.
func (c *REST) Call(ctx context.Context, path string, body map[string]string) error {
	var b any
	if body != nil {
		b = body
	}
	return c.do(ctx, http.MethodPost, rowsPath(path), nil, b, nil)
}

// ---- Mock ----------------------------------------------------------------

// CallRecord is one command POST seen by the Mock.
type CallRecord struct {
	Path string
	Body map[string]string
}

// SeedOne sets a singleton ("/ip/dns", "/system/device-mode") for One.
func (m *Mock) SeedOne(path string, r Row) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.singles == nil {
		m.singles = map[string]Row{}
	}
	m.singles[cleanPath(path)] = copyRow(r)
}

// One returns a copy of the seeded singleton (404 if none).
func (m *Mock) One(_ context.Context, path string) (Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.pathErrLocked(path); err != nil {
		return nil, err
	}
	if r, ok := m.singles[cleanPath(path)]; ok {
		return copyRow(r), nil
	}
	return nil, &APIError{Status: 404, Message: "Not Found", Detail: "no such command or directory", Method: "GET", Path: path}
}

// Call records the POST. A ".../set" call changes the singleton of the parent
// path, like the router would.
func (m *Mock) Call(_ context.Context, path string, body map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeErrLocked(path); err != nil {
		return err
	}
	p := cleanPath(path)
	b := copyRow(body)
	m.calls = append(m.calls, CallRecord{Path: p, Body: b})
	if base, ok := strings.CutSuffix(p, "/set"); ok {
		if m.singles == nil {
			m.singles = map[string]Row{}
		}
		r := m.singles[base]
		if r == nil {
			r = Row{}
			m.singles[base] = r
		}
		for k, v := range b {
			if k != RowID {
				r[k] = v
			}
		}
	}
	m.writes++
	return nil
}

// Calls returns the command POSTs made so far.
func (m *Mock) Calls() []CallRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CallRecord(nil), m.calls...)
}

// SetPathFail makes every call that touches path (reads, and for a table its
// row writes) fail with err. nil removes it.
func (m *Mock) SetPathFail(path string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.failPaths, cleanPath(path))
		return
	}
	if m.failPaths == nil {
		m.failPaths = map[string]error{}
	}
	m.failPaths[cleanPath(path)] = err
}

// SetWriteFail makes only the writes (add, set, remove, call) on path fail with
// err, reads keep working. nil removes it.
func (m *Mock) SetWriteFail(path string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.failWrites, cleanPath(path))
		return
	}
	if m.failWrites == nil {
		m.failWrites = map[string]error{}
	}
	m.failWrites[cleanPath(path)] = err
}

func (m *Mock) writeErrLocked(path string) error {
	if err := m.pathErrLocked(path); err != nil {
		return err
	}
	return m.failWrites[cleanPath(path)]
}

// pathErrLocked is the error to return for a call on path, if any.
func (m *Mock) pathErrLocked(path string) error {
	if m.fail != nil {
		return m.fail
	}
	return m.failPaths[cleanPath(path)]
}

// AddHostRow adds a hotspot host with every field as given (authorised or not).
func (m *Mock) AddHostRow(h Host) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hosts = append(m.hosts, h)
}

// ---- NoRouter --------------------------------------------------------------

func (noRouter) One(context.Context, string) (Row, error) { return nil, ErrNoRouter }
func (noRouter) Call(context.Context, string, map[string]string) error {
	return ErrNoRouter
}
