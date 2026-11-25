package olt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

// Errors of the service.
var (
	ErrNotFound        = errors.New("OLT not found")
	ErrNotConfigured   = errors.New("this OLT has no management address, login name and password yet")
	ErrNoKey           = errors.New("the server has no DATA_KEY, so an OLT password cannot be used")
	ErrBusy            = errors.New("another operation is running on this OLT")
	ErrProvisioningOff = errors.New("the server is not allowed to configure this OLT: an owner must turn provisioning on and confirm the dialect first")
	ErrPlanChanged     = errors.New("the plan changed since you looked at it: review it again")
	ErrNoRouter        = errors.New("the OLT is not on a router")
)

var (
	mgmtAAD   = []byte("olt.mgmt") // the OLT login password (sealed by billing)
	enableAAD = []byte("olt.enable")
	backupAAD = []byte("olt.backup")
)

// Options configure the service. Only Store is required.
type Options struct {
	Store *store.Store
	Box   *secretbox.Box // may be nil: then no password can be used
	// Reach gives the address to dial for an OLT (the router's tunnel address and the forwarded port).
	Reach func(d *store.Data, e store.Equipment, s store.OLTSettings) (string, error)
	Dial  DialFunc
	// SSHSource gives the address the OLT sees the server's SSH sessions coming from: the router's address on its OLT
	// management port (the router masquerades the sessions), or on the VLAN 98 path whatever source that path presents.
	// The SSH restriction of Secure permits exactly this host. Nil: the restriction is skipped.
	SSHSource func(d *store.Data, e store.Equipment, s store.OLTSettings) (string, error)
	// Alert tells the owners (subject, message); never carries a secret.
	Alert       func(subject, msg string)
	Now         func() time.Time
	Log         *slog.Logger
	MaxSessions int           // at once, over all OLTs (default 3)
	Interval    time.Duration // poll interval (default 5 minutes)
	Dialect     *Dialect
	// PortSeen reads what the router shows on its OLT management port of a site (nil: adoption through the port is off).
	PortSeen func(ctx context.Context, siteID string) (PortSeen, error)
	// EnsurePort makes the router's OLT management port rules match the site (heals them; by is the actor).
	EnsurePort func(ctx context.Context, siteID, by string) error
	// ProbeWait is how long to wait after the first knock on an empty port (tests: short; default 1.5 s).
	ProbeWait time.Duration
	// Vendor names the maker from a MAC's OUI ("" when unknown).
	Vendor func(mac string) string
	// ACS gives the address and the shared first-contact login of the TR-069 ACS (nil result: none). The login is a secret:
	// it only ever goes into the lines sent to the OLT.
	ACS func() *ACS
	// Timeout is the time one command may take (default 20 s).
	Timeout time.Duration
}

// Service manages OLTs: reads, backups, plans, applies and the automatic ONU add.
type Service struct {
	o     Options
	sem   chan struct{}
	mu    sync.Mutex
	locks map[string]chan struct{}
	// alerted rate-limits the possible-OLT alerts (key -> last sent).
	alerted map[string]time.Time
}

// New creates the service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = 3
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Minute
	}
	if o.Dialect == nil {
		o.Dialect = VSOL()
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	return &Service{o: o, sem: make(chan struct{}, o.MaxSessions), locks: map[string]chan struct{}{}}
}

// Dialect is the command dialect in use.
func (s *Service) Dialect() *Dialect { return s.o.Dialect }

func (s *Service) now() time.Time { return s.o.Now() }

func (s *Service) lockOf(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.locks[id]
	if !ok {
		c = make(chan struct{}, 1)
		s.locks[id] = c
	}
	return c
}

// ---- audit and state helpers ---------------------------------------------------------------------------------------

func (s *Service) audit(d *store.Data, e store.Equipment, by, msg string) {
	d.AddEvent(store.Event{Time: s.now(), SiteID: e.SiteID, EquipmentID: e.ID, Kind: "audit", Actor: by, Msg: clip(Redact(msg), 3800)})
}

// upd changes an OLT's state in one store update.
func (s *Service) upd(id string, fn func(d *store.Data, e store.Equipment, st *store.OLTState)) error {
	return s.o.Store.Update(func(d *store.Data) error {
		e, ok := d.Equipment[id]
		if !ok || e.Kind != store.KindOLT {
			return ErrNotFound
		}
		st := d.OLTStateOf(id).Clone()
		fn(d, e, &st)
		st.ID, st.Updated = id, s.now()
		d.OLTStates[id] = st
		return nil
	})
}

// Load returns an OLT and its state.
func (s *Service) Load(id string) (store.Equipment, store.OLTState, bool) {
	var e store.Equipment
	var st store.OLTState
	var ok bool
	s.o.Store.View(func(d *store.Data) {
		e, ok = d.Equipment[id]
		if ok && e.Kind == store.KindOLT {
			st = d.OLTStateOf(id).Clone()
		} else {
			ok = false
		}
	})
	if e.MgmtPass != "" {
		e.MgmtPass = "(stored)" // callers only need to know there is one, never the sealed value
	}
	return e, st, ok
}

func errText(err error) string { return clip(Redact(err.Error()), 300) }

// ---- sessions ---------------------------------------------------------------------------------------------------------

type opened struct {
	sess *Session
	e    store.Equipment
	st   store.OLTState
	addr string // where the server dialled
}

// cred is a login used for one session instead of the stored one (the adoption passes). Source says where it came from
// (stored, pending, factory, owner); it is never a secret.
type cred struct {
	User, Pass, Enable string
	Source             string
	// NoPin keeps the SSH host key from being pinned by this session: the device has not been identified yet.
	NoPin bool
}

// withSession logs in to an OLT and runs fn: one session at a time per OLT (wait=false gives up with ErrBusy), and at
// most MaxSessions over all OLTs.
func (s *Service) withSession(ctx context.Context, id string, wait bool, fn func(o *opened) error) error {
	return s.withSessionAs(ctx, id, wait, nil, fn)
}

// withSessionAs is withSession with a login that replaces the stored one (nil = the stored one).
func (s *Service) withSessionAs(ctx context.Context, id string, wait bool, c *cred, fn func(o *opened) error) error {
	lk := s.lockOf(id)
	if wait {
		select {
		case lk <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		select {
		case lk <- struct{}{}:
		default:
			return ErrBusy
		}
	}
	defer func() { <-lk }()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()

	var e store.Equipment
	var st store.OLTState
	var addr string
	var rerr error
	s.o.Store.View(func(d *store.Data) {
		var ok bool
		if e, ok = d.Equipment[id]; !ok || e.Kind != store.KindOLT {
			rerr = ErrNotFound
			return
		}
		st = d.OLTStateOf(id).Clone()
		if c == nil && (e.MgmtIP == "" || e.MgmtUser == "" || e.MgmtPass == "") {
			rerr = ErrNotConfigured
			return
		}
		if s.o.Reach == nil {
			rerr = errors.New("the server cannot reach OLTs: no route is configured")
			return
		}
		addr, rerr = s.o.Reach(d, e, st.Settings)
	})
	if rerr != nil {
		return rerr
	}
	if s.o.Box == nil {
		return ErrNoKey
	}
	user, pass, enable := e.MgmtUser, "", ""
	var err error
	if c != nil {
		user, pass, enable = c.User, c.Pass, c.Enable
	} else {
		if pass, err = s.o.Box.OpenAAD(e.MgmtPass, mgmtAAD); err != nil {
			return errors.New("the stored OLT password cannot be opened with this server's data key")
		}
		if st.Settings.EnablePass != "" {
			if enable, err = s.o.Box.OpenAAD(st.Settings.EnablePass, enableAAD); err != nil {
				return errors.New("the stored enable password cannot be opened with this server's data key")
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	proto := st.Settings.ProtoOrDefault()
	if c != nil {
		proto = store.OLTProtoSSH // the adoption passes never use Telnet
	}
	sess, err := Open(cctx, s.o.Dialect, Target{Addr: addr, Proto: proto, User: user, Pass: pass, Enable: enable,
		PinnedKey: st.Settings.HostKey, Protect: st.Settings.MgmtVLANOrDefault()}, s.o.Dial)
	if err != nil {
		if errors.Is(err, ErrHostKey) {
			s.hostKeyAlert(id, st)
		}
		return err
	}
	defer sess.Close()
	sess.Timeout = s.o.Timeout
	if sess.NewHostKey != "" && (c == nil || !c.NoPin) {
		fp := sess.NewHostKey
		_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
			if st.Settings.HostKey == "" {
				st.Settings.HostKey, st.Settings.HostKeyAt = fp, s.now()
				s.audit(d, e, "system", "SSH host key of OLT "+store.OLTLabel(e)+" pinned on first use: "+fp)
			}
		})
	}
	sess.DisablePaging(cctx)
	return fn(&opened{sess: sess, e: e, st: st, addr: addr})
}

// hostKeyAlert tells the owners that an OLT answered with another SSH host key than the pinned one (a replaced OLT, or
// somebody in the path). The server refused to log in. One alert while the mismatch lasts: the poll's error text
// marks it as sent.
func (s *Service) hostKeyAlert(id string, st store.OLTState) {
	if st.Status.Error == errText(ErrHostKey) {
		return
	}
	var label string
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		label = store.OLTLabel(e)
		st.Status.At, st.Status.OK, st.Status.Error = s.now(), false, errText(ErrHostKey)
		s.audit(d, e, "system", "OLT "+label+": SSH host key mismatch, login refused (pinned "+st.Settings.HostKey+")")
	})
	if s.o.Alert != nil {
		s.o.Alert("OLT "+label+": SSH host key changed", "The OLT answered with a different SSH host key than the one pinned, so the server did not log in. "+
			"If the OLT was replaced, an owner can forget the pinned key on its management page; otherwise check the path to it.")
	}
}

// ---- test, status, poll --------------------------------------------------------------------------------------------

// TestConnection logs in and reads "show version". The result is kept as the OLT's status.
func (s *Service) TestConnection(ctx context.Context, id, by string) (string, error) {
	var vi VersionInfo
	err := s.withSession(ctx, id, true, func(o *opened) error {
		out, err := o.sess.ShowAt(ctx, s.o.Dialect.VersionMode, s.o.Dialect.VersionCmd)
		if err != nil {
			return err
		}
		vi = ParseVersion(out)
		return nil
	})
	ver := vi.Summary()
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Status.At = s.now()
		if err != nil {
			st.Status.OK, st.Status.Error = false, errText(err)
		} else {
			st.Status.OK, st.Status.Error, st.Status.Version = true, "", ver
			st.Status.Model, st.Status.DeviceSerial, st.Status.HWVersion, st.Status.SWVersion = vi.Model, vi.Serial, vi.Hardware, vi.Software
		}
		if by != "" {
			res := "worked"
			if err != nil {
				res = "failed: " + errText(err)
			}
			s.audit(d, e, by, "OLT "+store.OLTLabel(e)+" connection test "+res)
		}
	})
	return ver, err
}

// PollAll polls every OLT that has a management login (one after the other).
func (s *Service) PollAll(ctx context.Context) {
	var ids []string
	s.o.Store.View(func(d *store.Data) {
		for _, e := range d.Equipment {
			if e.Kind == store.KindOLT && e.Status != store.EqRetired && e.SiteID != "" && e.MgmtIP != "" && e.MgmtUser != "" && e.MgmtPass != "" {
				ids = append(ids, e.ID)
			}
		}
	})
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.Poll(ctx, id, false); err != nil && !errors.Is(err, ErrBusy) {
			s.o.Log.Warn("OLT poll failed", "olt", id, "err", errText(err))
		}
	}
}

// Run polls on an interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.o.Store.Healthy() == nil {
				s.AdoptAll(ctx)
				s.PollAll(ctx)
			}
		}
	}
}

// Poll reads the OLT: version, the auto-find list, the ONU states and optics. It records the status and, when the OLT
// is allowed to be configured, authorises the found ONUs that belong here.
func (s *Service) Poll(ctx context.Context, id string, wait bool) error {
	var res pollResult
	err := s.withSession(ctx, id, wait, func(o *opened) error {
		var err error
		res, err = s.readAll(ctx, o)
		if err != nil {
			return err
		}
		s.record(id, res)
		if o.st.Settings.Provisioning && o.st.Settings.DialectChecked {
			s.autoProvision(ctx, o)
		}
		return nil
	})
	if err != nil {
		_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
			st.Status.At, st.Status.OK, st.Status.Error = s.now(), false, errText(err)
		})
	}
	return err
}

type pollResult struct {
	version  VersionInfo
	found    []Found
	onus     []store.OLTONU
	findings []string
	raw      string
	rawDebug string // raw with control characters made visible, for what was not understood
}

func (s *Service) readAll(ctx context.Context, o *opened) (pollResult, error) {
	var r pollResult
	dl := s.o.Dialect
	out, err := o.sess.ShowAt(ctx, dl.VersionMode, dl.VersionCmd)
	if err != nil {
		return r, err
	}
	r.version = ParseVersion(out)
	var raw, dbg []string
	n := o.e.PONPorts
	if n <= 0 {
		n = 4
	}
	seen := map[string]bool{}
	for pon := 1; pon <= n && pon <= 64; pon++ {
		outs, err := o.sess.QueryEach(ctx, dl.PONReadQuery(pon))
		if err != nil && len(outs) == 0 {
			raw = append(raw, fmt.Sprintf("PON %d: %s", pon, errText(err)))
			if errors.Is(err, ErrTimeout) || errors.Is(err, context.Canceled) {
				return r, err
			}
			continue
		}
		by := map[string]CmdOut{}
		for _, c := range outs {
			by[Norm(c.Cmd)] = c
		}
		get := func(cmd string) (string, bool) {
			c, ok := by[Norm(cmd)]
			return c.Out, ok && c.Err == nil
		}
		// The ONU table first; the state table adds to it. Each is read leniently: what cannot be read is shown as it came.
		rows, bad := ParseONUInfo(pon, outOf(by, dl.CmdInfo, &raw, pon))
		if bad != "" {
			raw = append(raw, fmt.Sprintf("PON %d onu info, not understood:\n%s", pon, bad))
			dbg = append(dbg, fmt.Sprintf("PON %d %s:\n%s", pon, dl.CmdInfo, by[Norm(dl.CmdInfo)].Dbg))
		}
		if st, ok := get(dl.CmdState); ok {
			var bad string
			rows, bad = MergeState(rows, pon, st)
			if bad != "" {
				raw = append(raw, fmt.Sprintf("PON %d state, not understood:\n%s", pon, bad))
				dbg = append(dbg, fmt.Sprintf("PON %d %s:\n%s", pon, dl.CmdState, by[Norm(dl.CmdState)].Dbg))
			}
		} else if c, there := by[Norm(dl.CmdState)]; there && c.Err != nil {
			raw = append(raw, fmt.Sprintf("PON %d state: %s", pon, errText(c.Err)))
		}
		if len(rows) > 0 {
			if op, ok := get(dl.CmdOptical); ok {
				if bad := MergeOptical(rows, pon, op); bad != "" {
					raw = append(raw, fmt.Sprintf("PON %d optics, not understood:\n%s", pon, bad))
					dbg = append(dbg, fmt.Sprintf("PON %d %s:\n%s", pon, dl.CmdOptical, by[Norm(dl.CmdOptical)].Dbg))
				}
			} else if c, there := by[Norm(dl.CmdOptical)]; there && c.Err != nil {
				raw = append(raw, fmt.Sprintf("PON %d optics: %s", pon, errText(c.Err)))
			}
			if dist, ok := get(dl.CmdDistance); ok {
				MergeDistance(rows, pon, dist)
			}
		}
		r.onus = append(r.onus, rows...)
		if af, ok := get(dl.CmdAutoFind); ok {
			for _, f := range ParseAutoFindOn(pon, af) {
				if k := fmt.Sprint(f.PON) + "|" + f.Serial; !seen[k] {
					seen[k] = true
					r.found = append(r.found, f)
				}
			}
		}
	}
	// The OLT's own settings worth knowing (best effort: a failure here is not a failed poll).
	if txt, err := s.readRunning(ctx, o.sess); err == nil {
		r.findings = ParseRunning(txt).Findings()
	}
	r.raw = clip(Redact(strings.Join(raw, "\n")), 4000)
	r.rawDebug = clip(Redact(strings.Join(dbg, "\n")), 6000)
	return r, nil
}

// outOf is the output of a command of a Query, noting a refusal in raw.
func outOf(by map[string]CmdOut, cmd string, raw *[]string, pon int) string {
	c, ok := by[Norm(cmd)]
	if !ok {
		return ""
	}
	if c.Err != nil {
		*raw = append(*raw, fmt.Sprintf("PON %d %s: %s", pon, cmd, errText(c.Err)))
		return ""
	}
	return c.Out
}

// record stores a poll's result, classifies what was found and sends the (rate-limited) alerts.
func (s *Service) record(id string, r pollResult) {
	var alerts []string
	var olt string
	_ = s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		olt = store.OLTLabel(e)
		now := s.now()
		st.Status = store.OLTStatus{At: now, OK: true, Version: r.version.Summary(), Model: r.version.Model, DeviceSerial: r.version.Serial,
			HWVersion: r.version.Hardware, SWVersion: r.version.Software, ONUs: r.onus, Raw: r.raw, RawDebug: r.rawDebug, Findings: r.findings}
		// ONUs that are on the OLT but not in the inventory: listed for the owner, never touched.
		for _, o := range r.onus {
			if o.Serial == "" {
				continue
			}
			if _, ok := d.ONTBySerial(o.Serial); !ok {
				st.Status.Findings = append(st.Status.Findings, fmt.Sprintf("ONU %s (PON %d, ID %d) is on the OLT but not in the inventory. The server never removes it.", o.Serial, o.PON, o.ID))
			}
		}
		old := map[string]store.OLTFound{}
		for _, f := range st.Found {
			old[fmt.Sprint(f.PON)+"|"+f.Serial] = f
		}
		var found []store.OLTFound
		for _, f := range r.found {
			key := fmt.Sprint(f.PON) + "|" + f.Serial
			v, detail, eid := Classify(d, e, f)
			rec := store.OLTFound{PON: f.PON, Serial: f.Serial, FirstSeen: now, LastSeen: now, Verdict: v, Detail: detail, EquipmentID: eid}
			if prev, ok := old[key]; ok {
				rec.FirstSeen, rec.AlertedAt, rec.TriedAt = prev.FirstSeen, prev.AlertedAt, prev.TriedAt
				if (prev.Verdict == store.OLTFoundFailed || prev.Verdict == store.OLTFoundHeld && prev.EquipmentID != "" && v == VerdictReady) && now.Sub(prev.TriedAt) < 30*time.Minute && v == VerdictReady {
					rec.Verdict, rec.Detail = prev.Verdict, prev.Detail // wait out the retry interval
				}
			}
			if (v == VerdictUnknown || v == VerdictHeld) && now.Sub(rec.AlertedAt) >= 24*time.Hour {
				rec.AlertedAt = now
				if v == VerdictUnknown {
					alerts = append(alerts, fmt.Sprintf("PON %d: %s is not in the inventory (%s). It was NOT added.", f.PON, f.Serial, detail))
				} else {
					alerts = append(alerts, fmt.Sprintf("PON %d: %s is held: %s.", f.PON, f.Serial, detail))
				}
			}
			found = append(found, rec)
		}
		if len(found) > 64 {
			found = found[:64]
		}
		st.Found = found
	})
	if len(alerts) > 0 && s.o.Alert != nil {
		s.o.Alert("OLT "+olt+": ONU found", clip(strings.Join(alerts, " "), 900))
	}
}

// ---- backups -----------------------------------------------------------------------------------------------------------

func (s *Service) newBackup(running, why, by string) store.OLTBackup {
	b := store.OLTBackup{ID: fmt.Sprintf("b%d", s.now().UnixNano()), At: s.now(), Why: why, By: by, Size: len(running)}
	if s.o.Box != nil {
		if sealed, err := s.o.Box.SealAAD(running, backupAAD); err == nil {
			b.Text, b.Sealed = sealed, true
			return b
		}
	}
	b.Text = Redact(running) // no data key: keep only the redacted copy
	return b
}

// backupIn reads the running configuration inside an open session and saves it.
func (s *Service) backupIn(ctx context.Context, o *opened, why, by string) (store.OLTBackup, string, error) {
	out, err := s.readRunning(ctx, o.sess)
	if err != nil {
		return store.OLTBackup{}, "", err
	}
	if len(out) > 2<<20 {
		out = out[:2<<20]
	}
	b := s.newBackup(out, why, by)
	err = s.upd(o.e.ID, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Backups = append(st.Backups, b)
		if n := len(st.Backups); n > store.MaxOLTBackups {
			st.Backups = append([]store.OLTBackup(nil), st.Backups[n-store.MaxOLTBackups:]...)
		}
		s.audit(d, e, by, fmt.Sprintf("OLT %s configuration saved (%s, %d bytes)", store.OLTLabel(e), why, len(out)))
	})
	return b, out, err
}

// Backup saves the OLT's running configuration now.
func (s *Service) Backup(ctx context.Context, id, why, by string) (store.OLTBackup, error) {
	var b store.OLTBackup
	err := s.withSession(ctx, id, true, func(o *opened) error {
		var err error
		b, _, err = s.backupIn(ctx, o, why, by)
		return err
	})
	b.Text = ""
	return b, err
}

// BackupList lists the saved configurations, newest first, without their text.
func (s *Service) BackupList(id string) []store.OLTBackup {
	_, st, ok := s.Load(id)
	if !ok {
		return nil
	}
	out := make([]store.OLTBackup, 0, len(st.Backups))
	for i := len(st.Backups) - 1; i >= 0; i-- {
		b := st.Backups[i]
		b.Text = ""
		out = append(out, b)
	}
	return out
}

// BackupText returns one saved configuration with its secrets redacted.
func (s *Service) BackupText(id, backupID string) (store.OLTBackup, string, error) {
	_, st, ok := s.Load(id)
	if !ok {
		return store.OLTBackup{}, "", ErrNotFound
	}
	for _, b := range st.Backups {
		if b.ID != backupID {
			continue
		}
		text := b.Text
		if b.Sealed {
			if s.o.Box == nil {
				return b, "", ErrNoKey
			}
			p, err := s.o.Box.OpenAAD(b.Text, backupAAD)
			if err != nil {
				return b, "", errors.New("the saved copy cannot be opened with this server's data key")
			}
			text = p
		}
		b.Text = ""
		return b, Redact(text), nil
	}
	return store.OLTBackup{}, "", ErrNotFound
}

// ---- settings and permission --------------------------------------------------------------------------------------------

// SettingsInput is the connection form of an OLT.
type SettingsInput struct {
	Proto        string
	Port         int
	EnablePass   string // "" keeps the stored one
	ClearEnable  bool
	ClearHostKey bool
	MgmtVLAN     int
	UplinkPort   string
	InternetVLAN int
}

// SaveSettings stores the connection settings. The enable password is sealed and never returned.
func (s *Service) SaveSettings(id string, in SettingsInput, by string) error {
	proto := strings.ToLower(strings.TrimSpace(in.Proto))
	if proto == "" {
		proto = store.OLTProtoSSH
	}
	if proto != store.OLTProtoSSH && proto != store.OLTProtoTelnet {
		return errors.New("choose SSH or Telnet")
	}
	if in.Port < 0 || in.Port > 65535 {
		return errors.New("the port must be 1 to 65535")
	}
	if in.MgmtVLAN != 0 && (in.MgmtVLAN < 2 || in.MgmtVLAN > 4094) {
		return errors.New("the OLT management VLAN must be 2 to 4094")
	}
	if in.InternetVLAN != 0 && (in.InternetVLAN < 2 || in.InternetVLAN > 4094) {
		return errors.New("the internet VLAN must be 2 to 4094")
	}
	up := strings.TrimSpace(in.UplinkPort)
	if up != "" && !uplinkRE.MatchString(up) {
		return errors.New("the uplink port is written like 0/1")
	}
	if len(in.EnablePass) > 128 {
		return errors.New("the enable password is too long")
	}
	var sealed string
	if in.EnablePass != "" {
		if s.o.Box == nil {
			return ErrNoKey
		}
		var err error
		if sealed, err = s.o.Box.SealAAD(in.EnablePass, enableAAD); err != nil {
			return errors.New("could not store the enable password")
		}
	}
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		set := &st.Settings
		set.Proto, set.Port, set.MgmtVLAN, set.UplinkPort, set.InternetVLAN = proto, in.Port, in.MgmtVLAN, up, in.InternetVLAN
		if proto == store.OLTProtoTelnet {
			set.HostKey, set.HostKeyAt = "", time.Time{}
		}
		switch {
		case in.ClearEnable:
			set.EnablePass = ""
		case sealed != "":
			set.EnablePass = sealed
		}
		if in.ClearHostKey {
			set.HostKey, set.HostKeyAt = "", time.Time{}
		}
		s.audit(d, e, by, "OLT "+store.OLTLabel(e)+" connection settings saved")
	})
}

// SetProvisioning turns the server's permission to configure the OLT on or off.
func (s *Service) SetProvisioning(id string, on bool, by string) error {
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Settings.Provisioning, st.Settings.ProvisioningBy, st.Settings.ProvisioningAt = on, by, s.now()
		word := "OFF"
		if on {
			word = "ON"
		}
		s.audit(d, e, by, "OLT "+store.OLTLabel(e)+": letting the server configure it turned "+word)
	})
}

// SetDialectChecked records that an owner looked at the plan preview and the command syntax is right for this OLT.
func (s *Service) SetDialectChecked(id string, on bool, by string) error {
	return s.upd(id, func(d *store.Data, e store.Equipment, st *store.OLTState) {
		st.Settings.DialectChecked, st.Settings.CheckedBy, st.Settings.CheckedAt = on, by, s.now()
		s.audit(d, e, by, fmt.Sprintf("OLT %s: command syntax marked as checked = %v", store.OLTLabel(e), on))
	})
}

var uplinkRE = regexp.MustCompile(`^\d{1,2}/\d{1,3}$`)
