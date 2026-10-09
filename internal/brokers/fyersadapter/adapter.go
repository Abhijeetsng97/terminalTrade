// Package fyersadapter wraps FyersDev/fyers-go-sdk behind the
// BrokerAdapter seam. The SDK returns raw JSON strings; this
// adapter unmarshals into canonical types. Fyers specifics never
// leak past this package.
package fyersadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	fyersgosdk "github.com/FyersDev/fyers-go-sdk"

	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// Fyers v3 order type/side ints.
const (
	typeLimit = 1
	typeSL    = 4 // stop-loss limit (trigger + limit)
	sideBuy   = 1
	sideSell  = -1
)

// Adapter talks to Fyers API v3.
type Adapter struct {
	mu          sync.Mutex
	appID       string
	appSecret   string
	redirectURI string
	pin         string
	client      *fyersgosdk.Client
	model       *fyersgosdk.FyersModel
	accessToken string
	refreshToken string
	resolve     SymbolResolver
}

// SymbolResolver resolves a Fyers symbol to a canonical instrument.
type SymbolResolver func(symbol string) (core.Instrument, bool)

func New(appID, appSecret, redirectURI, pin string) *Adapter {
	return &Adapter{appID: appID, appSecret: appSecret, redirectURI: redirectURI, pin: pin}
}

func (a *Adapter) Broker() core.Broker { return core.BrokerFyers }

// SetSymbolResolver wires the instruments-table lookup.
func (a *Adapter) SetSymbolResolver(r SymbolResolver) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resolve = r
}

func (a *Adapter) resolveSymbol(s string) (core.Instrument, bool) {
	if a.resolve == nil {
		return core.Instrument{Underlying: s}, false
	}
	return a.resolve(s)
}

// buildClient constructs the SDK model with the current access token.
func (a *Adapter) buildClient() {
	a.client = fyersgosdk.SetClientData(a.appID, a.appSecret, a.redirectURI)
	if a.accessToken != "" {
		a.client.SetAccessToken(a.accessToken)
	}
	a.model = fyersgosdk.NewFyersModel(a.appID, a.accessToken)
}

// CurrentTokens exposes access + refresh tokens (for persistence).
func (a *Adapter) CurrentTokens() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken, a.refreshToken
}

// SetAccessToken loads a stored (decrypted) token and rebuilds the model.
func (a *Adapter) SetAccessToken(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessToken = tok
	a.buildClient()
}

// SetRefreshToken stores the refresh token for the daily auto-refresh.
func (a *Adapter) SetRefreshToken(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshToken = tok
}

// LoginURL returns the browser login step URL.
func (a *Adapter) LoginURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		a.buildClient()
	}
	return a.client.GetLoginURL()
}

// ExchangeManualToken: auth-code paste flow (manual fallback).
func (a *Adapter) ExchangeManualToken(ctx context.Context, authCode string) (string, error) {
	a.mu.Lock()
	client := fyersgosdk.SetClientData(a.appID, a.appSecret, a.redirectURI)
	a.mu.Unlock()
	resp, err := client.GenerateAccessToken(authCode, client)
	if err != nil {
		return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: err.Error(), Err: err}
	}
	var tok AccessTokenResponse
	if err := json.Unmarshal([]byte(resp), &tok); err != nil {
		return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "unmarshal token response: " + err.Error(), Err: err}
	}
	if tok.Code != 200 {
		return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers auth failed: " + tok.Message}
	}
	a.mu.Lock()
	a.accessToken = tok.AccessToken
	a.refreshToken = tok.RefreshToken
	a.buildClient()
	a.mu.Unlock()
	return "fyers session established", nil
}

// AccessTokenResponse is the SDK's token payload (re-declared to read raw JSON).
type AccessTokenResponse struct {
	Code         int    `json:"code"`
	Message      string `json:"message"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// DailyRefresh refreshes the access token from the stored refresh
// token (spec: Fyers auto-refresh cron ~08:30 IST). Returns the new
// tokens to persist.
func (a *Adapter) DailyRefresh(ctx context.Context) (accessToken, refreshToken string, err error) {
	a.mu.Lock()
	rt := a.refreshToken
	a.mu.Unlock()
	if rt == "" {
		return "", "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "no refresh token stored — manual login (L) required"}
	}
	client := fyersgosdk.SetClientData(a.appID, a.appSecret, a.redirectURI)
	resp, err := client.GenerateAccessTokenFromRefreshToken(rt, a.pin, client)
	if err != nil {
		return "", "", &brokers.AdapterError{Kind: brokers.ErrTransport, Message: err.Error(), Err: err}
	}
	var tok AccessTokenResponse
	if err := json.Unmarshal([]byte(resp), &tok); err != nil {
		return "", "", &brokers.AdapterError{Kind: brokers.ErrTransport, Message: "unmarshal refresh response: " + err.Error(), Err: err}
	}
	if tok.Code != 200 {
		return "", "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers refresh failed: " + tok.Message}
	}
	a.mu.Lock()
	a.accessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.refreshToken = tok.RefreshToken
	}
	a.buildClient()
	a.mu.Unlock()
	return tok.AccessToken, tok.RefreshToken, nil
}

// SessionStatus validates with a cheap profile call.
func (a *Adapter) SessionStatus(ctx context.Context) brokers.SessionStatus {
	a.mu.Lock()
	model := a.model
	na := a.accessToken == ""
	a.mu.Unlock()
	if model == nil || na {
		return brokers.SessionStatus{Broker: core.BrokerFyers, Valid: false,
			Reason: "no access token — press L to login",
			ManualFlow: "open " + a.LoginURL() + " | authorize, copy auth code, paste here"}
	}
	resp, err := model.GetProfile()
	if err != nil {
		return brokers.SessionStatus{Broker: core.BrokerFyers, Valid: false, Reason: "token rejected: " + err.Error()}
	}
	var pr fyersgosdk.Profile
	if err := json.Unmarshal([]byte(resp), &pr); err != nil || pr.Code != 200 {
		return brokers.SessionStatus{Broker: core.BrokerFyers, Valid: false, Reason: "token rejected"}
	}
	return brokers.SessionStatus{Broker: core.BrokerFyers, Valid: true}
}

// PlaceOrder: always LIMIT (type 1); MKT-PROT is a limit at the band.
func (a *Adapter) PlaceOrder(ctx context.Context, req brokers.OrderRequest) (string, error) {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return "", &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	side := sideBuy
	if req.Side == core.SideSell {
		side = sideSell
	}
	orderType := typeLimit
	if req.OrderType == core.TypeStopLoss {
		orderType = typeSL
	}
	resp, err := model.SingleOrderAction(fyersgosdk.OrderRequest{
		Symbol:      req.Instrument.BrokerSymbols[core.BrokerFyers],
		Qty:         req.Qty,
		Type:        orderType,
		Side:        side,
		ProductType: fyersgosdk.ProductMargin,
		LimitPrice:  req.LimitPrice,
		StopPrice:   req.TriggerPrice,
		Validity:    "DAY",
		OrderTag:    req.IdempotencyTag,
	})
	if err != nil {
		return "", classifyFyers(err, "")
	}
	var out fyersgosdk.OrderResponse
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return "", classifyFyers(err, resp)
	}
	if out.Code != 200 {
		return "", classifyFyers(fmt.Errorf("fyers: %s", out.Message), resp)
	}
	return out.Id, nil
}

// ModifyOrder changes price/qty of a working order.
func (a *Adapter) ModifyOrder(ctx context.Context, m brokers.ModifyRequest) error {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	req := fyersgosdk.ModifyOrderRequest{Id: m.BrokerOrderID}
	if m.NewQty > 0 {
		req.Qty = m.NewQty
	}
	if m.NewPrice > 0 {
		req.LimitPrice = m.NewPrice
		req.Type = typeLimit
	}
	resp, err := model.ModifyOrder(req)
	if err != nil {
		return classifyFyers(err, "")
	}
	var out fyersgosdk.APIResponse
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return classifyFyers(err, resp)
	}
	if out.Code != 200 {
		return classifyFyers(fmt.Errorf("fyers: %s", out.Message), resp)
	}
	return nil
}

// CancelOrder cancels a working order.
func (a *Adapter) CancelOrder(ctx context.Context, id string) error {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	resp, err := model.CancelOrder(id)
	if err != nil {
		return classifyFyers(err, "")
	}
	var out fyersgosdk.APIResponse
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return classifyFyers(err, resp)
	}
	if out.Code != 200 {
		return classifyFyers(fmt.Errorf("fyers: %s", out.Message), resp)
	}
	return nil
}

// orderBookRow is one Fyers order-book entry (subset we need).
type orderBookRow struct {
	ID     string  `json:"id"`
	Symbol string  `json:"symbol"`
	Side   int     `json:"side"`
	Qty    int     `json:"qty"`
	Filled int     `json:"filledQty"`
	Status int     `json:"status"`
	Price  float64 `json:"limitPrice"`
	Tag    string  `json:"orderTag"`
}

// status codes: 1=Canceled 2=Traded/Filled 4=Transient 5=Rejected 6=Pending
func mapFyersStatus(s int) core.OrderState {
	switch s {
	case 2:
		return core.StateFilled
	case 1:
		return core.StateCancelled
	case 5:
		return core.StateRejected
	case 4, 6:
		return core.StateOpen
	default:
		return core.StateOpen
	}
}

// ListOrders returns today's order book (canonical rows).
func (a *Adapter) ListOrders(ctx context.Context) ([]brokers.BrokerOrder, error) {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return nil, &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	resp, err := model.OrderHistory(&fyersgosdk.OrderBookHistoryFilter{})
	if err != nil {
		return nil, classifyFyers(err, "")
	}
	var out struct {
		OrderBook []orderBookRow `json:"orderBook"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return nil, classifyFyers(err, resp)
	}
	rows := make([]brokers.BrokerOrder, 0, len(out.OrderBook))
	for _, o := range out.OrderBook {
		side := core.SideBuy
		if o.Side < 0 {
			side = core.SideSell
		}
		rows = append(rows, brokers.BrokerOrder{
			BrokerOrderID: o.ID,
			Symbol:        o.Symbol,
			InstrumentKey: "FYERS/" + o.Symbol,
			Side:          side,
			Qty:           o.Qty,
			FilledQty:     o.Filled,
			OrderType:     core.TypeLimit,
			LimitPrice:    o.Price,
			Status:        mapFyersStatus(o.Status),
			Tag:           o.Tag,
		})
	}
	return rows, nil
}

// ListTrades returns today's fills.
func (a *Adapter) ListTrades(ctx context.Context) ([]brokers.BrokerTrade, error) {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return nil, &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	resp, err := model.TradeHistory(&fyersgosdk.TradeBookHistoryFilter{})
	if err != nil {
		return nil, classifyFyers(err, "")
	}
	var out struct {
		TradeBook []struct {
			TradeID string  `json:"tradeTicketNo"`
			OrderID string  `json:"orderNum"`
			Symbol  string  `json:"symbol"`
			Side    int     `json:"side"`
			Qty     int     `json:"tradedQty"`
			Price   float64 `json:"tradePrice"`
		} `json:"tradeBook"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return nil, classifyFyers(err, resp)
	}
	trades := make([]brokers.BrokerTrade, 0, len(out.TradeBook))
	for _, t := range out.TradeBook {
		side := core.SideBuy
		if t.Side < 0 {
			side = core.SideSell
		}
		trades = append(trades, brokers.BrokerTrade{
			TradeID:       t.TradeID,
			BrokerOrderID: t.OrderID,
			InstrumentKey: "FYERS/" + t.Symbol,
			Side:          side,
			Qty:           t.Qty,
			Price:         t.Price,
		})
	}
	return trades, nil
}

// ListPositions returns live positions (canonical).
func (a *Adapter) ListPositions(ctx context.Context) ([]core.Position, error) {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return nil, &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	resp, err := model.GetPositions()
	if err != nil {
		return nil, classifyFyers(err, "")
	}
	var out fyersgosdk.Position
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return nil, classifyFyers(err, resp)
	}
	pos := make([]core.Position, 0, len(out.NetPositions))
	for _, p := range out.NetPositions {
		if p.NetQty == 0 {
			continue
		}
		inst, _ := a.resolveSymbol(p.Symbol)
		var lots float64
		if inst.LotSize > 0 {
			lots = float64(p.NetQty) / float64(inst.LotSize)
		}
		side := core.SideBuy
		if p.NetQty < 0 {
			side = core.SideSell
		}
		_ = side
		pos = append(pos, core.Position{
			Instrument: inst,
			Broker:     core.BrokerFyers,
			Lots:       lots,
			Qty:        p.NetQty,
			AvgPrice:   p.NetAvg,
			LastPrice:  p.Ltp,
			PnL:        p.Pl,
			UpdatedAt:  time.Now(),
		})
	}
	return pos, nil
}

// GetFunds parses the fund-limit rows.
func (a *Adapter) GetFunds(ctx context.Context) (core.Funds, error) {
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	if model == nil {
		return core.Funds{}, &brokers.AdapterError{Kind: brokers.ErrRejection, Message: "fyers session not established"}
	}
	resp, err := model.GetFunds()
	if err != nil {
		return core.Funds{}, classifyFyers(err, "")
	}
	var out fyersgosdk.Funds
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return core.Funds{}, classifyFyers(err, resp)
	}
	var available, used, span, exposure, premium, collateral float64
	for _, f := range out.FundLimit {
		switch f.Title {
		case "Available Balance", "Clear Balance", "Available Margin":
			available = f.EquityAmount
		case "Utilized Amount", "Utilized Debits", "Total Utilized":
			used = f.EquityAmount
		case "SPAN Amount", "SPAN":
			span = f.EquityAmount
		case "Exposure Amount", "Exposure":
			exposure = f.EquityAmount
		case "Option Premium":
			premium = f.EquityAmount
		case "Collateral", "Adhoc Margin", "Pledged Collateral Value":
			collateral += f.EquityAmount
		}
	}
	return core.Funds{
		Broker: core.BrokerFyers, Available: available, Used: used,
		// user's model: TOTAL = deployable = available + collateral
		Total:         available + collateral,
		Span:          span, Exposure: exposure, OptionPremium: premium,
		Collateral:    collateral,
		UpdatedAt:     time.Now(),
	}, nil
}

// LoadInstruments: v1 defers the Fyers symbol-master download (no
// stable public URL; CDN gated). The app layer derives Fyers index-
// option symbols from the canonical model instead (see app.deriveSymbolsFor
// + fyers.DeriveSymbol). Returning empty with nil error signals
// "nothing new" rather than failure, so Kite's dump still loads.
func (a *Adapter) LoadInstruments(ctx context.Context) ([]core.Instrument, error) {
	return nil, nil // symbols derived from canonical model in app layer
}

// classifyFyers separates rejections from transport errors.
func classifyFyers(err error, raw string) *brokers.AdapterError {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if brokers.IsMarginRejection(msg) {
		return &brokers.AdapterError{Kind: brokers.ErrMarginRejection, Message: msg, Err: err}
	}
	// SDK errors are transport; API-level failures return Code != 200
	// in JSON which callers turn into errors with the message — those
	// are rejections.
	if raw != "" && !strings.Contains(msg, "fyers:") {
		return &brokers.AdapterError{Kind: brokers.ErrTransport, Message: msg, Err: err}
	}
	return &brokers.AdapterError{Kind: brokers.ErrRejection, Message: msg, Err: err}
}