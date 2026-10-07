// Ops: background jobs — instruments refresh (nightly + login check
// + hourly cron) and the Fyers daily token refresh (~08:30 IST).
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

func fmtSprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// StartOps launches the background jobs and returns a stop func.
func (a *App) StartOps(ctx context.Context) (stop func(), err error) {
	// login check: refresh instruments once at startup
	go func() {
		if err := a.RefreshInstruments(ctx); err != nil {
			a.alertf("instrument refresh failed at startup: %v", err)
		}
	}()

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

// alertf records an alert (surfaced via API + TUI banner).
func (a *App) alertf(format string, args ...any) {
	a.alertsMu.Lock()
	a.alerts = append(a.alerts, time.Now().Format("15:04:05 ")+fmtSprintf(format, args...))
	if len(a.alerts) > 50 {
		a.alerts = a.alerts[len(a.alerts)-50:]
	}
	a.alertsMu.Unlock()
}

// Alerts returns the recent alert log.
func (a *App) Alerts() []string {
	a.alertsMu.Lock()
	defer a.alertsMu.Unlock()
	out := make([]string, len(a.alerts))
	copy(out, a.alerts)
	return out
}