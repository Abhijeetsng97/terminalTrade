// Package app is the composition root: it wires config, store,
// adapters, market data, engine, and ops into one App that both the
// SSH TUI and the REST API drive.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers/fyersadapter"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers/kiteadapter"
	"github.com/Abhijeetsng97/terminalTrade/internal/config"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
	"github.com/Abhijeetsng97/terminalTrade/internal/engine"
	"github.com/Abhijeetsng97/terminalTrade/internal/marketdata"
	"github.com/Abhijeetsng97/terminalTrade/internal/store"
)

// App owns every long-lived component.
type App struct {
	Cfg    *config.Config
	Store  *store.Store
	Engine *engine.Engine
	Quotes *marketdata.Cache
	Hours  *core.MarketHours

	Kite  *kiteadapter.Adapter
	Fyers *fyersadapter.Adapter

	// adapters slice for iteration (Kite first: chain prefers it).
	Adapters []brokers.BrokerAdapter

	// Risk caps.
	RiskMu sync.Mutex
	Risk   engine.RiskConfig

	UserID string // single-user v1

	// instrument lookups by broker symbol
	symbolMu    sync.RWMutex
	kiteBySym   map[string]core.Instrument
	fyersBySym  map[string]core.Instrument
	byKey       map[string]core.Instrument

	// alerts (ops + runtime)
	alertsMu sync.Mutex
	alerts   []string
}

// New builds the App (does not start background jobs).
func New(ctx context.Context, cfg *config.Config, st *store.Store) (*App, error) {
	hours := core.NewMarketHours()
	quotes := marketdata.NewCache()
	eng := engine.New(st, quotes, hours, core.RealClock)

	a := &App{
		Cfg:    cfg,
		Store:  st,
		Engine: eng,
		Quotes: quotes,
		Hours:  hours,
		UserID: "trader",
		Risk:   engine.NewRiskConfig(),
	}

	// wire adapters when credentials exist (sim-only mode runs without)
	if cfg.KiteAPIKey != "" {
		a.Kite = kiteadapter.New(cfg.KiteAPIKey, cfg.KiteAPISecret)
		a.Adapters = append(a.Adapters, a.Kite)
		eng.RegisterAdapter(a.Kite)
	}
	if cfg.FyersAppID != "" {
		a.Fyers = fyersadapter.New(cfg.FyersAppID, cfg.FyersAppSecret, cfg.FyersRedirectURI, cfg.FyersPIN)
		a.Adapters = append(a.Adapters, a.Fyers)
		eng.RegisterAdapter(a.Fyers)
	}

	// restore sessions + instruments from the store
	if err := a.restoreSessions(ctx); err != nil {
		return nil, err
	}
	a.reloadInstrumentMaps()
	a.Kite.SetSymbolResolver(func(sym string) (core.Instrument, bool) {
		a.symbolMu.RLock()
		defer a.symbolMu.RUnlock()
		i, ok := a.kiteBySym[sym]
		return i, ok
	})
	a.Fyers.SetSymbolResolver(func(sym string) (core.Instrument, bool) {
		a.symbolMu.RLock()
		defer a.symbolMu.RUnlock()
		i, ok := a.fyersBySym[sym]
		return i, ok
	})

	// default risk caps: per-order cap = freeze lots is enforced by
	// the instrument itself; big-order second confirm at 10 lots.
	if a.Risk.BigOrderLots == 0 {
		a.Risk.BigOrderLots = 10
	}
	return a, nil
}

// restoreSessions decrypts stored broker tokens into adapters.
func (a *App) restoreSessions(ctx context.Context) error {
	type credBlob struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	for _, b := range []core.Broker{core.BrokerKite, core.BrokerFyers} {
		raw, err := a.Store.LoadCredentials(ctx, string(b))
		if err != nil || len(raw) == 0 {
			continue // no stored session yet
		}
		var cb credBlob
		if err := jsonUnmarshal(raw, &cb); err != nil || cb.AccessToken == "" {
			continue
		}
		switch b {
		case core.BrokerKite:
			a.Kite.SetAccessToken(cb.AccessToken)
		case core.BrokerFyers:
			a.Fyers.SetAccessToken(cb.AccessToken)
			if cb.RefreshToken != "" {
				a.Fyers.SetRefreshToken(cb.RefreshToken)
			}
		}
	}
	return nil
}

// SaveBrokerTokens persists a broker session (encrypted).
func (a *App) SaveBrokerTokens(ctx context.Context, broker core.Broker, accessToken, refreshToken string) error {
	blob := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, accessToken, refreshToken)
	return a.Store.SaveCredentials(ctx, string(broker), []byte(blob))
}

// RefreshInstruments loads both dumps, merges freeze quantities,
// persists, and reloads the lookup maps (login check + cron + nightly).
func (a *App) RefreshInstruments(ctx context.Context) error {
	merged := map[string]core.Instrument{}
	for _, ad := range a.Adapters {
		insts, err := ad.LoadInstruments(ctx)
		if err != nil {
			return fmt.Errorf("%s instruments: %w", ad.Broker(), err)
		}
		for _, i := range insts {
			key := i.Key()
			if cur, ok := merged[key]; ok {
				// merge broker symbols; keep max lot/freeze
				if cur.BrokerSymbols == nil {
					cur.BrokerSymbols = map[core.Broker]string{}
				}
				for b, s := range i.BrokerSymbols {
					cur.BrokerSymbols[b] = s
				}
				if i.LotSize > 0 {
					cur.LotSize = i.LotSize
				}
				if i.FreezeQty > 0 {
					cur.FreezeQty = i.FreezeQty
				}
				if i.TickSize > 0 {
					cur.TickSize = i.TickSize
				}
				merged[key] = cur
			} else {
				if i.BrokerSymbols == nil {
					i.BrokerSymbols = map[core.Broker]string{}
				}
				merged[key] = i
			}
		}
	}
	list := make([]core.Instrument, 0, len(merged))
	for _, i := range merged {
		list = append(list, i)
	}
	if err := a.Store.SaveInstruments(ctx, list); err != nil {
		return err
	}
	a.reloadInstrumentMaps()
	return nil
}

// reloadInstrumentMaps rebuilds the symbol->instrument lookups from
// the store.
func (a *App) reloadInstrumentMaps() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insts, err := a.Store.LoadInstruments(ctx)
	if err != nil {
		return
	}
	a.symbolMu.Lock()
	defer a.symbolMu.Unlock()
	a.kiteBySym = map[string]core.Instrument{}
	a.fyersBySym = map[string]core.Instrument{}
	a.byKey = map[string]core.Instrument{}
	for _, i := range insts {
		a.byKey[i.Key()] = i
		if s, ok := i.BrokerSymbols[core.BrokerKite]; ok {
			a.kiteBySym[s] = i
		}
		if s, ok := i.BrokerSymbols[core.BrokerFyers]; ok {
			a.fyersBySym[s] = i
		}
	}
}

// InstrumentByKey looks up the canonical instrument.
func (a *App) InstrumentByKey(key string) (core.Instrument, bool) {
	a.symbolMu.RLock()
	defer a.symbolMu.RUnlock()
	i, ok := a.byKey[key]
	return i, ok
}

// UnderlyingKey returns the synthetic canonical key for the
// underlying index LTP.
func (a *App) UnderlyingKey(underlying string) string {
	return underlying + ":INDEX"
}

// ChainRows builds the chain for an underlying + expiry offset.
// Quotes come from the cache; the row set comes from instruments.
func (a *App) ChainRows(underlying string, expiryOffset int) []core.ChainRow {
	insts := a.Instruments()
	expiries := map[string]time.Time{}
	for _, i := range insts {
		if i.Underlying == underlying && i.IsOption() && !i.Expiry.IsZero() {
			expiries[i.Expiry.Format("2006-01-02")] = i.Expiry
		}
	}
	if len(expiries) == 0 {
		return nil
	}
	sorted := make([]string, 0, len(expiries))
	for d := range expiries {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	if expiryOffset >= len(sorted) {
		expiryOffset = expiryOffset % len(sorted)
	}
	target := expiries[sorted[expiryOffset]]

	byStrike := map[float64]*core.ChainRow{}
	strikes := []float64{}
	for _, i := range insts {
		if i.Underlying != underlying || !i.IsOption() || !sameDay(i.Expiry, target) {
			continue
		}
		row, ok := byStrike[i.Strike]
		if !ok {
			row = &core.ChainRow{Strike: i.Strike}
			byStrike[i.Strike] = row
			strikes = append(strikes, i.Strike)
		}
		if q, ok := a.Quotes.Get(i.Key()); ok {
			if i.OptionType == core.Call {
				row.CE = &q
			} else {
				row.PE = &q
			}
		}
	}
	slices.Sort(strikes)
	rows := make([]core.ChainRow, 0, len(strikes))
	for _, s := range strikes {
		rows = append(rows, *byStrike[s])
	}
	return rows
}

// ChainInstrument finds the canonical instrument for a chain cell.
func (a *App) ChainInstrument(underlying string, expiryOffset int, strike float64, otype core.OptionType) *core.Instrument {
	insts := a.Instruments()
	for _, i := range insts {
		if i.Underlying == underlying && i.IsOption() && i.Strike == strike && i.OptionType == otype {
			return &i
		}
	}
	return nil
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Instruments returns the universe (for chain building).
func (a *App) Instruments() []core.Instrument {
	a.symbolMu.RLock()
	defer a.symbolMu.RUnlock()
	out := make([]core.Instrument, 0, len(a.byKey))
	for _, i := range a.byKey {
		out = append(out, i)
	}
	return out
}

// Snapshot aggregates everything the pages need.
type Snapshot struct {
	Positions []core.PositionGroup
	Funds     []core.Funds
	FeedState string
	FeedAt    time.Time
	Market    core.MarketStatus
	Sessions  map[string]brokers.SessionStatus
}

// GetSnapshot fetches positions + funds + statuses in one call.
func (a *App) GetSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Sessions: map[string]brokers.SessionStatus{},
		Market:   a.Hours.StatusAt(time.Now()),
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var positions []core.Position
	for _, ad := range a.Adapters {
		wg.Add(1)
		go func(ad brokers.BrokerAdapter) {
			defer wg.Done()
			pos, err := ad.ListPositions(ctx)
			if err == nil {
				mu.Lock()
				positions = append(positions, pos...)
				mu.Unlock()
			}
			f, err := ad.GetFunds(ctx)
			if err == nil {
				mu.Lock()
				snap.Funds = append(snap.Funds, f)
				mu.Unlock()
			}
			ss := ad.SessionStatus(ctx)
			mu.Lock()
			snap.Sessions[string(ad.Broker())] = ss
			mu.Unlock()
		}(ad)
	}
	wg.Wait()
	snap.Positions = core.NetPositions(positions)
	state, at := a.Quotes.State()
	snap.FeedState = string(state)
	snap.FeedAt = at
	return snap
}

// jsonUnmarshal decodes stored credential blobs.
func jsonUnmarshal(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}