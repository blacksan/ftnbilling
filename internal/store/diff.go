package store

import (
	"reflect"
	"time"
)

// nt normalises a time for storage: UTC, microsecond precision (what a
// database keeps), no monotonic reading. The zero time stays the zero time.
func nt(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Microsecond)
}

func (x Site) norm() Site {
	x.Join.ExpiresAt, x.Join.CreatedAt, x.Join.UsedAt = nt(x.Join.ExpiresAt), nt(x.Join.CreatedAt), nt(x.Join.UsedAt)
	x.JoinedAt, x.GoLive, x.CreatedAt = nt(x.JoinedAt), nt(x.GoLive), nt(x.CreatedAt)
	if len(x.PkgOverrides) == 0 {
		x.PkgOverrides = nil
	}
	return x
}
func (x Package) norm() Package { return x }
func (x Customer) norm() Customer {
	x.AnonymizedAt, x.CreatedAt, x.UpdatedAt = nt(x.AnonymizedAt), nt(x.CreatedAt), nt(x.UpdatedAt)
	return x
}
func (x Equipment) norm() Equipment {
	x.InstalledAt, x.CreatedAt, x.UpdatedAt = nt(x.InstalledAt), nt(x.CreatedAt), nt(x.UpdatedAt)
	return x
}
func (x Account) norm() Account {
	x.ExpiresAt, x.RemindedFor, x.ExpiredFor = nt(x.ExpiresAt), nt(x.RemindedFor), nt(x.ExpiredFor)
	x.CreatedAt, x.ClosedAt = nt(x.CreatedAt), nt(x.ClosedAt)
	return x
}
func (x Tenancy) norm() Tenancy { x.MoveIn, x.MoveOut = nt(x.MoveIn), nt(x.MoveOut); return x }
func (x Contact) norm() Contact { x.CreatedAt = nt(x.CreatedAt); return x }
func (x Order) norm() Order     { x.CreatedAt, x.PaidAt = nt(x.CreatedAt), nt(x.PaidAt); return x }
func (x Voucher) norm() Voucher { x.UsedAt, x.CreatedAt = nt(x.UsedAt), nt(x.CreatedAt); return x }
func (x Payment) norm() Payment {
	x.PaidAt, x.CreatedAt, x.ResolvedAt = nt(x.PaidAt), nt(x.CreatedAt), nt(x.ResolvedAt)
	return x
}
func (x Device) norm() Device { x.FirstSeen, x.LastSeen = nt(x.FirstSeen), nt(x.LastSeen); return x }
func (x Task) norm() Task     { x.NextTry, x.CreatedAt = nt(x.NextTry), nt(x.CreatedAt); return x }
func (x EquipmentEvent) norm() EquipmentEvent {
	x.Time = nt(x.Time)
	return x
}
func (x Note) norm() Note       { x.Time = nt(x.Time); return x }
func (x Setting) norm() Setting { x.UpdatedAt = nt(x.UpdatedAt); return x }
func (x Event) norm() Event     { x.Time = nt(x.Time); return x }

// Norm returns the row as it will be stored: times in UTC at microsecond
// precision. Backends apply it on load; the Store applies it on write.
func (x Site) Norm() Site                     { return x.norm() }
func (x Customer) Norm() Customer             { return x.norm() }
func (x Equipment) Norm() Equipment           { return x.norm() }
func (x Account) Norm() Account               { return x.norm() }
func (x Tenancy) Norm() Tenancy               { return x.norm() }
func (x Contact) Norm() Contact               { return x.norm() }
func (x Order) Norm() Order                   { return x.norm() }
func (x Voucher) Norm() Voucher               { return x.norm() }
func (x Payment) Norm() Payment               { return x.norm() }
func (x Device) Norm() Device                 { return x.norm() }
func (x Task) Norm() Task                     { return x.norm() }
func (x EquipmentEvent) Norm() EquipmentEvent { return x.norm() }
func (x Note) Norm() Note                     { return x.norm() }
func (x Event) Norm() Event                   { return x.norm() }
func (x Setting) Norm() Setting               { return x.norm() }

func eqc[V comparable](a, b V) bool { return a == b }
func eqDeep[V any](a, b V) bool     { return reflect.DeepEqual(a, b) }

// table describes how to diff, apply and roll back one map of Data.
type table interface {
	diff(live, shadow *Data, cs *ChangeSet)
	apply(shadow *Data, cs *ChangeSet)
	rollback(live, shadow *Data, cs *ChangeSet)
	normAll(d *Data)
}

type tbl[V any] struct {
	m     func(*Data) *map[string]V
	delta func(*ChangeSet) *Delta[V]
	eq    func(a, b V) bool
	norm  func(V) V
	key   func(V) string
	clone func(V) V // nil = plain copy
}

func (t tbl[V]) cp(v V) V {
	if t.clone != nil {
		return t.clone(v)
	}
	return v
}

func (t tbl[V]) diff(l, s *Data, cs *ChangeSet) {
	live, shadow := *t.m(l), *t.m(s)
	out := t.delta(cs)
	matched := 0
	for k, v := range live {
		sv, ok := shadow[k]
		if ok {
			matched++
			if t.eq(v, sv) {
				continue
			}
		}
		nv := t.norm(v)
		live[k] = nv
		if ok && t.eq(nv, sv) {
			continue
		}
		out.Upsert = append(out.Upsert, nv)
		if !ok {
			if out.Fresh == nil {
				out.Fresh = map[string]bool{}
			}
			out.Fresh[t.key(nv)] = true
		}
	}
	if matched < len(shadow) {
		for k := range shadow {
			if _, ok := live[k]; !ok {
				out.Delete = append(out.Delete, k)
			}
		}
	}
}

func (t tbl[V]) apply(s *Data, cs *ChangeSet) {
	d := t.delta(cs)
	m := *t.m(s)
	for _, k := range d.Delete {
		delete(m, k)
	}
	for _, v := range d.Upsert {
		m[t.key(v)] = t.cp(v)
	}
}

func (t tbl[V]) rollback(l, s *Data, cs *ChangeSet) {
	d := t.delta(cs)
	live, shadow := *t.m(l), *t.m(s)
	for _, v := range d.Upsert {
		k := t.key(v)
		if sv, ok := shadow[k]; ok {
			live[k] = t.cp(sv)
		} else {
			delete(live, k)
		}
	}
	for _, k := range d.Delete {
		if sv, ok := shadow[k]; ok {
			live[k] = t.cp(sv)
		}
	}
}

func (t tbl[V]) normAll(d *Data) {
	m := *t.m(d)
	for k, v := range m {
		m[k] = t.norm(v)
	}
}

func cloneSite(s Site) Site {
	if s.PkgOverrides != nil {
		m := make(map[string]PkgOverride, len(s.PkgOverrides))
		for k, v := range s.PkgOverrides {
			m[k] = v
		}
		s.PkgOverrides = m
	}
	return s
}

// tables lists every table except accounts (two maps, one key space) and
// events (a slice).
var tables = []table{
	tbl[Site]{func(d *Data) *map[string]Site { return &d.Sites }, func(c *ChangeSet) *Delta[Site] { return &c.Sites },
		eqDeep[Site], Site.norm, func(v Site) string { return v.ID }, cloneSite},
	tbl[Package]{func(d *Data) *map[string]Package { return &d.Packages }, func(c *ChangeSet) *Delta[Package] { return &c.Packages },
		eqc[Package], Package.norm, func(v Package) string { return v.ID }, nil},
	tbl[Customer]{func(d *Data) *map[string]Customer { return &d.Customers }, func(c *ChangeSet) *Delta[Customer] { return &c.Customers },
		eqc[Customer], Customer.norm, func(v Customer) string { return v.ID }, nil},
	tbl[Equipment]{func(d *Data) *map[string]Equipment { return &d.Equipment }, func(c *ChangeSet) *Delta[Equipment] { return &c.Equipment },
		eqc[Equipment], Equipment.norm, func(v Equipment) string { return v.ID }, nil},
	tbl[Tenancy]{func(d *Data) *map[string]Tenancy { return &d.Tenancies }, func(c *ChangeSet) *Delta[Tenancy] { return &c.Tenancies },
		eqc[Tenancy], Tenancy.norm, func(v Tenancy) string { return v.ID }, nil},
	tbl[Contact]{func(d *Data) *map[string]Contact { return &d.Contacts }, func(c *ChangeSet) *Delta[Contact] { return &c.Contacts },
		eqc[Contact], Contact.norm, func(v Contact) string { return v.ID }, nil},
	tbl[Order]{func(d *Data) *map[string]Order { return &d.Orders }, func(c *ChangeSet) *Delta[Order] { return &c.Orders },
		eqc[Order], Order.norm, func(v Order) string { return v.ID }, nil},
	tbl[Voucher]{func(d *Data) *map[string]Voucher { return &d.Vouchers }, func(c *ChangeSet) *Delta[Voucher] { return &c.Vouchers },
		eqc[Voucher], Voucher.norm, func(v Voucher) string { return v.Code }, nil},
	tbl[Payment]{func(d *Data) *map[string]Payment { return &d.Payments }, func(c *ChangeSet) *Delta[Payment] { return &c.Payments },
		eqc[Payment], Payment.norm, func(v Payment) string { return v.Key }, nil},
	tbl[Device]{func(d *Data) *map[string]Device { return &d.Devices }, func(c *ChangeSet) *Delta[Device] { return &c.Devices },
		eqc[Device], Device.norm, func(v Device) string { return DeviceKey(v.SiteID, v.MAC) }, nil},
	tbl[Task]{func(d *Data) *map[string]Task { return &d.Tasks }, func(c *ChangeSet) *Delta[Task] { return &c.Tasks },
		eqc[Task], Task.norm, func(v Task) string { return v.ID }, nil},
	tbl[EquipmentEvent]{func(d *Data) *map[string]EquipmentEvent { return &d.EquipmentEvents }, func(c *ChangeSet) *Delta[EquipmentEvent] { return &c.EquipmentEvents },
		eqc[EquipmentEvent], EquipmentEvent.norm, func(v EquipmentEvent) string { return v.ID }, nil},
	tbl[Note]{func(d *Data) *map[string]Note { return &d.Notes }, func(c *ChangeSet) *Delta[Note] { return &c.Notes },
		eqc[Note], Note.norm, func(v Note) string { return v.ID }, nil},
	tbl[Setting]{func(d *Data) *map[string]Setting { return &d.Settings }, func(c *ChangeSet) *Delta[Setting] { return &c.Settings },
		eqc[Setting], Setting.norm, func(v Setting) string { return v.Key }, nil},
}

// ---- accounts: open and closed share one key space ------------------------

func lookupAccount(d *Data, k string) (Account, bool) {
	a, ok, _ := lookupAccountIn(d, k)
	return a, ok
}

// lookupAccountIn also tells which map the account is in.
func lookupAccountIn(d *Data, k string) (a Account, ok, closed bool) {
	if a, ok := d.Accounts[k]; ok {
		return a, true, false
	}
	a, ok = d.ClosedAccounts[k]
	return a, ok, ok
}

// fixAccount makes the row agree with the map it sits in.
func fixAccount(a Account, closed bool, now time.Time) Account {
	a = a.norm()
	if closed && a.ClosedAt.IsZero() {
		a.ClosedAt = nt(now)
	}
	if !closed {
		a.ClosedAt = time.Time{}
	}
	return a
}

func diffAccounts(l, s *Data, cs *ChangeSet, now time.Time) {
	matched := 0
	each := func(m map[string]Account, closed bool) {
		for k, v := range m {
			sv, ok, wasClosed := lookupAccountIn(s, k)
			if ok {
				matched++
				if v == sv && wasClosed == closed {
					continue
				}
			}
			nv := fixAccount(v, closed, now)
			m[k] = nv
			if ok && nv == sv && wasClosed == closed {
				continue
			}
			cs.Accounts.Upsert = append(cs.Accounts.Upsert, nv)
			if !ok {
				if cs.Accounts.Fresh == nil {
					cs.Accounts.Fresh = map[string]bool{}
				}
				cs.Accounts.Fresh[k] = true
			} else {
				if cs.Accounts.Old == nil {
					cs.Accounts.Old = map[string]Account{}
				}
				cs.Accounts.Old[k] = sv
			}
		}
	}
	each(l.Accounts, false)
	each(l.ClosedAccounts, true)
	if matched < len(s.Accounts)+len(s.ClosedAccounts) {
		for _, m := range []map[string]Account{s.Accounts, s.ClosedAccounts} {
			for k := range m {
				if _, ok := lookupAccount(l, k); !ok {
					cs.Accounts.Delete = append(cs.Accounts.Delete, k)
				}
			}
		}
	}
}

func applyAccounts(s *Data, cs *ChangeSet) {
	for _, k := range cs.Accounts.Delete {
		delete(s.Accounts, k)
		delete(s.ClosedAccounts, k)
	}
	for _, a := range cs.Accounts.Upsert {
		delete(s.Accounts, a.ID)
		delete(s.ClosedAccounts, a.ID)
		if a.ClosedAt.IsZero() {
			s.Accounts[a.ID] = a
		} else {
			s.ClosedAccounts[a.ID] = a
		}
	}
}

func rollbackAccounts(l, s *Data, cs *ChangeSet) {
	restore := func(k string) {
		delete(l.Accounts, k)
		delete(l.ClosedAccounts, k)
		if a, ok := s.Accounts[k]; ok {
			l.Accounts[k] = a
		} else if a, ok := s.ClosedAccounts[k]; ok {
			l.ClosedAccounts[k] = a
		}
	}
	for _, a := range cs.Accounts.Upsert {
		restore(a.ID)
	}
	for _, k := range cs.Accounts.Delete {
		restore(k)
	}
}

// ---- whole-data helpers ---------------------------------------------------

// diffData compares live against the shadow (the last committed state),
// normalises changed rows inside live and returns what changed.
func diffData(live, shadow *Data, now time.Time) *ChangeSet {
	cs := &ChangeSet{}
	for _, t := range tables {
		t.diff(live, shadow, cs)
	}
	diffAccounts(live, shadow, cs, now)
	for i := range live.Events {
		if live.Events[i].Seq > shadow.EventSeq {
			live.Events[i] = live.Events[i].norm()
			cs.NewEvents = append(cs.NewEvents, live.Events[i])
		}
	}
	return cs
}

func applyChangeSet(shadow, live *Data, cs *ChangeSet) {
	for _, t := range tables {
		t.apply(shadow, cs)
	}
	applyAccounts(shadow, cs)
	shadow.Events = append([]Event(nil), live.Events...)
	shadow.EventSeq = live.EventSeq
	shadow.Format = live.Format
}

func rollbackData(live, shadow *Data, cs *ChangeSet) {
	for _, t := range tables {
		t.rollback(live, shadow, cs)
	}
	rollbackAccounts(live, shadow, cs)
	live.Events = append([]Event(nil), shadow.Events...)
	live.EventSeq = shadow.EventSeq
}

// normalizeData normalises every time in d (used on load).
func normalizeData(d *Data) {
	for _, t := range tables {
		t.normAll(d)
	}
	for k, a := range d.Accounts {
		d.Accounts[k] = fixAccount(a, false, time.Time{})
	}
	for k, a := range d.ClosedAccounts {
		d.ClosedAccounts[k] = fixAccount(a, true, time.Time{})
	}
	for i := range d.Events {
		d.Events[i] = d.Events[i].norm()
	}
}

func cloneMap[V any](m map[string]V, f func(V) V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		if f != nil {
			v = f(v)
		}
		out[k] = v
	}
	return out
}

// cloneData copies every table (rows are values; Site's override map is cloned).
func cloneData(d *Data) Data {
	return Data{
		Format: d.Format, EventSeq: d.EventSeq,
		Sites: cloneMap(d.Sites, cloneSite), Packages: cloneMap(d.Packages, nil), Accounts: cloneMap(d.Accounts, nil),
		ClosedAccounts: cloneMap(d.ClosedAccounts, nil), Orders: cloneMap(d.Orders, nil), Vouchers: cloneMap(d.Vouchers, nil),
		Payments: cloneMap(d.Payments, nil), Devices: cloneMap(d.Devices, nil), Tasks: cloneMap(d.Tasks, nil),
		Customers: cloneMap(d.Customers, nil), Tenancies: cloneMap(d.Tenancies, nil), Contacts: cloneMap(d.Contacts, nil),
		Equipment: cloneMap(d.Equipment, nil), EquipmentEvents: cloneMap(d.EquipmentEvents, nil), Notes: cloneMap(d.Notes, nil), Settings: cloneMap(d.Settings, nil),
		Events: append([]Event(nil), d.Events...),
	}
}

// Diff is exported for tests and tools: what would change from before to after.
func Diff(after, before *Data) *ChangeSet { return diffData(after, before, time.Now()) }

// NormTime is nt for other packages: UTC, microseconds, zero stays zero.
func NormTime(t time.Time) time.Time { return nt(t) }

// Norm returns the package as stored (it has no times, so it is unchanged).
func (x Package) Norm() Package { return x }
