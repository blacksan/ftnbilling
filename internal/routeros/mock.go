package routeros

import (
	"context"
	"errors"
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
