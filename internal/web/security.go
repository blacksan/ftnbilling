package web

import (
	"errors"
	"net/http"
	"strings"

	"ispbill/internal/authn"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

// The security page: your password, authenticator, sessions and phones; for
// owners also every user, every phone and the audit log.

func (s *Server) securityRoutes(get, post func(string, http.HandlerFunc)) {
	get("/admin/security", s.securityPage)
	post("/admin/security/password", s.securityPassword)
	post("/admin/security/recovery", s.securityRecovery)
	post("/admin/security/sessions/revoke", s.securitySessionRevoke)
	post("/admin/security/devices/revoke", s.securityDeviceRevoke)
	post("/admin/security/users/create", s.securityUserCreate)
	post("/admin/security/users/disable", s.securityUserDisable)
	post("/admin/security/users/enable", s.securityUserEnable)
	post("/admin/security/users/role", s.securityUserRole)
	post("/admin/security/users/reset2fa", s.securityUserReset2FA)
}

type secSession struct {
	store.AdminSession
	Current bool
}

type secDevice struct {
	store.APIDevice
	Owner string
}

type secUser struct {
	store.AdminUser
	Enrolled bool
	Codes    int
	Self     bool
}

type revealBox struct {
	Title string
	Lines []string
	Note  string
}

func (s *Server) securityData(r *http.Request, p *adminPage, reveal *revealBox) {
	u, _ := userOf(r)
	sess, _ := sessOf(r)
	var sessions []secSession
	for _, x := range s.authn.UserSessions(u.ID) {
		sessions = append(sessions, secSession{x, x.ID == sess.ID})
	}
	names := map[string]string{}
	for _, x := range s.authn.Users() {
		names[x.ID] = x.Username
	}
	var devices []secDevice
	owner := u.Role == store.RoleOwner
	for _, x := range s.authn.Devices("") {
		if x.UserID == u.ID || owner {
			devices = append(devices, secDevice{x, names[x.UserID]})
		}
	}
	p.Data["Me"], p.Data["Enrolled"], p.Data["CodesLeft"] = u, authn.Enrolled(u), len(u.RecoveryCodes)
	p.Data["Sessions"], p.Data["Devices"], p.Data["IsOwner"] = sessions, devices, owner
	p.Data["Reveal"] = reveal
	p.Data["Roles"] = []string{store.RoleOwner, store.RoleOperator, store.RoleViewer}
	if owner {
		var users []secUser
		for _, x := range s.authn.Users() {
			users = append(users, secUser{x, authn.Enrolled(x), len(x.RecoveryCodes), x.ID == u.ID})
		}
		p.Data["Users"] = users
		evs, _ := s.st.Events(r.Context(), store.EventQuery{Kind: "audit", Limit: 100})
		p.Data["Audit"] = evs
	}
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "security")
	s.securityData(r, &p, nil)
	s.render(w, "admin_security", p)
}

// securityShow answers a POST that has something to show once (recovery codes,
// a setup code): the page itself, never a redirect that would put it in a URL.
func (s *Server) securityShow(w http.ResponseWriter, r *http.Request, flash string, reveal *revealBox) {
	p := s.page(r, "security")
	p.Flash, p.FlashBad = flash, strings.HasPrefix(flash, "Error")
	s.securityData(r, &p, reveal)
	s.render(w, "admin_security", p)
}

func (s *Server) securityBack(w http.ResponseWriter, r *http.Request, msg string) {
	s.back(w, r, "/admin/security", msg)
}

func (s *Server) securityPassword(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	sess, _ := sessOf(r)
	if msg := s.confirm(r, true); msg != "" {
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	pw, again := r.PostFormValue("new_password"), r.PostFormValue("confirm_password")
	if pw != again {
		s.securityBack(w, r, "Error: the two new passwords differ")
		return
	}
	if err := settings.CheckNewPassword(pw, u.Username); err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	h, err := settings.HashPassword(pw)
	if err == nil {
		err = s.authn.SetPassword(u.ID, h, u.Username, realIP(r), sess.ID)
	}
	if err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	s.securityBack(w, r, "Password changed. Every other browser and phone was signed out.")
}

// securityRecovery issues a new set of recovery codes; it needs the password
// and a current authenticator code, as a stolen session alone must not be
// enough to read the codes.
func (s *Server) securityRecovery(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	if msg := s.confirm(r, true); msg != "" {
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	codes, err := s.authn.RegenerateRecoveryCodes(u.ID, u.Username, realIP(r))
	if err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	s.securityShow(w, r, "New recovery codes made. The old ones no longer work.", &revealBox{
		Title: "Recovery codes", Lines: codes, Note: "Each code works once. Keep them somewhere safe and offline. They are not shown again."})
}

func (s *Server) securitySessionRevoke(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	sess, _ := sessOf(r)
	if r.PostFormValue("all") == "1" {
		n, err := s.authn.RevokeSessions(u.ID, sess.ID, "signed out everywhere else", u.Username, realIP(r))
		if err != nil {
			s.securityBack(w, r, "Error: "+err.Error())
			return
		}
		s.securityBack(w, r, "Signed out everywhere else ("+itoa(n)+" sign-ins ended).")
		return
	}
	id := r.PostFormValue("id")
	for _, x := range s.authn.UserSessions(u.ID) { // only your own
		if x.ID == id && id != sess.ID {
			s.authn.DeleteSession(id)
			s.authn.AuditNow(u.Username, realIP(r), "browser session ended")
			break
		}
	}
	s.securityBack(w, r, "Session ended.")
}

func (s *Server) securityDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	u, _ := userOf(r)
	dev, ok := s.authn.Device(r.PostFormValue("id"))
	if !ok || (dev.UserID != u.ID && u.Role != store.RoleOwner) {
		s.securityBack(w, r, "Error: no such phone")
		return
	}
	if err := s.authn.RevokeDevice(dev.ID, "revoked in the admin pages", u.Username, realIP(r)); err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	s.securityBack(w, r, "Phone signed out.")
}

// ---- users (owner) ---------------------------------------------------------------------------------

func (s *Server) userByID(id string) (store.AdminUser, bool) { return s.authn.UserByID(id) }

func (s *Server) securityUserCreate(w http.ResponseWriter, r *http.Request) {
	me, _ := userOf(r)
	if msg := s.confirm(r, true); msg != "" {
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	name, role, pw := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("role"), r.PostFormValue("password")
	if err := settings.CheckNewPassword(pw, name); err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	h, err := settings.HashPassword(pw)
	if err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	nu, err := s.authn.CreateUser(name, role, h, me.Username, realIP(r))
	if err != nil {
		msg := err.Error()
		if errors.Is(err, authn.ErrUsernameTaken) {
			msg = "that user name is already taken"
		}
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	code, err := s.authn.IssueSetupCode(nu.ID, me.Username, realIP(r))
	if err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	s.securityShow(w, r, "User "+nu.Username+" created as "+nu.Role+".", &revealBox{
		Title: "Setup code for " + nu.Username, Lines: []string{code},
		Note: "Give this code and the password to " + nu.Username + ". It is needed once, at the first sign-in, to set up the authenticator. It expires in 14 days and is not shown again."})
}

func (s *Server) securityUserDisable(w http.ResponseWriter, r *http.Request) {
	s.setDisabled(w, r, true)
}
func (s *Server) securityUserEnable(w http.ResponseWriter, r *http.Request) {
	s.setDisabled(w, r, false)
}

func (s *Server) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	me, _ := userOf(r)
	if msg := s.confirm(r, true); msg != "" {
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	t, ok := s.userByID(r.PostFormValue("id"))
	if !ok {
		s.securityBack(w, r, "Error: no such user")
		return
	}
	if err := s.authn.SetDisabled(t.ID, disabled, me.Username, realIP(r)); err != nil {
		s.securityBack(w, r, "Error: "+userMsg(err))
		return
	}
	s.securityBack(w, r, "Done.")
}

func (s *Server) securityUserRole(w http.ResponseWriter, r *http.Request) {
	me, _ := userOf(r)
	if msg := s.confirm(r, true); msg != "" {
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	t, ok := s.userByID(r.PostFormValue("id"))
	if !ok {
		s.securityBack(w, r, "Error: no such user")
		return
	}
	if err := s.authn.SetRole(t.ID, r.PostFormValue("role"), me.Username, realIP(r)); err != nil {
		s.securityBack(w, r, "Error: "+userMsg(err))
		return
	}
	s.securityBack(w, r, "Role changed. That user was signed out everywhere.")
}

func (s *Server) securityUserReset2FA(w http.ResponseWriter, r *http.Request) {
	me, _ := userOf(r)
	sess, _ := sessOf(r)
	if msg := s.confirm(r, true); msg != "" {
		s.securityBack(w, r, "Error: "+msg)
		return
	}
	t, ok := s.userByID(r.PostFormValue("id"))
	if !ok {
		s.securityBack(w, r, "Error: no such user")
		return
	}
	keep := ""
	if t.ID == me.ID {
		s.securityBack(w, r, "Error: reset your own two-factor sign-in on the server with ispbill admin-reset-password -reset-totp")
		return
	}
	if err := s.authn.ResetTOTP(t.ID, me.Username, realIP(r), keep); err != nil {
		s.securityBack(w, r, "Error: "+userMsg(err))
		return
	}
	_ = sess
	code, err := s.authn.IssueSetupCode(t.ID, me.Username, realIP(r))
	if err != nil {
		s.securityBack(w, r, "Error: "+err.Error())
		return
	}
	s.securityShow(w, r, "Two-factor sign-in of "+t.Username+" was reset and they were signed out.", &revealBox{
		Title: "New setup code for " + t.Username, Lines: []string{code},
		Note: t.Username + " signs in with the password and this code, then sets up the authenticator again. It expires in 14 days and is not shown again."})
}

func userMsg(err error) string {
	switch {
	case errors.Is(err, authn.ErrLastOwner):
		return "there must be at least one active owner"
	case errors.Is(err, authn.ErrBadRole):
		return "choose owner, operator or viewer"
	case errors.Is(err, authn.ErrNoUser):
		return "no such user"
	}
	return err.Error()
}
