package payments

import (
	"bytes"
	"context"
	"crypto/rand"
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
	Passkey       string
	PublicBaseURL string
	CallbackToken string // optional shared secret appended as ?t=
	Enabled       bool
}

// Mpesa is the Safaricom Daraja provider (STK Push + C2B).
type Mpesa struct {
	cfg   MpesaConfig
	c     Completer
	st    *store.Store
	log   *slog.Logger
	hc    *http.Client
	now   func() time.Time
	mu    sync.Mutex
	tok   string
	tokX  time.Time
	spool *Spool
}

func NewMpesa(cfg MpesaConfig, c Completer, st *store.Store, log *slog.Logger) *Mpesa {
	return &Mpesa{cfg: cfg, c: c, st: st, log: log, now: time.Now, hc: &http.Client{Timeout: 20 * time.Second}}
}

// SetClock overrides the clock (tests).
func (m *Mpesa) SetClock(f func() time.Time) { m.now = f }

func (m *Mpesa) Name() string  { return "mpesa" }
func (m *Mpesa) Enabled() bool { return m.cfg.Enabled }

// CallbackURL builds a public callback URL for a path under /pay/cb/.
func (m *Mpesa) CallbackURL(path string) string {
	u := m.cfg.PublicBaseURL + path
	if m.cfg.CallbackToken != "" {
		u += "?t=" + url.QueryEscape(m.cfg.CallbackToken)
	}
	return u
}

// token returns a cached OAuth token, refreshing 30s before expiry.
func (m *Mpesa) token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok != "" && m.now().Before(m.tokX) {
		return m.tok, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.BaseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(m.cfg.Key, m.cfg.Secret)
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("daraja oauth: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("daraja oauth: HTTP %d", resp.StatusCode)
	}
	var r struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.AccessToken == "" {
		return "", errors.New("daraja oauth: bad response")
	}
	secs, _ := strconv.Atoi(r.ExpiresIn)
	if secs <= 0 {
		secs = 3000
	}
	m.tok = r.AccessToken
	m.tokX = m.now().Add(time.Duration(secs-30) * time.Second)
	return m.tok, nil
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
	tok, err := m.token(ctx)
	if err != nil {
		return err
	}
	ts := m.now().In(eat).Format("20060102150405")
	body := map[string]any{
		"BusinessShortCode": m.cfg.Shortcode,
		"Password":          stkPassword(m.cfg.Shortcode, m.cfg.Passkey, ts),
		"Timestamp":         ts,
		"TransactionType":   "CustomerPayBillOnline",
		"Amount":            o.AmountKES,
		"PartyA":            o.Phone,
		"PartyB":            m.cfg.Shortcode,
		"PhoneNumber":       o.Phone,
		"CallBackURL":       m.CallbackURL("/pay/cb/stk"),
		"AccountReference":  acct.Code,
		"TransactionDesc":   "Internet",
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/mpesa/stkpush/v1/processrequest", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("stk push: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		CheckoutRequestID   string `json:"CheckoutRequestID"`
		ResponseCode        string `json:"ResponseCode"`
		ResponseDescription string `json:"ResponseDescription"`
		ErrorMessage        string `json:"errorMessage"`
	}
	_ = json.Unmarshal(rb, &r)
	if resp.StatusCode != 200 || r.ResponseCode != "0" || r.CheckoutRequestID == "" {
		msg := r.ErrorMessage
		if msg == "" {
			msg = r.ResponseDescription
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return fmt.Errorf("stk push rejected: %s", msg)
	}
	return m.c.SetCheckoutID(o.ID, r.CheckoutRequestID)
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
	p.BillRefNumber = strings.ToUpper(strings.TrimSpace(p.BillRefNumber))
	return p, amt, nil
}

// refKind tells what a BillRefNumber refers to: an account code (what
// customers type as the Paybill account number) or an order id.
func (m *Mpesa) refKind(ref string) (kind string, order store.Order, acct store.Account) {
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
	if kind, _, _ := m.refKind(p.BillRefNumber); kind == "" {
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
	default:
		return C2BAccept, unmatched("", "unknown account number")
	}
	return C2BAccept, nil
}
