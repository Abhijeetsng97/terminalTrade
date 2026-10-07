// Package core defines terminalTrade's canonical domain types.
//
// Everything in the system speaks these types. Broker-specific symbol
// formats and API payloads are translated at the adapter boundary and
// never leak past it. This is the vocabulary the spec calls the
// canonical instrument model.
package core

import "time"

// Broker identifies a connected broker.
type Broker string

const (
	BrokerKite  Broker = "KITE"
	BrokerFyers Broker = "FYERS"
)

// OptionType is the CE/PE side of an option.
type OptionType string

const (
	Call OptionType = "CE"
	Put  OptionType = "PE"
)

// InstrumentKind is the class of derivative contract.
type InstrumentKind string

const (
	KindIndexOption  InstrumentKind = "OPTIDX"
	KindStockOption  InstrumentKind = "OPTSTK"
	KindIndexFuture  InstrumentKind = "FUTIDX"
	KindStockFuture  InstrumentKind = "FUTSTK"
)

// Instrument is the canonical instrument model. Key() is the identity
// used across brokers, collections, and the UI. LotSize and FreezeQty
// are exchange facts loaded from the instruments refresh pipeline and
// must never be hardcoded in call sites.
type Instrument struct {
	// Underlying index or stock symbol, upper-cased, exchange-normalized (e.g. NIFTY).
	Underlying string
	Kind       InstrumentKind
	// Expiry date in IST (zero value for non-expiring instruments).
	Expiry time.Time
	// Strike price; zero for futures.
	Strike float64
	// OptionType is CE/PE; empty for futures.
	OptionType OptionType

	// LotSize is quantity per lot (e.g. 65 for NIFTY options as of 2026).
	LotSize int
	// FreezeQty is the exchange maximum quantity per single order
	// (e.g. 3510 for NIFTY as of 2026-10-05). Orders above this are sliced.
	FreezeQty int

	// BrokerSymbols maps Broker -> that broker's tradingsymbol.
	// An instrument missing an entry for a broker cannot be traded there.
	BrokerSymbols map[Broker]string
	// TickSize is the minimum price increment.
	TickSize float64
}

// Key returns the canonical identity: UNDERLYING:KIND:EXPIRY:STRIKE:TYPE.
// Futures omit strike/type (zero/empty render as "-").
func (i Instrument) Key() string {
	strike := "-"
	otype := "-"
	if i.Strike > 0 {
		strike = formatStrike(i.Strike)
	}
	if i.OptionType != "" {
		otype = string(i.OptionType)
	}
	expiry := "-"
	if !i.Expiry.IsZero() {
		expiry = i.Expiry.Format("20060102")
	}
	return i.Underlying + ":" + string(i.Kind) + ":" + expiry + ":" + strike + ":" + otype
}

// MaxLotsPerOrder derives the freeze limit in lots: FreezeQty / LotSize.
// NIFTY today: 3510 / 65 = 54. Never assume; always derive.
func (i Instrument) MaxLotsPerOrder() int {
	if i.LotSize <= 0 {
		return 0
	}
	return i.FreezeQty / i.LotSize
}

// IsOption reports whether the instrument is an option.
func (i Instrument) IsOption() bool {
	return i.Kind == KindIndexOption || i.Kind == KindStockOption
}

func formatStrike(s float64) string {
	if s == float64(int64(s)) {
		return itoa(int64(s))
	}
	return trimFloat(s)
}