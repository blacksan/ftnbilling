package web

import (
	"net/http"
	"strings"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

type apiPayment struct {
	Key         string `json:"key"`
	SiteID      string `json:"site_id"`
	Method      string `json:"method"`
	Status      string `json:"status"`
	AmountKES   int    `json:"amount_kes"`
	Phone       string `json:"phone"`
	Name        string `json:"name"`
	BillRef     string `json:"bill_ref"`
	AccountID   string `json:"account_id"`
	AccountCode string `json:"account_code"`
	Flat        string `json:"flat"`
	PackageID   string `json:"package_id"`
	Note        string `json:"note"`
	PaidAt      string `json:"paid_at"`
	CreatedAt   string `json:"created_at"`
}

// The ledger says "applied" for money that was matched to an account; the API
// calls it "matched" (the app's word).
func apiStatus(s string) string {
	if s == store.PayApplied {
		return "matched"
	}
	return s
}

func storeStatus(s string) string {
	if s == "matched" {
		return store.PayApplied
	}
	return s
}

func (s *Server) paymentsJSON(rows []store.Payment) []apiPayment {
	out := make([]apiPayment, 0, len(rows))
	s.st.View(func(d *store.Data) {
		for _, p := range rows {
			a, _ := d.AnyAccount(p.AccountID)
			out = append(out, apiPayment{Key: p.Key, SiteID: p.SiteID, Method: p.Method, Status: apiStatus(p.Status), AmountKES: p.AmountKES,
				Phone: p.Phone, Name: p.Name, BillRef: p.BillRef, AccountID: p.AccountID, AccountCode: a.Code, Flat: a.Flat, PackageID: p.PackageID,
				Note: p.Note, PaidAt: p.PaidAt.UTC().Format(time.RFC3339), CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339)})
		}
	})
	return out
}

// parseDay reads YYYY-MM-DD (or an RFC 3339 time, whose EAT date is used).
func parseAPIDay(v string) (time.Time, bool) {
	if t, err := time.ParseInLocation("2006-01-02", v, billing.EAT); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		e := t.In(billing.EAT)
		return time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, billing.EAT), true
	}
	return time.Time{}, false
}

func (s *Server) apiPayments(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	pg, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := billing.PaymentFilter{SiteID: q.Get("site"), Status: storeStatus(q.Get("status")), Method: q.Get("method"), Q: q.Get("q")}
	switch f.Status {
	case "", store.PayApplied, store.PayUnmatched, store.PayRefunded, store.PayIgnored:
	default:
		validationErr(w, map[string]string{"status": "must be matched, unmatched, refunded or ignored"})
		return
	}
	fields := map[string]string{}
	if v := q.Get("from"); v != "" {
		if t, ok := parseAPIDay(v); ok {
			f.From = t
		} else {
			fields["from"] = "must be a date like 2026-11-01"
		}
	}
	if v := q.Get("to"); v != "" {
		if t, ok := parseAPIDay(v); ok {
			f.To = t
		} else {
			fields["to"] = "must be a date like 2026-11-30"
		}
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	rows, tot := s.svc.FilterPayments(f)
	page, next := slicePage(rows, pg)
	writeJSON(w, http.StatusOK, map[string]any{"items": s.paymentsJSON(page), "next_cursor": next,
		"totals": map[string]any{"count": tot.Count, "revenue_kes": tot.Revenue}})
}

func (s *Server) paymentByKey(key string) (p store.Payment, ok bool) {
	s.st.View(func(d *store.Data) { p, ok = d.Payments[key] })
	return
}

func (s *Server) respondPayment(w http.ResponseWriter, status int, key string) {
	if p, ok := s.paymentByKey(key); ok {
		writeJSON(w, status, s.paymentsJSON([]store.Payment{p})[0])
		return
	}
	apiErr(w, http.StatusNotFound, codeNotFound, "No such payment.", nil)
}

func (s *Server) apiPaymentCreate(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	var req struct {
		AccountID string `json:"account_id"`
		AmountKES int    `json:"amount_kes"`
		TransCode string `json:"trans_code"`
		PackageID string `json:"package_id"`
		PaidAt    string `json:"paid_at"`
		Note      string `json:"note"`
		Force     bool   `json:"force"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	fields := map[string]string{}
	if req.AccountID == "" {
		fields["account_id"] = "choose an account"
	}
	if req.AmountKES <= 0 {
		fields["amount_kes"] = "enter the amount paid, in whole shillings"
	}
	var paidAt time.Time
	if req.PaidAt != "" {
		t, err := time.Parse(time.RFC3339, req.PaidAt)
		if err != nil {
			fields["paid_at"] = "must be an RFC 3339 time like 2026-11-18T09:30:00Z"
		} else if t.After(s.svc.Now().Add(time.Hour)) {
			fields["paid_at"] = "cannot be in the future"
		}
		paidAt = t
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	acct, ok := s.accountOr404(w, req.AccountID)
	if !ok {
		return
	}
	var pay store.Payment
	var err error
	if strings.TrimSpace(req.TransCode) != "" {
		pay, err = s.svc.RecordManual(r.Context(), billing.ManualInput{AccountID: acct.ID, TransCode: req.TransCode, AmountKES: req.AmountKES,
			PackageID: req.PackageID, Force: req.Force, PaidAt: paidAt, Note: req.Note})
	} else {
		pay, err = s.recordCash(r, acct, req.AmountKES, req.PackageID, req.Note, paidAt, req.Force)
	}
	if err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "payment "+pay.Key+" recorded for "+acct.Code)
	s.respondPayment(w, http.StatusCreated, pay.Key)
}

// recordCash records money taken without an M-Pesa code (cash, bank). It gets a
// generated key and the method "cash", and counts as revenue.
func (s *Server) recordCash(r *http.Request, acct store.Account, amount int, pkgID, note string, paidAt time.Time, force bool) (store.Payment, error) {
	if pkgID == "" {
		p, ok := s.svc.PackageForAmount(acct.SiteID, amount)
		if !ok {
			return store.Payment{}, billing.ErrNoPackage
		}
		pkgID = p.ID
	}
	key := "CASH-" + strings.ToUpper(billing.RandomID())
	p, created, err := s.svc.Credit(r.Context(), billing.CreditInput{Key: key, Method: "cash", AccountID: acct.ID, PackageID: pkgID,
		AmountKES: amount, Note: note, PaidAt: paidAt, Force: force})
	if err == billing.ErrLowerPlan {
		_, _ = s.svc.RecordUnmatched(billing.UnmatchedInput{Key: key, Method: "cash", AmountKES: amount, AccountID: acct.ID,
			Note: "lower plan while active", PaidAt: paidAt})
		return p, billing.ErrLowerPlan
	}
	if err == nil && !created {
		return p, billing.ErrDuplicateKey
	}
	return p, err
}

func (s *Server) apiPaymentAssign(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	key := r.PathValue("key")
	var req struct {
		AccountID string `json:"account_id"`
		PackageID string `json:"package_id"`
		Note      string `json:"note"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	fields := map[string]string{}
	if req.AccountID == "" {
		fields["account_id"] = "choose an account"
	}
	if req.PackageID == "" {
		fields["package_id"] = "choose a package"
	}
	if len(fields) > 0 {
		validationErr(w, fields)
		return
	}
	if _, ok := s.paymentByKey(key); !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such payment.", nil)
		return
	}
	if err := s.svc.AssignPayment(r.Context(), key, req.AccountID, req.PackageID, req.Note); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "payment "+key+" assigned")
	s.respondPayment(w, http.StatusOK, key)
}

func (s *Server) apiPaymentResolve(w http.ResponseWriter, r *http.Request, c *apiCtx) {
	key := r.PathValue("key")
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if req.Status != store.PayRefunded && req.Status != store.PayIgnored {
		validationErr(w, map[string]string{"status": "must be refunded or ignored"})
		return
	}
	if _, ok := s.paymentByKey(key); !ok {
		apiErr(w, http.StatusNotFound, codeNotFound, "No such payment.", nil)
		return
	}
	if err := s.svc.ResolvePayment(key, req.Status, req.Note); err != nil {
		s.billingErr(w, err, "")
		return
	}
	c.audit(s, "payment "+key+" marked "+req.Status)
	s.respondPayment(w, http.StatusOK, key)
}
