package routeros

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Mock is an in-memory fake router for ROUTER_MOCK=true and tests.
type Mock struct {
	mu   sync.Mutex
	fail error
	ms   *mockState
	// Executed records every command passed to Execute.
	Executed []string
	// ExecOut, when set, answers Execute instead of the built-in samples.
	ExecOut func(cmd string) (string, error)
	leases  []Lease
	// tables holds the generic rows (Rows, AddRow...): path -> rows in order.
	tables     map[string][]Row
	nextID     int
	singles    map[string]Row // singleton objects for One (see inspect.go)
	calls      []CallRecord
	failPaths  map[string]error
	failWrites map[string]error
	custDown   bool
	lastQ      map[string]map[string]string
	failReads  map[string]error
	writes     int // successful AddRow/SetRow/RemoveRow calls since the last ResetWrites
}

func NewMock() *Mock {
	return &Mock{}
}

// SetFail makes every call return err (nil restores normal behaviour).
func (m *Mock) SetFail(err error) { m.mu.Lock(); m.fail = err; m.mu.Unlock() }

func (m *Mock) Ping(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fail
}

// Inspection helpers for tests and the UI.

// AddLease adds a DHCP lease for Leases (demos and tests).
func (m *Mock) AddLease(l Lease) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leases = append(m.leases, l)
}

// Execute answers a few sample commands and records the rest.
func (m *Mock) Execute(_ context.Context, cmd string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return "", m.fail
	}
	m.Executed = append(m.Executed, cmd)
	if m.ExecOut != nil {
		return m.ExecOut(cmd)
	}
	c := strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(cmd, "/", " ")), " "))
	// The console forces these flags onto print; the samples ignore them.
	c = strings.TrimSuffix(strings.TrimSuffix(c, " without-paging"), " terse")
	switch {
	case strings.HasPrefix(c, ":put "):
		return strings.Trim(strings.TrimSpace(cmd[strings.Index(cmd, " ")+1:]), `"`), nil
	case c == "system identity print":
		return "name: mock-router", nil
	case c == "system resource print":
		return "uptime: 3d4h5m\nversion: 7.20 (stable)\ncpu-load: 3%\nboard-name: mock", nil
	case strings.HasPrefix(c, "ping "):
		host := strings.Fields(c)[1]
		var b strings.Builder
		b.WriteString("  SEQ HOST                                     SIZE TTL TIME       STATUS\n")
		for i, ms := range []string{"4ms512us", "4ms388us", "5ms101us", "4ms447us"} {
			fmt.Fprintf(&b, "    %d %-40s   56 117 %-10s\n", i, host, ms)
		}
		b.WriteString("    sent=4 received=4 packet-loss=0% min-rtt=4ms388us avg-rtt=4ms612us max-rtt=5ms101us")
		return b.String(), nil
	case c == "ip address print":
		return "0 address=10.5.50.1/24 interface=br-customers\n1 address=10.99.0.2/22 interface=wg-ftn", nil
	}
	return "", nil
}

// Traceroute returns three synthetic hops.
func (m *Mock) Traceroute(_ context.Context, target string, _ int) ([]Hop, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	return []Hop{
		{N: 1, Address: "10.99.0.1", Loss: "0%", Sent: "1", Last: "1.1ms", Avg: "1.1ms", Best: "1.1ms", Worst: "1.1ms"},
		{N: 2, Address: "196.201.0.1", Loss: "0%", Sent: "1", Last: "4.8ms", Avg: "4.8ms", Best: "4.8ms", Worst: "4.8ms"},
		{N: 3, Address: target, Loss: "0%", Sent: "1", Last: "9.6ms", Avg: "9.6ms", Best: "9.6ms", Worst: "9.6ms"},
	}, nil
}

// Leases returns the leases added with AddLease, filtered by MAC.
func (m *Mock) Leases(_ context.Context, mac string) ([]Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Lease
	for _, l := range m.leases {
		if mac == "" || strings.EqualFold(l.MAC, mac) {
			out = append(out, l)
		}
	}
	return out, nil
}

// ---- generic rows: in-memory tables ----------------------------------------

func cleanPath(p string) string { return "/" + strings.Trim(strings.TrimSpace(p), "/") }

func rowMatches(r, query Row) bool {
	for k, v := range query {
		if strings.HasPrefix(k, ".") { // .proplist and the like shape the answer, they do not filter rows
			continue
		}
		if !strings.EqualFold(r[k], v) {
			return false
		}
	}
	return true
}

func copyRow(r Row) Row {
	c := make(Row, len(r))
	for k, v := range r {
		c[k] = v
	}
	return c
}

// RowsProps is Rows; the mock returns every property, but records the
// proplist as ".proplist" in the query LastQuery gives back.
func (m *Mock) RowsProps(ctx context.Context, path string, query map[string]string, props []string) ([]Row, error) {
	if len(props) > 0 {
		q := make(map[string]string, len(query)+1)
		for k, v := range query {
			q[k] = v
		}
		q[".proplist"] = strings.Join(append([]string{RowID}, props...), ",")
		query = q
	}
	return m.Rows(ctx, path, query)
}

// Rows returns copies of the rows of path that match query.
func (m *Mock) Rows(_ context.Context, path string, query map[string]string) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.pathErrLocked(path); err != nil {
		return nil, err
	}
	if err := m.failReads[cleanPath(path)]; err != nil {
		return nil, err
	}
	if m.lastQ == nil {
		m.lastQ = map[string]map[string]string{}
	}
	m.lastQ[cleanPath(path)] = query
	var out []Row
	for _, r := range m.tables[cleanPath(path)] {
		if rowMatches(r, query) {
			out = append(out, copyRow(r))
		}
	}
	return out, nil
}

// AddRow appends a row with a new .id (*1, *2...).
func (m *Mock) AddRow(_ context.Context, path string, body map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeErrLocked(path); err != nil {
		return "", err
	}
	if m.tables == nil {
		m.tables = map[string][]Row{}
	}
	p := cleanPath(path)
	if p == "/ip/firewall/address-list" {
		// RouterOS refuses a second row with the same list and address.
		for _, e := range m.tables[p] {
			if e["list"] == body["list"] && e["address"] == body["address"] {
				return "", &APIError{Status: 400, Message: "Bad Request", Detail: "failure: already have such entry"}
			}
		}
	}
	m.nextID++
	m.writes++
	r := copyRow(body)
	r[RowID] = fmt.Sprintf("*%X", m.nextID)
	// place-before is an instruction, not a property: like the router, insert
	// before that row (an unknown id is an error) and do not store it.
	if before, ok := r["place-before"]; ok {
		delete(r, "place-before")
		rows := m.tables[p]
		for i, o := range rows {
			if o[RowID] == before {
				out := append(append(append([]Row(nil), rows[:i]...), r), rows[i:]...)
				m.tables[p] = out
				return r[RowID], nil
			}
		}
		m.nextID--
		m.writes--
		return "", &APIError{Status: 400, Message: "Bad Request", Detail: "place-before: no such item"}
	}
	m.tables[p] = append(m.tables[p], r)
	return r[RowID], nil
}

// SetRow changes the given properties of one row (404 if there is none).
func (m *Mock) SetRow(_ context.Context, path, id string, diff map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeErrLocked(path); err != nil {
		return err
	}
	for _, r := range m.tables[cleanPath(path)] {
		if r[RowID] == id {
			m.writes++
			for k, v := range diff {
				if k != RowID {
					r[k] = v
				}
			}
			return nil
		}
	}
	return &APIError{Status: 404, Message: "Not Found", Detail: "no such item"}
}

// RemoveRow deletes one row; a missing row is not an error.
func (m *Mock) RemoveRow(_ context.Context, path, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeErrLocked(path); err != nil {
		return err
	}
	p := cleanPath(path)
	rows := m.tables[p]
	for i, r := range rows {
		if r[RowID] == id {
			m.writes++
			m.tables[p] = append(rows[:i:i], rows[i+1:]...)
			return nil
		}
	}
	return nil
}

// Writes is the number of row writes (add, set, remove) that changed
// something since the last ResetWrites: tests use it to prove a second
// reconcile pass does nothing.
func (m *Mock) Writes() int { m.mu.Lock(); defer m.mu.Unlock(); return m.writes }

// ResetWrites zeroes the write counter.
func (m *Mock) ResetWrites() { m.mu.Lock(); m.writes = 0; m.mu.Unlock() }

// Table returns copies of every row of path in order (tests and the UI).
func (m *Mock) Table(path string) []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for _, r := range m.tables[cleanPath(path)] {
		out = append(out, copyRow(r))
	}
	return out
}

// SeedRow puts a row into a table as if it had been there before ispbill
// (a foreign row: tests of drift and conflicts).
func (m *Mock) SeedRow(path string, r Row) string {
	id, _ := m.AddRow(context.Background(), path, r)
	return id
}

// SeedLive fills the read paths the live router views use (see sites/live.go)
// with one plausible RB5009: its identity, health, interfaces with byte
// counters, addresses, two flats' leases, ARP, bridge hosts and per-flat
// queues. It only adds rows; call it on a mock that has none of these tables.
// Counters can then be moved with SetRow(path, id, ...) to produce a rate.
func (m *Mock) SeedLive() {
	m.SeedOne("/system/resource", Row{"uptime": "3d4h5m6s", "version": "7.25.1 (stable)", "board-name": "RB5009UG+S+", "architecture-name": "arm64",
		"cpu-count": "4", "cpu-load": "7", "free-memory": "805306368", "total-memory": "1073741824", "free-hdd-space": "900000000", "total-hdd-space": "1073741824"})
	m.SeedOne("/system/routerboard", Row{"routerboard": "true", "model": "RB5009UG+S+IN", "serial-number": "HF1234ABCDE", "firmware-type": "al64dr"})
	m.SeedRow("/system/health", Row{"name": "temperature", "value": "41", "type": "C"})
	m.SeedRow("/system/health", Row{"name": "cpu-temperature", "value": "47", "type": "C"})
	for _, r := range []Row{
		{"name": "ether1", "type": "ether", "running": "true", "disabled": "false", "mac-address": "78:9A:18:00:00:01", "mtu": "1500", "rx-byte": "1000000", "tx-byte": "200000", "comment": "WAN", "last-link-up-time": "2026-11-15 08:00:00"},
		{"name": "sfp-sfpplus1", "type": "ether", "running": "true", "disabled": "false", "mac-address": "78:9A:18:00:00:02", "mtu": "1500", "rx-byte": "300000", "tx-byte": "2000000", "comment": "OLT"},
		{"name": "ether5", "type": "ether", "running": "false", "disabled": "false", "mac-address": "78:9A:18:00:00:06", "mtu": "1500", "rx-byte": "0", "tx-byte": "0"},
		{"name": "br-customers", "type": "bridge", "running": "true", "disabled": "false", "mac-address": "78:9A:18:00:00:07", "mtu": "1500", "rx-byte": "300000", "tx-byte": "2000000"},
		{"name": "wg-ftn", "type": "wg", "running": "true", "disabled": "false", "mtu": "1380", "rx-byte": "5000", "tx-byte": "9000"},
	} {
		m.SeedRow("/interface", r)
	}
	m.SeedRow("/ip/address", Row{"address": "10.5.50.1/24", "network": "10.5.50.0", "interface": "br-customers", "disabled": "false"})
	m.SeedRow("/ip/address", Row{"address": "192.168.100.2/24", "network": "192.168.100.0", "interface": "ether1", "disabled": "false"})
	m.SeedRow("/ip/address", Row{"address": "10.99.0.2/22", "network": "10.99.0.0", "interface": "wg-ftn", "disabled": "false"})
	m.SeedRow("/ip/dhcp-server/lease", Row{"address": "10.5.50.60", "mac-address": "AA:BB:CC:DD:EE:01", "host-name": "Brians-iPhone", "status": "bound", "server": "ftn-dhcp", "last-seen": "12s", "comment": "ftn:acct:K7Q2MX Flat 3B"})
	m.SeedRow("/ip/dhcp-server/lease", Row{"address": "10.5.50.61", "mac-address": "AA:BB:CC:DD:EE:02", "status": "waiting", "server": "ftn-dhcp", "comment": "ftn:acct:P4R8TN Flat 10A"})
	m.SeedRow("/ip/dhcp-server/lease", Row{"address": "10.5.50.15", "mac-address": "3C:07:54:00:00:09", "host-name": "guest-laptop", "status": "bound", "server": "ftn-dhcp", "dynamic": "true", "last-seen": "40s"})
	m.SeedRow("/ip/arp", Row{"address": "10.5.50.60", "mac-address": "AA:BB:CC:DD:EE:01", "interface": "br-customers", "complete": "true", "dynamic": "false"})
	m.SeedRow("/ip/arp", Row{"address": "10.5.50.15", "mac-address": "3C:07:54:00:00:09", "interface": "br-customers", "complete": "true", "dynamic": "true"})
	m.SeedRow("/interface/bridge/host", Row{"mac-address": "AA:BB:CC:DD:EE:01", "on-interface": "sfp-sfpplus1", "bridge": "br-customers", "local": "false"})
	m.SeedRow("/interface/bridge/host", Row{"mac-address": "3C:07:54:00:00:09", "on-interface": "sfp-sfpplus1", "bridge": "br-customers", "local": "false"})
	m.SeedRow("/queue/simple", Row{"name": "ftn-K7Q2MX", "target": "10.5.50.60/32", "max-limit": "20000000/20000000", "bytes": "1000/5000", "rate": "0/0", "comment": "ftn:acct:K7Q2MX Flat 3B"})
	m.SeedRow("/queue/simple", Row{"name": "ftn-P4R8TN", "target": "10.5.50.61/32", "max-limit": "2000000/2000000", "bytes": "0/0", "rate": "0/0", "comment": "ftn:acct:P4R8TN Flat 10A"})
}

// LastQuery is the query of the last Rows call on path (tests: was a proplist asked for).
func (m *Mock) LastQuery(path string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastQ[cleanPath(path)]
}

// SetReadFail makes only the reads (Rows) of path fail with err, writes keep working. nil removes it.
func (m *Mock) SetReadFail(path string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.failReads, cleanPath(path))
		return
	}
	if m.failReads == nil {
		m.failReads = map[string]error{}
	}
	m.failReads[cleanPath(path)] = err
}
