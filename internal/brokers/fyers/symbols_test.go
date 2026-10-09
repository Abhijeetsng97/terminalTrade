package fyers

import (
	"testing"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// Fyers index-option symbology is what makes cross-broker netting
// work; round-trips must be exact.
func TestDeriveSymbol(t *testing.T) {
	cases := []struct {
		underlying string
		expiry     string
		strike     float64
		otype      core.OptionType
		want       string
	}{
		{"NIFTY", "2026-10-29", 26000, core.Call, "NSE:NIFTY26O2926000CE"},
		{"BANKNIFTY", "2026-11-26", 52000, core.Put, "NSE:BANKNIFTY26N2652000PE"},
		{"FINNIFTY", "2026-12-31", 24000, core.Call, "NSE:FINNIFTY26D3124000CE"},
		{"SENSEX", "2026-03-05", 82000, core.Put, "NSE:SENSEX2630582000PE"},
	}
	for _, c := range cases {
		e, _ := time.ParseInLocation("2006-01-02", c.expiry, core.LocalIST())
		inst := core.Instrument{
			Underlying: c.underlying, Kind: core.KindIndexOption,
			Expiry: e, Strike: c.strike, OptionType: c.otype,
		}
		got, ok := DeriveSymbol(inst)
		if !ok {
			t.Errorf("%v: derivation failed", c.want)
			continue
		}
		if got != c.want {
			t.Errorf("DeriveSymbol: got %s want %s", got, c.want)
		}
	}
}

func TestDeriveSymbolRejectsNonOptions(t *testing.T) {
	if _, ok := DeriveSymbol(core.Instrument{Underlying: "NIFTY"}); ok {
		t.Error("option-less instrument must not derive")
	}
}

func TestDeriveIndex(t *testing.T) {
	if got := DeriveIndex("NIFTY"); got != "NSE:NIFTY50-INDEX" {
		t.Errorf("got %s", got)
	}
	if got := DeriveIndex("BANKNIFTY"); got != "NSE:NIFTYBANK-INDEX" {
		t.Errorf("got %s", got)
	}
}