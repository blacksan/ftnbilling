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
	n      int // requests made
	seq    int
}

func (ss *session) touch(t time.Time) { ss.mu.Lock(); ss.lastAt = t; ss.mu.Unlock() }
func (ss *session) last() time.Time   { ss.mu.Lock(); defer ss.mu.Unlock(); return ss.lastAt }

const maxRPCs = 80

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

func (s *Service) handleInform(w http.ResponseWriter, r *http.Request, site store.Site, ip netip.Addr, msg *message) {
	in, err := parseInform(msg.Body)
	if err != nil {
		http.Error(w, "bad Inform", http.StatusBadRequest)
		return
	}
	now := s.now()
	var o store.ONU
	var isNew, capped bool
	var prevIP string
	err = s.st.Update(func(d *store.Data) error {
		cur, found := d.ONUBySerial(in.OUI, in.Serial)
		if !found {
			n := 0
			for _, x := range d.ONUs {
				if x.SiteID == site.ID {
					n++
				}
			}
			if n >= s.cfg.ACSMaxPerRouter {
				capped = true
				return store.ErrNoChange
			}
			cur = store.ONU{ID: newONUID(), Serial: in.Serial, OUI: in.OUI, FirstSeen: now}
			isNew = true
		}
		prevIP = cur.MgmtIP
		cur.Manufacturer, cur.ProductClass = nonEmpty(in.Manufacturer, cur.Manufacturer), nonEmpty(in.ProductClass, cur.ProductClass)
		if cur.OUI == "" {
			cur.OUI = in.OUI
		}
		cur.SiteID, cur.MgmtIP, cur.LastInform, cur.LastEvents = site.ID, ip.String(), now, strings.Join(in.Events, ",")
		if root := rootOf(in.Params); root != "" {
			if cur.Root != root {
				cur.Profile = store.ONUProfile{Root: root}
			}
			cur.Root = root
		}
		if cur.Root != "" {
			if v, ok := in.Param(cur.Root + "ManagementServer.ConnectionRequestURL"); ok && len(v) < 300 {
				cur.ConnReqURL = v
			}
			if v, ok := in.Param(cur.Root + "DeviceInfo.SoftwareVersion"); ok && len(v) < 100 {
				cur.SWVersion = v
			}
			if v, ok := in.Param(cur.Root + "DeviceInfo.HardwareVersion"); ok && len(v) < 100 {
				cur.HWVersion = v
			}
		}
		applyReported(&cur, in.Params)
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
		autoLink(d, cur.ID)
		o = d.ONUs[cur.ID]
		return nil
	})
	if capped {
		s.capNote(site)
		http.Error(w, "this router has the most ONUs the server accepts", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.log.Error("ACS: could not record an Inform", "err", err.Error())
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
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
	ss := &session{id: randHex(16), ip: ip, onuID: o.ID, ns: msg.NS, started: now, lastAt: now, cancel: cancel,
		out: make(chan []byte), in: make(chan *message, 1), done: make(chan struct{})}
	s.sessions[ss.id] = ss
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: ss.id, Path: "/", HttpOnly: true})
	s.log.Info("ACS: Inform", "onu", o.ID, "serial", o.Serial, "events", strings.Join(in.Events, ","), "from", ip.String())
	stable := !isNew && prevIP == ip.String()
	go func() {
		defer close(ss.done)
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("ACS: a session failed", "onu", o.ID, "panic", fmt.Sprint(rec))
			}
		}()
		s.program(ctx, ss, in, o.ID, stable)
	}()
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
func applyReported(o *store.ONU, params []ParamValue) {
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
		o.Desired.Changed = ssidDiffers(*o)
	}
}

// ssidDiffers: the device reports another name than the saved choice.
func ssidDiffers(o store.ONU) bool {
	if !o.Desired.Set {
		return false
	}
	if w, ok := primary(o.WiFi, "2.4"); ok && w.SSID != "" && w.SSID != o.Desired.SSID {
		return true
	}
	want5 := o.Desired.SSID5
	if o.Desired.Same {
		want5 = o.Desired.SSID
	}
	if w, ok := primary(o.WiFi, "5"); ok && w.SSID != "" && w.SSID != want5 {
		return true
	}
	return false
}
