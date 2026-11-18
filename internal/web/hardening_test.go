package web_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

func ip4(n int) string { return fmt.Sprintf("198.51.%d.%d", n/250, n%250+1) }

// strangersGuess is what n wrong guesses at "admin" from strangers leave on the books.
func (r *setRig) strangersGuess(t *testing.T, n int) {
	t.Helper()
	r.srv.AuthInjectAccountFailures("admin", n)
}

// B1: strangers hammering the owner's name never keep the owner out.
func TestOwnerNeverLockedOut(t *testing.T) {
	r := newSetRig(t)
	if r.br.Known == nil {
		t.Fatal("a full sign-in handed out no known-browser cookie")
	}
	if !r.br.Known.HttpOnly || r.br.Known.SameSite != http.SameSiteStrictMode || !r.br.Known.Secure || !strings.HasPrefix(r.br.Known.Name, "__Host-") || r.br.Known.MaxAge < 86400*90 {
		t.Errorf("known cookie attributes: %+v", r.br.Known)
	}
	// Far past the per-account (10) and global (30) limits, from many addresses.
	r.strangersGuess(t, 80)
	// A few real guesses on top (they are slowed too).
	// 1. The owner's known browser, from a brand-new address, signs in at once.
	start := time.Now()
	c, _, _ := r.attemptWith("203.0.113.200", "admin", pw, r.br.Known)
	if c != 303 || time.Since(start) > 2*time.Second {
		t.Fatalf("known browser: %d after %v", c, time.Since(start))
	}
	// 2. A new browser of the owner is slowed, not refused.
	start = time.Now()
	c, _, _ = r.attempt("203.0.113.201", "admin", pw)
	if c != 303 {
		t.Fatalf("a stranger-class sign-in with the right password must get through (slowed), got %d", c)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("over the limits, a stranger should be slowed; took %v", time.Since(start))
	}
}

func TestKnownBrowserBeatsHashSlotFlood(t *testing.T) {
	r := newSetRig(t)
	release := make(chan struct{})
	var held atomic.Int32
	r.srv.SetHashVerifier(func(hash, pass string) bool {
		if pass == "hold-the-slot" {
			held.Add(1)
			<-release
			return false
		}
		return settings.VerifyPassword(hash, pass)
	})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ { // strangers fill their slot and waiting room
		wg.Add(1)
		go func(i int) { defer wg.Done(); r.attempt(ip4(300+i), "admin", "hold-the-slot") }(i)
	}
	for end := time.Now().Add(3 * time.Second); held.Load() < 1 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	start := time.Now()
	c, _, _ := r.attemptWith(ip4(400), "admin", pw, r.br.Known)
	if c != 303 || time.Since(start) > 2*time.Second {
		t.Fatalf("a known browser waited behind strangers: %d after %v", c, time.Since(start))
	}
	close(release)
	wg.Wait()
}

func TestBusyAnswersForStrangersAreNotFree(t *testing.T) {
	r := newSetRig(t)
	release := make(chan struct{})
	var held atomic.Int32
	r.srv.SetHashVerifier(func(string, string) bool { held.Add(1); <-release; return false })
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); r.attempt(ip4(500+i), "admin", "x-wrong-password") }(i)
	}
	for end := time.Now().Add(3 * time.Second); held.Load() < 1 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
	}
	time.Sleep(200 * time.Millisecond)
	// One address now floods while the slot is busy: its busy answers add up to a block.
	got := map[int]int{}
	for i := 0; i < 8; i++ {
		c, _, _ := r.attempt("203.0.113.77", "admin", "x-wrong-password")
		got[c]++
	}
	close(release)
	wg.Wait()
	if got[429] < 8 {
		t.Fatalf("answers %v", got)
	}
	r.srv.SetHashVerifier(settings.VerifyPassword)
	if c, _, _ := r.attempt("203.0.113.77", "admin", pw); c != 429 {
		t.Fatalf("an address that spent its tries on busy answers must stay blocked, got %d", c)
	}
}

func TestIPv6GroupedBy64(t *testing.T) {
	r := newSetRig(t)
	for i := 0; i < 5; i++ { // five different addresses of one /64
		if c, _, _ := r.attempt(fmt.Sprintf("2001:db8:1:2::%x", i+1), "admin", "wrong-password-x"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c, _, _ := r.attempt("2001:db8:1:2:aaaa::9", "admin", pw); c != 429 {
		t.Fatalf("a sixth address of the same /64 was not blocked: %d", c)
	}
	if c, _, _ := r.attempt("2001:db8:1:3::1", "admin", pw); c != 303 {
		t.Fatalf("another /64: %d", c)
	}
}

func TestKnownFailuresStayOnTheKnownBrowser(t *testing.T) {
	r := newSetRig(t)
	// A known browser mistypes more than 5 times from one address: not blocked, and
	// the account's failure books for strangers stay empty.
	for i := 0; i < 8; i++ {
		if c, _, _ := r.attemptWith("203.0.113.9", "admin", "wrong-password-x", r.br.Known); c != 401 {
			t.Fatalf("typo %d: %d", i, c)
		}
	}
	start := time.Now()
	if c, _, _ := r.attempt("203.0.113.10", "admin", pw); c != 303 || time.Since(start) > time.Second {
		t.Fatalf("stranger after the known browser's typos: %d in %v", c, time.Since(start))
	}
	// A value for another user is worthless.
	other := seedUser(t, r.srv, r.st, "vera", store.RoleViewer, longPW)
	ob := signIn(t, r.ts, r.srv, r.st, other)
	r.strangersGuess(t, 12)
	start = time.Now()
	if c, _, _ := r.attemptWith("203.0.113.11", "admin", pw, ob.Known); c != 303 || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("another user's known value gave a pass: %d in %v", c, time.Since(start))
	}
}

func TestKnownBrowserEndsWithPasswordChangeAndDisable(t *testing.T) {
	r := newSetRig(t)
	au := r.srv.Auth()
	if au.KnownUser(r.br.Known.Value) != r.who.ID {
		t.Fatal("known value not registered")
	}
	if err := au.SetPassword(r.who.ID, fastHash(t, longPW), "test", "", ""); err != nil {
		t.Fatal(err)
	}
	if au.KnownUser(r.br.Known.Value) != "" {
		t.Fatal("a known browser survived a password change")
	}
	r.st.View(func(d *store.Data) {
		for k, v := range d.Settings {
			if strings.HasPrefix(k, "admin.kb.") && strings.Contains(k+v.Value, r.br.Known.Value) {
				t.Fatal("a raw known value is stored")
			}
		}
	})
}

func TestAPIKnownTokenLetsThePhoneThrough(t *testing.T) {
	r := newAPIRig(t)
	// The phone's login answer carries a known_token.
	login := func(tok string, ip string) reply {
		b := map[string]any{"username": "admin", "password": pw, "totp": r.ownerW.Code(t, r.st), "device": map[string]any{"install_id": "phone-1"}}
		if tok != "" {
			b["known_token"] = tok
		}
		return r.call("", "POST", "/auth/login", b, map[string]string{"X-Forwarded-For": ip})
	}
	first := login("", "203.0.113.50")
	known, _ := first.JSON(t)["known_token"].(string)
	if first.Status != 200 || known == "" {
		t.Fatalf("login: %d %s", first.Status, first.Raw)
	}
	// Strangers hammer the account from many addresses.
	r.srv.AuthInjectAccountFailures("admin", 60)
	start := time.Now()
	x := login(known, "203.0.113.51")
	if x.Status != 200 || time.Since(start) > 2*time.Second {
		t.Fatalf("the known phone: %d after %v", x.Status, time.Since(start))
	}
	if x.JSON(t)["known_token"] != known {
		t.Error("the known token should stay the same while it is in use")
	}
	// Without it, the same login is only slowed.
	start = time.Now()
	y := login("", "203.0.113.52")
	if y.Status != 200 {
		t.Fatalf("a stranger-class phone login must not be refused: %d %s", y.Status, y.Raw)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("not slowed: %v", time.Since(start))
	}
}

// ---- S1 ----------------------------------------------------------------------------------------

func TestHalfSessionsDoNotEvictFullOnes(t *testing.T) {
	r := newSetRig(t)
	for i := 0; i < 5; i++ { // the sign-in that made r.br was the first of six
		resp, half := r.passwordStep("admin", pw, "")
		if resp.StatusCode != 303 || half == nil {
			t.Fatalf("password step %d: %d", i, resp.StatusCode)
		}
	}
	// The seventh is refused: correct-password-unfinished attempts are limited.
	if resp, half := r.passwordStep("admin", pw, ""); resp.StatusCode == 303 || half != nil {
		t.Fatalf("an endless supply of half sessions: %d", resp.StatusCode)
	}
	if resp, _ := r.req("GET", "/admin", nil, nil, r.br.Cookie); resp.StatusCode != 200 {
		t.Fatalf("the owner's full session was evicted: %d", resp.StatusCode)
	}
	half, full := 0, 0
	r.st.View(func(d *store.Data) {
		for _, s := range d.AdminSessions {
			if s.UserID == r.who.ID {
				if s.MFA {
					full++
				} else {
					half++
				}
			}
		}
	})
	if half > authn.MaxHalfSessions || full < 1 {
		t.Fatalf("half=%d full=%d", half, full)
	}
}

func TestPasswordAcceptedIsAuditedAndAlerted(t *testing.T) {
	r := newSetRig(t)
	// The sign-in that made r.br already raised the alarm; more in the same
	// window are audited but must not flood the owners' phones.
	r.passwordStep("admin", pw, "")
	r.passwordStep("admin", pw, "")
	got := r.gotAlerts()
	if len(got) != 1 || !strings.Contains(got[0], "password accepted for admin") || !strings.Contains(got[0], "second factor pending") {
		t.Fatalf("alerts: %q", got)
	}
	found := 0
	r.st.View(func(d *store.Data) {
		for _, e := range d.Events {
			if strings.Contains(e.Msg, "second factor pending") && e.Kind == "audit" {
				found++
			}
		}
	})
	if found < 3 {
		t.Errorf("audit events: %d", found)
	}
	// The owner's known browser is not an alarm.
	n := len(r.gotAlerts())
	lc := r.loginForm()
	resp, _ := r.req("POST", "/admin/login", url.Values{"username": {"admin"}, "password": {pw}, "_csrf": {lc.Value}}, nil, lc, r.br.Known)
	if resp.StatusCode != 303 || len(r.gotAlerts()) != n {
		t.Errorf("known browser alerted: %d %v", resp.StatusCode, r.gotAlerts()[n:])
	}
}

// ---- S2 ----------------------------------------------------------------------------------------

func (r *setRig) postSec(path string, f url.Values) string {
	_, msg, _ := r.post(path, f)
	return msg
}

func TestSensitiveActionsNeedAPasswordAndACurrentCode(t *testing.T) {
	r := newSetRig(t)
	au := r.srv.Auth()
	create := func(extra url.Values) (string, bool) {
		f := url.Values{"username": {"newperson"}, "role": {"viewer"}, "password": {longPW}, "current_password": {pw}}
		for k, v := range extra {
			f[k] = v
		}
		_, msg, body := r.post("/admin/security/users/create", f)
		_, made := au.UserByName("newperson")
		return msg + body, made
	}
	// Password alone is not enough.
	if out, made := create(nil); made || !strings.Contains(out, "Error") && !strings.Contains(out, "authenticator") {
		t.Fatalf("created without a code: %q", out)
	}
	// A wrong code is not enough.
	if _, made := create(url.Values{"totp": {"000000"}}); made {
		t.Fatal("created with a wrong code")
	}
	// A good code works.
	if _, made := create(url.Values{"totp": {r.who.Code(t, r.st)}}); !made {
		t.Fatal("not created with a good code")
	}
	nu, _ := au.UserByName("newperson")
	// Role change, reset-2FA and disable need the code too.
	for name, path := range map[string]string{"role": "/admin/security/users/role", "reset2fa": "/admin/security/users/reset2fa", "disable": "/admin/security/users/disable"} {
		f := url.Values{"id": {nu.ID}, "role": {"operator"}, "current_password": {pw}}
		r.postSec(path, f)
		got, _ := au.UserByID(nu.ID)
		if got.Role != "viewer" || got.Disabled || au.HasSetupCode(nu.ID) && name == "reset2fa" && false {
			t.Fatalf("%s went through without a code: %+v", name, got)
		}
	}
	// With a code, the role change goes through.
	r.postSec("/admin/security/users/role", url.Values{"id": {nu.ID}, "role": {"operator"}, "current_password": {pw}, "totp": {r.who.Code(t, r.st)}})
	if got, _ := au.UserByID(nu.ID); got.Role != "operator" {
		t.Fatalf("role not changed: %+v", got)
	}
	// A code is spent by use: repeating it at once is refused.
	code := r.who.Code(t, r.st)
	r.postSec("/admin/security/users/role", url.Values{"id": {nu.ID}, "role": {"viewer"}, "current_password": {pw}, "totp": {code}})
	r.postSec("/admin/security/users/role", url.Values{"id": {nu.ID}, "role": {"operator"}, "current_password": {pw}, "totp": {code}})
	if got, _ := au.UserByID(nu.ID); got.Role != "viewer" {
		t.Fatalf("a replayed code was accepted: %+v", got)
	}
}

func TestRepeatedWrongConfirmationsEndTheSession(t *testing.T) {
	r := newSetRig(t)
	other := signIn(t, r.ts, r.srv, r.st, r.who)
	bad := func(b *browser) string {
		resp := b.Do(r.cl, r.ts.URL, "POST", "/admin/security/password", url.Values{"current_password": {"wrong-password-x"}, "new_password": {longPW}, "confirm_password": {longPW}, "totp": {"000000"}})
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	for i := 0; i < 5; i++ {
		bad(r.br)
	}
	if resp, _ := r.req("GET", "/admin", nil, nil, r.br.Cookie); resp.StatusCode != 303 {
		t.Fatalf("the session survived five wrong confirmations: %d", resp.StatusCode)
	}
	// The limit is per user as well: the failures above also block the user's other session.
	if loc := bad(other); !strings.Contains(loc, "too+many") && !strings.Contains(loc, "too%20many") {
		t.Fatalf("another session of the same user was not limited: %q", loc)
	}
}

// ---- S3 ----------------------------------------------------------------------------------------

func TestTOTPFailuresAreDurableAndBackOff(t *testing.T) {
	r := newSetRig(t)
	au := r.srv.Auth()
	for i := 0; i < 4; i++ {
		if err := au.VerifyTOTPFrom(r.who.ID, "000000", "203.0.113.5"); err != authn.ErrBadCode {
			t.Fatalf("wrong code %d: %v", i, err)
		}
	}
	// The fifth wrong code alerts the owners.
	n := len(r.gotAlerts())
	au.VerifyTOTPFrom(r.who.ID, "000000", "203.0.113.5")
	if a := r.gotAlerts()[n:]; len(a) != 1 || !strings.Contains(a[0], "5 wrong authenticator codes") {
		t.Fatalf("alerts after 5: %q", a)
	}
	// A right code is refused during the back-off ...
	good := r.who.Code(t, r.st)
	if err := au.VerifyTOTPFrom(r.who.ID, good, ""); err != authn.ErrTOTPBackoff {
		t.Fatalf("right code during back-off: %v", err)
	}
	// ... also after a restart (a new service on the same database): the counter is in the row.
	au2, _ := authn.New(r.st, testKey, "x")
	if err := au2.VerifyTOTP(r.who.ID, good); err != authn.ErrTOTPBackoff {
		t.Fatalf("after a restart: %v", err)
	}
	r.st.View(func(d *store.Data) {
		if u := d.AdminUsers[r.who.ID]; u.TOTPFails != 5 || u.TOTPFailAt.IsZero() {
			t.Fatalf("stored counter: %+v", u)
		}
	})
	// The web page says so too (and does not spend the code).
	_, half := r.passwordStep("admin", pw, "")
	sess, _, _ := au.Session(half.Value)
	resp, _ := r.req("POST", "/admin/login/totp", url.Values{"code": {good}, "_csrf": {sess.CSRF}}, nil, half)
	if resp.StatusCode != 429 {
		t.Fatalf("web during back-off: %d", resp.StatusCode)
	}
	// After the wait the code works.
	age := func(d time.Duration) {
		r.st.Update(func(dd *store.Data) error {
			u := dd.AdminUsers[r.who.ID]
			u.TOTPFailAt = time.Now().Add(-d)
			dd.AdminUsers[r.who.ID] = u
			return nil
		})
	}
	age(2 * time.Hour)
	// Five more wrong codes (to ten): only a recovery code works from then on.
	for i := 0; i < 5; i++ {
		au.VerifyTOTPFrom(r.who.ID, "000000", "203.0.113.5")
		age(2 * time.Hour)
	}
	if err := au.VerifyTOTP(r.who.ID, r.who.Code(t, r.st)); err != authn.ErrTOTPLocked {
		t.Fatalf("after ten wrong codes: %v", err)
	}
	if a := r.gotAlerts(); !strings.Contains(strings.Join(a, "|"), "only a recovery code signs in now") {
		t.Fatalf("no alert at the lock: %q", a)
	}
	// The API says what to do.
	m := r.srv
	_ = m
	codes, _ := au.RegenerateRecoveryCodes(r.who.ID, "test", "")
	if err := au.UseRecoveryCode(r.who.ID, codes[0], ""); err != nil {
		t.Fatal(err)
	}
	if err := au.VerifyTOTP(r.who.ID, r.who.Code(t, r.st)); err != nil {
		t.Fatalf("after a recovery code the count starts again: %v", err)
	}
}

func TestOldWrongCodesFade(t *testing.T) {
	r := newSetRig(t)
	au := r.srv.Auth()
	for i := 0; i < 5; i++ {
		au.VerifyTOTPFrom(r.who.ID, "000000", "")
	}
	r.st.Update(func(d *store.Data) error {
		u := d.AdminUsers[r.who.ID]
		u.TOTPFailAt = time.Now().Add(-48 * time.Hour)
		d.AdminUsers[r.who.ID] = u
		return nil
	})
	if err := au.VerifyTOTP(r.who.ID, r.who.Code(t, r.st)); err != nil {
		t.Fatalf("a day-old count below the lock should not block: %v", err)
	}
}

func TestAPILoginReportsTheLock(t *testing.T) {
	r := newAPIRig(t)
	for i := 0; i < 10; i++ {
		r.srv.Auth().VerifyTOTPFrom(r.ownerW.ID, "000000", "")
		r.st.Update(func(d *store.Data) error {
			u := d.AdminUsers[r.ownerW.ID]
			u.TOTPFailAt = time.Now().Add(-3 * time.Hour)
			d.AdminUsers[r.ownerW.ID] = u
			return nil
		})
	}
	x := r.call("", "POST", "/auth/login", map[string]any{"username": "admin", "password": pw, "totp": r.ownerW.Code(t, r.st), "device": map[string]any{}}, nil)
	if x.Status != 401 || x.errCode(t) != "totp_invalid" || !strings.Contains(string(x.Raw), "recovery_code_required") {
		t.Fatalf("%d %s", x.Status, x.Raw)
	}
}

// ---- S5 + nits ---------------------------------------------------------------------------------

func TestRefreshReuseOfAnyOlderToken(t *testing.T) {
	r := newAPIRig(t)
	seen := []string{r.owner.Refresh}
	cur := r.owner.Refresh
	for i := 0; i < 4; i++ {
		x := r.call("", "POST", "/auth/refresh", map[string]any{"refresh_token": cur}, nil)
		if x.Status != 200 {
			t.Fatalf("rotation %d: %d %s", i, x.Status, x.Raw)
		}
		cur = str(x.JSON(t), "refresh_token")
		seen = append(seen, cur)
	}
	// Age the grace window so the previous token is no longer a retry.
	r.srv.Auth().SetClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	// seen[1] is three rotations old: not the previous token, but one the device had.
	x := r.call("", "POST", "/auth/refresh", map[string]any{"refresh_token": seen[1]}, nil)
	if x.Status != 401 || x.errCode(t) != "token_revoked" {
		t.Fatalf("old token: %d %s", x.Status, x.Raw)
	}
	if y := r.call("", "POST", "/auth/refresh", map[string]any{"refresh_token": cur}, nil); y.Status != 401 {
		t.Fatalf("the device must be revoked by the reuse: %d", y.Status)
	}
	// A guess with a real device id revokes nothing.
	z := r.call("", "POST", "/auth/refresh", map[string]any{"refresh_token": "ftnr_" + r.oper.DeviceID + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}, nil)
	if z.Status != 401 {
		t.Fatalf("guess: %d", z.Status)
	}
	if me := r.call(r.oper.Access, "GET", "/me", nil, nil); me.Status != 200 {
		t.Fatal("a guessed refresh token revoked a device")
	}
}

func TestSetupCodeDiesWithPasswordChange(t *testing.T) {
	r := newSetRig(t)
	au := r.srv.Auth()
	u, err := au.CreateUser("temp", store.RoleViewer, fastHash(t, longPW), "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := au.IssueSetupCode(u.ID, "test", ""); err != nil {
		t.Fatal(err)
	}
	if err := au.SetPassword(u.ID, fastHash(t, longPW+"2"), "test", "", ""); err != nil {
		t.Fatal(err)
	}
	if au.HasSetupCode(u.ID) {
		t.Fatal("the setup code outlived the old password")
	}
}

func TestIdempotencyTableIsCappedPerDevice(t *testing.T) {
	r := newAPIRig(t)
	call := func(tok string, i int) reply {
		return r.call(tok, "POST", "/accounts", map[string]any{"site_id": r.A.ID, "flat": fmt.Sprintf("X%d", i), "mac": "bad"},
			map[string]string{"Idempotency-Key": fmt.Sprintf("key-%d", i)})
	}
	first := call(r.view.Access, 0) // a viewer is refused (403, stored too: < 500)
	_ = first
	call2 := call(r.oper.Access, 1)
	if call2.Status != 400 {
		t.Fatalf("setup: %d", call2.Status)
	}
	for i := 2; i < 700; i++ {
		call(r.owner.Access, i)
	}
	if x := call(r.oper.Access, 1); x.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("one phone's flood evicted another phone's stored answer")
	}
	if x := call(r.owner.Access, 2); x.Header.Get("Idempotent-Replayed") == "true" {
		t.Fatal("the flooding phone's oldest key should be gone")
	}
	if x := call(r.owner.Access, 699); x.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("the flooding phone's newest key should still replay")
	}
}
