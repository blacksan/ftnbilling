package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAdminPassRules(t *testing.T) {
	// Load no longer refuses a missing ADMIN_PASS (the password may live in the
	// database); CheckAdminPass is what main applies when the database has none.
	for _, pw := range []string{"", "admin"} {
		c, err := Load(env(map[string]string{"ADMIN_PASS": pw, "ROUTER_MOCK": "true"}))
		if err != nil {
			t.Errorf("ADMIN_PASS=%q must load: %v", pw, err)
		}
		if c.CheckAdminPass() == nil {
			t.Errorf("ADMIN_PASS=%q must not be usable as the admin password", pw)
		}
	}
	if c, _ := Load(env(map[string]string{"ADMIN_PASS": "s3cret!"})); c.CheckAdminPass() != nil {
		t.Error("a real ADMIN_PASS is usable")
	}
	if _, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "true"})); err != nil {
		t.Fatal(err)
	}
}

func TestWireGuardSettings(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "PUBLIC_IP": "203.0.113.5"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.WGSubnet != "10.99.0.0/22" || c.WGEndpoint != "203.0.113.5:51820" || c.JoinTokenTTL != 30*time.Minute ||
		c.AllowInsecureJoin || !c.JoinRequireMikrotikUA {
		t.Fatalf("%+v", c)
	}
	c, err = Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "WG_ENDPOINT": "198.51.100.1:51999", "JOIN_TOKEN_TTL": "10m",
		"ALLOW_INSECURE_JOIN": "true", "JOIN_REQUIRE_MIKROTIK_UA": "false",
		"WG_PUBLIC_KEY": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}))
	if err != nil || c.WGEndpoint != "198.51.100.1:51999" || c.JoinTokenTTL != 10*time.Minute || !c.AllowInsecureJoin ||
		c.JoinRequireMikrotikUA {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []map[string]string{{"WG_SUBNET": "nope"}, {"WG_PUBLIC_KEY": "short"}, {"WG_SUBNET": "10.99.0.0/31"}} {
		bad["ADMIN_PASS"] = "s3cret!"
		if _, err := Load(env(bad)); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}

func TestMpesaEnabledNeedsEverything(t *testing.T) {
	m := map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "1", "MPESA_CONSUMER_KEY": "k", "MPESA_CONSUMER_SECRET": "s",
		"MPESA_SHORTCODE": "1", "MPESA_PASSKEY": "p"}
	c, _ := Load(env(m))
	if c.MpesaEnabled() {
		t.Fatal("PUBLIC_BASE_URL missing")
	}
	m["PUBLIC_BASE_URL"] = "https://x.example/"
	c, _ = Load(env(m))
	if !c.MpesaEnabled() || c.PublicBaseURL != "https://x.example" || c.MpesaBaseURL() != "https://sandbox.safaricom.co.ke" {
		t.Fatalf("%+v", c)
	}
	m["MPESA_ENV"] = "production"
	c, _ = Load(env(m))
	if c.MpesaBaseURL() != "https://api.safaricom.co.ke" {
		t.Fatal(c.MpesaBaseURL())
	}
}

func TestMpesaNeedsCallbackToken(t *testing.T) {
	m := map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "1", "MPESA_CONSUMER_KEY": "k", "MPESA_CONSUMER_SECRET": "s",
		"MPESA_SHORTCODE": "1", "MPESA_PASSKEY": "p", "PUBLIC_BASE_URL": "https://x.example"}
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "MPESA_CALLBACK_TOKEN") {
		t.Fatalf("M-Pesa configured without a callback token must be refused, got %v", err)
	}
	m["MPESA_CALLBACK_TOKEN"] = "tok"
	if c, err := Load(env(m)); err != nil || c.MpesaCallbackToken != "tok" {
		t.Fatalf("with a token: %v", err)
	}
	// M-Pesa not configured: no token needed (the installer's blank Daraja settings).
	delete(m, "MPESA_CALLBACK_TOKEN")
	for _, k := range []string{"MPESA_CONSUMER_KEY", "MPESA_CONSUMER_SECRET", "MPESA_SHORTCODE", "MPESA_PASSKEY"} {
		delete(m, k)
	}
	if _, err := Load(env(m)); err != nil {
		t.Fatalf("no M-Pesa, no token: %v", err)
	}
}

func TestMetricsAndLifecycleDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.WANInterface != "ether1" || c.OLTInterface != "sfp-sfpplus1" || c.WANCapacityMbps != 500 || c.FUPTB != 15 ||
		c.NetflowAddr != ":2055" || c.CustomerSubnet != "10.5.50.0/24" || len(c.PingTargets) != 2 || c.ReminderDays != 3 || !c.ReminderOnExpiry {
		t.Fatalf("%+v", c)
	}
	c, err = Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "1", "FUP_TB": "7.5", "REMINDER_DAYS": "5", "REMINDER_ON_EXPIRY": "false", "QOE_PING_TARGETS": "9.9.9.9"}))
	if err != nil || c.FUPTB != 7.5 || c.ReminderDays != 5 || c.ReminderOnExpiry || len(c.PingTargets) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "1", "FUP_TB": "-1"})); err == nil {
		t.Fatal("negative FUP must be refused")
	}
}

func TestNotificationSettings(t *testing.T) {
	base := map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "1"}
	c, err := Load(env(base))
	if err != nil || c.SMTPPort != 587 || c.WADailyLimit != 240 || c.WAMinConfirmKES != 250 || c.WALang != "en" || c.WAAPIVersion == "" ||
		c.EmailConfigured() || c.WhatsAppConfigured() {
		t.Fatalf("%+v %v", c, err)
	}
	m := map[string]string{"SMTP_HOST": "smtp.x", "SMTP_FROM": "a@x", "SMTP_PORT": "465", "ALERT_EMAIL_TO": "a@x, b@x",
		"WA_TOKEN": "t", "WA_PHONE_NUMBER_ID": "99",
		"WA_ADMIN_NUMBERS": "0712345678,+254 722 111 222", "WA_DAILY_LIMIT": "100", "WA_MIN_CONFIRM_KES": "0"}
	for k, v := range base {
		m[k] = v
	}
	c, err = Load(env(m))
	if err != nil || !c.EmailConfigured() || !c.WhatsAppConfigured() || c.SMTPPort != 465 ||
		len(c.AlertEmailTo) != 2 || len(c.WAAdminNumbers) != 2 || c.WAAdminNumbers[0] != "254712345678" || c.WAAdminNumbers[1] != "254722111222" ||
		c.WADailyLimit != 100 || c.WAMinConfirmKES != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	m["WA_ADMIN_NUMBERS"] = "abc"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("bad admin number must be refused")
	}
	m["WA_ADMIN_NUMBERS"], m["SMTP_PORT"] = "", "x"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("bad SMTP_PORT must be refused")
	}
}

func TestDatabaseSettings(t *testing.T) {
	base := map[string]string{"ADMIN_PASS": "s3cret!"}
	c, err := Load(env(base))
	if err != nil || c.DatabaseURL != "" || c.DataKey != nil || c.DBCommitTimeout != 5*time.Second {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	key := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes
	c, err = Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "DATABASE_URL": "postgres://u:p@/db?host=/var/run/postgresql", "DATA_KEY": key, "DB_COMMIT_TIMEOUT": "9s"}))
	if err != nil || c.DatabaseURL == "" || len(c.DataKey) != 32 || c.DBCommitTimeout != 9*time.Second {
		t.Fatalf("set: %+v %v", c, err)
	}
	for _, bad := range []string{"not base64!", "c2hvcnQ="} { // not base64; 5 bytes
		if _, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "DATA_KEY": bad})); err == nil {
			t.Errorf("DATA_KEY=%q must be refused", bad)
		}
	}
}

func TestNormalizeWANumber(t *testing.T) {
	for in, want := range map[string]string{"0712345678": "254712345678", "+254 722-111 222": "254722111222", "254700000000": "254700000000"} {
		if got, err := NormalizeWANumber(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "12345", "0712", "+25471234567890123"} {
		if _, err := NormalizeWANumber(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// Old environment files still carry ADMIN_ALLOW_CIDR; it is ignored now (the
// admin pages have their own sign-in), not refused.
func TestOldAdminAllowCIDRIsIgnored(t *testing.T) {
	for _, v := range []string{"10.98.0.0/24", "not a cidr", ""} {
		if _, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ADMIN_ALLOW_CIDR": v, "ROUTER_MOCK": "true"})); err != nil {
			t.Errorf("ADMIN_ALLOW_CIDR=%q: %v", v, err)
		}
	}
}

func TestMgmtSubnet(t *testing.T) {
	base := map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "true"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := Load(env(with()))
	if err != nil || c.MgmtSubnet != "10.98.0.0/24" {
		t.Fatalf("default: %v %q", err, c.MgmtSubnet)
	}
	if c, err = Load(env(with("MGMT_SUBNET", "172.20.9.0/24"))); err != nil || c.MgmtSubnet != "172.20.9.0/24" {
		t.Fatalf("custom: %v", err)
	}
	for _, bad := range []string{"8.8.8.0/24", "10.98.0.0/8", "10.98.0.0/30", "nope", "10.99.1.0/24"} { // the last overlaps WG_SUBNET
		if _, err := Load(env(with("MGMT_SUBNET", bad))); err == nil {
			t.Errorf("MGMT_SUBNET=%q must be refused", bad)
		}
	}
}
