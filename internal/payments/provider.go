// Package payments contains the payment providers: test, voucher and M-Pesa.
package payments

import (
	"context"
	"errors"
	"sync"
	"time"

	"ispbill/internal/billing"
	"ispbill/internal/store"
)

// Provider starts a payment for an order. Start returns quickly; providers
// that complete later (test, M-Pesa) call the Completer when the money arrives.
type Provider interface {
	Name() string
	Enabled() bool
	Start(ctx context.Context, o store.Order) error
}

// Completer is implemented by billing.Service.
type Completer interface {
	CompleteOrder(ctx context.Context, orderID, receipt string) error
	FailOrder(ctx context.Context, orderID, reason string) error
	SetCheckoutID(orderID, checkoutID string) error
	Credit(ctx context.Context, in billing.CreditInput) (store.Payment, bool, error)
	RecordUnmatched(in billing.UnmatchedInput) (bool, error)
	PackageForAmount(siteID string, amount int) (store.Package, bool)
}

// Test marks the order paid after a short delay. Only when TEST_MODE=true.
type Test struct {
	Enabled_ bool
	Delay    time.Duration
	C        Completer
	Logf     func(msg string, args ...any)

	wg sync.WaitGroup // the delayed completions in flight
}

// Wait blocks until every delayed completion started by Start has finished
// (tests call it before the store goes away).
func (t *Test) Wait() { t.wg.Wait() }

func (t *Test) Name() string  { return "test" }
func (t *Test) Enabled() bool { return t.Enabled_ }

func (t *Test) Start(_ context.Context, o store.Order) error {
	if !t.Enabled_ {
		return errors.New("test payments are disabled")
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		time.Sleep(t.Delay)
		if err := t.C.CompleteOrder(context.Background(), o.ID, "TEST-"+o.ID); err != nil && t.Logf != nil {
			t.Logf("test payment completion failed", "order", o.ID, "err", err)
		}
	}()
	return nil
}

// Voucher redeems a pre-paid voucher. Single use is enforced inside
// CompleteOrder's atomic store update; an already-used voucher fails the order.
type Voucher struct{ C Completer }

func (Voucher) Name() string  { return "voucher" }
func (Voucher) Enabled() bool { return true }

func (v Voucher) Start(ctx context.Context, o store.Order) error {
	if o.VoucherCode == "" {
		return errors.New("no voucher code")
	}
	if err := v.C.CompleteOrder(ctx, o.ID, "VOUCHER-"+o.VoucherCode); err != nil {
		_ = v.C.FailOrder(ctx, o.ID, err.Error())
		return err
	}
	return nil
}
