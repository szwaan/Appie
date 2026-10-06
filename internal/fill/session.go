package fill

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

const searchLimit = 25

// MaxQuantity is the most of one product Set accepts.
const MaxQuantity = 99

// Session is one open order being filled. Only an Account creates it, and it
// is the only writer of that order in this process.
//
// It keeps AH's truth (base) apart from what the person asked for (wants).
// Requests only ever become absolute quantities, so pushing the same wants
// again is harmless and whichever push runs last sends the latest ones.
type Session struct {
	ah    AH
	order Order

	flight sync.Mutex

	mu    sync.Mutex // guards the fields below; never held across an AH call
	stage stage
	base  Snapshot
	wants map[ProductID]want
	// problem is the outcome of the last push, shown until the next one.
	problem error
	seq     uint64
}

// want is the quantity the person asked for, kept until AH's truth matches
// it or AH refuses it.
type want struct {
	product  Product
	quantity int
	seq      uint64
}

// stage is where a session is in its life. err is why the session no
// longer writes, nil while filling. View reports it as Halted.
type stage interface{ err() error }

// filling: AH accepts changes from this session.
type filling struct{}

// halted: AH stopped accepting changes (ErrLocked) or is working on another
// order (ErrNotActive). The session reads but no longer writes.
type halted struct{ why error }

// left: AH no longer has the order reopened.
type left struct{}

func (filling) err() error  { return nil }
func (h halted) err() error { return h.why }
func (left) err() error     { return ErrLeft }

func newSession(ah AH, o Order, base Snapshot) *Session {
	return &Session{ah: ah, order: o, stage: filling{}, base: base, wants: map[ProductID]want{}}
}

// View is the current merged picture. It never calls AH, so screens can call
// it on every update.
func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return View{
		Order:   s.order,
		Lines:   merge(s.base, s.wants),
		Total:   s.base.Total,
		Problem: s.problem,
		Halted:  s.stage.err(),
	}
}

// Set asks for an absolute quantity of a product, from 0, which removes it,
// to MaxQuantity. It never calls AH; the next Sync pushes it. Set fails with
// ErrQuantity for any other quantity, and with the session's Halted error
// once the session no longer sends changes.
func (s *Session) Set(p Product, quantity int) error {
	if p.ID == (ProductID{}) {
		return errors.New("set: the product has no id")
	}
	if quantity < 0 || quantity > MaxQuantity {
		return fmt.Errorf("%w, not %d", ErrQuantity, quantity)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.stage.err(); err != nil {
		return err
	}
	w, ok := s.wants[p.ID]
	if !ok {
		s.seq++
		w.seq = s.seq
	}
	w.product, w.quantity = p, quantity
	s.wants[p.ID] = w
	return nil
}

// Sync pushes pending requests to AH and reads AH's truth back. Calls run one
// at a time and each pushes whatever is pending when its turn starts, so
// rapid Set calls coalesce into few writes. The outcome is also in View.
func (s *Session) Sync(ctx context.Context) error {
	s.flight.Lock()
	defer s.flight.Unlock()
	return s.push(ctx)
}

// Search finds catalog products priced for this order's delivery date. A
// blank query returns no hits without calling AH.
func (s *Session) Search(ctx context.Context, query string) ([]Hit, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	if s.isLeft() {
		return nil, ErrLeft
	}
	return s.ah.Search(ctx, s.order.ID, q, searchLimit)
}

// push sends what differs from AH's truth and folds AH's answer back in.
func (s *Session) push(ctx context.Context) error {
	s.mu.Lock()
	if err := s.stage.err(); err != nil {
		s.mu.Unlock()
		return err
	}
	sent := diff(s.base, s.wants)
	maps.DeleteFunc(s.wants, func(id ProductID, _ want) bool {
		_, differs := sent[id]
		return !differs
	})
	s.mu.Unlock()
	if len(sent) == 0 {
		return nil
	}

	putErr := s.ah.SetQuantities(ctx, s.order.ID, sent)
	snap, readErr := s.ah.Snapshot(ctx, s.order.ID)

	s.mu.Lock()
	defer s.mu.Unlock()
	if readErr == nil {
		s.base = snap
	}
	refused := settle(s.base, s.wants, sent, putErr == nil && readErr == nil)
	switch {
	case errors.Is(putErr, ErrLocked):
		s.stage = halted{putErr}
	case errors.Is(readErr, ErrLocked), errors.Is(readErr, ErrNotActive):
		s.stage = halted{readErr}
	}
	s.problem = errors.Join(putErr, readErr, refusedError(refused))
	return s.problem
}

func (s *Session) isLeft() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.stage.(left)
	return ok
}

func (s *Session) markLeft() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stage = left{}
}

// refusal is a request AH accepted the call for but did not apply, for
// example an unorderable product or a capped quantity.
type refusal struct {
	product      Product
	asked, holds int
}

// diff is the absolute quantity to send for every want that differs from
// AH's truth.
func diff(base Snapshot, wants map[ProductID]want) map[ProductID]int {
	out := map[ProductID]int{}
	for id, w := range wants {
		if w.quantity != quantityIn(base, id) {
			out[id] = w.quantity
		}
	}
	return out
}

// settle drops the wants AH's new truth confirms. A clean push that did not
// apply a want becomes a refusal. A want that changed during the push stays.
func settle(base Snapshot, wants map[ProductID]want, sent map[ProductID]int, landed bool) []refusal {
	var out []refusal
	for id, q := range sent {
		w, ok := wants[id]
		if !ok || w.quantity != q {
			continue
		}
		switch held := quantityIn(base, id); {
		case held == q:
			delete(wants, id)
		case landed:
			delete(wants, id)
			out = append(out, refusal{product: w.product, asked: q, holds: held})
		}
	}
	slices.SortFunc(out, func(a, b refusal) int { return strings.Compare(a.product.Title, b.product.Title) })
	return out
}

// merge lays wants over AH's lines: AH's order first, then new products in
// the order they were first asked for. A new product asked for at 0 is
// hidden; a line asked down to 0 stays visible as pending until AH drops it.
func merge(base Snapshot, wants map[ProductID]want) []LineView {
	lines := make([]LineView, 0, len(base.Lines)+len(wants))
	for _, l := range base.Lines {
		v := LineView{Line: l}
		if w, ok := wants[l.ID]; ok && w.quantity != l.Quantity {
			v.Quantity, v.Pending = w.quantity, true
		}
		lines = append(lines, v)
	}
	var added []want
	for id, w := range wants {
		if w.quantity > 0 && quantityIn(base, id) == 0 {
			added = append(added, w)
		}
	}
	slices.SortFunc(added, func(a, b want) int { return cmp.Compare(a.seq, b.seq) })
	for _, w := range added {
		lines = append(lines, LineView{Line: Line{Product: w.product, Quantity: w.quantity}, Pending: true})
	}
	return lines
}

func quantityIn(s Snapshot, id ProductID) int {
	for _, l := range s.Lines {
		if l.ID == id {
			return l.Quantity
		}
	}
	return 0
}

// refusedError words refusals for the status line, e.g. "AH did not add
// Hertog Jan (asked 1)" or "AH kept Optimel at 2 (asked 3)". Nil when there
// are none.
func refusedError(rs []refusal) error {
	errs := make([]error, 0, len(rs))
	for _, r := range rs {
		switch {
		case r.holds == 0:
			errs = append(errs, fmt.Errorf("AH did not add %s (asked %d)", r.product.Title, r.asked))
		case r.asked == 0:
			errs = append(errs, fmt.Errorf("AH did not remove %s", r.product.Title))
		default:
			errs = append(errs, fmt.Errorf("AH kept %s at %d (asked %d)", r.product.Title, r.holds, r.asked))
		}
	}
	return errors.Join(errs...)
}
