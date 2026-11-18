package pgstore

import (
	"encoding/json"
	"fmt"
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
	table string
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
		"join_hash", "join_expires_at", "join_created_at", "join_used_at", "join_used_from", "joined_at", "hotspot_server", "wan_interface", "wan_list",
		"olt_interface", "vlan_id", "customer_subnet", "wan_cap_mbps", "olt_cap_mbps", "fup_tb", "provision", "bench_port", "legacy_container",
		"pkg_overrides", "address", "lat", "lng", "go_live", "created_at"},
	args: func(v store.Site) []any {
		return []any{v.ID, v.Name, v.Mode, v.Notes, v.SupportPhone, v.Disabled, v.Removed, v.TunnelIP, v.WGPublicKey, v.RESTUser, v.RESTPass, v.RESTPort,
			v.Join.Hash, nt(v.Join.ExpiresAt), nt(v.Join.CreatedAt), nt(v.Join.UsedAt), v.Join.UsedFrom, nt(v.JoinedAt), v.HotspotServer, v.WANInterface, v.WANList,
			v.OLTInterface, v.VLANID, v.CustomerSubnet, v.WANCapMbps, v.OLTCapMbps, v.FUPTB, v.Provision, v.BenchPort, v.LegacyContainer,
			overridesArg(v.PkgOverrides), v.Address, v.Lat, v.Lng, nt(v.GoLive), tm(v.CreatedAt)}
	},
	dst: func(v *store.Site) []any {
		return []any{&v.ID, &v.Name, &v.Mode, &v.Notes, &v.SupportPhone, &v.Disabled, &v.Removed, &v.TunnelIP, &v.WGPublicKey, &v.RESTUser, &v.RESTPass, &v.RESTPort,
			&v.Join.Hash, t_(&v.Join.ExpiresAt), t_(&v.Join.CreatedAt), t_(&v.Join.UsedAt), &v.Join.UsedFrom, t_(&v.JoinedAt), &v.HotspotServer, &v.WANInterface, &v.WANList,
			&v.OLTInterface, &v.VLANID, &v.CustomerSubnet, &v.WANCapMbps, &v.OLTCapMbps, &v.FUPTB, &v.Provision, &v.BenchPort, &v.LegacyContainer,
			overridesDst{&v.PkgOverrides}, &v.Address, &v.Lat, &v.Lng, t_(&v.GoLive), t_(&v.CreatedAt)}
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
		"anonymized_at", "created_at", "updated_at"},
	args: func(v store.Customer) []any {
		return []any{v.ID, v.Name, v.Phone, v.Phone2, v.WhatsApp, v.WAOptOut, v.Email, v.Notes, v.NationalIDEnc, v.NationalIDHMAC, v.NationalIDLast4,
			nt(v.AnonymizedAt), tm(v.CreatedAt), nt(v.UpdatedAt)}
	},
	dst: func(v *store.Customer) []any {
		return []any{&v.ID, &v.Name, &v.Phone, &v.Phone2, &v.WhatsApp, &v.WAOptOut, &v.Email, &v.Notes, &v.NationalIDEnc, &v.NationalIDHMAC, &v.NationalIDLast4,
			t_(&v.AnonymizedAt), t_(&v.CreatedAt), t_(&v.UpdatedAt)}
	},
}).build()

var equipmentC = (&codec[store.Equipment]{
	name: "equipment", pk: []string{"id"}, key: func(v store.Equipment) string { return v.ID },
	cols: []string{"id", "kind", "serial", "mac", "model", "firmware", "status", "site_id", "account_id", "installed_at", "notes", "created_at", "updated_at"},
	args: func(v store.Equipment) []any {
		return []any{v.ID, v.Kind, v.Serial, v.MAC, v.Model, v.Firmware, v.Status, fk(v.SiteID), fk(v.AccountID), nt(v.InstalledAt), v.Notes, tm(v.CreatedAt), nt(v.UpdatedAt)}
	},
	dst: func(v *store.Equipment) []any {
		return []any{&v.ID, &v.Kind, &v.Serial, &v.MAC, &v.Model, &v.Firmware, &v.Status, s_(&v.SiteID), s_(&v.AccountID), t_(&v.InstalledAt), &v.Notes, t_(&v.CreatedAt), t_(&v.UpdatedAt)}
	},
}).build()

var accountsC = (&codec[store.Account]{
	name: "accounts", pk: []string{"id"}, key: func(v store.Account) string { return v.ID },
	cols: []string{"id", "site_id", "code", "flat", "floor", "mac", "tenant", "phone", "notes", "whatsapp", "wa_opt_out", "customer_id", "package_id",
		"expires_at", "suspended", "hs_pass", "router_active", "reminded_for", "expired_for", "odb", "splitter_id", "splitter_port", "created_at", "closed_at"},
	args: func(v store.Account) []any {
		return []any{v.ID, v.SiteID, v.Code, v.Flat, v.Floor, v.MAC, v.Tenant, v.Phone, v.Notes, v.WhatsApp, v.WAOptOut, fk(v.CustomerID), fk(v.PackageID),
			nt(v.ExpiresAt), v.Suspended, v.HSPass, v.RouterActive, nt(v.RemindedFor), nt(v.ExpiredFor), v.ODB, fk(v.SplitterID), v.SplitterPort, tm(v.CreatedAt), nt(v.ClosedAt)}
	},
	dst: func(v *store.Account) []any {
		return []any{&v.ID, &v.SiteID, &v.Code, &v.Flat, &v.Floor, &v.MAC, &v.Tenant, &v.Phone, &v.Notes, &v.WhatsApp, &v.WAOptOut, s_(&v.CustomerID), s_(&v.PackageID),
			t_(&v.ExpiresAt), &v.Suspended, &v.HSPass, &v.RouterActive, t_(&v.RemindedFor), t_(&v.ExpiredFor), &v.ODB, s_(&v.SplitterID), &v.SplitterPort, t_(&v.CreatedAt), t_(&v.ClosedAt)}
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

var contactsC = (&codec[store.Contact]{
	name: "site_contacts", pk: []string{"id"}, key: func(v store.Contact) string { return v.ID },
	cols: []string{"id", "site_id", "role", "name", "phone", "email", "notes", "created_at"},
	args: func(v store.Contact) []any {
		return []any{v.ID, v.SiteID, v.Role, v.Name, v.Phone, v.Email, v.Notes, tm(v.CreatedAt)}
	},
	dst: func(v *store.Contact) []any {
		return []any{&v.ID, &v.SiteID, &v.Role, &v.Name, &v.Phone, &v.Email, &v.Notes, t_(&v.CreatedAt)}
	},
}).build()

var ordersC = (&codec[store.Order]{
	name: "orders", pk: []string{"id"}, key: func(v store.Order) string { return v.ID },
	cols: []string{"id", "site_id", "account_id", "package_id", "phone", "method", "amount_kes", "status", "reason", "checkout_request_id", "receipt",
		"voucher_code", "logged_in", "mac", "ip", "dst", "created_at", "paid_at"},
	args: func(v store.Order) []any {
		return []any{v.ID, fk(v.SiteID), fk(v.AccountID), fk(v.PackageID), v.Phone, v.Method, v.AmountKES, v.Status, v.Reason, v.CheckoutRequestID, v.Receipt,
			v.VoucherCode, v.LoggedIn, v.MAC, v.IP, v.Dst, tm(v.CreatedAt), nt(v.PaidAt)}
	},
	dst: func(v *store.Order) []any {
		return []any{&v.ID, s_(&v.SiteID), s_(&v.AccountID), s_(&v.PackageID), &v.Phone, &v.Method, &v.AmountKES, &v.Status, &v.Reason, &v.CheckoutRequestID, &v.Receipt,
			&v.VoucherCode, &v.LoggedIn, &v.MAC, &v.IP, &v.Dst, t_(&v.CreatedAt), t_(&v.PaidAt)}
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
	cols: []string{"site_id", "mac", "ip", "source", "first_seen", "last_seen", "hits"},
	args: func(v store.Device) []any {
		return []any{v.SiteID, v.MAC, v.IP, v.Source, nt(v.FirstSeen), nt(v.LastSeen), v.Hits}
	},
	dst: func(v *store.Device) []any {
		return []any{&v.SiteID, &v.MAC, &v.IP, &v.Source, t_(&v.FirstSeen), t_(&v.LastSeen), &v.Hits}
	},
}).build()

var tasksC = (&codec[store.Task]{
	name: "tasks", pk: []string{"id"}, key: func(v store.Task) string { return v.ID },
	cols: []string{"id", "site_id", "account_id", "code", "kick", "rev", "reason", "attempts", "next_try", "last_err", "created_at"},
	args: func(v store.Task) []any {
		return []any{v.ID, v.SiteID, v.AccountID, v.Code, v.Kick, v.Rev, v.Reason, v.Attempts, nt(v.NextTry), v.LastErr, tm(v.CreatedAt)}
	},
	dst: func(v *store.Task) []any {
		return []any{&v.ID, &v.SiteID, &v.AccountID, &v.Code, &v.Kick, &v.Rev, &v.Reason, &v.Attempts, t_(&v.NextTry), &v.LastErr, t_(&v.CreatedAt)}
	},
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
	cols: []string{"seq", "time", "site_id", "kind", "account_id", "customer_id", "equipment_id", "msg"},
	args: func(v store.Event) []any {
		return []any{v.Seq, tm(v.Time), v.SiteID, v.Kind, v.AccountID, v.CustomerID, v.EquipmentID, v.Msg}
	},
	dst: func(v *store.Event) []any {
		return []any{&v.Seq, t_(&v.Time), &v.SiteID, &v.Kind, &v.AccountID, &v.CustomerID, &v.EquipmentID, &v.Msg}
	},
}).build()
