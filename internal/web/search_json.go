package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

type searchItem struct {
	Title string `json:"title"`
	Sub   string `json:"sub,omitempty"`
	Href  string `json:"href"`
}

type searchGroup struct {
	Name  string       `json:"name"`
	Items []searchItem `json:"items"`
}

const searchPerGroup = 6

// adminSearchJSON feeds the Cmd-K palette: the same lookup as the search page
// (billing.Search, limited to the routers the admin is looking at), grouped for
// display. Nothing here is a secret: names, flats, codes, receipts.
func (s *Server) adminSearchJSON(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sc := s.scopeOf(r)
	out := []searchGroup{}
	if len(q) >= 2 {
		res := s.svc.Search(q, sc.Has)
		names := map[string]string{}
		for _, st := range sc.Sites {
			names[st.ID] = st.Name
		}
		cut := func(n int) int {
			if n > searchPerGroup {
				return searchPerGroup
			}
			return n
		}
		var g searchGroup
		g = searchGroup{Name: "Subscribers"}
		for _, a := range res.Accounts[:cut(len(res.Accounts))] {
			g.Items = append(g.Items, searchItem{Title: a.Flat, Sub: joinNonEmpty(" · ", a.Tenant, a.Code, names[a.SiteID]), Href: "/admin/accounts/view?code=" + url.QueryEscape(a.Code)})
		}
		out = append(out, g)
		g = searchGroup{Name: "Apartments"}
		for _, a := range res.Apartments[:cut(len(res.Apartments))] {
			g.Items = append(g.Items, searchItem{Title: a.Name, Sub: joinNonEmpty(" · ", a.Address, a.Caretaker.Name, a.Landlord.Name), Href: aptURL(a.ID)})
		}
		out = append(out, g)
		g = searchGroup{Name: "Contacts"}
		for _, c := range res.Customers[:cut(len(res.Customers))] {
			g.Items = append(g.Items, searchItem{Title: c.Name, Sub: c.Phone, Href: "/admin/customers/view?id=" + url.QueryEscape(c.ID)})
		}
		out = append(out, g)
		g = searchGroup{Name: "Routers"}
		for _, st := range res.Buildings[:cut(len(res.Buildings))] {
			g.Items = append(g.Items, searchItem{Title: st.Name, Href: "/admin/routers/view?id=" + url.QueryEscape(st.ID)})
		}
		out = append(out, g)
		g = searchGroup{Name: "Payments"}
		for _, p := range res.Payments[:cut(len(res.Payments))] {
			g.Items = append(g.Items, searchItem{Title: p.Key, Sub: joinNonEmpty(" · ", "KES "+strconv.Itoa(p.AmountKES), p.Phone, p.PaidAt.In(billing.EAT).Format("2 Jan")), Href: "/admin/payments?q=" + url.QueryEscape(p.Key)})
		}
		out = append(out, g)
		g = searchGroup{Name: "Equipment"}
		for _, e := range res.Equipment[:cut(len(res.Equipment))] {
			g.Items = append(g.Items, searchItem{Title: firstNonEmpty(e.Serial, e.MAC, e.Model), Sub: e.Model, Href: "/admin/equipment/view?id=" + url.QueryEscape(e.ID)})
		}
		out = append(out, g)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"q": q, "groups": out})
}

func joinNonEmpty(sep string, v ...string) string {
	var o []string
	for _, x := range v {
		if x != "" {
			o = append(o, x)
		}
	}
	return strings.Join(o, sep)
}

// ---- audit trail ---------------------------------------------------------------------

type auditRow struct {
	Time, Kind, Actor, Msg, Router, Href string
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "audit")
	sc := s.scopeOf(r)
	names := map[string]string{}
	for _, st := range sc.Sites {
		names[st.ID] = st.Name
	}
	var rows []auditRow
	var today, week int
	now := s.svc.Now()
	s.st.View(func(d *store.Data) {
		ev := d.Events
		for i := len(ev) - 1; i >= 0 && len(rows) < 300; i-- {
			e := ev[i]
			if e.Kind != "audit" || (e.SiteID != "" && !sc.Has(e.SiteID)) {
				continue
			}
			if now.Sub(e.Time) < 24*time.Hour {
				today++
			}
			if now.Sub(e.Time) < 7*24*time.Hour {
				week++
			}
			href := ""
			if e.AccountID != "" {
				href = "/admin/accounts"
			}
			rows = append(rows, auditRow{Time: nowEAT(e.Time), Kind: e.Kind, Actor: e.Actor, Msg: e.Msg, Router: names[e.SiteID], Href: href})
		}
	})
	p.Data["Rows"], p.Data["Today"], p.Data["Week"] = rows, today, week
	s.render(w, "admin_audit", p)
}
