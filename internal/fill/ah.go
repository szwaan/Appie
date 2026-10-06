package fill

import "context"

// AH is the Albert Heijn account as filling needs it. internal/ah implements
// it on appie-go; internal/ahfake implements it in memory for tests.
//
// Every order-scoped method names its order. An implementation must make each
// call act on that order and nothing else, whatever client-side state its
// transport keeps (appie-go keeps one order header that other calls rewrite).
type AH interface {
	// OpenOrders lists the account's open delivery orders.
	OpenOrders(ctx context.Context) ([]Order, error)

	// Reopen asks AH to accept changes to a Submitted order. From then on the
	// account is on that order's delivery-date bonus prices until Revert
	// succeeds.
	Reopen(ctx context.Context, id OrderID) error

	// Revert ends AH's reopen of the order and puts the account back on
	// today's prices. It fails when the order is not reopened.
	Revert(ctx context.Context, id OrderID) error

	// Snapshot reads the order's lines with unit prices and AH's payable
	// total. It fails with ErrNotActive when AH's active order is another
	// one, because then the total, and possibly writes, belong to that order.
	Snapshot(ctx context.Context, id OrderID) (Snapshot, error)

	// SetQuantities makes AH hold exactly these quantities; 0 removes the
	// line. Quantities are absolute, so repeating a call changes nothing.
	// The map makes a duplicate product unrepresentable, which matters
	// because AH sums duplicates within one call. Success does not mean AH
	// applied every quantity: read a Snapshot to find out.
	SetQuantities(ctx context.Context, id OrderID, quantities map[ProductID]int) error

	// Search returns up to limit catalog hits in AH's relevance order, priced
	// for the order's delivery date. The query is never blank.
	Search(ctx context.Context, id OrderID, query string, limit int) ([]Hit, error)
}
