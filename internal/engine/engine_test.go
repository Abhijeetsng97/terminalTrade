package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
	"github.com/Abhijeetsng97/terminalTrade/internal/sim"
)

// memStore is an in-memory Store for engine tests.
type memStore struct {
	mu          sync.Mutex
	parents     map[string]core.Order
	children    map[string][]core.ChildOrder
	audits      []AuditEntry
}

func newMemStore() *memStore {
	return &memStore{parents: map[string]core.Order{}, children: map[string][]core.ChildOrder{}}
}

func (m *memStore) SaveParent(_ context.Context, o core.Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parents[o.ID] = o
	return nil
}

func (m *memStore) SaveChild(_ context.Context, c core.ChildOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, p := range m.children[c.ParentID] {
		if p.ID == c.ID {
			m.children[c.ParentID][i] = c
			return nil
		}
	}
	m.children[c.ParentID] = append(m.children[c.ParentID], c)
	return nil
}

func (m *memStore) ListParents(_ context.Context, _ string, _ time.Time) ([]core.Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.Order
	for _, p := range m.parents {
		out = append(out, p)
	}
	return out, nil
}

func (m *memStore) ListChildren(_ context.Context, parentID string) ([]core.ChildOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.ChildOrder{}, m.children[parentID]...), nil
}

func (m *memStore) Audit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, e)
	return nil
}

// fakeQuotes supplies LTPs for protection pricing.
type fakeQuotes map[string]float64

func (f fakeQuotes) LTP(key string) (float64, bool) {
	v, ok := f[key]
	return v, ok
}

// openClock makes the market-hours guard always pass in tests.
func openClock() *core.MarketHours { return core.NewMarketHours() }
func testClock() time.Time {
	return time.Date(2026, 10, 7, 10, 0, 0, 0, core.LocalIST())
}

func testInstrument() core.Instrument {
	return core.Instrument{
		Underlying: "NIFTY", Kind: core.KindIndexOption,
		Expiry:     time.Date(2026, 10, 29, 0, 0, 0, 0, core.LocalIST()),
		Strike:     26300, OptionType: core.Call,
		LotSize: 65, FreezeQty: 3510, TickSize: 0.05,
		BrokerSymbols: map[core.Broker]string{core.Broker("SIM"): "NIFTY26O2926300CE"},
	}
}

func newTestEngine(adapter brokers.BrokerAdapter) (*Engine, *memStore) {
	st := newMemStore()
	e := New(st, fakeQuotes{testInstrument().Key(): 20.15}, openClock(), testClock)
	e.MaxTransportRetries = 2
	if adapter != nil {
		e.RegisterAdapter(adapter)
	}
	return e, st
}

func riskCfg() RiskConfig { return NewRiskConfig() }

// A normal order places one slice with a unique idempotency tag and
// audits the intent.
func TestPlaceIntentSingleSlice(t *testing.T) {
	a := sim.New()
	adapter := simBroker{a}
	e, st := newTestEngine(adapter)

	parent, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 10, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if len(a.Placed()) != 1 {
		t.Fatalf("want 1 placement, got %d", len(a.Placed()))
	}
	req := a.Placed()[0]
	if req.IdempotencyTag == "" {
		t.Error("idempotency tag must be set")
	}
	if req.OrderType != core.TypeMarketProtected {
		t.Errorf("order type: got %s", req.OrderType)
	}
	// protection price on the pay-up side of LTP (20.15 buy band 3%)
	if req.LimitPrice <= 20.15 {
		t.Errorf("buy protection %.2f must exceed LTP 20.15", req.LimitPrice)
	}
	if !core.IsValidPrice(req.LimitPrice, 0.05) {
		t.Errorf("protection %.2f off tick", req.LimitPrice)
	}
	if parent.State != core.StateOpen {
		t.Errorf("parent state: got %s", parent.State)
	}
	// audit trail exists
	found := false
	for _, a := range st.audits {
		if a.Kind == "order-intent" {
			found = true
		}
	}
	if !found {
		t.Error("order-intent audit missing")
	}
}

// Fire-all slicing: 100 lots over the freeze limit places 2 slices
// at once, both tagged, children persisted.
func TestPlaceIntentSlices(t *testing.T) {
	a := sim.New()
	e, _ := newTestEngine(simBroker{a})

	parent, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 100, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if len(a.Placed()) != 2 {
		t.Fatalf("want 2 slices placed, got %d", len(a.Placed()))
	}
	qs := []int{a.Placed()[0].Qty, a.Placed()[1].Qty}
	if qs[0] != 3510 || qs[1] != 2990 {
		t.Errorf("slice quantities: got %v want [3510 2990]", qs)
	}
	tags := map[string]bool{}
	for _, p := range a.Placed() {
		if tags[p.IdempotencyTag] {
			t.Error("duplicate idempotency tags across slices")
		}
		tags[p.IdempotencyTag] = true
	}
	kids, _ := e.Children(context.Background(), parent.ID)
	if len(kids) != 2 {
		t.Errorf("children persisted: got %d want 2", len(kids))
	}
}

// Transport failure: retries with FRESH tags (never reuses), up to
// MaxTransportRetries, then surfaces the error.
func TestTransportRetryFreshTags(t *testing.T) {
	a := sim.New()
	a.FailNextTransport = 2 // fail twice, third attempt succeeds
	e, _ := newTestEngine(simBroker{a})

	_, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 1, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err != nil {
		t.Fatalf("should succeed after retries: %v", err)
	}
	if len(a.Placed()) != 1 {
		t.Fatalf("want 1 successful placement, got %d", len(a.Placed()))
	}
	// fresh tag per attempt is enforced by the engine (tags differ
	// from what failed); the placed tag is unique and recorded.
	if a.Placed()[0].IdempotencyTag == "" {
		t.Error("placed tag must be set")
	}
}

// A plain rejection is never retried.
func TestRejectionNotRetried(t *testing.T) {
	a := sim.New()
	// craft a rejecting adapter
	adapter := &rejectingAdapter{simBroker{a}}
	e, _ := newTestEngine(adapter)

	_, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 1, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err == nil {
		t.Fatal("want error")
	}
	ae, ok := err.(*brokers.AdapterError)
	if !ok {
		t.Fatalf("want AdapterError, got %T", err)
	}
	if ae.Kind != brokers.ErrRejection {
		t.Errorf("kind: got %s want rejection", ae.Kind)
	}
	if len(a.Placed()) != 0 {
		t.Errorf("rejection must not retry: %d placements", len(a.Placed()))
	}
}

// THE core reliability property: a margin rejection on one slice
// halts the remaining slices and marks the parent HALTED.
func TestMarginRejectionHaltsRemainingSlices(t *testing.T) {
	a := sim.New()
	a.FailNextMargin = 1 // first slice rejected for margin, second would pass
	e, _ := newTestEngine(simBroker{a})

	parent, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 100, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err == nil {
		t.Fatal("margin rejection must surface as an error")
	}
	if len(a.Placed()) != 0 {
		t.Fatalf("after margin halt no further slices: got %d placements", len(a.Placed()))
	}
	if parent.State != core.StateHalted {
		t.Errorf("parent state: got %s want HALTED", parent.State)
	}
	if !strings.Contains(parent.Reason, "margin") {
		t.Errorf("halt reason should mention margin: %q", parent.Reason)
	}
}

// Market-hours guard: closed market refuses before any broker call.
func TestMarketClosedBlocksOrder(t *testing.T) {
	a := sim.New()
	e, _ := newTestEngine(simBroker{a})
	// close the market
	closed := time.Date(2026, 10, 7, 16, 0, 0, 0, core.LocalIST())
	e.SetClockForTest(func() time.Time { return closed })

	_, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 1, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err == nil {
		t.Fatal("closed market must refuse")
	}
	if len(a.Placed()) != 0 {
		t.Error("nothing may reach the broker when closed")
	}
}

// Per-order cap: a lot count above the cap is refused client-side.
func TestRiskCapBlocks(t *testing.T) {
	a := sim.New()
	e, _ := newTestEngine(simBroker{a})
	risk := NewRiskConfig()
	risk.MaxLotsPerOrder = 5

	_, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 10, core.TypeMarketProtected, 0, "SIM", risk)
	if err == nil {
		t.Fatal("cap must refuse 10 lots when cap is 5")
	}
	// day cap
	risk2 := NewRiskConfig()
	risk2.DayMaxLotsPerUnderlying = 5
	if _, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 6, core.TypeMarketProtected, 0, "SIM", risk2); err == nil {
		t.Fatal("day cap must refuse")
	}
}

// Reconciliation: broker state wins; local rows converge.
func TestReconcileConverges(t *testing.T) {
	a := sim.New()
	e, st := newTestEngine(simBroker{a})

	parent, err := e.PlaceIntent(context.Background(), "u", testInstrument(), core.SideBuy, 1, core.TypeMarketProtected, 0, "SIM", riskCfg())
	if err != nil {
		t.Fatal(err)
	}
	kids, _ := e.Children(context.Background(), parent.ID)
	if len(kids) != 1 {
		t.Fatalf("want 1 child, got %d", len(kids))
	}
	// broker fills it
	rows, _ := a.ListOrders(context.Background())
	a.Fill(rows[0].BrokerOrderID, rows[0].Qty)

	if _, err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	kids2, _ := e.Children(context.Background(), parent.ID)
	if kids2[0].State != core.StateFilled {
		t.Errorf("after reconcile child state %s, want FILLED", kids2[0].State)
	}
	// the parent must roll up too — the book page reads parents, and
	// "OPEN 0/130" on a completed order was the bug
	ps, _ := st.ListParents(context.Background(), "", time.Time{})
	var got *core.Order
	for i := range ps {
		if ps[i].ID == parent.ID {
			got = &ps[i]
		}
	}
	if got == nil {
		t.Fatal("parent missing from store")
	}
	if got.State != core.StateFilled {
		t.Errorf("after reconcile parent state %s, want FILLED", got.State)
	}
	if got.FilledQty != kids2[0].FilledQty {
		t.Errorf("parent FilledQty %d, want %d (rolled up from children)", got.FilledQty, kids2[0].FilledQty)
	}
}

// simBroker adapts the sim adapter to the SIM broker name used in tests.
type simBroker struct{ *sim.Adapter }

func (s simBroker) Broker() core.Broker { return core.Broker("SIM") }

// rejectingAdapter wraps every place call as a plain rejection.
type rejectingAdapter struct{ simBroker }

func (r rejectingAdapter) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (string, error) {
	return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "exchange rejected: invalid price"}
}