package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	get("/admin/sites/checks.json", s.adminSiteChecks)
	post("/admin/sites/fix", s.adminSiteFix)
	post("/admin/sites/create", s.adminSiteCreate)
	post("/admin/sites/update", s.adminSiteUpdate)
	post("/admin/sites/join", s.adminSiteJoin)
	post("/admin/sites/join/revoke", s.adminSiteJoinRevoke)
	post("/admin/sites/packages", s.adminSitePackages)
	post("/admin/sites/sync", s.adminSiteSync)
	post("/admin/sites/disable", s.adminSiteDisable)
	post("/admin/sites/remove", s.adminSiteRemove)
	post("/admin/sites/mode", s.adminSiteMode)
	post("/admin/sites/adopt", s.adminSiteAdopt)
	post("/admin/sites/confirm", s.adminSiteConfirm)
	post("/admin/sites/script", s.adminSiteScript)
	s.deviceRoutes(get, post)
}

// joinScript serves GET /j/{token}: the one-time router script. Every kind of
// bad token gets the same 404 so the endpoint cannot be used to probe.
func (s *Server) joinScript(w http.ResponseWriter, r *http.Request) {
	if !s.joinRL.allow(realIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	script, siteID, err := s.mgr.ConsumeJoinForce(r.PathValue("token"), r.UserAgent(), realIP(r), r.URL.Query().Get("nocheck") == "1", r.URL.Query().Get("stage"), r.URL.Query().Get("force") == "1")
	switch {
	case errors.Is(err, sites.ErrJoinInvalid):
		http.NotFound(w, r)
		return
	case errors.Is(err, sites.ErrCannotRender):
		// The token is still valid: say why, so the admin can fix it and run the command again.
		http.Error(w, err.Error(), http.StatusConflict)
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
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(err, "Saved"))
}

func (s *Server) adminSiteJoin(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cmd, noCheck, exp, err := s.joinLines(st)
	if err != nil {
		s.back(w, r, "/admin/routers/view?id="+st.ID+"&tab=setup", "Error: "+err.Error())
		return
	}
	// The line is rendered here and never put in a URL or the log.
	s.siteView(w, r, st, cmd, noCheck, exp, "")
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
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(err, "The join command no longer works"))
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
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(err, "Packages saved for this router"))
}

func (s *Server) adminSiteSync(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if billing.IPoE(st) {
		_, err := s.svc.ReconcileSite(ctx, st.ID)
		s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(err, "Router reconciled with the accounts"))
		return
	}
	err := s.svc.SyncProfiles(ctx, st.ID)
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(err, "Speed profiles pushed to the router"))
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
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(s.mgr.SetDisabled(st.ID, off), msg))
}

func (s *Server) adminSiteRemove(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.mgr.RemoveSite(st.ID, r.PostFormValue("confirm")); err != nil {
		s.back(w, r, "/admin/routers/view?id="+st.ID, "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/routers", "Router removed: "+st.Name)
}

// adminSiteScript mints a one-time command for one stage of the IPoE move:
// join (tunnel and API only), cutover (switch the building to server-driven
// IPoE with a 15 minute automatic rollback) or cleanup (remove the old hotspot
// objects). The command is shown once, like the join command.
func (s *Server) adminSiteScript(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	stage := r.PostFormValue("stage")
	switch stage {
	case sites.StageJoin, sites.StageCutover, sites.StageCleanup:
	default:
		http.NotFound(w, r)
		return
	}
	if stage != sites.StageJoin && !billing.IPoE(st) {
		s.back(w, r, "/admin/routers/view?id="+st.ID, "Error: set the access mode to IPoE observe or IPoE first")
		return
	}
	force := r.PostFormValue("force") == "1"
	if stage == sites.StageCutover && !force {
		// Paying flats the router has not seen would lose service: say so before a command exists.
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		unseen, uerr := s.mgr.UnseenEntitled(ctx, st.ID)
		unmanaged, _ := s.mgr.UnmanagedDevices(ctx, st.ID)
		cancel()
		var confirmed bool
		s.st.View(func(d *store.Data) { confirmed = billing.SiteMark(d, st.ID, billing.MarkCutoverConfirmed) })
		if uerr != nil || len(unseen) > 0 || len(unmanaged) > 0 || confirmed {
			ex := map[string]any{"Unseen": unseen, "Unmanaged": unmanaged, "AlreadyCutOver": confirmed, "UnseenStage": stage}
			if uerr != nil {
				ex["UnseenErr"] = uerr.Error()
			}
			s.siteView(w, r, st, "", "", time.Time{}, "", ex)
			return
		}
	}
	tok, exp, err := s.mgr.MintJoinStage(st.ID, stage)
	if err != nil {
		s.back(w, r, "/admin/routers/view?id="+st.ID, "Error: "+err.Error())
		return
	}
	base := s.cfg.PublicBaseURL
	mk := sites.JoinCommandStage
	if force {
		mk = sites.JoinCommandForce
	}
	s.siteView(w, r, st, mk(base, tok, true, stage), mk(base, tok, false, stage), exp, stage)
}

func (s *Server) adminSiteMode(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	mode := r.PostFormValue("mode")
	if err := s.mgr.SetAccessModeForce(st.ID, mode, r.PostFormValue("override") == "1"); err != nil {
		s.back(w, r, "/admin/routers/view?id="+st.ID, "Error: "+err.Error())
		return
	}
	msg := "Access mode set to " + mode
	if mode != store.AccessHotspot {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if _, err := s.svc.ReconcileSite(ctx, st.ID); err != nil {
			msg += ". First router pass failed: " + err.Error()
		}
	}
	s.back(w, r, "/admin/routers/view?id="+st.ID, msg)
}

func (s *Server) adminSiteAdopt(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := s.svc.AdoptLeases(ctx, st.ID)
	if err != nil {
		s.back(w, r, "/admin/routers/view?id="+st.ID, "Error: "+err.Error())
		return
	}
	s.back(w, r, "/admin/routers/view?id="+st.ID, fmt.Sprintf("Adopted %d leases (%d already matched, %d skipped)", len(res.Adopted), res.Unchanged, len(res.Skipped))+skippedNote(res.Skipped))
}

func (s *Server) adminSiteConfirm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(s.mgr.ConfirmCutover(ctx, st.ID), "Cutover confirmed: the automatic rollback is cancelled"))
}

func skippedNote(skipped []string) string {
	if len(skipped) == 0 {
		return ""
	}
	if len(skipped) > 5 {
		skipped = append(skipped[:5:5], "...")
	}
	return ". Skipped: " + strings.Join(skipped, "; ")
}
