// Sim-mode wiring: two simulated brokers (SIM-KITE, SIM-FYERS), a
// random-walk market feed, seeded instruments and positions. Lets the
// whole app run locally with zero broker credentials.
package app

import (
	"context"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
	"github.com/Abhijeetsng97/terminalTrade/internal/sim"
)

// simBrokers are the two simulated brokers. They satisfy Adapter via
// the sim package's Adapter and are namespaced so netting/routing
// behave like two distinct brokers.
type simBroker struct {
	name core.Broker
	*sim.Adapter
}

func (s simBroker) Broker() core.Broker { return s.name }

// wireSim builds the sim universe and adapters.
func (a *App) wireSim(ctx context.Context) {
	kt := simBroker{name: core.BrokerKite, Adapter: sim.New()}
	ft := simBroker{name: core.BrokerFyers, Adapter: sim.New()}

	mkt := sim.NewMarket()
	insts := mkt.Seed(a.Cfg.Underlyings)

	kt.WithInstruments(insts)
	ft.WithInstruments(insts)
	kt.WithPositions(mkt.SeedPositions(core.BrokerKite, core.BrokerFyers)...)

	a.Adapters = append(a.Adapters, kt, ft)
	a.Engine.RegisterAdapter(kt)
	a.Engine.RegisterAdapter(ft)

	// force-open: sim market trades at any hour
	if a.Cfg.SimAlwaysOpen {
		a.Hours.SetOpenOverride(true)
	}

	// seed the quote cache + start the tick loop
	seedQs := mkt.Step(time.Now())
	for _, q := range seedQs {
		a.Quotes.Set(q, "KITE", true)
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				for _, q := range mkt.Step(now) {
					a.Quotes.Set(q, "KITE", true)
				}
			}
		}
	}()
	a.simMarket = mkt
}