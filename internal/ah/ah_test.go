package ah

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appie "github.com/gwillem/appie-go"

	"github.com/szwaan/appie/internal/ahfake"
	"github.com/szwaan/appie/internal/fill"
)

func TestOrderStateReadsDeliveryStatus(t *testing.T) {
	cases := []struct {
		status     string
		modifiable bool
		want       fill.OrderState
	}{
		{"SUBMITTED", true, fill.Submitted},
		{"CONFIRMED", true, fill.Submitted},
		{"REOPENED", true, fill.Reopened},
		{"REOPENED", false, fill.Reopened},
		{"SUBMITTED", false, fill.Locked},
		{"IN_DELIVERY", true, fill.Locked},
		{"", true, fill.Locked},
	}
	for _, c := range cases {
		if got := orderState(c.status, c.modifiable); got != c.want {
			t.Errorf("orderState(%q, %v) = %d, want %d", c.status, c.modifiable, got, c.want)
		}
	}
}

func TestParseProductEvensOutPriceConventions(t *testing.T) {
	cases := []struct {
		name string
		in   appie.Product
		want fill.Product
	}{
		{
			name: "search hit without discount reports Was equal to Now",
			in:   appie.Product{ID: 1525, Title: "AH Halfvolle melk", UnitSize: "1 L", Price: appie.Price{Now: 1.15, Was: 1.15}},
			want: fill.Product{ID: ahfake.ProductID(1525), Title: "AH Halfvolle melk", Size: "1 L", Price: 115},
		},
		{
			name: "order line with a per-unit discount",
			in:   appie.Product{ID: 371880, Title: "Optimel", Price: appie.Price{Now: 1.19, Was: 1.59}, IsBonus: true, BonusMechanism: "25% korting"},
			want: fill.Product{ID: ahfake.ProductID(371880), Title: "Optimel", Price: 119, Was: 159, Bonus: "25% korting"},
		},
		{
			name: "group promotion has no per-unit price cut",
			in:   appie.Product{ID: 4, Title: "Worteltjes", Price: appie.Price{Now: 1.79}, IsBonus: true, BonusMechanism: "2e gratis"},
			want: fill.Product{ID: ahfake.ProductID(4), Title: "Worteltjes", Price: 179, Bonus: "2e gratis"},
		},
		{
			name: "bonus without wording",
			in:   appie.Product{ID: 5, Title: "Kaas", Price: appie.Price{Now: 3.00}, IsBonus: true},
			want: fill.Product{ID: ahfake.ProductID(5), Title: "Kaas", Price: 300, Bonus: "Bonus"},
		},
	}
	for _, c := range cases {
		if got, err := parseProduct(c.in); err != nil || got != c.want {
			t.Errorf("%s: parseProduct = %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	if got, err := parseProduct(appie.Product{Title: "Alle Hak*", BonusSegmentID: "123"}); err == nil {
		t.Errorf("parseProduct(bonus group) = %+v, want an error: it has no webshop id", got)
	}
}

func TestCentsRoundsFloatEuros(t *testing.T) {
	for in, want := range map[float64]fill.Cents{1.15: 115, 0.1 + 0.2: 30, 92.68: 9268, 0: 0} {
		if got := cents(in); got != want {
			t.Errorf("cents(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestClassifyUsesTheHTTPStatus(t *testing.T) {
	boom := errors.New("API error")
	if err := classify(boom, http.StatusUnauthorized); !errors.Is(err, fill.ErrSignedOut) || !errors.Is(err, boom) {
		t.Errorf("401 → %v, want ErrSignedOut wrapping the cause", err)
	}
	if err := classify(boom, http.StatusConflict); !errors.Is(err, fill.ErrLocked) {
		t.Errorf("409 → %v, want ErrLocked", err)
	}
	if err := classify(boom, http.StatusBadGateway); err != boom {
		t.Errorf("502 → %v, want the cause unchanged", err)
	}
	if err := classify(nil, http.StatusUnauthorized); err != nil {
		t.Errorf("nil error → %v, want nil", err)
	}
}

// fakeAPI serves the appie-go endpoints the adapter uses and records what
// reached it.
type fakeAPI struct {
	mu           sync.Mutex
	activeID     int // what AH's active-order summary reports
	fulfillments []map[string]any
	products     []map[string]any
	headers      []string
	puts         [][]map[string]any
	status       int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, r.Method+" "+r.URL.Path+" order="+r.Header.Get("appie-current-order-id"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/details-grouped-by-taxonomy"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"orderId": 7,
			"groupedProductsInTaxonomy": []any{map[string]any{
				"orderedProducts": []any{map[string]any{"quantity": 2, "product": map[string]any{
					"webshopId": 1525, "title": "AH Halfvolle melk", "salesUnitSize": "1 L", "priceBeforeBonus": 1.15,
				}}},
			}},
		})
	case r.URL.Path == "/mobile-services/order/v1/summaries/active":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         f.activeID,
			"totalPrice": map[string]any{"priceTotalPayable": 2.30, "priceDiscount": 0},
		})
	case r.URL.Path == "/mobile-services/order/v1/items" && r.Method == http.MethodPut:
		var body struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.puts = append(f.puts, body.Items)
	case r.URL.Path == "/graphql":
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var data map[string]any
		switch {
		case strings.Contains(body.Query, "orderFulfillments"):
			data = map[string]any{"orderFulfillments": map[string]any{"result": f.fulfillments}}
		case strings.Contains(body.Query, "orderReopen"):
			data = map[string]any{"orderReopen": map[string]any{"status": "SUCCESS"}}
		case strings.Contains(body.Query, "orderRevert"):
			data = map[string]any{"orderRevert": map[string]any{"status": "SUCCESS"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case r.URL.Path == "/mobile-services/product/search/v2":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"products": f.products,
			"page":     map[string]any{"totalElements": len(f.products)},
		})
	default:
		http.NotFound(w, r)
	}
}

func testClient(t *testing.T, f *fakeAPI) *Client {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	rec := &statusRecorder{next: http.DefaultTransport}
	api := appie.New(appie.WithBaseURL(srv.URL), appie.WithTokens("access", "refresh"), appie.WithHTTPClient(&http.Client{Transport: rec}))
	return newClient(api, rec)
}

var seven = ahfake.OrderID(7)

func TestSnapshotOfANonActiveOrderFailsAndWritesStayPinned(t *testing.T) {
	ctx := context.Background()
	f := &fakeAPI{activeID: 8}
	c := testClient(t, f)

	_, err := c.Snapshot(ctx, seven)
	if !errors.Is(err, fill.ErrNotActive) {
		t.Fatalf("Snapshot = %v, want ErrNotActive", err)
	}
	if err := c.SetQuantities(ctx, seven, map[fill.ProductID]int{ahfake.ProductID(1525): 3}); err != nil {
		t.Fatalf("SetQuantities: %v", err)
	}

	last := f.headers[len(f.headers)-1]
	if last != "PUT /mobile-services/order/v1/items order=7" {
		t.Fatalf("last request = %q, want the PUT pinned to order 7 although GetOrder saw order 8", last)
	}
	if len(f.puts) != 1 || len(f.puts[0]) != 1 || f.puts[0][0]["productId"] != 1525.0 || f.puts[0][0]["quantity"] != 3.0 {
		t.Fatalf("PUT items = %v, want one absolute quantity 3 for 1525", f.puts)
	}
}

func TestSnapshotOfTheActiveOrderCarriesAHsTotal(t *testing.T) {
	c := testClient(t, &fakeAPI{activeID: 7})
	snap, err := c.Snapshot(context.Background(), seven)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := fill.Snapshot{
		Lines: []fill.Line{{Product: fill.Product{ID: ahfake.ProductID(1525), Title: "AH Halfvolle melk", Size: "1 L", Price: 115}, Quantity: 2}},
		Total: fill.Total{Payable: 230},
	}
	if len(snap.Lines) != 1 || snap.Lines[0] != want.Lines[0] || snap.Total != want.Total {
		t.Fatalf("Snapshot = %+v, want %+v", snap, want)
	}
}

func TestA401IsReportedAsSignedOut(t *testing.T) {
	c := testClient(t, &fakeAPI{status: http.StatusUnauthorized})
	_, err := c.OpenOrders(context.Background())
	if !errors.Is(err, fill.ErrSignedOut) {
		t.Fatalf("OpenOrders = %v, want ErrSignedOut", err)
	}
}

// loginFailsLocally is appie-go with a Login that fails before sending a
// request, as it does when its localhost login server cannot listen.
type loginFailsLocally struct{ *appie.Client }

func (loginFailsLocally) Login(context.Context) error {
	return errors.New("failed to start login server")
}

func TestAFailureThatSentNothingIsNotReadByTheLastResponse(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(&fakeAPI{status: http.StatusUnauthorized})
	t.Cleanup(srv.Close)
	rec := &statusRecorder{next: http.DefaultTransport}
	api := appie.New(appie.WithBaseURL(srv.URL), appie.WithTokens("access", "refresh"), appie.WithHTTPClient(&http.Client{Transport: rec}))
	c := newClient(loginFailsLocally{api}, rec)

	if _, err := c.OpenOrders(ctx); !errors.Is(err, fill.ErrSignedOut) {
		t.Fatalf("OpenOrders = %v, want ErrSignedOut", err)
	}
	err := c.Login(ctx)
	if err == nil || errors.Is(err, fill.ErrSignedOut) {
		t.Fatalf("Login = %v, want its own failure, not the 401 OpenOrders got", err)
	}
}

func TestEveryCallAfterASnapshotCarriesTheNamedOrder(t *testing.T) {
	ctx := context.Background()
	f := &fakeAPI{activeID: 8, fulfillments: []map[string]any{{"orderId": 7, "delivery": map[string]any{"status": "REOPENED"}}}}
	c := testClient(t, f)

	if _, err := c.Snapshot(ctx, seven); !errors.Is(err, fill.ErrNotActive) {
		t.Fatalf("Snapshot = %v, want ErrNotActive", err)
	}
	if _, err := c.OpenOrders(ctx); err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	if err := c.Reopen(ctx, seven); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if _, err := c.Search(ctx, seven, "melk", 5); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if err := c.Revert(ctx, seven); err != nil {
		t.Fatalf("Revert: %v", err)
	}

	want := []string{
		"GET /mobile-services/order/v1/7/details-grouped-by-taxonomy order=7",
		"GET /mobile-services/order/v1/summaries/active order=7",
		"POST /graphql order=7",
		"POST /graphql order=7",
		"GET /mobile-services/product/search/v2 order=7",
		"POST /graphql order=7",
	}
	if !slices.Equal(f.headers, want) {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(f.headers, "\n"), strings.Join(want, "\n"))
	}
}

func TestOpenOrdersParsesFulfillments(t *testing.T) {
	f := &fakeAPI{fulfillments: []map[string]any{{
		"orderId":           316501042,
		"statusDescription": "Ingediend",
		"modifiable":        true,
		"totalPrice":        map[string]any{"totalPrice": map[string]any{"amount": 92.68}},
		"delivery": map[string]any{"status": "SUBMITTED", "slot": map[string]any{
			"date": "2026-10-14", "dateDisplay": "dinsdag 14 oktober", "timeDisplay": "18:00 - 20:00", "startTime": "18:00",
		}},
	}}}
	orders, err := testClient(t, f).OpenOrders(context.Background())
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	want := fill.Order{
		ID:       ahfake.OrderID(316501042),
		Slot:     "dinsdag 14 oktober 18:00 - 20:00",
		Delivery: time.Date(2026, 10, 14, 18, 0, 0, 0, amsterdam),
		Status:   "Ingediend",
		Total:    9268,
		State:    fill.Submitted,
	}
	if len(orders) != 1 || orders[0] != want {
		t.Fatalf("OpenOrders = %+v, want [%+v]", orders, want)
	}
}

func TestAnOrderWithoutANumberFailsTheList(t *testing.T) {
	f := &fakeAPI{fulfillments: []map[string]any{{"statusDescription": "Ingediend", "delivery": map[string]any{"status": "SUBMITTED"}}}}
	if orders, err := testClient(t, f).OpenOrders(context.Background()); err == nil {
		t.Fatalf("OpenOrders = %+v, want an error rather than an order nobody can open", orders)
	}
}

func TestSearchKeepsAHsOrderAndUnavailableHits(t *testing.T) {
	f := &fakeAPI{products: []map[string]any{
		{"webshopId": 220, "title": "Hertog Jan Pilsener", "salesUnitSize": "6 x 300 ml", "priceBeforeBonus": 5.49, "isOrderable": true, "availableOnline": false},
		{"webshopId": 0, "title": "Alle Hak*", "isBonus": true},
		{"webshopId": 1525, "title": "AH Halfvolle melk", "salesUnitSize": "1 L", "priceBeforeBonus": 1.15, "isOrderable": true, "availableOnline": true},
	}}
	hits, err := testClient(t, f).Search(context.Background(), seven, "pils", 25)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []fill.Hit{
		{Product: fill.Product{ID: ahfake.ProductID(220), Title: "Hertog Jan Pilsener", Size: "6 x 300 ml", Price: 549}, Orderable: false},
		{Product: fill.Product{ID: ahfake.ProductID(1525), Title: "AH Halfvolle melk", Size: "1 L", Price: 115}, Orderable: true},
	}
	if !slices.Equal(hits, want) {
		t.Fatalf("Search = %+v, want %+v", hits, want)
	}
}

func TestOpenReadsTokensFromTheAppieConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appie", "config.json")
	c, err := Open(path)
	if err != nil {
		t.Fatalf("Open without a config file: %v", err)
	}
	if c.SignedIn() {
		t.Fatal("SignedIn with no stored token")
	}
	if err := os.WriteFile(path, []byte(`{"access_token": "a", "refresh_token": "r"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(path); err != nil || !c.SignedIn() {
		t.Fatalf("Open with stored tokens: signed in %v, %v", err == nil && c.SignedIn(), err)
	}
}
