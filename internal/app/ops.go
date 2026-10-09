// Ops: background jobs — instruments refresh (nightly + login check
// + hourly cron) and the Fyers daily token refresh (~08:30 IST).
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/log"
	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

func fmtSprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// StartOps launches the background jobs and returns a stop func.
func (a *App) StartOps(ctx context.Context) (stop func(), err error) {
	// wipe the instrument store at every startup: sim leftovers (or
	// yesterday's expired contracts) must never seed today's chain.
	// Real mode refetches from broker dumps below; sim reseeds.
	if err := a.Store.ClearInstruments(ctx); err != nil {
		a.alertf("instrument wipe failed: %v", err)
	}

	// login check: refresh instruments once at startup
	go func() {
		if err := a.RefreshInstruments(ctx); err != nil {
			a.alertf("instrument refresh failed at startup: %v", err)
		}
	}()

	// session watcher: the moment a broker gains a valid session
	// (manual L login), refresh instruments so the option chain
	// populates immediately — not at the next hourly tick.
	go a.watchSessions(ctx)

	// reconcile loop: converge orders to broker truth every 15s.
	// Without this, fills never roll up and the book shows OPEN
	// forever on completed orders.
	go a.reconcileLoop(ctx)

	// hourly instruments cron (spec: nightly + login + background cron)
	instTicker := time.NewTicker(time.Hour)
	// fyers daily refresh at ~08:30 IST
	fyersTimer := time.NewTicker(time.Hour)

	stopCh := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-instTicker.C:
				if err := a.RefreshInstruments(ctx); err != nil {
					a.alertf("instrument refresh failed: %v", err)
				}
			case <-fyersTimer.C:
				a.tryFyersRefresh(ctx)
			}
		}
	}()

	return func() {
		instTicker.Stop()
		fyersTimer.Stop()
		close(stopCh)
	}, nil
}

// watchSessions polls session validity every 5s; on a new login
// (invalid -> valid transition) it triggers an instrument refresh +
// a session-token re-save.
func (a *App) watchSessions(ctx context.Context) {
	wasValid := map[core.Broker]bool{}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, ad := range a.Adapters {
				b := ad.Broker()
				ss := ad.SessionStatus(ctx)
				if ss.Valid && !wasValid[b] {
					a.alertf("%s session active — refreshing instruments", b)
					if err := a.RefreshInstruments(ctx); err != nil {
						a.alertf("post-login instrument refresh failed: %v", err)
					}
					// restart the quote poller (session-bound)
					if a.Kite != nil && len(a.Cfg.Underlyings) > 0 {
						a.StartQuotePoller(ctx, a.Cfg.Underlyings[0], 0)
					}
				}
				wasValid[b] = ss.Valid
			}
		}
	}
}

// reconcileLoop converges local order state to broker truth. Runs
// every 5s regardless of market hours — the broker order book is
// queryable outside the session, and completed/rejected orders must
// converge even after the close (the book showed OPEN forever when
// this was gated on CanPlace).
func (a *App) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := a.Engine.Reconcile(ctx)
			if err != nil {
				a.alertf("reconcile failed: %v", err)
				continue
			}
			if n > 0 {
				a.alertf("reconcile: %d order corrections", n)
			}
		}
	}
}

// tryFyersRefresh runs the Fyers token refresh in the 08:00–09:00
// IST window; tokens are rotated daily, refresh tokens are
// long-lived.
func (a *App) tryFyersRefresh(ctx context.Context) {
	if a.Fyers == nil {
		return
	}
	now := time.Now().In(core.LocalIST())
	if now.Hour() != 8 {
		return
	}
	at, rt, err := a.Fyers.DailyRefresh(ctx)
	if err != nil {
		a.alertf("fyers daily refresh failed: %v (manual login L may be needed)", err)
		return
	}
	_ = a.SaveBrokerTokens(ctx, core.BrokerFyers, at, rt)
	a.alertf("fyers token refreshed")
}

// alertf records an alert (surfaced via API + TUI banner) and logs
// to stdout — operators watching the server window must see these.
func (a *App) alertf(format string, args ...any) {
	msg := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	a.alertsMu.Lock()
	a.alerts = append(a.alerts, msg)
	if len(a.alerts) > 50 {
		a.alerts = a.alerts[len(a.alerts)-50:]
	}
	a.alertsMu.Unlock()
	log.Warn("alert", "msg", msg)
}

// Alerts returns the recent alert log.
func (a *App) Alerts() []string {
	a.alertsMu.Lock()
	defer a.alertsMu.Unlock()
	out := make([]string, len(a.alerts))
	copy(out, a.alerts)
	return out
}