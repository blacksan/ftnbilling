package sites

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// The Ports tab: every physical interface of a router with its role, and the
// actions on it. The server is the source of truth for roles:
//
//	wan       the Internet uplink (Site.WANInterface); locked, the server never writes it
//	lan       the admin LAN (Site.LANPorts, members of br-lan)
//	olt       the OLT uplink (Site.OLTInterface)
//	customer  an extra customer port (Site.CustomerPorts, members of br-customers)
//	unused    everything else
//
// A role change updates the stored fields, then the port pass applies them to
// the router over REST (fixWriter allowlist) and heals them on every sweep. A
// port switched off is written at once and remembered in Site.DisabledPorts, so
// the sweep keeps it off. Reads are few: one /interface read with a short
// proplist per refresh (cached 4 s), and the slow facts (negotiated speed, which
// flats are seen on which port) every 60 s.

// Port roles.
const (
	PortRoleWAN      = "wan"
	PortRoleLAN      = "lan"
	PortRoleOLT      = "olt"
	PortRoleCustomer = "customer"
	PortRoleUnused   = "unused"
)

// PortRoleLabels are the words shown to people.
var PortRoleLabels = map[string]string{
	PortRoleWAN: "Internet uplink (WAN)", PortRoleLAN: "Admin LAN", PortRoleOLT: "OLT uplink",
	PortRoleCustomer: "Customer port", PortRoleUnused: "Unused",
}

// PortsPolicyOff is Site.PortsPolicy of a router on the default-off policy: every physical port except the
// uplink and the admin LAN ports stays switched off until the owner enables it (Site.EnabledPorts).
const PortsPolicyOff = "off"

// Texts shown for ports.
const (
	OLTNoOffMsg   = "OLT port: can't be switched off here. Give the OLT role to another port first."
	AdminPortNote = "Admin — router management and the billing site only"
	NoRoleUpWarn  = "Up with no role — disable it if you don't use it"
)

// Warnings shown before a risky change.
const (
	WarnLAN = "You may lose your WinBox session on this port."
	WarnOLT = "All customers on the OLT go offline."
)

// Kinds of port error (PortRefusal.Kind).
const (
	PortErrLocked    = "locked"      // the WAN uplink
	PortErrLastLAN   = "last_lan"    // the last Admin LAN port
	PortErrConfirm   = "confirm"     // the typed router name is missing or wrong
	PortErrForbidden = "forbidden"   // the role may not change this port
	PortErrConflict  = "conflict"    // the change is not possible in this state
	PortErrNoPort    = "no_port"     // the router has no such physical port
	PortErrInvalid   = "invalid"     // a bad value
	PortErrOLTOff    = "olt_disable" // the OLT uplink is never switched off from here
)

// PortRefusal is a refused port change.
type PortRefusal struct {
	Kind, Msg string
	Warning   string // for a confirm: what may happen
}

func (e *PortRefusal) Error() string { return e.Msg }

func portErr(kind, format string, a ...any) *PortRefusal {
	return &PortRefusal{Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// PortInfo is one row of the Ports tab.
type PortInfo struct {
	Name      string     `json:"name"`
	Kind      string     `json:"kind"` // ethernet | sfp | combo | wlan
	Role      string     `json:"role"`
	RoleLabel string     `json:"role_label"`
	Locked    bool       `json:"locked"`
	Running   bool       `json:"running"`
	Link      string     `json:"link"`   // "1Gbps full duplex" where the router says
	Speed     string     `json:"speed"`  // "1Gbps"
	Duplex    string     `json:"duplex"` // full | half | ""
	Disabled  bool       `json:"disabled"`
	MAC       string     `json:"mac"`
	RxBps     *int64     `json:"rx_bps"`
	TxBps     *int64     `json:"tx_bps"`
	Comment   string     `json:"comment"`
	Apply     *PortApply `json:"apply,omitempty"`
	Flats     *int       `json:"flats,omitempty"` // customer ports: flats seen on the port, when known
	// What the person asking may do.
	CanChange   bool     `json:"can_change"`
	NeedsOwner  bool     `json:"needs_owner"`
	RoleOptions []string `json:"role_options"`
	// Warnings before the action, "" when none. ConfirmName: the router name must be typed.
	DisableWarning string `json:"disable_warning,omitempty"`
	RoleWarning    string `json:"role_warning,omitempty"`
	ConfirmDisable bool   `json:"confirm_disable,omitempty"`
	// OwnerEnabled: the owner enabled the port (always true for the uplink and the admin port). Note is a fixed line
	// for the port's role ("Admin — ..."); Warning is a hint about the port's state.
	OwnerEnabled bool   `json:"owner_enabled"`
	Note         string `json:"note,omitempty"`
	Warning      string `json:"warning,omitempty"`
	// CanDisable is false for the OLT uplink: it is moved to another port or unplugged, never switched off here.
	CanDisable bool `json:"can_disable"`
	// Missing: the server stores a role for this port but the router has no physical port of that name any more.
	// Its role can still be cleared (set to unused). Bond marks a non-physical interface that holds the OLT role.
	Missing bool `json:"missing,omitempty"`
}

// PortsView is the answer of the Ports tab.
type PortsView struct {
	Items       []PortInfo
	GeneratedAt time.Time
	Reachable   bool
	Status      CustomerPortStatus
}

// portsManaged: the router is on the default-off policy (see PortsPolicyOff).
func portsManaged(s store.Site) bool { return s.PortsPolicy == PortsPolicyOff }

// roleOf is the role the server stores for a port name.
func roleOf(eff store.Site, name string) string {
	switch {
	case name == eff.WANInterface:
		return PortRoleWAN
	case containsStr(SplitPorts(eff.LANPorts), name):
		return PortRoleLAN
	case name == eff.OLTInterface:
		return PortRoleOLT
	case containsStr(SplitPorts(eff.CustomerPorts), name):
		return PortRoleCustomer
	}
	return PortRoleUnused
}

// PortKind says what sort of port a name is.
func PortKind(name, typ string) string {
	n := strings.ToLower(name)
	switch {
	case typ == "wlan" || typ == "wifi" || strings.HasPrefix(n, "wlan") || strings.HasPrefix(n, "wifi"):
		return "wlan"
	case strings.HasPrefix(n, "combo"):
		return "combo"
	case strings.HasPrefix(n, "sfp") || strings.HasPrefix(n, "qsfp"):
		return "sfp"
	}
	return "ethernet"
}

// ---- reading ------------------------------------------------------------------------------------

const (
	viewPortsFast = "ports-fast"
	viewPortsSlow = "ports-slow"
	portsFastTTL  = 4 * time.Second
	portsSlowTTL  = 60 * time.Second
)

// portsFastProps is the proplist of the cheap read: one /interface print.
var portsFastProps = []string{"name", "type", "running", "disabled", "comment", "mac-address", "rx-byte", "tx-byte"}

type portFast struct {
	rows []routeros.Row
	all  map[string]routeros.Row // every interface by name, physical or not (a bond can be the OLT uplink)
	rx   map[string]*int64
	tx   map[string]*int64
	at   time.Time
}

type portSlow struct {
	speed  map[string]string // port -> "1Gbps"
	duplex map[string]string
	hosts  map[string]map[string]bool // port -> canonical MACs seen on it
	hostOK bool
}

type portsRates struct {
	mu   sync.Mutex
	prev map[string]map[string]ifSample
}

func (m *Manager) readPortsFast(ctx context.Context, siteID string, rt routeros.Client) (any, error) {
	rows, err := rt.RowsProps(ctx, "/interface", nil, portsFastProps)
	if err != nil {
		return nil, err
	}
	now := m.now()
	pf := portFast{rx: map[string]*int64{}, tx: map[string]*int64{}, at: now, all: map[string]routeros.Row{}}
	m.portRates.mu.Lock()
	if m.portRates.prev == nil {
		m.portRates.prev = map[string]map[string]ifSample{}
	}
	prev := m.portRates.prev[siteID]
	cur := map[string]ifSample{}
	for _, r := range rows {
		if r["name"] != "" {
			pf.all[r["name"]] = r
		}
		if !physicalType(r["type"]) || r["name"] == "" {
			continue
		}
		pf.rows = append(pf.rows, r)
		rx, tx := atou(r["rx-byte"]), atou(r["tx-byte"])
		cur[r["name"]] = ifSample{rx: rx, tx: tx, at: now}
		if p, ok := prev[r["name"]]; ok {
			if dt := now.Sub(p.at).Seconds(); dt > 0 && rx >= p.rx && tx >= p.tx {
				pf.rx[r["name"]] = int64p(int64(float64(rx-p.rx) * 8 / dt))
				pf.tx[r["name"]] = int64p(int64(float64(tx-p.tx) * 8 / dt))
			}
		}
	}
	m.portRates.prev[siteID] = cur
	m.portRates.mu.Unlock()
	return pf, nil
}

// readPortsSlow reads the negotiated speed and duplex, and the bridge hosts
// (which MACs sit behind which port). Either may fail: the page just shows less.
func (m *Manager) readPortsSlow(ctx context.Context, rt routeros.Client) (any, error) {
	ps := portSlow{speed: map[string]string{}, duplex: map[string]string{}, hosts: map[string]map[string]bool{}}
	var wg sync.WaitGroup
	var eth, hosts []routeros.Row
	var hostErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		eth, _ = rt.RowsProps(ctx, "/interface/ethernet", nil, []string{"name", "speed", "rate", "full-duplex", "running"})
	}()
	go func() {
		defer wg.Done()
		hosts, hostErr = rt.RowsProps(ctx, "/interface/bridge/host", nil, []string{"mac-address", "on-interface", "local"})
	}()
	wg.Wait()
	for _, r := range eth {
		if !yes(r["running"]) && r["rate"] == "" {
			continue
		}
		ps.speed[r["name"]] = firstNonEmpty(r["rate"], r["speed"])
		switch {
		case r["full-duplex"] == "":
		case isYes(r["full-duplex"]):
			ps.duplex[r["name"]] = "full"
		default:
			ps.duplex[r["name"]] = "half"
		}
	}
	if hostErr == nil {
		ps.hostOK = true
		for _, h := range hosts {
			if isYes(h["local"]) {
				continue
			}
			mac, ok := store.CanonMAC(h["mac-address"])
			if !ok {
				continue
			}
			if ps.hosts[h["on-interface"]] == nil {
				ps.hosts[h["on-interface"]] = map[string]bool{}
			}
			ps.hosts[h["on-interface"]][mac] = true
		}
	}
	return ps, nil
}

// Ports lists every physical port with its role. viewerRole decides what the
// person may do (the actions column); the router is read once per refresh.
func (m *Manager) Ports(ctx context.Context, siteID, viewerRole string) (PortsView, error) {
	site, err := m.liveSite(siteID)
	if err != nil {
		return PortsView{}, err
	}
	rt := m.Router(siteID)
	v, reach, ferr := m.liveGetTTL(ctx, siteID, viewPortsFast, portsFastTTL, func(c context.Context) (any, error) {
		return m.readPortsFast(c, siteID, rt)
	})
	out := PortsView{Items: []PortInfo{}, GeneratedAt: m.now().UTC(), Status: m.CustomerPortStatus(siteID)}
	pf, ok := v.(portFast)
	if ferr != nil || !ok {
		return out, nil
	}
	out.Reachable, out.GeneratedAt = reach, pf.at.UTC()
	var ps portSlow
	if sv, _, serr := m.liveGetTTL(ctx, siteID, viewPortsSlow, portsSlowTTL, func(c context.Context) (any, error) {
		return m.readPortsSlow(c, rt)
	}); serr == nil {
		ps, _ = sv.(portSlow)
	}
	flats := m.flatsPerPort(siteID, ps)
	apply := m.PortApplyStates(siteID)
	disabledByUs := SplitPorts(site.DisabledPorts)
	for _, r := range pf.rows {
		name := r["name"]
		role := roleOf(site, name)
		pi := PortInfo{Name: name, Kind: PortKind(name, r["type"]), Role: role, RoleLabel: PortRoleLabels[role], Locked: role == PortRoleWAN,
			Running: isYes(r["running"]), Disabled: isYes(r["disabled"]) || containsStr(disabledByUs, name), MAC: r["mac-address"], Comment: r["comment"],
			RxBps: pf.rx[name], TxBps: pf.tx[name], Speed: ps.speed[name], Duplex: ps.duplex[name]}
		if pi.Disabled && !isYes(r["disabled"]) {
			pi.Disabled = false // the router says it is on; the sweep will switch it off again
		}
		pi.OwnerEnabled = role == PortRoleWAN || role == PortRoleLAN || !portsManaged(site) || containsStr(SplitPorts(site.EnabledPorts), name)
		if !pi.OwnerEnabled {
			pi.Disabled = true // not enabled by the owner: the router switches it off at the next check, whatever it says now
		}
		if role == PortRoleLAN {
			pi.Note = AdminPortNote
		}
		if role == PortRoleUnused && pi.Running && !pi.Disabled {
			pi.Warning = NoRoleUpWarn
		}
		if !pi.Running || pi.Disabled {
			pi.Speed, pi.Duplex = "", ""
		}
		pi.Link = strings.TrimSpace(pi.Speed + " " + map[string]string{"full": "full duplex", "half": "half duplex"}[pi.Duplex])
		if a, ok := apply[name]; ok && role != PortRoleUnused {
			a := a
			pi.Apply = &a
		}
		if role == PortRoleCustomer && ps.hostOK {
			n := flats[name]
			pi.Flats = &n
		}
		m.decoratePort(&pi, site, viewerRole)
		out.Items = append(out.Items, pi)
	}
	// Ports that hold a stored role but have no physical row: an OLT uplink that is a bond, and any port that
	// vanished from the router (the tab would otherwise hide which port holds the role and offer no way to clear it).
	listed := map[string]bool{}
	for _, it := range out.Items {
		listed[it.Name] = true
	}
	stored := append(append(SplitPorts(site.LANPorts), SplitPorts(site.CustomerPorts)...), site.OLTInterface)
	for _, name := range stored {
		if name == "" || listed[name] {
			continue
		}
		listed[name] = true
		role := roleOf(site, name)
		pi := PortInfo{Name: name, Role: role, RoleLabel: PortRoleLabels[role], Kind: PortKind(name, ""), RoleOptions: []string{}}
		if r, ok := pf.all[name]; ok {
			pi.Kind = r["type"]
			pi.Running, pi.Disabled, pi.MAC, pi.Comment = isYes(r["running"]), isYes(r["disabled"]), r["mac-address"], r["comment"]
			pi.Comment = strings.TrimSpace(pi.Comment)
			pi.Link = ""
			pi.NeedsOwner = role == PortRoleOLT || role == PortRoleLAN
		} else {
			pi.Missing = true
			pi.Comment = "The router has no port with this name any more."
			pi.NeedsOwner = role == PortRoleLAN
			pi.CanChange = roleRank(viewerRole) >= 1 && (!pi.NeedsOwner || roleRank(viewerRole) >= 2)
			if role == PortRoleCustomer || (role == PortRoleLAN && len(SplitPorts(site.LANPorts)) > 1) {
				if pi.CanChange {
					pi.RoleOptions = []string{PortRoleUnused}
				}
			}
		}
		out.Items = append(out.Items, pi)
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return naturalLess(out.Items[i].Name, out.Items[j].Name) })
	return out, nil
}

// flatsPerPort counts the open flats of the building whose ONT MAC is seen behind each port.
func (m *Manager) flatsPerPort(siteID string, ps portSlow) map[string]int {
	out := map[string]int{}
	if !ps.hostOK {
		return out
	}
	macs := map[string]bool{}
	m.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.SiteID != siteID {
				continue
			}
			if c, ok := store.CanonMAC(a.MAC); ok {
				macs[c] = true
			}
		}
	})
	for port, seen := range ps.hosts {
		for mac := range seen {
			if macs[mac] {
				out[port]++
			}
		}
	}
	return out
}

// decoratePort fills what a person may do with the port and the warnings.
func (m *Manager) decoratePort(pi *PortInfo, site store.Site, viewerRole string) {
	pi.RoleOptions = []string{}
	rank := roleRank(viewerRole)
	switch pi.Role {
	case PortRoleWAN:
		return // locked: no actions
	case PortRoleLAN, PortRoleOLT:
		pi.NeedsOwner = true
		pi.CanChange = rank >= 2
	default:
		pi.CanChange = rank >= 1
	}
	pi.CanDisable = pi.CanChange && !(pi.Role == PortRoleOLT && !pi.Disabled)
	pi.ConfirmDisable = pi.Role == PortRoleLAN
	switch pi.Role {
	case PortRoleLAN:
		pi.DisableWarning, pi.RoleWarning = WarnLAN, WarnLAN
	case PortRoleOLT:
		pi.DisableWarning, pi.RoleWarning = OLTNoOffMsg, WarnOLT
	case PortRoleCustomer:
		if pi.Flats != nil && *pi.Flats > 0 {
			pi.DisableWarning = fmt.Sprintf("%d house(s) with leases are seen on this port and go offline.", *pi.Flats)
		}
	}
	if !pi.CanChange {
		return
	}
	wired := pi.Kind != "wlan"
	if !wired {
		if pi.Role != PortRoleUnused {
			pi.RoleOptions = []string{PortRoleUnused}
		}
		return
	}
	for _, r := range []string{PortRoleLAN, PortRoleOLT, PortRoleCustomer, PortRoleUnused} {
		if r == pi.Role {
			continue
		}
		if (r == PortRoleLAN || r == PortRoleOLT) && roleRank(viewerRole) < 2 {
			continue
		}
		if pi.Role == PortRoleOLT {
			continue // the OLT uplink moves by giving the role to another port
		}
		if pi.Role == PortRoleLAN && len(SplitPorts(site.LANPorts)) <= 1 {
			continue // never the last admin LAN port
		}
		pi.RoleOptions = append(pi.RoleOptions, r)
	}
}

// ---- changing -----------------------------------------------------------------------------------

// PortActor says who asks.
type PortActor struct{ Name, Role string }

// PortChange is a change request for one port.
type PortChange struct {
	Role        *string // lan | olt | customer | unused
	Disabled    *bool
	ConfirmName string
}

// PortResult says what was done.
type PortResult struct {
	Port    PortInfo
	Applied []string
}

// ifaceRows reads the router's interfaces once (name, type, disabled, running), keyed by name.
func (m *Manager) ifaceRows(ctx context.Context, rt routeros.Client) (map[string]routeros.Row, error) {
	rows, err := rt.RowsProps(ctx, "/interface", nil, []string{"name", "type", "disabled", "running"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRouterUnreachable, shortErr(err))
	}
	out := map[string]routeros.Row{}
	for _, r := range rows {
		out[r["name"]] = r
	}
	return out, nil
}

// physicalRow is the port's own row out of ifaces.
func physicalRow(ifaces map[string]routeros.Row, name string) (routeros.Row, error) {
	if r, ok := ifaces[name]; ok && physicalType(r["type"]) {
		return r, nil
	}
	return nil, portErr(PortErrNoPort, "the router has no physical port called %s", name)
}

// portUsable: enabled with a link (a cable that carries traffic).
func portUsable(r routeros.Row) bool { return r != nil && !isYes(r["disabled"]) && isYes(r["running"]) }

// lanSurvivor says whether at least one of remaining is an admin LAN port that is enabled, has a link and
// (when needMember) is already a member of br-lan. Without one, a change to the admin LAN could cut the
// server and WinBox off with no way back, so it is refused. A router that cannot be read refuses too.
func (m *Manager) lanSurvivor(ctx context.Context, rt routeros.Client, ifaces map[string]routeros.Row, remaining []string, needMember bool) bool {
	member := map[string]bool{}
	if needMember {
		rows, err := rt.RowsProps(ctx, "/interface/bridge/port", map[string]string{"bridge": LANBridge}, []string{"interface", "bridge", "disabled"})
		if err != nil {
			return false
		}
		for _, r := range rows {
			if r["bridge"] == LANBridge && !isYes(r["disabled"]) {
				member[r["interface"]] = true
			}
		}
	}
	for _, p := range remaining {
		if portUsable(ifaces[p]) && (!needMember || member[p]) {
			return true
		}
	}
	return false
}

const lastLANMsg = "%s is the last Admin LAN port that is switched on, connected and in the admin bridge. Connect and enable another Admin LAN port first, so you do not lose the router."

func nameConfirmed(site store.Site, typed string) bool {
	return strings.TrimSpace(typed) == site.Name
}

func removeStr(l []string, s string) []string {
	var out []string
	for _, x := range l {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// ChangePort enables, disables or gives a role to one port. See the package
// notes above for the rules; every refusal is a *PortRefusal.
func (m *Manager) ChangePort(ctx context.Context, siteID, port string, ch PortChange, who PortActor) (PortResult, error) {
	var res PortResult
	if ch.Role == nil && ch.Disabled == nil {
		return res, portErr(PortErrInvalid, "nothing to change: give a role or disabled")
	}
	if !ValidIface(port) {
		return res, portErr(PortErrInvalid, "port %q: use letters, digits, dot, dash and underscore only", port)
	}
	rank := roleRank(who.Role)
	if rank < 1 {
		return res, portErr(PortErrForbidden, "your role may not change the router")
	}
	e := m.cport.entry(siteID)
	if err := e.run.LockCtx(ctx); err != nil { // the same per-router lock as the sweep; nothing is read before it is held
		return res, portErr(PortErrConflict, "%s", ErrRouterBusy.Error())
	}
	defer e.run.Unlock()
	var site store.Site
	var found bool
	m.st.View(func(d *store.Data) { site, found = d.Sites[siteID] })
	if !found || site.Removed {
		return res, billing.ErrNoSite
	}
	eff := m.Effective(site)
	cur := roleOf(eff, port)
	if cur == PortRoleWAN {
		return res, portErr(PortErrLocked, "%s is the Internet uplink (WAN). The server never changes it; it is set with the router's Network settings (owner).", port)
	}
	target := ""
	if ch.Role != nil {
		target = strings.ToLower(strings.TrimSpace(*ch.Role))
		switch target {
		case PortRoleWAN:
			return res, portErr(PortErrLocked, "The Internet uplink (WAN) is not set here: it is part of the router's Network settings (owner) and a new line.")
		case PortRoleLAN, PortRoleOLT, PortRoleCustomer, PortRoleUnused:
		default:
			return res, portErr(PortErrInvalid, "role %q: use lan, olt, customer or unused", target)
		}
	}
	if !site.Runnable() {
		return res, portErr(PortErrConflict, "the router is not set up yet")
	}
	rt := m.Router(siteID)
	ifaces, err := m.ifaceRows(ctx, rt)
	if err != nil {
		return res, err
	}
	row, err := physicalRow(ifaces, port)
	if err != nil {
		// A port the server stores a role for that is gone from the router: its role can still be cleared.
		var pe *PortRefusal
		if errors.As(err, &pe) && pe.Kind == PortErrNoPort && target == PortRoleUnused && ch.Disabled == nil && (cur == PortRoleCustomer || cur == PortRoleLAN) {
			if cur == PortRoleLAN {
				if rank < 2 {
					return res, portErr(PortErrForbidden, "only an owner may change the admin LAN or the OLT uplink")
				}
				if len(SplitPorts(eff.LANPorts)) <= 1 {
					return res, portErr(PortErrLastLAN, "%s is the last Admin LAN port. Make another port an Admin LAN port first.", port)
				}
			}
			if err := m.setPortRole(siteID, port, cur, target, who); err != nil {
				return res, err
			}
			m.QueueCustomerPort(siteID)
			m.chk.drop(siteID)
			m.dropPortsView(siteID)
			return res, nil
		}
		return res, err
	}
	kind := PortKind(port, row["type"])
	roleChange := target != "" && target != cur
	if roleChange && kind == "wlan" && target != PortRoleUnused {
		return res, portErr(PortErrInvalid, "%s is a wireless interface: only a wired port can be an admin LAN, OLT or customer port", port)
	}
	// who may: owners for anything on the admin LAN or the OLT uplink
	touchesCore := func(r string) bool { return r == PortRoleLAN || r == PortRoleOLT }
	needOwner := (roleChange && (touchesCore(cur) || touchesCore(target))) || (ch.Disabled != nil && touchesCore(cur))
	if needOwner && rank < 2 {
		return res, portErr(PortErrForbidden, "only an owner may change the admin LAN or the OLT uplink")
	}
	// refusals that no confirmation can lift
	lans := SplitPorts(eff.LANPorts)
	if roleChange {
		switch {
		case cur == PortRoleOLT:
			return res, portErr(PortErrConflict, "%s is the OLT uplink: give the OLT uplink role to another port instead", port)
		case cur == PortRoleLAN && len(lans) <= 1:
			return res, portErr(PortErrLastLAN, "%s is the last Admin LAN port. Make another port an Admin LAN port first.", port)
		case cur == PortRoleLAN && !m.lanSurvivor(ctx, rt, ifaces, removeStr(lans, port), true):
			return res, portErr(PortErrLastLAN, lastLANMsg, port)
		case (target == PortRoleLAN || target == PortRoleOLT) && (isYes(row["disabled"]) || containsStr(SplitPorts(site.DisabledPorts), port) || (portsManaged(site) && !containsStr(SplitPorts(site.EnabledPorts), port))):
			return res, portErr(PortErrConflict, "%s is switched off. Enable it first.", port)
		}
	}
	if ch.Disabled != nil && *ch.Disabled {
		switch cur {
		case PortRoleOLT:
			return res, portErr(PortErrOLTOff, "%s", OLTNoOffMsg)
		case PortRoleLAN:
			if len(lans) <= 1 || !m.lanSurvivor(ctx, rt, ifaces, removeStr(lans, port), true) {
				return res, portErr(PortErrLastLAN, lastLANMsg, port)
			}
		}
	}
	// the typed router name
	confirmWhy := ""
	switch {
	case roleChange && cur == PortRoleLAN:
		confirmWhy = WarnLAN
	case roleChange && target == PortRoleOLT:
		confirmWhy = WarnOLT
	case ch.Disabled != nil && *ch.Disabled && cur == PortRoleLAN:
		confirmWhy = WarnLAN
	}
	if confirmWhy != "" && !nameConfirmed(site, ch.ConfirmName) {
		return res, &PortRefusal{Kind: PortErrConfirm, Msg: "type the router name to confirm. " + confirmWhy, Warning: confirmWhy}
	}

	wantDisable := ch.Disabled != nil && (*ch.Disabled != isYes(row["disabled"]) || *ch.Disabled != containsStr(SplitPorts(site.DisabledPorts), port) ||
		(portsManaged(site) && *ch.Disabled == containsStr(SplitPorts(site.EnabledPorts), port)))
	endRole := cur
	if roleChange {
		endRole = target
	}
	// only customer and unused ports are kept off by the sweep; a LAN port switched off is a one-off
	keep := endRole == PortRoleCustomer || endRole == PortRoleUnused
	if roleChange && wantDisable {
		// Both in one request: the router write is the part that can fail for reasons nobody can check up front,
		// so it goes first; the role is stored only when it worked, and the router is put back if the role is refused.
		if err := m.setPortDisabled(ctx, rt, eff, row, port, *ch.Disabled, keep, who, &res.Applied); err != nil {
			return res, err
		}
		if err := m.setPortRole(siteID, port, cur, target, who); err != nil {
			prev := isYes(row["disabled"])
			var undone []string
			if uerr := m.setPortDisabled(ctx, rt, eff, row, port, prev, keep && prev, who, &undone); uerr != nil {
				m.log.Warn("could not put the port back after a refused role change", "site", siteID, "port", port, "err", uerr)
			}
			res.Applied = nil
			return res, err
		}
		m.QueueCustomerPort(siteID)
		m.chk.drop(siteID)
		m.dropPortsView(siteID)
		return res, nil
	}
	if roleChange {
		if err := m.setPortRole(siteID, port, cur, target, who); err != nil {
			return res, err
		}
		m.QueueCustomerPort(siteID)
	}
	if wantDisable {
		if err := m.setPortDisabled(ctx, rt, eff, row, port, *ch.Disabled, keep, who, &res.Applied); err != nil {
			return res, err
		}
	}
	m.dropPortsView(siteID)
	m.chk.drop(siteID)
	return res, nil
}

// setPortRole stores the new role: one role per port, WAN, admin LAN, OLT and
// customer ports never overlap (ValidateNet says so again before it is saved).
func (m *Manager) setPortRole(siteID, port, cur, target string, who PortActor) error {
	var perr error
	var oltChanged bool
	err := m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		eff := m.Effective(s)
		lans, custs, olt := SplitPorts(eff.LANPorts), SplitPorts(eff.CustomerPorts), eff.OLTInterface
		lans, custs = removeStr(lans, port), removeStr(custs, port)
		oldOLT := olt
		switch target {
		case PortRoleLAN:
			lans = append(lans, port)
		case PortRoleCustomer:
			custs = append(custs, port)
		case PortRoleOLT:
			olt = port
		}
		if len(lans) == 0 {
			perr = portErr(PortErrLastLAN, "%s is the last Admin LAN port. Make another port an Admin LAN port first.", port)
			return perr
		}
		s.LANPorts, s.CustomerPorts, s.OLTInterface = strings.Join(lans, ","), strings.Join(custs, ","), olt
		s.BenchPort = ""
		if target == PortRoleLAN || target == PortRoleOLT {
			s.DisabledPorts = strings.Join(removeStr(SplitPorts(s.DisabledPorts), port), ",")
		}
		if err := ValidateNetMgmt(ApplyDefaults(m.cfg, s), m.pfx, m.mgmt); err != nil {
			perr = err
			return err
		}
		oltChanged = oldOLT != olt
		if oltChanged && s.OLTCapMbps == 0 {
			// not set stays not set: the new port's type decides
		}
		d.Sites[siteID] = s
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Actor: who.Name,
			Msg: fmt.Sprintf("port %s: role %s -> %s", port, PortRoleLabels[cur], PortRoleLabels[target])})
		return nil
	})
	if err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			return portErr(PortErrConflict, "%s", fe.Msg)
		}
		var pe *PortRefusal
		if errors.As(err, &pe) {
			return pe
		}
		return err
	}
	if oltChanged {
		m.Reload(siteID)
	}
	return nil
}

// setPortDisabled switches one port on the router (through the allowlist) and records it.
func (m *Manager) setPortDisabled(ctx context.Context, rt routeros.Client, eff store.Site, row routeros.Row, port string, off, keep bool, who PortActor, applied *[]string) error {
	w := m.newFixWriter(rt, eff)
	w.offPorts[port] = true
	val := "no"
	if off {
		val = "yes"
	}
	if err := w.setRow(ctx, "/interface", row, map[string]string{"disabled": val}); err != nil {
		var fe *RouterRefusedError
		if errors.As(classifyFixErr(err), &fe) || errors.Is(err, ErrFixNotAllowed) {
			return classifyFixErr(err)
		}
		return fmt.Errorf("%w: %v", ErrRouterUnreachable, shortErr(err))
	}
	word := "enabled"
	if off {
		word = "disabled"
	}
	*applied = append(*applied, port+" "+word)
	return m.st.Update(func(d *store.Data) error {
		s := d.Sites[eff.ID]
		l := removeStr(SplitPorts(s.DisabledPorts), port)
		if off && keep {
			l = append(l, port)
		}
		s.DisabledPorts = strings.Join(l, ",")
		// What the owner enabled is what the sweep keeps on; everything else of a router on the policy stays off.
		en := removeStr(SplitPorts(s.EnabledPorts), port)
		if !off {
			en = append(en, port)
		}
		s.EnabledPorts = strings.Join(en, ",")
		d.Sites[eff.ID] = s
		d.AddEvent(store.Event{Time: m.now(), SiteID: eff.ID, Kind: "audit", Actor: who.Name, Msg: "port " + port + " " + word})
		return nil
	})
}

// dropPortsView forgets the cached fast read of the router's ports, so the answer to a change and the next poll
// show what the router has now (the cache would keep the old enabled/disabled state for up to portsFastTTL).
func (m *Manager) dropPortsView(siteID string) {
	m.live.mu.Lock()
	delete(m.live.ent, siteID+"|"+viewPortsFast)
	m.live.mu.Unlock()
}
