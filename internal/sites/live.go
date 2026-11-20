package sites

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Live router data (docs/release-plan-routers.md D13, section 3 "W9 notes").
//
// The app and the web UI see a router only through three named views (live,
// interfaces, clients). Each view reads a FIXED list of RouterOS paths (see
// LiveReadPaths), keeps a few named fields of each row in a typed struct and
// throws the row away: no client-supplied path, no raw row and no secret ever
// leaves this file. Answers are cached 5 s per router and view, concurrent
// callers share one read (single flight), a read is cut at 3 s, and when the
// router cannot be read the last good answer is served for up to 60 s marked
// reachable:false.

// LiveReadPaths is every RouterOS path the live views read, nothing else.
// A test fails when a view reads a path that is not listed (and the other way).
var LiveReadPaths = []string{
	"/system/resource",
	"/system/health",
	"/system/routerboard",
	"/interface",
	"/ip/address",
	"/ip/dhcp-server/lease",
	"/ip/arp",
	"/interface/bridge/host",
	"/queue/simple",
}

// liveTimeout cuts one read of a router when the configuration gives none (a
// variable so a test can shorten it; production uses Config.RouterReadTimeout).
var liveTimeout = 8 * time.Second

func (m *Manager) liveReadTimeout() time.Duration {
	if liveTimeout != defaultLiveTimeout {
		return liveTimeout // a test shortened it
	}
	if m.cfg.RouterReadTimeout > 0 {
		return m.cfg.RouterReadTimeout
	}
	return liveTimeout
}

const defaultLiveTimeout = 8 * time.Second

const (
	liveTTL = 10 * time.Second

	liveStaleMax = 120 * time.Second
	liveMaxRows  = 5000
	liveSeries   = 36 // five-minute buckets of the throughput series (3 hours)

	// ClientOnlineWindow is how recent a sign of life must be for a flat to be online.
	ClientOnlineWindow = 3 * time.Minute
)

// ---- types (the API shapes, docs/api-v1.md addendum L) ------------------------------------------------

// LiveSystem is the router's identity and load.
type LiveSystem struct {
	Board      string   `json:"board"`
	Model      string   `json:"model"`
	Serial     string   `json:"serial"`
	Version    string   `json:"version"`
	Arch       string   `json:"arch"`
	CPUCount   int      `json:"cpu_count"`
	CPULoadPct int      `json:"cpu_load_pct"`
	MemUsed    int64    `json:"mem_used_bytes"`
	MemTotal   int64    `json:"mem_total_bytes"`
	DiskFree   int64    `json:"disk_free_bytes"`
	UptimeS    int64    `json:"uptime_s"`
	TempC      *float64 `json:"temp_c"`
}

// LiveWAN is the uplink.
type LiveWAN struct {
	Interface string   `json:"interface"`
	Running   bool     `json:"running"`
	DownBps   *int64   `json:"down_bps"`
	UpBps     *int64   `json:"up_bps"`
	CapMbps   float64  `json:"cap_mbps"`
	UtilPct   *float64 `json:"util_pct"`
}

// LiveTunnel is the WireGuard tunnel to the server (filled by the web layer).
type LiveTunnel struct {
	HandshakeAgeS *int64   `json:"handshake_age_s"`
	RTTMs         *float64 `json:"rtt_ms"`
	LossPct       *float64 `json:"loss_pct"`
}

// LiveThroughput is the WAN rate of the last three hours at 5-minute steps;
// a missing bucket is null.
type LiveThroughput struct {
	StepS   int      `json:"step_s"`
	T       []string `json:"t"`
	DownBps []*int64 `json:"down_bps"`
	UpBps   []*int64 `json:"up_bps"`
}

// LiveFlats counts the router's flats.
type LiveFlats struct {
	Total      int `json:"total"`
	Online     int `json:"online"`
	Active     int `json:"active"`
	Expired    int `json:"expired"`
	Suspended  int `json:"suspended"`
	Expiring7d int `json:"expiring_7d"`
}

// LiveEquipment counts the router's OLTs and ONTs in the inventory.
type LiveEquipment struct {
	OLT int `json:"olt"`
	ONT int `json:"ont"`
}

// LiveRevenue is KES taken, EAT calendar day and month.
type LiveRevenue struct {
	TodayKES int `json:"today_kes"`
	MonthKES int `json:"month_kes"`
}

// LiveProvision is the reconciler's state.
type LiveProvision struct {
	InSync     bool   `json:"in_sync"`
	Drift      int    `json:"drift"`
	PendingOps int    `json:"pending_ops"`
	LastError  string `json:"last_error"`
}

// RouterLive is the answer of GET /routers/{id}/live. Manager.LiveRouter fills
// RouterID, GeneratedAt, Reachable, System, WAN (rates from the collector) and
// Throughput; the web layer fills the rest from the store and the fleet.
type RouterLive struct {
	RouterID           string         `json:"router_id"`
	GeneratedAt        time.Time      `json:"generated_at"`
	Reachable          bool           `json:"reachable"`
	System             LiveSystem     `json:"system"`
	WAN                LiveWAN        `json:"wan"`
	Tunnel             LiveTunnel     `json:"tunnel"`
	Throughput         LiveThroughput `json:"throughput"`
	Flats              LiveFlats      `json:"flats"`
	UnregisteredOnline int            `json:"unregistered_online"`
	ONTsBound          int            `json:"onts_bound"`
	Equipment          LiveEquipment  `json:"equipment"`
	Revenue            LiveRevenue    `json:"revenue"`
	OpenAlerts         int            `json:"open_alerts"`
	Provision          LiveProvision  `json:"provision"`
	SetupState         string         `json:"setup_state"`
}

// Interface is one router interface.
type Interface struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Running    bool     `json:"running"`
	Disabled   bool     `json:"disabled"`
	Comment    string   `json:"comment"`
	MAC        string   `json:"mac"`
	MTU        int      `json:"mtu"`
	Role       string   `json:"role"` // wan | olt | customer | tunnel | bench | other
	RxBps      *int64   `json:"rx_bps"`
	TxBps      *int64   `json:"tx_bps"`
	RxBytes    uint64   `json:"rx_bytes"`
	TxBytes    uint64   `json:"tx_bytes"`
	Addresses  []string `json:"addresses"`
	LastLinkUp *string  `json:"last_link_up"` // the router's own clock text, as is
}

// Interface roles.
const (
	RoleWAN      = "wan"
	RoleOLT      = "olt"
	RoleCustomer = "customer"
	RoleTunnel   = "tunnel"
	RoleBench    = "bench"
	RoleOther    = "other"
)

// InterfacesView is the answer of the interfaces view.
type InterfacesView struct {
	Items       []Interface
	GeneratedAt time.Time
	Reachable   bool
}

// LiveLease is the kept part of one /ip/dhcp-server/lease row.
type LiveLease struct {
	IP, MAC, Host, Status, Server string
	LastSeenS                     int64 // seconds since the router saw it; -1 unknown
	Dynamic                       bool
}

// LiveARP is the kept part of one /ip/arp row.
type LiveARP struct {
	IP, MAC, Interface string
	Complete, Dynamic  bool
}

// LiveHost is the kept part of one /interface/bridge/host row.
type LiveHost struct {
	MAC, Port, Bridge string
	Local             bool
}

// LiveQueue is the kept part of one /queue/simple row.
type LiveQueue struct {
	Name, Target string
	Code         string // the flat's code when it is one of ours
	LimitDown    int64  // bit/s, 0 = none
	LimitUp      int64
}

// LiveTables is the clients view's raw material: four small typed tables.
type LiveTables struct {
	Leases      []LiveLease
	ARP         []LiveARP
	Hosts       []LiveHost
	Queues      []LiveQueue
	GeneratedAt time.Time
	Reachable   bool
}

// ---- cache ---------------------------------------------------------------------------------------------

type liveState struct {
	mu   sync.Mutex
	ent  map[string]*liveEntry
	prev map[string]map[string]ifSample // site -> interface -> last counters
}

type ifSample struct {
	rx, tx uint64
	at     time.Time
}

type liveEntry struct {
	val    any
	at     time.Time
	flight *liveFlight
}

type liveFlight struct {
	done chan struct{}
	err  error
}

const (
	viewLive       = "live"
	viewInterfaces = "interfaces"
	viewClients    = "clients"
)

// liveGet returns the cached value of (site, view), or reads it. reachable is
// false when the value is a stale one served after a failed read; err is set
// only when there is nothing (fresh or stale) to give.
func (m *Manager) liveGet(ctx context.Context, siteID, view string, fetch func(context.Context) (any, error)) (val any, reachable bool, err error) {
	return m.liveGetTTL(ctx, siteID, view, liveTTL, fetch)
}

// liveGetTTL is liveGet with its own freshness window.
func (m *Manager) liveGetTTL(ctx context.Context, siteID, view string, ttl time.Duration, fetch func(context.Context) (any, error)) (val any, reachable bool, err error) {
	key := siteID + "|" + view
	for {
		now := m.now()
		m.live.mu.Lock()
		if m.live.ent == nil {
			m.live.ent = map[string]*liveEntry{}
		}
		e := m.live.ent[key]
		if e == nil {
			e = &liveEntry{}
			m.live.ent[key] = e
		}
		if e.val != nil && e.flight == nil && now.Sub(e.at) < ttl && !now.Before(e.at) {
			v := e.val
			m.live.mu.Unlock()
			return v, true, nil
		}
		if f := e.flight; f != nil { // somebody is reading: wait for it
			m.live.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
			m.live.mu.Lock()
			v, at := e.val, e.at
			m.live.mu.Unlock()
			if f.err == nil && v != nil {
				return v, true, nil
			}
			return m.staleOr(v, at, f.err)
		}
		f := &liveFlight{done: make(chan struct{})}
		e.flight = f
		m.live.mu.Unlock()

		// The read outlives the caller that started it (others wait on it) but not longer than the read timeout.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.liveReadTimeout())
		v, ferr := fetch(rctx)
		cancel()

		m.live.mu.Lock()
		if ferr == nil {
			e.val, e.at = v, m.now()
		}
		f.err = ferr
		e.flight = nil
		old, oldAt := e.val, e.at
		m.live.mu.Unlock()
		close(f.done)
		if ferr == nil {
			return v, true, nil
		}
		return m.staleOr(old, oldAt, ferr)
	}
}

// staleOr gives the last good value when it is at most a minute old.
func (m *Manager) staleOr(v any, at time.Time, err error) (any, bool, error) {
	if v != nil && m.now().Sub(at) <= liveStaleMax {
		return v, false, nil
	}
	if err == nil {
		err = errors.New("no answer from the router")
	}
	return nil, false, err
}

func (m *Manager) liveSite(siteID string) (store.Site, error) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return store.Site{}, billing.ErrNoSite
	}
	return m.Effective(site), nil
}

// ---- reading -------------------------------------------------------------------------------------------

type liveReader struct {
	rt   routeros.Client
	insp routeros.Inspector
}

func newLiveReader(rt routeros.Client) (*liveReader, error) {
	insp, ok := rt.(routeros.Inspector)
	if !ok {
		return nil, routeros.ErrNoInspector
	}
	return &liveReader{rt: rt, insp: insp}, nil
}

func (r *liveReader) one(ctx context.Context, path string) (routeros.Row, error) {
	return r.insp.One(ctx, path)
}

func (r *liveReader) rows(ctx context.Context, path string) ([]routeros.Row, error) {
	rows, err := r.rt.Rows(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	if len(rows) > liveMaxRows {
		rows = rows[:liveMaxRows]
	}
	return rows, nil
}

func atoi(s string) int64 { n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64); return n }
func atou(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return n
}
func isYes(s string) bool { return s == "true" || s == "yes" }

func int64p(v int64) *int64 { return &v }

// ---- the live view -------------------------------------------------------------------------------------

// LiveRouter reads the router-side half of RouterLive. billing.ErrNoSite for an
// unknown router; any other trouble gives Reachable=false (with the last good
// reading while it is at most 60 s old).
func (m *Manager) LiveRouter(ctx context.Context, siteID string) (RouterLive, error) {
	site, err := m.liveSite(siteID)
	if err != nil {
		return RouterLive{}, err
	}
	rt := m.Router(siteID)
	v, reach, err := m.liveGet(ctx, siteID, viewLive, func(c context.Context) (any, error) {
		return m.readLive(c, site, rt)
	})
	out := RouterLive{RouterID: siteID, GeneratedAt: m.now().UTC()}
	if lv, ok := v.(RouterLive); ok && err == nil {
		out = lv
		out.Reachable = reach
	}
	out.WAN.Interface = site.WANInterface
	out.WAN.CapMbps = site.WANCapMbps
	m.liveCollector(&out, site)
	out.Tunnel = LiveTunnel{}
	return out, nil
}

// liveCollector adds what the metrics collector knows: the WAN's rate now and
// the throughput series. It is always read fresh (it is in memory).
func (m *Manager) liveCollector(out *RouterLive, site store.Site) {
	now := m.now()
	out.Throughput = LiveThroughput{StepS: 300, T: []string{}, DownBps: []*int64{}, UpBps: []*int64{}}
	out.WAN.DownBps, out.WAN.UpBps, out.WAN.UtilPct = nil, nil, nil
	col := m.Collector(site.ID)
	if col == nil {
		return
	}
	if lp, ok := col.Snapshot().Ports[site.WANInterface]; ok && now.Sub(lp.At) < 5*time.Minute {
		out.WAN.DownBps, out.WAN.UpBps = int64p(int64(lp.RxBps)), int64p(int64(lp.TxBps))
		if site.WANCapMbps > 0 {
			u := math.Round(math.Max(lp.RxBps, lp.TxBps)/(site.WANCapMbps*1e6)*1000) / 10
			out.WAN.UtilPct = &u
		}
	}
	ts, down, up := col.RecentWAN(now, liveSeries)
	for i, t := range ts {
		out.Throughput.T = append(out.Throughput.T, t.UTC().Format(time.RFC3339))
		out.Throughput.DownBps = append(out.Throughput.DownBps, down[i])
		out.Throughput.UpBps = append(out.Throughput.UpBps, up[i])
	}
}

func (m *Manager) readLive(ctx context.Context, site store.Site, rt routeros.Client) (any, error) {
	rd, err := newLiveReader(rt)
	if err != nil {
		return nil, err
	}
	var (
		res, rb     routeros.Row
		health, ifs []routeros.Row
		resErr      error
		wg          sync.WaitGroup
	)
	wg.Add(4)
	go func() { defer wg.Done(); res, resErr = rd.one(ctx, "/system/resource") }()
	go func() { defer wg.Done(); rb, _ = rd.one(ctx, "/system/routerboard") }()
	go func() {
		defer wg.Done()
		health, _ = rd.rows(ctx, "/system/health")
	}()
	go func() { defer wg.Done(); ifs, _ = rd.rows(ctx, "/interface") }()
	wg.Wait()
	if resErr != nil {
		return nil, resErr
	}
	lv := RouterLive{RouterID: site.ID, GeneratedAt: m.now().UTC()}
	sys := &lv.System
	sys.Board, sys.Version, sys.Arch = res["board-name"], res["version"], res["architecture-name"]
	sys.CPUCount, sys.CPULoadPct = int(atoi(res["cpu-count"])), int(atoi(res["cpu-load"]))
	sys.MemTotal = atoi(res["total-memory"])
	if free := atoi(res["free-memory"]); sys.MemTotal > 0 && free <= sys.MemTotal {
		sys.MemUsed = sys.MemTotal - free
	}
	sys.DiskFree = atoi(res["free-hdd-space"])
	sys.UptimeS = int64(routeros.ParseRouterDuration(res["uptime"]).Seconds())
	sys.Model, sys.Serial = rb["model"], rb["serial-number"]
	if sys.Model == "" {
		sys.Model = sys.Board
	}
	sys.TempC = healthTemp(health)
	for _, r := range ifs {
		if r["name"] == site.WANInterface {
			lv.WAN.Running = isYes(r["running"])
		}
	}
	return lv, nil
}

// healthTemp reads the board temperature from /system/health, which RouterOS 7
// answers as name/value rows (older builds as one row with the names as keys).
func healthTemp(rows []routeros.Row) *float64 {
	vals := map[string]string{}
	for _, r := range rows {
		if n := r["name"]; n != "" {
			vals[n] = r["value"]
			continue
		}
		for _, k := range []string{"temperature", "cpu-temperature"} {
			if v, ok := r[k]; ok {
				vals[k] = v
			}
		}
	}
	for _, k := range []string{"temperature", "cpu-temperature"} {
		if v, ok := vals[k]; ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return &f
			}
		}
	}
	return nil
}

// ---- the interfaces view -------------------------------------------------------------------------------

// LiveInterfaces reads the router's interfaces with their rates. A rate is the
// byte-counter delta between this read and the previous one, so the first read
// (and a read after a counter reset) has null rates.
func (m *Manager) LiveInterfaces(ctx context.Context, siteID string) (InterfacesView, error) {
	site, err := m.liveSite(siteID)
	if err != nil {
		return InterfacesView{}, err
	}
	rt := m.Router(siteID)
	v, reach, err := m.liveGet(ctx, siteID, viewInterfaces, func(c context.Context) (any, error) {
		return m.readInterfaces(c, site, rt)
	})
	if iv, ok := v.(InterfacesView); ok && err == nil {
		iv.Reachable = reach
		return iv, nil
	}
	return InterfacesView{Items: []Interface{}, GeneratedAt: m.now().UTC()}, nil
}

func (m *Manager) readInterfaces(ctx context.Context, site store.Site, rt routeros.Client) (any, error) {
	rd, err := newLiveReader(rt)
	if err != nil {
		return nil, err
	}
	var addrs []routeros.Row
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); addrs, _ = rd.rows(ctx, "/ip/address") }()
	rows, err := rd.rows(ctx, "/interface")
	wg.Wait()
	if err != nil {
		return nil, err
	}
	now := m.now()

	byIface := map[string][]string{}
	customer := ""
	gw := ""
	if pl, err := PlanSubnet(site.CustomerSubnet); err == nil {
		gw = pl.GatewayIP
	}
	for _, a := range addrs {
		if isYes(a["disabled"]) || a["interface"] == "" {
			continue
		}
		byIface[a["interface"]] = append(byIface[a["interface"]], a["address"])
		if gw != "" && a["address"] == gw {
			customer = a["interface"]
		}
	}
	if customer == "" {
		customer = CustomerBridge
	}

	m.live.mu.Lock()
	if m.live.prev == nil {
		m.live.prev = map[string]map[string]ifSample{}
	}
	prev := m.live.prev[site.ID]
	cur := make(map[string]ifSample, len(rows))
	m.live.mu.Unlock()

	items := make([]Interface, 0, len(rows))
	for _, r := range rows {
		name := r["name"]
		if name == "" {
			continue
		}
		it := Interface{Name: name, Type: r["type"], Running: isYes(r["running"]), Disabled: isYes(r["disabled"]), Comment: r["comment"],
			MAC: r["mac-address"], MTU: int(atoi(firstNonEmpty(r["actual-mtu"], r["mtu"]))), RxBytes: atou(r["rx-byte"]), TxBytes: atou(r["tx-byte"]),
			Addresses: append([]string{}, byIface[name]...)}
		if v := strings.TrimSpace(r["last-link-up-time"]); v != "" {
			it.LastLinkUp = &v
		}
		it.Role = ifaceRole(site, it, customer)
		cur[name] = ifSample{rx: it.RxBytes, tx: it.TxBytes, at: now}
		if p, ok := prev[name]; ok {
			if dt := now.Sub(p.at).Seconds(); dt > 0 && it.RxBytes >= p.rx && it.TxBytes >= p.tx {
				it.RxBps = int64p(int64(float64(it.RxBytes-p.rx) * 8 / dt))
				it.TxBps = int64p(int64(float64(it.TxBytes-p.tx) * 8 / dt))
			}
		}
		items = append(items, it)
	}
	m.live.mu.Lock()
	m.live.prev[site.ID] = cur
	m.live.mu.Unlock()
	return InterfacesView{Items: items, GeneratedAt: now.UTC()}, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func ifaceRole(site store.Site, it Interface, customer string) string {
	switch {
	case it.Type == "wg" || it.Type == "wireguard" || it.Name == WGInterface:
		return RoleTunnel
	case it.Name == site.WANInterface:
		return RoleWAN
	case it.Name == site.OLTInterface:
		return RoleOLT
	case containsStr(SplitPorts(site.CustomerPorts), it.Name):
		return RoleBench
	case it.Name == customer:
		return RoleCustomer
	}
	return RoleOther
}

// ---- the clients view (router side) --------------------------------------------------------------------

// LiveClientTables reads the leases, ARP, bridge hosts and queues of a router.
func (m *Manager) LiveClientTables(ctx context.Context, siteID string) (LiveTables, error) {
	if _, err := m.liveSite(siteID); err != nil {
		return LiveTables{}, err
	}
	rt := m.Router(siteID)
	v, reach, err := m.liveGet(ctx, siteID, viewClients, func(c context.Context) (any, error) {
		return m.readTables(c, rt)
	})
	if t, ok := v.(LiveTables); ok && err == nil {
		t.Reachable = reach
		return t, nil
	}
	return LiveTables{GeneratedAt: m.now().UTC()}, nil
}

func (m *Manager) readTables(ctx context.Context, rt routeros.Client) (any, error) {
	rd, err := newLiveReader(rt)
	if err != nil {
		return nil, err
	}
	paths := []string{"/ip/dhcp-server/lease", "/ip/arp", "/interface/bridge/host", "/queue/simple"}
	rows := make([][]routeros.Row, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func() { defer wg.Done(); rows[i], errs[i] = rd.rows(ctx, p) }()
	}
	wg.Wait()
	failed := 0
	var first error
	for _, e := range errs {
		if e != nil {
			failed++
			if first == nil {
				first = e
			}
		}
	}
	if failed == len(paths) {
		return nil, first
	}
	t := LiveTables{GeneratedAt: m.now().UTC()}
	for _, r := range rows[0] {
		mac, _ := store.CanonMAC(r["mac-address"])
		l := LiveLease{IP: r["address"], MAC: mac, Host: cleanHost(r["host-name"]), Status: r["status"], Server: r["server"],
			Dynamic: isYes(r["dynamic"]), LastSeenS: -1}
		if v := r["last-seen"]; v != "" {
			l.LastSeenS = int64(routeros.ParseRouterDuration(v).Seconds())
		}
		t.Leases = append(t.Leases, l)
	}
	for _, r := range rows[1] {
		mac, _ := store.CanonMAC(r["mac-address"])
		t.ARP = append(t.ARP, LiveARP{IP: r["address"], MAC: mac, Interface: r["interface"], Complete: isYes(r["complete"]), Dynamic: isYes(r["dynamic"])})
	}
	for _, r := range rows[2] {
		mac, _ := store.CanonMAC(r["mac-address"])
		t.Hosts = append(t.Hosts, LiveHost{MAC: mac, Port: r["on-interface"], Bridge: r["bridge"], Local: isYes(r["local"])})
	}
	for _, r := range rows[3] {
		q := LiveQueue{Name: r["name"], Target: r["target"]}
		q.LimitUp, q.LimitDown = splitPair(r["max-limit"])
		if c := strings.TrimPrefix(r["name"], "ftn-"); c != r["name"] && c != "" {
			q.Code = c
		}
		t.Queues = append(t.Queues, q)
	}
	return t, nil
}

func splitPair(s string) (int64, int64) {
	a, b, _ := strings.Cut(s, "/")
	return atoi(a), atoi(b)
}

// cleanHost keeps a DHCP host name printable and short.
func cleanHost(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 63 {
		s = string(r[:63])
	}
	return s
}

// ---- clients: the merge --------------------------------------------------------------------------------

// Client is one row of the clients view: a flat (an account) or a device that
// has no account.
type Client struct {
	Kind              string  `json:"kind"` // flat | unregistered
	AccountID         string  `json:"account_id"`
	Code              string  `json:"code"`
	Flat              string  `json:"flat"`
	Tenant            string  `json:"tenant"`
	State             string  `json:"state"`
	MAC               string  `json:"mac"`
	Vendor            string  `json:"vendor"`
	HostName          string  `json:"host_name"`
	IP                string  `json:"ip"`
	Online            bool    `json:"online"`
	LeaseStatus       string  `json:"lease_status"` // bound | waiting | none
	LastSeen          *string `json:"last_seen"`
	Port              string  `json:"port"`
	QueueLimit        string  `json:"queue_limit"` // "<down>/<up>", like 20M/20M; "" = no queue
	DownBps           int64   `json:"down_bps"`
	UpBps             int64   `json:"up_bps"`
	UsageTodayBytes   uint64  `json:"usage_today_bytes"`
	UsageMonthBytes   uint64  `json:"usage_month_bytes"`
	SuspendedOnRouter bool    `json:"suspended_on_router"`
	InSync            bool    `json:"in_sync"`
}

// ClientCounts are the totals of the whole list, before q/kind/online filters.
type ClientCounts struct {
	Flats        int `json:"flats"`
	Online       int `json:"online"`
	Unregistered int `json:"unregistered"`
}

// ClientAccount is what the merge needs to know about an account.
type ClientAccount struct {
	ID, Code, Flat, Tenant, State, MAC, IP string
	LeaseStatus                            string
	LastSeen                               time.Time
	SuspendedOnRouter, InSync              bool
	UsageToday, UsageMonth                 uint64
	LiveDown, LiveUp                       float64 // bit/s from the collector
	LiveAt                                 time.Time
}

// ClientDevice is a device that has no account (billing.UnregDevice, trimmed).
type ClientDevice struct {
	MAC, IP, HostName, Vendor string
	Online                    bool
	LastSeen                  time.Time
}

// ClientInput is everything MergeClients needs.
type ClientInput struct {
	Now      time.Time
	Accounts []ClientAccount
	Devices  []ClientDevice
	// Dismissed are the MACs the operator dismissed on this router: a bound lease of one of them is not listed
	// as a new device (it comes back only when the device opens the portal again, which clears the dismissal).
	Dismissed []string
	Tables    LiveTables
}

// MergeClients joins accounts, unregistered devices and the router's tables.
// Flats come first (by flat label), then devices (online first).
func MergeClients(in ClientInput) []Client {
	leaseByMAC := map[string]LiveLease{}
	leaseByIP := map[string]LiveLease{}
	for _, l := range in.Tables.Leases {
		if l.MAC != "" {
			// a bound row wins over a waiting one for the same MAC
			if old, ok := leaseByMAC[l.MAC]; !ok || (old.Status != "bound" && l.Status == "bound") {
				leaseByMAC[l.MAC] = l
			}
		}
		if l.IP != "" {
			leaseByIP[l.IP] = l
		}
	}
	hostByMAC := map[string]LiveHost{}
	for _, h := range in.Tables.Hosts {
		if h.MAC != "" && !h.Local {
			hostByMAC[h.MAC] = h
		}
	}
	arpOn := map[string]bool{}
	for _, a := range in.Tables.ARP {
		if a.MAC != "" && a.Dynamic && a.Complete {
			arpOn[a.MAC] = true
		}
	}
	queueByCode := map[string]LiveQueue{}
	for _, q := range in.Tables.Queues {
		if q.Code != "" {
			queueByCode[q.Code] = q
		}
	}
	seen := func(t time.Time) bool {
		return !t.IsZero() && in.Now.Sub(t) <= ClientOnlineWindow && !t.After(in.Now.Add(time.Minute))
	}
	macs := map[string]bool{}

	out := make([]Client, 0, len(in.Accounts)+len(in.Devices))
	for _, a := range in.Accounts {
		mac, _ := store.CanonMAC(a.MAC)
		if mac != "" {
			macs[mac] = true
		}
		c := Client{Kind: "flat", AccountID: a.ID, Code: a.Code, Flat: a.Flat, Tenant: a.Tenant, State: a.State, MAC: mac,
			Vendor: billing.Vendor(mac), IP: a.IP, LeaseStatus: a.LeaseStatus, SuspendedOnRouter: a.SuspendedOnRouter, InSync: a.InSync,
			UsageTodayBytes: a.UsageToday, UsageMonthBytes: a.UsageMonth}
		if c.LeaseStatus == "" {
			c.LeaseStatus = "none"
		}
		l, hasLease := leaseByMAC[mac]
		if !hasLease && a.IP != "" {
			l, hasLease = leaseByIP[a.IP]
			if hasLease && l.MAC != "" && l.MAC != mac {
				hasLease = false // the address belongs to someone else's MAC
			}
		}
		if hasLease {
			c.HostName = l.Host
			if l.IP != "" {
				c.IP = l.IP
			}
			if l.Status == "bound" || l.Status == "waiting" {
				c.LeaseStatus = l.Status
			}
		}
		h, hasHost := hostByMAC[mac]
		if hasHost {
			c.Port = h.Port
		}
		if q, ok := queueByCode[a.Code]; ok {
			c.QueueLimit = fmtRate(q.LimitDown) + "/" + fmtRate(q.LimitUp)
		}
		liveNow := seen(a.LiveAt)
		if liveNow {
			c.DownBps, c.UpBps = int64(a.LiveDown), int64(a.LiveUp)
		}
		c.Online = liveNow || seen(a.LastSeen) || hasHost || (mac != "" && arpOn[mac])
		if !a.LastSeen.IsZero() {
			s := a.LastSeen.UTC().Format(time.RFC3339)
			c.LastSeen = &s
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return naturalLess(out[i].Flat, out[j].Flat) })

	var devs []Client
	addDev := func(d ClientDevice) {
		mac, _ := store.CanonMAC(d.MAC)
		if mac == "" || macs[mac] {
			return
		}
		macs[mac] = true
		c := Client{Kind: "unregistered", MAC: mac, Vendor: firstNonEmpty(d.Vendor, billing.Vendor(mac)), HostName: d.HostName, IP: d.IP,
			LeaseStatus: "none", InSync: true}
		if l, ok := leaseByMAC[mac]; ok {
			c.HostName = firstNonEmpty(c.HostName, l.Host)
			c.IP = firstNonEmpty(l.IP, c.IP)
			if l.Status == "bound" || l.Status == "waiting" {
				c.LeaseStatus = l.Status
			}
		}
		if h, ok := hostByMAC[mac]; ok {
			c.Port = h.Port
		}
		c.Online = d.Online || hostByMAC[mac].MAC != "" || arpOn[mac]
		if !d.LastSeen.IsZero() {
			s := d.LastSeen.UTC().Format(time.RFC3339)
			c.LastSeen = &s
		}
		devs = append(devs, c)
	}
	for _, d := range in.Devices {
		addDev(d)
	}
	// devices the router shows that the server has not noted yet (not the ones the operator dismissed)
	dismissed := map[string]bool{}
	for _, m := range in.Dismissed {
		if c, _ := store.CanonMAC(m); c != "" {
			dismissed[c] = true
		}
	}
	for _, l := range in.Tables.Leases {
		if c, _ := store.CanonMAC(l.MAC); c != "" && dismissed[c] {
			continue
		}
		if l.MAC != "" && l.Status == "bound" && !macs[l.MAC] {
			addDev(ClientDevice{MAC: l.MAC, IP: l.IP, HostName: l.Host})
		}
	}
	sort.SliceStable(devs, func(i, j int) bool {
		if devs[i].Online != devs[j].Online {
			return devs[i].Online
		}
		return devs[i].MAC < devs[j].MAC
	})
	return append(out, devs...)
}

// CountClients totals a list.
func CountClients(all []Client) ClientCounts {
	var n ClientCounts
	for _, c := range all {
		if c.Kind == "flat" {
			n.Flats++
		} else {
			n.Unregistered++
		}
		if c.Online {
			n.Online++
		}
	}
	return n
}

// FilterClients applies the view's query: q (name, flat, code, IP, MAC, vendor,
// host name; case-insensitive substring), kind (all|flats|unregistered) and
// online ("" = any, "true", "false").
func FilterClients(all []Client, q, kind, online string) []Client {
	q = strings.ToLower(strings.TrimSpace(q))
	hexQ := ""
	if q != "" && macQueryRe(q) {
		hexQ = strings.NewReplacer(":", "", "-", "", ".", "", " ", "").Replace(q)
		if len(hexQ) < 2 {
			hexQ = ""
		}
	}
	out := make([]Client, 0, len(all))
	for _, c := range all {
		switch kind {
		case "flats":
			if c.Kind != "flat" {
				continue
			}
		case "unregistered":
			if c.Kind != "unregistered" {
				continue
			}
		}
		switch online {
		case "true":
			if !c.Online {
				continue
			}
		case "false":
			if c.Online {
				continue
			}
		}
		if q != "" && !clientMatches(c, q, hexQ) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func macQueryRe(q string) bool {
	for _, r := range q {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == ':' || r == '-' || r == '.' || r == ' ') {
			return false
		}
	}
	return true
}

func clientMatches(c Client, q, hexQ string) bool {
	for _, f := range []string{c.Tenant, c.Flat, c.Code, c.IP, c.MAC, c.Vendor, c.HostName} {
		if f != "" && strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	if hexQ != "" && strings.Contains(strings.ToLower(strings.ReplaceAll(c.MAC, ":", "")), hexQ) {
		return true
	}
	return false
}

// fmtRate renders bit/s the way RouterOS people write it: 20M, 512k, 0 = none.
func fmtRate(bps int64) string {
	switch {
	case bps <= 0:
		return "0"
	case bps%1_000_000 == 0:
		return fmt.Sprintf("%dM", bps/1_000_000)
	case bps >= 1_000_000:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", float64(bps)/1e6), "0"), ".") + "M"
	case bps%1000 == 0:
		return fmt.Sprintf("%dk", bps/1000)
	}
	return strconv.FormatInt(bps, 10)
}

// naturalLess orders "Flat 2" before "Flat 10".
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			si := i
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			sj := j
			for j < len(b) && isDigit(b[j]) {
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

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
