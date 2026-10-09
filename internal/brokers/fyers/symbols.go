// Package fyers holds Fyers symbology helpers shared by the app
// layer (adapters stay broker-private).
package fyers

import (
	"fmt"
	"strings"
	"time"

	"github.com/Abhijeetsng97/terminalTrade/internal/core"
)

// DeriveSymbol builds a Fyers v3 index-option symbol from the
// canonical instrument: NSE:<UNDERLYING><YY><M><DD><STRIKE><CE|PE>
// where M ∈ {1..9, O, N, D} (O=Oct, N=Nov, D=Dec).
// Example: NIFTY, expiry 2026-10-29, strike 26000, CE
//
//	-> NSE:NIFTY26O2926000CE
func DeriveSymbol(i core.Instrument) (string, bool) {
	if !i.IsOption() || i.Expiry.IsZero() || i.Strike <= 0 {
		return "", false
	}
	e := i.Expiry.In(core.LocalIST())
	m := int(e.Month())
	var mc string
	if m <= 9 {
		mc = fmt.Sprintf("%d", m)
	} else {
		mc = map[int]string{10: "O", 11: "N", 12: "D"}[m]
	}
	sym := fmt.Sprintf("%s%02d%s%02d%.0f%s",
		strings.ToUpper(i.Underlying), e.Year()%100, mc, e.Day(), i.Strike, string(i.OptionType))
	return "NSE:" + sym, true
}

// DeriveIndex builds the underlying index symbol (quotes/chain query):
// NSE:NIFTY50-INDEX, NSE:NIFTYBANK-INDEX.
func DeriveIndex(underlying string) string {
	switch strings.ToUpper(underlying) {
	case "NIFTY":
		return "NSE:NIFTY50-INDEX"
	case "BANKNIFTY":
		return "NSE:NIFTYBANK-INDEX"
	case "FINNIFTY":
		return "NSE:FINNIFTY-INDEX"
	case "MIDCPNIFTY":
		return "NSE:MIDCPNIFTY-INDEX"
	default:
		return "NSE:" + strings.ToUpper(underlying) + "-INDEX"
	}
}

var _ = time.Now