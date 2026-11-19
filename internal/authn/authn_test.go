package authn_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/settings"
	"ispbill/internal/store"
	"ispbill/internal/store/storetest"
)

var key = []byte("0123456789abcdef0123456789abcdef")

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newSvc(t *testing.T) (*authn.Service, *store.Store, *clock) {
	t.Helper()
	st, _ := storetest.Open(t, t.TempDir())
	s, err := authn.New(st, key, "Test ISP")
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	s.SetClock(c.now)
	return s, st, c
}

func seed(t *testing.T, s *authn.Service) store.AdminUser {
	t.Helper()
	if _, err := s.SeedOwner("admin", "hash"); err != nil {
		t.Fatal(err)
	}
	u, ok := s.UserByName("ADMIN")
	if !ok || u.Role != store.RoleOwner {
		t.Fatalf("owner not seeded: %+v", u)
	}
	return u
}

// enrolled returns a user with TOTP set up and the secret and recovery codes.
func enrolled(t *testing.T, s *authn.Service, c *clock) (store.AdminUser, []byte, []string) {
	t.Helper()
	u := seed(t, s)
	sec, err := s.BeginEnrol(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := s.ConfirmEnrol(u.ID, authn.TOTPCode(sec, authn.TOTPStep(c.now())), "admin", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	u, _ = s.UserByID(u.ID)
	return u, sec, codes
}

// RFC 6238 appendix B, SHA-1 secret "12345678901234567890", last six digits.
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471",
		1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := authn.TOTPCode(secret, authn.TOTPStep(time.Unix(unix, 0))); got != want {
			t.Errorf("t=%d: got %s want %s", unix, got, want)
		}
	}
}

func TestMatchTOTPWindowAndReplay(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111109, 0)
	step := authn.TOTPStep(now)
	for _, d := range []int64{-1, 0, 1} {
		code := authn.TOTPCode(secret, step+d)
		if got, ok := authn.MatchTOTP(secret, code, now, 0); !ok || got != step+d {
			t.Errorf("step offset %d: ok=%v got=%d", d, ok, got)
		}
	}
	for _, d := range []int64{-2, 2, 5} {
		if _, ok := authn.MatchTOTP(secret, authn.TOTPCode(secret, step+d), now, 0); ok {
			t.Errorf("step offset %d accepted", d)
		}
	}
	// A step at or below the last accepted one is a replay.
	code := authn.TOTPCode(secret, step)
	if _, ok := authn.MatchTOTP(secret, code, now, step); ok {
		t.Error("replay of the same step accepted")
	}
	if _, ok := authn.MatchTOTP(secret, authn.TOTPCode(secret, step-1), now, step); ok {
		t.Error("an older step after a newer one accepted")
	}
	if _, ok := authn.MatchTOTP(secret, code[:5], now, 0); ok {
		t.Error("short code accepted")
	}
	if _, ok := authn.MatchTOTP(secret, " "+code[:3]+" "+code[3:], now, 0); !ok {
		t.Error("spaces inside the code should be ignored")
	}
}

func TestOTPAuthURIAndSecretRoundTrip(t *testing.T) {
	sec := []byte("12345678901234567890")
	uri := authn.OTPAuthURI("Fast Tech Net", "admin", sec)
	if !strings.HasPrefix(uri, "otpauth://totp/Fast%20Tech%20Net:admin?") || !strings.Contains(uri, "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ") ||
		!strings.Contains(uri, "issuer=Fast+Tech+Net") && !strings.Contains(uri, "issuer=Fast%20Tech%20Net") {
		t.Fatalf("uri %s", uri)
	}
	back, err := authn.DecodeSecret("gezd gnbv-gy3t qojq gezd gnbv gy3t qojq")
	if err != nil || string(back) != string(sec) {
		t.Fatalf("decode: %v %q", err, back)
	}
}

func TestEnrolStoresSecretSealed(t *testing.T) {
	s, st, c := newSvc(t)
	u, sec, codes := enrolled(t, s, c)
	if !authn.Enrolled(u) || len(codes) != 10 {
		t.Fatalf("enrolled=%v codes=%d", authn.Enrolled(u), len(codes))
	}
	if u.TOTPSecretEnc == "" || strings.Contains(u.TOTPSecretEnc, authn.EncodeSecret(sec)) {
		t.Fatal("secret stored in clear")
	}
	st.View(func(d *store.Data) {
		for _, h := range d.AdminUsers[u.ID].RecoveryCodes {
			for _, code := range codes {
				if strings.Contains(h, strings.ReplaceAll(code, "-", "")) {
					t.Fatal("recovery code stored in clear")
				}
			}
		}
	})
	// Pending enrolment shows the same secret again; after it is done, no more.
	if _, err := s.BeginEnrol(u.ID); !errors.Is(err, authn.ErrAlreadyEnrolled) {
		t.Fatalf("second enrol: %v", err)
	}
}

func TestEnrolPendingSecretIsStableAndWrongCodeRefused(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	a, _ := s.BeginEnrol(u.ID)
	b, _ := s.BeginEnrol(u.ID)
	if string(a) != string(b) {
		t.Fatal("pending secret changed between page loads")
	}
	if _, err := s.ConfirmEnrol(u.ID, "000000", "a", ""); !errors.Is(err, authn.ErrBadCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if got, _ := s.UserByID(u.ID); authn.Enrolled(got) {
		t.Fatal("enrolled with a wrong code")
	}
	_ = c
}

func TestVerifyTOTPReplayRejected(t *testing.T) {
	s, _, c := newSvc(t)
	u, sec, _ := enrolled(t, s, c)
	// The enrolment code used the current step; the next step is fine once.
	c.add(30 * time.Second)
	code := authn.TOTPCode(sec, authn.TOTPStep(c.now()))
	if err := s.VerifyTOTP(u.ID, code); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyTOTP(u.ID, code); !errors.Is(err, authn.ErrBadCode) {
		t.Fatalf("replayed code: %v", err)
	}
	// The code that enrolled the user cannot be replayed either.
	if err := s.VerifyTOTP(u.ID, authn.TOTPCode(sec, authn.TOTPStep(c.now())-1)); !errors.Is(err, authn.ErrBadCode) {
		t.Fatalf("older step: %v", err)
	}
	if err := s.VerifyTOTP(u.ID, "12345"); !errors.Is(err, authn.ErrBadCode) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestRecoveryCodesSingleUse(t *testing.T) {
	s, _, c := newSvc(t)
	u, _, codes := enrolled(t, s, c)
	if err := s.UseRecoveryCode(u.ID, strings.ToUpper(codes[3]), "9.9.9.9"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := s.UseRecoveryCode(u.ID, codes[3], ""); !errors.Is(err, authn.ErrBadCode) {
		t.Fatalf("second use: %v", err)
	}
	if err := s.UseRecoveryCode(u.ID, strings.ReplaceAll(codes[4], "-", ""), ""); err != nil {
		t.Fatalf("dashes optional: %v", err)
	}
	if got, _ := s.UserByID(u.ID); len(got.RecoveryCodes) != 8 {
		t.Fatalf("codes left %d", len(got.RecoveryCodes))
	}
	fresh, err := s.RegenerateRecoveryCodes(u.ID, "admin", "")
	if err != nil || len(fresh) != 10 {
		t.Fatal(err)
	}
	if err := s.UseRecoveryCode(u.ID, codes[0], ""); !errors.Is(err, authn.ErrBadCode) {
		t.Fatal("an old code works after regeneration")
	}
	if err := s.UseRecoveryCode(u.ID, fresh[0], ""); err != nil {
		t.Fatal(err)
	}
}

func TestSetupCode(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	code, err := s.IssueSetupCode(u.ID, "cli", "")
	if err != nil || len(code) != 19 {
		t.Fatalf("%q %v", code, err)
	}
	if !s.CheckSetupCode(u.ID, code) || !s.CheckSetupCode(u.ID, strings.ToUpper(strings.ReplaceAll(code, "-", ""))) {
		t.Fatal("right code refused")
	}
	if s.CheckSetupCode(u.ID, "aaaa-bbbb-cccc-dddd") || s.CheckSetupCode("", code) || s.CheckSetupCode("nobody", code) {
		t.Fatal("wrong code accepted")
	}
	// A newer code replaces the old one.
	code2, _ := s.IssueSetupCode(u.ID, "cli", "")
	if s.CheckSetupCode(u.ID, code) || !s.CheckSetupCode(u.ID, code2) {
		t.Fatal("replacement not honoured")
	}
	c.add(authn.SetupCodeLife + time.Minute)
	if s.CheckSetupCode(u.ID, code2) {
		t.Fatal("expired code accepted")
	}
	// Enrolling consumes it, and an enrolled user gets no new one.
	c.add(-authn.SetupCodeLife)
	sec, _ := s.BeginEnrol(u.ID)
	if _, err := s.ConfirmEnrol(u.ID, authn.TOTPCode(sec, authn.TOTPStep(c.now())), "a", ""); err != nil {
		t.Fatal(err)
	}
	if s.HasSetupCode(u.ID) || s.CheckSetupCode(u.ID, code2) {
		t.Fatal("setup code survived enrolment")
	}
	if _, err := s.IssueSetupCode(u.ID, "cli", ""); !errors.Is(err, authn.ErrAlreadyEnrolled) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionLifetimesRotationAndRevocation(t *testing.T) {
	s, st, c := newSvc(t)
	u := seed(t, s)
	val, sess, err := s.CreateSession(u.ID, "1.1.1.1", "UA", false)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == val || len(val) < 40 {
		t.Fatal("session id is not hashed or too short")
	}
	if got, _, ok := s.Session(val); !ok || got.MFA {
		t.Fatal("fresh session not found")
	}
	// A password-only session is short.
	c.add(authn.PreMFALife + time.Second)
	if _, _, ok := s.Session(val); ok {
		t.Fatal("pre-MFA session outlived its limit")
	}
	// Rotation: new id, old one dead, MFA on, absolute 12 h from the rotation.
	val, _, _ = s.CreateSession(u.ID, "1.1.1.1", "UA", false)
	nv, ns, err := s.Rotate(authn.HashToken(val), true)
	if err != nil || nv == val || !ns.MFA {
		t.Fatalf("rotate: %v %+v", err, ns)
	}
	if _, _, ok := s.Session(val); ok {
		t.Fatal("old session id still works")
	}
	if _, _, ok := s.Session(nv); !ok {
		t.Fatal("rotated session missing")
	}
	// Idle expiry: 30 minutes without a touch.
	c.add(authn.SessionIdle - time.Minute)
	sess, _, ok := s.Session(nv)
	if !ok {
		t.Fatal("session expired early")
	}
	s.Touch(sess)
	c.add(authn.SessionIdle - time.Minute)
	if _, _, ok := s.Session(nv); !ok {
		t.Fatal("touch did not extend the idle time")
	}
	c.add(2 * time.Minute)
	if _, _, ok := s.Session(nv); ok {
		t.Fatal("idle session accepted")
	}
	// Absolute expiry: keep touching for 12 hours.
	nv, _, _ = s.Rotate(ns.ID, true)
	_ = nv
	v2, _, _ := s.CreateSession(u.ID, "1.1.1.1", "UA", true)
	for i := 0; i < 24; i++ {
		c.add(29 * time.Minute)
		sess, _, ok := s.Session(v2)
		if !ok {
			t.Fatalf("session expired early, after touch %d", i)
		}
		s.Touch(sess)
	}
	c.add(time.Hour)
	if _, _, ok := s.Session(v2); ok {
		t.Fatal("session outlived the absolute limit")
	}
	// Disabled users lose their sessions.
	v3, _, _ := s.CreateSession(u.ID, "1.1.1.1", "UA", true)
	o2, err := s.CreateUser("second", store.RoleOwner, "h", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(u.ID, true, "second", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(v3); ok {
		t.Fatal("a disabled user's session works")
	}
	if err := s.SetDisabled(o2.ID, true, "x", ""); !errors.Is(err, authn.ErrLastOwner) {
		t.Fatalf("last owner: %v", err)
	}
	st.View(func(d *store.Data) {
		if len(d.AdminSessions) > 3 {
			t.Errorf("sessions not pruned: %d", len(d.AdminSessions))
		}
	})
}

func TestResetTOTPEndsSignIns(t *testing.T) {
	s, _, c := newSvc(t)
	u, _, _ := enrolled(t, s, c)
	val, _, _ := s.CreateSession(u.ID, "1.1.1.1", "UA", true)
	tok, dev, err := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i1", Name: "Pixel"}, "2.2.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResetTOTP(u.ID, "cli", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(val); ok {
		t.Fatal("session survived a 2FA reset")
	}
	if _, _, err := s.AuthAccess(tok.Access); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("access after reset: %v", err)
	}
	if d, _ := s.Device(dev.ID); d.RevokedAt.IsZero() {
		t.Fatal("device not revoked")
	}
	if got, _ := s.UserByID(u.ID); authn.Enrolled(got) || got.TOTPSecretEnc != "" || len(got.RecoveryCodes) != 0 {
		t.Fatalf("2FA state remains: %+v", got)
	}
}

func TestDeviceLoginAccessAndExpiry(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	tok, dev, err := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "inst", Name: "Pixel 8", Platform: "android"}, "5.5.5.5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Access, "ftna_") || !strings.HasPrefix(tok.Refresh, "ftnr_") || tok.DeviceID != dev.ID {
		t.Fatalf("token shape: %+v", tok)
	}
	if got, user, err := s.AuthAccess(tok.Access); err != nil || got.ID != dev.ID || user.ID != u.ID {
		t.Fatalf("auth: %v", err)
	}
	if _, _, err := s.AuthAccess(tok.Access + "x"); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("tampered token: %v", err)
	}
	if _, _, err := s.AuthAccess(tok.Refresh); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatal("a refresh token works as an access token")
	}
	c.add(authn.AccessLife + time.Second)
	if _, _, err := s.AuthAccess(tok.Access); !errors.Is(err, authn.ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	// The same install logging in again reuses its row.
	_, dev2, _ := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "inst", Name: "Pixel 8"}, "5.5.5.5")
	if dev2.ID != dev.ID || len(s.Devices(u.ID)) != 1 {
		t.Fatalf("device rows: %d", len(s.Devices(u.ID)))
	}
	if _, _, err := s.AuthAccess(tok.Access); !errors.Is(err, authn.ErrTokenRevoked) && !errors.Is(err, authn.ErrTokenExpired) {
		t.Fatalf("old access after re-login: %v", err)
	}
}

func TestRefreshRotationReuseAndGrace(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	t0, dev, _ := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i"}, "1.1.1.1")
	t1, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if t1.Refresh == t0.Refresh || t1.Access == t0.Access {
		t.Fatal("tokens were not rotated")
	}
	if _, _, err := s.AuthAccess(t0.Access); err == nil {
		t.Fatal("the access token of the previous pair still works")
	}
	// Within the grace period the old refresh token is a retry: it gets a pair
	// and the device stays alive.
	c.add(30 * time.Second)
	t2, _, _, err := s.Refresh(t0.Refresh, "1.1.1.1")
	if err != nil {
		t.Fatalf("retry within grace: %v", err)
	}
	if _, _, err := s.AuthAccess(t2.Access); err != nil {
		t.Fatal(err)
	}
	// The token issued by the first rotation was superseded by the retry.
	if _, _, _, err := s.Refresh(t1.Refresh, "1.1.1.1"); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("superseded token: %v", err)
	}
	if d, _ := s.Device(dev.ID); !d.RevokedAt.IsZero() {
		t.Fatal("an unknown-but-old token must not revoke the device (it could be a guess)")
	}
	// Normal use continues with the newest token; then the old one, after the
	// grace period, is reuse: the device is revoked.
	t3, _, _, err := s.Refresh(t2.Refresh, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	c.add(2 * time.Minute)
	if _, _, _, err := s.Refresh(t2.Refresh, "6.6.6.6"); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("reuse after grace: %v", err)
	}
	if d, _ := s.Device(dev.ID); d.RevokedAt.IsZero() || d.RevokedReason == "" {
		t.Fatal("device not revoked after reuse")
	}
	if _, _, err := s.AuthAccess(t3.Access); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("access token of a revoked device: %v", err)
	}
	if _, _, _, err := s.Refresh(t3.Refresh, ""); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("refresh of a revoked device: %v", err)
	}
}

func TestRefreshExpiryAndSlidingWindow(t *testing.T) {
	s, _, c := newSvc(t)
	u := seed(t, s)
	tok, _, _ := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i"}, "")
	// Used every 20 days, the refresh token keeps sliding up to 90 days.
	for i := 0; i < 4; i++ {
		c.add(20 * 24 * time.Hour)
		var err error
		if tok, _, _, err = s.Refresh(tok.Refresh, ""); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	// 80 days in; the absolute limit (90) cuts the next window short.
	if want := tok.RefreshExpires; want.After(c.now().Add(10*24*time.Hour + time.Minute)) {
		t.Fatalf("refresh expiry %v goes beyond the absolute limit", want)
	}
	c.add(11 * 24 * time.Hour)
	if _, _, _, err := s.Refresh(tok.Refresh, ""); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("past the absolute limit: %v", err)
	}
	// A refresh token unused for 30 days dies.
	tok, _, _ = s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "j"}, "")
	c.add(31 * 24 * time.Hour)
	if _, _, _, err := s.Refresh(tok.Refresh, ""); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("idle refresh: %v", err)
	}
}

func TestRevokeDeviceAndRoleChangeEndTokens(t *testing.T) {
	s, _, _ := newSvc(t)
	u := seed(t, s)
	tok, dev, _ := s.LoginDevice(u.ID, authn.DeviceInfo{InstallID: "i"}, "")
	if err := s.RevokeDevice(dev.ID, "lost phone", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthAccess(tok.Access); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("revoked device: %v", err)
	}
	op, _ := s.CreateUser("oper", store.RoleOperator, "h", "admin", "")
	t2, _, _ := s.LoginDevice(op.ID, authn.DeviceInfo{InstallID: "k"}, "")
	if err := s.SetRole(op.ID, store.RoleViewer, "admin", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthAccess(t2.Access); !errors.Is(err, authn.ErrTokenRevoked) {
		t.Fatalf("a role change must end the user's tokens: %v", err)
	}
	if err := s.SetRole(u.ID, store.RoleViewer, "admin", ""); !errors.Is(err, authn.ErrLastOwner) {
		t.Fatalf("demoting the last owner: %v", err)
	}
	if _, err := s.CreateUser("OPER", store.RoleViewer, "h", "a", ""); !errors.Is(err, authn.ErrUsernameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateUser("x", store.RoleViewer, "h", "a", ""); err == nil {
		t.Fatal("short user name accepted")
	}
}

func TestSeedOwnerOnlyWhenEmptyAndAuditHasNoSecrets(t *testing.T) {
	s, st, c := newSvc(t)
	h, _ := settings.HashPassword("a-long-enough-password")
	if ok, err := s.SeedOwner("admin", h); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := s.SeedOwner("other", h); ok {
		t.Fatal("seeded twice")
	}
	u, sec, codes := enrolled2(t, s, c)
	_ = u
	st.View(func(d *store.Data) {
		for _, e := range d.Events {
			for _, bad := range append([]string{authn.EncodeSecret(sec), "a-long-enough-password"}, codes...) {
				if strings.Contains(e.Msg, bad) {
					t.Fatalf("a secret reached the audit log: %q", e.Msg)
				}
			}
		}
	})
}

func enrolled2(t *testing.T, s *authn.Service, c *clock) (store.AdminUser, []byte, []string) {
	u, _ := s.Owner()
	sec, _ := s.BeginEnrol(u.ID)
	codes, err := s.ConfirmEnrol(u.ID, authn.TOTPCode(sec, authn.TOTPStep(c.now())), "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	return u, sec, codes
}

func TestNoDataKeyRefusesEnrolment(t *testing.T) {
	st, _ := storetest.Open(t, t.TempDir())
	s, _ := authn.New(st, nil, "")
	u, _ := s.SeedOwner("admin", "h")
	_ = u
	o, _ := s.Owner()
	if _, err := s.BeginEnrol(o.ID); !errors.Is(err, authn.ErrNoDataKey) {
		t.Fatalf("got %v", err)
	}
}
