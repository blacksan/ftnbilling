package sites

import "testing"

func TestRouterUserAgent(t *testing.T) {
	for ua, want := range map[string]bool{
		"RouterOS 7.20":            true, // what RouterOS 7 /tool fetch sends
		"RouterOS 7.20.1 (stable)": true,
		"Mikrotik/6.49 Fetch":      true,
		"mikrotik":                 true,
		"":                         false,
		"curl/8.5.0":               false,
		"Mozilla/5.0 WhatsApp":     false,
		"Slackbot-LinkExpanding":   false,
	} {
		if got := routerUserAgent(ua); got != want {
			t.Errorf("routerUserAgent(%q) = %v, want %v", ua, got, want)
		}
	}
}
