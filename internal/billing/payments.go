package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ispbill/internal/store"
)

// CreditInput describes money (or a voucher) to apply to an account.
type CreditInput struct {
	Key         string // unique ledger key: M-Pesa code, VOUCHER-x, TEST-x, ADMIN-x
	Method      string // mpesa-manual, mpesa-stk, mpesa-c2b, voucher, test, admin
	AccountID   string
	PackageID   string
	AmountKES   int
	Phone, Name string
	BillRef     string
	OrderID     string
	Note        string
	PaidAt      time.Time // zero = now
	VoucherCode string    // single use, checked in the same atomic update
	Force       bool      // admin only: apply even if the package is slower than the active plan
}

// errHeld is internal: the payment was put in Unmatched inside creditLocked
// (it may be the confirmation of a payment an admin marked paid by hand), so the
// data changed but nothing was credited.
var errHeld = errors.New("payment held in Unmatched")

// ErrPossibleDuplicate is returned by AssignPayment when the account has a
// "Mark paid" payment of the same amount from the last few hours: the money may
// be the same. DuplicateError carries the manual payment's key.
var ErrPossibleDuplicate = errors.New("possible duplicate of a payment marked paid by hand")

// DuplicateError says which manual payment the assigned money may duplicate.
type DuplicateError struct{ ManualKey string }

func (e *DuplicateError) Error() string {
	return "this account has a payment marked paid by hand (" + e.ManualKey + ") for the same amount in the last 6 hours: it may be the same money"
}
func (e *DuplicateError) Is(t error) bool { return t == ErrPossibleDuplicate }

// Notes that tie a manual payment and its real receipt together.
const (
	manualDupNote       = "duplicate: already credited when an admin marked the order paid"
	possibleDupNote     = "possible duplicate of manual payment "
	ReasonPromptNotSent = "the payment prompt could not be sent"
)

// ErrLowerPlan is returned when an active account would be moved to a slower
// package. Downgrades wait for the end of the current plan.
var ErrLowerPlan = errors.New("lower plan while active")

// lowerWhileActive reports whether buying pkg would downgrade an account that
// still has time left on a faster package. Expired and suspended accounts may
// buy anything.
func lowerWhileActive(d *store.Data, a store.Account, pkg store.Package, now time.Time) bool {
	if a.Suspended || !a.ExpiresAt.After(now) {
		return false
	}
	cur, ok := d.Packages[a.PackageID]
	return ok && pkg.SpeedMbps < cur.SpeedMbps
}

// LowerPlanBlocked tells the portal whether pkg is unavailable to the account
// right now, and until when (the current expiry).
func (s *Service) LowerPlanBlocked(a store.Account, pkg store.Package) (until time.Time, blocked bool) {
	s.st.View(func(d *store.Data) { blocked = lowerWhileActive(d, a, pkg, s.now()) })
	return a.ExpiresAt, blocked
}

// applyPackage extends or starts the account's plan. A still-active plan is
// extended from its current expiry; an upgrade switches the package at once.
func applyPackage(a store.Account, pkg store.Package, now time.Time) store.Account {
	dur := time.Duration(pkg.DurationMin) * time.Minute
	if a.ExpiresAt.After(now) {
		a.ExpiresAt = a.ExpiresAt.Add(dur)
	} else {
		a.ExpiresAt = now.Add(dur)
	}
	a.PackageID = pkg.ID
	return a
}

// creditLocked is Credit inside an Update. created=false means the key already
// exists and nothing was changed (idempotent).
func (s *Service) creditLocked(d *store.Data, in CreditInput) (p store.Payment, created bool, err error) {
	key := strings.ToUpper(strings.TrimSpace(in.Key))
	if key == "" {
		return p, false, errors.New("payment key is required")
	}
	if in.VoucherCode != "" { // a used voucher never completes another order, even if the key repeats
		if v, ok := d.Vouchers[in.VoucherCode]; !ok || (v.Used && v.UsedBy != in.OrderID) {
			return p, false, ErrVoucherUsed
		}
	}
	if old, ok := d.Payments[key]; ok {
		switch {
		case old.Status == store.PayApplied:
			return old, false, nil
		case old.Status == store.PayUnmatched:
			// The same receipt first arrived without a home (an odd amount, a
			// deleted package, a lower plan) and now comes with an account and
			// a package: apply that row instead of reporting "already exists".
			return s.applyUnmatchedLocked(d, old, in)
		case old.Status == store.PayIgnored && strings.HasPrefix(old.Note, manualDupNote) && in.OrderID != "" && in.OrderID != old.OrderID:
			// Wrongly recorded as the duplicate of another order's manual
			// payment: this is a different order's money. Apply it here.
			return s.applyUnmatchedLocked(d, old, in)
		case old.Status == store.PayIgnored && strings.HasPrefix(old.Note, manualDupNote):
			return old, false, nil // a true duplicate of this order's manual payment
		default:
			return old, false, fmt.Errorf("%w: receipt %s was already marked %s", ErrBadState, key, old.Status)
		}
	}
	a, ok := d.Accounts[in.AccountID]
	if !ok {
		return p, false, ErrNotFound
	}
	// A Paybill confirmation by account code (no order id) right after a "Mark
	// paid" of the same amount may be that very payment, or a new one: a person
	// decides, so it waits in Unmatched instead of being credited or swallowed.
	if in.OrderID == "" && in.Method == "mpesa-c2b" && in.AmountKES > 0 {
		if mk, found := recentManual(d, in.AccountID, in.AmountKES, s.now()); found {
			now := s.now()
			paid := in.PaidAt
			if paid.IsZero() {
				paid = now
			}
			p = store.Payment{Key: key, SiteID: a.SiteID, Method: in.Method, Status: store.PayUnmatched, AmountKES: in.AmountKES,
				Phone: in.Phone, Name: in.Name, BillRef: in.BillRef, AccountID: a.ID, CustomerID: a.CustomerID,
				Note: possibleDupNote + mk + ": if it is the same money, mark it ignored; if the customer paid again, assign it", PaidAt: paid, CreatedAt: now}
			d.Payments[key] = p
			return p, false, errHeld
		}
	}
	pkg, ok := d.Packages[in.PackageID]
	if !ok {
		return p, false, errors.New("package no longer exists")
	}
	now := s.now()
	if !in.Force && lowerWhileActive(d, a, pkg, now) {
		return p, false, ErrLowerPlan
	}
	if in.VoucherCode != "" {
		v, ok := d.Vouchers[in.VoucherCode]
		if !ok || v.Used || v.PackageID != pkg.ID {
			return p, false, ErrVoucherUsed
		}
		if v.SiteID != "" && v.SiteID != a.SiteID {
			return p, false, ErrVoucherSite
		}
		v.Used, v.UsedAt, v.UsedBy = true, now, in.OrderID
		d.Vouchers[v.Code] = v
	}
	paid := in.PaidAt
	if paid.IsZero() {
		paid = now
	}
	a = applyPackage(a, pkg, now)
	d.Accounts[a.ID] = a
	s.enqueueLocked(d, a, "payment "+key)
	p = store.Payment{
		Key: key, SiteID: a.SiteID, Method: in.Method, Status: store.PayApplied, AmountKES: in.AmountKES,
		Phone: in.Phone, Name: in.Name, BillRef: in.BillRef, AccountID: a.ID, CustomerID: a.CustomerID, PackageID: pkg.ID,
		OrderID: in.OrderID, Note: in.Note, PaidAt: paid, CreatedAt: now,
	}
	d.Payments[key] = p
	return p, true, nil
}

// ManualPrefix starts the ledger key of an order an admin marked paid by hand.
const ManualPrefix = "MANUAL-"

// manualLinkWindow is how long after "Mark paid" the real confirmation of the
// same money is still recognised and linked instead of credited again.
const manualLinkWindow = 6 * time.Hour

// applyUnmatchedLocked turns an Unmatched row into an applied payment.
func (s *Service) applyUnmatchedLocked(d *store.Data, old store.Payment, in CreditInput) (store.Payment, bool, error) {
	a, ok := d.Accounts[in.AccountID]
	if !ok {
		return old, false, ErrNotFound
	}
	pkg, ok := d.Packages[in.PackageID]
	if !ok {
		return old, false, errors.New("package no longer exists")
	}
	now := s.now()
	if !in.Force && lowerWhileActive(d, a, pkg, now) {
		return old, false, ErrLowerPlan
	}
	if in.VoucherCode != "" {
		return old, false, ErrVoucherUsed
	}
	a = applyPackage(a, pkg, now)
	d.Accounts[a.ID] = a
	s.enqueueLocked(d, a, "payment "+old.Key)
	if strings.HasPrefix(old.Note, manualDupNote) { // it was filed as another order's duplicate: undo that
		mk := ManualPrefix + strings.ToUpper(old.OrderID)
		if m, ok := d.Payments[mk]; ok {
			c := "confirmed by M-Pesa receipt " + old.Key
			m.Note = strings.TrimSpace(strings.NewReplacer(" | "+c, "", c+" | ", "", c, "").Replace(m.Note))
			d.Payments[mk] = m
		}
		old.Note = "was wrongly filed as a duplicate of " + mk
	}
	old.Status, old.AccountID, old.CustomerID, old.SiteID, old.PackageID = store.PayApplied, a.ID, a.CustomerID, a.SiteID, pkg.ID
	old.ResolvedAt = now
	if in.OrderID != "" {
		old.OrderID = in.OrderID
	}
	if old.Phone == "" {
		old.Phone = in.Phone
	}
	old.Note = joinNote(old.Note, "applied when the matching confirmation arrived")
	d.Payments[old.Key] = old
	return old, true, nil
}

// recentManual finds a payment an admin marked paid (key MANUAL-...) for the
// account and amount within manualLinkWindow.
func recentManual(d *store.Data, accountID string, amount int, now time.Time) (key string, ok bool) {
	var best store.Payment
	for k, m := range d.Payments {
		if !strings.HasPrefix(k, ManualPrefix) || m.Status != store.PayApplied || m.AccountID != accountID ||
			m.AmountKES != amount || now.Sub(m.CreatedAt) > manualLinkWindow {
			continue
		}
		if !ok || m.CreatedAt.After(best.CreatedAt) {
			best, ok = m, true
		}
	}
	return best.Key, ok
}

// linkManualRow records the real confirmation of money an admin already marked
// paid. The ledger is never deleted from, so the manual row stays the one
// applied (and counted) payment; the real receipt is stored beside it as an
// ignored duplicate that says where the credit is, so a redelivery finds it.
func linkManualRow(d *store.Data, m store.Payment, key, method string, amount int, phone string, paidAt, now time.Time) {
	if paidAt.IsZero() {
		paidAt = m.PaidAt
	}
	if phone == "" {
		phone = m.Phone
	}
	d.Payments[key] = store.Payment{Key: key, SiteID: m.SiteID, Method: method, Status: store.PayIgnored, AmountKES: amount,
		Phone: phone, BillRef: m.BillRef, AccountID: m.AccountID, CustomerID: m.CustomerID, PackageID: m.PackageID, OrderID: m.OrderID,
		Note: manualDupNote + " (" + m.Key + "); not credited twice", PaidAt: paidAt, CreatedAt: now, ResolvedAt: now}
	m.Note = joinNote(m.Note, "confirmed by M-Pesa receipt "+key)
	d.Payments[m.Key] = m
}

// Credit applies a package to an account and records the ledger entry. A
// duplicate key is a no-op (created=false), so callbacks are idempotent.
func (s *Service) Credit(ctx context.Context, in CreditInput) (store.Payment, bool, error) {
	var p store.Payment
	var created bool
	err := s.st.Update(func(d *store.Data) error {
		var err error
		p, created, err = s.creditLocked(d, in)
		if errors.Is(err, errHeld) {
			return nil // kept in Unmatched; the caller sees created=false and the row
		}
		if err != nil || !created {
			if err == nil {
				return store.ErrNoChange
			}
			return err
		}
		return nil
	})
	if err != nil {
		return p, false, err
	}
	if created {
		s.log.Info("payment applied", "site", p.SiteID, "key", p.Key, "method", p.Method, "amount", p.AmountKES)
		s.runSiteShort(ctx, p.SiteID)
		s.credited()
	}
	return p, created, nil
}

// UnmatchedInput is a payment that could not be tied to an account and package.
type UnmatchedInput struct {
	SiteID               string // optional; taken from the account when AccountID is set
	Key, Method          string
	AmountKES            int
	Phone, Name, BillRef string
	AccountID, Note, Ord string
	PaidAt               time.Time
}

// RecordUnmatched puts a payment into the "Unmatched payments" queue. A
// duplicate key is a no-op. AccountID may be set when only the amount is odd.
func (s *Service) RecordUnmatched(in UnmatchedInput) (bool, error) {
	key := strings.ToUpper(strings.TrimSpace(in.Key))
	if key == "" {
		return false, errors.New("payment key is required")
	}
	created := false
	err := s.st.Update(func(d *store.Data) error {
		if _, ok := d.Payments[key]; ok {
			return store.ErrNoChange
		}
		now := s.now()
		paid := in.PaidAt
		if paid.IsZero() {
			paid = now
		}
		siteID, customerID := in.SiteID, ""
		if a, ok := d.Accounts[in.AccountID]; ok {
			siteID, customerID = a.SiteID, a.CustomerID
		}
		d.Payments[key] = store.Payment{
			Key: key, SiteID: siteID, Method: in.Method, Status: store.PayUnmatched, AmountKES: in.AmountKES,
			Phone: in.Phone, Name: in.Name, BillRef: in.BillRef, AccountID: in.AccountID, CustomerID: customerID,
			OrderID: in.Ord, Note: in.Note, PaidAt: paid, CreatedAt: now,
		}
		created = true
		return nil
	})
	return created, err
}

// How AssignPayment treats a payment for a plan slower than the account's
// active one.
const (
	AssignRefuseLower = ""       // refuse with ErrLowerPlan: the admin must choose
	AssignSwitchPlan  = "switch" // move the account to the slower plan now
	AssignAddTime     = "extend" // keep the current plan and add the purchased time to it
)

// AssignPayment applies an unmatched payment to an account and package. A plan
// slower than the active one is refused (ErrLowerPlan); use AssignPaymentMode.
func (s *Service) AssignPayment(ctx context.Context, key, accountID, pkgID, note string) error {
	return s.AssignPaymentMode(ctx, key, accountID, pkgID, note, AssignRefuseLower)
}

// AssignPaymentMode is AssignPayment with the admin's choice for a lower plan.
func (s *Service) AssignPaymentMode(ctx context.Context, key, accountID, pkgID, note, lower string) error {
	return s.AssignPaymentOpts(ctx, key, accountID, pkgID, note, AssignOpts{Lower: lower})
}

// AssignOpts are the admin's answers to the warnings Assign can raise.
type AssignOpts struct {
	Lower           string // AssignSwitchPlan or AssignAddTime for a plan slower than the active one
	AcceptDuplicate bool   // assign even though the account has a "Mark paid" payment of the same amount from the last 6 hours
}

// AssignPaymentOpts applies an unmatched payment. It refuses (ErrPossibleDuplicate)
// when the money may be the confirmation of a payment already marked paid by
// hand, and (ErrLowerPlan) when the plan is slower than the active one, unless
// the admin said how to go on.
func (s *Service) AssignPaymentOpts(ctx context.Context, key, accountID, pkgID, note string, opt AssignOpts) error {
	lower := opt.Lower
	var site string
	err := s.st.Update(func(d *store.Data) error {
		p, ok := d.Payments[key]
		if !ok {
			return ErrNotFound
		}
		if p.Status != store.PayUnmatched {
			return ErrBadState
		}
		a, ok := d.Accounts[accountID]
		if !ok {
			return ErrNotFound
		}
		pkg, ok := d.Packages[pkgID]
		if !ok || pkg.Deleted {
			return errors.New("choose a package")
		}
		now := s.now()
		if mk, dup := recentManual(d, a.ID, p.AmountKES, now); dup && !opt.AcceptDuplicate {
			return &DuplicateError{ManualKey: mk}
		}
		switch {
		case !lowerWhileActive(d, a, pkg, now):
			a = applyPackage(a, pkg, now)
		case lower == AssignSwitchPlan:
			a = applyPackage(a, pkg, now)
			note = joinNote(note, "switched to the slower plan")
		case lower == AssignAddTime:
			keep := a.PackageID
			a = applyPackage(a, pkg, now)
			a.PackageID = keep
			note = joinNote(note, "time added at the current plan")
		default:
			return ErrLowerPlan
		}
		d.Accounts[a.ID] = a
		s.enqueueLocked(d, a, "assigned payment "+key)
		p.Status, p.AccountID, p.PackageID, p.ResolvedAt = store.PayApplied, a.ID, pkg.ID, now
		p.CustomerID = a.CustomerID
		p.SiteID = a.SiteID
		site = a.SiteID
		p.Note = joinNote(p.Note, note)
		d.Payments[key] = p
		return nil
	})
	if err == nil {
		s.runSiteShort(ctx, site)
		s.credited()
	}
	return err
}

// ResolvePayment marks an unmatched payment refunded or ignored, with a note.
func (s *Service) ResolvePayment(key, status, note string) error {
	if status != store.PayRefunded && status != store.PayIgnored {
		return errors.New("status must be refunded or ignored")
	}
	return s.st.Update(func(d *store.Data) error {
		p, ok := d.Payments[key]
		if !ok {
			return ErrNotFound
		}
		if p.Status != store.PayUnmatched {
			return ErrBadState
		}
		p.Status, p.ResolvedAt = status, s.now()
		p.Note = joinNote(p.Note, note)
		d.Payments[key] = p
		return nil
	})
}

func joinNote(old, add string) string {
	add = cleanText(add, 200)
	switch {
	case old == "":
		return add
	case add == "":
		return old
	}
	return old + " | " + add
}

// ManualInput is the admin "Record M-Pesa payment" form.
type ManualInput struct {
	AccountID string
	TransCode string
	AmountKES int
	Phone     string
	PackageID string // empty = the package whose price equals the amount
	Force     bool   // apply even if slower than the active plan (changes speed now)
	PaidAt    time.Time
	Note      string
}

// RecordManual records a Paybill/Till payment the admin saw on the M-Pesa
// statement and applies the package exactly like an automatic payment.
func (s *Service) RecordManual(ctx context.Context, in ManualInput) (store.Payment, error) {
	code, err := NormalizeTransCode(in.TransCode)
	if err != nil {
		return store.Payment{}, err
	}
	if in.AmountKES <= 0 {
		return store.Payment{}, errors.New("enter the amount paid")
	}
	phone, err := normPhone(in.Phone)
	if err != nil {
		return store.Payment{}, err
	}
	acct, ok := s.GetAccount(in.AccountID)
	if !ok {
		return store.Payment{}, ErrNotFound
	}
	if in.PackageID == "" {
		p, ok := s.PackageForAmount(acct.SiteID, in.AmountKES)
		if !ok {
			return store.Payment{}, fmt.Errorf("%w (KES %d): choose a package", ErrNoPackage, in.AmountKES)
		}
		in.PackageID = p.ID
	}
	note := in.Note
	if pk, ok := s.packageByID(acct.SiteID, in.PackageID); ok && pk.PriceKES != in.AmountKES {
		note = joinNote(note, fmt.Sprintf("amount KES %d differs from package price KES %d at this building", in.AmountKES, pk.PriceKES))
	}
	p, created, err := s.Credit(ctx, CreditInput{
		Key: code, Method: "mpesa-manual", AccountID: in.AccountID, PackageID: in.PackageID,
		AmountKES: in.AmountKES, Phone: phone, Note: note, PaidAt: in.PaidAt, Force: in.Force,
	})
	if errors.Is(err, ErrLowerPlan) {
		// Keep the money on record; the admin decides in the Unmatched queue.
		_, _ = s.RecordUnmatched(UnmatchedInput{Key: code, Method: "mpesa-manual", AmountKES: in.AmountKES,
			Phone: phone, AccountID: in.AccountID, Note: joinNote(note, "lower plan while active"), PaidAt: in.PaidAt})
		return p, ErrLowerPlan
	}
	if err != nil {
		return p, err
	}
	if !created {
		return p, ErrDuplicateKey
	}
	return p, nil
}

// packageByID returns the package with the building's overrides applied.
func (s *Service) packageByID(siteID, id string) (p store.Package, ok bool) {
	s.st.View(func(d *store.Data) { p, ok = d.EffectivePackage(siteID, id) })
	return
}

// ExtendAccount is an admin grant (no money). It is logged in the ledger with
// amount 0 and method "admin".
func (s *Service) ExtendAccount(ctx context.Context, accountID, pkgID, note string, force bool) error {
	if p, ok := s.packageByID("", pkgID); !ok || p.Deleted {
		return errors.New("choose a package")
	}
	_, _, err := s.Credit(ctx, CreditInput{
		Key: "ADMIN-" + strings.ToUpper(RandomID()), Method: "admin", AccountID: accountID, PackageID: pkgID,
		Note: joinNote("admin grant", note), Force: force,
	})
	return err
}

// ---- orders (portal checkout) --------------------------------------------

// OrderInput describes a new pending order for an account.
type OrderInput struct {
	AccountID   string
	PackageID   string
	Phone       string // M-Pesa payer, normalised; may be empty for vouchers/test
	Method      string // test, mpesa, voucher
	VoucherCode string
}

// CreateOrder records a pending order priced from the package.
func (s *Service) CreateOrder(in OrderInput) (store.Order, error) {
	var o store.Order
	err := s.st.Update(func(d *store.Data) error {
		a, ok := d.Accounts[in.AccountID]
		if !ok {
			return ErrNotFound
		}
		if a.Suspended {
			return ErrSuspended
		}
		p, ok := d.EffectivePackage(a.SiteID, in.PackageID)
		// A voucher outlives its package being switched off ("disable the
		// package instead so the vouchers still work"): an unused voucher for
		// exactly this package redeems even if the package is disabled or
		// hidden at this building. A deleted package never does.
		voucherOK := false
		if in.VoucherCode != "" && ok {
			v, vok := d.Vouchers[in.VoucherCode]
			voucherOK = vok && !v.Used && v.PackageID == p.ID
		}
		if !ok || p.Deleted || (p.Disabled && !voucherOK) {
			return errors.New("that package is not available")
		}
		if lowerWhileActive(d, a, p, s.now()) {
			return fmt.Errorf("this plan is slower than your current one. It becomes available when your current plan ends on %s", a.ExpiresAt.In(EAT).Format("2 Jan 2006"))
		}
		o = store.Order{
			ID: RandomID(), SiteID: a.SiteID, AccountID: a.ID, PackageID: p.ID, Phone: in.Phone, Method: in.Method,
			AmountKES: p.PriceKES, Status: store.OrderPending, VoucherCode: in.VoucherCode,
			CreatedAt: s.now(),
		}
		d.Orders[o.ID] = o
		return nil
	})
	return o, err
}

// SetCheckoutID stores the Daraja CheckoutRequestID on an order.
func (s *Service) SetCheckoutID(orderID, checkoutID string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.Orders[orderID]
		if !ok {
			return ErrNotFound
		}
		o.CheckoutRequestID = checkoutID
		d.Orders[orderID] = o
		return nil
	})
}

// OrderTTL is how long a pending order waits for its payment before it is
// marked expired. An expired order is still completed by a late confirmation.
const OrderTTL = 15 * time.Minute

// ExpireOrders marks pending orders older than OrderTTL as expired (not
// failed: the customer may still pay, and the confirmation will credit them).
// It returns how many were changed.
func (s *Service) ExpireOrders() (int, error) {
	n := 0
	err := s.st.Update(func(d *store.Data) error {
		cut := s.now().Add(-OrderTTL)
		for id, o := range d.Orders {
			if o.Status == store.OrderPending && o.CreatedAt.Before(cut) {
				o.Status, o.Reason = store.OrderExpired, "no payment was received within 15 minutes"
				d.Orders[id] = o
				n++
			}
		}
		if n == 0 {
			return store.ErrNoChange
		}
		return nil
	})
	return n, err
}

// FailOrder marks a pending order failed (a paid order is never downgraded).
func (s *Service) FailOrder(_ context.Context, orderID, reason string) error {
	return s.st.Update(func(d *store.Data) error {
		o, ok := d.Orders[orderID]
		if !ok {
			return ErrNotFound
		}
		if o.Status != store.OrderPending {
			return store.ErrNoChange
		}
		o.Status, o.Reason = store.OrderFailed, reason
		d.Orders[orderID] = o
		return nil
	})
}

// GetOrder returns a copy of an order.
func (s *Service) GetOrder(id string) (o store.Order, ok bool) {
	s.st.View(func(d *store.Data) { o, ok = d.Orders[id] })
	return
}

func orderMethodLabel(m string) string {
	if m == "mpesa" {
		return "mpesa-stk"
	}
	return m
}

// CompleteOrder marks an order paid and credits its account. receipt is the
// ledger key (M-Pesa code, VOUCHER-x, TEST-x). Idempotent: a second call for a
// paid order, or a receipt that is already in the ledger, extends nothing.
// Router problems never fail the payment: the router task is retried.
func (s *Service) CompleteOrder(ctx context.Context, orderID, receipt string) error {
	return s.CompleteOrderAt(ctx, orderID, receipt, time.Time{})
}

// CompleteOrderAt is CompleteOrder with the time the customer paid (Daraja's
// transaction time); zero means now. An order an admin marked paid by hand
// (receipt MANUAL-...) takes the real receipt when it arrives and is not
// credited again.
func (s *Service) CompleteOrderAt(ctx context.Context, orderID, receipt string, paidAt time.Time) error {
	var o store.Order
	var acct store.Account
	already, lowered, gone := false, false, false
	var refused error // the receipt was already decided (refunded or ignored)
	err := s.st.Update(func(d *store.Data) error {
		var ok bool
		o, ok = d.Orders[orderID]
		if !ok {
			return ErrNotFound
		}
		if o.Status == store.OrderPaid {
			already = true
			key := strings.ToUpper(strings.TrimSpace(receipt))
			if m, ok := d.Payments[strings.ToUpper(o.Receipt)]; ok && strings.HasPrefix(m.Key, ManualPrefix) && key != "" &&
				!strings.HasPrefix(key, ManualPrefix) && m.Status == store.PayApplied {
				// The real receipt may already wait in Unmatched as "possible duplicate"
				// of this very manual payment: the order id proves it is.
				t, taken := d.Payments[key]
				if !taken || (t.Status == store.PayUnmatched && strings.HasPrefix(t.Note, possibleDupNote+m.Key)) {
					linkManualRow(d, m, key, orderMethodLabel(o.Method), o.AmountKES, o.Phone, paidAt, s.now())
					return nil
				}
			}
			return store.ErrNoChange
		}
		if _, _, err := s.creditLocked(d, CreditInput{
			Key: receipt, Method: orderMethodLabel(o.Method), AccountID: o.AccountID, PackageID: o.PackageID,
			AmountKES: o.AmountKES, Phone: o.Phone, OrderID: o.ID, VoucherCode: o.VoucherCode, PaidAt: paidAt,
		}); err != nil {
			if errors.Is(err, ErrBadState) {
				// Leave a trace a person will see: the order says why it is not paid,
				// and the event log carries it. The caller raises the alert.
				o.Reason = "payment " + strings.ToUpper(receipt) + " not credited: " + err.Error()
				d.Orders[o.ID] = o
				d.AddEvent(store.Event{Time: s.now(), SiteID: o.SiteID, Kind: "info", AccountID: o.AccountID,
					Msg: "payment " + strings.ToUpper(receipt) + " for order " + o.ID + " was not credited: " + err.Error()})
				refused = err
				return nil
			}
			_, acctExists := d.Accounts[o.AccountID]
			if errors.Is(err, ErrLowerPlan) || !acctExists { // plan changed, or account deleted, while the payment was in flight
				why := ErrLowerPlan.Error()
				if !acctExists {
					why = "account was deleted before the payment arrived"
				}
				o.Status, o.Reason = store.OrderFailed, why
				d.Orders[o.ID] = o
				if o.Method == "mpesa" {
					k := strings.ToUpper(receipt)
					if _, dup := d.Payments[k]; !dup {
						now := s.now()
						when := now
						if !paidAt.IsZero() {
							when = paidAt
						}
						d.Payments[k] = store.Payment{Key: k, SiteID: o.SiteID, Method: "mpesa-stk", Status: store.PayUnmatched, AmountKES: o.AmountKES,
							Phone: o.Phone, BillRef: o.ID, AccountID: o.AccountID, OrderID: o.ID, Note: why, PaidAt: when, CreatedAt: now}
					}
				}
				lowered, gone = acctExists, !acctExists
				return nil
			}
			return err
		}
		o.Status, o.Reason, o.Receipt, o.PaidAt = store.OrderPaid, "", strings.ToUpper(receipt), s.now()
		if !paidAt.IsZero() {
			o.PaidAt = paidAt
		}
		d.Orders[o.ID] = o
		acct = d.Accounts[o.AccountID]
		return nil
	})
	if err == nil && refused != nil {
		return refused
	}
	if err == nil && gone {
		s.log.Warn("payment for a deleted account put in Unmatched", "order", o.ID)
		return nil
	}
	if err != nil || already {
		return err
	}
	if lowered {
		return ErrLowerPlan
	}
	s.log.Info("order paid", "order", o.ID, "method", o.Method, "amount", o.AmountKES, "code", acct.Code)
	s.runSiteShort(ctx, acct.SiteID)
	s.credited()
	return nil
}
