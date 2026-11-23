package acs

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"ispbill/internal/store"
)

// Discovery: which Wi-Fi networks the box has (and which band each is) and its WAN MACs.
//
// The band of a network comes from what the box reports about it (its frequency band, its standards, its channel
// or the channels it may use), never from the position of the instance: an ONU may number its 5 GHz radio 1, 5 or 9.
//
// Wi-Fi is read with one partial-path GetParameterValues per object ("...WLANConfiguration.") instead of one request
// per leaf. A box that has no GetParameterNames (some Nokia ONUs) is probed with GetParameterValues on the known paths.

// maxProbe is how many instance numbers are tried when the box cannot list its objects.
const maxProbe = 8

// listInst lists the numbered children of an object ("...WLANConfiguration." -> ".1.", ".5."). A box that does not
// implement GetParameterNames is probed instead.
func (s *Service) listInst(ctx context.Context, ss *session, parent, probeLeaf string) ([]string, error) {
	names, f, err := ss.gpn(ctx, parent, true)
	if err != nil {
		return nil, err
	}
	if f != nil {
		if f.Code == 9000 || f.Code == 9001 { // not implemented / refused: probe the known paths
			return s.probeInst(ctx, ss, parent, probeLeaf)
		}
		return nil, nil // no such object (9005): none
	}
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n.Name, parent) || n.Name == parent {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(n.Name, parent), ".")
		if rest == "" || strings.Contains(rest, ".") {
			continue
		}
		if _, err := strconv.Atoi(rest); err != nil {
			continue
		}
		out = append(out, parent+rest+".")
	}
	sort.Slice(out, func(i, j int) bool { return natLess(out[i], out[j]) })
	if len(out) > 16 {
		out = out[:16]
	}
	return out, nil
}

// probeInst finds instances 1..maxProbe by asking for them.
func (s *Service) probeInst(ctx context.Context, ss *session, parent, leaf string) ([]string, error) {
	var out []string
	for n := 1; n <= maxProbe; n++ {
		p := parent + strconv.Itoa(n) + "."
		_, f, err := ss.gpv(ctx, []string{p})
		if err != nil {
			return out, err
		}
		if f != nil && leaf != "" { // some boxes answer only for a leaf name
			_, f, err = ss.gpv(ctx, []string{p + leaf})
			if err != nil {
				return out, err
			}
		}
		if f == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func natLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func valueOf(vs []ParamValue, name string) string {
	for _, v := range vs {
		if v.Name == name {
			return v.Value
		}
	}
	return ""
}

func isOn(v string) bool { return isTrue(v) }

// groupInst sorts the values of a partial-path read into instances: base "X.WLANConfiguration." and the name
// "X.WLANConfiguration.5.PreSharedKey.1.KeyPassphrase" give instance 5, leaf "PreSharedKey.1.KeyPassphrase".
// Values of secret leaves are dropped here: nothing that is read for discovery keeps a passphrase.
func groupInst(vals []ParamValue, base string) (order []string, rows map[string]map[string]string) {
	rows = map[string]map[string]string{}
	for _, v := range vals {
		if !strings.HasPrefix(v.Name, base) {
			continue
		}
		rest := v.Name[len(base):]
		num, leaf, ok := strings.Cut(rest, ".")
		if !ok || leaf == "" {
			continue
		}
		if _, err := strconv.Atoi(num); err != nil {
			continue
		}
		r := rows[num]
		if r == nil {
			r = map[string]string{}
			rows[num] = r
			order = append(order, num)
		}
		if !Sensitive(leaf) {
			r[leaf] = v.Value
		} else {
			r[leaf] = "" // the leaf exists; its value is never kept
		}
	}
	sort.Slice(order, func(i, j int) bool { return natLess(order[i], order[j]) })
	if len(order) > 16 {
		order = order[:16]
	}
	return order, rows
}

// discover reads the Wi-Fi instances and the WAN MACs of the ONU and saves them on its row.
func (s *Service) discover(ctx context.Context, ss *session, onuID string) error {
	o, ok := s.loadONU(onuID)
	if !ok {
		return nil
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	var wifi []store.ONUWiFi
	var macs []string
	var err error
	if o.Root == store.RootTR181 {
		wifi, macs, err = s.discover181(ctx, ss, o, prof)
	} else {
		wifi, macs, err = s.discover098(ctx, ss, o, prof)
	}
	if err != nil && len(wifi) == 0 && len(macs) == 0 {
		return err
	}
	canon := map[string]bool{}
	var cm []string
	for _, m := range macs {
		if c, ok := store.CanonMAC(m); ok && !canon[c] && c != "00:00:00:00:00:00" {
			canon[c] = true
			cm = append(cm, c)
		}
	}
	sort.Strings(cm)
	complete := err == nil
	uerr := s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		cur.WiFi, cur.MACs = wifi, cm
		cur.Profile.Root, cur.Profile.Vendor, cur.Profile.Discovered = cur.Root, prof.Name, true
		if complete {
			cur.State.NoWiFi = len(wifi) == 0 // an SFU or bridge: not discovered again at every Inform
		}
		cur.State.WiFiChanged = ssidDiffers(cur, homeWiFi(d, cur))
		d.ONUs[onuID] = cur
		autoLink(d, onuID, s.now())
		return nil
	})
	if uerr != nil && !errors.Is(uerr, store.ErrNoChange) {
		return uerr
	}
	return err
}

func (s *Service) discover098(ctx context.Context, ss *session, o store.ONU, prof Profile) (wifi []store.ONUWiFi, macs []string, err error) {
	lans, err := s.listInst(ctx, ss, o.Root+"LANDevice.", "")
	if err != nil {
		return nil, nil, err
	}
	for _, lan := range lans {
		base := lan + "WLANConfiguration."
		vals, f, err := ss.gpv(ctx, []string{base})
		if err != nil {
			return wifi, macs, err
		}
		if f != nil {
			// The box refuses a partial path: ask per instance.
			if vals, err = s.readWLANPerInstance(ctx, ss, base, prof); err != nil {
				return wifi, macs, err
			}
		}
		order, rows := groupInst(vals, base)
		for _, n := range order {
			r := rows[n]
			if _, has := r[prof.SSID]; !has {
				continue
			}
			inst := store.ONUWiFi{Path: base + n + ".", SSID: r[prof.SSID], Enabled: isOn(r[prof.Enable])}
			inst.Channel, _ = strconv.Atoi(r[prof.Channel])
			inst.Band = bandOf(bandHints(r, prof), inst.Channel)
			wifi = append(wifi, inst)
		}
	}
	macs, err = s.wanMACs098(ctx, ss, o.Root)
	return wifi, macs, err
}

// bandHints collects what a Wi-Fi instance says about its band, from the leaves of the profile.
func bandHints(r map[string]string, prof Profile) bandInfo {
	var b bandInfo
	for _, bl := range prof.BandLeaves {
		if bl == prof.Standard || bl == "OperatingStandards" {
			continue
		}
		if v := r[bl]; v != "" {
			b.Freq = v
		}
	}
	b.Standard = nonEmpty(r[prof.Standard], r["OperatingStandards"])
	b.Possible = r["PossibleChannels"]
	return b
}

// readWLANPerInstance is the fallback of a box that refuses "...WLANConfiguration." as a path.
func (s *Service) readWLANPerInstance(ctx context.Context, ss *session, base string, prof Profile) ([]ParamValue, error) {
	insts, err := s.listInst(ctx, ss, base, prof.SSID)
	if err != nil {
		return nil, err
	}
	var out []ParamValue
	for _, w := range insts {
		names := []string{w + prof.SSID, w + prof.Enable, w + prof.Channel, w + prof.Standard, w + "PossibleChannels"}
		for _, bl := range prof.BandLeaves {
			names = append(names, w+bl)
		}
		got, f, err := ss.gpv(ctx, uniq(names))
		if err != nil {
			return out, err
		}
		if f != nil { // a name this box does not have: ask for the leaves one at a time
			for _, n := range uniq(names) {
				one, f1, err := ss.gpv(ctx, []string{n})
				if err != nil {
					return out, err
				}
				if f1 == nil {
					out = append(out, one...)
				}
			}
			continue
		}
		out = append(out, got...)
	}
	return out, nil
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// wanMACs098: WANDevice.N.WANConnectionDevice.M.(WANIPConnection|WANPPPConnection).K.MACAddress
func (s *Service) wanMACs098(ctx context.Context, ss *session, root string) (macs []string, err error) {
	wans, err := s.listInst(ctx, ss, root+"WANDevice.", "")
	if err != nil {
		return nil, err
	}
	for _, wan := range wans {
		wcdBase := wan + "WANConnectionDevice."
		names, f, err := ss.gpn(ctx, wcdBase, true)
		if err != nil {
			return macs, err
		}
		if f != nil && (f.Code == 9000 || f.Code == 9001) {
			// No GetParameterNames: one partial read of the connection devices, keep the MAC leaves.
			vals, f2, err := ss.gpv(ctx, []string{wcdBase})
			if err != nil {
				return macs, err
			}
			if f2 == nil {
				for _, v := range vals {
					if strings.HasSuffix(v.Name, ".MACAddress") {
						macs = append(macs, v.Value)
					}
				}
			}
			continue
		}
		_ = names
		wcds, err := s.listInst(ctx, ss, wcdBase, "")
		if err != nil {
			return macs, err
		}
		for _, wcd := range wcds {
			for _, kind := range []string{"WANIPConnection.", "WANPPPConnection."} {
				conns, err := s.listInst(ctx, ss, wcd+kind, "")
				if err != nil {
					return macs, err
				}
				for _, c := range conns {
					vals, f, err := ss.gpv(ctx, []string{c + "MACAddress"})
					if err != nil {
						return macs, err
					}
					if f == nil {
						macs = append(macs, valueOf(vals, c+"MACAddress"))
					}
				}
			}
		}
	}
	return macs, nil
}

func (s *Service) discover181(ctx context.Context, ss *session, o store.ONU, prof Profile) (wifi []store.ONUWiFi, macs []string, err error) {
	// Radios: the frequency band, the standards and the channels say which band an SSID is on.
	radios := map[string]bandInfo{} // radio path -> hints
	rvals, f, err := ss.gpv(ctx, []string{"Device.WiFi.Radio."})
	if err != nil {
		return nil, nil, err
	}
	if f != nil {
		insts, err := s.listInst(ctx, ss, "Device.WiFi.Radio.", "Channel")
		if err != nil {
			return nil, nil, err
		}
		for _, r := range insts {
			got, f1, err := ss.gpv(ctx, []string{r + "OperatingFrequencyBand", r + "OperatingStandards", r + "Channel", r + "PossibleChannels"})
			if err != nil {
				return nil, nil, err
			}
			if f1 != nil { // a leaf this box lacks
				for _, n := range []string{"OperatingFrequencyBand", "OperatingStandards", "Channel", "PossibleChannels"} {
					one, f2, err := ss.gpv(ctx, []string{r + n})
					if err != nil {
						return nil, nil, err
					}
					if f2 == nil {
						got = append(got, one...)
					}
				}
			}
			rvals = append(rvals, got...)
		}
	}
	order, rows := groupInst(rvals, "Device.WiFi.Radio.")
	radioChan := map[string]int{}
	for _, n := range order {
		r := rows[n]
		path := "Device.WiFi.Radio." + n + "."
		radios[path] = bandInfo{Freq: r["OperatingFrequencyBand"], Standard: r["OperatingStandards"], Possible: r["PossibleChannels"]}
		radioChan[path], _ = strconv.Atoi(r["Channel"])
	}
	apOf := map[string]string{}
	avals, f, err := ss.gpv(ctx, []string{"Device.WiFi.AccessPoint."})
	if err != nil {
		return nil, nil, err
	}
	if f != nil {
		insts, err := s.listInst(ctx, ss, "Device.WiFi.AccessPoint.", "SSIDReference")
		if err != nil {
			return nil, nil, err
		}
		avals = nil
		for _, ap := range insts {
			got, f1, err := ss.gpv(ctx, []string{ap + "SSIDReference"})
			if err != nil {
				return nil, nil, err
			}
			if f1 == nil {
				avals = append(avals, got...)
			}
		}
	}
	aorder, arows := groupInst(avals, "Device.WiFi.AccessPoint.")
	for _, n := range aorder {
		ref := strings.TrimSuffix(arows[n]["SSIDReference"], ".") + "."
		apOf[ref] = "Device.WiFi.AccessPoint." + n + "."
	}
	svals, f, err := ss.gpv(ctx, []string{"Device.WiFi.SSID."})
	if err != nil {
		return nil, nil, err
	}
	if f != nil {
		insts, err := s.listInst(ctx, ss, "Device.WiFi.SSID.", "SSID")
		if err != nil {
			return nil, nil, err
		}
		svals = nil
		for _, sp := range insts {
			got, f1, err := ss.gpv(ctx, []string{sp + "SSID", sp + "Enable", sp + "LowerLayers"})
			if err != nil {
				return nil, nil, err
			}
			if f1 == nil {
				svals = append(svals, got...)
			}
		}
	}
	sorder, srows := groupInst(svals, "Device.WiFi.SSID.")
	for _, n := range sorder {
		r := srows[n]
		if _, has := r["SSID"]; !has {
			continue
		}
		sp := "Device.WiFi.SSID." + n + "."
		low := strings.TrimSuffix(r["LowerLayers"], ".") + "."
		hint := radios[low]
		wifi = append(wifi, store.ONUWiFi{Path: sp, AP: apOf[sp], SSID: r["SSID"], Enabled: isOn(r["Enable"]), Channel: radioChan[low], Band: bandOf(hint, radioChan[low])})
	}
	links, err := s.listInst(ctx, ss, "Device.Ethernet.Link.", "MACAddress")
	if err != nil {
		return wifi, macs, err
	}
	for _, l := range links {
		vals, f, err := ss.gpv(ctx, []string{l + "MACAddress"})
		if err != nil {
			return wifi, macs, err
		}
		if f == nil {
			macs = append(macs, valueOf(vals, l+"MACAddress"))
		}
	}
	return wifi, macs, nil
}
