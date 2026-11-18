package web

import (
	"context"
	"net/http"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// overviewRoutes registers GET /api/v1/overview (new in v4: /routers has no
// meaning here, the optional router comes as ?router=, mapped to ?site=).
func (s *Server) overviewRoutes(h func(pattern, role string, f apiFunc)) {
	h("GET /api/v1/overview", "viewer", s.apiOverview)
}

func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	site := r.URL.Query().Get("site")
	if site != "" && !s.siteExists(site) {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such router.", nil)
		return
	}
	list := s.liveSites(site)
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	hs := s.siteHealths(ctx, list)
	now := s.svc.Now()
	in := billing.OverviewInput{SiteID: site, RouterOnline: map[string]bool{}, RouterLive: map[string]bool{}, RouterLoad: map[string]billing.RouterLoad{}}
	for _, st := range list {
		on := hs[st.ID].online
		in.RouterOnline[st.ID] = on
		switch js := s.mgr.Jobs().State(st.ID); {
		case js != "":
			in.RouterLive[st.ID] = on && js == "live"
		default:
			in.RouterLive[st.ID] = on && accessMode(st) == store.AccessIPoE
		}
		if col := s.mgr.Collector(st.ID); col != nil {
			eff := s.mgr.Effective(st)
			if lp, ok := col.Snapshot().Ports[eff.WANInterface]; ok && now.Sub(lp.At) < 5*time.Minute {
				l := billing.RouterLoad{DownBps: int64(lp.RxBps)}
				if eff.WANCapMbps > 0 {
					l.UtilPct = float64(int(max(lp.RxBps, lp.TxBps)/(eff.WANCapMbps*1e6)*1000+0.5)) / 10
				}
				in.RouterLoad[st.ID] = l
			}
		}
	}
	facts := s.flatFacts()
	online := s.onlineFn(now)
	in.Online = online
	in.LastSeen = func(a store.Account) time.Time { return facts(a).rs.LastSeen }
	cfg := s.live()
	in.MpesaConfigured = cfg.MpesaKey != "" && cfg.MpesaSecret != "" && cfg.MpesaShortcode != ""
	in.WhatsAppConfigured = cfg.WhatsAppConfigured()
	writeJSON(w, http.StatusOK, s.svc.Overview(in))
}
