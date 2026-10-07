// Package api serves the REST API: a parallel consumer of the same
// trading core the TUI drives. Bearer-token authenticated
// (TT_API_KEY); every mutating endpoint routes through the engine,
// so the API inherits idempotency, slicing, margin-halt, and the
// market-hours guard for free.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/app"
	"github.com/Abhijeetsng97/terminalTrade/internal/brokers"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// Server is the REST API.
type Server struct {
	App     *app.App
apiKey string
}

func New(a *app.App, apiKey string) *Server {
	return &Server{App: a, apiKey: apiKey}
}

// Handler builds the mux with auth middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// health is unauthenticated
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now()})
	})

	mux.HandleFunc("GET /api/snapshot", s.auth(s.handleSnapshot))
	mux.HandleFunc("GET /api/positions", s.auth(s.handlePositions))
	mux.HandleFunc("GET /api/funds", s.auth(s.handleFunds))
	mux.HandleFunc("GET /api/instruments/{key}", s.auth(s.handleInstrument))
	mux.HandleFunc("GET /api/instruments", s.auth(s.handleInstruments))
	mux.HandleFunc("GET /api/chain/{underlying}", s.auth(s.handleChain))

	mux.HandleFunc("POST /api/orders", s.auth(s.handlePlaceOrder))
	mux.HandleFunc("GET /api/orders", s.auth(s.handleListOrders))
	mux.HandleFunc("POST /api/orders/{id}/cancel", s.auth(s.handleCancelOrder))
	mux.HandleFunc("POST /api/orders/{id}/modify", s.auth(s.handleModifyOrder))
	mux.HandleFunc("POST /api/positions/close-all", s.auth(s.handleSquareOffAll))

	mux.HandleFunc("GET /api/sessions", s.auth(s.handleSessions))
	mux.HandleFunc("POST /api/sessions/{broker}/token", s.auth(s.handleExchangeToken))

	mux.HandleFunc("POST /api/instruments/refresh", s.auth(s.handleRefreshInstruments))

	return mux
}

// auth wraps a handler with bearer-token verification.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			httpError(w, http.StatusUnauthorized, "TT_API_KEY not configured — API disabled")
			return
		}
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || strings.TrimPrefix(h, "Bearer ") != s.apiKey {
			httpError(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// ---- GET endpoints ----

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.GetSnapshot(r.Context()))
}

func (s *Server) handlePositions(w http.ResponseWriter, r *http.Request) {
	snap := s.App.GetSnapshot(r.Context())
	writeJSON(w, http.StatusOK, snap.Positions)
}

func (s *Server) handleFunds(w http.ResponseWriter, r *http.Request) {
	snap := s.App.GetSnapshot(r.Context())
	writeJSON(w, http.StatusOK, snap.Funds)
}

func (s *Server) handleInstrument(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	inst, ok := s.App.InstrumentByKey(key)
	if !ok {
		httpError(w, http.StatusNotFound, "instrument not found: "+key)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleInstruments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.Instruments())
}

// chainRow is one strike of the chain response.
type chainRow struct {
	Strike float64    `json:"strike"`
	CE     *core.Quote `json:"ce,omitempty"`
	PE     *core.Quote `json:"pe,omitempty"`
}

func (s *Server) handleChain(w http.ResponseWriter, r *http.Request) {
	underlying := strings.ToUpper(r.PathValue("underlying"))
	expiryStr := r.URL.Query().Get("expiry") // 2006-01-02; default nearest
	rows := s.buildChain(underlying, expiryStr)
	writeJSON(w, http.StatusOK, rows)
}

// buildChain assembles CE/PE rows from the quote cache.
func (s *Server) buildChain(underlying, expiryStr string) []chainRow {
	var expiry time.Time
	if expiryStr != "" {
		expiry, _ = time.ParseInLocation("2006-01-02", expiryStr, core.LocalIST())
	}
	insts := s.App.Instruments()
	// group by strike for the nearest (or requested) expiry
	byStrike := map[float64]*chainRow{}
	strikes := []float64{}
	for _, i := range insts {
		if i.Underlying != underlying || !i.IsOption() {
			continue
		}
		if !expiry.IsZero() && !sameDay(i.Expiry, expiry) {
			continue
		}
		row, ok := byStrike[i.Strike]
		if !ok {
			row = &chainRow{Strike: i.Strike}
			byStrike[i.Strike] = row
			strikes = append(strikes, i.Strike)
		}
		q, ok := s.App.Quotes.Get(i.Key())
		if !ok {
			continue
		}
		if i.OptionType == core.Call {
			row.CE = &q
		} else {
			row.PE = &q
		}
	}
	rows := make([]chainRow, 0, len(strikes))
	for _, st := range strikes {
		rows = append(rows, *byStrike[st])
	}
	return rows
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// ---- order endpoints ----

// PlaceOrderRequest is the order intent payload.
type PlaceOrderRequest struct {
	// Instrument canonical key, e.g.
	// NIFTY:OPTIDX:20261029:26000:CE
	InstrumentKey string `json:"instrument_key"`
	Side          string `json:"side"` // BUY | SELL
	Lots          int    `json:"lots"`
	// order_type: MKT-PROT (default) or LIMIT
	OrderType  string  `json:"order_type"`
	LimitPrice float64 `json:"limit_price"`
	Broker     string  `json:"broker"` // KITE | FYERS (optional: last-used)
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req PlaceOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	inst, ok := s.App.InstrumentByKey(req.InstrumentKey)
	if !ok {
		httpError(w, http.StatusNotFound, "unknown instrument: "+req.InstrumentKey)
		return
	}
	ot := core.TypeMarketProtected
	if strings.EqualFold(req.OrderType, "LIMIT") {
		ot = core.TypeLimit
	}
	broker := core.Broker(req.Broker)
	if broker == "" {
		broker = core.BrokerKite // default; TUI passes last-used
	}
	s.App.RiskMu.Lock()
	risk := s.App.Risk
	s.App.RiskMu.Unlock()

	parent, err := s.App.Engine.PlaceIntent(r.Context(), s.App.UserID, inst,
		core.Side(strings.ToUpper(req.Side)), req.Lots, ot, req.LimitPrice, broker, risk)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, parent)
}

// orderWithChildren is the order-book response row.
type orderWithChildren struct {
	core.Order
	Children []core.ChildOrder `json:"children"`
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	parents, err := s.App.Engine.ListParentsSince(r.Context(), s.App.UserID, midnightIST(time.Now()))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]orderWithChildren, 0, len(parents))
	for _, p := range parents {
		kids, _ := s.App.Engine.Children(r.Context(), p.ID)
		out = append(out, orderWithChildren{Order: p, Children: kids})
	}
	writeJSON(w, http.StatusOK, out)
}

// midnightIST returns 00:00 IST of the given time's day.
func midnightIST(t time.Time) time.Time {
	ist := t.In(core.LocalIST())
	return time.Date(ist.Year(), ist.Month(), ist.Day(), 0, 0, 0, 0, core.LocalIST())
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.App.Engine.CancelChild(r.Context(), s.App.UserID, id); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled", "id": id})
}

// ModifyOrderRequest changes a pending order.
type ModifyOrderRequest struct {
	Price float64 `json:"price"`
	Qty   int     `json:"qty"`
}

func (s *Server) handleModifyOrder(w http.ResponseWriter, r *http.Request) {
	var req ModifyOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if err := s.App.Engine.ModifyChild(r.Context(), s.App.UserID, r.PathValue("id"), req.Price, req.Qty); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "modified", "id": r.PathValue("id")})
}

func (s *Server) handleSquareOffAll(w http.ResponseWriter, r *http.Request) {
	s.App.RiskMu.Lock()
	risk := s.App.Risk
	s.App.RiskMu.Unlock()
	placed, err := s.App.Engine.SquareOffAll(r.Context(), s.App.UserID, risk)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, placed)
}

// ---- session endpoints ----

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	out := map[string]brokers.SessionStatus{}
	for _, ad := range s.App.Adapters {
		out[string(ad.Broker())] = ad.SessionStatus(r.Context())
	}
	writeJSON(w, http.StatusOK, out)
}

// TokenExchangeRequest carries the manual paste.
type TokenExchangeRequest struct {
	Token string `json:"token"`
}

func (s *Server) handleExchangeToken(w http.ResponseWriter, r *http.Request) {
	var req TokenExchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		httpError(w, http.StatusBadRequest, "token required")
		return
	}
	broker := r.PathValue("broker")
	var msg string
	var err error
	switch core.Broker(broker) {
	case core.BrokerKite:
		msg, err = s.App.Kite.ExchangeManualToken(r.Context(), req.Token)
	case core.BrokerFyers:
		msg, err = s.App.Fyers.ExchangeManualToken(r.Context(), req.Token)
	default:
		httpError(w, http.StatusNotFound, "unknown broker: "+broker)
		return
	}
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// persist the session so restarts keep it
	kt, _ := s.App.Kite.CurrentTokens()
	ft, frt := s.App.Fyers.CurrentTokens()
	switch core.Broker(broker) {
	case core.BrokerKite:
		_ = s.App.SaveBrokerTokens(r.Context(), core.BrokerKite, kt, "")
	case core.BrokerFyers:
		_ = s.App.SaveBrokerTokens(r.Context(), core.BrokerFyers, ft, frt)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "message": msg})
}

func (s *Server) handleRefreshInstruments(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RefreshInstruments(r.Context()); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "refreshed", "count": len(s.App.Instruments())})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}