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
	"ispbill/internal/sites"
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
	get("/admin/onus/paramtree", s.adminONUParamTree)
	get("/admin/onus/metrics", s.onuMetricsPage)
	post("/admin/onus/link", s.adminONULink)
	post("/admin/onus/unlink", s.adminONUUnlink)
	post("/admin/sites/onu/enable", s.adminSiteONUEnable)
	post("/admin/accounts/swaponu", s.adminAccountSwapONU)
	post("/admin/onus/inventory", s.adminONUInventory)
	post("/admin/onus/usemac", s.adminONUUseMAC)
	post("/admin/accounts/wifi", s.adminAccountWiFi)
	post("/admin/accounts/wifi/show", s.adminAccountWiFiShow)
	post("/admin/accounts/onu/reboot", s.adminAccountONUReboot)
	post("/admin/accounts/onu/refresh", s.adminAccountONURefresh)
	post("/admin/accounts/onu/confirm", s.adminAccountONUConfirm)
	post("/admin/accounts/onu/reject", s.adminAccountONUReject)
	post("/admin/onus/approve", s.adminONUApprove)
	post("/admin/onus/block", s.adminONUBlock)
	post("/admin/onus/trust", s.adminONUTrust)
	post("/admin/onus/link-unknown", s.adminONULinkUnknown)
	post("/admin/onus/unblock", s.adminONUUnblock)
	post("/admin/onus/task/cancel", s.adminONUTaskCancel)
	post("/admin/onus/adminpw", s.adminONUAdminPW)
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
	data["Unknown"] = s.acs.UnknownONUs(st.ID)
	data["AddBack"] = "/admin/equipment?tab=onu"
	data["ONUVLAN"] = st.MgmtVLANOrDefault()
	data["ONUDefault"] = store.DefaultONUMgmtVLAN
	if st.ONUMgmtN > 0 {
		data["ONUSubnet"], data["ONUGateway"] = st.MgmtSubnet(), st.MgmtGateway()
		if plan, err := s.mgr.ONUPlan(st); err == nil {
			data["ONUSnippet"] = plan.Snippet(sites.ONUMgmtParent(s.mgr.Effective(st)))
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
	p.eqCrumb(eqTabONU)
	p.Detail = "TR-069 check-ins"
	list := s.acs.ONUs(p.Site)
	p.Data["ONUs"] = list
	p.Data["Unknown"] = s.acs.UnknownONUs(p.Site)
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

// adminONUParamTree downloads the parameter tree of an ONU (plain text, or JSON with format=json): operators and owners
// only, secrets withheld, never cached, and every download is audited.
func (s *Server) adminONUParamTree(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	u, _ := userOf(r)
	if u.Role != store.RoleOwner && u.Role != store.RoleOperator {
		s.notice(w, r, http.StatusForbidden, "Not allowed", "Only an operator or an owner can download an ONU's parameter tree.", "/admin/onus", "Back")
		return
	}
	v, err := s.acs.ParamTree(r.URL.Query().Get("onu"))
	if err != nil {
		s.notice(w, r, http.StatusNotFound, "No parameter tree", err.Error()+".", "/admin/onus", "Back")
		return
	}
	s.audit(r, "ONU "+v.Serial+" parameter tree downloaded by "+u.Username+" ("+strconv.Itoa(v.Count)+" parameters)")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+paramTreeFile(v, "json")+`"`)
		writeJSON(w, http.StatusOK, v)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+paramTreeFile(v, "txt")+`"`)
	_, _ = w.Write([]byte(acs.TreeText(v)))
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
	s.back(w, r, to, msgOr(err, "ONU unlinked. It is reset to factory settings when it next checks in, and it never gets a house's Wi-Fi again."))
}

// ---- quarantine, confirmation, task cancel, the box's own web-admin password ----

func (s *Server) adminONUApprove(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	err := s.acs.ApproveONU(s.actorOf(r), realIP(r), r.PostFormValue("onu"))
	s.back(w, r, s.onuBack(r, "/admin/onus"), msgOr(err, "Approved. The ONU is bound to where it checked in from."))
}

// adminONUTrust is the owner's override for an ONU that waits for its management MAC to match the registered ONU: the
// MAC it checked in from is trusted for that serial. Owner only, behind the owner's own password, audited.
func (s *Server) adminONUTrust(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := s.onuBack(r, "/admin/onus")
	if u, _ := userOf(r); u.Role != store.RoleOwner {
		s.back(w, r, to, "Error: only an owner can trust an ONU whose management MAC does not match")
		return
	}
	if !s.reauth(w, r, to) {
		return
	}
	mac := r.PostFormValue("mac")
	if mac == "" {
		s.back(w, r, to, "Error: the page did not say which management MAC to trust: reload it and try again")
		return
	}
	err := s.acs.TrustONU(s.actorOf(r), realIP(r), r.PostFormValue("onu"), mac)
	if err == nil {
		s.audit(r, "ONU management MAC trusted for "+r.PostFormValue("onu"))
	}
	s.back(w, r, to, msgOr(err, "Trusted. The ONU gets its settings at its next check-in."))
}

// adminONULinkUnknown links the serial an unknown ONU reported to an inventory item (its TR-069 serial). Owner only,
// behind the owner's own password, audited. Never done automatically: serials and MACs can be faked.
func (s *Server) adminONULinkUnknown(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := s.onuBack(r, "/admin/onus")
	if u, _ := userOf(r); u.Role != store.RoleOwner {
		s.back(w, r, to, "Error: only an owner can link an unknown ONU to an inventory item")
		return
	}
	if !s.reauth(w, r, to) {
		return
	}
	e, err := s.acs.LinkUnknown(s.actorOf(r), realIP(r), r.PostFormValue("item"), r.PostFormValue("serial"))
	if err == nil {
		s.audit(r, "unknown ONU "+e.TR069Serial+" linked to inventory item "+e.ID)
	}
	s.back(w, r, to, msgOr(err, "Linked. The ONU is the registered one from its next check-in."))
}

func (s *Server) adminONUBlock(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	err := s.acs.BlockONU(s.actorOf(r), realIP(r), r.PostFormValue("onu"))
	s.back(w, r, s.onuBack(r, "/admin/onus"), msgOr(err, "Blocked. Check-ins from that place are ignored."))
}

func (s *Server) adminONUUnblock(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	err := s.acs.UnblockONU(s.actorOf(r), realIP(r), r.PostFormValue("onu"))
	s.back(w, r, s.onuBack(r, "/admin/onus"), msgOr(err, "Unblocked."))
}

func (s *Server) adminONUTaskCancel(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	err := s.acs.CancelTask(s.actorOf(r), realIP(r), r.PostFormValue("task"))
	s.back(w, r, s.onuBack(r, "/admin/onus"), msgOr(err, "Cancelled."))
}

func (s *Server) adminAccountONUConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if s.acsOff(w, r) {
		return
	}
	err := s.acs.ConfirmWiFi(s.actorOf(r), realIP(r), id)
	s.back(w, r, s.accountURL(id), msgOr(err, "Thank you. The Wi-Fi change is marked as working for this model of ONU."))
}

func (s *Server) adminAccountONUReject(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if s.acsOff(w, r) {
		return
	}
	_, err := s.acs.RejectWiFi(s.actorOf(r), realIP(r), id)
	s.back(w, r, s.accountURL(id), msgOr(err, "Noted. The change is sent again another way; check the Wi-Fi when it is accepted."))
}

// adminONUAdminPW shows the random web-admin password the ACS set on a box. Only an owner, and only with the
// owner's password again. Tenants must never get it: the box's admin page can change what the ACS set.
func (s *Server) adminONUAdminPW(w http.ResponseWriter, r *http.Request) {
	if s.acsOff(w, r) {
		return
	}
	to := s.onuBack(r, "/admin/onus")
	if u, _ := userOf(r); u.Role != store.RoleOwner {
		s.back(w, r, to, "Error: only an owner can see this password")
		return
	}
	if !s.reauth(w, r, to) {
		return
	}
	pw, err := s.acs.AdminPassword(r.PostFormValue("onu"))
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.audit(r, "ONU web-admin password shown")
	w.Header().Set("Cache-Control", "no-store")
	s.notice(w, r, http.StatusOK, "ONU web-admin password", "Password "+pw+" (set for the box's web-admin accounts; the user names are the ones the box already had). It is not verified on the box. Never give it to the tenant.", to, "Back")
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
		if b, ok := s.acs.Brief(v.ID); ok {
			data["ONUBrief"] = b
		}
		if t, ok := s.acs.TreeInfo(v.ID); ok {
			data["ONUTree"] = t
		}
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

// adminAccountWiFiShow shows the house's current Wi-Fi password: the one the ONU returns when it can be asked, else
// the saved one. It writes an audit entry (in the ACS) and the page is never cached. Only the customer's Wi-Fi key
// is shown here; system logins keep their step-up.
func (s *Server) adminAccountWiFiShow(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	to := s.accountURL(id)
	if s.acsOff(w, r) {
		return
	}
	res, err := s.acs.RevealWiFi(r.Context(), s.actorOf(r), realIP(r), id)
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	var parts []string
	for _, k := range res.Networks {
		name := "Wi-Fi"
		switch k.Band {
		case "2.4":
			name = "2.4 GHz"
		case "5":
			name = "5 GHz"
		}
		if k.Password == "" {
			parts = append(parts, name+" \""+k.SSID+"\": no password known.")
			continue
		}
		src := "saved here"
		if k.Source == "onu" {
			src = "read from the ONU"
		}
		parts = append(parts, name+" \""+k.SSID+"\": password "+k.Password+" ("+src+").")
	}
	msg := strings.Join(parts, " ")
	if res.Note != "" {
		msg += " " + res.Note
	}
	w.Header().Set("Cache-Control", "no-store")
	s.notice(w, r, http.StatusOK, "Wi-Fi password", msg, to, "Back")
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
// onuStockItem is an in-stock ONU with the apartments its PON port serves (for the Add customer dialog to preselect).
type onuStockItem struct {
	store.Equipment
	ApartmentIDs []string
}

func (s *Server) onuStock() []onuStockItem {
	var out []onuStockItem
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindONT && e.Status == store.EqInStock && e.MAC != "" {
				out = append(out, onuStockItem{e, billing.ONUApartmentIDs(d, e)})
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
	if s.acs != nil && !s.acs.CanUseMAC(acct, r.PostFormValue("mac")) {
		s.back(w, r, to, "Error: that MAC was not reported by a verified session of this house's ONU")
		return
	}
	err := s.svc.UseReportedMAC(r.Context(), acct, r.PostFormValue("mac"))
	if err == nil && s.acs != nil {
		if a, ok := s.svc.GetAccount(acct); ok {
			s.acs.AutoLinkSite(a.SiteID)
		}
	}
	s.back(w, r, to, msgOr(err, "The house now uses the MAC the ONU reported."))
}
