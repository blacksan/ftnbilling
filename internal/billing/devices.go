package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ispbill/internal/netplan"
	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

// Section H: the operator must see every MAC that is connected but not
// configured and attach it to an account in one tap. In an IPoE building the
// reconciler's pass reads the DHCP leases and the ARP table and records every
// MAC that has no open account here (NoteDevices); the pages and the API list
// them, online first, with a suggested account when a flat's own MAC has
// gone quiet while a new one appeared ("possible ONT swap").

const (
	// SourceDismissed marks a device the operator dismissed: it stays in the
	// table (so it does not come back every minute) but is not listed.
	SourceDismissed = "dismissed"
	// staleAfter: an account whose MAC was not seen for this long is a
	// candidate for "possible ONT swap / wrong MAC".
	staleAfter = 24 * time.Hour
	// OnlineWindow: a device only counts as online while it was seen this recently.
	OnlineWindow = 3 * time.Minute
)

type devObs struct {
	mac, ip, host, source string
	online                bool
}

// arpOnline: does an /ip/arp row mean the device answered recently?
func arpOnline(r routeros.Row) bool {
	switch r["status"] {
	case "":
		return r["complete"] != "false" && r["invalid"] != "true"
	case "reachable", "delay", "probe", "permanent":
		return true
	}
	return false // stale, failed, incomplete
}

// collectDevices reads the unknown MACs out of the lease and ARP rows of one
// pass, and refreshes "last seen" of the flats' own MACs. It writes nothing to
// the router.
func (s *Service) collectDevices(site store.Site, pl netplan.Plan, wants []wantAcct, leases, arp []routeros.Row, ps *provState) {
	now := s.now()
	byMAC := map[string]*devObs{}
	order := []string{}
	upsert := func(mac, ip, host, source string, online bool) {
		m, err := NormalizeMAC(mac)
		if err != nil {
			return
		}
		if ip != "" && !pl.InNetwork(ip) {
			return
		}
		o := byMAC[m]
		if o == nil {
			o = &devObs{mac: m, source: source}
			byMAC[m] = o
			order = append(order, m)
		}
		if ip != "" && (o.ip == "" || source == "dhcp") {
			o.ip = ip
		}
		if host != "" {
			if len(host) > 64 {
				host = host[:64]
			}
			o.host = host
		}
		if source == "dhcp" {
			o.source = "dhcp"
		}
		o.online = o.online || online
	}
	for _, r := range leases {
		if isOurs(r) {
			continue
		}
		upsert(r["mac-address"], firstNonEmpty(r["active-address"], r["address"]), r["host-name"], "dhcp", r["status"] == "bound")
	}
	for _, r := range arp {
		if isOurs(r) {
			continue // our static entry says nothing about whether the flat is there
		}
		upsert(r["mac-address"], r["address"], "", "arp", arpOnline(r))
	}
	// Flats that were seen right now.
	ps.mu.Lock()
	for _, w := range wants {
		if o := byMAC[strings.ToUpper(w.a.MAC)]; o != nil && o.online {
			ps.seen[w.a.Code] = now
		}
	}
	ps.mu.Unlock()
	obs := make([]devObs, 0, len(order))
	for _, m := range order {
		obs = append(obs, *byMAC[m])
	}
	s.noteDevices(site.ID, obs)
}

// noteDevices records the observed MACs that have no open account, and marks
// the building's other known devices offline. It is one store update.
func (s *Service) noteDevices(siteID string, obs []devObs) {
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		if st, ok := d.Sites[siteID]; !ok || st.Removed {
			return store.ErrNoChange
		}
		changed := false
		seen := map[string]bool{}
		for _, o := range obs {
			if len(d.OpenAccountsByMAC(o.mac)) > 0 {
				continue // already somebody's ONT (here or in another building)
			}
			key := store.DeviceKey(siteID, o.mac)
			seen[key] = true
			dev, had := d.Devices[key]
			if !had {
				dev = store.Device{SiteID: siteID, MAC: o.mac, FirstSeen: now}
			}
			if !had || (o.online && !dev.Online) {
				dev.Hits++
			}
			dev.LastSeen, dev.Online = now, o.online
			if o.ip != "" {
				dev.IP = o.ip
			}
			if o.host != "" {
				dev.HostName = o.host
			}
			dev.Vendor = Vendor(o.mac)
			if dev.Source == "" || dev.Source == "hotspot-host" || dev.Source == "dhcp" || dev.Source == "arp" {
				dev.Source = o.source // portal and dismissed are kept
			}
			d.Devices[key] = dev
			changed = true
		}
		for key, dev := range d.Devices {
			if dev.SiteID == siteID && !seen[key] && dev.Online {
				dev.Online = false
				d.Devices[key] = dev
				changed = true
			}
		}
		for len(d.Devices) > 500 { // bounded: drop the stalest
			var oldest string
			for m, v := range d.Devices {
				if oldest == "" || v.LastSeen.Before(d.Devices[oldest].LastSeen) {
					oldest = m
				}
			}
			delete(d.Devices, oldest)
			changed = true
		}
		if !changed {
			return store.ErrNoChange
		}
		return nil
	})
}

// SuggestedAccount is the flat a device probably belongs to.
type SuggestedAccount struct {
	ID, Code, Flat, MAC string
	Reason              string
}

// UnregDevice is one row of the unregistered devices list.
type UnregDevice struct {
	store.Device
	SiteName  string
	Online    bool // seen within the last few minutes
	Suggested *SuggestedAccount
}

// Unregistered lists the devices that have no account, online first, then by
// last seen. siteID "" means every building. Dismissed devices are not listed.
func (s *Service) Unregistered(siteID string, onlineOnly bool) []UnregDevice {
	now := s.now()
	var out []UnregDevice
	var devs []store.Device
	names := map[string]string{}
	s.st.View(func(d *store.Data) {
		for _, dv := range d.DeviceList() {
			if (siteID == "" || dv.SiteID == siteID) && dv.Source != SourceDismissed {
				devs = append(devs, dv)
				names[dv.SiteID] = d.Sites[dv.SiteID].Name
			}
		}
	})
	for _, dv := range devs {
		online := dv.Online && now.Sub(dv.LastSeen) <= OnlineWindow
		if onlineOnly && !online {
			continue
		}
		if dv.Vendor == "" {
			dv.Vendor = Vendor(dv.MAC)
		}
		out = append(out, UnregDevice{Device: dv, SiteName: names[dv.SiteID], Online: online})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	s.suggest(out)
	return out
}

// UnregisteredOnline counts the online unregistered devices of a building
// (siteID "" = all): the sidebar and dashboard badge.
func (s *Service) UnregisteredOnline(has func(siteID string) bool) int {
	n := 0
	now := s.now()
	s.st.View(func(d *store.Data) {
		for _, dv := range d.Devices {
			if dv.Source != SourceDismissed && dv.Online && now.Sub(dv.LastSeen) <= OnlineWindow && (has == nil || has(dv.SiteID)) {
				n++
			}
		}
	})
	return n
}

// staleAccounts lists the flats of an IPoE building whose MAC has not been
// seen for a day (or that are flagged and need a MAC): where a new device
// most likely belongs.
func (s *Service) staleAccounts(siteID string, now time.Time) []store.Account {
	site, ok := s.site(siteID)
	if !ok || !IPoE(site) {
		return nil
	}
	ps := s.prov(siteID)
	ps.mu.Lock()
	started := ps.started
	seen := make(map[string]time.Time, len(ps.seen))
	for k, v := range ps.seen {
		seen[k] = v
	}
	ps.mu.Unlock()
	if started.IsZero() {
		return nil // no pass yet: nothing is known about the building
	}
	var out []store.Account
	s.st.View(func(d *store.Data) {
		for _, a := range d.AccountList() {
			if a.SiteID != siteID {
				continue
			}
			if a.MACConflict {
				out = append(out, a)
				continue
			}
			last := seen[a.Code]
			base := a.CreatedAt
			if started.After(base) {
				base = started
			}
			if base.After(last) {
				last = base
			}
			if now.Sub(last) > staleAfter {
				out = append(out, a)
			}
		}
	})
	return out
}

// suggest fills Suggested on the online devices where the match is clear.
func (s *Service) suggest(list []UnregDevice) {
	now := s.now()
	perSite := map[string][]int{} // site -> indexes of online devices
	for i, u := range list {
		if u.Online {
			perSite[u.SiteID] = append(perSite[u.SiteID], i)
		}
	}
	for siteID, idx := range perSite {
		stale := s.staleAccounts(siteID, now)
		if len(stale) == 0 {
			continue
		}
		vendorCount := map[string]int{}
		for _, i := range idx {
			if v := realVendor(list[i].Vendor); v != "" {
				vendorCount[v]++
			}
		}
		for _, i := range idx {
			dv := list[i]
			var pick *store.Account
			reason := ""
			if len(stale) == 1 && len(idx) == 1 {
				pick, reason = &stale[0], "its MAC was not seen for a day and this is the only new device"
			} else if v := realVendor(dv.Vendor); v != "" && vendorCount[v] == 1 {
				var match []int
				for k, a := range stale {
					if realVendor(Vendor(a.MAC)) == v {
						match = append(match, k)
					}
				}
				if len(match) == 1 {
					pick, reason = &stale[match[0]], "its MAC was not seen for a day and both are "+v+" devices"
				}
			}
			if pick != nil {
				list[i].Suggested = &SuggestedAccount{ID: pick.ID, Code: pick.Code, Flat: pick.Flat, MAC: pick.MAC, Reason: reason}
			}
		}
	}
}

func realVendor(v string) string {
	if v == PrivateAddress {
		return ""
	}
	return v
}

// StaleAccountIDs is the set of flats a picker should offer first in a
// building: those whose MAC is flagged or has not been seen for a day.
func (s *Service) StaleAccountIDs(siteID string) map[string]bool {
	out := map[string]bool{}
	for _, a := range s.staleAccounts(siteID, s.now()) {
		out[a.ID] = true
	}
	return out
}

// ---- refresh, attach, create, dismiss ----------------------------------

// RefreshHosts brings the unregistered devices list up to date from the
// building's router. A hotspot building reads /ip/hotspot/host; an IPoE
// building reads the DHCP leases and the ARP table (the same reading the
// reconciler pass does, without any router write).
func (s *Service) RefreshHosts(ctx context.Context, siteID string) error {
	site, ok := s.site(siteID)
	if ok && IPoE(site) {
		return s.refreshIPoE(ctx, site)
	}
	hosts, err := s.router(siteID).Hosts(ctx)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if h.Authorized || h.Bypassed {
			continue
		}
		mac, err := NormalizeMAC(h.MAC)
		if err != nil {
			continue
		}
		s.NoteDevice(siteID, mac, h.Address, "hotspot-host")
	}
	return nil
}

func (s *Service) refreshIPoE(ctx context.Context, site store.Site) error {
	pl, err := s.PlanFor(site)
	if err != nil {
		return err
	}
	rt := s.router(site.ID)
	leases, err := rt.Rows(ctx, pathLease, nil)
	if err != nil {
		return err
	}
	arp, aerr := rt.Rows(ctx, pathARP, nil)
	if aerr != nil {
		arp = nil
	}
	var wants []wantAcct
	s.st.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.SiteID == site.ID && a.MAC != "" && !a.MACConflict {
				wants = append(wants, wantAcct{a: a})
			}
		}
	})
	s.collectDevices(site, pl, wants, leases, arp, s.prov(site.ID))
	return nil
}

// AttachDevice makes an unregistered device the ONT of an existing flat. It
// goes through ChangeMAC, so the one-MAC-one-account rule applies (the error
// names the flat and building that owns the MAC) and the router task is queued
// and run at once: the flat is online within seconds.
func (s *Service) AttachDevice(ctx context.Context, siteID, rawMAC, accountID string) (store.Account, error) {
	mac, err := NormalizeMAC(rawMAC)
	if err != nil {
		return store.Account{}, err
	}
	a, ok := s.GetAccount(accountID)
	if !ok {
		return store.Account{}, ErrNotFound
	}
	if siteID != "" && a.SiteID != siteID {
		return store.Account{}, errors.New("that flat is in another building than the device")
	}
	if err := s.ChangeMAC(ctx, accountID, mac); err != nil {
		return store.Account{}, err
	}
	a, _ = s.GetAccount(accountID)
	return a, nil
}

// DeviceAccountInput creates an account from an unregistered device.
type DeviceAccountInput struct {
	SiteID, MAC, Flat, Tenant, Phone, PackageID string
}

// CreateFromDevice creates a flat with the device's MAC. A package, when
// given, is set as the flat's plan (no time is granted: it is paid for as
// usual); the router gets the lease and queue at once.
func (s *Service) CreateFromDevice(ctx context.Context, in DeviceAccountInput) (store.Account, error) {
	if in.PackageID != "" { // checked first: a bad package must not leave a half-made flat behind
		ok := false
		s.st.View(func(d *store.Data) { p, found := d.Packages[in.PackageID]; ok = found && !p.Deleted })
		if !ok {
			return store.Account{}, fmt.Errorf("choose a package from the list")
		}
	}
	a, err := s.CreateAccount(AccountInput{SiteID: in.SiteID, Flat: in.Flat, MAC: in.MAC, Tenant: in.Tenant, Phone: in.Phone})
	if err != nil {
		return store.Account{}, err
	}
	if in.PackageID != "" {
		err = s.st.Update(func(d *store.Data) error {
			cur, ok := d.Accounts[a.ID]
			p, pok := d.Packages[in.PackageID]
			if !ok || !pok || p.Deleted {
				return fmt.Errorf("choose a package from the list")
			}
			cur.PackageID = in.PackageID
			d.Accounts[a.ID] = cur
			s.enqueueLocked(d, cur, false, "package set")
			return nil
		})
		if err != nil {
			return a, err
		}
	}
	s.runAccountSite(ctx, a.ID)
	a, _ = s.GetAccount(a.ID)
	return a, nil
}
