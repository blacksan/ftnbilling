package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// Receipt confirmation (security review M-2).
//
// Daraja's callbacks are unsigned: anyone who learns the callback token can post
// a "payment". So before a C2B confirmation or an STK result credits anything,
// the receipt is checked with Daraja's Transaction Status API. That API is
// asynchronous: we send the query, and Daraja posts the answer to our result URL
// a moment later. The callback body is parked in the spool (so a restart loses
// nothing), the query is sent with retries, and the payment is credited only
// when a result for OUR query says the receipt exists and is completed. A
// receipt that is not confirmed within confirmWindow goes to the Unmatched
// payments list with the reason, for a person to look at.
//
// The result callback itself is also unsigned, so it is only believed when it
// carries the ConversationID (or OriginatorConversationID) Daraja gave us when
// we sent the query: a forger holding the callback token does not have it.

const confirmWindow = 15 * time.Minute

// confirmGap is how long to wait for an answer before asking again, by number of queries sent so far.
var confirmGap = []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}

// maxPendingVerify bounds the spool a flood of forged callbacks can fill.
const maxPendingVerify = 500

type verifiedKey struct{}

func verifiedCtx(ctx context.Context) bool { return ctx.Value(verifiedKey{}) != nil }

// pending is the spooled wrapper around a callback that waits for confirmation.
type pending struct {
	Kind    string          `json:"kind"` // stk | c2b
	Receipt string          `json:"receipt"`
	Amount  int             `json:"amount"`
	First   time.Time       `json:"first"`
	Body    json.RawMessage `json:"body"`
}

// verifyState is what is known in memory about one receipt being checked.
type verifyState struct {
	amount    int // what the parked callback claimed
	queries   int
	lastQuery time.Time
	confirmed bool
	lastErr   string
}

// confirmOn reports whether callbacks are held for confirmation.
func (m *Mpesa) confirmOn() bool {
	c := m.conf()
	return c.ConfirmReceipts && c.Initiator != "" && c.SecurityCredential != "" && m.spool != nil
}

func verifyKey(kind, receipt string) string {
	return kind + ":" + strings.ToUpper(strings.TrimSpace(receipt))
}

// queueVerify parks a callback until its receipt is confirmed and starts the check.
func (m *Mpesa) queueVerify(ctx context.Context, kind, receipt string, amount int, body []byte) error {
	key := verifyKey(kind, receipt)
	m.vmu.Lock()
	if _, dup := m.verify[key]; dup {
		m.vmu.Unlock()
		return nil // already waiting: Daraja redelivered
	}
	if m.verify == nil {
		m.verify = map[string]*verifyState{}
		m.convs = map[string]string{}
	}
	if len(m.verify) >= maxPendingVerify {
		m.vmu.Unlock()
		m.log.Error("too many receipts waiting for confirmation: callback dropped; enter the payment by hand if it is real", "receipt", receipt)
		return nil
	}
	m.verify[key] = &verifyState{amount: amount}
	m.vmu.Unlock()
	w, _ := json.Marshal(pending{Kind: kind, Receipt: receipt, Amount: amount, First: m.now(), Body: body})
	if err := m.spool.Put("verify", w); err != nil {
		m.vmu.Lock()
		delete(m.verify, key)
		m.vmu.Unlock()
		return err
	}
	m.log.Info("payment held until Daraja confirms the receipt", "kind", kind, "receipt", receipt, "amount", amount)
	m.kick(ctx)
	return nil
}

// kick runs one pass over the spool in the background (the result callback and
// every new receipt call it; the minute ticker in main covers the retries).
func (m *Mpesa) kick(ctx context.Context) {
	bg := context.WithoutCancel(ctx)
	pass := func() {
		if _, err := m.Replay(bg); err != nil {
			m.log.Warn("spool replay", "err", err)
		}
	}
	m.bgMu.Lock()
	if m.closed {
		m.bgMu.Unlock()
		return
	}
	m.bg.Add(1)
	m.bgMu.Unlock()
	go func() {
		defer m.bg.Done()
		pass()
		// A pass that was already running ignores a second request, so look once more in a second.
		select {
		case <-m.stop:
			return
		case <-time.After(time.Second):
		}
		pass()
	}()
}

// Close stops the background receipt checks and waits for them. Callbacks that
// are still parked stay in the spool for the next run.
func (m *Mpesa) Close() {
	m.bgMu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stop)
	}
	m.bgMu.Unlock()
	m.bg.Wait()
}

// processVerify handles one parked callback. It returns ErrKeep while the answer is awaited.
func (m *Mpesa) processVerify(ctx context.Context, raw []byte) error {
	var p pending
	if err := json.Unmarshal(raw, &p); err != nil || p.Receipt == "" {
		return fmt.Errorf("bad parked callback: %v", err)
	}
	key := verifyKey(p.Kind, p.Receipt)
	m.vmu.Lock()
	if m.verify == nil {
		m.verify = map[string]*verifyState{}
		m.convs = map[string]string{}
	}
	st := m.verify[key]
	if st == nil { // after a restart
		st = &verifyState{amount: p.Amount}
		m.verify[key] = st
	}
	confirmed, lastErr, queries, last := st.confirmed, st.lastErr, st.queries, st.lastQuery
	m.vmu.Unlock()
	done := func() {
		m.vmu.Lock()
		delete(m.verify, key)
		m.vmu.Unlock()
	}
	vctx := context.WithValue(ctx, verifiedKey{}, true)
	switch {
	case confirmed:
		var err error
		if p.Kind == "stk" {
			err = m.stk(vctx, p.Body)
		} else {
			_, err = m.c2b(vctx, p.Body)
		}
		if err == nil {
			done()
		}
		return err
	case !m.confirmOn():
		// Confirmation was switched off while this waited: treat it as a normal callback.
		var err error
		if p.Kind == "stk" {
			err = m.stk(vctx, p.Body)
		} else {
			_, err = m.c2b(vctx, p.Body)
		}
		if err == nil {
			done()
		}
		return err
	case m.now().Sub(p.First) >= confirmWindow:
		reason := "Daraja did not confirm this receipt within " + confirmWindow.String()
		if lastErr != "" {
			reason += " (" + lastErr + ")"
		}
		err := m.rejectUnconfirmed(ctx, p, reason)
		if err == nil {
			done()
		}
		return err
	}
	gap := time.Duration(0)
	if queries > 0 {
		gap = confirmGap[min(queries-1, len(confirmGap)-1)]
	}
	if queries == 0 || m.now().Sub(last) >= gap {
		if err := m.queryTxStatus(ctx, key, p.Receipt); err != nil {
			m.vmu.Lock()
			st.lastErr = err.Error()
			m.vmu.Unlock()
			m.log.Warn("receipt check could not be sent", "receipt", p.Receipt, "err", err)
		}
	}
	return ErrKeep
}

// rejectUnconfirmed puts an unconfirmed payment in the Unmatched list with the reason.
func (m *Mpesa) rejectUnconfirmed(ctx context.Context, p pending, reason string) error {
	note := "not credited: " + reason
	switch p.Kind {
	case "c2b":
		pl, _, _ := parseC2B(p.Body)
		_, err := m.c.RecordUnmatched(billing.UnmatchedInput{Key: p.Receipt, Method: "mpesa-c2b", AmountKES: p.Amount,
			Phone: pl.MSISDN, Name: strings.TrimSpace(pl.FirstName + " " + pl.LastName), BillRef: pl.BillRefNumber, Note: note, PaidAt: m.now()})
		if err != nil && IsPersistError(err) {
			return err
		}
	case "stk":
		var cb stkCallback
		_ = json.Unmarshal(p.Body, &cb)
		var order store.Order
		var found bool
		m.st.View(func(d *store.Data) { order, found = d.OrderByCheckoutID(cb.Body.StkCallback.CheckoutRequestID) })
		in := billing.UnmatchedInput{Key: p.Receipt, Method: "mpesa-stk", AmountKES: p.Amount, Note: note, PaidAt: m.now()}
		if found {
			in.BillRef, in.AccountID, in.Ord = order.ID, order.AccountID, order.ID
		}
		if _, err := m.c.RecordUnmatched(in); err != nil && IsPersistError(err) {
			return err
		}
		if found && order.Status != store.OrderPaid {
			if err := m.c.FailOrder(ctx, order.ID, "the payment could not be confirmed with M-Pesa"); err != nil && IsPersistError(err) {
				return err
			}
		}
	}
	m.log.Warn("payment not credited: receipt not confirmed", "kind", p.Kind, "receipt", p.Receipt, "reason", reason)
	return nil
}

// queryTxStatus asks Daraja about a receipt. The answer arrives later at the result URL.
func (m *Mpesa) queryTxStatus(ctx context.Context, key, receipt string) error {
	m.vmu.Lock()
	if st := m.verify[key]; st != nil {
		st.queries++
		st.lastQuery = m.now()
	}
	m.vmu.Unlock()
	ids, _, err := m.sendTxStatus(ctx, receipt)
	if err != nil {
		return err
	}
	m.rememberConvs(key, ids)
	return nil
}

// rememberConvs lets the result callback find the check it belongs to.
func (m *Mpesa) rememberConvs(key string, ids []string) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	if m.convs == nil {
		m.convs = map[string]string{}
	}
	if len(m.convs) > 4*maxPendingVerify {
		m.convs = map[string]string{}
	}
	for _, id := range ids {
		if id != "" {
			m.convs[id] = key
		}
	}
}

// sendTxStatus posts one Transaction Status query and returns the conversation
// ids Daraja gave (the result callback carries one of them) and Daraja's own
// synchronous reply, scrubbed.
func (m *Mpesa) sendTxStatus(ctx context.Context, receipt string) (ids []string, reply string, err error) {
	tok, err := m.token(ctx)
	if err != nil {
		return nil, "", err
	}
	c := m.conf()
	party := c.C2BShortcode
	if party == "" {
		party = c.Shortcode
	}
	body, _ := json.Marshal(map[string]string{
		"Initiator": c.Initiator, "SecurityCredential": c.SecurityCredential, "CommandID": "TransactionStatusQuery",
		"TransactionID": strings.ToUpper(strings.TrimSpace(receipt)), "PartyA": party, "IdentifierType": "4",
		"ResultURL": m.CallbackPathURL("txstatus/result"), "QueueTimeOutURL": m.CallbackPathURL("txstatus/timeout"),
		"Remarks": "receipt check", "Occasion": "receipt check",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/mpesa/transactionstatus/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("transaction status: %s", m.scrub(err.Error()))
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	reply = m.scrub(string(rb))
	var r struct {
		ConversationID           string          `json:"ConversationID"`
		OriginatorConversationID string          `json:"OriginatorConversationID"`
		ResponseCode             json.RawMessage `json:"ResponseCode"`
		ResponseDescription      string          `json:"ResponseDescription"`
		ErrorMessage             string          `json:"errorMessage"`
	}
	_ = json.Unmarshal(rb, &r)
	if resp.StatusCode != 200 || rawString(r.ResponseCode) != "0" || (r.ConversationID == "" && r.OriginatorConversationID == "") {
		msg := r.ErrorMessage
		if msg == "" {
			msg = r.ResponseDescription
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return nil, reply, fmt.Errorf("transaction status rejected: %s", m.scrub(msg))
	}
	return []string{r.ConversationID, r.OriginatorConversationID}, reply, nil
}

// ReceiptTest is one run of the Settings "Test receipt confirmation" button.
type ReceiptTest struct {
	Receipt string    `json:"receipt"`
	Status  string    `json:"status"`  // waiting, confirmed, not_confirmed, error
	Verdict string    `json:"verdict"` // what the receipt check concluded, in words
	Sent    string    `json:"sent"`    // Daraja's immediate reply to the query
	Raw     string    `json:"raw"`     // the result callback exactly as Daraja posted it (secrets hidden)
	Started time.Time `json:"started"`
}

const receiptTestPrefix = "test:"

// TestReceipt runs one Transaction Status query for a receipt that is known to
// be real (copy it from the M-Pesa statement) and keeps Daraja's raw answer for
// ReceiptTestStatus. It does not need receipt confirmation to be switched on:
// it is how the initiator credentials are tested before it is.
func (m *Mpesa) TestReceipt(ctx context.Context, receipt string) (ReceiptTest, error) {
	c := m.conf()
	switch {
	case !c.Enabled:
		return ReceiptTest{}, errors.New("M-Pesa is not enabled: save the key, secret, shortcode, passkey and the public URL first")
	case c.Initiator == "" || c.SecurityCredential == "":
		return ReceiptTest{}, errors.New("set the initiator name and the security credential first")
	case c.CallbackToken == "":
		return ReceiptTest{}, errors.New("a callback token is needed: Daraja posts the answer to a URL that carries it")
	}
	receipt = strings.ToUpper(strings.TrimSpace(receipt))
	t := &ReceiptTest{Receipt: receipt, Status: "waiting", Started: m.now(), Verdict: "waiting for Daraja's answer (usually a few seconds)"}
	// Register the key before sending: the answer can beat the reply.
	key := receiptTestPrefix + receipt
	m.vmu.Lock()
	if m.rtests == nil {
		m.rtests = map[string]*ReceiptTest{}
	}
	for k, v := range m.rtests {
		if m.now().Sub(v.Started) > time.Hour {
			delete(m.rtests, k)
		}
	}
	m.rtests[key] = t
	m.vmu.Unlock()
	ids, reply, err := m.sendTxStatus(ctx, receipt)
	m.vmu.Lock()
	t.Sent = reply
	if err != nil {
		t.Status, t.Verdict = "error", err.Error()
		out := *t
		m.vmu.Unlock()
		return out, err
	}
	out := *t
	m.vmu.Unlock()
	m.rememberConvs(key, ids)
	return out, nil
}

// ReceiptTestStatus returns the latest state of a receipt test.
func (m *Mpesa) ReceiptTestStatus(receipt string) (ReceiptTest, bool) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	t, ok := m.rtests[receiptTestPrefix+strings.ToUpper(strings.TrimSpace(receipt))]
	if !ok {
		return ReceiptTest{}, false
	}
	return *t, true
}

// judge applies the receipt check's rules to a result and says what it concludes.
func (m *Mpesa) judge(res txStatusResult, receipt string) (ok bool, why string) {
	params := map[string]string{}
	for _, p := range res.Result.ResultParameters.ResultParameter {
		params[p.Key] = rawString(p.Value)
	}
	switch {
	case rawString(res.Result.ResultCode) != "0":
		return false, "Daraja: " + m.scrub(res.Result.ResultDesc)
	case !strings.EqualFold(params["ReceiptNo"], receipt):
		return false, "Daraja answered for a different receipt"
	case !strings.EqualFold(params["TransactionStatus"], "Completed"):
		return false, "Daraja says the transaction is " + m.scrub(params["TransactionStatus"])
	}
	return true, ""
}

// recordReceiptTest stores the raw answer of a receipt test.
func (m *Mpesa) recordReceiptTest(key string, body []byte, res txStatusResult) {
	_, receipt, _ := strings.Cut(key, ":")
	ok, why := m.judge(res, receipt)
	raw := m.scrub(string(body))
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(raw), "", "  ") == nil {
		raw = pretty.String()
	}
	if len(raw) > 8000 {
		raw = raw[:8000] + "\n... (cut)"
	}
	m.vmu.Lock()
	defer m.vmu.Unlock()
	t := m.rtests[key]
	if t == nil {
		return
	}
	t.Raw = raw
	if ok {
		t.Status, t.Verdict = "confirmed", "Daraja confirmed this receipt: receipt confirmation will work with these credentials."
	} else {
		t.Status, t.Verdict = "not_confirmed", "Daraja did not confirm this receipt: "+why+". Check the initiator name, the security credential and that the receipt is real; receipt confirmation would not credit payments with this setup."
	}
}

type txStatusResult struct {
	Result struct {
		ResultCode               json.RawMessage `json:"ResultCode"`
		ResultDesc               string          `json:"ResultDesc"`
		OriginatorConversationID string          `json:"OriginatorConversationID"`
		ConversationID           string          `json:"ConversationID"`
		ResultParameters         struct {
			ResultParameter []struct {
				Key   string          `json:"Key"`
				Value json.RawMessage `json:"Value"`
			} `json:"ResultParameter"`
		} `json:"ResultParameters"`
	} `json:"Result"`
}

// HandleTxStatusResult takes Daraja's answer to a receipt check. It always
// acknowledges. An answer that does not match a query we sent is ignored.
func (m *Mpesa) HandleTxStatusResult(ctx context.Context, body []byte) C2BResponse {
	var r txStatusResult
	if err := json.Unmarshal(body, &r); err != nil {
		return C2BAccept
	}
	res := r.Result
	m.vmu.Lock()
	key, ok := m.convs[res.ConversationID]
	if !ok {
		key, ok = m.convs[res.OriginatorConversationID]
	}
	if ok && strings.HasPrefix(key, receiptTestPrefix) { // the Settings test button, not a payment
		m.vmu.Unlock()
		m.recordReceiptTest(key, body, r)
		return C2BAccept
	}
	st := m.verify[key]
	if !ok || st == nil || (res.ConversationID == "" && res.OriginatorConversationID == "") {
		m.vmu.Unlock()
		m.log.Warn("receipt check answer that matches no query of ours: ignored")
		return C2BAccept
	}
	m.vmu.Unlock()
	params := map[string]string{}
	for _, p := range res.ResultParameters.ResultParameter {
		params[p.Key] = rawString(p.Value)
	}
	_, receipt, _ := strings.Cut(key, ":")
	fail := func(why string) {
		m.vmu.Lock()
		st.lastErr = why
		m.vmu.Unlock()
		m.log.Warn("receipt not confirmed by Daraja", "receipt", receipt, "why", why)
	}
	switch {
	case rawString(res.ResultCode) != "0":
		fail("Daraja: " + m.scrub(res.ResultDesc))
	case !strings.EqualFold(params["ReceiptNo"], receipt):
		fail("Daraja answered for a different receipt")
	case !strings.EqualFold(params["TransactionStatus"], "Completed"):
		fail("Daraja says the transaction is " + m.scrub(params["TransactionStatus"]))
	default:
		if want := st.amount; want > 0 && params["Amount"] != "" {
			if got, err := strconv.ParseFloat(strings.Map(func(r rune) rune {
				if (r >= '0' && r <= '9') || r == '.' {
					return r
				}
				return -1
			}, params["Amount"]), 64); err == nil && math.Floor(got) < float64(want) {
				fail("Daraja says the amount was KES " + strconv.FormatFloat(got, 'f', -1, 64) + ", the callback claimed " + strconv.Itoa(want))
				break
			}
		}
		m.vmu.Lock()
		st.confirmed = true
		m.vmu.Unlock()
		m.log.Info("receipt confirmed by Daraja", "receipt", receipt)
		m.kick(ctx)
	}
	return C2BAccept
}

// HandleTxStatusTimeout takes Daraja's "the query timed out" notice: the next retry asks again.
func (m *Mpesa) HandleTxStatusTimeout(body []byte) C2BResponse {
	m.log.Warn("Daraja receipt check timed out; it will be asked again")
	return C2BAccept
}
