package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/store"
)

type apiDeviceReq struct {
	InstallID  string `json:"install_id"`
	Name       string `json:"name"`
	Model      string `json:"model"`
	Platform   string `json:"platform"`
	AppVersion string `json:"app_version"`
}

type apiLoginReq struct {
	Username     string       `json:"username"`
	Password     string       `json:"password"`
	TOTP         string       `json:"totp"`
	RecoveryCode string       `json:"recovery_code"`
	SetupCode    string       `json:"setup_code"`  // reserved: enrolment is done on the web page
	KnownToken   string       `json:"known_token"` // from an earlier login answer: this phone signed in fully before
	Device       apiDeviceReq `json:"device"`
}

type apiUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func userJSON(u store.AdminUser) apiUser { return apiUser{u.ID, u.Username, u.Role} }

func tokenJSON(t authn.Tokens, u store.AdminUser) map[string]any {
	return map[string]any{
		"access_token": t.Access, "access_expires_at": t.AccessExpires.UTC().Format(time.RFC3339),
		"refresh_token": t.Refresh, "refresh_expires_at": t.RefreshExpires.UTC().Format(time.RFC3339),
		"device_id": t.DeviceID, "user": userJSON(u),
	}
}

// apiLogin signs a phone in. The password and the second factor come in one
// request. A correct password without a code counts as a failed attempt (and so
// does a correct password for a user who has not enrolled), so the password
// cannot be tested separately from the authenticator at no cost. A phone that
// signed in fully before sends back the known_token it was given; with it,
// strangers' failed guesses at the account never refuse or slow this phone.
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if s.fromTunnel(r) {
		http.NotFound(w, r)
		return
	}
	var req apiLoginReq
	if !decode(w, r, &req, false) {
		return
	}
	name := strings.TrimSpace(req.Username)
	if name == "" || req.Password == "" || len(req.Password) > 300 || len(name) > 80 {
		fields := map[string]string{}
		if name == "" {
			fields["username"] = "is required"
		}
		if req.Password == "" {
			fields["password"] = "is required"
		}
		if len(fields) == 0 {
			apiErr(w, http.StatusUnauthorized, codeInvalidCreds, "Wrong user name or password.", nil)
			return
		}
		validationErr(w, fields)
		return
	}
	ip := realIP(r)
	u, exists := s.authn.UserByName(name)
	att, out := s.auth.begin(ip, name, s.knownValueID(req.KnownToken, u.ID))
	if !out.ok {
		if out.limited {
			s.auth.auditBlocked(ip, time.Now())
		}
		rateLimited(w, out.wait, false)
		return
	}
	pause(r, out.delay)
	good, busy := s.auth.password(u, exists, req.Password, out.known)
	if busy {
		att.end(!out.known, false)
		rateLimited(w, 2*time.Second, true)
		return
	}
	if !good {
		att.end(true, false)
		apiErr(w, http.StatusUnauthorized, codeInvalidCreds, "Wrong user name or password.", nil)
		return
	}
	if !authn.Enrolled(u) {
		// Counted as a failure: with no second factor to stop them, an
		// unenrolled account's password must not be guessable for free.
		att.end(true, false)
		apiErr(w, http.StatusForbidden, codeEnrol, "Set up two-factor sign-in first, on the admin web page, then sign in here.", nil)
		return
	}
	code, rec := authn.NormalizeCode(req.TOTP), strings.TrimSpace(req.RecoveryCode)
	if code == "" && rec == "" {
		att.end(true, false)
		apiErr(w, http.StatusUnauthorized, codeTOTPInvalid, "Enter the code from your authenticator app.", map[string]any{"totp_required": true})
		return
	}
	var err error
	if rec != "" {
		err = s.authn.UseRecoveryCode(u.ID, rec, ip)
	} else {
		err = s.authn.VerifyTOTPFrom(u.ID, code, ip)
	}
	if err != nil {
		att.end(errors.Is(err, authn.ErrBadCode), false)
		if errors.Is(err, authn.ErrTOTPBackoff) || errors.Is(err, authn.ErrTOTPLocked) {
			if errors.Is(err, authn.ErrTOTPLocked) {
				apiErr(w, http.StatusUnauthorized, codeTOTPInvalid, "Too many wrong codes. Only a recovery code signs in now.", map[string]any{"recovery_code_required": true})
				return
			}
			rateLimited(w, authn.TOTPBackoff(u.TOTPFails), false)
			return
		}
		if errors.Is(err, authn.ErrBadCode) {
			apiErr(w, http.StatusUnauthorized, codeTOTPInvalid, "That code is not right or was already used.", nil)
			return
		}
		if !s.storeErr(w, err) {
			s.log.Error("api login", "err", err)
			apiErr(w, http.StatusInternalServerError, codeInternal, "Something went wrong.", nil)
		}
		return
	}
	att.end(false, true)
	tok, _, err := s.authn.LoginDevice(u.ID, authn.DeviceInfo{InstallID: req.Device.InstallID, Name: req.Device.Name, Model: req.Device.Model,
		Platform: req.Device.Platform, AppVersion: req.Device.AppVersion}, ip)
	if err != nil {
		if !s.storeErr(w, err) {
			apiErr(w, http.StatusInternalServerError, codeInternal, "Something went wrong.", nil)
		}
		return
	}
	out2 := tokenJSON(tok, u)
	// The phone keeps this value and sends it back as known_token at its next
	// sign-in, so strangers' failed guesses never lock the owner's phone out.
	if id := s.knownValueID(req.KnownToken, u.ID); id != "" {
		s.authn.RenewKnown(req.KnownToken)
		out2["known_token"] = req.KnownToken
	} else if v, err := s.authn.IssueKnown(u.ID); err == nil {
		out2["known_token"] = v
	}
	writeJSON(w, http.StatusOK, out2)
}

func (s *Server) apiRefresh(w http.ResponseWriter, r *http.Request) {
	if s.fromTunnel(r) {
		http.NotFound(w, r)
		return
	}
	ip := realIP(r)
	if !s.refreshRL.allow(ip) {
		rateLimited(w, time.Minute, false)
		return
	}
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	tok, _, u, err := s.authn.Refresh(strings.TrimSpace(req.RefreshToken), ip)
	if err != nil {
		if errors.Is(err, authn.ErrTokenRevoked) {
			apiErr(w, http.StatusUnauthorized, codeTokenRevoked, "This sign-in is no longer valid. Sign in again.", nil)
		} else if !s.storeErr(w, err) {
			apiErr(w, http.StatusInternalServerError, codeInternal, "Something went wrong.", nil)
		}
		return
	}
	writeJSON(w, http.StatusOK, tokenJSON(tok, u))
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	if err := s.authn.RevokeDevice(c.dev.ID, "signed out in the app", c.user.Username, c.ip); err != nil && !s.storeErr(w, err) {
		apiErr(w, http.StatusInternalServerError, codeInternal, "Something went wrong.", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user":   userJSON(c.user),
		"device": map[string]any{"id": c.dev.ID, "name": c.dev.Name, "created_at": c.dev.CreatedAt.UTC().Format(time.RFC3339)},
	})
}

func tp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) apiDevices(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	items := []map[string]any{}
	for _, d := range s.authn.Devices(c.user.ID) {
		items = append(items, map[string]any{"id": d.ID, "name": d.Name, "model": d.Model, "app_version": d.AppVersion,
			"created_at": tp(d.CreatedAt), "last_seen_at": tp(d.LastSeenAt), "last_ip": d.LastIP, "current": d.ID == c.dev.ID, "revoked_at": tp(d.RevokedAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) apiDeviceDelete(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	d, ok := s.authn.Device(r.PathValue("id"))
	if !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such device.", nil)
		return
	}
	if d.UserID != c.user.ID && c.user.Role != store.RoleOwner {
		apiErr(w, http.StatusForbidden, codeForbidden, "You can only sign out your own devices.", nil)
		return
	}
	if err := s.authn.RevokeDevice(d.ID, "revoked in the app", c.user.Username, c.ip); err != nil && !s.storeErr(w, err) {
		apiErr(w, http.StatusInternalServerError, codeInternal, "Something went wrong.", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
