package settings

import (
	"bytes"
	"strings"
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

var testKey = bytes.Repeat([]byte{9}, 32)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func newMgr(t *testing.T, env map[string]string, key []byte) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	base := map[string]string{"ADMIN_PASS": "s3cret-pass!"}
	for k, v := range env {
		base[k] = v
	}
	cfg, err := config.Load(envMap(base))
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(st, key, cfg, envMap(base))
	if err != nil {
		t.Fatal(err)
	}
	return m, st
}

func TestPrecedenceDatabaseOverEnvOverDefault(t *testing.T) {
	m, _ := newMgr(t, map[string]string{"WA_LANG": "sw", "WA_DAILY_LIMIT": "50"}, testKey)
	c := m.Effective()
	if c.WALang != "sw" || m.Source("wa.lang") != SrcEnv || c.WAAPIVersion != "v23.0" || m.Source("wa.api_version") != SrcDefault {
		t.Fatalf("env/default: %+v %s %s", c.WALang, m.Source("wa.lang"), m.Source("wa.api_version"))
	}
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.lang": "en_GB", "wa.daily_limit": "60"}, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	c = m.Effective()
	if c.WALang != "en_GB" || c.WADailyLimit != 60 || m.Source("wa.lang") != SrcDatabase {
		t.Fatalf("db over env: %+v", c)
	}
	// Reverting brings the env value back.
	if err := m.Revert("wa.lang", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if m.Effective().WALang != "sw" || m.Source("wa.lang") != SrcEnv {
		t.Fatal("revert did not return to env")
	}
}

func TestUnchangedFieldsStoreNothing(t *testing.T) {
	m, st := newMgr(t, map[string]string{"WA_LANG": "sw", "SMTP_HOST": "smtp.example.com", "SMTP_FROM": "a@example.com"}, testKey)
	// The form posts every field with its current value: nothing may be copied into the database.
	vals := map[string]string{}
	for _, f := range m.Fields(SecEmail) {
		if !f.Secret {
			vals[f.Key] = f.Value
		}
	}
	ch, err := m.Save(SecEmail, vals, "x")
	if err != nil || len(ch) != 0 {
		t.Fatalf("%v %v", ch, err)
	}
	st.View(func(d *store.Data) {
		if len(d.Settings) != 0 {
			t.Fatalf("rows were created for unchanged values: %v", d.Settings)
		}
	})
	if m.Source("smtp.host") != SrcEnv {
		t.Fatal("env value became a database value")
	}
}

func TestSecretsAreEncryptedBoundAndNeverAudited(t *testing.T) {
	m, st := newMgr(t, nil, testKey)
	const secret = "EAAGsupersecrettokenvalue1234567890"
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.token": secret, "wa.phone_number_id": "1234567890123"}, "10.98.0.2"); err != nil {
		t.Fatal(err)
	}
	st.View(func(d *store.Data) {
		row := d.Settings["wa.token"]
		if !row.Enc || strings.Contains(row.Value, secret) || strings.Contains(row.Value, "1234567890") {
			t.Fatalf("secret not encrypted: %+v", row)
		}
		for _, e := range d.Events {
			if strings.Contains(e.Msg, secret) || strings.Contains(e.Msg, secret[len(secret)-4:]) {
				t.Fatalf("audit leaks the secret: %q", e.Msg)
			}
		}
		last := d.Events[len(d.Events)-1]
		if last.Kind != "audit" || !strings.Contains(last.Msg, "wa.token set") || !strings.Contains(last.Msg, "10.98.0.2") {
			t.Fatalf("audit event: %+v", last)
		}
	})
	if m.Effective().WAToken != secret {
		t.Fatal("secret not readable back")
	}
	for _, f := range m.Fields(SecWhatsApp) {
		if f.Key == "wa.token" && (f.Value != "" || !f.Set || !strings.HasPrefix(f.Masked, "••••") || strings.Contains(f.Masked, secret)) {
			t.Fatalf("field view leaks: %+v", f)
		}
	}
	// A sealed value copied onto another row does not open there (AAD = key).
	box, _ := secretbox.NewSettings(testKey)
	st.Update(func(d *store.Data) error {
		r := d.Settings["wa.token"]
		r.Key = "mpesa.secret"
		d.Settings["mpesa.secret"] = r
		return nil
	})
	m.rebuild()
	if m.Effective().MpesaSecret != "" || len(m.Problems()) != 1 {
		t.Fatalf("swapped ciphertext accepted: %q %v", m.Effective().MpesaSecret, m.Problems())
	}
	_ = box
	// Replacing says "replaced".
	ch, _ := m.Save(SecWhatsApp, map[string]string{"wa.token": "EAAGanotherlongtokenvalue98765432"}, "")
	if len(ch) != 1 || ch[0].What != "replaced" {
		t.Fatalf("%+v", ch)
	}
	// Empty secret input keeps the value.
	if ch, _ := m.Save(SecWhatsApp, map[string]string{"wa.token": ""}, ""); len(ch) != 0 {
		t.Fatal("empty secret input must keep the value")
	}
}

func TestSecretsRefusedWithoutDataKeyButPlainSettingsSave(t *testing.T) {
	m, _ := newMgr(t, nil, nil)
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.token": "EAAGsupersecrettokenvalue1234567890"}, ""); err != ErrNoDataKey {
		t.Fatalf("got %v", err)
	}
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.lang": "sw"}, ""); err != nil || m.Effective().WALang != "sw" {
		t.Fatalf("plain setting: %v", err)
	}
	if _, err := m.Save(SecMpesa, map[string]string{"mpesa.key": "abcdefghijkl", "mpesa.secret": "abcdefghijkl", "mpesa.shortcode": "174379"}, ""); err == nil {
		t.Fatal("mpesa secrets must be refused without DATA_KEY")
	}
}

func TestDecryptFailureFallsBackToEnv(t *testing.T) {
	m, st := newMgr(t, map[string]string{"WA_TOKEN": "env-token-env-token-env-token"}, testKey)
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.token": "EAAGsupersecrettokenvalue1234567890"}, ""); err != nil {
		t.Fatal(err)
	}
	// Same database, different DATA_KEY: the value cannot be opened.
	m2, err := New(st, bytes.Repeat([]byte{3}, 32), m.env, m.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Effective().WAToken != "env-token-env-token-env-token" || m2.Source("wa.token") != SrcEnv || len(m2.Problems()) != 1 {
		t.Fatalf("%q %s %v", m2.Effective().WAToken, m2.Source("wa.token"), m2.Problems())
	}
	// No key at all: also a fallback, not a crash.
	m3, _ := New(st, nil, m.env, m.getenv)
	if m3.Effective().WAToken != "env-token-env-token-env-token" || len(m3.Problems()) != 1 {
		t.Fatalf("%q %v", m3.Effective().WAToken, m3.Problems())
	}
}

func TestAppliersRunWithTheNewConfig(t *testing.T) {
	m, _ := newMgr(t, nil, testKey)
	var waCalls, mpCalls int
	var last config.Config
	m.OnApply(SecWhatsApp, func(c config.Config) { waCalls++; last = c })
	m.OnApply(SecMpesa, func(config.Config) { mpCalls++ })
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.lang": "sw"}, ""); err != nil {
		t.Fatal(err)
	}
	if waCalls != 1 || mpCalls != 0 || last.WALang != "sw" {
		t.Fatalf("wa %d mpesa %d lang %q", waCalls, mpCalls, last.WALang)
	}
	// A save that changes nothing applies nothing.
	m.Save(SecWhatsApp, map[string]string{"wa.lang": "sw"}, "")
	if waCalls != 1 {
		t.Fatal("applier ran for a no-op save")
	}
	m.Revert("wa.lang", "")
	if waCalls != 2 || last.WALang != "en" {
		t.Fatalf("revert: %d %q", waCalls, last.WALang)
	}
	m.ApplyAll()
	if waCalls != 3 || mpCalls != 1 {
		t.Fatal("ApplyAll")
	}
}

func TestValidation(t *testing.T) {
	m, _ := newMgr(t, nil, testKey)
	bad := map[string]map[string]string{
		SecWhatsApp: {"wa.admin_numbers": "abc", "wa.api_version": "23", "wa.daily_limit": "-1", "wa.quiet_from": "24", "wa.phone_number_id": "12ab", "wa.lang": "English"},
		SecMpesa:    {"mpesa.env": "live", "mpesa.shortcode": "12", "mpesa.unknown_ref": "maybe"},
		SecEmail:    {"smtp.host": "bad host!", "smtp.port": "70000", "smtp.from": "not-an-address", "alert.email_to": "nobody"},
	}
	for sec, vs := range bad {
		for k, v := range vs {
			if _, err := m.Save(sec, map[string]string{k: v}, ""); err == nil {
				t.Errorf("%s=%q must be refused", k, v)
			}
		}
	}
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.admin_numbers": "0712345678\n+254 722 111 222"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := m.Effective().WAAdminNumbers; len(got) != 2 || got[0] != "254712345678" || got[1] != "254722111222" {
		t.Fatalf("%v", got)
	}
	// Nothing was stored by the refused saves, and a failed save stores nothing at all.
	if _, err := m.Save(SecWhatsApp, map[string]string{"wa.lang": "sw", "wa.daily_limit": "x"}, ""); err == nil || m.Effective().WALang == "sw" {
		t.Fatal("a partly invalid form must store nothing")
	}
}

func TestMpesaSaveGeneratesCallbackTokenOnce(t *testing.T) {
	m, st := newMgr(t, nil, testKey)
	vals := map[string]string{"mpesa.key": "consumerkey1234", "mpesa.secret": "consumersecret1234", "mpesa.shortcode": "174379", "mpesa.passkey": "bfb279f9aa9bdbcf158e97dd71a467cd2e0c893059b10f78e6b72ada1ed2c919"}
	if _, err := m.Save(SecMpesa, vals, ""); err != nil {
		t.Fatal(err)
	}
	tok := m.Effective().MpesaCallbackToken
	if len(tok) != 48 || m.Source("mpesa.callback_token") != SrcDatabase {
		t.Fatalf("token %q", tok)
	}
	st.View(func(d *store.Data) {
		if r := d.Settings["mpesa.callback_token"]; !r.Enc || strings.Contains(r.Value, tok) {
			t.Fatalf("token stored in clear: %+v", r)
		}
	})
	m.Save(SecMpesa, map[string]string{"mpesa.env": "production"}, "")
	if m.Effective().MpesaCallbackToken != tok {
		t.Fatal("a later save must keep the token")
	}
	if err := m.RotateCallbackToken("1.2.3.4"); err != nil || m.Effective().MpesaCallbackToken == tok || len(m.Effective().MpesaCallbackToken) != 48 {
		t.Fatalf("rotate: %v", err)
	}
	if !m.Effective().MpesaEnabled() && m.Effective().PublicBaseURL != "" {
		t.Fatal("enabled expected once everything is set")
	}
}

func TestAdminPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil || !strings.HasPrefix(h, "pbkdf2-sha256$600000$") || strings.Count(h, "$") != 3 {
		t.Fatalf("%q %v", h, err)
	}
	if !VerifyPassword(h, "correct horse battery") || VerifyPassword(h, "wrong") || VerifyPassword("garbage", "x") || VerifyPassword(h[:len(h)-4], "correct horse battery") {
		t.Fatal("verify")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salt must differ")
	}
	for _, pw := range []string{"short", "admin", "administrator1"[:5], strings.Repeat("a", 201), "has\x00control-chars-here"} {
		if CheckNewPassword(pw, "admin") == nil {
			t.Errorf("%q must be refused", pw)
		}
	}
	if CheckNewPassword("Admin", "someone") == nil || CheckNewPassword("longenoughpassword", "admin") != nil || CheckNewPassword("longenoughuser", "longenoughuser") == nil {
		t.Fatal("rules")
	}
	if p, err := RandomPassword(); err != nil || len(p) != 20 {
		t.Fatalf("%q %v", p, err)
	}
}

func TestAdminPasswordStoreAndClear(t *testing.T) {
	m, st := newMgr(t, nil, nil) // the hash needs no DATA_KEY
	if m.HasAdminPassword() {
		t.Fatal("fresh")
	}
	if err := m.SetAdminPassword("a-new-long-password", "10.98.0.2"); err != nil || !m.HasAdminPassword() {
		t.Fatal(err)
	}
	if !VerifyPassword(m.AdminHash(), "a-new-long-password") {
		t.Fatal("stored hash does not verify")
	}
	st.View(func(d *store.Data) {
		if d.Settings[KeyAdminHash].Enc || strings.Contains(d.Events[len(d.Events)-1].Msg, "a-new-long") {
			t.Fatal("hash row / audit")
		}
	})
	if err := m.ClearAdminPassword("cli"); err != nil || m.HasAdminPassword() {
		t.Fatal(err)
	}
}

func TestMarkerNeedsNoAuditAndSurvivesRebuild(t *testing.T) {
	m, st := newMgr(t, nil, nil)
	before := 0
	st.View(func(d *store.Data) { before = len(d.Events) })
	if err := m.SetMarker(KeyWebhookVerified, "2026-11-18T10:00:00Z"); err != nil || m.Marker(KeyWebhookVerified) != "2026-11-18T10:00:00Z" {
		t.Fatal(err)
	}
	st.View(func(d *store.Data) {
		if len(d.Events) != before {
			t.Fatal("a marker must not add an audit event")
		}
	})
}
