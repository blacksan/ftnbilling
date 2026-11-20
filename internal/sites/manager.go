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

	renderFail renderFails // join fetches whose script could not be made (joinreport.go)
	refused    refusals    // join fetches turned away for their User-Agent (joinreport.go)

	chk          checksState   // cache of the setup checks (checks.go)
	autoFixDelay time.Duration // wait after the first answer before the post-join fixes (runtime.go)

	cport     cpState    // customer-port and automatic-fix state per building (custport.go, autofix.go)
	prov      provMgr    // billing hooks and the reboot slot (provision.go)
	live      liveState  // live router views (live.go)
	portRates portsRates // byte counters of the Ports view (ports.go)
	box       *secretbox.Box
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
	// peers.conf is written after the store commit of a join, a reset or a removal; a restart in that gap would leave
	// wg0 without a new router (or with a removed one's key). Rewriting it at start (and so touching the apply stamp
	// the path unit watches) makes the file follow the store again.
	if err := m.ApplyPeers(); err != nil {
		m.log.Error("could not write the WireGuard peers at start", "err", err)
	}
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
	// Confirm says the operator accepted the warning for a change of the OLT port or VLAN on a router
	// that is set up (customers lose service until the OLT uses the new setting). Not stored.
	Confirm bool
	// ConfirmName is the typed router name that a change of the admin LAN ports of a set-up router needs. Not stored.
	ConfirmName string
	// Base is the layout the editor started from (the effective LAN ports, customer ports, OLT port and VLAN
	// it was shown). A field the editor left as it was keeps the stored value, so a form that has been open
	// while the Ports tab changed a port cannot put the old value back. A field the editor changed while the
	// stored value had moved on is refused (ErrStaleLayout). Nil: every field of the input counts.
	Base *SiteInput
}

// ErrStaleLayout: the ports changed since the editor loaded them; reload and try again.
var ErrStaleLayout = errors.New("The ports of this router changed since this page was loaded (for example on the Ports tab). Reload the page and make the change again.")

// SiteInputOf is the input that leaves a router as it is (a PATCH starts from it).
func SiteInputOf(st store.Site) SiteInput {
	return SiteInput{Name: st.Name, Notes: st.Notes, SupportPhone: st.SupportPhone, WANInterface: st.WANInterface, OLTInterface: st.OLTInterface,
		CustomerNet: st.CustomerSubnet, VLANID: st.VLANID, WANCapMbps: st.WANCapMbps, OLTCapMbps: st.OLTCapMbps, FUPTB: st.FUPTB, CustomerPorts: st.CustomerPorts,
		WANMode: st.WANMode, WANAddress: st.WANAddress, WANGateway: st.WANGateway, WANDNS: st.WANDNS, PPPoEUser: st.PPPoEUser,
		LANPorts: st.LANPorts, LANSubnet: st.LANSubnet}
}

// ErrOLTConfirmRequired: the OLT port or the VLAN of a set-up router cannot change without confirm.
var ErrOLTConfirmRequired = errors.New("Customers on the OLT lose service until the OLT uses the new setting. Confirm to change the OLT port or the VLAN.")

// ErrLANConfirmRequired: changing the admin LAN ports of a set-up router needs the typed router name.
var ErrLANConfirmRequired = errors.New("Type the router name to confirm. " + WarnLAN)

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

// layoutPorts is the port part of an input in the form the server compares: defaults filled in, no spaces.
type layoutPorts struct {
	lan, cust, olt string
	vlan           int
}

func (m *Manager) layoutOf(in SiteInput) layoutPorts {
	e := m.Effective(store.Site{LANPorts: in.LANPorts, OLTInterface: in.OLTInterface})
	return layoutPorts{lan: strings.Join(SplitPorts(e.LANPorts), ","), cust: strings.Join(SplitPorts(in.CustomerPorts), ","), olt: e.OLTInterface, vlan: in.VLANID}
}

// rebaseInput applies "only what the editor changed": a port field equal to the editor's Base takes the stored
// value; one that differs from Base while the store has moved on is stale.
func (m *Manager) rebaseInput(cur store.Site, in SiteInput) (SiteInput, error) {
	if in.Base == nil {
		return in, nil
	}
	base, have, want := m.layoutOf(*in.Base), m.layoutOf(SiteInputOf(cur)), m.layoutOf(in)
	if want.lan == base.lan {
		in.LANPorts = cur.LANPorts
	} else if have.lan != base.lan {
		return in, ErrStaleLayout
	}
	if want.cust == base.cust {
		in.CustomerPorts = cur.CustomerPorts
	} else if have.cust != base.cust {
		return in, ErrStaleLayout
	}
	if want.olt == base.olt {
		in.OLTInterface = cur.OLTInterface
	} else if have.olt != base.olt {
		return in, ErrStaleLayout
	}
	if want.vlan == base.vlan {
		in.VLANID = cur.VLANID
	} else if have.vlan != base.vlan {
		return in, ErrStaleLayout
	}
	return in, nil
}

// UpdateSite saves the editable fields and restarts the building's runtime. It takes the same per-router
// lock as the port pass and ChangePort, so a form save and a Ports-tab change never interleave.
func (m *Manager) UpdateSite(id string, in SiteInput) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return m.UpdateSiteCtx(ctx, id, in)
}

// UpdateSiteCtx is UpdateSite whose wait for the router's lock ends with ctx (ErrRouterBusy).
func (m *Manager) UpdateSiteCtx(ctx context.Context, id string, in SiteInput) error {
	var portsChanged bool
	e := m.cport.entry(id)
	// The wait for the router's lock is capped like every admin action (billing.AdminRunCap): a sweep or port pass
	// on a slow router must not hold a form for minutes. Nothing is saved when it gives up.
	lctx, lcancel := context.WithTimeout(ctx, billing.AdminRunCap)
	lerr := e.run.LockCtx(lctx)
	lcancel()
	if lerr != nil {
		return lerr
	}
	defer e.run.Unlock()
	var cur0 store.Site
	var found bool
	m.st.View(func(d *store.Data) { cur0, found = d.Sites[id] })
	if !found || cur0.Removed {
		return billing.ErrNoSite
	}
	asked := in // what the editor sent; rebased again inside the transaction against the stored site
	in, err0 := m.rebaseInput(cur0, asked)
	if err0 != nil {
		return err0
	}
	if err := m.checkPortsExist(id, in); err != nil {
		return err
	}
	err := m.st.Update(func(d *store.Data) error {
		portsChanged = false
		cur, ok := d.Sites[id]
		if !ok || cur.Removed {
			return billing.ErrNoSite
		}
		in, err := m.rebaseInput(cur, asked)
		if err != nil {
			return err
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
		if now.WANInterface != was.WANInterface && siteIsSetUp(cur) && TokenStateOf(cur.Join, m.now()) != TokenPending {
			return fieldErr("wan_interface", "%s", WANPortLockedMsg)
		}
		// the OLT port and the customer ports of a router on the ports-off policy must already be switched on by the owner
		if portsManaged(cur) && siteIsSetUp(cur) {
			var moved []string
			if now.OLTInterface != was.OLTInterface {
				moved = append(moved, now.OLTInterface)
			}
			moved = append(moved, addedPorts(SplitPorts(was.CustomerPorts), SplitPorts(now.CustomerPorts))...)
			for _, p := range moved {
				if p != "" && !containsStr(SplitPorts(cur.EnabledPorts), p) {
					field := "customer_ports"
					if p == now.OLTInterface && now.OLTInterface != was.OLTInterface {
						field = "olt_interface"
					}
					return fieldErr(field, "%s", PortNotEnabledMsg(p))
				}
			}
		}
		portsChanged = now.CustomerPorts != was.CustomerPorts || now.LANPorts != was.LANPorts || now.OLTInterface != was.OLTInterface || now.VLANID != was.VLANID
		if (now.OLTInterface != was.OLTInterface || now.VLANID != was.VLANID) && siteIsSetUp(cur) && !in.Confirm {
			return ErrOLTConfirmRequired
		}
		// a port that became an admin LAN port or the OLT uplink is not kept off any more
		var keepOff []string
		for _, p := range SplitPorts(s.DisabledPorts) {
			if p != now.OLTInterface && !containsStr(SplitPorts(now.LANPorts), p) {
				keepOff = append(keepOff, p)
			}
		}
		s.DisabledPorts = strings.Join(keepOff, ",")
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

// WANPortLockedMsg: the WAN port of a router that has joined cannot be edited in the router form.
const WANPortLockedMsg = "The internet port (WAN) of a router that has joined cannot be changed here: the router would lose its uplink. Make a new setup line (rebuild) for this router, then change it."

// PortNotEnabledMsg: a role cannot move to a port the owner has not switched on (same rule as ChangePort).
func PortNotEnabledMsg(port string) string {
	return "Enable " + port + " in the Ports tab first, then move the role to it."
}

// siteIsSetUp: the router has joined (its key is known and it has answered), whether or not billing has
// paused it. Changing its OLT port or VLAN cuts customers off, so it asks for a confirm.
func siteIsSetUp(s store.Site) bool { return s.WGPublicKey != "" && !s.JoinedAt.IsZero() }

// checkPortsExist refuses a new OLT port or customer port the router does not have (a typo would
// otherwise move the old port's membership away from working customers). It needs the router to
// answer; when it does not, or the router is not set up yet, the layout validation alone decides.
func (m *Manager) checkPortsExist(id string, in SiteInput) error {
	var cur store.Site
	var ok bool
	m.st.View(func(d *store.Data) { cur, ok = d.Sites[id] })
	if !ok || !siteIsSetUp(cur) {
		return nil
	}
	was := m.Effective(cur)
	if err := m.checkLANChange(cur, was, in); err != nil {
		return err
	}
	newCust := addedPorts(SplitPorts(was.CustomerPorts), SplitPorts(in.CustomerPorts))
	newLAN := addedPorts(SplitPorts(was.LANPorts), SplitPorts(in.LANPorts))
	olt := m.Effective(store.Site{OLTInterface: strings.TrimSpace(in.OLTInterface)}).OLTInterface
	if len(newCust) == 0 && len(newLAN) == 0 && olt == was.OLTInterface {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeros.DefaultTimeout)
	defer cancel()
	choices := m.PortChoices(ctx, id)
	if len(choices) == 0 {
		return nil
	}
	has := map[string]bool{}
	for _, c := range choices {
		has[c] = true
	}
	// the WAN, the admin LAN port and the like are refused by the layout rules with their own words
	if olt != was.OLTInterface && !has[olt] && olt != was.WANInterface {
		return fieldErr("olt_interface", "The router has no port called %s. Its ports: %s.", olt, strings.Join(choices, ", "))
	}
	for _, c := range newCust {
		if !has[c] && c != was.WANInterface {
			return fieldErr("customer_ports", "The router has no port called %s. Its ports: %s.", c, strings.Join(choices, ", "))
		}
	}
	for _, c := range newLAN {
		if !has[c] && c != was.WANInterface {
			return fieldErr("lan_ports", "The router has no port called %s. Its ports: %s.", c, strings.Join(choices, ", "))
		}
	}
	return nil
}

// addedPorts is what is in now and not in was.
func addedPorts(was, now []string) []string {
	var out []string
	for _, n := range now {
		if !containsStr(was, n) {
			out = append(out, n)
		}
	}
	return out
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
	if rf, ok := m.renderFail.get(siteID); ok && st.Token == TokenPending {
		st.RenderError = rf.Msg
	}
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

// checkLANChange guards the admin LAN ports of a set-up router: a change needs the typed router name, and when a
// port leaves the list at least one port that stays must be switched on and have a link (it is read from the
// router; a router that cannot be read refuses, because the old port's membership would go without proof).
func (m *Manager) checkLANChange(cur store.Site, was store.Site, in SiteInput) error {
	nowLAN := SplitPorts(m.Effective(store.Site{LANPorts: in.LANPorts}).LANPorts)
	oldLAN := SplitPorts(was.LANPorts)
	if sameList(oldLAN, nowLAN) {
		return nil
	}
	if strings.TrimSpace(in.ConfirmName) != cur.Name {
		return ErrLANConfirmRequired
	}
	if len(addedPorts(nowLAN, oldLAN)) == 0 {
		return nil // only ports added: nothing leaves
	}
	ctx, cancel := context.WithTimeout(context.Background(), routeros.DefaultTimeout)
	defer cancel()
	rt := m.Router(cur.ID)
	ifaces, err := m.ifaceRows(ctx, rt)
	if err != nil {
		return fieldErr("lan_ports", "The router did not answer, so the server cannot check that an admin LAN port stays connected. Try again when it is reachable.")
	}
	keep := 0
	for _, p := range nowLAN {
		if portUsable(ifaces[p]) {
			keep++
		}
	}
	if keep == 0 {
		return &PortRefusal{Kind: PortErrLastLAN, Msg: fmt.Sprintf("None of the admin LAN ports (%s) is switched on and connected, so the router would be cut off. Connect and enable one first.", strings.Join(nowLAN, ", "))}
	}
	return nil
}
