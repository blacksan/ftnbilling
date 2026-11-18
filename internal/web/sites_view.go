package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// ---- join line ----------------------------------------------------------------------------------

// servingIntent is what the operator said when adding the router; the one
// store of it is the sites manager's (site.<id>.serving_customers), the same
// one POST /routers writes.
func (s *Server) servingIntent(st store.Site) bool { return s.mgr.ServingIntent(st) }

// joinLines mints a one-time join line for the router. A router in the
// server-driven mode gets the same line as POST /routers (the server builds
// the rest, the intent decides which device-mode features it asks for).
// Hotspot routers keep the all-in-one script.
func (s *Server) joinLines(st store.Site) (cmd, nocheck string, exp time.Time, err error) {
	base := s.cfg.PublicBaseURL
	if billing.IPoE(st) {
		j, err := s.joinBody(st)
		if err != nil {
			return "", "", time.Time{}, err
		}
		return j["command"].(string), j["command_nocheck"].(string), j["expires_at"].(time.Time), nil
	}
	if st.AccessMode == store.AccessIPoEObserve {
		tok, e, err := s.mgr.MintJoinStage(st.ID, sites.StageJoin)
		if err != nil {
			return "", "", time.Time{}, err
		}
		intent := s.mgr.JoinIntentOf(st.ID)
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
	in := siteInput(r)
	if strings.TrimSpace(in.Name) == "" {
		fail("a router name is required")
		return
	}
	// The same call as POST /routers: IPoE mode, provision off, the serving
	// intent stored once in site.<id>.serving_customers.
	st, err := s.mgr.CreateRouter(in, serving)
	if err != nil {
		fail(err.Error())
		return
	}
	st, _ = s.siteByID(st.ID)
	dest := "/admin/routers/view?id=" + url.QueryEscape(st.ID) + "&tab=setup"
	if !wantsJSON(r) {
		// Without JavaScript: the router page, where the Setup tab makes the join line.
		s.back(w, r, dest, "Router added. Make its join line below.")
		return
	}
	cmd, noCheck, exp, jerr := s.joinLines(st)
	w.Header().Set("Cache-Control", "no-store")
	if jerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": st.ID, "name": st.Name, "url": dest,
			"note": "Router created, but no join line could be made: " + jerr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": st.ID, "name": st.Name, "serving_customers": serving, "command": cmd, "command_nocheck": noCheck, "expires_at": exp, "url": dest})
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
	s.routerTabData(r.Context(), p.Data, st, status, tab, strings.TrimSpace(r.URL.Query().Get("q")))
	if cmd != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.render(w, "admin_site_view", p)
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
