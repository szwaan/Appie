package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/szwaan/appie/internal/fill"
)

const (
	titleWidth = 40
	hitsShown  = 8
)

// orderScreen fills one open order. Keys act instantly on the session's
// view; pushes to AH follow a moment later and the footer total says
// "updating…" until AH confirms.
type orderScreen struct {
	session *fill.Session
	shown   fill.View // the session's View, re-read after every message

	searching bool // typing goes to the search box; otherwise keys act on lines
	input     textinput.Model
	hits      []fill.Hit
	hitCursor int
	notice    string // one-shot hint, e.g. "AH can't deliver this right now"; the next key clears it

	cursor fill.ProductID // the line under the cursor; follows its product when lines move
	entry  *entry         // a quantity being typed; it takes every key until enter or esc
	undo   *fill.Line     // the last line removed, at the quantity it had

	syncGen, searchGen int
	cancelSearch       context.CancelFunc

	dialog dialog
}

// entry is a quantity being typed for one product. It starts out holding a
// quantity, which the first digit typed replaces.
type entry struct {
	product   fill.Product
	digits    string
	prefilled bool // digits is the starting quantity, not typed
}

// typeDigit adds d to the quantity, or refuses it when the quantity would
// pass fill.MaxQuantity.
func (e *entry) typeDigit(d string) bool {
	next := e.digits + d
	if e.prefilled {
		next = d
	}
	n, err := strconv.Atoi(next)
	if err != nil || n > fill.MaxQuantity {
		return false
	}
	e.digits, e.prefilled = strconv.Itoa(n), false
	return true
}

func (e *entry) erase() {
	switch {
	case e.prefilled:
		e.digits = ""
	case e.digits != "":
		e.digits = e.digits[:len(e.digits)-1]
	}
	e.prefilled = false
}

// dialog is what the fill screen asks or shows on top of the lines.
type dialog interface{ isDialog() }

type (
	noDialog struct{}
	// confirmLeave: a push failed and changes are still pending.
	confirmLeave struct{ quit bool }
	leaving      struct{ quit bool }
	// leaveFailed: AH may still have the order reopened.
	leaveFailed struct {
		err  error
		quit bool
	}
	// leftUnsaved: the order was left without some requests; the dialog
	// shows what, from Leave's report, until the person moves on.
	leftUnsaved struct {
		what error
		quit bool
	}
)

func (noDialog) isDialog()     {}
func (confirmLeave) isDialog() {}
func (leaving) isDialog()      {}
func (leaveFailed) isDialog()  {}
func (leftUnsaved) isDialog()  {}

func newOrderScreen(s *fill.Session) *orderScreen {
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = "search the catalog"
	// Without a width the input shows only the placeholder's first letter.
	in.SetWidth(40)
	// update hands the input key presses only, so a blinking cursor would
	// never get its next blink.
	st := in.Styles()
	st.Cursor.Blink = false
	in.SetStyles(st)
	return &orderScreen{session: s, shown: s.View(), input: in, dialog: noDialog{}}
}

func (o *orderScreen) update(m *model, msg tea.Msg) (screen, tea.Cmd) {
	defer func() { o.shown = o.session.View() }()

	switch msg := msg.(type) {
	case syncDueMsg:
		if msg.gen == o.syncGen {
			return o, o.sync(m)
		}
	case syncedMsg:
		// the deferred re-read picks up the outcome
	case searchDueMsg:
		if msg.gen == o.searchGen {
			return o, o.search(m, msg.query)
		}
	case hitsMsg:
		if msg.query != o.input.Value() || errors.Is(msg.err, context.Canceled) {
			break // superseded by a newer search
		}
		o.hits, o.hitCursor = msg.hits, 0
		if msg.err != nil {
			o.notice = msg.err.Error()
		}
	case leftMsg:
		var unsaved *fill.UnsavedError
		switch {
		case msg.err == nil:
			return o.done(m, msg.quit)
		case errors.As(msg.err, &unsaved):
			o.dialog = leftUnsaved{what: unsaved.Unsaved, quit: msg.quit}
		default:
			o.dialog = leaveFailed{err: msg.err, quit: msg.quit}
		}
	case tea.KeyPressMsg:
		o.notice = ""
		return o.key(m, msg)
	}
	return o, nil
}

func (o *orderScreen) key(m *model, k tea.KeyPressMsg) (screen, tea.Cmd) {
	switch d := o.dialog.(type) {
	case leaving:
		if key.Matches(k, keys.Interrupt) {
			return o, tea.Quit // main's Close finishes leaving
		}
		return o, nil
	case confirmLeave:
		o.dialog = noDialog{}
		switch {
		case key.Matches(k, keys.Confirm):
			return o.leaveNow(m, d.quit)
		case key.Matches(k, keys.Retry):
			return o, o.sync(m)
		}
		return o, nil
	case leaveFailed:
		switch {
		case key.Matches(k, keys.Retry):
			return o.leaveNow(m, d.quit)
		case d.quit && key.Matches(k, keys.QuitAnyway):
			return o, tea.Quit // main's Close tries once more and says what is still reopened
		case key.Matches(k, keys.Lines):
			o.dialog = noDialog{}
		}
		return o, nil
	case leftUnsaved:
		return o.done(m, d.quit)
	case noDialog:
	}

	if key.Matches(k, keys.Interrupt) {
		o.entry = nil
		return o.leave(m, true)
	}
	if o.entry != nil {
		return o, o.entryKey(k)
	}
	if o.searching {
		return o.searchKey(m, k)
	}

	line, onLine := o.line()
	switch {
	case key.Matches(k, keys.Quit):
		return o.leave(m, true)
	case key.Matches(k, keys.Search):
		o.searching = true
		return o, o.input.Focus()
	case key.Matches(k, keys.LeaveOrder):
		return o.leave(m, false)
	case key.Matches(k, keys.Retry) && o.retryable():
		return o, o.sync(m)
	case key.Matches(k, keys.Up):
		o.move(-1)
	case key.Matches(k, keys.Down):
		o.move(+1)
	case key.Matches(k, keys.More) && onLine:
		return o, o.set(line.Product, line.Quantity+1)
	case key.Matches(k, keys.Less) && onLine:
		return o, o.set(line.Product, max(line.Quantity-1, 0))
	case key.Matches(k, keys.Digit) && onLine:
		o.entry = &entry{product: line.Product}
		o.entry.typeDigit(k.String())
	case key.Matches(k, keys.Edit) && onLine:
		o.entry = &entry{product: line.Product, digits: strconv.Itoa(line.Quantity), prefilled: true}
	case key.Matches(k, keys.Remove) && onLine:
		return o, o.set(line.Product, 0)
	case key.Matches(k, keys.Undo) && o.undo != nil:
		u := *o.undo
		o.undo = nil
		return o, o.set(u.Product, u.Quantity)
	}
	return o, nil
}

// entryKey handles keys while a quantity is being typed. Other keys do
// nothing, so a key meant for the entry cannot act on a line.
func (o *orderScreen) entryKey(k tea.KeyPressMsg) tea.Cmd {
	e := o.entry
	switch {
	case key.Matches(k, keys.Digit):
		if !e.typeDigit(k.String()) {
			o.notice = fill.ErrQuantity.Error()
		}
	case key.Matches(k, keys.Erase):
		e.erase()
	case key.Matches(k, keys.Cancel):
		o.entry = nil
	case key.Matches(k, keys.Commit):
		o.entry = nil
		q, err := strconv.Atoi(e.digits)
		if err != nil {
			return nil // everything typed was erased
		}
		o.cursor = e.product.ID
		return o.set(e.product, q)
	}
	return nil
}

// searchKey handles keys while the search box has focus. Printable keys
// always go to the box, so only non-printable keys act on the hits.
func (o *orderScreen) searchKey(m *model, k tea.KeyPressMsg) (screen, tea.Cmd) {
	switch {
	case key.Matches(k, keys.Lines):
		o.searching = false
		o.input.Blur()
		return o, nil
	case key.Matches(k, keys.Up):
		o.hitCursor = max(o.hitCursor-1, 0)
		return o, nil
	case key.Matches(k, keys.Down):
		o.hitCursor = max(min(o.hitCursor+1, len(o.hits)-1), 0)
		return o, nil
	case key.Matches(k, keys.Edit):
		if o.hitCursor >= len(o.hits) {
			return o, nil
		}
		h := o.hits[o.hitCursor]
		if !h.Orderable {
			o.notice = "AH can't deliver " + h.Title + " right now"
			return o, nil
		}
		o.entry = &entry{product: h.Product, digits: strconv.Itoa(cmp.Or(o.shown.Quantity(h.ID), 1)), prefilled: true}
		return o, nil
	}
	before := o.input.Value()
	var cmd tea.Cmd
	o.input, cmd = o.input.Update(k)
	if q := o.input.Value(); q != before {
		return o, tea.Batch(cmd, o.searchSoon(q))
	}
	return o, cmd
}

// set records the request on the session at once and schedules a push.
func (o *orderScreen) set(p fill.Product, quantity int) tea.Cmd {
	had := o.shown.Quantity(p.ID)
	if err := o.session.Set(p, quantity); err != nil {
		o.notice = err.Error()
		return nil
	}
	if quantity == 0 && had > 0 {
		o.undo = &fill.Line{Product: p, Quantity: had}
	}
	o.syncGen++
	gen := o.syncGen
	return tea.Tick(syncDelay, func(time.Time) tea.Msg { return syncDueMsg{gen} })
}

func (o *orderScreen) sync(m *model) tea.Cmd {
	s := o.session
	return call(m.ctx, func(ctx context.Context) (tea.Msg, error) {
		return syncedMsg{}, s.Sync(ctx) // the outcome is in the session's View
	})
}

func (o *orderScreen) retryable() bool {
	v := o.shown
	return v.Problem != nil && !v.Settled() && v.Halted == nil
}

func (o *orderScreen) searchSoon(query string) tea.Cmd {
	o.searchGen++
	gen := o.searchGen
	if len([]rune(query)) < searchLength {
		o.hits = nil
		return nil
	}
	return tea.Tick(searchDelay, func(time.Time) tea.Msg { return searchDueMsg{gen, query} })
}

// search runs one search and cancels the one before it.
func (o *orderScreen) search(m *model, query string) tea.Cmd {
	if o.cancelSearch != nil {
		o.cancelSearch()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	o.cancelSearch = cancel
	s := o.session
	return call(ctx, func(ctx context.Context) (tea.Msg, error) {
		defer cancel()
		hits, err := s.Search(ctx, query)
		return hitsMsg{query, hits, err}, err
	})
}

// leave asks first when a push already failed and changes are pending, then
// leaves the order. Account.Leave pushes what is still pending itself.
func (o *orderScreen) leave(m *model, quit bool) (screen, tea.Cmd) {
	if !o.shown.Settled() && o.shown.Problem != nil {
		o.dialog = confirmLeave{quit: quit}
		return o, nil
	}
	return o.leaveNow(m, quit)
}

func (o *orderScreen) leaveNow(m *model, quit bool) (screen, tea.Cmd) {
	o.dialog = leaving{quit: quit}
	return o, m.leave(o.shown.Order.ID, quit)
}

// done ends the screen once its order is left: quit, or back to the list.
func (o *orderScreen) done(m *model, quit bool) (screen, tea.Cmd) {
	if quit {
		return o, tea.Quit
	}
	return &ordersScreen{busy: "Loading orders…"}, m.loadOrders()
}

// cursorAt is the index of the line under the cursor: the cursor's product,
// or the first line when that product is no longer listed. It is -1 when
// there are no lines.
func (o *orderScreen) cursorAt() int {
	lines := o.shown.Lines
	if i := slices.IndexFunc(lines, func(l fill.LineView) bool { return l.ID == o.cursor }); i >= 0 {
		return i
	}
	if len(lines) == 0 {
		return -1
	}
	return 0
}

func (o *orderScreen) line() (fill.LineView, bool) {
	i := o.cursorAt()
	if i < 0 {
		return fill.LineView{}, false
	}
	return o.shown.Lines[i], true
}

// move puts the cursor delta lines away from the line under it.
func (o *orderScreen) move(delta int) {
	if i := o.cursorAt(); i >= 0 {
		lines := o.shown.Lines
		o.cursor = lines[min(max(i+delta, 0), len(lines)-1)].ID
	}
}

func (o *orderScreen) view(m *model) string {
	v := o.shown
	top := []string{
		spread(styles.bold.Render(v.Order.Slot)+" · order "+v.Order.ID.String(), "te betalen "+styles.bold.Render(v.Total.Payable.String()), m.width),
		o.input.View(),
	}
	if o.searching {
		hits := table(o.hitRows())
		from, to := window(len(hits), o.hitCursor, hitsShown)
		top = append(top, hits[from:to]...)
	}
	top = append(top, styles.faint.Render(strings.Repeat("─", cmp.Or(m.width, 80))))

	bottom := strings.Join(append([]string{o.footer()}, o.below(m)...), "\n")

	lines := table(o.lineRows())
	if len(lines) == 0 {
		lines = []string{styles.faint.Render("Nothing in this order yet. / searches the catalog.")}
	}
	height := 0
	if m.height > 0 {
		height = max(m.height-len(top)-lipgloss.Height(bottom), 1)
	}
	from, to := window(len(lines), max(o.cursorAt(), 0), height)
	return strings.Join(append(append(top, lines[from:to]...), bottom), "\n")
}

func (o *orderScreen) hitRows() [][]string {
	rows := make([][]string, len(o.hits))
	for i, h := range o.hits {
		mark := " "
		if i == o.hitCursor {
			mark = "▸"
		}
		var state []string
		if q := o.shown.Quantity(h.ID); q > 0 {
			state = append(state, fmt.Sprintf("in order %d", q))
		}
		if !h.Orderable {
			state = append(state, styles.faint.Render("niet leverbaar"))
		}
		now, was := price(h.Product)
		rows[i] = []string{mark, truncate(h.Title, titleWidth), h.Size, now, was, h.Bonus, strings.Join(state, " · ")}
	}
	return rows
}

func (o *orderScreen) lineRows() [][]string {
	at := o.cursorAt()
	rows := make([][]string, len(o.shown.Lines))
	for i, l := range o.shown.Lines {
		mark := " "
		if i == at && !o.searching {
			mark = "▸"
		}
		q := strconv.Itoa(l.Quantity)
		switch {
		case !l.Pending:
		case o.shown.Problem != nil:
			q += styles.warn.Render("!")
		default:
			q += "…"
		}
		title := truncate(l.Title, titleWidth)
		if l.Pending && l.Quantity == 0 {
			title = styles.strike.Render(title)
		}
		now, was := price(l.Product)
		rows[i] = []string{mark, q, title, l.Size, now, was, l.Bonus}
	}
	return rows
}

// footer counts the quantities the person sees but shows AH's confirmed
// total, because group promotions only show up there.
func (o *orderScreen) footer() string {
	v := o.shown
	n := 0
	for _, l := range v.Lines {
		n += l.Quantity
	}
	parts := []string{fmt.Sprintf("%d producten", n)}
	if n == 1 {
		parts[0] = "1 product"
	}
	if v.Total.Discount > 0 {
		parts = append(parts, "bonus −"+v.Total.Discount.String())
	}
	parts = append(parts, "te betalen "+styles.bold.Render(v.Total.Payable.String()))
	switch {
	case v.Settled():
	case v.Problem != nil || v.Halted != nil:
		parts = append(parts, styles.warn.Render("not all saved"))
	default:
		parts = append(parts, styles.faint.Render("updating…"))
	}
	return strings.Join(parts, " · ")
}

// below is the part under the footer. It shows a dialog, or else the status
// and the keys that act right now.
func (o *orderScreen) below(m *model) []string {
	switch d := o.dialog.(type) {
	case confirmLeave:
		return []string{
			styles.warn.Render("Some changes did not reach AH."),
			help(m.width, keys.Confirm, keys.Retry) + styles.faint.Render(" · any other key stay"),
		}
	case leaving:
		return []string{"Leaving the order at AH…"}
	case leaveFailed:
		bindings := []key.Binding{keys.Retry, keys.Lines}
		if d.quit {
			bindings = append(bindings, keys.QuitAnyway)
		}
		return []string{styles.warn.Render(d.err.Error()), help(m.width, bindings...)}
	case leftUnsaved:
		out := []string{styles.warn.Render("Left the order. AH did not take:")}
		for what := range strings.SplitSeq(d.what.Error(), "\n") {
			out = append(out, "  "+what)
		}
		return append(out, styles.faint.Render("any key"))
	case noDialog:
	}

	v := o.shown
	var out []string
	switch {
	case v.Halted != nil:
		out = append(out, styles.warn.Render(v.Halted.Error()+" · esc leaves the order"))
	case v.Problem != nil:
		out = append(out, styles.warn.Render(strings.ReplaceAll(v.Problem.Error(), "\n", "; ")))
	}
	if o.notice != "" {
		out = append(out, o.notice)
	}
	if e := o.entry; e != nil {
		out = append(out, styles.entry.Render(fmt.Sprintf("%2s", e.digits))+" × "+e.product.Title)
		return append(out, help(m.width, keys.Digit, keys.Commit, keys.Erase, keys.Cancel))
	}
	return append(out, help(m.width, o.bindings()...))
}

func (o *orderScreen) bindings() []key.Binding {
	switch {
	case o.searching:
		return []key.Binding{keys.Up, keys.Down, keys.Edit, keys.Lines, keys.Interrupt}
	case o.shown.Halted != nil:
		return []key.Binding{keys.Up, keys.Down, keys.LeaveOrder, keys.Quit}
	}
	b := []key.Binding{keys.Up, keys.Down, keys.More, keys.Less, keys.Digit, keys.Edit, keys.Remove}
	if o.undo != nil {
		b = append(b, keys.Undo)
	}
	b = append(b, keys.Search)
	if o.retryable() {
		b = append(b, keys.Retry)
	}
	return append(b, keys.LeaveOrder, keys.Quit)
}
