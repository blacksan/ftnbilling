package alerts

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeStatus(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupProblem(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	p := filepath.Join(dir, "backup-status.json")
	if BackupProblem(p, now) != "" {
		t.Fatal("a missing status file is not a problem (no backup installed)")
	}
	fresh := now.Add(-3 * time.Hour).Format(time.RFC3339)
	old := now.Add(-40 * time.Hour).Format(time.RFC3339)
	for _, c := range []struct{ name, body, want string }{
		{"healthy", `{"dump_at":"` + fresh + `","drill_ok":true,"upload_ok":true}`, ""},
		{"no drill yet", `{"dump_at":"` + fresh + `"}`, ""},
		{"stale", `{"dump_at":"` + old + `"}`, "40 hours old"},
		{"never dumped", `{"dump_at":"0001-01-01T00:00:00Z"}`, "no database dump"},
		{"error", `{"dump_at":"` + fresh + `","error":"pg_dump failed"}`, "pg_dump failed"},
		{"drill failed", `{"dump_at":"` + fresh + `","drill_ok":false,"drill_msg":"table missing"}`, "table missing"},
		{"upload failed", `{"dump_at":"` + fresh + `","upload_ok":false}`, "upload"},
		{"garbage", `{not json`, "cannot be read"},
	} {
		writeStatus(t, p, c.body)
		got := BackupProblem(p, now)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestBackupAlertFiresNotifiesAndRecovers(t *testing.T) {
	r := newRig(t, 1)
	problem := ""
	r.e = New(Options{Path: filepath.Join(t.TempDir(), "alerts.json"), In: r.in, Out: r.out, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return r.now }, Backup: func(time.Time) string { return problem },
		Targets: func() Targets { return Targets{Email: []string{"a@x.example"}} }})
	r.step(time.Minute)
	if len(r.e.Active()) != 0 {
		t.Fatal("alert without a problem")
	}
	problem = "the last database dump is 40 hours old"
	r.step(time.Minute)
	act := r.e.Active()
	if len(act) != 1 || act[0].Rule != RuleBackup || act[0].State != Firing || act[0].SiteID != "" {
		t.Fatalf("active: %+v", act)
	}
	texts := r.out.texts("email")
	if len(texts) != 1 || !strings.Contains(texts[0], "Backup problem") || !strings.Contains(texts[0], ServerName) {
		t.Fatalf("messages: %q", texts)
	}
	// It is not re-sent on every pass.
	r.step(time.Minute)
	if n := len(r.out.texts("email")); n != 1 {
		t.Fatalf("sent %d messages", n)
	}
	problem = ""
	r.step(time.Minute)
	if len(r.e.Active()) != 0 {
		t.Fatal("alert did not clear")
	}
	if n := len(r.out.texts("email")); n != 2 || !strings.Contains(r.out.texts("email")[1], "RECOVERED") {
		t.Fatalf("recovery message missing: %q", r.out.texts("email"))
	}
	// The rule is server-wide: it is not an editable per-building rule.
	if _, ok := DefaultSettings().Rules[RuleBackup]; ok {
		t.Fatal("backup rule should not be in the editable rules")
	}
}
