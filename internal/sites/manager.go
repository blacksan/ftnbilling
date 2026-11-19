package sites

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
	"ispbill/internal/metrics"
	"ispbill/internal/routeros"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
	"ispbill/internal/wg"
)

// Options configures a Manager.
type Options struct {
	Cfg   config.Config
	Store *store.Store
	// Dir gives a building's metrics collector its flats (billing.Service.Directory).
	Dir func(siteID string) metrics.Directory
	Log *slog.Logger
	// NewClient overrides how a router client is built (tests).
	NewClient func(store.Site) routeros.Client
	Now       func() time.Time
	// DataKey seals the PPPoE password at rest (DATA_KEY, 32 bytes). Without it a
	// PPPoE router cannot be saved.
	DataKey []byte
}

// Manager owns the per-building runtimes.
type Manager struct {
	cfg  config.Config
	st   *store.Store
	dir  func(string) metrics.Directory
	log  *slog.Logger
	opt  Options
	now  func() time.Time
	pfx  netip.Prefix
	mgmt netip.Prefix   // remote-WinBox subnet (MGMT_SUBNET)
	mock *routeros.Mock // shared by every building when ROUTER_MOCK=true

	mu       sync.RWMutex
	rts      map[string]*Runtime
	ctx      context.Context
	wg       sync.WaitGroup
	reloadMu sync.Mutex

	stray atomic.Uint64 // NetFlow datagrams from unknown sources

	refused refusals // join fetches turned away for their User-Agent (joinreport.go)

	foreignIPs atomic.Pointer[func(string) map[string]bool] // set by billing.NewService

	chk          checksState   // cache of the setup checks (checks.go)
	autoFixDelay time.Duration // wait after the first answer before the post-join fixes (runtime.go)

	cport cpState   // customer-port and automatic-fix state per building (custport.go, autofix.go)
	prov  provMgr   // billing hooks and the reboot slot (provision.go)
	live  liveState // live router views (live.go)
	box   *secretbox.Box
}

// SetForeignIPs lets the billing service tell the sites package which addresses
// the router's own static leases hold, so no flat is given one of them.
func (m *Manager) SetForeignIPs(f func(siteID string) map[string]bool) { m.foreignIPs.Store(&f) }

func (m *Manager) foreignIPsOf(siteID string) map[string]bool {
	if f := m.foreignIPs.Load(); f != nil {
		return (*f)(siteID)
	}
	return nil
}

// New builds a Manager. Nothing runs until Start.
func New(o Options) *Manager {
	pfx, err := netip.ParsePrefix(o.Cfg.WGSubnet)
	if err != nil {
		pfx = netip.MustParsePrefix("10.99.0.0/22")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	mg, err := netip.ParsePrefix(o.Cfg.MgmtSubnet)
	if err != nil || ValidMgmtSubnet(mg) != nil {
		mg = DefaultMgmtRange
	}
	m := &Manager{cfg: o.Cfg, st: o.Store, dir: o.Dir, log: o.Log, opt: o, now: now, pfx: pfx.Masked(), mgmt: mg.Masked(), rts: map[string]*Runtime{},
		autoFixDelay: 20 * time.Second}
	if o.Cfg.RouterMock {
		m.mock = routeros.NewMock()
	}
	if len(o.DataKey) == 32 {
		if b, err := secretbox.New(o.DataKey); err == nil {
			m.box = b
		}
	}
	return m
}

// MockRouter returns the shared fake router (ROUTER_MOCK=true), else nil.
func (m *Manager) MockRouter() *routeros.Mock { return m.mock }

// Prefix is the WireGuard tunnel subnet.
func (m *Manager) Prefix() netip.Prefix { return m.pfx }

// MgmtPrefix is the remote-WinBox (wg-mgmt) subnet.
func (m *Manager) MgmtPrefix() netip.Prefix { return m.mgmt }

// MetricsPath is the file a building's collector saves to.
func MetricsPath(dataDir, siteID string) string {
	return filepath.Join(dataDir, "metrics", siteID+".json")
}

// Start launches the runtime of every runnable building and the NetFlow
// socket. It returns at once; Wait blocks until ctx is cancelled and
// everything has saved and stopped.
func (m *Manager) Start(ctx context.Context) {
	m.reloadMu.Lock()
	m.ctx = ctx
	var all []store.Site
	m.st.View(func(d *store.Data) {
		for _, s := range d.Sites {
			if s.Runnable() {
				all = append(all, s)
			}
		}
	})
	for _, s := range all {
		m.startLocked(s)
	}
	m.reloadMu.Unlock()
	if addr := m.cfg.NetflowAddr; addr != "" && !strings.EqualFold(addr, "off") && !m.cfg.RouterMock {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if err := m.ListenNetflow(ctx, addr, nil); err != nil {
				m.log.Warn("netflow listener stopped", "addr", addr, "err", err)
			}
		}()
	}
}

// Wait blocks until every runtime and the NetFlow socket have stopped.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) newClient(s store.Site) routeros.Client {
	if m.opt.NewClient != nil {
		return m.opt.NewClient(s)
	}
	if m.cfg.RouterMock {
		return m.mock
	}
	return routeros.NewREST(fmt.Sprintf("http://%s:%d", s.TunnelIP, s.RESTPort), s.RESTUser, s.RESTPass).WithTimeout(m.cfg.RouterReadTimeout)
}

// Effective returns the site as the runtime sees it: defaults filled in.
func (m *Manager) Effective(s store.Site) store.Site { return ApplyDefaults(m.cfg, s) }

func (m *Manager) collectorConfig(s store.Site) metrics.Config {
	subnet, _ := netip.ParsePrefix(s.CustomerSubnet)
	c := metrics.Config{
		WAN: s.WANInterface, OLT: s.OLTInterface, WANCapMbps: s.WANCapMbps, OLTCapMbps: s.OLTCapMbps, FUPTB: s.FUPTB,
		PingTargets: m.cfg.PingTargets, Subnet: subnet.Masked(),
		FlowEvery: m.cfg.FlowEvery, PortEvery: m.cfg.PortEvery, PingEvery: m.cfg.PingEvery, DNSEvery: m.cfg.DNSEvery, SaveEvery: m.cfg.SaveEvery,
		NetflowAddr: m.cfg.NetflowAddr, SiteID: s.ID,
	}
	if m.cfg.RouterMock {
		c.NetflowAddr = "off" // the fake router sends no traffic-flow
		c.Synthetic = true
	}
	if s.Mode == store.SiteModeWG {
		c.PortAllow = []string{s.WANInterface, s.OLTInterface, CustomerBridge}
		if s.VLANID > 0 {
			c.PortAllow = append(c.PortAllow, fmt.Sprintf("vlan%d-customers", s.VLANID))
		}
	}
	return c
}

// startLocked builds and starts a runtime (reloadMu held, m.ctx set).
func (m *Manager) startLocked(s store.Site) {
	if m.ctx == nil || !s.Runnable() {
		return
	}
	s = ApplyDefaults(m.cfg, s)
	id := s.ID
	client := m.newClient(s)
	var dir metrics.Directory
	if m.dir != nil {
		dir = m.dir(id)
	}
	col := metrics.New(MetricsPath(m.cfg.DataDir, id), m.collectorConfig(s), client, dir, m.log.With("site", id))
	rt := &Runtime{Site: s, Client: client, Col: col, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(m.ctx)
	rt.cancel = cancel
	m.mu.Lock()
	m.rts[id] = rt
	m.mu.Unlock()
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.run(ctx, rt) }()
	m.log.Info("router started", "site", id, "name", s.Name, "mode", s.Mode)
}

// stopLocked cancels a runtime and waits until it has saved its metrics.
func (m *Manager) stopLocked(siteID string) {
	m.mu.Lock()
	rt := m.rts[siteID]
	delete(m.rts, siteID)
	m.mu.Unlock()
	if rt != nil {
		rt.cancel()
		<-rt.done
	}
}

// Reload stops the building's runtime (waiting for its final metrics save) and
// rebuilds it from the store. A removed, paused or unjoined building is left stopped.
func (m *Manager) Reload(siteID string) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.stopLocked(siteID)
	var s store.Site
	var ok bool
	m.st.View(func(d *store.Data) { s, ok = d.Sites[siteID] })
	if ok {
		m.startLocked(s)
	}
}

func (m *Manager) runtime(siteID string) *Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rts[siteID]
}

// Router implements billing.RouterSource. An unknown, unjoined or paused
// building gets a client whose every call fails with routeros.ErrNoRouter.
func (m *Manager) Router(siteID string) routeros.Client {
	if rt := m.runtime(siteID); rt != nil {
		return rt.Client
	}
	return routeros.NoRouter()
}

// Collector returns the building's metrics collector, or nil if it is not running.
func (m *Manager) Collector(siteID string) *metrics.Collector {
	if rt := m.runtime(siteID); rt != nil {
		return rt.Col
	}
	return nil
}

// Stray is how many NetFlow datagrams came from a source that belongs to no building.
func (m *Manager) Stray() uint64 { return m.stray.Load() }

// ---- WireGuard peers ------------------------------------------------------

// Peers lists the routers that may connect: every non-removed WireGuard
// building with a key, paused ones included (a pause keeps the peer).
func (m *Manager) Peers() []wg.Peer {
	var out []wg.Peer
	m.st.View(func(d *store.Data) {
		for _, s := range d.Sites {
			if !s.Removed && s.Mode == store.SiteModeWG && s.WGPublicKey != "" && s.TunnelIP != "" {
				out = append(out, wg.Peer{PublicKey: s.WGPublicKey, TunnelIP: s.TunnelIP})
			}
		}
	})
	return out
}

// ApplyPeers writes DATA/wg/peers.conf and touches DATA/wg/apply; a root
// systemd path unit then validates the file and runs `wg syncconf`.
func (m *Manager) ApplyPeers() error {
	return wg.WritePeers(filepath.Join(m.cfg.DataDir, "wg"), m.Peers())
}

// PeerStatus returns the last WireGuard status of a building's peer. known is
// false when the status job has not written its dump file.
func (m *Manager) PeerStatus(siteID string) (ps wg.PeerStatus, found, known bool) {
	var key string
	m.st.View(func(d *store.Data) { key = d.Sites[siteID].WGPublicKey })
	dump, _, err := wg.ReadDump(m.cfg.WGDumpPath)
	if err != nil {
		return wg.PeerStatus{}, false, false
	}
	ps, found = dump[key]
	return ps, found && key != "", true
}

// ---- site operations ------------------------------------------------------

// SiteInput is the editable part of a router.
type SiteInput struct {
	Name, Notes, SupportPhone     string
	WANInterface, OLTInterface    string
	CustomerNet                   string
	VLANID                        int
	WANCapMbps, OLTCapMbps, FUPTB float64
	CustomerPorts                 string // comma list of extra customer ports
	// WAN, admin LAN. PPPoEPass is the clear text to seal; "" keeps the stored
	// password unless PPPoEPassClear is set.
	WANMode, WANAddress, WANGateway, WANDNS string
	PPPoEUser, PPPoEPass                    string
	PPPoEPassClear                          bool
	LANPorts, LANSubnet                     string
}

// SiteInputOf is the input that leaves a router as it is (a PATCH starts from it).
func SiteInputOf(st store.Site) SiteInput {
	return SiteInput{Name: st.Name, Notes: st.Notes, SupportPhone: st.SupportPhone, WANInterface: st.WANInterface, OLTInterface: st.OLTInterface,
		CustomerNet: st.CustomerSubnet, VLANID: st.VLANID, WANCapMbps: st.WANCapMbps, OLTCapMbps: st.OLTCapMbps, FUPTB: st.FUPTB, CustomerPorts: st.CustomerPorts,
		WANMode: st.WANMode, WANAddress: st.WANAddress, WANGateway: st.WANGateway, WANDNS: st.WANDNS, PPPoEUser: st.PPPoEUser,
		LANPorts: st.LANPorts, LANSubnet: st.LANSubnet}
}

// ErrNoDataKey: a PPPoE password cannot be stored without DATA_KEY.
var ErrNoDataKey = errors.New("the PPPoE password cannot be saved: DATA_KEY is not set on the server")

func clean(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func pppoeAAD(siteID string) []byte { return []byte("pppoe:" + siteID) }

// OpenPPPoEPass returns the router's PPPoE password in clear.
func (m *Manager) OpenPPPoEPass(s store.Site) (string, error) {
	if s.PPPoEPassEnc == "" {
		return "", nil
	}
	if m.box == nil {
		return "", ErrNoDataKey
	}
	return m.box.OpenAAD(s.PPPoEPassEnc, pppoeAAD(s.ID))
}

// applyInput copies the input over the site. id is the site's ID (the sealed
// password is bound to it).
func (m *Manager) applyInput(s store.Site, in SiteInput) (store.Site, error) {
	s.Name, s.Notes, s.SupportPhone = clean(in.Name, 60), clean(in.Notes, 300), clean(in.SupportPhone, 30)
	s.WANInterface, s.OLTInterface = strings.TrimSpace(in.WANInterface), strings.TrimSpace(in.OLTInterface)
	s.CustomerSubnet, s.VLANID = strings.TrimSpace(in.CustomerNet), in.VLANID
	s.WANCapMbps, s.OLTCapMbps, s.FUPTB = in.WANCapMbps, in.OLTCapMbps, in.FUPTB
	s.CustomerPorts, s.BenchPort = strings.Join(SplitPorts(in.CustomerPorts), ","), ""
	s.WANMode = strings.ToLower(strings.TrimSpace(in.WANMode))
	s.LANPorts, s.LANSubnet = strings.Join(SplitPorts(in.LANPorts), ","), strings.TrimSpace(in.LANSubnet)
	if s.WANMode == WANStatic {
		s.WANAddress, s.WANGateway, s.WANDNS = strings.TrimSpace(in.WANAddress), strings.TrimSpace(in.WANGateway), strings.Join(SplitPorts(in.WANDNS), ",")
	} else {
		s.WANAddress, s.WANGateway, s.WANDNS = "", "", ""
	}
	if s.WANMode == WANPPPoE {
		s.PPPoEUser = strings.TrimSpace(in.PPPoEUser)
		switch {
		case in.PPPoEPassClear:
			s.PPPoEPassEnc = ""
		case in.PPPoEPass != "":
			if HasControl(in.PPPoEPass) {
				return s, errors.New("the PPPoE password has a control character (a line break or tab) that cannot be used")
			}
			if m.box == nil {
				return s, ErrNoDataKey
			}
			enc, err := m.box.SealAAD(in.PPPoEPass, pppoeAAD(s.ID))
			if err != nil {
				return s, err
			}
			s.PPPoEPassEnc = enc
		}
	} else {
		s.PPPoEUser, s.PPPoEPassEnc = "", ""
	}
	return s, nil
}

func nameTaken(d *store.Data, name, exceptID string) bool {
	for _, o := range d.Sites {
		if !o.Removed && o.ID != exceptID && strings.EqualFold(o.Name, name) {
			return true
		}
	}
	return false
}

// CreateSite adds a building (mode wg, not joined yet).
func (m *Manager) CreateSite(in SiteInput) (store.Site, error) {
	var out store.Site
	err := m.st.Update(func(d *store.Data) error {
		id := store.NewSiteID()
		for {
			if _, clash := d.Sites[id]; !clash {
				break
			}
			id = store.NewSiteID()
		}
		s, err := m.applyInput(store.Site{ID: id, Mode: store.SiteModeWG, AccessMode: store.AccessIPoE}, in)
		if err != nil {
			return err
		}
		raw := s.OLTCapMbps
		s = ApplyDefaults(m.cfg, s)
		if err := ValidateNetMgmt(s, m.pfx, m.mgmt); err != nil {
			return err
		}
		s.OLTCapMbps = raw // not set stays not set: the OLT port's type decides
		if nameTaken(d, s.Name, "") {
			return fmt.Errorf("there is already a router called %q", s.Name)
		}
		s.CreatedAt = m.now()
		d.Sites[s.ID] = s
		out = s
		return nil
	})
	if err != nil {
		return out, err
	}
	if err := m.ApplyPeers(); err != nil {
		m.log.Warn("could not write the WireGuard peers file", "err", err)
	}
	return out, nil
}

// UpdateSite saves the editable fields and restarts the building's runtime.
func (m *Manager) UpdateSite(id string, in SiteInput) error {
	var portsChanged bool
	err := m.st.Update(func(d *store.Data) error {
		portsChanged = false
		cur, ok := d.Sites[id]
		if !ok || cur.Removed {
			return billing.ErrNoSite
		}
		s, err := m.applyInput(cur, in)
		if err != nil {
			return err
		}
		// A form that was filled from the effective values sends the default
		// back: it stays "not set" so the port type keeps deciding.
		if cur.OLTCapMbps == 0 && in.OLTCapMbps == m.Effective(cur).OLTCapMbps {
			s.OLTCapMbps = 0
		}

		raw := s.OLTCapMbps
		if s.Mode == store.SiteModeWG {
			s = ApplyDefaults(m.cfg, s)
			s.OLTCapMbps = raw // not set stays not set: the OLT port's type decides
		}
		if err := ValidateNetMgmt(ApplyDefaults(m.cfg, s), m.pfx, m.mgmt); err != nil {
			return err
		}
		if nameTaken(d, s.Name, id) {
			return fmt.Errorf("there is already a router called %q", s.Name)
		}
		was, now := m.Effective(cur), m.Effective(s)
		portsChanged = now.CustomerPorts != was.CustomerPorts || now.LANPorts != was.LANPorts || now.OLTInterface != was.OLTInterface || now.VLANID != was.VLANID
		d.Sites[id] = s
		return nil
	})
	if err == nil {
		m.Reload(id)
		if portsChanged {
			m.QueueCustomerPort(id)
		}
	}
	return err
}

// SetDisabled pauses or resumes a building. The WireGuard peer is kept.
func (m *Manager) SetDisabled(id string, disabled bool) error {
	err := m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[id]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		if s.Disabled == disabled {
			return store.ErrNoChange
		}
		s.Disabled = disabled
		d.Sites[id] = s
		return nil
	})
	if err == nil {
		m.Reload(id)
	}
	return err
}

// ErrSiteHasAccounts is returned when removing a building that still has flats.
var ErrSiteHasAccounts = errors.New("this router still has accounts. Move or delete them first")

// RemoveSite removes a building. confirm must equal its name, and it is
// refused while accounts exist. The peer is dropped, the runtime stopped, the
// metrics file deleted, the tunnel address freed and the secrets wiped; a
// tombstone stays so past payments keep the building's name.
func (m *Manager) RemoveSite(id, confirm string) error {
	err := m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[id]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		if strings.TrimSpace(confirm) != s.Name {
			return fmt.Errorf("type the router name %q to confirm", s.Name)
		}
		for _, a := range d.Accounts {
			if a.SiteID == id {
				return ErrSiteHasAccounts
			}
		}
		s.Removed, s.Disabled = true, false
		s.TunnelIP, s.WGPublicKey, s.RESTPass, s.Join = "", "", "", store.JoinToken{}
		s.PPPoEPassEnc = ""
		d.Sites[id] = s
		for k, t := range d.Tasks {
			if t.SiteID == id {
				delete(d.Tasks, k)
			}
		}
		for k, dev := range d.Devices {
			if dev.SiteID == id {
				delete(d.Devices, k)
			}
		}
		d.AddEvent(store.Event{Time: m.now(), SiteID: id, Kind: "info", Msg: "router removed: " + s.Name})
		return nil
	})
	if err != nil {
		return err
	}
	m.Reload(id) // stops it (the site is removed now)
	_ = os.Remove(MetricsPath(m.cfg.DataDir, id))
	if err := m.ApplyPeers(); err != nil {
		m.log.Warn("could not write the WireGuard peers file", "err", err)
	}
	return nil
}

// Status reports the join token, the WireGuard handshake and whether the REST
// API answers, for the building page.
func (m *Manager) Status(ctx context.Context, siteID string) (SiteStatus, bool) {
	var s store.Site
	var ok bool
	m.st.View(func(d *store.Data) { s, ok = d.Sites[siteID] })
	if !ok {
		return SiteStatus{}, false
	}
	now := m.now()
	st := SiteStatus{Token: TokenStateOf(s.Join, now), TokenExpires: s.Join.ExpiresAt, UsedFrom: s.Join.UsedFrom, Joined: !s.JoinedAt.IsZero()}
	if rf, ok := m.refused.get(siteID); ok && st.Token == TokenPending {
		st.RefusedUA, st.RefusedAt = rf.UA, rf.At
	}
	if s.Mode == store.SiteModeWG && s.WGPublicKey != "" {
		if ps, found, known := m.PeerStatus(siteID); known {
			st.WGKnown = true
			if age, has := ps.HandshakeAge(now); found && has {
				st.Handshake, st.HandshakeAge = true, int64(age.Seconds())
			}
		}
	}
	pc, cancel := context.WithTimeout(ctx, routeros.DefaultTimeout)
	defer cancel()
	if err := m.Router(siteID).Ping(pc); err != nil {
		st.RESTErr = err.Error()
	} else {
		st.RESTOK = true
	}
	return st, true
}

func nowFn() time.Time { return time.Now() }
