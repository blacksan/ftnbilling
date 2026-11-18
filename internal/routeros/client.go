// Package routeros talks to a MikroTik router's hotspot via the v7 REST API.
package routeros

import "context"

// Profile is a hotspot user profile.
type Profile struct {
	Name        string
	RateLimit   string // e.g. "10M/10M"
	SharedUsers int
}

// User is a hotspot user.
type User struct {
	Name     string
	Password string
	Profile  string
	MAC      string // optional; binds the user to one device
	Comment  string
}

// Client is everything ispbill needs from the router.
type Client interface {
	Ping(ctx context.Context) error
	EnsureProfile(ctx context.Context, p Profile) error
	EnsureUser(ctx context.Context, u User) error
	RemoveUser(ctx context.Context, name string) error
	RemoveProfile(ctx context.Context, name string) error
	KickUser(ctx context.Context, name string) error
	Login(ctx context.Context, user, password, mac, ip string) error
	// Hosts lists /ip/hotspot/host rows (authorised or not).
	Hosts(ctx context.Context) ([]Host, error)
	Telemetry
}

// Host is one /ip/hotspot/host row: a device the hotspot has seen.
type Host struct {
	MAC, Address, HostName string
	Authorized, Bypassed   bool
}
