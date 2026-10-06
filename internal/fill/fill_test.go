package fill_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/szwaan/appie/internal/ahfake"
	"github.com/szwaan/appie/internal/fill"
)

var (
	milk    = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(1525), Title: "AH Halfvolle melk", Size: "1 L", Price: 115}, Orderable: true}
	yoghurt = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(371880), Title: "Optimel Drinkyoghurt aardbei", Size: "1 L", Price: 119, Was: 159, Bonus: "25% korting"}, Orderable: true}
	beer    = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(220), Title: "Hertog Jan Pilsener", Size: "6 x 300 ml", Price: 549}, Orderable: false}
	catalog = []fill.Hit{milk, yoghurt, beer}
)

var tuesday, friday = ahfake.OrderID(7), ahfake.OrderID(8)

func account(orders ...ahfake.Order) (*fill.Account, *ahfake.AH) {
	fake := ahfake.New(catalog, orders...)
	return fill.NewAccount(fake), fake
}

func submitted(id fill.OrderID, lines map[fill.ProductID]int) ahfake.Order {
	return ahfake.Order{ID: id, State: fill.Submitted, Lines: lines}
}

func open(t *testing.T, a *fill.Account, id fill.OrderID) *fill.Session {
	t.Helper()
	s, err := a.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open(%v): %v", id, err)
	}
	return s
}

func assertOrder(t *testing.T, fake *ahfake.AH, id fill.OrderID, state fill.OrderState, lines map[fill.ProductID]int) {
	t.Helper()
	got := fake.Order(id)
	if got.State != state || !maps.Equal(got.Lines, lines) {
		t.Fatalf("order %v = state %d lines %v, want state %d lines %v", id, got.State, got.Lines, state, lines)
	}
}

// assertUnsaved fails unless err reports that the order was left without
// some of the person's requests, naming each of want.
func assertUnsaved(t *testing.T, err error, want ...string) {
	t.Helper()
	var unsaved *fill.UnsavedError
	if !errors.As(err, &unsaved) {
		t.Fatalf("err = %v, want an *UnsavedError", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("err = %q, want it to name %q", err, w)
		}
	}
}

func TestFillLoopKeepsChangesAndLeavesTheOrder(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, map[fill.ProductID]int{milk.ID: 1}))

	s := open(t, a, tuesday)
	s.Set(milk.Product, 3)
	s.Set(yoghurt.Product, 1)
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	v := s.View()
	if !v.Settled() || v.Quantity(milk.ID) != 3 || v.Quantity(yoghurt.ID) != 1 {
		t.Fatalf("view = %+v, want settled with 3 milk and 1 yoghurt", v.Lines)
	}
	if v.Total != (fill.Total{Payable: 464, Discount: 40}) {
		t.Fatalf("total = %+v, want payable 464 discount 40", v.Total)
	}

	if err := a.Leave(ctx, tuesday); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{milk.ID: 3, yoghurt.ID: 1})
}

func TestOpeningTwiceResumesTheSameSession(t *testing.T) {
	a, _ := account(submitted(tuesday, nil))
	first := open(t, a, tuesday)
	if again := open(t, a, tuesday); again != first {
		t.Fatal("second Open returned a different session")
	}
}

func TestOpeningASecondOrderIsRefused(t *testing.T) {
	a, _ := account(submitted(tuesday, nil), submitted(friday, nil))
	open(t, a, tuesday)
	if _, err := a.Open(context.Background(), friday); !errors.Is(err, fill.ErrAnotherOrderOpen) {
		t.Fatalf("Open(friday) = %v, want ErrAnotherOrderOpen", err)
	}
}

func TestALostReopenReplyStillOpens(t *testing.T) {
	a, fake := account(submitted(tuesday, nil))
	fake.LoseNextReply("Reopen")

	open(t, a, tuesday)
	assertOrder(t, fake, tuesday, fill.Reopened, map[fill.ProductID]int{})
}

func TestAFailedOpenLeavesNothingReopened(t *testing.T) {
	a, fake := account(submitted(tuesday, nil))
	fake.FailNext("Snapshot", errors.New("AH is down"))

	_, err := a.Open(context.Background(), tuesday)
	if err == nil {
		t.Fatal("Open succeeded although AH could not load the order")
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{})
}

func TestQuantitiesAreAbsolute(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)

	s.Set(milk.Product, 2)
	s.Set(milk.Product, 2)
	_ = s.Sync(ctx)
	s.Set(milk.Product, 2)
	_ = s.Sync(ctx)

	if err := a.Leave(ctx, tuesday); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{milk.ID: 2})
}

func TestAChangeMadeWhileAPushIsInFlightIsSentNext(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, map[fill.ProductID]int{milk.ID: 1}))
	s := open(t, a, tuesday)

	entered, release := fake.Hold("SetQuantities")
	s.Set(milk.Product, 3)
	done := make(chan error)
	go func() { done <- s.Sync(ctx) }()
	<-entered
	s.Set(milk.Product, 1)
	release()
	if err := <-done; err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if v := s.View(); v.Settled() || v.Quantity(milk.ID) != 1 {
		t.Fatalf("lines = %+v, want milk pending at 1", v.Lines)
	}

	if err := s.Sync(ctx); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if v := s.View(); !v.Settled() || v.Quantity(milk.ID) != 1 {
		t.Fatalf("lines = %+v, want milk settled at 1", v.Lines)
	}
}

func TestLeavingTwiceIsFine(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, map[fill.ProductID]int{milk.ID: 1}))
	open(t, a, tuesday)

	for i := range 2 {
		if err := a.Leave(ctx, tuesday); err != nil {
			t.Fatalf("Leave #%d: %v", i+1, err)
		}
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{milk.ID: 1})
}

func TestARunThatDiedWhileReopenedCanBeLeftOrResumed(t *testing.T) {
	ctx := context.Background()
	a, fake := account(ahfake.Order{ID: tuesday, State: fill.Reopened, Lines: map[fill.ProductID]int{milk.ID: 1}})

	orders, err := a.Orders(ctx)
	if err != nil || len(orders) != 1 || orders[0].State != fill.Reopened {
		t.Fatalf("Orders = %+v, %v; want the order listed as Reopened", orders, err)
	}

	s := open(t, a, tuesday)
	s.Set(milk.Product, 4)
	if err := a.Leave(ctx, tuesday); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{milk.ID: 4})
}

func TestALostRevertReplyStillCountsAsLeft(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil))
	open(t, a, tuesday)
	fake.LoseNextReply("Revert")

	if err := a.Leave(ctx, tuesday); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{})
}

func TestAFailedRevertKeepsTheOrderOpenUntilRetried(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)
	fake.FailNext("Revert", errors.New("AH is down"))

	if err := a.Leave(ctx, tuesday); err == nil {
		t.Fatal("Leave succeeded although AH did not revert")
	}
	s.Set(milk.Product, 1)
	if err := a.Leave(ctx, tuesday); err != nil {
		t.Fatalf("second Leave: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{milk.ID: 1})
}

func TestAnOrderReopenedElsewhereHaltsWrites(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil), submitted(friday, nil))
	s := open(t, a, tuesday)
	fake.ReopenElsewhere(friday)

	s.Set(milk.Product, 1)
	if err := s.Sync(ctx); !errors.Is(err, fill.ErrNotActive) {
		t.Fatalf("Sync = %v, want ErrNotActive", err)
	}
	s.Set(milk.Product, 5)
	_ = s.Sync(ctx)

	if v := s.View(); !errors.Is(v.Halted, fill.ErrNotActive) {
		t.Fatalf("Halted = %v, want ErrNotActive", v.Halted)
	}
	// The first push landed on friday before the read-back caught it, as it
	// would if AH routes writes by its active order. Nothing after the halt
	// was sent.
	assertOrder(t, fake, friday, fill.Reopened, map[fill.ProductID]int{milk.ID: 1})
	assertUnsaved(t, a.Leave(ctx, tuesday), milk.Title)
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{})
}

func TestARefusedProductIsReportedNotSilentlyDropped(t *testing.T) {
	ctx := context.Background()
	a, _ := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)

	s.Set(beer.Product, 1)
	_ = s.Sync(ctx)

	v := s.View()
	if v.Problem == nil || !strings.Contains(v.Problem.Error(), beer.Title) {
		t.Fatalf("Problem = %v, want it to name %q", v.Problem, beer.Title)
	}
	if !v.Settled() || v.Quantity(beer.ID) != 0 {
		t.Fatalf("lines = %+v, want the refused product gone and the view settled", v.Lines)
	}
}

func TestCloseLeavesTheLiveOrderWithItsPendingChanges(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)
	s.Set(yoghurt.Product, 2)

	for i := range 2 {
		if err := a.Close(ctx); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{yoghurt.ID: 2})
}

func TestLeavingReportsAndKeepsWhatAHDidNotTake(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)
	s.Set(beer.Product, 1)

	err := a.Leave(ctx, tuesday)
	assertUnsaved(t, err, beer.Title)
	if errors.Is(err, fill.ErrStillReopened) {
		t.Fatalf("Leave = %v, want the order left", err)
	}
	if again := a.Leave(ctx, tuesday); again != nil {
		t.Fatalf("second Leave = %v, want nil: the first already reported", again)
	}
	v := s.View()
	if !errors.Is(v.Halted, fill.ErrLeft) || v.Problem == nil || !strings.Contains(v.Problem.Error(), beer.Title) {
		t.Fatalf("view after Leave: halted %v, problem %v; want ErrLeft and a problem naming %q", v.Halted, v.Problem, beer.Title)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{})
}

func TestCloseReportsWhatAHDidNotTake(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)
	s.Set(milk.Product, 2)
	fake.FailNext("SetQuantities", errors.New("AH is down"))

	err := a.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), milk.Title) {
		t.Fatalf("Close = %v, want an error naming %q", err, milk.Title)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{})
}

func TestLeaveReportsWhatAHDidNotTakeTheWayCloseDoes(t *testing.T) {
	ctx := context.Background()
	report := func(end func(*fill.Account) error) string {
		a, fake := account(submitted(tuesday, nil))
		open(t, a, tuesday).Set(milk.Product, 2)
		fake.FailNext("SetQuantities", errors.New("AH is down"))
		err := end(a)
		assertUnsaved(t, err, milk.Title, "AH is down")
		assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{})
		return err.Error()
	}

	left := report(func(a *fill.Account) error { return a.Leave(ctx, tuesday) })
	closed := report(func(a *fill.Account) error { return a.Close(ctx) })
	if left != closed {
		t.Fatalf("Leave reported %q, Close reported %q; want the same report", left, closed)
	}
}

func TestBlankSearchDoesNotReachAH(t *testing.T) {
	a, _ := account(submitted(tuesday, nil))
	s := open(t, a, tuesday)
	hits, err := s.Search(context.Background(), "   ")
	if err != nil || hits != nil {
		t.Fatalf("Search(blank) = %v, %v; want no hits and no error", hits, err)
	}
}

func TestSetTakesZeroToNinetyNine(t *testing.T) {
	ctx := context.Background()
	a, fake := account(submitted(tuesday, map[fill.ProductID]int{milk.ID: 1, yoghurt.ID: 2}))
	s := open(t, a, tuesday)

	for _, q := range []int{-1, 100} {
		if err := s.Set(milk.Product, q); !errors.Is(err, fill.ErrQuantity) {
			t.Fatalf("Set(milk, %d) = %v, want ErrQuantity", q, err)
		}
	}
	if v := s.View(); !v.Settled() || v.Quantity(milk.ID) != 1 {
		t.Fatalf("lines after rejected Sets = %+v, want milk settled at 1", v.Lines)
	}

	if err := s.Set(milk.Product, 99); err != nil {
		t.Fatalf("Set(milk, 99): %v", err)
	}
	if err := s.Set(yoghurt.Product, 0); err != nil {
		t.Fatalf("Set(yoghurt, 0): %v", err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Reopened, map[fill.ProductID]int{milk.ID: 99})
}

func TestZeroIDsNameNothing(t *testing.T) {
	a, fake := account(submitted(tuesday, map[fill.ProductID]int{milk.ID: 1}))
	if _, err := a.Open(context.Background(), fill.OrderID{}); !errors.Is(err, fill.ErrOrderNotOpen) {
		t.Fatalf("Open(zero order) = %v, want ErrOrderNotOpen", err)
	}

	s := open(t, a, tuesday)
	if err := s.Set(fill.Product{Title: "no id"}, 1); err == nil {
		t.Fatal("Set accepted a product without an id")
	}
	if err := a.Leave(context.Background(), tuesday); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	assertOrder(t, fake, tuesday, fill.Submitted, map[fill.ProductID]int{milk.ID: 1})
}

func TestIDConstructorsRefuseNumbersAHNeverUses(t *testing.T) {
	for _, n := range []int{0, -7} {
		if id, err := fill.NewOrderID(n); err == nil {
			t.Errorf("NewOrderID(%d) = %v, want an error", n, id)
		}
		if id, err := fill.NewProductID(n); err == nil {
			t.Errorf("NewProductID(%d) = %v, want an error", n, id)
		}
	}
	order, err := fill.NewOrderID(316501042)
	if err != nil || order.Int() != 316501042 || order.String() != "316501042" {
		t.Errorf("NewOrderID(316501042) = %v (%d), %v", order, order.Int(), err)
	}
	product, err := fill.NewProductID(1525)
	if err != nil || product.Int() != 1525 || product.String() != "1525" {
		t.Errorf("NewProductID(1525) = %v (%d), %v", product, product.Int(), err)
	}
}
