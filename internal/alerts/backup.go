package alerts

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// BackupMaxAge is how old the last dump may be before the backup alert fires.
const BackupMaxAge = 36 * time.Hour

// BackupProblem reads the status file the backup script writes and returns
// what is wrong, or "" when all is well (or when there is no status file: the
// backup is not installed on this server, so there is nothing to watch).
func BackupProblem(path string, now time.Time) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var b struct {
		DumpAt   time.Time `json:"dump_at"`
		UploadAt time.Time `json:"upload_at"`
		DrillOK  *bool     `json:"drill_ok"`
		DrillMsg string    `json:"drill_msg"`
		UploadOK *bool     `json:"upload_ok"`
		Error    string    `json:"error"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return "the backup status file cannot be read: " + err.Error()
	}
	switch {
	case b.Error != "":
		return "the last backup reported an error: " + b.Error
	case b.DumpAt.IsZero() && !b.UploadAt.IsZero():
		// A server on the JSON store has no database dump; its backup is the weekly Drive upload.
		if b.UploadOK != nil && !*b.UploadOK {
			return "the last upload to Google Drive failed"
		}
	case b.DumpAt.IsZero():
		return "no database dump has been made yet"
	case now.Sub(b.DumpAt) > BackupMaxAge:
		return fmt.Sprintf("the last database dump is %d hours old", int(now.Sub(b.DumpAt).Hours()))
	case b.DrillOK != nil && !*b.DrillOK:
		return "the last restore drill failed: " + b.DrillMsg
	case b.UploadOK != nil && !*b.UploadOK:
		return "the last upload to Google Drive failed"
	}
	return ""
}
