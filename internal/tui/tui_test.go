package tui

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/szwaan/appie/internal/ahfake"
	"github.com/szwaan/appie/internal/fill"
)

var (
	milk    = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(1525), Title: "AH Halfvolle melk", Size: "1 L", Price: 115}, Orderable: true}
	optimel = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(2001), Title: "Optimel Drinkyoghurt aardbei", Size: "1 L", Price: 119, Was: 159, Bonus: "25% korting"}, Orderable: true}
	carrots = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(3003), Title: "AH Worteltjes", Size: "1 kg", Price: 179, Bonus: "2e gratis"}, Orderable: true}
	beer    = fill.Hit{Product: fill.Product{ID: ahfake.ProductID(4004), Title: "Hertog Jan Pilsener", Size: "6 x 300 ml", Price: 549}}
	catalog = []fill.Hit{milk, optimel, carrots, beer}
	tuesday = ahfake.OrderID(7)
)

func TestTypedDigitsSetOneAbsoluteQuantity(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 3})
	d := opened(t, fake)

	d.press("1", "2")
	d.shows("\n12 × AH Halfvolle melk\n", "0-9 type quantity · enter set · ⌫ erase · esc cancel")
	if got := fake.Order(tuesday).Lines[milk.ID]; got != 3 {
		t.Fatalf("AH holds %d milk before enter, want the 3 it had", got)
	}

	d.press("enter")
	if got := fake.Order(tuesday).Lines[milk.ID]; got != 12 {
		t.Fatalf("AH holds %d milk, want 12", got)
	}
	d.row("▸", "12", "AH Halfvolle melk", "1 L", "€ 1,15")
	d.shows("12 producten · te betalen € 13,80")
}

func TestATypedQuantityCanBeErasedAndCancelled(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 3})
	d := opened(t, fake)

	d.press("enter")
	d.shows("\n 3 × AH Halfvolle melk\n")
	d.press("1", "2", "backspace")
	d.shows("\n 1 × AH Halfvolle melk\n")

	d.press("esc")
	d.lacks("× AH Halfvolle melk", "Leaving")
	d.row("▸", "3", "AH Halfvolle melk")
	if o := fake.Order(tuesday); o.State != fill.Reopened || o.Lines[milk.ID] != 3 {
		t.Fatalf("after esc AH has %+v, want the order still reopened with 3 milk", o)
	}
}

func TestEnterOnAHitStartsFromItsQuantityInTheOrder(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 1, optimel.ID: 3})
	d := opened(t, fake)
	d.shows("\n/ search the catalog")

	d.press("/")
	d.typeText("op")
	d.row("▸", "Optimel Drinkyoghurt aardbei", "1 L", "€ 1,19", "was € 1,59", "25% korting", "in order 3")
	d.press("enter")
	d.shows("\n 3 × Optimel Drinkyoghurt aardbei\n")

	d.press("5", "enter")
	if got := fake.Order(tuesday).Lines[optimel.ID]; got != 5 {
		t.Fatalf("AH holds %d Optimel, want 5", got)
	}
	d.row("▸", "Optimel Drinkyoghurt aardbei", "1 L", "€ 1,19", "was € 1,59", "25% korting", "in order 5")
}

func TestEnterOnAHitNotInTheOrderStartsFromOne(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 1})
	d := opened(t, fake)

	d.press("/")
	d.typeText("wo")
	d.press("enter")
	d.shows("\n 1 × AH Worteltjes\n")

	d.press("enter", "esc")
	if got := fake.Order(tuesday).Lines[carrots.ID]; got != 1 {
		t.Fatalf("AH holds %d carrots, want 1", got)
	}
	d.row("▸", "1", "AH Worteltjes", "1 kg", "€ 1,79", "2e gratis")
}

func TestAnUndeliverableHitIsListedButCannotBeAdded(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 1})
	d := opened(t, fake)

	d.press("/")
	d.typeText("he")
	d.row("▸", "Hertog Jan Pilsener", "6 x 300 ml", "€ 5,49", "niet leverbaar")

	d.press("enter")
	d.shows("AH can't deliver Hertog Jan Pilsener right now")
	d.lacks("× Hertog Jan Pilsener")
	if got := fake.Order(tuesday).Lines; len(got) != 1 || got[milk.ID] != 1 {
		t.Fatalf("AH holds %v, want only the 1 milk", got)
	}

	d.press("up")
	d.lacks("AH can't deliver")
}

func TestAQuantityStopsAtNinetyNine(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 1})
	d := opened(t, fake)

	d.press("9", "9", "9")
	d.shows("\n99 × AH Halfvolle melk\n", "a quantity runs from 0 to 99")

	d.press("enter")
	if got := fake.Order(tuesday).Lines[milk.ID]; got != 99 {
		t.Fatalf("AH holds %d milk, want 99", got)
	}
}

func TestZeroRemovesALineAndUndoBringsItBack(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 3, optimel.ID: 1})
	d := opened(t, fake)

	d.press("0", "enter")
	if got, ok := fake.Order(tuesday).Lines[milk.ID]; ok {
		t.Fatalf("AH still holds %d milk after 0", got)
	}
	d.lacks("AH Halfvolle melk")
	d.shows("u undo remove")

	d.press("u")
	if got := fake.Order(tuesday).Lines[milk.ID]; got != 3 {
		t.Fatalf("AH holds %d milk after undo, want the 3 it had", got)
	}
	d.row("3", "AH Halfvolle melk")
}

func TestLeavingNamesWhatAHDidNotTake(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 1})
	d := opened(t, fake)
	down := errors.New("AH is down")
	fake.FailNext("SetQuantities", down)
	fake.FailNext("SetQuantities", down)

	d.press("+")
	d.row("▸", "2!", "AH Halfvolle melk")
	d.shows("AH is down", "not all saved", "r retry")

	d.press("esc")
	d.shows("Some changes did not reach AH.", "y leave anyway · r retry · any other key stay")
	d.press("y")
	d.shows("Left the order. AH did not take:\n  AH is down\n  AH Halfvolle melk (asked 2)\nany key")
	if o := fake.Order(tuesday); o.State != fill.Submitted || o.Lines[milk.ID] != 1 {
		t.Fatalf("AH has %+v, want the order left with its 1 milk", o)
	}

	d.press("x")
	d.row("▸", "slot of order 7", "€ 1,15", "state 1")
}

func TestQuitPushesAndLeavesTheOrder(t *testing.T) {
	fake := submitted(map[fill.ProductID]int{milk.ID: 1})
	d := opened(t, fake)

	d.press("+", "q")
	if !d.quit {
		t.Fatal("the program did not quit")
	}
	if o := fake.Order(tuesday); o.State != fill.Submitted || o.Lines[milk.ID] != 2 {
		t.Fatalf("AH has %+v, want the order left with 2 milk", o)
	}
}

func TestTheListOffersToLeaveAReopenedOrder(t *testing.T) {
	fake := ahfake.New(catalog, ahfake.Order{ID: tuesday, State: fill.Reopened, Lines: map[fill.ProductID]int{milk.ID: 1}})
	d := start(t, fake)
	d.row("▸", "slot of order 7", "€ 1,15", "state 2", "Reopened: your account is on this date's bonus prices", "enter resume · x leave")

	d.press("x")
	if st := fake.Order(tuesday).State; st != fill.Submitted {
		t.Fatalf("order is in state %d after x, want Submitted", st)
	}
	d.row("▸", "slot of order 7", "€ 1,15", "state 1")
	d.lacks("Reopened")
}

func TestAFailedLeaveFromTheListStaysOnScreen(t *testing.T) {
	fake := ahfake.New(catalog, ahfake.Order{ID: tuesday, State: fill.Reopened, Lines: map[fill.ProductID]int{milk.ID: 1}})
	d := start(t, fake)
	fake.FailNext("Revert", errors.New("AH is down"))

	d.press("x")
	d.shows("AH may still have the order reopened: order 7: AH is down")
	d.row("▸", "slot of order 7", "€ 1,15", "state 2", "Reopened: your account is on this date's bonus prices")
	if st := fake.Order(tuesday).State; st != fill.Reopened {
		t.Fatalf("order is in state %d after a failed revert, want Reopened", st)
	}
}

func submitted(lines map[fill.ProductID]int) *ahfake.AH {
	return ahfake.New(catalog, ahfake.Order{ID: tuesday, State: fill.Submitted, Lines: lines})
}

type driver struct {
	t     *testing.T
	model tea.Model
	quit  bool
}

func start(t *testing.T, fake *ahfake.AH) *driver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := model{ctx: ctx, acct: fill.NewAccount(fake), screen: &ordersScreen{busy: "Loading orders…"}}
	d := &driver{t: t, model: m}
	d.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, msg := range d.run(m.Init()) {
		d.send(msg)
	}
	return d
}

func opened(t *testing.T, fake *ahfake.AH) *driver {
	t.Helper()
	d := start(t, fake)
	d.press("enter")
	d.shows("slot of order 7 · order 7")
	return d
}

func (d *driver) send(msg tea.Msg) {
	queue := []tea.Msg{msg}
	for len(queue) > 0 && !d.quit {
		next, cmd := d.model.Update(queue[0])
		d.model = next
		queue = append(queue[1:], d.run(cmd)...)
	}
}

// run runs cmd, and every command it batches, to the messages they return.
func (d *driver) run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case nil:
		return nil
	case tea.QuitMsg:
		d.quit = true
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range msg {
			out = append(out, d.run(c)...)
		}
		return out
	default:
		return []tea.Msg{msg}
	}
}

func (d *driver) press(names ...string) {
	special := map[string]tea.Key{
		"enter":     {Code: tea.KeyEnter},
		"esc":       {Code: tea.KeyEscape},
		"backspace": {Code: tea.KeyBackspace},
	}
	for _, name := range names {
		k, ok := special[name]
		if !ok {
			k = tea.Key{Code: []rune(name)[0], Text: name}
		}
		d.send(tea.KeyPressMsg(k))
	}
}

func (d *driver) typeText(s string) {
	for _, r := range s {
		d.press(string(r))
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]`)

func (d *driver) screen() string {
	return ansi.ReplaceAllString(d.model.View().Content, "")
}

func (d *driver) shows(want ...string) {
	d.t.Helper()
	s := d.screen()
	for _, w := range want {
		if !strings.Contains(s, w) {
			d.t.Fatalf("screen lacks %q:\n%s", w, s)
		}
	}
}

func (d *driver) lacks(unwanted ...string) {
	d.t.Helper()
	s := d.screen()
	for _, u := range unwanted {
		if strings.Contains(s, u) {
			d.t.Fatalf("screen shows %q:\n%s", u, s)
		}
	}
}

// row fails unless one screen line holds cells in this order, with only
// spaces between them.
func (d *driver) row(cells ...string) {
	d.t.Helper()
	quoted := make([]string, len(cells))
	for i, c := range cells {
		quoted[i] = regexp.QuoteMeta(c)
	}
	re := regexp.MustCompile(strings.Join(quoted, " +"))
	s := d.screen()
	for line := range strings.SplitSeq(s, "\n") {
		if re.MatchString(line) {
			return
		}
	}
	d.t.Fatalf("no screen line holds %q:\n%s", cells, s)
}
