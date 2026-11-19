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
	// Generic row API for any RouterOS table (path like "/ip/dhcp-server/lease").
	// The IPoE reconciler uses it; see Row.
	//
	// Rows lists the rows whose properties equal every key/value in query
	// (nil = all rows; values compare case-insensitively, so MACs match).
	Rows(ctx context.Context, path string, query map[string]string) ([]Row, error)
	// RowsProps is Rows returning only the listed properties (RouterOS
	// .proplist); the mock and NoRouter ignore props.
	RowsProps(ctx context.Context, path string, query map[string]string, props []string) ([]Row, error)
	// AddRow creates a row and returns its .id.
	AddRow(ctx context.Context, path string, body map[string]string) (id string, err error)
	// SetRow changes only the properties in diff on the row with .id id.
	SetRow(ctx context.Context, path, id string, diff map[string]string) error
	// RemoveRow deletes the row with .id id. A row that is already gone is not an error.
	RemoveRow(ctx context.Context, path, id string) error
	Telemetry
}

// Row is one row of a RouterOS table: property name to value, with the row's
// identifier under RowID.
type Row map[string]string

// RowID is the key of a Row's identifier (".id", like *1A).
const RowID = ".id"

// Host is one /ip/hotspot/host row: a device the hotspot has seen.
type Host struct {
	MAC, Address, HostName string
	Authorized, Bypassed   bool
}
