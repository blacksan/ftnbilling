package acs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"ispbill/internal/store"
)

// A session is one CWMP conversation with one ONU, from its Inform to the empty answer that ends it. The
// program of the session (what to ask, in which order) runs in its own goroutine and reads like a script:
// it calls rpc() and gets the ONU's answer back; the HTTP handler only carries the messages between the
// ONU's POSTs and that goroutine.

type session struct {
	id      string
	ip      netip.Addr
	onuID   string
	ns      string
	started time.Time
	out     chan []byte   // the program's next request, for the handler to send
	in      chan *message // the ONU's answer, for the program
	done    chan struct{}
	cancel  context.CancelFunc

	mu     sync.Mutex
	lastAt time.Time
	auth   authInfo
	n      int // requests made
	seq    int
}

func (ss *session) touch(t time.Time) { ss.mu.Lock(); ss.lastAt = t; ss.mu.Unlock() }
func (ss *session) last() time.Time   { ss.mu.Lock(); defer ss.mu.Unlock(); return ss.lastAt }

const maxRPCs = 150

var errSessionLimit = errors.New("too many requests in one session")

// call sends one request and waits for the ONU's answer.
func (ss *session) call(ctx context.Context, build func(id string) []byte) (*message, error) {
	ss.mu.Lock()
	ss.n++
	ss.seq++
	id := fmt.Sprintf("ftn-%d", ss.seq)
	over := ss.n > maxRPCs
	ss.mu.Unlock()
	if over {
		return nil, errSessionLimit
	}
	select {
	case ss.out <- build(id):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case m := <-ss.in:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (ss *session) fault(m *message) *Fault {
	if m.Method == "Fault" {
		return parseFault(m.Body)
	}
	return nil
}

var errUnexpected = errors.New("the ONU answered something else than asked")

func (ss *session) gpv(ctx context.Context, names []string) ([]ParamValue, *Fault, error) {
	m, err := ss.call(ctx, func(id string) []byte { return getParameterValues(ss.ns, id, names) })
	if err != nil {
		return nil, nil, err
	}
	if f := ss.fault(m); f != nil {
		return nil, f, nil
	}
	if m.Method != "GetParameterValuesResponse" {
		return nil, nil, errUnexpected
	}
	return parseValues(m.Body), nil, nil
}

func (ss *session) gpn(ctx context.Context, path string, next bool) ([]NameInfo, *Fault, error) {
	m, err := ss.call(ctx, func(id string) []byte { return getParameterNames(ss.ns, id, path, next) })
	if err != nil {
		return nil, nil, err
	}
	if f := ss.fault(m); f != nil {
		return nil, f, nil
	}
	if m.Method != "GetParameterNamesResponse" {
		return nil, nil, errUnexpected
	}
	return parseNames(m.Body), nil, nil
}

func (ss *session) spv(ctx context.Context, ps []ParamValue, key string) (int, *Fault, error) {
	m, err := ss.call(ctx, func(id string) []byte { return setParameterValues(ss.ns, id, ps, key) })
	if err != nil {
		return 0, nil, err
	}
	if f := ss.fault(m); f != nil {
		return 0, f, nil
	}
	if m.Method != "SetParameterValuesResponse" {
		return 0, nil, errUnexpected
	}
	st := 0
	fmt.Sscanf(m.Body.childText("Status"), "%d", &st)
	return st, nil, nil
}

func (ss *session) simple(ctx context.Context, build func(id string) []byte, want string) (*Fault, error) {
	m, err := ss.call(ctx, build)
	if err != nil {
		return nil, err
	}
	if f := ss.fault(m); f != nil {
		return f, nil
	}
	if m.Method != want {
		return nil, errUnexpected
	}
	return nil, nil
}

// ---- Inform ----

func (s *Service) handleInform(w http.ResponseWriter, r *http.Request, site store.Site, ip netip.Addr, msg *message, ai authInfo) {
	in, err := parseInform(msg.Body)
	if err != nil {
		http.Error(w, "bad Inform", http.StatusBadRequest)
		return
	}
	if in.OUI == "" {
		s.log.Warn("ACS: an Inform without an OUI was accepted", "from", ip.String())
	}
	now := s.now()
	// The router's lease table says which MAC holds the address the Inform came from. It is read (from the sweep's
	// copy) only when the answer matters: a new serial, a shared login on a known serial, or a changed address.
	var needLease bool
	s.st.View(func(d *store.Data) {
		cur, found := d.ONUBySerial(in.OUI, in.Serial)
		needLease = s.needLease(cur, found, site.ID, ip.String(), ai)
	})
	var lease leaseResult
	if needLease {
		lease = s.lookupLease(r.Context(), site.ID, ip.String())
	}
	var o store.ONU
	var v verdict
	err = s.st.Update(func(d *store.Data) error {
		var cur store.ONU
		cur, v = s.decide(d, in, site, ip, ai, lease, now)
		switch v.state {
		case stBlocked, stBadAuth, stCapped:
			return store.ErrNoChange
		}
		o = cur
		return nil
	})
	switch v.state {
	case stBadAuth:
		s.authFailed(ip)
		s.log.Warn("ACS: an Inform used the login of another device", "from", ip.String())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case stBlocked:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case stCapped:
		s.capNote(site)
		http.Error(w, "this router has the most ONUs the server accepts", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.log.Error("ACS: could not record an Inform", "err", err.Error())
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if v.alert != "" && s.alert != nil {
		s.alert("ONU management", v.alert)
	}
	s.mu.Lock()
	if len(s.sessions) >= maxSessions {
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	for _, old := range s.sessions { // one session per device: a new Inform ends the old one
		if old.onuID == o.ID {
			old.cancel()
			delete(s.sessions, old.id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ss := &session{id: randHex(16), ip: ip, onuID: o.ID, ns: msg.NS, started: now, lastAt: now, cancel: cancel, auth: ai,
		out: make(chan []byte), in: make(chan *message, 1), done: make(chan struct{})}
	s.sessions[ss.id] = ss
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: ss.id, Path: "/", HttpOnly: true})
	s.log.Info("ACS: Inform", "onu", o.ID, "serial", o.Serial, "events", strings.Join(in.Events, ","), "from", ip.String(), "state", v.state)
	if v.trust == trustNone {
		// Quarantined or not yet verified: the box gets its answer and nothing else (no secret, no task).
		close(ss.done)
		cancel()
	} else {
		go func() {
			defer close(ss.done)
			defer cancel()
			defer func() {
				if rec := recover(); rec != nil {
					s.log.Error("ACS: a session failed", "onu", o.ID, "panic", fmt.Sprint(rec))
				}
			}()
			s.program(ctx, ss, in, o.ID, v, ai)
		}()
	}
	writeXML(w, informResponse(msg.NS, msg.ID))
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Service) capNote(site store.Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.capLog[site.ID]; ok && s.now().Sub(t) < time.Minute {
		return
	}
	s.capLog[site.ID] = s.now()
	s.log.Warn("ACS: a router has the most ONUs the server accepts; a new one was turned away", "site", site.ID, "max", s.cfg.ACSMaxPerRouter)
}

// applyReported takes the Wi-Fi names an Inform carries (a value change, or a boot) into the device row and
// says whether the saved choice still matches. Values of secret parameters are never looked at.
func applyReported(o *store.ONU, params []ParamValue, want store.WiFiDesired) {
	if len(o.WiFi) == 0 {
		return
	}
	prof := pickProfile(o.Root, o.Manufacturer, o.OUI, o.ProductClass)
	changed := false
	for _, p := range params {
		for i := range o.WiFi {
			if p.Name == o.WiFi[i].Path+prof.SSID {
				if o.WiFi[i].SSID != p.Value && len(p.Value) <= 32 {
					o.WiFi[i].SSID, changed = p.Value, true
				}
			}
		}
	}
	if changed {
		o.State.WiFiChanged = ssidDiffers(*o, want)
	}
}

// ssidDiffers: the device reports another name than the saved choice.
func ssidDiffers(o store.ONU, want store.WiFiDesired) bool {
	if !want.Set {
		return false
	}
	if w, ok := primary(o.WiFi, "2.4"); ok && w.SSID != "" && w.SSID != want.SSID {
		return true
	}
	want5 := want.SSID5
	if want.Same {
		want5 = want.SSID
	}
	if w, ok := primary(o.WiFi, "5"); ok && w.SSID != "" && w.SSID != want5 {
		return true
	}
	return false
}

// homeWiFi is the Wi-Fi choice of the house the ONU is linked to (zero when it is not linked).
func homeWiFi(d *store.Data, o store.ONU) store.WiFiDesired {
	if o.AccountID == "" {
		return store.WiFiDesired{}
	}
	return d.Accounts[o.AccountID].HomeWiFi
}
