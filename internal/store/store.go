// Package store is the in-memory database of ispbill. Every read and write
// goes through View and Update; a Backend (a JSON file by default, Postgres
// when DATABASE_URL is set) makes each Update durable.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ErrNoChange can be returned from an Update callback to skip the disk write.
var ErrNoChange = errors.New("no change")

// ErrOldFormat is returned by Open when ispbill.json was written by an older,
// incompatible version (the PIN/subscriber model).
var ErrOldFormat = errors.New("ispbill.json is in an old or unknown data format")

// FormatVersion is the data format. 2 = flat accounts locked to an ONT MAC
// (single building); 3 = adds Sites and a SiteID on every row; 4 = adds
// customers, tenancies, equipment, contacts, notes, closed accounts and event
// sequence numbers; 5 = adds admin settings (and the admin password hash);
// 6 = adds IPoE and sign-in data: Account.MACConflict and IP, Site.AccessMode,
// Event.Actor and IP, admin users, admin sessions and API devices, and makes
// an ONT MAC unique across all buildings; 7 = the bootstrap script: Site.WANMode
// and the WAN, PPPoE and admin LAN fields, every router is IPoE, and the
// settings of the old provisioning job are gone; 8 = customer ports: Site.BenchPort
// moves into Site.CustomerPorts, and Site.DisabledPorts is new; 9 = apartments (buildings)
// with their router links and caretaker/landlord contacts, Account.ApartmentID
// and Account.Unit, and the old per-router contacts are gone.
const FormatVersion = 9

const (
	OrderPending = "pending"
	OrderPaid    = "paid"
	OrderFailed  = "failed"
	OrderExpired = "expired" // no payment within the wait; a late confirmation still credits it

	PayApplied   = "applied"
	PayUnmatched = "unmatched"
	PayRefunded  = "refunded"
	PayIgnored   = "ignored"
)

// Site modes.
const (
	SiteModeWG  = "wg"  // joined over WireGuard
	SiteModeEnv = "env" // legacy rows only (the on-router container): kept so old data loads, never run
)

// Site access mode (Site.AccessMode). Every router is IPoE: the column keeps
// the single value "ipoe" (an empty value reads as ipoe).
const AccessIPoE = "ipoe"

// Site is one building: one MikroTik router, built by the bootstrap script.
type Site struct {
	ID           string `json:"id"` // "s" + 6 hex, immutable, used in portal URLs
	Name         string `json:"name"`
	Mode         string `json:"mode"` // wg | env
	Notes        string `json:"notes,omitempty"`
	SupportPhone string `json:"support_phone,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"` // paused: no collection, no router tasks
	Removed      bool   `json:"removed,omitempty"`  // tombstone; keeps ledger names

	// WireGuard / REST (Mode=wg)
	TunnelIP    string    `json:"tunnel_ip,omitempty"` // 10.99.0.x
	WGPublicKey string    `json:"wg_pub,omitempty"`
	RESTUser    string    `json:"rest_user,omitempty"` // "ftn-api"
	RESTPass    string    `json:"rest_pass,omitempty"` // 32 alphanumeric
	RESTPort    int       `json:"rest_port,omitempty"` // 8081
	Join        JoinToken `json:"join"`
	JoinedAt    time.Time `json:"joined_at,omitempty"` // first successful REST ping after a join

	// Router layout (metrics + join script)
	WANInterface   string  `json:"wan_interface,omitempty"` // "ether1"
	WANList        string  `json:"wan_list,omitempty"`      // "WAN"
	OLTInterface   string  `json:"olt_interface,omitempty"` // "sfp-sfpplus1"
	VLANID         int     `json:"vlan_id,omitempty"`       // 0 = untagged
	CustomerSubnet string  `json:"customer_subnet,omitempty"`
	WANCapMbps     float64 `json:"wan_cap_mbps,omitempty"`
	OLTCapMbps     float64 `json:"olt_cap_mbps,omitempty"`
	FUPTB          float64 `json:"fup_tb,omitempty"`
	// BenchPort is the legacy single customer port. Format 8 moves it into
	// CustomerPorts; it is always empty after load and is never written.
	BenchPort string `json:"bench_port,omitempty"`
	// CustomerPorts is the comma list of extra customer ports (members of
	// br-customers). DisabledPorts is the comma list of physical ports the
	// server switched off and keeps off (self-healed by the sweep).
	CustomerPorts string `json:"customer_ports,omitempty"`
	DisabledPorts string `json:"disabled_ports,omitempty"`
	// PortsPolicy "off" means every port except the uplink and the admin LAN stays switched off until the
	// owner enables it (EnabledPorts is the comma list of owner-enabled ports). Empty = a router that predates
	// the policy: the first sweep after the upgrade records what is in service and then sets "off".
	PortsPolicy  string `json:"ports_policy,omitempty"`
	EnabledPorts string `json:"enabled_ports,omitempty"`

	// Bootstrap script (format 7). WANMode is dhcp (the default, also when
	// empty), static or pppoe. The static fields are used for static only. The
	// PPPoE password is stored sealed with DATA_KEY (internal/secretbox), never
	// in clear. LANPorts is the admin LAN (comma list, default ether2) and
	// LANSubnet its network (default 192.168.88.0/24).
	WANMode      string `json:"wan_mode,omitempty"`
	WANAddress   string `json:"wan_address,omitempty"`
	WANGateway   string `json:"wan_gateway,omitempty"`
	WANDNS       string `json:"wan_dns,omitempty"`
	PPPoEUser    string `json:"pppoe_user,omitempty"`
	PPPoEPassEnc string `json:"pppoe_pass_enc,omitempty"`
	LANPorts     string `json:"lan_ports,omitempty"`
	LANSubnet    string `json:"lan_subnet,omitempty"`

	// PkgOverrides adjust the shared package list for this building, by package
	// ID. The map is always reassigned, never edited in place.
	PkgOverrides map[string]PkgOverride `json:"pkg_overrides,omitempty"`

	// Building profile.
	Address string    `json:"address,omitempty"`
	Lat     float64   `json:"lat,omitempty"` // 0 = unset
	Lng     float64   `json:"lng,omitempty"`
	GoLive  time.Time `json:"go_live,omitempty"`

	// AccessMode is always ipoe; empty is stored as ipoe.
	AccessMode string `json:"access_mode,omitempty"`

	// ONU management (TR-069). ONUMgmtN > 0 gives the router the management subnet 10.6.<n>.0/24 (unique per
	// router, allocated like the tunnel address) on VLAN ONUMgmtVLAN (0 = 99) of the OLT port.
	ONUMgmtN    int `json:"onu_mgmt_n,omitempty"`
	ONUMgmtVLAN int `json:"onu_mgmt_vlan,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// PkgOverride is a per-building tweak of a shared package.
type PkgOverride struct {
	Hidden   bool `json:"hidden,omitempty"`
	PriceKES int  `json:"price_kes,omitempty"` // 0 = use the shared price
}

// JoinToken is a single-use, short-lived token to fetch the join script. Only
// a SHA-256 hash of the token is stored.
type JoinToken struct {
	Hash      string    `json:"hash,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UsedAt    time.Time `json:"used_at,omitempty"`
	UsedFrom  string    `json:"used_from,omitempty"`
}

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

// RateLimit is the RouterOS rate-limit string, e.g. "10M/10M".
func (p Package) RateLimit() string { return fmt.Sprintf("%dM/%dM", p.SpeedMbps, p.SpeedMbps) }

// Account is one flat. It is created by the admin only and is locked to the
// MAC address of the flat's ONT (WAN side). Code is immutable and doubles as
// the M-Pesa Paybill account number.
type Account struct {
	ID     string `json:"id"`
	SiteID string `json:"site_id"` // the router that serves the flat
	Code   string `json:"code"`
	Flat   string `json:"flat"`
	// ApartmentID is the building the customer lives in (format 8). SiteID must
	// be one of that apartment's routers; the reconciler writes nothing for an
	// account whose router does not serve its apartment. Unit is the flat or
	// door inside the apartment ("B4"), unique per apartment among open
	// accounts. Unit is the label; Flat is kept equal to it (Account.norm), so
	// every older reader of Flat sees the same text.
	ApartmentID string `json:"apartment_id,omitempty"`
	Unit        string `json:"unit,omitempty"`
	MAC         string `json:"mac"`
	// MACConflict marks an open account whose MAC was found on another open
	// account (or malformed) by a migration. It is never provisioned and is
	// excluded from the MAC uniqueness rule until somebody fixes the MAC.
	MACConflict bool `json:"mac_conflict,omitempty"`
	// IP is the static address given to the flat (IPoE); empty = none yet.
	// Unique per building among open accounts.
	IP     string `json:"ip,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	Phone  string `json:"phone,omitempty"`
	Notes  string `json:"notes,omitempty"`
	// WhatsApp (format 2547XXXXXXXX) and WAOptOut are used by tenant messaging.
	WhatsApp string `json:"whatsapp,omitempty"`
	WAOptOut bool   `json:"wa_opt_out,omitempty"`
	// WASamePhone: the WhatsApp number is the phone number, now and when the phone changes.
	WASamePhone bool `json:"wa_same_phone,omitempty"`

	PackageID string    `json:"package_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Suspended bool      `json:"suspended,omitempty"`

	// Reminder bookkeeping: the ExpiresAt value an event was already logged for.
	RemindedFor time.Time `json:"reminded_for,omitempty"`
	ExpiredFor  time.Time `json:"expired_for,omitempty"`

	// CustomerID is the current occupant. Tenant, Phone, WhatsApp and
	// WAOptOut above stay authoritative for messaging and are copied from
	// the customer when it is saved or linked.
	CustomerID string `json:"customer_id,omitempty"`
	// Location inside the building.
	Floor        string `json:"floor,omitempty"`
	ODB          string `json:"odb,omitempty"`
	SplitterID   string `json:"splitter_id,omitempty"` // equipment id of the splitter
	SplitterPort int    `json:"splitter_port,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	// ClosedAt is set when the account is closed (it then lives in
	// Data.ClosedAccounts).
	ClosedAt time.Time `json:"closed_at,omitempty"`
}

type Order struct {
	ID                string    `json:"id"`
	SiteID            string    `json:"site_id"`
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
	CreatedAt         time.Time `json:"created_at"`
	PaidAt            time.Time `json:"paid_at,omitempty"`
}

type Voucher struct {
	Code      string    `json:"code"`
	SiteID    string    `json:"site_id,omitempty"` // "" = valid at any building
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
	SiteID     string    `json:"site_id"`
	Method     string    `json:"method"` // mpesa-manual, mpesa-stk, mpesa-c2b, voucher, test, admin
	Status     string    `json:"status"` // applied, unmatched, refunded, ignored
	AmountKES  int       `json:"amount_kes"`
	Phone      string    `json:"phone,omitempty"` // payer
	Name       string    `json:"name,omitempty"`
	BillRef    string    `json:"bill_ref,omitempty"` // what the payer typed as account number
	AccountID  string    `json:"account_id,omitempty"`
	CustomerID string    `json:"customer_id,omitempty"` // the occupant when the money was credited
	PackageID  string    `json:"package_id,omitempty"`
	OrderID    string    `json:"order_id,omitempty"`
	Note       string    `json:"note,omitempty"`
	PaidAt     time.Time `json:"paid_at"`    // when the money arrived (entered date for manual entries)
	CreatedAt  time.Time `json:"created_at"` // when it was recorded here
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// Device is a MAC that opened the payment page (or showed up on the router)
// without a matching account.
type Device struct {
	SiteID string `json:"site_id"`
	MAC    string `json:"mac"`
	IP     string `json:"ip,omitempty"`
	Source string `json:"source"` // portal, dhcp, arp, bridge (seen on a port, no address yet)
	// Port is the router port the device sits on, when the bridge host table said so.
	Port string `json:"port,omitempty"`
	// Vendor (from the MAC prefix), HostName (DHCP) and Online (lease bound or
	// ARP complete recently) help the operator pick the right MAC.
	Vendor    string    `json:"vendor,omitempty"`
	HostName  string    `json:"host_name,omitempty"`
	Online    bool      `json:"online,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Hits      int       `json:"hits"`
}

// Task is a pending router action. Reconcile tasks make the router match the
// account's current state; they are retried with backoff until they succeed
// and they survive a restart.
type Task struct {
	ID        string    `json:"id"`
	SiteID    string    `json:"site_id"`
	AccountID string    `json:"account_id"`
	Code      string    `json:"code"`
	Rev       int       `json:"rev"` // bumped when the task is refreshed
	Reason    string    `json:"reason,omitempty"`
	Attempts  int       `json:"attempts"`
	NextTry   time.Time `json:"next_try"`
	LastErr   string    `json:"last_err,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Event is a lifecycle note (reminder due, expired, audit). Seq is assigned
// by AddEvent and never reused.
type Event struct {
	Seq         int64     `json:"seq,omitempty"`
	Time        time.Time `json:"time"`
	SiteID      string    `json:"site_id,omitempty"`
	Kind        string    `json:"kind"` // reminder_due, expired, info, audit
	AccountID   string    `json:"account_id,omitempty"`
	CustomerID  string    `json:"customer_id,omitempty"`
	EquipmentID string    `json:"equipment_id,omitempty"`
	Msg         string    `json:"msg"`
	// Actor and IP say who did it and from where (audit events).
	Actor string `json:"actor,omitempty"`
	IP    string `json:"ip,omitempty"`
}

// Customer is a person (or business) who takes internet service, kept apart
// from the flat so that a flat can change hands and a person can have several.
type Customer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Phone    string `json:"phone,omitempty"`
	Phone2   string `json:"phone2,omitempty"`
	WhatsApp string `json:"whatsapp,omitempty"`
	Email    string `json:"email,omitempty"`
	Notes    string `json:"notes,omitempty"`
	WAOptOut bool   `json:"wa_opt_out,omitempty"`
	// WASamePhone: the WhatsApp number follows the phone number.
	WASamePhone bool `json:"wa_same_phone,omitempty"`
	// The national ID is optional and kept encrypted (see billing.Secretbox):
	// ciphertext, the last four digits, and a keyed hash for exact search.
	NationalIDEnc   string    `json:"national_id_enc,omitempty"`
	NationalIDHMAC  string    `json:"national_id_hmac,omitempty"`
	NationalIDLast4 string    `json:"national_id_last4,omitempty"`
	AnonymizedAt    time.Time `json:"anonymized_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// Tenancy links a customer to an account over time. MoveOut zero = current.
type Tenancy struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customer_id"`
	AccountID  string    `json:"account_id"`
	Note       string    `json:"note,omitempty"`
	MoveIn     time.Time `json:"move_in"`
	MoveOut    time.Time `json:"move_out,omitempty"`
}

// ApartmentContact is a person who looks after or owns an apartment: the
// caretaker, or the landlord or agent. Phones are stored as +254XXXXXXXXX.
type ApartmentContact struct {
	Name   string `json:"name,omitempty"`
	Phone  string `json:"phone,omitempty"`
	Phone2 string `json:"phone2,omitempty"`
	Email  string `json:"email,omitempty"`
	Notes  string `json:"notes,omitempty"`
}

// Empty reports whether nothing is filled in.
func (c ApartmentContact) Empty() bool { return c == ApartmentContact{} }

// Apartment is a building customers live in (for example "MG One Central").
// Routers serve apartments many to many: ApartmentSite rows link them. Accounts
// belong to one apartment and have a unit inside it.
type Apartment struct {
	ID        string           `json:"id"`   // "a" + 6 hex, immutable
	Name      string           `json:"name"` // unique, ignoring case
	Address   string           `json:"address,omitempty"`
	Notes     string           `json:"notes,omitempty"`
	Caretaker ApartmentContact `json:"caretaker"`
	Landlord  ApartmentContact `json:"landlord"` // landlord or agent
	// Lat and Lng are the building's GPS point in decimal degrees (WGS84). Both
	// are nil when unknown, and both are set together: 0,0 is a real point, so
	// zero never means "unset". Validate with ValidGPS; compare with SameApartment
	// (the pointers make == compare addresses, not values).
	Lat       *float64  `json:"lat,omitempty"`
	Lng       *float64  `json:"lng,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// HasGPS reports whether the apartment has a GPS point.
func (a Apartment) HasGPS() bool { return a.Lat != nil && a.Lng != nil }

// MapURL is the one-click Google Maps link for the apartment's GPS point, or
// "" when it has none.
func (a Apartment) MapURL() string {
	if !a.HasGPS() {
		return ""
	}
	return GoogleMapsURL(*a.Lat, *a.Lng)
}

// SameApartment compares two apartments by value (GPS pointers by what they point at).
func SameApartment(a, b Apartment) bool {
	return reflect.DeepEqual(a, b)
}

// GoogleMapsURL is the Google Maps "search" link that drops a pin at lat,lng.
func GoogleMapsURL(lat, lng float64) string {
	return "https://www.google.com/maps/search/?api=1&query=" + url.QueryEscape(FormatGPS(lat, lng))
}

// FormatGPS writes a point as "lat,lng" with up to 7 decimals (about 1 cm), no trailing zeros.
func FormatGPS(lat, lng float64) string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return f(math.Round(lat*1e7)/1e7) + "," + f(math.Round(lng*1e7)/1e7)
}

// ValidGPS checks a GPS point: both set or both nil, finite, lat in [-90,90], lng in [-180,180].
func ValidGPS(lat, lng *float64) error {
	if lat == nil && lng == nil {
		return nil
	}
	if lat == nil || lng == nil {
		return errors.New("give both latitude and longitude, or neither")
	}
	if math.IsNaN(*lat) || math.IsInf(*lat, 0) || *lat < -90 || *lat > 90 {
		return errors.New("latitude must be between -90 and 90")
	}
	if math.IsNaN(*lng) || math.IsInf(*lng, 0) || *lng < -180 || *lng > 180 {
		return errors.New("longitude must be between -180 and 180")
	}
	return nil
}

// ApartmentSite says that a router (site) serves an apartment.
type ApartmentSite struct {
	ApartmentID string    `json:"apartment_id"`
	SiteID      string    `json:"site_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// ApartmentSiteKey is the key of Data.ApartmentSites.
func ApartmentSiteKey(apartmentID, siteID string) string { return apartmentID + "|" + siteID }

// legacyContact is a format 7 router contact, read only to migrate it onto the
// apartment of its router (see migrateV7toV8).
type legacyContact struct {
	ID        string    `json:"id"`
	SiteID    string    `json:"site_id"`
	Role      string    `json:"role"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone,omitempty"`
	Email     string    `json:"email,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Equipment kinds and statuses.
const (
	KindONT      = "ont"
	KindRouter   = "router"
	KindOLT      = "olt"
	KindSplitter = "splitter"
	KindSwitch   = "switch"
	KindOther    = "other"

	EqInStock   = "in_stock"
	EqInstalled = "installed"
	EqFaulty    = "faulty"
	EqReturned  = "returned"
	EqRetired   = "retired"
)

// Equipment is one physical item (ONT, router, OLT, splitter...). An item
// that is installed at a customer has an AccountID; one that belongs to a
// building (its router, its OLT) has a SiteID and no account.
type Equipment struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Serial      string    `json:"serial,omitempty"`
	MAC         string    `json:"mac,omitempty"`
	Model       string    `json:"model,omitempty"`
	Firmware    string    `json:"firmware,omitempty"`
	Status      string    `json:"status"`
	SiteID      string    `json:"site_id,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// Equipment event kinds.
const (
	EqEvReceived   = "received"
	EqEvAssigned   = "assigned"
	EqEvUnassigned = "unassigned"
	EqEvReturned   = "returned"
	EqEvFaulty     = "faulty"
	EqEvRepaired   = "repaired"
	EqEvMoved      = "moved"
	EqEvEdited     = "edited"
	EqEvNote       = "note"
)

// EquipmentEvent is one line of an item's history.
type EquipmentEvent struct {
	ID          string    `json:"id"`
	EquipmentID string    `json:"equipment_id"`
	Kind        string    `json:"kind"`
	FromStatus  string    `json:"from_status,omitempty"`
	ToStatus    string    `json:"to_status,omitempty"`
	SiteID      string    `json:"site_id,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	CustomerID  string    `json:"customer_id,omitempty"`
	Note        string    `json:"note,omitempty"`
	Time        time.Time `json:"time"`
}

// Note is a free-text entry on a customer, account, building or item.
type Note struct {
	ID          string    `json:"id"`
	CustomerID  string    `json:"customer_id,omitempty"`
	AccountID   string    `json:"account_id,omitempty"`
	SiteID      string    `json:"site_id,omitempty"`
	EquipmentID string    `json:"equipment_id,omitempty"`
	Text        string    `json:"text"`
	Time        time.Time `json:"time"`
}

// Setting is one admin-edited setting. Enc means Value is sealed with the
// settings secretbox (see internal/secretbox); otherwise it is plain text.
type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Enc       bool      `json:"enc,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// maxEvents is how many events stay in memory (a database keeps them all).
const maxEvents = 300

// Data is the whole database.
type Data struct {
	Format   int                `json:"format"`
	Sites    map[string]Site    `json:"sites"`
	Packages map[string]Package `json:"packages"`
	Accounts map[string]Account `json:"accounts"`
	Orders   map[string]Order   `json:"orders"`
	Vouchers map[string]Voucher `json:"vouchers"`
	Payments map[string]Payment `json:"payments"` // key -> entry
	Devices  map[string]Device  `json:"devices"`  // siteID|MAC -> device
	Tasks    map[string]Task    `json:"tasks"`
	Events   []Event            `json:"events"`

	// Format 4.
	// ClosedAccounts holds deleted flats (same table, ClosedAt set). A key is
	// in Accounts or in ClosedAccounts, never both.
	ClosedAccounts  map[string]Account        `json:"closed_accounts"`
	Customers       map[string]Customer       `json:"customers"`
	Tenancies       map[string]Tenancy        `json:"tenancies"`
	Equipment       map[string]Equipment      `json:"equipment"`
	EquipmentEvents map[string]EquipmentEvent `json:"equipment_events"`
	Notes           map[string]Note           `json:"notes"`
	EventSeq        int64                     `json:"event_seq"` // last Event.Seq handed out

	// Format 5: admin settings (key -> row), including the admin password hash.
	Settings map[string]Setting `json:"settings"`

	// Format 8: apartments and the routers that serve them (key apartmentID|siteID).
	Apartments     map[string]Apartment     `json:"apartments"`
	ApartmentSites map[string]ApartmentSite `json:"apartment_sites"`
	// LegacyContacts holds format 7 router contacts while an old file is read;
	// migrateV7toV8 empties it, so it is never written back.
	LegacyContacts map[string]legacyContact `json:"contacts,omitempty"`

	// Format 6: sign-in. Users, browser sessions and API devices (phones).
	AdminUsers    map[string]AdminUser    `json:"admin_users"`
	AdminSessions map[string]AdminSession `json:"admin_sessions"`
	APIDevices    map[string]APIDevice    `json:"api_devices"`

	// Registrations are the captive page's self-registrations (pending, approved, rejected).
	Registrations map[string]Registration `json:"registrations"`

	// ONUs are the TR-069 devices the built-in ACS has met; ONUTasks their queued actions.
	ONUs     map[string]ONU     `json:"onus"`
	ONUTasks map[string]ONUTask `json:"onu_tasks"`
}

func (d *Data) init() {
	d.Format = FormatVersion
	if d.Sites == nil {
		d.Sites = map[string]Site{}
	}
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
	if d.Registrations == nil {
		d.Registrations = map[string]Registration{}
	}
	if d.ONUs == nil {
		d.ONUs = map[string]ONU{}
	}
	if d.ONUTasks == nil {
		d.ONUTasks = map[string]ONUTask{}
	}
	if d.ClosedAccounts == nil {
		d.ClosedAccounts = map[string]Account{}
	}
	if d.Customers == nil {
		d.Customers = map[string]Customer{}
	}
	if d.Tenancies == nil {
		d.Tenancies = map[string]Tenancy{}
	}
	if d.Apartments == nil {
		d.Apartments = map[string]Apartment{}
	}
	if d.ApartmentSites == nil {
		d.ApartmentSites = map[string]ApartmentSite{}
	}
	if d.Equipment == nil {
		d.Equipment = map[string]Equipment{}
	}
	if d.EquipmentEvents == nil {
		d.EquipmentEvents = map[string]EquipmentEvent{}
	}
	if d.Notes == nil {
		d.Notes = map[string]Note{}
	}
	if d.Settings == nil {
		d.Settings = map[string]Setting{}
	}
	if d.AdminUsers == nil {
		d.AdminUsers = map[string]AdminUser{}
	}
	if d.AdminSessions == nil {
		d.AdminSessions = map[string]AdminSession{}
	}
	if d.APIDevices == nil {
		d.APIDevices = map[string]APIDevice{}
	}
}

// AddEvent appends a lifecycle event, assigning its sequence number and
// keeping only the newest maxEvents in memory.
func (d *Data) AddEvent(e Event) {
	d.EventSeq++
	e.Seq = d.EventSeq
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

// DeviceKey is the key of d.Devices.
func DeviceKey(siteID, mac string) string { return siteID + "|" + mac }

// AccountByMAC finds the open account that owns a MAC, in any building. It
// expects a normalised MAC (AA:BB:CC:DD:EE:FF). Accounts flagged MACConflict
// do not own their MAC and are skipped; see MACConflicts to list them.
func (d *Data) AccountByMAC(mac string) (Account, bool) {
	if mac == "" {
		return Account{}, false
	}
	for _, a := range d.Accounts {
		if !a.MACConflict && strings.EqualFold(a.MAC, mac) {
			return a, true
		}
	}
	return Account{}, false
}

// OpenAccountsByMAC lists every open account with the MAC, flagged ones
// included, the owner first.
func (d *Data) OpenAccountsByMAC(mac string) []Account {
	var out []Account
	for _, a := range d.Accounts {
		if mac != "" && strings.EqualFold(a.MAC, mac) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MACConflict != out[j].MACConflict {
			return !out[i].MACConflict
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MACConflicts lists the open accounts flagged MACConflict, by building name
// then natural flat order.
func (d *Data) MACConflicts() []Account {
	var out []Account
	for _, a := range d.Accounts {
		if a.MACConflict {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		si, sj := d.Sites[out[i].SiteID].Name, d.Sites[out[j].SiteID].Name
		if si != sj {
			return NaturalLess(si, sj)
		}
		if out[i].Flat != out[j].Flat {
			return NaturalLess(out[i].Flat, out[j].Flat)
		}
		return out[i].ID < out[j].ID
	})
	return out
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

// ---- sites ----------------------------------------------------------------

// NewSiteID returns "s" + 6 random hex characters.
func NewSiteID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "s" + hex.EncodeToString(b)
}

// SiteList returns the non-removed sites, natural-sorted by name.
func (d *Data) SiteList() []Site {
	out := make([]Site, 0, len(d.Sites))
	for _, s := range d.Sites {
		if !s.Removed {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return NaturalLess(out[i].Name, out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SiteByTunnelIP finds the non-removed site that owns a WireGuard tunnel IP.
func (d *Data) SiteByTunnelIP(ip string) (Site, bool) {
	if ip == "" {
		return Site{}, false
	}
	for _, s := range d.Sites {
		if !s.Removed && s.TunnelIP == ip {
			return s, true
		}
	}
	return Site{}, false
}

// EffectivePackage is the shared package with the building's overrides
// applied: the price is replaced when the override is non-zero, and the
// package counts as disabled when the building hides it.
func (d *Data) EffectivePackage(siteID, pkgID string) (Package, bool) {
	p, ok := d.Packages[pkgID]
	if !ok {
		return Package{}, false
	}
	if o, has := d.Sites[siteID].PkgOverrides[pkgID]; has {
		if o.PriceKES > 0 {
			p.PriceKES = o.PriceKES
		}
		p.Disabled = p.Disabled || o.Hidden
	}
	return p, true
}

// EffectivePackages is PackageList with the building's overrides applied.
func (d *Data) EffectivePackages(siteID string) []Package {
	out := d.PackageList()
	for i, p := range out {
		if ep, ok := d.EffectivePackage(siteID, p.ID); ok {
			out[i] = ep
		}
	}
	return out
}

// NextTunnelIP returns the lowest free host address in prefix after the first
// host (the server), skipping addresses held by non-removed sites.
func (d *Data) NextTunnelIP(prefix netip.Prefix) (string, error) {
	prefix = prefix.Masked()
	used := map[string]bool{}
	for _, s := range d.Sites {
		if !s.Removed && s.TunnelIP != "" {
			used[s.TunnelIP] = true
		}
	}
	first := prefix.Addr().Next() // network address + 1 = the server
	for a := first.Next(); prefix.Contains(a); a = a.Next() {
		next := a.Next()
		if !prefix.Contains(next) { // the broadcast address
			break
		}
		if !used[a.String()] {
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("no free tunnel address left in %s", prefix)
}

// Runnable reports whether router work (tasks, profile sync, collection) may
// run for the site: not removed, not paused, and, for a WireGuard site, joined
// (the join script was fetched, so it has a key).
func (s Site) Runnable() bool {
	if s.Removed || s.Disabled {
		return false
	}
	return s.Mode == SiteModeWG && s.WGPublicKey != ""
}
