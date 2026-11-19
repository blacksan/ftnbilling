package web

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/store"
)

// fleetTile is one building on the fleet page.
type fleetTile struct {
	S         store.Site
	State     string // up, down, unknown, paused, waiting
	Class     string // good, warn, bad or ""
	Since     string
	Cause     string
	Handshake string
	WAN       string
	WANPct    float64
	HasWAN    bool
	Router    string // router ping to the internet
	Tunnel    string
	Active    int
	Total     int
	Today     int
	Month     int
	Alerts    int
	Muted     bool
	Notice    bool // the "tell tenants" button applies
	Notified  int  // outage notices already sent for the current outage
	Err       string
}

type fleetTotals struct {
	Sites, Up, Down, Other int
	Active, Total          int
	Today, Month           int
	Alerts                 int
}

func (s *Server) fleetTiles(sc scope) ([]fleetTile, fleetTotals) {
	now := s.svc.Now()
	nowE := now.In(billing.EAT)
	dayStart := time.Date(nowE.Year(), nowE.Month(), nowE.Day(), 0, 0, 0, 0, billing.EAT)
	monthStart := time.Date(nowE.Year(), nowE.Month(), 1, 0, 0, 0, 0, billing.EAT)
	type agg struct{ active, total, today, month int }
	per := map[string]*agg{}
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			g := per[a.SiteID]
			if g == nil {
				g = &agg{}
				per[a.SiteID] = g
			}
			g.total++
			if billing.AccountState(a, now) == billing.StateActive {
				g.active++
			}
		}
		for _, p := range d.Payments {
			if !countsAsRevenue(p) {
				continue
			}
			g := per[p.SiteID]
			if g == nil {
				g = &agg{}
				per[p.SiteID] = g
			}
			if !p.PaidAt.Before(dayStart) {
				g.today += p.AmountKES
			}
			if !p.PaidAt.Before(monthStart) {
				g.month += p.AmountKES
			}
		}
	})
	states := s.ops.Fleet.States()
	mutes := map[string]time.Time{}
	if s.ops.Alerts != nil {
		mutes = s.ops.Alerts.Mutes()
	}
	var tiles []fleetTile
	var tot fleetTotals
	for _, st := range sc.Sites {
		if !sc.All && st.ID != sc.ID {
			continue
		}
		t := fleetTile{S: st, State: "unknown"}
		g := per[st.ID]
		if g != nil {
			t.Active, t.Total, t.Today, t.Month = g.active, g.total, g.today, g.month
		}
		if s.ops.Alerts != nil {
			t.Alerts = s.ops.Alerts.OpenCount(st.ID)
		}
		_, t.Muted = mutes[st.ID]
		fs := states[st.ID]
		switch {
		case st.Disabled:
			t.State = "paused"
		case !st.Runnable():
			t.State = "waiting"
		case fs.Status == fleet.Up:
			t.State, t.Class = "up", "good"
		case fs.Status == fleet.Down:
			t.State, t.Class = "down", "bad"
		}
		if t.State == "up" && t.Alerts > 0 {
			t.Class = "warn"
		}
		if !fs.Since.IsZero() && (t.State == "up" || t.State == "down") {
			t.Since = ago(now.Sub(fs.Since))
		}
		t.Err = fs.LastErr
		if t.State == "down" {
			if o, ok := s.ops.Fleet.OpenOutage(st.ID); ok {
				t.Cause = causeText(o.Cause)
			}
			t.Notice = s.ops.Tenant != nil && s.ops.Alerts != nil && s.ops.Alerts.Settings().OutageNotice != "off"
		}
		switch {
		case st.Mode == store.SiteModeEnv:
			t.Handshake = "no tunnel (local router)"
		case fs.HandshakeAge >= 0:
			t.Handshake = ago(time.Duration(fs.HandshakeAge)*time.Second) + " ago"
		case fs.WGKnown:
			t.Handshake = "never"
		default:
			t.Handshake = "unknown"
		}
		if ms, loss, ok := s.ops.Fleet.Recent(st.ID, 10*time.Minute, now); ok {
			if st.Mode == store.SiteModeEnv {
				t.Tunnel = "-"
			} else if ms > 0 {
				t.Tunnel = fmt.Sprintf("%.0f ms", ms)
				if loss > 0 {
					t.Tunnel += fmt.Sprintf(", %.0f%% lost", loss)
				}
			} else {
				t.Tunnel = "no reply"
			}
		}
		if col := s.mgr.Collector(st.ID); col != nil && t.State != "down" {
			live, cfg := col.Snapshot(), col.Config()
			if p, ok := live.Ports[cfg.WAN]; ok && cfg.WANCapMbps > 0 && time.Since(p.At) < 5*time.Minute {
				mbps := math.Max(p.RxBps, p.TxBps) / 1e6
				t.HasWAN, t.WANPct = true, math.Min(100, mbps/cfg.WANCapMbps*100)
				t.WAN = fmt.Sprintf("%.0f of %.0f Mbps", mbps, cfg.WANCapMbps)
			}
			var rtt, loss float64
			var n, nr int
			for _, p := range live.Ping {
				n++
				loss += p.LossPct
				if p.LossPct < 100 {
					rtt += p.RTTms
					nr++
				}
			}
			if n > 0 {
				t.Router = fmt.Sprintf("%.1f%% lost", loss/float64(n))
				if nr > 0 {
					t.Router = fmt.Sprintf("%.0f ms, ", rtt/float64(nr)) + t.Router
				}
			}
		}
		if s.ops.Box != nil && t.State == "down" {
			if o, ok := s.ops.Fleet.OpenOutage(st.ID); ok {
				t.Notified = len(s.ops.Box.Find(func(m notify.Message) bool {
					return m.Kind == "outage_notice" && m.SiteID == st.ID && m.Ref == o.Start.Unix()
				}))
			}
		}
		tiles = append(tiles, t)
		tot.Sites++
		switch t.State {
		case "up":
			tot.Up++
		case "down":
			tot.Down++
		default:
			tot.Other++
		}
		tot.Active += t.Active
		tot.Total += t.Total
		tot.Today += t.Today
		tot.Month += t.Month
		tot.Alerts += t.Alerts
	}
	return tiles, tot
}

// adminFleet: the fleet overview is the Routers page now (it carries the same
// status, flats, WAN and tunnel columns). The old address keeps working.
func (s *Server) adminFleet(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/routers", http.StatusFound)
}

// ---- uptime ---------------------------------------------------------------

type dayCell struct {
	Label, Class string
	Title        string
}

func availClass(pct float64) string {
	switch {
	case pct < 0:
		return "none"
	case pct >= 99.9:
		return "a"
	case pct >= 99:
		return "b"
	case pct >= 95:
		return "c"
	}
	return "d"
}

type uptimeRow struct {
	S       store.Site
	Pct     string
	Outages int
	Down    string
}

func (s *Server) adminUptime(w http.ResponseWriter, r *http.Request) {
	if !s.hasOps() {
		s.notReady(w, r, "uptime", "The fleet monitor")
		return
	}
	p := s.page(r, "uptime")
	sc := s.scopeOf(r)
	days := 30
	if r.URL.Query().Get("days") == "90" {
		days = 90
	}
	id := r.URL.Query().Get("site")
	if id == "" {
		id = sc.ID
	}
	var cur store.Site
	for _, st := range sc.Sites {
		if st.ID == id {
			cur = st
		}
	}
	if cur.ID == "" && len(sc.Sites) > 0 {
		cur = sc.Sites[0]
	}
	p.Data["Days"], p.Data["Sites"] = days, sc.Sites
	if cur.ID == "" {
		p.Data["None"] = true
		s.render(w, "admin_uptime", p)
		return
	}
	now := s.svc.Now()
	cells, overall := s.ops.Fleet.Availability(cur.ID, days, now)
	var strip []dayCell
	for _, c := range cells {
		t := c.Day.In(billing.EAT).Format("Mon 2 Jan")
		title := t + ": no data"
		if c.Pct >= 0 {
			title = fmt.Sprintf("%s: %.2f%% available", t, c.Pct)
		}
		strip = append(strip, dayCell{Class: availClass(c.Pct), Title: title})
	}
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	ts, vs := s.ops.Fleet.RTT(cur.ID, metrics.TD, from, now.Add(time.Minute))
	chart := metrics.LineChart(metrics.LineOpts{T: ts, XLayout: "2 Jan", Unit: "ms (daily average tunnel round trip)", Height: 220,
		Series: []metrics.LineSeries{{Name: "tunnel RTT", Color: metrics.ColViol, Vals: vs}}, Empty: "No probe data yet. It fills as the monitor runs."})
	outs := s.ops.Fleet.Outages(cur.ID, from)
	type outRow struct {
		Start, End, Dur, Cause string
		Open                   bool
	}
	var rows []outRow
	var downTotal time.Duration
	for _, o := range outs {
		end := now
		if !o.Open() {
			end = o.End
		}
		d := end.Sub(o.Start)
		downTotal += d
		row := outRow{Start: o.Start.In(billing.EAT).Format("Mon 2 Jan, 15:04"), Dur: ago(d), Cause: causeText(o.Cause), Open: o.Open()}
		if !o.Open() {
			row.End = o.End.In(billing.EAT).Format("Mon 2 Jan, 15:04")
		}
		rows = append(rows, row)
	}
	// every building at a glance
	var all []uptimeRow
	for _, st := range sc.Sites {
		_, ov := s.ops.Fleet.Availability(st.ID, days, now)
		ur := uptimeRow{S: st, Pct: "no data"}
		if ov >= 0 {
			ur.Pct = fmt.Sprintf("%.2f%%", ov)
		}
		var dt time.Duration
		for _, o := range s.ops.Fleet.Outages(st.ID, from) {
			ur.Outages++
			end := now
			if !o.Open() {
				end = o.End
			}
			dt += end.Sub(o.Start)
		}
		ur.Down = ago(dt)
		all = append(all, ur)
	}
	pct := "no data yet"
	if overall >= 0 {
		pct = fmt.Sprintf("%.2f%%", overall)
	}
	p.Data["Cur"], p.Data["Strip"], p.Data["Pct"], p.Data["Chart"] = cur, strip, pct, chart
	p.Data["Outages"], p.Data["DownTotal"], p.Data["All"] = rows, ago(downTotal), all
	p.Data["QS"] = "site=" + url.QueryEscape(cur.ID)
	p.Data["Days30"], p.Data["Days90"] = strconv.Itoa(30), strconv.Itoa(90)
	s.render(w, "admin_uptime", p)
}
