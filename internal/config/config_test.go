package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAdminPassRules(t *testing.T) {
	for _, pw := range []string{"", "admin"} {
		if _, err := Load(env(map[string]string{"ADMIN_PASS": pw, "ROUTER_MOCK": "true"})); err == nil {
			t.Errorf("ADMIN_PASS=%q must be refused", pw)
		}
	}
	if _, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_MOCK": "true"})); err != nil {
		t.Fatal(err)
	}
}

func TestRouterPassNoLongerRequired(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!"}))
	if err != nil {
		t.Fatalf("a fresh multi-building server has no ROUTER_PASS: %v", err)
	}
	if c.RouterURLSet {
		t.Fatal("ROUTER_URL was not set")
	}
	c, _ = Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "ROUTER_URL": "http://172.17.0.1:8081/"}))
	if !c.RouterURLSet || c.RouterURL != "http://172.17.0.1:8081" {
		t.Fatalf("%+v", c)
	}
}

func TestWireGuardSettings(t *testing.T) {
	c, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "PUBLIC_IP": "203.0.113.5"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.WGSubnet != "10.99.0.0/22" || c.WGEndpoint != "203.0.113.5:51820" || c.JoinTokenTTL != 30*time.Minute ||
		c.AllowInsecureJoin || !c.JoinRequireMikrotikUA || c.SiteName != "" {
		t.Fatalf("%+v", c)
	}
	c, err = Load(env(map[string]string{"ADMIN_PASS": "s3cret!", "WG_ENDPOINT": "198.51.100.1:51999", "JOIN_TOKEN_TTL": "10m",
		"ALLOW_INSECURE_JOIN": "true", "JOIN_REQUIRE_MIKROTIK_UA": "false", "SITE_NAME": "Block A",
		"WG_PUBLIC_KEY": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}))
	if err != nil || c.WGEndpoint != "198.51.100.1:51999" || c.JoinTokenTTL != 10*time.Minute || !c.AllowInsecureJoin ||
		c.JoinRequireMikrotikUA || c.SiteName != "Block A" {
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
