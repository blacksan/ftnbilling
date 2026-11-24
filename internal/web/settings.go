package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/notify"
	"ispbill/internal/routeros"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

func (s *Server) settingsRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/settings", s.adminSettings)
	post("/admin/settings/revert", s.settingsRevert)

	get("/admin/settings/whatsapp", s.settingsWhatsApp)
	post("/admin/settings/whatsapp", s.settingsSaveWhatsApp)
	post("/admin/settings/whatsapp/test", s.settingsWhatsAppTest)

	get("/admin/settings/mpesa", s.settingsMpesa)
	post("/admin/settings/mpesa", s.settingsSaveMpesa)
	post("/admin/settings/mpesa/oauth-test", s.settingsMpesaOAuth)
	post("/admin/settings/mpesa/register-c2b", s.settingsMpesaRegister)
	post("/admin/settings/mpesa/stk-test", s.settingsMpesaSTK)
	post("/admin/settings/mpesa/receipt-test", s.settingsMpesaReceiptTest)
	get("/admin/settings/mpesa/stk-test", s.settingsMpesaSTKPoll)
	post("/admin/settings/mpesa/rotate-token", s.settingsMpesaRotate)
	post("/admin/settings/mpesa/urls", s.settingsMpesaReveal)
	// The reveal page is a POST result; its address must not be a dead end when
	// opened again, or when a form on it redirects "back" to it.
	get("/admin/settings/mpesa/urls", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/settings/mpesa", http.StatusSeeOther)
	})

	get("/admin/settings/email", s.settingsEmail)
	post("/admin/settings/email", s.settingsSaveEmail)
	post("/admin/settings/email/test", s.settingsEmailTest)

	get("/admin/settings/support", s.settingsSupport)
	post("/admin/settings/support", s.settingsSaveSupport)

	get("/admin/settings/maps", s.settingsMaps)
	post("/admin/settings/maps", s.settingsSaveMaps)

	get("/admin/settings/equipment", s.settingsEquipment)
	get("/admin/settings/equipment/find", s.settingsEquipmentFind)

	s.toolsRoutes(get, post)
}

// ---- General --------------------------------------------------------------------------

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "settings")
	live := s.live()
	pol := s.svc.Policy()
	p.Data = map[string]any{
		"Policy": pol, "Cfg": s.cfg, "Mpesa": live.MpesaEnabled(),
		"Support":    s.cfg.SupportPhone,
		"WGEndpoint": s.cfg.WGEndpoint, "WGSubnet": s.cfg.WGSubnet, "JoinTTL": s.cfg.JoinTokenTTL.String(),
		"Stray": s.mgr.Stray(), "Backup": s.backupStatus(), "DBKind": s.st.Kind(), "DBErr": errString(s.st.Healthy()),
		"HasKey": len(s.cfg.DataKey) > 0,
		"Listen": s.cfg.ListenAddr, "PublicURL": s.cfg.PublicBaseURL, "AdminUser": p.User,
		"Problems": s.problems(), "Tabs": s.settingsTabs("general"),
		"WAOn": live.WhatsAppConfigured(), "EmailOn": live.EmailConfigured(),
	}
	s.render(w, "admin_settings", p)
}

func (s *Server) problems() []string {
	if s.setm == nil {
		return nil
	}
	return s.setm.Problems()
}

type settingsTab struct{ Key, Label, URL string }

func (s *Server) settingsTabs(active string) []settingsTab {
	return []settingsTab{
		{"general", "General and password", "/admin/settings"},
		{"whatsapp", "WhatsApp", "/admin/settings/whatsapp"},
		{"mpesa", "M-Pesa", "/admin/settings/mpesa"},
		{"email", "Email", "/admin/settings/email"},
		{"support", "Support contacts", "/admin/settings/support"},
		{"maps", "Maps", "/admin/settings/maps"},
		{"equipment", "Equipment", "/admin/settings/equipment"},
		{"onu", "ONU management", "/admin/settings/onu"},
		{"tools", "Tools", "/admin/tools"},
	}
}

// sectionPage starts a settings page with the shared parts every section has.
func (s *Server) sectionPage(r *http.Request, active, section string) adminPage {
	p := s.page(r, active)
	p.Data["Tabs"] = s.settingsTabs(strings.TrimPrefix(active, "set-"))
	p.Data["ActiveTab"] = strings.TrimPrefix(active, "set-")
	p.Data["Problems"] = s.problems()
	p.Data["HasKey"] = s.setm != nil && s.setm.HasDataKey()
	p.Data["NoManager"] = s.setm == nil
	p.Data["Next"] = r.URL.Path
	for _, t := range s.settingsTabs("") { // forms return to the tab, never to a POST-only address
		if t.Key == strings.TrimPrefix(active, "set-") {
			p.Data["Next"] = t.URL
		}
	}
	if s.setm != nil && section != "" {
		p.Data["Fields"] = s.setm.Fields(section)
	}
	return p
}

// ---- WhatsApp ---------------------------------------------------------------------------

// waTemplates are the templates the test button can send, with sample values.
var waTemplates = []struct {
	Name   string
	Params []string
}{
	{"ftn_admin_alert", []string{"TEST", "Test alert", "Test router", "this is a test from Settings", "now"}},
	{"ftn_expiry_reminder", []string{"1 Month", "House 1, Test router", "Fri 1 Jan, 10:00", "1500", "M-Pesa Paybill 000000", "ABC234"}},
	{"ftn_plan_expired", []string{"House 1, Test router", "Fri 1 Jan, 10:00", "M-Pesa Paybill 000000", "ABC234"}},
	{"ftn_payment_received", []string{"1500", "TEST123456", "1 Month", "House 1, Test router", "Fri 1 Jan, 10:00"}},
	{"ftn_outage_notice", []string{"Test router", "Fri 1 Jan, 10:00"}},
	{"ftn_outage_resolved", []string{"Test router", "Fri 1 Jan, 11:00"}},
}

func (s *Server) settingsWhatsApp(w http.ResponseWriter, r *http.Request) {
	p := s.sectionPage(r, "set-whatsapp", settings.SecWhatsApp)
	live := s.live()
	base := live.PublicBaseURL
	if base == "" {
		base = "https://YOUR-PUBLIC-URL"
	}
	var names []string
	for _, t := range waTemplates {
		names = append(names, t.Name)
	}
	p.Data["WebhookURL"] = base + "/wa/webhook"
	p.Data["Templates"], p.Data["Numbers"] = names, live.WAAdminNumbers
	p.Data["Configured"] = live.WhatsAppConfigured()
	p.Data["WebhookOn"] = live.WAAppSecret != "" && live.WAVerifyToken != ""
	if s.setm != nil {
		if t, err := time.Parse(time.RFC3339, s.setm.Marker(settings.KeyWebhookVerified)); err == nil {
			p.Data["VerifiedAt"] = t
		}
	}
	if s.ops.Inbound != nil {
		if t := s.ops.Inbound.LastSignedEvent(); !t.IsZero() {
			p.Data["LastEvent"] = t
		}
	}
	if s.ops.Alerts != nil {
		p.Data["Notice"] = s.ops.Alerts.Settings()
		p.Data["HasAlerts"] = true
	}
	if s.ops.Box != nil {
		p.Data["WAToday"], p.Data["WALimit"] = s.ops.Box.WASentToday(), live.WADailyLimit
		if pz, ok := s.ops.Box.Paused()[notify.ChanWhatsApp]; ok {
			p.Data["Paused"] = pz.Reason
		}
	}
	s.render(w, "admin_settings_wa", p)
}

// ---- M-Pesa -------------------------------------------------------------------------------

func (s *Server) settingsMpesa(w http.ResponseWriter, r *http.Request) {
	p := s.sectionPage(r, "set-mpesa", settings.SecMpesa)
	live := s.live()
	base := live.PublicBaseURL
	if base == "" {
		base = "https://YOUR-PUBLIC-URL"
	}
	reveal, _ := r.Context().Value(revealKey{}).(bool)
	tok := "<token>"
	if reveal && live.MpesaCallbackToken != "" {
		tok = url.PathEscape(live.MpesaCallbackToken)
	}
	p.Data["URLStk"] = base + "/pay/cb/t/" + tok + "/stk"
	p.Data["URLVal"] = base + "/pay/cb/t/" + tok + "/c2b/validate"
	p.Data["URLConf"] = base + "/pay/cb/t/" + tok + "/c2b/confirm"
	p.Data["Revealed"] = reveal
	p.Data["Enabled"] = live.MpesaEnabled()
	p.Data["HTTPS"] = strings.HasPrefix(live.PublicBaseURL, "https://")
	p.Data["Production"] = live.MpesaEnv == "production"
	p.Data["HasToken"] = live.MpesaCallbackToken != ""
	if s.setm != nil {
		if until, ok := s.setm.PreviousCallbackToken(); ok {
			p.Data["PrevUntil"] = until
		}
	}
	p.Data["PublicURL"] = live.PublicBaseURL
	if id := r.URL.Query().Get("test"); id != "" && s.mpesa != nil {
		if t, ok := s.mpesa.TestStatus(id); ok {
			p.Data["Test"] = t
		}
	}
	if on, why := live.MpesaConfirm(); on {
		p.Data["ConfirmOn"] = true
	} else {
		p.Data["ConfirmWhy"] = why
	}
	p.Data["HasInitiator"] = live.MpesaInitiator != "" && live.MpesaSecurityCredential != ""
	if rc := r.URL.Query().Get("rtest"); rc != "" && s.mpesa != nil {
		if t, ok := s.mpesa.ReceiptTestStatus(rc); ok {
			p.Data["RTest"] = t
			if t.Status == "waiting" && time.Since(t.Started) < 2*time.Minute {
				p.Refresh = 4 // the answer comes by itself a few seconds later
			}
		}
	}
	s.render(w, "admin_settings_mpesa", p)
}

type revealKey struct{}

// ---- Support contacts ---------------------------------------------------------------------

func (s *Server) settingsSupport(w http.ResponseWriter, r *http.Request) {
	p := s.sectionPage(r, "set-support", settings.SecSupport)
	live := s.live()
	p.Data["Phone"], p.Data["Email"], p.Data["WhatsApp"] = live.SupportPhone, live.SupportEmail, live.SupportWhatsApp
	s.render(w, "admin_settings_support", p)
}

// ---- Maps (the Network page's Google Maps key) -----------------------------------------------

func (s *Server) settingsMaps(w http.ResponseWriter, r *http.Request) {
	p := s.sectionPage(r, "set-maps", settings.SecMaps)
	p.Data["KeySet"] = s.live().GoogleMapsAPIKey != ""
	s.render(w, "admin_settings_maps", p)
}

// ---- Email -----------------------------------------------------------------------------------

func (s *Server) settingsEmail(w http.ResponseWriter, r *http.Request) {
	p := s.sectionPage(r, "set-email", settings.SecEmail)
	live := s.live()
	p.Data["Configured"] = live.EmailConfigured()
	p.Data["Recipients"] = live.AlertEmailTo
	s.render(w, "admin_settings_email", p)
}

// ---- Equipment ------------------------------------------------------------------------------

type ontRow struct {
	E    store.Equipment
	Site string
}

// found is one place a MAC address was seen.
type found struct {
	Site, IP, Host, State, Source string
}

func (s *Server) settingsEquipment(w http.ResponseWriter, r *http.Request) {
	s.renderEquipment(w, r, "", nil, "")
}

func (s *Server) renderEquipment(w http.ResponseWriter, r *http.Request, mac string, res []found, note string) {
	p := s.sectionPage(r, "set-equipment", "")
	var rows []ontRow
	sc := s.scopeOf(r)
	s.st.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindONT && e.MAC != "" && e.AccountID == "" && (e.SiteID == "" || sc.Has(e.SiteID)) {
				rows = append(rows, ontRow{e, d.Sites[e.SiteID].Name})
			}
		}
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].E.MAC < rows[j].E.MAC })
	p.Data["Rows"], p.Data["MAC"], p.Data["Found"], p.Data["FindNote"] = rows, mac, res, note
	p.Data["Searched"] = mac != ""
	s.render(w, "admin_settings_equipment", p)
}

// settingsEquipmentFind asks every connected building's router where a MAC
// address is: the DHCP leases. Each building gets 5 s.
func (s *Server) settingsEquipmentFind(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("mac"))
	if raw == "" {
		s.renderEquipment(w, r, "", nil, "")
		return
	}
	// Each search asks every building's router, so searches are rate limited.
	if !s.findRL.allow(realIP(r)) {
		s.back(w, r, "/admin/settings/equipment", "Error: too many searches in a short time; wait a minute and try again")
		return
	}
	mac, err := billing.NormalizeMAC(raw)
	if err != nil {
		s.back(w, r, "/admin/settings/equipment", "Error: "+err.Error())
		return
	}
	var list []store.Site
	s.st.View(func(d *store.Data) {
		for _, st := range d.SiteList() {
			if st.Runnable() {
				list = append(list, st)
			}
		}
	})
	var mu sync.Mutex
	var res []found
	var wg sync.WaitGroup
	for _, st := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			rt := s.mgr.Router(st.ID)
			rows := map[string]*found{}
			var errs []string
			if ex, ok := rt.(routeros.Executor); ok {
				if ls, err := ex.Leases(ctx, mac); err != nil {
					errs = append(errs, "DHCP leases: "+err.Error())
				} else {
					for _, l := range ls {
						if f, ok := rows[l.Address]; ok {
							f.Source += " + DHCP lease"
							if f.Host == "" {
								f.Host = l.HostName
							}
							continue
						}
						rows[l.Address] = &found{Site: st.Name, IP: l.Address, Host: l.HostName, State: l.Status, Source: "DHCP lease"}
					}
				}
			}
			mu.Lock()
			for _, f := range rows {
				res = append(res, *f)
			}
			if len(rows) == 0 && len(errs) > 0 {
				res = append(res, found{Site: st.Name, State: "could not ask: " + strings.Join(errs, "; ")})
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(res, func(i, j int) bool {
		if res[i].Site != res[j].Site {
			return res[i].Site < res[j].Site
		}
		return res[i].IP < res[j].IP
	})
	note := ""
	if len(list) == 0 {
		note = "No router is connected yet."
	}
	s.renderEquipment(w, r, mac, res, note)
}
