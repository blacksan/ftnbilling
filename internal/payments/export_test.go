package payments

import "time"

// SetLegacyBuildTime overrides the build time the legacy-window fallback derives from (tests).
func SetLegacyBuildTime(t time.Time) func() {
	old := legacyBuildTime
	legacyBuildTime = func() time.Time { return t }
	return func() { legacyBuildTime = old }
}
