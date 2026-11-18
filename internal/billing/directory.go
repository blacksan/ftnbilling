package billing

import (
	"ispbill/internal/metrics"
	"ispbill/internal/store"
)

type dir struct{ s *Service }

// Directory lets the metrics collector see the flats.
func (s *Service) Directory() metrics.Directory { return dir{s} }

func (d dir) Accounts() []metrics.AcctInfo {
	var out []metrics.AcctInfo
	now := d.s.now()
	d.s.st.View(func(data *store.Data) {
		for _, a := range data.AccountList() {
			pk := data.Packages[a.PackageID]
			out = append(out, metrics.AcctInfo{Code: a.Code, Flat: a.Flat, Package: pk.Name, SpeedMbps: pk.SpeedMbps, State: AccountState(a, now)})
		}
	})
	return out
}
