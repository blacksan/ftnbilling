package acs

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"ispbill/internal/store"
)

const (
	cookieName     = "ftn-acs"
	sessionIdle    = 60 * time.Second // a session with no request for this long is dropped (slow CPEs)
	ipFallbackTTL  = 30 * time.Second // a CPE that drops the cookie is matched to its session by source address for this long
	sessionMax     = 5 * time.Minute
	maxSessions    = 600
	perIPActive    = 6   // concurrent requests from one address
	perIPPerMinute = 240 // requests from one address per minute
	authFailsMax   = 20  // wrong logins per minute from one address before it is turned away
)

type ipState struct {
	active    int
	winStart  time.Time
	reqs      int
	failStart time.Time
	fails     int
}

// Handler is the HTTP handler the ONUs talk to (POST /). Everything else is 404.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

// siteOf maps a source address to the router whose management subnet holds it.
func (s *Service) siteOf(ip netip.Addr) (store.Site, bool) {
	var site store.Site
	var ok bool
	s.st.View(func(d *store.Data) { site, ok = d.SiteByONUIP(ip.String()) })
	if ok {
		if p, err := netip.ParsePrefix(site.MgmtSubnet()); err != nil || !p.Contains(ip) || ip == p.Addr() {
			return store.Site{}, false
		}
		if site.Disabled {
			return store.Site{}, false
		}
	}
	return site, ok
}

func (s *Service) ipEnter(ip netip.Addr) (ok bool, leave func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ips) > 5000 {
		s.ips = map[netip.Addr]*ipState{} // an address flood: forget the old windows
	}
	st := s.ips[ip]
	if st == nil {
		st = &ipState{}
		s.ips[ip] = st
	}
	now := s.now()
	if now.Sub(st.winStart) > time.Minute {
		st.winStart, st.reqs = now, 0
	}
	if st.active >= perIPActive || st.reqs >= perIPPerMinute {
		return false, nil
	}
	if st.fails >= authFailsMax && now.Sub(st.failStart) < time.Minute {
		return false, nil
	}
	st.active++
	st.reqs++
	return true, func() {
		s.mu.Lock()
		st.active--
		s.mu.Unlock()
	}
}

func (s *Service) authFailed(ip netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.ips[ip]; st != nil {
		now := s.now()
		if now.Sub(st.failStart) > time.Minute {
			st.failStart, st.fails = now, 0
		}
		st.fails++
	}
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	ip, ok := s.ipOf(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Only a source inside a known router's management subnet is served at all (no login prompt for anyone else).
	site, known := s.siteOf(ip)
	if !known {
		s.log.Warn("ACS: connection from an address outside every management subnet refused", "from", ip.String())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/" && r.URL.Path != "") {
		http.NotFound(w, r)
		return
	}
	enter, leave := s.ipEnter(ip)
	if !enter {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	defer leave()

	sess := s.cookieSession(r, ip)
	var ai authInfo
	if sess == nil {
		var ok bool
		if ai, ok = s.authenticate(w, r, ip); !ok {
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		if sess == nil {
			// A CPE that does not keep the cookie logs in again for every request: the one live session of
			// its address, opened with the same login, is the one it means.
			if sess = s.ipSession(ip, ai); sess == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		s.step(w, sess, nil)
		return
	}
	msg, err := parseMessage(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch msg.Method {
	case "Inform":
		s.handleInform(w, r, site, ip, msg, ai)
	case "GetRPCMethods":
		writeXML(w, getRPCMethodsResponse(msg.NS, msg.ID))
	case "TransferComplete", "AutonomousTransferComplete", "Kicked", "RequestDownload", "AutonomousDUStateChangeComplete", "DUStateChangeComplete":
		writeXML(w, simpleResponse(msg.NS, msg.ID, msg.Method))
	default:
		if sess == nil {
			sess = s.ipSession(ip, ai)
		}
		if sess == nil {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		s.step(w, sess, msg)
	}
}

// authInfo says how a request logged in.
type authInfo struct {
	kind  int    // authShared | authDevice
	onuID string // the device whose own login was used (authDevice)
}

const (
	authShared = iota // the login every ONU is given by the OLT profile
	authDevice        // the ONU's own login, set by this ACS
)

// authenticate checks the request's login: the shared one, or a device's own. It answers the 401 itself when the
// request is not (yet) authenticated.
func (s *Service) authenticate(w http.ResponseWriter, r *http.Request, ip netip.Addr) (authInfo, bool) {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		s.da.challenge(w, false)
		return authInfo{}, false
	}
	gUser, gPass, err := s.Credentials()
	if err != nil {
		s.log.Error("ACS: login unavailable", "err", err.Error())
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return authInfo{}, false
	}
	user := ""
	f := parseDigest(hdr)
	if f != nil {
		user = f["username"]
	} else if s.cfg.ACSCompatAuth {
		user, _, _ = r.BasicAuth()
	}
	ai := authInfo{kind: authShared}
	pass := gPass
	if user != "" && user != gUser {
		if id, p, ok := s.deviceLogin(user); ok {
			ai, pass = authInfo{kind: authDevice, onuID: id}, p
		}
	}
	res := dgBad
	switch {
	case f != nil:
		wantUser := gUser
		if ai.kind == authDevice {
			wantUser = user
		}
		res = s.da.verify(r, wantUser, pass, s.cfg.ACSCompatAuth)
	case s.cfg.ACSCompatAuth:
		if u, p, ok := r.BasicAuth(); ok && subtle.ConstantTimeCompare([]byte(u), []byte(map[bool]string{true: user, false: gUser}[ai.kind == authDevice])) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1 {
			res = dgOK
		}
	}
	switch res {
	case dgOK:
		return ai, true
	case dgStale:
		s.da.challenge(w, true)
	default:
		s.authFailed(ip)
		s.da.challenge(w, false)
	}
	return authInfo{}, false
}

// deviceLogin finds the ONU whose own ACS user name is user and opens its password.
func (s *Service) deviceLogin(user string) (id, pass string, ok bool) {
	var enc string
	s.st.View(func(d *store.Data) {
		for _, o := range d.ONUs {
			if o.Sec.ACSUser == user && o.Sec.ACSPassEnc != "" {
				id, enc = o.ID, o.Sec.ACSPassEnc
				return
			}
		}
	})
	if id == "" {
		return "", "", false
	}
	p, err := s.box.OpenAAD(enc, []byte("acs.dev"))
	if err != nil {
		return "", "", false
	}
	return id, p, true
}

// ipSession is the one live session of an address that was opened with the same login (M3: a CPE that drops the
// cookie). With two candidates it answers nothing: it cannot tell them apart.
func (s *Service) ipSession(ip netip.Addr, ai authInfo) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *session
	n := 0
	now := s.now()
	for _, ss := range s.sessions {
		if ss.ip == ip && ss.auth.kind == ai.kind && ss.auth.onuID == ai.onuID && now.Sub(ss.last()) <= ipFallbackTTL {
			found = ss
			n++
		}
	}
	if n != 1 {
		return nil
	}
	return found
}

func writeXML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Service) cookieSession(r *http.Request, ip netip.Addr) *session {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[c.Value]
	if ss == nil || ss.ip != ip {
		return nil
	}
	return ss
}

// step hands the CPE's answer (nil for an empty POST) to the session and writes what the session says next.
func (s *Service) step(w http.ResponseWriter, ss *session, msg *message) {
	if msg != nil {
		select {
		case ss.in <- msg:
		default: // an answer nobody asked for
			http.Error(w, "unexpected message", http.StatusBadRequest)
			return
		}
	}
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: ss.id, Path: "/", HttpOnly: true}) // on every response
	select {
	case req := <-ss.out:
		ss.touch(s.now())
		writeXML(w, req)
	case <-ss.done:
		s.drop(ss)
		w.WriteHeader(http.StatusNoContent)
	case <-timer.C:
		ss.cancel()
		s.drop(ss)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Service) drop(ss *session) {
	s.mu.Lock()
	delete(s.sessions, ss.id)
	s.mu.Unlock()
}

// reap drops sessions that went quiet. It runs from the listener loop.
func (s *Service) reap() {
	s.mu.Lock()
	var dead []*session
	now := s.now()
	for id, ss := range s.sessions {
		if now.Sub(ss.last()) > sessionIdle || now.Sub(ss.started) > sessionMax {
			dead = append(dead, ss)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, ss := range dead {
		ss.cancel()
	}
}

// ---- listener ----

// ErrPublicListen: the configured ACS address is not a private one.
var ErrPublicListen = errors.New("the ACS address is not private")

// Run listens on cfg.ACSListen until ctx ends. It refuses a public address before binding. A private address that
// is not up yet (the tunnel interface is not there) is retried every 10 seconds.
func (s *Service) Run(ctx context.Context) error {
	if !s.cfg.ACSEnabled() {
		return nil
	}
	if err := s.cfg.CheckACSListenAddr(); err != nil {
		return fmt.Errorf("%w: %v", ErrPublicListen, err)
	}
	go s.reapLoop(ctx)
	for {
		err := s.listenOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		s.log.Warn("ACS listener stopped; trying again in 10s", "addr", s.cfg.ACSListen, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

func (s *Service) reapLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reap()
			if n++; n%4 == 0 { // once a minute
				s.Upkeep()
			}
		}
	}
}

func (s *Service) listenOnce(ctx context.Context) error {
	ln, err := net.Listen("tcp4", s.cfg.ACSListen)
	if err != nil {
		return err
	}
	// Defence in depth: whatever was configured, the socket really is on a private address.
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		if a, ok := netip.AddrFromSlice(ta.IP); !ok || !(a.Unmap().IsPrivate() || a.Unmap().IsLoopback()) {
			ln.Close()
			return ErrPublicListen
		}
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 40 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if s.cfg.ACSTLSCert != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.ACSTLSCert, s.cfg.ACSTLSKey)
		if err != nil {
			ln.Close()
			return fmt.Errorf("ACS_TLS_CERT/KEY: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	s.log.Info("ACS (ONU management) listening", "addr", s.cfg.ACSListen, "tls", s.cfg.ACSTLSCert != "")
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sh)
		case <-done:
		}
	}()
	err = srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
