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

	mu       sync.Mutex
	sessions map[string]*session
	ips      map[netip.Addr]*ipState
	kicks    map[string]Kick
	capLog   map[string]time.Time
	wg       sync.WaitGroup
}

// New makes the service. DATA_KEY is required (passphrases and logins are sealed with it).
func New(o Options) (*Service, error) {
	box, err := secretbox.NewLabelled(o.DataKey, "ispbill/acs", "ispbill/acs-idx")
	if err != nil {
		return nil, fmt.Errorf("ONU management needs DATA_KEY: %w", err)
	}
	s := &Service{st: o.Store, cfg: o.Cfg, log: o.Log, box: box, now: o.Now, client: o.HTTPClient, ipOf: o.ClientIP,
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
		s.client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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

// EnsureCredentials makes the global ACS user and password on first run and keeps them (sealed) in settings.
func (s *Service) EnsureCredentials() error {
	return s.st.Update(func(d *store.Data) error {
		if d.Settings[KeyUser].Value != "" && d.Settings[KeyPass].Value != "" {
			if _, err := s.box.OpenAAD(d.Settings[KeyPass].Value, []byte(KeyPass)); err == nil {
				return store.ErrNoChange
			}
		}
		return s.writeCreds(d, "ftn-"+randHex(3), randAlnum(20))
	})
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
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`      // queued | applied | failed
	StatusText string    `json:"status_text"` // "Queued", "Applied", "Failed: <fault>"
	Fault      string    `json:"fault,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	By         string    `json:"by,omitempty"`
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
	Desired      DesiredView `json:"desired"`
	LastTask     *TaskView   `json:"last_task,omitempty"`
	CRApplied    bool        `json:"cr_applied"`
	Kick         string      `json:"kick,omitempty"` // what the last "contact the ONU now" did
}

// StatusOf is the words of a task's status.
func taskView(t store.ONUTask) TaskView {
	v := TaskView{ID: t.ID, Kind: t.Kind, Fault: t.Fault, CreatedAt: t.CreatedAt, FinishedAt: t.FinishedAt, By: t.By}
	switch t.Status {
	case store.TaskDone:
		v.Status, v.StatusText = "applied", "Applied"
	case store.TaskFailed:
		v.Status, v.StatusText = "failed", "Failed"
		if t.Fault != "" {
			v.StatusText = "Failed: " + t.Fault
		}
	default:
		v.Status, v.StatusText = "queued", "Queued"
	}
	return v
}

func (s *Service) view(d *store.Data, o store.ONU) ONUView {
	v := ONUView{ID: o.ID, Serial: o.Serial, Manufacturer: o.Manufacturer, Model: o.ProductClass, SoftwareVer: o.SWVersion, SiteID: o.SiteID,
		SiteName: d.Sites[o.SiteID].Name, MgmtIP: o.MgmtIP, MACs: append([]string{}, o.MACs...), LastInform: o.LastInform, LastEvents: o.LastEvents,
		AccountID: o.AccountID, Link: o.Link, CRApplied: o.CRApplied, WiFi: []WiFiView{}}
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
	ds := o.Desired
	v.Desired = DesiredView{Set: ds.Set, Same: ds.Same, SSID: ds.SSID, SSID5: ds.SSID5, HasPass: ds.PassEnc != "", UpdatedAt: ds.UpdatedAt, UpdatedBy: ds.UpdatedBy, Changed: ds.Changed}
	if ts := d.ONUTasksOf(o.ID); len(ts) > 0 {
		tv := taskView(ts[0])
		v.LastTask = &tv
	}
	if k, ok := s.kickOf(o.ID); ok {
		v.Kick = k.Text()
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
