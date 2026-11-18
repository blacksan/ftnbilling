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
		return old, false, nil
	}
	a, ok := d.Accounts[in.AccountID]
	if !ok {
		return p, false, ErrNotFound
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
		v.Used, v.UsedAt, v.UsedBy = true, now, in.OrderID
		d.Vouchers[v.Code] = v
	}
	paid := in.PaidAt
	if paid.IsZero() {
		paid = now
	}
	a = applyPackage(a, pkg, now)
	d.Accounts[a.ID] = a
	s.enqueueLocked(d, a, false, "payment "+key)
	p = store.Payment{
		Key: key, Method: in.Method, Status: store.PayApplied, AmountKES: in.AmountKES,
		Phone: in.Phone, Name: in.Name, BillRef: in.BillRef, AccountID: a.ID, PackageID: pkg.ID,
		OrderID: in.OrderID, Note: in.Note, PaidAt: paid, CreatedAt: now,
	}
	d.Payments[key] = p
	return p, true, nil
}

// Credit applies a package to an account and records the ledger entry. A
// duplicate key is a no-op (created=false), so callbacks are idempotent.
func (s *Service) Credit(ctx context.Context, in CreditInput) (store.Payment, bool, error) {
	var p store.Payment
	var created bool
	err := s.st.Update(func(d *store.Data) error {
		var err error
		p, created, err = s.creditLocked(d, in)
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
		s.log.Info("payment applied", "key", p.Key, "method", p.Method, "amount", p.AmountKES)
		s.runTasksShort(ctx)
	}
	return p, created, nil
}

// UnmatchedInput is a payment that could not be tied to an account and package.
type UnmatchedInput struct {
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
		d.Payments[key] = store.Payment{
			Key: key, Method: in.Method, Status: store.PayUnmatched, AmountKES: in.AmountKES,
			Phone: in.Phone, Name: in.Name, BillRef: in.BillRef, AccountID: in.AccountID,
			OrderID: in.Ord, Note: in.Note, PaidAt: paid, CreatedAt: now,
		}
		created = true
		return nil
	})
	return created, err
}

// AssignPayment applies an unmatched payment to an account and package.
func (s *Service) AssignPayment(ctx context.Context, key, accountID, pkgID, note string) error {
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
		a = applyPackage(a, pkg, now)
		d.Accounts[a.ID] = a
		s.enqueueLocked(d, a, false, "assigned payment "+key)
		p.Status, p.AccountID, p.PackageID, p.ResolvedAt = store.PayApplied, a.ID, pkg.ID, now
		p.Note = joinNote(p.Note, note)
		d.Payments[key] = p
		return nil
	})
	if err == nil {
		s.runTasksShort(ctx)
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
	if in.PackageID == "" {
		p, ok := s.PackageForAmount(in.AmountKES)
		if !ok {
			return store.Payment{}, fmt.Errorf("%w (KES %d): choose a package", ErrNoPackage, in.AmountKES)
		}
		in.PackageID = p.ID
	}
	note := in.Note
	if pk, ok := s.packageByID(in.PackageID); ok && pk.PriceKES != in.AmountKES {
		note = joinNote(note, fmt.Sprintf("amount KES %d differs from package price KES %d", in.AmountKES, pk.PriceKES))
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

func (s *Service) packageByID(id string) (p store.Package, ok bool) {
	s.st.View(func(d *store.Data) { p, ok = d.Packages[id] })
	return
}

// ExtendAccount is an admin grant (no money). It is logged in the ledger with
// amount 0 and method "admin".
func (s *Service) ExtendAccount(ctx context.Context, accountID, pkgID, note string, force bool) error {
	if p, ok := s.packageByID(pkgID); !ok || p.Deleted {
		return errors.New("choose a package")
	}
	_, _, err := s.Credit(ctx, CreditInput{
		Key: "ADMIN-" + strings.ToUpper(RandomID()), Method: "admin", AccountID: accountID, PackageID: pkgID,
		Note: joinNote("admin grant", note), Force: force,
	})
	return err
}

// ---- orders (portal checkout) --------------------------------------------

// OrderInput describes a new pending order for the account behind a MAC.
type OrderInput struct {
	AccountID   string
	PackageID   string
	Phone       string // M-Pesa payer, normalised; may be empty for vouchers/test
	Method      string // test, mpesa, voucher
	VoucherCode string
	MAC, IP     string
	Dst         string
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
		p, ok := d.Packages[in.PackageID]
		if !ok || p.Disabled || p.Deleted {
			return errors.New("that package is not available")
		}
		if lowerWhileActive(d, a, p, s.now()) {
			return fmt.Errorf("this plan is slower than your current one. It becomes available when your current plan ends on %s", a.ExpiresAt.In(EAT).Format("2 Jan 2006"))
		}
		o = store.Order{
			ID: RandomID(), AccountID: a.ID, PackageID: p.ID, Phone: in.Phone, Method: in.Method,
			AmountKES: p.PriceKES, Status: store.OrderPending, VoucherCode: in.VoucherCode,
			MAC: in.MAC, IP: in.IP, Dst: in.Dst, CreatedAt: s.now(),
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
	var o store.Order
	var acct store.Account
	already, lowered, gone := false, false, false
	err := s.st.Update(func(d *store.Data) error {
		var ok bool
		o, ok = d.Orders[orderID]
		if !ok {
			return ErrNotFound
		}
		if o.Status == store.OrderPaid {
			already = true
			return store.ErrNoChange
		}
		if _, _, err := s.creditLocked(d, CreditInput{
			Key: receipt, Method: orderMethodLabel(o.Method), AccountID: o.AccountID, PackageID: o.PackageID,
			AmountKES: o.AmountKES, Phone: o.Phone, OrderID: o.ID, VoucherCode: o.VoucherCode,
		}); err != nil {
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
						d.Payments[k] = store.Payment{Key: k, Method: "mpesa-stk", Status: store.PayUnmatched, AmountKES: o.AmountKES,
							Phone: o.Phone, BillRef: o.ID, AccountID: o.AccountID, OrderID: o.ID, Note: why, PaidAt: now, CreatedAt: now}
					}
				}
				lowered, gone = acctExists, !acctExists
				return nil
			}
			return err
		}
		o.Status, o.Reason, o.Receipt, o.PaidAt = store.OrderPaid, "", strings.ToUpper(receipt), s.now()
		d.Orders[o.ID] = o
		acct = d.Accounts[o.AccountID]
		return nil
	})
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
	s.runTasksShort(ctx)
	if o.IP != "" && strings.EqualFold(o.MAC, acct.MAC) {
		cur, _ := s.GetAccount(acct.ID)
		if s.AutoLogin(ctx, cur, o.IP) {
			_ = s.st.Update(func(d *store.Data) error {
				oo := d.Orders[o.ID]
				oo.LoggedIn = true
				d.Orders[o.ID] = oo
				return nil
			})
		}
	}
	return nil
}
