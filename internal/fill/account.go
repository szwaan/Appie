package fill

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// rollbackTimeout bounds the revert a failed Open runs after its caller's
// context has ended.
const rollbackTimeout = 10 * time.Second

// Account is the person's AH account with at most one order open for
// changes. It is the only door the UI has to AH.
type Account struct {
	ah AH

	// mu makes Open, Leave and Close run one at a time, AH calls included,
	// so Close waits out an Open that is still reopening an order.
	mu sync.Mutex
	// live is the session Open last returned. At most one session is not
	// left, and only live can be that one; whether it is left is the
	// session's own stage.
	live *Session
}

func NewAccount(ah AH) *Account {
	return &Account{ah: ah}
}

// Orders lists the open delivery orders, soonest delivery first. It always
// asks AH, so an order reopened elsewhere or by a run that died shows up as
// Reopened.
func (a *Account) Orders(ctx context.Context) ([]Order, error) {
	orders, err := a.ah.OpenOrders(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(orders, func(x, y Order) int { return x.Delivery.Compare(y.Delivery) })
	return orders, nil
}

// Open opens an order for changes and returns its session, reopening the
// order at AH when it is Submitted and resuming it when it is already
// Reopened.
//
// Open is idempotent: opening the live order again returns the same session
// without calling AH. Opening a different order while one is live fails with
// ErrAnotherOrderOpen. If Open asked AH to reopen the order and then fails,
// it reverts the order before returning, so a failed Open never leaves the
// account on that order. Cancelling ctx does not cut that revert short.
func (a *Account) Open(ctx context.Context, id OrderID) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.live; s != nil && !s.isLeft() {
		if s.order.ID == id {
			return s, nil
		}
		return nil, fmt.Errorf("%w: order %v", ErrAnotherOrderOpen, s.order.ID)
	}

	o, err := a.find(ctx, id)
	if err != nil {
		return nil, err
	}
	reopenedHere := false
	switch o.State {
	case Locked:
		return nil, fmt.Errorf("%w: order %v", ErrLocked, id)
	case Submitted:
		reopenedHere = true
		if err := a.ah.Reopen(ctx, id); err != nil {
			if o2, ferr := a.find(ctx, id); ferr != nil || o2.State != Reopened {
				return nil, errors.Join(err, a.rollback(ctx, id))
			}
		}
		o.State = Reopened
	case Reopened:
	}

	snap, err := a.ah.Snapshot(ctx, id)
	if err != nil {
		if reopenedHere {
			err = errors.Join(err, a.rollback(ctx, id))
		}
		return nil, err
	}
	a.live = newSession(a.ah, o, snap)
	return a.live, nil
}

// rollback reverts an order a failed Open may have reopened. It outlives
// ctx's cancellation, because the caller giving up is often why Open failed.
func (a *Account) rollback(ctx context.Context, id OrderID) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	return a.revert(ctx, id)
}

// Leave makes sure AH no longer has the order reopened, which puts the
// account back on today's bonus prices. If the order is the live session's,
// Leave first pushes the person's pending requests once.
//
// Leave fails with ErrStillReopened when AH may still have the order
// reopened; the session then goes on filling and Leave can be retried. When
// it left the order but AH did not take everything the person asked for, it
// returns an *UnsavedError that says what. Leave is idempotent and works for
// any order, including one found Reopened at startup after a run died
// between reopen and revert.
func (a *Account) Leave(ctx context.Context, id OrderID) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.leave(ctx, id)
}

// Close leaves the live order, if there is one, and reports it the way
// Leave does. main defers it so the account is not left on a reopened order
// however the UI ends; screens call Leave.
func (a *Account) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == nil {
		return nil
	}
	return a.leave(ctx, a.live.order.ID)
}

// leave is Leave for a caller that holds a.mu.
func (a *Account) leave(ctx context.Context, id OrderID) error {
	s := a.live
	if s == nil || s.order.ID != id {
		return a.revert(ctx, id)
	}
	if s.isLeft() {
		return nil
	}
	s.flight.Lock()
	defer s.flight.Unlock()
	_ = s.push(ctx)
	if err := a.revert(ctx, id); err != nil {
		return err
	}
	s.markLeft()
	if unsaved := s.View().Unsaved(); unsaved != nil {
		return &UnsavedError{Order: id, Unsaved: unsaved}
	}
	return nil
}

// revert reverts the order. A failed revert counts as done when AH no longer
// has the order reopened.
func (a *Account) revert(ctx context.Context, id OrderID) error {
	err := a.ah.Revert(ctx, id)
	if err == nil {
		return nil
	}
	o, ferr := a.find(ctx, id)
	if errors.Is(ferr, ErrOrderNotOpen) || ferr == nil && o.State != Reopened {
		return nil
	}
	return fmt.Errorf("%w: order %v: %w", ErrStillReopened, id, err)
}

// find reads the order fresh from AH, or fails with ErrOrderNotOpen.
func (a *Account) find(ctx context.Context, id OrderID) (Order, error) {
	orders, err := a.ah.OpenOrders(ctx)
	if err != nil {
		return Order{}, err
	}
	for _, o := range orders {
		if o.ID == id {
			return o, nil
		}
	}
	return Order{}, fmt.Errorf("%w: order %v", ErrOrderNotOpen, id)
}
