package acs

import (
	"context"
	"strings"

	"ispbill/internal/store"
)

// The Wi-Fi change of a house, written to the box.
//
// What counts as success. A box that answers SetParameterValues with status 0 has ACCEPTED the change; it has not
// proved that a phone can join the new network (one ONU answered success for a vendor-named passphrase and kept its
// old password). So the ACS
//   - never drops the name or the passphrase from a Set to get past a refusal (it changes the passphrase NAME instead);
//   - reads the name and the security mode back after the Set and fails the task, with the reason, when the name is
//     not what was asked for or the network is not WPA2-PSK (or better), re-applying the security settings first;
//   - ends the task "accepted" (or "accepted, applies after a restart" for status 1), never "applied": the owner
//     confirms with "It works" after a phone joined, and that confirmation is what teaches the per-model table.

type wifiResult struct {
	fault       string // the task failed: why
	note        string // an accepted task's caution (shown beside "Accepted")
	afterReboot bool   // status 1: the box applies it after a restart
	leaf        string // the passphrase name the box accepted
}

// doWiFi writes the house's Wi-Fi choice to the main instance of each band.
func (s *Service) doWiFi(ctx context.Context, ss *session, onuID string, t store.ONUTask) (wifiResult, error) {
	var res wifiResult
	o, ok := s.loadONU(onuID)
	if !ok {
		res.fault = "the ONU is gone"
		return res, nil
	}
	var want store.WiFiDesired
	s.st.View(func(d *store.Data) { want = homeWiFi(d, o) })
	if !want.Set {
		res.fault = "no Wi-Fi name and password are saved for this house"
		return res, nil
	}
	pass, err := s.openPass(want.PassEnc)
	if err != nil {
		res.fault = "the saved Wi-Fi password cannot be opened"
		return res, nil
	}
	pass5, ssid5 := pass, want.SSID
	if !want.Same {
		ssid5 = want.SSID5
		if pass5, err = s.openPass(want.Pass5Enc); err != nil {
			res.fault = "the saved 5 GHz Wi-Fi password cannot be opened"
			return res, nil
		}
	}
	if len(o.WiFi) == 0 && !o.State.NoWiFi {
		if err := s.discover(ctx, ss, onuID); err != nil && ctx.Err() != nil {
			return res, err
		}
		if o, ok = s.loadONU(onuID); !ok {
			res.fault = "the ONU is gone"
			return res, nil
		}
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	var cands []string
	s.st.View(func(d *store.Data) { cands = passCandidates(d, o, prof) })
	type target struct{ band, ssid, pass string }
	var faults, notes []string
	attempted := 0
	for _, tg := range []target{{"2.4", want.SSID, pass}, {"5", ssid5, pass5}} {
		w, ok := primary(o.WiFi, tg.band)
		if !ok {
			continue
		}
		attempted++
		r, err := s.setBand(ctx, ss, onuID, o, prof, w, tg.ssid, tg.pass, cands, []string{pass, pass5})
		if err != nil {
			return res, err
		}
		if r.fault != "" {
			faults = append(faults, tg.band+" GHz: "+r.fault)
			continue
		}
		if res.leaf == "" {
			res.leaf = r.leaf
		}
		res.afterReboot = res.afterReboot || r.afterReboot
		if r.note != "" {
			notes = append(notes, tg.band+" GHz: "+r.note)
		}
	}
	if attempted == 0 {
		switch {
		case len(o.WiFi) == 0:
			res.fault = "the ONU reports no Wi-Fi interface"
		default:
			res.fault = "the ONU did not say which of its Wi-Fi networks is 2.4 or 5 GHz (no band, standard or channel to tell by), so nothing was written"
		}
		return res, nil
	}
	if len(faults) > 0 {
		res.fault = strings.Join(faults, "; ")
		return res, nil
	}
	res.note = strings.Join(notes, "; ")
	_ = s.refreshSSIDs(ctx, ss, onuID)
	_ = s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		cur.State.AppliedFor, cur.State.AppliedAt = cur.AccountID, want.UpdatedAt
		cur.State.WiFiChanged = ssidDiffers(cur, homeWiFi(d, cur))
		d.ONUs[onuID] = cur
		return nil
	})
	return res, nil
}

// setBand writes one network and checks it.
func (s *Service) setBand(ctx context.Context, ss *session, onuID string, o store.ONU, prof Profile, w store.ONUWiFi, ssid, pass string, cands, secrets []string) (wifiResult, error) {
	var res wifiResult
	skipSec := map[string]bool{}
	withSec := true
	var last *Fault
	ci := 0
	for try := 0; try < 14 && ci < len(cands); try++ {
		leaf := cands[ci]
		ps := prof.wifiSettings(w, ssid, pass, leaf, withSec, skipSec)
		st, f, err := s.spvKeyed(ctx, ss, onuID, ps)
		if err != nil {
			return res, err
		}
		if f == nil {
			res.leaf, res.afterReboot = leaf, st == 1
			return s.verifyBand(ctx, ss, onuID, prof, w, ssid, withSec, skipSec, res)
		}
		last = f
		if !namingFault(f) {
			break // not a naming problem: no other name will help
		}
		refused := map[string]bool{}
		for _, pf := range f.Params {
			if pf.Name != "" {
				refused[pf.Name] = true
			}
		}
		switch {
		case refused[w.Path+prof.SSID]:
			res.fault = "the ONU refused the Wi-Fi name: " + f.SafeText(secrets...)
			return res, nil
		case refused[apPath(w)+leaf]:
			ci++ // this passphrase name is not the one: the next candidate (the name and the passphrase stay in the Set)
			withSec = true
		case len(refused) > 0:
			for n := range refused { // only security settings were named: drop those, keep everything else
				skipSec[n] = true
			}
		case withSec:
			withSec = false // nothing named: the name and the passphrase alone, security settings sorted out afterwards
		default:
			ci++
			withSec = true
		}
	}
	res.fault = "the ONU refused the Wi-Fi settings"
	if last != nil {
		res.fault = last.SafeText(secrets...)
	}
	return res, nil
}

// verifyBand reads the name and the security mode back after a Set the box accepted.
func (s *Service) verifyBand(ctx context.Context, ss *session, onuID string, prof Profile, w store.ONUWiFi, ssid string, sentSec bool, skipSec map[string]bool, res wifiResult) (wifiResult, error) {
	got, mode, err := s.readSecurity(ctx, ss, prof, w)
	if err != nil {
		return res, err
	}
	if got != "" && got != ssid {
		res.fault = "the ONU accepted the Wi-Fi settings but kept another name (" + got + ")"
		return res, nil
	}
	if mode == modeWeak || (!sentSec && mode != modeWPA2) {
		// Not WPA2-PSK: apply the security settings on their own (the first Set may have gone without them), then look again.
		var ps []ParamValue
		for _, sec := range prof.Security {
			ps = append(ps, ParamValue{Name: apPath(w) + sec.Leaf, Value: sec.Value, Type: sec.Type})
		}
		if _, _, err := s.setLoose(ctx, ss, onuID, ps); err != nil {
			return res, err
		}
		if _, mode, err = s.readSecurity(ctx, ss, prof, w); err != nil {
			return res, err
		}
	}
	switch mode {
	case modeWPA2:
	case modeWeak:
		res.fault = "the ONU accepted the Wi-Fi name and password but the network is not protected with WPA2 (its security mode could not be set): check it on the ONU before anyone relies on it"
	default:
		res.note = "the ONU's security mode could not be read back: join the network from a phone to check it"
	}
	return res, nil
}

// Security modes read back.
const (
	modeUnknown = iota
	modeWPA2
	modeWeak
)

// readSecurity reads the Wi-Fi name and the security mode of one network.
func (s *Service) readSecurity(ctx context.Context, ss *session, prof Profile, w store.ONUWiFi) (ssid string, mode int, err error) {
	var names []string
	if w.AP != "" { // TR-181
		names = []string{w.Path + prof.SSID, w.AP + "Security.ModeEnabled"}
	} else {
		names = []string{w.Path + prof.SSID, w.Path + "BeaconType", w.Path + "WPAAuthenticationMode", w.Path + "IEEE11iAuthenticationMode"}
	}
	vals, f, err := ss.gpv(ctx, names)
	if err != nil {
		return "", modeUnknown, err
	}
	if f != nil { // a name this box lacks: one at a time
		vals = nil
		for _, n := range names {
			one, f1, err := ss.gpv(ctx, []string{n})
			if err != nil {
				return "", modeUnknown, err
			}
			if f1 == nil {
				vals = append(vals, one...)
			}
		}
	}
	ssid = valueOf(vals, w.Path+prof.SSID)
	if w.AP != "" {
		m := valueOf(vals, w.AP+"Security.ModeEnabled")
		switch {
		case m == "":
			return ssid, modeUnknown, nil
		case strings.Contains(m, "WPA2-Personal"), strings.Contains(m, "WPA3-Personal"), m == "WPA-WPA2-Personal":
			return ssid, modeWPA2, nil
		}
		return ssid, modeWeak, nil
	}
	beacon := valueOf(vals, w.Path+"BeaconType")
	auth := valueOf(vals, w.Path+"IEEE11iAuthenticationMode")
	switch beacon {
	case "":
		return ssid, modeUnknown, nil
	case "11i", "WPAand11i", "WPA2":
		if auth != "" && !strings.Contains(auth, "PSK") {
			return ssid, modeWeak, nil // an enterprise (EAP) network
		}
		return ssid, modeWPA2, nil
	case "None", "Basic", "WPA":
		return ssid, modeWeak, nil
	}
	return ssid, modeUnknown, nil
}

// namingFault: the device says a name or value is not acceptable (9005-9008), either as the fault itself or, as many
// do, as the per-parameter detail of a 9003 "Invalid arguments".
func namingFault(f *Fault) bool {
	is := func(c int) bool { return c >= 9005 && c <= 9008 }
	if is(f.Code) {
		return true
	}
	for _, pf := range f.Params {
		if is(pf.Code) {
			return true
		}
	}
	return false
}
