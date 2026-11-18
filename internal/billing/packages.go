package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// SeedPackages creates the default packages if there are none.
func (s *Service) SeedPackages() error {
	return s.st.Update(func(d *store.Data) error {
		if len(d.Packages) > 0 {
			return store.ErrNoChange
		}
		defaults := []store.Package{
			{Name: "1 Hour", DurationMin: 60, SpeedMbps: 5, PriceKES: 10},
			{Name: "1 Day", DurationMin: 1440, SpeedMbps: 10, PriceKES: 50},
			{Name: "1 Week", DurationMin: 10080, SpeedMbps: 10, PriceKES: 250},
			{Name: "Monthly 20 Mbps", DurationMin: 43200, SpeedMbps: 20, PriceKES: 2000},
			{Name: "Monthly 40 Mbps", DurationMin: 43200, SpeedMbps: 40, PriceKES: 3000},
			{Name: "Monthly 80 Mbps", DurationMin: 43200, SpeedMbps: 80, PriceKES: 4000},
		}
		for i, p := range defaults {
			p.ID, p.Slug, p.Sort = RandomID(), Slugify(p.Name), (i+1)*10
			d.Packages[p.ID] = p
		}
		return nil
	})
}

// SyncProfiles creates/updates one router profile per package.
func (s *Service) SyncProfiles(ctx context.Context) error {
	var pkgs []store.Package
	s.st.View(func(d *store.Data) { pkgs = d.PackageList() })
	s.profilesOK.Store(false)
	for _, p := range pkgs {
		if err := s.rt.EnsureProfile(ctx, routeros.Profile{Name: p.Profile(), RateLimit: p.RateLimit(), SharedUsers: 1}); err != nil {
			return fmt.Errorf("profile %s: %w", p.Profile(), err)
		}
	}
	s.profilesOK.Store(true)
	return nil
}

// SavePackage adds (ID empty) or edits a package and pushes its profile to the
// router. The slug, and so the router profile name, never changes after creation.
func (s *Service) SavePackage(ctx context.Context, p store.Package) (store.Package, error) {
	if p.Name == "" || p.DurationMin <= 0 || p.SpeedMbps <= 0 || p.PriceKES < 0 {
		return p, errors.New("name, duration, speed must be set and price cannot be negative")
	}
	err := s.st.Update(func(d *store.Data) error {
		if p.ID == "" {
			p.ID = RandomID()
			base, slug := Slugify(p.Name), ""
			for i := 1; ; i++ {
				slug = base
				if i > 1 {
					slug = fmt.Sprintf("%s-%d", base, i)
				}
				clash := false
				for _, o := range d.Packages {
					clash = clash || o.Slug == slug
				}
				if !clash {
					break
				}
			}
			p.Slug = slug
			p.Sort = (len(d.Packages) + 1) * 10
		} else {
			old, ok := d.Packages[p.ID]
			if !ok || old.Deleted {
				return ErrNotFound
			}
			p.Slug, p.Sort = old.Slug, old.Sort
		}
		d.Packages[p.ID] = p
		return nil
	})
	if err != nil {
		return p, err
	}
	if err := s.rt.EnsureProfile(ctx, routeros.Profile{Name: p.Profile(), RateLimit: p.RateLimit(), SharedUsers: 1}); err != nil {
		s.profilesOK.Store(false) // sweeper will retry
		s.log.Warn("profile push failed; will retry", "profile", p.Profile(), "err", err)
	}
	return p, nil
}

// ---- vouchers -----------------------------------------------------------

// GenerateVouchers makes n unique 8-char single-use vouchers; returns the batch id.
func (s *Service) GenerateVouchers(pkgID string, n int) (string, []store.Voucher, error) {
	if n < 1 || n > 500 {
		return "", nil, errors.New("count must be between 1 and 500")
	}
	var out []store.Voucher
	batch := s.now().In(EAT).Format("20060102-1504") + "-" + RandomCode(3)
	err := s.st.Update(func(d *store.Data) error {
		if p, ok := d.Packages[pkgID]; !ok || p.Deleted {
			return ErrNotFound
		}
		now := s.now()
		for len(out) < n {
			c := RandomCode(8)
			if _, dup := d.Vouchers[c]; dup {
				continue
			}
			v := store.Voucher{Code: c, PackageID: pkgID, Batch: batch, CreatedAt: now}
			d.Vouchers[c] = v
			out = append(out, v)
		}
		return nil
	})
	return batch, out, err
}

// LookupVoucher returns an unused voucher.
func (s *Service) LookupVoucher(code string) (v store.Voucher, err error) {
	s.st.View(func(d *store.Data) {
		var ok bool
		v, ok = d.Vouchers[NormalizeCode(code)]
		if !ok || v.Used {
			err = ErrVoucherUsed
		}
	})
	return
}

// Remaining formats the time left until t as "2 h 5 min".
func Remaining(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "expired"
	}
	m := int(d.Minutes())
	switch {
	case m >= 1440:
		return fmt.Sprintf("%d d %d h", m/1440, (m%1440)/60)
	case m >= 60:
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	return fmt.Sprintf("%d min", m+1)
}
