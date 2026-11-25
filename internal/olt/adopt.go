package olt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"ispbill/internal/store"
)

// Adoption of an OLT through the router's OLT management port.
//
// The owner has the OLT in the inventory (serial, MAC, router) and cables its management port to the router port that has
// the role "OLT management". The router (internal/sites/oltadopt.go) lets only the server reach the OLT's SSH. This file
// is the server's side: it sees the OLT's MAC on that port, connects over SSH (host key pinned on first use), reads
// `show version` and adopts the OLT only when the serial is the inventory serial and the model is a V1600. A device that
// is not this OLT is listed as a "Possible OLT" (New devices) with a rate-limited alert and is never configured.
//
// Logins: the stored login of the OLT; with none, the vendor's factory login once; when that works the OLT is secured
// at once (secure.go). When no login works the owner is asked on the OLT page.

// The vendor's published factory login of a V-SOL OLT. It is only ever sent to a device that is on the OLT management
// port and has the inventory OLT's MAC, and it is never shown, logged or put in an alert, an audit entry or an error.
const (
	factoryUser = "admin"
	factoryPass = "Xpon@Olt9417#"
)

// factoryIP is where a factory OLT answers.
const factoryIP = "192.168.8.200"

// PortSeen is what the router shows on its OLT management port.
type PortSeen struct {
	Port string
	Rows []SeenMAC
}

// SeenMAC is one device on the port (MAC upper case with colons).
type SeenMAC struct{ MAC, IP, Status string }

// Errors of the adoption.
var (
	ErrNoPort        = errors.New("the router has no OLT management port")
	ErrNotIdentified = errors.New("the server has not identified this OLT yet: it must be seen on the OLT management port and its serial must match the inventory")
	ErrAdoptOff      = errors.New("adoption through the OLT management port is not set up on this server")
)

// IdentityOK reports whether `show version` is this inventory OLT: the same serial and a V1600 model.
func IdentityOK(e store.Equipment, vi VersionInfo) (bool, string) {
	inv := NormSerial(e.Serial)
	if inv == "" {
		return false, "the inventory has no serial number for this OLT, so it cannot be told from another device"
	}
	if vi.Serial == "" {
		return false, "the device did not print a serial number"
	}
	if NormSerial(vi.Serial) != inv {
		return false, "the serial number the device prints is not the one in the inventory"
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(vi.Model)), "V1600") {
		return false, "the device's model is not a V1600"
	}
	return true, ""
}

func (s *Service) setAdopt(id string, fn func(a *store.OLTAdopt, e *store.Equipment, d *store.Data)) error {
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		fn(&st.Settings.Adopt, &e, d)
		d.Equipment[id] = e
	})
}

// ---- the pass over the sites ---------------------------------------------------------------------------------------

// AdoptAll runs one adoption pass for every router that has an OLT management port.
func (s *Service) AdoptAll(ctx context.Context) {
	if s.o.PortSeen == nil {
		return
	}
	var ids []string
	s.o.Store.View(func(d *store.Data) {
		for id, st := range d.Sites {
			if !st.Removed && st.OLTMgmtPort != "" {
				ids = append(ids, id)
			}
		}
	})
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.AdoptSite(ctx, id, "system"); err != nil {
			s.o.Log.Warn("OLT adoption pass failed", "site", id, "err", errText(err))
		}
	}
}

// AdoptSite runs one pass for a router: heals the router's rules, looks at the port and adopts the inventory OLT that is
// on it. by is the actor ("system" for the timer).
func (s *Service) AdoptSite(ctx context.Context, siteID, by string) error {
	if s.o.PortSeen == nil {
		return ErrAdoptOff
	}
	var port string
	var olts []store.Equipment
	var allMACs map[string]string
	s.o.Store.View(func(d *store.Data) {
		st, ok := d.Sites[siteID]
		if !ok || st.Removed {
			return
		}
		port = st.OLTMgmtPort
		olts = d.OLTsOfSite(siteID)
		allMACs = map[string]string{}
		for _, o := range d.Equipment {
			if o.Kind == store.KindOLT && o.Status != store.EqRetired && o.MAC != "" {
				allMACs[strings.ToUpper(o.MAC)] = o.ID
			}
		}
	})
	if port == "" {
		return ErrNoPort
	}
	// Heal the router's side first (the rules and the address); a failure is the finding.
	if s.o.EnsurePort != nil {
		if err := s.o.EnsurePort(ctx, siteID, by); err != nil {
			for _, o := range olts {
				s.markAdopt(o.ID, store.AdoptFailed, "The router could not be set up for the OLT management port: "+errText(err), "")
			}
			return err
		}
	}
	seen, err := s.o.PortSeen(ctx, siteID)
	if err != nil {
		for _, o := range olts {
			s.markAdopt(o.ID, store.AdoptFailed, "The router could not be read: "+errText(err), "")
		}
		return err
	}
	// Nothing on the port yet: knock once through the forwarded port, so the router asks the OLT for its MAC (ARP).
	if len(seen.Rows) == 0 {
		for _, o := range olts {
			// An OLT with no address yet is a factory one: Reach dials the factory address through the port.
			if s.adoptedOrManual(o.ID) {
				continue
			}
			s.probe(ctx, o.ID)
		}
		select {
		case <-time.After(s.probeWait()):
		case <-ctx.Done():
			return ctx.Err()
		}
		if again, err := s.o.PortSeen(ctx, siteID); err == nil {
			seen = again
		}
	}
	macs := map[string]SeenMAC{}
	for _, r := range seen.Rows {
		macs[strings.ToUpper(r.MAC)] = r
	}
	// A device that is no OLT of the inventory: listed as a possible OLT, never configured.
	for mac, r := range macs {
		if _, ours := allMACs[mac]; !ours {
			s.notePossible(siteID, mac, r.IP, port, "a device on the OLT management port is not an OLT of the inventory")
		}
	}
	var firstErr error
	for _, o := range olts {
		if s.adoptedOrManual(o.ID) {
			continue
		}
		r, there := macs[strings.ToUpper(o.MAC)]
		if !there {
			stage, note := store.AdoptNoCable, "Connect the OLT's management port (GE0/2) to "+port+" on the router, power it on and wait a minute."
			var other []string
			for m := range macs {
				if _, ours := allMACs[m]; !ours {
					other = append(other, m)
				}
			}
			sort.Strings(other)
			if len(other) > 0 {
				stage, note = store.AdoptUnknownMAC, "Another device is on "+port+" (listed under New devices as a possible OLT), not this OLT. Check the OLT's MAC in the inventory."
			}
			s.setAdopt(o.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
				a.Stage, a.Note, a.Error, a.Port, a.MACSeen, a.OtherMACs, a.CheckedAt = stage, note, "", port, false, other, s.now()
			})
			continue
		}
		s.setAdopt(o.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
			a.Port, a.MACSeen, a.MACSeenAt = port, true, s.now()
		})
		_ = r
		if err := s.adoptOLT(ctx, o.ID, by); err != nil && !errors.Is(err, ErrBusy) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// adoptedOrManual: the OLT is already adopted (the normal poll takes over) or the owner chose the console snippet.
func (s *Service) adoptedOrManual(id string) bool {
	_, st, ok := s.Load(id)
	return ok && (st.Settings.Adopt.Adopted || st.Settings.Adopt.Manual)
}

func (s *Service) probeWait() time.Duration {
	if s.o.ProbeWait > 0 {
		return s.o.ProbeWait
	}
	return 1500 * time.Millisecond
}

// probe opens a TCP connection to the OLT's forwarded SSH port and closes it. It makes the router ask for the OLT's MAC.
func (s *Service) probe(ctx context.Context, id string) {
	var addr string
	var rerr error
	s.o.Store.View(func(d *store.Data) {
		e, ok := d.Equipment[id]
		if !ok || s.o.Reach == nil {
			rerr = ErrNotFound
			return
		}
		addr, rerr = s.o.Reach(d, e, d.OLTStateOf(id).Settings)
	})
	if rerr != nil {
		return
	}
	dial := s.o.Dial
	if dial == nil {
		nd := net.Dialer{Timeout: 3 * time.Second}
		dial = nd.DialContext
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c, err := dial(cctx, "tcp", addr); err == nil {
		_ = c.Close()
	}
}

// markAdopt records a stage and its note.
func (s *Service) markAdopt(id, stage, note, errMsg string) {
	_ = s.setAdopt(id, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
		a.Stage, a.Note, a.Error, a.CheckedAt = stage, note, errMsg, s.now()
	})
}

// ---- possible OLTs -------------------------------------------------------------------------------------------------

// notePossible lists a device of the OLT management port under New devices as a "Possible OLT" and, at most once a day per
// device, tells the owners. Nothing is configured on it.
func (s *Service) notePossible(siteID, mac, ip, port, why string) {
	vendor := ""
	if s.o.Vendor != nil {
		vendor = s.o.Vendor(mac)
	}
	now := s.now()
	_ = s.o.Store.Update(func(d *store.Data) error {
		key := store.DeviceKey(siteID, mac)
		dev, had := d.Devices[key]
		if had && dev.Source == "dismissed" {
			return store.ErrNoChange
		}
		if !had {
			dev = store.Device{SiteID: siteID, MAC: mac, FirstSeen: now}
		}
		if !had || !dev.Online || dev.Source != store.DeviceSourceOLTMgmt {
			dev.Hits++
		}
		dev.Source, dev.Online, dev.LastSeen, dev.Port, dev.Vendor = store.DeviceSourceOLTMgmt, true, now, port, vendor
		if ip != "" {
			dev.IP = ip
		}
		d.Devices[key] = dev
		return nil
	})
	s.alertOnce("possible|"+siteID+"|"+mac, "Possible OLT on a router's OLT management port",
		fmt.Sprintf("A device with MAC %s%s is on the OLT management port %s of the router: %s. It is listed under New devices as a possible OLT. The server did not configure it.",
			mac, vendorSuffix(vendor), port, why))
}

func vendorSuffix(v string) string {
	if v == "" {
		return ""
	}
	return " (" + v + ")"
}

// clearPossible forgets the possible-OLT listing of a MAC (it turned out to be the inventory OLT).
func (s *Service) clearPossible(siteID, mac string) {
	_ = s.o.Store.Update(func(d *store.Data) error {
		key := store.DeviceKey(siteID, mac)
		if dev, ok := d.Devices[key]; ok && dev.Source == store.DeviceSourceOLTMgmt {
			delete(d.Devices, key)
			return nil
		}
		return store.ErrNoChange
	})
}

// alertOnce tells the owners at most once per day for a key.
func (s *Service) alertOnce(key, subject, msg string) {
	s.mu.Lock()
	if s.alerted == nil {
		s.alerted = map[string]time.Time{}
	}
	if t, ok := s.alerted[key]; ok && s.now().Sub(t) < 24*time.Hour {
		s.mu.Unlock()
		return
	}
	s.alerted[key] = s.now()
	s.mu.Unlock()
	if s.o.Alert != nil {
		s.o.Alert(subject, clip(msg, 900))
	}
}

// ---- adopting one OLT ----------------------------------------------------------------------------------------------

// AdoptCheck is the "Check now" of the OLT page: one adoption pass for the router of this OLT.
func (s *Service) AdoptCheck(ctx context.Context, id, by string) error {
	e, _, ok := s.Load(id)
	if !ok {
		return ErrNotFound
	}
	if e.SiteID == "" {
		s.markAdopt(id, store.AdoptNoRouter, "Put the OLT on a router first (edit the OLT).", "")
		return ErrNoRouter
	}
	var port string
	s.o.Store.View(func(d *store.Data) { port = d.Sites[e.SiteID].OLTMgmtPort })
	if port == "" {
		s.markAdopt(id, store.AdoptNoPort, "Give a spare router port the role OLT management, then cable the OLT's management port to it.", "")
		return ErrNoPort
	}
	return s.AdoptSite(ctx, e.SiteID, by)
}

// credCandidates are the logins to try, in order. The vendor's factory login only when the OLT has no stored login and the
// server has not tried it before (or, for the secure action, when it is known to work).
func (s *Service) credCandidates(id string, forSecure bool) []cred {
	e, st, ok := s.Load(id)
	if !ok {
		return nil
	}
	var cc []cred
	s.o.Store.View(func(d *store.Data) { e = d.Equipment[id] })
	a := st.Settings.Adopt
	if e.MgmtUser != "" && e.MgmtPass != "" && s.o.Box != nil {
		if pass, err := s.o.Box.OpenAAD(e.MgmtPass, mgmtAAD); err == nil {
			c := cred{User: e.MgmtUser, Pass: pass, Source: "stored"}
			if st.Settings.EnablePass != "" {
				c.Enable, _ = s.o.Box.OpenAAD(st.Settings.EnablePass, enableAAD)
			}
			cc = append(cc, c)
		}
	}
	if a.PendingPass != "" && s.o.Box != nil {
		if pass, err := s.o.Box.OpenAAD(a.PendingPass, mgmtAAD); err == nil {
			c := cred{User: firstNonEmpty(e.MgmtUser, factoryUser), Pass: pass, Source: "pending"}
			if a.PendingEn != "" {
				c.Enable, _ = s.o.Box.OpenAAD(a.PendingEn, enableAAD)
			}
			cc = append(cc, c)
		}
	}
	noStored := len(cc) == 0 || (len(cc) == 1 && cc[0].Source == "pending")
	if noStored && (((a.FactoryAt.IsZero() || a.FactoryOK) && !forSecure) || (forSecure && a.FactoryOK)) {
		cc = append(cc, cred{User: factoryUser, Pass: factoryPass, Source: "factory"})
	}
	for i := range cc {
		cc[i].NoPin = true // the host key is pinned only after the serial check (identify)
	}
	return cc
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// adoptOLT connects to an OLT whose MAC is on the port, identifies it and, when it is the inventory OLT, adopts it.
func (s *Service) adoptOLT(ctx context.Context, id, by string) error {
	cands := s.credCandidates(id, false)
	if len(cands) == 0 {
		// The factory login was tried before and nothing is stored: the owner has to type one.
		s.markAdopt(id, store.AdoptNeedLogin, "The server has no login for this OLT. Enter the OLT's login and password below.", "")
		return nil
	}
	var lastErr error
	for _, c := range cands {
		c := c
		var outcome string
		err := s.withSessionAs(ctx, id, true, &c, func(o *opened) error {
			var err error
			outcome, err = s.identify(ctx, o, c, by)
			return err
		})
		switch {
		case err == nil:
			_ = outcome
			return nil
		case errors.Is(err, ErrLoginFailed):
			if c.Source == "factory" {
				_ = s.setAdopt(id, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
					a.FactoryAt, a.FactoryOK = s.now(), false
				})
			}
			lastErr = err
			continue
		default:
			s.sshProblem(id, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil // recorded as the OLT's adoption state
		}
	}
	if errors.Is(lastErr, ErrLoginFailed) {
		s.markAdopt(id, store.AdoptNeedLogin, "The OLT refused the login the server has. Enter the OLT's login and password below.", "")
		_ = s.setAdopt(id, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) { a.SSHOK, a.SSHAt = true, s.now() })
		return nil
	}
	return lastErr
}

// sshProblem records why the connection failed.
func (s *Service) sshProblem(id string, err error) {
	switch {
	case errors.Is(err, ErrBusy):
		return
	case errors.Is(err, ErrHostKey):
		s.markAdopt(id, store.AdoptFailed, "The OLT's SSH host key is not the pinned one. If the OLT was replaced, an owner can forget the pinned key on the connection settings.", errText(err))
	default:
		_ = s.setAdopt(id, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
			a.SSHOK = false
			a.Stage = store.AdoptNoSSH
			a.Note = "The OLT does not answer on SSH. A factory V1600 has SSH off: enable it once on the OLT (console or web page: configure terminal, sshd, sshd enable, exit, write), then press Check now."
			a.Error = errText(err)
			a.CheckedAt = s.now()
		})
	}
}

// identify runs inside a logged-in session: it reads the version and adopts the OLT when it is the inventory OLT.
func (s *Service) identify(ctx context.Context, o *opened, c cred, by string) (string, error) {
	dl := s.o.Dialect
	out, err := o.sess.ShowAt(ctx, dl.VersionMode, dl.VersionCmd)
	if err != nil {
		return "", err
	}
	vi := ParseVersion(out)
	now := s.now()
	if c.Source == "factory" {
		_ = s.setAdopt(o.e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) { a.FactoryAt, a.FactoryOK = now, true })
	}
	ok, why := IdentityOK(o.e, vi)
	if !ok {
		// Not this OLT: nothing is configured, nothing is pinned, and the device is listed as a possible OLT.
		_ = s.setAdopt(o.e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
			a.SSHOK, a.SSHAt, a.SerialOK, a.SerialSeen, a.ModelSeen = true, now, "mismatch", vi.Serial, vi.Model
			a.Stage, a.Note, a.Error, a.CheckedAt = store.AdoptMismatch, "The device on the port is not this OLT ("+why+"). Nothing was configured. Check the serial number and MAC in the inventory, or unplug the other device.", "", now
		})
		s.notePossible(o.e.SiteID, strings.ToUpper(o.e.MAC), "", s.portOf(o.e.SiteID), why)
		return "mismatch", nil
	}
	s.clearPossible(o.e.SiteID, strings.ToUpper(o.e.MAC))
	// The device is this OLT: now its host key may be pinned.
	if fp := o.sess.NewHostKey; fp != "" {
		s.pinKey(o.e.ID, fp)
	}
	_ = s.upd(o.e.ID, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Status.Model, st.Status.DeviceSerial, st.Status.HWVersion, st.Status.SWVersion, st.Status.Version = vi.Model, vi.Serial, vi.Hardware, vi.Software, vi.Summary()
		a := &st.Settings.Adopt
		a.SSHOK, a.SSHAt, a.SerialOK, a.SerialSeen, a.ModelSeen, a.CheckedAt, a.Error = true, now, "ok", vi.Serial, vi.Model, now, ""
		if c.Source != "stored" || a.CredSource == "" {
			a.CredSource = c.Source
		}
		if e.MgmtIP == "" {
			e.MgmtIP = factoryIP
			d.Equipment[e.ID] = e
		}
		s.audit(d, e, firstNonEmpty(by, "system"), "OLT "+store.OLTLabel(e)+" identified on the OLT management port: serial and model match the inventory")
	})
	switch c.Source {
	case "factory":
		// The vendor's login works: replace it now with generated ones (the owner's action, done automatically here).
		// Once the passwords are being changed the change must run to its end (or its restore), whatever deadline the
		// caller had ("Check now" runs inside a web request): the saves alone may take minutes.
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), secureJobTimeout)
		defer scancel()
		if _, err := s.secureIn(sctx, o, c, "system"); err != nil {
			s.markAdopt(o.e.ID, store.AdoptNeedSecure, "The OLT still has its factory login. Press Secure this OLT to replace it.", errText(err))
			return "need_secure", nil
		}
		return "secured", nil
	case "pending":
		// A change of the login was interrupted: the pending login works, so it is the OLT's login now. It may never have
		// been saved on the OLT (the server stopped before `write`): save it first, or a restart of the OLT would bring
		// back the old login, which the server no longer keeps.
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer wcancel()
		if err := o.sess.Save(wctx); err != nil {
			s.markAdopt(o.e.ID, store.AdoptFailed, "The OLT works with the generated login of an interrupted change but would not save its configuration; the server tries again on its next check.", errText(err))
			return "", err
		}
		if err := s.adoptPending(o.e.ID, by); err != nil {
			return "", err
		}
		return "secured", nil
	case "stored":
		// The stored login works, so an interrupted change (a pending pair) did not take: forget it.
		if p := o.st.Settings.Adopt; p.PendingPass != "" || p.PendingEn != "" {
			_ = s.setAdopt(o.e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) { a.PendingPass, a.PendingEn = "", "" })
		}
	}
	_ = s.setAdopt(o.e.ID, func(a *store.OLTAdopt, e *store.Equipment, d *store.Data) {
		a.Adopted = true
		if a.Secured {
			a.Stage, a.Note = store.AdoptSecured, "Adopted and secured."
		} else {
			a.Stage, a.Note = store.AdoptAdopted, "Adopted with the login the owner typed. Press Secure this OLT to replace it with a generated one."
		}
	})
	return "adopted", nil
}

func (s *Service) portOf(siteID string) string {
	var p string
	s.o.Store.View(func(d *store.Data) { p = d.Sites[siteID].OLTMgmtPort })
	return p
}

// pinKey pins an OLT's SSH host key (first use) once the device has been identified.
func (s *Service) pinKey(id, fp string) {
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		if st.Settings.HostKey == "" {
			st.Settings.HostKey, st.Settings.HostKeyAt = fp, s.now()
			s.audit(d, e, "system", "SSH host key of OLT "+store.OLTLabel(e)+" pinned on first use: "+fp)
		}
	})
}

// ---- the view ------------------------------------------------------------------------------------------------------

// AdoptStep is one line of the adoption checklist.
type AdoptStep struct {
	Label  string
	State  string // ok, todo, fail, off
	Detail string
}

// AdoptView is what the OLT page and the API show about the adoption. It holds no secret.
type AdoptView struct {
	Stage, Note, Error string
	Manual             bool
	Port               string
	Steps              []AdoptStep
	CheckedAt          time.Time
	Secured            bool
	CanSecure          bool // the server has identified the OLT and may replace its login
	NeedLogin          bool // the owner must type a login
	SNMPNote, ACLNote  string
	ACLRules           []string // login-access-list rules read back from the OLT
	ACLFinding         string
	HasLogin           bool
}

// AdoptView composes the checklist of an OLT.
func (s *Service) AdoptView(id string) (AdoptView, bool) {
	var v AdoptView
	var e store.Equipment
	var st store.OLTState
	var site store.Site
	ok := false
	s.o.Store.View(func(d *store.Data) {
		var found bool
		e, found = d.Equipment[id]
		if !found || e.Kind != store.KindOLT {
			return
		}
		ok = true
		st = d.OLTStateOf(id).Clone()
		site = d.Sites[e.SiteID]
	})
	if !ok {
		return v, false
	}
	a := st.Settings.Adopt
	v.Stage, v.Note, v.Error, v.Manual, v.CheckedAt, v.Secured = a.Stage, a.Note, a.Error, a.Manual, a.CheckedAt, a.Secured
	v.Port, v.SNMPNote, v.ACLNote = site.OLTMgmtPort, a.SNMPNote, a.ACLNote
	v.ACLRules, v.ACLFinding = a.ACLRules, a.ACLFinding
	v.HasLogin = e.MgmtUser != "" && e.MgmtPass != ""
	step := func(label, state, detail string) { v.Steps = append(v.Steps, AdoptStep{label, state, detail}) }
	yn := func(b bool) string {
		if b {
			return "ok"
		}
		return "todo"
	}
	switch {
	case e.SiteID == "":
		step("Cable and port", "todo", "The OLT is not on a router yet.")
	case site.OLTMgmtPort == "":
		step("Cable and port", "todo", "The router has no OLT management port. Give a spare port the role OLT management.")
	default:
		step("Cable and port", "ok", "Router port "+site.OLTMgmtPort+" ("+st.Settings.Adopt.Port+")")
	}
	if site.OLTMgmtPort != "" {
		step("OLT seen on the port", yn(a.MACSeen), map[bool]string{true: "The OLT's MAC " + e.MAC + " is on " + site.OLTMgmtPort + ".", false: "The OLT's MAC " + e.MAC + " is not seen on " + site.OLTMgmtPort + "."}[a.MACSeen])
		sshState := yn(a.SSHOK)
		if a.Stage == store.AdoptNoSSH {
			sshState = "fail"
		}
		step("SSH reached", sshState, map[bool]string{true: "The server logged in over SSH.", false: "No SSH session yet."}[a.SSHOK])
		serial := "todo"
		detail := "Not checked yet."
		switch a.SerialOK {
		case "ok":
			serial, detail = "ok", "The serial and model match the inventory."
		case "mismatch":
			serial, detail = "fail", "The device is not this OLT. Nothing was configured."
		}
		step("Serial and model", serial, detail)
		cs := "todo"
		cd := "No login yet."
		switch {
		case a.Stage == store.AdoptNeedLogin:
			cs, cd = "fail", "The server has no working login. Enter it below."
		case a.CredSource != "" && a.SSHOK:
			cs, cd = "ok", "The server logs in with a "+map[string]string{"stored": "stored", "pending": "generated", "factory": "factory (replaced)", "owner": "typed", "secured": "generated"}[a.CredSource]+" login."
		case v.HasLogin:
			cs, cd = "ok", "A login is stored."
		}
		step("Credentials", cs, cd)
		sec := "todo"
		secd := "The login and enable password are not replaced yet."
		if a.Secured {
			sec, secd = "ok", "The login and enable password were replaced by generated ones, tested with a new session and saved."
		}
		step("Secured", sec, secd)
		snmp := "todo"
		if a.SNMPOff {
			snmp = "ok"
		}
		step("SNMP", snmp, firstNonEmpty(a.SNMPNote, "Not done yet."))
		acl := "off"
		if a.ACLDone {
			acl = "ok"
		} else if strings.HasPrefix(a.ACLNote, "Couldn't restrict SSH") {
			acl = "fail"
		}
		step("SSH access list", acl, firstNonEmpty(a.ACLNote, "Not done yet."))
	}
	v.CanSecure = a.SerialOK == "ok" && !a.Secured && (v.HasLogin || a.FactoryOK)
	v.NeedLogin = a.Stage == store.AdoptNeedLogin
	if v.Stage == "" {
		switch {
		case e.SiteID == "":
			v.Stage, v.Note = store.AdoptNoRouter, "Put the OLT on a router first (edit the OLT)."
		case site.OLTMgmtPort == "":
			v.Stage, v.Note = store.AdoptNoPort, "Give a spare router port the role OLT management (Ports tab), then cable the OLT's management port to it."
		default:
			v.Note = "Press Check now."
		}
	}
	if a.Manual {
		v.Stage, v.Note = store.AdoptManual, "Manual: the OLT is set up with the console snippet (VLAN 98) and the server does not use the management port."
	}
	return v, true
}
