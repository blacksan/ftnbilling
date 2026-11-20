package sites

import (
	"context"
	"errors"
	"sync"
)

// ErrRouterBusy: another change or pass holds this router's writer lock and the caller's time ran out waiting for it.
var ErrRouterBusy = errors.New("the router is busy with another change, so nothing was saved; try again in a few seconds")

// runLock is a mutex whose waiters give up with their context. The zero value is ready to use.
type runLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *runLock) init() { l.once.Do(func() { l.ch = make(chan struct{}, 1) }) }

// Lock waits without a deadline (background passes).
func (l *runLock) Lock() { l.init(); l.ch <- struct{}{} }

// LockCtx waits until the lock is free or ctx ends (then ErrRouterBusy, wrapping ctx's error).
func (l *runLock) LockCtx(ctx context.Context) error {
	l.init()
	select {
	case l.ch <- struct{}{}:
		return nil
	default:
	}
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return errors.Join(ErrRouterBusy, ctx.Err())
	}
}

// TryLock takes the lock only when it is free.
func (l *runLock) TryLock() bool {
	l.init()
	select {
	case l.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *runLock) Unlock() { l.init(); <-l.ch }
