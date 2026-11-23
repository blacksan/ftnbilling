package store

import (
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

	TaskPending = "pending"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
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
	// PassPath is the index into the profile's passphrase candidates that last worked (-1 = unknown).
	PassIdx int `json:"pass_idx,omitempty"`
	// Vendor is the quirk set applied (profile name, e.g. "tr098", "tr098-huawei", "tr181").
	Vendor string `json:"vendor,omitempty"`
	// Names are the top-level object names found by GetParameterNames NextLevel (for diagnostics).
	Discovered bool `json:"discovered,omitempty"`
}

// WiFiDesired is the Wi-Fi configuration the owner wants on the device. The passphrases are sealed with
// DATA_KEY and never shown again; SSIDs are plain.
type WiFiDesired struct {
	Set       bool      `json:"set,omitempty"`
	Same      bool      `json:"same,omitempty"` // one name and password for both bands (SSID5/Pass5Enc are ignored)
	SSID      string    `json:"ssid,omitempty"` // 2.4 GHz (or both)
	PassEnc   string    `json:"pass_enc,omitempty"`
	SSID5     string    `json:"ssid5,omitempty"`
	Pass5Enc  string    `json:"pass5_enc,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	// Changed is set when the device reports an SSID different from the desired one (VALUE CHANGE): somebody
	// changed it on the device itself.
	Changed bool `json:"changed,omitempty"`
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
	ConnReqUserEnc string      `json:"cr_user_enc,omitempty"`
	ConnReqPassEnc string      `json:"cr_pass_enc,omitempty"`
	CRApplied      bool        `json:"cr_applied,omitempty"` // the credentials were written to the device
	FirstSeen      time.Time   `json:"first_seen,omitempty"`
	LastInform     time.Time   `json:"last_inform,omitempty"`
	LastEvents     string      `json:"last_events,omitempty"` // comma list of event codes of the last Inform
	MACs           []string    `json:"macs,omitempty"`        // WAN MACs, AA:BB:CC:DD:EE:FF
	WiFi           []ONUWiFi   `json:"wifi,omitempty"`
	Profile        ONUProfile  `json:"profile"`
	AccountID      string      `json:"account_id,omitempty"`
	Link           string      `json:"link,omitempty"`
	Desired        WiFiDesired `json:"desired"`
}

func (x ONU) norm() ONU {
	x.FirstSeen, x.LastInform = nt(x.FirstSeen), nt(x.LastInform)
	x.Desired.UpdatedAt = nt(x.Desired.UpdatedAt)
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
}

func (x ONUTask) norm() ONUTask {
	x.CreatedAt, x.StartedAt, x.FinishedAt = nt(x.CreatedAt), nt(x.StartedAt), nt(x.FinishedAt)
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
