// Package ah implements fill.AH on github.com/gwillem/appie-go. It is the
// only package that imports appie-go; internal/archtest fails the build
// otherwise.
package ah

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata"

	appie "github.com/gwillem/appie-go"

	"github.com/szwaan/appie/internal/fill"
)

// Client is one AH account reached through appie-go.
//
// appie-go keeps a single appie-current-order-id header on its client:
// SetOrderID sets it, GetOrder overwrites it with whatever order AH calls
// active, and only a successful RevertOrder clears it. Client runs one appie
// call at a time and pins the header to the order a method names inside that
// turn, so no request carries a header another call left behind. The turn
// also keeps appie-go's unsynchronised token refresh from running twice.
type Client struct {
	turn   chan struct{} // capacity 1
	api    api
	status *statusRecorder
}

// api is the part of *appie.Client this package relies on.
type api interface {
	IsAuthenticated() bool
	Login(ctx context.Context) error
	GetFulfillments(ctx context.Context) ([]appie.Fulfillment, error)
	ReopenOrder(ctx context.Context, orderID int) error
	RevertOrder(ctx context.Context, orderID int) error
	SetOrderID(id int)
	GetOrderDetails(ctx context.Context, orderID int) (*appie.Order, error)
	GetOrder(ctx context.Context) (*appie.Order, error)
	AddToOrder(ctx context.Context, items []appie.OrderItem) error
	SearchProducts(ctx context.Context, query string, limit int) ([]appie.Product, error)
}

var (
	_ api     = (*appie.Client)(nil)
	_ fill.AH = (*Client)(nil)
)

// Open loads the stored login from configPath, or from appie-go's default
// ($XDG_CONFIG_HOME/appie/config.json, shared with the appie CLI) when
// configPath is empty.
func Open(configPath string) (*Client, error) {
	if configPath == "" {
		configPath = appie.DefaultConfigPath()
	}
	// Login saves the tokens there and does not create the directory.
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return nil, err
	}
	rec := &statusRecorder{next: http.DefaultTransport}
	api, err := appie.NewWithConfig(configPath, appie.WithHTTPClient(&http.Client{Transport: rec, Timeout: 30 * time.Second}))
	if err != nil {
		return nil, err
	}
	return newClient(api, rec), nil
}

func newClient(api api, rec *statusRecorder) *Client {
	return &Client{turn: make(chan struct{}, 1), api: api, status: rec}
}

// SignedIn reports whether a token is stored. That is necessary but not
// sufficient: an expired or anonymous token shows up as fill.ErrSignedOut
// on the first order call.
func (c *Client) SignedIn() bool {
	return c.api.IsAuthenticated()
}

// Login runs appie-go's browser login: a localhost proxy to login.ah.nl
// that saves the tokens. appie-go prints the login URL to stdout, so call it
// before the TUI takes the terminal.
func (c *Client) Login(ctx context.Context) error {
	return c.do(ctx, func() error { return c.api.Login(ctx) })
}

func (c *Client) OpenOrders(ctx context.Context) ([]fill.Order, error) {
	var fulfillments []appie.Fulfillment
	err := c.do(ctx, func() (err error) {
		fulfillments, err = c.api.GetFulfillments(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	orders := make([]fill.Order, 0, len(fulfillments))
	for _, f := range fulfillments {
		o, err := parseOrder(f)
		if err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, nil
}

func (c *Client) Reopen(ctx context.Context, id fill.OrderID) error {
	return c.pinned(ctx, id, func() error { return c.api.ReopenOrder(ctx, id.Int()) })
}

// Revert reverts the order. appie-go clears the header only when AH answers
// SUCCESS; a header left behind is harmless because the next order-scoped
// call pins its own.
func (c *Client) Revert(ctx context.Context, id fill.OrderID) error {
	return c.pinned(ctx, id, func() error { return c.api.RevertOrder(ctx, id.Int()) })
}

func (c *Client) Snapshot(ctx context.Context, id fill.OrderID) (fill.Snapshot, error) {
	var details, summary *appie.Order
	err := c.pinned(ctx, id, func() (err error) {
		if details, err = c.api.GetOrderDetails(ctx, id.Int()); err != nil {
			return err
		}
		// GetOrder rewrites the header to AH's active order; undo that
		// before the turn ends.
		summary, err = c.api.GetOrder(ctx)
		c.api.SetOrderID(id.Int())
		return err
	})
	if err != nil {
		return fill.Snapshot{}, err
	}
	if summary.ID != id.String() {
		return fill.Snapshot{}, fmt.Errorf("%w: AH's active order is %s, not %v", fill.ErrNotActive, summary.ID, id)
	}

	snap := fill.Snapshot{Total: fill.Total{Payable: cents(summary.TotalPrice), Discount: cents(summary.TotalDiscount)}}
	for _, item := range details.Items {
		p, err := parseProduct(*item.Product)
		if err != nil || item.Quantity <= 0 {
			continue // bonus-group entries have product id 0 and are out of scope
		}
		snap.Lines = append(snap.Lines, fill.Line{Product: p, Quantity: item.Quantity})
	}
	return snap, nil
}

// SetQuantities puts absolute quantities. appie-go sums duplicate product
// ids within one call; map keys cannot repeat.
func (c *Client) SetQuantities(ctx context.Context, id fill.OrderID, quantities map[fill.ProductID]int) error {
	items := make([]appie.OrderItem, 0, len(quantities))
	for pid, q := range quantities {
		items = append(items, appie.OrderItem{ProductID: pid.Int(), Quantity: q})
	}
	return c.pinned(ctx, id, func() error { return c.api.AddToOrder(ctx, items) })
}

// Search pins the header too, in case AH prices search by it rather than by
// its active order.
func (c *Client) Search(ctx context.Context, id fill.OrderID, query string, limit int) ([]fill.Hit, error) {
	var products []appie.Product
	err := c.pinned(ctx, id, func() (err error) {
		products, err = c.api.SearchProducts(ctx, query, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	hits := make([]fill.Hit, 0, len(products))
	for _, p := range products {
		product, err := parseProduct(p)
		if err != nil {
			continue
		}
		hits = append(hits, fill.Hit{Product: product, Orderable: p.IsOrderable && p.IsAvailable})
	}
	return hits, nil
}

// pinned runs f as this client's only appie call, with the order header set
// to id whatever an earlier call left there.
func (c *Client) pinned(ctx context.Context, id fill.OrderID, f func() error) error {
	return c.do(ctx, func() error {
		c.api.SetOrderID(id.Int())
		return f()
	})
}

// do runs f as this client's only appie call and classifies its error.
func (c *Client) do(ctx context.Context, f func() error) error {
	select {
	case c.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.turn }()
	c.status.last = 0
	return classify(f(), c.status.last)
}

// statusRecorder remembers the HTTP status of the last response. appie-go's
// error type is unexported, so the status is how this package tells a
// signed-out or locked order apart from other failures. Client.turn guards
// last.
type statusRecorder struct {
	next http.RoundTripper
	last int
}

func (r *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	r.last = 0
	if err == nil {
		r.last = resp.StatusCode
	}
	return resp, err
}

func classify(err error, status int) error {
	if err == nil {
		return nil
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %w", fill.ErrSignedOut, err)
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", fill.ErrLocked, err)
	}
	return err
}

var amsterdam = mustLoad("Europe/Amsterdam")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func parseOrder(f appie.Fulfillment) (fill.Order, error) {
	id, err := fill.NewOrderID(f.OrderID)
	if err != nil {
		return fill.Order{}, fmt.Errorf("AH listed an order without a usable number: %w", err)
	}
	slot := f.Delivery.Slot
	start, _ := time.ParseInLocation("2006-01-02 15:04", slot.Date+" "+slot.StartTime, amsterdam)
	return fill.Order{
		ID:       id,
		Slot:     strings.TrimSpace(slot.DateDisplay + " " + slot.TimeDisplay),
		Delivery: start,
		Status:   f.StatusDescription,
		Total:    cents(f.TotalPrice),
		State:    orderState(f.Status, f.Modifiable),
	}, nil
}

// orderState reads a fulfillment's delivery status, which is not the order's
// state field. A REOPENED order can always be left, even once it is no longer
// modifiable, so it never maps to Locked.
func orderState(deliveryStatus string, modifiable bool) fill.OrderState {
	switch {
	case deliveryStatus == "REOPENED":
		return fill.Reopened
	case modifiable && (deliveryStatus == "SUBMITTED" || deliveryStatus == "CONFIRMED"):
		return fill.Submitted
	default:
		return fill.Locked
	}
}

// parseProduct evens out appie-go's two price conventions: search sets Was to
// the pre-bonus price even when nothing is discounted, order details leave it
// 0 unless AH sent a discounted price. It fails for entries without a webshop
// id, such as bonus groups.
func parseProduct(p appie.Product) (fill.Product, error) {
	id, err := fill.NewProductID(p.ID)
	if err != nil {
		return fill.Product{}, err
	}
	price, was := cents(p.Price.Now), cents(p.Price.Was)
	if was <= price {
		was = 0
	}
	bonus := p.BonusMechanism
	if bonus == "" && p.IsBonus {
		bonus = "Bonus"
	}
	return fill.Product{
		ID:    id,
		Title: p.Title,
		Size:  p.UnitSize,
		Price: price,
		Was:   was,
		Bonus: bonus,
	}, nil
}

func cents(euros float64) fill.Cents {
	return fill.Cents(math.Round(euros * 100))
}
