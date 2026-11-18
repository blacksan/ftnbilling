package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fake is a Sender that records messages and returns scripted errors.
type fake struct {
	mu   sync.Mutex
	sent []Message
	errs []error // consumed one per call; nil entry = success
}

func (f *fake) Send(_ context.Context, m Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	if err != nil {
		return "", err
	}
	f.sent = append(f.sent, m)
	return fmt.Sprintf("p%d", len(f.sent)), nil
}

func (f *fake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

// noon EAT on a Wednesday.
func noon() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, EAT) }

func newBox(t *testing.T, c *clock, f *fake, mut func(*Options)) *Outbox {
	t.Helper()
	o := Options{Path: filepath.Join(t.TempDir(), "outbox.json"), Senders: map[string]Sender{ChanWhatsApp: f, ChanTelegram: f, ChanEmail: f},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: c.Now,
		Intervals: map[string]time.Duration{ChanWhatsApp: 200 * time.Millisecond, ChanTelegram: time.Second, ChanEmail: 2 * time.Second}}
	if mut != nil {
		mut(&o)
	}
	b, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDedupe(t *testing.T) {
	c := &clock{t: noon()}
	b := newBox(t, c, &fake{}, nil)
	m := Message{Channel: ChanTelegram, To: "1", Text: "x", DedupeKey: "k1"}
	if _, ok := b.Enqueue(m); !ok {
		t.Fatal("first enqueue")
	}
	if _, ok := b.Enqueue(m); ok {
		t.Fatal("duplicate must be ignored")
	}
	// still a duplicate after it was sent and after a reload
	b.Step(context.Background(), ChanTelegram)
	if _, ok := b.Enqueue(m); ok {
		t.Fatal("duplicate after send")
	}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	b2, err := Open(b.o)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b2.Enqueue(m); ok {
		t.Fatal("duplicate after reload")
	}
	// keys expire after 90 days
	c.Add(91 * 24 * time.Hour)
	b2.dirty = true
	b2.Save()
	if _, ok := b2.Enqueue(m); !ok {
		t.Fatal("key must expire after 90 days")
	}
}

func TestBackoffAndGiveUp(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := backoff(i + 1); got != w && !(w == time.Hour && got == time.Hour) {
			t.Fatalf("backoff(%d)=%v want %v", i+1, got, w)
		}
	}
	c := &clock{t: noon()}
	f := &fake{}
	for i := 0; i < 20; i++ {
		f.errs = append(f.errs, errors.New("network down"))
	}
	b := newBox(t, c, f, nil)
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "x"})
	ctx := context.Background()
	for i := 1; i <= maxAttempts; i++ {
		if !b.Step(ctx, ChanTelegram) {
			t.Fatalf("attempt %d should have been made", i)
		}
		m := b.List(Filter{})[0]
		if i < maxAttempts {
			if m.Status != StatusQueued || m.Attempts != i || !m.NextTry.Equal(c.Now().Add(backoff(i))) {
				t.Fatalf("after %d: %+v", i, m)
			}
			// not due yet
			c.Add(backoff(i) - time.Second)
			if b.Step(ctx, ChanTelegram) {
				t.Fatal("sent before the backoff elapsed")
			}
			c.Add(time.Second)
		}
	}
	m := b.List(Filter{})[0]
	if m.Status != StatusFailed || m.Attempts != maxAttempts {
		t.Fatalf("must give up after 8 attempts: %+v", m)
	}
}

func TestPermanentFailsAtOnceAndRetry(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{errs: []error{Permanent("recipient not on WhatsApp")}}
	b := newBox(t, c, f, nil)
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "x"})
	b.Step(context.Background(), ChanTelegram)
	m := b.List(Filter{})[0]
	if m.Status != StatusFailed || m.Attempts != 1 {
		t.Fatalf("%+v", m)
	}
	if !b.Retry(m.ID) || b.List(Filter{})[0].Status != StatusQueued {
		t.Fatal("retry")
	}
	c.Add(2 * time.Second)
	b.Step(context.Background(), ChanTelegram)
	if m := b.List(Filter{})[0]; m.Status != StatusSent {
		t.Fatalf("%+v", m)
	}
	if b.Retry(m.ID) {
		t.Fatal("a sent message cannot be retried")
	}
}

func TestAuthErrorPausesChannel(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{errs: []error{Auth("token expired")}}
	b := newBox(t, c, f, nil)
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "a"})
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "b"})
	ctx := context.Background()
	b.Step(ctx, ChanTelegram)
	if _, p := b.Paused()[ChanTelegram]; !p {
		t.Fatal("channel must be paused")
	}
	c.Add(time.Hour)
	if b.Step(ctx, ChanTelegram) || f.count() != 0 {
		t.Fatal("a paused channel sends nothing")
	}
	b.Resume(ChanTelegram)
	for i := 0; i < 2; i++ {
		c.Add(2 * time.Second)
		b.Step(ctx, ChanTelegram)
	}
	if f.count() != 2 {
		t.Fatalf("sent %d after resume", f.count())
	}
	// Direct success lifts a pause too.
	f.errs = []error{Auth("again")}
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "c"})
	c.Add(2 * time.Second)
	b.Step(ctx, ChanTelegram)
	if len(b.Paused()) != 1 {
		t.Fatal("paused again")
	}
	if err := b.Direct(ctx, Message{Channel: ChanTelegram, To: "1", Text: "test"}); err != nil || len(b.Paused()) != 0 {
		t.Fatalf("direct: %v %v", err, b.Paused())
	}
}

func TestRateLimitAndPriority(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{}
	b := newBox(t, c, f, nil)
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000001", Template: "t", Text: "low"})
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000002", Template: "t", Text: "high", Priority: 5})
	ctx := context.Background()
	if !b.Step(ctx, ChanWhatsApp) {
		t.Fatal("first")
	}
	if b.Step(ctx, ChanWhatsApp) {
		t.Fatal("WhatsApp is limited to 5 per second")
	}
	c.Add(199 * time.Millisecond)
	if b.Step(ctx, ChanWhatsApp) {
		t.Fatal("still inside 200ms")
	}
	c.Add(time.Millisecond)
	if !b.Step(ctx, ChanWhatsApp) {
		t.Fatal("second")
	}
	if f.sent[0].Text != "high" || f.sent[1].Text != "low" {
		t.Fatalf("priority order: %v %v", f.sent[0].Text, f.sent[1].Text)
	}
}

func TestWADailyCapExemptsAdminAlerts(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{}
	b := newBox(t, c, f, func(o *Options) { o.WADailyLimit = 3 })
	for i := 0; i < 5; i++ {
		b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000001", AccountID: "a", Kind: KindPaymentReceived, Template: "t", DedupeKey: fmt.Sprint("p", i)})
	}
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		c.Add(time.Second)
		b.Step(ctx, ChanWhatsApp)
	}
	if f.count() != 3 {
		t.Fatalf("daily cap: sent %d, want 3", f.count())
	}
	// An admin alert is exempt.
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000009", Kind: KindAdminAlert, Template: TplAdminAlert})
	c.Add(time.Second)
	if !b.Step(ctx, ChanWhatsApp) || f.count() != 4 {
		t.Fatalf("admin alert must bypass the cap, sent %d", f.count())
	}
	// Next EAT morning the rest go out.
	c.t = time.Date(2026, 11, 19, 8, 0, 0, 0, EAT)
	for i := 0; i < 4; i++ {
		c.Add(time.Second)
		b.Step(ctx, ChanWhatsApp)
	}
	if f.count() != 6 {
		t.Fatalf("after midnight sent %d, want 6", f.count())
	}
}

func TestQuietHours(t *testing.T) {
	cases := map[int]bool{6: true, 7: false, 12: false, 20: false, 21: true, 23: true, 0: true}
	for h, quiet := range cases {
		if got := QuietNow(time.Date(2026, 11, 18, h, 30, 0, 0, EAT)); got != quiet {
			t.Fatalf("hour %d quiet=%v", h, got)
		}
	}
	if got := NextOpen(time.Date(2026, 11, 18, 22, 0, 0, 0, EAT)); !got.Equal(time.Date(2026, 11, 19, 7, 0, 0, 0, EAT)) {
		t.Fatal(got)
	}
	if got := NextOpen(time.Date(2026, 11, 18, 3, 0, 0, 0, EAT)); !got.Equal(time.Date(2026, 11, 18, 7, 0, 0, 0, EAT)) {
		t.Fatal(got)
	}
	c := &clock{t: time.Date(2026, 11, 18, 22, 0, 0, 0, EAT)}
	f := &fake{}
	b := newBox(t, c, f, nil)
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000001", AccountID: "a", Kind: KindExpiryReminder, Template: "t"})
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000001", AccountID: "a", Kind: KindPaymentReceived, Template: "t"})
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000009", Kind: KindAdminAlert, Template: "t"})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.Add(time.Second)
		b.Step(ctx, ChanWhatsApp)
	}
	if f.count() != 2 {
		t.Fatalf("at 22:00 the reminder waits; payment confirmation and admin alert go: sent %d", f.count())
	}
	c.t = time.Date(2026, 11, 19, 7, 0, 5, 0, EAT)
	b.Step(ctx, ChanWhatsApp)
	if f.count() != 3 || f.sent[2].Kind != KindExpiryReminder {
		t.Fatalf("reminder at 07:00: %d", f.count())
	}
}

func TestValidateSkips(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{}
	b := newBox(t, c, f, func(o *Options) {
		o.Validate = func(m Message) (bool, string) { return m.Ref != 1, "plan was renewed" }
	})
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000001", AccountID: "a", Kind: KindExpiryReminder, Template: "t", Ref: 1})
	b.Enqueue(Message{Channel: ChanWhatsApp, To: "254700000002", AccountID: "b", Kind: KindExpiryReminder, Template: "t", Ref: 2})
	ctx := context.Background()
	b.Step(ctx, ChanWhatsApp) // skips the first, sends the second in the same step
	if f.count() != 1 || f.sent[0].AccountID != "b" {
		t.Fatalf("%+v", f.sent)
	}
	var skipped Message
	for _, m := range b.List(Filter{Status: StatusSkipped}) {
		skipped = m
	}
	if skipped.AccountID != "a" || skipped.LastErr != "plan was renewed" {
		t.Fatalf("%+v", skipped)
	}
}

func TestRetentionAndPersistence(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{}
	b := newBox(t, c, f, nil)
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "old"})
	b.Step(context.Background(), ChanTelegram)
	b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "still queued", NotBefore: c.Now().Add(1000 * time.Hour)})
	c.Add(61 * 24 * time.Hour)
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	b2, err := Open(b.o)
	if err != nil {
		t.Fatal(err)
	}
	l := b2.List(Filter{})
	if len(l) != 1 || l[0].Text != "still queued" {
		t.Fatalf("sent rows older than 60 days must go: %+v", l)
	}
	// cap of 5000 finished rows
	b2.mu.Lock()
	for i := 0; i < maxFinished+10; i++ {
		b2.msgs = append(b2.msgs, Message{ID: fmt.Sprint(i), Status: StatusSent, CreatedAt: c.Now().Add(time.Duration(i) * time.Second), SentAt: c.Now()})
	}
	b2.prune(c.Now())
	n := len(b2.msgs)
	b2.mu.Unlock()
	if n != maxFinished+1 {
		t.Fatalf("kept %d rows", n)
	}
}

func TestCancelAndDelivery(t *testing.T) {
	c := &clock{t: noon()}
	f := &fake{}
	b := newBox(t, c, f, nil)
	m, _ := b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "x"})
	if !b.Cancel(m.ID) {
		t.Fatal("cancel")
	}
	if b.Step(context.Background(), ChanTelegram) {
		t.Fatal("cancelled message was sent")
	}
	m2, _ := b.Enqueue(Message{Channel: ChanTelegram, To: "1", Text: "y"})
	b.Step(context.Background(), ChanTelegram)
	got, _ := b.Get(m2.ID)
	if got.ProviderID == "" || !b.SetDelivery(got.ProviderID, "delivered", "") {
		t.Fatal("delivery")
	}
	b.SetDelivery(got.ProviderID, "sent", "") // a late, lower status must not move it back
	if got, _ = b.Get(m2.ID); got.Delivery != "delivered" {
		t.Fatal(got.Delivery)
	}
}
