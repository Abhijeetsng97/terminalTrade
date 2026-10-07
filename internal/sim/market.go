// Sim market feed: a random-walk price generator that makes local
// testing feel alive — the option chain and underlying tick every
// second. Seeds NIFTY/BANKNIFTY around ATM-like strikes with realistic
// CE/PE premia (roughly sqrt-moneyness scaling), plus live positions
// on both simulated brokers.
package sim

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// UnderlyingState tracks the random walk per underlying.
type UnderlyingState struct {
	Price float64
	Drift float64
	Vol   float64
	rng   *rand.Rand
}

// Market generates quotes for the seeded universe continuously.
type Market struct {
	mu          sync.Mutex
	underlyings map[string]*UnderlyingState
	insts       []core.Instrument
}

func NewMarket() *Market {
	return &Market{underlyings: map[string]*UnderlyingState{}}
}

// Seed builds the instrument universe for the given underlyings:
// the nearest weekly expiry (Thursday) with strikes around today's
// presumed level, and open positions on both sim brokers.
func (m *Market) Seed(underlyings []string) []core.Instrument {
	now := time.Now().In(core.LocalIST())
	// nearest Thursday expiry
	offset := (4 - int(now.Weekday()) + 7) % 7
	if offset == 0 {
		offset = 7
	}
	expiry := time.Date(now.Year(), now.Month(), now.Day()+offset, 15, 30, 0, 0, core.LocalIST())

	base := map[string]float64{"NIFTY": 26120, "BANKNIFTY": 58900}
	var insts []core.Instrument
	for _, u := range underlyings {
		px, ok := base[u]
		if !ok {
			px = 10000
		}
		lot, freeze := lotFreezeFallback(u)
		st := &UnderlyingState{Price: px, Drift: 0.0, Vol: 0.0012, rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
		m.underlyings[u] = st

		for strike := int(px) - 500; strike <= int(px)+500; strike += 100 {
			for _, ot := range []core.OptionType{core.Call, core.Put} {
				inst := core.Instrument{
					Underlying: u, Kind: core.KindIndexOption,
					Expiry: expiry, Strike: float64(strike), OptionType: ot,
					LotSize: lot, FreezeQty: freeze, TickSize: 0.05,
				}
				inst.BrokerSymbols = map[core.Broker]string{
					core.BrokerKite:  simSymbol(u, expiry, float64(strike), ot, "KITE"),
					core.BrokerFyers: simSymbol(u, expiry, float64(strike), ot, "FYERS"),
				}
				insts = append(insts, inst)
			}
		}
	}
	m.mu.Lock()
	m.insts = insts
	m.mu.Unlock()
	return insts
}

// lotFreezeFallback mirrors the current NSE limits (2026-10-05+).
func lotFreezeFallback(u string) (int, int) {
	switch strings.ToUpper(u) {
	case "NIFTY":
		return 65, 3510
	case "BANKNIFTY":
		return 30, 1440
	case "FINNIFTY":
		return 65, 3240
	case "MIDCPNIFTY":
		return 120, 5760
	default:
		return 65, 3510
	}
}

// simSymbol renders a plausible broker symbol.
func simSymbol(u string, expiry time.Time, strike float64, ot core.OptionType, broker string) string {
	m := expiry.Month()
	var mchar byte
	switch {
	case m >= 1 && m <= 9:
		mchar = byte('0' + m)
	case m == 10:
		mchar = 'O'
	case m == 11:
		mchar = 'N'
	case m == 12:
		mchar = 'D'
	}
	return fmt.Sprintf("%s%s%d%.0f%s-%s", u, expiry.Format("06"), int(mchar), strike, ot, broker)
}

// Step advances the random walk and returns quotes for every option
// plus the underlying (synthetic key UNDERLYING:INDEX).
func (m *Market) Step(t time.Time) []core.Quote {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.Quote
	for u, st := range m.underlyings {
		// GBM-ish random walk
		shock := st.rng.NormFloat64() * st.Vol
		st.Price = math.Max(1, st.Price*(1+st.Drift+shock))
		out = append(out, core.Quote{
			InstrumentKey: u + ":INDEX", LTP: st.Price, Source: core.BrokerKite, At: t,
		})
		for _, inst := range m.insts {
			if inst.Underlying != u || !inst.IsOption() {
				continue
			}
			ltp := optionPremium(st.Price, inst.Strike, inst.OptionType)
			spread := math.Max(0.05, ltp*0.004)
			q := core.Quote{
				InstrumentKey: inst.Key(),
				LTP:           round05(ltp),
				Bid:           round05(ltp - spread/2),
				Ask:           round05(ltp + spread/2),
				OI:            int64(50000 + st.rng.Intn(450000)),
				Source:        "SIM-KITE",
				At:            t,
			}
			out = append(out, q)
		}
	}
	return out
}

// optionPremium: intrinsic + rough time value by sqrt distance.
func optionPremium(spot, strike float64, ot core.OptionType) float64 {
	moneyness := strike - spot
	if ot == core.Put {
		moneyness = -moneyness
	}
	intrinsic := math.Max(0, moneyness)
	timeValue := 120 * math.Exp(-math.Abs(moneyness)/600)
	return intrinsic + timeValue
}

func round05(f float64) float64 {
	return math.Round(f/0.05) * 0.05
}

// SeedPositions gives open positions on both sim brokers: long on
// the ATM-adjacent strike at KITE, short at FYERS on the same
// instrument — exercises cross-broker netting end to end.
func (m *Market) SeedPositions(brokerA, brokerB core.Broker) []core.Position {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.Position
	now := time.Now()
	for _, inst := range m.insts {
		if inst.OptionType == core.Call && inst.Strike == roundedStrike(m.underlyingPrice(inst.Underlying)) {
			out = append(out,
				core.Position{Instrument: inst, Broker: brokerA, Lots: 5, Qty: 5 * inst.LotSize, AvgPrice: 100, LastPrice: inst.Strike * 0.01, PnL: 1520, UpdatedAt: now},
				core.Position{Instrument: inst, Broker: brokerB, Lots: -3, Qty: -3 * inst.LotSize, AvgPrice: 98, LastPrice: inst.Strike * 0.01, PnL: -640, UpdatedAt: now},
			)
		}
	}
	return out
}

// underlyingPrice reads the seeded start price.
func (m *Market) underlyingPrice(u string) float64 {
	if st, ok := m.underlyings[u]; ok {
		return st.Price
	}
	return 0
}

// roundedStrike picks the 100-round strike just below the price.
func roundedStrike(price float64) float64 {
	return float64(int(price/100)) * 100
}
