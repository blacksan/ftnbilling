package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// validTarget accepts a plain IP address (no zone, so no %) or a strict host
// name, and nothing else. Router tools pass the target as a JSON field, never
// inside a script, but it is checked all the same.
func validTarget(t string) bool { return routeros.ValidTarget(t) }

// pinger is what the router client offers for ping (REST and Mock do).
type pinger interface {
	PingHost(ctx context.Context, target string, count int) (routeros.PingResult, error)
}

func formatPing(r routeros.PingResult) string {
	return fmt.Sprintf("sent=%d received=%d packet-loss=%.0f%% avg-rtt=%s", r.Sent, r.Received, r.LossPct, r.AvgRTT.Round(time.Microsecond))
}

func formatHops(h []routeros.Hop) string {
	var b strings.Builder
	for _, x := range h {
		fmt.Fprintf(&b, "%2d  %-40s loss=%s sent=%s last=%s avg=%s best=%s worst=%s %s\n", x.N, x.Address, x.Loss, x.Sent, x.Last, x.Avg, x.Best, x.Worst, x.Status)
	}
	return b.String()
}

func (s *Server) toolsPing(w http.ResponseWriter, r *http.Request) {
	st, _, release, ok := s.toolStart(w, r)
	if !ok {
		return
	}
	defer release()
	target := strings.TrimSpace(r.PostFormValue("target"))
	if !validTarget(target) {
		s.toolsRender(w, r, nil, st.ID, "", "Error: enter an IP address or a host name")
		return
	}
	pg, isP := s.mgr.Router(st.ID).(pinger)
	if !isP {
		s.toolsRender(w, r, nil, st.ID, "", "Error: this building's router connection cannot ping")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	// The router's own ping endpoint: the target travels as a JSON field.
	pr, err := pg.PingHost(ctx, target, 4)
	res := &toolResult{Tool: "Ping", Site: st.Name, Target: target, Ran: s.svc.Now()}
	if err != nil {
		res.Err = routeros.RedactOutput(err.Error())
	} else {
		res.Output = formatPing(pr)
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
		res.Err = routeros.RedactOutput(err.Error())
	}
	s.toolsRender(w, r, res, st.ID, "", "")
}

// toolsConsole runs one console command. The policy accepts only a single
// plain command (see routeros.Classify); read-only ones run at once, ones that
// change the router need the building's name and the current admin password,
// and everything else is refused. Every submission is audited with the
// building, the redacted command, the outcome and the output size, never the
// output itself.
func (s *Server) toolsConsole(w http.ResponseWriter, r *http.Request) {
	st, ex, release, ok := s.toolStart(w, r)
	if !ok {
		return
	}
	defer release()
	cmd := strings.TrimSpace(r.PostFormValue("cmd"))
	dec := routeros.Classify(cmd)
	res := &toolResult{Tool: "Console", Site: st.Name, Target: dec.Redacted, Ran: s.svc.Now()}
	audit := func(outcome string, n int) {
		s.audit(r, fmt.Sprintf("console on %s: %s, %s, %d bytes: %s", st.Name, dec.Verdict, outcome, n, dec.Redacted))
	}
	switch dec.Verdict {
	case routeros.Deny:
		audit("denied", 0)
		s.toolsRender(w, r, nil, st.ID, cmd, "Error: not allowed: "+dec.Reason)
		return
	case routeros.Confirm:
		if strings.TrimSpace(r.PostFormValue("confirm_site")) != st.Name || !s.checkPassword(r, r.PostFormValue("current_password")) {
			audit("confirmation failed", 0)
			s.toolsRender(w, r, nil, st.ID, cmd, "Error: "+dec.Reason+". To run it, type the building name exactly and your current password.")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	var out string
	var err error
	switch dec.Kind {
	case routeros.KindPing:
		pg, isP := s.mgr.Router(st.ID).(pinger)
		if !isP {
			err = errors.New("this building's router connection cannot ping")
			break
		}
		var pr routeros.PingResult
		if pr, err = pg.PingHost(ctx, dec.Target, dec.Count); err == nil {
			out = formatPing(pr)
		}
	case routeros.KindTraceroute:
		var hops []routeros.Hop
		if hops, err = ex.Traceroute(ctx, dec.Target, 15); err == nil {
			out = formatHops(hops)
		}
	default:
		out, err = ex.Execute(ctx, dec.Exec)
	}
	res.Output = routeros.RedactOutput(out)
	if err != nil {
		res.Err = routeros.RedactOutput(err.Error())
		audit("error", len(res.Output))
	} else {
		audit("ok", len(res.Output))
	}
	s.toolsRender(w, r, res, st.ID, "", "")
}
