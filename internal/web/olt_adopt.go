package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/olt"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// Adoption of an OLT through the router's OLT management port: the checklist on the OLT page, "Check now", "Secure this
// OLT", the login the owner types, "Reveal password" and the router port. Nothing here prints a password except the
// owner's Reveal page (owner, password again, audited, never cached).

func (s *Server) oltRoutes3(get, post func(string, http.HandlerFunc)) {
	get("/admin/olts/manage/secure-status", s.adminOLTSecureStatus)
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
	if owner {
		p.Data["Secure"] = s.secureViewOf(e.ID)
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

// adminOLTSecure starts "Secure this OLT" in the background and returns at once: the action takes minutes, longer than
// the HTTP server (and the proxy in front of it) will hold a response. The OLT page shows the progress.
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
	switch err := s.olt.StartSecure(id, u.Username); {
	case err == nil:
		s.back(w, r, to, "Securing started. This takes a few minutes; this page shows the progress.")
	case errors.Is(err, olt.ErrSecureRunning):
		s.back(w, r, to, "Securing is already running; this page shows the progress.")
	default:
		s.back(w, r, to, "Error: "+oltErrText(err))
	}
}

// secureMessage is the outcome of a finished Secure, for the page.
func secureMessage(res olt.SecureResult) string {
	msg := "Secured: the login and enable password were replaced by generated ones, tested with a new session and saved."
	if res.SNMPNote != "" {
		msg += " " + res.SNMPNote
	}
	if res.ACLNote != "" {
		msg += " " + res.ACLNote
	}
	return msg
}

// secureJSON is the status of the background Secure of an OLT (no secret in it).
func (s *Server) secureJSON(id string) map[string]any {
	v := s.olt.SecureStatus(id)
	m := map[string]any{"known": v.Known, "running": v.Running, "stage": v.Stage, "interrupted": v.Interrupted, "done": v.Known && !v.Running}
	if v.Known {
		m["started_at"] = v.Started.UTC().Format(time.RFC3339)
		m["by"] = v.By
	}
	if v.Known && !v.Running {
		m["finished_at"] = v.Finished.UTC().Format(time.RFC3339)
		m["ok"] = v.OK
		if v.OK {
			m["message"] = secureMessage(v.Result)
			m["enable_checked"], m["snmp_removed"], m["snmp_note"] = v.Result.EnableChecked, nz(v.Result.SNMPRemoved), v.Result.SNMPNote
			m["access_list_note"], m["access_list_done"], m["access_list_rules"], m["access_list_finding"] = v.Result.ACLNote, v.Result.ACLDone, nz(v.Result.ACLRules), v.Result.ACLFinding
		} else {
			m["error"] = v.Error
		}
	}
	return m
}

// adminOLTSecureStatus is the JSON the OLT page's script polls while a Secure runs.
func (s *Server) adminOLTSecureStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	if _, ok := s.ownerOnly(w, r, manageURL(id)); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.secureJSON(id))
}

// secureView is what the OLT page shows about the background Secure.
type secureView struct {
	Running, Failed, Done, Interrupted bool
	Stage, Started, Error, Message     string
}

func (s *Server) secureViewOf(id string) *secureView {
	v := s.olt.SecureStatus(id)
	if !v.Known && !v.Interrupted {
		return nil
	}
	sv := &secureView{Running: v.Running, Interrupted: v.Interrupted, Stage: v.Stage}
	if v.Known {
		sv.Started = v.Started.In(billing.EAT).Format("15:04")
		if !v.Running {
			if v.OK {
				sv.Done, sv.Message = true, secureMessage(v.Result)
			} else {
				sv.Failed, sv.Error = true, v.Error
			}
		}
	}
	return sv
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
