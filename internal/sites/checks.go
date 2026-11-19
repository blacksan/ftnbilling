package sites

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/netplan"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Setup checks: one read pass over the building's router (REST, read only),
// evaluated into plain-words items the web admin and the app show as a
// checklist. The matching writes live in fixes.go.

// Check statuses.
const (
	StatusOK      = "ok"
	StatusWarn    = "warn"
	StatusFail    = "fail"
	StatusUnknown = "unknown"
	StatusInfo    = "info"
)

// Fix kinds.
const (
	FixAuto    = "auto"    // one button, safe, the server re-checks before writing
	FixConfirm = "confirm" // needs confirm=true (and an owner where Role says so)
	FixManual  = "manual"  // somebody must run Command on the router
	FixNone    = "none"
)

// Check ids.
const (
	CheckVersion      = "router.version"
	CheckAPIPolicy    = "api_user.policy"
	CheckDMScheduler  = "device_mode.scheduler"
	CheckDMFetch      = "device_mode.fetch"
	CheckDMHotspot    = "device_mode.hotspot"
	CheckKit          = "kit.installed"
	CheckDNS          = "dns.remote_requests"
	CheckCustDNS      = "customer.dns_input"
	CheckHooks        = "router.script_hooks"
	CheckCustomerIf   = "customer.interface"
	CheckOLT          = "olt.bridge_conflict"
	CheckBench        = "bench.port"
	CheckOrphans      = "hotspot.orphan_users"
	CheckTrafficFlow  = "traffic_flow"
	CheckWWW          = "service.www"
	CheckContainers   = "containers"
	CheckUnmanaged    = "unmanaged.devices"
	CheckUnseen       = "flats.unseen"
	CheckIPv6         = "ipv6"
	CheckPublicIP     = "server.public_ip"
	kitStamp          = "ftn: kit v1"
	kitTimerName      = "ftn-kit-timer"
	kitTickName       = "ftn-kit-tick"
	kitRollbackName   = "ftn-kit-rollback"
	netflowComment    = "ftn: netflow"
	checksCacheFor    = 15 * time.Second
	extraReadTimeout  = 12 * time.Second
	recheckMax        = 6                // ... this many times in a row
	kitAdvanceAfter   = 45 * time.Second // the kit timer ticks every 30 s
	requiredAPIPolicy = "read,write,test,sensitive,api,rest-api,reboot"
	custDNSComment    = "ftn: customer dns"
)

// The read budget of one pass. The tunnel to a building can take a quarter of a
// second per request, so the pass reads a few paths at a time (not thirty at
// once, which makes every read slow), gives each read time, and tries a read
// that timed out once more. Variables so a test can shorten them.
var (
	recheckAfter    = 20 * time.Second // a pass with timed-out reads is repeated by the server
	readTimeout     = 10 * time.Second
	readConcurrency = 4
)

// tableProps names the only fields the checks and the job read of a big table,
// so the router sends less over a slow tunnel (a .proplist query).
var tableProps = map[string]string{
	"/interface/bridge/host": "mac-address,on-interface,local,bridge",
}

func tableQuery(path string) map[string]string {
	if p, ok := tableProps[path]; ok {
		return map[string]string{".proplist": p}
	}
	return nil
}

// isTimeout says whether err is a read that ran out of time (not a refusal).
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// CheckFix says how an item can be fixed.
type CheckFix struct {
	Kind        string `json:"kind"`
	Label       string `json:"label,omitempty"`
	ConfirmText string `json:"confirm_text,omitempty"`
	Command     string `json:"command,omitempty"`
	Physical    bool   `json:"physical,omitempty"`
	Role        string `json:"role,omitempty"` // operator | owner
}

// CheckItem is one line of the checklist.
type CheckItem struct {
	ID            string   `json:"id"`
	Group         string   `json:"group"` // router | network | devices | services
	Status        string   `json:"status"`
	BlocksCutover bool     `json:"blocks_cutover"`
	Title         string   `json:"title"`
	Detail        string   `json:"detail"`
	Evidence      []string `json:"evidence"`
	Fix           CheckFix `json:"fix"`
	// Override names the cutover option that lets the operator go on in spite
	// of this item ("keep_unknown", "force"); "" if none.
	Override string `json:"override,omitempty"`
}

// RouterInfo is what the pass learned about the router itself.
type RouterInfo struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version"`
	Board     string `json:"board"`
}

// CheckSummary counts the items by status.
type CheckSummary struct {
	OK           int  `json:"ok"`
	Warn         int  `json:"warn"`
	Fail         int  `json:"fail"`
	Unknown      int  `json:"unknown"`
	CutoverReady bool `json:"cutover_ready"`
}

// Checks is the answer of GET /sites/{id}/checks.
type Checks struct {
	SiteID      string       `json:"site_id"`
	GeneratedAt time.Time    `json:"generated_at"`
	Router      RouterInfo   `json:"router"`
	Summary     CheckSummary `json:"summary"`
	JoinReport  *JoinReport  `json:"join_report"`
	Items       []CheckItem  `json:"items"`
	// Slow is true when some read of the router timed out: the server repeats the pass by itself.
	Slow bool `json:"slow,omitempty"`
}

// Item returns the item with the id.
func (c Checks) Item(id string) (CheckItem, bool) {
	for _, it := range c.Items {
		if it.ID == id {
			return it, true
		}
	}
	return CheckItem{}, false
}

// Blockers lists the ids of the items that stop a cutover (status fail or
// unknown, blocks_cutover), leaving out the ones the options override:
// keepUnknown covers unmanaged.devices, force covers flats.unseen.
func (c Checks) Blockers(keepUnknown, force bool) []string {
	var out []string
	for _, it := range c.Items {
		if !it.BlocksCutover || (it.Status != StatusFail && it.Status != StatusUnknown) {
			continue
		}
		if (it.Override == "keep_unknown" && keepUnknown) || (it.Override == "force" && force) {
			continue
		}
		out = append(out, it.ID)
	}
	return out
}

// ErrRouterUnreachable is returned when a fix cannot even read the router.
var ErrRouterUnreachable = errors.New("the router does not answer: check the tunnel and try again")

// ---- state -------------------------------------------------------------------

type kitSample struct {
	count int
	at    time.Time
}

type cachedChecks struct {
	at time.Time
	c  Checks
}

// checksState is the Manager's cache of the last pass of each building and
// the last kit run-count seen (to tell a dead timer from a live one).
type checksState struct {
	mu      sync.Mutex
	cache   map[string]cachedChecks
	kit     map[string]kitSample
	pending map[string]bool // automatic re-checks waiting
}

func (cs *checksState) get(siteID string, now time.Time) (Checks, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if e, ok := cs.cache[siteID]; ok && now.Sub(e.at) < checksCacheFor && !now.Before(e.at) {
		return e.c, true
	}
	return Checks{}, false
}

func (cs *checksState) put(siteID string, now time.Time, c Checks) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.cache == nil {
		cs.cache = map[string]cachedChecks{}
	}
	cs.cache[siteID] = cachedChecks{at: now, c: c}
}

func (cs *checksState) drop(siteID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.cache, siteID)
}

// kitMoving records the kit timer's run-count and says whether it is known
// to be stuck: the same count at least kitAdvanceAfter after the last sample.
func (cs *checksState) kitMoving(siteID string, count int, now time.Time) (stuck bool, sample kitSample, had bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.kit == nil {
		cs.kit = map[string]kitSample{}
	}
	prev, had := cs.kit[siteID]
	switch {
	case !had || count != prev.count:
		cs.kit[siteID] = kitSample{count, now}
	case now.Sub(prev.at) >= kitAdvanceAfter:
		stuck = true
	}
	return stuck, prev, had
}

// ---- reading -----------------------------------------------------------------

// The singleton paths of the pass (everything else is a table).
var checkSingles = []string{"/system/resource", "/system/device-mode", "/ip/dns", "/ip/traffic-flow", "/ipv6/settings"}

var checkTables = []string{
	"/ip/address", "/interface", "/interface/bridge", "/interface/bridge/port", "/interface/bridge/host", "/interface/vlan",
	"/interface/list/member", "/ip/hotspot", "/ip/hotspot/user", "/ip/hotspot/ip-binding", "/ip/dhcp-server",
	"/ip/dhcp-server/lease", "/ip/dhcp-client", "/ip/arp", "/ip/pool", "/ip/traffic-flow/target", "/ip/service",
	"/container", "/user/group", "/system/scheduler", "/system/script", "/file", "/ip/firewall/filter",
	"/ipv6/firewall/filter", "/ip/route", "/interface/pppoe-client", "/ip/firewall/raw",
	// read-only, for router.script_hooks
	"/ppp/profile", "/ip/hotspot/user/profile", "/tool/netwatch",
}

// rowReader is all a read pass needs from the router client (singletons come
// from its optional routeros.Inspector).
type rowReader interface {
	Rows(ctx context.Context, path string, query map[string]string) ([]routeros.Row, error)
}

// snapshot is one read pass: singletons, tables and the error of each path
// that could not be read.
type snapshot struct {
	one  map[string]routeros.Row
	rows map[string][]routeros.Row
	errs map[string]error
	slow map[string]bool // paths whose read timed out (after the retry)
}

func newSnapshot() *snapshot {
	return &snapshot{one: map[string]routeros.Row{}, rows: map[string][]routeros.Row{}, errs: map[string]error{}, slow: map[string]bool{}}
}

func (sn *snapshot) tbl(p string) []routeros.Row { return sn.rows[p] }
func (sn *snapshot) single(p string) routeros.Row {
	if r := sn.one[p]; r != nil {
		return r
	}
	return routeros.Row{}
}

// failed returns the first of paths that could not be read.
func (sn *snapshot) failed(paths ...string) (string, error) {
	for _, p := range paths {
		if err := sn.errs[p]; err != nil {
			return p, err
		}
	}
	return "", nil
}

// readSnapshot reads the given singletons and tables, a few at a time, each
// with its own short timeout.
func readSnapshot(ctx context.Context, rt rowReader, singles, tables []string) *snapshot {
	sn := newSnapshot()
	insp, _ := rt.(routeros.Inspector)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, readConcurrency)
	run := func(path string, fn func(context.Context) error) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var err error
			for try := 0; try < 2; try++ {
				c, cancel := context.WithTimeout(ctx, readTimeout)
				err = fn(c)
				cancel()
				if err == nil || !isTimeout(err) || ctx.Err() != nil {
					break
				}
			}
			if err != nil {
				mu.Lock()
				sn.errs[path] = err
				if isTimeout(err) {
					sn.slow[path] = true
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range singles {
		run(p, func(c context.Context) error {
			if insp == nil {
				return routeros.ErrNoInspector
			}
			r, err := insp.One(c, p)
			if err != nil {
				return err
			}
			mu.Lock()
			sn.one[p] = r
			mu.Unlock()
			return nil
		})
	}
	for _, p := range tables {
		run(p, func(c context.Context) error {
			rows, err := rt.Rows(c, p, tableQuery(p))
			if err != nil {
				return err
			}
			mu.Lock()
			sn.rows[p] = rows
			mu.Unlock()
			return nil
		})
	}
	wg.Wait()
	return sn
}

// ---- value helpers --------------------------------------------------------------

// RouterOS REST answers every value as a string, "true"/"false" or "yes"/"no".
func yes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on":
		return true
	}
	return false
}

func no(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "no", "off":
		return true
	}
	return false
}

func rowDisabled(r routeros.Row) bool { return yes(r["disabled"]) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hasWord(list, w string) bool {
	for _, p := range splitList(list) {
		if p == w {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// parseVersion reads "7.25.1 (stable)" and "7.13rc2".
func parseVersion(v string) (major, minor int, ok bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, true
}

func printCmd(path string) string { return path + "/print" }

// ---- evaluation ----------------------------------------------------------------

// acctIndex is what the checks need to know of the accounts table.
type acctIndex struct {
	macs  map[string]bool // canonical MACs of every account
	codes map[string]bool
}

func (m *Manager) accountIndex() acctIndex {
	ix := acctIndex{macs: map[string]bool{}, codes: map[string]bool{}}
	m.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if mac, ok := store.CanonMAC(a.MAC); ok {
				ix.macs[mac] = true
			}
			ix.codes[a.Code] = true
		}
	})
	return ix
}

// custInfo is the customer-facing interface the router really has.
type custInfo struct {
	iface    string
	addr     string
	found    bool
	hotspots []string
	dhcps    []string
	isBridge bool
}

type evalCtx struct {
	m       *Manager
	site    store.Site // effective
	sn      *snapshot
	plan    netplan.Plan
	planErr error
	slow    bool // some read failed: the server will look again
	cust    custInfo
	ix      acctIndex
	now     time.Time

	unmanaged, unseen       []string
	unmanagedErr, unseenErr error
}

func detectCustomer(sn *snapshot, plan netplan.Plan) custInfo {
	var c custInfo
	for _, r := range sn.tbl("/ip/address") {
		if rowDisabled(r) || yes(r["invalid"]) {
			continue
		}
		if r["address"] == plan.GatewayIP {
			c.iface, c.addr, c.found = r["interface"], r["address"], true
			break
		}
	}
	if !c.found {
		return c
	}
	for _, r := range sn.tbl("/ip/hotspot") {
		if r["interface"] == c.iface {
			n := r["name"]
			if rowDisabled(r) {
				n += " (disabled)"
			}
			c.hotspots = append(c.hotspots, n)
		}
	}
	for _, r := range sn.tbl("/ip/dhcp-server") {
		if r["interface"] == c.iface {
			n := r["name"]
			if rowDisabled(r) {
				n += " (disabled)"
			}
			c.dhcps = append(c.dhcps, n)
		}
	}
	for _, r := range sn.tbl("/interface/bridge") {
		if r["name"] == c.iface {
			c.isBridge = true
		}
	}
	return c
}

func (e *evalCtx) hotspotMode() bool { return !billing.IPoE(e.site) }

// readPaths each check needs; a failed read makes the item unknown.
var needs = map[string][]string{
	CheckVersion:     {"/system/resource"},
	CheckAPIPolicy:   {"/user/group"},
	CheckDMScheduler: {"/system/device-mode"},
	CheckDMFetch:     {"/system/device-mode"},
	CheckDMHotspot:   {"/system/device-mode"},
	CheckKit:         {"/system/script", "/system/scheduler"},
	CheckDNS:         {"/ip/dns"},
	CheckCustDNS:     {"/ip/firewall/filter", "/ip/address", "/ip/hotspot", "/ip/dhcp-server"},
	CheckHooks:       {"/ip/dhcp-server", "/ip/dhcp-client", "/ppp/profile", "/ip/hotspot/user/profile", "/tool/netwatch"},
	CheckCustomerIf:  {"/ip/address", "/ip/hotspot", "/ip/dhcp-server"},
	CheckOLT:         {"/interface/bridge/port", "/interface/vlan"},
	CheckBench:       {"/interface/bridge/port", "/interface", "/ip/address", "/ip/hotspot", "/ip/dhcp-server"},
	CheckOrphans:     {"/ip/hotspot/user", "/ip/hotspot/ip-binding"},
	CheckTrafficFlow: {"/ip/traffic-flow", "/ip/traffic-flow/target", "/ip/address", "/ip/hotspot", "/ip/dhcp-server"},
	CheckWWW:         {"/ip/service"},
	CheckContainers:  {"/container"},
	CheckIPv6:        {"/ipv6/settings"},
}

type checkDef struct {
	id, group, title string
	blocks           bool
	override         string
	eval             func(e *evalCtx, it *CheckItem)
}

// checkDefs is the checklist, in display order.
var checkDefs = []checkDef{
	{CheckVersion, "router", "Router software version", false, "", evalVersion},
	{CheckAPIPolicy, "router", "Server login has the rights it needs", false, "", evalAPIPolicy},
	{CheckDMScheduler, "router", "Router allows scheduled scripts", true, "", evalDMScheduler},
	{CheckDMFetch, "router", "Router allows file downloads", false, "", evalDMFetch},
	{CheckDMHotspot, "router", "Router allows the hotspot", false, "", evalDMHotspot},
	{CheckKit, "router", "Safety kit is installed on the router", true, "", evalKit},
	{CheckDNS, "services", "Customers can use the router for DNS", true, "", evalDNS},
	{CheckCustDNS, "services", "Router accepts DNS from the customer network", true, "", evalCustDNS},
	{CheckHooks, "router", "Scripts that run by themselves on the router", false, "", evalHooks},
	{CheckCustomerIf, "network", "Customer network is on one interface", true, "", evalCustomerIf},
	{CheckOLT, "network", "Fibre (OLT) port is not stuck in the bridge", true, "", evalOLT},
	{CheckBench, "network", "Bench test port", false, "", evalBench},
	{CheckOrphans, "devices", "Leftover hotspot logins", false, "", evalOrphans},
	{CheckTrafficFlow, "network", "Traffic reporting to the server", false, "", evalTrafficFlow},
	{CheckWWW, "services", "Router web page (WebFig)", false, "", evalWWW},
	{CheckContainers, "services", "Containers on the router", false, "", evalContainers},
	{CheckUnmanaged, "devices", "Devices the server does not know", true, "keep_unknown", evalUnmanaged},
	{CheckUnseen, "devices", "Paid flats not seen on the router", true, "force", evalUnseen},
	{CheckIPv6, "network", "IPv6", false, "", evalIPv6},
	{CheckPublicIP, "services", "Server public address is set", true, "", evalPublicIP},
}

func okItem(it *CheckItem, detail string, ev ...string) {
	it.Status, it.Detail = StatusOK, detail
	it.Evidence = append(it.Evidence, ev...)
}

func (e *evalCtx) unknown(it *CheckItem, id string) bool {
	paths := needs[id]
	p, err := e.sn.failed(paths...)
	if err == nil {
		return false
	}
	it.Status = StatusUnknown
	if isTimeout(err) {
		// Nothing for the operator to run: the server asks again by itself.
		it.Detail = unreadDetail("", err)
		it.Fix = CheckFix{Kind: FixNone}
		e.slow = true
		return true
	}
	it.Detail = fmt.Sprintf("The server could not read %s from the router (%s), so this cannot be judged yet. The server keeps trying.", p, shortErr(err))
	it.Fix = CheckFix{Kind: FixNone}
	e.slow = true
	return true
}

// unreadDetail is the text of an item the server could not judge for lack of an answer.
func unreadDetail(what string, err error) string {
	if isTimeout(err) {
		return "The router was slow to answer; checking again automatically."
	}
	return "The server could not " + what + " (" + shortErr(err) + "). It keeps trying."
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// buildChecks evaluates every check on a snapshot.
func (e *evalCtx) buildChecks() []CheckItem {
	items := make([]CheckItem, 0, len(checkDefs))
	for _, d := range checkDefs {
		it := CheckItem{ID: d.id, Group: d.group, Title: d.title, BlocksCutover: d.blocks, Override: d.override,
			Evidence: []string{}, Fix: CheckFix{Kind: FixNone}}
		if !e.unknown(&it, d.id) {
			d.eval(e, &it)
		}
		if it.Evidence == nil {
			it.Evidence = []string{}
		}
		items = append(items, it)
	}
	return items
}

func evalVersion(e *evalCtx, it *CheckItem) {
	v := e.sn.single("/system/resource")["version"]
	major, minor, ok := parseVersion(v)
	it.Evidence = append(it.Evidence, "version: "+v)
	switch {
	case !ok:
		it.Status = StatusUnknown
		it.Detail = "The router did not say which RouterOS version it runs."
		it.Fix = CheckFix{Kind: FixManual, Label: "Look at the version", Command: "/system/resource/print"}
	case major < 7 || (major == 7 && minor < 13):
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("The router runs RouterOS %s. Version 7.13 or newer is needed.", v)
		it.Fix = CheckFix{Kind: FixManual, Label: "Upgrade RouterOS", Command: "/system/package/update/check-for-updates"}
	default:
		okItem(it, fmt.Sprintf("RouterOS %s is new enough (7.13 or newer is needed).", v))
	}
}

func evalAPIPolicy(e *evalCtx, it *CheckItem) {
	name := e.site.RESTUser
	if name == "" {
		name = DefaultUser
	}
	var grp routeros.Row
	for _, r := range e.sn.tbl("/user/group") {
		if r["name"] == name {
			grp = r
		}
	}
	cmd := "/user/group/set " + name + " policy=" + requiredAPIPolicy
	if grp == nil {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("The router has no user group called %s, so the server's login may be missing rights.", name)
		it.Fix = CheckFix{Kind: FixManual, Label: "Create the group", Command: cmd}
		return
	}
	pol := grp["policy"]
	it.Evidence = append(it.Evidence, "policy: "+pol)
	var missing []string
	for _, w := range splitList(requiredAPIPolicy) {
		if !hasWord(pol, w) {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("The server's group %s lacks: %s. Some checks and fixes will not work.", name, strings.Join(missing, ", "))
		it.Fix = CheckFix{Kind: FixManual, Label: "Give the group its rights", Command: cmd}
		return
	}
	var extra []string
	for _, w := range extraAPIRights {
		if hasWord(pol, w) {
			extra = append(extra, w)
		}
	}
	if len(extra) > 0 {
		it.Status = StatusWarn
		it.Detail = fmt.Sprintf("The server's group %s has more rights than it needs (%s). If the server were broken into, those rights would reach the router itself.", name, strings.Join(extra, ", "))
		it.Fix = CheckFix{Kind: FixManual, Label: "Take the extra rights away", Command: cmd}
		return
	}
	okItem(it, fmt.Sprintf("Group %s has exactly the rights the server needs.", name))
}

// extraAPIRights are the rights the server's API group must not have.
var extraAPIRights = []string{"policy", "ftp", "password", "sniff", "romon", "winbox", "ssh", "telnet", "web", "local"}

func dmField(e *evalCtx, it *CheckItem, key string) (string, bool) {
	dm := e.sn.single("/system/device-mode")
	v, ok := dm[key]
	if !ok {
		it.Status = StatusUnknown
		it.Detail = "The router's device-mode answer has no " + key + " setting."
		it.Fix = CheckFix{Kind: FixManual, Label: "Look at device-mode", Command: "/system/device-mode/print"}
		return "", false
	}
	it.Evidence = append(it.Evidence, fmt.Sprintf("device-mode %s: %s", key, v))
	return v, true
}

const physicalNote = " This needs the router's button (or a power cut) within 5 minutes of the command, and the router restarts."

func evalDMScheduler(e *evalCtx, it *CheckItem) {
	v, ok := dmField(e, it, "scheduler")
	if !ok {
		return
	}
	flagged := yes(e.sn.single("/system/device-mode")["flagged"])
	switch {
	case flagged:
		it.Status = StatusFail
		it.Detail = "The router flagged its device-mode (it was changed without confirmation). The scheduler may not run until it is confirmed." + physicalNote
		it.Fix = CheckFix{Kind: FixManual, Label: "Allow the scheduler", Command: "/system/device-mode/update scheduler=yes", Physical: true, Role: store.RoleOperator}
	case !yes(v):
		it.Status = StatusFail
		it.Detail = "The router does not allow scheduled scripts, so the cutover safety timer cannot run." + physicalNote
		it.Fix = CheckFix{Kind: FixManual, Label: "Allow the scheduler", Command: "/system/device-mode/update scheduler=yes", Physical: true, Role: store.RoleOperator}
	default:
		okItem(it, "The router allows scheduled scripts.")
	}
}

func evalDMFetch(e *evalCtx, it *CheckItem) {
	v, ok := dmField(e, it, "fetch")
	if !ok {
		return
	}
	if !yes(v) {
		it.Status = StatusWarn
		it.Detail = "The router does not allow file downloads, so the join command cannot fetch its script." + physicalNote
		it.Fix = CheckFix{Kind: FixManual, Label: "Allow downloads", Command: "/system/device-mode/update fetch=yes", Physical: true, Role: store.RoleOperator}
		return
	}
	okItem(it, "The router allows file downloads.")
}

func evalDMHotspot(e *evalCtx, it *CheckItem) {
	v, ok := dmField(e, it, "hotspot")
	if !ok {
		return
	}
	switch {
	case yes(v):
		okItem(it, "The router allows the hotspot.")
	case e.hotspotMode():
		it.Status = StatusFail
		it.Detail = "This building runs in hotspot mode but the router does not allow the hotspot." + physicalNote
		it.Fix = CheckFix{Kind: FixManual, Label: "Allow the hotspot", Command: "/system/device-mode/update hotspot=yes", Physical: true, Role: store.RoleOperator}
	default:
		it.Status = StatusInfo
		it.Detail = "The hotspot is not allowed on this router. That is fine: this building is in IPoE mode and does not use it."
	}
}

func evalKit(e *evalCtx, it *CheckItem) {
	fix := CheckFix{Kind: FixManual, Label: "Paste the join command again", Command: "", Role: store.RoleOwner}
	byName := func(rows []routeros.Row, n string) routeros.Row {
		for _, r := range rows {
			if r["name"] == n {
				return r
			}
		}
		return nil
	}
	scripts, sched := e.sn.tbl("/system/script"), e.sn.tbl("/system/scheduler")
	var miss []string
	for _, n := range []string{kitTickName, kitRollbackName} {
		if byName(scripts, n) == nil {
			miss = append(miss, "script "+n)
		}
	}
	timer := byName(sched, kitTimerName)
	if timer == nil {
		miss = append(miss, "scheduler "+kitTimerName)
	}
	if len(miss) > 0 {
		it.Status = StatusFail
		it.Detail = "The safety kit is not on the router (missing " + strings.Join(miss, ", ") + "). Paste the building's join command again to install it."
		it.Fix = fix
		return
	}
	it.Evidence = append(it.Evidence, "scheduler "+kitTimerName+" found", fmt.Sprintf("run-count: %s", timer["run-count"]))
	// The kit runs with the join admin's rights for ever: any drift from the
	// text the server ships (source, policy, owner, the timer's target) means
	// somebody changed what runs with those rights.
	if drift := kitDrift(e, byName(scripts, kitTickName), byName(scripts, kitRollbackName), timer); len(drift) > 0 {
		it.Status = StatusFail
		it.Evidence = append(it.Evidence, drift...)
		it.Detail = "The safety kit on the router is not what the server installed (" + strings.Join(drift, "; ") + "). It runs with full rights, so paste the join command again to put the right one back."
		it.Fix = fix
		return
	}
	if rowDisabled(timer) {
		it.Status = StatusFail
		it.Detail = "The safety kit's timer is switched off on the router. Paste the join command again to repair it."
		it.Fix = fix
		return
	}
	if !strings.Contains(timer["comment"], kitStamp) {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("The safety kit on the router is an old or unknown version (expected %q). Paste the join command again to update it.", kitStamp)
		it.Evidence = append(it.Evidence, "comment: "+timer["comment"])
		it.Fix = fix
		return
	}
	if n, err := strconv.Atoi(strings.TrimSpace(timer["run-count"])); err == nil {
		stuck, prev, had := e.m.chk.kitMoving(e.site.ID, n, e.now)
		if stuck {
			it.Status = StatusFail
			it.Detail = fmt.Sprintf("The safety kit timer has not run for %d seconds, so the router would not roll back by itself. Paste the join command again.", int(e.now.Sub(prev.at).Seconds()))
			it.Fix = fix
			return
		}
		if !had {
			okItem(it, "The safety kit is installed (its timer will be checked again on the next look).")
			return
		}
	}
	okItem(it, "The safety kit is installed and its timer is running.")
}

// normSource makes two copies of a script comparable (line ends, trailing space).
func normSource(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// kitDrift lists how the installed kit differs from the one the server ships.
func kitDrift(e *evalCtx, tick, roll, timer routeros.Row) []string {
	var out []string
	api := e.site.RESTUser
	if api == "" {
		api = DefaultUser
	}
	chk := func(what string, r routeros.Row, wantSrc, wantPolicy string) {
		if wantSrc != "" && normSource(r["source"]) != normSource(wantSrc) {
			out = append(out, what+" source was changed")
		}
		if !sameSet(splitList(r["policy"]), splitList(wantPolicy)) {
			out = append(out, fmt.Sprintf("%s policy is %q, expected %q", what, r["policy"], wantPolicy))
		}
		if o := r["owner"]; o != "" && o == api {
			out = append(out, what+" is owned by the server's API user")
		}
	}
	chk("script "+kitTickName, tick, kitTickSource(), kitTickPolicy)
	chk("script "+kitRollbackName, roll, kitRollbackSource(e.site.HotspotServer), kitScriptPolic)
	chk("scheduler "+kitTimerName, timer, "", kitTickPolicy)
	if timer["on-event"] != kitTickName {
		out = append(out, fmt.Sprintf("the timer runs %q instead of %s", timer["on-event"], kitTickName))
	}
	return out
}

// ---- scripts that run by themselves ---------------------------------------------------

// hookTables are the places a RouterOS script can be hung so that it runs
// with full rights when something happens.
var hookTables = []struct {
	path, what string
	props      []string
}{
	{"/ip/dhcp-server", "DHCP server", []string{"lease-script"}},
	{"/ip/dhcp-client", "DHCP client", []string{"script"}},
	{"/ppp/profile", "PPP profile", []string{"on-up", "on-down"}},
	{"/ip/hotspot/user/profile", "hotspot user profile", []string{"on-login", "on-logout"}},
	{"/tool/netwatch", "netwatch entry", []string{"up-script", "down-script"}},
}

func evalHooks(e *evalCtx, it *CheckItem) {
	for _, h := range hookTables {
		for _, r := range e.sn.tbl(h.path) {
			for _, p := range h.props {
				if v := strings.TrimSpace(r[p]); v != "" {
					if len(v) > 80 {
						v = v[:80] + "..."
					}
					it.Evidence = append(it.Evidence, fmt.Sprintf("%s %s %s: %s", h.what, firstOf(r["name"], r["interface"], r["host"], r[routeros.RowID]), p, v))
				}
			}
		}
	}
	if len(it.Evidence) == 0 {
		okItem(it, "No scripts are hung on DHCP, PPP, hotspot or netwatch events.")
		return
	}
	it.Status = StatusWarn
	it.Detail = fmt.Sprintf("%d script hook(s) on the router run by themselves with full rights. The server never creates them; look at each and remove any you do not recognise (they are a way to turn a limited login into a full one).", len(it.Evidence))
	it.Fix = CheckFix{Kind: FixNone, Label: "Review them on the router"}
}

// ---- DNS -----------------------------------------------------------------------

// ---- firewall reading ----------------------------------------------------------

type tri int

const (
	triNo tri = iota
	triYes
	triMaybe
)

// listMembers maps list name to its (enabled) member interfaces, from the
// /interface/list/member table of the snapshot.
func listMembers(sn *snapshot) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, r := range sn.tbl("/interface/list/member") {
		if rowDisabled(r) {
			continue
		}
		if out[r["list"]] == nil {
			out[r["list"]] = map[string]bool{}
		}
		out[r["list"]][r["interface"]] = true
	}
	return out
}

// matchIface says whether a firewall rule's in-interface / in-interface-list
// matches packets arriving on iface. A list that is not in the member table
// (other than the built-in ones) is a maybe.
func matchIface(lists map[string]map[string]bool, r routeros.Row, iface string) tri {
	if v := r["in-interface"]; v != "" {
		neg := strings.HasPrefix(v, "!")
		m := strings.TrimPrefix(v, "!") == iface
		if neg {
			m = !m
		}
		if !m {
			return triNo
		}
	}
	if v := r["in-interface-list"]; v != "" {
		neg := strings.HasPrefix(v, "!")
		n := strings.TrimPrefix(v, "!")
		var m bool
		switch members, known := lists[n]; {
		case n == "all":
			m = true
		case n == "none":
			m = false
		case known:
			m = members[iface]
		case n == "static" || n == "dynamic":
			return triMaybe
		default:
			m = false // a list with no members matches nothing; a negated one matches everything
		}
		if neg {
			m = !m
		}
		if !m {
			return triNo
		}
	}
	return triYes
}

// protoMatch: does the rule's protocol match proto (udp/tcp)? A negated
// protocol ("!udp") matches the other one.
func protoMatch(r routeros.Row, proto string) tri {
	p := r["protocol"]
	switch {
	case p == "":
		return triYes
	case strings.HasPrefix(p, "!"):
		if strings.TrimPrefix(p, "!") == proto {
			return triNo
		}
		return triMaybe
	case p == proto:
		return triYes
	}
	return triNo
}

// portMatch: does the rule's dst-port include 53?
func portMatch(r routeros.Row) tri {
	dp := r["dst-port"]
	switch {
	case dp == "":
		return triYes
	case strings.HasPrefix(dp, "!"):
		return triMaybe
	case portListHas(dp, 53):
		return triYes
	}
	return triNo
}

// stateMatch says whether the rule can match a new packet, and an untracked one.
func stateMatch(r routeros.Row) (isNew, untracked bool) {
	cs := r["connection-state"]
	if cs == "" {
		return true, true
	}
	if strings.HasPrefix(cs, "!") {
		return !strings.Contains(cs, "new"), !strings.Contains(cs, "untracked")
	}
	return strings.Contains(cs, "new"), strings.Contains(cs, "untracked")
}

// inputRules returns the enabled input rules in order.
func inputRules(rows []routeros.Row) []routeros.Row {
	var out []routeros.Row
	for _, r := range rows {
		if r["chain"] == "input" && !rowDisabled(r) {
			out = append(out, r)
		}
	}
	return out
}

// dnsDropsWAN reports whether the input chain provably drops DNS (port 53,
// UDP and TCP) arriving from every WAN interface before any rule could accept
// it. It is conservative: a rule it does not fully understand never counts as
// a drop, and an accept only gets skipped when it is bound to a LAN-list
// member or one of the safe (customer) interfaces; any other accept is a hole.
func dnsDropsWAN(sn *snapshot, rules []routeros.Row, site store.Site, safe map[string]bool, v6 bool) (bool, string) {
	wan := wanIfaces(sn, site)
	if len(wan) == 0 {
		return false, "no WAN interface is known"
	}
	lists := listMembers(sn)
	for w := range wan {
		if lists["LAN"][w] {
			return false, w + " is both a WAN and a LAN interface"
		}
	}
	rules = inputRules(rules)
	if len(rules) == 0 {
		return false, "the router has no input firewall rules"
	}
	for _, proto := range []string{"udp", "tcp"} {
		decided := false
	rule:
		for i, r := range rules {
			pm, port := protoMatch(r, proto), portMatch(r)
			if pm == triNo || port == triNo {
				continue
			}
			isNew, untr := stateMatch(r)
			if !isNew && !untr {
				continue
			}
			switch r["action"] {
			case "accept":
				if r["dst-address"] == "127.0.0.1" {
					continue // loopback, not reachable from outside
				}
				if v := r["in-interface"]; v != "" && !strings.HasPrefix(v, "!") && (lists["LAN"][v] || safe[v]) {
					continue
				}
				if r["in-interface-list"] == "LAN" {
					continue
				}
				if !isNew && untr && noNotrack(sn, v6) {
					continue // only untracked packets, and nothing on this router creates any
				}
				return false, fmt.Sprintf("input rule %d accepts %s traffic that could come from the WAN", i, proto)
			case "drop", "reject":
				if pm != triYes || port != triYes || !(isNew && untr) || !knownMatchers(r) {
					continue // cannot count on it
				}
				covers := true
				for w := range wan {
					if matchIface(lists, r, w) != triYes {
						covers = false
					}
				}
				if covers {
					decided = true
					break rule
				}
			case "jump", "fasttrack-connection":
				return false, fmt.Sprintf("input rule %d (%s) is too complex to judge", i, r["action"])
			}
		}
		if !decided {
			return false, "no input rule drops " + proto + " DNS from the WAN"
		}
	}
	return true, ""
}

// noNotrack is true when the raw table is readable and holds no enabled
// notrack rule, so no packet can be "untracked" and an accept that matches
// only untracked traffic is harmless. v6 reads the IPv6 raw table (not read:
// then it is false unless IPv6 is off, which dnsSafe handles).
func noNotrack(sn *snapshot, v6 bool) bool {
	if v6 {
		return false
	}
	if sn.errs["/ip/firewall/raw"] != nil {
		return false
	}
	for _, r := range sn.tbl("/ip/firewall/raw") {
		if !rowDisabled(r) && r["action"] == "notrack" {
			return false
		}
	}
	return true
}

// knownMatchers is false when the rule has a matcher beyond the ones
// dnsDropsWAN understands (a source restriction, for instance).
func knownMatchers(r routeros.Row) bool {
	for k, v := range r {
		if v == "" {
			continue
		}
		switch k {
		case "chain", "action", "protocol", "dst-port", "in-interface-list", "in-interface", "connection-state",
			"disabled", "comment", "dynamic", "invalid", "bytes", "packets", "log", "log-prefix", ".id", ".nextid", "place-before":
		default:
			return false
		}
	}
	return true
}

func portListHas(list string, port int) bool {
	for _, p := range splitList(list) {
		if lo, hi, ok := strings.Cut(p, "-"); ok {
			a, e1 := strconv.Atoi(lo)
			b, e2 := strconv.Atoi(hi)
			if e1 == nil && e2 == nil && a <= port && port <= b {
				return true
			}
		} else if n, err := strconv.Atoi(p); err == nil && n == port {
			return true
		}
	}
	return false
}

// uplinkInWAN checks the interfaces the router really reaches the internet
// through (default routes, DHCP clients, PPPoE clients) are all in the WAN
// list: a firewall that drops DNS from "the WAN" is worth nothing if the
// uplink is somewhere else.
func uplinkInWAN(sn *snapshot, site store.Site) (bool, string) {
	for _, p := range []string{"/ip/route", "/ip/dhcp-client"} {
		if err := sn.errs[p]; err != nil {
			return false, "could not read " + p
		}
	}
	wan := wanIfaces(sn, site)
	ups := map[string]string{}
	for _, r := range sn.tbl("/ip/route") {
		if rowDisabled(r) || r["dst-address"] != "0.0.0.0/0" {
			continue
		}
		var ifc string
		for _, g := range []string{r["immediate-gw"], r["gateway"]} {
			if _, after, ok := strings.Cut(g, "%"); ok {
				ifc = after
				break
			}
		}
		if ifc == "" {
			g := r["gateway"]
			for _, t := range []string{"/interface", "/interface/pppoe-client"} {
				for _, ir := range sn.tbl(t) {
					if ir["name"] == g && g != "" {
						ifc = g
					}
				}
			}
		}
		if ifc == "" {
			return false, "the default route (" + firstOf(r["gateway"], "?") + ") does not say which interface it uses"
		}
		ups[ifc] = "default route"
	}
	for _, r := range sn.tbl("/ip/dhcp-client") {
		if !rowDisabled(r) && r["interface"] != "" {
			ups[r["interface"]] = "DHCP client"
		}
	}
	for _, r := range sn.tbl("/interface/pppoe-client") {
		if rowDisabled(r) {
			continue
		}
		for _, n := range []string{r["name"], r["interface"]} {
			if n != "" {
				ups[n] = "PPPoE client"
			}
		}
	}
	if len(ups) == 0 {
		return false, "the router's uplink could not be found"
	}
	for ifc, how := range ups {
		if !wan[ifc] {
			return false, fmt.Sprintf("%s (%s) is not in the WAN list", ifc, how)
		}
	}
	return true, ""
}

// dnsSafe says whether turning on DNS for customers cannot open a resolver to
// the internet: the uplink is the WAN, IPv4 input drops DNS from it, and IPv6
// is off or drops it too.
func dnsSafe(sn *snapshot, site store.Site, safe map[string]bool) (bool, string) {
	if _, err := sn.failed("/ip/firewall/filter", "/interface/list/member"); err != nil {
		return false, "the router's firewall rules could not be read"
	}
	if ok, why := uplinkInWAN(sn, site); !ok {
		return false, why
	}
	if ok, why := dnsDropsWAN(sn, sn.tbl("/ip/firewall/filter"), site, safe, false); !ok {
		return false, why
	}
	if v6, has := sn.single("/ipv6/settings")["disable-ipv6"]; has && yes(v6) {
		return true, ""
	}
	if err := sn.errs["/ipv6/firewall/filter"]; err != nil || sn.errs["/ipv6/settings"] != nil {
		return false, "IPv6 is on and its firewall could not be read"
	}
	if ok, why := dnsDropsWAN(sn, sn.tbl("/ipv6/firewall/filter"), site, safe, true); !ok {
		return false, "IPv6 is on and " + why
	}
	return true, ""
}

const dnsFixCommand = `/ip/firewall/filter/add chain=input action=drop protocol=udp dst-port=53 in-interface-list=WAN place-before=0 comment="ftn: no WAN DNS"
/ip/firewall/filter/add chain=input action=drop protocol=tcp dst-port=53 in-interface-list=WAN place-before=0 comment="ftn: no WAN DNS"
/ipv6/settings/set disable-ipv6=yes
/ip/dns/set allow-remote-requests=yes`

func (c custInfo) safeSet() map[string]bool {
	if c.found {
		return map[string]bool{c.iface: true}
	}
	return nil
}

func evalDNS(e *evalCtx, it *CheckItem) {
	v := e.sn.single("/ip/dns")["allow-remote-requests"]
	it.Evidence = append(it.Evidence, "allow-remote-requests: "+v)
	if yes(v) {
		okItem(it, "The router answers DNS questions from customers.")
		return
	}
	it.Status = StatusFail
	it.Detail = "The router does not answer DNS for customers (allow-remote-requests is off), so customers would have no working names after the cutover."
	safe, why := dnsSafe(e.sn, e.site, e.cust.safeSet())
	if safe {
		it.Fix = CheckFix{Kind: FixAuto, Label: "Turn on DNS for customers", Role: store.RoleOperator}
		it.Evidence = append(it.Evidence, "the uplink is the WAN and the firewall already drops DNS from it")
		return
	}
	it.Detail += " It is not turned on for you because the router might then answer the whole internet (" + why + ")."
	it.Fix = CheckFix{Kind: FixManual, Label: "Block DNS from the WAN, then turn it on", Command: dnsFixCommand, Role: store.RoleOwner}
}

// custDNSProblems lists the protocols (udp, tcp) whose DNS from the customer
// interface the input chain does not accept before a drop, with the reason.
func custDNSProblems(sn *snapshot, cust custInfo) (bad []string, why string) {
	lists := listMembers(sn)
	rules := inputRules(sn.tbl("/ip/firewall/filter"))
	for _, proto := range []string{"udp", "tcp"} {
	rule:
		for i, r := range rules {
			pm, port := protoMatch(r, proto), portMatch(r)
			if pm == triNo || port == triNo {
				continue
			}
			if isNew, _ := stateMatch(r); !isNew {
				continue
			}
			im := matchIface(lists, r, cust.iface)
			if im == triNo {
				continue
			}
			switch r["action"] {
			case "accept":
				if im == triYes && pm == triYes && port == triYes && knownMatchers(r) {
					break rule // accepted first: good
				}
				bad, why = append(bad, proto), fmt.Sprintf("input rule %d may accept %s DNS from %s but with conditions the server cannot judge", i, proto, cust.iface)
				break rule
			case "drop", "reject":
				bad, why = append(bad, proto), fmt.Sprintf("input rule %d drops %s DNS from %s before anything accepts it", i, proto, cust.iface)
				break rule
			case "jump", "fasttrack-connection":
				bad, why = append(bad, proto), fmt.Sprintf("input rule %d (%s) is too complex to judge", i, r["action"])
				break rule
			}
		}
	}
	return bad, why
}

// firstStaticInput is the id of the first non-dynamic input rule.
func firstStaticInput(rows []routeros.Row) (string, bool) {
	for _, r := range rows {
		if r["chain"] == "input" && !yes(r["dynamic"]) && r[routeros.RowID] != "" {
			return r[routeros.RowID], true
		}
	}
	return "", false
}

func custDNSCommand(iface string) string {
	return fmt.Sprintf(`/ip/firewall/filter/add chain=input in-interface=%[1]s protocol=udp dst-port=53 action=accept place-before=0 comment=%[2]q
/ip/firewall/filter/add chain=input in-interface=%[1]s protocol=tcp dst-port=53 action=accept place-before=0 comment=%[2]q`, iface, custDNSComment)
}

func evalCustDNS(e *evalCtx, it *CheckItem) {
	if !e.cust.found {
		it.Status = StatusUnknown
		it.Detail = "The customer interface is not known yet, so the firewall cannot be judged. See the customer network check."
		return
	}
	bad, why := custDNSProblems(e.sn, e.cust)
	if len(bad) == 0 {
		okItem(it, fmt.Sprintf("The firewall lets customers on %s ask the router for DNS.", e.cust.iface))
		return
	}
	it.Status = StatusFail
	it.Evidence = append(it.Evidence, why)
	it.Detail = fmt.Sprintf("The router's firewall would drop DNS questions from the customer network %s (%s), so customers could ping the internet but not open any website.", e.cust.iface, why)
	for _, r := range e.sn.tbl("/ip/firewall/filter") {
		if r["comment"] == custDNSComment {
			it.Detail += " A rule \"" + custDNSComment + "\" exists but does not take effect: move it above the drop by hand."
			it.Fix = CheckFix{Kind: FixManual, Label: "Put the DNS accept rules first", Command: custDNSCommand(e.cust.iface), Role: store.RoleOwner}
			return
		}
	}
	it.Fix = CheckFix{Kind: FixAuto, Label: "Let customers ask the router for DNS", Role: store.RoleOperator}
}

// ---- customer interface -------------------------------------------------------------

func evalCustomerIf(e *evalCtx, it *CheckItem) {
	manual := CheckFix{Kind: FixManual, Label: "Fix the building's fields", Role: store.RoleOperator}
	if e.planErr != nil {
		it.Status = StatusFail
		it.Detail = "The building's customer network is not valid: " + e.planErr.Error()
		it.Fix = manual
		return
	}
	c := e.cust
	if !c.found {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("No interface on the router carries %s, the gateway address of this building's customer network. Check the building's customer subnet.", e.plan.GatewayIP)
		it.Fix = manual
		return
	}
	it.Evidence = append(it.Evidence, fmt.Sprintf("%s is on %s", c.addr, c.iface))
	if len(c.hotspots) == 0 && len(c.dhcps) == 0 {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("%s has the gateway address but no hotspot or DHCP server runs on it, so customers get no address.", c.iface)
		it.Fix = manual
		return
	}
	if len(c.hotspots) > 0 {
		it.Evidence = append(it.Evidence, "hotspot: "+strings.Join(c.hotspots, ", "))
	}
	if len(c.dhcps) > 0 {
		it.Evidence = append(it.Evidence, "DHCP server: "+strings.Join(c.dhcps, ", "))
	}
	okItem(it, fmt.Sprintf("Customers are served on %s.", c.iface))
}

// ---- bridge ports: OLT and bench ---------------------------------------------------------

// portAssess is the verdict on one physical port the server may move out of
// (or into) a bridge.
type portAssess struct {
	iface    string
	rows     []routeros.Row // the port's bridge-port rows
	hard     []string       // reasons the server must never touch it
	unknown  []string       // things that could not be checked
	learned  []string       // MACs of other devices learned on it
	vlans    []string       // VLAN interfaces on it
	strands  []string       // VLAN interfaces that are no bridge port (moving would cut them off)
	conflict bool
}

func (a portAssess) autoSafe() bool {
	return len(a.hard) == 0 && len(a.unknown) == 0 && len(a.learned) == 0
}

// wanIfaces is every interface name the router treats as the WAN.
func wanIfaces(sn *snapshot, site store.Site) map[string]bool {
	out := map[string]bool{}
	if site.WANInterface != "" {
		out[site.WANInterface] = true
	}
	for _, r := range sn.tbl("/interface/list/member") {
		if r["list"] == site.WANList && !rowDisabled(r) {
			out[r["interface"]] = true
		}
	}
	return out
}

// assessPort looks at an interface for the OLT and bench rules: is it a WAN
// port, does it carry an address, DHCP or hotspot itself, which hosts are
// learned on it, which VLANs sit on it.
func assessPort(sn *snapshot, site store.Site, iface string) portAssess {
	a := portAssess{iface: iface}
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["interface"] == iface {
			a.rows = append(a.rows, r)
		}
	}
	for _, r := range sn.tbl("/interface/vlan") {
		if r["interface"] == iface {
			a.vlans = append(a.vlans, r["name"])
		}
	}
	for _, v := range a.vlans {
		in := false
		for _, r := range sn.tbl("/interface/bridge/port") {
			if r["interface"] == v && !rowDisabled(r) {
				in = true
			}
		}
		if !in {
			a.strands = append(a.strands, v)
		}
	}
	if wanIfaces(sn, site)[iface] {
		a.hard = append(a.hard, iface+" is a WAN port")
	}
	if _, err := sn.failed("/interface/list/member"); err != nil {
		a.unknown = append(a.unknown, "the interface lists could not be read")
	}
	for _, c := range []struct {
		path, key, what string
	}{
		{"/ip/address", "interface", "an IP address"},
		{"/ip/dhcp-server", "interface", "a DHCP server"},
		{"/ip/dhcp-client", "interface", "a DHCP client"},
		{"/ip/hotspot", "interface", "a hotspot"},
	} {
		if _, err := sn.failed(c.path); err != nil {
			a.unknown = append(a.unknown, c.path+" could not be read")
			continue
		}
		for _, r := range sn.tbl(c.path) {
			if r[c.key] == iface {
				a.hard = append(a.hard, iface+" itself has "+c.what)
				break
			}
		}
	}
	if _, err := sn.failed("/interface/bridge/host"); err != nil {
		a.unknown = append(a.unknown, "the learned hosts could not be read")
	} else {
		seen := map[string]bool{}
		for _, r := range sn.tbl("/interface/bridge/host") {
			if r["on-interface"] == iface && !yes(r["local"]) {
				mac := strings.ToUpper(r["mac-address"])
				if !seen[mac] {
					seen[mac] = true
					a.learned = append(a.learned, mac)
				}
			}
		}
		sort.Strings(a.learned)
	}
	return a
}

// assessOLT: the OLT port is a bridge port while a VLAN interface sits on it
// (or the building says VLANID > 0).
func assessOLT(sn *snapshot, site store.Site) portAssess {
	a := assessPort(sn, site, site.OLTInterface)
	a.conflict = len(a.rows) > 0 && (len(a.vlans) > 0 || site.VLANID > 0)
	if a.conflict && len(a.vlans) == 0 {
		a.unknown = append(a.unknown, "the building has a VLAN id but no VLAN interface exists on the port yet")
	}
	if len(a.strands) > 0 {
		a.hard = append(a.hard, "the VLAN interface "+strings.Join(a.strands, ", ")+" is not a bridge port, so removing the port would cut customers off")
	}
	return a
}

func oltCommand(site store.Site) string {
	return fmt.Sprintf("/interface/bridge/port/remove [find interface=%s]", site.OLTInterface)
}

func evalOLT(e *evalCtx, it *CheckItem) {
	if e.site.OLTInterface == "" {
		it.Status = StatusInfo
		it.Detail = "No fibre (OLT) interface is set for this building."
		return
	}
	a := assessOLT(e.sn, e.site)
	if !a.conflict {
		it.Evidence = append(it.Evidence, fmt.Sprintf("%s bridge ports: %d, VLANs: %s", a.iface, len(a.rows), orNone(a.vlans)))
		okItem(it, fmt.Sprintf("%s is not a bridge port with a VLAN on it.", a.iface))
		return
	}
	it.Status = StatusFail
	it.Evidence = append(it.Evidence, fmt.Sprintf("%s is a port of bridge %s", a.iface, a.rows[0]["bridge"]), "VLAN interface(s) on it: "+orNone(a.vlans))
	it.Detail = fmt.Sprintf("%s is a bridge port and a VLAN interface sits on it. RouterOS then does not hand the VLAN traffic to the VLAN interface, so customers get nothing.", a.iface)
	switch {
	case len(a.hard) > 0:
		it.Evidence = append(it.Evidence, a.hard...)
		it.Detail += " The server will not change it itself: " + strings.Join(a.hard, "; ") + "."
		it.Fix = CheckFix{Kind: FixManual, Label: "Take the port out of the bridge", Command: oltCommand(e.site), Role: store.RoleOwner}
	case len(a.unknown) > 0:
		it.Evidence = append(it.Evidence, a.unknown...)
		it.Detail += " The server could not prove it is safe to change (" + strings.Join(a.unknown, "; ") + ")."
		it.Fix = CheckFix{Kind: FixManual, Label: "Take the port out of the bridge", Command: oltCommand(e.site), Role: store.RoleOwner}
	case len(a.learned) > 0 || len(otherBridges(a, e.cust)) > 0:
		var why []string
		if ob := otherBridges(a, e.cust); len(ob) > 0 {
			it.Evidence = append(it.Evidence, "the port is in bridge "+strings.Join(ob, ", ")+", not the customer bridge")
			why = append(why, fmt.Sprintf("%s is in bridge %s, which is not the customer bridge: other devices may reach the router through it", a.iface, strings.Join(ob, ", ")))
		}
		if len(a.learned) > 0 {
			it.Evidence = append(it.Evidence, "devices learned on the port: "+strings.Join(a.learned, ", "))
			why = append(why, "devices are talking through it right now ("+strings.Join(a.learned, ", ")+")")
		}
		it.Fix = CheckFix{Kind: FixConfirm, Label: "Take the port out of the bridge", Role: store.RoleOwner,
			ConfirmText: fmt.Sprintf("%s. Taking it out of the bridge may cut them off. Do it?", strings.Join(why, "; "))}
	default:
		it.Fix = CheckFix{Kind: FixAuto, Label: "Take the port out of the bridge", Role: store.RoleOperator}
	}
}

// otherBridges lists the bridges the port is in that are not the customer
// bridge (all of them when the customer bridge is not known).
func otherBridges(a portAssess, cust custInfo) []string {
	var out []string
	for _, r := range a.rows {
		if !cust.found || r["bridge"] != cust.iface {
			out = append(out, r["bridge"])
		}
	}
	return out
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// benchPlan is the bench port's state against the customer bridge.
type benchPlan struct {
	portAssess
	inCustomer bool
	inOther    string // another bridge that holds it
	noIface    bool
}

func assessBench(sn *snapshot, site store.Site, cust custInfo) benchPlan {
	b := benchPlan{portAssess: assessPort(sn, site, site.BenchPort)}
	exists := false
	for _, r := range sn.tbl("/interface") {
		if r["name"] == site.BenchPort {
			exists = true
		}
	}
	b.noIface = !exists
	for _, r := range b.rows {
		switch {
		case r["bridge"] == cust.iface:
			b.inCustomer = true
		case b.inOther == "":
			b.inOther = r["bridge"]
		}
	}
	if site.BenchPort == site.OLTInterface {
		b.hard = append(b.hard, "it is the fibre (OLT) port")
	}
	// Learned hosts only matter when a port is moved out of another bridge.
	if b.inOther == "" {
		b.learned = nil
		kept := b.unknown[:0]
		for _, u := range b.unknown {
			if u != "the learned hosts could not be read" {
				kept = append(kept, u)
			}
		}
		b.unknown = kept
	}
	return b
}

func evalBench(e *evalCtx, it *CheckItem) {
	if e.site.BenchPort == "" {
		it.Status = StatusInfo
		it.Detail = "No bench port is set for this building."
		return
	}
	if !e.cust.found {
		it.Status = StatusUnknown
		it.Detail = "The customer interface is not known yet, so the bench port cannot be judged. See the customer network check."
		return
	}
	if !e.cust.isBridge {
		it.Status = StatusInfo
		it.Detail = fmt.Sprintf("The customer network is on %s, which is not a bridge, so a bench port cannot be added to it.", e.cust.iface)
		return
	}
	b := assessBench(e.sn, e.site, e.cust)
	if b.noIface {
		it.Status = StatusWarn
		it.Detail = fmt.Sprintf("The router has no interface called %s. Check the bench port in the building's fields.", e.site.BenchPort)
		it.Fix = CheckFix{Kind: FixManual, Label: "Fix the building's fields", Role: store.RoleOperator}
		return
	}
	if b.inCustomer {
		okItem(it, fmt.Sprintf("%s is on the customer bridge %s.", e.site.BenchPort, e.cust.iface))
		return
	}
	it.Status = StatusWarn
	where := "in no bridge"
	if b.inOther != "" {
		where = "in bridge " + b.inOther
		it.Evidence = append(it.Evidence, "bench port is a port of "+b.inOther)
	}
	it.Detail = fmt.Sprintf("The bench port %s is %s, not on the customer bridge %s, so a laptop there cannot be tested as a customer.", e.site.BenchPort, where, e.cust.iface)
	cmd := fmt.Sprintf("/interface/bridge/port/add bridge=%s interface=%s comment=\"ftn: bench port\"", e.cust.iface, e.site.BenchPort)
	switch {
	case len(b.hard) > 0:
		it.Evidence = append(it.Evidence, b.hard...)
		it.Detail += " The server will not move it: " + strings.Join(b.hard, "; ") + "."
		it.Fix = CheckFix{Kind: FixManual, Label: "Put the bench port on the bridge", Command: cmd}
	case len(b.unknown) > 0:
		it.Evidence = append(it.Evidence, b.unknown...)
		it.Fix = CheckFix{Kind: FixManual, Label: "Put the bench port on the bridge", Command: cmd}
	case len(b.learned) > 0:
		it.Evidence = append(it.Evidence, "devices learned on the port: "+strings.Join(b.learned, ", "))
		it.Fix = CheckFix{Kind: FixConfirm, Label: "Move the bench port", Role: store.RoleOwner,
			ConfirmText: fmt.Sprintf("Devices are connected to %s (%s) in bridge %s. Moving it to %s will interrupt them. Do it?", e.site.BenchPort, strings.Join(b.learned, ", "), b.inOther, e.cust.iface)}
	default:
		it.Fix = CheckFix{Kind: FixAuto, Label: "Put the bench port on the bridge", Role: store.RoleOperator}
	}
}

// ---- hotspot orphans ------------------------------------------------------------------

// orphan is a hotspot user or IP binding that holds a MAC no account has.
type orphan struct {
	path, id, name, mac string
	serverMade          bool
}

func (o orphan) String() string {
	k := "hotspot user"
	if o.path == "/ip/hotspot/ip-binding" {
		k = "IP binding"
	}
	s := fmt.Sprintf("%s %s", k, o.name)
	if o.mac != "" {
		s += " (" + o.mac + ")"
	}
	if o.serverMade {
		s += " made by the server"
	}
	return s
}

// findOrphans lists the leftovers. A hotspot user the server made (comment
// "ispbill:...") is an orphan when no account has its code; any other user or
// binding is one when it holds a MAC that no account has. Rows holding no MAC
// that are not ours are never judged.
func findOrphans(sn *snapshot, ix acctIndex) []orphan {
	var out []orphan
	for _, path := range []string{"/ip/hotspot/user", "/ip/hotspot/ip-binding"} {
		for _, r := range sn.tbl(path) {
			if o, ok := orphanOf(path, r, ix); ok {
				out = append(out, o)
			}
		}
	}
	return out
}

// orphanOf judges one hotspot user or IP binding row (see findOrphans).
func orphanOf(path string, r routeros.Row, ix acctIndex) (orphan, bool) {
	mac, hasMAC := store.CanonMAC(r["mac-address"])
	switch path {
	case "/ip/hotspot/user":
		name := r["name"]
		if strings.HasPrefix(r["comment"], "ispbill:") {
			if !ix.codes[name] {
				return orphan{path: path, id: r[routeros.RowID], name: name, mac: macIf(hasMAC, mac), serverMade: true}, true
			}
			return orphan{}, false
		}
		if hasMAC && !ix.macs[mac] {
			return orphan{path: path, id: r[routeros.RowID], name: name, mac: mac}, true
		}
	case "/ip/hotspot/ip-binding":
		if strings.HasPrefix(r["comment"], "ftn:") {
			return orphan{}, false // the server's own rows
		}
		if hasMAC && !ix.macs[mac] {
			name := firstOf(r["comment"], r["type"], r["mac-address"])
			return orphan{path: path, id: r[routeros.RowID], name: name, mac: mac}, true
		}
	}
	return orphan{}, false
}

func macIf(ok bool, m string) string {
	if ok {
		return m
	}
	return ""
}

func evalOrphans(e *evalCtx, it *CheckItem) {
	orphans := findOrphans(e.sn, e.ix)
	if len(orphans) == 0 {
		okItem(it, "No leftover hotspot logins on the router.")
		return
	}
	foreign := 0
	for _, o := range orphans {
		it.Evidence = append(it.Evidence, o.String())
		if !o.serverMade {
			foreign++
		}
	}
	it.Status = StatusWarn
	it.Detail = fmt.Sprintf("%d hotspot login(s) on the router belong to no flat here. They let those devices online without the server knowing, and the server lists them as new devices (with the login name).", len(orphans))
	if foreign == 0 {
		it.Fix = CheckFix{Kind: FixAuto, Label: "Remove the leftovers", Role: store.RoleOperator}
		return
	}
	it.Fix = CheckFix{Kind: FixConfirm, Label: "Remove the leftovers", Role: store.RoleOperator,
		ConfirmText: fmt.Sprintf("Remove %d hotspot login(s) from the router? %d of them were not made by the server (for example a test login); those devices lose free access.", len(orphans), foreign)}
}

// ---- traffic flow -------------------------------------------------------------------------

// flowWant is what traffic-flow should look like.
type flowWant struct{ iface, dst, port string }

func (m *Manager) flowWant(iface string) (flowWant, bool) {
	port := "2055"
	if a := m.cfg.NetflowAddr; a != "" && !strings.EqualFold(a, "off") {
		if _, p, err := net.SplitHostPort(a); err == nil && p != "" {
			port = p
		}
	} else if strings.EqualFold(a, "off") {
		return flowWant{}, false
	}
	return flowWant{iface: iface, dst: ServerTunnelIP(m.pfx), port: port}, true
}

// flowProblems says what differs from the wanted traffic-flow set-up.
func flowProblems(sn *snapshot, w flowWant) (problems []string, targetID string) {
	tf := sn.single("/ip/traffic-flow")
	if !yes(tf["enabled"]) {
		problems = append(problems, "traffic-flow is switched off")
	}
	if !sameSet(splitList(tf["interfaces"]), []string{w.iface}) {
		problems = append(problems, fmt.Sprintf("it watches %q instead of %s", tf["interfaces"], w.iface))
	}
	var tgt routeros.Row
	for _, r := range sn.tbl("/ip/traffic-flow/target") {
		if r["comment"] == netflowComment {
			tgt = r
			break
		}
	}
	switch {
	case tgt == nil:
		problems = append(problems, "there is no \"ftn: netflow\" target")
	default:
		targetID = tgt[routeros.RowID]
		if rowDisabled(tgt) || tgt["dst-address"] != w.dst || (tgt["port"] != "" && tgt["port"] != w.port) {
			problems = append(problems, fmt.Sprintf("the \"ftn: netflow\" target is off or points to %s:%s instead of %s:%s", tgt["dst-address"], tgt["port"], w.dst, w.port))
		}
	}
	return problems, targetID
}

func evalTrafficFlow(e *evalCtx, it *CheckItem) {
	if !e.cust.found {
		it.Status = StatusUnknown
		it.Detail = "The customer interface is not known yet, so traffic reporting cannot be judged. See the customer network check."
		return
	}
	w, ok := e.m.flowWant(e.cust.iface)
	if !ok {
		it.Status = StatusInfo
		it.Detail = "The server's traffic-flow listener is switched off (NETFLOW_ADDR=off)."
		return
	}
	problems, _ := flowProblems(e.sn, w)
	if len(problems) == 0 {
		okItem(it, fmt.Sprintf("The router reports traffic of %s to the server.", w.iface), "target "+w.dst+":"+w.port)
		return
	}
	it.Status = StatusWarn
	it.Evidence = append(it.Evidence, problems...)
	it.Detail = "Traffic reporting is not set up as the server expects, so per-service usage is missing: " + strings.Join(problems, "; ") + "."
	it.Fix = CheckFix{Kind: FixAuto, Label: "Set up traffic reporting", Role: store.RoleOperator}
}

// ---- information items -----------------------------------------------------------------------

func evalWWW(e *evalCtx, it *CheckItem) {
	var www routeros.Row
	for _, r := range e.sn.tbl("/ip/service") {
		if r["name"] == "www" {
			www = r
		}
	}
	if www == nil {
		it.Status = StatusInfo
		it.Detail = "The router has no www service."
		return
	}
	it.Evidence = append(it.Evidence, fmt.Sprintf("www: address=%s disabled=%s", www["address"], www["disabled"]))
	it.Status = StatusInfo
	switch {
	case rowDisabled(www):
		it.Detail = "The router's web page (WebFig) is switched off. WinBox still works."
	case www["address"] != "":
		it.Detail = "The router's web page (WebFig) only answers the billing server (" + www["address"] + "). WinBox still works from the LAN. If you want WebFig from the LAN too, add the LAN address to /ip/service www by hand."
	default:
		it.Detail = "The router's web page (WebFig) is open to every address the firewall allows."
	}
}

func evalContainers(e *evalCtx, it *CheckItem) {
	rows := e.sn.tbl("/container")
	var legacyRunning []string
	for _, r := range rows {
		name := firstOf(r["name"], r["hostname"], r["comment"], r["remote-image"])
		status := firstOf(r["status"], map[bool]string{true: "running", false: "stopped"}[yes(r["running"])])
		it.Evidence = append(it.Evidence, name+": "+status)
		if strings.Contains(strings.ToLower(name+" "+r["remote-image"]), "ispbill") && (status == "running" || yes(r["running"])) {
			legacyRunning = append(legacyRunning, name)
		}
	}
	switch {
	case e.site.LegacyContainer && len(legacyRunning) > 0:
		it.Status = StatusWarn
		it.Detail = "The old ispbill container is still running on the router (" + strings.Join(legacyRunning, ", ") + "). It should be stopped now that the server runs the building."
	case len(rows) == 0:
		it.Status = StatusInfo
		it.Detail = "No containers on the router."
	default:
		it.Status = StatusInfo
		it.Detail = fmt.Sprintf("%d container(s) on the router.", len(rows))
	}
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func evalIPv6(e *evalCtx, it *CheckItem) {
	v := e.sn.single("/ipv6/settings")
	it.Evidence = append(it.Evidence, "disable-ipv6: "+v["disable-ipv6"])
	if yes(v["disable-ipv6"]) {
		okItem(it, "IPv6 is switched off on the router.")
		return
	}
	it.Status = StatusInfo
	it.Detail = "IPv6 is on. At the IPoE cutover the flats' IPv6 forwarding is dropped, so flats keep IPv4 only."
}

func evalPublicIP(e *evalCtx, it *CheckItem) {
	if e.m.cfg.PublicIP == "" {
		it.Status = StatusFail
		it.Detail = "PUBLIC_IP is not set on the billing server, so the routers cannot be told where to find it."
		it.Fix = CheckFix{Kind: FixManual, Label: "Set PUBLIC_IP in the server's environment", Role: store.RoleOwner}
		return
	}
	okItem(it, "The server's public address is "+e.m.cfg.PublicIP+".")
}

func evalUnmanaged(e *evalCtx, it *CheckItem) {
	switch {
	case billing.IPoE(e.site) && e.site.AccessMode == store.AccessIPoE:
		it.Status = StatusInfo
		it.Detail = "This building already runs in IPoE mode."
	case e.unmanagedErr != nil:
		it.Status = StatusUnknown
		it.Detail = unreadDetail("list the devices on the customer network", e.unmanagedErr)
		it.Fix = CheckFix{Kind: FixNone}
		e.slow = true
	case len(e.unmanaged) == 0:
		okItem(it, "No devices on the customer network that the server does not know.")
	default:
		it.Status = StatusFail
		it.Evidence = append(it.Evidence, e.unmanaged...)
		it.Detail = fmt.Sprintf("%d device(s) on the customer network are not flats the server knows (an access point or the OLT's management address, for instance). The cutover would cut them off unless you choose to keep them.", len(e.unmanaged))
		it.Fix = CheckFix{Kind: FixNone, Label: "Keep them at cutover"}
	}
}

func evalUnseen(e *evalCtx, it *CheckItem) {
	switch {
	case billing.IPoE(e.site) && e.site.AccessMode == store.AccessIPoE:
		it.Status = StatusInfo
		it.Detail = "This building already runs in IPoE mode."
	case e.unseenErr != nil:
		it.Status = StatusUnknown
		it.Detail = unreadDetail("compare the flats with the router's devices", e.unseenErr)
		it.Fix = CheckFix{Kind: FixNone}
		e.slow = true
	case len(e.unseen) == 0:
		okItem(it, "Every flat with paid time was seen on the router.")
	default:
		it.Status = StatusFail
		it.Evidence = append(it.Evidence, e.unseen...)
		it.Detail = fmt.Sprintf("%d flat(s) with paid time were not seen on the router, so they would have no service after the cutover. Fix the MAC under Unregistered devices, or go on anyway.", len(e.unseen))
		it.Fix = CheckFix{Kind: FixNone}
	}
}

// ---- the pass -----------------------------------------------------------------------------------

// loadJoinReport returns the router's own account of its last join (the
// report endpoint's stored text, parsed by JoinReport in joinreport.go).
func (m *Manager) loadJoinReport(siteID string) *JoinReport {
	if r, ok := m.JoinReport(siteID); ok {
		return &r
	}
	return nil
}

func summarize(items []CheckItem) CheckSummary {
	var s CheckSummary
	for _, it := range items {
		switch it.Status {
		case StatusOK:
			s.OK++
		case StatusWarn:
			s.Warn++
		case StatusFail:
			s.Fail++
		case StatusUnknown:
			s.Unknown++
		}
	}
	return s
}

// Checks reads the building's router and evaluates the setup checklist. The
// answer is cached for 15 seconds; fresh bypasses the cache. A router that
// cannot be read gives items with status unknown and Router.Reachable false.
func (m *Manager) Checks(ctx context.Context, siteID string, fresh bool) (Checks, error) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return Checks{}, billing.ErrNoSite
	}
	now := m.now()
	if !fresh {
		if c, hit := m.chk.get(siteID, now); hit {
			return c, nil
		}
	}
	c := m.runChecks(ctx, site)
	if c.Router.Reachable {
		m.chk.put(siteID, now, c) // an unreachable router is never cached: the next look must try again
	}
	if c.Slow {
		m.scheduleRecheck(siteID, 1)
	} else {
		m.chk.rechecks(siteID, false)
	}
	return c, nil
}

// rechecks marks (set) or clears a pending automatic re-check of a building
// and says whether one was already pending.
func (cs *checksState) rechecks(siteID string, set bool) (was bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.pending == nil {
		cs.pending = map[string]bool{}
	}
	was = cs.pending[siteID]
	if set {
		cs.pending[siteID] = true
	} else {
		delete(cs.pending, siteID)
	}
	return was
}

// scheduleRecheck repeats a pass that had reads which timed out, without
// anybody pressing a button: the Setup page shows "checking again" meanwhile.
func (m *Manager) scheduleRecheck(siteID string, attempt int) {
	if attempt > recheckMax || m.chk.rechecks(siteID, true) {
		return
	}
	time.AfterFunc(recheckAfter, func() {
		m.chk.rechecks(siteID, false)
		m.mu.RLock()
		ctx := m.ctx
		m.mu.RUnlock()
		if ctx == nil || ctx.Err() != nil {
			return
		}
		var site store.Site
		var ok bool
		m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
		if !ok || site.Removed {
			return
		}
		c := m.runChecks(ctx, site)
		if c.Router.Reachable {
			m.chk.put(siteID, m.now(), c)
		}
		if c.Slow {
			m.scheduleRecheck(siteID, attempt+1)
		}
	})
}

func (m *Manager) runChecks(ctx context.Context, site store.Site) Checks {
	now := m.now()
	eff := m.Effective(site)
	rt := m.Router(site.ID)
	e := &evalCtx{m: m, site: eff, ix: m.accountIndex(), now: now}
	tables := append([]string(nil), checkTables...)
	e.sn = readSnapshot(ctx, rt, checkSingles, tables)
	e.plan, e.planErr = PlanSubnet(eff.CustomerSubnet)
	if e.planErr == nil {
		e.cust = detectCustomer(e.sn, e.plan)
	}
	// The two cutover guards read the router themselves (own timeouts).
	if e.sn.errs["/system/resource"] == nil {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, extraReadTimeout)
			defer cancel()
			e.unmanaged, e.unmanagedErr = m.UnmanagedDevices(c, site.ID)
		}()
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, extraReadTimeout)
			defer cancel()
			e.unseen, e.unseenErr = m.UnseenEntitled(c, site.ID)
		}()
		wg.Wait()
	} else {
		e.unmanagedErr, e.unseenErr = e.sn.errs["/system/resource"], e.sn.errs["/system/resource"]
	}
	items := e.buildChecks()
	res := e.sn.single("/system/resource")
	out := Checks{SiteID: site.ID, GeneratedAt: now, Items: items, JoinReport: m.loadJoinReport(site.ID),
		Router: RouterInfo{Reachable: e.sn.errs["/system/resource"] == nil, Version: res["version"], Board: res["board-name"]}}
	out.Slow = e.slow
	out.Summary = summarize(items)
	out.Summary.CutoverReady = out.Router.Reachable && len(out.Blockers(false, false)) == 0
	return out
}
