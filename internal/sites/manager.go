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
	// OnJoined is called after a join script was handed out and the runtime
	// rebuilt: the router may be new, so its profiles must be pushed again.
	OnJoined func(siteID string)
	Now      func() time.Time
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
	mock *routeros.Mock // shared by env-mode buildings when ROUTER_MOCK=true

	mu       sync.RWMutex
	rts      map[string]*Runtime
	ctx      context.Context
	wg       sync.WaitGroup
	reloadMu sync.Mutex

	stray atomic.Uint64 // NetFlow datagrams from unknown sources
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
	m := &Manager{cfg: o.Cfg, st: o.Store, dir: o.Dir, log: o.Log, opt: o, now: now, pfx: pfx.Masked(), rts: map[string]*Runtime{}}
	if o.Cfg.RouterMock {
		m.mock = routeros.NewMock()
	}
	return m
}

// MockRouter returns the shared fake router (ROUTER_MOCK=true), else nil.
func (m *Manager) MockRouter() *routeros.Mock { return m.mock }

// Prefix is the WireGuard tunnel subnet.
func (m *Manager) Prefix() netip.Prefix { return m.pfx }

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
	if s.Mode == store.SiteModeWG {
		return routeros.NewREST(fmt.Sprintf("http://%s:%d", s.TunnelIP, s.RESTPort), s.RESTUser, s.RESTPass)
	}
	if m.cfg.RouterMock {
		return m.mock
	}
	return routeros.NewREST(m.cfg.RouterURL, m.cfg.RouterUser, m.cfg.RouterPass)
}

// Effective returns the site as the runtime sees it: defaults filled in, and
// for an env-mode building the layout read from the environment.
func (m *Manager) Effective(s store.Site) store.Site { return ApplyDefaults(m.cfg, s) }

// GuardRequired tells whether portal requests for this building must prove
// that the MAC is on its hotspot. Always for wg buildings; VERIFY_HOTSPOT_HOST
// only decides it for the env building.
func (m *Manager) GuardRequired(s store.Site) bool {
	return s.Mode == store.SiteModeWG || m.cfg.VerifyHotspotHost
}

func (m *Manager) collectorConfig(s store.Site) metrics.Config {
	subnet, _ := netip.ParsePrefix(s.CustomerSubnet)
	c := metrics.Config{
		WAN: s.WANInterface, OLT: s.OLTInterface, WANCapMbps: s.WANCapMbps, OLTCapMbps: s.OLTCapMbps, FUPTB: s.FUPTB,
		PingTargets: m.cfg.PingTargets, Subnet: subnet.Masked(),
		FlowEvery: m.cfg.FlowEvery, PortEvery: m.cfg.PortEvery, PingEvery: m.cfg.PingEvery, DNSEvery: m.cfg.DNSEvery, SaveEvery: m.cfg.SaveEvery,
		NetflowAddr: m.cfg.NetflowAddr, SiteID: s.ID, IPoE: billing.IPoE(s),
	}
	if m.cfg.RouterMock {
		c.NetflowAddr = "off" // the fake router sends no traffic-flow
		if s.Mode == store.SiteModeEnv {
			c.Synthetic = true
		}
	}
	if s.Mode == store.SiteModeWG {
		c.PortAllow = []string{s.WANInterface, s.OLTInterface, HotspotBridge}
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
	if m.GuardRequired(s) {
		rt.Guard = NewHostGuard(func(ctx context.Context) (map[string]string, error) {
			hs, err := m.Router(id).Hosts(ctx)
			if err != nil {
				return nil, err
			}
			out := make(map[string]string, len(hs))
			for _, h := range hs {
				if mac, err := billing.NormalizeMAC(h.MAC); err == nil {
					out[mac] = strings.TrimSpace(h.Address)
				}
			}
			return out, nil
		})
	}
	ctx, cancel := context.WithCancel(m.ctx)
	rt.cancel = cancel
	m.mu.Lock()
	m.rts[id] = rt
	m.mu.Unlock()
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.run(ctx, rt) }()
	m.log.Info("building started", "site", id, "name", s.Name, "mode", s.Mode)
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

// Guard returns the building's host guard, or nil when none runs for it.
func (m *Manager) Guard(siteID string) *HostGuard {
	if rt := m.runtime(siteID); rt != nil {
		return rt.Guard
	}
	return nil
}

// VerifyHost reports whether a portal request for (mac, ip) may be trusted as
// coming from the building's hotspot. It fails closed: when a guard is
// required but the building has no running guard, the answer is no.
func (m *Manager) VerifyHost(ctx context.Context, siteID, mac, ip string) bool {
	var s store.Site
	var ok bool
	m.st.View(func(d *store.Data) { s, ok = d.Sites[siteID] })
	if !ok || s.Removed {
		return false
	}
	if !m.GuardRequired(s) {
		return true
	}
	g := m.Guard(siteID)
	return g != nil && g.OK(ctx, mac, ip)
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

// SiteInput is the editable part of a building.
type SiteInput struct {
	Name, Notes, SupportPhone          string
	HotspotServer, WANInterface        string
	WANList, OLTInterface, CustomerNet string
	VLANID                             int
	WANCapMbps, OLTCapMbps, FUPTB      float64
	Provision, LegacyContainer         bool
	BenchPort                          string
}

func clean(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func applyInput(s store.Site, in SiteInput) store.Site {
	s.Name, s.Notes, s.SupportPhone = clean(in.Name, 60), clean(in.Notes, 300), clean(in.SupportPhone, 30)
	if s.Mode == store.SiteModeEnv { // the layout of an env building comes from the environment
		return s
	}
	s.HotspotServer, s.WANInterface = strings.TrimSpace(in.HotspotServer), strings.TrimSpace(in.WANInterface)
	s.WANList, s.OLTInterface = strings.TrimSpace(in.WANList), strings.TrimSpace(in.OLTInterface)
	s.CustomerSubnet, s.VLANID = strings.TrimSpace(in.CustomerNet), in.VLANID
	s.WANCapMbps, s.OLTCapMbps, s.FUPTB = in.WANCapMbps, in.OLTCapMbps, in.FUPTB
	s.Provision, s.LegacyContainer, s.BenchPort = in.Provision, in.LegacyContainer, strings.TrimSpace(in.BenchPort)
	return s
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
		s := applyInput(store.Site{Mode: store.SiteModeWG}, in)
		s = ApplyDefaults(m.cfg, s)
		if err := Validate(s); err != nil {
			return err
		}
		if nameTaken(d, s.Name, "") {
			return fmt.Errorf("there is already a building called %q", s.Name)
		}
		s.ID, s.CreatedAt = store.NewSiteID(), m.now()
		for {
			if _, clash := d.Sites[s.ID]; !clash {
				break
			}
			s.ID = store.NewSiteID()
		}
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
	err := m.st.Update(func(d *store.Data) error {
		cur, ok := d.Sites[id]
		if !ok || cur.Removed {
			return billing.ErrNoSite
		}
		s := applyInput(cur, in)
		if s.Mode == store.SiteModeWG {
			s = ApplyDefaults(m.cfg, s)
		}
		if err := Validate(ApplyDefaults(m.cfg, s)); err != nil {
			return err
		}
		if nameTaken(d, s.Name, id) {
			return fmt.Errorf("there is already a building called %q", s.Name)
		}
		d.Sites[id] = s
		return nil
	})
	if err == nil {
		m.Reload(id)
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
var ErrSiteHasAccounts = errors.New("this building still has accounts. Move or delete them first")

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
			return fmt.Errorf("type the building name %q to confirm", s.Name)
		}
		for _, a := range d.Accounts {
			if a.SiteID == id {
				return ErrSiteHasAccounts
			}
		}
		s.Removed, s.Disabled = true, false
		s.TunnelIP, s.WGPublicKey, s.RESTPass, s.Join = "", "", "", store.JoinToken{}
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
		d.AddEvent(store.Event{Time: m.now(), SiteID: id, Kind: "info", Msg: "building removed: " + s.Name})
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
	if s.Mode == store.SiteModeWG && s.WGPublicKey != "" {
		if ps, found, known := m.PeerStatus(siteID); known {
			st.WGKnown = true
			if age, has := ps.HandshakeAge(now); found && has {
				st.Handshake, st.HandshakeAge = true, int64(age.Seconds())
			}
		}
	}
	pc, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := m.Router(siteID).Ping(pc); err != nil {
		st.RESTErr = err.Error()
	} else {
		st.RESTOK = true
	}
	return st, true
}

func nowFn() time.Time { return time.Now() }
