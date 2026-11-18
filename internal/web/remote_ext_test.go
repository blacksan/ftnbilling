package web_test

import (
	"strings"
	"testing"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/config"
)

func remoteApp(t *testing.T) *app {
	return newAppCfg(t, true, func(c *config.Config) {
		c.PublicBaseURL = "https://billing.example.co.ke"
		c.PublicIP = "203.0.113.10"
		c.VerifyHotspotHost = true
	})
}

func TestRouterFiles(t *testing.T) {
	a := remoteApp(t)
	code, b := a.portal(t, "/routeros/hotspot/login.html")
	if code != 200 || !strings.Contains(b, "https://billing.example.co.ke/portal?site="+a.site+"&mac=$(mac-esc)") || strings.Contains(b, "172.17.0.2") {
		t.Fatalf("login.html %d %s", code, b)
	}
	code, b = a.portal(t, "/routeros/remote.rsc")
	if code != 200 || !strings.Contains(b, "dst-address=203.0.113.10") || strings.Contains(b, "{{") {
		t.Fatalf("remote.rsc %d", code)
	}
	if code, _ = a.portal(t, "/routeros/hotspot/setup.rsc"); code != 404 {
		t.Fatalf("unknown file got %d", code)
	}
}

func TestPublicPortalNeedsHotspotHost(t *testing.T) {
	a := remoteApp(t)
	acct, err := a.svc.CreateAccount(billing.AccountInput{SiteID: a.site, Flat: "Flat 9Z", MAC: "AA:BB:CC:DD:EE:09"})
	if err != nil {
		t.Fatal(err)
	}
	// From the internet: the MAC is not on the hotspot, so no account shown.
	_, b := a.portal(t, "/portal?mac=AA:BB:CC:DD:EE:09&ip=10.5.50.29")
	if strings.Contains(b, acct.Code) || strings.Contains(b, "Flat 9Z") {
		t.Fatal("public portal leaked an account for a MAC not on the hotspot")
	}
	// The same device on the building hotspot.
	a.rt.AddHost("AA:BB:CC:DD:EE:09", "10.5.50.29")
	time.Sleep(1100 * time.Millisecond) // the guard refreshes the host table at most once a second
	_, b = a.portal(t, "/portal?mac=AA:BB:CC:DD:EE:09&ip=10.5.50.29")
	if !strings.Contains(b, "Flat 9Z") {
		t.Fatal("hotspot device not recognised")
	}
}
