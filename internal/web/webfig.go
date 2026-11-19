package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/sites"
	"ispbill/internal/store"
	"ispbill/internal/webfig"
)

// Remote WebFig (WEBFIG_DOMAIN). An owner opens it from the router page after
// a fresh step-up (password and authenticator code within the last
// webfigStepUpLife); the admin then mints a one-time ticket and sends the
// browser to the WebFig host, where internal/webfig takes over. See
// deploy/README.md for the security model.

const webfigStepUpLife = 10 * time.Minute

// webfigState is the part of Server that remote WebFig needs.
type webfigState struct {
	mu     sync.Mutex
	stepUp map[string]time.Time // admin session id -> when password and code were last checked
	mgr    *webfig.Manager
	now    func() time.Time
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (s *Server) webfigOn() bool { return s.cfg.WebFigDomain != "" && s.wf.mgr != nil }

func (s *Server) wfNow() time.Time {
	if s.wf.now != nil {
		return s.wf.now()
	}
	return time.Now()
}

// initWebFig builds the manager when the feature is on.
func (s *Server) initWebFig() {
	s.wf.stepUp = map[string]time.Time{}
	if s.cfg.WebFigDomain == "" {
		return
	}
	var tunnel netip.Prefix
	var srvIP netip.Addr
	if s.wgNet.IsValid() {
		tunnel = s.wgNet
		srvIP = s.wgNet.Addr().Next() // the server is .1 of the tunnel network
	}
	s.wf.mgr = webfig.New(webfig.Options{
		Domain: s.cfg.WebFigDomain, Secure: true, Tunnel: tunnel, ServerIP: srvIP,
		Resolve:  s.webfigTarget,
		UserOK:   func(id string) bool { u, ok := s.userByID(id); return ok && !u.Disabled && u.Role == store.RoleOwner },
		ClientIP: realIP,
		Now:      s.wfNow,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if s.wf.dial != nil {
				return s.wf.dial(ctx, network, addr)
			}
			return (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
		},
		OnOpen: func(x webfig.Session) {
			s.webfigAudit(x, "WebFig opened on "+s.wfSiteLabel(x.SiteID)+" (session "+x.PublicID+")")
		},
		OnEnd: func(x webfig.Session, reason string, at time.Time) {
			s.webfigAudit(x, "WebFig session "+x.PublicID+" on "+s.wfSiteLabel(x.SiteID)+" ended: "+reason+" (opened "+x.Started.In(timeEAT()).Format("2 Jan 15:04")+")")
		},
	})
}

func timeEAT() *time.Location { return time.FixedZone("EAT", 3*3600) }

func (s *Server) wfSiteLabel(id string) string {
	if st, ok := s.siteByID(id); ok {
		return st.Name
	}
	return id
}

func (s *Server) webfigAudit(x webfig.Session, msg string) {
	_ = s.st.Update(func(d *store.Data) error {
		s.authn.Audit(d, x.User, x.IP, msg, x.SiteID, "")
		return nil
	})
}

// webfigTarget is the router's tunnel address and www port: the REST port the
// bootstrap script gave the www service. Only routers built over WireGuard.
func (s *Server) webfigTarget(id string) (webfig.Target, bool) {
	st, ok := s.siteByID(id)
	if !ok || st.Mode != store.SiteModeWG || st.TunnelIP == "" {
		return webfig.Target{}, false
	}
	port := st.RESTPort
	if port == 0 {
		port = sites.DefaultREST
	}
	return webfig.Target{IP: st.TunnelIP, Port: port, Name: st.Name}, true
}

// WebFigHandler is the handler for WEBFIG_DOMAIN; nil when the feature is off.
func (s *Server) WebFigHandler() http.Handler {
	if !s.webfigOn() {
		return nil
	}
	return s.wf.mgr.Handler()
}

// RunWebFig ends timed-out WebFig sessions once a minute until ctx is done.
func (s *Server) RunWebFig(ctx context.Context) {
	if !s.webfigOn() {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.wf.mgr.Sweep()
		}
	}
}

// SetWebFigDial and SetWebFigClock are test hooks.
func (s *Server) SetWebFigDial(f func(ctx context.Context, network, addr string) (net.Conn, error)) {
	s.wf.dial = f
}
func (s *Server) SetWebFigClock(f func() time.Time) { s.wf.now = f }

// WebFigSweep runs the timeout sweep now (tests).
func (s *Server) WebFigSweep() {
	if s.webfigOn() {
		s.wf.mgr.Sweep()
	}
}

// WebFigURL is the admin step-up address of a router, or "" when WebFig is off
// or the router has no tunnel. Android opens it in the system browser.
func (s *Server) webfigAdminURL(st store.Site) string {
	if !s.webfigOn() {
		return ""
	}
	if _, ok := s.webfigTarget(st.ID); !ok {
		return ""
	}
	base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	if base == "" {
		return ""
	}
	return base + "/admin/routers/webfig?id=" + url.QueryEscape(st.ID)
}

// ---- admin pages -------------------------------------------------------------------------------

func (s *Server) webfigRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/sites/webfig", s.webfigPage)
	post("/admin/sites/webfig", s.webfigPost)
	post("/admin/sites/webfig/end", s.webfigEnd)
}

type webfigPage struct {
	authPage
	RouterID, RouterName string
	Fresh                bool
}

func (s *Server) stepFresh(sessID string) bool {
	s.wf.mu.Lock()
	defer s.wf.mu.Unlock()
	t, ok := s.wf.stepUp[sessID]
	return ok && s.wfNow().Sub(t) < webfigStepUpLife
}

func (s *Server) markStep(sessID string) {
	now := s.wfNow()
	s.wf.mu.Lock()
	defer s.wf.mu.Unlock()
	for k, t := range s.wf.stepUp {
		if now.Sub(t) >= webfigStepUpLife {
			delete(s.wf.stepUp, k)
		}
	}
	s.wf.stepUp[sessID] = now
}

// webfigSite finds the router of the request and checks WebFig applies to it.
func (s *Server) webfigSite(w http.ResponseWriter, r *http.Request) (store.Site, bool) {
	if !s.webfigOn() {
		s.webfigMsg(w, http.StatusNotFound, "Remote WebFig is not switched on. Set WEBFIG_DOMAIN on the server (see deploy/README.md) and run the installer again.")
		return store.Site{}, false
	}
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return st, false
	}
	if _, ok := s.webfigTarget(st.ID); !ok {
		s.webfigMsg(w, http.StatusConflict, "This router has no tunnel yet. Set it up first, then WebFig can be opened.")
		return st, false
	}
	return st, true
}

func (s *Server) webfigMsg(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`<!doctype html><meta name="viewport" content="width=device-width"><body style="font:16px system-ui;background:#0d1117;color:#e6e9ef;display:grid;place-items:center;height:90vh"><div style="max-width:30em">` +
		htmlEscape(msg) + ` <a style="color:#8b83ff" href="/admin/routers">Back</a></div>`))
}

func htmlEscape(v string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(v)
}

func (s *Server) webfigForm(w http.ResponseWriter, r *http.Request, st store.Site, code int, errMsg string) {
	sess, _ := sessOf(r)
	u, _ := userOf(r)
	p := webfigPage{authPage: s.authPage(r), RouterID: st.ID, RouterName: st.Name, Fresh: s.stepFresh(sess.ID)}
	p.CSRF, p.Username, p.Error = sess.CSRF, u.Username, errMsg
	w.Header().Set("Cache-Control", "no-store")
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	s.render(w, "admin_webfig_step", p)
}

// webfigPage is what the Android app (and any link) opens. A GET never mints a
// ticket: it shows the step-up form, or the button when the step-up is fresh.
func (s *Server) webfigPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.webfigSite(w, r)
	if !ok {
		return
	}
	s.webfigForm(w, r, st, http.StatusOK, "")
}

// webfigPost opens WebFig: with a fresh step-up it mints the ticket at once;
// otherwise it checks the password and code in the form first.
func (s *Server) webfigPost(w http.ResponseWriter, r *http.Request) {
	st, ok := s.webfigSite(w, r)
	if !ok {
		return
	}
	sess, _ := sessOf(r)
	u, _ := userOf(r)
	ip := realIP(r)
	if !s.stepFresh(sess.ID) {
		pass, rawCode := r.PostFormValue("password"), r.PostFormValue("code")
		if pass == "" && rawCode == "" {
			s.webfigForm(w, r, st, http.StatusOK, "")
			return
		}
		if !s.webfigStepUp(w, r, st, u, sess, ip, pass, authn.NormalizeCode(rawCode)) {
			return
		}
	}
	tok, err := s.wf.mgr.Mint(u.ID, u.Username, st.ID, ip)
	if err != nil {
		s.webfigMsg(w, http.StatusConflict, "WebFig cannot be opened for this router right now.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.wf.mgr.OpenURL(tok), http.StatusSeeOther)
}

// webfigStepUp checks the password and the authenticator code like the sign-in
// does (same limits, same back-off). It answers the request itself on failure.
func (s *Server) webfigStepUp(w http.ResponseWriter, r *http.Request, st store.Site, u store.AdminUser, sess store.AdminSession, ip, pass, code string) bool {
	fail := func(status int, msg string) bool { s.webfigForm(w, r, st, status, msg); return false }
	if pass == "" || len(pass) > 300 || code == "" {
		return fail(http.StatusUnauthorized, "Enter your password and the 6-digit code from your authenticator app.")
	}
	att, out := s.auth.begin(ip, u.Username, s.knownID(r, u.ID))
	if !out.ok {
		if out.limited {
			s.auth.auditBlocked(ip, time.Now())
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(out.wait.Seconds())+1))
		return fail(http.StatusTooManyRequests, "Too many attempts. Wait a few minutes and try again.")
	}
	pause(r, out.delay)
	good, busy := s.auth.password(u, true, pass, out.known)
	if busy {
		att.end(!out.known, false)
		return fail(http.StatusTooManyRequests, "The server is busy checking passwords. Try again in a moment.")
	}
	if !good {
		att.end(true, false)
		return fail(http.StatusUnauthorized, "Wrong password or code.")
	}
	if err := s.authn.VerifyTOTPFrom(u.ID, code, ip); err != nil {
		att.end(errors.Is(err, authn.ErrBadCode), false)
		if errors.Is(err, authn.ErrTOTPBackoff) || errors.Is(err, authn.ErrTOTPLocked) {
			return fail(http.StatusTooManyRequests, "Too many wrong codes. Wait a while and try again.")
		}
		return fail(http.StatusUnauthorized, "Wrong password or code.")
	}
	att.end(false, true)
	s.markStep(sess.ID)
	s.authn.AuditNow(u.Username, ip, "WebFig step-up passed")
	return true
}

// webfigEnd ends one WebFig session of a router (owner only).
func (s *Server) webfigEnd(w http.ResponseWriter, r *http.Request) {
	st, ok := s.webfigSite(w, r)
	if !ok {
		return
	}
	u, _ := userOf(r)
	to := "/admin/routers/view?id=" + st.ID
	if _, ok := s.wf.mgr.End(st.ID, strings.TrimSpace(r.PostFormValue("sid")), webfig.EndOwner+" ("+u.Username+")"); !ok {
		s.back(w, r, to, "Error: that WebFig session is already over")
		return
	}
	s.back(w, r, to, "WebFig session ended")
}

// webfigView is what the router page shows an owner.
type webfigView struct {
	On       bool
	Sessions []webfigRow
	Activity []store.Event
}

type webfigRow struct {
	ID, User, IP     string
	Started, Expires time.Time
}

func (s *Server) webfigViewFor(st store.Site) webfigView {
	var v webfigView
	if !s.webfigOn() {
		return v
	}
	if _, ok := s.webfigTarget(st.ID); !ok {
		return v
	}
	v.On = true
	for _, x := range s.wf.mgr.List(st.ID) {
		v.Sessions = append(v.Sessions, webfigRow{ID: x.PublicID, User: x.User, IP: x.IP, Started: x.Started, Expires: x.Expires()})
	}
	s.st.View(func(d *store.Data) {
		for i := len(d.Events) - 1; i >= 0 && len(v.Activity) < 10; i-- {
			e := d.Events[i]
			if e.SiteID == st.ID && e.Kind == "audit" && strings.HasPrefix(e.Msg, "WebFig ") {
				v.Activity = append(v.Activity, e)
			}
		}
	})
	return v
}
