package store

import (
	"strings"
	"time"
)

// OLTState is the server-side management state of one OLT (an Equipment item of kind "olt"; the key is that item's
// ID): how the server reaches it, whether the server may configure it, the last status it read, the ONUs it found on
// the PON ports, and the saved copies of the running configuration. The OLT's login password stays on the Equipment
// item (sealed); every secret here (the enable password, the saved configurations) is sealed with the data key too.
// Nothing in this record is shown with a secret in it.
type OLTState struct {
	ID       string      `json:"id"`
	Settings OLTSettings `json:"settings"`
	Status   OLTStatus   `json:"status"`
	Found    []OLTFound  `json:"found,omitempty"`
	Backups  []OLTBackup `json:"backups,omitempty"`
	Updated  time.Time   `json:"updated"`
}

// OLT connection protocols.
const (
	OLTProtoSSH    = "ssh"
	OLTProtoTelnet = "telnet"
)

// DefaultOLTMgmtVLAN is the VLAN of the OLT management network (VLAN 99 is the ONU TR-069 network).
const DefaultOLTMgmtVLAN = 98

// OLTSettings are the per-OLT connection and permission settings.
type OLTSettings struct {
	Proto string `json:"proto,omitempty"` // ssh (default) or telnet
	Port  int    `json:"port,omitempty"`  // 0 = the protocol's usual port (22, 23)
	// EnablePass is the sealed enable password ("" = none).
	EnablePass string `json:"enable_pass,omitempty"`
	// HostKey is the SSH host key fingerprint pinned on first use (SHA256:...); HostKeyAt says when. A different key
	// is refused until the owner clears the pin.
	HostKey   string    `json:"host_key,omitempty"`
	HostKeyAt time.Time `json:"host_key_at,omitempty"`
	// MgmtVLAN is the OLT management VLAN (0 = 98). UplinkPort is the OLT port that goes to the router ("0/1" for
	// gigabitethernet 0/1). InternetVLAN is the customer internet VLAN on the OLT (0 = the router's VLAN, else 100).
	MgmtVLAN     int    `json:"mgmt_vlan,omitempty"`
	UplinkPort   string `json:"uplink_port,omitempty"`
	InternetVLAN int    `json:"internet_vlan,omitempty"`

	// Provisioning lets the server change the OLT's configuration; it is off until an owner turns it on (with their
	// password). DialectChecked is set by the owner after the first plan preview looked right for this OLT.
	Provisioning   bool      `json:"provisioning,omitempty"`
	ProvisioningBy string    `json:"provisioning_by,omitempty"`
	ProvisioningAt time.Time `json:"provisioning_at,omitempty"`
	DialectChecked bool      `json:"dialect_checked,omitempty"`
	CheckedBy      string    `json:"checked_by,omitempty"`
	CheckedAt      time.Time `json:"checked_at,omitempty"`
}

// OLTONU is one ONU as the OLT reports it. Strings keep what the OLT printed when it could not be parsed.
type OLTONU struct {
	PON      int    `json:"pon"`
	ID       int    `json:"id"`
	Serial   string `json:"serial,omitempty"`
	State    string `json:"state,omitempty"` // online, offline, los, dying-gasp, ... as normalised, or the raw word
	Online   bool   `json:"online"`
	RxPower  string `json:"rx_power,omitempty"` // dBm as printed
	TxPower  string `json:"tx_power,omitempty"`
	Distance string `json:"distance,omitempty"` // metres as printed
	LastDown string `json:"last_down,omitempty"`
}

// OLTStatus is the last poll.
type OLTStatus struct {
	At      time.Time `json:"at,omitempty"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Version string    `json:"version,omitempty"`
	ONUs    []OLTONU  `json:"onus,omitempty"`
	// Raw is the part of the OLT's answer that could not be parsed (capped), shown to the owner as it came.
	Raw string `json:"raw,omitempty"`
}

// Verdicts of an ONU found on a PON port.
const (
	OLTFoundAdded      = "added"      // the server authorised it
	OLTFoundReady      = "ready"      // would be added once provisioning is on
	OLTFoundUnknown    = "unknown"    // not in the inventory: never added
	OLTFoundHeld       = "held"       // in the inventory but on the wrong OLT/PON, or not assigned to a house
	OLTFoundFailed     = "failed"     // the add was tried and the OLT refused it
	OLTFoundAuthorised = "authorised" // already on the OLT
)

// OLTFound is one serial the OLT's auto-find list showed.
type OLTFound struct {
	PON         int       `json:"pon"`
	Serial      string    `json:"serial"`
	FirstSeen   time.Time `json:"first_seen,omitempty"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
	Verdict     string    `json:"verdict"`
	Detail      string    `json:"detail,omitempty"`
	EquipmentID string    `json:"equipment_id,omitempty"`
	AlertedAt   time.Time `json:"alerted_at,omitempty"`
	TriedAt     time.Time `json:"tried_at,omitempty"`
}

// OLTBackup is one saved copy of the OLT's running configuration. Text is sealed with the data key (a copy of a
// configuration may hold secrets); with no data key it is saved already redacted.
type OLTBackup struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Why    string    `json:"why,omitempty"`
	By     string    `json:"by,omitempty"`
	Size   int       `json:"size"`
	Sealed bool      `json:"sealed"`
	Text   string    `json:"text"`
}

// MaxOLTBackups is how many saved configurations an OLT keeps.
const MaxOLTBackups = 20

func (x OLTState) norm() OLTState {
	x.Settings.HostKeyAt, x.Settings.ProvisioningAt, x.Settings.CheckedAt = nt(x.Settings.HostKeyAt), nt(x.Settings.ProvisioningAt), nt(x.Settings.CheckedAt)
	x.Status.At = nt(x.Status.At)
	x.Updated = nt(x.Updated)
	for i := range x.Found {
		f := &x.Found[i]
		f.FirstSeen, f.LastSeen, f.AlertedAt, f.TriedAt = nt(f.FirstSeen), nt(f.LastSeen), nt(f.AlertedAt), nt(f.TriedAt)
	}
	for i := range x.Backups {
		x.Backups[i].At = nt(x.Backups[i].At)
	}
	if len(x.Found) == 0 {
		x.Found = nil
	}
	if len(x.Backups) == 0 {
		x.Backups = nil
	}
	if len(x.Status.ONUs) == 0 {
		x.Status.ONUs = nil
	}
	return x
}

// Norm is the form the store keeps (for tests).
func (x OLTState) Norm() OLTState { return x.norm() }

func cloneOLTState(x OLTState) OLTState {
	x.Found = append([]OLTFound(nil), x.Found...)
	x.Backups = append([]OLTBackup(nil), x.Backups...)
	x.Status.ONUs = append([]OLTONU(nil), x.Status.ONUs...)
	return x
}

// OLTProto is the protocol with its default.
func (s OLTSettings) ProtoOrDefault() string {
	if strings.EqualFold(s.Proto, OLTProtoTelnet) {
		return OLTProtoTelnet
	}
	return OLTProtoSSH
}

// PortOrDefault is the TCP port with its default.
func (s OLTSettings) PortOrDefault() int {
	if s.Port > 0 {
		return s.Port
	}
	if s.ProtoOrDefault() == OLTProtoTelnet {
		return 23
	}
	return 22
}

// MgmtVLANOrDefault is the OLT management VLAN with its default.
func (s OLTSettings) MgmtVLANOrDefault() int {
	if s.MgmtVLAN > 0 {
		return s.MgmtVLAN
	}
	return DefaultOLTMgmtVLAN
}

// UplinkOrDefault is the OLT's uplink port ("0/1").
func (s OLTSettings) UplinkOrDefault() string {
	if s.UplinkPort != "" {
		return s.UplinkPort
	}
	return "0/1"
}

// OLTStateOf returns the state of an OLT (the zero value, with the ID set, when it has none yet).
func (d *Data) OLTStateOf(id string) OLTState {
	if s, ok := d.OLTStates[id]; ok {
		return s
	}
	return OLTState{ID: id}
}
