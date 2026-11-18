package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"ispbill/internal/store"
)

// EAT is East Africa Time (quiet hours and the daily WhatsApp cap are in EAT).
var EAT = time.FixedZone("EAT", 3*3600)

const (
	maxAttempts   = 8
	baseBackoff   = 30 * time.Second
	maxBackoff    = time.Hour
	dedupeKeep    = 90 * 24 * time.Hour
	finishedKeep  = 60 * 24 * time.Hour
	maxFinished   = 5000
	quietFromHour = 21 // tenant reminders go out from 07:00 until 21:00 EAT
	quietToHour   = 7
)

// Options configures an Outbox.
type Options struct {
	Path    string
	Senders map[string]Sender // channel -> sender; a channel without one is left alone
	Log     *slog.Logger
	Now     func() time.Time
	// Intervals is the minimum gap between two sends on a channel.
	Intervals map[string]time.Duration
	// WADailyLimit caps tenant WhatsApp messages per EAT day (0 = no cap).
	WADailyLimit int
	// Validate is called just before a tenant message is sent. Returning
	// ok=false marks it skipped (it no longer applies).
	Validate func(Message) (ok bool, reason string)
}

// DefaultIntervals are the per-channel rate limits: WhatsApp 5 per second,
// Telegram 1 per second, SMTP 1 every 2 seconds.
func DefaultIntervals() map[string]time.Duration {
	return map[string]time.Duration{ChanWhatsApp: 200 * time.Millisecond, ChanTelegram: time.Second, ChanEmail: 2 * time.Second}
}

// Pause records why a channel stopped (bad credentials).
type Pause struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type fileData struct {
	Messages []Message            `json:"messages"`
	Keys     map[string]time.Time `json:"keys"` // dedupe key -> first enqueued
	Paused   map[string]Pause     `json:"paused,omitempty"`
}

// Outbox is the persistent queue.
type Outbox struct {
	o   Options
	now func() time.Time

	mu    sync.Mutex
	msgs  []Message
	keys  map[string]time.Time
	pause map[string]Pause
	last  map[string]time.Time // channel -> last send attempt
	dirty bool
	wake  chan struct{}
}

// Open loads the outbox file (a missing file is an empty outbox).
func Open(o Options) (*Outbox, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Intervals == nil {
		o.Intervals = DefaultIntervals()
	}
	b := &Outbox{o: o, now: o.Now, keys: map[string]time.Time{}, pause: map[string]Pause{}, last: map[string]time.Time{}, wake: make(chan struct{}, 1)}
	if o.Path == "" {
		return b, nil
	}
	raw, err := os.ReadFile(o.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var fd fileData
		if err := json.Unmarshal(raw, &fd); err != nil {
			return nil, fmt.Errorf("corrupt %s: %w", o.Path, err)
		}
		b.msgs = fd.Messages
		if fd.Keys != nil {
			b.keys = fd.Keys
		}
		if fd.Paused != nil {
			b.pause = fd.Paused
		}
	}
	return b, nil
}

// SetValidate installs the tenant Validate hook (after construction, because
// the hook usually needs the store and the outbox itself).
func (b *Outbox) SetValidate(f func(Message) (bool, string)) {
	b.mu.Lock()
	b.o.Validate = f
	b.mu.Unlock()
}

// SetSender installs or replaces the sender of a channel.
func (b *Outbox) SetSender(channel string, s Sender) {
	b.mu.Lock()
	if b.o.Senders == nil {
		b.o.Senders = map[string]Sender{}
	}
	b.o.Senders[channel] = s
	b.mu.Unlock()
}

// HasSender reports whether a channel is configured.
func (b *Outbox) HasSender(channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.o.Senders[channel] != nil
}

func newID() string {
	x := make([]byte, 8)
	if _, err := rand.Read(x); err != nil {
		panic(err)
	}
	return hex.EncodeToString(x)
}

// QuietNow reports whether a time is in the tenant quiet hours (21:00 to
// 07:00 EAT).
func QuietNow(t time.Time) bool {
	h := t.In(EAT).Hour()
	return h >= quietFromHour || h < quietToHour
}

// NextOpen returns the next 07:00 EAT at or after t when t is in quiet hours,
// else t itself.
func NextOpen(t time.Time) time.Time {
	if !QuietNow(t) {
		return t
	}
	e := t.In(EAT)
	open := time.Date(e.Year(), e.Month(), e.Day(), quietToHour, 0, 0, 0, EAT)
	if !open.After(e) {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

// Enqueue adds a message. It does nothing (created=false) when the dedupe key
// was already used in the last 90 days, whether or not that message was sent.
func (b *Outbox) Enqueue(m Message) (Message, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if m.DedupeKey != "" {
		if _, dup := b.keys[m.DedupeKey]; dup {
			return m, false
		}
		b.keys[m.DedupeKey] = now
	}
	m.ID, m.Status, m.CreatedAt = newID(), StatusQueued, now
	if m.Lang == "" && m.Template != "" {
		m.Lang = "en"
	}
	b.msgs = append(b.msgs, m)
	b.dirty = true
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return m, true
}

// backoff is the wait before attempt n+1 after n failures: 30s doubling to 1h.
func backoff(attempts int) time.Duration {
	d := baseBackoff
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

func eatDayStart(t time.Time) time.Time {
	e := t.In(EAT)
	return time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, EAT)
}

// waSentToday counts tenant WhatsApp messages sent since EAT midnight (mu held).
func (b *Outbox) waSentToday(now time.Time) int {
	from, n := eatDayStart(now), 0
	for _, m := range b.msgs {
		if m.Channel == ChanWhatsApp && m.Tenant() && m.Status == StatusSent && !m.SentAt.Before(from) {
			n++
		}
	}
	return n
}

// WASentToday is waSentToday for the admin page.
func (b *Outbox) WASentToday() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waSentToday(b.now())
}

// next picks the due message of a channel (mu held): highest priority, then oldest.
func (b *Outbox) next(ch string, now time.Time) int {
	best := -1
	for i, m := range b.msgs {
		if m.Channel != ch || m.Status != StatusQueued || m.NextTry.After(now) || m.NotBefore.After(now) {
			continue
		}
		if best < 0 || m.Priority > b.msgs[best].Priority ||
			(m.Priority == b.msgs[best].Priority && m.CreatedAt.Before(b.msgs[best].CreatedAt)) {
			best = i
		}
	}
	return best
}

// Step sends at most one due message of the channel and reports whether it
// attempted a send. It honours the channel's rate limit, its pause, quiet
// hours, the WhatsApp daily cap and the Validate hook.
func (b *Outbox) Step(ctx context.Context, ch string) bool {
	b.mu.Lock()
	now := b.now()
	snd := b.o.Senders[ch]
	if snd == nil {
		b.mu.Unlock()
		return false
	}
	if _, paused := b.pause[ch]; paused {
		b.mu.Unlock()
		return false
	}
	if gap := b.o.Intervals[ch]; gap > 0 && now.Sub(b.last[ch]) < gap {
		b.mu.Unlock()
		return false
	}
	for {
		i := b.next(ch, now)
		if i < 0 {
			b.mu.Unlock()
			return false
		}
		m := b.msgs[i]
		if m.Tenant() {
			// Reminders are not sent at night.
			if (m.Kind == KindExpiryReminder || m.Kind == KindPlanExpired) && QuietNow(now) {
				b.msgs[i].NotBefore, b.dirty = NextOpen(now), true
				continue
			}
			if ch == ChanWhatsApp && b.o.WADailyLimit > 0 && b.waSentToday(now) >= b.o.WADailyLimit {
				// Over today's cap: every tenant WhatsApp waits for tomorrow.
				tomorrow := eatDayStart(now).AddDate(0, 0, 1)
				for j := range b.msgs {
					if b.msgs[j].Channel == ch && b.msgs[j].Tenant() && b.msgs[j].Status == StatusQueued && b.msgs[j].NotBefore.Before(tomorrow) {
						b.msgs[j].NotBefore = tomorrow
					}
				}
				b.dirty = true
				continue
			}
			if v := b.o.Validate; v != nil {
				b.mu.Unlock()
				ok, why := v(m)
				b.mu.Lock()
				if !ok {
					b.msgs[i].Status, b.msgs[i].LastErr, b.dirty = StatusSkipped, why, true
					continue
				}
			}
		}
		b.last[ch] = now
		b.mu.Unlock()
		pid, err := snd.Send(ctx, m)
		b.finish(m.ID, pid, err)
		return true
	}
}

// finish records the outcome of a send.
func (b *Outbox) finish(id, providerID string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for i := range b.msgs {
		m := &b.msgs[i]
		if m.ID != id {
			continue
		}
		b.dirty = true
		if m.Status != StatusQueued { // cancelled while sending: keep that decision
			return
		}
		m.Attempts++
		switch {
		case err == nil:
			m.Status, m.SentAt, m.ProviderID, m.LastErr, m.Delivery = StatusSent, now, providerID, "", "sent"
		case isAuth(err):
			m.LastErr = err.Error()
			m.NextTry = now // retried as soon as the channel is resumed
			b.pause[m.Channel] = Pause{Reason: err.Error(), At: now}
			b.o.Log.Warn("notification channel paused: credentials rejected", "channel", m.Channel, "err", err)
		case isPermanent(err):
			m.Status, m.LastErr = StatusFailed, err.Error()
		case m.Attempts >= maxAttempts:
			m.Status, m.LastErr = StatusFailed, fmt.Sprintf("gave up after %d attempts: %v", m.Attempts, err)
		default:
			m.LastErr, m.NextTry = err.Error(), now.Add(backoff(m.Attempts))
		}
		return
	}
}

// Direct sends one message right now, outside the queue (the Test buttons).
// An auth error pauses the channel; success lifts a pause.
func (b *Outbox) Direct(ctx context.Context, m Message) error {
	b.mu.Lock()
	snd := b.o.Senders[m.Channel]
	b.mu.Unlock()
	if snd == nil {
		return fmt.Errorf("%s is not configured", m.Channel)
	}
	_, err := snd.Send(ctx, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case err == nil:
		if _, p := b.pause[m.Channel]; p {
			delete(b.pause, m.Channel)
			b.dirty = true
		}
	case isAuth(err):
		b.pause[m.Channel] = Pause{Reason: err.Error(), At: b.now()}
		b.dirty = true
	}
	return err
}

// Paused returns the channels paused by an authentication error.
func (b *Outbox) Paused() map[string]Pause {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]Pause, len(b.pause))
	for k, v := range b.pause {
		out[k] = v
	}
	return out
}

// Resume lifts a channel's pause.
func (b *Outbox) Resume(ch string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.pause[ch]; ok {
		delete(b.pause, ch)
		b.dirty = true
	}
}

// Retry puts a failed, skipped or cancelled message back in the queue.
func (b *Outbox) Retry(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.msgs {
		m := &b.msgs[i]
		if m.ID == id && (m.Status == StatusFailed || m.Status == StatusSkipped || m.Status == StatusCancelled) {
			m.Status, m.Attempts, m.NextTry, m.NotBefore, m.LastErr = StatusQueued, 0, time.Time{}, time.Time{}, ""
			b.dirty = true
			return true
		}
	}
	return false
}

// Cancel stops a queued message.
func (b *Outbox) Cancel(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.msgs {
		if b.msgs[i].ID == id && b.msgs[i].Status == StatusQueued {
			b.msgs[i].Status, b.msgs[i].LastErr, b.dirty = StatusCancelled, "cancelled by the admin", true
			return true
		}
	}
	return false
}

// SetDelivery records a delivery status reported by the WhatsApp webhook.
func (b *Outbox) SetDelivery(providerID, status, detail string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.msgs {
		if b.msgs[i].ProviderID == providerID && providerID != "" {
			rank := map[string]int{"sent": 1, "delivered": 2, "read": 3}
			if status == "failed" || rank[status] >= rank[b.msgs[i].Delivery] {
				b.msgs[i].Delivery = status
			}
			if status == "failed" && detail != "" {
				b.msgs[i].LastErr = detail
			}
			b.dirty = true
			return true
		}
	}
	return false
}

// Filter selects messages for the admin page (empty field = any).
type Filter struct {
	Status, Channel, SiteID string
	Limit                   int
}

// List returns messages, newest first.
func (b *Outbox) List(f Filter) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Message
	for _, m := range b.msgs {
		if (f.Status != "" && m.Status != f.Status) || (f.Channel != "" && m.Channel != f.Channel) || (f.SiteID != "" && m.SiteID != f.SiteID) {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// Find returns the messages for which match is true (newest first).
func (b *Outbox) Find(match func(Message) bool) []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Message
	for _, m := range b.msgs {
		if match(m) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Counts returns how many messages have each status.
func (b *Outbox) Counts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{}
	for _, m := range b.msgs {
		out[m.Status]++
	}
	return out
}

// Get returns one message.
func (b *Outbox) Get(id string) (Message, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if m.ID == id {
			return m, true
		}
	}
	return Message{}, false
}

// prune applies the retention rules (mu held).
func (b *Outbox) prune(now time.Time) {
	for k, t := range b.keys {
		if now.Sub(t) > dedupeKeep {
			delete(b.keys, k)
		}
	}
	var live, done []Message
	for _, m := range b.msgs {
		if m.Status == StatusQueued {
			live = append(live, m)
			continue
		}
		at := m.SentAt
		if at.IsZero() {
			at = m.CreatedAt
		}
		if now.Sub(at) <= finishedKeep {
			done = append(done, m)
		}
	}
	if len(done) > maxFinished {
		sort.SliceStable(done, func(i, j int) bool { return done[i].CreatedAt.After(done[j].CreatedAt) })
		done = done[:maxFinished]
	}
	b.msgs = append(live, done...)
}

// Save writes the file if anything changed.
func (b *Outbox) Save() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dirty || b.o.Path == "" {
		return nil
	}
	b.prune(b.now())
	raw, err := json.Marshal(fileData{Messages: b.msgs, Keys: b.keys, Paused: b.pause})
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(b.o.Path, raw); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

// Run starts one worker per channel and saves every few seconds, then saves
// once more and returns when ctx is cancelled.
func (b *Outbox) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ch := range []string{ChanEmail, ChanTelegram, ChanWhatsApp} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.worker(ctx, ch)
		}()
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			if err := b.Save(); err != nil {
				b.o.Log.Warn("outbox save failed", "err", err)
			}
		}
	}
	wg.Wait()
	if err := b.Save(); err != nil {
		b.o.Log.Warn("outbox final save failed", "err", err)
	}
}

func (b *Outbox) worker(ctx context.Context, ch string) {
	for {
		wait := time.Second
		if b.Step(ctx, ch) {
			wait = b.o.Intervals[ch]
			if wait < 50*time.Millisecond {
				wait = 50 * time.Millisecond
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-b.wake:
		}
	}
}
