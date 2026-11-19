package web

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"ispbill/internal/alerts"
	"ispbill/internal/billing"
	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/sites"
	"ispbill/internal/store"
)

func (s *Server) liveSites(filter string) []store.Site {
	var out []store.Site
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if filter == "" || st.ID == filter {
				out = append(out, st)
			}
		}
	})
	return out
}

// siteHealth is whether a building's router answers, and why not.
type siteHealth struct {
	online    bool
	cause     string
	handshake any // seconds, or nil when unknown
}

// siteHealths looks at every building at once. With the fleet monitor running
// its probe results are used; without it the router is pinged (cached 15 s).
func (s *Server) siteHealths(ctx context.Context, list []store.Site) map[string]siteHealth {
	out := make(map[string]siteHealth, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var states map[string]fleet.SiteState
	if s.ops.Fleet != nil {
		states = s.ops.Fleet.States()
	}
	for _, st := range list {
		st := st
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := siteHealth{}
			switch {
			case !st.Runnable():
			case states != nil:
				fs := states[st.ID]
				h.online = fs.Status == fleet.Up
				if fs.HandshakeAge >= 0 {
					h.handshake = fs.HandshakeAge
				}
				if fs.Status == fleet.Down {
					if o, ok := s.ops.Fleet.OpenOutage(st.ID); ok {
						h.cause = o.Cause
					}
				}
			default:
				h.online = s.siteOnline(ctx, st.ID)
				if ps, found, known := s.mgr.PeerStatus(st.ID); known && found {
					if age, has := ps.HandshakeAge(time.Now()); has {
						h.handshake = int64(age.Seconds())
					}
				}
				if !h.online {
					h.cause = fleet.CauseUnknown
				}
			}
			mu.Lock()
			out[st.ID] = h
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

type siteCounts struct{ Total, Active, Expired, Suspended int }

func (s *Server) siteCounts() map[string]*siteCounts {
	now := s.svc.Now()
	out := map[string]*siteCounts{}
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			c := out[a.SiteID]
			if c == nil {
				c = &siteCounts{}
				out[a.SiteID] = c
			}
			c.Total++
			switch billing.AccountState(a, now) {
			case billing.StateActive:
				c.Active++
			case billing.StateExpired:
				c.Expired++
			case billing.StateSuspended:
				c.Suspended++
			}
		}
	})
	return out
}

func accessMode(st store.Site) string {
	return store.AccessIPoE
}

func (s *Server) siteJSON(st store.Site, h siteHealth, cnt *siteCounts) map[string]any {
	if cnt == nil {
		cnt = &siteCounts{}
	}
	alertsOpen := 0
	if s.ops.Alerts != nil {
		alertsOpen = s.ops.Alerts.OpenCount(st.ID)
	}
	ps := s.svc.ProvisionStatus(st.ID)
	conflicts := ps.ForeignConflicts
	if conflicts == nil {
		conflicts = []string{}
	}
	eff := s.mgr.Effective(st)
	return map[string]any{
		"wan_interface": eff.WANInterface, "olt_interface": eff.OLTInterface, "vlan_id": st.VLANID, "customer_subnet": eff.CustomerSubnet,
		"customer_ports": portsJSON(st.CustomerPorts), "disabled_ports": portsJSON(st.DisabledPorts), "bench_port": firstPort(st.CustomerPorts), "customer_port": s.mgr.CustomerPortStatus(st.ID), "auto_fixes": s.mgr.AutoFixPause(st.ID),
		"wan_cap_mbps": eff.WANCapMbps, "olt_cap_mbps": eff.OLTCapMbps, "support_phone": st.SupportPhone,
		"id": st.ID, "name": st.Name, "access_mode": accessMode(st), "online": h.online, "cause": h.cause,
		"wan_mode": st.WANMode, "wan_address": st.WANAddress, "wan_gateway": st.WANGateway, "wan_dns": st.WANDNS,
		"pppoe_user": st.PPPoEUser, "pppoe_pass_set": st.PPPoEPassEnc != "", "lan_ports": st.LANPorts, "lan_subnet": st.LANSubnet,
		"tunnel_address": st.TunnelIP, "mgmt_winbox": s.mgr.MgmtWinbox(st.ID),
		"tunnel":      map[string]any{"handshake_age_s": h.handshake},
		"accounts":    map[string]any{"total": cnt.Total, "active": cnt.Active, "expired": cnt.Expired, "suspended": cnt.Suspended},
		"open_alerts": alertsOpen,
		"provision": map[string]any{"last_run_at": tp(ps.LastRunAt), "in_sync": ps.InSync, "drift": ps.Drift, "pending_ops": ps.PendingOps,
			"last_error": ps.LastError, "foreign_conflicts": conflicts},
	}
}

func (s *Server) apiSites(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	list := s.liveSites("")
	hs := s.siteHealths(ctx, list)
	cnt := s.siteCounts()
	items := make([]map[string]any, 0, len(list))
	for _, st := range list {
		items = append(items, s.siteJSON(st, hs[st.ID], cnt[st.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) site404(w http.ResponseWriter, id string) (store.Site, bool) {
	l := s.liveSites(id)
	if id == "" || len(l) != 1 {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return store.Site{}, false
	}
	return l[0], true
}

func (s *Server) apiSite(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	h := s.siteHealths(ctx, []store.Site{st})[st.ID]
	out := s.siteJSON(st, h, s.siteCounts()[st.ID])
	out["uptime_30d_pct"] = nil
	if s.ops.Fleet != nil {
		if _, pct := s.ops.Fleet.Availability(st.ID, 30, time.Now()); pct >= 0 {
			out["uptime_30d_pct"] = math.Round(pct*100) / 100
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// nanNil turns NaN into null for JSON.
func nanNil(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

func mbpsToBps(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return int64(v * 1e6)
}

// apiQoE is the building's quality report: ?period=24h|7d|30d ("24h" is the
// day so far, from midnight EAT, at 5-minute resolution).
func (s *Server) apiQoE(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var p metrics.Period
	switch v := r.URL.Query().Get("period"); v {
	case "", "24h":
		p = metrics.PeriodToday
	case "7d":
		p = metrics.Period7d
	case "30d":
		p = metrics.Period30d
	default:
		validationErr(w, map[string]string{"period": "must be 24h, 7d or 30d"})
		return
	}
	col := s.mgr.Collector(st.ID)
	if col == nil {
		writeJSON(w, http.StatusOK, map[string]any{"latency_ms": nil, "loss_pct": nil,
			"wan":    map[string]any{"down_bps": nil, "up_bps": nil, "cap_mbps": nil, "peak_util_pct": nil},
			"series": map[string]any{"t": []string{}, "down_bps": []any{}, "up_bps": []any{}, "rtt_ms": []any{}},
			"flags":  []map[string]string{{"kind": "nodata", "text": "This router is not collecting data yet: it is paused or not joined."}}})
		return
	}
	rep := col.BuildReport(p, s.svc.Now())
	t := make([]string, 0, len(rep.WAN.T))
	down, up, rtt := []any{}, []any{}, []any{}
	for i, ts := range rep.WAN.T {
		t = append(t, ts.UTC().Format(time.RFC3339))
		down = append(down, mbpsToBps(at(rep.WAN.Vals["down"], i)))
		up = append(up, mbpsToBps(at(rep.WAN.Vals["up"], i)))
		rtt = append(rtt, nanNil(meanAt(rep.Latency.Vals, i)))
	}
	flags := []map[string]string{}
	for _, f := range rep.Flags {
		flags = append(flags, map[string]string{"kind": f.Level, "text": f.Title + ": " + f.Text})
	}
	var lat, loss any
	if rep.HasPing {
		lat, loss = rep.AvgLatencyMs, rep.AvgLossPct
	}
	writeJSON(w, http.StatusOK, map[string]any{"latency_ms": lat, "loss_pct": loss,
		"wan":    map[string]any{"down_bps": mbpsToBps(rep.WANDownMbps), "up_bps": mbpsToBps(rep.WANUpMbps), "cap_mbps": rep.WANCapMbps, "peak_util_pct": nanNil(rep.PeakUtilPct)},
		"series": map[string]any{"t": t, "down_bps": down, "up_bps": up, "rtt_ms": rtt}, "flags": flags})
}

func at(v []float64, i int) float64 {
	if i < len(v) {
		return v[i]
	}
	return math.NaN()
}

func meanAt(m map[string][]float64, i int) float64 {
	var sum float64
	n := 0
	for _, v := range m {
		if x := at(v, i); !math.IsNaN(x) {
			sum += x
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return sum / float64(n)
}

func (s *Server) apiONTs(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	st, ok := s.site404(w, r.PathValue("id"))
	if !ok {
		return
	}
	var accts []store.Account
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID == st.ID {
				accts = append(accts, a)
			}
		}
	})
	var live metrics.Live
	if col := s.mgr.Collector(st.ID); col != nil {
		live = col.Snapshot()
	}
	items := make([]map[string]any, 0, len(accts))
	for _, a := range accts {
		rs := s.svc.AccountRouterState(a.ID)
		var dn, upv any
		if lr, ok := live.Flats[a.Code]; ok {
			dn, upv = int64(lr.DownBps), int64(lr.UpBps)
		}
		items = append(items, map[string]any{"account_id": a.ID, "code": a.Code, "flat": a.Flat, "mac": a.MAC, "ip": a.IP,
			"lease_status": leaseStatus(rs.LeaseStatus), "last_seen": tp(rs.LastSeen), "host_name": "", "down_bps": dn, "up_bps": upv,
			"suspended_on_router": rs.SuspendedOnRouter, "optical": nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---- dashboard -----------------------------------------------------------------------------------

func (s *Server) apiDashboard(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	site := r.URL.Query().Get("site")
	if site != "" && !s.siteExists(site) {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	has := func(id string) bool { return site == "" || id == site }
	now := s.svc.Now()
	nowE := now.In(billing.EAT)
	dayStart := time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, billing.EAT)
	monthStart := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, billing.EAT)
	lastMonth := monthStart.AddDate(0, -1, 0)
	acc := map[string]int{"active": 0, "expired": 0, "suspended": 0, "new": 0, "mac_conflict": 0}
	var today, month, last, unmatched, unreg, tasks int
	var siteIDs []store.Site
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if has(st.ID) {
				siteIDs = append(siteIDs, st)
			}
		}
		for _, a := range d.Accounts {
			if !has(a.SiteID) {
				continue
			}
			acc[billing.AccountState(a, now)]++
			if a.MACConflict {
				acc["mac_conflict"]++
			}
		}
		for _, p := range d.Payments {
			if p.Status == store.PayUnmatched && (p.SiteID == "" || has(p.SiteID)) {
				unmatched++
			}
			if !billing.CountsAsRevenue(p) || !has(p.SiteID) {
				continue
			}
			switch {
			case !p.PaidAt.Before(monthStart):
				month += p.AmountKES
				if !p.PaidAt.Before(dayStart) {
					today += p.AmountKES
				}
			case !p.PaidAt.Before(lastMonth):
				last += p.AmountKES
			}
		}
		for _, dv := range d.Devices {
			if has(dv.SiteID) {
				unreg++
			}
		}
		for _, t := range d.Tasks {
			if has(t.SiteID) {
				tasks++
			}
		}
	})
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	hs := s.siteHealths(ctx, siteIDs)
	online := 0
	for _, h := range hs {
		if h.online {
			online++
		}
	}
	open := 0
	if s.ops.Alerts != nil {
		open = s.ops.Alerts.OpenCount(site)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites_total": len(siteIDs), "sites_online": online, "accounts": acc,
		"revenue":            map[string]any{"today_kes": today, "month_kes": month, "last_month_kes": last},
		"unmatched_payments": unmatched, "unregistered_devices": unreg, "open_alerts": open, "pending_router_tasks": tasks,
		"generated_at": now.UTC().Format(time.RFC3339)})
}

// ---- alerts --------------------------------------------------------------------------------------

func (s *Server) apiAlerts(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	names := map[string]string{}
	for _, st := range s.liveSites("") {
		names[st.ID] = st.Name
	}
	active, history, mutes := []map[string]any{}, []map[string]any{}, []map[string]any{}
	if e := s.ops.Alerts; e != nil {
		for _, a := range e.Active() {
			active = append(active, map[string]any{"key": a.Key, "rule": a.Rule, "label": alerts.Label(a.Rule), "site_id": a.SiteID,
				"site_name": names[a.SiteID], "state": a.State, "since": tp(a.Since), "detail": a.Detail})
		}
		for _, h := range e.History(50) {
			history = append(history, map[string]any{"key": h.Key, "rule": h.Rule, "label": alerts.Label(h.Rule), "site_id": h.SiteID,
				"site_name": names[h.SiteID], "start": tp(h.Start), "fired_at": tp(h.FiredAt), "end": tp(h.End), "detail": h.Detail})
		}
		ms := e.Mutes()
		ids := make([]string, 0, len(ms))
		for id := range ms {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			mutes = append(mutes, map[string]any{"site_id": id, "until": tp(ms[id])})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "history": history, "mutes": mutes})
}

func (s *Server) apiAlertMute(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		SiteID  string `json:"site_id"`
		Minutes int    `json:"minutes"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if !s.siteExists(req.SiteID) {
		validationErr(w, map[string]string{"site_id": "no such router"})
		return
	}
	if req.Minutes < 0 || req.Minutes > 7*24*60 {
		validationErr(w, map[string]string{"minutes": "must be 0 (unmute) to 10080 (a week)"})
		return
	}
	if s.ops.Alerts == nil {
		apiErr(w, http.StatusConflict, codeBadState, "The alert engine is not running.", nil)
		return
	}
	if req.Minutes == 0 {
		s.ops.Alerts.Unmute(req.SiteID)
		c.audit(s, "alerts unmuted for "+req.SiteID)
	} else {
		s.ops.Alerts.Mute(req.SiteID, s.svc.Now().Add(time.Duration(req.Minutes)*time.Minute))
		c.audit(s, "alerts muted for "+req.SiteID+" for "+strconv.Itoa(req.Minutes)+" min")
	}
	w.WriteHeader(http.StatusNoContent)
}

// firstPort is the first of a comma list of ports ("" when empty): the deprecated bench_port.
func firstPort(list string) string {
	if p := sites.SplitPorts(list); len(p) > 0 {
		return p[0]
	}
	return ""
}

// portsJSON is a port list as a JSON array ([] when empty, never null).
func portsJSON(list string) []string {
	if l := sites.SplitPorts(list); l != nil {
		return l
	}
	return []string{}
}
