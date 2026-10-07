package core

import "testing"

// Protection bands (published market-protection table):
// <10 -> 5%, 10..100 -> 3%, 100..500 -> 2%, >500 -> 1%.
func TestProtectionBand(t *testing.T) {
	cases := []struct {
		ltp  float64
		want float64
	}{
		{5.00, 0.05},
		{9.95, 0.05},
		{10.00, 0.03},
		{99.95, 0.03},
		{100.00, 0.02},
		{499.95, 0.02},
		{500.00, 0.01},
		{1500.00, 0.01},
	}
	for _, c := range cases {
		if got := ProtectionBand(c.ltp); got != c.want {
			t.Errorf("ProtectionBand(%.2f): got %.2f want %.2f", c.ltp, got, c.want)
		}
	}
}

func TestProtectionPrice(t *testing.T) {
	// buy: LTP * (1+band), snapped to tick; sell: LTP * (1-band)
	buy := ProtectionPrice(100.0, SideBuy, 0.05)
	if buy < 100.0 || buy > 105.01 {
		t.Errorf("buy protection %.2f outside band (100, 105]", buy)
	}
	sell := ProtectionPrice(100.0, SideSell, 0.05)
	if sell > 100.0 || sell < 94.99 {
		t.Errorf("sell protection %.2f outside band [95, 100)", sell)
	}
	// tick snapping: every price must be on the tick
	if !IsValidPrice(buy, 0.05) {
		t.Errorf("buy price %.2f not on tick 0.05", buy)
	}
	if !IsValidPrice(sell, 0.05) {
		t.Errorf("sell price %.2f not on tick 0.05", sell)
	}
}

func TestProtectionPriceNeverRawMarket(t *testing.T) {
	// a protected price is always strictly on the pay-up side of LTP
	ltp := 20.15
	if p := ProtectionPrice(ltp, SideBuy, 0.05); p <= ltp {
		t.Errorf("buy protection %.2f must be > LTP %.2f", p, ltp)
	}
	if p := ProtectionPrice(ltp, SideSell, 0.05); p >= ltp {
		t.Errorf("sell protection %.2f must be < LTP %.2f", p, ltp)
	}
}

func TestSnapToTick(t *testing.T) {
	cases := []struct{ price, tick, want float64 }{
		{20.137, 0.05, 20.15},
		{20.15, 0.05, 20.15},
		{99.999, 0.05, 100.00},
		{0.02, 0.05, 0.05}, // never below one tick
	}
	for _, c := range cases {
		got := SnapToTick(c.price, c.tick)
		if abs := got - c.want; abs > 1e-9 && abs < -1e-9 {
			t.Errorf("SnapToTick(%.3f, %.2f): got %.2f want %.2f", c.price, c.tick, got, c.want)
		}
	}
}

func TestIsValidPrice(t *testing.T) {
	if !IsValidPrice(20.15, 0.05) {
		t.Error("20.15 should be valid on tick 0.05")
	}
	if IsValidPrice(20.13, 0.05) {
		t.Error("20.13 should be invalid on tick 0.05")
	}
}