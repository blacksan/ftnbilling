package web_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCaptiveShotsServe is not a test: with CAPTIVE_SHOTS_DIR set it seeds the captive-page story (a router, a flat
// whose plan just ended, a fake Daraja) and serves it until <dir>/done appears, so a browser script can drive the
// pages (screenshots). Without the variable it is skipped.
func TestCaptiveShotsServe(t *testing.T) {
	dir := os.Getenv("CAPTIVE_SHOTS_DIR")
	if dir == "" {
		t.Skip("set CAPTIVE_SHOTS_DIR to serve the captive pages to a browser script")
	}
	c := newCaptive(t, true)
	info := map[string]any{"url": c.ts.URL, "code": c.acct.Code, "flat": "Flat 4B", "wan_ip": capWANIP, "token": capToken,
		"day_price": priceOf(c, "1 Day"), "day_package": c.pkg("1 Day"), "checkout": "ws_CO_CAPTIVE"}
	b, _ := json.Marshal(info)
	os.WriteFile(filepath.Join(dir, "session.json"), b, 0o644)
	for i := 0; i < 1200; i++ {
		if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
}
