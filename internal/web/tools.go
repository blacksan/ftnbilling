package web

import (
	"context"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

func (s *Server) toolsRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/tools", s.toolsPage)
	post("/admin/tools/ping", s.toolsPing)
	post("/admin/tools/traceroute", s.toolsTraceroute)
	post("/admin/tools/console", s.toolsConsole)
}

type toolResult struct {
	Tool, Site, Target, Output, Err string
	Hops                            []routeros.Hop
	Ran                             time.Time
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func (s *Server) toolSites(r *http.Request) []store.Site {
	var list []store.Site
	sc := s.scopeOf(r)
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if st.Runnable() && sc.Has(st.ID) {
				list = append(list, st)
			}
		}
	})
	return list
}

func (s *Server) toolsRender(w http.ResponseWriter, r *http.Request, res *toolResult, site, cmd, msg string) {
	p := s.page(r, "set-tools")
	if msg != "" {
		p.Flash = msg
	}
	p.Data = map[string]any{
		"Tabs": s.settingsTabs("tools"), "ActiveTab": "tools", "Sites": s.toolSites(r),
		"Result": res, "SiteID": site, "Cmd": cmd, "Problems": s.problems(),
	}
	s.render(w, "admin_tools", p)
}

func (s *Server) toolsPage(w http.ResponseWriter, r *http.Request) {
	s.toolsRender(w, r, nil, "", "", "")
}

// toolStart does the checks every tool shares: the building is in scope and
// its router can run tools, the rate limit allows it, and a slot is free.
// The caller must call release when it returns ok.
func (s *Server) toolStart(w http.ResponseWriter, r *http.Request) (st store.Site, ex routeros.Executor, release func(), ok bool) {
	siteID := r.PostFormValue("site")
	found := false
	for _, c := range s.toolSites(r) {
		if c.ID == siteID {
			st, found = c, true
		}
	}
	cmd := r.PostFormValue("cmd")
	if !found {
		s.toolsRender(w, r, nil, "", cmd, "Error: choose a building")
		return
	}
	if !s.toolRL.allow(realIP(r)) {
		s.toolsRender(w, r, nil, siteID, cmd, "Error: too many tool runs in a short time; wait a minute")
		return
	}
	e, isEx := s.mgr.Router(siteID).(routeros.Executor)
	if !isEx {
		s.toolsRender(w, r, nil, siteID, cmd, "Error: this building's router connection cannot run tools")
		return
	}
	select {
	case s.toolSem <- struct{}{}:
	default:
		s.toolsRender(w, r, nil, siteID, cmd, "Error: two tools are already running; try again in a few seconds")
		return
	}
	return st, e, func() { <-s.toolSem }, true
}

func validTarget(t string) bool {
	if _, err := netip.ParseAddr(t); err == nil {
		return true
	}
	return hostRe.MatchString(t)
}

func (s *Server) toolsPing(w http.ResponseWriter, r *http.Request) {
	st, ex, release, ok := s.toolStart(w, r)
	if !ok {
		return
	}
	defer release()
	target := strings.TrimSpace(r.PostFormValue("target"))
	if !validTarget(target) {
		s.toolsRender(w, r, nil, st.ID, "", "Error: enter an IP address or a host name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	out, err := ex.Execute(ctx, "/ping "+target+" count=4")
	res := &toolResult{Tool: "Ping", Site: st.Name, Target: target, Output: routeros.RedactOutput(out), Ran: s.svc.Now()}
	if err != nil {
		res.Err = err.Error()
	}
	s.toolsRender(w, r, res, st.ID, "", "")
}

func (s *Server) toolsTraceroute(w http.ResponseWriter, r *http.Request) {
	st, ex, release, ok := s.toolStart(w, r)
	if !ok {
		return
	}
	defer release()
	target := strings.TrimSpace(r.PostFormValue("target"))
	if !validTarget(target) {
		s.toolsRender(w, r, nil, st.ID, "", "Error: enter an IP address or a host name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	hops, err := ex.Traceroute(ctx, target, 15)
	res := &toolResult{Tool: "Traceroute", Site: st.Name, Target: target, Hops: hops, Ran: s.svc.Now()}
	if err != nil {
		res.Err = err.Error()
	}
	s.toolsRender(w, r, res, st.ID, "", "")
}

// toolsConsole runs one console command. Read-only commands run at once;
// commands that change the router need the building's name and the current
// admin password; denied commands never run. Every attempt is audited with
// secret values masked.
func (s *Server) toolsConsole(w http.ResponseWriter, r *http.Request) {
	st, ex, release, ok := s.toolStart(w, r)
	if !ok {
		return
	}
	defer release()
	cmd := strings.TrimSpace(r.PostFormValue("cmd"))
	dec := routeros.Classify(cmd)
	res := &toolResult{Tool: "Console", Site: st.Name, Target: dec.Redacted, Ran: s.svc.Now()}
	switch dec.Verdict {
	case routeros.Deny:
		s.audit(r, "console DENIED on "+st.Name+": "+dec.Redacted)
		s.toolsRender(w, r, nil, st.ID, cmd, "Error: not allowed: "+dec.Reason)
		return
	case routeros.Confirm:
		if strings.TrimSpace(r.PostFormValue("confirm_site")) != st.Name || !s.checkPassword(r, r.PostFormValue("current_password")) {
			s.audit(r, "console confirmation failed on "+st.Name+": "+dec.Redacted)
			s.toolsRender(w, r, nil, st.ID, cmd, "Error: "+dec.Reason+". To run it, type the building name exactly and your current password.")
			return
		}
	}
	s.audit(r, "console ("+dec.Verdict.String()+") on "+st.Name+": "+dec.Redacted)
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	out, err := ex.Execute(ctx, cmd)
	res.Output = routeros.RedactOutput(out)
	if err != nil {
		res.Err = routeros.RedactOutput(err.Error())
	}
	s.toolsRender(w, r, res, st.ID, "", "")
}
