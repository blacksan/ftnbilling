package sites

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"ispbill/internal/config"
	"ispbill/internal/store"
)

// DefaultEnvSiteName is what the migrated or seeded legacy building is called.
const DefaultEnvSiteName = "Main building"

// Bootstrap reconciles the store with the environment at start-up and returns
// the env-mode building's ID ("" if there is none):
//
//   - Fresh data with ROUTER_URL or ROUTER_MOCK set (container mode) gets a
//     seeded env-mode building, so the on-router container works with no admin action.
//   - Fresh data without them (a new VPS) starts with no buildings.
//   - SITE_NAME renames the env building once, while it is still "Main building".
//   - The old single DATA/metrics.json moves to DATA/metrics/<envSiteID>.json,
//     unless DATA/metrics/ already exists.
//   - It fails only when an env building exists, ROUTER_MOCK is off and
//     ROUTER_PASS is empty.
func Bootstrap(cfg config.Config, st *store.Store, log *slog.Logger) (string, error) {
	var envID string
	err := st.Update(func(d *store.Data) error {
		changed := false
		if len(d.Sites) == 0 && (cfg.RouterURLSet || cfg.RouterMock) {
			name := DefaultEnvSiteName
			if cfg.SiteName != "" {
				name = cfg.SiteName
			}
			id := store.NewSiteID()
			d.Sites[id] = store.Site{ID: id, Name: name, Mode: store.SiteModeEnv, HotspotServer: "hotspot1", LegacyContainer: true, CreatedAt: nowFn()}
			changed = true
			log.Info("container mode: created the env-mode building", "site", id, "name", name)
		}
		if s, ok := d.EnvSite(); ok {
			envID = s.ID
			if cfg.SiteName != "" && s.Name == DefaultEnvSiteName && cfg.SiteName != s.Name {
				s.Name = cfg.SiteName
				d.Sites[s.ID] = s
				changed = true
			}
		}
		if !changed {
			return store.ErrNoChange
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if envID == "" {
		return "", nil
	}
	if !cfg.RouterMock && cfg.RouterPass == "" {
		return envID, errors.New("ROUTER_PASS is required: this server still has a legacy building that uses the router settings from the environment (set ROUTER_PASS, or ROUTER_MOCK=true)")
	}
	old := filepath.Join(cfg.DataDir, "metrics.json")
	dir := filepath.Join(cfg.DataDir, "metrics")
	if _, err := os.Stat(old); err == nil {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return envID, err
			}
			if err := os.Rename(old, MetricsPath(cfg.DataDir, envID)); err != nil {
				return envID, err
			}
			log.Info("moved metrics.json to the legacy building's own metrics file", "site", envID)
		}
	}
	return envID, nil
}
