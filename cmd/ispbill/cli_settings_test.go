package main

import (
	"bytes"
	"encoding/json"
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
	if !strings.Contains(pg.String(), "RESTART REQUIRED") {
		t.Fatalf("no restart notice for Postgres: %s", pg.String())
	}
}
