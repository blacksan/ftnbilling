// Package alerts turns fleet probes and router metrics into admin alerts:
// rules with hold times, firing and recovery messages, cooldown, repeat,
// mute, and a storm guard for many buildings failing at once.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/fleet"
	"ispbill/internal/metrics"
	"ispbill/internal/notify"
	"ispbill/internal/store"
)

// Rule keys.
const (
	RuleOffline = "offline"
	RuleWANSat  = "wan_sat"
	RuleLoss    = "loss"
	RuleLatency = "latency"
	RuleOLTDown = "olt_down"
)

// RuleOrder is the display order.
var RuleOrder = []string{RuleOffline, RuleWANSat, RuleLoss, RuleLatency, RuleOLTDown}

// Label is the short name used in messages and on the page.
func Label(rule string) string {
	switch rule {
	case RuleOffline:
		return "Building offline"
	case RuleWANSat:
		return "WAN saturated"
	case RuleLoss:
		return "Packet loss"
	case RuleLatency:
		return "High latency"
	case RuleOLTDown:
		return "OLT port down"
	}
	return rule
}

// Unit describes a rule's threshold for the settings page.
func Unit(rule string) string {
	switch rule {
	case RuleWANSat:
		return "% of WAN capacity"
	case RuleLoss:
		return "% packet loss"
	case RuleLatency:
		return "ms average round trip"
	}
	return ""
}

// Alert states.
const (
	Pending = "pending"
	Firing  = "firing"
)

const (
	historyCap  = 1000
	stormWindow = 2 * time.Minute
	stale       = 5 * time.Minute // live readings older than this are ignored
)

// Rule is one editable alert rule.
type Rule struct {
	Enabled   bool    `json:"enabled"`
	Threshold float64 `json:"threshold"` // not used by offline and olt_down
	HoldMin   int     `json:"hold_min"`
}

// Settings are edited on the Alerts page.
type Settings struct {
	Rules         map[string]Rule `json:"rules"`
	Email         bool            `json:"email"`
	WhatsApp      bool            `json:"whatsapp"`
	CooldownMin   int             `json:"cooldown_min"` // per key, between two "problem" messages
	RepeatEnabled bool            `json:"repeat_enabled"`
	RepeatHours   int             `json:"repeat_hours"`
	// OutageNotice decides whether tenants are told about an outage: off,
	// manual (a button), or auto (after the offline alert has fired for AutoMin).
	OutageNotice string `json:"outage_notice"`
	AutoMin      int    `json:"auto_min"`
}

// DefaultSettings are the factory rules.
func DefaultSettings() Settings {
	return Settings{
		Rules: map[string]Rule{
			RuleOffline: {Enabled: true, HoldMin: 5},
			RuleWANSat:  {Enabled: true, Threshold: 85, HoldMin: 15},
			RuleLoss:    {Enabled: true, Threshold: 5, HoldMin: 10},
			RuleLatency: {Enabled: true, Threshold: 150, HoldMin: 10},
			RuleOLTDown: {Enabled: true, HoldMin: 2},
		},
		Email: true, WhatsApp: true,
		CooldownMin: 60, RepeatHours: 6, OutageNotice: "manual", AutoMin: 30,
	}
}

// Validate checks the numbers an admin typed.
func (s Settings) Validate() error {
	for _, k := range RuleOrder {
		r, ok := s.Rules[k]
		if !ok {
			return fmt.Errorf("rule %s is missing", k)
		}
		if r.HoldMin < 1 || r.HoldMin > 1440 {
			return fmt.Errorf("%s: the hold time must be 1 to 1440 minutes", Label(k))
		}
		switch k {
		case RuleWANSat, RuleLoss:
			if r.Threshold <= 0 || r.Threshold > 100 {
				return fmt.Errorf("%s: the threshold must be above 0 and at most 100", Label(k))
			}
		case RuleLatency:
			if r.Threshold <= 0 || r.Threshold > 10000 {
				return fmt.Errorf("%s: the threshold must be 1 to 10000 ms", Label(k))
			}
		}
	}
	if s.CooldownMin < 0 || s.CooldownMin > 1440 {
		return errors.New("the cooldown must be 0 to 1440 minutes")
	}
	if s.RepeatHours < 1 || s.RepeatHours > 72 {
		return errors.New("the repeat interval must be 1 to 72 hours")
	}
	switch s.OutageNotice {
	case "off", "manual", "auto":
	default:
		return errors.New("tenant outage notices must be off, manual or auto")
	}
	if s.AutoMin < 1 || s.AutoMin > 1440 {
		return errors.New("the automatic outage notice delay must be 1 to 1440 minutes")
	}
	return nil
}

// Alert is an open alert (pending or firing).
type Alert struct {
	Key          string    `json:"key"` // rule:site
	Rule         string    `json:"rule"`
	SiteID       string    `json:"site_id"`
	State        string    `json:"state"`
	Since        time.Time `json:"since"`
	FiredAt      time.Time `json:"fired_at,omitempty"`
	NotifiedAt   time.Time `json:"notified_at,omitempty"`   // when the problem message was queued
	LastNotified time.Time `json:"last_notified,omitempty"` // latest problem or repeat message
	Notices      int       `json:"notices,omitempty"`       // how many problem messages went out (repeats included)
	Detail       string    `json:"detail"`
	Group        string    `json:"group,omitempty"` // storm id
}

// Event is a resolved alert in the history.
type Event struct {
	Key      string    `json:"key"`
	Rule     string    `json:"rule"`
	SiteID   string    `json:"site_id"`
	Start    time.Time `json:"start"`
	FiredAt  time.Time `json:"fired_at"`
	End      time.Time `json:"end"`
	Detail   string    `json:"detail"`
	Notified bool      `json:"notified"`
}

// Storm is a group of buildings that went offline together.
type Storm struct {
	Started  time.Time `json:"started"`
	Total    int       `json:"total"` // buildings monitored at the time
	Names    []string  `json:"names"`
	Resolved bool      `json:"resolved,omitempty"`
}

type fileData struct {
	Settings   Settings             `json:"settings"`
	Active     map[string]*Alert    `json:"active"`
	History    []Event              `json:"history"`
	LastNotify map[string]time.Time `json:"last_notify"`
	Mutes      map[string]time.Time `json:"mutes"`
	Storms     map[string]*Storm    `json:"storms"`
}

// Inputs is what the engine reads (implemented in main over the manager,
// the fleet prober and the store).
type Inputs interface {
	// Sites lists the buildings to evaluate (probed ones).
	Sites() []store.Site
	Fleet(siteID string) fleet.SiteState
	// Live is the collector's latest view and its configuration.
	Live(siteID string) (metrics.Live, metrics.Config, bool)
}

// Sink takes the messages the engine produces (notify.Outbox).
type Sink interface {
	Enqueue(m notify.Message) (notify.Message, bool)
}

// Targets are where admin messages go. A channel with no entries is skipped.
type Targets struct {
	Email    []string
	WhatsApp []string
}

// Options configures an Engine.
type Options struct {
	Path    string
	In      Inputs
	Out     Sink
	Targets func() Targets
	Log     *slog.Logger
	Now     func() time.Time
	Every   time.Duration // default 30s
	Lang    string
}

// Engine evaluates the rules.
type Engine struct {
	o   Options
	now func() time.Time

	mu    sync.Mutex
	d     fileData
	dirty bool
}

// New loads alerts.json (missing is fine; a corrupt file is logged and replaced).
func New(o Options) *Engine {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Every <= 0 {
		o.Every = 30 * time.Second
	}
	e := &Engine{o: o, now: o.Now}
	e.d = fileData{Settings: DefaultSettings()}
	if o.Path != "" {
		if raw, err := os.ReadFile(o.Path); err == nil {
			var fd fileData
			if err := json.Unmarshal(raw, &fd); err != nil {
				o.Log.Warn("alerts file unreadable; starting empty", "err", err)
			} else {
				e.d = fd
				// A newer version may know more rules: fill in defaults.
				def := DefaultSettings()
				if e.d.Settings.Rules == nil {
					e.d.Settings = def
				}
				for k, r := range def.Rules {
					if _, ok := e.d.Settings.Rules[k]; !ok {
						e.d.Settings.Rules[k] = r
					}
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			o.Log.Warn("alerts file unreadable; starting empty", "err", err)
		}
	}
	e.d.fix()
	return e
}

func (d *fileData) fix() {
	if d.Active == nil {
		d.Active = map[string]*Alert{}
	}
	if d.LastNotify == nil {
		d.LastNotify = map[string]time.Time{}
	}
	if d.Mutes == nil {
		d.Mutes = map[string]time.Time{}
	}
	if d.Storms == nil {
		d.Storms = map[string]*Storm{}
	}
}

// ---- reading ----------------------------------------------------------

// Settings returns a copy of the settings.
func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.d.Settings
	s.Rules = make(map[string]Rule, len(e.d.Settings.Rules))
	for k, v := range e.d.Settings.Rules {
		s.Rules[k] = v
	}
	return s
}

// SetSettings validates and stores new settings.
func (e *Engine) SetSettings(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	e.d.Settings, e.dirty = s, true
	e.mu.Unlock()
	return nil
}

// Active returns the open alerts, firing first, then by start.
func (e *Engine) Active() []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Alert, 0, len(e.d.Active))
	for _, a := range e.d.Active {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == Firing
		}
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// History returns up to n resolved alerts, newest first.
func (e *Engine) History(n int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, 0, len(e.d.History))
	for i := len(e.d.History) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		out = append(out, e.d.History[i])
	}
	return out
}

// OpenCount is how many alerts are firing for a building ("" = all).
func (e *Engine) OpenCount(siteID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, a := range e.d.Active {
		if a.State == Firing && (siteID == "" || a.SiteID == siteID) {
			n++
		}
	}
	return n
}

// Firing returns the open alert of a rule at a building if it is firing.
func (e *Engine) Firing(rule, siteID string) (Alert, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.d.Active[rule+":"+siteID]; a != nil && a.State == Firing {
		return *a, true
	}
	return Alert{}, false
}

// Mutes returns the active mutes (building id -> until).
func (e *Engine) Mutes() map[string]time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	out := map[string]time.Time{}
	for k, v := range e.d.Mutes {
		if v.After(now) {
			out[k] = v
		}
	}
	return out
}

// Mute silences a building's messages until the given time.
func (e *Engine) Mute(siteID string, until time.Time) {
	e.mu.Lock()
	e.d.Mutes[siteID], e.dirty = until, true
	e.mu.Unlock()
}

// Unmute lifts a mute.
func (e *Engine) Unmute(siteID string) {
	e.mu.Lock()
	delete(e.d.Mutes, siteID)
	e.dirty = true
	e.mu.Unlock()
}

func (e *Engine) mutedLocked(siteID string, now time.Time) bool {
	return e.d.Mutes[siteID].After(now)
}

// ---- conditions ---------------------------------------------------------

type reading struct {
	known, active bool
	detail        string
	since         time.Time // zero = now
}

func (e *Engine) readOffline(site store.Site) reading {
	st := e.o.In.Fleet(site.ID)
	switch st.Status {
	case fleet.Down:
		d := "router unreachable"
		if st.LastErr != "" {
			d += ": " + st.LastErr
		}
		return reading{known: true, active: true, detail: d, since: st.Since}
	case fleet.Up:
		return reading{known: true}
	}
	return reading{}
}

func recent(t, now time.Time) bool { return !t.IsZero() && now.Sub(t) <= stale }

func (e *Engine) readMetric(rule string, site store.Site, set Settings, now time.Time) reading {
	live, cfg, ok := e.o.In.Live(site.ID)
	if !ok {
		return reading{}
	}
	r := set.Rules[rule]
	switch rule {
	case RuleWANSat:
		p, has := live.Ports[cfg.WAN]
		if !has || cfg.WANCapMbps <= 0 || !recent(p.At, now) {
			return reading{}
		}
		rx, tx := p.RxBps/1e6, p.TxBps/1e6
		util := math.Max(rx, tx) / cfg.WANCapMbps * 100
		return reading{known: true, active: util >= r.Threshold,
			detail: fmt.Sprintf("WAN at %.0f%% of %.0f Mbps (rx %.0f, tx %.0f Mbps)", util, cfg.WANCapMbps, rx, tx)}
	case RuleOLTDown:
		p, has := live.Ports[cfg.OLT]
		if !has || !recent(p.At, now) {
			return reading{}
		}
		return reading{known: true, active: !p.Running, detail: fmt.Sprintf("OLT port %s is not running", cfg.OLT)}
	case RuleLoss, RuleLatency:
		var loss, rtt float64
		var n, nrtt int
		var targets []string
		for t, p := range live.Ping {
			if !recent(p.At, now) {
				continue
			}
			n++
			loss += p.LossPct
			targets = append(targets, t)
			if p.LossPct < 100 {
				rtt += p.RTTms
				nrtt++
			}
		}
		sort.Strings(targets)
		if n == 0 {
			return reading{}
		}
		if rule == RuleLoss {
			avg := loss / float64(n)
			return reading{known: true, active: avg >= r.Threshold, detail: fmt.Sprintf("%.1f%% packet loss to %s", avg, strings.Join(targets, ", "))}
		}
		if nrtt == 0 {
			return reading{}
		}
		avg := rtt / float64(nrtt)
		return reading{known: true, active: avg >= r.Threshold, detail: fmt.Sprintf("average round trip %.0f ms to %s", avg, strings.Join(targets, ", "))}
	}
	return reading{}
}

// ---- evaluation ---------------------------------------------------------

// Evaluate runs every rule once. It is called every 30 seconds and by tests.
func (e *Engine) Evaluate(now time.Time) {
	sites := e.o.In.Sites()
	e.mu.Lock()
	defer e.mu.Unlock()
	set := e.d.Settings
	byID := map[string]store.Site{}
	for _, s := range sites {
		byID[s.ID] = s
	}
	// A building that is gone (removed, paused, not joined) closes its alerts silently.
	for k, a := range e.d.Active {
		if _, ok := byID[a.SiteID]; !ok {
			e.resolveLocked(k, a, now, false)
		}
	}
	for _, s := range sites {
		offline := false
		for _, rule := range RuleOrder {
			key := rule + ":" + s.ID
			cfg := set.Rules[rule]
			if !cfg.Enabled {
				if a := e.d.Active[key]; a != nil {
					e.resolveLocked(key, a, now, false)
				}
				continue
			}
			if rule != RuleOffline && offline {
				// While a building is offline its other readings are stale:
				// pending alerts are dropped, firing ones stay untouched.
				if a := e.d.Active[key]; a != nil && a.State == Pending {
					delete(e.d.Active, key)
					e.dirty = true
				}
				continue
			}
			var rd reading
			if rule == RuleOffline {
				rd = e.readOffline(s)
				offline = rd.active
			} else {
				rd = e.readMetric(rule, s, set, now)
			}
			if !rd.known {
				continue
			}
			a := e.d.Active[key]
			switch {
			case rd.active && a == nil:
				since := rd.since
				if since.IsZero() || since.After(now) {
					since = now
				}
				a = &Alert{Key: key, Rule: rule, SiteID: s.ID, State: Pending, Since: since, Detail: rd.detail}
				e.d.Active[key] = a
				e.dirty = true
				fallthrough
			case rd.active:
				a.Detail = rd.detail
				if a.State == Pending && now.Sub(a.Since) >= time.Duration(cfg.HoldMin)*time.Minute {
					a.State, a.FiredAt = Firing, now
					e.dirty = true
					if a.Group != "" { // the storm message already covered it
						a.NotifiedAt, a.LastNotified = now, now
					}
				}
			case a != nil:
				e.resolveLocked(key, a, now, true)
			}
		}
	}
	e.notifyLocked(sites, set, now)
	e.dirty = true
}

// resolveLocked closes an alert. A recovery message goes out only if the
// problem message was sent, and (for a storm) once when the whole group is back.
func (e *Engine) resolveLocked(key string, a *Alert, now time.Time, announce bool) {
	delete(e.d.Active, key)
	e.dirty = true
	if a.State != Firing {
		return
	}
	e.d.History = append(e.d.History, Event{Key: key, Rule: a.Rule, SiteID: a.SiteID, Start: a.Since, FiredAt: a.FiredAt, End: now, Detail: a.Detail, Notified: !a.NotifiedAt.IsZero()})
	if n := len(e.d.History); n > historyCap {
		e.d.History = append([]Event(nil), e.d.History[n-historyCap:]...)
	}
	if !announce || a.NotifiedAt.IsZero() {
		return
	}
	if a.Group != "" {
		for _, o := range e.d.Active {
			if o.Group == a.Group {
				return
			}
		}
		if st := e.d.Storms[a.Group]; st != nil && !st.Resolved {
			st.Resolved = true
			e.sendLocked("RECOVERED", "Many buildings offline", fmt.Sprintf("%d of %d buildings", len(st.Names), st.Total),
				"all buildings are reachable again: "+strings.Join(st.Names, ", "), "alertok:"+a.Group, now, "")
		}
		return
	}
	e.sendLocked("RECOVERED", Label(a.Rule), e.siteName(a.SiteID), a.Detail, "alertok:"+key+":"+fmt.Sprint(a.FiredAt.Unix()), now, a.SiteID)
}

func (e *Engine) siteName(id string) string {
	for _, s := range e.o.In.Sites() {
		if s.ID == id {
			return s.Name
		}
	}
	return id
}

func (e *Engine) cooldownOK(key string, set Settings, now time.Time) bool {
	last, had := e.d.LastNotify[key]
	return !had || now.Sub(last) >= time.Duration(set.CooldownMin)*time.Minute
}

// notifyLocked sends the problem messages that are due.
func (e *Engine) notifyLocked(sites []store.Site, set Settings, now time.Time) {
	names := map[string]string{}
	for _, s := range sites {
		names[s.ID] = s.Name
	}
	// Alerts that fired and have not been announced, in start order.
	var cands []*Alert
	for _, a := range e.d.Active {
		if a.State == Firing && a.NotifiedAt.IsZero() && a.Group == "" && !e.mutedLocked(a.SiteID, now) && e.cooldownOK(a.Key, set, now) {
			cands = append(cands, a)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].Since.Equal(cands[j].Since) {
			return cands[i].Since.Before(cands[j].Since)
		}
		return cands[i].Key < cands[j].Key
	})
	for len(cands) > 0 {
		c := cands[0]
		if c.Rule == RuleOffline {
			// Storm guard: more than half of the buildings (and at least 3)
			// going offline within 2 minutes of each other is one alert.
			var group []*Alert
			for _, o := range e.d.Active {
				if o.Rule == RuleOffline && o.Group == "" && o.NotifiedAt.IsZero() && !e.mutedLocked(o.SiteID, now) {
					if d := o.Since.Sub(c.Since); d >= -stormWindow && d <= stormWindow {
						group = append(group, o)
					}
				}
			}
			if len(group) >= 3 && len(group)*2 > len(sites) {
				id := fmt.Sprintf("storm:%d", c.Since.Unix())
				st := &Storm{Started: c.Since, Total: len(sites)}
				in := map[*Alert]bool{}
				for _, g := range group {
					g.Group = id
					if g.State == Firing {
						g.NotifiedAt, g.LastNotified = now, now
					}
					st.Names = append(st.Names, names[g.SiteID])
					in[g] = true
				}
				sort.Strings(st.Names)
				e.d.Storms[id] = st
				e.sendLocked("PROBLEM", "Many buildings offline", fmt.Sprintf("%d of %d buildings", len(group), len(sites)),
					"offline together: "+strings.Join(st.Names, ", "), "alert:"+id, now, "")
				var rest []*Alert
				for _, o := range cands {
					if !in[o] {
						rest = append(rest, o)
					}
				}
				cands = rest
				continue
			}
		}
		cands = cands[1:]
		c.NotifiedAt, c.LastNotified = now, now
		c.Notices++
		e.d.LastNotify[c.Key] = now
		e.sendLocked("PROBLEM", Label(c.Rule), names[c.SiteID], c.Detail, fmt.Sprintf("alert:%s:%d", c.Key, c.FiredAt.Unix()), now, c.SiteID)
	}
	// Repeats of problems that are still firing.
	if set.RepeatEnabled {
		for _, a := range e.d.Active {
			if a.State == Firing && !a.NotifiedAt.IsZero() && a.Group == "" && !e.mutedLocked(a.SiteID, now) &&
				now.Sub(a.LastNotified) >= time.Duration(set.RepeatHours)*time.Hour {
				a.LastNotified = now
				a.Notices++
				e.sendLocked("PROBLEM", Label(a.Rule), names[a.SiteID], "still firing: "+a.Detail,
					fmt.Sprintf("alert:%s:%d:rep%d", a.Key, a.FiredAt.Unix(), a.Notices), now, a.SiteID)
			}
		}
	}
}

// timeText formats a time for messages (EAT).
func timeText(t time.Time) string { return t.In(metrics.EAT).Format("2 Jan 15:04") + " EAT" }

// Compose builds the messages for one alert event, one per enabled channel and
// recipient. dedupe == "" means no dedupe key (test messages).
func (e *Engine) compose(status, rule, building, detail, dedupe string, at time.Time, siteID string, set Settings) []notify.Message {
	var tg Targets
	if e.o.Targets != nil {
		tg = e.o.Targets()
	}
	head := fmt.Sprintf("%s: %s at %s", status, rule, building)
	text := head + "\n" + detail + "\nTime: " + timeText(at)
	var out []notify.Message
	dk := func(ch, to string) string {
		if dedupe == "" {
			return ""
		}
		return dedupe + ":" + ch + ":" + to
	}
	base := notify.Message{Kind: notify.KindAdminAlert, SiteID: siteID, Priority: 10}
	if set.Email {
		for _, to := range tg.Email {
			m := base
			m.Channel, m.To, m.Subject, m.Text, m.DedupeKey = notify.ChanEmail, to, "[Fast Tech Net] "+head, text, dk("email", to)
			out = append(out, m)
		}
	}
	if set.WhatsApp {
		for _, to := range tg.WhatsApp {
			m := base
			m.Channel, m.To, m.Template, m.Lang, m.DedupeKey = notify.ChanWhatsApp, to, notify.TplAdminAlert, e.o.Lang, dk("whatsapp", to)
			m.Params = []string{status, rule, building, detail, timeText(at)}
			m.Text = text
			out = append(out, m)
		}
	}
	return out
}

func (e *Engine) sendLocked(status, rule, building, detail, dedupe string, at time.Time, siteID string) {
	if e.o.Out == nil {
		return
	}
	for _, m := range e.compose(status, rule, building, detail, dedupe, at, siteID, e.d.Settings) {
		e.o.Out.Enqueue(m)
	}
}

// TestMessage is the message the Test button sends on one channel (never deduped).
func (e *Engine) TestMessage(channel string) (notify.Message, bool) {
	e.mu.Lock()
	set := e.d.Settings
	e.mu.Unlock()
	set.Email, set.WhatsApp = true, true
	for _, m := range e.compose("PROBLEM", "Test alert", "Test building", "this is a test from the Alerts page", "", e.now(), "", set) {
		if m.Channel == channel {
			m.Kind = notify.KindTest
			return m, true
		}
	}
	return notify.Message{}, false
}

// ---- persistence and loop -------------------------------------------------

// Save writes alerts.json when something changed.
func (e *Engine) Save() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.o.Path == "" || !e.dirty {
		return nil
	}
	now := e.now()
	for k, v := range e.d.Mutes {
		if !v.After(now) {
			delete(e.d.Mutes, k)
		}
	}
	for id, st := range e.d.Storms { // forget finished storms after a week
		if st.Resolved && now.Sub(st.Started) > 7*24*time.Hour {
			delete(e.d.Storms, id)
		}
	}
	for k, t := range e.d.LastNotify {
		if now.Sub(t) > 7*24*time.Hour {
			delete(e.d.LastNotify, k)
		}
	}
	raw, err := json.Marshal(&e.d)
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(e.o.Path, raw); err != nil {
		return err
	}
	e.dirty = false
	return nil
}

// Run evaluates every Every and saves after each pass and on the way out.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.o.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := e.Save(); err != nil {
				e.o.Log.Warn("alerts final save failed", "err", err)
			}
			return
		case <-t.C:
			e.Evaluate(e.now())
			if err := e.Save(); err != nil {
				e.o.Log.Warn("alerts save failed", "err", err)
			}
		}
	}
}
