package web

import (
	"context"
	"encoding/json"
	"errors"
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
	get("/admin/sites/customer-port.json", s.adminSiteCustomerPort)
	get("/admin/sites/ports.json", s.adminPorts)
	post("/admin/sites/ports/update", s.adminPortChange)
	post("/admin/sites/fix", s.adminSiteFix)
	post("/admin/sites/create", s.adminSiteCreate)
	post("/admin/sites/update", s.adminSiteUpdate)
	post("/admin/sites/auto-fixes", s.adminSiteAutoFixes)
	post("/admin/sites/join", s.adminSiteJoin)
	post("/admin/sites/join/revoke", s.adminSiteJoinRevoke)
	post("/admin/sites/packages", s.adminSitePackages)
	post("/admin/sites/sync", s.adminSiteSync)
	post("/admin/sites/disable", s.adminSiteDisable)
	post("/admin/sites/remove", s.adminSiteRemove)
	post("/admin/sites/reboot", s.adminSiteReboot)
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
	script, siteID, err := s.mgr.ConsumeJoin(r.PathValue("token"), r.UserAgent(), realIP(r), r.URL.Query().Get("nocheck") == "1")
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

// joinReset serves GET /j/{token}/reset: the fixed reset stub, for five minutes
// after the setup script was fetched with the same token. Same 404 as a bad link.
func (s *Server) joinReset(w http.ResponseWriter, r *http.Request) {
	if !s.joinRL.allow(realIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	stub, err := s.mgr.ResetStub(r.PathValue("token"), r.UserAgent())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(stub)
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
	_ = r.ParseForm()
	var base *sites.SiteInput
	if _, ok := r.PostForm["orig_lanports"]; ok { // what the page was built with: unchanged ports keep the stored value
		ov, _ := strconv.Atoi(f("orig_vlan"))
		base = &sites.SiteInput{LANPorts: f("orig_lanports"), CustomerPorts: f("orig_customerports"), OLTInterface: f("orig_olt"), VLANID: ov}
	}
	return sites.SiteInput{
		Base: base,
		Name: f("name"), Notes: f("notes"), SupportPhone: f("support"),
		WANInterface: f("wan"), OLTInterface: f("olt"),
		CustomerNet: f("subnet"), VLANID: vlan, WANCapMbps: fl("wancap"), OLTCapMbps: fl("oltcap"), FUPTB: fl("fup"), CustomerPorts: f("customerports"),
		WANMode: f("wanmode"), WANAddress: f("wanaddr"), WANGateway: f("wangw"), WANDNS: f("wandns"),
		PPPoEUser: f("pppoeuser"), PPPoEPass: r.PostFormValue("pppoepass"), PPPoEPassClear: r.PostFormValue("pppoeclear") == "1",
		LANPorts: f("lanports"), LANSubnet: f("lansubnet"),
		Confirm: f("confirm") == "1", ConfirmName: f("confirmname"),
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
	err := s.mgr.UpdateSiteCtx(r.Context(), st.ID, siteInput(r))
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
	s.siteView(w, r, st, cmd, noCheck, exp)
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
	_, err := s.svc.ReconcileSite(ctx, st.ID)
	s.back(w, r, "/admin/routers/view?id="+st.ID, msgOr(err, "Router reconciled with the accounts"))
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
