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
	get("/admin/olts/manage/job-status", s.adminOLTJobStatus)
	get("/admin/olts/manage/secure-status", s.adminOLTJobStatus) // the first name of the same endpoint
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
	p.Data["Job"] = s.jobViewOf(e.ID)
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
		// The configured port comes first and is the one selected: submitting the form unchanged must not move the role.
		if cur := site.OLTMgmtPort; cur != "" {
			choices := []string{cur}
			for _, f := range free {
				if f != cur {
					choices = append(choices, f)
				}
			}
			free = choices
		}
		p.Data["FreePorts"] = free
	}
}

// adminOLTAdoptCheck starts "Check now" in the background: the pass may secure a factory OLT, which takes minutes.
func (s *Server) adminOLTAdoptCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, _ := userOf(r)
	to := manageURL(id)
	switch err := s.olt.StartCheck(id, u.Username); {
	case err == nil:
		s.back(w, r, to, "Checking started; this page shows the progress.")
	case errors.Is(err, olt.ErrJobRunning):
		s.back(w, r, to, runningMsg(err))
	case errors.Is(err, olt.ErrNoPort), errors.Is(err, olt.ErrNoRouter), errors.Is(err, olt.ErrAdoptOff):
		s.back(w, r, to, "Error: "+err.Error())
	default:
		s.back(w, r, to, "Error: "+oltErrText(err))
	}
}

// runningMsg is the flash for a start refused because a job holds the OLT's slot.
func runningMsg(err error) string {
	if olt.ByPass(err) {
		return err.Error() + "."
	}
	if k, ok := olt.RunningKind(err); ok {
		return k.Label() + " is already running for this OLT; this page shows the progress."
	}
	return "Another job is already running for this OLT; this page shows the progress."
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
	case errors.Is(err, olt.ErrJobRunning):
		s.back(w, r, to, runningMsg(err))
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

// jobMessage is the outcome of a finished job, for the page.
func jobMessage(v olt.JobStatus) string {
	switch v.Kind {
	case olt.JobSecure:
		return secureMessage(v.Secure)
	case olt.JobApply:
		return applyMsg(v.Apply, nil)
	}
	return "Checked the OLT management port"
}

// jobJSON is the status of the latest background job of an OLT: Secure, Apply or Check (no secret in it).
func (s *Server) jobJSON(id string) map[string]any {
	v := s.olt.JobStatus(id)
	m := map[string]any{"known": v.Known, "running": v.Running, "stage": v.Stage, "interrupted": v.Interrupted, "done": v.Known && !v.Running}
	if v.Interrupted {
		m["interrupted_kind"] = string(v.InterruptedKind)
		if v.InterruptedKind == olt.JobApply {
			m["interrupted_note"] = applyInterrupted
		}
	}
	if v.Known {
		m["kind"], m["label"] = string(v.Kind), v.Kind.Label()
		m["started_at"] = v.Started.UTC().Format(time.RFC3339)
		m["by"] = v.By
	}
	if v.Known && !v.Running {
		m["finished_at"] = v.Finished.UTC().Format(time.RFC3339)
		m["ok"] = v.OK
		if !v.OK {
			m["error"] = v.Error
			return m
		}
		m["message"] = jobMessage(v)
		switch v.Kind {
		case olt.JobSecure:
			r := v.Secure
			m["enable_checked"], m["snmp_removed"], m["snmp_note"] = r.EnableChecked, nz(r.SNMPRemoved), r.SNMPNote
			m["access_list_note"], m["access_list_done"], m["access_list_rules"], m["access_list_finding"] = r.ACLNote, r.ACLDone, nz(r.ACLRules), r.ACLFinding
		case olt.JobApply:
			m["backup_id"], m["sent"], m["verified"], m["remaining"] = v.Apply.Backup, len(v.Apply.Sent), v.Apply.Verified, nz(v.Apply.Remaining)
		}
	}
	return m
}

// applyInterrupted is what the page says when the server stopped in the middle of an apply.
const applyInterrupted = "An Apply did not finish (the server stopped). Some of its lines may be on the OLT. Open Plan to read the plan again: it is built from what the OLT has now, so nothing is applied twice."

// adminOLTJobStatus is the JSON the OLT page's script polls while a job runs. Owners only, never cached.
func (s *Server) adminOLTJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	if _, ok := s.ownerOnly(w, r, manageURL(id)); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.jobJSON(id))
}

// jobView is what the OLT page shows about the latest background job.
type jobView struct {
	Running, Failed, Done             bool
	Kind, Label, Stage                string
	Started, Error, Message           string
	UnfinishedSecure, UnfinishedApply bool
	ApplyNote                         string
}

func (s *Server) jobViewOf(id string) *jobView {
	v := s.olt.JobStatus(id)
	if !v.Known && !v.Interrupted {
		return nil
	}
	jv := &jobView{Running: v.Running, Stage: v.Stage}
	if v.Interrupted && !v.Running {
		jv.UnfinishedSecure, jv.UnfinishedApply = v.InterruptedKind == olt.JobSecure, v.InterruptedKind == olt.JobApply
		jv.ApplyNote = applyInterrupted
	}
	if v.Known {
		jv.Kind, jv.Label = string(v.Kind), v.Kind.Label()
		jv.Started = v.Started.In(billing.EAT).Format("15:04")
		if !v.Running {
			if v.OK {
				jv.Done, jv.Message = true, jobMessage(v)
			} else {
				jv.Failed, jv.Error = true, v.Error
			}
		}
	}
	return jv
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
		if errors.Is(err, olt.ErrJobRunning) {
			s.back(w, r, to, "Error: the login was not saved. "+runningMsg(err))
			return
		}
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	// Then "Check now", in the background like the button (with the new login it may adopt, and then secure, the OLT).
	switch err := s.olt.StartCheck(id, u.Username); {
	case err == nil:
		s.back(w, r, to, "Login saved. Checking started; this page shows the progress.")
	case errors.Is(err, olt.ErrNoPort), errors.Is(err, olt.ErrNoRouter), errors.Is(err, olt.ErrAdoptOff):
		s.back(w, r, to, "Login saved")
	case errors.Is(err, olt.ErrJobRunning):
		s.back(w, r, to, "Login saved. "+runningMsg(err))
	default:
		s.back(w, r, to, "Login saved. The check: "+oltErrText(err))
	}
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
	var site store.Site
	s.st.View(func(d *store.Data) { site = d.Sites[e.SiteID] })
	port := strings.TrimSpace(r.PostFormValue("port"))
	n := strings.TrimSpace(r.PostFormValue("net"))
	netChanged := n != "" && n != sites.OLTMgmtNetOf(site)
	action := r.PostFormValue("action")
	if action == "set" && port == site.OLTMgmtPort && port != "" && !netChanged {
		s.back(w, r, to, "Nothing changed: "+port+" is already the OLT management port, with router address "+sites.OLTMgmtNetOf(site))
		return
	}
	if netChanged && action != "clear" {
		if err := s.mgr.SetOLTMgmtNet(ctx, e.SiteID, n, u.Username); err != nil {
			s.back(w, r, to, "Error: "+err.Error())
			return
		}
		if action == "set" && port == site.OLTMgmtPort {
			s.back(w, r, to, "The router address on "+port+" is now "+n)
			return
		}
	}
	switch action {
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
