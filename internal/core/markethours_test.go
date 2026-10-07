package core

import (
	"testing"
	"time"
)

// Market-hours guard: orders only 09:15-15:30 IST; pre-open
// readable but blocked; holidays closed.
func TestMarketHours(t *testing.T) {
	mh := NewMarketHours()
	mh.Holidays["2026-10-20"] = true // Diwali-ish holiday

	cases := []struct {
		name string
		ist  string
		want MarketStatus
	}{
		{"before open", "2026-10-07 08:59", MarketClosed},
		{"pre-open start", "2026-10-07 09:00", MarketPreOpen},
		{"pre-open end", "2026-10-07 09:14", MarketPreOpen},
		{"open", "2026-10-07 09:15", MarketOpen},
		{"midday", "2026-10-07 12:30", MarketOpen},
		{"close boundary", "2026-10-07 15:29", MarketOpen},
		{"after close", "2026-10-07 15:30", MarketClosed},
		{"evening", "2026-10-07 18:00", MarketClosed},
		{"holiday", "2026-10-20 11:00", MarketClosed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, err := time.ParseInLocation("2006-01-02 15:04", c.ist, LocalIST())
			if err != nil {
				t.Fatal(err)
			}
			if got := mh.StatusAt(ts); got != c.want {
				t.Errorf("StatusAt(%s): got %s want %s", c.ist, got, c.want)
			}
			if c.want == MarketOpen && !mh.CanPlace(ts) {
				t.Errorf("CanPlace must be true when open (%s)", c.ist)
			}
			if c.want != MarketOpen && mh.CanPlace(ts) {
				t.Errorf("CanPlace must be false when %s (%s)", c.want, c.ist)
			}
		})
	}
}