// Package tui renders the terminal trading UI as a bubbletea program.
// It is a pure consumer of app.App: it renders state and submits
// intents; all money logic lives in the engine.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Abhijeetsng97/terminalTrade/internal/app"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
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

	// data (refreshed by tick)
	snapshot app.Snapshot
	chain    []core.ChainRow
	underlyingIdx int // index into configured underlyings
	expiryOffset  int

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
		app:      a,
		expanded: map[string]bool{},
	}
}

// Init starts the refresh loop.
func (m *model) Init() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

// Update handles input and ticks.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.refresh()
		return m, tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })

	case tea.KeyMsg:
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

// refresh pulls a snapshot (1s cadence).
func (m *model) refresh() {
	ctx, cancel := contextWithTimeout(3 * time.Second)
	defer cancel()
	m.snapshot = m.app.GetSnapshot(ctx)
	m.chain = m.app.ChainRows(m.underlying(), m.expiryOffset)
	for _, a := range collectAlerts(m.snapshot, m.app) {
		m.alerts = append(m.alerts, a)
	}
	if len(m.alerts) > 20 {
		m.alerts = m.alerts[len(m.alerts)-20:]
	}
}

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
		m.cursor = m.centerOnATM()
	case "a":
		m.cursor = m.centerOnATM()
	case "b", "B":
		m.openTradeForm(core.SideBuy)
	case "s", "S":
		m.openTradeForm(core.SideSell)
	}
	return m, nil
}

func (m *model) updateBookKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// v1: cancel via API key handling on selected row is delegated
	// to the form layer; book is read-mostly with X to cancel.
	return m, nil
}

func (m *model) centerOnATM() int {
	// find strike closest to underlying LTP
	ltpKey := m.app.UnderlyingKey(m.underlying())
	ltp, _ := m.app.Quotes.LTP(ltpKey)
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
	if m.zone == 0 {
		otype = core.Call
	} else if m.zone == 2 {
		otype = core.Put
	} else {
		// strike zone: default CE
		otype = core.Call
	}
	inst := m.app.ChainInstrument(m.underlying(), m.expiryOffset, row.Strike, otype)
	if inst == nil {
		m.alerts = append(m.alerts, "instrument not found for strike")
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

func (m *model) renderTopBar() string {
	status := m.snapshot.Market
	var feed string
	switch m.snapshot.FeedState {
	case "LIVE":
		feed = greenStyle.Render("LIVE")
	case "POLL":
		feed = warnStyle.Render("POLL")
	default:
		feed = redStyle.Render("STALE " + m.snapshot.FeedAt.Format("15:04:05"))
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
	b.WriteString(headerStyle.Render(fmt.Sprintf("OPTION CHAIN  %s   Expiry offset: %d   [w] cycle  [a] ATM", m.underlying(), m.expiryOffset)))
	b.WriteString("\n")
	if len(m.chain) == 0 {
		b.WriteString(dimStyle.Render("no chain data — instruments not loaded?"))
		return b.String()
	}
	b.WriteString(fmt.Sprintf("%10s %10s %10s %10s  %8s  %10s %10s %10s %10s\n",
		"CE OI", "CE ASK", "CE BID", "CE LTP", "STRIKE", "PE LTP", "PE BID", "PE ASK", "PE OI"))
	for i, row := range m.chain {
		line := fmt.Sprintf("%10s %10s %10s %10s  %8.0f  %10s %10s %10s %10s",
			oi(row.CE), px(row.CE, true), px(row.CE, false), qLTP(row.CE),
			row.Strike,
			qLTP(row.PE), px(row.PE, false), px(row.PE, true), oi(row.PE))
		if i == m.cursor {
			line = atmStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m *model) renderFunds() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("FUNDS"))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%-8s %15s %15s %15s\n", "BROKER", "AVAILABLE", "USED", "TOTAL"))
	var avail, used float64
	for _, f := range m.snapshot.Funds {
		b.WriteString(fmt.Sprintf("%-8s %15s %15s %15s\n",
			brokerColor(f.Broker, string(f.Broker)), money(f.Available), money(f.Used), money(f.Total)))
		avail += f.Available
		used += f.Used
	}
	b.WriteString(fmt.Sprintf("%-8s %15s %15s %15s\n", "COMBINED", money(avail), money(used), money(avail+used)))
	return b.String()
}

func (m *model) renderBook() string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("ORDER BOOK (today)"))
	b.WriteString("\n")
	// v1: orders from the engine store
	parents, _ := m.app.Engine.ListParentsSince(ctxBG(), m.app.UserID, midnightIST())
	if len(parents) == 0 {
		b.WriteString(dimStyle.Render("no orders today"))
		return b.String()
	}
	b.WriteString(fmt.Sprintf("%-6s %-30s %-5s %-14s %-9s %-8s\n",
		"TIME", "INSTRUMENT", "SIDE", "QTY", "TYPE", "STATE"))
	for _, p := range parents {
		kids, _ := m.app.Engine.Children(ctxBG(), p.ID)
		filled := 0
		for _, k := range kids {
			filled += k.FilledQty
		}
		b.WriteString(fmt.Sprintf("%-6s %-30s %-5s %5d/%-8d %-9s %-8s\n",
			p.Created.Format("15:04"), displayInstrument(p.Instrument), p.Side,
			filled, p.TotalQty, p.OrderType, p.State))
		for _, k := range kids {
			if m.expanded[p.ID] {
				b.WriteString(fmt.Sprintf("    slice %-25s %5d/%-8d %-9s %s\n",
					k.ID, k.FilledQty, k.Qty, "", k.State))
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
		return dimStyle.Render("1/2/3/4 pages · ↑↓ strike · ←→ CE/strike/PE · w expiry · a ATM · B buy S sell · L login · q q quit")
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
	s := fmt.Sprintf("₹%.2f", f)
	if neg {
		return redStyle.Render("-" + s)
	}
	return s
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
	isClose    bool
	confirming bool
	confirmText string
	err        string
	done       bool
	result     string
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
	switch msg.String() {
	case "esc":
		f.done = true
		return m, nil
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
			f.place(m)
			return m, nil
		}
		f.buildConfirm()
	case "y":
		if f.confirming {
			f.place(m)
			return m, nil
		}
	case "n":
		if f.confirming {
			f.confirming = false
			return m, nil
		}
	}
	if !f.confirming {
		var cmd tea.Cmd
		f.lotsInput, cmd = f.lotsInput.Update(msg)
		return m, cmd
	}
	return m, nil
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

func (f *orderForm) place(m *model) {
	lots, err := parseLots(f.lotsInput.Value())
	if err != nil {
		f.err = err.Error()
		return
	}
	f.app.RiskMu.Lock()
	risk := f.app.Risk
	f.app.RiskMu.Unlock()
	_, err = f.app.Engine.PlaceIntent(ctxBG(), f.app.UserID, f.inst, f.side, lots, core.TypeMarketProtected, 0, f.broker, risk)
	if err != nil {
		f.err = err.Error()
		f.confirming = false
		return
	}
	f.result = "order placed"
	f.done = true
}

func (f *orderForm) view() string {
	var b strings.Builder
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
	b.WriteString(dimStyle.Render("(Enter) review · (Esc) cancel"))
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
		// try both brokers; the right one accepts its token shape
		if msg, err := lm.app.Kite.ExchangeManualToken(ctxBG(), tok); err == nil {
			lm.info = msg
			kt, _ := lm.app.Kite.CurrentTokens()
			_ = lm.app.SaveBrokerTokens(ctxBG(), core.BrokerKite, kt, "")
			return m, nil
		} else if _, err := lm.app.Fyers.ExchangeManualToken(ctxBG(), tok); err == nil {
			kt, krt := lm.app.Fyers.CurrentTokens()
			_ = lm.app.SaveBrokerTokens(ctxBG(), core.BrokerFyers, kt, krt)
			return m, nil
		} else {
			lm.err = "token rejected by both brokers"
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
	b.WriteString("\n" + dimStyle.Render("KITE: open " + lm.app.Kite.LoginURL()))
	b.WriteString("\nlogin, copy request_token from redirect, paste below\n")
	b.WriteString(lm.input.View() + "\n")
	if lm.err != "" {
		b.WriteString(redStyle.Render(lm.err) + "\n")
	}
	if lm.info != "" {
		b.WriteString(greenStyle.Render(lm.info) + "\n")
	}
	b.WriteString(dimStyle.Render("(Enter) submit · (Esc) close"))
	return b.String()
}

// collectAlerts derives banner alerts from a snapshot.
func collectAlerts(s app.Snapshot, a *app.App) []string {
	var out []string
	for _, ss := range s.Sessions {
		if !ss.Valid && s.Market == core.MarketOpen {
			out = append(out, string(ss.Broker)+" session invalid during market hours — press L")
		}
	}
	if s.Market == core.MarketPreOpen {
		out = append(out, "pre-open: order placement blocked until 09:15")
	}
	return out
}