// Package engine implements the order lifecycle: confirmation,
// slicing, idempotent placement with fresh-tag retries, margin-halt,
// and reconciliation to broker truth.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// Store persists orders/trades/audit. Implemented by store.Store.
type Store interface {
	SaveParent(ctx context.Context, o core.Order) error
	SaveChild(ctx context.Context, c core.ChildOrder) error
	ListParents(ctx context.Context, userID string, since time.Time) ([]core.Order, error)
	ListChildren(ctx context.Context, parentID string) ([]core.ChildOrder, error)
	Audit(ctx context.Context, entry AuditEntry) error
}

// AuditEntry is one append-only audit row.
type AuditEntry struct {
	Time    time.Time
	UserID  string
	Kind    string // order-placed, order-rejected, margin-halt, session-refresh, ...
	Subject string
	Detail  string
}

// QuoteSource supplies LTP for protection pricing at confirm time.
type QuoteSource interface {
	LTP(instrumentKey string) (float64, bool)
}

// Engine owns the order state machine.
type Engine struct {
	store   Store
	quotes  QuoteSource
	hours   *core.MarketHours
	clock   func() time.Time
	// adapters by broker
	adapters map[core.Broker]brokers.BrokerAdapter

	// RetryPolicy: transport errors retried at most N times.
	MaxTransportRetries int

	mu sync.Mutex
}

func New(store Store, quotes QuoteSource, hours *core.MarketHours, clock func() time.Time) *Engine {
	return &Engine{
		store:               store,
		quotes:              quotes,
		hours:               hours,
		clock:               clock,
		adapters:            make(map[core.Broker]brokers.BrokerAdapter),
		MaxTransportRetries: 2,
	}
}

// RegisterAdapter wires a broker adapter.
func (e *Engine) RegisterAdapter(a brokers.BrokerAdapter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.adapters[a.Broker()] = a
}

// RiskConfig are the caps validated before confirmation.
type RiskConfig struct {
	// MaxLotsPerOrder caps a single order (default: freeze lots).
	MaxLotsPerOrder int
	// BigOrderLots triggers a second confirmation.
	BigOrderLots int
	// DayMaxLotsPerUnderlying: 0 = off.
	DayMaxLotsPerUnderlying int
	// today's placed lots per underlying (engine-tracked).
	placedToday map[string]int
}

func NewRiskConfig() RiskConfig {
	return RiskConfig{placedToday: make(map[string]int)}
}

// ValidateIntent runs all pre-trade checks. Returns the slice plan.
func (e *Engine) ValidateIntent(ctx context.Context, inst core.Instrument, side core.Side, lots int, ot core.OrderType, limitPrice float64, broker core.Broker, risk RiskConfig) (core.SlicePlan, error) {
	if lots <= 0 {
		return core.SlicePlan{}, fmt.Errorf("lots must be > 0")
	}
	if _, ok := e.adapters[broker]; !ok {
		return core.SlicePlan{}, fmt.Errorf("broker %s not configured", broker)
	}
	if inst.LotSize <= 0 {
		return core.SlicePlan{}, fmt.Errorf("instrument %s has no lot size (stale instruments?)", inst.Key())
	}
	if symbol, ok := inst.BrokerSymbols[broker]; !ok || symbol == "" {
		return core.SlicePlan{}, fmt.Errorf("%s does not list %s", broker, inst.Key())
	}
	if !e.hours.CanPlace(e.clock()) {
		return core.SlicePlan{}, fmt.Errorf("market closed: orders only 09:15-15:30 IST on trading days")
	}
	if risk.MaxLotsPerOrder > 0 && lots > risk.MaxLotsPerOrder {
		return core.SlicePlan{}, fmt.Errorf("order of %d lots exceeds per-order cap %d", lots, risk.MaxLotsPerOrder)
	}
	if risk.DayMaxLotsPerUnderlying > 0 {
		placed := risk.placedToday[inst.Underlying]
		if placed+lots > risk.DayMaxLotsPerUnderlying {
			return core.SlicePlan{}, fmt.Errorf("day cap: %d placed + %d requested > %d max for %s", placed, lots, risk.DayMaxLotsPerUnderlying, inst.Underlying)
		}
	}
	if ot == core.TypeLimit && limitPrice <= 0 {
		return core.SlicePlan{}, fmt.Errorf("limit order requires a price")
	}
	if limitPrice > 0 && !core.IsValidPrice(limitPrice, inst.TickSize) {
		return core.SlicePlan{}, fmt.Errorf("price %.2f not on tick %.2f", limitPrice, inst.TickSize)
	}
	return core.PlanSlices(inst, lots), nil
}

// PlaceIntent validates, persists, and fires a confirmed intent
// fire-all style: all slices placed, each with its idempotency tag,
// transport failures retried with fresh tags, margin rejections
// halt the remaining slices.
func (e *Engine) PlaceIntent(ctx context.Context, userID string, inst core.Instrument, side core.Side, lots int, ot core.OrderType, limitPrice float64, broker core.Broker, risk RiskConfig) (core.Order, error) {
	plan, err := e.ValidateIntent(ctx, inst, side, lots, ot, limitPrice, broker, risk)
	if err != nil {
		return core.Order{}, err
	}

	// price for MKT-PROT from live quote
	price := limitPrice
	if ot == core.TypeMarketProtected {
		ltp, ok := e.quotes.LTP(inst.Key())
		if !ok || ltp <= 0 {
			return core.Order{}, fmt.Errorf("no live quote for %s; cannot compute protection price", inst.Key())
		}
		price = core.ProtectionPrice(ltp, side, inst.TickSize)
	}

	now := e.clock()
	parent := core.Order{
		ID:         newID("ord"),
		UserID:     userID,
		Instrument: inst,
		Side:       side,
		Lots:       lots,
		TotalQty:   lots * inst.LotSize,
		OrderType:  ot,
		LimitPrice: price,
		Broker:     broker,
		State:      core.StatePlacing,
		Created:    now,
		Updated:    now,
	}
	if err := e.store.SaveParent(ctx, parent); err != nil {
		return core.Order{}, err
	}
	e.audit(ctx, userID, "order-intent", parent.ID, fmt.Sprintf("%s %d lots %s %s @%s", side, lots, inst.Key(), broker, ot))

	// fire-all: place every slice; margin rejection halts the rest
	var firstMarginErr error
	var lastRejection error
	children := make([]core.ChildOrder, 0, len(plan.Quantities))
	for _, qty := range plan.Quantities {
		child := core.ChildOrder{
			ID:        newID("sli"),
			ParentID:  parent.ID,
			UserID:    userID,
			Instrument: inst,
			Side:      side,
			Qty:       qty,
			OrderType: ot,
			LimitPrice: price,
			Broker:    broker,
			State:     core.StatePlacing,
			Created:   now,
			Updated:   now,
		}
		child.IdempotencyTag = newID("tag")
		if err := e.store.SaveChild(ctx, child); err != nil {
			return parent, err
		}

		brokerOrderID, err := e.placeWithRetry(ctx, child)
		if err != nil {
			ae := brokers.ClassifyError(err)
			if ae.Kind == brokers.ErrMarginRejection {
				child.State = core.StateRejected
				child.Reason = ae.Message
				_ = e.store.SaveChild(ctx, child)
				if firstMarginErr == nil {
					firstMarginErr = err
				}
				// halt remaining slices
				parent.State = core.StateHalted
				parent.Reason = "margin rejection halted remaining slices: " + ae.Message
				parent.Updated = e.clock()
				_ = e.store.SaveParent(ctx, parent)
				e.audit(ctx, userID, "margin-halt", parent.ID, ae.Message)
				break
			}
			child.State = core.StateRejected
			child.Reason = ae.Message
			_ = e.store.SaveChild(ctx, child)
			if lastRejection == nil {
				lastRejection = ae
			}
			continue
		}
		child.BrokerOrderID = brokerOrderID
		child.State = core.StateOpen
		child.Updated = e.clock()
		_ = e.store.SaveChild(ctx, child)
		children = append(children, child)
	}
	if firstMarginErr != nil {
		return parent, firstMarginErr
	}
	if len(children) == 0 {
		parent.State = core.StateRejected
		parent.Updated = e.clock()
		_ = e.store.SaveParent(ctx, parent)
		if lastRejection != nil {
			return parent, lastRejection
		}
		return parent, fmt.Errorf("all slices rejected")
	}
	parent.State = core.StateOpen
	parent.Updated = e.clock()
	_ = e.store.SaveParent(ctx, parent)
	return parent, nil
}

// placeWithRetry: transport errors only, <= MaxTransportRetries,
// fresh idempotency tag each attempt. Rejections are returned
// immediately — never auto-retried.
func (e *Engine) placeWithRetry(ctx context.Context, child core.ChildOrder) (string, error) {
	adapter := e.adapters[child.Broker]
	var lastErr error
	for attempt := 0; attempt <= e.MaxTransportRetries; attempt++ {
		if attempt > 0 {
			// fresh tag every retry
			child.IdempotencyTag = newID("tag")
		}
		id, err := adapter.PlaceOrder(ctx, brokers.OrderRequest{
			Instrument:       child.Instrument,
			Side:             child.Side,
			Qty:              child.Qty,
			OrderType:        child.OrderType,
			LimitPrice:       child.LimitPrice,
			IdempotencyTag:   child.IdempotencyTag,
		})
		if err == nil {
			return id, nil
		}
		ae := brokers.ClassifyError(err)
		if ae.Kind != brokers.ErrTransport {
			return "", err // rejection: surface, don't retry
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	return "", lastErr
}

// CancelChild cancels an open slice.
func (e *Engine) CancelChild(ctx context.Context, userID, childID string) error {
	child, adapter, err := e.loadChild(ctx, childID)
	if err != nil {
		return err
	}
	if child.State != core.StateOpen && child.State != core.StatePartiallyFilled {
		return fmt.Errorf("child %s not cancellable in state %s", childID, child.State)
	}
	if err := adapter.CancelOrder(ctx, child.BrokerOrderID); err != nil {
		return err
	}
	child.State = core.StateCancelled
	child.Updated = e.clock()
	_ = e.store.SaveChild(ctx, child)
	e.audit(ctx, userID, "order-cancelled", childID, "")
	return nil
}

// ModifyChild changes price/qty of an open slice. Re-validates
// freeze limits: a modify exceeding them is rejected client-side.
func (e *Engine) ModifyChild(ctx context.Context, userID, childID string, newPrice float64, newQty int) error {
	child, adapter, err := e.loadChild(ctx, childID)
	if err != nil {
		return err
	}
	if child.State != core.StateOpen && child.State != core.StatePartiallyFilled {
		return fmt.Errorf("child %s not modifiable in state %s", childID, child.State)
	}
	if newQty > 0 && child.Instrument.FreezeQty > 0 && newQty > child.Instrument.FreezeQty {
		return fmt.Errorf("modified qty %d exceeds freeze %d — cancel and re-place as a sliced order", newQty, child.Instrument.FreezeQty)
	}
	if newPrice > 0 && !core.IsValidPrice(newPrice, child.Instrument.TickSize) {
		return fmt.Errorf("price %.2f not on tick %.2f", newPrice, child.Instrument.TickSize)
	}
	if err := adapter.ModifyOrder(ctx, brokers.ModifyRequest{
		BrokerOrderID: child.BrokerOrderID,
		NewQty:        newQty,
		NewPrice:      newPrice,
	}); err != nil {
		return err
	}
	if newPrice > 0 {
		child.LimitPrice = newPrice
	}
	if newQty > 0 {
		child.Qty = newQty
	}
	child.Updated = e.clock()
	_ = e.store.SaveChild(ctx, child)
	e.audit(ctx, userID, "order-modified", childID, fmt.Sprintf("price=%.2f qty=%d", newPrice, newQty))
	return nil
}

func (e *Engine) loadChild(ctx context.Context, childID string) (core.ChildOrder, brokers.BrokerAdapter, error) {
	// children are found via parents; store interface provides
	// ListChildren per parent. For lookup we scan today's parents.
	parents, err := e.store.ListParents(ctx, "", e.clock().Add(-24*time.Hour))
	if err != nil {
		return core.ChildOrder{}, nil, err
	}
	for _, p := range parents {
		kids, err := e.store.ListChildren(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, k := range kids {
			if k.ID == childID {
				return k, e.adapters[k.Broker], nil
			}
		}
	}
	return core.ChildOrder{}, nil, fmt.Errorf("child %s not found", childID)
}

// Reconcile converges local state to broker truth. Poll wins: any
// child whose broker row disagrees is corrected. Returns the number
// of corrections.
func (e *Engine) Reconcile(ctx context.Context) (int, error) {
	e.mu.Lock()
	adapters := make([]brokers.BrokerAdapter, 0, len(e.adapters))
	for _, a := range e.adapters {
		adapters = append(adapters, a)
	}
	e.mu.Unlock()

	corrections := 0
	for _, a := range adapters {
		rows, err := a.ListOrders(ctx)
		if err != nil {
			continue // broker unreachable: keep local state
		}
		byTag := make(map[string]brokers.BrokerOrder, len(rows))
		for _, r := range rows {
			byTag[r.Tag] = r
		}
		_ = byTag
		// The store-backed reconciliation matches children by tag.
		corrections += e.reconcileBroker(ctx, a.Broker(), rows)
	}
	return corrections, nil
}

// reconcileBroker is store-implementation specific; the engine calls
// back into store to find children by tag. Simplified: matching by
// BrokerOrderID set on child rows.
func (e *Engine) reconcileBroker(ctx context.Context, b core.Broker, rows []brokers.BrokerOrder) int {
	if len(rows) == 0 {
		return 0
	}
	parents, err := e.store.ListParents(ctx, "", e.clock().Add(-24*time.Hour))
	if err != nil {
		return 0
	}
	fixed := 0
	for _, p := range parents {
		if p.Broker != b {
			continue
		}
		kids, err := e.store.ListChildren(ctx, p.ID)
		if err != nil {
			continue
		}
		for i, k := range kids {
			for _, r := range rows {
				if k.BrokerOrderID != "" && k.BrokerOrderID == r.BrokerOrderID {
					if k.State != mapBrokerStatus(r.Status) || k.FilledQty != r.FilledQty {
						kids[i].State = mapBrokerStatus(r.Status)
						kids[i].FilledQty = r.FilledQty
						kids[i].Updated = e.clock()
						_ = e.store.SaveChild(ctx, kids[i])
						fixed++
					}
				}
			}
		}
	}
	return fixed
}

func mapBrokerStatus(s core.OrderState) core.OrderState {
	// broker rows already canonical
	return s
}

// SquareOffAll closes every open position across brokers, one at a
// time (spec: sequential, never a basket race).
func (e *Engine) SquareOffAll(ctx context.Context, userID string, risk RiskConfig) ([]core.Order, error) {
	var placed []core.Order
	for _, a := range e.adapters {
		positions, err := a.ListPositions(ctx)
		if err != nil {
			return placed, err
		}
		for _, p := range positions {
			if p.Lots == 0 {
				continue
			}
			lots := p.Lots
			side := core.SideSell
			if p.Lots < 0 {
				side = core.SideBuy
				lots = -lots
			}
			// route to the broker holding the position
			o, err := e.PlaceIntent(ctx, userID, p.Instrument, side, int(lots), core.TypeMarketProtected, 0, p.Broker, risk)
			if err != nil {
				return placed, err
			}
			placed = append(placed, o)
		}
	}
	return placed, nil
}

func (e *Engine) audit(ctx context.Context, userID, kind, subject, detail string) {
	_ = e.store.Audit(ctx, AuditEntry{
		Time:    e.clock(),
		UserID:  userID,
		Kind:    kind,
		Subject: subject,
		Detail:  detail,
	})
}

// ClosePosition closes one broker position via the standard order
// path (opposite side, exact qty, routed to the holding broker).
func (e *Engine) ClosePosition(ctx context.Context, userID string, p core.Position, risk RiskConfig) (core.Order, error) {
	lots := p.Lots
	side := core.SideSell
	if p.Lots < 0 {
		side = core.SideBuy
		lots = -lots
	}
	return e.PlaceIntent(ctx, userID, p.Instrument, side, int(lots), core.TypeMarketProtected, 0, p.Broker, risk)
}

// ListParentsSince lists parent orders since a time (any user when
// userID is empty — single-user v1).
func (e *Engine) ListParentsSince(ctx context.Context, userID string, since time.Time) ([]core.Order, error) {
	return e.store.ListParents(ctx, userID, since)
}

// Children lists a parent's slices.
func (e *Engine) Children(ctx context.Context, parentID string) ([]core.ChildOrder, error) {
	return e.store.ListChildren(ctx, parentID)
}

// SetClockForTest swaps the clock (market-hours tests).
func (e *Engine) SetClockForTest(clock func() time.Time) { e.clock = clock }

// newID generates a short random id with a prefix.
func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}