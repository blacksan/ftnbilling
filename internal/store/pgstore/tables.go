package pgstore

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"ispbill/internal/store"
)

// This file holds one hand-written codec per table. Conventions:
//   - a Go string is "text NOT NULL DEFAULT ''"; a foreign key column stores
//     NULL for "" (fk / nfk);
//   - a nullable time column stores NULL for the zero time (nt); a NOT NULL
//     time column stores the zero time as year 1 and reads it back as zero (tm);
//   - times are UTC at microsecond precision.
// Two guard tests catch a forgotten column: codec_test.go perturbs every Go
// field and requires the arguments to change, and round-trips a fully
// populated row through the database.

// fk is a foreign key value: "" becomes NULL.
func fk(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nt is a nullable time: the zero time becomes NULL.
func nt(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Truncate(time.Microsecond)
}

// tm is a NOT NULL time.
func tm(t time.Time) any {
	if t.IsZero() {
		return time.Time{}.UTC()
	}
	return t.UTC().Truncate(time.Microsecond)
}

// strDst scans text or NULL into a string ("" for NULL).
type strDst struct{ p *string }

func (d strDst) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*d.p = ""
	case string:
		*d.p = x
	case []byte:
		*d.p = string(x)
	default:
		return fmt.Errorf("pgstore: cannot scan %T into string", v)
	}
	return nil
}

// timeDst scans timestamptz or NULL into a time.Time (zero for NULL).
type timeDst struct{ p *time.Time }

func (d timeDst) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*d.p = time.Time{}
	case time.Time:
		*d.p = store.NormTime(x)
	default:
		return fmt.Errorf("pgstore: cannot scan %T into time", v)
	}
	return nil
}

// jsonDst scans jsonb into the overrides map.
type overridesDst struct{ p *map[string]store.PkgOverride }

func (d overridesDst) Scan(v any) error {
	var raw []byte
	switch x := v.(type) {
	case nil:
		*d.p = nil
		return nil
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	default:
		return fmt.Errorf("pgstore: cannot scan %T into jsonb", v)
	}
	var m map[string]store.PkgOverride
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if len(m) == 0 {
		m = nil
	}
	*d.p = m
	return nil
}

func s_(p *string) any    { return strDst{p} }
func t_(p *time.Time) any { return timeDst{p} }

// codec describes one table.
type codec[V any] struct {
	name  string
	cols  []string
	pk    []string
	del   string // SQL expression of the key matched by Delete (default: pk[0])
	key   func(V) string
	args  func(V) []any
	dst   func(*V) []any
	upsql string
	// guard, when set, is a condition (on the existing row and EXCLUDED) that
	// must hold for an update to apply; a write that fails it changes nothing
	// and Commit reports store.ErrStaleWrite.
	guard string
}

func (c *codec[V]) build() *codec[V] {
	ph := make([]string, len(c.cols))
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	isPK := map[string]bool{}
	for _, k := range c.pk {
		isPK[k] = true
	}
	var sets []string
	for _, col := range c.cols {
		if !isPK[col] {
			sets = append(sets, col+" = EXCLUDED."+col)
		}
	}
	c.upsql = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		c.name, strings.Join(c.cols, ", "), strings.Join(ph, ", "), strings.Join(c.pk, ", "), strings.Join(sets, ", "))
	if len(sets) == 0 {
		c.upsql = strings.Replace(c.upsql, " DO UPDATE SET ", " DO NOTHING", 1)
	} else if c.guard != "" {
		c.upsql += " WHERE " + c.guard
	}
	if c.del == "" {
		c.del = c.pk[0]
	}
	return c
}

func (c *codec[V]) selectSQL(where string) string {
	q := "SELECT " + strings.Join(c.cols, ", ") + " FROM " + c.name
	if where != "" {
		q += " WHERE " + where
	}
	return q
}

// queueUpserts queues one delta's upserts in a deterministic order: updates
// of rows that already exist first, then inserts of brand-new rows, each group
// sorted by key. Partial unique indexes cannot be deferred, so a row that
// gives up a slot (closing the open tenancy) must be written before the row
// that takes it (the new open tenancy). Go map order used to decide this.
func (c *codec[V]) queueUpserts(b *pgx.Batch, d store.Delta[V]) {
	rows := append([]V(nil), d.Upsert...)
	sort.SliceStable(rows, func(i, j int) bool {
		ki, kj := c.key(rows[i]), c.key(rows[j])
		fi, fj := d.Fresh[ki], d.Fresh[kj]
		if fi != fj {
			return !fi // updates before inserts
		}
		return ki < kj
	})
	for _, r := range rows {
		b.Queue(c.upsql, c.args(r)...)
	}
}

func (c *codec[V]) queueDeletes(b *pgx.Batch, keys []string) {
	if len(keys) > 0 {
		b.Queue("DELETE FROM "+c.name+" WHERE "+c.del+" = ANY($1)", keys)
	}
}

func (c *codec[V]) scan(rows pgx.Rows) ([]V, error) {
	defer rows.Close()
	var out []V
	for rows.Next() {
		var v V
		if err := rows.Scan(c.dst(&v)...); err != nil {
			return nil, fmt.Errorf("%s: %w", c.name, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// accessMode writes the empty mode as the default, "ipoe" (the Store
// normalises it the same way; this keeps the database constraint satisfied for
// callers that bypass the Store).
func accessMode(m string) string {
	if m == "" {
		return store.AccessIPoE
	}
	return m
}

// stringsArg is a text[]-like list stored as a jsonb array.
func stringsArg(l []string) any {
	if len(l) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// stringsDst scans a jsonb array of strings (nil when empty).
type stringsDst struct{ p *[]string }

func (d stringsDst) Scan(v any) error {
	var raw []byte
	switch x := v.(type) {
	case nil:
		*d.p = nil
		return nil
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	default:
		return fmt.Errorf("pgstore: cannot scan %T into jsonb", v)
	}
	var l []string
	if err := json.Unmarshal(raw, &l); err != nil {
		return err
	}
	if len(l) == 0 {
		l = nil
	}
	*d.p = l
	return nil
}

func overridesArg(m map[string]store.PkgOverride) any {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

var sitesC = (&codec[store.Site]{
	name: "sites", pk: []string{"id"}, key: func(v store.Site) string { return v.ID },
	cols: []string{"id", "name", "mode", "notes", "support_phone", "disabled", "removed", "tunnel_ip", "wg_pub", "rest_user", "rest_pass", "rest_port",
		"join_hash", "join_expires_at", "join_created_at", "join_used_at", "join_used_from", "joined_at", "wan_interface", "wan_list",
		"olt_interface", "vlan_id", "customer_subnet", "wan_cap_mbps", "olt_cap_mbps", "fup_tb", "bench_port",
		"pkg_overrides", "address", "lat", "lng", "go_live", "access_mode", "created_at",
		"wan_mode", "wan_address", "wan_gateway", "wan_dns", "pppoe_user", "pppoe_pass_enc", "lan_ports", "lan_subnet", "customer_ports", "disabled_ports", "ports_policy", "enabled_ports", "onu_mgmt_n", "onu_mgmt_vlan"},
	args: func(v store.Site) []any {
		return []any{v.ID, v.Name, v.Mode, v.Notes, v.SupportPhone, v.Disabled, v.Removed, v.TunnelIP, v.WGPublicKey, v.RESTUser, v.RESTPass, v.RESTPort,
			v.Join.Hash, nt(v.Join.ExpiresAt), nt(v.Join.CreatedAt), nt(v.Join.UsedAt), v.Join.UsedFrom, nt(v.JoinedAt), v.WANInterface, v.WANList,
			v.OLTInterface, v.VLANID, v.CustomerSubnet, v.WANCapMbps, v.OLTCapMbps, v.FUPTB, v.BenchPort,
			overridesArg(v.PkgOverrides), v.Address, v.Lat, v.Lng, nt(v.GoLive), accessMode(v.AccessMode), tm(v.CreatedAt),
			v.WANMode, v.WANAddress, v.WANGateway, v.WANDNS, v.PPPoEUser, v.PPPoEPassEnc, v.LANPorts, v.LANSubnet, v.CustomerPorts, v.DisabledPorts, v.PortsPolicy, v.EnabledPorts, v.ONUMgmtN, v.ONUMgmtVLAN}
	},
	dst: func(v *store.Site) []any {
		return []any{&v.ID, &v.Name, &v.Mode, &v.Notes, &v.SupportPhone, &v.Disabled, &v.Removed, &v.TunnelIP, &v.WGPublicKey, &v.RESTUser, &v.RESTPass, &v.RESTPort,
			&v.Join.Hash, t_(&v.Join.ExpiresAt), t_(&v.Join.CreatedAt), t_(&v.Join.UsedAt), &v.Join.UsedFrom, t_(&v.JoinedAt), &v.WANInterface, &v.WANList,
			&v.OLTInterface, &v.VLANID, &v.CustomerSubnet, &v.WANCapMbps, &v.OLTCapMbps, &v.FUPTB, &v.BenchPort,
			overridesDst{&v.PkgOverrides}, &v.Address, &v.Lat, &v.Lng, t_(&v.GoLive), &v.AccessMode, t_(&v.CreatedAt),
			&v.WANMode, &v.WANAddress, &v.WANGateway, &v.WANDNS, &v.PPPoEUser, &v.PPPoEPassEnc, &v.LANPorts, &v.LANSubnet, &v.CustomerPorts, &v.DisabledPorts, &v.PortsPolicy, &v.EnabledPorts, &v.ONUMgmtN, &v.ONUMgmtVLAN}
	},
}).build()

var packagesC = (&codec[store.Package]{
	name: "packages", pk: []string{"id"}, key: func(v store.Package) string { return v.ID },
	cols: []string{"id", "name", "slug", "duration_min", "speed_mbps", "price_kes", "disabled", "deleted", "sort"},
	args: func(v store.Package) []any {
		return []any{v.ID, v.Name, v.Slug, v.DurationMin, v.SpeedMbps, v.PriceKES, v.Disabled, v.Deleted, v.Sort}
	},
	dst: func(v *store.Package) []any {
		return []any{&v.ID, &v.Name, &v.Slug, &v.DurationMin, &v.SpeedMbps, &v.PriceKES, &v.Disabled, &v.Deleted, &v.Sort}
	},
}).build()

var customersC = (&codec[store.Customer]{
	name: "customers", pk: []string{"id"}, key: func(v store.Customer) string { return v.ID },
	cols: []string{"id", "name", "phone", "phone2", "whatsapp", "wa_opt_out", "email", "notes", "national_id_enc", "national_id_hmac", "national_id_last4",
		"anonymized_at", "created_at", "updated_at", "wa_same_phone"},
	args: func(v store.Customer) []any {
		return []any{v.ID, v.Name, v.Phone, v.Phone2, v.WhatsApp, v.WAOptOut, v.Email, v.Notes, v.NationalIDEnc, v.NationalIDHMAC, v.NationalIDLast4,
			nt(v.AnonymizedAt), tm(v.CreatedAt), nt(v.UpdatedAt), v.WASamePhone}
	},
	dst: func(v *store.Customer) []any {
		return []any{&v.ID, &v.Name, &v.Phone, &v.Phone2, &v.WhatsApp, &v.WAOptOut, &v.Email, &v.Notes, &v.NationalIDEnc, &v.NationalIDHMAC, &v.NationalIDLast4,
			t_(&v.AnonymizedAt), t_(&v.CreatedAt), t_(&v.UpdatedAt), &v.WASamePhone}
	},
}).build()

var equipmentC = (&codec[store.Equipment]{
	name: "equipment", pk: []string{"id"}, key: func(v store.Equipment) string { return v.ID },
	cols: []string{"id", "kind", "serial", "mac", "model", "firmware", "status", "site_id", "account_id", "installed_at", "notes", "created_at", "updated_at", "gpon_serial",
		"name", "router_port", "pon_ports", "pon_apartments", "mgmt_ip", "mgmt_user", "mgmt_pass", "olt_id", "pon_port"},
	args: func(v store.Equipment) []any {
		return []any{v.ID, v.Kind, v.Serial, v.MAC, v.Model, v.Firmware, v.Status, fk(v.SiteID), fk(v.AccountID), nt(v.InstalledAt), v.Notes, tm(v.CreatedAt), nt(v.UpdatedAt), v.GPONSerial,
			v.Name, v.RouterPort, v.PONPorts, jsonArg(v.PONApartments, "{}"), v.MgmtIP, v.MgmtUser, v.MgmtPass, v.OLTID, v.PONPort}
	},
	dst: func(v *store.Equipment) []any {
		return []any{&v.ID, &v.Kind, &v.Serial, &v.MAC, &v.Model, &v.Firmware, &v.Status, s_(&v.SiteID), s_(&v.AccountID), t_(&v.InstalledAt), &v.Notes, t_(&v.CreatedAt), t_(&v.UpdatedAt), &v.GPONSerial,
			&v.Name, &v.RouterPort, &v.PONPorts, jsonScan[map[string][]string]{&v.PONApartments}, &v.MgmtIP, &v.MgmtUser, &v.MgmtPass, &v.OLTID, &v.PONPort}
	},
}).build()

var accountsC = (&codec[store.Account]{
	name: "accounts", pk: []string{"id"}, key: func(v store.Account) string { return v.ID },
	cols: []string{"id", "site_id", "code", "flat", "floor", "mac", "tenant", "phone", "notes", "whatsapp", "wa_opt_out", "customer_id", "package_id",
		"expires_at", "suspended", "reminded_for", "expired_for", "odb", "splitter_id", "splitter_port", "created_at", "closed_at",
		"mac_conflict", "ip", "apartment_id", "unit", "wa_same_phone", "home_wifi"},
	args: func(v store.Account) []any {
		return []any{v.ID, v.SiteID, v.Code, v.Flat, v.Floor, v.MAC, v.Tenant, v.Phone, v.Notes, v.WhatsApp, v.WAOptOut, fk(v.CustomerID), fk(v.PackageID),
			nt(v.ExpiresAt), v.Suspended, nt(v.RemindedFor), nt(v.ExpiredFor), v.ODB, fk(v.SplitterID), v.SplitterPort, tm(v.CreatedAt), nt(v.ClosedAt),
			v.MACConflict, v.IP, fk(v.ApartmentID), v.Unit, v.WASamePhone, jsonArg(v.HomeWiFi, "{}")}
	},
	dst: func(v *store.Account) []any {
		return []any{&v.ID, &v.SiteID, &v.Code, &v.Flat, &v.Floor, &v.MAC, &v.Tenant, &v.Phone, &v.Notes, &v.WhatsApp, &v.WAOptOut, s_(&v.CustomerID), s_(&v.PackageID),
			t_(&v.ExpiresAt), &v.Suspended, t_(&v.RemindedFor), t_(&v.ExpiredFor), &v.ODB, s_(&v.SplitterID), &v.SplitterPort, t_(&v.CreatedAt), t_(&v.ClosedAt),
			&v.MACConflict, &v.IP, s_(&v.ApartmentID), &v.Unit, &v.WASamePhone, jsonScan[store.WiFiDesired]{&v.HomeWiFi}}
	},
}).build()

var tenanciesC = (&codec[store.Tenancy]{
	name: "tenancies", pk: []string{"id"}, key: func(v store.Tenancy) string { return v.ID },
	cols: []string{"id", "customer_id", "account_id", "move_in", "move_out", "note"},
	args: func(v store.Tenancy) []any {
		return []any{v.ID, v.CustomerID, v.AccountID, tm(v.MoveIn), nt(v.MoveOut), v.Note}
	},
	dst: func(v *store.Tenancy) []any {
		return []any{&v.ID, &v.CustomerID, &v.AccountID, t_(&v.MoveIn), t_(&v.MoveOut), &v.Note}
	},
}).build()

var apartmentsC = (&codec[store.Apartment]{
	name: "apartments", pk: []string{"id"}, key: func(v store.Apartment) string { return v.ID },
	cols: []string{"id", "name", "address", "notes",
		"caretaker_name", "caretaker_phone", "caretaker_phone2", "caretaker_email", "caretaker_notes",
		"landlord_name", "landlord_phone", "landlord_phone2", "landlord_email", "landlord_notes", "created_at", "lat", "lng"},
	args: func(v store.Apartment) []any {
		return []any{v.ID, v.Name, v.Address, v.Notes,
			v.Caretaker.Name, v.Caretaker.Phone, v.Caretaker.Phone2, v.Caretaker.Email, v.Caretaker.Notes,
			v.Landlord.Name, v.Landlord.Phone, v.Landlord.Phone2, v.Landlord.Email, v.Landlord.Notes, tm(v.CreatedAt), v.Lat, v.Lng}
	},
	dst: func(v *store.Apartment) []any {
		return []any{&v.ID, &v.Name, &v.Address, &v.Notes,
			&v.Caretaker.Name, &v.Caretaker.Phone, &v.Caretaker.Phone2, &v.Caretaker.Email, &v.Caretaker.Notes,
			&v.Landlord.Name, &v.Landlord.Phone, &v.Landlord.Phone2, &v.Landlord.Email, &v.Landlord.Notes, t_(&v.CreatedAt), &v.Lat, &v.Lng}
	},
}).build()

var apartmentSitesC = (&codec[store.ApartmentSite]{
	name: "apartment_sites", pk: []string{"apartment_id", "site_id"}, del: "apartment_id || '|' || site_id",
	key:  func(v store.ApartmentSite) string { return store.ApartmentSiteKey(v.ApartmentID, v.SiteID) },
	cols: []string{"apartment_id", "site_id", "created_at"},
	args: func(v store.ApartmentSite) []any { return []any{v.ApartmentID, v.SiteID, tm(v.CreatedAt)} },
	dst:  func(v *store.ApartmentSite) []any { return []any{&v.ApartmentID, &v.SiteID, t_(&v.CreatedAt)} },
}).build()

var ordersC = (&codec[store.Order]{
	name: "orders", pk: []string{"id"}, key: func(v store.Order) string { return v.ID },
	cols: []string{"id", "site_id", "account_id", "package_id", "phone", "method", "amount_kes", "status", "reason", "checkout_request_id", "receipt",
		"voucher_code", "created_at", "paid_at"},
	args: func(v store.Order) []any {
		return []any{v.ID, fk(v.SiteID), fk(v.AccountID), fk(v.PackageID), v.Phone, v.Method, v.AmountKES, v.Status, v.Reason, v.CheckoutRequestID, v.Receipt,
			v.VoucherCode, tm(v.CreatedAt), nt(v.PaidAt)}
	},
	dst: func(v *store.Order) []any {
		return []any{&v.ID, s_(&v.SiteID), s_(&v.AccountID), s_(&v.PackageID), &v.Phone, &v.Method, &v.AmountKES, &v.Status, &v.Reason, &v.CheckoutRequestID, &v.Receipt,
			&v.VoucherCode, t_(&v.CreatedAt), t_(&v.PaidAt)}
	},
}).build()

var vouchersC = (&codec[store.Voucher]{
	name: "vouchers", pk: []string{"code"}, key: func(v store.Voucher) string { return v.Code },
	cols: []string{"code", "site_id", "package_id", "batch", "used", "used_at", "used_by", "created_at"},
	args: func(v store.Voucher) []any {
		return []any{v.Code, fk(v.SiteID), v.PackageID, v.Batch, v.Used, nt(v.UsedAt), v.UsedBy, tm(v.CreatedAt)}
	},
	dst: func(v *store.Voucher) []any {
		return []any{&v.Code, s_(&v.SiteID), &v.PackageID, &v.Batch, &v.Used, t_(&v.UsedAt), &v.UsedBy, t_(&v.CreatedAt)}
	},
}).build()

var paymentsC = (&codec[store.Payment]{
	name: "payments", pk: []string{"key"}, key: func(v store.Payment) string { return v.Key },
	cols: []string{"key", "site_id", "method", "status", "amount_kes", "phone", "name", "bill_ref", "account_id", "customer_id", "package_id", "order_id", "note",
		"paid_at", "created_at", "resolved_at"},
	args: func(v store.Payment) []any {
		return []any{v.Key, fk(v.SiteID), v.Method, v.Status, v.AmountKES, v.Phone, v.Name, v.BillRef, fk(v.AccountID), fk(v.CustomerID), fk(v.PackageID), v.OrderID, v.Note,
			tm(v.PaidAt), tm(v.CreatedAt), nt(v.ResolvedAt)}
	},
	dst: func(v *store.Payment) []any {
		return []any{&v.Key, s_(&v.SiteID), &v.Method, &v.Status, &v.AmountKES, &v.Phone, &v.Name, &v.BillRef, s_(&v.AccountID), s_(&v.CustomerID), s_(&v.PackageID), &v.OrderID, &v.Note,
			t_(&v.PaidAt), t_(&v.CreatedAt), t_(&v.ResolvedAt)}
	},
}).build()

var devicesC = (&codec[store.Device]{
	name: "devices", pk: []string{"site_id", "mac"}, del: "site_id || '|' || mac", key: func(v store.Device) string { return store.DeviceKey(v.SiteID, v.MAC) },
	cols: []string{"site_id", "mac", "ip", "source", "first_seen", "last_seen", "hits", "vendor", "host_name", "online", "port"},
	args: func(v store.Device) []any {
		return []any{v.SiteID, v.MAC, v.IP, v.Source, nt(v.FirstSeen), nt(v.LastSeen), v.Hits, v.Vendor, v.HostName, v.Online, v.Port}
	},
	dst: func(v *store.Device) []any {
		return []any{&v.SiteID, &v.MAC, &v.IP, &v.Source, t_(&v.FirstSeen), t_(&v.LastSeen), &v.Hits, &v.Vendor, &v.HostName, &v.Online, &v.Port}
	},
}).build()

var tasksC = (&codec[store.Task]{
	name: "tasks", pk: []string{"id"}, key: func(v store.Task) string { return v.ID },
	cols: []string{"id", "site_id", "account_id", "code", "rev", "reason", "attempts", "next_try", "last_err", "created_at"},
	args: func(v store.Task) []any {
		return []any{v.ID, v.SiteID, v.AccountID, v.Code, v.Rev, v.Reason, v.Attempts, nt(v.NextTry), v.LastErr, tm(v.CreatedAt)}
	},
	dst: func(v *store.Task) []any {
		return []any{&v.ID, &v.SiteID, &v.AccountID, &v.Code, &v.Rev, &v.Reason, &v.Attempts, t_(&v.NextTry), &v.LastErr, t_(&v.CreatedAt)}
	},
}).build()

var registrationsC = (&codec[store.Registration]{
	name: "registrations", pk: []string{"id"}, key: func(v store.Registration) string { return v.ID },
	cols: []string{"id", "site_id", "apartment_id", "unit", "name", "phone", "email", "mac", "guess_mac", "status", "consent_at", "created_at", "decided_at", "decided_by", "account_id", "device_name", "building"},
	args: func(v store.Registration) []any {
		return []any{v.ID, v.SiteID, v.ApartmentID, v.Unit, v.Name, v.Phone, v.Email, v.MAC, v.GuessMAC, v.Status, nt(v.ConsentAt), tm(v.CreatedAt), nt(v.DecidedAt), v.DecidedBy, v.AccountID, v.DeviceName, v.Building}
	},
	dst: func(v *store.Registration) []any {
		return []any{&v.ID, &v.SiteID, &v.ApartmentID, &v.Unit, &v.Name, &v.Phone, &v.Email, &v.MAC, &v.GuessMAC, &v.Status, t_(&v.ConsentAt), t_(&v.CreatedAt), t_(&v.DecidedAt), &v.DecidedBy, &v.AccountID, &v.DeviceName, &v.Building}
	},
}).build()

// jsonArg stores a nested value as jsonb ("null" never reaches the column: a nil slice is "[]").
func jsonArg(v any, empty string) any {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return empty
	}
	return string(b)
}

// jsonScan scans jsonb into *p.
type jsonScan[T any] struct{ p *T }

func (d jsonScan[T]) Scan(v any) error {
	var raw []byte
	switch x := v.(type) {
	case nil:
		var zero T
		*d.p = zero
		return nil
	case string:
		raw = []byte(x)
	case []byte:
		raw = x
	default:
		return fmt.Errorf("pgstore: cannot scan %T into jsonb", v)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if rv := reflect.ValueOf(&out).Elem(); (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && rv.Len() == 0 {
		rv.Set(reflect.Zero(rv.Type())) // an empty list or map is nil, as the JSON file reads it
	}
	*d.p = out
	return nil
}

var onusC = (&codec[store.ONU]{
	name: "onus", pk: []string{"id"}, key: func(v store.ONU) string { return v.ID },
	cols: []string{"id", "serial", "oui", "manufacturer", "product_class", "sw_version", "hw_version", "root", "site_id", "mgmt_ip", "conn_req_url",
		"cr_user_enc", "cr_pass_enc", "cr_applied", "first_seen", "last_inform", "last_events", "macs", "wifi", "profile", "account_id", "link", "sec", "state"},
	args: func(v store.ONU) []any {
		return []any{v.ID, v.Serial, v.OUI, v.Manufacturer, v.ProductClass, v.SWVersion, v.HWVersion, v.Root, v.SiteID, v.MgmtIP, v.ConnReqURL,
			v.ConnReqUserEnc, v.ConnReqPassEnc, v.CRApplied, nt(v.FirstSeen), nt(v.LastInform), v.LastEvents, jsonArg(v.MACs, "[]"), jsonArg(v.WiFi, "[]"),
			jsonArg(v.Profile, "{}"), v.AccountID, v.Link, jsonArg(v.Sec, "{}"), jsonArg(v.State, "{}")}
	},
	dst: func(v *store.ONU) []any {
		return []any{&v.ID, &v.Serial, &v.OUI, &v.Manufacturer, &v.ProductClass, &v.SWVersion, &v.HWVersion, &v.Root, &v.SiteID, &v.MgmtIP, &v.ConnReqURL,
			&v.ConnReqUserEnc, &v.ConnReqPassEnc, &v.CRApplied, t_(&v.FirstSeen), t_(&v.LastInform), &v.LastEvents, jsonScan[[]string]{&v.MACs}, jsonScan[[]store.ONUWiFi]{&v.WiFi},
			jsonScan[store.ONUProfile]{&v.Profile}, &v.AccountID, &v.Link, jsonScan[store.ONUSec]{&v.Sec}, jsonScan[store.ONUState]{&v.State}}
	},
}).build()

var onuTasksC = (&codec[store.ONUTask]{
	name: "onu_tasks", pk: []string{"id"}, key: func(v store.ONUTask) string { return v.ID },
	cols: []string{"id", "onu_id", "site_id", "account_id", "kind", "status", "fault", "by", "attempts", "created_at", "started_at", "finished_at",
		"after_reboot", "pass_leaf", "confirmed_at", "confirmed_by"},
	args: func(v store.ONUTask) []any {
		return []any{v.ID, v.ONUID, v.SiteID, v.AccountID, v.Kind, v.Status, v.Fault, v.By, v.Attempts, tm(v.CreatedAt), nt(v.StartedAt), nt(v.FinishedAt),
			v.AfterReboot, v.PassLeaf, nt(v.ConfirmedAt), v.ConfirmedBy}
	},
	dst: func(v *store.ONUTask) []any {
		return []any{&v.ID, &v.ONUID, &v.SiteID, &v.AccountID, &v.Kind, &v.Status, &v.Fault, &v.By, &v.Attempts, t_(&v.CreatedAt), t_(&v.StartedAt), t_(&v.FinishedAt),
			&v.AfterReboot, &v.PassLeaf, t_(&v.ConfirmedAt), &v.ConfirmedBy}
	},
}).build()

var ouiCacheC = (&codec[store.OUIEntry]{
	name: "oui_cache", pk: []string{"prefix"}, key: func(v store.OUIEntry) string { return v.Prefix },
	cols: []string{"prefix", "vendor", "found", "checked_at"},
	args: func(v store.OUIEntry) []any { return []any{v.Prefix, v.Vendor, v.Found, tm(v.CheckedAt)} },
	dst:  func(v *store.OUIEntry) []any { return []any{&v.Prefix, &v.Vendor, &v.Found, t_(&v.CheckedAt)} },
}).build()

var eqEventsC = (&codec[store.EquipmentEvent]{
	name: "equipment_events", pk: []string{"id"}, key: func(v store.EquipmentEvent) string { return v.ID },
	cols: []string{"id", "equipment_id", "time", "kind", "from_status", "to_status", "site_id", "account_id", "customer_id", "note"},
	args: func(v store.EquipmentEvent) []any {
		return []any{v.ID, v.EquipmentID, tm(v.Time), v.Kind, v.FromStatus, v.ToStatus, v.SiteID, v.AccountID, v.CustomerID, v.Note}
	},
	dst: func(v *store.EquipmentEvent) []any {
		return []any{&v.ID, &v.EquipmentID, t_(&v.Time), &v.Kind, &v.FromStatus, &v.ToStatus, &v.SiteID, &v.AccountID, &v.CustomerID, &v.Note}
	},
}).build()

var notesC = (&codec[store.Note]{
	name: "notes", pk: []string{"id"}, key: func(v store.Note) string { return v.ID },
	cols: []string{"id", "time", "customer_id", "account_id", "site_id", "equipment_id", "text"},
	args: func(v store.Note) []any {
		return []any{v.ID, tm(v.Time), v.CustomerID, v.AccountID, v.SiteID, v.EquipmentID, v.Text}
	},
	dst: func(v *store.Note) []any {
		return []any{&v.ID, t_(&v.Time), &v.CustomerID, &v.AccountID, &v.SiteID, &v.EquipmentID, &v.Text}
	},
}).build()

var settingsC = (&codec[store.Setting]{
	name: "settings", pk: []string{"key"}, key: func(v store.Setting) string { return v.Key },
	cols: []string{"key", "value", "enc", "updated_at"},
	args: func(v store.Setting) []any { return []any{v.Key, v.Value, v.Enc, tm(v.UpdatedAt)} },
	dst:  func(v *store.Setting) []any { return []any{&v.Key, &v.Value, &v.Enc, t_(&v.UpdatedAt)} },
}).build()

var eventsC = (&codec[store.Event]{
	name: "events", pk: []string{"seq"}, key: func(v store.Event) string { return fmt.Sprint(v.Seq) },
	cols: []string{"seq", "time", "site_id", "kind", "account_id", "customer_id", "equipment_id", "msg", "actor", "ip"},
	args: func(v store.Event) []any {
		return []any{v.Seq, tm(v.Time), v.SiteID, v.Kind, v.AccountID, v.CustomerID, v.EquipmentID, v.Msg, v.Actor, v.IP}
	},
	dst: func(v *store.Event) []any {
		return []any{&v.Seq, t_(&v.Time), &v.SiteID, &v.Kind, &v.AccountID, &v.CustomerID, &v.EquipmentID, &v.Msg, &v.Actor, &v.IP}
	},
}).build()

var adminUsersC = (&codec[store.AdminUser]{
	name: "admin_users", pk: []string{"id"},
	guard: "admin_users.updated_at IS NULL OR EXCLUDED.updated_at >= admin_users.updated_at", key: func(v store.AdminUser) string { return v.ID },
	cols: []string{"id", "username", "role", "pass_hash", "totp_secret_enc", "totp_confirmed_at", "totp_last_step", "recovery_codes", "disabled",
		"pass_changed_at", "created_at", "updated_at", "totp_fails", "totp_fail_at"},
	args: func(v store.AdminUser) []any {
		return []any{v.ID, v.Username, v.Role, v.PassHash, v.TOTPSecretEnc, nt(v.TOTPConfirmedAt), v.TOTPLastStep, stringsArg(v.RecoveryCodes), v.Disabled,
			nt(v.PassChangedAt), tm(v.CreatedAt), nt(v.UpdatedAt), v.TOTPFails, nt(v.TOTPFailAt)}
	},
	dst: func(v *store.AdminUser) []any {
		return []any{&v.ID, &v.Username, &v.Role, &v.PassHash, &v.TOTPSecretEnc, t_(&v.TOTPConfirmedAt), &v.TOTPLastStep, stringsDst{&v.RecoveryCodes}, &v.Disabled,
			t_(&v.PassChangedAt), t_(&v.CreatedAt), t_(&v.UpdatedAt), &v.TOTPFails, t_(&v.TOTPFailAt)}
	},
}).build()

var adminSessionsC = (&codec[store.AdminSession]{
	name: "admin_sessions", pk: []string{"id"}, key: func(v store.AdminSession) string { return v.ID },
	cols: []string{"id", "user_id", "csrf", "mfa", "ip", "user_agent", "created_at", "last_seen_at", "expires_at"},
	args: func(v store.AdminSession) []any {
		return []any{v.ID, v.UserID, v.CSRF, v.MFA, v.IP, v.UserAgent, tm(v.CreatedAt), tm(v.LastSeenAt), tm(v.ExpiresAt)}
	},
	dst: func(v *store.AdminSession) []any {
		return []any{&v.ID, &v.UserID, &v.CSRF, &v.MFA, &v.IP, &v.UserAgent, t_(&v.CreatedAt), t_(&v.LastSeenAt), t_(&v.ExpiresAt)}
	},
}).build()

var apiDevicesC = (&codec[store.APIDevice]{
	name: "api_devices", pk: []string{"id"},
	guard: "(api_devices.revoked_at IS NULL OR EXCLUDED.revoked_at IS NOT NULL) AND " +
		"(api_devices.refresh_rotated_at IS NULL OR EXCLUDED.refresh_rotated_at >= api_devices.refresh_rotated_at)", key: func(v store.APIDevice) string { return v.ID },
	cols: []string{"id", "user_id", "install_id", "name", "model", "platform", "app_version", "access_hash", "access_expires_at", "refresh_hash",
		"refresh_prev_hash", "refresh_rotated_at", "refresh_expires_at", "absolute_expires_at", "last_ip", "created_at", "last_seen_at", "revoked_at", "revoked_reason",
		"refresh_older"},
	args: func(v store.APIDevice) []any {
		return []any{v.ID, v.UserID, v.InstallID, v.Name, v.Model, v.Platform, v.AppVersion, v.AccessHash, nt(v.AccessExpiresAt), v.RefreshHash,
			v.RefreshPrevHash, nt(v.RefreshRotatedAt), nt(v.RefreshExpiresAt), nt(v.AbsoluteExpiresAt), v.LastIP, tm(v.CreatedAt), nt(v.LastSeenAt), nt(v.RevokedAt), v.RevokedReason, stringsArg(v.RefreshOlder)}
	},
	dst: func(v *store.APIDevice) []any {
		return []any{&v.ID, &v.UserID, &v.InstallID, &v.Name, &v.Model, &v.Platform, &v.AppVersion, &v.AccessHash, t_(&v.AccessExpiresAt), &v.RefreshHash,
			&v.RefreshPrevHash, t_(&v.RefreshRotatedAt), t_(&v.RefreshExpiresAt), t_(&v.AbsoluteExpiresAt), &v.LastIP, t_(&v.CreatedAt), t_(&v.LastSeenAt), t_(&v.RevokedAt), &v.RevokedReason, stringsDst{&v.RefreshOlder}}
	},
}).build()
