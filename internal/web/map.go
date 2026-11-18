package web

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"ispbill/internal/store"
)

// Network map: a drawing of server, tunnel, router, OLT and flats, made on the
// server as inline SVG. There are no map tiles: the CSP keeps the page on this
// origin, so a location is a coordinate and a geo: link for the phone's map app.

type mapRouter struct {
	S          store.Site
	State      string // up | down | paused | waiting
	Flats      int
	Online     int
	HasGeo     bool
	GeoLink    string
	Accounts   int
	Tunnel     string
	StateClass string
}

// siteStates pings the runnable sites in parallel (each answer is cached for 15 s).
func (s *Server) siteStates(ctx context.Context, list []store.Site) map[string]string {
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, st := range list {
		switch {
		case st.Disabled:
			out[st.ID] = "paused"
		case st.Mode != store.SiteModeEnv && st.WGPublicKey == "":
			out[st.ID] = "waiting"
		case !st.Runnable():
			out[st.ID] = "waiting"
		default:
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				st := "down"
				if s.siteOnline(ctx, id) {
					st = "up"
				}
				mu.Lock()
				out[id] = st
				mu.Unlock()
			}(st.ID)
		}
	}
	wg.Wait()
	return out
}

func (s *Server) adminMap(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "map")
	sc := s.scopeOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var list []store.Site
	for _, st := range sc.Sites {
		if sc.Has(st.ID) {
			list = append(list, st)
		}
	}
	states := s.siteStates(ctx, list)
	live := s.liveRows(sc)
	counts := map[string][2]int{}
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			c := counts[a.SiteID]
			c[0]++
			counts[a.SiteID] = c
		}
	})
	for _, row := range live.Rows {
		if row.Kind == "flat" {
			c := counts[row.SiteID]
			c[1]++
			counts[row.SiteID] = c
		}
	}
	var rows []mapRouter
	geo, down := 0, 0
	for _, st := range list {
		mr := mapRouter{S: st, State: states[st.ID], Accounts: counts[st.ID][0], Online: counts[st.ID][1]}
		mr.HasGeo = st.Lat != 0 || st.Lng != 0
		if mr.HasGeo {
			geo++
			mr.GeoLink = fmt.Sprintf("geo:%.6f,%.6f", st.Lat, st.Lng)
		}
		if mr.State == "down" {
			down++
		}
		rows = append(rows, mr)
	}
	p.Data["Rows"], p.Data["Total"], p.Data["Geo"], p.Data["Down"] = rows, len(rows), geo, down
	p.Data["Svg"] = topoSVG(rows, s.cfg.BrandName)
	s.render(w, "admin_map", p)
}

func esc(v string) string { return html.EscapeString(v) }

func topoSVG(rows []mapRouter, brand string) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	const colW, top = 200, 20
	w := colW * len(rows)
	if w < 640 {
		w = 640
	}
	h := 330
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="topo" viewBox="0 0 %d %d" role="img" aria-label="Server, then each router, its OLT and its flats"><title>Network topology</title>`, w, h)
	cx := w / 2
	// edges first so nodes draw over them
	for i, mr := range rows {
		x := (w-colW*len(rows))/2 + colW*i + colW/2
		cls := "edge"
		switch mr.State {
		case "up":
			cls += " up"
		case "down":
			cls += " down"
		}
		fmt.Fprintf(&b, `<path class="%s" d="M%d %d C%d %d %d %d %d %d"/>`, cls, cx, top+44, cx, top+100, x, top+80, x, top+120)
		fmt.Fprintf(&b, `<path class="%s" d="M%d %d V%d"/>`, cls, x, top+164, top+210)
		fmt.Fprintf(&b, `<path class="edge" d="M%d %d V%d"/>`, x, top+254, top+276)
	}
	fmt.Fprintf(&b, `<rect class="nd root" x="%d" y="%d" width="150" height="44" rx="14"/><text class="root" x="%d" y="%d" text-anchor="middle">%s</text>`, cx-75, top, cx, top+27, esc(clip(brand, 18)))
	for i, mr := range rows {
		x := (w-colW*len(rows))/2 + colW*i + colW/2
		cls := "nd"
		if mr.State == "up" || mr.State == "down" {
			cls += " " + mr.State
		}
		fmt.Fprintf(&b, `<a href="/admin/routers/view?id=%s"><rect class="%s" x="%d" y="%d" width="164" height="44" rx="12"/><text x="%d" y="%d" text-anchor="middle">%s</text><text class="sm" x="%d" y="%d" text-anchor="middle">%s</text></a>`,
			esc(mr.S.ID), cls, x-82, top+120, x, top+140, esc(clip(mr.S.Name, 20)), x, top+155, esc(mr.State))
		fmt.Fprintf(&b, `<rect class="nd" x="%d" y="%d" width="96" height="40" rx="12"/><text x="%d" y="%d" text-anchor="middle">OLT</text><text class="sm" x="%d" y="%d" text-anchor="middle">%s</text>`,
			x-48, top+210, x, top+228, x, top+242, esc(clip(oltName(mr.S), 14)))
		fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="middle">%d flats</text><text class="sm" x="%d" y="%d" text-anchor="middle">%d online now</text>`, x, top+292, mr.Accounts, x, top+308, mr.Online)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func oltName(s store.Site) string {
	if s.OLTInterface != "" {
		return s.OLTInterface
	}
	return "uplink"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
