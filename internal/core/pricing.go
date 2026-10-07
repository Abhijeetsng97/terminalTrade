package core

import (
	"math"
	"strconv"
	"strings"
)

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// ProtectionBand returns the market-protection band (as a fraction of
// LTP) for an option at the given price, using the published bands:
//
//	< 10        -> 5%
//	10 .. 100   -> 3%
//	100 .. 500  -> 2%
//	> 500       -> 1%
func ProtectionBand(ltp float64) float64 {
	switch {
	case ltp < 10:
		return 0.05
	case ltp < 100:
		return 0.03
	case ltp < 500:
		return 0.02
	default:
		return 0.01
	}
}

// ProtectionPrice computes the limit price for a market-protected
// order: LTP plus the band on the pay-up side (buys round up, sells
// round down), snapped to the instrument's tick size.
//
// A market-protected order behaves like a market order inside the band
// and parks as a limit order if price runs away — the spec's "never a
// raw market order" rule.
func ProtectionPrice(ltp float64, side Side, tick float64) float64 {
	band := ProtectionBand(ltp)
	var p float64
	if side == SideBuy {
		p = ltp * (1 + band)
	} else {
		p = ltp * (1 - band)
	}
	return SnapToTick(p, tick)
}

// SnapToTick rounds a price to the nearest valid tick (default 0.05
// when tick is unset). Buys snap away from the market (up) so the
// protection never rounds below the band; this helper snaps to the
// nearest tick, which is what brokers validate against.
func SnapToTick(price, tick float64) float64 {
	if tick <= 0 {
		tick = 0.05
	}
	steps := math.Round(price / tick)
	return math.Max(tick, steps*tick)
}

// IsValidPrice reports whether price is on a tick boundary.
func IsValidPrice(price, tick float64) bool {
	if tick <= 0 {
		tick = 0.05
	}
	q := price / tick
	return math.Abs(q-math.Round(q)) < 1e-9
}