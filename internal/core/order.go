package core

// Order lifecycle types shared by every consumer of the trading core
// (TUI, REST API, future CLI). The order engine owns transitions;
// consumers only read state and submit intents.

import "time"

// Side is the direction of an order.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Opposite returns the reversing side (used by close-position).
func (s Side) Opposite() Side {
	if s == SideBuy {
		return SideSell
	}
	return SideBuy
}

// OrderType is the execution policy of an order.
type OrderType string

const (
	// TypeMarketProtected is the default: a limit order placed at
	// LTP +/- the protection band. Behaves like a market order
	// within the band, parks as a limit if price runs away.
	TypeMarketProtected OrderType = "MKT-PROT"
	// TypeLimit is an explicit limit order at a chosen price.
	TypeLimit OrderType = "LIMIT"
)

// OrderState is the state of a single order (parent or slice).
type OrderState string

const (
	// StateIntent: created locally, not yet confirmed by the user.
	StateIntent OrderState = "INTENT"
	// StateConfirmed: user confirmed; engine has not placed it yet.
	StateConfirmed OrderState = "CONFIRMED"
	// StatePlacing: adapter call in flight.
	StatePlacing OrderState = "PLACING"
	// StateOpen: accepted by the broker, working.
	StateOpen OrderState = "OPEN"
	// StatePartiallyFilled: some quantity traded.
	StatePartiallyFilled OrderState = "PARTIALLY_FILLED"
	// StateFilled: fully traded.
	StateFilled OrderState = "FILLED"
	// StateCancelled: cancelled before full fill.
	StateCancelled OrderState = "CANCELLED"
	// StateRejected: broker refused the order.
	StateRejected OrderState = "REJECTED"
	// StateHalted means the parent was stopped: a margin rejection
	// halted remaining slices (spec: margin rejection halts parent).
	StateHalted OrderState = "HALTED"
)

// Order is a parent order: the trader's confirmed intent.
type Order struct {
	ID        string
	UserID    string
	Instrument Instrument
	Side      Side
	// Lots is the total quantity in lots.
	Lots int
	// TotalQty is lots * lot size.
	TotalQty int
	OrderType OrderType
	// LimitPrice is set for TypeLimit orders.
	LimitPrice float64
	// Broker placement is routed to.
	Broker   Broker
	State    OrderState
	// FilledQty rolls up from children (reconcile keeps it fresh).
	FilledQty int
	// Reason carries the broker rejection reason / halt cause.
	Reason  string
	Created time.Time
	Updated time.Time
}

// ChildOrder is a broker-visible slice of a parent.
type ChildOrder struct {
	ID         string
	ParentID   string
	UserID     string
	Instrument  Instrument
	Side       Side
	// Qty is this slice's quantity in units.
	Qty int
	OrderType   OrderType
	// LimitPrice of this slice (protection or explicit limit).
	LimitPrice float64
	// IdempotencyTag is the client-generated unique tag sent to the
	// broker so retries can never double-place (Kite tag / Fyers
	// clientOrderId).
	IdempotencyTag string
	Broker         Broker
	// BrokerOrderID is the broker's order id once accepted.
	BrokerOrderID string
	State         OrderState
	FilledQty     int
	Reason        string
	Created       time.Time
	Updated       time.Time
}

// AggregateState summarizes children into the parent-level state.
func AggregateState(children []ChildOrder) OrderState {
	if len(children) == 0 {
		return StateIntent
	}
	var open, filled, rejected, cancelled, partially int
	for _, c := range children {
		switch c.State {
		case StateOpen, StatePlacing:
			open++
		case StateFilled:
			filled++
		case StatePartiallyFilled:
			partially++
		case StateRejected:
			rejected++
		case StateCancelled:
			cancelled++
		}
	}
	// Margin halt: engine marks the parent halted explicitly.
	if filled+partially > 0 && (rejected > 0 || open > 0) {
		// some filled, some not — mixed states are surfaced as-is
		return StatePartiallyFilled
	}
	if filled > 0 && filled+partially == len(children) {
		if partially > 0 {
			return StatePartiallyFilled
		}
		return StateFilled
	}
	if filled == 0 && rejected > 0 && open == 0 && partially == 0 {
		if cancelled > 0 {
			return StateCancelled
		}
		return StateRejected
	}
	if open > 0 || partially > 0 {
		return StateOpen
	}
	// cancelled remainder
	return StateCancelled
}

// Position is a per-broker position snapshot, canonical instrument.
type Position struct {
	Instrument Instrument
	Broker     Broker
	// Lots signed: positive long, negative short.
	Lots float64
	// Qty signed units.
	Qty      int
	AvgPrice float64
	// LastPrice is the current LTP (from WS or poll).
	LastPrice float64
	// PnL is the unrealized P&L for this position row.
	PnL       float64
	UpdatedAt time.Time
}

// Funds is a per-broker funds/margin snapshot. Total is the total
// margin: Available + Used. Span/Exposure/OptionPremium are the
// used-margin components where the broker reports them (Kite does;
// Fyers reports a flat utilized amount — zeros there). Collateral
// is pledged holdings value usable as margin.
type Funds struct {
	Broker    Broker
	Available float64
	Used      float64
	Total     float64 // Available + Used
	// Used-margin components (Kite equity segment).
	Span          float64
	Exposure      float64
	OptionPremium float64
	Debits        float64
	// Collateral is the pledged-holdings margin component.
	Collateral float64
	// Net is the broker-reported net balance when available.
	Net       float64
	UpdatedAt time.Time
}