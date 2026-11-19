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
	FixManual  = "manual"  // the server cannot change it; the item's Detail says in words what to do
	FixNone    = "none"
)

// Check ids.
const (
	CheckVersion      = "router.version"
	CheckAPIPolicy    = "api_user.policy"
	CheckLayout       = "router.layout"
	CheckDMFetch      = "device_mode.fetch"
	CheckDNS          = "dns.remote_requests"
	CheckCustDNS      = "customer.dns_input"
	CheckHooks        = "router.script_hooks"
	CheckCustomerIf   = "customer.interface"
	CheckOLT          = "olt.bridge_conflict"
	CheckBench        = "bench.port"
	CheckTrafficFlow  = "traffic_flow"
	CheckWWW          = "service.www"
	CheckIPv6         = "ipv6"
	CheckPublicIP     = "server.public_ip"
	CheckWalled       = "walled.rules"
	CheckWalledDNS    = "walled.dns"
	CheckDHCP         = "dhcp.ftn"
	CheckWAN          = "wan.internet"
	CheckBootFile     = "bootstrap.file"
	checksCacheFor    = 60 * time.Second
	checksBudget      = 45 * time.Second // the whole pass, all reads together
	recheckMax        = 6                // ... this many times in a row
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
	Physical    bool   `json:"physical,omitempty"`
	Role        string `json:"role,omitempty"` // operator | owner
	// Automatic: the server applies this fix by itself (no button). TryNow: the
	// last automatic attempt failed, so a button to try again is shown.
	Automatic bool `json:"automatic,omitempty"`
	TryNow    bool `json:"try_now,omitempty"`
}

// CheckItem is one line of the checklist.
type CheckItem struct {
	ID       string   `json:"id"`
	Group    string   `json:"group"` // router | network | devices | services
	Status   string   `json:"status"`
	Blocks   bool     `json:"-"` // a failing item that stops the router from counting as ready
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Evidence []string `json:"evidence"`
	Fix      CheckFix `json:"fix"`
	// Auto says what the server did about the item by itself (nil if nothing).
	Auto *AutoInfo `json:"auto,omitempty"`
	// Override names the option that lets the operator go on in spite of this
	// item ("keep_unknown", "force"); "" if none. Not part of the JSON.
	Override string `json:"-"`
}

// RouterInfo is what the pass learned about the router itself.
type RouterInfo struct {
	Reachable bool   `json:"reachable"`
	Version   string `json:"version"`
	Board     string `json:"board"`
}

// CheckSummary counts the items by status.
type CheckSummary struct {
	OK      int `json:"ok"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Unknown int `json:"unknown"`
}

// Checks is the answer of GET /sites/{id}/checks.
type Checks struct {
	SiteID      string       `json:"site_id"`
	GeneratedAt time.Time    `json:"generated_at"`
	Router      RouterInfo   `json:"router"`
	Summary     CheckSummary `json:"summary"`
	JoinReport  *JoinReport  `json:"join_report"`
	// Layout is "v5" (the router has the layout the bootstrap script builds),
	// "old" (it has not: nothing is written to it, it must be rebuilt) or
	// "unknown" (the bridges could not be read).
	Layout string      `json:"layout"`
	Items  []CheckItem `json:"items"`
	// AutoFixes says whether the server's automatic fixes of this router are paused.
	AutoFixes *PauseState `json:"auto_fixes,omitempty"`
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

// Blockers lists the ids of the items that stop the router from counting as
// ready (status fail or unknown), leaving out the ones the options override:
// keepUnknown covers unmanaged.devices, force covers flats.unseen.
func (c Checks) Blockers(keepUnknown, force bool) []string {
	var out []string
	for _, it := range c.Items {
		if !it.Blocks || (it.Status != StatusFail && it.Status != StatusUnknown) {
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

type cachedChecks struct {
	at time.Time
	c  Checks
}

// checksState is the Manager's cache of the last pass of each building.
type checksState struct {
	mu      sync.Mutex
	cache   map[string]cachedChecks
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

// ---- reading -----------------------------------------------------------------

// The singleton paths of the pass (everything else is a table).
var checkSingles = []string{"/system/resource", "/system/device-mode", "/ip/dns", "/ip/traffic-flow", "/ipv6/settings"}

var checkTables = []string{
	"/ip/address", "/interface", "/interface/bridge", "/interface/bridge/port", "/interface/bridge/host", "/interface/vlan",
	"/interface/list/member", "/ip/dhcp-server", "/ip/dhcp-client", "/ip/traffic-flow/target", "/ip/service",
	"/user/group", "/file", "/ip/firewall/filter", "/ip/firewall/nat", "/ip/route",
	// read-only, for router.script_hooks
	"/ppp/profile", "/tool/netwatch",
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

// readPaths each check needs; a failed read makes the item unknown.
var needs = map[string][]string{
	CheckLayout:      {"/interface/bridge"},
	CheckVersion:     {"/system/resource"},
	CheckAPIPolicy:   {"/user/group"},
	CheckDMFetch:     {"/system/device-mode"},
	CheckDNS:         {"/ip/dns"},
	CheckCustDNS:     {"/ip/firewall/filter", "/ip/address", "/ip/dhcp-server"},
	CheckHooks:       {"/ip/dhcp-server", "/ip/dhcp-client", "/ppp/profile", "/tool/netwatch"},
	CheckCustomerIf:  {"/ip/address", "/ip/dhcp-server"},
	CheckOLT:         {"/interface/bridge/port", "/interface/vlan"},
	CheckBench:       {"/interface/bridge/port", "/interface", "/interface/vlan", "/interface/list/member", "/ip/address", "/ip/dhcp-server", "/ip/dhcp-client"},
	CheckTrafficFlow: {"/ip/traffic-flow", "/ip/traffic-flow/target", "/ip/address", "/ip/dhcp-server"},
	CheckWWW:         {"/ip/service"},
	CheckIPv6:        {"/ipv6/settings"},
	CheckWalled:      {"/ip/firewall/filter", "/ip/firewall/nat"},
	CheckWalledDNS:   {"/ip/dns"},
	CheckDHCP:        {"/ip/dhcp-server"},
	CheckWAN:         {"/ip/route"},
	CheckBootFile:    {"/file"},
}

type checkDef struct {
	id, group, title string
	blocks           bool
	override         string
	eval             func(e *evalCtx, it *CheckItem)
}

// checkDefs is the checklist, in display order.
var checkDefs = []checkDef{
	{CheckLayout, "router", "Router was built by the set-up script", true, "", evalLayout},
	{CheckVersion, "router", "Router software version", false, "", evalVersion},
	{CheckAPIPolicy, "router", "Server login has the rights it needs", false, "", evalAPIPolicy},
	{CheckDMFetch, "router", "Router allows file downloads", false, "", evalDMFetch},
	{CheckDNS, "services", "Customers can use the router for DNS", true, "", evalDNS},
	{CheckCustDNS, "services", "Router accepts DNS from the customer network", true, "", evalCustDNS},
	{CheckHooks, "router", "Scripts that run by themselves on the router", false, "", evalHooks},
	{CheckCustomerIf, "network", "Customer network is on one interface", true, "", evalCustomerIf},
	{CheckDHCP, "network", "Customer DHCP server is on", true, "", evalDHCP},
	{CheckWalled, "network", "Unpaid and unregistered flats are walled", true, "", evalWalled},
	{CheckWalledDNS, "network", "Walled flats can still resolve any name", false, "", evalWalledDNS},
	{CheckWAN, "network", "Router has a way to the internet", false, "", evalWAN},
	{CheckOLT, "network", "Fibre (OLT) port is not stuck in the bridge", true, "", evalOLT},
	{CheckBench, "network", "Customer port (extra)", false, "", evalBench},
	{CheckTrafficFlow, "network", "Traffic reporting to the server", false, "", evalTrafficFlow},
	{CheckWWW, "services", "Router web page (WebFig)", false, "", evalWWW},
	{CheckBootFile, "router", "No set-up script left on the router", false, "", evalBootFile},
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
		it := CheckItem{ID: d.id, Group: d.group, Title: d.title, Blocks: d.blocks, Override: d.override,
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

// Layout values of Checks.
const (
	LayoutV5      = "v5"
	LayoutOld     = "old"
	LayoutUnknown = "unknown"
)

// RebuildText is what the operator is told for a router without the v5 layout.
const RebuildText = "Rebuild required: reset the router and paste a new line."

// ErrRebuildRequired is returned by a fix on a router that does not have the v5 layout.
var ErrRebuildRequired = errors.New("rebuild required: reset the router and paste a new line (the server writes nothing to a router the set-up script did not build)")

// LayoutOf reads the v5 marker (the customer bridge the script makes, with its
// comment) from a snapshot.
func LayoutOf(sn *snapshot) string {
	if _, err := sn.failed("/interface/bridge"); err != nil {
		return LayoutUnknown
	}
	if billing.HasV5Layout(sn.tbl("/interface/bridge")) {
		return LayoutV5
	}
	return LayoutOld
}

func evalLayout(e *evalCtx, it *CheckItem) {
	switch LayoutOf(e.sn) {
	case LayoutV5:
		okItem(it, "The router has the layout the set-up script builds.")
	default:
		it.Status = StatusFail
		it.Detail = RebuildText + " This router was not built by the set-up script (it has no " + billing.CustomerBridgeName + " bridge made by it), so the server changes nothing on it."
		it.Fix = CheckFix{Kind: FixNone}
	}
}

func evalVersion(e *evalCtx, it *CheckItem) {
	v := e.sn.single("/system/resource")["version"]
	major, minor, ok := parseVersion(v)
	it.Evidence = append(it.Evidence, "version: "+v)
	switch {
	case !ok:
		it.Status = StatusUnknown
		it.Detail = "The router did not say which RouterOS version it runs."
		it.Detail += " Paste a new line on the router: the server reads the version from the router it builds."
	case major < 7 || (major == 7 && minor < 13):
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("The router runs RouterOS %s. Version 7.13 or newer is needed.", v)
		it.Detail += " Upgrade RouterOS on the router (System, Packages), then check again."
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
	if grp == nil {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("The router has no user group called %s, so the server's login may be missing rights.", name)
		it.Detail += " Paste a new line on the router: the line builds the group with exactly the rights the server needs."
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
		it.Detail += " Paste a new line on the router: the line rebuilds the group with the right rights."
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
		it.Detail += " Paste a new line on the router: the line rebuilds the group with only the rights the server needs."
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
		it.Detail += " Open System, Device-mode on the router, then check again."
		return "", false
	}
	it.Evidence = append(it.Evidence, fmt.Sprintf("device-mode %s: %s", key, v))
	return v, true
}

const physicalNote = " This needs the router's button (or a power cut) within 5 minutes of the command, and the router restarts."

func evalDMFetch(e *evalCtx, it *CheckItem) {
	v, ok := dmField(e, it, "fetch")
	if !ok {
		return
	}
	if !yes(v) {
		it.Status = StatusWarn
		it.Detail = "The router does not allow file downloads, so the line cannot fetch its script. Paste the line on the router: it turns downloads on when asked and tells you to press the router's button." + physicalNote
		it.Fix = CheckFix{Kind: FixManual, Label: "Allow downloads", Physical: true, Role: store.RoleOperator}
		return
	}
	okItem(it, "The router allows file downloads.")
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
		okItem(it, "No scripts are hung on DHCP, PPP or netwatch events.")
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
	it.Detail = "The router does not answer DNS for customers (allow-remote-requests is off), so customers would have no working names."
	safe, why := dnsSafe(e.sn, e.site, e.cust.safeSet())
	if safe {
		it.Fix = CheckFix{Kind: FixAuto, Label: "Turn on DNS for customers", Role: store.RoleOperator}
		it.Evidence = append(it.Evidence, "the uplink is the WAN and the firewall already drops DNS from it")
		return
	}
	it.Detail += " It is not turned on for you because the router might then answer the whole internet (" + why + ")."
	it.Detail += " Paste a new line on the router: the line builds a firewall that drops DNS from the WAN, and turns DNS on."
	it.Fix = CheckFix{Kind: FixManual, Label: "Block DNS from the WAN, then turn it on", Role: store.RoleOwner}
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
			it.Detail += " A rule \"" + custDNSComment + "\" exists but does not take effect: it is below a drop. Paste a new line on the router to rebuild its firewall in the right order."
			it.Fix = CheckFix{Kind: FixManual, Label: "Put the DNS accept rules first", Role: store.RoleOwner}
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
	if len(c.dhcps) == 0 {
		it.Status = StatusFail
		it.Detail = fmt.Sprintf("%s has the gateway address but no DHCP server runs on it, so customers get no address.", c.iface)
		it.Fix = manual
		return
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
// port, does it carry an address or DHCP itself, which hosts are
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
	// Only a customer VLAN makes a bridged OLT port a conflict. With no VLAN set a VLAN interface on the
	// OLT port is reported in words by the server's own pass (custport.go), not "fixed" here.
	a.conflict = len(a.rows) > 0 && site.VLANID > 0
	if a.conflict && len(a.vlans) == 0 {
		a.unknown = append(a.unknown, "the building has a VLAN id but no VLAN interface exists on the port yet")
	}
	if len(a.strands) > 0 {
		a.hard = append(a.hard, "the VLAN interface "+strings.Join(a.strands, ", ")+" is not a bridge port, so removing the port would cut customers off")
	}
	return a
}

func evalOLT(e *evalCtx, it *CheckItem) {
	if e.site.OLTInterface == "" {
		it.Status = StatusInfo
		it.Detail = "No fibre (OLT) interface is set for this building."
		return
	}
	a := assessOLT(e.sn, e.site)
	if msg := e.m.oltApplyError(e.site.ID); msg != "" && !a.conflict {
		it.Status = StatusWarn
		it.Detail = fmt.Sprintf("The server could not set the OLT uplink port %s up: %s. It tries again by itself.", e.site.OLTInterface, msg)
		return
	}
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
		it.Detail += " The server will not change it itself: " + strings.Join(a.hard, "; ") + ". Paste a new line on the router: it builds the VLAN on the OLT port outside the bridge."
		it.Fix = CheckFix{Kind: FixManual, Label: "Take the port out of the bridge", Role: store.RoleOwner}
	case len(a.unknown) > 0:
		it.Evidence = append(it.Evidence, a.unknown...)
		it.Detail += " The server could not prove it is safe to change (" + strings.Join(a.unknown, "; ") + "). Paste a new line on the router: it builds the VLAN on the OLT port outside the bridge."
		it.Fix = CheckFix{Kind: FixManual, Label: "Take the port out of the bridge", Role: store.RoleOwner}
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
		// Never automatic: the server's own pass owns the OLT port; taking it out of the bridge
		// by hand is the operator's decision.
		it.Fix = CheckFix{Kind: FixConfirm, Label: "Take the port out of the bridge", Role: store.RoleOwner,
			ConfirmText: fmt.Sprintf("%s is a bridge port and the customer VLAN sits on it. Taking it out of the bridge may cut customers off. Do it?", a.iface)}
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

// custPortApplied: the port is a member of br-customers with horizon=1 and the server's comment.
func custPortApplied(sn *snapshot, port string) bool {
	for _, r := range sn.tbl("/interface/bridge/port") {
		if r["interface"] == port && r["bridge"] == CustomerBridge && !rowDisabled(r) && r["horizon"] == "1" && r["comment"] == CustPortComment {
			return true
		}
	}
	return false
}

// evalBench judges the extra customer port. It is read only: the server puts
// the port on the customer bridge by itself (custport.go), so there is no button;
// the item says OK, or in words why the port is not applied.
func evalBench(e *evalCtx, it *CheckItem) {
	ports := SplitPorts(e.site.CustomerPorts)
	if len(ports) == 0 {
		it.Status = StatusInfo
		it.Detail = "No extra customer port is set for this router."
		return
	}
	var notOK []string
	for _, port := range ports {
		if custPortApplied(e.sn, port) {
			continue
		}
		it.Status = StatusWarn
		if pr := portProblems(e.sn, e.site, port, "bench"); len(pr) > 0 {
			it.Evidence = append(it.Evidence, pr...)
			notOK = append(notOK, fmt.Sprintf("%s: %s", port, strings.Join(pr, "; ")))
		} else {
			notOK = append(notOK, port+": the server is setting it up")
		}
	}
	if len(notOK) == 0 {
		okItem(it, fmt.Sprintf("%s: customer port(s) on %s: a device plugged in gets a customer address and waits to be registered.", strings.Join(ports, ", "), CustomerBridge))
		return
	}
	it.Detail = "The server could not set up every customer port yet: " + strings.Join(notOK, " | ") + ". It tries again by itself; change the customer ports in the router's fields or the Ports tab."
	if st := e.m.CustomerPortStatus(e.site.ID); st.State == PortError && st.Message != "" {
		it.Detail += " Last error: " + st.Message + "."
	}
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
	// RouterOS 7.25 traffic-flow targets have no comment, so ours is the one aimed at the
	// server's tunnel address, which nothing else on the router uses.
	for _, r := range sn.tbl("/ip/traffic-flow/target") {
		if r["dst-address"] == w.dst {
			tgt = r
			break
		}
	}
	switch {
	case tgt == nil:
		problems = append(problems, "there is no traffic-flow target aimed at the server ("+w.dst+")")
	default:
		targetID = tgt[routeros.RowID]
		if rowDisabled(tgt) || tgt["dst-address"] != w.dst || (tgt["port"] != "" && tgt["port"] != w.port) {
			problems = append(problems, fmt.Sprintf("the server's traffic-flow target is off or uses port %s instead of %s", tgt["port"], w.port))
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
	case wwwAddrOK(www["address"], e.site, ServerTunnelIP(e.m.pfx)):
		okItem(it, "The router's web page (WebFig) answers the admin LAN ("+lanNetOf(e.site)+") and the billing server only.")
	case www["address"] != "":
		it.Detail = "The router's web page (WebFig) only answers " + www["address"] + ". WinBox still works from the LAN. A new line (rebuild) opens WebFig to the admin LAN and the server's tunnel address."
	default:
		it.Detail = "The router's web page (WebFig) is open to every address the firewall allows."
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
	it.Detail = "IPv6 is on. The flats' IPv6 forwarding is dropped, so flats keep IPv4 only."
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
	c, err := m.checksRaw(ctx, siteID, fresh)
	if err != nil {
		return c, err
	}
	return m.decorate(siteID, c), nil
}

func (m *Manager) checksRaw(ctx context.Context, siteID string, fresh bool) (Checks, error) {
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
	ctx, cancelBudget := context.WithTimeout(ctx, checksBudget)
	defer cancelBudget()
	e.sn = readSnapshot(ctx, rt, checkSingles, tables)
	e.plan, e.planErr = PlanSubnet(eff.CustomerSubnet)
	if e.planErr == nil {
		e.cust = detectCustomer(e.sn, e.plan)
	}
	items := e.buildChecks()
	layout := LayoutOf(e.sn)
	if layout != LayoutV5 {
		// No fix of any kind on a router that does not have the v5 layout: nothing is written to it.
		for i := range items {
			items[i].Fix = CheckFix{Kind: FixNone}
		}
	}
	res := e.sn.single("/system/resource")
	out := Checks{SiteID: site.ID, GeneratedAt: now, Items: items, JoinReport: m.loadJoinReport(site.ID),
		Router: RouterInfo{Reachable: e.sn.errs["/system/resource"] == nil, Version: res["version"], Board: res["board-name"]}}
	out.Layout = layout
	out.Slow = e.slow
	out.Summary = summarize(items)
	return out
}

// wwwAddrOK: the www service address list is the admin LAN network plus the
// server's tunnel /32, as the bootstrap script sets it.
func wwwAddrOK(addr string, site store.Site, serverIP string) bool {
	lan, err := PlanLAN(site.LANSubnet)
	if err != nil {
		return false
	}
	var hasLAN, hasSrv bool
	for _, a := range strings.Split(addr, ",") {
		a = strings.TrimSpace(a)
		switch {
		case a == lan.Network:
			hasLAN = true
		case a == serverIP+"/32":
			hasSrv = true
		}
	}
	return hasLAN && hasSrv
}

func lanNetOf(site store.Site) string {
	if lan, err := PlanLAN(site.LANSubnet); err == nil {
		return lan.Network
	}
	return site.LANSubnet
}
