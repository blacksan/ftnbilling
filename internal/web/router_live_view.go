package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ispbill/internal/sites"
	"ispbill/internal/store"
)

// The router page's live tabs. They read the same views as the API
// (docs/api/v4-w9.md): sites.Manager's LiveRouter, LiveInterfaces and the
// clients merge, so the web admin and the apps show one set of numbers.

// rtStats is the Overview tab's cards, from RouterLive.
type rtStats struct {
	Reachable                                     bool
	CPU, Mem, MemSub, Uptime, WAN, WANSub, Tunnel string
	TunnelSub, FlatsOn, FlatsSub                  string
	Model, Version, Temp, Equip                   string
	OpenAlerts                                    int
	UnregOnline                                   int
	Revenue                                       string
	WANPct                                        float64
}

func fmtUptime(sec int64) string {
	if sec <= 0 {
		return "n/a"
	}
	d, h, m := sec/86400, sec%86400/3600, sec%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func fmtBytes(n uint64) string {
	f := float64(n)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%.1f TB", f/(1<<40))
	case f >= 1<<30:
		return fmt.Sprintf("%.1f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.0f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f KB", f/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func fmtRateP(v *int64) string {
	if v == nil {
		return "–"
	}
	return fmtBps(float64(*v))
}

func statsOf(lv sites.RouterLive) rtStats {
	x := rtStats{Reachable: lv.Reachable, CPU: "n/a", Mem: "n/a", Uptime: "n/a", WAN: "n/a", Tunnel: "n/a"}
	if lv.Reachable || lv.System.UptimeS > 0 {
		x.CPU = fmt.Sprintf("%d%%", lv.System.CPULoadPct)
		if lv.System.MemTotal > 0 {
			x.Mem = fmt.Sprintf("%.0f%%", 100*float64(lv.System.MemUsed)/float64(lv.System.MemTotal))
			x.MemSub = fmtBytes(uint64(lv.System.MemUsed)) + " of " + fmtBytes(uint64(lv.System.MemTotal))
		}
		x.Uptime = fmtUptime(lv.System.UptimeS)
		x.Model, x.Version = lv.System.Model, lv.System.Version
		if lv.System.TempC != nil {
			x.Temp = fmt.Sprintf("%.0f C", *lv.System.TempC)
		}
	}
	if lv.WAN.DownBps != nil {
		x.WAN = fmtBps(float64(*lv.WAN.DownBps))
		up := int64(0)
		if lv.WAN.UpBps != nil {
			up = *lv.WAN.UpBps
		}
		x.WANSub = "down now, " + fmtBps(float64(up)) + " up"
		if lv.WAN.UtilPct != nil {
			x.WANPct = *lv.WAN.UtilPct
		}
	} else if lv.WAN.Interface != "" {
		x.WANSub = lv.WAN.Interface
		if !lv.WAN.Running {
			x.WANSub += " is down"
		}
	}
	switch {
	case lv.Tunnel.RTTMs != nil:
		x.Tunnel, x.TunnelSub = fmt.Sprintf("%.0f ms", *lv.Tunnel.RTTMs), "router to the server"
	case lv.Tunnel.HandshakeAgeS != nil:
		x.Tunnel = "up"
	}
	if lv.Tunnel.HandshakeAgeS != nil {
		x.TunnelSub = fmt.Sprintf("handshake %d s ago", *lv.Tunnel.HandshakeAgeS)
	}
	x.FlatsOn = fmt.Sprint(lv.Flats.Online)
	x.FlatsSub = fmt.Sprintf("of %d flats", lv.Flats.Total)
	x.Equip = fmt.Sprintf("%d OLT, %d ONT (%d bound)", lv.Equipment.OLT, lv.Equipment.ONT, lv.ONTsBound)
	x.OpenAlerts, x.UnregOnline = lv.OpenAlerts, lv.UnregisteredOnline
	x.Revenue = fmt.Sprintf("KES %d today, KES %d this month", lv.Revenue.TodayKES, lv.Revenue.MonthKES)
	return x
}

// ifaceView is one row of the Interfaces tab.
type ifaceView struct {
	sites.Interface
	Rx, Tx, Addrs, Link string
}

// routerTabData fills p.Data for the tab being drawn. Only the tab's own
// views are read, each with its own short deadline.
func (s *Server) routerTabData(ctx context.Context, data map[string]any, st store.Site, status sites.SiteStatus, tab, q string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	switch tab {
	case "overview":
		lv, err := s.buildLive(ctx, st)
		if err == nil {
			data["Stats"], data["Live"] = statsOf(lv), lv
		} else {
			data["Stats"] = rtStats{CPU: "n/a", Mem: "n/a", Uptime: "n/a", WAN: "n/a", Tunnel: "n/a", FlatsOn: "0"}
		}
	case "clients":
		v, err := s.buildClients(ctx, st, q, "", "")
		data["Q"] = q
		if err == nil {
			data["Clients"], data["Counts"], data["Truncated"], data["Reachable"] = v.Items, v.Counts, v.Truncated, v.Reachable
			data["ClientsAt"] = nowEAT(v.GeneratedAt)
		}
	case "interfaces":
		iv, err := s.mgr.LiveInterfaces(ctx, st.ID)
		if err != nil {
			return
		}
		rows := make([]ifaceView, 0, len(iv.Items))
		for _, it := range iv.Items {
			r := ifaceView{Interface: it, Rx: fmtRateP(it.RxBps), Tx: fmtRateP(it.TxBps), Addrs: strings.Join(it.Addresses, ", ")}
			if it.LastLinkUp != nil {
				r.Link = *it.LastLinkUp
			}
			rows = append(rows, r)
		}
		data["Ifaces"], data["Reachable"], data["IfacesAt"] = rows, iv.Reachable, nowEAT(iv.GeneratedAt)
	}
}

// adminSiteReboot is the Danger tab's Reboot router: the API's guarded path
// (owner, typed name, refused while a set-up job runs, one per 10 minutes).
func (s *Server) adminSiteReboot(w http.ResponseWriter, r *http.Request) {
	st, ok := s.siteID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	dest := "/admin/routers/view?id=" + url.QueryEscape(st.ID) + "&tab=danger"
	if f := s.rebootRouter(r.Context(), st, r.PostFormValue("confirm_name")); f != nil {
		code := http.StatusConflict
		switch f.Kind {
		case "validation":
			code = http.StatusUnprocessableEntity
		case "rate":
			code = http.StatusTooManyRequests
			w.Header().Set("Retry-After", fmt.Sprint(int(f.Wait.Seconds())+1))
		}
		if wantsJSON(r) {
			writeJSON(w, code, map[string]any{"ok": false, "error": f.Msg})
			return
		}
		s.back(w, r, dest, "Error: "+f.Msg)
		return
	}
	s.audit(r, "router "+st.Name+" ("+st.ID+") reboot sent")
	if wantsJSON(r) {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "queued": true})
		return
	}
	s.back(w, r, dest, "Reboot sent to "+st.Name+". It is back in a minute or two.")
}
