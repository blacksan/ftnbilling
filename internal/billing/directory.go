package billing

import (
	"ispbill/internal/metrics"
	"ispbill/internal/store"
)

type dir struct {
	s    *Service
	site string
}

// Directory lets a building's metrics collector see that building's flats.
func (s *Service) Directory(siteID string) metrics.Directory { return dir{s, siteID} }

func (d dir) Accounts() []metrics.AcctInfo {
	var out []metrics.AcctInfo
	now := d.s.now()
	d.s.st.View(func(data *store.Data) {
		for _, a := range data.AccountList() {
			if a.SiteID != d.site {
				continue
			}
			pk := data.Packages[a.PackageID]
			out = append(out, metrics.AcctInfo{Code: a.Code, Flat: a.Flat, Package: pk.Name, SpeedMbps: pk.SpeedMbps, State: AccountState(a, now)})
		}
	})
	return out
}
