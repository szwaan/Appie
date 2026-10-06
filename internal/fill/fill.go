// Package fill keeps one Albert Heijn delivery order open for changes while a
// person fills it, and leaves it so the account never stays on that delivery
// date's bonus prices.
//
// An Account lists the open orders and has at most one of them open. The
// Session it returns holds two things: AH's last confirmed truth about the
// order (a Snapshot) and the absolute quantities the person asked for since.
// Set records a request instantly, Sync pushes requests to AH and reads the
// truth back, and View merges the two for display. Account.Leave pushes what
// is still pending and reverts the order at AH.
//
// fill imports only the standard library and reaches AH through the AH
// interface.
package fill

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// OrderID is AH's order number (a fulfillment's orderId). The zero value is
// no order.
type OrderID struct{ order int }

// NewOrderID is the OrderID for AH's order number n.
func NewOrderID(n int) (OrderID, error) {
	if n <= 0 {
		return OrderID{}, fmt.Errorf("order number %d is not positive", n)
	}
	return OrderID{order: n}, nil
}

// Int is AH's order number.
func (id OrderID) Int() int { return id.order }

func (id OrderID) String() string { return strconv.Itoa(id.order) }

// ProductID is AH's webshop product id (webshopId), the only product id AH's
// order writes accept. Receipt POS ids and hqId are different numbers and
// nothing converts them to a ProductID. The zero value is no product.
type ProductID struct{ product int }

// NewProductID is the ProductID for AH's webshop id n.
func NewProductID(n int) (ProductID, error) {
	if n <= 0 {
		return ProductID{}, fmt.Errorf("product id %d is not positive", n)
	}
	return ProductID{product: n}, nil
}

// Int is AH's webshop id.
func (id ProductID) Int() int { return id.product }

func (id ProductID) String() string { return strconv.Itoa(id.product) }

// Cents is an amount of euro cents.
type Cents int64

func (c Cents) String() string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s€ %d,%02d", sign, c/100, c%100)
}

// Product is what the person sees about one product, priced for the open
// order's delivery date.
type Product struct {
	ID    ProductID
	Title string
	Size  string // "1 L", "500 g"; empty when AH gives none
	Price Cents  // unit price after a per-unit bonus
	Was   Cents  // unit price before a per-unit bonus; zero when none applies
	Bonus string // promotion as AH words it ("25% korting", "2e gratis"); empty when none
}

// Hit is one catalog search result.
type Hit struct {
	Product
	Orderable bool // AH reports it orderable and available online right now
}

// Line is one product in an order as AH holds it.
type Line struct {
	Product
	Quantity int // at least 1
}

// Total is AH's own sum for an order. Per-line prices cannot reproduce it:
// group promotions ("2e gratis") and scratch cards only show up here.
type Total struct {
	Payable  Cents // what AH will charge for the order as it stands
	Discount Cents // bonus discount already taken off Payable
}

// Snapshot is AH's confirmed truth about one order at one moment.
type Snapshot struct {
	Lines []Line // in AH's order
	Total Total
}

// OrderState is what can be done with an open order. The zero value is the
// safe one.
type OrderState int

const (
	// Locked orders no longer accept changes and cannot be opened.
	Locked OrderState = iota
	// Submitted orders accept changes once reopened; Open reopens them.
	Submitted
	// Reopened orders are open for changes at AH right now, so the account is
	// on their delivery date's bonus prices until someone leaves them. Open
	// resumes them without reopening; Leave reverts them.
	Reopened
)

// Order is one of the account's open delivery orders.
type Order struct {
	ID       OrderID
	Slot     string    // AH's wording, e.g. "dinsdag 14 oktober 18:00 - 20:00"
	Delivery time.Time // start of the delivery slot
	Status   string    // AH's description, e.g. "Ingediend"
	Total    Cents     // AH's total when the list was read
	State    OrderState
}

// LineView is a line as the person should see it right now.
type LineView struct {
	Line
	// Pending means AH has not confirmed this quantity yet. Quantity is then
	// what the person asked for, and 0 means the line is being removed.
	Pending bool
}

// View is everything the fill screen shows, merged from AH's truth and the
// person's pending requests.
type View struct {
	Order Order
	// Lines lists AH's lines in AH's order, followed by products the person
	// added that AH has not confirmed yet, in the order they were added.
	Lines []LineView
	// Total is AH's last confirmed total. It is out of date while !Settled().
	Total Total
	// Problem is why AH did not take everything the last push sent, or nil
	// if it did. It stays until a later push sends something.
	Problem error
	// Halted is non-nil once this session no longer sends changes to AH:
	// ErrLocked, ErrNotActive, or ErrLeft. Leaving is the only useful action.
	Halted error
}

// Settled reports whether AH has confirmed every quantity in the view.
func (v View) Settled() bool {
	for _, l := range v.Lines {
		if l.Pending {
			return false
		}
	}
	return true
}

// Quantity is the quantity the person sees for a product, 0 when it is not in
// the order.
func (v View) Quantity(id ProductID) int {
	for _, l := range v.Lines {
		if l.ID == id {
			return l.Quantity
		}
	}
	return 0
}

// Unsaved says what AH has not taken of the person's requests: the last
// push's Problem and every pending line. It is nil once AH holds everything
// the person asked for. After a Leave it is what was not saved.
func (v View) Unsaved() error {
	errs := []error{v.Problem}
	for _, l := range v.Lines {
		switch {
		case !l.Pending:
		case l.Quantity == 0:
			errs = append(errs, fmt.Errorf("removing %s", l.Title))
		default:
			errs = append(errs, fmt.Errorf("%s (asked %d)", l.Title, l.Quantity))
		}
	}
	return errors.Join(errs...)
}

var (
	// ErrSignedOut means AH rejected the stored login; log in again.
	ErrSignedOut = errors.New("AH signed you out")
	// ErrLocked means AH no longer accepts changes to the order, for example
	// because its closing time has passed.
	ErrLocked = errors.New("AH no longer accepts changes to this order")
	// ErrNotActive means AH's active order is a different one, so the session
	// stops writing rather than risk changing the wrong order.
	ErrNotActive = errors.New("AH is working on a different order")
	// ErrOrderNotOpen means the order is not among the account's open orders.
	ErrOrderNotOpen = errors.New("order is not open at AH")
	// ErrAnotherOrderOpen means the Account already has a different order
	// open. Leave it first: AH works on one order per account at a time.
	ErrAnotherOrderOpen = errors.New("another order is still open")
	// ErrLeft means the session's order was left. Open it again to continue.
	ErrLeft = errors.New("order was left")
	// ErrStillReopened means leaving failed and AH may still have the order
	// reopened, so the account may still be on its delivery date's bonus
	// prices. Leaving again is safe.
	ErrStillReopened = errors.New("AH may still have the order reopened")
	// ErrQuantity means Set was asked for a quantity it does not accept.
	ErrQuantity = fmt.Errorf("a quantity runs from 0 to %d", MaxQuantity)
)

// UnsavedError is what Leave and Close return when they left the order but
// AH did not take everything the person asked for.
//
// It does not unwrap to Unsaved. A push that failed before the order was
// left, for example with ErrSignedOut, is not the outcome of leaving.
type UnsavedError struct {
	Order OrderID
	// Unsaved is the session's View.Unsaved at the moment the order was left.
	Unsaved error
}

func (e *UnsavedError) Error() string {
	return fmt.Sprintf("left order %v, but AH did not take: %v", e.Order, e.Unsaved)
}
