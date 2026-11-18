package main

import (
	"bytes"
	"strings"
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

func cliEnv(dir string) func(string) string {
	m := map[string]string{"DATA_DIR": dir, "ADMIN_PASS": "from-the-env-file-123", "DATA_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"MPESA_ENV": "sandbox", "PUBLIC_BASE_URL": "https://billing.example.co.ke"}
	return func(k string) string { return m[k] }
}

func TestAdminResetPassword(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	var out bytes.Buffer
	handled, err := runCLI([]string{"admin-reset-password"}, env, &out)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v out=%s", handled, err, out.String())
	}
	// The printed password is 20 characters and is now the stored one.
	var pw string
	for _, line := range strings.Split(out.String(), "\n") {
		if l := strings.TrimSpace(line); len(l) == 20 && !strings.Contains(l, " ") {
			pw = l
		}
	}
	if pw == "" {
		t.Fatalf("no password printed: %q", out.String())
	}
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := settings.New(st, cfg.DataKey, cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasAdminPassword() || !settings.VerifyPassword(m.AdminHash(), pw) {
		t.Fatal("the printed password does not match the stored hash")
	}
	if strings.Contains(m.AdminHash(), pw) {
		t.Fatal("the password is stored in clear")
	}
}

func TestSettingsCheckAndStdinReset(t *testing.T) {
	dir := t.TempDir()
	env := cliEnv(dir)
	var out bytes.Buffer
	if err := runSettingsCommand("settings-check", nil, env, strings.NewReader(""), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"DATA_KEY: set", "from ADMIN_PASS", "[mpesa]", "[whatsapp]", "[email]", "wa.token"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("settings-check output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "from-the-env-file-123") {
		t.Fatal("a secret was printed")
	}
	out.Reset()
	if err := runSettingsCommand("admin-reset-password", []string{"-password-stdin"}, env, strings.NewReader("short\n"), &out); err == nil {
		t.Fatal("a short password was accepted")
	}
	if err := runSettingsCommand("admin-reset-password", []string{"-password-stdin"}, env, strings.NewReader("a-good-long-password-9\n"), &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSettingsCommand("settings-check", nil, env, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), "stored in the database") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}
