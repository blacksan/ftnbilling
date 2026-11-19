package sites

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// ErrCannotRender wraps the reasons a cutover or cleanup script cannot be made
// (router not joined, no PUBLIC_IP, a flat with no usable MAC...). The text is
// meant for the admin, so the web layer shows it.
var ErrCannotRender = errors.New("cannot make this script")

// renderLaterStage renders the cutover or cleanup script inside the join
// token's store update. It marks the token used and, for the cutover, gives
// every flat a fixed address first (so the script and the reconciler agree).
// The router must already be joined: no key is created and none is in the script.
func (m *Manager) renderLaterStage(d *store.Data, site store.Site, stage string, now time.Time, from string, noCheck, force bool, script *[]byte, siteID *string) error {
	if site.Mode != store.SiteModeWG || site.WGPublicKey == "" || site.TunnelIP == "" {
		return fmt.Errorf("%w: join the router first (the join stage), then cut over", ErrCannotRender)
	}
	if stage == StageCutover && !force && billing.SiteMark(d, site.ID, billing.MarkCutoverConfirmed) {
		return fmt.Errorf("%w: already cut over (the cutover was confirmed); changes are applied by the server now. Run it again only if you mean to (force)", ErrCannotRender)
	}
	eff := ApplyDefaults(m.cfg, site)
	in := ScriptInput{
		Site: eff, ServerPub: m.cfg.WGPublicKey, Endpoint: m.cfg.WGEndpoint, WGSubnet: m.pfx,
		PublicBase: m.cfg.PublicBaseURL, PublicIP: m.cfg.PublicIP, Generated: now, NoCheck: noCheck, Stage: stage, Force: force,
	}
	if stage == StageCutover {
		plan, err := PlanSubnet(eff.CustomerSubnet)
		if err != nil {
			return err
		}
		if _, err := billing.EnsureIPsLocked(d, site.ID, plan, m.foreignIPsOf(site.ID)); err != nil {
			return fmt.Errorf("%w: giving the flats their addresses: %v", ErrCannotRender, err)
		}
		enforce := site.AccessMode == store.AccessIPoE // observe mode never suspends anybody
		in.Observe = !enforce
		for _, a := range d.AccountList() {
			if a.SiteID != site.ID || a.MACConflict || a.MAC == "" || a.IP == "" {
				continue
			}
			if !plan.InStatic(a.IP) {
				return fmt.Errorf("%w: %s has the address %s outside the static range", ErrCannotRender, a.Flat, a.IP)
			}
			in.Accounts = append(in.Accounts, LeaseSeed{
				Code: a.Code, Flat: a.Flat, MAC: a.MAC, IP: a.IP, MaxLimit: billing.QueueLimit(d.Packages[a.PackageID]),
				Suspended: enforce && billing.AccountState(a, now) != billing.StateActive,
			})
		}
	}
	out, err := RenderJoinScript(in)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCannotRender, stage, err)
	}
	site.Join.UsedAt, site.Join.UsedFrom = now, from
	d.Sites[site.ID] = site
	d.AddEvent(store.Event{Time: now, SiteID: site.ID, Kind: "audit", Msg: fmt.Sprintf("%s script handed out (%d flats, mode %s)", stage, len(in.Accounts), modeName(site))})
	*script, *siteID = out, site.ID
	return nil
}

func modeName(s store.Site) string {
	if s.AccessMode == "" {
		return store.AccessHotspot
	}
	return s.AccessMode
}

// SetAccessMode switches a building between hotspot, ipoe_observe and ipoe.
// It does not touch the router: the reconciler (or the hotspot) takes over on
// its next pass. The building's collector is rebuilt, since IPoE buildings
// read usage from the flats' queues.
func (m *Manager) SetAccessMode(siteID, mode string) error {
	return m.SetAccessModeForce(siteID, mode, false)
}

// ErrRouterUnknown is returned when the hotspot mode is chosen and the router
// cannot be asked whether it serves the flats from ftn-dhcp. The check fails
// closed; the admin can override it.
var ErrRouterUnknown = errors.New("the router cannot be reached, so it is not known whether it still serves the flats from ftn-dhcp. Switching to hotspot now could break them. Fix the connection, or tick the override if you know the router is back on the hotspot")

// SetAccessModeForce is SetAccessMode with override: it skips the check of the
// router when the router cannot be reached.
func (m *Manager) SetAccessModeForce(siteID, mode string, override bool) error {
	switch mode {
	case store.AccessHotspot, store.AccessIPoEObserve, store.AccessIPoE:
	default:
		return store.ErrBadAccessMode
	}
	if mode == store.AccessHotspot {
		// The router decides: while it serves the flats from ftn-dhcp, "hotspot"
		// would make the server write hotspot users into a router that has
		// none. Roll the router back first (or let the 15 minute timer do it).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		on, err := m.ftnDHCPEnabled(ctx, siteID)
		switch {
		case err == nil && on:
			return ErrRouterIsIPoE
		case err != nil && !errors.Is(err, routeros.ErrNoRouter) && !override:
			return ErrRouterUnknown
		}
	}
	err := m.st.Update(func(d *store.Data) error {
		s, ok := d.Sites[siteID]
		if !ok || s.Removed {
			return billing.ErrNoSite
		}
		if modeName(s) == mode {
			return store.ErrNoChange
		}
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: fmt.Sprintf("access mode changed from %s to %s", modeName(s), mode)})
		if mode == store.AccessHotspot {
			billing.ClearCutoverMarks(d, siteID)
		}
		s.AccessMode = mode
		d.Sites[siteID] = s
		return nil
	})
	if err != nil {
		return err
	}
	m.Reload(siteID)
	return nil
}

// ErrRouterIsIPoE is returned when the hotspot mode is chosen for a building
// whose router is serving its flats from ftn-dhcp.
var ErrRouterIsIPoE = errors.New("the router is serving the flats from ftn-dhcp (IPoE): roll the router back first (run /system/script/run ftn-rollback on it), then switch the mode to hotspot")

func (m *Manager) ftnDHCPEnabled(ctx context.Context, siteID string) (bool, error) {
	servers, err := m.Router(siteID).Rows(ctx, "/ip/dhcp-server", map[string]string{"name": billing.DHCPServerName})
	if err != nil {
		return false, err
	}
	return len(servers) > 0 && servers[0]["disabled"] != "true" && servers[0]["disabled"] != "yes", nil
}

// ErrNotCutOver is returned by ConfirmCutover when the router does not run ftn-dhcp.
var ErrNotCutOver = errors.New("the router is not running the IPoE DHCP server (ftn-dhcp): it was not cut over, or it already rolled back. Nothing was confirmed")

// ConfirmCutover ends the safety net of a cutover: it removes the rollback
// timer from the router through the REST API (the script stays on the router
// for a manual rollback until the cleanup stage). It refuses unless the router
// is really serving the flats from ftn-dhcp.
func (m *Manager) ConfirmCutover(ctx context.Context, siteID string) error {
	rt := m.Router(siteID)
	servers, err := rt.Rows(ctx, "/ip/dhcp-server", map[string]string{"name": billing.DHCPServerName})
	if err != nil {
		return err
	}
	if len(servers) == 0 || servers[0]["disabled"] == "true" || servers[0]["disabled"] == "yes" {
		return ErrNotCutOver
	}
	// The kit's markers (server-run setup): confirming removes ftn-armed and ftn-undo.
	if armed, aerr := rt.Rows(ctx, pathAddrList, map[string]string{"list": KitListArmed}); aerr == nil && len(armed) > 0 {
		var site store.Site
		m.st.View(func(d *store.Data) { site = d.Sites[siteID] })
		j := &jobCtl{m: m, ctx: ctx, siteID: siteID, rt: rt, site: m.Effective(site), job: ProvJob{}}
		j.insp, _ = rt.(routeros.Inspector)
		j.newWriter()
		if err := j.disarm(ctx); err != nil {
			return err
		}
		_ = m.st.Update(func(d *store.Data) error {
			billing.SetSiteMarkLocked(d, siteID, billing.MarkCutoverConfirmed, true, m.now())
			billing.SetSiteMarkLocked(d, siteID, billing.MarkCutoverSeen, true, m.now())
			d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: "cutover confirmed: kit markers removed"})
			return nil
		})
		return nil
	}
	timers, err := rt.Rows(ctx, "/system/scheduler", map[string]string{"name": RollbackSchedule})
	if err != nil {
		return err
	}
	for _, r := range timers {
		if err := rt.RemoveRow(ctx, "/system/scheduler", r[routeros.RowID]); err != nil {
			return err
		}
	}
	ticks, err := rt.Rows(ctx, "/system/script", map[string]string{"name": RollbackTick})
	if err == nil {
		for _, r := range ticks {
			_ = rt.RemoveRow(ctx, "/system/script", r[routeros.RowID])
		}
	}
	_ = m.st.Update(func(d *store.Data) error {
		billing.SetSiteMarkLocked(d, siteID, billing.MarkCutoverConfirmed, true, m.now())
		billing.SetSiteMarkLocked(d, siteID, billing.MarkCutoverSeen, true, m.now())
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: fmt.Sprintf("cutover confirmed: rollback timer removed (%d)", len(timers))})
		return nil
	})
	return nil
}

// CutoverPending reports whether the router still has the rollback timer,
// that is, a cutover was run and not confirmed. ok is false when the router
// cannot be asked.
func (m *Manager) CutoverPending(ctx context.Context, siteID string) (pending, ok bool) {
	rt := m.Router(siteID)
	rows, err := rt.Rows(ctx, "/system/scheduler", map[string]string{"name": RollbackSchedule})
	if err != nil {
		return false, false
	}
	if len(rows) == 0 {
		if armed, e := rt.Rows(ctx, pathAddrList, map[string]string{"list": KitListArmed}); e == nil {
			return len(armed) > 0, true
		}
	}
	return len(rows) > 0, true
}

// UnseenEntitled lists the flats that have paid time (and a usable MAC) whose
// MAC is in neither the router's DHCP leases nor its ARP table: the cutover
// would leave them without service. It reads the router only.
func (m *Manager) UnseenEntitled(ctx context.Context, siteID string) ([]string, error) {
	rt := m.Router(siteID)
	seen := map[string]bool{}
	for _, path := range []string{"/ip/dhcp-server/lease", "/ip/arp"} {
		rows, err := rt.Rows(ctx, path, nil)
		if err != nil {
			if path == "/ip/arp" {
				continue // best effort
			}
			return nil, err
		}
		for _, r := range rows {
			if mac, ok := store.CanonMAC(r["mac-address"]); ok {
				seen[mac] = true
			}
		}
	}
	now := m.now()
	var out []string
	m.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID != siteID || a.MACConflict || a.MAC == "" || billing.AccountState(a, now) != billing.StateActive {
				continue
			}
			if mac, ok := store.CanonMAC(a.MAC); ok && !seen[mac] {
				out = append(out, fmt.Sprintf("%s (%s)", a.Flat, a.MAC))
			}
		}
	})
	return out, nil
}

// UnmanagedDevices lists what the cutover would cut off because the server
// does not know it: devices with an ARP entry on the customer network that
// are no flat's MAC and have no DHCP lease (an OLT's management address, an
// access point on a fixed address), and static leases on the old DHCP server
// that are not ours and not a flat's. Under reply-only ARP they would stop
// working; a forced cutover copies them ("ftn: keep") instead.
func (m *Manager) UnmanagedDevices(ctx context.Context, siteID string) ([]string, error) {
	var site store.Site
	m.st.View(func(d *store.Data) { site = d.Sites[siteID] })
	plan, err := PlanSubnet(m.Effective(site).CustomerSubnet)
	if err != nil {
		return nil, err
	}
	rt := m.Router(siteID)
	leases, err := rt.Rows(ctx, "/ip/dhcp-server/lease", nil)
	if err != nil {
		return nil, err
	}
	arp, aerr := rt.Rows(ctx, "/ip/arp", nil)
	if aerr != nil {
		arp = nil
	}
	flats := map[string]bool{}
	m.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == siteID && a.MAC != "" {
				flats[strings.ToUpper(a.MAC)] = true
			}
		}
	})
	leased := map[string]bool{}
	var out []string
	for _, r := range leases {
		mac, ok := store.CanonMAC(r["mac-address"])
		if !ok {
			continue
		}
		leased[mac] = true
		static := r["dynamic"] != "true" && r["dynamic"] != "yes"
		if static && r["server"] != billing.DHCPServerName && !strings.HasPrefix(r["comment"], "ftn:") && !flats[mac] {
			out = append(out, fmt.Sprintf("static lease %s %s", mac, r["address"]))
		}
	}
	for _, r := range arp {
		mac, ok := store.CanonMAC(r["mac-address"])
		if !ok || flats[mac] || leased[mac] || strings.HasPrefix(r["comment"], "ftn:") || !plan.InNetwork(r["address"]) {
			continue
		}
		out = append(out, fmt.Sprintf("device %s %s (no lease)", mac, r["address"]))
	}
	sort.Strings(out)
	return out, nil
}
