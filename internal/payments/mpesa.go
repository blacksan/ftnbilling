package payments

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

var eat = time.FixedZone("EAT", 3*3600)

// MpesaConfig mirrors the MPESA_* settings.
type MpesaConfig struct {
	BaseURL       string // https://sandbox.safaricom.co.ke or https://api.safaricom.co.ke
	Key, Secret   string
	Shortcode     string
	C2BShortcode  string // the shortcode C2B URLs are registered for when it differs from the STK one (sandbox); "" = Shortcode
	Passkey       string
	PublicBaseURL string
	CallbackToken string // shared secret in the callback URL (?t= or /pay/cb/t/<token>/)
	// PrevToken is the token CallbackToken replaced; it is accepted, in both
	// URL forms, until PrevUntil (a grace period for re-registration).
	PrevToken string
	PrevUntil time.Time
	// UnknownRef decides what happens to a Paybill payment whose account
	// number matches nothing: "reject" (default) bounces it at validation,
	// "accept" takes it into the Unmatched queue.
	UnknownRef string
	Enabled    bool
	// Initiator and SecurityCredential (base64, already encrypted with
	// Safaricom's certificate) let the server call the Transaction Status API.
	// ConfirmReceipts turns the receipt check on (the caller decides, see
	// config.MpesaConfirm); it also needs a callback token and a spool.
	Initiator, SecurityCredential string
	ConfirmReceipts               bool
}

// FTNTestRef is the reserved account reference of the Settings > M-Pesa test
// payment. It never matches a flat.
const FTNTestRef = "FTNTEST"

// TestState is the progress of one Settings test payment.
type TestState struct {
	CheckoutID string    `json:"checkout_id"`
	Phone      string    `json:"phone"`
	Status     string    `json:"status"` // pending, paid, failed
	Detail     string    `json:"detail"`
	Receipt    string    `json:"receipt,omitempty"`
	Started    time.Time `json:"started"`
}

// Mpesa is the Safaricom Daraja provider (STK Push + C2B).
type Mpesa struct {
	cfg   MpesaConfig // guarded by mu; replaced as a whole by SetConfig
	c     Completer
	st    *store.Store
	log   *slog.Logger
	hc    *http.Client
	now   func() time.Time
	mu    sync.Mutex
	tok   string
	tokX  time.Time
	spool *Spool
	tests map[string]*TestState // checkout id -> test payment, in memory only

	vmu    sync.Mutex
	verify map[string]*verifyState // "kind:receipt" -> receipt check in progress (mpesa_confirm.go)
	convs  map[string]string       // Daraja conversation id -> "kind:receipt" (or "test:receipt")
	rtests map[string]*ReceiptTest // "test:receipt" -> the Settings receipt test

	bgMu   sync.Mutex
	bg     sync.WaitGroup
	closed bool
	stop   chan struct{}

	pushTO  time.Duration   // 0 = defaultPushTimeout; guarded by mu
	alerted map[string]bool // held receipts already announced; guarded by mu
	alert   func(string)    // owner alert for a payment that was not credited; guarded by mu

	legacyQueryUntil time.Time // the old STK callback form (?t=) is accepted until then
}

func NewMpesa(cfg MpesaConfig, c Completer, st *store.Store, log *slog.Logger) *Mpesa {
	return &Mpesa{cfg: cfg, c: c, st: st, log: log, now: time.Now, hc: &http.Client{Timeout: 20 * time.Second}, stop: make(chan struct{})}
}

// SetClock overrides the clock (tests).
func (m *Mpesa) SetClock(f func() time.Time) { m.now = f }

func (m *Mpesa) Name() string { return "mpesa" }

// Enabled reads the live configuration.
func (m *Mpesa) Enabled() bool { return m.conf().Enabled }

// conf returns the current configuration snapshot.
func (m *Mpesa) conf() MpesaConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Config returns the current configuration (for the settings page; it holds
// secrets, so callers must not render it).
func (m *Mpesa) Config() MpesaConfig { return m.conf() }

// SetConfig replaces the configuration at run time and drops the cached
// OAuth token, so new credentials take effect on the next call.
func (m *Mpesa) SetConfig(c MpesaConfig) {
	m.mu.Lock()
	m.cfg = c
	m.tok, m.tokX = "", time.Time{}
	m.mu.Unlock()
}

// CheckCallbackToken reports whether M-Pesa is enabled, a callback token is
// set, and tok equals it (constant time). Callbacks are 404 otherwise.
func (m *Mpesa) CheckCallbackToken(tok string) bool {
	c := m.conf()
	if !c.Enabled || tok == "" {
		return false
	}
	// Both comparisons always run, so timing does not tell which one matched.
	cur := c.CallbackToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(c.CallbackToken)) == 1
	prev := c.PrevToken != "" && m.now().Before(c.PrevUntil) && subtle.ConstantTimeCompare([]byte(tok), []byte(c.PrevToken)) == 1
	return cur || prev
}

// CallbackURL builds a public callback URL for a path under /pay/cb/, with the
// secret as ?t= (the form the STK request carries).
func (m *Mpesa) CallbackURL(path string) string {
	c := m.conf()
	u := c.PublicBaseURL + path
	if c.CallbackToken != "" {
		u += "?t=" + url.QueryEscape(c.CallbackToken)
	}
	return u
}

// CallbackPathURL is the same callback with the secret in the path
// (/pay/cb/t/<token>/stk): the form registered for C2B, because Daraja's
// RegisterURL may refuse a query string.
func (m *Mpesa) CallbackPathURL(rest string) string {
	c := m.conf()
	if c.CallbackToken == "" {
		return c.PublicBaseURL + "/pay/cb/" + strings.TrimPrefix(rest, "/")
	}
	return c.PublicBaseURL + "/pay/cb/t/" + url.PathEscape(c.CallbackToken) + "/" + strings.TrimPrefix(rest, "/")
}

// SetLegacyQueryUntil sets when the old STK callback form (token in the query
// string) stops being accepted.
func (m *Mpesa) SetLegacyQueryUntil(t time.Time) {
	m.mu.Lock()
	m.legacyQueryUntil = t
	m.mu.Unlock()
}

// LegacyQueryOK reports whether an STK callback that carries its token in the
// query string is still accepted. Without SetLegacyQueryUntil it is not.
func (m *Mpesa) LegacyQueryOK() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now().Before(m.legacyQueryUntil)
}

// LegacyQueryWindow is how long the old STK callback form keeps working after
// the version that moved the token into the path first runs.
const LegacyQueryWindow = 30 * 24 * time.Hour

// scrub removes every credential from a message that may be shown or logged.
func (m *Mpesa) scrub(s string) string {
	c := m.conf()
	for _, secret := range []string{c.Key, c.Secret, c.Passkey, c.CallbackToken, c.PrevToken, m.tokenNow()} {
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, "[hidden]")
		}
	}
	return s
}

func (m *Mpesa) tokenNow() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tok
}

// token returns a cached OAuth token, refreshing 30s before expiry.
func (m *Mpesa) token(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.tok != "" && m.now().Before(m.tokX) {
		t := m.tok
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()
	tok, _, err := m.fetchToken(ctx, true)
	return tok, err
}

// TestOAuth asks Daraja for a fresh access token, bypassing the cache, and
// reports how long it is valid. Used by the Settings page.
func (m *Mpesa) TestOAuth(ctx context.Context) (time.Duration, error) {
	_, ttl, err := m.fetchToken(ctx, true)
	return ttl, err
}

func (m *Mpesa) fetchToken(ctx context.Context, store bool) (string, time.Duration, error) {
	c := m.conf()
	if c.Key == "" || c.Secret == "" {
		return "", 0, errors.New("daraja oauth: the consumer key and secret are not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", 0, err
	}
	req.SetBasicAuth(c.Key, c.Secret)
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("daraja oauth: %s", m.scrub(err.Error()))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("daraja oauth: HTTP %d (check the consumer key and secret, and that the environment matches the app)", resp.StatusCode)
	}
	var r struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.AccessToken == "" {
		return "", 0, errors.New("daraja oauth: bad response")
	}
	secs, _ := strconv.Atoi(rawString(r.ExpiresIn))
	if secs <= 0 {
		secs = 3000
	}
	ttl := time.Duration(secs) * time.Second
	m.mu.Lock()
	// The configuration may have changed while the request was in flight:
	// only keep the token if it is still for the same key.
	if store && m.cfg.Key == c.Key && m.cfg.Secret == c.Secret && m.cfg.BaseURL == c.BaseURL {
		m.tok = r.AccessToken
		m.tokX = m.now().Add(ttl - 30*time.Second)
	}
	m.mu.Unlock()
	return r.AccessToken, ttl, nil
}

// stkPassword is base64(shortcode+passkey+timestamp).
func stkPassword(shortcode, passkey, ts string) string {
	return base64.StdEncoding.EncodeToString([]byte(shortcode + passkey + ts))
}

// Start sends an STK Push to the customer's phone.
func (m *Mpesa) Start(ctx context.Context, o store.Order) error {
	if !m.Enabled() {
		return errors.New("M-Pesa is not configured")
	}
	if o.Phone == "" {
		return errors.New("phone number required")
	}
	var acct store.Account
	var okAcct bool
	m.st.View(func(d *store.Data) { acct, okAcct = d.Accounts[o.AccountID] })
	if !okAcct {
		return errors.New("unknown account")
	}
	// The push must not die with the visitor's browser request: once Daraja has
	// accepted it the customer may pay, and the order must stay payable.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.pushTimeout())
	defer cancel()
	id, err := m.stkPush(pctx, o.Phone, o.AmountKES, acct.Code)
	if err != nil {
		return err
	}
	if err := m.c.SetCheckoutID(o.ID, id); err != nil {
		if err2 := m.c.SetCheckoutID(o.ID, id); err2 != nil {
			m.log.Error("stk push accepted but its checkout id could not be stored; the payment will be matched by phone and amount", "order", o.ID, "checkout", id, "err", err2)
			return fmt.Errorf("%w: %v", ErrPushUncertain, err2)
		}
	}
	return nil
}

// ErrPushUncertain means Daraja may have accepted the push although we did not
// get a clear answer (timeout, dropped connection, unreadable reply, checkout
// id not stored). The prompt may be on the phone: leave the order pending and
// let the callback settle it.
var ErrPushUncertain = errors.New("the payment prompt may have been sent")

const defaultPushTimeout = 15 * time.Second

// SetPushTimeout overrides how long Start waits for Daraja (tests).
func (m *Mpesa) SetPushTimeout(d time.Duration) {
	m.mu.Lock()
	m.pushTO = d
	m.mu.Unlock()
}

func (m *Mpesa) pushTimeout() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pushTO > 0 {
		return m.pushTO
	}
	return defaultPushTimeout
}

// isDialError reports a failure before any byte was sent (nothing reached Daraja).
func isDialError(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

// stkPush sends one STK Push and returns its CheckoutRequestID.
func (m *Mpesa) stkPush(ctx context.Context, phone string, amount int, accountRef string) (string, error) {
	tok, err := m.token(ctx)
	if err != nil {
		return "", err
	}
	c := m.conf()
	ts := m.now().In(eat).Format("20060102150405")
	body := map[string]any{
		"BusinessShortCode": c.Shortcode,
		"Password":          stkPassword(c.Shortcode, c.Passkey, ts),
		"Timestamp":         ts,
		"TransactionType":   "CustomerPayBillOnline",
		"Amount":            amount,
		"PartyA":            phone,
		"PartyB":            c.Shortcode,
		"PhoneNumber":       phone,
		"CallBackURL":       m.CallbackPathURL("stk"), // the secret in the path: a query string ends up in logs and screenshots
		"AccountReference":  accountRef,
		"TransactionDesc":   "Internet",
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/mpesa/stkpush/v1/processrequest", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		if isDialError(err) {
			return "", fmt.Errorf("stk push: %s", m.scrub(err.Error()))
		}
		return "", fmt.Errorf("%w: stk push: %s", ErrPushUncertain, m.scrub(err.Error()))
	}
	defer resp.Body.Close()
	rb, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		CheckoutRequestID   string          `json:"CheckoutRequestID"`
		ResponseCode        json.RawMessage `json:"ResponseCode"`
		ResponseDescription string          `json:"ResponseDescription"`
		ErrorMessage        string          `json:"errorMessage"`
	}
	jerr := json.Unmarshal(rb, &r)
	if resp.StatusCode == 200 && (rerr != nil || jerr != nil || (rawString(r.ResponseCode) == "0" && r.CheckoutRequestID == "")) {
		return "", fmt.Errorf("%w: stk push: unreadable answer from Daraja", ErrPushUncertain)
	}
	if resp.StatusCode != 200 || rawString(r.ResponseCode) != "0" || r.CheckoutRequestID == "" {
		msg := r.ErrorMessage
		if msg == "" {
			msg = r.ResponseDescription
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		if resp.StatusCode >= 500 && r.ErrorMessage == "" && r.ResponseDescription == "" { // a gateway error, not Daraja's own refusal
			return "", fmt.Errorf("%w: stk push rejected: %s", ErrPushUncertain, m.scrub(msg))
		}
		return "", fmt.Errorf("stk push rejected: %s", m.scrub(msg))
	}
	return r.CheckoutRequestID, nil
}

// ---- Settings page: test payment, status query, C2B registration ---------

// NormalizePhone turns 07xx, 01xx, +254... into 254XXXXXXXXX for Daraja.
func NormalizePhone(s string) (string, error) {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	p := b.String()
	switch {
	case len(p) == 10 && p[0] == '0':
		p = "254" + p[1:]
	case len(p) == 9 && (p[0] == '7' || p[0] == '1'):
		p = "254" + p
	}
	if len(p) != 12 || !strings.HasPrefix(p, "254") {
		return "", errors.New("enter a Kenyan mobile number like 0712345678")
	}
	return p, nil
}

// StartTest sends a KES 1 STK Push with the reserved reference FTNTEST and
// tracks it in memory by CheckoutRequestID. The confirmation is recorded in
// the Unmatched queue with the note "settings test".
func (m *Mpesa) StartTest(ctx context.Context, phone string) (TestState, error) {
	if !m.Enabled() {
		return TestState{}, errors.New("M-Pesa is not enabled: save the key, secret, shortcode, passkey and the public URL first")
	}
	p, err := NormalizePhone(phone)
	if err != nil {
		return TestState{}, err
	}
	id, err := m.stkPush(ctx, p, 1, FTNTestRef)
	if err != nil {
		return TestState{}, err
	}
	ts := &TestState{CheckoutID: id, Phone: p, Status: "pending", Detail: "waiting for the prompt on the phone", Started: m.now()}
	m.mu.Lock()
	if m.tests == nil {
		m.tests = map[string]*TestState{}
	}
	for k, v := range m.tests { // keep the table small
		if m.now().Sub(v.Started) > time.Hour {
			delete(m.tests, k)
		}
	}
	m.tests[id] = ts
	out := *ts
	m.mu.Unlock()
	return out, nil
}

// testWindow is how long a Settings test payment stays pending.
const testWindow = 10 * time.Minute

// testPending reports whether a test payment is waiting for its result.
func (m *Mpesa) testPending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, t := range m.tests {
		if t.Status == "pending" && now.Sub(t.Started) < testWindow {
			return true
		}
	}
	return false
}

// TestStatus returns the state of a test payment (false = unknown id).
func (m *Mpesa) TestStatus(id string) (TestState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tests[id]
	if !ok {
		return TestState{}, false
	}
	return *t, true
}

// setTest records a test payment's outcome. It reports whether id is a test.
func (m *Mpesa) setTest(id, status, detail, receipt string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tests[id]
	if !ok {
		return false
	}
	t.Status, t.Detail = status, detail
	if receipt != "" {
		t.Receipt = receipt
	}
	return true
}

// ApplyQuery records what an STK status query said about a test payment, for
// the case where the callback never arrives (code "0" is success; anything
// else is a refusal such as 1032 "cancelled by user").
func (m *Mpesa) ApplyQuery(id, code, desc string) TestState {
	if code == "0" {
		m.setTest(id, "paid", "Daraja reports the payment as successful", "")
	} else if code != "" {
		m.setTest(id, "failed", desc, "")
	}
	t, _ := m.TestStatus(id)
	return t
}

// STKQuery asks Daraja for the state of an STK Push. A transaction still being
// processed is not an error: it returns code "" and a description.
func (m *Mpesa) STKQuery(ctx context.Context, checkoutID string) (code, desc string, err error) {
	tok, err := m.token(ctx)
	if err != nil {
		return "", "", err
	}
	c := m.conf()
	ts := m.now().In(eat).Format("20060102150405")
	b, _ := json.Marshal(map[string]any{"BusinessShortCode": c.Shortcode, "Password": stkPassword(c.Shortcode, c.Passkey, ts),
		"Timestamp": ts, "CheckoutRequestID": checkoutID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/mpesa/stkpushquery/v1/query", bytes.NewReader(b))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("stk query: %s", m.scrub(err.Error()))
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	// Daraja sends the codes as strings in some answers and as numbers in
	// others, so they are read raw (like the other fields).
	var r struct {
		ResponseCode        json.RawMessage `json:"ResponseCode"`
		ResponseDescription string          `json:"ResponseDescription"`
		ResultCode          json.RawMessage `json:"ResultCode"`
		ResultDesc          string          `json:"ResultDesc"`
		ErrorCode           json.RawMessage `json:"errorCode"`
		ErrorMessage        string          `json:"errorMessage"`
	}
	_ = json.Unmarshal(rb, &r)
	resultCode, responseCode, errorCode := rawString(r.ResultCode), rawString(r.ResponseCode), rawString(r.ErrorCode)
	switch {
	case resultCode != "" && resultCode != "null":
		return resultCode, m.scrub(r.ResultDesc), nil
	case errorCode == "500.001.1001": // Daraja's answer while the customer has not responded yet
		return "", "the transaction is still being processed", nil
	case resp.StatusCode != 200 || responseCode != "0":
		msg := r.ErrorMessage
		if msg == "" {
			msg = r.ResponseDescription
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return "", "", fmt.Errorf("stk query: %s", m.scrub(msg))
	}
	return "", m.scrub(r.ResponseDescription), nil
}

// RegisterC2B registers the validation and confirmation URLs (path-token form)
// for the C2B shortcode, response type Completed. It returns Daraja's
// description. Production refuses a second registration until the old URLs
// are deleted on the Daraja portal.
func (m *Mpesa) RegisterC2B(ctx context.Context) (string, error) {
	if !m.Enabled() {
		return "", errors.New("M-Pesa is not enabled: save the key, secret, shortcode, passkey and the public URL first")
	}
	c := m.conf()
	if c.CallbackToken == "" {
		return "", errors.New("a callback token is needed (save the M-Pesa settings once and one is generated)")
	}
	if !strings.HasPrefix(c.PublicBaseURL, "https://") {
		return "", errors.New("Daraja only accepts https callback URLs: set the public URL to https://...")
	}
	tok, err := m.token(ctx)
	if err != nil {
		return "", err
	}
	short := c.C2BShortcode
	if short == "" {
		short = c.Shortcode
	}
	b, _ := json.Marshal(map[string]string{"ShortCode": short, "ResponseType": "Completed",
		"ConfirmationURL": m.CallbackPathURL("c2b/confirm"), "ValidationURL": m.CallbackPathURL("c2b/validate")})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/mpesa/c2b/v2/registerurl", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("register urls: %s", m.scrub(err.Error()))
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		ResponseCode        json.RawMessage `json:"ResponseCode"`
		ResponseDescription string          `json:"ResponseDescription"`
		ErrorMessage        string          `json:"errorMessage"`
	}
	_ = json.Unmarshal(rb, &r)
	if resp.StatusCode == 200 && rawString(r.ResponseCode) == "0" {
		if r.ResponseDescription == "" {
			r.ResponseDescription = "Success"
		}
		return m.scrub(r.ResponseDescription), nil
	}
	msg := r.ErrorMessage
	if msg == "" {
		msg = r.ResponseDescription
	}
	if msg == "" {
		msg = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	return "", fmt.Errorf("register urls rejected: %s", m.scrub(msg))
}

// ---- STK callback -------------------------------------------------------

type stkCallback struct {
	Body struct {
		StkCallback struct {
			MerchantRequestID string `json:"MerchantRequestID"`
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResultCode        int    `json:"ResultCode"`
			ResultDesc        string `json:"ResultDesc"`
			CallbackMetadata  struct {
				Item []struct {
					Name  string          `json:"Name"`
					Value json.RawMessage `json:"Value"`
				} `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

func rawString(r json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(r)), `"`)
	return s
}

// SetSpool makes callbacks survive a database outage: a body whose payment
// could not be saved is written to the spool and applied later by Replay.
func (m *Mpesa) SetSpool(sp *Spool) { m.spool = sp }

// Replay applies spooled callbacks. It returns how many were applied.
func (m *Mpesa) Replay(ctx context.Context) (int, error) {
	if m.spool == nil {
		return 0, nil
	}
	return m.spool.Replay(func(kind string, body []byte) error {
		switch kind {
		case "stk":
			return m.stk(ctx, body)
		case "c2b":
			_, err := m.c2b(ctx, body)
			return err
		case "verify":
			return m.processVerify(ctx, body)
		}
		return fmt.Errorf("unknown spool kind %q", kind)
	})
}

func (m *Mpesa) keep(kind string, body []byte, err error) bool {
	if err == nil || m.spool == nil || !IsPersistError(err) {
		return false
	}
	if perr := m.spool.Put(kind, body); perr != nil {
		m.log.Error("could not spool a callback: the payment must be entered by hand from the M-Pesa statement", "kind", kind, "err", perr)
		return false
	}
	m.log.Warn("database unavailable: callback spooled and will be applied later", "kind", kind)
	return true
}

// HandleSTKCallback processes Daraja's STK result. It is idempotent.
func (m *Mpesa) HandleSTKCallback(ctx context.Context, body []byte) error {
	err := m.stk(ctx, body)
	if m.keep("stk", body, err) {
		return nil
	}
	return err
}

func (m *Mpesa) stk(ctx context.Context, body []byte) error {
	var cb stkCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return fmt.Errorf("bad stk callback: %w", err)
	}
	k := cb.Body.StkCallback
	if m.stkTest(k.CheckoutRequestID, k.ResultCode, k.ResultDesc, k.CallbackMetadata.Item) {
		return nil
	}
	var order store.Order
	var found bool
	m.st.View(func(d *store.Data) { order, found = d.OrderByCheckoutID(k.CheckoutRequestID) })
	if k.ResultCode != 0 {
		if !found {
			m.log.Warn("stk failure for unknown checkout id", "checkout", k.CheckoutRequestID)
			return nil
		}
		if order.Status == store.OrderPaid {
			return nil // a failure after a success never undoes it
		}
		return m.c.FailOrder(ctx, order.ID, k.ResultDesc)
	}
	var receipt, phone, date string
	var amount float64
	for _, it := range k.CallbackMetadata.Item {
		switch it.Name {
		case "MpesaReceiptNumber":
			receipt = rawString(it.Value)
		case "Amount":
			amount, _ = strconv.ParseFloat(rawString(it.Value), 64)
		case "PhoneNumber":
			phone = rawString(it.Value)
		case "TransactionDate":
			date = rawString(it.Value)
		}
	}
	if receipt == "" {
		return errors.New("stk callback without receipt number")
	}
	inLedger := m.ledgerStatus(receipt)
	if found && order.Status == store.OrderPaid && strings.EqualFold(order.Receipt, receipt) {
		return nil // a repeated delivery
	}
	if !found && inLedger != "" && inLedger != store.PayApplied {
		return nil // already decided or waiting in Unmatched
	}
	// A receipt already credited needs no confirmation; the order is only closed.
	if !verifiedCtx(ctx) && m.confirmOn() && inLedger != store.PayApplied {
		return m.queueVerify(ctx, "stk", receipt, int(math.Floor(amount)), body)
	}
	paidAt := m.transTime(date)
	if !found {
		return m.stkUnknownCheckout(ctx, k.CheckoutRequestID, receipt, phone, int(math.Floor(amount)), paidAt, inLedger == store.PayApplied)
	}
	if order.Status == store.OrderPaid {
		return m.orderAlreadyPaid(ctx, order, "mpesa-stk", receipt, int(math.Floor(amount)), phone, paidAt)
	}
	if amount < float64(order.AmountKES) {
		m.log.Warn("stk amount below order amount", "order", order.ID, "paid", amount, "expected", order.AmountKES)
		if err := m.unmatched(billing.UnmatchedInput{Key: receipt, Method: "mpesa-stk", AmountKES: int(math.Floor(amount)),
			Phone: phone, BillRef: order.ID, AccountID: order.AccountID, Ord: order.ID, Note: "STK amount below the package price", PaidAt: paidAt}); err != nil {
			return err
		}
		return m.c.FailOrder(ctx, order.ID, "amount paid is less than the price")
	}
	return m.completeOrder(ctx, order, receipt, paidAt, true)
}

// completeOrder credits an order. A receipt that was already decided (refunded
// or ignored) is not credited: that leaves the order unpaid, so the owners are
// told.
func (m *Mpesa) completeOrder(ctx context.Context, order store.Order, receipt string, paidAt time.Time, fromSTK bool) error {
	var err error
	if fromSTK {
		err = m.c.CompleteOrderSTK(ctx, order.ID, receipt, paidAt)
	} else {
		err = m.c.CompleteOrderAt(ctx, order.ID, receipt, paidAt)
	}
	if errors.Is(err, billing.ErrBadState) {
		msg := "M-Pesa payment " + strings.ToUpper(receipt) + " (KES " + strconv.Itoa(order.AmountKES) + ") was NOT credited to order " + order.ID + ": " + err.Error() + ". Check Payments."
		m.log.Error(msg)
		m.mu.Lock()
		al := m.alert
		m.mu.Unlock()
		if al != nil {
			al(msg)
		}
		return nil // nothing more a retry could do
	}
	return err
}

// firstAlert is true once per held receipt (per process), so a redelivery does
// not alert the owners again.
func (m *Mpesa) firstAlert(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.alerted == nil || len(m.alerted) > 2000 {
		m.alerted = map[string]bool{}
	}
	if m.alerted[key] {
		return false
	}
	m.alerted[key] = true
	return true
}

func (m *Mpesa) tell(msg string) {
	m.log.Warn(msg)
	m.mu.Lock()
	al := m.alert
	m.mu.Unlock()
	if al != nil {
		al(msg)
	}
}

// SetAlert sets what tells the owners about a payment that needs a person.
func (m *Mpesa) SetAlert(f func(msg string)) {
	m.mu.Lock()
	m.alert = f
	m.mu.Unlock()
}

// ledgerStatus is the status of a receipt in the ledger, "" if it is not there.
func (m *Mpesa) ledgerStatus(receipt string) (status string) {
	m.st.View(func(d *store.Data) { status = d.Payments[strings.ToUpper(strings.TrimSpace(receipt))].Status })
	return status
}

// unmatched records a payment in the Unmatched queue. Only a database failure
// is returned (so the callback is spooled); anything else is logged.
func (m *Mpesa) unmatched(in billing.UnmatchedInput) error {
	if _, err := m.c.RecordUnmatched(in); err != nil {
		m.log.Error("could not store unmatched payment", "receipt", in.Key, "err", err)
		if IsPersistError(err) {
			return err
		}
	}
	return nil
}

// matchWindow is how far back an order may have been created for a payment with
// an unknown checkout id to be matched to it by phone and amount.
const matchWindow = time.Hour

// stkUnknownCheckout takes a successful STK result whose CheckoutRequestID no
// order carries: the push was accepted by Daraja but we never stored its id
// (timeout, restart, a callback faster than the store). The money is never
// dropped: an unambiguous recent order for the same phone and amount is
// credited; anything else goes to Unmatched with the phone and receipt.
func (m *Mpesa) stkUnknownCheckout(ctx context.Context, checkout, receipt, phone string, amount int, paidAt time.Time, inLedger bool) error {
	var notAfter time.Time
	if !paidAt.IsZero() {
		notAfter = paidAt.Add(time.Minute)
	}
	cand := m.openOrders("", phone, amount, true, notAfter)
	if len(cand) == 1 {
		match := cand[0]
		m.log.Warn("stk result for an unknown checkout id matched to the only recent order for this phone and amount", "order", match.ID, "checkout", checkout)
		if err := m.c.SetCheckoutID(match.ID, checkout); err != nil && IsPersistError(err) {
			return err
		}
		return m.completeOrder(ctx, match, receipt, paidAt, true)
	}
	if inLedger {
		return nil // credited by another route already; no order to close
	}
	note := "STK payment for a checkout id this system never stored (the prompt was sent but the answer was lost)"
	if len(cand) > 1 {
		note = "STK payment for an unknown checkout id; " + strconv.Itoa(len(cand)) + " recent orders have the same phone and amount"
	}
	m.log.Warn("stk success for an unknown checkout id: recorded as unmatched", "checkout", checkout, "receipt", receipt, "amount", amount)
	return m.unmatched(billing.UnmatchedInput{Key: receipt, Method: "mpesa-stk", AmountKES: amount, Phone: phone, Note: note, PaidAt: paidAt})
}

// openOrders lists the unpaid mpesa orders of the last hour that could be the
// one a payment belongs to: by account (accountID) or by phone, with this
// amount. Only orders with no stored checkout id count when noCheckout is set.
// Pending and expired orders are preferred; a failed one is used only when
// there is no other, and never one that failed because Daraja refused the push.
func (m *Mpesa) openOrders(accountID, phone string, amount int, noCheckout bool, notAfter time.Time) []store.Order {
	var live, failed []store.Order
	now := m.now()
	m.st.View(func(d *store.Data) {
		for _, o := range d.Orders {
			if o.Method != "mpesa" || o.Status == store.OrderPaid || o.AmountKES != amount || now.Sub(o.CreatedAt) > matchWindow || o.CreatedAt.After(now.Add(time.Minute)) {
				continue
			}
			if (accountID != "" && o.AccountID != accountID) || (phone != "" && o.Phone != phone) || (accountID == "" && phone == "") {
				continue
			}
			if noCheckout && o.CheckoutRequestID != "" {
				continue
			}
			if !notAfter.IsZero() && o.CreatedAt.After(notAfter) {
				continue // created after the money was paid: it cannot be this payment's order
			}
			switch {
			case o.Status == store.OrderPending || o.Status == store.OrderExpired:
				live = append(live, o)
			case o.Status == store.OrderFailed && o.Reason != billing.ReasonPromptNotSent:
				failed = append(failed, o)
			}
		}
	})
	if len(live) > 0 {
		return live
	}
	return failed
}

// orderAlreadyPaid handles a payment confirmation for an order that is paid.
// The same receipt is a repeated delivery. A receipt for an order an admin
// marked paid by hand is the real confirmation of that money and replaces the
// manual receipt without crediting again. Any other receipt is a second payment
// that is kept, visibly, in Unmatched.
func (m *Mpesa) orderAlreadyPaid(ctx context.Context, order store.Order, method, receipt string, amount int, phone string, paidAt time.Time) error {
	switch {
	case strings.EqualFold(order.Receipt, receipt):
		return nil
	case strings.HasPrefix(strings.ToUpper(order.Receipt), billing.ManualPrefix):
		return m.completeOrder(ctx, order, receipt, paidAt, false)
	}
	return m.unmatched(billing.UnmatchedInput{Key: receipt, Method: method, AmountKES: amount, Phone: phone, BillRef: order.ID,
		AccountID: order.AccountID, Ord: order.ID, PaidAt: paidAt,
		Note: "a second payment for an order that was already paid with " + order.Receipt + ": refund it or assign it"})
}

// transTime parses Daraja's transaction time (yyyymmddhhmmss, East Africa
// Time). A missing, malformed or implausible value (in the future, or more
// than 30 days old) gives the zero time, which means "use the arrival time".
func (m *Mpesa) transTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) != 14 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102150405", s, eat)
	now := m.now()
	if err != nil || t.After(now.Add(5*time.Minute)) || t.Before(now.Add(-30*24*time.Hour)) {
		return time.Time{}
	}
	return t
}

// stkTest handles the result of a Settings test payment. It reports whether
// the checkout id was one.
func (m *Mpesa) stkTest(id string, code int, desc string, items []struct {
	Name  string          `json:"Name"`
	Value json.RawMessage `json:"Value"`
}) bool {
	if _, ok := m.TestStatus(id); !ok {
		return false
	}
	if code != 0 {
		m.setTest(id, "failed", m.scrub(desc), "")
		return true
	}
	var receipt, phone string
	var amount float64
	for _, it := range items {
		switch it.Name {
		case "MpesaReceiptNumber":
			receipt = rawString(it.Value)
		case "Amount":
			amount, _ = strconv.ParseFloat(rawString(it.Value), 64)
		case "PhoneNumber":
			phone = rawString(it.Value)
		}
	}
	m.setTest(id, "paid", "the payment of KES "+strconv.Itoa(int(amount))+" went through", receipt)
	if receipt != "" {
		if _, err := m.c.RecordUnmatched(billing.UnmatchedInput{Key: receipt, Method: "mpesa-stk", AmountKES: int(math.Floor(amount)),
			Phone: phone, BillRef: FTNTestRef, Note: "settings test"}); err != nil {
			m.log.Warn("could not record the settings test payment", "err", err)
		}
	}
	return true
}

// ---- C2B ----------------------------------------------------------------

// C2BResponse is the acknowledgement Daraja expects.
type C2BResponse struct {
	ResultCode string `json:"ResultCode"`
	ResultDesc string `json:"ResultDesc"`
}

// C2BAccept / C2BReject are the standard replies.
var (
	C2BAccept = C2BResponse{"0", "Accepted"}
	C2BReject = C2BResponse{"C2B00012", "Rejected"}
)

type c2bPayload struct {
	TransactionType string          `json:"TransactionType"`
	TransID         string          `json:"TransID"`
	TransAmount     json.RawMessage `json:"TransAmount"`
	BillRefNumber   string          `json:"BillRefNumber"`
	MSISDN          string          `json:"MSISDN"`
	TransTime       string          `json:"TransTime"`
	FirstName       string          `json:"FirstName"`
	LastName        string          `json:"LastName"`
}

func parseC2B(body []byte) (c2bPayload, float64, error) {
	var p c2bPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return p, 0, err
	}
	amt, _ := strconv.ParseFloat(rawString(p.TransAmount), 64)
	p.BillRefNumber = NormalizeRef(p.BillRefNumber)
	return p, amt, nil
}

// NormalizeRef trims a typed account number, upper-cases it and removes spaces
// and dashes, so "abc 234" and "ABC-234" both find flat ABC234.
func NormalizeRef(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(strings.TrimSpace(s)))
}

// refKind tells what a BillRefNumber refers to: an account code (what
// customers type as the Paybill account number), an order id, or the reserved
// settings test reference.
//
// The test reference counts only while a Settings test payment is pending in
// memory (started less than testWindow ago) and the amount is the KES 1 the
// test sends: anyone can type FTNTEST as an account number, and without a
// test under way it is just an unknown reference.
func (m *Mpesa) refKind(ref string, amount float64) (kind string, order store.Order, acct store.Account) {
	if ref == FTNTestRef {
		if amount == 1 && m.testPending() {
			return "test", order, acct
		}
		return "", order, acct
	}
	lower := strings.ToLower(ref)
	m.st.View(func(d *store.Data) {
		if o, ok := d.Orders[lower]; ok {
			kind, order = "order", o
		} else if a, ok := d.AccountByCode(ref); ok {
			kind, acct = "account", a
		}
	})
	return
}

// HandleC2BValidation accepts a payment only if its account number is known.
func (m *Mpesa) HandleC2BValidation(body []byte) C2BResponse {
	p, amt, err := parseC2B(body)
	if err != nil {
		return C2BReject
	}
	kind, order, acct := m.refKind(p.BillRefNumber, amt)
	if kind == "" && m.conf().UnknownRef != "accept" {
		return C2BReject
	}
	// Money below the cheapest package can only end up in Unmatched: say no now.
	site := acct.SiteID
	if kind == "order" {
		site = order.SiteID
	}
	if kind == "account" || kind == "order" {
		if low, ok := m.cheapest(site); ok && amt < float64(low) {
			return C2BReject
		}
	}
	return C2BAccept
}

// cheapest is the price of the cheapest enabled package at a building.
func (m *Mpesa) cheapest(siteID string) (low int, ok bool) {
	m.st.View(func(d *store.Data) {
		for _, p := range d.EffectivePackages(siteID) {
			if !p.Disabled && !p.Deleted && p.PriceKES > 0 && (!ok || p.PriceKES < low) {
				low, ok = p.PriceKES, true
			}
		}
	})
	return
}

// HandleC2BConfirmation records a finished C2B payment. Always acknowledges.
// Every payment lands in the ledger under its TransID, so a repeated delivery
// (or the STK result for the same payment) changes nothing.
func (m *Mpesa) HandleC2BConfirmation(ctx context.Context, body []byte) C2BResponse {
	resp, err := m.c2b(ctx, body)
	m.keep("c2b", body, err)
	return resp
}

// c2b does the work; a non-nil error means the database did not take the payment.
func (m *Mpesa) c2b(ctx context.Context, body []byte) (C2BResponse, error) {
	p, amt, err := parseC2B(body)
	if err != nil || p.TransID == "" {
		m.log.Warn("bad c2b confirmation")
		return C2BAccept, nil
	}
	amount := int(math.Floor(amt))
	if !verifiedCtx(ctx) && p.BillRefNumber != FTNTestRef && m.confirmOn() {
		var already bool
		m.st.View(func(d *store.Data) { _, already = d.Payments[strings.ToUpper(strings.TrimSpace(p.TransID))] })
		if already {
			return C2BAccept, nil // a repeated delivery of a receipt that is already in the ledger
		}
		return C2BAccept, m.queueVerify(ctx, "c2b", p.TransID, amount, body)
	}
	name := strings.TrimSpace(p.FirstName + " " + p.LastName)
	paidAt := m.transTime(p.TransTime) // when the customer paid, not when we heard of it
	unmatched := func(accountID, note string) error {
		return m.unmatched(billing.UnmatchedInput{Key: p.TransID, Method: "mpesa-c2b", AmountKES: amount,
			Phone: p.MSISDN, Name: name, BillRef: p.BillRefNumber, AccountID: accountID, Note: note, PaidAt: paidAt})
	}
	kind, order, acct := m.refKind(p.BillRefNumber, amt)
	if kind == "account" && !paidAt.IsZero() && m.ledgerStatus(p.TransID) == "" {
		if _, dup := m.c.PossibleManualDuplicate(acct.ID, amount, paidAt); dup {
			kind = "held" // may be the money of a Mark paid: hold it, do not give it to another order
		}
	}
	if kind == "account" && !paidAt.IsZero() && m.ledgerStatus(p.TransID) == "" {
		// The STK push also reports to the Paybill with the account code. If the
		// flat has exactly one unpaid M-Pesa order for this amount, this is its
		// payment: complete that order, so the customer's wait page closes too.
		if open := m.openOrders(acct.ID, "", amount, false, paidAt.Add(time.Minute)); len(open) == 1 {
			kind, order = "order", open[0]
		}
	}
	if kind == "held" {
		kind = "account"
	}
	switch kind {
	case "order": // an STK push also triggers C2B; the reference may be the order id
		if order.Status == store.OrderPaid {
			return C2BAccept, m.orderAlreadyPaid(ctx, order, "mpesa-c2b", p.TransID, amount, p.MSISDN, paidAt)
		}
		if amount < order.AmountKES {
			return C2BAccept, unmatched(order.AccountID, "amount below the package price")
		}
		if err := m.completeOrder(ctx, order, p.TransID, paidAt, false); err != nil {
			if IsPersistError(err) {
				return C2BAccept, err
			}
			return C2BAccept, unmatched(order.AccountID, "order completion failed: "+err.Error())
		}
	case "account":
		pkg, ok := m.c.PackageForAmount(acct.SiteID, amount)
		if !ok {
			return C2BAccept, unmatched(acct.ID, "no package costs KES "+strconv.Itoa(amount))
		}
		if held, _, err := m.c.Credit(ctx, billing.CreditInput{Key: p.TransID, Method: "mpesa-c2b", AccountID: acct.ID, PackageID: pkg.ID,
			AmountKES: amount, Phone: p.MSISDN, Name: name, BillRef: p.BillRefNumber, PaidAt: paidAt}); err == nil {
			if held.Status == store.PayUnmatched && strings.HasPrefix(held.Note, "possible duplicate of manual payment") && m.firstAlert(held.Key) {
				m.tell("M-Pesa payment " + held.Key + " (KES " + strconv.Itoa(amount) + ") for flat " + acct.Flat + " is held, not credited: " + held.Note + " Open Unmatched payments.")
			}
		} else {
			if IsPersistError(err) {
				return C2BAccept, err
			}
			return C2BAccept, unmatched(acct.ID, "could not apply: "+err.Error())
		}
	case "test":
		return C2BAccept, unmatched("", "settings test")
	default:
		return C2BAccept, unmatched("", "unknown account number")
	}
	return C2BAccept, nil
}

// LegacyQueryDeadline returns when the old STK callback form stops working. The
// first run of this version writes now+LegacyQueryWindow to dir/stk-query-until
// and later runs read it back, so a restart does not extend the window.
func LegacyQueryDeadline(dir string, now time.Time, warn func(msg string, args ...any)) time.Time {
	path := filepath.Join(dir, "stk-query-until")
	if b, err := os.ReadFile(path); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
			return t
		}
	}
	t := now.Add(LegacyQueryWindow)
	_ = os.MkdirAll(dir, 0o700)
	if err := store.WriteFileAtomic(path, []byte(t.UTC().Format(time.RFC3339)+"\n")); err != nil {
		// Without the file every restart would start a new 30 days. Use a fixed
		// end instead, so a read-only data directory cannot keep the old form alive.
		if warn != nil {
			warn("cannot save the end of the old STK callback window (" + path + "): using the fixed date " + LegacyQueryFallback().Format("2006-01-02"))
		}
		return LegacyQueryFallback()
	}
	return t
}

// LegacyQueryFallback is when the old STK callback form ends if the data
// directory cannot hold the deadline file: 30 days after this binary was built
// (the VCS time stamped into it, else the executable's modification time).
func LegacyQueryFallback() time.Time { return legacyBuildTime().Add(LegacyQueryWindow) }

// legacyBuildTime is replaced by tests.
var legacyBuildTime = func() time.Time {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.time" {
				if t, err := time.Parse(time.RFC3339, kv.Value); err == nil {
					return t
				}
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			return fi.ModTime()
		}
	}
	return time.Time{} // unknown: the old form ends at once, which is the safe side
}
