// Package sim provides a scriptable BrokerAdapter for tests and
// local development: the seam the whole trading core is tested
// against (no broker credentials, no network).
package sim

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// Adapter simulates a broker. Configure failures via script to test
// retry, margin-halt, partial fills, and reconciliation.
type Adapter struct {
	mu sync.Mutex

	// FailNextMargin tags the next N place calls as margin-rejected.
	FailNextMargin int
	// FailNextTransport tags the next N calls as transport errors.
	FailNextTransport int
	// RejectMessage customizes rejection text.
	RejectMessage string

	// Latency added to every place call (for race testing).
	Latency time.Duration

	orders    map[string]*simOrder
	placed    []brokers.OrderRequest
	positions []core.Position
	instruments []core.Instrument
	session   bool
	nextID    int
}

type simOrder struct {
	id     string
	req    brokers.OrderRequest
	status core.OrderState
	filled int
}

func New() *Adapter {
	return &Adapter{orders: map[string]*simOrder{}, session: true}
}

func (a *Adapter) Broker() core.Broker { return core.Broker("SIM") }

// WithInstruments seeds the instrument universe.
func (a *Adapter) WithInstruments(insts []core.Instrument) *Adapter {
	a.instruments = insts
	return a
}

// WithPositions seeds live positions.
func (a *Adapter) WithPositions(pos ...core.Position) *Adapter {
	a.positions = append(a.positions, pos...)
	return a
}

// Placed returns every placement request the adapter saw (in order).
func (a *Adapter) Placed() []brokers.OrderRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]brokers.OrderRequest, len(a.placed))
	copy(out, a.placed)
	return out
}

// Fill simulates a fill on a broker order.
func (a *Adapter) Fill(id string, qty int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if o, ok := a.orders[id]; ok {
		o.filled += qty
		if o.filled >= o.req.Qty {
			o.status = core.StateFilled
		} else {
			o.status = core.StatePartiallyFilled
		}
	}
}

func (a *Adapter) SessionStatus(ctx context.Context) brokers.SessionStatus {
	return brokers.SessionStatus{Broker: a.Broker(), Valid: a.session}
}

func (a *Adapter) ExchangeManualToken(ctx context.Context, token string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.session = true
	return "sim session established", nil
}

func (a *Adapter) LoginURL() string { return "https://sim.local/login" }

// PlaceOrder accepts the order and schedules the auto-fill.
func (a *Adapter) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (string, error) {
	a.mu.Lock()
	if a.Latency > 0 {
		a.mu.Unlock()
		time.Sleep(a.Latency)
		a.mu.Lock()
	}
	if a.FailNextTransport > 0 {
		a.FailNextTransport--
		a.mu.Unlock()
		return "", &brokers.AdapterError{Kind: brokers.ErrTransport, Message: "simulated timeout"}
	}
	if a.FailNextMargin > 0 {
		a.FailNextMargin--
		a.mu.Unlock()
		return "", &brokers.AdapterError{Kind: brokers.ErrMarginRejection, Message: "RMS: insufficient margin"}
	}
	id := "sim-" + req.IdempotencyTag
	if !a.session {
		return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "session expired"}
	}
	a.orders[id] = &simOrder{id: id, req: req, status: core.StateOpen}
	a.placed = append(a.placed, req)
	a.mu.Unlock()

	// auto-fill: market-protected orders fill on the next tick, like
	// a real exchange matching a marketable limit. This is what makes
	// sim-mode testing meaningful (fills land in the book).
	if req.OrderType == core.TypeMarketProtected {
		go func() {
			time.Sleep(1 * time.Second)
			a.Fill(id, req.Qty)
		}()
	}
	return id, nil
}

func (a *Adapter) ModifyOrder(ctx context.Context, req brokers.ModifyRequest) error {
	if o, ok := a.orders[req.BrokerOrderID]; ok && o.status == core.StateOpen {
		if req.NewPrice > 0 {
			o.req.LimitPrice = req.NewPrice
		}
		if req.NewQty > 0 {
			o.req.Qty = req.NewQty
		}
		return nil
}
	return &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "order not modifiable"}
}

func (a *Adapter) CancelOrder(ctx context.Context, id string) error {
	if o, ok := a.orders[id]; ok && (o.status == core.StateOpen || o.status == core.StatePartiallyFilled) {
		o.status = core.StateCancelled
		return nil
}
	return &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "order not cancellable"}
}

func (a *Adapter) ListOrders(ctx context.Context) ([]brokers.BrokerOrder, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := make([]brokers.BrokerOrder, 0, len(a.orders))
	for id, o := range a.orders {
		rows = append(rows, brokers.BrokerOrder{
			BrokerOrderID: id,
			InstrumentKey: o.req.Instrument.Key(),
			Side:          o.req.Side,
			Qty:           o.req.Qty,
			FilledQty:     o.filled,
			OrderType:     o.req.OrderType,
			LimitPrice:    o.req.LimitPrice,
			Status:        o.status,
			Tag:           o.req.IdempotencyTag,
		})
	}
	return rows, nil
}

func (a *Adapter) ListTrades(ctx context.Context) ([]brokers.BrokerTrade, error) {
	return nil, nil
}

func (a *Adapter) ListPositions(ctx context.Context) ([]core.Position, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]core.Position, len(a.positions))
	copy(out, a.positions)
	return out, nil
}

func (a *Adapter) GetFunds(ctx context.Context) (core.Funds, error) {
	return core.Funds{Broker: a.Broker(), Available: 1_000_000, Used: 0, Total: 1_000_000}, nil
}

func (a *Adapter) LoadInstruments(ctx context.Context) ([]core.Instrument, error) {
	return a.instruments, nil
}

// Parse helpers make fixture-building concise in tests.
func Instrument(underlying string, lotSize, freezeQty int) core.Instrument {
	return core.Instrument{
		Underlying: underlying,
		Kind:       core.KindIndexOption,
		Expiry:     time.Now().AddDate(0, 0, 3),
		Strike:     26000,
		OptionType: core.Call,
		LotSize:    lotSize,
		FreezeQty:  freezeQty,
		TickSize:   0.05,
	}
}

var _ = strings.TrimSpace // keep strings import if unused above