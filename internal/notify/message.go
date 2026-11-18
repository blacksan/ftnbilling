// Package notify delivers messages to the admin (email, Telegram, WhatsApp)
// and to tenants (WhatsApp). Everything goes through a persistent outbox, so
// a message is never sent twice and is retried after a restart.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Channels.
const (
	ChanEmail    = "email"
	ChanTelegram = "telegram"
	ChanWhatsApp = "whatsapp"
)

// Statuses.
const (
	StatusQueued    = "queued"
	StatusSent      = "sent"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
	StatusCancelled = "cancelled"
)

// Kinds (what the message is about).
const (
	KindAdminAlert      = "admin_alert"
	KindExpiryReminder  = "expiry_reminder"
	KindPlanExpired     = "plan_expired"
	KindPaymentReceived = "payment_received"
	KindOutageNotice    = "outage_notice"
	KindOutageResolved  = "outage_resolved"
	KindReply           = "reply" // free-form session reply to a tenant command
	KindTest            = "test"
)

// WhatsApp template names (see the README for their wording).
const (
	TplExpiryReminder  = "ftn_expiry_reminder"
	TplPlanExpired     = "ftn_plan_expired"
	TplPaymentReceived = "ftn_payment_received"
	TplOutageNotice    = "ftn_outage_notice"
	TplOutageResolved  = "ftn_outage_resolved"
	TplAdminAlert      = "ftn_admin_alert"
)

// Message is one outgoing message.
type Message struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"` // email | telegram | whatsapp
	Kind      string `json:"kind"`
	To        string `json:"to"`
	SiteID    string `json:"site_id,omitempty"`
	AccountID string `json:"account_id,omitempty"` // set for tenant messages

	Template string   `json:"template,omitempty"` // WhatsApp template name
	Lang     string   `json:"lang,omitempty"`
	Params   []string `json:"params,omitempty"`
	Subject  string   `json:"subject,omitempty"`
	Text     string   `json:"text,omitempty"`

	DedupeKey string `json:"dedupe_key,omitempty"`
	Priority  int    `json:"priority,omitempty"` // higher goes first
	// Ref ties a tenant message to what it is about (the plan expiry, or the
	// outage start, as unix seconds) so Validate can tell it is stale.
	Ref int64 `json:"ref,omitempty"`

	Status    string    `json:"status"`
	Attempts  int       `json:"attempts,omitempty"`
	NotBefore time.Time `json:"not_before,omitempty"`
	NextTry   time.Time `json:"next_try,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	SentAt    time.Time `json:"sent_at,omitempty"`

	LastErr    string `json:"last_err,omitempty"`
	ProviderID string `json:"provider_id,omitempty"`
	Delivery   string `json:"delivery,omitempty"` // sent | delivered | read | failed, from the webhook
}

// Tenant reports whether the message goes to a tenant (not the admin).
func (m Message) Tenant() bool { return m.AccountID != "" }

// Sender delivers one message on one channel.
type Sender interface {
	// Send returns the provider's message id. Errors should wrap
	// ErrPermanent (never retry) or ErrAuth (pause the channel) when they can.
	Send(ctx context.Context, m Message) (providerID string, err error)
}

// PermanentError wraps an error that retrying cannot fix.
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// AuthError wraps an error caused by bad or expired credentials: the channel
// is paused until the admin fixes them.
type AuthError struct{ Err error }

func (e AuthError) Error() string { return e.Err.Error() }
func (e AuthError) Unwrap() error { return e.Err }

// Permanent and Auth build the wrapped errors.
func Permanent(format string, a ...any) error { return PermanentError{fmt.Errorf(format, a...)} }
func Auth(format string, a ...any) error      { return AuthError{fmt.Errorf(format, a...)} }

func isPermanent(err error) bool { var p PermanentError; return errors.As(err, &p) }
func isAuth(err error) bool      { var p AuthError; return errors.As(err, &p) }

// CleanParam makes a value safe as a WhatsApp template parameter: no
// newlines or tabs, runs of spaces collapsed, at most 200 characters, never empty.
func CleanParam(s string) string {
	s = strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	if s == "" {
		s = "-"
	}
	return s
}
