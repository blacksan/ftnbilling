package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/metrics"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// The Routers pages: the list, Add router, and the router page with its tabs
// (Overview, Setup, Clients, Interfaces, Plans, Profile, Danger).

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// ---- intent: is the router already serving customers? (D15) -------------------------------

func servingKey(siteID string) string { return "site." + siteID + ".serving_customers" }

// servingIntent is what the operator said when adding the router. Routers
// that predate the question are serving customers if they use the hotspot.
func (s *Server) servingIntent(st store.Site) bool {
	var v string
	var ok bool
	s.st.View(func(d *store.Data) {
		var set store.Setting
		set, ok = d.Settings[servingKey(st.ID)]
		v = set.Value
	})
	if ok {
		return v == "1"
	}
	return st.AccessMode == "" || st.AccessMode == store.AccessHotspot
}

func (s *Server) setServingIntent(siteID string, serving bool) error {
	v := "0"
	if serving {
		v = "1"
	}
	return s.st.Update(func(d *store.Data) error {
		d.Settings[servingKey(siteID)] = store.Setting{Key: servingKey(siteID), Value: v, UpdatedAt: s.svc.Now()}
		return nil
	})
}

// joinLines mints a one-time join line for the router. Routers that use the
// server-driven mode get the "join only" stage: the tunnel and API, nothing
// else changes on the router; the server does the rest. Hotspot routers keep
// the all-in-one script.
func (s *Server) joinLines(st store.Site) (cmd, nocheck string, exp time.Time, err error) {
	base := s.cfg.PublicBaseURL
	if billing.IPoE(st) || st.AccessMode == store.AccessIPoEObserve {
		tok, e, err := s.mgr.MintJoinStage(st.ID, sites.StageJoin)
		if err != nil {
			return "", "", time.Time{}, err
		}
		intent := sites.IntentFresh
		if s.servingIntent(st) {
			intent = sites.IntentServing
		}
		return sites.JoinCommandIntent(base, tok, true, sites.StageJoin, intent, false), sites.JoinCommandIntent(base, tok, false, sites.StageJoin, intent, false), e, nil
	}
	tok, e, err := s.mgr.MintJoin(st.ID)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return sites.JoinCommand(base, tok, true), sites.JoinCommand(base, tok, false), e, nil
}

// ---- list -----------------------------------------------------------------------------------

type rtRow struct {
	S                      store.Site
	State                  string // up | down | paused | waiting
	Mode, Setup, SetupTone string
	Flats, Active, Online  int
	WAN, Tunnel, Seen      string
	WANPct                 float64
	Attention              bool
	Detail                 string
}

func modeLabel(st store.Site) string {
	switch st.AccessMode {
	case store.AccessIPoE:
		return "Server-driven"
	case store.AccessIPoEObserve:
		return "Observing"
	}
	return "Hotspot"
}

func (s *Server) routerRows(ctx context.Context, sc scope) []rtRow {
	var list []store.Site
	for _, st := range sc.Sites {
		if sc.Has(st.ID) {
			list = append(list, st)
		}
	}
	states := s.siteStates(ctx, list)
	live := s.liveRows(sc)
	onl := map[string]int{}
	for _, row := range live.Rows {
		if row.Kind == "flat" {
			onl[row.SiteID]++
		}
	}
	base := map[string]siteRow{}
	for _, r := range s.siteRows() {
		base[r.S.ID] = r
	}
	now := s.svc.Now()
	var out []rtRow
	for _, st := range list {
		b := base[st.ID]
		r := rtRow{S: st, State: states[st.ID], Mode: modeLabel(st), Flats: b.Accounts, Active: b.Active, Online: onl[st.ID], Detail: b.Detail}
		if col := s.mgr.Collector(st.ID); col != nil {
			cfg, snap := col.Config(), col.Snapshot()
			if pt, ok := snap.Ports[cfg.WAN]; ok {
				rate := pt.RxBps
				if pt.TxBps > rate {
					rate = pt.TxBps
				}
				r.WAN = fmtBps(rate)
				if cfg.WANCapMbps > 0 {
					r.WANPct = clampPct(100 * rate / (cfg.WANCapMbps * 1e6))
				}
			}
		}
		if ps, found, known := s.mgr.PeerStatus(st.ID); known && found {
			if age, ok := ps.HandshakeAge(now); ok {
				r.Tunnel = "handshake " + shortAge(age)
				if r.State == "up" {
					r.Seen = "now"
				} else {
					r.Seen = shortAge(age) + " ago"
				}
			}
		}
		if r.State == "up" && r.Seen == "" {
			r.Seen = "now"
		}
		switch {
		case r.State == "paused":
			r.Setup, r.SetupTone = "Paused", "paused"
		case r.State == "waiting":
			r.Setup, r.SetupTone = "Waiting to join", "waiting"
		case billing.IPoE(st):
			ps := s.svc.ProvisionStatus(st.ID)
			if ps.InSync {
				r.Setup, r.SetupTone = "Live", "live"
			} else {
				r.Setup, r.SetupTone = fmt.Sprintf("Drift %d", ps.Drift), "drift"
				r.Attention = true
			}
		case st.AccessMode == store.AccessIPoEObserve:
			r.Setup, r.SetupTone = "Observing", "info"
		default:
			r.Setup, r.SetupTone = "Hotspot", "info"
		}
		if r.State == "down" {
			r.Attention = true
		}
		out = append(out, r)
	}
	return out
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func (s *Server) adminSites(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "sites")
	sc := scope{All: true, Sites: s.scopeOf(r).Sites} // the router list always shows every router
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	all := s.routerRows(ctx, sc)
	tab := r.URL.Query().Get("tab")
	cnt := map[string]int{"all": len(all)}
	var rows []rtRow
	var online, offline, setting, attn, flatsOn int
	for _, x := range all {
		isSetting := x.State == "waiting"
		switch {
		case x.State == "up":
			online++
		case x.State == "down":
			offline++
		}
		if isSetting {
			setting++
		}
		if x.Attention {
			attn++
		}
		flatsOn += x.Online
		match := tab == "" || tab == "all" ||
			(tab == "online" && x.State == "up") || (tab == "offline" && x.State == "down") || (tab == "setting" && isSetting)
		if match {
			rows = append(rows, x)
		}
	}
	cnt["online"], cnt["offline"], cnt["setting"] = online, offline, setting
	if tab == "" {
		tab = "all"
	}
	p.Data["Rows"], p.Data["Tab"], p.Data["Cnt"], p.Data["All"] = rows, tab, cnt, len(all)
	p.Data["Online"], p.Data["Offline"], p.Data["FlatsOn"], p.Data["Attn"] = online, offline, flatsOn, attn
	p.Data["AutoOpen"] = r.URL.Query().Get("add") == "1" && p.CanWrite
	var notices []fleetTile
	if p.CanWrite && p.HasOps {
		tiles, _ := s.fleetTiles(sc)
		for _, t := range tiles {
			if t.Notice {
				notices = append(notices, t)
			}
		}
	}
	p.Data["Notices"] = notices
	s.render(w, "admin_sites", p)
}

// adminSiteNew keeps the old "add" address working: the form is a dialog now.
func (s *Server) adminSiteNew(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Set("add", "1")
	r.URL.RawQuery = q.Encode()
	s.adminSites(w, r)
}

// ---- add router ---------------------------------------------------------------------------------

func (s *Server) adminSiteCreate(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		if wantsJSON(r) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": msg})
			return
		}
		s.back(w, r, "/admin/routers?add=1", "Error: "+msg)
	}
	serving := r.PostFormValue("serving") == "1"
	st, err := s.mgr.CreateSite(siteInput(r))
	if err != nil {
		fail(err.Error())
		return
	}
	_ = s.setServingIntent(st.ID, serving)
	if !serving {
		// D3: a router with no customers is set up by the server, so it starts in
		// the server-driven mode. A router that serves customers stays as it is
		// until it is switched over.
		_ = s.mgr.SetAccessModeForce(st.ID, store.AccessIPoE, true)
	}
	st, _ = s.siteByID(st.ID)
	if !wantsJSON(r) {
		// Without JavaScript: the router page, where the Setup tab makes the join line.
		s.back(w, r, "/admin/routers/view?id="+url.QueryEscape(st.ID)+"&tab=setup", "Router added. Make its join line below.")
		return
	}
	cmd, noCheck, exp, err := s.joinLines(st)
	if err != nil {
		if wantsJSON(r) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": st.ID, "name": st.Name, "url": "/admin/routers/view?id=" + url.QueryEscape(st.ID) + "&tab=setup",
				"note": "Router created, but no join line could be made: " + err.Error()})
			return
		}
		s.back(w, r, "/admin/routers/view?id="+st.ID+"&tab=setup", "Router added. "+err.Error())
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": st.ID, "name": st.Name, "command": cmd, "command_nocheck": noCheck, "expires_at": exp,
			"url": "/admin/routers/view?id=" + url.QueryEscape(st.ID) + "&tab=setup"})
		return
	}
}

func (s *Server) siteByID(id string) (store.Site, bool) {
	var st store.Site
	var ok bool
	s.st.View(func(d *store.Data) { st, ok = d.Sites[id]; ok = ok && !st.Removed })
	return st, ok
}

// ---- the router page ------------------------------------------------------------------------------

type tabDef struct{ Key, Label string }

var routerTabs = []tabDef{{"overview", "Overview"}, {"setup", "Setup"}, {"clients", "Clients"}, {"interfaces", "Interfaces"}, {"plans", "Plans"}, {"profile", "Profile"}, {"danger", "Danger"}}

type rtStats struct{ CPU, Mem, MemPct, Uptime, WAN, WANSub, Tunnel, TunnelSub, FlatsOn, FlatsSub string }

type ifRow struct {
	Name, Role, Rx, Tx string
	Running            bool
	Seen               string
}

type clientRow struct {
	A       store.Account
	Pkg     string
	State   string
	IP, Dn  string
	Up      string
	Online  bool
	Sync    string
	SyncCls string
}

// siteView builds the router page; cmd and noCheck are set only in the
// response to a join request, which is the one time the line is shown.
func (s *Server) siteView(w http.ResponseWriter, r *http.Request, st store.Site, cmd, cmdNoCheck string, exp time.Time, stage string, extra ...map[string]any) {
	p := s.page(r, "sites")
	tab := r.URL.Query().Get("tab")
	valid := false
	for _, t := range routerTabs {
		valid = valid || t.Key == tab
	}
	if !valid {
		tab = "overview"
	}
	if cmd != "" || len(extra) > 0 {
		tab = "setup"
	}
	eff := s.mgr.Effective(st)
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	status, _ := s.mgr.Status(ctx, st.ID)
	p.Data["Site"], p.Data["Eff"], p.Data["Status"] = st, eff, status
	p.Data["Env"] = st.Mode == store.SiteModeEnv
	for _, m := range extra {
		for k, v := range m {
			p.Data[k] = v
		}
	}
	p.Data["Cmd"], p.Data["CmdNoCheck"], p.Data["CmdExpires"], p.Data["CmdStage"] = cmd, cmdNoCheck, exp, stage
	mode := st.AccessMode
	if mode == "" {
		mode = store.AccessHotspot
	}
	p.Data["Mode"], p.Data["IPoE"] = mode, billing.IPoE(st)
	p.Data["ModeLabel"] = modeLabel(st)
	p.Data["Serving"] = s.servingIntent(st)
	if billing.IPoE(st) {
		p.Data["Prov"] = s.svc.ProvisionStatus(st.ID)
		if pend, ok := s.mgr.CutoverPending(ctx, st.ID); ok {
			p.Data["CutoverPending"] = pend
		}
	}
	if jr, ok := s.mgr.JoinReport(st.ID); ok {
		p.Data["JoinReport"] = jr
	}
	p.Data["Insecure"] = !strings.HasPrefix(s.cfg.PublicBaseURL, "https://")
	p.Data["AllowInsecure"] = s.cfg.AllowInsecureJoin
	p.Data["TTL"] = int(s.cfg.JoinTokenTTL.Minutes())
	p.Data["Portal"] = s.portalBase(r) + "/portal?site=" + url.QueryEscape(st.ID)
	var pkgs []store.Package
	var accts int
	s.st.View(func(d *store.Data) {
		pkgs = d.PackageList()
		for _, a := range d.Accounts {
			if a.SiteID == st.ID {
				accts++
			}
		}
	})
	p.Data["Pkgs"], p.Data["Accounts"] = pkgs, accts
	if rep, ok := s.svc.Building(st.ID); ok {
		p.Data["Rep"] = rep
	}
	p.Data["Roles"] = []string{store.RoleLandlord, store.RoleCaretaker, store.RoleAgent, store.RoleSecurity, store.RoleOther}

	// header
	p.Detail = st.Name
	p.H1a, p.H1b = "Router", st.Name
	p.Lead = ""
	p.Acts = nil
	p.Data["Tab"], p.Data["Tabs"] = tab, routerTabs
	states := s.siteStates(ctx, []store.Site{st})
	p.Data["State"] = states[st.ID]
	p.Data["Stats"] = s.routerStats(st, status)
	switch tab {
	case "clients":
		p.Data["Clients"], p.Data["Unreg"] = s.routerClients(st)
	case "interfaces":
		p.Data["Ifaces"] = s.routerInterfaces(st)
	}
	if cmd != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.render(w, "admin_site_view", p)
}

func (s *Server) routerStats(st store.Site, status sites.SiteStatus) rtStats {
	var x rtStats
	x.CPU, x.Mem, x.Uptime, x.WAN, x.Tunnel, x.FlatsOn = "n/a", "n/a", "n/a", "n/a", "n/a", "0"
	col := s.mgr.Collector(st.ID)
	if col != nil {
		cfg, snap := col.Config(), col.Snapshot()
		if !snap.SysAt.IsZero() {
			x.CPU = fmt.Sprintf("%d%%", snap.Sys.CPULoad)
			if snap.Sys.TotalMem > 0 {
				used := snap.Sys.TotalMem - snap.Sys.FreeMem
				x.MemPct = fmt.Sprintf("%.0f%%", 100*float64(used)/float64(snap.Sys.TotalMem))
				x.Mem = x.MemPct
			}
			x.Uptime = snap.Sys.Uptime
		}
		if pt, ok := snap.Ports[cfg.WAN]; ok {
			x.WAN = fmtBps(pt.RxBps)
			x.WANSub = "down now, " + fmtBps(pt.TxBps) + " up"
		}
		for _, lp := range snap.Ping {
			if lp.RTTms > 0 {
				x.Tunnel = fmt.Sprintf("%.0f ms", lp.RTTms)
				x.TunnelSub = "router to the internet"
				break
			}
		}
	}
	if status.Handshake {
		if x.Tunnel == "n/a" {
			x.Tunnel = "up"
		}
		x.TunnelSub = fmt.Sprintf("handshake %d s ago", status.HandshakeAge)
	}
	ls := s.liveRows(scope{ID: st.ID, Sites: []store.Site{st}})
	x.FlatsOn = fmt.Sprint(ls.FlatsOnline)
	return x
}

// routerClients lists the router's flats with their live rate and sync state.
// W9 hook: when internal/sites/live.go lands, replace this with its Clients
// call (merged leases, usage today and month, port) and keep the same columns.
func (s *Server) routerClients(st store.Site) ([]clientRow, []billing.UnregDevice) {
	now := s.svc.Now()
	live := map[string]liveRow{}
	for _, row := range s.liveRows(scope{ID: st.ID, Sites: []store.Site{st}}).Rows {
		if row.Kind == "flat" {
			live[row.Code] = row
		}
	}
	var rows []clientRow
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID != st.ID {
				continue
			}
			c := clientRow{A: a, Pkg: d.Packages[a.PackageID].Name, State: billing.AccountState(a, now)}
			if lr, ok := live[a.Code]; ok {
				c.Online, c.IP, c.Dn, c.Up = true, lr.IP, fmtBps(lr.Down), fmtBps(lr.Up)
			} else {
				c.IP = a.IP
			}
			rows = append(rows, c)
		}
	})
	for i := range rows {
		rs := s.svc.AccountRouterState(rows[i].A.ID)
		switch {
		case rs.PendingTask:
			rows[i].Sync, rows[i].SyncCls = "pending", "pending"
		case !rs.InSync:
			rows[i].Sync, rows[i].SyncCls = "drift", "drift"
		default:
			rows[i].Sync, rows[i].SyncCls = "in sync", "synced"
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Online != rows[j].Online {
			return rows[i].Online
		}
		return strings.ToLower(rows[i].A.Flat) < strings.ToLower(rows[j].A.Flat)
	})
	return rows, s.svc.Unregistered(st.ID, true)
}

// routerInterfaces lists the ports the collector samples. W9 hook: replace
// with sites.Interfaces (all interfaces with roles, addresses, MAC, MTU).
func (s *Server) routerInterfaces(st store.Site) []ifRow {
	col := s.mgr.Collector(st.ID)
	if col == nil {
		return nil
	}
	cfg, snap := col.Config(), col.Snapshot()
	var out []ifRow
	for name, pt := range snap.Ports {
		role := "other"
		switch name {
		case cfg.WAN:
			role = "WAN"
		case cfg.OLT:
			role = "OLT"
		}
		out = append(out, ifRow{Name: name, Role: role, Running: pt.Running, Rx: fmtBps(pt.RxBps), Tx: fmtBps(pt.TxBps), Seen: nowEAT(pt.At)})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Role == "other") != (out[j].Role == "other") {
			return out[j].Role == "other"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Server) adminSiteView(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.siteView(w, r, st, "", "", time.Time{}, "")
}

// ---- checklist and fixes (W5a), as session JSON ---------------------------------------------------

func (s *Server) adminSiteChecks(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	c, err := s.mgr.Checks(ctx, st.ID, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error(), "items": nil})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// adminSiteFix runs one fix from the checklist. A fix of kind "confirm" needs
// confirm=1 (the page asks first and shows the check's confirm_text).
func (s *Server) adminSiteFix(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _ := userOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := s.mgr.ApplyFix(ctx, st.ID, r.PostFormValue("check"), r.PostFormValue("confirm") == "1", u.Role)
	if err != nil {
		code := http.StatusConflict
		switch {
		case errors.Is(err, sites.ErrFixForbidden), errors.Is(err, sites.ErrNeedsOwner):
			code = http.StatusForbidden
		case errors.Is(err, sites.ErrUnknownCheck):
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": res.Applied, "item": res.Item})
}

var _ = metrics.ColDown
