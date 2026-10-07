package core

import (
	"testing"
	"time"
)

// Slicing is the money-critical arithmetic: every slice must be <=
// freeze, and slices must sum to the total. Freeze limits change by
// NSE circular (NIFTY: 1800 -> 3510 on 2026-10-05) so the tests pin
// the *math*, not the numbers.
func TestPlanSlices(t *testing.T) {
	nifty := Instrument{
		Underlying: "NIFTY", Kind: KindIndexOption,
		OptionType: Call, Strike: 26000,
		LotSize: 65, FreezeQty: 3510, TickSize: 0.05,
	}

	tests := []struct {
		name      string
		lots      int
		freeze    int
		lotSize   int
		wantLens  []int // per-slice quantities
	}{
		{"single slice under freeze", 10, 3510, 65, []int{650}},
		{"exactly freeze", 54, 3510, 65, []int{3510}},
		{"one over freeze", 55, 3510, 65, []int{3510, 65}},
		{"100 lots fire-all", 100, 3510, 65, []int{3510, 2990}},
		{"old freeze limit 27 lots", 27, 1800, 65, []int{1755}},
		{"old freeze one over", 28, 1800, 65, []int{1800, 20}},
		{"banknifty current", 48, 1440, 30, []int{1440}},
		{"banknifty over", 50, 1440, 30, []int{1440, 60}},
		{"zero lots", 0, 3510, 65, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := nifty
			inst.FreezeQty = tc.freeze
			inst.LotSize = tc.lotSize
			plan := PlanSlices(inst, tc.lots)
			if len(plan.Quantities) != len(tc.wantLens) {
				t.Fatalf("got %d slices %v, want %d %v", len(plan.Quantities), plan.Quantities, len(tc.wantLens), tc.wantLens)
			}
			total := 0
			for i, q := range plan.Quantities {
				if q != tc.wantLens[i] {
					t.Errorf("slice %d: got %d want %d", i, q, tc.wantLens[i])
				}
				if q > tc.freeze {
					t.Errorf("slice %d qty %d exceeds freeze %d", i, q, tc.freeze)
				}
				total += q
			}
			if tc.lots > 0 && total != tc.lots*tc.lotSize {
				t.Errorf("slices sum %d != total %d", total, tc.lots*tc.lotSize)
			}
		})
	}
}

// Unknown freeze (stale instruments) must not block small orders.
func TestPlanSlicesUnknownFreeze(t *testing.T) {
	inst := Instrument{LotSize: 65, FreezeQty: 0}
	plan := PlanSlices(inst, 2)
	if len(plan.Quantities) != 1 || plan.Quantities[0] != 130 {
		t.Fatalf("unknown freeze should single-slice, got %v", plan.Quantities)
	}
}

func TestMaxLotsPerOrder(t *testing.T) {
	nifty := Instrument{LotSize: 65, FreezeQty: 3510}
	if got := nifty.MaxLotsPerOrder(); got != 54 {
		t.Errorf("NIFTY max lots: got %d want 54 (3510/65, post 2026-10-05)", got)
	}
	old := Instrument{LotSize: 65, FreezeQty: 1800}
	if got := old.MaxLotsPerOrder(); got != 27 {
		t.Errorf("old NIFTY freeze: got %d want 27", got)
	}
	if got := (Instrument{LotSize: 65, FreezeQty: 0}).MaxLotsPerOrder(); got != 0 {
		t.Errorf("no freeze: got %d want 0", got)
	}
}

func TestInstrumentKey(t *testing.T) {
	i := Instrument{Underlying: "NIFTY", Kind: KindIndexOption,
		Expiry: mustDate("2026-10-29"), Strike: 26000, OptionType: Call}
	want := "NIFTY:OPTIDX:20261029:26000:CE"
	if got := i.Key(); got != want {
		t.Errorf("Key: got %q want %q", got, want)
	}
	// same instrument from the two brokers must yield the same key
	fyers := i
	fyers.BrokerSymbols = nil
	if got := fyers.Key(); got != want {
		t.Errorf("key must be broker-independent: got %q", got)
	}
}

func mustDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, LocalIST())
	if err != nil {
		panic(err)
	}
	return t
}