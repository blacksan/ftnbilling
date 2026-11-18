package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// migrateV2 upgrades a format 2 (single building) file to format 3 in place.
// It first copies the original to <path>.v2.bak (mode 0600). If the file holds
// any accounts or payments, one env-mode site called "Main building" is
// created and stamped on every row; vouchers keep SiteID "" (valid anywhere).
func migrateV2(path string, raw []byte, d *Data) error {
	if err := WriteFileAtomic(path+".v2.bak", raw); err != nil {
		return fmt.Errorf("could not back up %s before migrating: %w", path, err)
	}
	*d = Data{}
	if err := json.Unmarshal(raw, d); err != nil {
		return fmt.Errorf("corrupt %s: %w", path, err)
	}
	d.init()
	d.migrateRows(time.Now())
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(path, b); err != nil {
		return fmt.Errorf("could not save migrated %s: %w", path, err)
	}
	return nil
}

// migrateRows is the pure part of the v2 -> v3 migration.
func (d *Data) migrateRows(now time.Time) {
	d.Format = FormatVersion
	if len(d.Accounts) == 0 && len(d.Payments) == 0 {
		return
	}
	site := Site{ID: NewSiteID(), Name: "Main building", Mode: SiteModeEnv, HotspotServer: "hotspot1",
		LegacyContainer: true, CreatedAt: now}
	d.Sites[site.ID] = site
	for k, a := range d.Accounts {
		a.SiteID = site.ID
		d.Accounts[k] = a
	}
	for k, o := range d.Orders {
		o.SiteID = site.ID
		d.Orders[k] = o
	}
	for k, p := range d.Payments {
		p.SiteID = site.ID
		d.Payments[k] = p
	}
	for k, t := range d.Tasks {
		t.SiteID = site.ID
		d.Tasks[k] = t
	}
	for i := range d.Events {
		d.Events[i].SiteID = site.ID
	}
	devs := make(map[string]Device, len(d.Devices))
	for mac, dev := range d.Devices {
		if dev.MAC == "" {
			dev.MAC = mac
		}
		dev.SiteID = site.ID
		devs[DeviceKey(site.ID, dev.MAC)] = dev
	}
	d.Devices = devs
}
