package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

func (s *Server) siteRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/sites", s.adminSites)
	get("/admin/sites/new", s.adminSiteNew)
	get("/admin/sites/view", s.adminSiteView)
	get("/admin/sites/status.json", s.adminSiteStatus)
	post("/admin/sites/create", s.adminSiteCreate)
	post("/admin/sites/update", s.adminSiteUpdate)
	post("/admin/sites/join", s.adminSiteJoin)
	post("/admin/sites/join/revoke", s.adminSiteJoinRevoke)
	post("/admin/sites/packages", s.adminSitePackages)
	post("/admin/sites/sync", s.adminSiteSync)
	post("/admin/sites/disable", s.adminSiteDisable)
	post("/admin/sites/remove", s.adminSiteRemove)
}

// joinScript serves GET /j/{token}: the one-time router script. Every kind of
// bad token gets the same 404 so the endpoint cannot be used to probe.
func (s *Server) joinScript(w http.ResponseWriter, r *http.Request) {
	if !s.joinRL.allow(realIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	script, siteID, err := s.mgr.ConsumeJoin(r.PathValue("token"), r.UserAgent(), realIP(r))
	switch {
	case errors.Is(err, sites.ErrJoinInvalid):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("join script", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Info("join script served", "site", siteID, "remote", realIP(r))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(script)
}

type siteRow struct {
	S        store.Site
	Accounts int
	Active   int
	State    string // joined | waiting | paused | legacy
	Detail   string
}

func (s *Server) siteRows() []siteRow {
	now := s.svc.Now()
	var rows []siteRow
	var list []store.Site
	counts := map[string][2]int{}
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if !st.Removed {
				list = append(list, st)
			}
		}
		for _, a := range d.Accounts {
			c := counts[a.SiteID]
			c[0]++
			if billing.AccountState(a, now) == billing.StateActive {
				c[1]++
			}
			counts[a.SiteID] = c
		}
	})
	for _, st := range list {
		r := siteRow{S: st, Accounts: counts[st.ID][0], Active: counts[st.ID][1]}
		switch {
		case st.Disabled:
			r.State, r.Detail = "paused", "paused"
		case st.Mode == store.SiteModeEnv:
			r.State, r.Detail = "joined", "connected by the server's settings"
		case st.WGPublicKey == "":
			r.State, r.Detail = "waiting", "not joined yet"
		default:
			r.State, r.Detail = "joined", "joined"
			if ps, found, known := s.mgr.PeerStatus(st.ID); known && found {
				if age, ok := ps.HandshakeAge(now); ok {
					r.Detail = "last handshake " + billing.DescribeMinutes(int(age.Minutes())) + " ago"
				}
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func (s *Server) adminSites(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "sites")
	p.Data["Rows"] = s.siteRows()
	s.render(w, "admin_sites", p)
}

func (s *Server) adminSiteNew(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "sites")
	p.Data["Mode"] = "new"
	p.Data["Form"] = sites.ApplyDefaults(s.cfg, store.Site{Mode: store.SiteModeWG})
	s.render(w, "admin_sites", p)
}

func siteInput(r *http.Request) sites.SiteInput {
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	fl := func(k string) float64 { v, _ := strconv.ParseFloat(f(k), 64); return v }
	vlan, _ := strconv.Atoi(f("vlan"))
	return sites.SiteInput{
		Name: f("name"), Notes: f("notes"), SupportPhone: f("support"),
		HotspotServer: f("hotspot"), WANInterface: f("wan"), WANList: f("wanlist"), OLTInterface: f("olt"),
		CustomerNet: f("subnet"), VLANID: vlan, WANCapMbps: fl("wancap"), OLTCapMbps: fl("oltcap"), FUPTB: fl("fup"),
		Provision: r.PostFormValue("provision") == "1", LegacyContainer: r.PostFormValue("legacy") == "1", BenchPort: f("bench"),
	}
}

func (s *Server) adminSiteCreate(w http.ResponseWriter, r *http.Request) {
	st, err := s.mgr.CreateSite(siteInput(r))
	if err != nil {
		s.back(w, r, "/admin/sites/new", "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/sites/view?id="+st.ID, "Building added. Now connect its router with the join command below.")
}

func (s *Server) siteID(r *http.Request) (store.Site, bool) {
	id := r.URL.Query().Get("id")
	if id == "" {
		id = r.PostFormValue("id")
	}
	var st store.Site
	var ok bool
	s.st.View(func(d *store.Data) { st, ok = d.Sites[id]; ok = ok && !st.Removed })
	return st, ok
}

// siteView builds the building page; cmd and fallback are set only in the
// response to a join request, which is the one time the command is shown.
func (s *Server) siteView(w http.ResponseWriter, r *http.Request, st store.Site, cmd, cmdNoCheck string, exp time.Time) {
	p := s.page(r, "sites")
	eff := s.mgr.Effective(st)
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	status, _ := s.mgr.Status(ctx, st.ID)
	p.Data["Site"], p.Data["Eff"], p.Data["Status"] = st, eff, status
	p.Data["Env"] = st.Mode == store.SiteModeEnv
	p.Data["Cmd"], p.Data["CmdNoCheck"], p.Data["CmdExpires"] = cmd, cmdNoCheck, exp
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
	if cmd != "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.render(w, "admin_sites", p)
}

func (s *Server) adminSiteView(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.siteView(w, r, st, "", "", time.Time{})
}

func (s *Server) adminSiteStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	status, _ := s.mgr.Status(r.Context(), st.ID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) adminSiteUpdate(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := s.mgr.UpdateSite(st.ID, siteInput(r))
	s.back(w, r, "/admin/sites/view?id="+st.ID, msgOr(err, "Saved"))
}

func (s *Server) adminSiteJoin(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tok, exp, err := s.mgr.MintJoin(st.ID)
	if err != nil {
		s.back(w, r, "/admin/sites/view?id="+st.ID, "Error: "+err.Error())
		return
	}
	base := s.cfg.PublicBaseURL
	// The command is rendered here and never put in a URL or the log.
	s.siteView(w, r, st, sites.JoinCommand(base, tok, true), sites.JoinCommand(base, tok, false), exp)
}

func (s *Server) adminSiteJoinRevoke(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := s.mgr.RevokeJoin(st.ID)
	if errors.Is(err, store.ErrNoChange) {
		err = nil
	}
	s.back(w, r, "/admin/sites/view?id="+st.ID, msgOr(err, "The join command no longer works"))
}

func (s *Server) adminSitePackages(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ov := map[string]store.PkgOverride{}
	s.st.View(func(d *store.Data) {
		for id := range d.Packages {
			price, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("price_" + id)))
			hidden := r.PostFormValue("hidden_"+id) == "1"
			if hidden || price > 0 {
				ov[id] = store.PkgOverride{Hidden: hidden, PriceKES: price}
			}
		}
	})
	err := s.svc.SaveSiteOverrides(st.ID, ov)
	s.back(w, r, "/admin/sites/view?id="+st.ID, msgOr(err, "Packages saved for this building"))
}

func (s *Server) adminSiteSync(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := s.svc.SyncProfiles(ctx, st.ID)
	s.back(w, r, "/admin/sites/view?id="+st.ID, msgOr(err, "Speed profiles pushed to the router"))
}

func (s *Server) adminSiteDisable(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	off := r.PostFormValue("disable") == "1"
	msg := "Resumed"
	if off {
		msg = "Paused: no collection and no router changes until resumed"
	}
	s.back(w, r, "/admin/sites/view?id="+st.ID, msgOr(s.mgr.SetDisabled(st.ID, off), msg))
}

func (s *Server) adminSiteRemove(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.mgr.RemoveSite(st.ID, r.PostFormValue("confirm")); err != nil {
		s.back(w, r, "/admin/sites/view?id="+st.ID, "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/sites", "Building removed: "+st.Name)
}
