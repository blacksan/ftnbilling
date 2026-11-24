package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"ispbill/internal/olt"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// Adoption of an OLT through the router's OLT management port: the checklist on the OLT page, "Check now", "Secure this
// OLT", the login the owner types, "Reveal password" and the router port. Nothing here prints a password except the
// owner's Reveal page (owner, password again, audited, never cached).

func (s *Server) oltRoutes3(post func(string, http.HandlerFunc)) {
	post("/admin/olts/manage/adopt-check", s.adminOLTAdoptCheck)
	post("/admin/olts/manage/secure", s.adminOLTSecure)
	post("/admin/olts/manage/credentials", s.adminOLTCredentials)
	post("/admin/olts/manage/reveal", s.adminOLTRevealLogin)
	post("/admin/olts/manage/mgmt-port", s.adminOLTMgmtPort)
	post("/admin/olts/manage/manual", s.adminOLTManual)
}

// adoptData puts the adoption checklist and the router port choices in a page.
func (s *Server) adoptData(r *http.Request, e store.Equipment, p *adminPage, owner bool) {
	if v, ok := s.olt.AdoptView(e.ID); ok {
		p.Data["Adopt"] = v
	}
	var site store.Site
	s.st.View(func(d *store.Data) { site = d.Sites[e.SiteID] })
	p.Data["MgmtPort"], p.Data["MgmtNet"] = site.OLTMgmtPort, sites.OLTMgmtNetOf(site)
	if owner && e.SiteID != "" && s.mgr != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		var free []string
		if v, err := s.mgr.Ports(ctx, e.SiteID, store.RoleOwner); err == nil {
			for _, it := range v.Items {
				if it.Role == sites.PortRoleUnused && it.Kind != "wlan" && !it.Missing {
					free = append(free, it.Name)
				}
			}
		}
		sort.Strings(free)
		p.Data["FreePorts"] = free
	}
}

func (s *Server) adminOLTAdoptCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, _ := userOf(r)
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	err := s.olt.AdoptCheck(ctx, id, u.Username)
	switch {
	case err == nil:
		s.back(w, r, manageURL(id), "Checked the OLT management port")
	case errors.Is(err, olt.ErrNoPort), errors.Is(err, olt.ErrNoRouter):
		s.back(w, r, manageURL(id), "Error: "+err.Error())
	default:
		s.back(w, r, manageURL(id), "Error: "+oltErrText(err))
	}
}

func (s *Server) adminOLTSecure(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := s.olt.Secure(ctx, id, u.Username)
	if err != nil {
		s.back(w, r, to, "Error: "+oltErrText(err))
		return
	}
	msg := "Secured: the login and enable password were replaced by generated ones, tested with a new session and saved."
	if res.SNMPNote != "" {
		msg += " " + res.SNMPNote
	}
	if res.ACLNote != "" {
		msg += " " + res.ACLNote
	}
	s.back(w, r, to, msg)
}

func (s *Server) adminOLTCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	if err := s.olt.SetCredentials(id, r.PostFormValue("user"), r.PostFormValue("pass"), r.PostFormValue("enable"), u.Username); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	if err := s.olt.AdoptCheck(ctx, id, u.Username); err != nil && !errors.Is(err, olt.ErrNoPort) && !errors.Is(err, olt.ErrNoRouter) {
		s.back(w, r, to, "Login saved. The check: "+oltErrText(err))
		return
	}
	s.back(w, r, to, "Login saved and checked")
}

// adminOLTRevealLogin shows the stored login and enable password once, to an owner who typed their password again.
func (s *Server) adminOLTRevealLogin(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	e, _, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	user, login, enable, err := s.olt.Reveal(id, u.Username)
	if err != nil {
		s.back(w, r, to, "Error: "+oltErrText(err))
		return
	}
	s.audit(r, "OLT "+store.OLTLabel(e)+" login revealed to "+u.Username)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	p := s.eqPage(r, "equipment", eqTabOLT)
	p.Detail = store.OLTLabel(e) + " login"
	p.H1a, p.H1b = "OLT login", store.OLTLabel(e)
	p.Data["E"], p.Data["User"], p.Data["Login"], p.Data["Enable"] = e, user, login, enable
	s.render(w, "admin_olt_reveal", p)
}

// adminOLTMgmtPort gives a spare router port the role OLT management, takes it away, or changes the router's address on it.
func (s *Server) adminOLTMgmtPort(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	e, _, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	u, ok := s.ownerOnly(w, r, to)
	if !ok {
		return
	}
	if e.SiteID == "" || s.mgr == nil {
		s.back(w, r, to, "Error: put the OLT on a router first (edit the OLT)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	who := sites.PortActor{Name: u.Username, Role: u.Role}
	if n := strings.TrimSpace(r.PostFormValue("net")); n != "" {
		if err := s.mgr.SetOLTMgmtNet(ctx, e.SiteID, n, u.Username); err != nil {
			s.back(w, r, to, "Error: "+err.Error())
			return
		}
	}
	port := strings.TrimSpace(r.PostFormValue("port"))
	switch r.PostFormValue("action") {
	case "clear":
		var cur string
		s.st.View(func(d *store.Data) { cur = d.Sites[e.SiteID].OLTMgmtPort })
		if cur == "" {
			s.back(w, r, to, "The router has no OLT management port")
			return
		}
		role := sites.PortRoleUnused
		if _, err := s.mgr.ChangePort(ctx, e.SiteID, cur, sites.PortChange{Role: &role}, who); err != nil {
			s.back(w, r, to, "Error: "+err.Error())
			return
		}
		s.back(w, r, to, "The OLT management role was removed from "+cur+" and its router rules were removed")
	case "set":
		if port == "" {
			s.back(w, r, to, "Error: choose a port")
			return
		}
		role := sites.PortRoleOLTMgmt
		if _, err := s.mgr.ChangePort(ctx, e.SiteID, port, sites.PortChange{Role: &role}, who); err != nil {
			s.back(w, r, to, "Error: "+err.Error())
			return
		}
		s.back(w, r, to, port+" is now the OLT management port. Cable the OLT's management port (GE0/2) to it and press Check now.")
	default:
		s.back(w, r, to, "Saved")
	}
}

func (s *Server) adminOLTManual(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.ownerOnly(w, r, to)
	if !ok {
		return
	}
	on := r.PostFormValue("on") == "1"
	err := s.olt.SetManual(id, on, u.Username)
	s.back(w, r, to, msgOr(err, map[bool]string{true: "Manual: use the console snippet", false: "The server uses the OLT management port"}[on]))
}
