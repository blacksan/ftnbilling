package store

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// ONU management (TR-069 / CWMP). An ONU (the customer's fibre modem with Wi-Fi) is a CPE of the built-in
// ACS (internal/acs). One row per device, found by its serial number; an account owns at most one.

// Data model roots of a CPE.
const (
	RootTR098 = "InternetGatewayDevice."
	RootTR181 = "Device."
)

// Link kinds of an ONU to an account.
const (
	LinkAuto      = "auto"      // the ONU's WAN MAC is the account's MAC on the same router
	LinkInventory = "inventory" // the ONU's serial is an ONT of the equipment register installed at the house
	LinkManual    = "manual"    // the owner picked the account
)

// ONU task kinds and statuses.
const (
	TaskWiFi         = "wifi"          // apply the desired Wi-Fi configuration
	TaskReboot       = "reboot"        // Reboot RPC
	TaskFactoryReset = "factory_reset" // FactoryReset RPC (implemented; not offered in the UI)
	TaskRefresh      = "refresh"       // read the current Wi-Fi names (no change)

	TaskPending = "pending" // queued: the ONU has not checked in yet
	TaskRunning = "running" // sent: the ACS is talking to the ONU now
	TaskDone    = "done"    // finished (reboot, factory reset, refresh)
	TaskFailed  = "failed"
	// A Wi-Fi change is never "done" on the ONU's word alone: the ONU accepts it (accepted, or "after a reboot"),
	// and only the owner, who has seen a phone join the new network, confirms it. A task that waited too long for
	// the ONU is expired and never sent.
	TaskAccepted  = "accepted"
	TaskConfirmed = "confirmed"
	TaskExpired   = "expired"
)

// How long a queued task waits for the ONU before it expires (it would otherwise run days later, surprising
// whoever then holds the box).
const (
	ExpireReboot       = time.Hour
	ExpireWiFi         = 24 * time.Hour
	ExpireFactoryReset = 7 * 24 * time.Hour
	ExpireRefresh      = time.Hour
	// ONUStaleAfter: an unlinked, unknown or quarantined ONU that has not informed for this long is forgotten.
	ONUStaleAfter = 7 * 24 * time.Hour
)

// TaskTTL is how long a task of this kind may wait in the queue.
func TaskTTL(kind string) time.Duration {
	switch kind {
	case TaskReboot:
		return ExpireReboot
	case TaskWiFi:
		return ExpireWiFi
	case TaskFactoryReset:
		return ExpireFactoryReset
	}
	return ExpireRefresh
}

// Per-device ACS login states (ONUSec.ACSCred).
const (
	CredNone    = ""        // not yet written
	CredPending = "pending" // written; the ONU has not logged in with it yet
	CredDevice  = "device"  // the ONU logs in with its own login
	CredShared  = "shared"  // the ONU refused the write: it logs in with the shared login only (weaker)
)

// Admin-lock states (ONUSec.AdminPass).
const (
	AdminNone        = ""
	AdminUnverified  = "unverified"  // a password was written; it cannot be read back
	AdminUnsupported = "unsupported" // no known path took it
)

// ONUWiFi is one Wi-Fi radio/SSID instance found on the device. Path is the object path up to and including the
// instance number and the trailing dot (".WLANConfiguration.1." under TR-098, "Device.WiFi.SSID.1." under TR-181).
// AP is, for TR-181, the AccessPoint object that belongs to that SSID ("" for TR-098).
type ONUWiFi struct {
	Path    string `json:"path"`
	AP      string `json:"ap,omitempty"`
	Band    string `json:"band,omitempty"` // "2.4" | "5" | ""
	SSID    string `json:"ssid,omitempty"`
	Enabled bool   `json:"enabled,omitempty"`
	Channel int    `json:"channel,omitempty"`
}

// ONUProfile is what discovery learned about the device: which data model root it speaks and the parameter
// candidates that worked (cached so the next Set goes straight to the right paths).
type ONUProfile struct {
	Root string `json:"root,omitempty"`
	// PassLeaf is the passphrase parameter (relative to the Wi-Fi instance) the owner confirmed on this box
	// ("It works"); it is tried first. A change the box merely accepted is not remembered here.
	PassLeaf string `json:"pass_leaf,omitempty"`
	// Vendor is the quirk set applied (profile name, e.g. "tr098", "tr098-huawei", "tr181").
	Vendor string `json:"vendor,omitempty"`
	// Names are the top-level object names found by GetParameterNames NextLevel (for diagnostics).
	Discovered bool `json:"discovered,omitempty"`
}

// WiFiDesired is the Wi-Fi configuration the owner wants at a house (it belongs to the account, not to the box:
// a swapped or returned ONU never carries it away). The passphrases are sealed with DATA_KEY and never shown
// again; SSIDs are plain.
type WiFiDesired struct {
	Set       bool      `json:"set,omitempty"`
	Same      bool      `json:"same,omitempty"` // one name and password for both bands (SSID5/Pass5Enc are ignored)
	SSID      string    `json:"ssid,omitempty"` // 2.4 GHz (or both)
	PassEnc   string    `json:"pass_enc,omitempty"`
	SSID5     string    `json:"ssid5,omitempty"`
	Pass5Enc  string    `json:"pass5_enc,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// ONUQuarantine: a check-in of a known, linked serial that could not be tied to the box it belongs to (another
// router, another address whose lease belongs to another MAC, or a flapping serial). The ACS answers it with no
// secret and no task, leaves the record where it was, and waits for the owner to approve or block it.
type ONUQuarantine struct {
	Active  bool      `json:"active,omitempty"`
	Since   time.Time `json:"since,omitempty"`
	LastAt  time.Time `json:"last_at,omitempty"`
	Count   int       `json:"count,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	SiteID  string    `json:"site_id,omitempty"` // where the offending check-in came from
	IP      string    `json:"ip,omitempty"`
	MAC     string    `json:"mac,omitempty"` // the MAC the router leased that address to ("" = unknown)
	Alerted time.Time `json:"alerted,omitempty"`
}

// ONUSec is the security state of an ONU: its own ACS login, the management-WAN MAC it is bound to, the
// quarantine, the admin lock.
type ONUSec struct {
	// ACSUser is this device's own ACS user name (not secret); ACSPassEnc its password, sealed.
	ACSUser    string `json:"acs_user,omitempty"`
	ACSPassEnc string `json:"acs_pass_enc,omitempty"`
	ACSCred    string `json:"acs_cred,omitempty"` // Cred*
	ACSTries   int    `json:"acs_tries,omitempty"`
	// MgmtMAC is the MAC the router's management DHCP server leased the ONU's management address to. It is
	// hardware: it survives a factory reset.
	MgmtMAC    string        `json:"mgmt_mac,omitempty"`
	Blocked    bool          `json:"blocked,omitempty"`
	Quarantine ONUQuarantine `json:"quarantine"`
	FlapCount  int           `json:"flap_count,omitempty"`
	FlapSince  time.Time     `json:"flap_since,omitempty"`
	// AdminPassEnc is the random password set on the ONU's own web admin (sealed); AdminPass the state (Admin*).
	AdminPassEnc string `json:"admin_pass_enc,omitempty"`
	AdminPass    string `json:"admin_pass,omitempty"`
}

// ONUState is what the ACS remembers about the box between sessions.
type ONUState struct {
	// LastKey and PrevKey are the ParameterKey values of the last two writes. An Inform that reports another
	// (or an empty) key says the box forgot what we wrote: a factory reset.
	LastKey string `json:"last_key,omitempty"`
	PrevKey string `json:"prev_key,omitempty"`
	// NoWiFi: discovery found no Wi-Fi on this box (an SFU or bridge): it is not discovered again every Inform.
	NoWiFi bool `json:"no_wifi,omitempty"`
	// AppliedFor and AppliedAt: the house and the Wi-Fi version (Account.HomeWiFi.UpdatedAt) this box accepted last.
	AppliedFor string    `json:"applied_for,omitempty"`
	AppliedAt  time.Time `json:"applied_at,omitempty"`
	// WiFiChanged is set when the box reports an SSID different from the house's choice (VALUE CHANGE): somebody
	// changed it on the box itself.
	WiFiChanged bool `json:"wifi_changed,omitempty"`
	// Returned: the box left its house (swap or unlink). It never receives a house's Wi-Fi until linked again.
	Returned   bool      `json:"returned,omitempty"`
	ReturnedAt time.Time `json:"returned_at,omitempty"`
}

// ONU is one CPE.
type ONU struct {
	ID           string `json:"id"` // "o" + 8 hex
	Serial       string `json:"serial"`
	OUI          string `json:"oui,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	ProductClass string `json:"product_class,omitempty"`
	SWVersion    string `json:"sw_version,omitempty"`
	HWVersion    string `json:"hw_version,omitempty"`
	Root         string `json:"root,omitempty"` // RootTR098 | RootTR181
	SiteID       string `json:"site_id,omitempty"`
	MgmtIP       string `json:"mgmt_ip,omitempty"`
	ConnReqURL   string `json:"conn_req_url,omitempty"`
	// ConnReqUserEnc and ConnReqPassEnc are the per-device connection-request credentials, sealed.
	ConnReqUserEnc string     `json:"cr_user_enc,omitempty"`
	ConnReqPassEnc string     `json:"cr_pass_enc,omitempty"`
	CRApplied      bool       `json:"cr_applied,omitempty"` // the credentials were written to the device
	FirstSeen      time.Time  `json:"first_seen,omitempty"`
	LastInform     time.Time  `json:"last_inform,omitempty"`
	LastEvents     string     `json:"last_events,omitempty"` // comma list of event codes of the last Inform
	MACs           []string   `json:"macs,omitempty"`        // WAN MACs, AA:BB:CC:DD:EE:FF
	WiFi           []ONUWiFi  `json:"wifi,omitempty"`
	Profile        ONUProfile `json:"profile"`
	AccountID      string     `json:"account_id,omitempty"`
	Link           string     `json:"link,omitempty"`
	Sec            ONUSec     `json:"sec"`
	State          ONUState   `json:"state"`
	// LegacyDesired is read from older files only: the Wi-Fi choice used to be kept on the box and now belongs to
	// the account (Account.HomeWiFi). It is moved on load and never written.
	LegacyDesired *WiFiDesired `json:"desired,omitempty"`
}

func (x ONU) norm() ONU {
	x.FirstSeen, x.LastInform = nt(x.FirstSeen), nt(x.LastInform)
	q := &x.Sec.Quarantine
	q.Since, q.LastAt, q.Alerted = nt(q.Since), nt(q.LastAt), nt(q.Alerted)
	x.Sec.FlapSince = nt(x.Sec.FlapSince)
	x.State.AppliedAt, x.State.ReturnedAt = nt(x.State.AppliedAt), nt(x.State.ReturnedAt)
	if x.LegacyDesired != nil {
		c := *x.LegacyDesired
		c.UpdatedAt = nt(c.UpdatedAt)
		x.LegacyDesired = &c
	}
	if len(x.MACs) == 0 {
		x.MACs = nil
	}
	if len(x.WiFi) == 0 {
		x.WiFi = nil
	}
	return x
}

// Norm returns the row as it will be stored.
func (x ONU) Norm() ONU { return x.norm() }

func cloneONU(o ONU) ONU {
	o.MACs = append([]string(nil), o.MACs...)
	o.WiFi = append([]ONUWiFi(nil), o.WiFi...)
	if o.LegacyDesired != nil {
		c := *o.LegacyDesired
		o.LegacyDesired = &c
	}
	return o
}

// CloneONU copies the slices of an ONU so a caller may change them.
func CloneONU(o ONU) ONU { return cloneONU(o) }

// ONUTask is one queued action for an ONU. It carries no secret: a wifi task reads the sealed desired
// configuration of the ONU when it runs.
type ONUTask struct {
	ID         string    `json:"id"` // "t" + 8 hex
	ONUID      string    `json:"onu_id"`
	SiteID     string    `json:"site_id,omitempty"`
	AccountID  string    `json:"account_id,omitempty"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Fault      string    `json:"fault,omitempty"` // fault text of a failed task, never a parameter value
	By         string    `json:"by,omitempty"`    // who asked ("system" for the automatic restore)
	Attempts   int       `json:"attempts,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// AfterReboot: the ONU accepted the change with status 1 (it applies after the box restarts).
	AfterReboot bool `json:"after_reboot,omitempty"`
	// PassLeaf is the passphrase parameter (relative to the instance) the accepted change used; it is the thing the
	// owner's "It works" confirmation promotes into the per-model table. Not secret.
	PassLeaf    string    `json:"pass_leaf,omitempty"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
	ConfirmedBy string    `json:"confirmed_by,omitempty"`
}

func (x ONUTask) norm() ONUTask {
	x.CreatedAt, x.StartedAt, x.FinishedAt = nt(x.CreatedAt), nt(x.StartedAt), nt(x.FinishedAt)
	x.ConfirmedAt = nt(x.ConfirmedAt)
	return x
}

// Norm returns the row as it will be stored.
func (x ONUTask) Norm() ONUTask { return x.norm() }

// Active reports whether the task still has to run (or is running).
func (x ONUTask) Active() bool { return x.Status == TaskPending || x.Status == TaskRunning }

// ONUBySerial finds a device by serial number (and OUI when both are known).
func (d *Data) ONUBySerial(oui, serial string) (ONU, bool) {
	for _, o := range d.ONUs {
		if o.Serial == serial && (o.OUI == "" || oui == "" || o.OUI == oui) {
			return o, true
		}
	}
	return ONU{}, false
}

// ONUOfAccount finds the ONU linked to an account.
func (d *Data) ONUOfAccount(accountID string) (ONU, bool) {
	if accountID == "" {
		return ONU{}, false
	}
	for _, o := range d.ONUs {
		if o.AccountID == accountID {
			return o, true
		}
	}
	return ONU{}, false
}

// ONUList lists the ONUs, optionally of one router (site "" = all), by last inform (newest first).
func (d *Data) ONUList(siteID string) []ONU {
	var out []ONU
	for _, o := range d.ONUs {
		if siteID == "" || o.SiteID == siteID {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastInform.Equal(out[j].LastInform) {
			return out[i].LastInform.After(out[j].LastInform)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ONUTasksOf lists the tasks of one ONU, newest first.
func (d *Data) ONUTasksOf(onuID string) []ONUTask {
	var out []ONUTask
	for _, t := range d.ONUTasks {
		if t.ONUID == onuID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// MgmtSubnet is the ONU management subnet of a site ("10.6.<n>.0/24"), "" when none is allocated.
func (s Site) MgmtSubnet() string {
	if s.ONUMgmtN <= 0 || s.ONUMgmtN > 255 {
		return ""
	}
	return "10.6." + itoa(s.ONUMgmtN) + ".0/24"
}

// MgmtGateway is the router's own address on the management VLAN ("10.6.<n>.1").
func (s Site) MgmtGateway() string {
	if s.ONUMgmtN <= 0 || s.ONUMgmtN > 255 {
		return ""
	}
	return "10.6." + itoa(s.ONUMgmtN) + ".1"
}

// MgmtVLANOrDefault is the ONU management VLAN of a site (99 unless set).
func (s Site) MgmtVLANOrDefault() int {
	if s.ONUMgmtVLAN > 0 {
		return s.ONUMgmtVLAN
	}
	return DefaultONUMgmtVLAN
}

// DefaultONUMgmtVLAN is the VLAN the ONUs' TR-069 management network uses unless a router says otherwise.
const DefaultONUMgmtVLAN = 99

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// NextONUMgmtN returns the lowest free management subnet number (1..255) among the non-removed sites.
func (d *Data) NextONUMgmtN() (int, bool) {
	used := map[int]bool{}
	for _, s := range d.Sites {
		if !s.Removed && s.ONUMgmtN > 0 {
			used[s.ONUMgmtN] = true
		}
	}
	for n := 1; n <= 255; n++ {
		if !used[n] {
			return n, true
		}
	}
	return 0, false
}

// SiteByONUIP finds the non-removed site whose management subnet holds ip (dotted quad).
func (d *Data) SiteByONUIP(ip string) (Site, bool) {
	if !strings.HasPrefix(ip, "10.6.") {
		return Site{}, false
	}
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return Site{}, false
	}
	for _, s := range d.Sites {
		if s.Removed || s.ONUMgmtN <= 0 {
			continue
		}
		if parts[2] == itoa(s.ONUMgmtN) {
			return s, true
		}
	}
	return Site{}, false
}

// NormSerial is the form serials are compared in: upper case, no spaces.
func NormSerial(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// ONTBySerial finds an ONT of the equipment register by the serial an ONU reports over TR-069, matching its
// Serial or its GPONSerial. Retired items are skipped.
func (d *Data) ONTBySerial(serial string) (Equipment, bool) {
	n := NormSerial(serial)
	if n == "" {
		return Equipment{}, false
	}
	var best Equipment
	found := false
	for _, e := range d.Equipment {
		if e.Kind != KindONT || e.Status == EqRetired {
			continue
		}
		if NormSerial(e.Serial) == n || NormSerial(e.GPONSerial) == n {
			// an installed item beats a stock one when (wrongly) two carry the serial
			if !found || e.Status == EqInstalled {
				best, found = e, true
			}
		}
	}
	return best, found
}

func newONUTaskID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "t" + hex.EncodeToString(b)
}

// QueueONUTask adds a task to the ONU unless an active one of the same kind exists (then that one is returned).
// A Wi-Fi change joins only a pending one: a running one may already have sent the previous name and password.
func (d *Data) QueueONUTask(o ONU, kind, by string, now time.Time) ONUTask {
	for _, t := range d.ONUTasksOf(o.ID) {
		if t.Kind == kind && t.Active() && (kind != TaskWiFi || t.Status == TaskPending) {
			return t
		}
	}
	t := ONUTask{ID: newONUTaskID(), ONUID: o.ID, SiteID: o.SiteID, AccountID: o.AccountID, Kind: kind, Status: TaskPending, By: by, CreatedAt: now}
	d.ONUTasks[t.ID] = t
	d.pruneONUTasks(o.ID)
	return t
}

// pruneONUTasks keeps the newest 30 finished tasks of a device.
func (d *Data) pruneONUTasks(onuID string) {
	var done []ONUTask
	for _, t := range d.ONUTasksOf(onuID) {
		if !t.Active() {
			done = append(done, t)
		}
	}
	for i := 30; i < len(done); i++ {
		delete(d.ONUTasks, done[i].ID)
	}
}

// CancelONUTasks cancels the queued (not yet running) tasks of an ONU of one kind ("" = every kind) and returns how many.
func (d *Data) CancelONUTasks(onuID, kind string, now time.Time, why string) int {
	n := 0
	for _, t := range d.ONUTasksOf(onuID) {
		if t.Status == TaskPending && (kind == "" || t.Kind == kind) {
			t.Status, t.Fault, t.FinishedAt = TaskFailed, why, nt(now)
			d.ONUTasks[t.ID] = t
			n++
		}
	}
	return n
}

// ReleaseONU takes an ONU off its house: the link goes, the box is marked returned (it never receives a house's
// Wi-Fi again until the owner links it), its queued Wi-Fi changes are cancelled, and a FactoryReset is queued for its
// next check-in so the old tenant's Wi-Fi name and password do not travel with it. Returns the queued reset.
func (d *Data) ReleaseONU(onuID string, now time.Time, by string) (ONUTask, bool) {
	o, ok := d.ONUs[onuID]
	if !ok {
		return ONUTask{}, false
	}
	d.CancelONUTasks(o.ID, TaskWiFi, now, "the ONU left this house")
	o.AccountID, o.Link = "", ""
	o.State.Returned, o.State.ReturnedAt = true, nt(now)
	o.State.AppliedFor, o.State.AppliedAt = "", time.Time{}
	d.ONUs[o.ID] = o
	t := d.QueueONUTask(o, TaskFactoryReset, by, now)
	return t, true
}

// ONUsOfSerial finds every ONU row whose serial is serial (normalised), for the returned equipment item.
func (d *Data) ONUsOfSerial(serials ...string) []ONU {
	want := map[string]bool{}
	for _, s := range serials {
		if n := NormSerial(s); n != "" {
			want[n] = true
		}
	}
	var out []ONU
	for _, o := range d.ONUs {
		if want[NormSerial(o.Serial)] {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// moveLegacyDesired moves the Wi-Fi choice older files kept on the ONU to its house, once. Idempotent.
func (d *Data) moveLegacyDesired() {
	for id, o := range d.ONUs {
		if o.LegacyDesired == nil {
			continue
		}
		ld := *o.LegacyDesired
		o.LegacyDesired = nil
		d.ONUs[id] = o
		if a, ok := d.Accounts[o.AccountID]; ok && ld.Set && !a.HomeWiFi.Set {
			a.HomeWiFi = ld
			d.Accounts[a.ID] = a
		}
	}
}
