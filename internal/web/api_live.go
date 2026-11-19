package web

import (
	"context"
	"errors"
	"io"
	"net"
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
	lv, err := s.buildLive(r.Context(), st)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	writeJSON(w, http.StatusOK, lv)
}

// buildLive is the whole RouterLive answer: the router's half from the sites
// manager, the store's half and the tunnel from here. The API and the web
// admin's router page both use it.
func (s *Server) buildLive(ctx context.Context, st store.Site) (sites.RouterLive, error) {
	lv, err := s.mgr.LiveRouter(ctx, st.ID)
	if err != nil {
		return lv, err
	}
	now := s.svc.Now()
	// tunnel
	h := s.siteHealths(ctx, []store.Site{st})[st.ID]
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
	lv.SetupState = s.mgr.SetupStateOf(st.ID)
	return lv, nil
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

// clientsView is the answer of the clients view.
type clientsView struct {
	Items       []sites.Client
	Counts      sites.ClientCounts
	Truncated   bool
	GeneratedAt time.Time
	Reachable   bool
}

// buildClients merges the accounts, the unregistered devices and the router's
// tables; q, kind and online are the API's filters (already validated).
func (s *Server) buildClients(ctx context.Context, st store.Site, q, kind, online string) (clientsView, error) {
	now := s.svc.Now()
	tables, err := s.mgr.LiveClientTables(ctx, st.ID)
	if err != nil {
		return clientsView{}, err
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
	items := sites.FilterClients(all, q, kind, online)
	v := clientsView{Counts: sites.CountClients(all), GeneratedAt: tables.GeneratedAt, Reachable: tables.Reachable}
	if len(items) > maxClients {
		items, v.Truncated = items[:maxClients], true
	}
	v.Items = items
	return v, nil
}

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
	v, err := s.buildClients(r.Context(), st, q.Get("q"), kind, online)
	if err != nil {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": v.Items, "counts": v.Counts, "truncated": v.Truncated,
		"generated_at": v.GeneratedAt.UTC().Format(time.RFC3339), "reachable": v.Reachable})
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

// rebootFail is why a reboot was not sent.
type rebootFail struct {
	Kind   string // validation | bad_state | rate | refused
	Msg    string
	Wait   time.Duration
	Fields map[string]string
}

// rebootRouter is the one guarded reboot path (release plan A2), used by the
// API and the web admin: the typed name, no other reboot in flight, the
// router connected, one reboot per router per 10 minutes (one shared limiter),
// then /system/reboot. The caller has already checked the owner role and audits.
func (s *Server) rebootRouter(ctx context.Context, st store.Site, confirmName string) *rebootFail {
	if !strings.EqualFold(strings.TrimSpace(confirmName), strings.TrimSpace(st.Name)) {
		return &rebootFail{Kind: "validation", Msg: "type the router's name to confirm", Fields: map[string]string{"confirm_name": "type the router's name to confirm"}}
	}
	endReboot, free := s.mgr.BeginReboot(st.ID)
	if !free {
		return &rebootFail{Kind: "bad_state", Msg: "The router is being rebooted right now."}
	}
	defer endReboot()
	insp, isInsp := s.mgr.Router(st.ID).(routeros.Inspector)
	if !isInsp || !st.Runnable() {
		return &rebootFail{Kind: "bad_state", Msg: "The router is not connected."}
	}
	now := s.svc.Now()
	prev, wait, ok := s.reboots.reserve(st.ID, now)
	if !ok {
		return &rebootFail{Kind: "rate", Wait: wait, Msg: "This router was rebooted a moment ago. Try again in " + billing.DescribeMinutes(int(wait.Minutes())+1) + "."}
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	err := insp.Call(cctx, "/system/reboot", nil)
	if err != nil && !rebootDropped(err) {
		s.reboots.release(st.ID, prev)
		var ae *routeros.APIError
		if errors.As(err, &ae) {
			return &rebootFail{Kind: "refused", Msg: "The router refused the reboot: " + firstLine(ae.Error())}
		}
		return &rebootFail{Kind: "refused", Msg: "The router did not accept the reboot. Check that it is online."}
	}
	return nil
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
	if f := s.rebootRouter(r.Context(), st, req.ConfirmName); f != nil {
		switch f.Kind {
		case "validation":
			validationErr(w, f.Fields)
		case "bad_state":
			apiErr(w, http.StatusConflict, codeBadState, f.Msg, nil)
		case "rate":
			rateLimited(w, f.Wait, false)
		default:
			apiErr(w, http.StatusConflict, codeRouterRefused, f.Msg, nil)
		}
		return
	}
	c.audit(s, "router "+st.Name+" ("+st.ID+") reboot sent")
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true})
}

// notSent: the connection was never made (dial failure, refused, no route, DNS), so the reboot
// request did not leave. A timeout while connecting is the same; only a timeout after the request
// was written means the router may be going down.
func notSent(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	m := err.Error()
	return strings.Contains(m, "dial tcp") || strings.Contains(m, "connection refused") || strings.Contains(m, "no route to host") ||
		strings.Contains(m, "network is unreachable") || strings.Contains(m, "no such host")
}

// rebootDropped is true when the router cut the connection while it went down:
// the reboot did start.
func rebootDropped(err error) bool {
	if notSent(err) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// A timeout after the request was sent usually means the router is already
	// going down; keep the 10-minute slot rather than allow a second reboot.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
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
