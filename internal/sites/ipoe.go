package sites

import (
	"context"
	"errors"
	"fmt"
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
func (m *Manager) renderLaterStage(d *store.Data, site store.Site, stage string, now time.Time, from string, noCheck bool, script *[]byte, siteID *string) error {
	if site.Mode != store.SiteModeWG || site.WGPublicKey == "" || site.TunnelIP == "" {
		return fmt.Errorf("%w: join the router first (the join stage), then cut over", ErrCannotRender)
	}
	eff := ApplyDefaults(m.cfg, site)
	in := ScriptInput{
		Site: eff, ServerPub: m.cfg.WGPublicKey, Endpoint: m.cfg.WGEndpoint, WGSubnet: m.pfx,
		PublicBase: m.cfg.PublicBaseURL, PublicIP: m.cfg.PublicIP, Generated: now, NoCheck: noCheck, Stage: stage,
	}
	if stage == StageCutover {
		plan, err := PlanSubnet(eff.CustomerSubnet)
		if err != nil {
			return err
		}
		if _, err := billing.EnsureIPsLocked(d, site.ID, plan); err != nil {
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
		if on, err := m.ftnDHCPEnabled(ctx, siteID); err == nil && on {
			return ErrRouterIsIPoE
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
		d.AddEvent(store.Event{Time: m.now(), SiteID: siteID, Kind: "audit", Msg: fmt.Sprintf("cutover confirmed: rollback timer removed (%d)", len(timers))})
		return nil
	})
	return nil
}

// CutoverPending reports whether the router still has the rollback timer,
// that is, a cutover was run and not confirmed. ok is false when the router
// cannot be asked.
func (m *Manager) CutoverPending(ctx context.Context, siteID string) (pending, ok bool) {
	rows, err := m.Router(siteID).Rows(ctx, "/system/scheduler", map[string]string{"name": RollbackSchedule})
	if err != nil {
		return false, false
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
