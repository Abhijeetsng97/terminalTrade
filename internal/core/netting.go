package core

import (
	"sort"
	"time"
)

// Netting: collapse per-broker positions into canonical groups.
// +5 NIFTY 25000CE at Kite and -3 at Fyers read as NET +2 with
// per-broker rows preserved (spec decision on positions view).

type PositionGroup struct {
	Instrument Instrument
	// NetLots is the summed signed lots across brokers.
	NetLots float64
	// BrokerRows are per-broker legs (sorted by broker for stability).
	BrokerRows []Position
	// NetPnL sums the per-broker P&L.
	NetPnL float64
}

// NetPositions groups positions by canonical key. Single-broker
// groups keep one row; multi-broker groups get a NET header.
func NetPositions(positions []Position) []PositionGroup {
	byKey := make(map[string]*PositionGroup)
	order := []string{}
	for _, p := range positions {
		key := p.Instrument.Key()
		g, ok := byKey[key]
		if !ok {
			g = &PositionGroup{Instrument: p.Instrument}
			byKey[key] = g
			order = append(order, key)
		}
		g.NetLots += p.Lots
		g.NetPnL += p.PnL
		g.BrokerRows = append(g.BrokerRows, p)
	}
	for _, g := range byKey {
		sort.Slice(g.BrokerRows, func(i, j int) bool {
			return g.BrokerRows[i].Broker < g.BrokerRows[j].Broker
		})
	}
	groups := make([]PositionGroup, 0, len(byKey))
	for _, k := range order {
		groups = append(groups, *byKey[k])
	}
	return groups
}

// Quote is a canonical quote for an instrument (chain rows, LTPs).
type Quote struct {
	InstrumentKey string
	// LTP, top-of-book bid/ask, and open interest.
	LTP  float64
	Bid  float64
	Ask  float64
	OI   int64
	// Depth quantities at top of book.
	BidQty int
	AskQty int
	// Source broker of this quote (Kite preferred in the chain).
	Source Broker
	// At is the tick time.
	At time.Time
}

// ChainRow is one strike's CE + PE quotes for the chain page.
type ChainRow struct {
	Strike float64
	CE     *Quote
	PE     *Quote
}