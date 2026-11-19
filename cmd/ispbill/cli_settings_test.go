package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/config"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

func cliEnv(dir string) func(string) string {
	m := map[string]string{"DATA_DIR": dir, "ADMIN_PASS": "from-the-env-file-123", "DATA_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"MPESA_ENV": "sandbox", "PUBLIC_BASE_URL": "https://billing.example.co.ke"}
	return func(k string) string { return m[k] }
}

// openAuth opens the database a command line run left behind.
func openAuth(t *testing.T, dir string, env func(string) string) (*authn.Service, *store.Store) {
	t.Helper()
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	au, err := authn.New(st, cfg.DataKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	return au, st
}

func cli(t *testing.T, env func(string) string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	handled, err := runCLI(args, env, &out)
	if !handled {
		t.Fatalf("not handled: %v", args)
	}
	return out.String(), err
}

func printedPassword(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); len(l) == 20 && !strings.Contains(l, " ") {
			return l
		}
	}
	return ""
}

func printedSetupCode(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); len(l) == 19 && strings.Count(l, "-") == 3 && !strings.Contains(l, " ") {
			return l
		}
	}
	return ""
}

func TestAdminResetGenerateCreatesOwner(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	out, err := cli(t, env, "admin-reset-password", "-generate")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pw := printedPassword(out)
	if pw == "" {
		t.Fatalf("no password printed: %q", out)
	}
	au, _ := openAuth(t, dir, env)
	u, ok := au.Owner()
	if !ok || u.Username != "admin" || u.Role != store.RoleOwner {
		t.Fatalf("owner: %+v %v", u, ok)
	}
	if !settings.VerifyPassword(u.PassHash, pw) {
		t.Fatal("the printed password does not match the stored hash")
	}
	if strings.Contains(u.PassHash, pw) {
		t.Fatal("the password is stored in clear")
	}
}

func TestAdminResetSetupCode(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	out, err := cli(t, env, "admin-reset-password", "-generate", "-setup-code")
	if err != nil {
		t.Fatal(err)
	}
	code := printedSetupCode(out)
	if code == "" {
		t.Fatalf("no setup code printed: %s", out)
	}
	au, _ := openAuth(t, dir, env)
	u, _ := au.Owner()
	if !au.CheckSetupCode(u.ID, code) {
		t.Fatal("the printed setup code does not verify")
	}
	// Once enrolled, the installer's every-run call prints a note, exits 0 and issues nothing.
	secret, err := au.BeginEnrol(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := au.ConfirmEnrol(u.ID, authn.TOTPCode(secret, authn.TOTPStep(time.Now())), "t", ""); err != nil {
		t.Fatal(err)
	}
	out, err = cli(t, env, "admin-reset-password", "-setup-code")
	if err != nil {
		t.Fatalf("an enrolled owner must not be an error: %v", err)
	}
	if printedSetupCode(out) != "" || !strings.Contains(out, "already has an authenticator") {
		t.Fatalf("no note for an enrolled owner: %s", out)
	}
	if au2, _ := openAuth(t, dir, env); au2 == nil {
		t.Fatal("reopen")
	}
}

func TestAdminResetTOTPAndSessions(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	if _, err := cli(t, env, "admin-reset-password", "-generate"); err != nil {
		t.Fatal(err)
	}
	au, st := openAuth(t, dir, env)
	u, _ := au.Owner()
	secret, _ := au.BeginEnrol(u.ID)
	if _, err := au.ConfirmEnrol(u.ID, authn.TOTPCode(secret, authn.TOTPStep(time.Now())), "t", ""); err != nil {
		t.Fatal(err)
	}
	val, _, err := au.CreateSession(u.ID, "10.0.0.1", "ua", true)
	if err != nil {
		t.Fatal(err)
	}
	au2 := au
	st.Close()

	// -revoke-sessions alone leaves the password and the app alone.
	out, err := cli(t, env, "admin-reset-password", "-revoke-sessions")
	if err != nil || !strings.Contains(out, "signed out 1") {
		t.Fatalf("%v\n%s", err, out)
	}
	au2, _ = openAuth(t, dir, env)
	if _, _, ok := au2.Session(val); ok {
		t.Fatal("the session survived -revoke-sessions")
	}
	if u2, _ := au2.Owner(); !authn.Enrolled(u2) || u2.PassHash != u.PassHash {
		t.Fatal("-revoke-sessions changed more than the sessions")
	}
	// -reset-totp removes the app, keeps the password.
	out, err = cli(t, env, "admin-reset-password", "-reset-totp", "-setup-code")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	au3, _ := openAuth(t, dir, env)
	u3, _ := au3.Owner()
	if authn.Enrolled(u3) || u3.PassHash != u.PassHash || len(u3.RecoveryCodes) != 0 {
		t.Fatalf("after -reset-totp: %+v", u3)
	}
	if code := printedSetupCode(out); code == "" || !au3.CheckSetupCode(u3.ID, code) {
		t.Fatalf("no usable setup code after -reset-totp: %s", out)
	}
}

// With no flag the password goes back to ADMIN_PASS; an unusable ADMIN_PASS is refused.
func TestPlainResetUsesEnvPassword(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	if _, err := cli(t, env, "admin-reset-password", "-generate"); err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, env, "admin-reset-password")
	if err != nil || !strings.Contains(out, "ADMIN_PASS") {
		t.Fatalf("%v\n%s", err, out)
	}
	au, _ := openAuth(t, dir, env)
	if u, _ := au.Owner(); !settings.VerifyPassword(u.PassHash, "from-the-env-file-123") {
		t.Fatal("the password is not ADMIN_PASS")
	}
	for _, envPass := range []string{"", "admin"} {
		weak := func(k string) string {
			if k == "ADMIN_PASS" {
				return envPass
			}
			return env(k)
		}
		before, _ := au.Owner()
		_, err := cli(t, weak, "admin-reset-password")
		if err == nil || !strings.Contains(err.Error(), "-generate") {
			t.Fatalf("ADMIN_PASS=%q: want a refusal pointing at -generate, got %v", envPass, err)
		}
		au, _ = openAuth(t, dir, env)
		if after, _ := au.Owner(); after.PassHash != before.PassHash {
			t.Fatalf("ADMIN_PASS=%q: the password changed anyway", envPass)
		}
	}
}

func TestAdminStatusJSON(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	out, err := cli(t, env, "admin-status", "-json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["owner"] != "" || got["totp_enrolled"] != false {
		t.Fatalf("no owner yet: %s %v", out, err)
	}
	if _, err := cli(t, env, "admin-reset-password", "-generate"); err != nil {
		t.Fatal(err)
	}
	out, _ = cli(t, env, "admin-status", "-json")
	if strings.TrimSpace(out) != `{"owner":"admin","totp_enrolled":false}` {
		t.Fatalf("%s", out)
	}
	au, _ := openAuth(t, dir, env)
	u, _ := au.Owner()
	secret, _ := au.BeginEnrol(u.ID)
	if _, err := au.ConfirmEnrol(u.ID, authn.TOTPCode(secret, authn.TOTPStep(time.Now())), "t", ""); err != nil {
		t.Fatal(err)
	}
	out, _ = cli(t, env, "admin-status", "-json")
	if strings.TrimSpace(out) != `{"owner":"admin","totp_enrolled":true}` {
		t.Fatalf("%s", out)
	}
}

func TestSettingsCheck(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	out, err := cli(t, env, "settings-check")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"DATA_KEY: set", "admin owner: not created", "[mpesa]", "[whatsapp]", "[email]", "wa.token"} {
		if !strings.Contains(out, want) {
			t.Errorf("settings-check output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "from-the-env-file-123") {
		t.Fatal("a secret was printed")
	}
}

func TestResetNotesRestart(t *testing.T) {
	env := cliEnv(t.TempDir())
	out, err := cli(t, env, "admin-reset-password", "-generate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "service again") {
		t.Fatalf("no start hint for the JSON database: %s", out)
	}
	var pg bytes.Buffer
	restartNote(&pg, config.Config{DatabaseURL: "postgres://x"})
	if !strings.Contains(pg.String(), "systemctl start ftnbilling") {
		t.Fatalf("no start notice for Postgres: %s", pg.String())
	}
}

// -setup-code works on a fresh database, before the service ever started: the
// owner is made from ADMIN_PASS (or the old admin.pass_hash setting) as main does.
func TestSetupCodeBeforeFirstStart(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	out, err := cli(t, env, "admin-reset-password", "-setup-code")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	code := printedSetupCode(out)
	if code == "" {
		t.Fatalf("no code: %s", out)
	}
	au, _ := openAuth(t, dir, env)
	u, ok := au.Owner()
	if !ok || u.Username != "admin" || !settings.VerifyPassword(u.PassHash, "from-the-env-file-123") || !au.CheckSetupCode(u.ID, code) {
		t.Fatalf("owner after -setup-code: %+v %v", u, ok)
	}
	// With the old Settings-page password stored, that is the password.
	dir2 := t.TempDir()
	env2 := cliEnv(dir2)
	hash, _ := settings.HashPasswordCost("old-ui-password-1", 1000)
	s2, err := store.Open(dir2)
	if err != nil {
		t.Fatal(err)
	}
	s2.Update(func(d *store.Data) error {
		d.Settings["admin.pass_hash"] = store.Setting{Key: "admin.pass_hash", Value: hash, UpdatedAt: time.Now()}
		return nil
	})
	s2.Close()
	if out, err := cli(t, env2, "admin-reset-password", "-setup-code"); err != nil || printedSetupCode(out) == "" {
		t.Fatalf("%v\n%s", err, out)
	}
	au2, _ := openAuth(t, dir2, env2)
	if u2, _ := au2.Owner(); !settings.VerifyPassword(u2.PassHash, "old-ui-password-1") {
		t.Fatal("the stored admin.pass_hash was not used")
	}
	// No usable password anywhere: a clear error, nothing created.
	fresh := cliEnv(t.TempDir())
	weak := func(k string) string {
		if k == "ADMIN_PASS" {
			return ""
		}
		return fresh(k)
	}
	if _, err := cli(t, weak, "admin-reset-password", "-setup-code"); err == nil || !strings.Contains(err.Error(), "ADMIN_PASS") {
		t.Fatalf("want an ADMIN_PASS error, got %v", err)
	}
}

func TestJSONRefusesWhileServiceRuns(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })}
	go srv.Serve(ln)
	defer srv.Close()
	env := func(k string) string {
		if k == "LISTEN_ADDR" {
			return ln.Addr().String()
		}
		return cliEnv(dir)(k)
	}
	for _, args := range [][]string{{"admin-reset-password", "-generate"}, {"admin-status", "-json"}} {
		if _, err := cli(t, env, args...); err == nil || !strings.Contains(err.Error(), "running") {
			t.Fatalf("%v while the service runs: %v", args, err)
		}
	}
}

// The doctor reads the "receipt confirmation:" line of settings-check.
func TestSettingsCheckReportsReceiptConfirmation(t *testing.T) {
	mp := map[string]string{"MPESA_CONSUMER_KEY": "key-12345678", "MPESA_CONSUMER_SECRET": "secret-12345678", "MPESA_SHORTCODE": "174379",
		"MPESA_PASSKEY": "passkey-0123456789abc", "MPESA_CALLBACK_TOKEN": "token-0123456789abcdef"}
	run := func(extra map[string]string) string {
		base := cliEnv(t.TempDir())
		env := func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			if v, ok := mp[k]; ok {
				return v
			}
			return base(k)
		}
		out, err := cli(t, env, "settings-check")
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	if out := run(nil); !strings.Contains(out, "receipt confirmation: OFF") || !strings.Contains(out, "MPESA_INITIATOR_NAME") {
		t.Errorf("without initiator settings:\n%s", out)
	}
	if out := run(map[string]string{"MPESA_INITIATOR_NAME": "apiop", "MPESA_SECURITY_CREDENTIAL": strings.Repeat("Q", 64)}); !strings.Contains(out, "receipt confirmation: on") {
		t.Errorf("with initiator settings:\n%s", out)
	}
	if out := run(map[string]string{"MPESA_INITIATOR_NAME": "apiop", "MPESA_SECURITY_CREDENTIAL": strings.Repeat("Q", 64), "MPESA_CONFIRM_RECEIPTS": "off"}); !strings.Contains(out, "receipt confirmation: OFF") {
		t.Errorf("explicitly off:\n%s", out)
	}
}
