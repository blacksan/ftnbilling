package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"ispbill/internal/acs"
	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// ONU management (TR-069): the web screens. The ACS itself lives in internal/acs; nothing here ever shows or logs a
// Wi-Fi password, and the ACS login is revealed only behind the owner's password.

// SetACS installs the ONU management service. Call it before Handler. Without it the screens say it is off.
func (s *Server) SetACS(a *acs.Service) { s.acs = a }

type onuRevealKey struct{}

func (s *Server) onuRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/settings/onu", s.settingsONU)
	post("/admin/settings/onu/reveal", s.settingsONUReveal)
	post("/admin/settings/onu/rotate", s.settingsONURotate)
	get("/admin/settings/onu/reveal", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/settings/onu", http.StatusSeeOther)
	})
	get("/admin/onus", s.adminONUs)
	post("/admin/onus/link", s.adminONULink)
	post("/admin/onus/unlink", s.adminONUUnlink)
	post("/admin/sites/onu/enable", s.adminSiteONUEnable)
	post("/admin/accounts/swaponu", s.adminAccountSwapONU)
	post("/admin/onus/inventory", s.adminONUInventory)
	post("/admin/onus/usemac", s.adminONUUseMAC)
	post("/admin/accounts/wifi", s.adminAccountWiFi)
	post("/admin/accounts/onu/reboot", s.adminAccountONUReboot)
	post("/admin/accounts/onu/refresh", s.adminAccountONURefresh)
}

func (s *Server) acsOff(w http.ResponseWriter, r *http.Request) bool {
	if s.acs != nil {
		return false
	}
	s.notice(w, r, http.StatusNotFound, "ONU management is off", "This server runs without the TR-069 service (ACS_LISTEN=off, or no DATA_KEY).", "/admin", "Back to the admin")
	return true
}

// ---- settings page ----

func (s *Server) settingsONU(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "set-onu")
	p.Data["Tabs"] = s.settingsTabs("onu")
	p.Data["ActiveTab"] = "onu"
	p.Data["On"] = s.acs != nil
	if s.acs != nil {
		port, url := s.mgr.ACSAddress()
		p.Data["URL"], p.Data["Port"] = url, port
		p.Data["User"] = s.acs.ACSUser()
		p.Data["Interval"] = s.cfg.ACSInformInterval
		p.Data["CRPorts"] = s.cfg.ACSCRPorts
		if r.Context().Value(onuRevealKey{}) == true {
			if _, pw, err := s.acs.Credentials(); err == nil {
				p.Data["Pass"] = pw
			}
			w.Header().Set("Cache-Control", "no-store")
		}
		p.Data["Default"] = store.DefaultONUMgmtVLAN
	}
	s.render(w, "admin_onu_settings", p)
}

func (s *Server) settingsONUReveal(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	if !s.reauth(w, r, "/admin/settings/onu") {
		return
	}
	s.audit(r, "ONU management login shown")
	s.settingsONU(w, r.WithContext(context.WithValue(r.Context(), onuRevealKey{}, true)))
}

func (s *Server) settingsONURotate(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := "/admin/settings/onu"
	if !s.reauth(w, r, to) {
		return
	}
	if err := s.acs.RotateCredentials(s.actorOf(r), realIP(r)); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, to, "New ONU management password made. Every ONU stays unreachable until the OLT profile gets it (see the steps below).")
}

// ---- router tab and the global list ----

func (s *Server) onuTabData(data map[string]any, st store.Site) {
	data["ONUOn"] = s.acs != nil
	if s.acs == nil {
		return
	}
	data["ONUs"] = s.acs.ONUs(st.ID)
	data["ONUVLAN"] = st.MgmtVLANOrDefault()
	data["ONUDefault"] = store.DefaultONUMgmtVLAN
	if st.ONUMgmtN > 0 {
		data["ONUSubnet"], data["ONUGateway"] = st.MgmtSubnet(), st.MgmtGateway()
		if plan, err := s.mgr.ONUPlan(st); err == nil {
			data["ONUSnippet"] = plan.Snippet(s.mgr.Effective(st).OLTInterface)
		} else {
			data["ONUPlanErr"] = err.Error()
		}
	}
	var open []store.Account
	s.st.View(func(d *store.Data) {
		linked := map[string]bool{}
		for _, o := range d.ONUs {
			if o.AccountID != "" {
				linked[o.AccountID] = true
			}
		}
		for _, a := range d.Accounts {
			if a.SiteID == st.ID && !linked[a.ID] {
				open = append(open, a)
			}
		}
	})
	sortAccountsByUnit(open)
	data["FreeAccounts"] = open
}

func sortAccountsByUnit(a []store.Account) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].Unit < a[j-1].Unit; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func (s *Server) adminONUs(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	p := s.page(r, "onus")
	list := s.acs.ONUs(p.Site)
	p.Data["ONUs"] = list
	var free []store.Account
	s.st.View(func(d *store.Data) {
		linked := map[string]bool{}
		for _, o := range d.ONUs {
			if o.AccountID != "" {
				linked[o.AccountID] = true
			}
		}
		for _, a := range d.Accounts {
			if !linked[a.ID] && (p.Site == "" || a.SiteID == p.Site) {
				free = append(free, a)
			}
		}
	})
	sortAccountsByUnit(free)
	p.Data["FreeAccounts"] = free
	s.render(w, "admin_onus", p)
}

func (s *Server) onuBack(r *http.Request, def string) string {
	if n := r.PostFormValue("next"); strings.HasPrefix(n, "/admin/") && !strings.HasPrefix(n, "//") {
		return n
	}
	return def
}

func (s *Server) adminONULink(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := s.onuBack(r, "/admin/onus")
	err := s.acs.LinkONU(s.actorOf(r), realIP(r), r.PostFormValue("onu"), r.PostFormValue("account"))
	if errors.Is(err, store.ErrNoChange) {
		err = nil
	}
	s.back(w, r, to, msgOr(err, "ONU linked to the house"))
}

func (s *Server) adminONUUnlink(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := s.onuBack(r, "/admin/onus")
	err := s.acs.UnlinkONU(s.actorOf(r), realIP(r), r.PostFormValue("onu"))
	if errors.Is(err, store.ErrNoChange) {
		err = nil
	}
	s.back(w, r, to, msgOr(err, "ONU unlinked. Its saved Wi-Fi choice was dropped."))
}

func (s *Server) adminSiteONUEnable(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := "/admin/routers/view?id=" + id + "&tab=onus"
	if s.acsOff(w, r) {
		return
	}
	vlan, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("vlan")))
	st, err := s.mgr.EnsureONUMgmt(id, vlan)
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.acs.AutoLinkSite(st.ID)
	s.audit(r, "ONU management network "+st.MgmtSubnet()+" set for router "+st.Name)
	s.back(w, r, to, "Management network "+st.MgmtSubnet()+" is allocated. Paste the lines below on the router, then set up the OLT.")
}

// ---- account card ----

func (s *Server) onuCardData(data map[string]any, a store.Account) {
	data["ONUOn"] = s.acs != nil
	if s.acs == nil {
		return
	}
	if v, ok := s.acs.OfAccount(a.ID); ok {
		data["ONU"] = v
	}
}

func (s *Server) adminAccountWiFi(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := s.accountURL(id)
	if s.acsOff(w, r) {
		return
	}
	in := acs.WiFiInput{SSID: r.PostFormValue("ssid"), Pass: r.PostFormValue("password"), Same: r.PostFormValue("same") == "1",
		SSID5: r.PostFormValue("ssid5"), Pass5: r.PostFormValue("password5")}
	if _, err := s.acs.SetWiFi(s.actorOf(r), realIP(r), id, in); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, to, "Wi-Fi change queued. The ONU is being asked to connect now; use Refresh to see the result.")
}

func (s *Server) adminAccountONUReboot(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if s.acsOff(w, r) {
		return
	}
	_, err := s.acs.RebootONU(s.actorOf(r), realIP(r), id)
	s.back(w, r, s.accountURL(id), msgOr(err, "ONU reboot queued. The house loses internet for a minute or two."))
}

func (s *Server) adminAccountONURefresh(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if s.acsOff(w, r) {
		return
	}
	_, err := s.acs.RefreshONU(s.actorOf(r), realIP(r), id)
	s.back(w, r, s.accountURL(id), msgOr(err, "Asked the ONU to report its Wi-Fi."))
}

// onuStock lists the ONUs of the register that can be given to a new house: in stock, with a WAN MAC.
func (s *Server) onuStock() []store.Equipment {
	var out []store.Equipment
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindONT && e.Status == store.EqInStock && e.MAC != "" {
				out = append(out, e)
			}
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

func (s *Server) adminAccountSwapONU(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := s.svc.SwapONU(r.Context(), id, r.PostFormValue("equipment"), r.PostFormValue("old"), "swapped from the account page")
	if err == nil && s.acs != nil {
		if a, ok := s.svc.GetAccount(id); ok {
			s.acs.AutoLinkSite(a.SiteID)
		}
	}
	s.back(w, r, s.accountURL(id), msgOr(err, "ONU swapped. The router now expects the new ONU's WAN MAC."))
}

// adminONUInventory puts an ONU that reported itself over TR-069 into the register (existing houses that have no
// register entry): serial and the chosen reported WAN MAC. When the ONU is linked to a house whose MAC is that
// MAC, the item is installed at the house too.
func (s *Server) adminONUInventory(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := s.onuBack(r, "/admin/onus")
	v, ok := s.acs.ONU(r.PostFormValue("onu"))
	if !ok {
		s.back(w, r, to, "Error: that ONU is not known")
		return
	}
	e, err := s.svc.SaveEquipment(billing.EquipmentInput{Kind: store.KindONT, Serial: v.Serial, MAC: r.PostFormValue("mac"), Model: v.Model})
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	if v.AccountID != "" {
		if a, ok := s.svc.GetAccount(v.AccountID); ok && strings.EqualFold(a.MAC, e.MAC) {
			err = s.svc.AssignEquipment(r.Context(), e.ID, a.ID, "", false)
		}
	}
	s.acs.AutoLinkSite(v.SiteID)
	s.back(w, r, to, msgOr(err, "ONU added to the register"))
}

// adminONUUseMAC trusts the MAC the ONU reported: the register and the house get it.
func (s *Server) adminONUUseMAC(w http.ResponseWriter, r *http.Request) {
	to := s.onuBack(r, "/admin/onus")
	acct := r.PostFormValue("account")
	err := s.svc.UseReportedMAC(r.Context(), acct, r.PostFormValue("mac"))
	if err == nil && s.acs != nil {
		if a, ok := s.svc.GetAccount(acct); ok {
			s.acs.AutoLinkSite(a.SiteID)
		}
	}
	s.back(w, r, to, msgOr(err, "The house now uses the MAC the ONU reported."))
}
