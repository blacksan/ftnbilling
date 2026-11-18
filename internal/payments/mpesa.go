package payments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

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
}

func NewMpesa(cfg MpesaConfig, c Completer, st *store.Store, log *slog.Logger) *Mpesa {
	return &Mpesa{cfg: cfg, c: c, st: st, log: log, now: time.Now, hc: &http.Client{Timeout: 20 * time.Second}}
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
	id, err := m.stkPush(ctx, o.Phone, o.AmountKES, acct.Code)
	if err != nil {
		return err
	}
	return m.c.SetCheckoutID(o.ID, id)
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
		"CallBackURL":       m.CallbackURL("/pay/cb/stk"),
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
		return "", fmt.Errorf("stk push: %s", m.scrub(err.Error()))
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		CheckoutRequestID   string          `json:"CheckoutRequestID"`
		ResponseCode        json.RawMessage `json:"ResponseCode"`
		ResponseDescription string          `json:"ResponseDescription"`
		ErrorMessage        string          `json:"errorMessage"`
	}
	_ = json.Unmarshal(rb, &r)
	if resp.StatusCode != 200 || rawString(r.ResponseCode) != "0" || r.CheckoutRequestID == "" {
		msg := r.ErrorMessage
		if msg == "" {
			msg = r.ResponseDescription
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
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
	if !found {
		m.log.Warn("stk callback for unknown checkout id", "checkout", k.CheckoutRequestID)
		return nil
	}
	if order.Status == store.OrderPaid {
		return nil
	}
	if k.ResultCode != 0 {
		return m.c.FailOrder(ctx, order.ID, k.ResultDesc)
	}
	var receipt, phone string
	var amount float64
	for _, it := range k.CallbackMetadata.Item {
		switch it.Name {
		case "MpesaReceiptNumber":
			receipt = rawString(it.Value)
		case "Amount":
			amount, _ = strconv.ParseFloat(rawString(it.Value), 64)
		case "PhoneNumber":
			phone = rawString(it.Value)
		}
	}
	if receipt == "" {
		return errors.New("stk callback without receipt number")
	}
	if amount < float64(order.AmountKES) {
		m.log.Warn("stk amount below order amount", "order", order.ID, "paid", amount, "expected", order.AmountKES)
		_, _ = m.c.RecordUnmatched(billing.UnmatchedInput{Key: receipt, Method: "mpesa-stk", AmountKES: int(math.Floor(amount)),
			Phone: phone, BillRef: order.ID, AccountID: order.AccountID, Ord: order.ID, Note: "STK amount below the package price"})
		return m.c.FailOrder(ctx, order.ID, "amount paid is less than the price")
	}
	return m.c.CompleteOrder(ctx, order.ID, receipt)
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
func (m *Mpesa) refKind(ref string) (kind string, order store.Order, acct store.Account) {
	if ref == FTNTestRef {
		return "test", order, acct
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
	p, _, err := parseC2B(body)
	if err != nil {
		return C2BReject
	}
	if kind, _, _ := m.refKind(p.BillRefNumber); kind == "" && m.conf().UnknownRef != "accept" {
		return C2BReject
	}
	return C2BAccept
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
	name := strings.TrimSpace(p.FirstName + " " + p.LastName)
	unmatched := func(accountID, note string) error {
		if _, err := m.c.RecordUnmatched(billing.UnmatchedInput{Key: p.TransID, Method: "mpesa-c2b", AmountKES: amount,
			Phone: p.MSISDN, Name: name, BillRef: p.BillRefNumber, AccountID: accountID, Note: note, PaidAt: m.now()}); err != nil {
			m.log.Error("could not store unmatched payment", "trans", p.TransID, "err", err)
			if IsPersistError(err) {
				return err
			}
		}
		return nil
	}
	kind, order, acct := m.refKind(p.BillRefNumber)
	switch kind {
	case "order": // an STK push also triggers C2B; the reference may be the order id
		if order.Status == store.OrderPaid {
			return C2BAccept, nil
		}
		if amount < order.AmountKES {
			return C2BAccept, unmatched(order.AccountID, "amount below the package price")
		}
		if err := m.c.CompleteOrder(ctx, order.ID, p.TransID); err != nil {
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
		if _, _, err := m.c.Credit(ctx, billing.CreditInput{Key: p.TransID, Method: "mpesa-c2b", AccountID: acct.ID, PackageID: pkg.ID,
			AmountKES: amount, Phone: p.MSISDN, Name: name, BillRef: p.BillRefNumber}); err != nil {
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
