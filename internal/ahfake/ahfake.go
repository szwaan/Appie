// Package ahfake is an in-memory Albert Heijn account that implements fill.AH
// for tests. Where AH's real behaviour is unknown it picks the least helpful
// plausible one, so fill has to verify by reading back:
//
//   - SetQuantities writes to AH's active order, whatever order it names.
//   - SetQuantities silently ignores unorderable and unknown products.
//   - Revert fails when the order is not reopened.
package ahfake

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/szwaan/appie/internal/fill"
)

// OrderID is the OrderID for AH's order number n. It panics when AH would
// never use n, so a test cannot seed an id the real adapter would refuse.
func OrderID(n int) fill.OrderID {
	id, err := fill.NewOrderID(n)
	if err != nil {
		panic(err)
	}
	return id
}

// ProductID is the ProductID for AH's webshop id n, and panics like OrderID.
func ProductID(n int) fill.ProductID {
	id, err := fill.NewProductID(n)
	if err != nil {
		panic(err)
	}
	return id
}

// Order seeds or reports one order. Lines maps products to quantities.
type Order struct {
	ID    fill.OrderID
	State fill.OrderState
	Lines map[fill.ProductID]int
}

// AH is the fake account. The zero value is not usable; call New.
type AH struct {
	mu       sync.Mutex
	catalog  []fill.Hit
	orders   map[fill.OrderID]*order
	active   fill.OrderID // AH's active order: set by Reopen, cleared by Revert
	failures map[string][]failure
	holds    map[string]*hold
}

type hold struct{ entered, release chan struct{} }

type order struct {
	state fill.OrderState
	qty   map[fill.ProductID]int
	seq   []fill.ProductID
}

type failure struct {
	err     error
	applied bool // the call takes effect before failing
}

var _ fill.AH = (*AH)(nil)

// New seeds a fake account. A Reopened order becomes AH's active order, as
// it would be after a run died between reopen and revert.
func New(catalog []fill.Hit, orders ...Order) *AH {
	f := &AH{catalog: catalog, orders: map[fill.OrderID]*order{}, failures: map[string][]failure{}, holds: map[string]*hold{}}
	for _, o := range orders {
		ord := &order{state: o.State, qty: map[fill.ProductID]int{}}
		ids := make([]fill.ProductID, 0, len(o.Lines))
		for id := range o.Lines {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b fill.ProductID) int { return cmp.Compare(a.Int(), b.Int()) })
		for _, id := range ids {
			ord.set(id, o.Lines[id])
		}
		f.orders[o.ID] = ord
		if o.State == fill.Reopened {
			f.active = o.ID
		}
	}
	return f
}

// FailNext makes the next call to method ("Revert", "Snapshot", ...) fail
// with err and have no effect.
func (f *AH) FailNext(method string, err error) {
	mustBeMethod(method)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = append(f.failures[method], failure{err: err})
}

// LoseNextReply makes the next call to method take effect and then report a
// timeout, as when the reply is lost after AH applied the call.
func (f *AH) LoseNextReply(method string) {
	mustBeMethod(method)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = append(f.failures[method], failure{err: context.DeadlineExceeded, applied: true})
}

// Hold makes the next call to method wait, before it takes effect, until
// release is called. entered is closed once the call is waiting.
func (f *AH) Hold(method string) (entered <-chan struct{}, release func()) {
	mustBeMethod(method)
	h := &hold{entered: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds[method] = h
	return h.entered, func() { close(h.release) }
}

// ReopenElsewhere reopens an order as the AH app would, moving AH's active
// order away from whatever fill has open.
func (f *AH) ReopenElsewhere(id fill.OrderID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.orders[id]; ok {
		o.state = fill.Reopened
		f.active = id
	}
}

// Order reports an order's current state for assertions.
func (f *AH) Order(id fill.OrderID) Order {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[id]
	if !ok {
		return Order{ID: id}
	}
	lines := map[fill.ProductID]int{}
	for id, q := range o.qty {
		lines[id] = q
	}
	return Order{ID: id, State: o.state, Lines: lines}
}

func (f *AH) OpenOrders(ctx context.Context) ([]fill.Order, error) {
	f.wait("OpenOrders")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, done := f.fail("OpenOrders", nil); done {
		return nil, err
	}
	ids := make([]fill.OrderID, 0, len(f.orders))
	for id := range f.orders {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b fill.OrderID) int { return cmp.Compare(a.Int(), b.Int()) })
	out := make([]fill.Order, 0, len(ids))
	for _, id := range ids {
		o := f.orders[id]
		out = append(out, fill.Order{
			ID:     id,
			Slot:   fmt.Sprintf("slot of order %v", id),
			Status: fmt.Sprintf("state %d", o.state),
			Total:  f.total(o).Payable,
			State:  o.state,
		})
	}
	return out, nil
}

func (f *AH) Reopen(ctx context.Context, id fill.OrderID) error {
	f.wait("Reopen")
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[id]
	if !ok {
		return fmt.Errorf("orderReopen %v: unknown order", id)
	}
	err, _ := f.fail("Reopen", func() error {
		if o.state != fill.Submitted {
			return fmt.Errorf("orderReopen %v: order cannot be reopened", id)
		}
		o.state = fill.Reopened
		f.active = id
		return nil
	})
	return err
}

func (f *AH) Revert(ctx context.Context, id fill.OrderID) error {
	f.wait("Revert")
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orders[id]
	if !ok {
		return fmt.Errorf("orderRevert %v: unknown order", id)
	}
	err, _ := f.fail("Revert", func() error {
		if o.state != fill.Reopened {
			return fmt.Errorf("orderRevert %v: order is not reopened", id)
		}
		o.state = fill.Submitted
		if f.active == id {
			f.active = fill.OrderID{}
		}
		return nil
	})
	return err
}

func (f *AH) Snapshot(ctx context.Context, id fill.OrderID) (fill.Snapshot, error) {
	f.wait("Snapshot")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, done := f.fail("Snapshot", nil); done {
		return fill.Snapshot{}, err
	}
	if f.active != id {
		return fill.Snapshot{}, fmt.Errorf("%w: active order is %v, not %v", fill.ErrNotActive, f.active, id)
	}
	o := f.orders[id]
	snap := fill.Snapshot{Total: f.total(o)}
	for _, pid := range o.seq {
		if q := o.qty[pid]; q > 0 {
			snap.Lines = append(snap.Lines, fill.Line{Product: f.product(pid).Product, Quantity: q})
		}
	}
	return snap, nil
}

func (f *AH) SetQuantities(ctx context.Context, id fill.OrderID, quantities map[fill.ProductID]int) error {
	f.wait("SetQuantities")
	f.mu.Lock()
	defer f.mu.Unlock()
	err, _ := f.fail("SetQuantities", func() error {
		target, ok := f.orders[f.active]
		if !ok {
			return errors.New("put items: no active order")
		}
		if target.state != fill.Reopened {
			return fmt.Errorf("%w: order %v", fill.ErrLocked, f.active)
		}
		for pid, q := range quantities {
			if q < 0 {
				return fmt.Errorf("put items: negative quantity for %v", pid)
			}
			if !f.product(pid).Orderable {
				continue
			}
			target.set(pid, q)
		}
		return nil
	})
	return err
}

func (f *AH) Search(ctx context.Context, id fill.OrderID, query string, limit int) ([]fill.Hit, error) {
	f.wait("Search")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, done := f.fail("Search", nil); done {
		return nil, err
	}
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("search: blank query reached AH")
	}
	var hits []fill.Hit
	for _, h := range f.catalog {
		if strings.Contains(strings.ToLower(h.Title), strings.ToLower(query)) {
			hits = append(hits, h)
			if len(hits) == limit {
				break
			}
		}
	}
	return hits, nil
}

// mustBeMethod fails a test that names a method fill.AH does not have, so a
// renamed method cannot leave an injected failure silently unused.
func mustBeMethod(name string) {
	if _, ok := reflect.TypeFor[fill.AH]().MethodByName(name); !ok {
		panic("ahfake: fill.AH has no method " + name)
	}
}

// wait blocks a call to method that Hold asked to wait. It runs before the
// call takes f.mu, so other calls go on meanwhile.
func (f *AH) wait(method string) {
	f.mu.Lock()
	h := f.holds[method]
	delete(f.holds, method)
	f.mu.Unlock()
	if h != nil {
		close(h.entered)
		<-h.release
	}
}

// fail consumes the next queued failure for method.
func (f *AH) fail(method string, apply func() error) (err error, done bool) {
	q := f.failures[method]
	if len(q) == 0 {
		if apply != nil {
			return apply(), false
		}
		return nil, false
	}
	next := q[0]
	f.failures[method] = q[1:]
	if next.applied && apply != nil {
		_ = apply()
	}
	return next.err, true
}

func (f *AH) product(id fill.ProductID) fill.Hit {
	for _, h := range f.catalog {
		if h.ID == id {
			return h
		}
	}
	return fill.Hit{Product: fill.Product{ID: id, Title: fmt.Sprintf("product %v", id)}}
}

func (f *AH) total(o *order) fill.Total {
	var t fill.Total
	for pid, q := range o.qty {
		p := f.product(pid).Product
		t.Payable += fill.Cents(q) * p.Price
		if p.Was > p.Price {
			t.Discount += fill.Cents(q) * (p.Was - p.Price)
		}
	}
	return t
}

func (o *order) set(id fill.ProductID, q int) {
	if q == 0 {
		delete(o.qty, id)
		o.seq = slices.DeleteFunc(o.seq, func(x fill.ProductID) bool { return x == id })
		return
	}
	if _, ok := o.qty[id]; !ok {
		o.seq = append(o.seq, id)
	}
	o.qty[id] = q
}
