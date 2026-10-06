// Package tui is Appie's terminal UI: a list of open orders and a fill screen,
// both drawn from a fill.Account. It holds no order state of its own; every
// update re-reads the session's View.
package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/szwaan/appie/internal/fill"
)

// Run shows the UI until the person quits or ctx ends. It returns
// fill.ErrSignedOut as soon as AH rejects the stored login, from any screen,
// so main can log in and run it again. An order open at that moment stays
// open in acct, and the order list offers to resume it.
func Run(ctx context.Context, acct *fill.Account) error {
	// Calls still running when the UI ends are cut short, so main's Close
	// does not wait out their deadlines.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	final, err := tea.NewProgram(model{ctx: ctx, acct: acct, screen: &ordersScreen{busy: "Loading orders…"}}, tea.WithContext(ctx)).Run()
	if err != nil {
		return err
	}
	return final.(model).exit
}

type model struct {
	ctx           context.Context
	acct          *fill.Account
	screen        screen
	width, height int
	exit          error // returned by Run
}

// screen is the one screen showing.
type screen interface {
	update(m *model, msg tea.Msg) (screen, tea.Cmd)
	view(m *model) string
}

func (m model) Init() tea.Cmd {
	return m.loadOrders()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case signedOutMsg:
		m.exit = fill.ErrSignedOut
		return m, tea.Quit
	}
	next, cmd := m.screen.update(&m, msg)
	m.screen = next
	return m, cmd
}

func (m model) View() tea.View {
	v := tea.NewView(m.screen.view(&m))
	v.AltScreen = true
	return v
}

const (
	callTimeout  = 20 * time.Second
	syncDelay    = 150 * time.Millisecond
	searchDelay  = 200 * time.Millisecond
	searchLength = 2
)

// call runs one AH call as a command, off the update loop, with its own
// deadline. f returns its result message and the call's error. A rejected
// login becomes signedOutMsg instead of the result, whichever screen asked.
func call(parent context.Context, f func(ctx context.Context) (tea.Msg, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, callTimeout)
		defer cancel()
		msg, err := f(ctx)
		if errors.Is(err, fill.ErrSignedOut) {
			return signedOutMsg{}
		}
		return msg
	}
}

type (
	signedOutMsg struct{}
	ordersMsg    struct {
		orders []fill.Order
		err    error
	}
	openedMsg struct {
		session *fill.Session
		err     error
	}
	// leftMsg reports Account.Leave. quit says whether the person was on
	// their way out.
	leftMsg struct {
		id   fill.OrderID
		err  error
		quit bool
	}
	syncDueMsg   struct{ gen int }
	syncedMsg    struct{}
	searchDueMsg struct {
		gen   int
		query string
	}
	hitsMsg struct {
		query string
		hits  []fill.Hit
		err   error
	}
)

func (m *model) loadOrders() tea.Cmd {
	acct := m.acct
	return call(m.ctx, func(ctx context.Context) (tea.Msg, error) {
		orders, err := acct.Orders(ctx)
		return ordersMsg{orders, err}, err
	})
}

func (m *model) open(id fill.OrderID) tea.Cmd {
	acct := m.acct
	return call(m.ctx, func(ctx context.Context) (tea.Msg, error) {
		s, err := acct.Open(ctx, id)
		return openedMsg{s, err}, err
	})
}

func (m *model) leave(id fill.OrderID, quit bool) tea.Cmd {
	acct := m.acct
	return call(m.ctx, func(ctx context.Context) (tea.Msg, error) {
		err := acct.Leave(ctx, id)
		return leftMsg{id: id, err: err, quit: quit}, err
	})
}

// keys is the one list of bindings; help lines are rendered from it. Up and
// Down stay off letters because they also move through search hits while
// the search box takes every printable key.
var keys = struct {
	Up, Down, Open, LeaveReopened, Refresh  key.Binding
	Search, Edit, More, Less, Digit, Remove key.Binding
	Commit, Erase, Cancel                   key.Binding
	Undo, Lines, LeaveOrder, Retry, Confirm key.Binding
	Quit, Interrupt, QuitAnyway             key.Binding
}{
	Up:            key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "up")),
	Down:          key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "down")),
	Open:          key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	LeaveReopened: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "leave reopened order")),
	Refresh:       key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Search:        key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
	Edit:          key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "quantity")),
	More:          key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "one more")),
	Less:          key.NewBinding(key.WithKeys("-"), key.WithHelp("-", "one less")),
	Digit:         key.NewBinding(key.WithKeys("0", "1", "2", "3", "4", "5", "6", "7", "8", "9"), key.WithHelp("0-9", "type quantity")),
	Remove:        key.NewBinding(key.WithKeys("delete", "backspace"), key.WithHelp("del", "remove")),
	Commit:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "set")),
	Erase:         key.NewBinding(key.WithKeys("backspace"), key.WithHelp("⌫", "erase")),
	Cancel:        key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	Undo:          key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "undo remove")),
	Lines:         key.NewBinding(key.WithKeys("esc", "tab"), key.WithHelp("esc", "back to lines")),
	LeaveOrder:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "leave order")),
	Retry:         key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "retry")),
	Confirm:       key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "leave anyway")),
	Quit:          key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
	Interrupt:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	QuitAnyway:    key.NewBinding(key.WithKeys("Q"), key.WithHelp("Q", "quit anyway")),
}

var styles = struct {
	faint, bold, strike, warn, entry lipgloss.Style
}{
	faint:  lipgloss.NewStyle().Faint(true),
	bold:   lipgloss.NewStyle().Bold(true),
	strike: lipgloss.NewStyle().Strikethrough(true).Faint(true),
	warn:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208")),
	entry:  lipgloss.NewStyle().Reverse(true),
}

// help lists bindings as "key what", wrapped to width; a width of 0 or less
// keeps them on one line.
func help(width int, bindings ...key.Binding) string {
	var lines []string
	line := ""
	for _, b := range bindings {
		item := b.Help().Key + " " + b.Help().Desc
		switch {
		case line == "":
			line = item
		case width > 0 && lipgloss.Width(line+" · "+item) > width:
			lines = append(lines, line)
			line = item
		default:
			line += " · " + item
		}
	}
	return styles.faint.Render(strings.Join(append(lines, line), "\n"))
}

// spread sets right flush with the end of a line width cells wide, or two
// spaces after left when that does not fit or the width is unknown.
func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	return left + strings.Repeat(" ", max(gap, 2)) + right
}

// table pads every column to its widest cell, so rows line up whatever
// styling their cells carry.
func table(rows [][]string) []string {
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], lipgloss.Width(cell))
		}
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = cell + strings.Repeat(" ", widths[j]-lipgloss.Width(cell))
		}
		out[i] = strings.TrimRight(strings.Join(cells, "  "), " ")
	}
	return out
}

// window is the range of n rows to draw when only height fit, keeping the
// cursor row in view. A height of 0 or less means no limit.
func window(n, cursor, height int) (from, to int) {
	if height <= 0 || n <= height {
		return 0, n
	}
	from = min(max(cursor-height/2, 0), n-height)
	return from, from + height
}

func truncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// price is a product's unit price as the person sees it, with the price
// before a per-unit bonus when there is one.
func price(p fill.Product) (now, was string) {
	if p.Was > 0 {
		was = styles.strike.Render("was " + p.Was.String())
	}
	return p.Price.String(), was
}
