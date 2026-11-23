package acs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"ispbill/internal/store"
)

// WiFiInput is what the owner types on the Wi-Fi card.
type WiFiInput struct {
	SSID string
	Pass string
	// Same: one name and password on both bands (SSID5 and Pass5 are then ignored).
	Same  bool
	SSID5 string
	Pass5 string
}

func (in WiFiInput) check() error {
	if m := ValidSSID(in.SSID); m != "" {
		return &FieldError{"ssid", m}
	}
	if m := ValidPassphrase(in.Pass); m != "" {
		return &FieldError{"password", m}
	}
	if !in.Same {
		if m := ValidSSID(in.SSID5); m != "" {
			return &FieldError{"ssid_5ghz", m}
		}
		if m := ValidPassphrase(in.Pass5); m != "" {
			return &FieldError{"password_5ghz", m}
		}
	}
	return nil
}

func newTaskID() string { return "t" + randHex(4) }
func newONUID() string  { return "o" + randHex(4) }

func (s *Service) audit(d *store.Data, o store.ONU, by, ip, msg string) {
	d.AddEvent(store.Event{Time: s.now(), SiteID: o.SiteID, Kind: "audit", AccountID: o.AccountID, Msg: msg, Actor: by, IP: ip})
}

// queue adds a task to the ONU unless an active one of the same kind exists (then that one is returned).
func queue(d *store.Data, o store.ONU, kind, by string, now time.Time) store.ONUTask {
	for _, t := range d.ONUTasksOf(o.ID) {
		if t.Kind == kind && t.Active() {
			return t
		}
	}
	t := store.ONUTask{ID: newTaskID(), ONUID: o.ID, SiteID: o.SiteID, AccountID: o.AccountID, Kind: kind, Status: store.TaskPending, By: by, CreatedAt: now}
	d.ONUTasks[t.ID] = t
	pruneTasks(d, o.ID)
	return t
}

// pruneTasks keeps the newest 30 finished tasks of a device.
func pruneTasks(d *store.Data, onuID string) {
	var done []store.ONUTask
	for _, t := range d.ONUTasksOf(onuID) {
		if !t.Active() {
			done = append(done, t)
		}
	}
	for i := 30; i < len(done); i++ {
		delete(d.ONUTasks, done[i].ID)
	}
}

// SetWiFi saves the desired Wi-Fi configuration of the ONU linked to a house (the passphrases sealed), queues the
// change and asks the ONU to connect now. The password is never stored in the clear and never returned.
func (s *Service) SetWiFi(by, ip, accountID string, in WiFiInput) (store.ONUTask, error) {
	if err := in.check(); err != nil {
		return store.ONUTask{}, err
	}
	enc, err := s.sealPass(in.Pass)
	if err != nil {
		return store.ONUTask{}, err
	}
	enc5 := ""
	if !in.Same {
		if enc5, err = s.sealPass(in.Pass5); err != nil {
			return store.ONUTask{}, err
		}
	}
	var t store.ONUTask
	var onuID string
	err = s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUOfAccount(accountID)
		if !ok {
			return ErrNoONU
		}
		now := s.now()
		o.Desired = store.WiFiDesired{Set: true, Same: in.Same, SSID: in.SSID, PassEnc: enc, UpdatedAt: now, UpdatedBy: by}
		if !in.Same {
			o.Desired.SSID5, o.Desired.Pass5Enc = in.SSID5, enc5
		}
		d.ONUs[o.ID] = o
		t = queue(d, o, store.TaskWiFi, by, now)
		msg := "Wi-Fi name and password changed (2.4 and 5 GHz: " + in.SSID + ")"
		if !in.Same {
			msg = "Wi-Fi name and password changed (2.4 GHz: " + in.SSID + ", 5 GHz: " + in.SSID5 + ")"
		}
		s.audit(d, o, by, ip, msg+"; the password is not logged")
		onuID = o.ID
		return nil
	})
	if err != nil {
		return store.ONUTask{}, err
	}
	s.kickAsync(onuID)
	return t, nil
}

// RebootONU queues a Reboot of the ONU linked to a house and asks it to connect now.
func (s *Service) RebootONU(by, ip, accountID string) (store.ONUTask, error) {
	var t store.ONUTask
	var onuID string
	err := s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUOfAccount(accountID)
		if !ok {
			return ErrNoONU
		}
		t = queue(d, o, store.TaskReboot, by, s.now())
		s.audit(d, o, by, ip, "ONU reboot queued")
		onuID = o.ID
		return nil
	})
	if err != nil {
		return store.ONUTask{}, err
	}
	s.kickAsync(onuID)
	return t, nil
}

// RefreshONU asks the ONU to report its Wi-Fi names now.
func (s *Service) RefreshONU(by, ip, accountID string) (store.ONUTask, error) {
	var t store.ONUTask
	var onuID string
	err := s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUOfAccount(accountID)
		if !ok {
			return ErrNoONU
		}
		t = queue(d, o, store.TaskRefresh, by, s.now())
		onuID = o.ID
		return nil
	})
	if err != nil {
		return store.ONUTask{}, err
	}
	s.kickAsync(onuID)
	return t, nil
}

// TaskStatus is the newest task of the ONU of a house, for the status line that refreshes.
func (s *Service) TaskStatus(accountID string) (TaskView, bool) {
	var tv TaskView
	ok := false
	s.st.View(func(d *store.Data) {
		if o, found := d.ONUOfAccount(accountID); found {
			if ts := d.ONUTasksOf(o.ID); len(ts) > 0 {
				tv, ok = taskView(ts[0]), true
			}
		}
	})
	return tv, ok
}

// ---- linking ----

// LinkONU links an ONU to a house by the owner's choice. One ONU per house; the router must be the same.
func (s *Service) LinkONU(by, ip, onuID, accountID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return ErrONUGone
		}
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNoAccount
		}
		if o.AccountID == accountID {
			return store.ErrNoChange
		}
		if o.AccountID != "" {
			return ErrONULinked
		}
		if other, ok := d.ONUOfAccount(accountID); ok && other.ID != o.ID {
			return ErrLinked
		}
		if o.SiteID != a.SiteID {
			return ErrSite
		}
		o.AccountID, o.Link = accountID, store.LinkManual
		d.ONUs[o.ID] = o
		s.audit(d, o, by, ip, "ONU "+o.Serial+" linked to "+a.Code)
		return nil
	})
}

// UnlinkONU removes the link of an ONU. Its saved Wi-Fi configuration stays with the ONU row until it is linked
// again; it is not restored to another house's ONU.
func (s *Service) UnlinkONU(by, ip, onuID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.ONUs[onuID]
		if !ok {
			return ErrONUGone
		}
		if o.AccountID == "" {
			return store.ErrNoChange
		}
		code := d.Accounts[o.AccountID].Code
		s.audit(d, o, by, ip, "ONU "+o.Serial+" unlinked from "+code)
		o.AccountID, o.Link = "", ""
		o.Desired = store.WiFiDesired{} // the owner's Wi-Fi choice belongs to the house, not to the box
		d.ONUs[o.ID] = o
		return nil
	})
}

// autoLink links ONUs to houses whose MAC is one of the ONU's WAN MACs on the same router (and clears links to
// houses that no longer exist). It changes d in place and says whether it did.
func autoLink(d *store.Data, onuID string) bool {
	o, ok := d.ONUs[onuID]
	if !ok {
		return false
	}
	changed := false
	if o.AccountID != "" {
		if _, ok := d.Accounts[o.AccountID]; !ok {
			o.AccountID, o.Link, o.Desired = "", "", store.WiFiDesired{}
			d.ONUs[o.ID], changed = o, true
		}
	}
	if o.AccountID != "" {
		return changed
	}
	for _, a := range d.Accounts {
		if a.SiteID != o.SiteID || a.MACConflict {
			continue
		}
		am, ok := store.CanonMAC(a.MAC)
		if !ok {
			continue
		}
		for _, m := range o.MACs {
			if m == am {
				if other, taken := d.ONUOfAccount(a.ID); taken {
					// The house's MAC moved to this ONU (the box was replaced): an automatic link to the old box
					// gives way, and the new box inherits the house's Wi-Fi choice. A link the owner made stays.
					if other.Link != store.LinkAuto || containsMAC(other.MACs, am) {
						return changed
					}
					o.Desired = other.Desired
					other.AccountID, other.Link, other.Desired = "", "", store.WiFiDesired{}
					d.ONUs[other.ID] = other
				}
				o.AccountID, o.Link = a.ID, store.LinkAuto
				d.ONUs[o.ID] = o
				return true
			}
		}
	}
	return changed
}

func containsMAC(list []string, m string) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

// AutoLinkSite links every unlinked ONU of a router whose MAC matches a house (called when a house gets its MAC).
func (s *Service) AutoLinkSite(siteID string) {
	_ = s.st.Update(func(d *store.Data) error {
		n := 0
		for _, o := range d.ONUList(siteID) {
			if o.AccountID == "" && autoLink(d, o.ID) {
				n++
			}
		}
		if n == 0 {
			return store.ErrNoChange
		}
		return nil
	})
}

// ---- connection request ----

// Kick is the outcome of the last attempt to make an ONU connect now.
type Kick struct {
	At  time.Time
	OK  bool
	Err string
}

// Text says it in words.
func (k Kick) Text() string {
	switch {
	case k.OK:
		return "The ONU was asked to connect now."
	case k.Err != "":
		return "The ONU did not answer the request (" + k.Err + "); the change is applied at its next check-in (within a few minutes)."
	}
	return ""
}

func (s *Service) kickOf(id string) (Kick, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.kicks[id]
	return k, ok && s.now().Sub(k.At) < 10*time.Minute
}

func (s *Service) setKick(id string, k Kick) {
	s.mu.Lock()
	s.kicks[id] = k
	s.mu.Unlock()
}

func (s *Service) kickAsync(onuID string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.ConnectionRequest(context.Background(), onuID)
	}()
}

// Wait waits for background connection requests (tests).
func (s *Service) Wait() { s.wg.Wait() }

// crTarget validates the connection-request URL of an ONU: http(s) to an address inside the management subnet
// of the ONU's own router, on an allowed port. A host outside the subnet (some ONUs report a WAN address that is
// not the management one) is replaced by the address the ONU connected from; anything else is refused.
func (s *Service) crTarget(d *store.Data, o store.ONU) (*url.URL, error) {
	site := d.Sites[o.SiteID]
	if site.ONUMgmtN <= 0 {
		return nil, errors.New("the router has no ONU management network")
	}
	pfx, err := netip.ParsePrefix(site.MgmtSubnet())
	if err != nil {
		return nil, errors.New("bad management subnet")
	}
	u, err := url.Parse(o.ConnReqURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("the ONU has not reported a connection-request address yet")
	}
	if u.User != nil {
		return nil, errors.New("the connection-request address carries a login")
	}
	host := u.Hostname()
	ip, err := netip.ParseAddr(host)
	mg, mgErr := netip.ParseAddr(o.MgmtIP)
	if err != nil || !pfx.Contains(ip.Unmap()) {
		if mgErr != nil || !pfx.Contains(mg) {
			return nil, errors.New("the ONU's address is outside the router's management network")
		}
		ip = mg
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	pn, _ := strconv.Atoi(port)
	allowed := false
	for _, p := range s.cfg.ACSCRPorts {
		if p == pn {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("port %d is not one of the allowed connection-request ports", pn)
	}
	u2 := *u
	u2.Host = net.JoinHostPort(ip.String(), port)
	return &u2, nil
}

// ConnectionRequest asks an ONU to open a session now (HTTP GET with digest authentication to its
// ConnectionRequestURL). The attempt and its outcome are remembered for the screens; a failure is not an error
// for the caller: the task waits for the ONU's next periodic Inform.
func (s *Service) ConnectionRequest(ctx context.Context, onuID string) error {
	var o store.ONU
	var target *url.URL
	var terr error
	s.st.View(func(d *store.Data) {
		var ok bool
		if o, ok = d.ONUs[onuID]; !ok {
			terr = ErrONUGone
			return
		}
		target, terr = s.crTarget(d, o)
	})
	if terr != nil {
		s.setKick(onuID, Kick{At: s.now(), Err: terr.Error()})
		return terr
	}
	user, pass, haveCR := s.openCR(o)
	err := s.doConnReq(ctx, target, user, pass, haveCR)
	if err != nil {
		s.setKick(onuID, Kick{At: s.now(), Err: err.Error()})
		s.log.Info("connection request failed", "onu", o.ID, "err", err.Error())
		return err
	}
	s.setKick(onuID, Kick{At: s.now(), OK: true})
	return nil
}

func (s *Service) doConnReq(ctx context.Context, target *url.URL, user, pass string, haveCR bool) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	get := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, err
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return s.client.Do(req)
	}
	resp, err := get("")
	if err != nil {
		return connErr(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && haveCR {
		auth, ok := clientDigest(resp.Header.Get("WWW-Authenticate"), user, pass, http.MethodGet, target.RequestURI())
		if !ok {
			return errors.New("the ONU asked for a login it could not be given")
		}
		resp, err = get(auth)
		if err != nil {
			return connErr(err)
		}
		resp.Body.Close()
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errors.New("the ONU refused the connection-request login")
	}
	return fmt.Errorf("the ONU answered %d", resp.StatusCode)
}

func connErr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return errors.New("no answer")
	}
	return errors.New("could not connect")
}
