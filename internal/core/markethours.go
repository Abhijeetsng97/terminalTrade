package core

import (
	"sync"
	"time"
)

// MarketHours enforces the 09:15–15:30 IST trading window and the
// holiday calendar. Client-side guard: orders outside the window are
// refused before any broker call (spec decision #12).
type MarketHours struct {
	// Holidays are closed dates (IST, midnight-aligned), loaded by
	// the instruments/ops refresh pipeline.
	Holidays map[string]bool

	// forceOpen ignores hours (sim/dev only).
	forceOpen bool
	mu        sync.Mutex
}

func NewMarketHours() *MarketHours {
	return &MarketHours{Holidays: make(map[string]bool)}
}

// istLoc is Asia/Kolkata. Fixed offset +05:30, no DST.
var istLoc = time.FixedZone("IST", 5*60*60+30*60)

// SetOpenOverride forces the open state (sim/dev only — the guard
// logs that it is active).
func (m *MarketHours) SetOpenOverride(force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceOpen = force
}

// StatusAt classifies the given UTC time into the IST trading day.
func (m *MarketHours) StatusAt(t time.Time) MarketStatus {
	m.mu.Lock()
	forced := m.forceOpen
	holidays := m.Holidays
	m.mu.Unlock()
	if forced {
		return MarketOpen
	}
	ist := t.In(istLoc)
	if holidays[ist.Format("2006-01-02")] {
		return MarketClosed
	}
	mins := ist.Hour()*60 + ist.Minute()
	switch {
	case mins < 9*60: // before 09:00
		return MarketClosed
	case mins < 9*60+15: // 09:00–09:15 pre-open
		return MarketPreOpen
	case mins < 15*60+30: // 09:15–15:30
		return MarketOpen
	default:
		return MarketClosed
	}
}

// CanPlace reports whether new orders may be placed.
func (m *MarketHours) CanPlace(t time.Time) bool {
	return m.StatusAt(t) == MarketOpen
}

type MarketStatus string

const (
	MarketOpen    MarketStatus = "OPEN"
	MarketPreOpen MarketStatus = "PRE-OPEN"
	MarketClosed  MarketStatus = "CLOSED"
)

// MarketClock allows tests to fix time.
type MarketClock func() time.Time

func RealClock() time.Time { return time.Now() }

// LocalIST exposes the IST location for formatting.
func LocalIST() *time.Location { return istLoc }