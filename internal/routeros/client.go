// Package routeros talks to a MikroTik router via the v7 REST API.
package routeros

import "context"

// Client is everything ispbill needs from the router.
type Client interface {
	Ping(ctx context.Context) error
	// Generic row API for any RouterOS table (path like "/ip/dhcp-server/lease").
	// The reconciler uses it; see Row.
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
