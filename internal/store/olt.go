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

	// Adopt is how far the OLT has come with being taken over through the router's OLT management port.
	Adopt OLTAdopt `json:"adopt,omitempty"`
}

// OLT adoption states (OLTAdopt.Stage): the next thing that has to happen.
const (
	AdoptNoRouter   = "no_router"   // the OLT is not on a router
	AdoptNoPort     = "no_port"     // the router has no OLT management port
	AdoptNoSSH      = "no_ssh"      // the OLT's SSH does not answer (a factory OLT has it off)
	AdoptNoCable    = "no_cable"    // nothing is seen on the OLT management port
	AdoptUnknownMAC = "unknown_mac" // another device than this OLT is on the port: listed as a possible OLT
	AdoptNeedLogin  = "need_login"  // the OLT was reached but no login worked: the owner types one
	AdoptMismatch   = "mismatch"    // the device on the port is not this OLT (serial or model): nothing is configured
	AdoptNeedSecure = "need_secure" // the login works with a login that must be replaced (owner presses Secure)
	AdoptSecured    = "secured"     // adopted and secured
	AdoptAdopted    = "adopted"     // adopted with the owner's own login (not changed by the server)
	AdoptManual     = "manual"      // the owner chose the console snippet instead
	AdoptFailed     = "failed"      // the last step failed (see Error)
)

// OLTAdopt is the adoption progress of one OLT. Nothing in it is a secret: the passwords are sealed on the equipment item
// (login) and in OLTSettings.EnablePass (enable); Pending* hold a new pair sealed while a change is in flight.
type OLTAdopt struct {
	Stage     string    `json:"stage,omitempty"`
	Note      string    `json:"note,omitempty"` // the next action in words
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// Manual: the owner uses the console snippet (the old adopt page) and the server does not use the management port.
	Manual bool `json:"manual,omitempty"`

	Port      string    `json:"port,omitempty"` // the router's OLT management port
	MACSeen   bool      `json:"mac_seen"`       // this OLT's MAC is on that port
	MACSeenAt time.Time `json:"mac_seen_at,omitempty"`
	OtherMACs []string  `json:"other_macs,omitempty"` // other devices seen on the port

	SSHOK       bool      `json:"ssh_ok"`
	SSHAt       time.Time `json:"ssh_at,omitempty"`
	SerialOK    string    `json:"serial_ok,omitempty"` // "" not checked, "ok", "mismatch"
	SerialSeen  string    `json:"serial_seen,omitempty"`
	ModelSeen   string    `json:"model_seen,omitempty"`
	CredSource  string    `json:"cred_source,omitempty"` // stored, factory, owner, secured
	FactoryAt   time.Time `json:"factory_at,omitempty"`  // the vendor default login was tried (once; never again by the server)
	FactoryOK   bool      `json:"factory_ok,omitempty"`
	Adopted     bool      `json:"adopted,omitempty"`
	Secured     bool      `json:"secured,omitempty"`
	SecuredAt   time.Time `json:"secured_at,omitempty"`
	SecuredBy   string    `json:"secured_by,omitempty"`
	SNMPOff     bool      `json:"snmp_off,omitempty"`
	SNMPNote    string    `json:"snmp_note,omitempty"`
	ACLNote     string    `json:"acl_note,omitempty"`
	ACLDone     bool      `json:"acl_done,omitempty"`
	AlertedAt   time.Time `json:"alerted_at,omitempty"` // the last "possible OLT" alert
	RevealedAt  time.Time `json:"revealed_at,omitempty"`
	RevealedBy  string    `json:"revealed_by,omitempty"`
	PendingPass string    `json:"pending_pass,omitempty"` // sealed: a new login password being set (crash recovery)
	PendingEn   string    `json:"pending_en,omitempty"`   // sealed: its enable password
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
	Voltage  string `json:"voltage,omitempty"`  // V
	Bias     string `json:"bias,omitempty"`     // mA
	Temp     string `json:"temp,omitempty"`     // C
	Distance string `json:"distance,omitempty"` // metres as printed
	LastDown string `json:"last_down,omitempty"`
}

// OLTStatus is the last poll.
type OLTStatus struct {
	At      time.Time `json:"at,omitempty"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Version string    `json:"version,omitempty"`
	// What "show version" printed, one field each (empty when the OLT did not say).
	Model        string `json:"model,omitempty"`
	DeviceSerial string `json:"device_serial,omitempty"`
	HWVersion    string `json:"hw_version,omitempty"`
	SWVersion    string `json:"sw_version,omitempty"`
	// Findings are things the last read noticed about the OLT's own settings (read only; the server never changes them).
	Findings []string `json:"findings,omitempty"`
	ONUs     []OLTONU `json:"onus,omitempty"`
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
	a := &x.Settings.Adopt
	a.CheckedAt, a.MACSeenAt, a.SSHAt, a.FactoryAt, a.SecuredAt, a.AlertedAt, a.RevealedAt = nt(a.CheckedAt), nt(a.MACSeenAt), nt(a.SSHAt), nt(a.FactoryAt), nt(a.SecuredAt), nt(a.AlertedAt), nt(a.RevealedAt)
	if len(a.OtherMACs) == 0 {
		a.OtherMACs = nil
	}
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

// Clone copies the slices, so a copy can be changed without touching the stored value.
func (x OLTState) Clone() OLTState { return cloneOLTState(x) }

func cloneOLTState(x OLTState) OLTState {
	x.Settings.Adopt.OtherMACs = append([]string(nil), x.Settings.Adopt.OtherMACs...)
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

// DeviceSourceOLTMgmt is Device.Source of a device seen on a router's OLT management port that is not (or not yet proven to
// be) an OLT of the inventory: it is listed under New devices as a "Possible OLT" and is never configured.
const DeviceSourceOLTMgmt = "oltmgmt"
