package settings

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ispbill/internal/config"
	"ispbill/internal/secretbox"
	"ispbill/internal/store"
)

// Sources of a setting's effective value.
const (
	SrcDatabase = "database"
	SrcEnv      = "env"
	SrcDefault  = "default"
)

// ErrNoDataKey is returned when a secret is saved without DATA_KEY.
var ErrNoDataKey = errors.New("secrets cannot be saved yet: set DATA_KEY in /etc/ftnbilling/ftnbilling.env (openssl rand -base64 32), restart, then try again")

type snapshot struct {
	cfg    config.Config
	src    map[string]string
	bad    map[string]string // key -> why its stored value was ignored
	rows   map[string]store.Setting
	hasPwd bool
}

// Manager lays the settings rows over the environment configuration.
type Manager struct {
	st     *store.Store
	box    *secretbox.Box // nil without DATA_KEY
	env    config.Config
	getenv func(string) string
	now    func() time.Time
	grace  time.Duration // how long a replaced callback token keeps working
	// note, when set, receives each audit message instead of an event row in
	// the database (see NoteAuditTo).
	note func(string)

	snap atomic.Pointer[snapshot]

	mu       sync.Mutex // serialises Save, Revert and the password calls
	appliers map[string][]func(config.Config)
}

// New builds the manager and its first snapshot. dataKey may be nil: plain
// settings then work, secrets are refused. getenv tells which keys the
// environment really sets (to tell "env" from "default").
func New(st *store.Store, dataKey []byte, env config.Config, getenv func(string) string) (*Manager, error) {
	m := &Manager{st: st, env: env, getenv: getenv, now: time.Now, appliers: map[string][]func(config.Config){}, grace: CallbackGrace}
	if getenv == nil {
		m.getenv = func(string) string { return "" }
	}
	if len(dataKey) > 0 {
		b, err := secretbox.NewSettings(dataKey)
		if err != nil {
			return nil, err
		}
		m.box = b
	}
	m.rebuild()
	return m, nil
}

// SetClock overrides the clock (tests).
func (m *Manager) SetClock(f func() time.Time) { m.now = f }

// HasDataKey reports whether secrets can be stored.
func (m *Manager) HasDataKey() bool { return m.box != nil }

// OnApply registers fn to run, with the new effective config, after a save or
// revert in section. Appliers must be quick and must not call back into Save.
func (m *Manager) OnApply(section string, fn func(config.Config)) {
	m.mu.Lock()
	m.appliers[section] = append(m.appliers[section], fn)
	m.mu.Unlock()
}

// ApplyAll runs every applier once (at start-up).
func (m *Manager) ApplyAll() {
	m.mu.Lock()
	var fns []func(config.Config)
	for _, f := range m.appliers {
		fns = append(fns, f...)
	}
	m.mu.Unlock()
	cfg := m.Effective()
	for _, f := range fns {
		f(cfg)
	}
}

// Effective is the configuration in force now: the environment with database
// rows laid over it. It holds secrets; never render or log it.
func (m *Manager) Effective() config.Config { return m.snap.Load().cfg }

// Source tells where key's effective value comes from.
func (m *Manager) Source(key string) string {
	if s, ok := m.snap.Load().src[key]; ok {
		return s
	}
	return SrcDefault
}

// Problems lists stored values that were ignored (for a banner).
func (m *Manager) Problems() []string {
	var out []string
	for k, why := range m.snap.Load().bad {
		out = append(out, fmt.Sprintf("%s: %s", k, why))
	}
	sort.Strings(out)
	return out
}

// rebuild recomputes the snapshot from the store.
func (m *Manager) rebuild() {
	rows := map[string]store.Setting{}
	m.st.View(func(d *store.Data) {
		for k, v := range d.Settings {
			rows[k] = v
		}
	})
	cfg := m.env
	// Slices in the env config are shared with the snapshot: copy before laying rows over them.
	cfg.WAAdminNumbers = append([]string(nil), cfg.WAAdminNumbers...)
	cfg.AlertEmailTo = append([]string(nil), cfg.AlertEmailTo...)
	src := map[string]string{}
	bad := map[string]string{}
	for _, d := range Registry {
		src[d.Key] = SrcDefault
		if d.Env != "" && strings.TrimSpace(m.getenv(d.Env)) != "" {
			src[d.Key] = SrcEnv
		}
		row, ok := rows[d.Key]
		if !ok {
			continue
		}
		v, err := m.open(d, row)
		if err != nil {
			bad[d.Key] = err.Error()
			continue
		}
		d.Set(&cfg, v)
		src[d.Key] = SrcDatabase
	}
	// The token a rotation replaced keeps working for a while (see graceRows).
	if row, ok := rows[KeyCallbackPrev]; ok && row.Enc && m.box != nil {
		if tok, err := m.box.OpenAAD(row.Value, []byte(KeyCallbackPrev)); err == nil {
			if until, err := time.Parse(time.RFC3339, rows[KeyCallbackPrevUntil].Value); err == nil {
				cfg.MpesaCallbackTokenPrev, cfg.MpesaCallbackTokenPrevUntil = tok, until
			}
		}
	}
	m.snap.Store(&snapshot{cfg: cfg, src: src, bad: bad, rows: rows, hasPwd: rows[KeyAdminHash].Value != ""})
}

// open returns a row's plain value; a secret that cannot be decrypted is an
// error (the caller falls back to the environment value).
func (m *Manager) open(d Def, row store.Setting) (string, error) {
	if !row.Enc {
		if d.Secret {
			return "", errors.New("a secret is stored unencrypted; ignored")
		}
		return row.Value, nil
	}
	if m.box == nil {
		return "", errors.New("stored encrypted but DATA_KEY is not set; using the environment value")
	}
	v, err := m.box.OpenAAD(row.Value, []byte(d.Key))
	if err != nil {
		return "", fmt.Errorf("%v; using the environment value", err)
	}
	return v, nil
}

// Field is one setting as a page shows it. A secret's value is never included.
type Field struct {
	Def
	Source  string // database, env or default
	Value   string // plain value (empty for secrets)
	Set     bool   // a secret has a value
	Masked  string // bullets plus the last four characters of a long secret
	EnvName string
	EnvSet  bool // the environment sets this key (so "revert" has something to go back to)
	Problem string
}

// Fields returns a section's settings for display.
func (m *Manager) Fields(section string) []Field {
	sn := m.snap.Load()
	var out []Field
	for _, d := range SectionDefs(section) {
		f := Field{Def: d, Source: sn.src[d.Key], EnvName: d.Env, Problem: sn.bad[d.Key],
			EnvSet: d.Env != "" && strings.TrimSpace(m.getenv(d.Env)) != ""}
		v := d.Get(sn.cfg)
		if d.Secret {
			f.Set = v != ""
			if f.Set {
				f.Masked = "••••"
				if len(v) >= 12 {
					f.Masked += v[len(v)-4:]
				}
			}
		} else {
			f.Value = v
		}
		out = append(out, f)
	}
	return out
}

// Change describes one key Save changed (names only, never values).
type Change struct{ Key, What string }

// Save validates values for a section and stores what differs from the
// effective configuration. For a secret, "" keeps the stored value; for any
// other field a value equal to the current one stores nothing, so an env value
// stays an env value until it is really changed. Rows and one audit event are
// written in a single update, then the section's appliers run.
func (m *Manager) Save(section string, values map[string]string, actor string) ([]Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.snap.Load()
	type write struct {
		d    Def
		val  string
		what string
	}
	var writes []write
	for _, d := range SectionDefs(section) {
		raw, present := values[d.Key]
		if !present {
			continue
		}
		if d.Secret {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			v, err := d.Norm(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.Label, err)
			}
			if m.box == nil {
				return nil, ErrNoDataKey
			}
			what := "set"
			if d.Get(cur.cfg) != "" {
				what = "replaced"
			}
			if v == d.Get(cur.cfg) && cur.src[d.Key] == SrcDatabase {
				continue
			}
			writes = append(writes, write{d, v, what})
			continue
		}
		if strings.TrimSpace(raw) == "" && (d.Kind == KNumber || d.Kind == KSelect) {
			continue // a blank number or choice means "leave it"
		}
		v, err := d.Norm(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Label, err)
		}
		if v == d.Get(cur.cfg) {
			continue
		}
		what := "changed"
		if v == "" {
			what = "cleared"
		}
		writes = append(writes, write{d, v, what})
	}
	// A callback token is made as soon as M-Pesa has credentials and none exists.
	if section == SecMpesa {
		after := cur.cfg
		for _, w := range writes {
			w.d.Set(&after, w.val)
		}
		if after.MpesaCallbackToken == "" && after.MpesaKey != "" && after.MpesaSecret != "" && after.MpesaShortcode != "" {
			if m.box == nil {
				return nil, ErrNoDataKey
			}
			tok, err := newToken()
			if err != nil {
				return nil, err
			}
			d, _ := Lookup("mpesa.callback_token")
			writes = append(writes, write{*d, tok, "generated"})
		}
	}
	if len(writes) == 0 {
		return nil, nil
	}
	now := m.now()
	rows := make([]store.Setting, 0, len(writes)+2)
	changes := make([]Change, 0, len(writes))
	for _, w := range writes {
		if w.d.Key == "mpesa.callback_token" && w.what != "generated" {
			// Replacing a working token: the old one stays valid for a while.
			gr, err := m.graceRows(cur.cfg.MpesaCallbackToken, w.val, now)
			if err != nil {
				return nil, err
			}
			rows = append(rows, gr...)
		}
		row := store.Setting{Key: w.d.Key, Value: w.val, UpdatedAt: now}
		if w.d.Secret {
			enc, err := m.box.SealAAD(w.val, []byte(w.d.Key))
			if err != nil {
				return nil, err
			}
			row.Value, row.Enc = enc, true
		}
		rows = append(rows, row)
		changes = append(changes, Change{w.d.Key, w.what})
	}
	if err := m.commit(rows, nil, section, changes, actor, now); err != nil {
		return nil, err
	}
	m.applyLocked(section)
	return changes, nil
}

// commit writes rows, deletes keys and records one audit event.
func (m *Manager) commit(rows []store.Setting, del []string, section string, changes []Change, actor string, now time.Time) error {
	err := m.st.Update(func(d *store.Data) error {
		for _, r := range rows {
			d.Settings[r.Key] = r
		}
		for _, k := range del {
			delete(d.Settings, k)
		}
		if msg := auditMsg(section, changes, actor); m.note != nil {
			m.note(msg)
		} else {
			d.AddEvent(store.Event{Time: now, Kind: "audit", Msg: msg})
		}
		return nil
	})
	if err != nil {
		return err
	}
	m.rebuild()
	return nil
}

func auditMsg(section string, changes []Change, actor string) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.Key+" "+c.What)
	}
	msg := "settings"
	if section != "" {
		msg += " (" + section + ")"
	}
	msg += ": " + strings.Join(parts, ", ")
	if actor != "" {
		msg += " from " + actor
	}
	return msg
}

// Revert deletes a key's row so the environment value (or the default) applies
// again.
func (m *Manager) Revert(key, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	cur := m.snap.Load()
	if _, has := cur.rows[key]; !has {
		return nil
	}
	// A revert must not leave M-Pesa switched on without a callback token
	// (callbacks would all be refused, and the config would not validate).
	after := cur.cfg
	d.Set(&after, d.Get(m.env))
	if d.Section == SecMpesa && after.MpesaCallbackToken == "" && after.MpesaKey != "" && after.MpesaSecret != "" && after.MpesaShortcode != "" {
		return errors.New("M-Pesa is configured and needs a callback token, and the environment file has none: set or rotate the token instead of going back")
	}
	now := m.now()
	var rows []store.Setting
	if key == "mpesa.callback_token" {
		gr, err := m.graceRows(cur.cfg.MpesaCallbackToken, after.MpesaCallbackToken, now)
		if err != nil {
			return err
		}
		rows = gr
	}
	if err := m.commit(rows, []string{key}, d.Section, []Change{{key, "reverted to " + SrcEnv}}, actor, now); err != nil {
		return err
	}
	m.applyLocked(d.Section)
	return nil
}

// RotateCallbackToken replaces the M-Pesa callback token.
func (m *Manager) RotateCallbackToken(actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.box == nil {
		return ErrNoDataKey
	}
	tok, err := newToken()
	if err != nil {
		return err
	}
	now := m.now()
	enc, err := m.box.SealAAD(tok, []byte("mpesa.callback_token"))
	if err != nil {
		return err
	}
	row := store.Setting{Key: "mpesa.callback_token", Value: enc, Enc: true, UpdatedAt: now}
	gr, err := m.graceRows(m.snap.Load().cfg.MpesaCallbackToken, tok, now)
	if err != nil {
		return err
	}
	if err := m.commit(append(gr, row), nil, SecMpesa, []Change{{"mpesa.callback_token", "rotated"}}, actor, now); err != nil {
		return err
	}
	m.applyLocked(SecMpesa)
	return nil
}

// Keys of the replaced callback token, kept for a grace period. They are not
// registry settings: the pages never show or edit them.
const (
	KeyCallbackPrev      = "mpesa.callback_token_prev"
	KeyCallbackPrevUntil = "mpesa.callback_token_prev_until"
)

// CallbackGrace is how long a replaced callback token keeps working. Safaricom
// keeps calling the registered URLs, and in production they cannot simply be
// registered again, so cutting the old token off at once would lose payments.
const CallbackGrace = 7 * 24 * time.Hour

// graceRows keeps old as the previous callback token until now+grace, when it
// is being replaced by a different value. Without an old token, or without a
// DATA_KEY to seal it, there is nothing to keep.
func (m *Manager) graceRows(old, replacement string, now time.Time) ([]store.Setting, error) {
	if old == "" || old == replacement || m.box == nil || m.grace <= 0 {
		return nil, nil
	}
	enc, err := m.box.SealAAD(old, []byte(KeyCallbackPrev))
	if err != nil {
		return nil, err
	}
	return []store.Setting{
		{Key: KeyCallbackPrev, Value: enc, Enc: true, UpdatedAt: now},
		{Key: KeyCallbackPrevUntil, Value: now.Add(m.grace).UTC().Format(time.RFC3339), UpdatedAt: now},
	}, nil
}

// SetCallbackGrace changes the grace period (tests).
func (m *Manager) SetCallbackGrace(d time.Duration) { m.grace = d }

// PreviousCallbackToken reports whether a replaced token still works, and
// until when. It never returns the token.
func (m *Manager) PreviousCallbackToken() (until time.Time, ok bool) {
	c := m.snap.Load().cfg
	if c.MpesaCallbackTokenPrev == "" || !m.now().Before(c.MpesaCallbackTokenPrevUntil) {
		return time.Time{}, false
	}
	return c.MpesaCallbackTokenPrevUntil, true
}

func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// applyLocked runs a section's appliers with the fresh snapshot.
func (m *Manager) applyLocked(section string) {
	cfg := m.snap.Load().cfg
	for _, f := range m.appliers[section] {
		f(cfg)
	}
}

// ---- plain markers (no audit event) ----------------------------------------

// KeyWebhookVerified is the time Meta's GET handshake last succeeded.
const KeyWebhookVerified = "wa.webhook_verified_at"

// Marker returns a plain internal value (like the webhook handshake time).
func (m *Manager) Marker(key string) string { return m.snap.Load().rows[key].Value }

// SetMarker stores a plain internal value without an audit event.
func (m *Manager) SetMarker(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.st.Update(func(d *store.Data) error {
		if d.Settings[key].Value == value {
			return store.ErrNoChange
		}
		d.Settings[key] = store.Setting{Key: key, Value: value, UpdatedAt: m.now()}
		return nil
	})
	if err == nil {
		m.rebuild()
	}
	return err
}

// ---- admin password --------------------------------------------------------

// HasAdminPassword reports whether a password hash is stored.
func (m *Manager) HasAdminPassword() bool { return m.snap.Load().hasPwd }

// NoteAuditTo sends audit messages to fn instead of writing event rows. The
// command line tools use it while the service runs on the same Postgres
// database: event numbers (Event.Seq) come from the service's own counter, so
// a second writer adding events would collide with it. The message goes to
// stderr (the journal when run by the wrapper) instead.
func (m *Manager) NoteAuditTo(fn func(string)) { m.note = fn }

// AdminHash returns the stored hash ("" = none; the environment ADMIN_PASS
// applies).
func (m *Manager) AdminHash() string { return m.snap.Load().rows[KeyAdminHash].Value }

// SetAdminPassword stores a new password hash. The caller has already
// checked the current password and CheckNewPassword.
func (m *Manager) SetAdminPassword(pw, actor string) error {
	h, err := HashPassword(pw)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	return m.commit([]store.Setting{{Key: KeyAdminHash, Value: h, UpdatedAt: now}}, nil, "",
		[]Change{{KeyAdminHash, "changed"}}, actor, now)
}

// ClearAdminPassword removes the hash, so ADMIN_PASS from the environment
// works again.
func (m *Manager) ClearAdminPassword(actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.snap.Load().hasPwd {
		return nil
	}
	return m.commit(nil, []string{KeyAdminHash}, "", []Change{{KeyAdminHash, "removed"}}, actor, m.now())
}
