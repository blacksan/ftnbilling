package billing

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	mac, ip, host, source, port string
	online                      bool
	// mgmt: the MAC holds a lease of the ONU-management DHCP server or sits on the ONU-management VLAN (an ONU's TR-069 WAN).
	mgmt bool
}

// The router objects of the ONU management network (internal/sites/onumgmt.go names them; billing cannot import sites).
const (
	onuMgmtDHCPServer = "ftn-onu-dhcp"
	onuMgmtVLANPrefix = "vlan"
	onuMgmtVLANSuffix = "-onu"
)

// isONUMgmtIface: the VLAN interface of the ONU management network (vlan99-onu).
func isONUMgmtIface(name string) bool {
	return strings.HasPrefix(name, onuMgmtVLANPrefix) && strings.HasSuffix(name, onuMgmtVLANSuffix) && len(name) > len(onuMgmtVLANPrefix)+len(onuMgmtVLANSuffix)
}

// ErrRouterOwnMAC: an account cannot be given a MAC that is one of a router's own interfaces.
var ErrRouterOwnMAC = errors.New("That MAC belongs to the router itself")

// routerFacts is what one pass learned about a router besides its leases and ARP rows: its own MACs and
// the bridge host table (which MACs sit behind which port at layer 2).
type routerFacts struct {
	own     map[string]bool
	hosts   []routeros.Row
	hostsOK bool
}

// readFacts reads the router's interfaces and bridge hosts once (best effort, sequential, so it stays
// inside the router's REST read budget) and remembers the router's own MACs for the account checks.
func (s *Service) readFacts(ctx context.Context, rt routeros.Client, siteID string) routerFacts {
	f := routerFacts{own: map[string]bool{}}
	ifs, ierr := rt.RowsProps(ctx, pathInterface, nil, []string{"name", "mac-address"})
	if ierr == nil {
		for _, r := range ifs {
			if m, err := NormalizeMAC(r["mac-address"]); err == nil {
				f.own[m] = true
			}
		}
	}
	hosts, herr := rt.RowsProps(ctx, pathBridgeHost, nil, []string{"mac-address", "on-interface", "local", "bridge", "vid"})
	if herr == nil {
		f.hostsOK = true
		f.hosts = hosts
		for _, r := range hosts {
			if r["local"] == "true" || r["local"] == "yes" {
				if m, err := NormalizeMAC(r["mac-address"]); err == nil {
					f.own[m] = true
				}
			}
		}
	}
	if ierr == nil || herr == nil {
		s.ownMu.Lock()
		if s.ownMACs == nil {
			s.ownMACs = map[string]map[string]bool{}
		}
		s.ownMACs[siteID] = f.own
		s.ownMu.Unlock()
	}
	return f
}

// isRouterMAC: the MAC is one of the interfaces of a router this server has read.
func (s *Service) isRouterMAC(mac string) bool {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	for _, set := range s.ownMACs {
		if set[mac] {
			return true
		}
	}
	return false
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
	s.collectDevicesX(site, pl, true, true, wants, leases, arp, routerFacts{}, ps)
}

// collectDevicesX is collectDevices with hasPlan: it
// hasPlan false (the plan could not be computed) turns the network filter off.
//
// complete is false when one of the two reads failed: the devices not seen this time are then left as they were
// (a failed ARP read must not flip every device seen only through ARP offline and count a new hit when it returns).
func (s *Service) collectDevicesX(site store.Site, pl netplan.Plan, hasPlan, complete bool, wants []wantAcct, leases, arp []routeros.Row, facts routerFacts, ps *provState) {
	now := s.now()
	byMAC := map[string]*devObs{}
	order := []string{}
	upsert := func(mac, ip, host, source string, online, anyNet bool) *devObs {
		m, err := NormalizeMAC(mac)
		if err != nil || facts.own[m] {
			return nil // the router's own interfaces are never devices
		}
		if ip != "" && hasPlan && !anyNet && !pl.InNetwork(ip) {
			return nil
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
		return o
	}
	// ONU management: a MAC with a lease of the management DHCP server, an ARP row on its VLAN interface, or a bridge host
	// on that VLAN is an ONU's TR-069 WAN, whatever address it has (the management subnet is not the customer network).
	mgmt := map[string]bool{}
	markMgmt := func(mac string) {
		if m, err := NormalizeMAC(mac); err == nil {
			mgmt[m] = true
		}
	}
	for _, r := range leases {
		if r["server"] == onuMgmtDHCPServer {
			markMgmt(r["mac-address"])
		}
	}
	for _, r := range arp {
		if isONUMgmtIface(r["interface"]) {
			markMgmt(r["mac-address"])
		}
	}
	for _, r := range facts.hosts {
		if r["local"] == "true" || r["local"] == "yes" {
			continue
		}
		if isONUMgmtIface(r["on-interface"]) || (site.ONUMgmtVLAN > 1 && r["vid"] == strconv.Itoa(site.ONUMgmtVLAN)) {
			markMgmt(r["mac-address"])
		}
	}
	for _, r := range leases {
		if isOurs(r) {
			continue
		}
		upsert(r["mac-address"], firstNonEmpty(r["active-address"], r["address"]), r["host-name"], "dhcp", r["status"] == "bound", mgmt[strings.ToUpper(r["mac-address"])])
	}
	for _, r := range arp {
		if isOurs(r) {
			continue // our static entry says nothing about whether the flat is there
		}
		upsert(r["mac-address"], r["address"], "", "arp", arpOnline(r), mgmt[strings.ToUpper(r["mac-address"])])
	}
	// Layer-2 only: a device on a customer port that has not asked for an address (no lease, no ARP row).
	// An address from DHCP or ARP above wins; the bridge entry adds the port and keeps it online.
	for _, r := range facts.hosts {
		if r["local"] == "true" || r["local"] == "yes" || r["bridge"] != CustomerBridgeName || r["on-interface"] == "" {
			continue
		}
		if o := upsert(r["mac-address"], "", "", "bridge", true, true); o != nil && o.port == "" {
			o.port = r["on-interface"]
		}
	}
	// Flats that were seen right now.
	ps.mu.Lock()
	for _, w := range wants {
		o := byMAC[strings.ToUpper(w.a.MAC)]
		if o != nil && o.online {
			ps.seen[w.a.Code] = now
		}
		if facts.hostsOK { // the port is only re-learned from a pass that could read the bridge hosts
			if o != nil && o.port != "" {
				ps.ports[w.a.Code] = o.port
			} else {
				delete(ps.ports, w.a.Code)
			}
		}
	}
	ps.mu.Unlock()
	obs := make([]devObs, 0, len(order))
	for _, m := range order {
		o := *byMAC[m]
		o.mgmt = mgmt[m]
		obs = append(obs, o)
	}
	s.noteDevices(site.ID, obs, complete)
}

// maxDevicesPerSite bounds the unregistered-device table of one building.
const maxDevicesPerSite = 500

// noteDevices records the observed MACs that have no open account, and marks
// the building's other known devices offline. It is one store update.
func (s *Service) noteDevices(siteID string, obs []devObs, complete bool) {
	now := s.now()
	_ = s.st.Update(func(d *store.Data) error {
		if st, ok := d.Sites[siteID]; !ok || st.Removed {
			return store.ErrNoChange
		}
		changed := false
		seen := map[string]bool{}
		for _, o := range obs {
			if own := d.OpenAccountsByMAC(o.mac); len(own) > 0 && own[0].SiteID == siteID {
				continue // already somebody's ONT here. On another router it is listed as "belongs to", never provisioned here.
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
			if o.port != "" {
				dev.Port = o.port
			}
			switch {
			case dev.Source == "" || dev.Source == "bridge":
				dev.Source = o.source
			case dev.Source == "dhcp" || dev.Source == "arp":
				if o.source != "bridge" { // a device that was heard asking for an address keeps saying so
					dev.Source = o.source
				}
			} // portal and dismissed are kept
			if o.mgmt && (dev.Source == "" || dev.Source == "bridge" || dev.Source == "dhcp" || dev.Source == "arp") {
				dev.Source = store.DeviceSourceONUMgmt // sticky: the later passes that only see it at layer 2 leave it as it is
			}
			d.Devices[key] = dev
			changed = true
		}
		for key, dev := range d.Devices {
			if complete && dev.SiteID == siteID && !seen[key] && dev.Online && dev.Source != store.DeviceSourceOLTMgmt {
				dev.Online = false
				d.Devices[key] = dev
				changed = true
			}
		}
		// Bounded per building (a noisy network must not push a quiet building's devices out): drop the stalest,
		// dismissed ones last (losing one would list it as new again).
		for {
			n := 0
			for _, v := range d.Devices {
				if v.SiteID == siteID {
					n++
				}
			}
			if n <= maxDevicesPerSite {
				break
			}
			var oldest string
			for m, v := range d.Devices {
				if v.SiteID != siteID {
					continue
				}
				if oldest == "" {
					oldest = m
					continue
				}
				o := d.Devices[oldest]
				ov, vv := o.Source == SourceDismissed, v.Source == SourceDismissed
				if ov != vv {
					if ov {
						oldest = m // keep the dismissed one, drop this
					}
					continue
				}
				if v.LastSeen.Before(o.LastSeen) {
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

// AddressNote says why a device has no address in the list: it sits on a port
// but has not asked for one.
func (d UnregDevice) AddrNote() string {
	if d.IP == "" && d.Source == "bridge" {
		if d.Port != "" {
			return "On " + d.Port + ", no address yet (it hasn't asked for one)"
		}
		return "No address yet (it hasn't asked for one)"
	}
	return ""
}

// SuggestedAccount is the flat a device probably belongs to.
type SuggestedAccount struct {
	ID, Code, Flat, MAC string
	Reason              string
}

// BelongsTo says that an unregistered device is the ONT of an open account
// that is served somewhere else: it is never provisioned on the router that
// sees it. The operator can move the customer here (MoveCustomerHere).
type BelongsTo struct {
	AccountID, Code, Customer string
	ApartmentID, Apartment    string
	Unit                      string
	SiteID, SiteName          string // the account's own router
	// SameApartment: the router that sees the device serves the account's
	// apartment too (a second router of a large apartment); moving then only
	// changes the account's router.
	SameApartment bool
	// ApartmentChoices are the apartments the seeing router serves, to move
	// the customer into (empty when SameApartment).
	ApartmentChoices []store.Apartment
}

// UnregDevice is one row of the unregistered devices list.
type UnregDevice struct {
	store.Device
	SiteName  string
	Online    bool // seen within the last few minutes
	Suggested *SuggestedAccount
	// BelongsTo is set when the MAC is the ONT of an open account on another router.
	BelongsTo *BelongsTo
	// Class says what the device is in the equipment hierarchy: an OLT, an ONU of the register, or unknown.
	Class DeviceClass
}

// DismissedMACs lists the MACs the operator dismissed on a building's router.
func (s *Service) DismissedMACs(siteID string) []string {
	var out []string
	s.st.View(func(d *store.Data) {
		for _, dv := range d.DeviceList() {
			if dv.SiteID == siteID && dv.Source == SourceDismissed {
				out = append(out, dv.MAC)
			}
		}
	})
	return out
}

// Unregistered lists the devices that have no account, online first, then by
// last seen. siteID "" means every building. Dismissed devices are not listed.
func (s *Service) Unregistered(siteID string, onlineOnly bool) []UnregDevice {
	now := s.now()
	var out []UnregDevice
	var devs []store.Device
	names := map[string]string{}
	belongs := map[string]*BelongsTo{}
	classes := map[string]DeviceClass{}
	s.st.View(func(d *store.Data) {
		for _, dv := range d.DeviceList() {
			if (siteID == "" || dv.SiteID == siteID) && dv.Source != SourceDismissed {
				if s.isRouterMAC(dv.MAC) {
					continue // a router's own interface is never a device
				}
				b, own := belongsToLocked(d, dv)
				if own {
					continue // the account is on this very router: not an unknown device
				}
				devs = append(devs, dv)
				classes[store.DeviceKey(dv.SiteID, dv.MAC)] = classifyLocked(d, dv, false)
				names[dv.SiteID] = d.Sites[dv.SiteID].Name
				if b != nil {
					belongs[store.DeviceKey(dv.SiteID, dv.MAC)] = b
				}
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
		out = append(out, UnregDevice{Device: dv, SiteName: names[dv.SiteID], Online: online, BelongsTo: belongs[store.DeviceKey(dv.SiteID, dv.MAC)], Class: classes[store.DeviceKey(dv.SiteID, dv.MAC)]})
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

// belongsToLocked tells whether the device's MAC is the ONT of an open account.
// own is true when that account is served by the router that sees the device
// (then the device is not unknown at all).
func belongsToLocked(d *store.Data, dv store.Device) (b *BelongsTo, own bool) {
	owners := d.OpenAccountsByMAC(dv.MAC)
	if len(owners) == 0 || owners[0].MACConflict {
		return nil, false
	}
	a := owners[0]
	if a.SiteID == dv.SiteID {
		return nil, true
	}
	b = &BelongsTo{AccountID: a.ID, Code: a.Code, Customer: accountCustomerName(d, a), ApartmentID: a.ApartmentID,
		Apartment: d.Apartments[a.ApartmentID].Name, Unit: a.Unit, SiteID: a.SiteID, SiteName: d.Sites[a.SiteID].Name,
		SameApartment: a.ApartmentID != "" && d.ApartmentServedBy(a.ApartmentID, dv.SiteID)}
	if b.Unit == "" {
		b.Unit = a.Flat
	}
	if !b.SameApartment {
		for _, id := range d.SiteApartmentIDs(dv.SiteID) {
			b.ApartmentChoices = append(b.ApartmentChoices, d.Apartments[id])
		}
	}
	return b, false
}

// DeviceOwnership reports, for a MAC seen on a router, which open account it
// belongs to when that account is served by another router (b), or that it is
// the ONT of an account of this very router (own), in which case it is not an
// unknown device at all.
func (s *Service) DeviceOwnership(siteID, mac string) (b *BelongsTo, own bool) {
	s.st.View(func(d *store.Data) {
		b, own = belongsToLocked(d, store.Device{SiteID: siteID, MAC: mac})
	})
	return
}

// MoveCustomerHere is the operator action on a device that belongs to a
// customer of another building: it changes the account's apartment (and
// router) to the router that sees the device. apartmentID may be empty when
// the router serves exactly one apartment, or the account's apartment is
// already served here (then only the router changes).
func (s *Service) MoveCustomerHere(ctx context.Context, siteID, rawMAC, apartmentID string) (store.Account, error) {
	mac, err := NormalizeMAC(rawMAC)
	if err != nil {
		return store.Account{}, err
	}
	var owner store.Account
	var served bool
	var choices []string
	s.st.View(func(d *store.Data) {
		if a, ok := d.AccountByMAC(mac); ok {
			owner = a
			served = a.ApartmentID != "" && d.ApartmentServedBy(a.ApartmentID, siteID)
		}
		choices = d.SiteApartmentIDs(siteID)
	})
	if owner.ID == "" {
		return store.Account{}, errors.New("that device does not belong to any customer")
	}
	if owner.SiteID == siteID {
		return store.Account{}, store.ErrNoChange
	}
	target := apartmentID
	switch {
	case target != "":
	case served:
		target = owner.ApartmentID
	case len(choices) == 1:
		target = choices[0]
	case len(choices) == 0:
		return store.Account{}, fmt.Errorf("%w: this router serves no apartment yet", ErrApartmentNoRoute)
	default:
		return store.Account{}, ErrNoApartment
	}
	return s.MoveToApartment(ctx, MoveInput{AccountID: owner.ID, ApartmentID: target, SiteID: siteID, ExpectMAC: mac})
}

// UnregisteredOnline counts the online unregistered devices of a building
// (siteID "" = all): the sidebar and dashboard badge.
func (s *Service) UnregisteredOnline(has func(siteID string) bool) int {
	n := 0
	now := s.now()
	s.st.View(func(d *store.Data) {
		for _, dv := range d.Devices {
			if dv.Source != SourceDismissed && dv.Online && now.Sub(dv.LastSeen) <= OnlineWindow && (has == nil || has(dv.SiteID)) {
				if _, isOLT := d.OLTByMAC(dv.MAC); isOLT && dv.Source != store.DeviceSourceOLTMgmt {
					continue // an OLT needs nothing from the operator: it is not a new device to deal with
				}
				if onuMgmtDevice(d, dv) {
					continue // an ONU's TR-069 WAN needs nothing from the operator either
				}
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
	if _, ok := s.site(siteID); !ok {
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
		if u.Online && u.BelongsTo == nil && u.Class.Class != ClassOLT && u.Class.Class != ClassPossibleOLT && u.Class.Class != ClassONUMgmt {
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
// building's router, It is read-only on the router.
func (s *Service) RefreshHosts(ctx context.Context, siteID string) error {
	site, ok := s.site(siteID)
	if !ok {
		return ErrNotFound
	}
	return s.refreshDevices(ctx, site)
}

// refreshDevices reads what is connected to a building's router and records
// every MAC that has no open account: the DHCP leases and the ARP table (each
// best effort, an error only when neither could be read). noteDevices already
// ignores a MAC that has an open account.
func (s *Service) refreshDevices(ctx context.Context, site store.Site) error {
	pl, perr := s.PlanFor(site)
	rt := s.router(site.ID)
	leases, lerr := rt.Rows(ctx, pathLease, nil)
	arp, aerr := rt.Rows(ctx, pathARP, nil)
	facts := s.readFacts(ctx, rt, site.ID)
	if lerr != nil && aerr != nil && !facts.hostsOK {
		return lerr
	}
	if lerr != nil {
		leases = nil
	}
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
	s.collectDevicesX(site, pl, perr == nil, lerr == nil && aerr == nil && facts.hostsOK, wants, leases, arp, facts, s.prov(site.ID))
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
	// A MAC that is some other open account's ONT is refused first, and in
	// words that say whether that account lives in another building.
	var dupErr error
	s.st.View(func(d *store.Data) {
		if owner, ok := d.AccountByMAC(mac); ok && owner.ID != accountID {
			dupErr = duplicateMAC(d, owner, a.ApartmentID)
		}
	})
	if dupErr != nil {
		return store.Account{}, dupErr
	}
	if siteID != "" && a.SiteID != siteID {
		return store.Account{}, errors.New("that account is served by another router than the device: pick an account of this router")
	}
	if err := s.ChangeMAC(ctx, accountID, mac); err != nil {
		return store.Account{}, err
	}
	a, _ = s.GetAccount(accountID)
	return a, nil
}

// DeviceAccountInput creates an account from an unregistered device.
type DeviceAccountInput struct {
	SiteID, ApartmentID, Unit, MAC, Flat, Tenant, Phone, PackageID string
	ConfirmNoAddress                                               bool
}

// CreateFromDevice creates a flat with the device's MAC. A package, when
// given, is set as the flat's plan (no time is granted: it is paid for as
// usual); the router gets the lease and queue at once.
func (s *Service) CreateFromDevice(ctx context.Context, in DeviceAccountInput) (store.Account, error) {
	a, _, err := s.CreateCustomer(ctx, AddCustomerInput{SiteID: in.SiteID, ApartmentID: in.ApartmentID, Unit: in.Unit, MAC: in.MAC, Flat: in.Flat, Tenant: in.Tenant, Phone: in.Phone, PackageID: in.PackageID, ConfirmNoAddress: in.ConfirmNoAddress})
	return a, err
}

// AddCustomerInput is the one "add customer" form: the flat, the tenant, the
// device's MAC, a package and whether to grant its time now.
type AddCustomerInput struct {
	// ApartmentID, SiteID (the router: empty = the apartment's only router) and
	// Unit say where the customer lives. Flat is the older name of Unit.
	ApartmentID, SiteID, Unit, Flat, Tenant, Phone, MAC, PackageID string
	// EquipmentID: an ONU of the register (in stock) given to the house; its WAN MAC becomes the house MAC.
	EquipmentID string
	// ConfirmNoAddress: see AccountInput.
	ConfirmNoAddress bool
	// Activate grants the package's time at once (an admin grant, like
	// ExtendAccount). Without it the package is only set as the flat's plan
	// and the tenant pays on the payment page: the flat gets its lease and
	// queue and is walled until then.
	Activate bool
	Note     string
}

// CreateCustomer makes the account, sets its package, optionally grants the
// package's time, and makes one synchronous attempt to put it on the router
// (8 s). It returns the account and what the router side looks like after
// that attempt: a router that is offline leaves a queued task
// (PendingTask), applied by the retry loop when it is back. Errors are the
// ones of CreateAccount (DuplicateMACError for a MAC that is in use) and
// "choose a package from the list".
func (s *Service) CreateCustomer(ctx context.Context, in AddCustomerInput) (store.Account, AccountRouterState, error) {
	if in.Activate && in.PackageID == "" {
		return store.Account{}, AccountRouterState{}, fmt.Errorf("choose a package from the list")
	}
	if in.PackageID != "" { // checked first: a bad package must not leave a half-made flat behind
		ok := false
		pkgSite := in.SiteID
		s.st.View(func(d *store.Data) {
			if ids := d.ApartmentSiteIDs(in.ApartmentID); pkgSite == "" && len(ids) == 1 {
				pkgSite = ids[0] // the router the account will get
			}
			p, found := d.EffectivePackage(pkgSite, in.PackageID)
			ok = found && !p.Deleted
		})
		if !ok {
			return store.Account{}, AccountRouterState{}, fmt.Errorf("choose a package from the list")
		}
	}
	a, err := s.CreateAccount(AccountInput{ApartmentID: in.ApartmentID, SiteID: in.SiteID, Unit: in.Unit, Flat: in.Flat, MAC: in.MAC, Tenant: in.Tenant, Phone: in.Phone, Notes: in.Note, EquipmentID: in.EquipmentID, ConfirmNoAddress: in.ConfirmNoAddress})
	if err != nil {
		return store.Account{}, AccountRouterState{}, err
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
			s.enqueueLocked(d, cur, "package set")
			return nil
		})
		if err != nil {
			return a, AccountRouterState{}, err
		}
	}
	if in.Activate {
		if err := s.ExtendAccount(ctx, a.ID, in.PackageID, strings.TrimSpace(in.Note), false); err != nil {
			a, _ = s.GetAccount(a.ID)
			return a, s.AccountRouterState(a.ID), err
		}
	}
	rc, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	s.runAccountSiteNoCap(rc, a.ID)
	a, _ = s.GetAccount(a.ID)
	return a, s.AccountRouterState(a.ID), nil
}

// neverAskedForAddressLocked: the MAC is on the router's unknown-device list only because the bridge saw it (no
// lease, no ARP row, no portal visit) and it is not an ONU of the register. Making a customer of it needs a
// confirmation: a bridge-only MAC is as likely a switch, a camera or an OLT as an ONU.
func neverAskedForAddressLocked(d *store.Data, siteID, mac string) bool {
	dv, ok := d.Devices[store.DeviceKey(siteID, mac)]
	if !ok || dv.IP != "" || dv.Source != "bridge" {
		return false
	}
	_, inv := d.ONTByMAC(mac)
	return !inv
}

// ---- classification --------------------------------------------------------------------------------------

// Device classes of the New devices list: what a MAC is in the equipment hierarchy.
const (
	ClassRouter  = "router"  // an interface of the router itself (never listed)
	ClassOLT     = "olt"     // the MAC of an OLT of the register
	ClassONU     = "onu"     // the WAN MAC of an ONU of the register
	ClassUnknown = "unknown" // nothing known
	// ClassONUMgmt: the TR-069 WAN of an ONU on the ONU-management network (a lease of its DHCP server, or seen on its VLAN).
	// Not a customer device: not creatable, not attachable, hidden from the customer lists.
	ClassONUMgmt = "onu_mgmt"
	// ClassPossibleOLT: a device on a router's OLT management port that is not (or not yet proven to be) an OLT of the register.
	ClassPossibleOLT = "possible_olt"
)

// DeviceClass says what a device row is.
type DeviceClass struct {
	Class string
	// Label is "OLT <model> <name>" or "ONU <model> · <serial>"; empty for unknown.
	Label string
	// EquipmentID is the register item of an OLT or ONU.
	EquipmentID string
	// Creatable: a customer can be made of it (an in-stock ONU, or an unknown device).
	Creatable bool
	// ApartmentIDs: the apartments an ONU's PON port serves (to preselect).
	ApartmentIDs []string
	// NeverAsked: an unknown device that only the bridge has seen (confirmation needed to make a customer).
	NeverAsked bool
}

// IsEquipment says the device is network equipment that is never a customer: an OLT or one of the router's own
// interfaces. An ONU is not: its WAN MAC is the customer's MAC. An ONU of the register that is not in stock and has
// no account here (returned, faulty, or another house's) is still listed, without Create customer, so it can be
// attached or dismissed like on the New devices page.
func (c DeviceClass) IsEquipment() bool {
	return c.Class == ClassOLT || c.Class == ClassRouter || c.Class == ClassONUMgmt
}

func classifyLocked(d *store.Data, dv store.Device, router bool) DeviceClass {
	if router {
		return DeviceClass{Class: ClassRouter}
	}
	if onuMgmtDevice(d, dv) {
		return classifyONUMgmt(d, dv)
	}
	if dv.Source == store.DeviceSourceOLTMgmt {
		label := "Possible OLT"
		if dv.Vendor != "" {
			label += " (" + dv.Vendor + ")"
		}
		return DeviceClass{Class: ClassPossibleOLT, Label: label}
	}
	if o, ok := d.OLTByMAC(dv.MAC); ok {
		return DeviceClass{Class: ClassOLT, Label: strings.Join(strings.Fields("OLT "+o.Model+" "+o.Name), " "), EquipmentID: o.ID}
	}
	if e, ok := d.ONTByMAC(dv.MAC); ok {
		c := DeviceClass{Class: ClassONU, Label: "ONU " + firstNonEmpty(e.Model, "ONU") + " · " + firstNonEmpty(e.Serial, "no serial"), EquipmentID: e.ID,
			Creatable: e.Status == store.EqInStock, ApartmentIDs: ONUApartmentIDs(d, e)}
		c.Label = strings.Replace(c.Label, "ONU ONU ", "ONU ", 1)
		return c
	}
	return DeviceClass{Class: ClassUnknown, Creatable: true, NeverAsked: dv.IP == "" && dv.Source == "bridge"}
}

// macNear24: same OUI and the last 24 bits within 8 (the V-SOL case: label ...0F, WAN ...10, TR-069 WAN ...11).
func macNear24(a, b string) bool {
	ca, ok1 := store.CanonMAC(a)
	cb, ok2 := store.CanonMAC(b)
	if !ok1 || !ok2 || ca[:8] != cb[:8] {
		return false
	}
	var x, y int64
	fmt.Sscanf(strings.ReplaceAll(ca[9:], ":", ""), "%x", &x)
	fmt.Sscanf(strings.ReplaceAll(cb[9:], ":", ""), "%x", &y)
	if x < y {
		x, y = y, x
	}
	return x-y <= 8
}

// classifyONUMgmt names the ONU behind a management MAC: the ACS device record bound to that MAC (or address) first, then
// the register's ONT whose WAN MAC is within 8 of it. No match: "unknown ONU".
func classifyONUMgmt(d *store.Data, dv store.Device) DeviceClass {
	c := DeviceClass{Class: ClassONUMgmt}
	who := "unknown ONU"
	var inv store.Equipment
	found := false
	for _, o := range d.ONUs {
		if o.SiteID == dv.SiteID && ((o.Sec.MgmtMAC != "" && strings.EqualFold(o.Sec.MgmtMAC, dv.MAC)) || (dv.IP != "" && o.MgmtIP == dv.IP)) {
			if e, ok := d.ONTBySerial(o.Serial); ok {
				inv, found = e, true
			} else {
				who = "ONU " + o.Serial
			}
			break
		}
	}
	if !found {
		ids := make([]string, 0, 2)
		for id, e := range d.Equipment {
			if e.Kind == store.KindONT && e.Status != store.EqRetired && e.MAC != "" && macNear24(e.MAC, dv.MAC) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) == 1 { // two candidates say nothing
			inv, found = d.Equipment[ids[0]], true
		}
	}
	if found {
		who = strings.TrimSpace(firstNonEmpty(inv.Model, "ONU") + " · " + firstNonEmpty(inv.Serial, inv.GPONSerial, "no serial"))
		c.EquipmentID = inv.ID
	}
	c.Label = "ONU management (TR-069) — " + who
	return c
}

// ONUMgmtMACError: the MAC is an ONU's TR-069 WAN on the management network, not a customer's device.
var ErrONUMgmtMAC = errors.New("That MAC is an ONU's TR-069 management address, not a customer device")

// onuMgmtDevice: the device is an ONU's TR-069 WAN. An account's MAC wins, and so does a register ONT's WAN MAC: an ONU whose
// firmware uses one MAC for its TR-069 and its internet WAN is still created as the house's ONU (class onu).
func onuMgmtDevice(d *store.Data, dv store.Device) bool {
	if dv.Source != store.DeviceSourceONUMgmt || len(d.OpenAccountsByMAC(dv.MAC)) > 0 {
		return false
	}
	_, isONT := d.ONTByMAC(dv.MAC)
	return !isONT
}

// onuMgmtMACLocked refuses a MAC the router showed on the ONU-management network (unless an account already has it, or it
// is a register ONT's WAN MAC).
func onuMgmtMACLocked(d *store.Data, mac string) error {
	if len(d.OpenAccountsByMAC(mac)) > 0 {
		return nil
	}
	if _, isONT := d.ONTByMAC(mac); isONT {
		return nil
	}
	for _, dv := range d.Devices {
		if dv.Source == store.DeviceSourceONUMgmt && strings.EqualFold(dv.MAC, mac) {
			return ErrONUMgmtMAC
		}
	}
	return nil
}

// ClassifyMAC classes a MAC seen on a router (service entry point for the API and tests).
func (s *Service) ClassifyMAC(siteID, mac string) DeviceClass {
	m, err := NormalizeMAC(mac)
	if err != nil {
		return DeviceClass{Class: ClassUnknown, Creatable: true}
	}
	var c DeviceClass
	router := s.isRouterMAC(m)
	s.st.View(func(d *store.Data) {
		dv, ok := d.Devices[store.DeviceKey(siteID, m)]
		if !ok {
			dv = store.Device{SiteID: siteID, MAC: m}
		}
		c = classifyLocked(d, dv, router)
	})
	return c
}
