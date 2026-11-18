package web

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/notify"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

const actionTimeout = 25 * time.Second // the server's write timeout is 30 s

// noManager answers a settings POST when the server runs without a manager.
func (s *Server) noManager(w http.ResponseWriter, r *http.Request, to string) bool {
	if s.setm == nil {
		s.back(w, r, to, "Error: settings are not available in this build")
		return true
	}
	return false
}

// reauth checks the "current password" field of a form. It refuses (and sends
// the admin back with a message) when it is missing or wrong.
func (s *Server) reauth(w http.ResponseWriter, r *http.Request, to string) bool {
	if !s.checkPassword(r, r.PostFormValue("current_password")) {
		s.back(w, r, to, "Error: the current password is missing or wrong")
		return false
	}
	return true
}

// allow applies a rate limit to an action button.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, rl *limiter, to, what string) bool {
	if !rl.allow(realIP(r)) {
		s.back(w, r, to, "Error: too many "+what+" in a short time; wait a minute and try again")
		return false
	}
	return true
}

func nextOr(r *http.Request, def string) string {
	n := r.PostFormValue("next")
	if strings.HasPrefix(n, "/admin/") && !strings.ContainsAny(n, "\r\n") {
		return n
	}
	return def
}

// ---- password -------------------------------------------------------------------------------

func (s *Server) settingsPassword(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings"
	if s.noManager(w, r, to) {
		return
	}
	if !s.reauth(w, r, to) {
		return
	}
	pw, again := r.PostFormValue("new_password"), r.PostFormValue("confirm_password")
	if pw != again {
		s.back(w, r, to, "Error: the two new passwords differ")
		return
	}
	if err := settings.CheckNewPassword(pw, s.live().AdminUser); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	if err := s.setm.SetAdminPassword(pw, realIP(r)); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.auth.Flush()
	// The browser still holds the old password: send it through the logout
	// page, which asks for the new one.
	http.Redirect(w, r, "/admin/logout", http.StatusSeeOther)
}

// ---- generic save and revert ------------------------------------------------------------

// saveSection stores a section's form. A secret that is being set or replaced
// needs the current admin password.
func (s *Server) saveSection(w http.ResponseWriter, r *http.Request, section, to string) (changes []settings.Change, ok bool) {
	if s.noManager(w, r, to) {
		return nil, false
	}
	values := map[string]string{}
	needPass := false
	for _, f := range s.setm.Fields(section) {
		if f.Kind == settings.KBool {
			if r.PostFormValue(f.Key) == "1" {
				values[f.Key] = "true"
			} else {
				values[f.Key] = "false"
			}
			continue
		}
		if _, present := r.PostForm[f.Key]; !present {
			continue
		}
		v := r.PostFormValue(f.Key)
		values[f.Key] = v
		if f.Secret && strings.TrimSpace(v) != "" {
			needPass = true
		}
	}
	if needPass && !s.reauth(w, r, to) {
		return nil, false
	}
	ch, err := s.setm.Save(section, values, realIP(r))
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return nil, false
	}
	return ch, true
}

func savedMsg(ch []settings.Change) string {
	if len(ch) == 0 {
		return "Nothing changed"
	}
	names := make([]string, len(ch))
	for i, c := range ch {
		names[i] = c.Key
	}
	return "Saved: " + strings.Join(names, ", ") + ". It is in use now; no restart needed."
}

func (s *Server) settingsRevert(w http.ResponseWriter, r *http.Request) {
	to := nextOr(r, "/admin/settings")
	if s.noManager(w, r, to) {
		return
	}
	key := r.PostFormValue("key")
	d, ok := settings.Lookup(key)
	if !ok {
		s.back(w, r, to, "Error: unknown setting")
		return
	}
	if d.Secret && !s.reauth(w, r, to) {
		return
	}
	if err := s.setm.Revert(key, realIP(r)); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, to, d.Label+" now comes from the environment file (or its default)")
}

// ---- WhatsApp ----------------------------------------------------------------------------

func (s *Server) settingsSaveWhatsApp(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/whatsapp"
	ch, ok := s.saveSection(w, r, settings.SecWhatsApp, to)
	if !ok {
		return
	}
	msg := savedMsg(ch)
	// The outage notice mode lives in the alert engine's own settings.
	if s.ops.Alerts != nil && r.PostForm.Has("notice") {
		set := s.ops.Alerts.Settings()
		set.OutageNotice = r.PostFormValue("notice")
		if v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("auto_min"))); err == nil {
			set.AutoMin = v
		}
		if err := s.ops.Alerts.SetSettings(set); err != nil {
			s.back(w, r, to, "Error: "+err.Error())
			return
		}
		if len(ch) == 0 {
			msg = "Saved"
		}
	}
	s.back(w, r, to, msg)
}

func (s *Server) settingsWhatsAppTest(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/whatsapp"
	if !s.allow(w, r, s.testRL, to, "test messages") {
		return
	}
	live := s.live()
	if !live.WhatsAppConfigured() {
		s.back(w, r, to, "Error: save the access token and phone number ID first")
		return
	}
	tpl := r.PostFormValue("template")
	var params []string
	for _, t := range waTemplates {
		if t.Name == tpl {
			params = t.Params
		}
	}
	if params == nil {
		s.back(w, r, to, "Error: choose one of the ftn_ templates")
		return
	}
	num := strings.TrimSpace(r.PostFormValue("to"))
	valid := false
	for _, n := range live.WAAdminNumbers {
		valid = valid || n == num
	}
	if !valid {
		s.back(w, r, to, "Error: choose one of the admin numbers (add yours above and save first)")
		return
	}
	snd := &notify.WhatsApp{Token: live.WAToken, PhoneNumberID: live.WAPhoneNumberID, APIVersion: live.WAAPIVersion, Lang: live.WALang}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	id, err := snd.Send(ctx, notify.Message{To: num, Template: tpl, Params: params, Lang: live.WALang})
	if err != nil {
		s.back(w, r, to, "Error: Meta refused the test: "+scrubSecrets(err.Error(), live.WAToken, live.WAAppSecret, live.WAVerifyToken))
		return
	}
	if s.ops.Box != nil {
		s.ops.Box.Resume(notify.ChanWhatsApp)
	}
	s.back(w, r, to, "Test sent: Meta accepted message "+id+". If it does not arrive, the template may not be approved in this language yet.")
}

// scrubSecrets hides any of the secrets in a text that may be shown.
func scrubSecrets(text string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 {
			text = strings.ReplaceAll(text, sec, "[hidden]")
		}
	}
	return text
}

// ---- M-Pesa --------------------------------------------------------------------------------

func (s *Server) settingsSaveMpesa(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/mpesa"
	ch, ok := s.saveSection(w, r, settings.SecMpesa, to)
	if !ok {
		return
	}
	s.back(w, r, to, savedMsg(ch))
}

func (s *Server) settingsMpesaOAuth(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/mpesa"
	if !s.allow(w, r, s.testRL, to, "tests") {
		return
	}
	if s.mpesa == nil {
		s.back(w, r, to, "Error: M-Pesa is not available in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	ttl, err := s.mpesa.TestOAuth(ctx)
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, to, "Daraja accepted the key and secret: an access token was issued, valid for "+ago(ttl)+".")
}

func (s *Server) settingsMpesaRegister(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/mpesa"
	if !s.allow(w, r, s.testRL, to, "registrations") {
		return
	}
	if s.mpesa == nil {
		s.back(w, r, to, "Error: M-Pesa is not available in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	desc, err := s.mpesa.RegisterC2B(ctx)
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.audit(r, "settings (mpesa): C2B URLs registered with Daraja")
	s.back(w, r, to, "Daraja replied: "+desc+". Payments to the Paybill now reach this server.")
}

func (s *Server) settingsMpesaSTK(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/mpesa"
	if !s.allow(w, r, s.stkRL, to, "test payments") {
		return
	}
	if s.mpesa == nil {
		s.back(w, r, to, "Error: M-Pesa is not available in this build")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	t, err := s.mpesa.StartTest(ctx, r.PostFormValue("phone"))
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.audit(r, "settings (mpesa): KES 1 test payment sent")
	s.back(w, r, to+"?test="+url.QueryEscape(t.CheckoutID), "KES 1 prompt sent to "+t.Phone+". Enter your M-Pesa PIN on the phone.")
}

// settingsMpesaSTKPoll answers the test page's polling: the in-memory state
// (the callback updates it) and, while still pending, Daraja's own answer.
func (s *Server) settingsMpesaSTKPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if s.mpesa == nil {
		http.NotFound(w, r)
		return
	}
	t, ok := s.mpesa.TestStatus(id)
	if !ok {
		writeJSON(w, map[string]any{"status": "unknown", "detail": "this test is not known (the server restarted?)"})
		return
	}
	if t.Status == "pending" && time.Since(t.Started) > 5*time.Second {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if code, desc, err := s.mpesa.STKQuery(ctx, id); err == nil {
			t = s.mpesa.ApplyQuery(id, code, desc)
		}
	}
	if t.Status == "pending" && time.Since(t.Started) > 3*time.Minute {
		t.Status, t.Detail = "failed", "no answer after 3 minutes: the prompt expired or the phone was off"
	}
	writeJSON(w, map[string]any{"status": t.Status, "detail": t.Detail, "receipt": t.Receipt})
}

func (s *Server) settingsMpesaRotate(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/mpesa"
	if s.noManager(w, r, to) || !s.reauth(w, r, to) {
		return
	}
	if err := s.setm.RotateCallbackToken(realIP(r)); err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	s.back(w, r, to, "The callback token was replaced. Register the C2B URLs again, or Daraja keeps calling the old ones (which now answer 404).")
}

// settingsMpesaReveal shows the real callback URLs once, after the password.
func (s *Server) settingsMpesaReveal(w http.ResponseWriter, r *http.Request) {
	if !s.checkPassword(r, r.PostFormValue("current_password")) {
		s.back(w, r, "/admin/settings/mpesa", "Error: the current password is missing or wrong")
		return
	}
	s.settingsMpesa(w, r.WithContext(context.WithValue(r.Context(), revealKey{}, true)))
}

// audit writes an audit event for an action that changes no setting.
func (s *Server) audit(r *http.Request, msg string) {
	_ = s.st.Update(func(d *store.Data) error {
		d.AddEvent(store.Event{Time: s.svc.Now(), Kind: "audit", Msg: msg + " from " + realIP(r)})
		return nil
	})
}

// ---- Email ---------------------------------------------------------------------------------

func (s *Server) settingsSaveEmail(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/email"
	ch, ok := s.saveSection(w, r, settings.SecEmail, to)
	if !ok {
		return
	}
	s.back(w, r, to, savedMsg(ch))
}

func (s *Server) settingsEmailTest(w http.ResponseWriter, r *http.Request) {
	to := "/admin/settings/email"
	if !s.allow(w, r, s.testRL, to, "test emails") {
		return
	}
	live := s.live()
	if !live.EmailConfigured() {
		s.back(w, r, to, "Error: save the mail server and the From address first")
		return
	}
	rcpt := strings.TrimSpace(r.PostFormValue("to"))
	if rcpt == "" && len(live.AlertEmailTo) > 0 {
		rcpt = live.AlertEmailTo[0]
	}
	norm, err := emailAddr(rcpt)
	if err != nil {
		s.back(w, r, to, "Error: "+err.Error())
		return
	}
	snd := &notify.SMTP{Host: live.SMTPHost, Port: live.SMTPPort, User: live.SMTPUser, Pass: live.SMTPPass, From: live.SMTPFrom, Implicit: live.SMTPImplicit}
	ctx, cancel := context.WithTimeout(r.Context(), actionTimeout)
	defer cancel()
	_, err = snd.Send(ctx, notify.Message{ID: "test", To: norm, Kind: notify.KindTest, Subject: "[" + s.cfg.BrandName + "] test email",
		Text: "This is a test email from Settings > Email. If you can read it, alert emails will reach you."})
	if err != nil {
		s.back(w, r, to, "Error: the mail server refused the test: "+scrubSecrets(err.Error(), live.SMTPPass))
		return
	}
	if s.ops.Box != nil {
		s.ops.Box.Resume(notify.ChanEmail)
	}
	s.back(w, r, to, "Test email sent to "+norm+".")
}

// emailAddr checks one recipient.
func emailAddr(v string) (string, error) {
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address == "" {
		return "", errors.New("enter a valid email address")
	}
	return a.Address, nil
}

// ---- small helpers ------------------------------------------------------------------------------
