package sites

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/store"
)

// The Setup view of a router (docs/api/v5-bootstrap.md). There is no job: the
// view is derived from what the server knows (the join token, the WireGuard
// handshake, whether the REST login worked, the cached checks).

// Setup states.
const (
	StateNotStarted    = "not_started"
	StateWaitingRouter = "waiting_router"
	StateOnline        = "online"
	StateReady         = "ready"
)

// TunnelFresh is how recent a WireGuard handshake must be for the tunnel to count as up.
const TunnelFresh = 3 * time.Minute

// SetupJoin is the join part of Setup.
type SetupJoin struct {
	State     string      `json:"state"` // none | waiting | used | expired
	ExpiresAt *time.Time  `json:"expires_at"`
	Report    *JoinReport `json:"report"`
}

// SetupNext tells the client what to offer.
type SetupNext struct {
	Kind    string `json:"kind"` // paste_line | press_button | fix | wait | none
	Text    string `json:"text"`
	CheckID string `json:"check_id"`
}

// SetupChecks counts the checklist.
type SetupChecks struct {
	OK      int `json:"ok"`
	Warn    int `json:"warn"`
	Fail    int `json:"fail"`
	Unknown int `json:"unknown"`
}

// SetupStep is one of the four derived steps.
type SetupStep struct {
	N      int        `json:"n"`
	Key    string     `json:"key"`
	Title  string     `json:"title"`
	Status string     `json:"status"` // pending | running | ok | failed
	Detail string     `json:"detail"`
	At     *time.Time `json:"at"`
}

// Setup is the answer of GET /routers/{id}/setup.
type Setup struct {
	RouterID   string      `json:"router_id"`
	State      string      `json:"state"`
	Join       SetupJoin   `json:"join"`
	NextAction SetupNext   `json:"next_action"`
	Steps      []SetupStep `json:"steps"`
	Checks     SetupChecks `json:"checks"`
	Blockers   []string    `json:"blockers"`
	LastError  string      `json:"last_error"`
}

// Step keys.
const (
	StepFetched = "script_fetched"
	StepTunnel  = "tunnel_up"
	StepLogin   = "server_login"
	StepChecks  = "checks_green"
)

// peek returns the last pass of a router, however old, and when it was made.
func (cs *checksState) peek(siteID string) (Checks, time.Time, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	e, ok := cs.cache[siteID]
	return e.c, e.at, ok
}

// refreshChecks reads the router in the background (one pass at a time per
// router) so the Setup view never waits for the router.
func (m *Manager) refreshChecks(siteID string) {
	if m.chk.rechecks(siteID, true) {
		return // one is already waiting or running
	}
	m.mu.RLock()
	ctx := m.ctx
	m.mu.RUnlock()
	if ctx == nil || ctx.Err() != nil {
		m.chk.rechecks(siteID, false)
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.chk.rechecks(siteID, false)
		_, _ = m.Checks(ctx, siteID, true)
	}()
}

// SetupView builds the Setup of a router from the server's own state. It never
// waits for the router: the checks come from the cache, which a background
// pass refreshes when it is older than a minute.
func (m *Manager) SetupView(_ context.Context, siteID string) (Setup, bool) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return Setup{}, false
	}
	now := m.now()
	out := Setup{RouterID: siteID, Steps: make([]SetupStep, 0, 4), Blockers: []string{}}
	out.Join.State = "none"
	switch TokenStateOf(site.Join, now) {
	case TokenPending:
		out.Join.State = "waiting"
	case TokenUsed:
		out.Join.State = "used"
	case TokenExpired:
		out.Join.State = "expired"
	}
	if !site.Join.ExpiresAt.IsZero() && out.Join.State != "none" {
		t := site.Join.ExpiresAt
		out.Join.ExpiresAt = &t
	}
	out.Join.Report = m.loadJoinReport(siteID)
	joined := site.WGPublicKey != "" && !site.JoinedAt.IsZero()
	fetched := !site.Join.UsedAt.IsZero() || joined

	// Tunnel: a handshake younger than 3 minutes. Without the status dump
	// (a dev machine) a joined router counts as up.
	var handshakeAt *time.Time
	tunnel := false
	if site.WGPublicKey != "" {
		ps, found, known := m.PeerStatus(siteID)
		if known {
			if age, has := ps.HandshakeAge(now); found && has {
				t := now.Add(-age)
				handshakeAt = &t
				tunnel = age < TunnelFresh
			}
		} else {
			tunnel = joined
		}
	}

	// Checks: from the cache; refresh in the background when old.
	var cs Checks
	haveChecks := false
	if joined && tunnel {
		c, at, hit := m.chk.peek(siteID)
		if hit {
			cs, haveChecks = c, true
		}
		if !hit || now.Sub(at) >= checksCacheFor {
			m.refreshChecks(siteID)
		}
	}
	green := false
	if haveChecks {
		out.Checks = SetupChecks{cs.Summary.OK, cs.Summary.Warn, cs.Summary.Fail, cs.Summary.Unknown}
		if b := cs.Blockers(false, false); b != nil {
			out.Blockers = b
		}
		green = cs.Summary.Fail == 0 && len(out.Blockers) == 0
	}

	switch {
	case !fetched:
		out.State = StateNotStarted
	case !tunnel:
		out.State = StateWaitingRouter
	case !joined || !green:
		out.State = StateOnline
	default:
		out.State = StateReady
	}

	// Steps.
	failedReport := out.Join.Report != nil && !out.Join.Report.OK
	step := func(n int, key, title string) *SetupStep {
		out.Steps = append(out.Steps, SetupStep{N: n, Key: key, Title: title, Status: "pending"})
		return &out.Steps[len(out.Steps)-1]
	}
	s1 := step(1, StepFetched, "Router fetched the script")
	s2 := step(2, StepTunnel, "Tunnel is up")
	s3 := step(3, StepLogin, "Server can log in")
	s4 := step(4, StepChecks, "Checks are green")
	if fetched {
		s1.Status = "ok"
		if !site.Join.UsedAt.IsZero() {
			t := site.Join.UsedAt
			s1.At = &t
		}
		if failedReport {
			s1.Status, s1.Detail = "failed", "The script reported a problem: "+firstLine(out.Join.Report.Lines)
		}
	} else {
		s1.Status, s1.Detail = "running", "Waiting for the line to be pasted on the router."
	}
	if tunnel {
		s2.Status, s2.At = "ok", handshakeAt
	} else if fetched {
		s2.Status, s2.Detail = "running", "Waiting for the router to build itself and connect."
	}
	if joined && tunnel {
		t := site.JoinedAt
		s3.Status, s3.At = "ok", &t
	} else if tunnel {
		s3.Status, s3.Detail = "running", "The tunnel is up; trying the server's login."
	}
	switch {
	case joined && tunnel && green:
		t := cs.GeneratedAt
		s4.Status, s4.At = "ok", &t
	case joined && tunnel && haveChecks:
		s4.Status, s4.Detail = "failed", checksDetail(out)
	case joined && tunnel:
		s4.Status, s4.Detail = "running", "Checking the router."
	}

	out.NextAction = nextAction(out, site, cs, haveChecks)
	if failedReport {
		out.LastError = firstLine(out.Join.Report.Lines)
	}
	return out, true
}

func firstLine(l []string) string {
	if len(l) == 0 {
		return "see the router's report"
	}
	return l[0]
}

func checksDetail(s Setup) string {
	if len(s.Blockers) > 0 {
		return "Fix this first: " + strings.Join(s.Blockers, ", ")
	}
	return "Some checks fail."
}

func nextAction(s Setup, site store.Site, cs Checks, haveChecks bool) SetupNext {
	switch s.State {
	case StateNotStarted:
		if s.Join.State == "waiting" {
			return SetupNext{Kind: "paste_line", Text: "Paste the line on the router (it erases the router and builds it again)."}
		}
		return SetupNext{Kind: "paste_line", Text: "Make a new line, then paste it on the router."}
	case StateWaitingRouter:
		if it, ok := cs.Item(CheckDMFetch); haveChecks && ok && it.Status == StatusFail {
			return SetupNext{Kind: "press_button", Text: "Press the router's reset button within 5 minutes when it asks, then paste the same line again.", CheckID: CheckDMFetch}
		}
		return SetupNext{Kind: "wait", Text: "The router is building itself. This takes about two minutes."}
	case StateOnline:
		if !haveChecks {
			return SetupNext{Kind: "wait", Text: "Checking the router."}
		}
		for _, id := range s.Blockers {
			return SetupNext{Kind: "fix", Text: "Fix this first: " + id, CheckID: id}
		}
		for _, it := range cs.Items {
			if it.Status == StatusFail {
				return SetupNext{Kind: "fix", Text: it.Title, CheckID: it.ID}
			}
		}
		return SetupNext{Kind: "wait", Text: "Almost there."}
	}
	return SetupNext{Kind: "none"}
}

// SetupStateOf is the state name of a router (live data and the overview show it).
func (m *Manager) SetupStateOf(siteID string) string {
	v, ok := m.SetupView(context.Background(), siteID)
	if !ok {
		return ""
	}
	return v.State
}

// CreateRouter makes a router in the new flow: IPoE, nothing built until the
// operator pastes the line.
func (m *Manager) CreateRouter(in SiteInput) (store.Site, error) {
	return m.CreateSite(in)
}

// afterJoin runs once the router first answered (after the automatic fixes):
// a fresh pass of checks, a fixed address for every flat, a task for every
// flat, and one reconcile pass so the router gets the flats' leases and queues.
func (m *Manager) afterJoin(ctx context.Context, siteID string) {
	if _, err := m.Checks(ctx, siteID, true); err != nil {
		m.log.Warn("checks after the join failed", "site", siteID, "err", err)
	}
	p := m.provisioner()
	if p == nil {
		return
	}
	if _, err := p.EnsureIPs(siteID); err != nil {
		m.log.Warn("could not give the flats their addresses", "site", siteID, "err", err)
	}
	p.EnqueueSite(siteID, "router set up")
	rc, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := p.ReconcileSite(rc, siteID); err != nil {
		m.log.Info("first reconcile after the join did not finish; the task queue retries", "site", siteID, "err", err)
	}
}

// ErrNameMismatch: the typed router name does not match.
var ErrNameMismatch = fmt.Errorf("type the router name to confirm")
