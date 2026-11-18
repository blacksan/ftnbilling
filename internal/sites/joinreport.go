package sites

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"ispbill/internal/routeros"
	"ispbill/internal/store"
)

const (
	// ReportWindow is how long after a join script was handed out its token
	// still accepts the script's report.
	ReportWindow = 30 * time.Minute
	// MaxReportBytes caps a report body.
	MaxReportBytes = 2048
)

// ErrReportTooLarge is returned for a report body over MaxReportBytes.
var ErrReportTooLarge = errors.New("join report is too large")

// JoinReport is what the join script said about itself: whether every section
// worked, and one line per problem with the router's own error text.
type JoinReport struct {
	At    time.Time `json:"at"`
	OK    bool      `json:"ok"`
	ROS   string    `json:"ros,omitempty"`
	Lines []string  `json:"lines"`
}

// JoinReportKey is the setting that holds a building's last join report.
func JoinReportKey(siteID string) string { return "site." + siteID + ".join_report" }

// sanitizeReport keeps printable ASCII and newlines (anything else becomes
// "?") and masks secrets the way router output is masked.
func sanitizeReport(b []byte) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case c == '\n' || (c >= 0x20 && c < 0x7f):
			out = append(out, c)
		case c == '\r':
		default:
			out = append(out, '?')
		}
	}
	return routeros.RedactOutput(string(out))
}

// parseReport reads "result ok|fail ros <version>" on the first line and one
// problem per following line.
func parseReport(text string, at time.Time) JoinReport {
	r := JoinReport{At: at, Lines: []string{}}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > 0 {
		f := strings.Fields(lines[0])
		if len(f) >= 2 && f[0] == "result" {
			r.OK = f[1] == "ok"
			for i := 2; i+1 < len(f); i++ {
				if f[i] == "ros" {
					r.ROS = f[i+1]
				}
			}
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			r.Lines = append(r.Lines, l)
		}
	}
	return r
}

// SubmitJoinReport stores a join script's report. The token must be the one
// the script was fetched with, within ReportWindow of that fetch; every other
// case is ErrJoinInvalid (the web layer answers it with the same 404 as a bad
// join link).
func (m *Manager) SubmitJoinReport(token string, body []byte) error {
	if token == "" || len(token) > 128 {
		return ErrJoinInvalid
	}
	want := HashToken(token)
	now := m.now()
	err := m.st.Update(func(d *store.Data) error {
		var site store.Site
		found := false
		for _, s := range d.Sites {
			if s.Join.Hash != "" && subtle.ConstantTimeCompare([]byte(want), []byte(s.Join.Hash)) == 1 && !s.Removed {
				site, found = s, true
			}
		}
		if !found || site.Join.UsedAt.IsZero() || now.After(site.Join.UsedAt.Add(ReportWindow)) {
			return ErrJoinInvalid
		}
		if len(body) > MaxReportBytes {
			return ErrReportTooLarge
		}
		rep := parseReport(sanitizeReport(body), now)
		v, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		k := JoinReportKey(site.ID)
		if d.Settings == nil {
			d.Settings = map[string]store.Setting{}
		}
		d.Settings[k] = store.Setting{Key: k, Value: string(v), UpdatedAt: now}
		d.AddEvent(store.Event{Time: now, SiteID: site.ID, Kind: "audit", Msg: joinReportAudit(rep)})
		return nil
	})
	return err
}

func joinReportAudit(r JoinReport) string {
	if r.OK {
		return "join report: all sections worked"
	}
	return "join report: problems (see the building page)"
}

// JoinReport returns a building's last join report (W5a's checks and the
// admin pages show it).
func (m *Manager) JoinReport(siteID string) (JoinReport, bool) {
	var raw string
	var ok bool
	m.st.View(func(d *store.Data) {
		var s store.Setting
		s, ok = d.Settings[JoinReportKey(siteID)]
		raw = s.Value
	})
	if !ok {
		return JoinReport{}, false
	}
	var r JoinReport
	if json.Unmarshal([]byte(raw), &r) != nil {
		return JoinReport{}, false
	}
	return r, true
}

// refusals remembers, per building, the last fetch with a valid join token
// that was turned away for its User-Agent (a browser, a link previewer, or a
// router whose fetch sends something this server does not accept). It lives
// in memory: it only explains why a join link seems dead.
type refusals struct {
	mu sync.Mutex
	m  map[string]refusal
}

type refusal struct {
	At time.Time
	UA string
}

func (r *refusals) note(siteID, ua string, at time.Time) {
	ua = strings.Map(func(c rune) rune {
		if c < 0x20 || c > 0x7e {
			return '?'
		}
		return c
	}, ua)
	if len(ua) > 80 {
		ua = ua[:80]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]refusal{}
	}
	r.m[siteID] = refusal{At: at, UA: ua}
}

func (r *refusals) get(siteID string) (refusal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[siteID]
	return v, ok
}

func (r *refusals) clear(siteID string) {
	r.mu.Lock()
	delete(r.m, siteID)
	r.mu.Unlock()
}

// noteRefusedUA records a refused fetch if the token is a live, unused one.
func (m *Manager) noteRefusedUA(token, ua string) {
	if token == "" || len(token) > 128 {
		return
	}
	want := HashToken(token)
	now := m.now()
	var sid string
	m.st.View(func(d *store.Data) {
		for _, s := range d.Sites {
			if s.Join.Hash != "" && subtle.ConstantTimeCompare([]byte(want), []byte(s.Join.Hash)) == 1 && !s.Removed && s.Join.UsedAt.IsZero() && now.Before(s.Join.ExpiresAt) {
				sid = s.ID
			}
		}
	})
	if sid != "" {
		m.refused.note(sid, ua, now)
	}
}

// stageROS carries the fetching router's RouterOS version from
// ConsumeJoinForce to the cutover and cleanup renderers (which build their
// own ScriptInput in ipoe.go), keyed by building. Used only when
// ScriptInput.ROSVersion is empty. Set and cleared around the render.
var stageROS sync.Map
