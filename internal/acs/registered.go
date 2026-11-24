package acs

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Registered ONUs only (ACS_REGISTERED_ONLY, default on).
//
//  1. A serial that is not in the ONU inventory (equipment kind "ont", not retired; the TR-069 serial or the GPON
//     serial) gets an answer to its Inform and nothing else: no task, no secret, no device record. It is listed as an
//     "unknown ONU tried to check in" (one entry per serial, kept for a week) so the owner can add it to the inventory.
//  2. A registered ONU's first contact, and a return on the shared login after a factory reset, is held until the
//     management-VLAN lease MAC of its address is near the inventory WAN MAC (same OUI, last 24 bits within macNearBy).
//     Until then the session gets no secret, and the owner may override it for that one lease MAC ("Trust this ONU").
//     An inventory item with no WAN MAC always needs the override.

const (
	keyUnknown     = "acs.unknown" // setting: JSON list of UnknownONU
	unknownTTL     = 7 * 24 * time.Hour
	unknownMax     = 100 // entries kept; a flood of invented serials cannot grow it further
	unknownTouch   = time.Minute
	unknownNotify  = 24 * time.Hour // the owners hear about one serial at most once a day
	unknownNotifyN = 5              // and about at most this many serials an hour
	macNearBy      = 8
)

// Errors of the override.
var (
	ErrNotWaiting = errors.New("that ONU is not waiting for its management MAC to match")
	ErrNoLeaseMAC = errors.New("the router has not said which MAC holds this ONU's address yet: wait for its next check-in")
)

// UnknownONU is a serial that tried to check in and is not in the ONU inventory.
type UnknownONU struct {
	Serial       string    `json:"serial"`
	OUI          string    `json:"oui,omitempty"`
	Manufacturer string    `json:"manufacturer,omitempty"`
	Model        string    `json:"model,omitempty"`
	SiteID       string    `json:"site_id,omitempty"`
	SiteName     string    `json:"site_name,omitempty"`
	IP           string    `json:"ip,omitempty"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	Count        int       `json:"count"`
	Notified     time.Time `json:"notified,omitempty"`
}

func readUnknown(d *store.Data) []UnknownONU {
	var l []UnknownONU
	if v := d.Settings[keyUnknown].Value; v != "" {
		_ = json.Unmarshal([]byte(v), &l)
	}
	return l
}

func writeUnknown(d *store.Data, l []UnknownONU, now time.Time) {
	b, _ := json.Marshal(l)
	d.Settings[keyUnknown] = store.Setting{Key: keyUnknown, Value: string(b), UpdatedAt: now}
}

// liveUnknown drops what expired and what the inventory knows by now.
func liveUnknown(d *store.Data, l []UnknownONU, now time.Time) []UnknownONU {
	out := l[:0:0]
	for _, u := range l {
		if now.Sub(u.LastSeen) > unknownTTL {
			continue
		}
		if _, ok := d.ONTBySerial(u.Serial); ok {
			continue
		}
		out = append(out, u)
	}
	return out
}

// noteUnknown records an unregistered serial's check-in. It returns the owner alert to send (empty = none) and whether
// anything was written.
func (s *Service) noteUnknown(d *store.Data, in Inform, site store.Site, ip string, now time.Time) (alert string, wrote bool) {
	all := readUnknown(d)
	l := liveUnknown(d, all, now)
	changed := len(l) != len(all)
	key := store.NormSerial(in.Serial)
	idx := -1
	for i, u := range l {
		if store.NormSerial(u.Serial) == key {
			idx = i
			break
		}
	}
	notify := false
	switch {
	case idx >= 0:
		u := l[idx]
		if now.Sub(u.LastSeen) >= unknownTouch { // at most one write a minute per serial
			u.LastSeen, u.Count, u.IP, u.SiteID, u.SiteName = now, u.Count+1, ip, site.ID, site.Name
			u.Model, u.Manufacturer = nonEmpty(cleanText(in.ProductClass), u.Model), nonEmpty(cleanText(in.Manufacturer), u.Manufacturer)
			changed = true
		}
		if now.Sub(u.Notified) >= unknownNotify {
			notify = true
		}
		l[idx] = u
	case len(l) < unknownMax:
		l = append(l, UnknownONU{Serial: cleanText(in.Serial), OUI: in.OUI, Manufacturer: cleanText(in.Manufacturer), Model: cleanText(in.ProductClass),
			SiteID: site.ID, SiteName: site.Name, IP: ip, FirstSeen: now, LastSeen: now, Count: 1})
		idx, changed, notify = len(l)-1, true, true
	}
	if notify && idx >= 0 && s.unknownBudget(now) {
		l[idx].Notified = now
		changed = true
		alert = fmt.Sprintf("An unknown ONU tried to check in: serial %s, model %s, router %s. It is not in the ONU inventory, so it was given nothing. If it is yours, add it to the inventory (Equipment > ONU).",
			l[idx].Serial, nonEmpty(l[idx].Model, "?"), nonEmpty(site.Name, site.ID))
	}
	if changed {
		sort.Slice(l, func(i, j int) bool { return l[i].LastSeen.After(l[j].LastSeen) })
		writeUnknown(d, l, now)
	}
	return alert, changed
}

// unknownBudget limits the owner messages about unknown serials (the list itself is not limited this way).
func (s *Service) unknownBudget(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.unkLog[:0]
	for _, t := range s.unkLog {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	s.unkLog = keep
	if len(s.unkLog) >= unknownNotifyN {
		return false
	}
	s.unkLog = append(s.unkLog, now)
	return true
}

// UnknownONUs lists the serials that tried to check in and are not registered (newest first), optionally of one router.
func (s *Service) UnknownONUs(siteID string) []UnknownONU {
	var out []UnknownONU
	now := s.now()
	s.st.View(func(d *store.Data) {
		for _, u := range liveUnknown(d, readUnknown(d), now) {
			if siteID == "" || u.SiteID == siteID {
				out = append(out, u)
			}
		}
	})
	return out
}

// pruneUnknown is called by Upkeep.
func pruneUnknown(d *store.Data, now time.Time) bool {
	all := readUnknown(d)
	if len(all) == 0 {
		return false
	}
	l := liveUnknown(d, all, now)
	if len(l) == len(all) {
		return false
	}
	writeUnknown(d, l, now)
	return true
}

// ---- the management MAC check ----

// macNear: same OUI, and the last 24 bits within macNearBy (V-SOL: label ...7E:0F, WAN and TR-069 WAN ...7E:10).
func macNear(a, b string) bool {
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
	return x-y <= macNearBy
}

type macGate struct {
	hold bool
	// quiet: nothing wrong was seen (the router has not answered yet), so nobody is alerted
	quiet bool
	note  string
	wan   string // the registered WAN MAC
}

// macGate decides whether the lease MAC of the address matches the registered ONU of this serial. With the setting off,
// or for a serial the inventory does not know (a record from before), it lets everything through.
func (s *Service) macGate(d *store.Data, serial string, sec store.ONUSec, lease leaseResult) macGate {
	if !s.cfg.ACSRegisteredOnly {
		return macGate{}
	}
	inv, ok := d.ONTBySerial(serial)
	if !ok {
		return macGate{}
	}
	g := macGate{hold: true, wan: inv.MAC}
	switch {
	case !lease.ok:
		g.quiet, g.note = true, "the router could not say which MAC holds the ONU's address yet"
	case lease.mac == "":
		g.quiet, g.note = true, "the router has no lease for the ONU's address yet"
	case sec.MACTrusted != "" && sec.MACTrusted == lease.mac:
		return macGate{wan: inv.MAC}
	case inv.MAC == "":
		g.note = "the registered ONU has no WAN MAC, so the owner has to trust this one"
	case macNear(lease.mac, inv.MAC):
		return macGate{wan: inv.MAC}
	default:
		g.note = "the management MAC is not near the registered WAN MAC"
	}
	return g
}

// TrustONU is the owner's override: the lease MAC the held ONU checked in from is accepted for this serial.
func (s *Service) TrustONU(by, ip, onuID string) error {
	err := s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return ErrONUGone
		}
		if o.Sec.MACCheck != store.MACWaiting {
			return ErrNotWaiting
		}
		if o.Sec.MACWaitLease == "" {
			return ErrNoLeaseMAC
		}
		o.Sec.MACTrusted = o.Sec.MACWaitLease
		d.ONUs[o.ID] = o
		s.audit(d, o, by, ip, "ONU "+o.Serial+": the owner trusts the management MAC "+o.Sec.MACTrusted+" for this serial (registered WAN MAC "+macText(s.regMAC(d, o.Serial))+")")
		return nil
	})
	if err == nil {
		s.kickAsync(onuID)
	}
	return err
}

func (s *Service) regMAC(d *store.Data, serial string) string {
	if inv, ok := d.ONTBySerial(serial); ok {
		return inv.MAC
	}
	return ""
}

// MACWaitView is what the ONU list shows for a box that waits for its management MAC to match.
type MACWaitView struct {
	Text     string `json:"text"`
	LeaseMAC string `json:"lease_mac,omitempty"`      // the MAC the router leased its address to
	WANMAC   string `json:"registered_mac,omitempty"` // the inventory WAN MAC
	Reason   string `json:"reason,omitempty"`
	CanTrust bool   `json:"can_trust"`
}

const macWaitText = "Waiting: management MAC doesn't match the registered ONU yet"

func (s *Service) macWaitView(d *store.Data, o store.ONU) *MACWaitView {
	if o.Sec.MACCheck != store.MACWaiting {
		return nil
	}
	return &MACWaitView{Text: macWaitText, LeaseMAC: o.Sec.MACWaitLease, WANMAC: s.regMAC(d, o.Serial), Reason: o.Sec.MACWaitNote, CanTrust: o.Sec.MACWaitLease != ""}
}

// holdMAC marks a box as waiting for its management MAC to match, and returns the owner alert (at most hourly, and
// none while the router simply has not said yet).
func (s *Service) holdMAC(o *store.ONU, g macGate, lease leaseResult, serial, siteName string, now time.Time) string {
	o.Sec.MACCheck, o.Sec.MACWaitLease, o.Sec.MACWaitNote = store.MACWaiting, lease.mac, g.note
	if g.quiet || (!o.Sec.MACAlerted.IsZero() && now.Sub(o.Sec.MACAlerted) < alertEvery) {
		return ""
	}
	o.Sec.MACAlerted = now
	return fmt.Sprintf("ONU %s on router %s is registered, but its management MAC %s is not near the registered WAN MAC %s (%s). Nothing is sent to it. If it is the right box, press Trust this ONU in the ONU list.",
		serial, siteName, macText(lease.mac), macText(g.wan), g.note)
}
