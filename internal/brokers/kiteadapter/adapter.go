// Package kiteadapter wraps zerodha/gokiteconnect behind the
// BrokerAdapter seam. Kite symbol formats and payloads never leak
// past this package.
package kiteadapter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// Adapter talks to Kite Connect v4.
type Adapter struct {
	mu        sync.Mutex
	kc        *kiteconnect.Client
	apiKey    string
	apiSecret string
	accessToken string
	resolve   SymbolResolver
}

func New(apiKey, apiSecret string) *Adapter {
	return &Adapter{
		kc:       kiteconnect.New(apiKey),
		apiKey:   apiKey,
		apiSecret: apiSecret,
	}
}

func (a *Adapter) Broker() core.Broker { return core.BrokerKite }

// SetAccessToken loads a stored (decrypted) session token.
func (a *Adapter) SetAccessToken(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessToken = tok
	a.kc.SetAccessToken(tok)
}

// CurrentTokens exposes the live access token (for persistence).
func (a *Adapter) CurrentTokens() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken, ""
}

// LoginURL returns the browser login step URL.
func (a *Adapter) LoginURL() string { return a.kc.GetLoginURL() }

// ExchangeManualToken completes the daily manual flow: the user
// pastes the request_token from the redirect URL after browser login.
func (a *Adapter) ExchangeManualToken(ctx context.Context, requestToken string) (string, error) {
	sess, err := a.kc.GenerateSession(requestToken, a.apiSecret)
	if err != nil {
		return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: err.Error(), Err: err}
	}
	a.SetAccessToken(sess.AccessToken)
	return "kite session established for " + sess.UserID, nil
}

// SessionStatus: Kite tokens die daily; validate with a cheap call.
func (a *Adapter) SessionStatus(ctx context.Context) brokers.SessionStatus {
	flow := "open " + a.LoginURL() + " | login, copy request_token from redirect URL, paste here (L)"
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.kc.GetUserProfile()
	if err != nil {
		return brokers.SessionStatus{
			Broker: core.BrokerKite, Valid: false,
			Reason: "token missing or rejected: " + err.Error(), ManualFlow: flow,
		}
	}
	return brokers.SessionStatus{Broker: core.BrokerKite, Valid: true}
}

// PlaceOrder: always a LIMIT order (MKT-PROT is a limit at the
// protection band; market orders are never sent).
func (a *Adapter) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	resp, err := a.kc.PlaceOrder("regular", kiteconnect.OrderParams{
		Exchange:        exchangeOf(req.Instrument),
		Tradingsymbol:   req.Instrument.BrokerSymbols[core.BrokerKite],
		TransactionType: string(req.Side),
		OrderType:       "LIMIT",
		Quantity:        req.Qty,
		Price:           req.LimitPrice,
		Product:         "NRML",
		Validity:        "DAY",
		Tag:             req.IdempotencyTag,
	})
	if err != nil {
		return "", classifyKite(err)
	}
	return resp.OrderID, nil
}

// ModifyOrder changes price/qty of a working order.
func (a *Adapter) ModifyOrder(ctx context.Context, m brokers.ModifyRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	params := kiteconnect.OrderParams{}
	if m.NewQty > 0 {
		params.Quantity = m.NewQty
	}
	if m.NewPrice > 0 {
		params.Price = m.NewPrice
		params.OrderType = "LIMIT"
	}
	_, err := a.kc.ModifyOrder("regular", m.BrokerOrderID, params)
	if err != nil {
		return classifyKite(err)
	}
	return nil
}

// CancelOrder cancels a working order.
func (a *Adapter) CancelOrder(ctx context.Context, brokerOrderID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.kc.CancelOrder("regular", brokerOrderID, nil)
	if err != nil {
		return classifyKite(err)
	}
	return nil
}

// ListOrders maps Kite's order book into canonical rows.
func (a *Adapter) ListOrders(ctx context.Context) ([]brokers.BrokerOrder, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	orders, err := a.kc.GetOrders()
	if err != nil {
		return nil, classifyKite(err)
	}
	out := make([]brokers.BrokerOrder, 0, len(orders))
	for _, o := range orders {
		out = append(out, brokers.BrokerOrder{
			BrokerOrderID: o.OrderID,
			Symbol:        o.TradingSymbol,
			InstrumentKey: "KITE/" + o.TradingSymbol,
			Side:          core.Side(strings.ToUpper(o.TransactionType)),
			Qty:           int(o.Quantity),
			FilledQty:     int(o.FilledQuantity),
			OrderType:     core.TypeLimit,
			LimitPrice:    o.Price,
			Status:        mapKiteStatus(o.Status),
			Tag:           o.Tag,
			ExchangeTime:  o.ExchangeTimestamp.Time,
		})
	}
	return out, nil
}

func mapKiteStatus(s string) core.OrderState {
	switch strings.ToUpper(s) {
	case "COMPLETE":
		return core.StateFilled
	case "OPEN", "AMO REQ RECEIVED":
		return core.StateOpen
	case "REJECTED":
		return core.StateRejected
	case "CANCELLED", "CANCEL AMO REQ RECEIVED":
		return core.StateCancelled
	default:
		return core.StateOpen
	}
}

// ListTrades returns today's fills.
func (a *Adapter) ListTrades(ctx context.Context) ([]brokers.BrokerTrade, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	trades, err := a.kc.GetTrades()
	if err != nil {
		return nil, classifyKite(err)
	}
	out := make([]brokers.BrokerTrade, 0, len(trades))
	for _, t := range trades {
		out = append(out, brokers.BrokerTrade{
			TradeID:       t.TradeID,
			BrokerOrderID: t.OrderID,
			InstrumentKey: "KITE/" + t.TradingSymbol,
			Side:          core.Side(strings.ToUpper(t.TransactionType)),
			Qty:           int(t.Quantity),
			Price:         t.AveragePrice,
			ExchangeTime:  t.ExchangeTimestamp.Time,
		})
	}
	return out, nil
}

// ListPositions maps Kite net positions to canonical. Requires the
// instrument universe (set via SetSymbolResolver) to resolve lot
// sizes and canonical keys.

// SymbolResolver resolves a Kite tradingsymbol to a canonical
// instrument. Wired at startup from the instruments table.
type SymbolResolver func(tradingSymbol string) (core.Instrument, bool)

// SetSymbolResolver wires the instruments-table lookup.
func (a *Adapter) SetSymbolResolver(r SymbolResolver) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resolve = r
}

// resolveSymbol falls back to a raw-symbol instrument when no
// resolver is wired (positions still render, marked unmapped).
func (a *Adapter) resolveSymbol(s string) (core.Instrument, bool) {
	if a.resolve == nil {
		return core.Instrument{Underlying: s}, false
	}
	return a.resolve(s)
}

// ListPositions maps positions.
func (a *Adapter) ListPositions(ctx context.Context) ([]core.Position, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	positions, err := a.kc.GetPositions()
	if err != nil {
		return nil, classifyKite(err)
	}
	out := make([]core.Position, 0, len(positions.Net))
	for _, p := range positions.Net {
		inst, _ := a.resolveSymbol(p.Tradingsymbol)
		var lots float64
		if inst.LotSize > 0 {
			lots = float64(p.Quantity) / float64(inst.LotSize)
		}
		out = append(out, core.Position{
			Instrument: inst,
			Broker:     core.BrokerKite,
			Lots:       lots,
			Qty:        p.Quantity,
			AvgPrice:   p.AveragePrice,
			LastPrice:  p.LastPrice,
			PnL:        p.PnL,
			UpdatedAt:  time.Now(),
		})
	}
	return out, nil
}

// GetFunds: equity-segment margins.
func (a *Adapter) GetFunds(ctx context.Context) (core.Funds, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	margins, err := a.kc.GetUserMargins()
	if err != nil {
		return core.Funds{}, classifyKite(err)
	}
	eq := margins.Equity
	av := eq.Available.LiveBalance
	used := eq.Used.Debits
	return core.Funds{
		Broker: core.BrokerKite, Available: av, Used: used, Total: av + used, UpdatedAt: time.Now(),
	}, nil
}

// LoadInstruments: Kite's NFO CSV dump -> canonical instruments.
// Freeze quantities are NOT in Kite's dump; they are filled by the
// NSE-contract-file refresh (ops pipeline) and default to 0 here.
func (a *Adapter) LoadInstruments(ctx context.Context) ([]core.Instrument, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := a.kc.GetInstrumentsByExchange("NFO")
	if err != nil {
		return nil, classifyKite(err)
	}
	out := make([]core.Instrument, 0, len(raw))
	for _, r := range raw {
		if r.InstrumentType != "OPTIDX" && r.InstrumentType != "OPTSTK" {
			continue
		}
		var otype core.OptionType
		switch {
		case strings.HasSuffix(r.Tradingsymbol, "CE"):
			otype = core.Call
		case strings.HasSuffix(r.Tradingsymbol, "PE"):
			otype = core.Put
		default:
			continue
		}
		var kind core.InstrumentKind
		if r.InstrumentType == "OPTIDX" {
			kind = core.KindIndexOption
		} else {
			kind = core.KindStockOption
		}
		expiry := r.Expiry.Time
		out = append(out, core.Instrument{
			Underlying:    strings.ToUpper(strings.TrimSpace(r.Name)),
			Kind:          kind,
			Expiry:        expiry,
			Strike:        r.StrikePrice,
			OptionType:    otype,
			LotSize:       int(r.LotSize),
			TickSize:      r.TickSize,
			BrokerSymbols: map[core.Broker]string{core.BrokerKite: r.Tradingsymbol},
		})
	}
	out = NightlyFreezeFill(out)
	return out, nil
}

// NightlyFreezeFill: Kite dump lacks freeze qty; default by
// underlying from the config fallback table. Overwritten by the NSE
// refresh job. (NIFTY 3510 since 2026-10-05; check NSE circulars.)
var freezeFallback = map[string]int{
	"NIFTY":      3510,
	"BANKNIFTY":  1440,
	"FINNIFTY":   3240,
	"MIDCPNIFTY": 5760,
	"NIFTYNXT50": 1125,
}

func NightlyFreezeFill(insts []core.Instrument) []core.Instrument {
	for i := range insts {
		if insts[i].FreezeQty == 0 {
			insts[i].FreezeQty = freezeFallback[strings.ToUpper(insts[i].Underlying)]
		}
	}
	return insts
}

// exchangeOf picks the exchange segment for placement.
func exchangeOf(i core.Instrument) string {
	// All v1 tradables are NSE F&O.
	return "NFO"
}

// classifyKite separates broker rejections from transport errors.
func classifyKite(err error) *brokers.AdapterError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if brokers.IsMarginRejection(msg) {
		return &brokers.AdapterError{Kind: brokers.ErrMarginRejection, Message: msg, Err: err}
	}
	if isKiteAPIError(err) {
		return &brokers.AdapterError{Kind: brokers.ErrRejection, Message: msg, Err: err}
	}
	return &brokers.AdapterError{Kind: brokers.ErrTransport, Message: msg, Err: err}
}

// kiteconnect.Error is the JSON API error type.
func isKiteAPIError(err error) bool {
	_, ok := err.(kiteconnect.Error)
	return ok
}

// fmt import guard
var _ = fmt.Sprintf
