package acs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

// Settings keys of the global ACS login (the CPEs' ManagementServer.Username/Password).
const (
	KeyUser = "acs.user"
	KeyPass = "acs.pass"
)

// Options builds a Service.
type Options struct {
	Store   *store.Store
	Cfg     config.Config
	Log     *slog.Logger
	DataKey []byte
	Now     func() time.Time
	// HTTPClient makes the connection requests (tests replace its transport). Nil = a client with short timeouts.
	HTTPClient *http.Client
	// ClientIP finds the source address of a request; nil = the TCP peer address (the ACS has no proxy in front).
	ClientIP func(*http.Request) (netip.Addr, bool)
	// Lease says which MAC the router leased a management address to (the router sweep's copy of its DHCP leases).
	// Nil = unknown: a known serial that logs in with the shared login is then answered with nothing.
	Lease LeaseFunc
	// Alert tells the owners something needs a look (WhatsApp and e-mail). Nil = the audit trail only.
	Alert func(subject, msg string)
}

// Service is the ACS: the HTTP handler the ONUs talk to, and the operations the admin pages and the app use.
type Service struct {
	st     *store.Store
	cfg    config.Config
	log    *slog.Logger
	box    *secretbox.Box
	now    func() time.Time
	da     *digestAuth
	client *http.Client
	ipOf   func(*http.Request) (netip.Addr, bool)
	lease  LeaseFunc
	alert  func(subject, msg string)

	mu       sync.Mutex
	sessions map[string]*session
	ips      map[netip.Addr]*ipState
	kicks    map[string]Kick
	capLog   map[string]time.Time
	unkLog   []time.Time // when owners were last told about an unknown serial
	wg       sync.WaitGroup
	live     liveReads // pending reads of the Wi-Fi passphrase from a box (wifipass.go)
}

// New makes the service. DATA_KEY is required (passphrases and logins are sealed with it).
func New(o Options) (*Service, error) {
	box, err := secretbox.NewLabelled(o.DataKey, "ispbill/acs", "ispbill/acs-idx")
	if err != nil {
		return nil, fmt.Errorf("ONU management needs DATA_KEY: %w", err)
	}
	s := &Service{st: o.Store, cfg: o.Cfg, log: o.Log, box: box, now: o.Now, client: o.HTTPClient, ipOf: o.ClientIP, lease: o.Lease, alert: o.Alert,
		sessions: map[string]*session{}, ips: map[netip.Addr]*ipState{}, kicks: map[string]Kick{}, capLog: map[string]time.Time{}}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.ipOf == nil {
		s.ipOf = peerAddr
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: crTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	s.da = newDigestAuth(s.now)
	if err := s.EnsureCredentials(); err != nil {
		return nil, err
	}
	return s, nil
}

func peerAddr(r *http.Request) (netip.Addr, bool) {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// Errors of the operations.
var (
	ErrNoONU     = errors.New("this house has no ONU linked yet")
	ErrONUGone   = errors.New("that ONU is not known")
	ErrNoAccount = errors.New("that house was not found")
	ErrLinked    = errors.New("that house already has an ONU linked: unlink it first")
	ErrONULinked = errors.New("that ONU is already linked to another house: unlink it first")
	ErrSite      = errors.New("the ONU and the house are on different routers")
)

// FieldError is a refused input.
type FieldError struct{ Field, Msg string }

func (e *FieldError) Error() string { return e.Msg }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const alnum = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randAlnum(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

// ---- the global ACS login ----

// ErrCredsUnreadable: the stored ACS password cannot be opened (DATA_KEY changed). Making a new one silently would
// cut every ONU off without a word, so ONU management refuses to start instead.
var ErrCredsUnreadable = errors.New("the stored ACS password cannot be opened, so ONU management (TR-069) stays off: DATA_KEY has probably changed. " +
	"Restore the previous DATA_KEY. If it is lost for good, start once with ACS_RESET_CREDENTIALS=true to make a new ACS login " +
	"(then every ONU profile on the OLT must be updated with it, and every box needs a factory reset or a visit)")

// EnsureCredentials makes the global ACS user and password on first run and keeps them (sealed) in settings. A
// stored password that cannot be opened is never replaced by itself (L5): New fails with ErrCredsUnreadable unless
// ACS_RESET_CREDENTIALS is set.
func (s *Service) EnsureCredentials() error {
	err := s.st.Update(func(d *store.Data) error {
		if d.Settings[KeyUser].Value != "" && d.Settings[KeyPass].Value != "" {
			if _, err := s.box.OpenAAD(d.Settings[KeyPass].Value, []byte(KeyPass)); err == nil {
				return store.ErrNoChange
			}
			if !s.cfg.ACSResetCreds {
				return ErrCredsUnreadable
			}
			s.log.Warn("ACS: the stored ACS login could not be opened; a new one is made because ACS_RESET_CREDENTIALS is set")
		}
		return s.writeCreds(d, "ftn-"+randHex(3), randAlnum(20))
	})
	if errors.Is(err, ErrCredsUnreadable) {
		s.log.Error("ACS: " + err.Error())
	}
	return err
}

func (s *Service) writeCreds(d *store.Data, user, pass string) error {
	sealed, err := s.box.SealAAD(pass, []byte(KeyPass))
	if err != nil {
		return err
	}
	now := s.now()
	d.Settings[KeyUser] = store.Setting{Key: KeyUser, Value: user, UpdatedAt: now}
	d.Settings[KeyPass] = store.Setting{Key: KeyPass, Value: sealed, Enc: true, UpdatedAt: now}
	return nil
}

// Credentials returns the global ACS login. Callers that show it must make the owner step up first.
func (s *Service) Credentials() (user, pass string, err error) {
	var u, sealed string
	s.st.View(func(d *store.Data) { u, sealed = d.Settings[KeyUser].Value, d.Settings[KeyPass].Value })
	if u == "" || sealed == "" {
		return "", "", errors.New("the ACS login is not set")
	}
	p, err := s.box.OpenAAD(sealed, []byte(KeyPass))
	if err != nil {
		return "", "", errors.New("the ACS password cannot be opened (DATA_KEY changed?)")
	}
	return u, p, nil
}

// ACSUser is the global ACS user name (not secret).
func (s *Service) ACSUser() string {
	var u string
	s.st.View(func(d *store.Data) { u = d.Settings[KeyUser].Value })
	return u
}

// RotateCredentials makes a new global ACS password. Every ONU still holds the old one in its profile: they
// stop being accepted until the OLT profile is updated, so this is a deliberate act.
func (s *Service) RotateCredentials(by, ip string) error {
	return s.st.Update(func(d *store.Data) error {
		u := d.Settings[KeyUser].Value
		if u == "" {
			u = "ftn-" + randHex(3)
		}
		if err := s.writeCreds(d, u, randAlnum(20)); err != nil {
			return err
		}
		d.AddEvent(store.Event{Time: s.now(), Kind: "audit", Msg: "ONU management (TR-069): the ACS password was replaced", Actor: by, IP: ip})
		return nil
	})
}

// ---- per-device connection-request login ----

func (s *Service) sealCR(user, pass string) (string, string, error) {
	u, err := s.box.SealAAD(user, []byte("cr.user"))
	if err != nil {
		return "", "", err
	}
	p, err := s.box.SealAAD(pass, []byte("cr.pass"))
	return u, p, err
}

func (s *Service) openCR(o store.ONU) (user, pass string, ok bool) {
	if o.ConnReqUserEnc == "" || o.ConnReqPassEnc == "" {
		return "", "", false
	}
	u, err1 := s.box.OpenAAD(o.ConnReqUserEnc, []byte("cr.user"))
	p, err2 := s.box.OpenAAD(o.ConnReqPassEnc, []byte("cr.pass"))
	return u, p, err1 == nil && err2 == nil
}

func (s *Service) sealDev(p string) (string, error) { return s.box.SealAAD(p, []byte("acs.dev")) }

func (s *Service) sealAdmin(p string) (string, error) { return s.box.SealAAD(p, []byte("onu.admin")) }

func (s *Service) openAdmin(enc string) (string, error) {
	return s.box.OpenAAD(enc, []byte("onu.admin"))
}

func (s *Service) sealPass(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	return s.box.SealAAD(p, []byte("wifi.pass"))
}

func (s *Service) openPass(enc string) (string, error) {
	if enc == "" {
		return "", errors.New("no password is saved")
	}
	return s.box.OpenAAD(enc, []byte("wifi.pass"))
}

// ---- views ----

// WiFiView is one Wi-Fi instance as the device reported it.
type WiFiView struct {
	Band    string `json:"band"`
	SSID    string `json:"ssid"`
	Enabled bool   `json:"enabled"`
}

// DesiredView is the saved configuration without any password.
type DesiredView struct {
	Set       bool      `json:"set"`
	Same      bool      `json:"same"`
	SSID      string    `json:"ssid,omitempty"`
	SSID5     string    `json:"ssid5,omitempty"`
	HasPass   bool      `json:"has_password"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	Changed   bool      `json:"changed_on_device"`
}

// TaskView is a task in words for the screens.
type TaskView struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"` // queued | sending | accepted | confirmed | failed | expired | done
	// StatusText: "Queued", "Sent to the ONU", "Accepted by the ONU", "Accepted: applies after the ONU restarts",
	// "Confirmed working", "Failed: <fault>", "Expired: ...", "Done".
	StatusText  string    `json:"status_text"`
	Fault       string    `json:"fault,omitempty"` // the failure, or the caution of an accepted change
	AfterReboot bool      `json:"after_reboot,omitempty"`
	CanConfirm  bool      `json:"can_confirm,omitempty"` // an accepted Wi-Fi change the owner can confirm ("It works")
	CanCancel   bool      `json:"can_cancel,omitempty"`  // queued: not sent yet
	CreatedAt   time.Time `json:"created_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	By          string    `json:"by,omitempty"`
}

// QuarantineView says why an ONU's check-in was held back.
type QuarantineView struct {
	Reason   string    `json:"reason"`
	Since    time.Time `json:"since"`
	LastAt   time.Time `json:"last_at"`
	Count    int       `json:"count"`
	IP       string    `json:"ip,omitempty"`
	MAC      string    `json:"mac,omitempty"`
	SiteID   string    `json:"site_id,omitempty"`
	SiteName string    `json:"site_name,omitempty"`
	Places   int       `json:"places,omitempty"` // more than 1: two devices claim the serial; Approve is refused
}

// ONUView is a device for the lists and the account card.
type ONUView struct {
	ID           string      `json:"id"`
	Serial       string      `json:"serial"`
	Manufacturer string      `json:"manufacturer"`
	Model        string      `json:"model"`
	SoftwareVer  string      `json:"software_version,omitempty"`
	DataModel    string      `json:"data_model"` // "TR-098" | "TR-181"
	SiteID       string      `json:"site_id"`
	SiteName     string      `json:"site_name,omitempty"`
	MgmtIP       string      `json:"mgmt_ip,omitempty"`
	MACs         []string    `json:"macs"`
	LastInform   time.Time   `json:"last_inform,omitempty"`
	Online       bool        `json:"online"` // informed within three intervals
	LastEvents   string      `json:"last_events,omitempty"`
	AccountID    string      `json:"account_id,omitempty"`
	AccountCode  string      `json:"account_code,omitempty"`
	Unit         string      `json:"unit,omitempty"`
	Link         string      `json:"link,omitempty"`
	WiFi         []WiFiView  `json:"wifi"`
	Bands        []string    `json:"bands"`    // the bands of the radios the ONU reported: "2.4", "5"
	Has5GHz      bool        `json:"has_5ghz"` // false for a 2.4 GHz-only box: one Wi-Fi name and password
	Desired      DesiredView `json:"desired"`
	LastTask     *TaskView   `json:"last_task,omitempty"`
	CRApplied    bool        `json:"cr_applied"`
	Kick         string      `json:"kick,omitempty"` // what the last "contact the ONU now" did
	// Security. Quarantine is set while a check-in from an unexpected place waits for the owner (Approve / Block).
	// Login says how the ONU logs in: "own" (its own login), "shared" (it refused one: weaker), "pending" or "".
	Quarantine *QuarantineView `json:"quarantine,omitempty"`
	Login      string          `json:"login,omitempty"`
	SharedOnly bool            `json:"shared_login_only,omitempty"` // flag for the list: weaker identity
	AdminLock  string          `json:"admin_lock,omitempty"`        // "unverified" | "unsupported" | ""
	HasAdminPW bool            `json:"has_admin_password,omitempty"`
	Returned   bool            `json:"returned,omitempty"` // left a house: reset queued, never gets a house's Wi-Fi
	Blocked    bool            `json:"blocked,omitempty"`  // places are blocked for this serial
	ResetTask  string          `json:"reset_task,omitempty"`
	// The equipment register: InInventory says the serial is registered; InventoryID/Status/MAC are its pair.
	InInventory bool     `json:"in_inventory"`
	InventoryID string   `json:"inventory_id,omitempty"`
	InvStatus   string   `json:"inventory_status,omitempty"`
	InvMAC      string   `json:"inventory_mac,omitempty"`
	Warnings    []string `json:"warnings"`
	// MACWait is set while the ONU waits for its management MAC to match the registered ONU (nothing is sent to it).
	MACWait *MACWaitView `json:"mac_wait,omitempty"`
	UseMACs []string     `json:"use_macs,omitempty"` // reported MACs the house can be switched to in one click
}

func taskView(t store.ONUTask) TaskView {
	v := TaskView{ID: t.ID, Kind: t.Kind, Fault: t.Fault, CreatedAt: t.CreatedAt, FinishedAt: t.FinishedAt, By: t.By, AfterReboot: t.AfterReboot}
	switch t.Status {
	case store.TaskDone:
		v.Status, v.StatusText = "done", "Done"
	case store.TaskAccepted:
		v.Status, v.StatusText = "accepted", "Accepted by the ONU"
		if t.AfterReboot {
			v.StatusText = "Accepted: applies after the ONU restarts"
		}
		v.CanConfirm = t.Kind == store.TaskWiFi
	case store.TaskConfirmed:
		v.Status, v.StatusText = "confirmed", "Confirmed working"
	case store.TaskFailed:
		v.Status, v.StatusText = "failed", "Failed"
		if t.Fault != "" {
			v.StatusText = "Failed: " + t.Fault
		}
	case store.TaskExpired:
		v.Status, v.StatusText = "expired", "Expired: the ONU did not check in in time"
	case store.TaskRunning:
		v.Status, v.StatusText = "sending", "Sent to the ONU"
	default:
		v.Status, v.StatusText, v.CanCancel = "queued", "Queued", true
	}
	return v
}

func (s *Service) view(d *store.Data, o store.ONU) ONUView {
	v := ONUView{ID: o.ID, Serial: o.Serial, Manufacturer: o.Manufacturer, Model: o.ProductClass, SoftwareVer: o.SWVersion, SiteID: o.SiteID,
		SiteName: d.Sites[o.SiteID].Name, MgmtIP: o.MgmtIP, MACs: append([]string{}, o.MACs...), LastInform: o.LastInform, LastEvents: o.LastEvents,
		AccountID: o.AccountID, Link: o.Link, CRApplied: o.CRApplied, WiFi: []WiFiView{}, Bands: []string{}}
	v.Bands, v.Has5GHz = append(v.Bands, bandsOf(o)...), Has5GHz(o)
	switch o.Root {
	case store.RootTR181:
		v.DataModel = "TR-181"
	case store.RootTR098:
		v.DataModel = "TR-098"
	}
	if iv := s.informEvery(); !o.LastInform.IsZero() && s.now().Sub(o.LastInform) < 3*iv {
		v.Online = true
	}
	if a, ok := d.Accounts[o.AccountID]; ok {
		v.AccountCode, v.Unit = a.Code, a.Unit
	} else if o.AccountID != "" {
		v.AccountID, v.Link = "", "" // the house was deleted
	}
	for _, w := range o.WiFi {
		v.WiFi = append(v.WiFi, WiFiView{Band: w.Band, SSID: w.SSID, Enabled: w.Enabled})
	}
	if q := o.Sec.Quarantine; q.Active {
		v.Quarantine = &QuarantineView{Reason: q.Reason, Since: q.Since, LastAt: q.LastAt, Count: q.Count, IP: q.IP, MAC: q.MAC, SiteID: q.SiteID, SiteName: d.Sites[q.SiteID].Name, Places: q.Places}
	}
	switch o.Sec.ACSCred {
	case store.CredDevice:
		v.Login = "own"
	case store.CredShared:
		v.Login, v.SharedOnly = "shared", true
	case store.CredPending:
		v.Login = "pending"
	}
	v.AdminLock, v.HasAdminPW = o.Sec.AdminPass, o.Sec.AdminPassEnc != ""
	v.Returned, v.Blocked = o.State.Returned && o.AccountID == "", len(o.Sec.BlockedFrom) > 0
	for _, t := range d.ONUTasksOf(o.ID) {
		if t.Kind == store.TaskFactoryReset && t.Status == store.TaskPending {
			v.ResetTask = t.ID
		}
	}
	ds := homeWiFi(d, o)
	v.Desired = DesiredView{Set: ds.Set, Same: ds.Same, SSID: ds.SSID, SSID5: ds.SSID5, HasPass: ds.PassEnc != "", UpdatedAt: ds.UpdatedAt, UpdatedBy: ds.UpdatedBy, Changed: o.State.WiFiChanged && ds.Set}
	if ts := d.ONUTasksOf(o.ID); len(ts) > 0 {
		tv := taskView(ts[0])
		v.LastTask = &tv
	}
	if k, ok := s.kickOf(o.ID); ok {
		v.Kick = k.Text()
	}
	s.inventoryView(d, o, &v)
	v.MACWait = s.macWaitView(d, o)
	if o.Sec.RestoreOwed && o.AccountID != "" {
		v.Warnings = append(v.Warnings, "This ONU came back on the shared login (a factory reset?). The house's Wi-Fi is restored at its first check-in after "+o.Sec.HoldUntil.Local().Format("15:04")+", once no other device has claimed to be it.")
	}
	return v
}

func (s *Service) informEvery() time.Duration {
	n := s.cfg.ACSInformInterval
	if n <= 0 {
		n = 300
	}
	return time.Duration(n) * time.Second
}

// ONUs lists the devices of a router (site "" = all) for the lists.
func (s *Service) ONUs(siteID string) []ONUView {
	var out []ONUView
	s.st.View(func(d *store.Data) {
		for _, o := range d.ONUList(siteID) {
			out = append(out, s.view(d, o))
		}
	})
	return out
}

// OfAccount is the ONU linked to a house.
func (s *Service) OfAccount(accountID string) (ONUView, bool) {
	var v ONUView
	var ok bool
	s.st.View(func(d *store.Data) {
		var o store.ONU
		if o, ok = d.ONUOfAccount(accountID); ok {
			v = s.view(d, o)
		}
	})
	return v, ok
}

// ONU is one device by id.
func (s *Service) ONU(id string) (ONUView, bool) {
	var v ONUView
	var ok bool
	s.st.View(func(d *store.Data) {
		var o store.ONU
		if o, ok = d.ONUs[id]; ok {
			v = s.view(d, o)
		}
	})
	return v, ok
}

// CountsBySite is how many ONUs each router has, and how many are online.
func (s *Service) CountsBySite() (total, online map[string]int) {
	total, online = map[string]int{}, map[string]int{}
	s.st.View(func(d *store.Data) {
		for _, o := range d.ONUs {
			total[o.SiteID]++
			if !o.LastInform.IsZero() && s.now().Sub(o.LastInform) < 3*s.informEvery() {
				online[o.SiteID]++
			}
		}
	})
	return
}

func trimMAC(m string) string { return strings.ToUpper(strings.TrimSpace(m)) }

// inventoryView fills the register fields of an ONU and the warnings that come from comparing the register with
// what the ONU reports. Nothing is changed automatically.
func (s *Service) inventoryView(d *store.Data, o store.ONU, v *ONUView) {
	v.Warnings = []string{}
	inv, ok := d.ONTBySerial(o.Serial)
	if !ok {
		return
	}
	v.InInventory, v.InventoryID, v.InvStatus, v.InvMAC = true, inv.ID, inv.Status, inv.MAC
	if m, ok := store.CanonMAC(inv.MAC); ok && len(o.MACs) > 0 && !containsMAC(o.MACs, m) {
		v.Warnings = append(v.Warnings, "The WAN MAC this ONU reports ("+strings.Join(o.MACs, ", ")+") is not the one in the register ("+m+"). The label's MAC is often the LAN MAC: check which one the router sees.")
		if inv.AccountID != "" && verifiedReports(o, s.now()) {
			v.UseMACs = append(v.UseMACs, o.MACs...)
		}
	}
	if inv.Status == store.EqInstalled && inv.AccountID != "" {
		if a, ok := d.Accounts[inv.AccountID]; ok && a.SiteID != o.SiteID {
			v.Warnings = append(v.Warnings, "This ONU is registered to house "+a.Code+" on router "+d.Sites[a.SiteID].Name+" but it is online on router "+d.Sites[o.SiteID].Name+".")
		} else if ok && o.AccountID != "" && o.AccountID != inv.AccountID {
			v.Warnings = append(v.Warnings, "This ONU is registered to house "+a.Code+" but is linked to another house.")
		}
	}
	if inv.Status != store.EqInstalled && v.Online {
		v.Warnings = append(v.Warnings, "This ONU is online but the register says it is "+strings.ReplaceAll(inv.Status, "_", " ")+".")
	}
}

// verifiedReports: what the ONU reports (its WAN MACs) comes from a box that is not quarantined, is bound to a management
// MAC, and has been seen logging in with its own ACS login (or, when it refuses one, from the place the router leased to
// its MAC). Only then may the owner switch a house's MAC to a reported one in one click.
func verifiedReports(o store.ONU, now time.Time) bool {
	if o.Sec.Quarantine.Active || o.Sec.MgmtMAC == "" {
		return false
	}
	if o.Sec.RestoreOwed || (!o.Sec.CloneAt.IsZero() && now.Sub(o.Sec.CloneAt) < cloneMemory) {
		return false // back on the shared login and still held, or another device claimed it lately: what it reports may be a copy's
	}
	return o.Sec.ACSCred == store.CredDevice || o.Sec.ACSCred == store.CredShared
}

// CanUseMAC says whether the owner may switch the house of an ONU to the MAC it reported (checked again by the screens
// and the API at the moment of the click).
func (s *Service) CanUseMAC(accountID, mac string) bool {
	ok := false
	s.st.View(func(d *store.Data) {
		o, found := d.ONUOfAccount(accountID)
		if !found || !verifiedReports(o, s.now()) {
			return
		}
		m, good := store.CanonMAC(mac)
		ok = good && containsMAC(o.MACs, m)
	})
	return ok
}
