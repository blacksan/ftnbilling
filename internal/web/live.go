package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"ispbill/internal/store"
)

// Live now: who is connected and how hard they are pulling. Rates come from
// the collectors' latest samples (no router call), unregistered devices from
// the devices the routers reported.

type liveRow struct {
	SiteID string  `json:"-"`
	Kind   string  `json:"kind"` // flat | unregistered
	Code   string  `json:"code,omitempty"`
	Flat   string  `json:"flat,omitempty"`
	Tenant string  `json:"tenant,omitempty"`
	IP     string  `json:"ip,omitempty"`
	MAC    string  `json:"mac,omitempty"`
	Down   float64 `json:"down_bps"`
	Up     float64 `json:"up_bps"`
	Cap    float64 `json:"cap_bps"`
	Router string  `json:"router"`
	Href   string  `json:"href"`
}

type liveSet struct {
	Rows         []liveRow
	FlatsOnline  int
	Unregistered int
	Down, Up     float64
	Busiest      string
}

// liveWindow is how fresh a flat's last rate sample must be to count as online.
const liveWindow = 3 * time.Minute

func (s *Server) liveRows(sc scope) liveSet {
	var ls liveSet
	now := s.svc.Now()
	names := map[string]string{}
	byCode := map[string]map[string]store.Account{}
	s.st.View(func(d *store.Data) {
		for _, st := range sc.Sites {
			if sc.Has(st.ID) {
				names[st.ID] = st.Name
			}
		}
		for _, a := range d.Accounts {
			if !sc.Has(a.SiteID) {
				continue
			}
			if byCode[a.SiteID] == nil {
				byCode[a.SiteID] = map[string]store.Account{}
			}
			byCode[a.SiteID][a.Code] = a
		}
	})
	var best float64
	for id := range names {
		col := s.mgr.Collector(id)
		if col == nil {
			continue
		}
		for code, lr := range col.Snapshot().Flats {
			a, ok := byCode[id][code]
			if !ok || now.Sub(lr.At) > liveWindow {
				continue
			}
			ls.Rows = append(ls.Rows, liveRow{SiteID: id, Kind: "flat", Code: code, Flat: a.Flat, Tenant: a.Tenant, IP: firstNonEmpty(lr.Address, a.IP), MAC: a.MAC,
				Down: lr.DownBps, Up: lr.UpBps, Cap: lr.CapBps, Router: names[id], Href: "/admin/accounts/view?code=" + code})
			ls.FlatsOnline++
			ls.Down += lr.DownBps
			ls.Up += lr.UpBps
			if lr.DownBps > best {
				best = lr.DownBps
				ls.Busiest = a.Flat + " (" + fmtBps(lr.DownBps) + ")"
			}
		}
		for _, u := range s.svc.Unregistered(id, true) {
			ls.Rows = append(ls.Rows, liveRow{SiteID: id, Kind: "unregistered", MAC: u.MAC, IP: u.IP, Flat: u.MAC, Tenant: firstNonEmpty(u.Vendor, u.HostName), Router: names[id], Href: "/admin/devices"})
			ls.Unregistered++
		}
	}
	sort.SliceStable(ls.Rows, func(i, j int) bool {
		if ls.Rows[i].Kind != ls.Rows[j].Kind {
			return ls.Rows[i].Kind == "flat"
		}
		return ls.Rows[i].Down > ls.Rows[j].Down
	})
	return ls
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

func (s *Server) adminLive(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "live")
	ls := s.liveRows(s.scopeOf(r))
	p.Data["L"] = ls
	p.Data["Collecting"] = s.anyCollector(s.scopeOf(r))
	s.render(w, "admin_live", p)
}

func (s *Server) anyCollector(sc scope) bool {
	for _, st := range sc.Sites {
		if sc.Has(st.ID) && s.mgr.Collector(st.ID) != nil {
			return true
		}
	}
	return false
}

// adminLiveJSON feeds the 10 second refresh on the Live now page.
func (s *Server) adminLiveJSON(w http.ResponseWriter, r *http.Request) {
	ls := s.liveRows(s.scopeOf(r))
	rows := ls.Rows
	if rows == nil {
		rows = []liveRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"generated_at": s.svc.Now(), "rows": rows, "online": ls.FlatsOnline, "flats": ls.FlatsOnline, "unregistered": ls.Unregistered,
		"down_bps": ls.Down, "up_bps": ls.Up, "busiest": ls.Busiest,
	})
}
