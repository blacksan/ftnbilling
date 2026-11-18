package sites

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// afterJoin runs the job once the router first answered (after the automatic
// fixes). Routers of the legacy hotspot flow, with no IPoE mode and no
// "serving customers" intent, are left alone.
func (m *Manager) afterJoin(siteID string) {
	var s store.Site
	m.st.View(func(d *store.Data) { s = d.Sites[siteID] })
	if !billing.IPoE(s) && !m.ServingCustomers(siteID) {
		return
	}
	if err := m.StartProvision(siteID, ProvOptions{Trigger: "join", Actor: ActorSystem}); err != nil {
		m.log.Info("setup job not started after join", "site", siteID, "err", err)
	}
}

// resumeProvisioning applies the restart rules: a job that never armed the kit
// starts over from step 1; one that did is never resumed, the router is told to
// roll back (and its own timer is the backstop).
func (m *Manager) resumeProvisioning() {
	var ids []string
	m.st.View(func(d *store.Data) {
		for id, s := range d.Sites {
			if !s.Removed && d.Settings[provKey(id)].Value != "" {
				ids = append(ids, id)
			}
		}
	})
	for _, id := range ids {
		j, ok := m.ProvJobOf(id)
		if !ok {
			continue
		}
		switch j.State {
		case JobWaiting, JobChecking, JobFixing, JobStaging, JobBuilding, JobArming, JobSwitching, JobVerifying, JobRollingBack:
		default:
			continue
		}
		confirmed := false
		m.st.View(func(d *store.Data) { confirmed = billing.SiteMark(d, id, billing.MarkCutoverConfirmed) })
		if confirmed && j.State != JobRollingBack { // a confirmed router is never rolled back by a restart
			j.Armed = false
		}
		if !j.Armed && j.State != JobRollingBack {
			_ = m.StartProvision(id, ProvOptions{Trigger: "restart", StartNow: j.StartNow, KeepUnknown: j.KeepUnknown, Actor: ActorSystem})
			continue
		}
		j.LastError = "the server restarted during the migration: the router is rolling back"
		m.saveJob(id, j)
		go func(id string) { _ = m.RollbackNow(id, "restart") }(id)
	}
}

// ---- Setup view ----------------------------------------------------------------------------

// SetupJoin is the join part of Setup.
type SetupJoin struct {
	State     string      `json:"state"` // none | waiting | used | expired
	ExpiresAt *time.Time  `json:"expires_at"`
	Report    *JoinReport `json:"report"`
}

// SetupNext tells the client what to offer.
type SetupNext struct {
	Kind    string `json:"kind"` // paste_join | press_button | fix | migrate | retry | none
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

// Setup is the answer of GET /routers/{id}/setup.
type Setup struct {
	RouterID         string       `json:"router_id"`
	State            string       `json:"state"`
	Kind             string       `json:"kind"`
	ServingCustomers bool         `json:"serving_customers"`
	AutoStart        bool         `json:"auto_start"`
	Join             SetupJoin    `json:"join"`
	NextAction       SetupNext    `json:"next_action"`
	Steps            []ProvStep   `json:"steps"`
	Checks           SetupChecks  `json:"checks"`
	Blockers         []string     `json:"blockers"`
	LiveDevices      int          `json:"live_devices"`
	Evidence         ProvEvidence `json:"evidence"`
	DeadlineAt       *time.Time   `json:"deadline_at"`
	SecondsLeft      int          `json:"seconds_left"`
	LastError        string       `json:"last_error"`
}

// SetupView builds the Setup of a router. It reads the router only when no job
// is running and the cached checks are older than 15 seconds.
func (m *Manager) SetupView(ctx context.Context, siteID string) (Setup, bool) {
	var site store.Site
	var ok bool
	m.st.View(func(d *store.Data) { site, ok = d.Sites[siteID] })
	if !ok || site.Removed {
		return Setup{}, false
	}
	now := m.now()
	out := Setup{RouterID: siteID, ServingCustomers: m.ServingCustomers(siteID), Steps: []ProvStep{}, Blockers: []string{}}
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
	j, has := m.ProvJobOf(siteID)
	switch {
	case has:
		out.State, out.Kind, out.Steps, out.LiveDevices, out.Evidence, out.LastError = j.State, j.Kind, j.Steps, j.LiveDevices, j.Evidence, j.LastError
		if j.Blockers != nil {
			out.Blockers = j.Blockers
		}
		if !j.DeadlineAt.IsZero() && (j.State == JobVerifying || j.State == JobSwitching || j.State == JobArming || j.State == JobRollingBack) {
			t := j.DeadlineAt
			out.DeadlineAt = &t
			if s := int(t.Sub(now).Seconds()); s > 0 {
				out.SecondsLeft = s
			}
		}
	case !joined:
		out.State = JobWaiting
	default:
		out.State = "idle"
	}
	if out.Steps == nil {
		out.Steps = []ProvStep{}
	}
	out.AutoStart = billing.IPoE(site) && !out.ServingCustomers || out.ServingCustomers && m.autoStartWindow(siteID)
	if m.SetupBusy(siteID) {
		if c, hit := m.chk.get(siteID, now); hit {
			out.Checks = SetupChecks{c.Summary.OK, c.Summary.Warn, c.Summary.Fail, c.Summary.Unknown}
		}
	} else if joined {
		c, cancel := context.WithTimeout(ctx, 6*time.Second)
		if cs, err := m.Checks(c, siteID, false); err == nil && cs.Router.Reachable {
			out.Checks = SetupChecks{cs.Summary.OK, cs.Summary.Warn, cs.Summary.Fail, cs.Summary.Unknown}
		}
		cancel()
	}
	out.NextAction = nextAction(out, joined, has, j)
	return out, true
}

func nextAction(s Setup, joined, has bool, j ProvJob) SetupNext {
	switch {
	case !joined && !has:
		return SetupNext{Kind: "paste_join", Text: "Paste the join line on the router."}
	case s.State == JobNeedsAction && len(s.Blockers) > 0:
		return SetupNext{Kind: "fix", Text: "Fix this first: " + strings.Join(s.Blockers, ", "), CheckID: s.Blockers[0]}
	case s.State == JobNeedsAction && j.LiveDevices > 0:
		return SetupNext{Kind: "migrate", Text: s.LastError}
	case s.State == JobNeedsAction:
		return SetupNext{Kind: "fix", Text: s.LastError}
	case s.State == JobReady:
		return SetupNext{Kind: "press_button", Text: "Everything is checked. Press Start migration."}
	case s.State == JobFailed || s.State == JobRolledBack || s.State == JobWaiting || s.State == "idle":
		return SetupNext{Kind: "retry", Text: orStr(s.LastError, "Run the set-up.")}
	}
	return SetupNext{Kind: "none", Text: ""}
}

// ErrNameMismatch: the typed router name does not match.
var ErrNameMismatch = fmt.Errorf("type the router name to confirm")

// StartMigration starts the job as a migration (the "Start migration" button).
func (m *Manager) StartMigration(siteID, confirmName string, keepUnknown bool, actor string) error {
	var s store.Site
	var ok bool
	m.st.View(func(d *store.Data) { s, ok = d.Sites[siteID] })
	if !ok || s.Removed {
		return billing.ErrNoSite
	}
	if strings.TrimSpace(confirmName) != s.Name {
		return ErrNameMismatch
	}
	return m.StartProvision(siteID, ProvOptions{Trigger: "migrate", StartNow: true, KeepUnknown: keepUnknown, Actor: actor})
}

// CreateRouter makes a router in the new flow: IPoE access mode (D3), nothing
// built by the join script (the job builds the network), and the intent
// "already serving customers" recorded.
func (m *Manager) CreateRouter(in SiteInput, serving bool) (store.Site, error) {
	in.Provision = false
	s, err := m.CreateSite(in)
	if err != nil {
		return s, err
	}
	err = m.st.Update(func(d *store.Data) error {
		cur := d.Sites[s.ID]
		cur.AccessMode = store.AccessIPoE
		d.Sites[s.ID] = cur
		return nil
	})
	if err != nil {
		return s, err
	}
	m.SetServingCustomers(s.ID, serving)
	s.AccessMode = store.AccessIPoE
	return s, nil
}

// JoinIntentOf is the intent of the router's join line.
func (m *Manager) JoinIntentOf(siteID string) JoinIntent {
	if m.ServingCustomers(siteID) {
		return IntentServing
	}
	return IntentFresh
}
