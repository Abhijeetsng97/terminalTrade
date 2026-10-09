// Real-mode quote polling: Kite's REST quote API (1 req/s limit)
// feeds the canonical quote cache while the TUI is open. The Kite
// ticker (WS) upgrade lands later; polling is the v1 floor and works
// with the same fallback cadences as the spec (chain 10s via this
// poller; positions 1s via GetSnapshot).
package app

import (
	"context"
	"strings"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// StartQuotePoller begins polling Kite quotes for the instruments on
// the requested expiry. Idempotent: a second call while one is
// running only retargets the poller (the session watcher and the TUI
// both call this). Stop via the returned func. No-op when Kite isn't
// configured.
func (a *App) StartQuotePoller(ctx context.Context, underlying string, expiryOffset int) (stop func()) {
	if a.Kite == nil {
		return func() {}
	}
	a.pollMu.Lock()
	defer a.pollMu.Unlock()
	a.pollTargetMu.Lock()
	a.pollUnderlying, a.pollOffset = underlying, expiryOffset
	a.pollTargetMu.Unlock()
	if a.pollRunning {
		return func() {} // already polling the (retargeted) stream
	}
	a.pollRunning = true
	done := make(chan struct{})
	go func() {
		defer func() {
			a.pollMu.Lock()
			a.pollRunning = false
			a.pollMu.Unlock()
		}()
		t := time.NewTicker(2 * time.Second) // kite rate limit: ~1/s; 2s is safe
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				u, off := a.PollTarget()
				a.pollQuotesOnce(ctx, u, off)
			}
		}
	}()
	return func() { close(done) }
}

// RetargetPoller points the running quote poller at a new
// underlying/expiry (the TUI calls this when the user presses w or
// switches underlyings). Cheap: an atomic-ish swap, no network I/O.
func (a *App) RetargetPoller(underlying string, expiryOffset int) {
	if a.Kite == nil {
		return
	}
	a.pollTargetMu.Lock()
	a.pollUnderlying, a.pollOffset = underlying, expiryOffset
	a.pollTargetMu.Unlock()
}

// PollTarget reads the current poller target.
func (a *App) PollTarget() (string, int) {
	a.pollTargetMu.Lock()
	defer a.pollTargetMu.Unlock()
	return a.pollUnderlying, a.pollOffset
}

// pollQuotesOnce fetches quotes for the chain strikes + underlying.
// Not market-hours gated: Kite's GetQuote returns the last close price
// after the session, so the chain keeps showing data for testing and
// review. (Order placement is where the broker — not us — enforces
// market hours.)
func (a *App) pollQuotesOnce(ctx context.Context, underlying string, expiryOffset int) {
	insts := a.chainInstruments(underlying, expiryOffset)
	syms := make([]string, 0, len(insts)+2)
	symToKey := map[string]string{}
	for _, i := range insts {
		if s, ok := i.BrokerSymbols[core.BrokerKite]; ok {
			syms = append(syms, "NFO:"+s)
			symToKey["NFO:"+s] = i.Key()
		}
	}
	// the underlying index itself (synthetic :INDEX key) so ATM
	// centering and the header have a real LTP
	if idxSym := kiteIndexSymbol(underlying); idxSym != "" {
		syms = append(syms, idxSym)
		symToKey[idxSym] = a.UnderlyingKey(underlying)
	}
	if len(syms) == 0 {
		return
	}
	quotes, err := a.Kite.RawQuotes(ctx, syms)
	if err != nil {
		a.Quotes.MarkWSDown(core.BrokerKite) // flipped to POLL state
		return
	}
	a.Quotes.PollTick(core.BrokerKite)
	now := time.Now()
	idxLTP := 0.0
	for sym, q := range quotes {
		key, ok := symToKey[sym]
		if !ok {
			continue
		}
		a.Quotes.Set(core.Quote{
			InstrumentKey: key,
			LTP:           q.LastPrice,
			Bid:           q.Bid,
			Ask:           q.Ask,
			OI:            int64(q.OI),
			BidQty:        q.BidQty,
			AskQty:        q.AskQty,
			Source:        core.BrokerKite,
			At:            now,
		}, core.BrokerKite, false) // false: REST poll, not WS
		if strings.HasSuffix(sym, "-INDEX") {
			idxLTP = q.LastPrice
		}
	}
	_ = idxLTP // read via Quotes.LTP(UnderlyingKey) elsewhere
}

// kiteIndexSymbol: Kite's index quote symbol for an underlying.
func kiteIndexSymbol(u string) string {
	switch strings.ToUpper(u) {
	case "NIFTY":
		return "NSE:NIFTY 50" // Kite index quote key
	case "BANKNIFTY":
		return "NSE:NIFTY BANK"
	case "FINNIFTY":
		return "NSE:FIN NIFTY"
	case "MIDCPNIFTY":
		return "NSE:NIFTY MID SELECT"
	case "SENSEX":
		return "BSE:SENSEX"
	default:
		return ""
	}
}

// chainInstruments returns the instruments for the chain page —
// same expiry selection as ChainRows (index-based, no 36k scans) so
// the poller always quotes exactly what the TUI displays.
func (a *App) chainInstruments(underlying string, expiryOffset int) []core.Instrument {
	exps := a.ChainExpiries(underlying)
	if len(exps) == 0 {
		return nil
	}
	off := expiryOffset % len(exps)
	expiry := exps[off]

	a.symbolMu.RLock()
	defer a.symbolMu.RUnlock()
	prefix := underlying + "|" + expiry.Format("20060102") + "|"
	out := make([]core.Instrument, 0, 512)
	for cell, i := range a.byChainCell {
		if strings.HasPrefix(cell, prefix) {
			out = append(out, i)
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var _ = strings.TrimSpace