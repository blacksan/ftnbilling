package acs

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/store"
)

// program is what the server does in one session with one ONU, after the Inform:
//
//  1. First contact, factory reset or boot: discover the Wi-Fi instances and WAN MACs (GetParameterNames, NextLevel).
//  2. First contact or factory reset (0 BOOTSTRAP): set the per-device connection-request login and the
//     periodic Inform.
//  3. A factory-reset ONU that is linked to a house with a saved Wi-Fi choice gets that choice back.
//  4. Run the queued tasks.
//
// A boot (1 BOOT) only refreshes what the ONU reports; it never writes the Wi-Fi configuration (the owner of the
// box may have changed it by hand).
func (s *Service) program(ctx context.Context, ss *session, in Inform, onuID string, stable bool) {
	o, ok := s.loadONU(onuID)
	if !ok {
		return
	}
	bootstrap := in.HasEvent("0")
	boot := in.HasEvent("1")
	needDiscover := !o.Profile.Discovered || bootstrap || boot || len(o.WiFi) == 0 || hasTask(s, onuID, store.TaskRefresh)
	if needDiscover && o.Root != "" {
		if err := s.discover(ctx, ss, onuID); err != nil {
			s.log.Info("ACS: discovery stopped", "onu", onuID, "err", err.Error())
			if ctx.Err() != nil {
				return
			}
		}
	}
	if o.Root != "" && (!o.CRApplied || bootstrap) {
		if err := s.maintain(ctx, ss, onuID); err != nil && ctx.Err() != nil {
			return
		}
	}
	if bootstrap {
		s.restoreWiFi(onuID)
	}
	s.runTasks(ctx, ss, onuID, stable)
}

func (s *Service) loadONU(id string) (o store.ONU, ok bool) {
	s.st.View(func(d *store.Data) { o, ok = d.ONUs[id]; o = store.CloneONU(o) })
	return
}

func hasTask(s *Service, onuID, kind string) bool {
	found := false
	s.st.View(func(d *store.Data) {
		for _, t := range d.ONUTasksOf(onuID) {
			if t.Kind == kind && t.Active() {
				found = true
			}
		}
	})
	return found
}

// restoreWiFi queues the saved Wi-Fi choice of a linked ONU that came back from a factory reset.
func (s *Service) restoreWiFi(onuID string) {
	_ = s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok || o.AccountID == "" || !d.Accounts[o.AccountID].HomeWiFi.Set {
			return store.ErrNoChange
		}
		t := queue(d, o, store.TaskWiFi, "system", s.now())
		if t.By == "system" {
			s.audit(d, o, "system", "", "ONU "+o.Serial+" started again from factory settings: the saved Wi-Fi name and password are being restored")
		}
		return nil
	})
}

// ---- discovery ----

// instances lists the numbered children of an object ("...WLANConfiguration." -> ".1.", ".2.").
func instances(ctx context.Context, ss *session, parent string) ([]string, error) {
	names, f, err := ss.gpn(ctx, parent, true)
	if err != nil {
		return nil, err
	}
	if f != nil {
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

func natLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func leafSet(ctx context.Context, ss *session, obj string) (map[string]bool, error) {
	names, f, err := ss.gpn(ctx, obj, true)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	if f != nil {
		return set, nil
	}
	for _, n := range names {
		if strings.HasPrefix(n.Name, obj) && n.Name != obj {
			set[strings.TrimSuffix(strings.TrimPrefix(n.Name, obj), ".")] = true
		}
	}
	return set, nil
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
	assignBands(wifi)
	canon := map[string]bool{}
	var cm []string
	for _, m := range macs {
		if c, ok := store.CanonMAC(m); ok && !canon[c] && c != "00:00:00:00:00:00" {
			canon[c] = true
			cm = append(cm, c)
		}
	}
	sort.Strings(cm)
	uerr := s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok {
			return store.ErrNoChange
		}
		cur.WiFi, cur.MACs = wifi, cm
		cur.Profile.Root, cur.Profile.Vendor, cur.Profile.Discovered = cur.Root, prof.Name, true
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
	lans, err := instances(ctx, ss, o.Root+"LANDevice.")
	if err != nil {
		return nil, nil, err
	}
	for _, lan := range lans {
		wls, err := instances(ctx, ss, lan+"WLANConfiguration.")
		if err != nil {
			return wifi, macs, err
		}
		for _, w := range wls {
			leaves, err := leafSet(ctx, ss, w)
			if err != nil {
				return wifi, macs, err
			}
			want := []string{prof.SSID, prof.Enable, prof.Channel, prof.Standard}
			want = append(want, prof.BandLeaves...)
			var names []string
			seen := map[string]bool{}
			for _, l := range want {
				if l == "" || seen[l] || (len(leaves) > 0 && !leaves[l]) {
					continue
				}
				seen[l] = true
				names = append(names, w+l)
			}
			vals, f, err := ss.gpv(ctx, names)
			if err != nil {
				return wifi, macs, err
			}
			if f != nil {
				continue
			}
			inst := store.ONUWiFi{Path: w, SSID: valueOf(vals, w+prof.SSID), Enabled: isOn(valueOf(vals, w+prof.Enable))}
			inst.Channel, _ = strconv.Atoi(valueOf(vals, w+prof.Channel))
			freq := ""
			for _, bl := range prof.BandLeaves {
				if bl != prof.Standard {
					if v := valueOf(vals, w+bl); v != "" {
						freq = v
					}
				}
			}
			inst.Band = bandOf(freq, valueOf(vals, w+prof.Standard), inst.Channel)
			wifi = append(wifi, inst)
		}
	}
	// WAN MACs: WANDevice.N.WANConnectionDevice.M.(WANIPConnection|WANPPPConnection).K.MACAddress
	wans, err := instances(ctx, ss, o.Root+"WANDevice.")
	if err != nil {
		return wifi, macs, err
	}
	for _, wan := range wans {
		wcds, err := instances(ctx, ss, wan+"WANConnectionDevice.")
		if err != nil {
			return wifi, macs, err
		}
		for _, wcd := range wcds {
			for _, kind := range []string{"WANIPConnection.", "WANPPPConnection."} {
				conns, err := instances(ctx, ss, wcd+kind)
				if err != nil {
					return wifi, macs, err
				}
				for _, c := range conns {
					vals, f, err := ss.gpv(ctx, []string{c + "MACAddress"})
					if err != nil {
						return wifi, macs, err
					}
					if f == nil {
						macs = append(macs, valueOf(vals, c+"MACAddress"))
					}
				}
			}
		}
	}
	return wifi, macs, nil
}

func (s *Service) discover181(ctx context.Context, ss *session, o store.ONU, prof Profile) (wifi []store.ONUWiFi, macs []string, err error) {
	radios, err := instances(ctx, ss, "Device.WiFi.Radio.")
	if err != nil {
		return nil, nil, err
	}
	band := map[string]string{}
	for _, r := range radios {
		vals, f, err := ss.gpv(ctx, []string{r + "OperatingFrequencyBand", r + "Channel"})
		if err != nil {
			return wifi, macs, err
		}
		if f == nil {
			ch, _ := strconv.Atoi(valueOf(vals, r+"Channel"))
			band[r] = bandOf(valueOf(vals, r+"OperatingFrequencyBand"), "", ch)
		}
	}
	ssids, err := instances(ctx, ss, "Device.WiFi.SSID.")
	if err != nil {
		return wifi, macs, err
	}
	aps, err := instances(ctx, ss, "Device.WiFi.AccessPoint.")
	if err != nil {
		return wifi, macs, err
	}
	apOf := map[string]string{}
	for _, ap := range aps {
		vals, f, err := ss.gpv(ctx, []string{ap + "SSIDReference"})
		if err != nil {
			return wifi, macs, err
		}
		if f == nil {
			ref := strings.TrimSuffix(valueOf(vals, ap+"SSIDReference"), ".") + "."
			apOf[ref] = ap
		}
	}
	for _, sp := range ssids {
		vals, f, err := ss.gpv(ctx, []string{sp + "SSID", sp + "Enable", sp + "LowerLayers"})
		if err != nil {
			return wifi, macs, err
		}
		if f != nil {
			continue
		}
		low := strings.TrimSuffix(valueOf(vals, sp+"LowerLayers"), ".") + "."
		wifi = append(wifi, store.ONUWiFi{Path: sp, AP: apOf[sp], SSID: valueOf(vals, sp+"SSID"), Enabled: isOn(valueOf(vals, sp+"Enable")), Band: band[low]})
	}
	links, err := instances(ctx, ss, "Device.Ethernet.Link.")
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

// assignBands gives instances whose band could not be read one by elimination (first unknown = 2.4, next = 5).
func assignBands(w []store.ONUWiFi) {
	have := map[string]bool{}
	for _, x := range w {
		have[x.Band] = true
	}
	for i := range w {
		if w[i].Band != "" {
			continue
		}
		switch {
		case !have["2.4"]:
			w[i].Band, have["2.4"] = "2.4", true
		case !have["5"]:
			w[i].Band, have["5"] = "5", true
		}
	}
}

// ---- connection-request login and periodic inform ----

func (s *Service) maintain(ctx context.Context, ss *session, onuID string) error {
	o, ok := s.loadONU(onuID)
	if !ok {
		return nil
	}
	user, pass, have := s.openCR(o)
	if !have {
		user, pass = "cr"+randHex(3), randAlnum(16)
		eu, ep, err := s.sealCR(user, pass)
		if err != nil {
			return err
		}
		if err := s.st.Update(func(d *store.Data) error {
			cur, ok := d.ONUs[onuID]
			if !ok {
				return store.ErrNoChange
			}
			cur.ConnReqUserEnc, cur.ConnReqPassEnc, cur.CRApplied = eu, ep, false
			d.ONUs[onuID] = cur
			return nil
		}); err != nil {
			return err
		}
	}
	ms := o.Root + "ManagementServer."
	every := strconv.Itoa(int(s.informEvery() / time.Second))
	ps := []ParamValue{
		{Name: ms + "ConnectionRequestUsername", Value: user},
		{Name: ms + "ConnectionRequestPassword", Value: pass},
		{Name: ms + "PeriodicInformEnable", Value: "1", Type: "xsd:boolean"},
		{Name: ms + "PeriodicInformInterval", Value: every, Type: "xsd:unsignedInt"},
	}
	_, f, err := ss.spv(ctx, ps, "ftn-mgmt")
	if err != nil {
		return err
	}
	if f != nil && f.Code == 9005 {
		// Some builds have no settable periodic-inform parameters: set the login alone.
		_, f, err = ss.spv(ctx, ps[:2], "ftn-mgmt")
		if err != nil {
			return err
		}
	}
	if f != nil {
		s.log.Info("ACS: could not set the connection-request login", "onu", onuID, "fault", f.Text())
		return nil
	}
	return s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok || cur.CRApplied {
			return store.ErrNoChange
		}
		cur.CRApplied = true
		d.ONUs[onuID] = cur
		return nil
	})
}

// ---- tasks ----

func (s *Service) nextTask(onuID string, skip map[string]bool) (store.ONUTask, bool) {
	var best store.ONUTask
	found := false
	s.st.View(func(d *store.Data) {
		for _, t := range d.ONUTasks {
			if t.ONUID != onuID || t.Status != store.TaskPending || skip[t.ID] {
				continue
			}
			if !found || t.CreatedAt.Before(best.CreatedAt) || (t.CreatedAt.Equal(best.CreatedAt) && t.ID < best.ID) {
				best, found = t, true
			}
		}
	})
	return best, found
}

func (s *Service) setTask(id string, f func(*store.ONUTask)) {
	_ = s.st.Update(func(d *store.Data) error {
		t, ok := d.ONUTasks[id]
		if !ok {
			return store.ErrNoChange
		}
		f(&t)
		d.ONUTasks[id] = t
		return nil
	})
}

func (s *Service) finish(id, fault string) {
	now := s.now()
	s.setTask(id, func(t *store.ONUTask) {
		t.FinishedAt = now
		if fault == "" {
			t.Status, t.Fault = store.TaskDone, ""
		} else {
			t.Status, t.Fault = store.TaskFailed, fault
		}
	})
}

func (s *Service) runTasks(ctx context.Context, ss *session, onuID string, stable bool) {
	skipped := map[string]bool{}
	for ctx.Err() == nil {
		t, ok := s.nextTask(onuID, skipped)
		if !ok {
			return
		}
		if t.Kind == store.TaskWiFi && !stable {
			// The password goes only to a box that has been seen at this address before: a new address may be
			// another customer's box using a stolen serial number. The next Inform from the same address sends it.
			skipped[t.ID] = true
			s.log.Info("ACS: Wi-Fi change waits: the ONU's address is new", "onu", onuID)
			continue
		}
		now := s.now()
		s.setTask(t.ID, func(x *store.ONUTask) { x.Status, x.StartedAt, x.Attempts = store.TaskRunning, now, x.Attempts+1 })
		var fault string
		var err error
		switch t.Kind {
		case store.TaskWiFi:
			fault, err = s.doWiFi(ctx, ss, onuID, t)
		case store.TaskReboot:
			fault, err = s.doSimple(ctx, ss, func(id string) []byte { return rebootRPC(ss.ns, id, t.ID) }, "RebootResponse")
		case store.TaskFactoryReset:
			fault, err = s.doSimple(ctx, ss, func(id string) []byte { return factoryResetRPC(ss.ns, id) }, "FactoryResetResponse")
		case store.TaskRefresh:
			err = s.refreshSSIDs(ctx, ss, onuID)
		default:
			fault = "unknown task"
		}
		if err != nil {
			if ctx.Err() != nil {
				return // the session died: the task is picked up again at the next Inform
			}
			fault = "the ONU did not answer properly (" + err.Error() + ")"
		}
		s.finish(t.ID, fault)
		if t.Kind == store.TaskReboot || t.Kind == store.TaskFactoryReset {
			return // the ONU restarts: nothing more in this session
		}
	}
}

func (s *Service) doSimple(ctx context.Context, ss *session, build func(id string) []byte, want string) (string, error) {
	f, err := ss.simple(ctx, build, want)
	if err != nil {
		return "", err
	}
	if f != nil {
		return f.Text(), nil
	}
	return "", nil
}

// doWiFi writes the saved Wi-Fi choice of the ONU: the name and the WPA2 passphrase of the main instance of each
// band. A name the device refuses (9005) moves to the next candidate of the profile; the one that works is remembered.
func (s *Service) doWiFi(ctx context.Context, ss *session, onuID string, t store.ONUTask) (string, error) {
	o, ok := s.loadONU(onuID)
	if !ok {
		return "the ONU is gone", nil
	}
	var want store.WiFiDesired
	s.st.View(func(d *store.Data) { want = homeWiFi(d, o) })
	if !want.Set {
		return "no Wi-Fi name and password are saved for this house", nil
	}
	pass, err := s.openPass(want.PassEnc)
	if err != nil {
		return "the saved Wi-Fi password cannot be opened", nil
	}
	pass5, ssid5 := pass, want.SSID
	if !want.Same {
		ssid5 = want.SSID5
		if pass5, err = s.openPass(want.Pass5Enc); err != nil {
			return "the saved 5 GHz Wi-Fi password cannot be opened", nil
		}
	}
	if len(o.WiFi) == 0 {
		if err := s.discover(ctx, ss, onuID); err != nil && ctx.Err() != nil {
			return "", err
		}
		if o, ok = s.loadONU(onuID); !ok {
			return "the ONU is gone", nil
		}
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	idx := 0
	type target struct {
		band, ssid, pass string
	}
	var faults []string
	attempted := 0
	for _, tg := range []target{{"2.4", want.SSID, pass}, {"5", ssid5, pass5}} {
		w, ok := primary(o.WiFi, tg.band)
		if !ok {
			continue
		}
		attempted++
		skip := map[string]bool{}
		withSec := true
		done := false
		var last *Fault
		for try := 0; try < 8 && !done; try++ {
			ps := prof.wifiSettings(w, tg.ssid, tg.pass, idx, withSec, skip)
			_, f, err := ss.spv(ctx, ps, t.ID)
			if err != nil {
				return "", err
			}
			if f == nil {
				done = true
				break
			}
			last = f
			if !namingFault(f) {
				break // not a naming problem: no candidate will help
			}
			// Drop what the device named, if it named something; else the security settings first, then the next passphrase name.
			named := false
			for _, pf := range f.Params {
				if pf.Name != "" && !skip[pf.Name] {
					skip[pf.Name] = true
					named = true
				}
			}
			if named {
				if passRefused(prof, w, idx, skip) {
					idx++ // what the device refused stays dropped; only the passphrase name changes
				}
			} else if withSec {
				withSec = false
			} else {
				idx++
			}
			if idx >= len(prof.Pass) {
				break
			}
		}
		if !done {
			text := "the ONU refused the Wi-Fi settings"
			if last != nil {
				text = last.Text()
			}
			faults = append(faults, tg.band+" GHz: "+text)
		}
	}
	if attempted == 0 {
		return "the ONU reports no Wi-Fi interface", nil
	}
	if len(faults) > 0 {
		return strings.Join(faults, "; "), nil
	}
	_ = s.refreshSSIDs(ctx, ss, onuID)
	_ = s.st.Update(func(d *store.Data) error {
		cur, ok := d.ONUs[onuID]
		if !ok || !cur.State.WiFiChanged {
			return store.ErrNoChange
		}
		cur.State.WiFiChanged = ssidDiffers(cur, homeWiFi(d, cur))
		d.ONUs[onuID] = cur
		return nil
	})
	return "", nil
}

// passRefused: the device named the passphrase parameter of the current candidate as refused.
func passRefused(p Profile, w store.ONUWiFi, idx int, skip map[string]bool) bool {
	return idx < len(p.Pass) && skip[apPath(w)+p.Pass[idx]]
}

// refreshSSIDs reads the Wi-Fi names the ONU has now.
func (s *Service) refreshSSIDs(ctx context.Context, ss *session, onuID string) error {
	o, ok := s.loadONU(onuID)
	if !ok {
		return nil
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	for _, w := range o.WiFi {
		vals, f, err := ss.gpv(ctx, []string{w.Path + prof.SSID, w.Path + prof.Enable})
		if err != nil {
			return err
		}
		if f != nil {
			continue
		}
		ssid, en := valueOf(vals, w.Path+prof.SSID), isOn(valueOf(vals, w.Path+prof.Enable))
		path := w.Path
		_ = s.st.Update(func(d *store.Data) error {
			cur, ok := d.ONUs[onuID]
			if !ok {
				return store.ErrNoChange
			}
			for i := range cur.WiFi {
				if cur.WiFi[i].Path == path {
					cur.WiFi[i].SSID, cur.WiFi[i].Enabled = ssid, en
				}
			}
			cur.State.WiFiChanged = ssidDiffers(cur, homeWiFi(d, cur))
			d.ONUs[onuID] = cur
			return nil
		})
	}
	return nil
}

// namingFault: the device says a name is unknown (9005-9008), either as the fault itself or, as many do, as the
// per-parameter detail of a 9003 "Invalid arguments".
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
