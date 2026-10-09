// Package tui renders the terminal trading UI as a bubbletea program.
// It is a pure consumer of app.App: it renders state and submits
// intents; all money logic lives in the engine.
package tui

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Abhijeetsng97/terminalTrade/internal/app"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
	"github.com/Abhijeetsng97/terminalTrade/internal/marketdata"
)

// styles
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	greenStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	redStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	orangeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	atmStyle      = lipgloss.NewStyle().Reverse(true)
	headerStyle   = lipgloss.NewStyle().Bold(true)
	selStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	kiteStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	fyersStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// page ids
type page int

const (
	pagePositions page = iota
	pageChain
	pageFunds
	pageBook
)

// model is the bubbletea root model.
type model struct {
	app *app.App

	// navigation
	page    page
	cursor  int
	zone    int // chain: 0=CE 1=strike 2=PE
	expanded map[string]bool
	// alert dedup state: condition-key -> currently active
	alertActive map[string]bool
	// alert overlay open
	alertView bool

	// data (refreshed via refreshMsg; never touched off the UI loop)
	snapshot app.Snapshot
	chain    []core.ChainRow
	// chainTop is the first visible chain row (viewport scrolling:
	// the cursor must never leave the screen).
	chainTop      int
	underlyingIdx int // index into configured underlyings
	expiryOffset  int
	// refreshBusy: one data fetch in flight at a time (set on tick,
	// cleared when the result message lands).
	refreshBusy bool

	// order book page cache (fetched by the refresh loop, rendered
	// from memory — View never queries the store)
	bookParents []core.Order
	bookChildren map[string][]core.ChildOrder

	// order form
	form        *orderForm
	squareOffConfirm bool
	// broker login modal
	loginModal  *loginModal
	// alert banner
	alerts []string
	lastKeyTime time.Time
	pendingQuit bool
	width, height int
}

// tickMsg drives periodic refresh.
type tickMsg time.Time

// New creates the root model.
func New(a *app.App) *model {
	return &model{
		app:         a,
		expanded:    map[string]bool{},
		alertActive: map[string]bool{},
	}
}

// refreshMsg carries the result of a background data fetch. The model
// is only ever mutated when Update processes this message — never
// from goroutines — so no locks are needed.
type refreshMsg struct {
	underlying    string
	expiryOffset  int
	snapshot      app.Snapshot
	chain         []core.ChainRow
	bookParents   []core.Order
	bookChildren  map[string][]core.ChildOrder
	alerts        []string
	panic         bool // data fetch panicked: land the error, keep the loop alive
}

// placeResultMsg lands when the engine finishes an order round-trip.
type placeResultMsg struct{ err error }

// loginResultMsg lands when a broker token exchange completes.
type loginResultMsg struct{ info, err string }

// Init starts the refresh loop.
func (m *model) Init() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

// Update handles input and ticks.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, m.tickCmd()

	case refreshMsg:
		cmd := m.applyRefresh(msg)
		return m, cmd

	case placeResultMsg:
		if m.form != nil {
			m.form.placing = false
			if msg.err != nil {
				m.form.err = msg.err.Error()
				m.form.confirming = false
			} else {
				// placed: close the form, surface confirmation in the
				// alert banner (the book page shows the real state)
				m.alerts = append(m.alerts, "order placed — see book (4)")
				if len(m.alerts) > 20 {
					m.alerts = m.alerts[len(m.alerts)-20:]
				}
				m.form = nil
			}
		}
		return m, nil

	case loginResultMsg:
		if m.loginModal != nil {
			m.loginModal.working = false
			if msg.err != "" {
				m.loginModal.err = msg.err
			} else {
				m.loginModal.info = msg.info
			}
		}
		return m, nil

	case tea.KeyMsg:
		// Ctrl+C exits from anywhere (gate, modal, form, page) —
		// standard terminal muscle memory.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// modal-first handling
		if m.loginModal != nil {
			return m.loginModal.update(m, msg)
		}
		if m.form != nil {
			return m.updateForm(msg)
		}
		return m.updateKeys(msg)
	}
	return m, nil
}

// tickCmd kicks off the next data refresh as a tea.Cmd (or schedules
// the next tick when one is already in flight). All broker/network
// I/O happens inside the Cmd — the Update loop stays non-blocking.
func (m *model) tickCmd() tea.Cmd {
	if m.refreshBusy {
		return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	m.refreshBusy = true
	underlying := m.underlying()
	offset := m.expiryOffset
	userID := m.app.UserID
	a := m.app
	return func() tea.Msg {
		msg := refreshMsg{underlying: underlying, expiryOffset: offset}
		defer func() {
			if r := recover(); r != nil {
				log.Printf("refresh panic: %v", r)
				msg.panic = true
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		msg.snapshot = a.GetSnapshot(ctx)
		msg.chain = a.ChainRows(underlying, offset)
		// order book (today's parents + children) — View never hits Mongo
		midnight := midnightIST()
		parents, _ := a.Engine.ListParentsSince(ctx, userID, midnight)
		children := map[string][]core.ChildOrder{}
		for _, p := range parents {
			kids, _ := a.Engine.Children(ctx, p.ID)
			children[p.ID] = kids
		}
		msg.bookParents = parents
		msg.bookChildren = children
		return msg
	}
}

// applyRefresh lands a completed data fetch onto the model. Runs on
// the UI loop — single-threaded, no locks. Returns the next command
// (usually the next tick).
func (m *model) applyRefresh(msg refreshMsg) tea.Cmd {
	m.refreshBusy = false
	if msg.panic {
		// data layer hiccup: log it, surface a one-shot alert, keep
		// the loop alive — the trader's terminal must never freeze
		m.alerts = append(m.alerts, "refresh error — see server log (recovered)")
		return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
	}
	// only apply if the user hasn't switched underlying/expiry while
	// the fetch was in flight — otherwise the chain would flicker
	if msg.underlying != m.underlying() || msg.expiryOffset != m.expiryOffset {
		// params changed mid-fetch: refetch immediately with the new
		// params (keeps `w` feeling instant)
		return m.tickCmd()
	}
	m.snapshot = msg.snapshot
	m.chain = msg.chain
	m.bookParents = msg.bookParents
	m.bookChildren = msg.bookChildren
	// clamp cursor and viewport to the fresh chain
	if m.cursor >= len(m.chain) {
		m.cursor = len(m.chain) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.chainTop = m.syncViewport(m.chainTop)
	for _, al := range m.collectAlerts(m.snapshot) {
		m.alerts = append(m.alerts, al)
	}
	if len(m.alerts) > 20 {
		m.alerts = m.alerts[len(m.alerts)-20:]
	}
	// schedule the next tick; the fetch itself may have taken a
	// moment, so the cadence is max(1s, fetch duration)
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refresh() was removed: the tick loop is now tickCmd/applyRefresh —
// a tea.Cmd performs the fetch off-loop and a refreshMsg lands the
// result back on the UI goroutine. No goroutines ever touch model
// state; the previous version deadlocked here (double Lock).

func (m *model) underlying() string {
	us := m.app.Cfg.Underlyings
	if m.underlyingIdx >= len(us) {
		m.underlyingIdx = 0
	}
	if len(us) == 0 {
		return "NIFTY"
	}
	return us[m.underlyingIdx]
}

// updateKeys is global + per-page key handling.
func (m *model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "1":
		m.page, m.cursor = pagePositions, 0
	case "2":
		m.page, m.cursor = pageChain, m.centerOnATM()
	case "3":
		m.page = pageFunds
	case "4":
		m.page, m.cursor = pageBook, 0
	case "l", "L":
		m.loginModal = newLoginModal(m.app)
	case "!":
		m.alertView = !m.alertView
	case "?":
		// help: rendered inline in the footer for v1
	case "q":
		if time.Since(m.lastKeyTime) < 500*time.Millisecond && m.pendingQuit {
			return m, tea.Quit
		}
		m.pendingQuit = true
		m.lastKeyTime = time.Now()
		return m, nil
	}
	m.pendingQuit = false

	switch m.page {
	case pagePositions:
		return m.updatePositionsKeys(msg)
	case pageChain:
		return m.updateChainKeys(msg)
	case pageBook:
		return m.updateBookKeys(msg)
	}
	return m, nil
}

func (m *model) updatePositionsKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.snapshot.Positions)-1 {
			m.cursor++
		}
	case "e":
		if g := m.selectedGroup(); g != nil {
			m.expanded[g.Instrument.Key()] = !m.expanded[g.Instrument.Key()]
		}
	case "c", "C":
		if g := m.selectedGroup(); g != nil {
			m.openCloseForm(g)
		}
	case "Q":
		m.openConfirmSquareOff()
	}
	return m, nil
}

func (m *model) updateChainKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.chain)-1 {
			m.cursor++
		}
	case "left", "h":
		if m.zone > 0 {
			m.zone--
		}
	case "right", "l":
		if m.zone < 2 {
			m.zone++
		}
	case "w":
		m.expiryOffset++
		// retarget the quote poller so the new expiry starts
		// receiving quotes immediately (not on the next cycle)
		m.app.RetargetPoller(m.underlying(), m.expiryOffset)
		m.cursor = m.centerOnATM()
		m.chainTop = m.syncViewport(m.chainTop)
		return m, m.tickCmd() // fetch the new expiry's chain now
	case "a":
		m.cursor = m.centerOnATM()
		m.chainTop = m.syncViewport(m.chainTop)
	case "b", "B":
		m.openTradeForm(core.SideBuy)
	case "s", "S":
		m.openTradeForm(core.SideSell)
	}
	// keep the cursor inside the viewport after any move
	m.chainTop = m.syncViewport(m.chainTop)
	return m, nil
}

// visibleChainRows computes how many chain rows fit the terminal,
// capped at 25 so the page shows a tight window around ATM instead
// of every strike on tall screens.
func (m *model) visibleChainRows() int {
	// chrome: top bar(1) + blank(1) + header(1) + colheader(1) + alert(1) + keys(1)
	h := m.height - 6
	if h < 3 {
		h = 3
	}
	if h > 25 {
		h = 25
	}
	return h
}

// syncViewport clamps chainTop so the cursor is visible.
func (m *model) syncViewport(top int) int {
	vis := m.visibleChainRows()
	if vis >= len(m.chain) {
		return 0 // everything fits
	}
	if m.cursor < top {
		return m.cursor
	}
	if m.cursor >= top+vis {
		return m.cursor - vis + 1
	}
	if top > len(m.chain)-vis {
		return len(m.chain) - vis
	}
	return top
}

func (m *model) updateBookKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// v1: cancel via API key handling on selected row is delegated
	// to the form layer; book is read-mostly with X to cancel.
	return m, nil
}

func (m *model) centerOnATM() int {
	// find strike closest to the underlying's live LTP (real mode:
	// the poller quotes NSE:NIFTY 50 under the synthetic :INDEX key)
	ltp, _ := m.app.Quotes.LTP(m.app.UnderlyingKey(m.underlying()))
	if ltp <= 0 {
		// fallback: middle of the loaded chain
		if len(m.chain) == 0 {
			return 0
		}
		return len(m.chain) / 2
	}
	best, bestIdx := 1e18, 0
	for i, row := range m.chain {
		if d := abs(row.Strike - ltp); d < best {
			best, bestIdx = d, i
		}
	}
	return bestIdx
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func (m *model) selectedGroup() *core.PositionGroup {
	if m.cursor < 0 || m.cursor >= len(m.snapshot.Positions) {
		return nil
	}
	g := m.snapshot.Positions[m.cursor]
	return &g
}

// openTradeForm opens the buy/sell form for the cursor's chain row.
func (m *model) openTradeForm(side core.Side) {
	row := m.selectedChainRow()
	if row == nil {
		return
	}
	var otype core.OptionType
	switch m.zone {
	case 2:
		otype = core.Put
	default: // CE zone or strike zone: CE (arrows switch to PE while open)
		otype = core.Call
	}
	var inst *core.Instrument
	if otype == core.Call {
		inst = row.CE
	} else {
		inst = row.PE
	}
	if inst == nil {
		m.alerts = append(m.alerts, "instrument not listed for this side/strike")
		return
	}
	m.form = newOrderForm(m.app, *inst, side, 0, "")
}

// openCloseForm opens the close form for a position group.
func (m *model) openCloseForm(g *core.PositionGroup) {
	if len(g.BrokerRows) == 0 {
		return
	}
	// single-broker: direct close; multi-broker: close the selected
	// broker row if expanded, else the largest.
	var pos core.Position
	if len(g.BrokerRows) == 1 {
		pos = g.BrokerRows[0]
	} else {
		pos = g.BrokerRows[0]
	}
	side := core.SideSell
	lots := pos.Lots
	if pos.Lots < 0 {
		side = core.SideBuy
		lots = -lots
	}
	m.form = newOrderForm(m.app, pos.Instrument, side, lots, string(pos.Broker))
	m.form.isClose = true
}

func (m *model) openConfirmSquareOff() {
	m.form = nil
	// v1 square-off-all goes through a confirm form
	m.squareOffConfirm = true
}

func (m *model) selectedChainRow() *core.ChainRow {
	if m.cursor < 0 || m.cursor >= len(m.chain) {
		return nil
	}
	r := m.chain[m.cursor]
	return &r
}

// updateForm delegates to the order form.
func (m *model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.form.done {
		m.form = nil
		m.squareOffConfirm = false
		return m, nil
	}
	return m.form.update(m, msg)
}

// View renders the page. Layout: top bar / page body (padded to
// full height) / alert bar / key bar pinned at the bottom.
func (m *model) View() string {
	if m.width == 0 {
		// wish's bubbletea middleware sends WindowSizeMsg before the
		// first View; belt-and-braces fallback.
		return "\n  terminalTrade — waiting for terminal size..."
	}
	var b strings.Builder
	b.WriteString(m.renderTopBar())
	b.WriteString("\n")

	// page body or modals
	var middle string
	switch {
	case m.alertView:
		middle = m.renderAlerts()
	case m.loginModal != nil:
		middle = m.loginModal.view(m)
	default:
		switch m.page {
		case pagePositions:
			middle = m.renderPositions()
		case pageChain:
			middle = m.renderChain()
		case pageFunds:
			middle = m.renderFunds()
		case pageBook:
			middle = m.renderBook()
		}
	}
	if m.form != nil {
		middle += "\n\n" + m.form.view()
	}

	// bottom chrome
	bottom := m.renderAlertBar() + "\n" + m.renderKeyBar()

	// pad the middle so the chrome pins to the terminal bottom
	body := m.trimLines(middle, m.height-3)
	used := len(strings.Split(body, "\n"))
	pad := m.height - 3 - used
	if pad > 0 {
		body += strings.Repeat("\n", pad)
	}
	b.WriteString(body)
	b.WriteString(bottom)
	return b.String()
}

// trimLines caps a block at n lines (long pages scroll by cursor in
// v1; the cap keeps the frame stable).
func (m *model) trimLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// renderAlerts renders the alert overlay: TUI alerts + app-level ops
// alerts (instrument refresh, session transitions), newest last.
func (m *model) renderAlerts() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("ALERTS") + "\n")
	// merge app ops alerts (dedup by exact text, keep order)
	seen := map[string]bool{}
	for _, a := range m.alerts {
		if !seen[a] {
			seen[a] = true
			b.WriteString("· " + a + "\n")
		}
	}
	for _, a := range m.app.Alerts() {
		if !seen[a] {
			seen[a] = true
			b.WriteString("· " + a + "\n")
		}
	}
	if len(seen) == 0 {
		b.WriteString(dimStyle.Render("no alerts — all quiet") + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("(!) close · alerts clear when their condition clears"))
	return b.String()
}

func (m *model) renderTopBar() string {
	status := m.snapshot.Market
	var feed string
	switch m.snapshot.FeedState {
	case "LIVE":
		feed = greenStyle.Render("LIVE")
	case "POLL":
		feed = warnStyle.Render("POLL")
	default:
		feed = redStyle.Render(marketdata.FeedSummary(marketdata.FeedState(m.snapshot.FeedState), m.snapshot.FeedAt))
	}
	tabs := []string{"[1]Pos", "[2]Chain", "[3]Funds", "[4]Book"}
	return fmt.Sprintf("%s  %s   %s   %s  %s",
		titleStyle.Render("terminalTrade"),
		strings.Join(tabs, " "),
		m.renderBrokerDots(),
		"Mkt:"+string(status),
		feed)
}

func (m *model) renderBrokerDots() string {
	parts := []string{}
	for _, b := range []string{"KITE", "FYERS"} {
		ss, ok := m.snapshot.Sessions[b]
		if !ok {
			parts = append(parts, dimStyle.Render("○ "+b))
			continue
		}
		dot := greenStyle.Render("●")
		if !ss.Valid {
			dot = redStyle.Render("●")
		}
		parts = append(parts, dot+" "+b)
	}
	return strings.Join(parts, " ")
}

func (m *model) renderPositions() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("POSITIONS  %s fut LTP: live via feed", m.underlying())))
	b.WriteString("\n")
	if len(m.snapshot.Positions) == 0 {
		b.WriteString(dimStyle.Render("no open positions"))
		return b.String()
	}
	b.WriteString(fmt.Sprintf("%-34s %-7s %8s %10s %10s %12s\n",
		"INSTRUMENT", "BROKER", "LOTS", "AVG", "LTP", "P&L"))
	for i, g := range m.snapshot.Positions {
		mark := " "
		if i == m.cursor {
			mark = selStyle.Render("▸")
		}
		net := fmt.Sprintf("%+.0f", g.NetLots)
		b.WriteString(fmt.Sprintf("%s %-33s %-7s %8s %10s %10s %12s\n",
			mark, displayInstrument(g.Instrument), "NET", net,
			"", "", money(g.NetPnL)))
		if m.expanded[g.Instrument.Key()] {
			for _, r := range g.BrokerRows {
				b.WriteString(fmt.Sprintf("    %-31s %-7s %8.0f %10.2f %10.2f %12s\n",
					"", brokerColor(r.Broker, string(r.Broker)), r.Lots,
					r.AvgPrice, r.LastPrice, money(r.PnL)))
			}
		}
	}
	return b.String()
}

func (m *model) renderChain() string {
	var b strings.Builder
	// expiry label from the actual expiries list
	expLabel := "n/a"
	if exps := m.app.ChainExpiries(m.underlying()); len(exps) > 0 {
		off := m.expiryOffset % len(exps)
		expLabel = exps[off].Format("02Jan")
		if len(exps) > 1 {
			expLabel = fmt.Sprintf("%s (%d/%d, w to cycle)", expLabel, off+1, len(exps))
		}
	}
	// underlying LTP (real: from the index quote)
	ltpStr := "n/a"
	if ltp, ok := m.app.Quotes.LTP(m.app.UnderlyingKey(m.underlying())); ok && ltp > 0 {
		ltpStr = fmt.Sprintf("%.2f", ltp)
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("OPTION CHAIN  %s spot %s  exp %s  [a] ATM",
		m.underlying(), ltpStr, expLabel)))
	b.WriteString("\n")
	if len(m.chain) == 0 {
		// explain WHY, based on actual state
		b.WriteString(warnStyle.Render("chain empty — diagnosing below") + "\n")
		if len(m.app.Instruments()) == 0 {
			b.WriteString(dimStyle.Render("· no instruments loaded: broker login needed (L) or instrument fetch failing") + "\n")
		} else {
			b.WriteString(fmt.Sprintf("%s %d instruments loaded, but none for %s at this expiry (press w)\n",
				dimStyle.Render("·"), len(m.app.Instruments()), m.underlying()))
		}
		if m.snapshot.FeedState != "LIVE" && m.snapshot.FeedState != "POLL" {
			b.WriteString(dimStyle.Render("· feed "+marketdata.FeedSummary(marketdata.FeedState(m.snapshot.FeedState), m.snapshot.FeedAt)+" — quotes not arriving") + "\n")
		}
		return b.String()
	}
	b.WriteString(fmt.Sprintf("%10s %10s %10s %10s  %8s  %10s %10s %10s %10s\n",
		"CE OI", "CE ASK", "CE BID", "CE LTP", "STRIKE", "PE LTP", "PE BID", "PE ASK", "PE OI"))
	// viewport: rows [chainTop, chainTop+visible)
	top := m.chainTop
	if top > len(m.chain)-1 {
		top = 0
	}
	vis := m.visibleChainRows()
	end := top + vis
	if end > len(m.chain) {
		end = len(m.chain)
	}
	for i := top; i < end; i++ {
		row := m.chain[i]
		line := fmt.Sprintf("%10s %10s %10s %10s  %8.0f  %10s %10s %10s %10s",
			oi(row.CEQuote), px(row.CEQuote, true), px(row.CEQuote, false), qLTP(row.CEQuote),
			row.Strike,
			qLTP(row.PEQuote), px(row.PEQuote, false), px(row.PEQuote, true), oi(row.PEQuote))
		if i == m.cursor {
			line = atmStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if end < len(m.chain) {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more strikes below (↓ to scroll)", len(m.chain)-end)) + "\n")
	}
	return b.String()
}

// padRight pads s to display width w (rune-count, not bytes — the ₹
// glyph is one cell but 3 bytes, which broke %-Ns alignment).
func padRight(s string, w int) string {
	width := utf8.RuneCountInString(strings.TrimSpace(s))
	if width >= w {
		return s
	}
	return s + strings.Repeat(" ", w-width)
}

// padLeft right-aligns s to display width w.
func padLeft(s string, w int) string {
	width := utf8.RuneCountInString(strings.TrimSpace(s))
	if width >= w {
		return s
	}
	return strings.Repeat(" ", w-width) + s
}

func (m *model) renderFunds() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render(fmt.Sprintf("FUNDS, MARGIN & COLLATERAL   refreshed %.0fs ago", time.Since(m.snapshot.FundsAt).Seconds())))
	b.WriteString("\n")
	// header (plain ASCII widths, left-aligned first col)
	b.WriteString(padRight("BROKER", 12) +
		padLeft("AVAILABLE", 16) +
		padLeft("USED MARGIN", 16) +
		padLeft("COLLATERAL", 16) +
		padLeft("TOTAL", 16))
	b.WriteString("\n")
	var avail, used, coll, total float64
	for _, f := range m.snapshot.Funds {
		if f.Total == 0 && f.Available == 0 && f.Collateral == 0 {
			// broker with no session: skip the fake zero row, say why
			b.WriteString(padRight(brokerColor(f.Broker, string(f.Broker)), 12) +
				dimStyle.Render("no session — press L to login"))
			b.WriteString("\n")
			continue
		}
		b.WriteString(padRight(brokerColor(f.Broker, string(f.Broker)), 12) +
			padLeft(money(f.Available), 16) +
			padLeft(money(f.Used), 16) +
			padLeft(money(f.Collateral), 16) +
			padLeft(money(f.Total), 16))
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(
			padRight("  components", 12) +
				padLeft("span "+money(f.Span), 16) +
				padLeft("expos "+money(f.Exposure), 16) +
				padLeft("prem "+money(f.OptionPremium), 16) +
				padLeft("deb "+money(f.Debits), 16)))
		b.WriteString("\n")
		avail += f.Available
		used += f.Used
		coll += f.Collateral
		total += f.Total
	}
	b.WriteString(headerStyle.Render(
		padRight("COMBINED", 12) +
			padLeft(money(avail), 16) +
			padLeft(money(used), 16) +
			padLeft(money(coll), 16) +
			padLeft(money(total), 16)))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("TOTAL = available + collateral (deployable)   ·   used = span + exposure + premium + debits"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("buying power ≈ total (available + collateral)"))
	return b.String()
}

func (m *model) renderBook() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("ORDER BOOK (today)"))
	b.WriteString("\n")
	// renders from the refresh-cached book (fetched by tickCmd);
	// View never queries the store — a slow Mongo must never stall
	// a paint.
	parents := m.bookParents
	if len(parents) == 0 {
		b.WriteString(dimStyle.Render("no orders today"))
		return b.String()
	}
	b.WriteString(fmt.Sprintf("%-6s %-30s %-5s %-14s %-9s %-8s\n",
		"TIME", "INSTRUMENT", "SIDE", "QTY", "TYPE", "STATE"))
	for _, p := range parents {
		kids := m.bookChildren[p.ID]
		filled := p.FilledQty
		if filled == 0 && len(kids) > 0 {
			for _, k := range kids {
				filled += k.FilledQty
			}
		}
		stateTxt := string(p.State)
		switch p.State {
		case core.StateFilled:
			stateTxt = greenStyle.Render(string(p.State))
		case core.StateRejected, core.StateHalted:
			stateTxt = redStyle.Render(string(p.State))
		}
		b.WriteString(fmt.Sprintf("%-6s %-30s %-5s %5d/%-8d %-9s %-8s\n",
			p.Created.Format("15:04"), displayInstrument(p.Instrument), p.Side,
			filled, p.TotalQty, p.OrderType, stateTxt))
		// rejection/halt reason: the trader must see WHY, not just
		// REJECTED — and it must be visible without expanding slices
		if p.Reason != "" && (p.State == core.StateRejected || p.State == core.StateHalted) {
			b.WriteString("        " + redStyle.Render("↳ "+p.Reason) + "\n")
		}
		for _, k := range kids {
			if m.expanded[p.ID] {
				b.WriteString(fmt.Sprintf("    slice %-25s %5d/%-8d %-9s %s\n",
					k.ID, k.FilledQty, k.Qty, "", k.State))
				if k.Reason != "" && (k.State == core.StateRejected) {
					b.WriteString("        " + redStyle.Render("↳ "+k.Reason) + "\n")
				}
			}
		}
	}
	return b.String()
}

func (m *model) renderAlertBar() string {
	dayPnL := 0.0
	for _, g := range m.snapshot.Positions {
		dayPnL += g.NetPnL
	}
	pnl := greenStyle.Render(fmt.Sprintf("+₹%.0f", dayPnL))
	if dayPnL < 0 {
		pnl = redStyle.Render(fmt.Sprintf("₹%.0f", dayPnL))
	}
	alerts := ""
	if len(m.alerts) > 0 {
		alerts = warnStyle.Render(fmt.Sprintf("⚠ %d alerts [!]", len(m.alerts)))
	}
	return fmt.Sprintf("%s  Day P&L: %s", alerts, pnl)
}

func (m *model) renderKeyBar() string {
	switch m.page {
	case pagePositions:
		return dimStyle.Render("1/2/3/4 pages · ↑↓ select · e expand · C close · Q square-off-all · L login · q q quit")
	case pageChain:
		return dimStyle.Render("1/2/3/4 pages · ↑↓ strike · ←→ CE/strike/PE · w expiry · a ATM · B buy S sell · ! alerts · L login · q q quit")
	case pageFunds:
		return dimStyle.Render("1/2/3/4 pages · L login · q q quit")
	case pageBook:
		return dimStyle.Render("1/2/3/4 pages · e expand · X cancel · M modify · L login · q q quit")
	}
	return ""
}

// ---- rendering helpers ----

func money(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	s := indianRupees(f)
	if neg {
		return redStyle.Render("-" + s)
	}
	return s
}

// indianRupees renders Indian-grouped rupees: ₹12,34,56,789.00
// (2-2-3 grouping from the right: crore, lakh, thousand).
func indianRupees(f float64) string {
	paise := int64(math.Round(f * 100))
	whole := paise / 100
	p := paise % 100

	digits := strconv.FormatInt(whole, 10)
	// Indian grouping: last 3, then pairs
	var b strings.Builder
	n := len(digits)
	if n > 3 {
		rest, last3 := digits[:n-3], digits[n-3:]
		// pad rest to a multiple of 2 for pair-splitting
		if pad := len(rest) % 2; pad != 0 {
			rest = "0" + rest
		}
		for i, ch := range rest {
			if i > 0 && i%2 == 0 {
				b.WriteByte(',')
			}
			b.WriteRune(ch)
		}
		// strip the padding comma we may have introduced
		b.WriteString(",")
		b.WriteString(last3)
	} else {
		b.WriteString(digits)
	}
	if p > 0 {
		fmt.Fprintf(&b, ".%02d", p)
	}
	return "₹" + b.String()
}

func displayInstrument(i core.Instrument) string {
	if i.IsOption() {
		return fmt.Sprintf("%s %s %.0f %s", i.Underlying, i.Expiry.Format("02Jan"), i.Strike, i.OptionType)
	}
	return fmt.Sprintf("%s %s FUT", i.Underlying, i.Expiry.Format("02Jan"))
}

func brokerColor(b core.Broker, s string) string {
	if b == core.BrokerKite {
		return kiteStyle.Render(s)
	}
	return fyersStyle.Render(s)
}

func qLTP(q *core.Quote) string {
	if q == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", q.LTP)
}

func px(q *core.Quote, ask bool) string {
	if q == nil {
		return "-"
	}
	if ask {
		return fmt.Sprintf("%.2f", q.Ask)
	}
	return fmt.Sprintf("%.2f", q.Bid)
}

func oi(q *core.Quote) string {
	if q == nil {
		return "-"
	}
	return fmt.Sprintf("%d", q.OI)
}

// context helpers to avoid importing context everywhere
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func ctxBG() context.Context { return context.Background() }

func midnightIST() time.Time {
	now := time.Now().In(core.LocalIST())
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, core.LocalIST())
}

// order form sub-model (defined in form.go)
type orderForm struct {
	app        *app.App
	inst       core.Instrument
	side       core.Side
	lotsInput  textinput.Model
	broker     core.Broker
	brokerLocked bool
	isClose        bool
	confirming     bool
	confirmText    string
	// placing locks the form while the order round-trip runs (async)
	placing bool
	err     string
	done    bool
	result  string
}

func newOrderForm(a *app.App, inst core.Instrument, side core.Side, lots float64, broker string) *orderForm {
	ti := textinput.New()
	ti.Placeholder = "lots"
	ti.Focus()
	if lots > 0 {
		ti.SetValue(fmt.Sprintf("%.0f", lots))
	}
	b := core.Broker(broker)
	if b == "" {
		b = core.BrokerKite
	}
	return &orderForm{app: a, inst: inst, side: side, lotsInput: ti, broker: b}
}

func (f *orderForm) update(m *model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// chain navigation while the form is open: arrows move the chain
	// cursor and the form retargets to the newly selected instrument.
	// Blocked while confirming/placing — the reviewed instrument must
	// not change under a pending order.
	chainNav := !f.confirming && !f.placing && m.page == pageChain
	switch msg.String() {
	case "esc":
		if f.placing {
			// an order is in flight: don't let the user think they
			// cancelled it — the result message will close the form
			return m, nil
		}
		f.done = true
		return m, nil
	case "left", "h":
		if chainNav && m.zone > 0 {
			m.zone--
			if !m.retargetForm() {
				m.zone++ // no instrument on that side: snap back
			}
			return m, nil
		}
	case "right", "l":
		if chainNav && m.zone < 2 {
			m.zone++
			if !m.retargetForm() {
				m.zone--
			}
			return m, nil
		}
	case "up", "k":
		if chainNav && m.cursor > 0 {
			m.cursor--
			m.chainTop = m.syncViewport(m.chainTop)
			if !m.retargetForm() {
				m.cursor++ // unlisted cell: snap back
				m.chainTop = m.syncViewport(m.chainTop)
			}
			return m, nil
		}
	case "down", "j":
		if chainNav && m.cursor < len(m.chain)-1 {
			m.cursor++
			m.chainTop = m.syncViewport(m.chainTop)
			if !m.retargetForm() {
				m.cursor--
				m.chainTop = m.syncViewport(m.chainTop)
			}
			return m, nil
		}
	case "tab":
		if !f.brokerLocked {
			if f.broker == core.BrokerKite {
				f.broker = core.BrokerFyers
			} else {
				f.broker = core.BrokerKite
			}
		}
	case "enter":
		if f.confirming {
			return m, f.place(m)
		}
		f.buildConfirm()
	case "y":
		if f.confirming {
			return m, f.place(m)
		}
	case "n":
		if f.confirming {
			f.confirming = false
			return m, nil
		}
	}
	if !f.confirming {
		if f.placing {
			return m, nil // locked while the order round-trip runs
		}
		var cmd tea.Cmd
		f.lotsInput, cmd = f.lotsInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// retargetForm re-points the open order form at the instrument under
// the chain cursor (zone CE/strike/PE, current strike). The lots value
// is preserved. Returns false when the cell has no listed instrument
// (the caller snaps the cursor/zone back).
func (m *model) retargetForm() bool {
	if m.form == nil {
		return false
	}
	row := m.selectedChainRow()
	if row == nil {
		return false
	}
	inst := row.CE
	if m.zone == 2 {
		inst = row.PE
	}
	if inst == nil {
		return false
	}
	if inst.Key() != m.form.inst.Key() {
		m.form.inst = *inst
		m.form.err = ""
	}
	return true
}

func (f *orderForm) buildConfirm() {
	lots, err := parseLots(f.lotsInput.Value())
	if err != nil {
		f.err = err.Error()
		return
	}
	f.err = ""
	plan := core.PlanSlices(f.inst, lots)
	ltp, _ := f.app.Quotes.LTP(f.inst.Key())
	price := f.inst.Key() + " MKT-PROT"
	var est float64
	if ltp > 0 {
		est = float64(lots*f.inst.LotSize) * core.ProtectionPrice(ltp, f.side, f.inst.TickSize)
	}
	f.confirmText = fmt.Sprintf(
		"%s %d lots (%d) %s @ %s\nBroker: %s   Est value ₹%.0f   Slices: %d",
		f.side, lots, lots*f.inst.LotSize, displayInstrument(f.inst), price,
		f.broker, est, len(plan.Quantities))
	f.confirming = true
}

func (f *orderForm) place(m *model) tea.Cmd {
	lots, err := parseLots(f.lotsInput.Value())
	if err != nil {
		f.err = err.Error()
		return nil
	}
	// placing state: form locks, no double-submit
	if f.placing {
		return nil
	}
	f.placing = true
	f.err = ""
	f.app.RiskMu.Lock()
	risk := f.app.Risk
	f.app.RiskMu.Unlock()
	inst, side, broker := f.inst, f.side, f.broker
	userID := f.app.UserID
	eng := f.app.Engine

	// placement runs as a tea.Cmd off the UI loop; the result lands
	// as placeResultMsg (handled in model.Update). The UI stays live.
	return func() tea.Msg {
		_, err := eng.PlaceIntent(ctxBG(), userID, inst, side, lots, core.TypeMarketProtected, 0, broker, risk)
		return placeResultMsg{err: err}
	}
}

func (f *orderForm) view() string {
	var b strings.Builder
	if f.placing {
		b.WriteString(titleStyle.Render("PLACING ORDER…"))
		b.WriteString("\n" + dimStyle.Render("slices are being placed via the broker — this closes when done") + "\n")
		return b.String()
	}
	if f.confirming {
		b.WriteString(titleStyle.Render("CONFIRM ORDER"))
		b.WriteString("\n" + f.confirmText + "\n")
		b.WriteString(dimStyle.Render("(y) place   (n) back   (esc) cancel"))
		if f.err != "" {
			b.WriteString("\n" + redStyle.Render(f.err))
		}
		return b.String()
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("%s  %s  LTP %s", f.side, displayInstrument(f.inst), qLTPOf(f.app, f.inst))))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("Lots: %s  (= %s units)\n", f.lotsInput.View(), dimStyle.Render("lots x lot size")))
	b.WriteString(fmt.Sprintf("Type: MKT-PROT (default) · Broker: %s (Tab switch)\n", brokerColor(f.broker, string(f.broker))))
	if f.err != "" {
		b.WriteString(redStyle.Render(f.err) + "\n")
	}
	b.WriteString(dimStyle.Render("(↑↓ strike · ←→ CE/PE while open) · (Tab) broker · (Enter) review · (Esc) cancel"))
	return b.String()
}

func parseLots(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("enter lots")
	}
	var lots int
	if _, err := fmt.Sscanf(s, "%d", &lots); err != nil || lots <= 0 {
		return 0, fmt.Errorf("lots must be a positive number")
	}
	return lots, nil
}

func qLTPOf(a *app.App, i core.Instrument) string {
	if ltp, ok := a.Quotes.LTP(i.Key()); ok {
		return fmt.Sprintf("%.2f", ltp)
	}
	return "-"
}

// loginModal is the broker session modal.
type loginModal struct {
	app     *app.App
	sessions map[string]brokers.SessionStatus
	input   textinput.Model
	err     string
	info    string
	// working: token exchange in flight (locks the form, one at a time)
	working bool
}

func newLoginModal(a *app.App) *loginModal {
	ti := textinput.New()
	ti.Placeholder = "paste request_token / auth code"
	ti.Focus()
	return &loginModal{app: a, input: ti}
}

func (lm *loginModal) update(m *model, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.loginModal = nil
		return m, nil
	case "enter":
		tok := strings.TrimSpace(lm.input.Value())
		if tok == "" {
			lm.err = "paste a token first"
			return m, nil
		}
		// exchange runs off-loop as a tea.Cmd; the result lands as
		// loginResultMsg. No network I/O inside Update.
		if lm.working {
			return m, nil
		}
		lm.working = true
		lm.err = ""
		kite, fyers := lm.app.Kite, lm.app.Fyers
		a := lm.app
		return m, func() tea.Msg {
			var info, errMsg string
			if kite != nil {
				if msg, err := kite.ExchangeManualToken(ctxBG(), tok); err == nil {
					kt, _ := kite.CurrentTokens()
					_ = a.SaveBrokerTokens(ctxBG(), core.BrokerKite, kt, "")
					info = msg
				} else {
					errMsg = "kite rejected token: " + err.Error()
				}
			}
			if errMsg == "" && fyers != nil {
				if _, err := fyers.ExchangeManualToken(ctxBG(), tok); err == nil {
					kt, krt := fyers.CurrentTokens()
					_ = a.SaveBrokerTokens(ctxBG(), core.BrokerFyers, kt, krt)
					info = "fyers session established"
				} else if errMsg == "" {
					errMsg = "fyers rejected token: " + err.Error()
				}
			}
			if kite == nil && fyers == nil {
				errMsg = "no real brokers configured (sim mode) — nothing to login"
			}
			return loginResultMsg{info: info, err: errMsg}
		}
	}
	var cmd tea.Cmd
	lm.input, cmd = lm.input.Update(msg)
	return m, cmd
}

func (lm *loginModal) view(m *model) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("BROKER SESSIONS") + "\n")
	for _, name := range []string{"KITE", "FYERS"} {
		ss, ok := m.snapshot.Sessions[name]
		if !ok {
			continue
		}
		status := greenStyle.Render("OK")
		if !ss.Valid {
			status = redStyle.Render("invalid: " + ss.Reason)
		}
		b.WriteString(fmt.Sprintf("%-7s %s\n", name, status))
	}
	if lm.app.Kite != nil {
		b.WriteString("\n" + dimStyle.Render("KITE: open " + lm.app.Kite.LoginURL()))
		b.WriteString("\n" + dimStyle.Render("      login, copy request_token from redirect URL"))
	} else {
		b.WriteString("\n" + warnStyle.Render("KITE not configured (set KITE_API_KEY/KITE_API_SECRET in .env)"))
	}
	if lm.app.Fyers != nil {
		b.WriteString("\n" + dimStyle.Render("FYERS: open " + lm.app.Fyers.LoginURL()))
		b.WriteString("\n" + dimStyle.Render("       login (creds + OTP), copy auth_code from redirect URL"))
	} else {
		b.WriteString("\n" + warnStyle.Render("FYERS not configured (set FYERS_* in .env)"))
	}
	b.WriteString("\n\n" + dimStyle.Render("paste the token below — Kite request_token or Fyers auth_code; the app detects it"))
	b.WriteString("\n" + lm.input.View() + "\n")
	if lm.err != "" {
		b.WriteString(redStyle.Render(lm.err) + "\n")
	}
	if lm.info != "" {
		b.WriteString(greenStyle.Render(lm.info) + "\n")
	}
	b.WriteString(dimStyle.Render("(Enter) submit · (Esc) close"))
	return b.String()
}

// collectAlerts derives NEW alerts from a snapshot. Dedup: each
// condition fires once until it clears (re-arms when the condition
// goes away) — not once per second.
func (m *model) collectAlerts(s app.Snapshot) []string {
	var out []string
	emit := func(key, msg string) {
		if !m.alertActive[key] {
			m.alertActive[key] = true
			out = append(out, msg)
		}
	}
	for _, ss := range s.Sessions {
		key := "session-" + string(ss.Broker)
		if !ss.Valid && s.Market == core.MarketOpen {
			emit(key, string(ss.Broker)+" session invalid during market hours — press L")
		} else {
			delete(m.alertActive, key) // condition gone: re-arm
		}
	}
	if s.Market == core.MarketPreOpen {
		out2 := "pre-open: order placement blocked until 09:15"
		if !m.alertActive["preopen"] {
			m.alertActive["preopen"] = true
			out = append(out, out2)
		}
	} else {
		delete(m.alertActive, "preopen")
	}
	return out
}
