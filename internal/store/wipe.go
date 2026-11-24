package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// "Start fresh": delete every business entry and keep the configuration.
//
// Deleted: routers and everything tied to them (join tokens, tunnel keys and
// addresses, join reports and the other "site.<id>.*" settings), packages,
// customers, flats (open and closed), tenancies, equipment and its history,
// apartments (with their router links and contacts), orders, vouchers, payments (M-Pesa included), devices and
// leases, tasks, the event/audit log, CRM notes, admin browser sessions and
// phone sessions (everybody signs in again).
//
// Kept: admin users (passwords, authenticator secrets, recovery codes), every
// other setting (M-Pesa credentials, branding, alert and message options,
// known browsers, the admin password hash), the schema version and the format.
// DATA_KEY lives in the environment file and is never touched.

// Wipe report kinds, in print order. Both backends report these names.
const (
	WipeRouters         = "routers"
	WipePackages        = "packages"
	WipeCustomers       = "customers"
	WipeAccounts        = "accounts (open and closed)"
	WipeTenancies       = "tenancies"
	WipeEquipment       = "equipment"
	WipeEquipmentEvents = "equipment history"
	WipeApartments      = "apartments and their router links"
	WipeOrders          = "orders"
	WipeVouchers        = "vouchers"
	WipePayments        = "payments"
	WipeDevices         = "devices and leases"
	WipeRegistrations   = "self-registrations"
	WipeONUs            = "ONUs and their TR-069 tasks"
	WipeTasks           = "tasks"
	WipeEvents          = "events and audit log"
	WipeNotes           = "CRM notes"
	WipeRouterSettings  = "router settings and join reports"
	WipeSessions        = "admin browser sessions"
	WipePhones          = "phone sessions"
)

// WipeItem is how many entries of one kind a wipe deletes.
type WipeItem struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// WipeReport says what a wipe deletes (Delete, in print order) and what stays.
type WipeReport struct {
	Delete        []WipeItem `json:"delete"`
	KeptAdminUser int        `json:"kept_admin_users"`
	KeptSettings  int        `json:"kept_settings"`
}

// Total is the number of entries deleted.
func (r WipeReport) Total() int {
	n := 0
	for _, it := range r.Delete {
		n += it.Count
	}
	return n
}

// IsRouterSetting reports whether a settings key belongs to one router
// ("site.<id>.<name>": the join report and the old per-router job state).
func IsRouterSetting(key string) bool { return strings.HasPrefix(key, "site.") }

// WipeReport counts what WipeBusiness would delete in d.
func (d *Data) WipeReport() WipeReport {
	rs := 0
	for k := range d.Settings {
		if IsRouterSetting(k) {
			rs++
		}
	}
	return WipeReport{
		Delete: []WipeItem{
			{WipeRouters, len(d.Sites)}, {WipePackages, len(d.Packages)}, {WipeCustomers, len(d.Customers)},
			{WipeAccounts, len(d.Accounts) + len(d.ClosedAccounts)}, {WipeTenancies, len(d.Tenancies)},
			{WipeEquipment, len(d.Equipment) + len(d.OLTStates)}, {WipeEquipmentEvents, len(d.EquipmentEvents)}, {WipeApartments, len(d.Apartments) + len(d.ApartmentSites)},
			{WipeOrders, len(d.Orders)}, {WipeVouchers, len(d.Vouchers)}, {WipePayments, len(d.Payments)},
			{WipeDevices, len(d.Devices)}, {WipeRegistrations, len(d.Registrations)}, {WipeONUs, len(d.ONUs) + len(d.ONUTasks)}, {WipeTasks, len(d.Tasks)}, {WipeEvents, len(d.Events)}, {WipeNotes, len(d.Notes)},
			{WipeRouterSettings, rs}, {WipeSessions, len(d.AdminSessions)}, {WipePhones, len(d.APIDevices)},
		},
		KeptAdminUser: len(d.AdminUsers),
		KeptSettings:  len(d.Settings) - rs,
	}
}

// WipeBusiness deletes the business data of d in place and keeps admin users
// and settings (see the comment at the top of this file).
func (d *Data) WipeBusiness() {
	keep := map[string]Setting{}
	for k, s := range d.Settings {
		if !IsRouterSetting(k) {
			keep[k] = s
		}
	}
	users := d.AdminUsers
	format := d.Format
	*d = Data{Format: format, AdminUsers: users, Settings: keep}
	d.init()
}

// WipeJSON reads the JSON database in dir and reports what a wipe deletes. With
// apply it also writes the emptied database back atomically (temp file, fsync,
// rename). A missing file is an empty database and is not created.
func WipeJSON(dir string, apply bool) (WipeReport, error) {
	path := filepath.Join(dir, "ispbill.json")
	if !apply {
		// Read-only: nothing is written or backed up, so it is safe while the service runs.
		d, _, err := ReadJSON(path)
		if errors.Is(err, os.ErrNotExist) {
			d = &Data{}
			d.init()
		} else if err != nil {
			return WipeReport{}, err
		}
		return d.WipeReport(), nil
	}
	b := &jsonBackend{path: path}
	d, err := b.Load(context.Background())
	if err != nil {
		return WipeReport{}, err
	}
	rep := d.WipeReport()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	d.WipeBusiness()
	return rep, b.write(d)
}
