package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/metrics"
	"ispbill/internal/routeros"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// Live router data and router actions (docs/release-plan-routers.md section 3,
// D13; notes in docs/api/v4-w9.md). Every route here is new in v4, so it is
// registered under /api/v1/routers/... only: h adds a twin for /sites patterns
// and these have none. The router's data is read by sites.Manager from a fixed
// list of paths and shown as typed fields; nothing here takes a path from the
// caller.

// codeRouterRefused is declared in api_setup.go.

// rebootEvery is the pause between two reboots of one router.
const rebootEvery = 10 * time.Minute

func (s *Server) liveRoutes(h func(pattern, role string, f apiFunc)) {
	h("GET /api/v1/routers/{id}/live", "viewer", s.apiRouterLive)
	h("GET /api/v1/routers/{id}/interfaces", "viewer", s.apiRouterInterfaces)
	h("GET /api/v1/routers/{id}/clients", "viewer", s.apiRouterClients)
	h("POST /api/v1/routers/{id}/actions/reboot", "owner", s.apiRouterReboot)
	h("POST /api/v1/routers/{id}/actions/recheck", "operator", s.apiRouterRecheck)
	h("POST /api/v1/routers/{id}/actions/resync", "operator", s.apiRouterResync)
}

// ---- flat facts ----------------------------------------------------------------------------------------

// flatInfo is what the server knows about one flat's router side right now.
type flatInfo struct {
	rs      billing.AccountRouterState
	live    metrics.LiveRate
	hasLive bool
}

// flatFacts returns a memoising lookup of flatInfo. It must not be called from
// inside a store read (AccountRouterState reads the store itself).
func (s *Server) flatFacts() func(store.Account) flatInfo {
	snaps := map[string]map[string]metrics.LiveRate{}
	memo := map[string]flatInfo{}
	var mu sync.Mutex
	return func(a store.Account) flatInfo {
		mu.Lock()
		defer mu.Unlock()
		if fi, ok := memo[a.ID]; ok {
			return fi
		}
		fl, ok := snaps[a.SiteID]
		if !ok {
			if col := s.mgr.Collector(a.SiteID); col != nil {
				fl = col.Snapshot().Flats
			}
			snaps[a.SiteID] = fl
		}
		fi := flatInfo{rs: s.svc.AccountRouterState(a.ID)}
		fi.live, fi.hasLive = fl[a.Code]
		memo[a.ID] = fi
		return fi
	}
}

// seenRecently is the "online" test shared by the live view and the overview.
func (s *Server) onlineFn(now time.Time) func(store.Account) bool {
	facts := s.flatFacts()
	return func(a store.Account) bool {
		fi := facts(a)
		recent := func(t time.Time) bool {
			return !t.IsZero() && now.Sub(t) <= sites.ClientOnlineWindow && !t.After(now.Add(time.Minute))
		}
		return (fi.hasLive && recent(fi.live.At)) || recent(fi.rs.LastSeen)
	}
}

func (s *Server) siteAccounts(siteID string) []store.Account {
	var out []store.Account
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == siteID {
				out = append(out, a)
			}
		}
	})
	return out
}

// ---- GET live ------------------------------------------------------------------------------------------

func (s *Server) apiRouterLive(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	lv, err := s.mgr.LiveRouter(r.Context(), st.ID)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	now := s.svc.Now()
	// tunnel
	h := s.siteHealths(r.Context(), []store.Site{st})[st.ID]
	switch v := h.handshake.(type) {
	case int64:
		lv.Tunnel.HandshakeAgeS = &v
	case int:
		x := int64(v)
		lv.Tunnel.HandshakeAgeS = &x
	}
	if s.ops.Fleet != nil {
		if rtt, loss, ok := s.ops.Fleet.Recent(st.ID, 10*time.Minute, now); ok {
			lv.Tunnel.RTTMs, lv.Tunnel.LossPct = &rtt, &loss
		}
	}
	// the store's half
	f := s.svc.LiveFacts(st.ID)
	lv.Flats = sites.LiveFlats{Total: f.Total, Active: f.Active, Expired: f.Expired, Suspended: f.Suspended, Expiring7d: f.Expiring7d}
	online := s.onlineFn(now)
	for _, a := range s.siteAccounts(st.ID) {
		if online(a) {
			lv.Flats.Online++
		}
	}
	lv.UnregisteredOnline = s.svc.UnregisteredOnline(func(id string) bool { return id == st.ID })
	lv.ONTsBound = f.Equipment.ONTBound
	lv.Equipment = sites.LiveEquipment{OLT: f.Equipment.OLT, ONT: f.Equipment.ONT}
	lv.Revenue = sites.LiveRevenue{TodayKES: f.TodayKES, MonthKES: f.MonthKES}
	if s.ops.Alerts != nil {
		lv.OpenAlerts = s.ops.Alerts.OpenCount(st.ID)
	}
	ps := s.svc.ProvisionStatus(st.ID)
	lv.Provision = sites.LiveProvision{InSync: ps.InSync, Drift: ps.Drift, PendingOps: ps.PendingOps, LastError: ps.LastError}
	lv.SetupState = s.mgr.Jobs().State(st.ID)
	writeJSON(w, http.StatusOK, lv)
}

// ---- GET interfaces ------------------------------------------------------------------------------------

func (s *Server) apiRouterInterfaces(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	iv, err := s.mgr.LiveInterfaces(r.Context(), st.ID)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	if iv.Items == nil {
		iv.Items = []sites.Interface{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": iv.Items, "generated_at": iv.GeneratedAt.UTC().Format(time.RFC3339), "reachable": iv.Reachable})
}

// ---- GET clients ---------------------------------------------------------------------------------------

const maxClients = 1000

func (s *Server) apiRouterClients(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	q := r.URL.Query()
	kind, online := q.Get("kind"), q.Get("online")
	switch kind {
	case "", "all", "flats", "unregistered":
	default:
		validationErr(w, map[string]string{"kind": "must be all, flats or unregistered"})
		return
	}
	switch online {
	case "", "true", "false":
	default:
		validationErr(w, map[string]string{"online": "must be true or false"})
		return
	}
	now := s.svc.Now()
	tables, err := s.mgr.LiveClientTables(r.Context(), st.ID)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	facts := s.flatFacts()
	col := s.mgr.Collector(st.ID)
	in := sites.ClientInput{Now: now, Tables: tables}
	var pkgState = func(a store.Account) string { return billing.AccountState(a, now) }
	for _, a := range s.siteAccounts(st.ID) {
		fi := facts(a)
		ca := sites.ClientAccount{ID: a.ID, Code: a.Code, Flat: a.Flat, Tenant: a.Tenant, State: pkgState(a), MAC: a.MAC, IP: a.IP,
			LeaseStatus: fi.rs.LeaseStatus, LastSeen: fi.rs.LastSeen, SuspendedOnRouter: fi.rs.SuspendedOnRouter, InSync: fi.rs.InSync,
			RouterUser: s.svc.RouterUser(st.ID, a.MAC)}
		if col != nil {
			ca.UsageToday, ca.UsageMonth = col.FlatUsage(a.Code, now)
		}
		if fi.hasLive {
			ca.LiveDown, ca.LiveUp, ca.LiveAt = fi.live.DownBps, fi.live.UpBps, fi.live.At
		}
		in.Accounts = append(in.Accounts, ca)
	}
	for _, d := range s.svc.Unregistered(st.ID, false) {
		in.Devices = append(in.Devices, sites.ClientDevice{MAC: d.MAC, IP: d.IP, HostName: "", Vendor: d.Vendor, RouterUser: d.RouterUser, Online: d.Online, LastSeen: d.LastSeen})
	}
	all := sites.MergeClients(in)
	items := sites.FilterClients(all, q.Get("q"), kind, online)
	truncated := false
	if len(items) > maxClients {
		items, truncated = items[:maxClients], true
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "counts": sites.CountClients(all), "truncated": truncated,
		"generated_at": tables.GeneratedAt.UTC().Format(time.RFC3339), "reachable": tables.Reachable})
}

// ---- actions -------------------------------------------------------------------------------------------

// rebootGuard lets one reboot per router through every rebootEvery.
type rebootGuard struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// reserve takes the router's slot; ok is false (with the wait) when it is taken.
func (g *rebootGuard) reserve(id string, now time.Time) (prev time.Time, wait time.Duration, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	prev = g.last[id]
	if !prev.IsZero() && now.Sub(prev) < rebootEvery {
		return prev, rebootEvery - now.Sub(prev), false
	}
	g.last[id] = now
	return prev, 0, true
}

// release gives the slot back (the reboot was not sent).
func (g *rebootGuard) release(id string, prev time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if prev.IsZero() {
		delete(g.last, id)
	} else {
		g.last[id] = prev
	}
}

func (s *Server) apiRouterReboot(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req struct {
		ConfirmName string `json:"confirm_name"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(req.ConfirmName), strings.TrimSpace(st.Name)) {
		validationErr(w, map[string]string{"confirm_name": "type the router's name to confirm"})
		return
	}
	if s.mgr.Jobs().Running(st.ID) {
		apiErr(w, http.StatusConflict, codeBadState, "The router is being set up right now. Wait for that to finish before rebooting.", nil)
		return
	}
	insp, isInsp := s.mgr.Router(st.ID).(routeros.Inspector)
	if !isInsp || !st.Runnable() {
		apiErr(w, http.StatusConflict, codeBadState, "The router is not connected.", nil)
		return
	}
	now := s.svc.Now()
	prev, wait, ok := s.reboots.reserve(st.ID, now)
	if !ok {
		rateLimited(w, wait, false)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	err := insp.Call(ctx, "/system/reboot", nil)
	if err != nil && !rebootDropped(err) {
		s.reboots.release(st.ID, prev)
		var ae *routeros.APIError
		switch {
		case errors.As(err, &ae):
			apiErr(w, http.StatusConflict, codeRouterRefused, "The router refused the reboot: "+firstLine(ae.Error()), nil)
		default:
			apiErr(w, http.StatusConflict, codeRouterRefused, "The router did not accept the reboot. Check that it is online.", nil)
		}
		return
	}
	c.audit(s, "router "+st.Name+" ("+st.ID+") reboot sent")
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

// rebootDropped is true when the router cut the connection while it went down:
// the reboot did start.
func rebootDropped(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	m := err.Error()
	return strings.Contains(m, "connection reset") || strings.Contains(m, "EOF")
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (s *Server) apiRouterRecheck(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	chk, err := s.mgr.Checks(ctx, st.ID, true)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	c.audit(s, "router "+st.Name+" ("+st.ID+") rechecked")
	writeJSON(w, http.StatusOK, chk)
}

func (s *Server) apiRouterResync(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	if err := s.svc.ResyncSite(st.ID); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "router "+st.Name+" ("+st.ID+") resync queued")
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}
