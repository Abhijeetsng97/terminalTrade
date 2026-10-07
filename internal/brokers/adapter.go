// Package brokers defines the BrokerAdapter seam: the single
// broker-agnostic contract every broker SDK is wrapped behind. All
// broker-agnostic logic (order engine, netting, chain, slicer) is
// tested against the simulator and never imports an SDK.
package brokers

import (
	"context"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// OrderRequest is a canonical order placement request.
type OrderRequest struct {
	Instrument core.Instrument
	Side       core.Side
	// Qty in units (a slice of the parent).
	Qty        int
	OrderType  core.OrderType
	// LimitPrice for MKT-PROT and LIMIT (both are limit orders at
	// the broker; market orders are never sent).
	LimitPrice  float64
	// IdempotencyTag: client-generated unique tag per placement
	// attempt (fresh tag on every retry).
	IdempotencyTag string
}

// ModifyRequest changes a working order's price and/or quantity.
type ModifyRequest struct {
	BrokerOrderID string
	// NewQty in units; 0 = unchanged.
	NewQty int
	// NewPrice; 0 = unchanged.
	NewPrice float64
}

// SessionStatus describes a broker session.
type SessionStatus struct {
	Broker core.Broker
	// Valid right now.
	Valid bool
	// Reason when invalid (e.g. "token expired").
	Reason string
	// ExpiresAt when known (Kite: today ~09:00 IST next expiry).
	ExpiresAt time.Time
	// ManualFlow describes what the user must paste when invalid.
	ManualFlow string
}

// AdapterError distinguishes transport failures from broker
// rejections — the retry policy depends on it (transport retry <=2
// with fresh tags; rejections never retried; margin rejection halts
// the parent).
type AdapterError struct {
	// Kind: transport | rejection | margin-rejection
	Kind    ErrKind
	Message string
	Err     error
}

type ErrKind string

const (
	ErrTransport        ErrKind = "transport"
	ErrRejection        ErrKind = "rejection"
	ErrMarginRejection  ErrKind = "margin-rejection"
)

func (e *AdapterError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *AdapterError) Unwrap() error { return e.Err }

// ClassifyError inspects a raw adapter error into an AdapterError.
// Margin rejections are detected by message content (broker messages
// vary; RMS/margin are the words both brokers use).
func ClassifyError(err error) *AdapterError {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*AdapterError); ok {
		return ae
	}
	return &AdapterError{Kind: ErrTransport, Message: err.Error(), Err: err}
}

// IsMarginRejection detects margin/RMS rejections from broker text.
func IsMarginRejection(msg string) bool {
	m := lower(msg)
	return contains(m, "margin") || contains(m, "rms") ||
		contains(m, "insufficient funds") || contains(m, "insufficient balance")
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// BrokerAdapter is the complete broker-facing contract.
type BrokerAdapter interface {
	// Broker identifies the implementation.
	Broker() core.Broker

	// ---- sessions ----

	// SessionStatus reports current validity and manual flow text.
	SessionStatus(ctx context.Context) SessionStatus
	// ExchangeManualToken completes a manual login: request_token
	// (Kite) or auth code (Fyers fallback). Returns a display message.
	ExchangeManualToken(ctx context.Context, token string) (string, error)
	// LoginURL for the manual flow (browser step).
	LoginURL() string

	// ---- trading ----

	// PlaceOrder submits one slice. Never sends market orders.
	PlaceOrder(ctx context.Context, req OrderRequest) (brokerOrderID string, err error)
	// ModifyOrder changes price/qty of a working order.
	ModifyOrder(ctx context.Context, req ModifyRequest) error
	// CancelOrder cancels a working order.
	CancelOrder(ctx context.Context, brokerOrderID string) error

	// ---- snapshots (broker is truth) ----

	// ListOrders returns today's orders from the broker's book.
	ListOrders(ctx context.Context) ([]BrokerOrder, error)
	// ListTrades returns today's trades.
	ListTrades(ctx context.Context) ([]BrokerTrade, error)
	// ListPositions returns live per-broker positions (canonical).
	ListPositions(ctx context.Context) ([]core.Position, error)
	// GetFunds returns the funds/margin snapshot.
	GetFunds(ctx context.Context) (core.Funds, error)

	// ---- instruments ----

	// LoadInstruments fetches the broker's instrument dump and
	// returns canonical instruments with BrokerSymbols filled in.
	LoadInstruments(ctx context.Context) ([]core.Instrument, error)
}

// BrokerOrder is one row of the broker's order book, canonical.
type BrokerOrder struct {
	BrokerOrderID string
	Symbol        string // broker-native, for correlation only
	InstrumentKey string // canonical
	Side          core.Side
	Qty           int
	FilledQty     int
	OrderType     core.OrderType
	LimitPrice    float64
	Status        core.OrderState
	// Tag echo lets reconciliation match broker rows to our slices.
	Tag          string
	ExchangeTime time.Time
}

// BrokerTrade is one fill.
type BrokerTrade struct {
	TradeID       string
	BrokerOrderID  string
	InstrumentKey  string
	Side           core.Side
	Qty            int
	Price          float64
	ExchangeTime   time.Time
}