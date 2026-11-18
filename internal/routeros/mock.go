package routeros

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Mock is an in-memory fake router for ROUTER_MOCK=true and tests.
type Mock struct {
	mu       sync.Mutex
	profiles map[string]Profile
	users    map[string]User
	active   map[string]string // user -> ip
	fail     error
	ms       *mockState
	hosts    []Host // extra (unauthorised) hosts for demos and tests
	// Kicks records the names passed to KickUser (including no-op kicks).
	Kicks []string
	// Executed records every command passed to Execute.
	Executed []string
	// ExecOut, when set, answers Execute instead of the built-in samples.
	ExecOut func(cmd string) (string, error)
	leases  []Lease
}

func NewMock() *Mock {
	return &Mock{profiles: map[string]Profile{}, users: map[string]User{}, active: map[string]string{}}
}

// SetFail makes every call return err (nil restores normal behaviour).
func (m *Mock) SetFail(err error) { m.mu.Lock(); m.fail = err; m.mu.Unlock() }

func (m *Mock) Ping(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fail
}

func (m *Mock) EnsureProfile(_ context.Context, p Profile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.profiles[p.Name] = p
	return nil
}

func (m *Mock) EnsureUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if _, ok := m.profiles[u.Profile]; !ok {
		return &APIError{Status: 400, Message: "input does not match any value of profile"}
	}
	if old, ok := m.users[u.Name]; ok && u.MAC == "" {
		u.MAC = old.MAC // like PATCH: omitted fields stay as they were
	}
	m.users[u.Name] = u
	return nil
}

func (m *Mock) RemoveUser(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	delete(m.users, name)
	return nil
}

func (m *Mock) RemoveProfile(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	delete(m.profiles, name)
	return nil
}

func (m *Mock) KickUser(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.Kicks = append(m.Kicks, name)
	delete(m.active, name)
	return nil
}

func (m *Mock) Login(_ context.Context, user, password, mac, ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	u, ok := m.users[user]
	if !ok || u.Password != password {
		return &APIError{Status: 400, Message: "failure: invalid username or password"}
	}
	if u.MAC != "" && mac != "" && !strings.EqualFold(u.MAC, mac) {
		return &APIError{Status: 400, Message: "failure: user is bound to another mac-address"}
	}
	if _, on := m.active[user]; on {
		return &APIError{Status: 400, Message: "failure: already logged in"}
	}
	if ip == "" {
		return errors.New("mock: ip required")
	}
	m.active[user] = ip
	return nil
}

// AddHost adds an unauthorised hotspot host (a device that has not logged in).
func (m *Mock) AddHost(mac, ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hosts = append(m.hosts, Host{MAC: mac, Address: ip})
}

// Hosts returns authorised hosts (active sessions) plus any added with AddHost.
func (m *Mock) Hosts(context.Context) ([]Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []Host
	for user, ip := range m.active {
		out = append(out, Host{MAC: m.users[user].MAC, Address: ip, Authorized: true})
	}
	return append(out, m.hosts...), nil
}

// Inspection helpers for tests and the UI.

func (m *Mock) GetUser(name string) (User, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[name]
	return u, ok
}

func (m *Mock) GetProfile(name string) (Profile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles[name]
	return p, ok
}

func (m *Mock) IsActive(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[name]
	return ok
}

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
		return "0 address=10.5.50.1/24 interface=bridge-hotspot\n1 address=10.99.0.2/22 interface=wg-ftn", nil
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
