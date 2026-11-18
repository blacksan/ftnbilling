package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

const (
	seenCap       = 2000
	repliesPerMin = 5
)

// InboundOptions configures the WhatsApp webhook handler.
type InboundOptions struct {
	Store        *store.Store
	Box          *Outbox
	Log          *slog.Logger
	Now          func() time.Time
	AppSecret    string // WA_APP_SECRET: signs every POST
	VerifyToken  string // WA_VERIFY_TOKEN: the GET handshake
	SupportPhone string
	PayHint      string
}

// Inbound handles Meta's webhook: delivery statuses and tenant commands.
type Inbound struct {
	o   InboundOptions
	now func() time.Time

	mu    sync.Mutex
	seen  map[string]bool
	order []string
	sent  map[string][]time.Time // sender -> reply times
}

// NewInbound builds the handler.
func NewInbound(o InboundOptions) *Inbound {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Inbound{o: o, now: o.Now, seen: map[string]bool{}, sent: map[string][]time.Time{}}
}

// Verify answers the GET handshake: it returns the challenge to echo when the
// mode and the verify token are right.
func (in *Inbound) Verify(mode, token, challenge string) (string, bool) {
	if mode != "subscribe" || in.o.VerifyToken == "" || !hmac.Equal([]byte(token), []byte(in.o.VerifyToken)) {
		return "", false
	}
	return challenge, true
}

// ValidSignature checks X-Hub-Signature-256 ("sha256=" + hex HMAC of the raw
// body keyed with the app secret) in constant time.
func (in *Inbound) ValidSignature(body []byte, header string) bool {
	const pfx = "sha256="
	if in.o.AppSecret == "" || !strings.HasPrefix(header, pfx) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, pfx))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(in.o.AppSecret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// Sign returns the header value for body (tests and tools).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type waEvent struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
				Statuses []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Errors []struct {
						Title   string `json:"title"`
						Message string `json:"message"`
					} `json:"errors"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// Handle processes one verified POST body.
func (in *Inbound) Handle(body []byte) error {
	var ev waEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return err
	}
	for _, e := range ev.Entry {
		for _, c := range e.Changes {
			for _, s := range c.Value.Statuses {
				detail := ""
				if len(s.Errors) > 0 {
					detail = s.Errors[0].Title
					if s.Errors[0].Message != "" {
						detail += ": " + s.Errors[0].Message
					}
				}
				in.o.Box.SetDelivery(s.ID, s.Status, detail)
			}
			for _, m := range c.Value.Messages {
				if m.ID == "" || in.dup(m.ID) {
					continue
				}
				text := ""
				if m.Type == "text" {
					text = m.Text.Body
				}
				in.command(m.ID, strings.TrimPrefix(m.From, "+"), text)
			}
		}
	}
	return nil
}

// dup remembers the last 2000 message ids.
func (in *Inbound) dup(id string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.seen[id] {
		return true
	}
	in.seen[id] = true
	in.order = append(in.order, id)
	if len(in.order) > seenCap {
		delete(in.seen, in.order[0])
		in.order = in.order[1:]
	}
	return false
}

// allowReply is the 5 replies per sender per minute limit.
func (in *Inbound) allowReply(from string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	now := in.now()
	keep := in.sent[from][:0]
	for _, t := range in.sent[from] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= repliesPerMin {
		in.sent[from] = keep
		return false
	}
	in.sent[from] = append(keep, now)
	if len(in.sent) > 5000 {
		in.sent = map[string][]time.Time{from: in.sent[from]}
	}
	return true
}

func (in *Inbound) reply(id, to, siteID, text string) {
	if !in.allowReply(to) {
		return
	}
	// Free-form text: valid because the tenant just wrote to us (24 hour session).
	in.o.Box.Enqueue(Message{Channel: ChanWhatsApp, Kind: KindReply, To: to, SiteID: siteID, Text: text, Priority: 20, DedupeKey: "wa-in:" + id})
}

const helpText = "Reply BALANCE to see your plan, STOP to stop these messages, START to turn them back on."

func (in *Inbound) command(id, from, text string) {
	word := strings.ToUpper(strings.TrimSpace(text))
	if i := strings.IndexAny(word, " \n\t"); i > 0 {
		word = word[:i]
	}
	var accts []store.Account
	siteName := map[string]string{}
	in.o.Store.View(func(d *store.Data) {
		for _, a := range d.Accounts {
			if a.WhatsApp == from {
				accts = append(accts, a)
				siteName[a.SiteID] = d.Sites[a.SiteID].Name
			}
		}
	})
	if len(accts) == 0 {
		msg := "We could not find an account for this number."
		if in.o.SupportPhone != "" {
			msg += " Please call " + in.o.SupportPhone + "."
		}
		in.reply(id, from, "", msg)
		return
	}
	site := accts[0].SiteID
	switch word {
	case "STOP":
		in.setOptOut(from, true)
		in.reply(id, from, site, "You will not get more messages from us. Reply START to turn them back on.")
	case "START":
		in.setOptOut(from, false)
		in.reply(id, from, site, "Messages are on again. Reply BALANCE any time to see your plan.")
	case "BALANCE", "STATUS":
		in.reply(id, from, site, in.balanceText(accts, siteName))
	default:
		in.reply(id, from, site, helpText)
	}
}

func (in *Inbound) setOptOut(from string, out bool) {
	_ = in.o.Store.Update(func(d *store.Data) error {
		if !d.SetWAOptOutByNumber(from, out) { // updates the linked customer too
			return store.ErrNoChange
		}
		return nil
	})
}

func (in *Inbound) balanceText(accts []store.Account, siteName map[string]string) string {
	now := in.now()
	var lines []string
	for i, a := range accts {
		if i >= 5 {
			break
		}
		var pkg store.Package
		in.o.Store.View(func(d *store.Data) { pkg, _ = d.EffectivePackage(a.SiteID, a.PackageID) })
		where := a.Flat + ", " + siteName[a.SiteID]
		switch billing.AccountState(a, now) {
		case billing.StateActive:
			lines = append(lines, fmt.Sprintf("%s: %s plan active until %s (%s left). Account number %s.", where, pkg.Name, eatText(a.ExpiresAt), billing.Remaining(a.ExpiresAt, now), a.Code))
		case billing.StateExpired:
			lines = append(lines, fmt.Sprintf("%s: your plan ended on %s. Pay via %s, account number %s, to reconnect.", where, eatText(a.ExpiresAt), in.o.PayHint, a.Code))
		case billing.StateSuspended:
			lines = append(lines, fmt.Sprintf("%s: this account is suspended.%s", where, supportSuffix(in.o.SupportPhone)))
		default:
			lines = append(lines, fmt.Sprintf("%s: no plan yet. Pay via %s, account number %s.", where, in.o.PayHint, a.Code))
		}
	}
	return strings.Join(lines, "\n")
}

func supportSuffix(p string) string {
	if p == "" {
		return ""
	}
	return " Please call " + p + "."
}
