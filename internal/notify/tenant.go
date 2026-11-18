package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// TenantOptions configures the tenant message scanners.
type TenantOptions struct {
	Store *store.Store
	Box   *Outbox
	Log   *slog.Logger
	Now   func() time.Time
	Lang  string
	// Policy is the lifecycle policy (reminder days, reminder on expiry).
	Policy func() billing.Policy
	// PayHint tells a tenant how to pay, e.g. "M-Pesa Paybill 123456".
	PayHint string
	// MinConfirmKES is the smallest payment that gets a confirmation.
	MinConfirmKES int

	// Outage notices. Mode is "off", "manual" or "auto"; AutoAfter is how long
	// the offline alert must have fired before the automatic notice.
	Mode      func() string
	AutoAfter func() time.Duration
	// OfflineFiredAt returns the start of a building's outage and when its
	// offline alert fired, if it is firing.
	OfflineFiredAt func(siteID string) (start, firedAt time.Time, ok bool)
	// OpenOutage is the outage in progress at a building.
	OpenOutage func(siteID string) (start time.Time, ok bool)
	// OutageEnd tells whether the outage that started at start is over.
	OutageEnd func(siteID string, start time.Time) (time.Time, bool)
}

// Tenant creates the messages tenants get: plan reminders, payment
// confirmations and outage notices. Everything is idempotent (dedupe keys), so
// scanning often is safe.
type Tenant struct {
	o    TenantOptions
	now  func() time.Time
	kick chan struct{}
	mu   sync.Mutex
}

// NewTenant builds the scanners and installs the outbox Validate hook.
func NewTenant(o TenantOptions) *Tenant {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Policy == nil {
		o.Policy = billing.DefaultPolicy
	}
	if o.Mode == nil {
		o.Mode = func() string { return "manual" }
	}
	if o.AutoAfter == nil {
		o.AutoAfter = func() time.Duration { return 30 * time.Minute }
	}
	t := &Tenant{o: o, now: o.Now, kick: make(chan struct{}, 1)}
	o.Box.SetValidate(t.Validate)
	return t
}

// Kick asks the scanner to run now (after a payment was applied).
func (t *Tenant) Kick() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// Run scans every 60 seconds and whenever kicked, until ctx ends.
func (t *Tenant) Run(ctx context.Context) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	t.Scan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-t.kick:
		}
		t.Scan()
	}
}

func eatText(t time.Time) string { return t.In(EAT).Format("Mon 2 Jan, 15:04") }

func (t *Tenant) pkgName(d *store.Data, a store.Account) (store.Package, bool) {
	p, ok := d.EffectivePackage(a.SiteID, a.PackageID)
	return p, ok
}

func reachable(a store.Account) bool { return a.WhatsApp != "" && !a.WAOptOut }

// Scan runs every scanner once.
func (t *Tenant) Scan() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	pol := t.o.Policy()
	var out []Message
	siteName := map[string]string{}
	t.o.Store.View(func(d *store.Data) {
		for id, s := range d.Sites {
			siteName[id] = s.Name
		}
		flatAt := func(a store.Account) string { return a.Flat + ", " + siteName[a.SiteID] }
		base := func(a store.Account, kind, tpl string, ref int64, key string, params ...string) Message {
			return Message{Channel: ChanWhatsApp, Kind: kind, To: a.WhatsApp, SiteID: a.SiteID, AccountID: a.ID,
				Template: tpl, Lang: t.o.Lang, Params: params, Ref: ref, DedupeKey: key}
		}
		for _, a := range d.Accounts {
			if !reachable(a) || a.Suspended || a.ExpiresAt.IsZero() {
				continue
			}
			pkg, ok := t.pkgName(d, a)
			if !ok {
				continue
			}
			exp := a.ExpiresAt
			// Expiry reminder: the sweeper stamped RemindedFor for this expiry.
			if pol.ReminderDays > 0 && a.RemindedFor.Equal(exp) && exp.After(now) {
				m := base(a, KindExpiryReminder, TplExpiryReminder, exp.Unix(), fmt.Sprintf("rem:%s:%d", a.ID, exp.Unix()),
					pkg.Name, flatAt(a), eatText(exp), fmt.Sprint(pkg.PriceKES), t.o.PayHint, a.Code)
				m.Text = fmt.Sprintf("Your %s plan for %s ends on %s. Pay KES %d via %s, account number %s.", pkg.Name, flatAt(a), eatText(exp), pkg.PriceKES, t.o.PayHint, a.Code)
				out = append(out, m)
			}
			// Plan ended (plans of 7 days or longer; not for something long ago).
			if pol.ReminderOnExpiry && a.ExpiredFor.Equal(exp) && !exp.After(now) && now.Sub(exp) <= 48*time.Hour &&
				time.Duration(pkg.DurationMin)*time.Minute >= 7*24*time.Hour {
				m := base(a, KindPlanExpired, TplPlanExpired, exp.Unix(), fmt.Sprintf("exp:%s:%d", a.ID, exp.Unix()),
					flatAt(a), eatText(exp), t.o.PayHint, a.Code)
				m.Text = fmt.Sprintf("The internet plan for %s ended on %s. Pay via %s, account number %s.", flatAt(a), eatText(exp), t.o.PayHint, a.Code)
				out = append(out, m)
			}
		}
		// Payment received: applied recently, real money, big enough.
		for _, p := range d.Payments {
			if p.Status != store.PayApplied || p.AccountID == "" || p.Method == "admin" || p.Method == "voucher" || p.AmountKES < t.o.MinConfirmKES {
				continue
			}
			at := p.CreatedAt
			if !p.ResolvedAt.IsZero() {
				at = p.ResolvedAt
			}
			if now.Sub(at) > 6*time.Hour {
				continue
			}
			a, ok := d.Accounts[p.AccountID]
			if !ok || !reachable(a) {
				continue
			}
			pkg, _ := t.pkgName(d, a)
			if pp, ok := d.EffectivePackage(a.SiteID, p.PackageID); ok {
				pkg = pp
			}
			m := base(a, KindPaymentReceived, TplPaymentReceived, 0, "pay:"+p.Key,
				fmt.Sprint(p.AmountKES), p.Key, pkg.Name, flatAt(a), eatText(a.ExpiresAt))
			m.Text = fmt.Sprintf("Payment received: KES %d, reference %s. Your %s plan for %s is active until %s.", p.AmountKES, p.Key, pkg.Name, flatAt(a), eatText(a.ExpiresAt))
			out = append(out, m)
		}
	})
	for _, m := range out {
		t.o.Box.Enqueue(m)
	}
	t.scanOutages(now)
}

// scanOutages sends the automatic notices and the "restored" messages.
func (t *Tenant) scanOutages(now time.Time) {
	if t.o.Mode() == "auto" && t.o.OfflineFiredAt != nil {
		var ids []string
		t.o.Store.View(func(d *store.Data) {
			for _, s := range d.SiteList() {
				ids = append(ids, s.ID)
			}
		})
		for _, id := range ids {
			if start, fired, ok := t.o.OfflineFiredAt(id); ok && now.Sub(fired) >= t.o.AutoAfter() {
				t.SendOutageNotice(id, start)
			}
		}
	}
	if t.o.OutageEnd == nil {
		return
	}
	sent := t.o.Box.Find(func(m Message) bool { return m.Kind == KindOutageNotice && m.Status == StatusSent })
	for _, n := range sent {
		start := time.Unix(n.Ref, 0)
		end, over := t.o.OutageEnd(n.SiteID, start)
		if !over {
			continue
		}
		var name string
		t.o.Store.View(func(d *store.Data) { name = d.Sites[n.SiteID].Name })
		m := Message{Channel: ChanWhatsApp, Kind: KindOutageResolved, To: n.To, SiteID: n.SiteID, AccountID: n.AccountID, Template: TplOutageResolved,
			Lang: t.o.Lang, Params: []string{name, eatText(end)}, Ref: n.Ref, Priority: 5,
			DedupeKey: fmt.Sprintf("outok:%s:%d:%s", n.SiteID, n.Ref, n.AccountID),
			Text:      fmt.Sprintf("Internet service at %s was restored at %s.", name, eatText(end))}
		t.o.Box.Enqueue(m)
	}
}

// SendOutageNotice queues the outage message for every reachable tenant of a
// building. start is the outage start (its dedupe key, so a second press does
// nothing); it returns how many messages were queued.
func (t *Tenant) SendOutageNotice(siteID string, start time.Time) int {
	var msgs []Message
	t.o.Store.View(func(d *store.Data) {
		name := d.Sites[siteID].Name
		var accts []store.Account
		for _, a := range d.Accounts {
			if a.SiteID == siteID && reachable(a) {
				accts = append(accts, a)
			}
		}
		sort.Slice(accts, func(i, j int) bool { return accts[i].Code < accts[j].Code })
		for _, a := range accts {
			msgs = append(msgs, Message{Channel: ChanWhatsApp, Kind: KindOutageNotice, To: a.WhatsApp, SiteID: siteID, AccountID: a.ID,
				Template: TplOutageNotice, Lang: t.o.Lang, Params: []string{name, eatText(start)}, Ref: start.Unix(), Priority: 5,
				DedupeKey: fmt.Sprintf("out:%s:%d:%s", siteID, start.Unix(), a.ID),
				Text:      fmt.Sprintf("We are aware of an internet outage at %s since %s.", name, eatText(start))})
		}
	})
	n := 0
	for _, m := range msgs {
		if _, ok := t.o.Box.Enqueue(m); ok {
			n++
		}
	}
	return n
}

// ManualOutageStart is the start to use for a manual notice: the outage in
// progress, else now.
func (t *Tenant) ManualOutageStart(siteID string) time.Time {
	if t.o.OpenOutage != nil {
		if s, ok := t.o.OpenOutage(siteID); ok {
			return s
		}
	}
	return t.now()
}

// Validate is the outbox hook run just before a tenant message is sent: it
// skips messages that no longer apply.
func (t *Tenant) Validate(m Message) (bool, string) {
	var a store.Account
	var ok bool
	t.o.Store.View(func(d *store.Data) { a, ok = d.Accounts[m.AccountID] })
	if !ok {
		return false, "the account no longer exists"
	}
	if a.WAOptOut {
		return false, "the tenant opted out"
	}
	if a.WhatsApp == "" || a.WhatsApp != m.To {
		return false, "the WhatsApp number changed"
	}
	now := t.now()
	switch m.Kind {
	case KindExpiryReminder:
		if a.ExpiresAt.Unix() != m.Ref || a.Suspended || !a.ExpiresAt.After(now) {
			return false, "the plan was renewed or changed"
		}
	case KindPlanExpired:
		if a.ExpiresAt.Unix() != m.Ref || a.Suspended {
			return false, "the plan was renewed or changed"
		}
	case KindOutageNotice:
		if t.o.OutageEnd != nil {
			if _, over := t.o.OutageEnd(m.SiteID, time.Unix(m.Ref, 0)); over {
				return false, "the outage was already over"
			}
		}
	}
	return true, ""
}

// ReplyText builds a free-form session reply (inbound webhook).
func ReplyText(parts ...string) string { return strings.Join(parts, "\n") }
