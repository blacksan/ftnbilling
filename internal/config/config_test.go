package config

import "testing"

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

func TestRouterPassRequiredUnlessMock(t *testing.T) {
	if _, err := Load(env(map[string]string{"ADMIN_PASS": "s3cret!"})); err == nil {
		t.Fatal("ROUTER_PASS required")
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
