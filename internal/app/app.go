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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers/fyers"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers/fyersadapter"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers/kiteadapter"
	"github.com/Abhijeetsng97/terminalTrade/internal/config"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
	"github.com/Abhijeetsng97/terminalTrade/internal/engine"
	"github.com/Abhijeetsng97/terminalTrade/internal/marketdata"
	"github.com/Abhijeetsng97/terminalTrade/internal/sim"
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
	// chain-cell index (UNDERLYING|EXPIRY|STRIKE|TYPE -> instrument)
	// and per-underlying sorted expiries — avoids 36k scans
	byChainCell          map[string]core.Instrument
	expiriesByUnderlying map[string][]time.Time

	// alerts (ops + runtime)
	alertsMu sync.Mutex
	alerts   []string

	// sim market (sim mode only)
	simMarket *sim.Market

	// quote-poller single-instance guard
	pollMu      sync.Mutex
	pollRunning bool
	// poller target (underlying, expiry offset) — retargetable so the
	// TUI can point it at whatever expiry the user is viewing
	pollTargetMu   sync.Mutex
	pollUnderlying string
	pollOffset     int
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
	if cfg.Sim {
		a.wireSim(ctx)
	} else if cfg.KiteAPIKey != "" {
		a.Kite = kiteadapter.New(cfg.KiteAPIKey, cfg.KiteAPISecret)
		a.Adapters = append(a.Adapters, a.Kite)
		eng.RegisterAdapter(a.Kite)
	}
	if !cfg.Sim && cfg.FyersAppID != "" {
		a.Fyers = fyersadapter.New(cfg.FyersAppID, cfg.FyersAppSecret, cfg.FyersRedirectURI, cfg.FyersPIN)
		a.Adapters = append(a.Adapters, a.Fyers)
		eng.RegisterAdapter(a.Fyers)
	}

	// restore sessions + instruments from the store (real brokers
	// only; sim seeds its own)
	if !cfg.Sim {
		if err := a.restoreSessions(ctx); err != nil {
			return nil, err
		}
	}
	a.reloadInstrumentMaps()
	if a.Kite != nil {
		a.Kite.SetSymbolResolver(func(sym string) (core.Instrument, bool) {
			a.symbolMu.RLock()
			defer a.symbolMu.RUnlock()
			i, ok := a.kiteBySym[sym]
			return i, ok
		})
	}
	if a.Fyers != nil {
		a.Fyers.SetSymbolResolver(func(sym string) (core.Instrument, bool) {
			a.symbolMu.RLock()
			defer a.symbolMu.RUnlock()
			i, ok := a.fyersBySym[sym]
			return i, ok
		})
	}
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

// RefreshInstruments loads instrument dumps per broker and merges
// them into the store. Per-broker tolerant: one broker failing does
// NOT block the others (the chain runs on whichever broker loaded).
func (a *App) RefreshInstruments(ctx context.Context) error {
	merged := map[string]core.Instrument{}
	var failures []string
	for _, ad := range a.Adapters {
		insts, err := ad.LoadInstruments(ctx)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ad.Broker(), err))
			continue // Kite alone can still carry the chain
		}
		// broker provided nothing (e.g. Fyers mapping deferred):
		// synthesize from the canonical model where documented
		if len(insts) == 0 {
			insts = a.deriveSymbolsFor(ad.Broker())
		}
		for _, i := range insts {
			key := i.Key()
			if cur, ok := merged[key]; ok {
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
	if len(merged) == 0 {
		return fmt.Errorf("no instruments loaded (errors: %v)", failures)
	}
	list := make([]core.Instrument, 0, len(merged))
	for _, i := range merged {
		list = append(list, i)
	}
	if err := a.Store.SaveInstruments(ctx, list); err != nil {
		return err
	}
	a.reloadInstrumentMaps()
	a.alertf("instruments loaded: %d", len(list))
	if len(failures) > 0 {
		a.alertf("instrument refresh partial: %s", strings.Join(failures, "; "))
	}
	return nil
}

// deriveSymbolsFor synthesizes broker symbols from the canonical
// instruments already in the store. v1: Fyers index-option symbology
// is documented and deterministic (NSE:NIFTY26O2926000CE); wrong
// derivations surface as broker rejections (-300) at order time.
// Filled instruments get the derived symbol merged into the store.
func (a *App) deriveSymbolsFor(b core.Broker) []core.Instrument {
	if b != core.BrokerFyers {
		return nil
	}
	existing, err := a.Store.LoadInstruments(context.Background())
	if err != nil || len(existing) == 0 {
		return nil
	}
	var out []core.Instrument
	changed := false
	for _, i := range existing {
		if i.IsOption() && i.Kind == core.KindIndexOption && i.BrokerSymbols[b] == "" {
			if sym, ok := fyers.DeriveSymbol(i); ok {
				if i.BrokerSymbols == nil {
					i.BrokerSymbols = map[core.Broker]string{}
				}
				i.BrokerSymbols[b] = sym
				changed = true
			}
		}
		out = append(out, i)
	}
	if changed {
		a.alertf("fyers symbols derived for index options (validate first trade)")
	}
	return out
}

// reloadInstrumentMaps rebuilds the symbol->instrument lookups from
// the store. Also builds the chain index (underlying+expiry+strike+
// type -> instrument) so form opens and chain builds never scan the
// 36k universe.
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
	a.byChainCell = map[string]core.Instrument{}
	for _, i := range insts {
		a.byKey[i.Key()] = i
		if s, ok := i.BrokerSymbols[core.BrokerKite]; ok {
			a.kiteBySym[s] = i
		}
		if s, ok := i.BrokerSymbols[core.BrokerFyers]; ok {
			a.fyersBySym[s] = i
		}
		// chain-cell index: UNDERLYING|YYYYMMDD|STRIKE|TYPE
		cell := chainCellKey(i)
		a.byChainCell[cell] = i
	}
	// expiries per underlying
	a.expiriesByUnderlying = map[string][]time.Time{}
	for _, i := range insts {
		if !i.IsOption() || i.Expiry.IsZero() {
			continue
		}
		a.expiriesByUnderlying[i.Underlying] = append(a.expiriesByUnderlying[i.Underlying], i.Expiry)
	}
	for u := range a.expiriesByUnderlying {
		dedup := map[string]time.Time{}
		for _, e := range a.expiriesByUnderlying[u] {
			dedup[e.Format("2006-01-02")] = e
		}
		var list []time.Time
		for _, e := range dedup {
			list = append(list, e)
		}
		sort.Slice(list, func(x, j int) bool { return list[x].Before(list[j]) })
		a.expiriesByUnderlying[u] = list
	}
}

// chainCellKey builds the chain-lookup key for an instrument.
func chainCellKey(i core.Instrument) string {
	return i.Underlying + "|" + i.Expiry.Format("20060102") + "|" +
		strconv.FormatFloat(i.Strike, 'f', -1, 64) + "|" + string(i.OptionType)
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

// ChainExpiries — superseded by the index-based version above.

// ChainRows builds the chain for an underlying + expiry offset from
// the chain-cell index (O(rows), no universe scans).
func (a *App) ChainRows(underlying string, expiryOffset int) []core.ChainRow {
	exps := a.ChainExpiries(underlying)
	if len(exps) == 0 {
		return nil
	}
	off := expiryOffset % len(exps)
	expiry := exps[off]

	a.symbolMu.RLock()
	defer a.symbolMu.RUnlock()
	// collect strikes for this underlying+expiry from the cell index
	byStrike := map[float64]*core.ChainRow{}
	var strikes []float64
	prefix := underlying + "|" + expiry.Format("20060102") + "|"
	for cell, i := range a.byChainCell {
		if !strings.HasPrefix(cell, prefix) {
			continue
		}
		row, ok := byStrike[i.Strike]
		if !ok {
			row = &core.ChainRow{Strike: i.Strike}
			byStrike[i.Strike] = row
			strikes = append(strikes, i.Strike)
		}
		if i.OptionType == core.Call {
			ce := i
			row.CE = &ce
			if q, ok := a.Quotes.Get(i.Key()); ok {
				qc := q
				row.CEQuote = &qc
			}
		} else {
			pe := i
			row.PE = &pe
			if q, ok := a.Quotes.Get(i.Key()); ok {
				qp := q
				row.PEQuote = &qp
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

// ChainInstrument finds the canonical instrument for a chain cell
// (O(1) via the chain index; rebuilds nothing).
func (a *App) ChainInstrument(underlying string, expiryOffset int, strike float64, otype core.OptionType) *core.Instrument {
	// resolve the expiry for the offset
	exps := a.ChainExpiries(underlying)
	if len(exps) == 0 {
		return nil
	}
	off := expiryOffset % len(exps)
	cell := underlying + "|" + exps[off].Format("20060102") + "|" +
		strconv.FormatFloat(strike, 'f', -1, 64) + "|" + string(otype)
	a.symbolMu.RLock()
	defer a.symbolMu.RUnlock()
	if i, ok := a.byChainCell[cell]; ok {
		return &i
	}
	return nil
}

// ChainExpiries returns the sorted non-expired expiries for an
// underlying (from the reload-time index).
func (a *App) ChainExpiries(underlying string) []time.Time {
	a.symbolMu.RLock()
	defer a.symbolMu.RUnlock()
	today := time.Now().In(core.LocalIST())
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, core.LocalIST())
	all := a.expiriesByUnderlying[underlying]
	out := make([]time.Time, 0, len(all))
	for _, e := range all {
		if !e.Before(today) {
			out = append(out, e)
		}
	}
	return out
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
	// FundsAt is when the funds rows were fetched.
	FundsAt time.Time
}

// GetSnapshot fetches positions + funds + statuses in one call.
func (a *App) GetSnapshot(ctx context.Context) Snapshot {
	snap := Snapshot{
		Sessions: map[string]brokers.SessionStatus{},
		Market:   a.Hours.StatusAt(time.Now()),
		FundsAt:  time.Now(),
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