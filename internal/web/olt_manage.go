package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/olt"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// OLT management screens: connection settings, status, backups, the plan and apply, and the adopt page. The server
// talks to the OLT only through internal/olt; this file only asks it and shows the answer. Secrets (the OLT login,
// the enable password, saved configurations) are never put in a page: backups show redacted.

// SetOLT gives the web server the OLT service (nil = OLT management is off).
func (s *Server) SetOLT(o *olt.Service) { s.olt = o }

// OLTAlert tells the owners about an OLT event. The message never carries a secret.
func (s *Server) OLTAlert(subject, msg string) { s.ownerAlert(subject, msg) }

const oltOpTimeout = 24 * time.Second // the HTTP server cuts a response off at 30 s

func (s *Server) oltRoutes2(get, post func(string, http.HandlerFunc)) {
	get("/admin/olts/manage", s.adminOLTManage)
	get("/admin/olts/adopt", s.adminOLTAdopt)
	get("/admin/olts/backup", s.adminOLTBackup)
	post("/admin/olts/manage/settings", s.adminOLTSettings)
	post("/admin/olts/manage/test", s.adminOLTTest)
	post("/admin/olts/manage/poll", s.adminOLTPoll)
	post("/admin/olts/manage/backup", s.adminOLTBackupNow)
	post("/admin/olts/manage/provisioning", s.adminOLTProvisioning)
	post("/admin/olts/manage/dialect", s.adminOLTDialect)
	post("/admin/olts/manage/plan", s.adminOLTPlan)
	post("/admin/olts/manage/apply", s.adminOLTApply)
	post("/admin/olts/manage/remove-plan", s.adminOLTRemovePlan)
	post("/admin/olts/manage/remove-apply", s.adminOLTRemoveApply)
	s.oltRoutes3(get, post)
}

func manageURL(id string) string { return "/admin/olts/manage?id=" + url.QueryEscape(id) }

// oltOf loads the OLT of a request and checks the admin's scope. ok is false after an error page was sent.
func (s *Server) oltOf(w http.ResponseWriter, r *http.Request, id string) (store.Equipment, store.OLTState, bool) {
	if s.olt == nil {
		s.back(w, r, "/admin/equipment?tab=olt", "Error: OLT management is off on this server (it needs DATA_KEY)")
		return store.Equipment{}, store.OLTState{}, false
	}
	e, st, ok := s.olt.Load(id)
	if !ok || (e.SiteID != "" && !s.scopeAllows(r, e.SiteID)) {
		http.NotFound(w, r)
		return store.Equipment{}, store.OLTState{}, false
	}
	return e, st, true
}

// ownerOnly sends the admin back with a message unless they are an owner.
func (s *Server) ownerOnly(w http.ResponseWriter, r *http.Request, to string) (store.AdminUser, bool) {
	u, _ := userOf(r)
	if u.Role != store.RoleOwner {
		s.back(w, r, to, "Error: only an owner can do this")
		return u, false
	}
	return u, true
}

// oltFoundRow is one line of the "found on the PON" lists, with the prefill link for the unknown ones.
type oltFoundRow struct {
	store.OLTFound
	AddURL string
}

type oltBackupRow struct {
	ID, At, Why, By string
	Size            int
}

func (s *Server) adminOLTManage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	e, st, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	p := s.eqPage(r, "equipment", eqTabOLT)
	p.Detail = store.OLTLabel(e) + " management"
	p.H1a, p.H1b = "OLT", store.OLTLabel(e)
	if p.Flash != "" {
		p.FlashBad = strings.HasPrefix(p.Flash, "Error")
	}
	var rows, unknown, held []oltFoundRow
	for _, f := range st.Found {
		row := oltFoundRow{OLTFound: f}
		if f.Verdict == store.OLTFoundUnknown {
			row.AddURL = "/admin/equipment?tab=onu&add_serial=" + url.QueryEscape(f.Serial) + "#add"
			unknown = append(unknown, row)
			continue
		}
		if f.Verdict == store.OLTFoundHeld || f.Verdict == store.OLTFoundFailed {
			held = append(held, row)
		}
		rows = append(rows, row)
	}
	var backups []oltBackupRow
	if p.IsOwner {
		for _, b := range s.olt.BackupList(e.ID) {
			backups = append(backups, oltBackupRow{b.ID, s.ageText(b.At) + " (" + b.At.In(time.FixedZone("EAT", 3*3600)).Format("2 Jan 15:04") + ")", b.Why, b.By, b.Size})
		}
	}
	online := 0
	for _, o := range st.Status.ONUs {
		if o.Online {
			online++
		}
	}
	pw := e.MgmtIP != "" && e.MgmtUser != "" && e.MgmtPass != ""
	p.Data["E"], p.Data["S"], p.Data["Set"] = e, st, st.Settings
	p.Data["Rows"], p.Data["Unknown"], p.Data["Held"], p.Data["Backups"] = rows, unknown, held, backups
	p.Data["Online"], p.Data["Total"] = online, len(st.Status.ONUs)
	p.Data["Proto"], p.Data["Port"] = st.Settings.ProtoOrDefault(), st.Settings.PortOrDefault()
	p.Data["MgmtVLAN"], p.Data["Uplink"] = st.Settings.MgmtVLANOrDefault(), st.Settings.UplinkOrDefault()
	p.Data["HasEnable"], p.Data["Configured"] = st.Settings.EnablePass != "", pw
	p.Data["PolledAt"] = s.ageText(st.Status.At)
	p.Data["Note"] = s.olt.Dialect().VersionNote(st.Status.Model, st.Status.SWVersion)
	s.adoptData(r, e, &p, p.IsOwner)
	s.render(w, "admin_olt_manage", p)
}

func (s *Server) adminOLTSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	// The password again: the protocol (Telnet is a downgrade), the pinned host key and the enable password are here.
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	atoi := func(v string) int { n, _ := strconv.Atoi(strings.TrimSpace(v)); return n }
	err := s.olt.SaveSettings(id, olt.SettingsInput{Proto: r.PostFormValue("proto"), Port: atoi(r.PostFormValue("port")), EnablePass: r.PostFormValue("enable_pass"),
		ClearEnable: r.PostFormValue("clear_enable") == "1", ClearHostKey: r.PostFormValue("clear_hostkey") == "1", MgmtVLAN: atoi(r.PostFormValue("mgmt_vlan")),
		UplinkPort: r.PostFormValue("uplink"), InternetVLAN: atoi(r.PostFormValue("internet_vlan"))}, u.Username)
	s.back(w, r, to, msgOr(err, "Saved"))
}

func (s *Server) oltCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), oltOpTimeout)
}

func (s *Server) adminOLTTest(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, _ := userOf(r)
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	v, err := s.olt.TestConnection(ctx, id, u.Username)
	if err != nil {
		s.back(w, r, manageURL(id), "Error: "+oltErrText(err))
		return
	}
	s.back(w, r, manageURL(id), "Connected: "+v)
}

func (s *Server) adminOLTPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	err := s.olt.Poll(ctx, id, true)
	s.back(w, r, manageURL(id), msgOr(oltErr(err), "Read the OLT"))
}

func (s *Server) adminOLTBackupNow(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, _ := userOf(r)
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	_, err := s.olt.Backup(ctx, id, "manual", u.Username)
	s.back(w, r, manageURL(id), msgOr(oltErr(err), "Configuration saved"))
}

func oltErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(oltErrText(err))
}

func oltErrText(err error) string {
	switch {
	case errors.Is(err, olt.ErrBusy):
		return "another operation is running on this OLT; try again in a minute"
	case errors.Is(err, context.DeadlineExceeded):
		return "the OLT took too long to answer"
	}
	return olt.Redact(err.Error())
}

// adminOLTBackup shows or downloads one saved configuration: owners only, redacted, never cached.
func (s *Server) adminOLTBackup(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	e, _, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	if u, _ := userOf(r); u.Role != store.RoleOwner {
		s.back(w, r, manageURL(id), "Error: only an owner can see saved configurations")
		return
	}
	b, text, err := s.olt.BackupText(id, r.URL.Query().Get("b"))
	if err != nil {
		s.back(w, r, manageURL(id), "Error: "+err.Error())
		return
	}
	u, _ := userOf(r)
	s.audit(r, "OLT "+store.OLTLabel(e)+" saved configuration "+b.ID+" viewed by "+u.Username)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name := fmt.Sprintf("olt-%s-%s.txt", sanitizeFile(store.OLTLabel(e)), b.At.UTC().Format("20060102-150405"))
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	fmt.Fprintf(w, "# %s, saved %s (%s), secrets hidden\n%s", store.OLTLabel(e), b.At.UTC().Format(time.RFC3339), b.Why, text)
}

func sanitizeFile(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// stepUp is the owner check of the permission actions: an owner who typed their own password again.
func (s *Server) stepUp(w http.ResponseWriter, r *http.Request, to string) (store.AdminUser, bool) {
	u, ok := s.ownerOnly(w, r, to)
	if !ok {
		return u, false
	}
	if !s.oltRL.allow("olt-step:" + u.ID) {
		s.back(w, r, to, "Error: too many password checks in a short time; wait a few minutes")
		return u, false
	}
	if !s.reauth(w, r, to) {
		return u, false
	}
	return u, true
}

func (s *Server) adminOLTProvisioning(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	on := r.PostFormValue("on") == "1"
	err := s.olt.SetProvisioning(id, on, u.Username)
	s.back(w, r, to, msgOr(err, map[bool]string{true: "The server may now configure this OLT (after the dialect is confirmed)", false: "The server will not change this OLT"}[on]))
}

func (s *Server) adminOLTDialect(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	err := s.olt.SetDialectChecked(id, r.PostFormValue("on") == "1", u.Username)
	s.back(w, r, to, msgOr(err, "Saved"))
}

// planPage renders a plan (or a removal plan) with the form that applies it.
func (s *Server) planPage(w http.ResponseWriter, r *http.Request, e store.Equipment, st store.OLTState, p olt.Plan, hash, removeSerial string) {
	pg := s.eqPage(r, "equipment", eqTabOLT)
	pg.Detail = store.OLTLabel(e) + " plan"
	pg.H1a, pg.H1b = "OLT plan", store.OLTLabel(e)
	pg.Data["E"], pg.Data["Plan"], pg.Data["Hash"], pg.Data["Remove"] = e, p.Display(), hash, removeSerial
	pg.Data["Cmds"] = len(p.Commands())
	pg.Data["MayApply"] = st.Settings.Provisioning && st.Settings.DialectChecked
	pg.Data["Set"] = st.Settings
	s.render(w, "admin_olt_plan", pg)
}

func (s *Server) adminOLTPlan(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	e, st, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	if _, ok := s.ownerOnly(w, r, to); !ok {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	p, hash, err := s.olt.Plan(ctx, id)
	if err != nil {
		s.back(w, r, to, "Error: "+oltErrText(err))
		return
	}
	s.planPage(w, r, e, st, p, hash, "")
}

func (s *Server) adminOLTApply(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	// The password was checked above and the plan is compared with the one the owner reviewed here, in the request; the
	// apply itself (minutes: a save alone may take two) runs in the background and the page shows its progress.
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	switch err := s.olt.StartApply(ctx, id, r.PostFormValue("hash"), u.Username); {
	case err == nil:
		s.back(w, r, to, "Applying started. This takes a few minutes; this page shows the progress.")
	case errors.Is(err, olt.ErrJobRunning):
		s.back(w, r, to, runningMsg(err))
	default:
		s.back(w, r, to, "Error: "+oltErrText(err))
	}
}

func applyMsg(res olt.Result, err error) string {
	switch {
	case err != nil:
		return "Error: " + oltErrText(err)
	case len(res.Sent) == 0:
		return "Nothing to change"
	case !res.Verified:
		return fmt.Sprintf("Applied %d lines, but %d lines are still missing when the OLT is read back: open Plan", len(res.Sent), len(res.Remaining))
	}
	return fmt.Sprintf("Applied %d lines and checked them (the configuration was saved first)", len(res.Sent))
}

func (s *Server) adminOLTRemovePlan(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	e, st, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	if _, ok := s.ownerOnly(w, r, to); !ok {
		return
	}
	serial := r.PostFormValue("serial")
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	p, hash, err := s.olt.PlanRemove(ctx, id, serial)
	if err != nil {
		s.back(w, r, to, "Error: "+oltErrText(err))
		return
	}
	s.planPage(w, r, e, st, p, hash, serial)
}

func (s *Server) adminOLTRemoveApply(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := manageURL(id)
	if _, _, ok := s.oltOf(w, r, id); !ok {
		return
	}
	u, ok := s.stepUp(w, r, to)
	if !ok {
		return
	}
	ctx, cancel := s.oltCtx(r)
	defer cancel()
	res, err := s.olt.ApplyRemove(ctx, id, r.PostFormValue("serial"), r.PostFormValue("hash"), u.Username)
	s.back(w, r, to, applyMsg(res, err))
}

// adminOLTAdopt shows how to make a factory OLT reachable: the short snippet for the OLT console and the script for
// the router terminal (both paste-once).
func (s *Server) adminOLTAdopt(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	e, st, ok := s.oltOf(w, r, id)
	if !ok {
		return
	}
	if u, _ := userOf(r); u.Role != store.RoleOwner && u.Role != store.RoleOperator {
		s.back(w, r, manageURL(id), "Error: only an owner or operator can open this page")
		return
	}
	p := s.eqPage(r, "equipment", eqTabOLT)
	p.Detail = store.OLTLabel(e) + " adopt"
	p.H1a, p.H1b = "Adopt", store.OLTLabel(e)
	var site store.Site
	var targets []sites.OLTTarget
	s.st.View(func(d *store.Data) {
		site = d.Sites[e.SiteID]
		for _, o := range d.OLTsOfSite(e.SiteID) {
			if o.MgmtIP == "" {
				continue
			}
			os := d.OLTStateOf(o.ID).Settings
			targets = append(targets, sites.OLTTarget{IP: o.MgmtIP, Proto: os.ProtoOrDefault(), Port: os.PortOrDefault()})
		}
	})
	sort.Slice(targets, func(i, j int) bool { return targets[i].IP < targets[j].IP })
	if site.ID == "" {
		p.Data["Problem"] = "Put the OLT on a router first (Edit the OLT)."
	} else if plan, err := s.mgr.OLTPlan(site, st.Settings.MgmtVLAN, targets); err != nil {
		p.Data["Problem"] = err.Error()
	} else if !plan.Contains(e.MgmtIP) {
		p.Data["Problem"] = fmt.Sprintf("Give the OLT a management IP inside %s (from .2 to .6), for example 10.7.%d.2, then come back.", plan.Subnet, site.ONUMgmtN)
	} else {
		parent := sites.ONUMgmtParent(site)
		p.Data["Plan"], p.Data["RouterScript"] = plan, plan.Snippet(parent)
		p.Data["ConsoleScript"] = strings.Join(s.olt.Dialect().AdoptSnippet(plan.VLAN, e.MgmtIP, plan.Mask, plan.Gateway, st.Settings.UplinkOrDefault()), "\n")
		p.Data["Parent"] = parent
	}
	p.Data["E"] = e
	s.render(w, "admin_olt_adopt", p)
}
