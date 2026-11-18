// Package store is a tiny JSON-file database: one mutex, atomic writes.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ErrNoChange can be returned from an Update callback to skip the disk write.
var ErrNoChange = errors.New("no change")

// ErrOldFormat is returned by Open when ispbill.json was written by an older,
// incompatible version (the PIN/subscriber model).
var ErrOldFormat = errors.New("ispbill.json is in an old or unknown data format")

// FormatVersion is the on-disk data format. 2 = flat accounts locked to an ONT MAC.
const FormatVersion = 2

const (
	OrderPending = "pending"
	OrderPaid    = "paid"
	OrderFailed  = "failed"

	PayApplied   = "applied"
	PayUnmatched = "unmatched"
	PayRefunded  = "refunded"
	PayIgnored   = "ignored"
)

type Package struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"` // stable; router profile is "ispbill-<slug>"
	DurationMin int    `json:"duration_min"`
	SpeedMbps   int    `json:"speed_mbps"`
	PriceKES    int    `json:"price_kes"`
	Disabled    bool   `json:"disabled"`
	Deleted     bool   `json:"deleted,omitempty"` // kept only so old payments show its name
	Sort        int    `json:"sort"`
}

// Profile is the RouterOS hotspot user profile name for the package.
func (p Package) Profile() string { return "ispbill-" + p.Slug }

// RateLimit is the RouterOS rate-limit string, e.g. "10M/10M".
func (p Package) RateLimit() string { return fmt.Sprintf("%dM/%dM", p.SpeedMbps, p.SpeedMbps) }

// Account is one flat. It is created by the admin only and is locked to the
// MAC address of the flat's ONT (WAN side). Code is immutable and doubles as
// the M-Pesa Paybill account number.
type Account struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Flat   string `json:"flat"`
	MAC    string `json:"mac"`
	Tenant string `json:"tenant,omitempty"`
	Phone  string `json:"phone,omitempty"`
	Notes  string `json:"notes,omitempty"`

	PackageID string    `json:"package_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Suspended bool      `json:"suspended,omitempty"`

	// HSPass is the internal hotspot password. It is random and never shown.
	HSPass string `json:"hs_pass"`
	// RouterActive is what the router was last told: true = user present.
	RouterActive bool `json:"router_active,omitempty"`

	// Reminder bookkeeping: the ExpiresAt value an event was already logged for.
	RemindedFor time.Time `json:"reminded_for,omitempty"`
	ExpiredFor  time.Time `json:"expired_for,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

type Order struct {
	ID                string    `json:"id"`
	AccountID         string    `json:"account_id"`
	PackageID         string    `json:"package_id"`
	Phone             string    `json:"phone,omitempty"` // M-Pesa payer
	Method            string    `json:"method"`          // test, mpesa, voucher
	AmountKES         int       `json:"amount_kes"`
	Status            string    `json:"status"`
	Reason            string    `json:"reason,omitempty"`
	CheckoutRequestID string    `json:"checkout_request_id,omitempty"`
	Receipt           string    `json:"receipt,omitempty"`
	VoucherCode       string    `json:"voucher_code,omitempty"`
	LoggedIn          bool      `json:"logged_in"`
	MAC               string    `json:"mac,omitempty"`
	IP                string    `json:"ip,omitempty"`
	Dst               string    `json:"dst,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	PaidAt            time.Time `json:"paid_at,omitempty"`
}

type Voucher struct {
	Code      string    `json:"code"`
	PackageID string    `json:"package_id"`
	Batch     string    `json:"batch"`
	Used      bool      `json:"used"`
	UsedAt    time.Time `json:"used_at,omitempty"`
	UsedBy    string    `json:"used_by,omitempty"` // order id
	CreatedAt time.Time `json:"created_at"`
}

// Payment is one ledger entry. Key is unique: the M-Pesa receipt code, or
// VOUCHER-<code>, TEST-<order>, ADMIN-<id>. Inserting a duplicate key is a no-op.
type Payment struct {
	Key        string    `json:"key"`
	Method     string    `json:"method"` // mpesa-manual, mpesa-stk, mpesa-c2b, voucher, test, admin
	Status     string    `json:"status"` // applied, unmatched, refunded, ignored
	AmountKES  int       `json:"amount_kes"`
	Phone      string    `json:"phone,omitempty"` // payer
	Name       string    `json:"name,omitempty"`
	BillRef    string    `json:"bill_ref,omitempty"` // what the payer typed as account number
	AccountID  string    `json:"account_id,omitempty"`
	PackageID  string    `json:"package_id,omitempty"`
	OrderID    string    `json:"order_id,omitempty"`
	Note       string    `json:"note,omitempty"`
	PaidAt     time.Time `json:"paid_at"`    // when the money arrived (entered date for manual entries)
	CreatedAt  time.Time `json:"created_at"` // when it was recorded here
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// Device is a MAC that opened the portal (or showed up as a hotspot host)
// without a matching account.
type Device struct {
	MAC       string    `json:"mac"`
	IP        string    `json:"ip,omitempty"`
	Source    string    `json:"source"` // portal, hotspot-host
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Hits      int       `json:"hits"`
}

// Task is a pending router action. Reconcile tasks make the router match the
// account's current state; they are retried with backoff until they succeed
// and they survive a restart.
type Task struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	Code      string    `json:"code"`
	Kick      bool      `json:"kick,omitempty"` // also end the live session (MAC change)
	Rev       int       `json:"rev"`            // bumped when the task is refreshed
	Reason    string    `json:"reason,omitempty"`
	Attempts  int       `json:"attempts"`
	NextTry   time.Time `json:"next_try"`
	LastErr   string    `json:"last_err,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Event is a lifecycle note (reminder due, expired). No SMS is sent yet.
type Event struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"` // reminder_due, expired, info
	AccountID string    `json:"account_id,omitempty"`
	Msg       string    `json:"msg"`
}

const maxEvents = 300

// Data is the whole database.
type Data struct {
	Format   int                `json:"format"`
	Packages map[string]Package `json:"packages"`
	Accounts map[string]Account `json:"accounts"`
	Orders   map[string]Order   `json:"orders"`
	Vouchers map[string]Voucher `json:"vouchers"`
	Payments map[string]Payment `json:"payments"` // key -> entry
	Devices  map[string]Device  `json:"devices"`  // mac -> device
	Tasks    map[string]Task    `json:"tasks"`
	Events   []Event            `json:"events"`
}

func (d *Data) init() {
	d.Format = FormatVersion
	if d.Packages == nil {
		d.Packages = map[string]Package{}
	}
	if d.Accounts == nil {
		d.Accounts = map[string]Account{}
	}
	if d.Orders == nil {
		d.Orders = map[string]Order{}
	}
	if d.Vouchers == nil {
		d.Vouchers = map[string]Voucher{}
	}
	if d.Payments == nil {
		d.Payments = map[string]Payment{}
	}
	if d.Devices == nil {
		d.Devices = map[string]Device{}
	}
	if d.Tasks == nil {
		d.Tasks = map[string]Task{}
	}
}

// AddEvent appends a lifecycle event, keeping only the newest maxEvents.
func (d *Data) AddEvent(e Event) {
	d.Events = append(d.Events, e)
	if n := len(d.Events); n > maxEvents {
		d.Events = append([]Event(nil), d.Events[n-maxEvents:]...)
	}
}

// Lookup helpers (call inside View/Update).

func (d *Data) AccountByCode(code string) (Account, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return Account{}, false
	}
	for _, a := range d.Accounts {
		if a.Code == code {
			return a, true
		}
	}
	return Account{}, false
}

// AccountByMAC expects a normalised MAC (AA:BB:CC:DD:EE:FF).
func (d *Data) AccountByMAC(mac string) (Account, bool) {
	if mac == "" {
		return Account{}, false
	}
	for _, a := range d.Accounts {
		if strings.EqualFold(a.MAC, mac) {
			return a, true
		}
	}
	return Account{}, false
}

func (d *Data) OrderByCheckoutID(id string) (Order, bool) {
	if id == "" {
		return Order{}, false
	}
	for _, o := range d.Orders {
		if o.CheckoutRequestID == id {
			return o, true
		}
	}
	return Order{}, false
}

// Sorted lists (copies).

func (d *Data) PackageList() []Package {
	out := make([]Package, 0, len(d.Packages))
	for _, p := range d.Packages {
		if !p.Deleted {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].DurationMin < out[j].DurationMin
	})
	return out
}

// NaturalLess orders "Flat 2A" before "Flat 10A".
func NaturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if unicode.IsDigit(rune(a[i])) && unicode.IsDigit(rune(b[j])) {
			si := i
			for i < len(a) && unicode.IsDigit(rune(a[i])) {
				i++
			}
			sj := j
			for j < len(b) && unicode.IsDigit(rune(b[j])) {
				j++
			}
			na, nb := strings.TrimLeft(a[si:i], "0"), strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

func (d *Data) AccountList() []Account {
	out := make([]Account, 0, len(d.Accounts))
	for _, a := range d.Accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Flat != out[j].Flat {
			return NaturalLess(out[i].Flat, out[j].Flat)
		}
		return out[i].Code < out[j].Code
	})
	return out
}

func (d *Data) OrderList() []Order {
	out := make([]Order, 0, len(d.Orders))
	for _, o := range d.Orders {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (d *Data) VoucherList() []Voucher {
	out := make([]Voucher, 0, len(d.Vouchers))
	for _, v := range d.Vouchers {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// PaymentList returns the ledger, newest first.
func (d *Data) PaymentList() []Payment {
	out := make([]Payment, 0, len(d.Payments))
	for _, p := range d.Payments {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].PaidAt.Equal(out[j].PaidAt) {
			return out[i].PaidAt.After(out[j].PaidAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (d *Data) DeviceList() []Device {
	out := make([]Device, 0, len(d.Devices))
	for _, v := range d.Devices {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

func (d *Data) TaskList() []Task {
	out := make([]Task, 0, len(d.Tasks))
	for _, t := range d.Tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Store guards Data with a mutex and persists it to a JSON file.
type Store struct {
	mu   sync.RWMutex
	path string
	data Data
}

// Open loads (or creates) the database file at dir/ispbill.json. It refuses
// to start on a file written by an older, incompatible version instead of
// silently reading it wrongly.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "ispbill.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var probe struct {
			Format int `json:"format"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			return nil, fmt.Errorf("corrupt %s: %w", s.path, err)
		}
		if probe.Format != FormatVersion {
			return nil, fmt.Errorf("%w: %s has format %d, this version needs %d (old PIN/subscriber model). "+
				"Move or delete the file to start fresh; ispbill will not touch it", ErrOldFormat, s.path, probe.Format, FormatVersion)
		}
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("corrupt %s: %w", s.path, err)
		}
	}
	s.data.init()
	return s, nil
}

// View runs fn under a read lock. fn must not retain or modify d's maps.
func (s *Store) View(fn func(d *Data)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.data)
}

// Update runs fn under the write lock and then saves atomically. fn should
// validate before it mutates: if it returns an error nothing is written
// (but earlier in-memory edits would remain). Return ErrNoChange to skip the save.
func (s *Store) Update(fn func(d *Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	return s.saveLocked()
}

// saveLocked persists the whole database atomically.
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.data, "", " ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, b)
}

// WriteFileAtomic writes to a temp file in the same directory, fsyncs, renames
// over path, then fsyncs the directory so the rename survives power loss.
func WriteFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ispbill-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if _, err := f.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
