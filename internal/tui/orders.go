package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/szwaan/appie/internal/fill"
)

// ordersScreen lists the open orders. Reopened orders come first in the
// person's attention: the account is on their bonus prices until they are
// resumed and left, or left right here.
type ordersScreen struct {
	orders []fill.Order
	cursor int
	busy   string // what is in flight, e.g. "Opening…"; empty when idle
	err    error  // why the list could not be loaded
	last   error
}

func (o *ordersScreen) update(m *model, msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case ordersMsg:
		o.orders, o.err, o.busy = msg.orders, msg.err, ""
		o.cursor = min(o.cursor, max(len(o.orders)-1, 0))
	case openedMsg:
		if msg.err != nil {
			o.last, o.busy = msg.err, ""
			return o, m.loadOrders()
		}
		return newOrderScreen(msg.session), nil
	case leftMsg:
		o.last, o.busy = msg.err, ""
		return o, m.loadOrders()
	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Quit, keys.Interrupt) {
			return o, tea.Quit // an Open still running is waited out by main's Close
		}
		if o.busy != "" {
			return o, nil
		}
		o.last = nil
		sel, ok := o.selected()
		switch {
		case key.Matches(msg, keys.Refresh):
			o.busy = "Refreshing…"
			return o, m.loadOrders()
		case key.Matches(msg, keys.Up):
			o.cursor = max(o.cursor-1, 0)
		case key.Matches(msg, keys.Down):
			o.cursor = max(min(o.cursor+1, len(o.orders)-1), 0)
		case key.Matches(msg, keys.Open) && ok && sel.State != fill.Locked:
			o.busy = "Opening " + sel.Slot + "…"
			return o, m.open(sel.ID)
		case key.Matches(msg, keys.LeaveReopened) && ok && sel.State == fill.Reopened:
			o.busy = "Leaving " + sel.Slot + "…"
			return o, m.leave(sel.ID, false)
		}
	}
	return o, nil
}

func (o *ordersScreen) selected() (fill.Order, bool) {
	if o.cursor < 0 || o.cursor >= len(o.orders) {
		return fill.Order{}, false
	}
	return o.orders[o.cursor], true
}

func (o *ordersScreen) view(m *model) string {
	rows := make([][]string, len(o.orders))
	for i, ord := range o.orders {
		mark := " "
		switch {
		case i == o.cursor:
			mark = "▸"
		case ord.State == fill.Reopened:
			mark = styles.warn.Render("!")
		}
		row := []string{mark, ord.Slot, ord.Total.String(), ord.Status, ""}
		switch ord.State {
		case fill.Reopened:
			row[4] = styles.warn.Render("Reopened: your account is on this date's bonus prices") + "  " +
				styles.faint.Render(keys.Open.Help().Key+" resume · "+keys.LeaveReopened.Help().Key+" leave")
		case fill.Locked:
			for j := 1; j < 4; j++ {
				row[j] = styles.faint.Render(row[j])
			}
		case fill.Submitted:
		}
		rows[i] = row
	}
	from, to := window(len(rows), o.cursor, m.height-6)

	out := []string{styles.bold.Render("Appie · open orders"), ""}
	out = append(out, table(rows)[from:to]...)
	out = append(out, "")
	switch {
	case o.busy != "":
		out = append(out, o.busy)
	case o.err != nil:
		out = append(out, styles.warn.Render(o.err.Error()))
	case len(o.orders) == 0:
		out = append(out, "No open orders. Plan a delivery in the AH app.")
	}
	if o.last != nil {
		out = append(out, styles.warn.Render(o.last.Error()))
	}
	out = append(out, help(m.width, keys.Open, keys.LeaveReopened, keys.Refresh, keys.Quit))
	return strings.Join(out, "\n")
}
