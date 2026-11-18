package main

import (
	"testing"

	"ispbill/internal/config"
	"ispbill/internal/notify"
)

func TestWhatsAppPauseEndsWhenANewTokenIsSaved(t *testing.T) {
	var resumed []string
	f := resumeOnNewWAToken(func(ch string) { resumed = append(resumed, ch) })
	wa := func(tok, id string) config.Config { return config.Config{WAToken: tok, WAPhoneNumberID: id} }
	f(wa("old-token-123456", "111111")) // start-up: nothing to resume
	if len(resumed) != 0 {
		t.Fatalf("resumed at start-up: %v", resumed)
	}
	f(wa("old-token-123456", "111111")) // another save, same credentials
	f(config.Config{WAToken: "old-token-123456"})
	if len(resumed) != 0 {
		t.Fatalf("resumed without a change: %v", resumed)
	}
	f(wa("new-token-654321", "111111"))
	if len(resumed) != 1 || resumed[0] != notify.ChanWhatsApp {
		t.Fatalf("new token did not resume WhatsApp: %v", resumed)
	}
	f(wa("new-token-654321", "222222")) // a new phone number ID is a fix too
	if len(resumed) != 2 {
		t.Fatalf("new phone number ID: %v", resumed)
	}
	f(wa("", "")) // removing the credentials resumes nothing
	if len(resumed) != 2 {
		t.Fatalf("cleared credentials: %v", resumed)
	}
}
