package alerts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The file exactly as ftnbilling-backup writes it after the first weekly Drive upload on a
// server without PostgreSQL (see deploy/p5_platform_test.go, TestP5JSONServerStatusFileHasNoDumpAt).
func TestP5BackupProblemOnJSONServerAfterSuccessfulUpload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "backup-status.json")
	os.WriteFile(p, []byte("{\n  \"upload_at\": \"2026-11-22T23:30:00Z\",\n  \"upload_ok\": true\n}\n"), 0o640)
	if msg := BackupProblem(p, time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)); msg != "" {
		t.Fatalf("a healthy JSON-store server raises a Backup problem alert: %q", msg)
	}
}
