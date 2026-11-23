package acs

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Who may talk to a box, and what it may be told.
//
// Every ONU is given the same ACS login by the OLT's profile, so a login alone does not say which box is talking.
// What does: the box's own ACS login (this ACS writes one per device at first contact), and the management-VLAN
// DHCP lease of the router, which names the MAC that holds the address the Inform came from. That MAC is hardware
// and survives a factory reset.
//
//   - A serial seen for the first time is accepted (the first claim wins; the OLT only lets registered serials
//     online) and bound to its router, address and leased MAC.
//   - A known serial, linked to a house, is trusted when it logs in with its own login from where it was, or
//     with the shared login (what a factory-reset box has again) from the same router and from an address leased
//     to the recorded MAC.
//   - Anything else (another router, an address leased to another MAC, a serial that keeps changing address) is
//     QUARANTINED: the box is answered with no secret and no task, the record is left where it was, and the owner
//     is told to approve or block it.
//   - If the router cannot say who holds the address, the check-in is answered with nothing and tried again at the
//     next Inform: no secret goes out on a guess, and nobody is accused on one either.

// Verdicts of an Inform.
const (
	stOK          = "ok"
	stNew         = "new"
	stQuarantined = "quarantined"
	stUnverified  = "unverified"
	stBlocked     = "blocked"
	stBadAuth     = "badauth"
	stCapped      = "capped"
)

// trustLevel is what the session program may do.
type trustLevel int

const (
	trustNone trustLevel = iota // answer the Inform, do nothing else
	trustNew                    // first contact, or a box that is not at a house: provisioning, no house secrets
	trustFull                   // verified, linked: house secrets and every task
)

type verdict struct {
	state string
	trust trustLevel
	alert string
	// reset: the box came back with the shared login although it had its own (it lost its settings).
	reset bool
}

// leaseResult is the router's answer for an address: ok=false when it could not be asked, else mac is the MAC the
// address is leased to ("" = no lease).
type leaseResult struct {
	ok  bool
	mac string
}

// LeaseFunc says which MAC holds a management address on a router (the router sweep's copy of the DHCP leases).
type LeaseFunc func(ctx context.Context, siteID, ip string) (mac string, ok bool)

func (s *Service) lookupLease(ctx context.Context, siteID, ip string) leaseResult {
	if s.lease == nil {
		return leaseResult{}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	mac, ok := s.lease(ctx, siteID, ip)
	if !ok {
		return leaseResult{}
	}
	if c, good := store.CanonMAC(mac); good {
		mac = c
	}
	return leaseResult{ok: true, mac: mac}
}

// needLease: does this Inform need the lease table?
func (s *Service) needLease(cur store.ONU, found bool, siteID, ip string, ai authInfo) bool {
	switch {
	case !found, cur.Sec.MgmtMAC == "", ai.kind == authShared, cur.SiteID != siteID, cur.MgmtIP != ip:
		return true
	}
	return false
}

const (
	cloneMemory = 24 * time.Hour // after the original box showed up with its old login, shared-login returns are held back
	flapWindow  = time.Hour
	flapMax     = 3 // address or router changes inside the window before the serial is quarantined
	alertEvery  = time.Hour
)

// resetHold is how long the house's secrets wait after a box with its own login came back on the shared login: longer
// than two periodic Informs, so the original box, if it is still online, logs in with its old login first.
func (s *Service) resetHold() time.Duration { return 2*s.informEvery() + 2*time.Minute }

// decide judges an Inform and records it. It runs inside the store transaction and returns the row as it is stored.
func (s *Service) decide(d *store.Data, in Inform, site store.Site, ip netip.Addr, ai authInfo, lease leaseResult, now time.Time) (store.ONU, verdict) {
	ipS := ip.String()
	cur, found := d.ONUBySerial(in.OUI, in.Serial)
	var v verdict
	if ai.kind == authDevice && (!found || ai.onuID != cur.ID) {
		v.state = stBadAuth // a device's own login with another serial
		return cur, v
	}
	if found && blockedFrom(cur, site.ID, ipS, lease.mac) {
		v.state = stBlocked
		return cur, v
	}
	isNew := !found
	linked := found && cur.AccountID != ""
	if isNew {
		n := 0
		for _, x := range d.ONUs {
			if x.SiteID == site.ID && !x.Sec.Quarantine.Active { // quarantined rows must not fill a router's quota
				n++
			}
		}
		if n >= s.cfg.ACSMaxPerRouter {
			v.state = stCapped
			return cur, v
		}
		cur = store.ONU{ID: newONUID(), Serial: in.Serial, OUI: in.OUI, FirstSeen: now}
		if lease.ok && lease.mac != "" {
			cur.Sec.MgmtMAC = lease.mac // the first claim of a serial binds it to this box's MAC
		}
		v.state, v.trust = stNew, trustNew
	} else {
		siteChanged := cur.SiteID != "" && cur.SiteID != site.ID
		ipChanged := cur.MgmtIP != "" && cur.MgmtIP != ipS
		need := s.needLease(cur, true, site.ID, ipS, ai)
		macState := "n/a"
		if need {
			switch {
			case !lease.ok:
				macState = "unknown"
			case lease.mac == "":
				macState = "nolease"
			case cur.Sec.MgmtMAC == "":
				macState = "unbound"
			case lease.mac == cur.Sec.MgmtMAC:
				macState = "match"
			default:
				macState = "mismatch"
			}
		}
		changed := siteChanged || ipChanged || macState == "mismatch"
		reason := ""
		if linked {
			switch {
			case siteChanged:
				reason = fmt.Sprintf("it checked in from router %s, but it is on router %s", s.siteName(d, site.ID), s.siteName(d, cur.SiteID))
			case macState == "mismatch":
				reason = fmt.Sprintf("its address %s is leased to MAC %s, not to this box's MAC %s", ipS, macText(lease.mac), cur.Sec.MgmtMAC)
			case macState == "unbound" && ipChanged:
				// No MAC was bound yet (a record from before binding, or a first contact the router could not answer
				// for): only the address it was seen at can bind it. Anyone else on the router could claim it here.
				reason = fmt.Sprintf("it checked in from %s, not from %s where it was seen, before its MAC was bound", ipS, cur.MgmtIP)
			case ai.kind == authShared && cur.Sec.ACSCred == store.CredDevice && !cur.Sec.CloneAt.IsZero() && now.Sub(cur.Sec.CloneAt) < cloneMemory:
				// The original box was seen with its previous login after a shared-login return not long ago: another
				// device at its place claims to be it. Its next "reset" gets nothing, not even a new own login.
				reason = fmt.Sprintf("a device at its address (MAC %s) logs in with the shared login while the box itself still uses its own: its MAC and address may be copied", macText(lease.mac))
			case macState == "unknown" || macState == "nolease":
				// The router could not say who holds the address: no secret goes out, and nobody is accused.
				cur.LastInform, cur.LastEvents = now, strings.Join(in.Events, ",")
				d.ONUs[cur.ID] = cur
				v.state, v.trust = stUnverified, trustNone
				return cur, v
			}
		}
		if reason == "" && changed {
			if now.Sub(cur.Sec.FlapSince) > flapWindow {
				cur.Sec.FlapSince, cur.Sec.FlapCount = now, 0
			}
			cur.Sec.FlapCount++
			if cur.Sec.FlapCount >= flapMax {
				reason = fmt.Sprintf("its address or router changed %d times within an hour", cur.Sec.FlapCount)
			}
		}
		if reason != "" {
			q := &cur.Sec.Quarantine
			if !q.Active {
				q.Active, q.Since, q.Count, q.Places = true, now, 0, 1
			} else if q.SiteID != site.ID || q.IP != ipS || q.MAC != lease.mac {
				// Another place than the held-back check-in the owner may be looking at: Approve must not bind the serial
				// to whichever came last, and the owner hears about it now.
				if q.Places++; q.Places == 2 {
					q.Alerted = time.Time{} // once at once; after that hourly, like any held-back check-in
				}
				reason = fmt.Sprintf("%s (and the serial is claimed from %d different places)", reason, q.Places)
			}
			q.Count++
			q.LastAt, q.Reason, q.SiteID, q.IP, q.MAC = now, reason, site.ID, ipS, lease.mac
			if q.Alerted.IsZero() || now.Sub(q.Alerted) >= alertEvery {
				q.Alerted = now
				v.alert = fmt.Sprintf("ONU %s checked in from an unexpected place: %s. Approve or block it in the ONU list.", cur.Serial, reason)
			}
			d.ONUs[cur.ID] = cur // nothing else about the record moves
			v.state, v.trust = stQuarantined, trustNone
			return cur, v
		}
		if macState == "unbound" || (!linked && lease.ok && lease.mac != "") {
			cur.Sec.MgmtMAC = lease.mac
		}
		switch {
		case ai.kind == authDevice && ai.prev:
			// The box that was there before still has the login it had: the shared-login "reset" came from another
			// device at its place. The original keeps its login (the one written since is dropped), nothing owed is
			// sent, and the owner is told.
			sent := !cur.Sec.RestoreOwed && !cur.Sec.HoldUntil.IsZero()
			cur.Sec.ACSPassEnc, cur.Sec.ACSPrevPassEnc = cur.Sec.ACSPrevPassEnc, ""
			cur.Sec.ACSCred, cur.Sec.ACSTries = store.CredDevice, 0
			cur.Sec.HoldUntil, cur.Sec.RestoreOwed = time.Time{}, false
			first := cur.Sec.CloneAt.IsZero() || now.Sub(cur.Sec.CloneAt) >= alertEvery
			cur.Sec.CloneAt = now
			if first {
				v.alert = fmt.Sprintf("ONU %s: another device used the shared login with this box's serial on router %s while the box itself still uses its own login. Someone on the management VLAN knows the shared login (and may copy this box's MAC %s and address): check the OLT for that MAC on another port.", cur.Serial, s.siteName(d, site.ID), cur.Sec.MgmtMAC)
				if sent {
					v.alert += " The house's Wi-Fi may have been sent to it: change the house's Wi-Fi password."
				} else {
					v.alert += " Nothing secret was sent to it."
				}
			}
		case ai.kind == authDevice && cur.Sec.ACSCred == store.CredPending:
			cur.Sec.ACSCred, cur.Sec.ACSTries = store.CredDevice, 0
		case ai.kind == authShared && cur.Sec.ACSCred == store.CredPending:
			if cur.Sec.ACSTries++; cur.Sec.ACSTries >= 3 {
				cur.Sec.ACSCred = store.CredShared // the box keeps the shared login: it will not take its own
			}
		case ai.kind == authShared && cur.Sec.ACSCred == store.CredDevice:
			cur.Sec.ACSCred, v.reset = store.CredNone, true // it lost its own login: a factory reset, or the OLT re-pushed its profile
			// Anyone who knows the shared login and the serial can say the same. The old login is kept, so the original
			// box, if it still has it, is not locked out by the new one written now, and shows who it is.
			if !cur.Sec.RestoreOwed && cur.Sec.ACSPassEnc != "" {
				cur.Sec.ACSPrevPassEnc = cur.Sec.ACSPassEnc // during a hold the first one is kept: it is the original's
			}
			if linked {
				// A device that copied the box's MAC and address passes the place check. The house's secrets wait until
				// the original box, if it is still there, has had two periodic Informs to show itself with its old login.
				cur.Sec.HoldUntil, cur.Sec.RestoreOwed = now.Add(s.resetHold()), true
			}
		}
		v.state = stOK
		v.trust = trustNew
		if linked {
			v.trust = trustFull
			if cur.Sec.RestoreOwed {
				if now.Before(cur.Sec.HoldUntil) {
					v.trust = trustNew // provisioning only while the hold lasts
				} else {
					v.reset, cur.Sec.RestoreOwed = true, false // the hold passed with no sign of another box: restore now
				}
			}
		}
	}
	cur.Manufacturer, cur.ProductClass = nonEmpty(in.Manufacturer, cur.Manufacturer), nonEmpty(cleanText(in.ProductClass), cur.ProductClass)
	if cur.OUI == "" {
		cur.OUI = in.OUI
	}
	cur.SiteID, cur.MgmtIP, cur.LastInform, cur.LastEvents = site.ID, ipS, now, strings.Join(in.Events, ",")
	if root := rootOf(in.Params); root != "" {
		if cur.Root != root {
			cur.Profile = store.ONUProfile{Root: root}
		}
		cur.Root = root
	}
	if cur.Root != "" {
		if val, ok := in.Param(cur.Root + "ManagementServer.ConnectionRequestURL"); ok && len(val) < 300 {
			cur.ConnReqURL = val
		}
		if val, ok := in.Param(cur.Root + "DeviceInfo.SoftwareVersion"); ok && len(val) < 100 {
			cur.SWVersion = cleanText(val)
		}
		if val, ok := in.Param(cur.Root + "DeviceInfo.HardwareVersion"); ok && len(val) < 100 {
			cur.HWVersion = cleanText(val)
		}
	}
	applyReported(&cur, in.Params, homeWiFi(d, cur))
	expireTasks(d, now)
	// A task that was running when the session died goes back in the queue (three tries).
	for _, t := range d.ONUTasksOf(cur.ID) {
		if t.Status == store.TaskRunning && now.Sub(t.StartedAt) > 2*time.Minute {
			if t.Attempts >= 3 {
				t.Status, t.Fault, t.FinishedAt = store.TaskFailed, "the ONU did not finish the session three times", now
			} else {
				t.Status = store.TaskPending
			}
			d.ONUTasks[t.ID] = t
		}
	}
	d.ONUs[cur.ID] = cur
	autoLink(d, cur.ID, now)
	cur = d.ONUs[cur.ID]
	// A box that was linked to a house during this Inform keeps trustNew: its address or MAC may have just been
	// (re)bound, so this session gives it no house secret; the next one, from the same place, does.
	return cur, v
}

// blockedFrom: the owner blocked this MAC or address for this serial.
func blockedFrom(o store.ONU, siteID, ip, mac string) bool {
	for _, b := range o.Sec.BlockedFrom {
		if mac != "" && b == "mac:"+mac {
			return true
		}
		if b == "ip:"+siteID+"/"+ip {
			return true
		}
	}
	return false
}

// cleanText keeps a device-supplied text printable and short (a product class may hold hyphens, spaces, anything).
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func macText(m string) string {
	if m == "" {
		return "no lease"
	}
	return m
}

func (s *Service) siteName(d *store.Data, id string) string {
	if n := d.Sites[id].Name; n != "" {
		return n
	}
	return id
}
