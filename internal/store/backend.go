package store

import (
	"context"
	"errors"
	"fmt"
)

// Errors returned by Update.
var (
	// ErrPersist: the change could not be made durable. Memory was rolled
	// back, so nothing happened. It wraps the backend's error.
	ErrPersist = errors.New("could not save to the database")
	// ErrUnavailable: the store lost its database and has not recovered yet.
	// Reads still work from memory; writes are refused.
	ErrUnavailable = errors.New("the database is unavailable")
	// ErrAmbiguous is wrapped by a backend when it cannot tell whether a
	// commit happened (the connection broke during COMMIT). The store then
	// reloads everything from the backend to find out.
	ErrAmbiguous = errors.New("commit outcome unknown")
	// ErrIntegrity is wrapped by a backend when the database refused a change
	// because it breaks an integrity rule (SQLSTATE class 23: unique, foreign
	// key, check, not null). The change was wrong, not the database: Update
	// rolls back, returns the error, and never counts it towards degrading.
	ErrIntegrity = errors.New("the database refused the change")
	// ErrStaleWrite is an integrity refusal: the sign-in row this process
	// tried to write is older than the one in the database (another process,
	// such as the admin-reset command, changed it). The write is dropped so a
	// stale copy can never undo a password, 2FA or revoke change. Restart the
	// service to load the newer rows.
	ErrStaleWrite = fmt.Errorf("%w: a newer copy of this sign-in row is in the database (changed by another process); restart the service", ErrIntegrity)
	// ErrConnection is wrapped by a backend when a commit failed because the
	// database could not be reached or stopped answering (as opposed to the
	// database refusing the change). The store turns degraded at once.
	ErrConnection = errors.New("database connection failed")
)

// Backend makes a Store durable.
type Backend interface {
	// Kind is "json" or "postgres".
	Kind() string
	// Load returns the whole database as of now.
	Load(ctx context.Context) (*Data, error)
	// Commit makes one Update durable. cs lists exactly what changed since
	// the last successful commit; d is the new complete state (the JSON
	// backend writes d, a database writes cs).
	Commit(ctx context.Context, d *Data, cs *ChangeSet) error
	Close() error
}

// EventQuery selects events from the full history.
type EventQuery struct {
	SiteID, AccountID, CustomerID, EquipmentID string
	Kind                                       string
	BeforeSeq                                  int64 // 0 = from the newest
	Limit                                      int   // 0 = 100
}

// EventReader is implemented by backends that keep every event.
type EventReader interface {
	Events(ctx context.Context, q EventQuery) ([]Event, error)
}

// Delta is the change to one table: rows to insert or update, and keys to delete.
type Delta[V any] struct {
	Upsert []V
	Delete []string
	// Fresh holds the keys of Upsert rows that are not in the last committed
	// state (brand-new rows); every other upserted row is an update. A
	// database uses it to send updates before inserts, so a partial unique
	// index (one open tenancy per account) never sees two rows at once.
	Fresh map[string]bool
	// Old holds the previous version of updated rows, for tables where a
	// backend needs it to order writes (accounts only).
	Old map[string]V
}

// Empty reports whether the delta changes nothing.
func (d Delta[V]) Empty() bool { return len(d.Upsert) == 0 && len(d.Delete) == 0 }

// ChangeSet is everything one Update changed, per table. Accounts covers
// open and closed accounts together (Account.ClosedAt tells which).
type ChangeSet struct {
	Sites           Delta[Site]
	Packages        Delta[Package]
	Customers       Delta[Customer]
	Equipment       Delta[Equipment]
	Accounts        Delta[Account]
	Tenancies       Delta[Tenancy]
	Apartments      Delta[Apartment]
	ApartmentSites  Delta[ApartmentSite]
	Orders          Delta[Order]
	Vouchers        Delta[Voucher]
	Payments        Delta[Payment]
	Devices         Delta[Device]
	Tasks           Delta[Task]
	EquipmentEvents Delta[EquipmentEvent]
	Notes           Delta[Note]
	Settings        Delta[Setting]
	AdminUsers      Delta[AdminUser]
	AdminSessions   Delta[AdminSession]
	APIDevices      Delta[APIDevice]
	Registrations   Delta[Registration]
	ONUs            Delta[ONU]
	ONUTasks        Delta[ONUTask]
	OUICache        Delta[OUIEntry]
	NewEvents       []Event
}

// Empty reports whether nothing changed.
func (c *ChangeSet) Empty() bool {
	return c.Sites.Empty() && c.Packages.Empty() && c.Customers.Empty() && c.Equipment.Empty() &&
		c.Accounts.Empty() && c.Tenancies.Empty() && c.Apartments.Empty() && c.ApartmentSites.Empty() && c.Orders.Empty() &&
		c.Vouchers.Empty() && c.Payments.Empty() && c.Devices.Empty() && c.Tasks.Empty() &&
		c.EquipmentEvents.Empty() && c.Notes.Empty() && c.Settings.Empty() &&
		c.AdminUsers.Empty() && c.AdminSessions.Empty() && c.APIDevices.Empty() && c.Registrations.Empty() && c.ONUs.Empty() && c.ONUTasks.Empty() && c.OUICache.Empty() && len(c.NewEvents) == 0
}

// Summary is a short human description (counts only, never row contents).
func (c *ChangeSet) Summary() string {
	n := func(name string, up, del int) string {
		if up == 0 && del == 0 {
			return ""
		}
		return fmt.Sprintf(" %s+%d/-%d", name, up, del)
	}
	s := n("sites", len(c.Sites.Upsert), len(c.Sites.Delete)) + n("packages", len(c.Packages.Upsert), len(c.Packages.Delete)) +
		n("customers", len(c.Customers.Upsert), len(c.Customers.Delete)) + n("equipment", len(c.Equipment.Upsert), len(c.Equipment.Delete)) +
		n("accounts", len(c.Accounts.Upsert), len(c.Accounts.Delete)) + n("tenancies", len(c.Tenancies.Upsert), len(c.Tenancies.Delete)) +
		n("apartments", len(c.Apartments.Upsert), len(c.Apartments.Delete)) + n("apartment_sites", len(c.ApartmentSites.Upsert), len(c.ApartmentSites.Delete)) + n("orders", len(c.Orders.Upsert), len(c.Orders.Delete)) +
		n("vouchers", len(c.Vouchers.Upsert), len(c.Vouchers.Delete)) + n("payments", len(c.Payments.Upsert), len(c.Payments.Delete)) +
		n("devices", len(c.Devices.Upsert), len(c.Devices.Delete)) + n("tasks", len(c.Tasks.Upsert), len(c.Tasks.Delete)) +
		n("equipment_events", len(c.EquipmentEvents.Upsert), len(c.EquipmentEvents.Delete)) + n("notes", len(c.Notes.Upsert), len(c.Notes.Delete)) +
		n("settings", len(c.Settings.Upsert), len(c.Settings.Delete)) +
		n("admin_users", len(c.AdminUsers.Upsert), len(c.AdminUsers.Delete)) + n("admin_sessions", len(c.AdminSessions.Upsert), len(c.AdminSessions.Delete)) +
		n("api_devices", len(c.APIDevices.Upsert), len(c.APIDevices.Delete)) +
		n("registrations", len(c.Registrations.Upsert), len(c.Registrations.Delete)) +
		n("onus", len(c.ONUs.Upsert), len(c.ONUs.Delete)) + n("onu_tasks", len(c.ONUTasks.Upsert), len(c.ONUTasks.Delete)) + n("oui_cache", len(c.OUICache.Upsert), len(c.OUICache.Delete))
	if len(c.NewEvents) > 0 {
		s += fmt.Sprintf(" events+%d", len(c.NewEvents))
	}
	return s
}
