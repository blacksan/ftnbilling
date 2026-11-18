package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"ispbill/internal/authn"
	"ispbill/internal/config"
	"ispbill/internal/settings"
	"ispbill/internal/store"
)

func TestSeedOwnerFromEnvAndSetting(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fast, _ := settings.HashPasswordCost("old-ui-password-1", 1000)
	for name, tc := range map[string]struct {
		setting, envPass, want string
		wantErr                bool
	}{
		"setting wins": {fast, "env-password-123", "old-ui-password-1", false},
		"env":          {"", "env-password-123", "env-password-123", false},
		"nothing":      {"", "admin", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if tc.setting != "" {
				st.Update(func(d *store.Data) error {
					d.Settings["admin.pass_hash"] = store.Setting{Key: "admin.pass_hash", Value: tc.setting, UpdatedAt: time.Now()}
					return nil
				})
			}
			au, _ := authn.New(st, []byte("0123456789abcdef0123456789abcdef"), "t")
			cfg := config.Config{AdminUser: "boss", AdminPass: tc.envPass}
			err = seedOwner(au, st, cfg, log)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v", err)
			}
			if tc.wantErr {
				return
			}
			u, ok := au.Owner()
			if !ok || u.Username != "boss" || !settings.VerifyPassword(u.PassHash, tc.want) || authn.Enrolled(u) {
				t.Fatalf("owner: %+v", u)
			}
			if strings.Contains(u.PassHash, tc.want) {
				t.Fatal("stored in clear")
			}
			// Second boot: nothing changes, even if the environment password did.
			cfg.AdminPass = "changed-later-123"
			if err := seedOwner(au, st, cfg, log); err != nil {
				t.Fatal(err)
			}
			if u2, _ := au.Owner(); u2.PassHash != u.PassHash {
				t.Fatal("the owner was reseeded")
			}
		})
	}
}
