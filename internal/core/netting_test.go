package core

import (
	"testing"
	"time"
)

// Netting: +5 at Kite and -3 at Fyers read as NET +2 with both
// broker rows preserved.
func TestNetPositions(t *testing.T) {
	inst := Instrument{Underlying: "NIFTY", Kind: KindIndexOption,
		Expiry: mustDate("2026-10-29"), Strike: 25000, OptionType: Call, LotSize: 65}

	positions := []Position{
		{Instrument: inst, Broker: BrokerKite, Lots: 5, Qty: 325, PnL: 1000},
		{Instrument: inst, Broker: BrokerFyers, Lots: -3, Qty: -195, PnL: -600},
		{Instrument: inst2(), Broker: BrokerKite, Lots: 2, Qty: 130, PnL: 250},
	}
	groups := NetPositions(positions)

	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	g := groups[0]
	if g.NetLots != 2 {
		t.Errorf("net lots: got %v want 2", g.NetLots)
	}
	if g.NetPnL != 400 {
		t.Errorf("net pnl: got %v want 400", g.NetPnL)
	}
	if len(g.BrokerRows) != 2 {
		t.Fatalf("broker rows: got %d want 2", len(g.BrokerRows))
	}
	// single-broker group stays single
	if groups[1].NetLots != 2 || len(groups[1].BrokerRows) != 1 {
		t.Errorf("single-broker group wrong: %+v", groups[1])
	}
}

func TestNetPositionsEmpty(t *testing.T) {
	if got := NetPositions(nil); len(got) != 0 {
		t.Errorf("empty in: got %d groups", len(got))
	}
}

func inst2() Instrument {
	return Instrument{Underlying: "BANKNIFTY", Kind: KindIndexOption,
		Expiry: mustDate("2026-10-28"), Strike: 52000, OptionType: Put, LotSize: 30}
}

func TestAggregateState(t *testing.T) {
	mk := func(states ...OrderState) []ChildOrder {
		var kids []ChildOrder
		for _, s := range states {
			kids = append(kids, ChildOrder{State: s})
		}
		return kids
	}
	cases := []struct {
		name  string
		kids  []ChildOrder
		want  OrderState
	}{
		{"all filled", mk(StateFilled, StateFilled), StateFilled},
		{"some partial", mk(StateFilled, StatePartiallyFilled), StatePartiallyFilled},
		{"all open", mk(StateOpen, StateOpen), StateOpen},
		{"all rejected", mk(StateRejected, StateRejected), StateRejected},
		{"all cancelled", mk(StateCancelled, StateCancelled), StateCancelled},
		{"mixed filled+open", mk(StateFilled, StateOpen), StatePartiallyFilled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AggregateState(c.kids); got != c.want {
				t.Errorf("got %s want %s", got, c.want)
			}
		})
	}
}

var _ = time.Now